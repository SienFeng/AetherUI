package service

// 多日场景推演：2026-11-10 修「防共享 / 防滥用」几条规则叠在一起的冲突时加的回归测试。
//
// 单条判定的测试看不出规则叠加的后果：那次的问题（新用户被锁后出差没有兜底、开通第一天在外地的人被锁在外地省、
// 两段出差回家反被锁、额度快用完时搬家被锁死）都只在按天推演时才出现。这里用真实的纯判定函数（decideAutoRegion /
// decideRelocation / decideAutoTravel 及其取数辅助函数）每 10 分钟推演一步，数据库、访问日志、在线表由一个小世界
// 模型代替；另有两个共享场景守着「修了误伤、没放松防共享」。
//
// 镜像的是 AutoRegionService.Run、AutoTravelService.Run 与 assemble 的单入站取数和执行顺序：改那几处的取数逻辑时
// 要同步改这里（接线本身由 TestRun* 端到端测试守着）。共享风险评分不推演，恒为「低」；机房拦截不推演。

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"a-ui/database/model"
)

var scenLoc = time.FixedZone("CST", 8*3600)

type scenProv struct{ name, ip, cidr string }

var (
	scenA      = scenProv{"江苏省", "192.0.2.10", "192.0.2.0/25"}
	scenB      = scenProv{"河南省", "198.51.100.10", "198.51.100.0/24"}
	scenC      = scenProv{"浙江省", "203.0.113.10", "203.0.113.0/24"}
	scenAbroad = scenProv{ip: "192.0.2.200"}  // 境外的 IPv4：不在任何省的网段里，查不到省份
	scenIPv6   = scenProv{ip: "2001:db8::20"} // 双栈设备的 IPv6：地区限制下一律被拦
	scenRoad   = scenProv{}                   // 在路上：不用
	scenProvs  = []scenProv{scenA, scenB, scenC}
)

func scenSetFor(regions []string) (*cidrSet, error) {
	var cidrs []string
	for _, r := range regions {
		for _, p := range scenProvs {
			if p.name == r {
				cidrs = append(cidrs, p.cidr)
			}
		}
	}
	return newCIDRSet(cidrs), nil
}

func scenProvinceOf(ip string) string {
	for _, p := range scenProvs {
		if newCIDRSet([]string{p.cidr}).Contains(ip) {
			return p.name
		}
	}
	return ""
}

func scenAt(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, scenLoc) }

// scenDaytime：每天 08:00~23:00 在用。
func scenDaytime(t time.Time) bool { return t.Hour() >= 8 && t.Hour() < 23 }

type scenUser struct {
	name  string
	where func(time.Time) scenProv
	using func(time.Time) bool
	// perTick 是放行时每 10 分钟传的字节数，0 表示重度使用（10 MB）。低于 1 MB/小时的设备不算「实质使用」，
	// 在线表里也不算「在用」。
	perTick int64
}

type scenWorld struct {
	t         *testing.T
	params    autoTravelParams
	arParams  autoRegionParams
	masterOn  bool
	regions   []string
	state     *model.InboundAutoRegion
	passes    []*model.InboundRegionPass
	nextID    int
	rows      map[string]*model.InboundIPHour
	blockedHr map[string]int64 // ip|整点毫秒 → 这一小时最后一次被拦的毫秒
	lastBlock map[string]int64 // ip → 最后一次被拦的毫秒
	users     []scenUser
	blocked   map[string]map[string]int // 用户 → 日期 → 想用却被拦的分钟数
	served    map[string]int            // 用户 → 被放行的分钟数
	servedDay map[string]map[string]int // 用户 → 日期 → 被放行的分钟数
	relocated []time.Time               // 每次长住改地区的时刻
	log       []string
	lastKey   string
}

func newScenWorld(t *testing.T, users ...scenUser) *scenWorld {
	return &scenWorld{t: t, params: defaultAutoTravelParams(), arParams: defaultAutoRegionParams(), masterOn: true,
		rows: map[string]*model.InboundIPHour{}, blockedHr: map[string]int64{}, lastBlock: map[string]int64{},
		users: users, blocked: map[string]map[string]int{}, served: map[string]int{}, servedDay: map[string]map[string]int{}}
}

// withHome：已有地区限制（接管过的存量用户），并预置 from~to 之间每天 08~23 点在家里的实质使用。
func (w *scenWorld) withHome(p scenProv, from, to time.Time) *scenWorld {
	w.regions = []string{p.name}
	w.state = &model.InboundAutoRegion{State: model.AutoRegionAdopted, AdoptedAt: 1}
	for t := from; t.Before(to); t = t.Add(time.Hour) {
		if scenDaytime(t) {
			h := hourStartOf(t)
			w.rows[fmt.Sprintf("%s|%d", p.ip, h)] = &model.InboundIPHour{IP: p.ip, HourStart: h, Province: p.name,
				ActiveBytes: 60 << 20, ActiveSeconds: 3600}
		}
	}
	return w
}

func (w *scenWorld) logf(now time.Time, format string, args ...any) {
	w.log = append(w.log, now.Format("01-02 15:04")+"  "+fmt.Sprintf(format, args...))
}

func (w *scenWorld) active() *model.InboundRegionPass {
	for _, p := range w.passes {
		if p.EndedAt == 0 {
			return p
		}
	}
	return nil
}

func (w *scenWorld) rowsSince(cutoff int64) []model.InboundIPHour {
	var out []model.InboundIPHour
	for _, r := range w.rows {
		if r.HourStart >= cutoff {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HourStart != out[j].HourStart {
			return out[i].HourStart < out[j].HourStart
		}
		return out[i].IP < out[j].IP
	})
	return out
}

func (w *scenWorld) run(from, to time.Time) {
	for t := from; t.Before(to); t = t.Add(10 * time.Minute) {
		w.tick(t)
	}
}

func (w *scenWorld) tick(now time.Time) {
	nowMs := now.UnixMilli()
	for _, p := range w.passes { // CheckInboundJob → ExpireDue
		if p.EndedAt == 0 && p.Until <= nowMs {
			p.EndedAt, p.EndReason = nowMs, model.RegionPassExpired
			w.logf(now, "通行证 %s 到期", passProvince(*p))
		}
	}
	act := w.active()
	eff := append([]string(nil), w.regions...)
	if act != nil {
		eff = append(eff, passProvince(*act))
	}
	effSet, _ := scenSetFor(eff)
	noRegion := len(w.regions) == 0

	// 用户的连接：放行的记进小时行与在线表，被拦的记进访问日志。
	var online []OnlineIP
	for _, u := range w.users {
		loc := u.where(now)
		if loc.ip == "" || !u.using(now) {
			continue
		}
		if noRegion || effSet.Contains(loc.ip) {
			key := fmt.Sprintf("%s|%d", loc.ip, hourStartOf(now))
			r := w.rows[key]
			if r == nil {
				r = &model.InboundIPHour{IP: loc.ip, HourStart: hourStartOf(now), Province: loc.name}
				w.rows[key] = r
			}
			perTick := u.perTick
			if perTick == 0 {
				perTick = 10 << 20
			}
			r.ActiveBytes += perTick
			r.ActiveSeconds += 600
			w.served[u.name] += 10
			if w.servedDay[u.name] == nil {
				w.servedDay[u.name] = map[string]int{}
			}
			w.servedDay[u.name][now.Format("01-02")] += 10
			online = append(online, OnlineIP{IP: loc.ip, Conns: 1, Up: perTick})
			continue
		}
		day := now.Format("01-02")
		if w.blocked[u.name] == nil {
			w.blocked[u.name] = map[string]int{}
		}
		w.blocked[u.name][day] += 10
		w.blockedHr[fmt.Sprintf("%s|%d", loc.ip, hourStartOf(now)*1000)] = nowMs
		w.lastBlock[loc.ip] = nowMs
	}
	rows7 := w.rowsSince(now.Add(-time.Duration(sharingWindowDays) * 24 * time.Hour).Unix())

	if noRegion { // AutoRegionService.Run（真实任务 10 分钟一轮）
		span := w.rowsSince(now.Add(-time.Duration(max(sharingWindowDays, observeDaysWithinRetention(w.arParams)+1)) * 24 * time.Hour).Unix())
		d := decideAutoRegion(autoRegionInput{Now: now, Loc: scenLoc, Rows: rows7, SpanRows: span, RiskLevel: RiskLevelLow,
			Params: w.arParams, AllowedSet: scenSetFor,
			UnplaceablePercent: func(regions []string) (float64, error) {
				set, _ := scenSetFor(regions)
				return unplaceableShare(rows7, set), nil
			}})
		if d.Action == autoRegionApply {
			enc, _ := EncodeRegions(d.Regions)
			w.regions = d.Regions
			w.state = &model.InboundAutoRegion{State: model.AutoRegionApplied, Regions: enc}
			w.logf(now, "【自动设置地区】%s —— %s", strings.Join(d.Regions, "、"), d.Reason)
		}
		return
	}

	// AutoTravelService.Run + assemble 的单入站路径。
	home, _ := scenSetFor(w.regions)
	var passSet *cidrSet
	if act != nil {
		passSet, _ = scenSetFor([]string{passProvince(*act)})
	}
	in := autoTravelInput{Now: now, Loc: scenLoc, Active: act, Params: w.params, RiskLevel: RiskLevelLow}
	dayAgo := now.Add(-24 * time.Hour).Unix()
	for _, r := range rows7 {
		if home.Contains(r.IP) {
			in.HomeRows = append(in.HomeRows, r)
		} else if passSet.Contains(r.IP) && r.HourStart >= dayAgo {
			in.PassRows = append(in.PassRows, r)
		}
	}
	in.HomeLiveActive, in.PassLiveActive = liveSignals(online, home, passSet)
	historySince := now.Add(-autoTravelHistoryWindow).UnixMilli() // 通行证历史先读（换省判断要用）
	for _, p := range w.passes {
		if p.GrantedAt >= historySince || p.EndedAt == 0 || p.EndedAt >= historySince {
			in.History = append(in.History, *p)
		}
	}
	isV4 := func(ip string) bool { p := net.ParseIP(ip); return p != nil && p.To4() != nil }
	if act == nil || act.Auto {
		var cands []autoTravelCandidate
		for ip, at := range w.lastBlock { // recentGeoBlocked：只看 IPv4
			if at >= nowMs-autoTravelBlockedLookback.Milliseconds() && isV4(ip) {
				cands = append(cands, autoTravelCandidate{IP: ip, Province: scenProvinceOf(ip)})
			}
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].IP < cands[j].IP })
		in.Blocked = dropStoredCandidates(cands, home)
		needSuspect := act == nil
		if act != nil {
			in.Blocked = dropStoredCandidates(in.Blocked, passSet)
			probe := autoTravelInput{Now: now, Active: act, Params: w.params, Blocked: in.Blocked, History: in.History,
				PassRows: in.PassRows, PassLiveActive: in.PassLiveActive, ObservationStale: in.ObservationStale}
			needSuspect = len(moveOnCandidates(probe, passInUse(probe))) > 0
		}
		if needSuspect && len(in.Blocked) > 0 && w.params.Cooldown > 0 {
			since := now.Add(-min(w.params.Cooldown, time.Duration(sharingWindowDays)*24*time.Hour)).UnixMilli()
			var bh []blockedHour
			for key, last := range w.blockedHr {
				parts := strings.SplitN(key, "|", 2)
				var hourMs int64
				fmt.Sscan(parts[1], &hourMs)
				if last >= since && !home.Contains(parts[0]) {
					bh = append(bh, blockedHour{IP: parts[0], HourMs: hourMs})
				}
			}
			in.SuspectProvinces = suspectProvinces(bh, rows7, home, scenProvinceOf)
		}
	}
	in.ServedAway = servedAwayRows(rows7, home, in.History, scenSetFor, now)
	in.Recent = func() ([]recentSource, error) { // recentBlockedSources：IPv4、家里之外
		var out []recentSource
		for ip, at := range w.lastBlock {
			if at >= now.Add(-relocatePresenceWindow).UnixMilli() && isV4(ip) && !home.Contains(ip) {
				sec := at / 1000
				out = append(out, recentSource{Province: scenProvinceOf(ip), Hour: sec - sec%3600})
			}
		}
		return out, nil
	}

	if w.masterOn && w.params.RelocateDays > 0 {
		in.Relocation = &autoTravelRelocation{Days: w.params.RelocateDays, State: w.state}
		rd := decideRelocation(relocateInput{Now: now, Loc: scenLoc, Enabled: true, Days: w.params.RelocateDays,
			Active: act, HomeRows: in.HomeRows, HomeLiveActive: in.HomeLiveActive, ServedAway: in.ServedAway,
			History: in.History, State: w.state, RiskLevel: in.RiskLevel, Recent: in.Recent})
		if rd.Province != "" {
			w.relocateTo(now, rd.Province, act)
			w.logf(now, "【长住改地区】%s", rd.Reason)
			w.lastKey = ""
			return
		}
	}

	d := decideAutoTravel(in)
	switch d.Action {
	case autoTravelGranted:
		w.nextID++
		enc, _ := EncodeRegions([]string{d.Province})
		w.passes = append(w.passes, &model.InboundRegionPass{Id: w.nextID, Auto: true, Regions: enc, GrantedAt: nowMs, Until: d.Until})
	case autoTravelRenewed:
		act.Until = d.Until
	case autoTravelEnded:
		act.EndedAt, act.EndReason = nowMs, d.EndReason
	case autoTravelMoved: // 同一轮先收回、再发放
		act.EndedAt, act.EndReason = nowMs, model.RegionPassMoved
		w.nextID++
		enc, _ := EncodeRegions([]string{d.Province})
		w.passes = append(w.passes, &model.InboundRegionPass{Id: w.nextID, Auto: true, Regions: enc, GrantedAt: nowMs, Until: d.Until})
	}
	if key := d.Action + "|" + d.Reason; key != w.lastKey {
		w.lastKey = key
		w.logf(now, "[%s] %s %s", d.Action, d.Province, d.Reason)
	}
}

// relocateTo 镜像 relocate()：改地区、以 relocated 结束生效中的通行证、把这个省近 7 天到期或换省结束的自动通行证
// 改记成 relocated、写状态行。
func (w *scenWorld) relocateTo(now time.Time, province string, act *model.InboundRegionPass) {
	nowMs := now.UnixMilli()
	if act != nil {
		act.EndedAt, act.EndReason = nowMs, model.RegionPassRelocated
	}
	since := now.Add(-time.Duration(sharingWindowDays) * 24 * time.Hour).UnixMilli()
	for _, p := range w.passes {
		if p.Auto && (p.EndReason == model.RegionPassExpired || p.EndReason == model.RegionPassMoved) &&
			p.EndedAt >= since && passProvince(*p) == province {
			p.EndReason = model.RegionPassRelocated
		}
	}
	prev, _ := EncodeRegions(w.regions)
	relocatedAt := nowMs
	if w.state != nil && regionsContain(w.state.PrevRegions, province) {
		relocatedAt = w.state.RelocatedAt
	}
	enc, _ := EncodeRegions([]string{province})
	w.state = &model.InboundAutoRegion{State: model.AutoRegionApplied, Regions: enc, PrevRegions: prev, RelocatedAt: relocatedAt}
	w.regions = []string{province}
	w.relocated = append(w.relocated, now)
}

// worstDay 是某个用户一天里想用却被拦得最久的那一天（分钟）。
func (w *scenWorld) worstDay(user string) (string, int) {
	day, most := "", 0
	for d, m := range w.blocked[user] {
		if m > most || (m == most && d < day) {
			day, most = d, m
		}
	}
	return day, most
}

func (w *scenWorld) failf(format string, args ...any) {
	w.t.Helper()
	w.t.Errorf(format, args...)
	w.t.Log("推演记录：\n" + strings.Join(w.log, "\n"))
}

// expectNoLongBlock：用户任何一天被拦都不超过 limit 分钟（通行证从被拦到发放要等一两轮）。
func (w *scenWorld) expectNoLongBlock(user string, limit int) {
	w.t.Helper()
	if day, most := w.worstDay(user); most > limit {
		w.failf("%s 在 %s 被拦了 %d 分钟（上限 %d），每天：%v", user, day, most, limit, w.blocked[user])
	}
}

// expectNoBlockFrom：从 day（含）起用户一分钟也没被拦。
func (w *scenWorld) expectNoBlockFrom(user, day string) {
	w.t.Helper()
	for d, m := range w.blocked[user] {
		if d >= day && m > 0 {
			w.failf("%s 在 %s 被拦了 %d 分钟，期望 %s 起不再被拦，每天：%v", user, d, m, day, w.blocked[user])
			return
		}
	}
}

// expectServedEveryDay：from~to（含两头）的每一天，这些设备里至少有一台被放行过——人没有被整天锁在外面。
func (w *scenWorld) expectServedEveryDay(from, to time.Time, devices ...string) {
	w.t.Helper()
	for d := from; !d.After(to); d = d.Add(24 * time.Hour) {
		day, total := d.Format("01-02"), 0
		for _, dev := range devices {
			total += w.servedDay[dev][day]
		}
		if total == 0 {
			w.failf("%s 这一天 %v 一台都没被放行（被拦：%v）", day, devices, w.blocked)
			return
		}
	}
}

func (w *scenWorld) expectRegion(want string) {
	w.t.Helper()
	if len(w.regions) != 1 || w.regions[0] != want {
		w.failf("最终地区 = %v，期望 %s", w.regions, want)
	}
}

func onlyAt(p scenProv) func(time.Time) scenProv { return func(time.Time) scenProv { return p } }

// legs 按时间段给出用户所在：legs(scenA, t1, scenRoad, t2, scenB) 表示 t1 之前在 A、t1~t2 在路上、t2 之后在 B。
func legs(parts ...any) func(time.Time) scenProv {
	return func(t time.Time) scenProv {
		for i := 0; i+2 < len(parts); i += 2 {
			if t.Before(parts[i+1].(time.Time)) {
				return parts[i].(scenProv)
			}
		}
		return parts[len(parts)-1].(scenProv)
	}
}

// 新用户在家用了两天、第三天出差 5 天：改动前第 2 天晚上就被锁到家里的省，整趟出差每天被拦约 15 小时（出差放行
// 要求家里先用满 3 天）。现在等那个省用满 3 天才锁：出差期间不被拦，最后落在家里。
func TestScenarioNewUserTravelsBeforeHomeIsStable(t *testing.T) {
	w := newScenWorld(t, scenUser{name: "用户", using: scenDaytime,
		where: legs(scenA, scenAt(11, 3, 21), scenRoad, scenAt(11, 4, 8), scenB, scenAt(11, 9, 12), scenRoad, scenAt(11, 9, 15), scenA)})
	w.run(scenAt(11, 2, 20), scenAt(11, 24, 0))
	w.expectNoLongBlock("用户", 30)
	w.expectRegion(scenA.name)
}

// 开通的第一天恰好在外地：改动前地区被锁成外地省，回家后天天被拦、不会自愈。现在外地省凑不够 3 天不锁，
// 回家用满 3 天后锁成家里的省。
func TestScenarioNewUserFirstDayAway(t *testing.T) {
	w := newScenWorld(t, scenUser{name: "用户", using: scenDaytime,
		where: legs(scenB, scenAt(11, 3, 12), scenRoad, scenAt(11, 3, 16), scenA)})
	w.run(scenAt(11, 2, 9), scenAt(11, 20, 0))
	w.expectNoLongBlock("用户", 0)
	w.expectRegion(scenA.name)
}

// 老用户两段出差 A → B → C → 回家。改动前：到 C 后被 B 的通行证挡约 2 天；总开关开着时第 5 天地区被改成 B（人在
// C），回家以后被一直拦着。现在 B 没人用了、C 在被拦就换省；长住只改到人真在的那个省；回家后最终回到 A。
func TestScenarioTwoLegTrip(t *testing.T) {
	cases := []struct {
		name                   string
		bEnd, cStart, cEnd, aT time.Time
		master                 bool
	}{
		{"B 3 天、C 6 天", scenAt(11, 13, 12), scenAt(11, 13, 15), scenAt(11, 19, 12), scenAt(11, 19, 15), true},
		{"B 3 天、C 6 天（总开关关）", scenAt(11, 13, 12), scenAt(11, 13, 15), scenAt(11, 19, 12), scenAt(11, 19, 15), false},
		{"B 3 天、C 3 天", scenAt(11, 13, 12), scenAt(11, 13, 15), scenAt(11, 16, 12), scenAt(11, 16, 15), true},
		{"B 3 天、C 2 天", scenAt(11, 13, 12), scenAt(11, 13, 15), scenAt(11, 15, 12), scenAt(11, 15, 15), true},
		{"B 2 天、C 6 天", scenAt(11, 12, 12), scenAt(11, 12, 15), scenAt(11, 18, 12), scenAt(11, 18, 15), true},
		{"B 1 天、C 6 天", scenAt(11, 11, 12), scenAt(11, 11, 15), scenAt(11, 17, 12), scenAt(11, 17, 15), true},
		{"B 到 C 只坐 1 小时车", scenAt(11, 13, 12), scenAt(11, 13, 13), scenAt(11, 19, 12), scenAt(11, 19, 15), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newScenWorld(t, scenUser{name: "用户", using: scenDaytime,
				where: legs(scenA, scenAt(11, 10, 10), scenRoad, scenAt(11, 10, 14), scenB, c.bEnd, scenRoad, c.cStart, scenC, c.cEnd, scenRoad, c.aT, scenA)})
			w.masterOn = c.master
			w.withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
			w.run(scenAt(11, 10, 0), scenAt(12, 8, 0))
			// 换省要等 B 静默满 2 小时：B→C 车程短于 2 小时的那一段会多挡一会儿。
			w.expectNoLongBlock("用户", 150)
			w.expectRegion(scenA.name)
		})
	}
}

// 一个人两台设备分属两个省（笔记本连酒店 Wi-Fi 在 B，手机流量的地址判在 C），白天用笔记本、晚上用手机。
// 「换了地方」不能把人整个锁在外面：换过去之前要确认新的省此刻发得出通行证（每周次数、短命通行证……），发不出就
// 保留手上这张。改动前（没有换省）笔记本那张一直续着；只换不看的话，来回换两次撞上每周 3 次，旧的已结束、新的
// 发不出，两台都被拦，近 7 天在家用过的天数一过期就再也发不出来。
func TestScenarioTwoDevicesInTwoProvinces(t *testing.T) {
	for _, master := range []bool{true, false} {
		t.Run(fmt.Sprintf("总开关=%v", master), func(t *testing.T) {
			// 出差 8 天：不碰每月 10 天的额度（总开关关着时没有预支，超过额度被拦是额度在起作用，不是这里要测的）。
			arrive, leave := scenAt(11, 10, 14), scenAt(11, 18, 12)
			laptop := scenUser{name: "笔记本", using: func(t time.Time) bool { return t.Hour() >= 9 && t.Hour() < 18 },
				where: legs(scenA, scenAt(11, 10, 10), scenRoad, arrive, scenB, leave, scenRoad, scenAt(11, 18, 16), scenA)}
			phone := scenUser{name: "手机", using: func(t time.Time) bool { return t.Hour() >= 19 && t.Hour() < 23 },
				where: legs(scenA, scenAt(11, 10, 10), scenRoad, arrive, scenC, leave, scenRoad, scenAt(11, 18, 16), scenA)}
			w := newScenWorld(t, laptop, phone).withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
			w.masterOn = master
			w.run(scenAt(11, 10, 0), scenAt(11, 24, 0))
			w.expectServedEveryDay(scenAt(11, 11, 0), scenAt(11, 17, 0), "笔记本", "手机")
		})
	}
}

// 换了地方之后，旧省还留着一台低流量、一直在连的设备（比如落在那边的摄像头）。人带着笔记本从 B 去了 C：换省时选的是
// C，下一步也必须给 C 发通行证，不能按来源 IP 的顺序又发回 B（代码审查指出）；B 那台低流量设备也不该把通行证一再抢回去。
func TestScenarioLightDeviceLeftInOldProvince(t *testing.T) {
	laptop := scenUser{name: "笔记本", using: scenDaytime,
		where: legs(scenA, scenAt(11, 10, 10), scenRoad, scenAt(11, 10, 14), scenB, scenAt(11, 12, 12), scenRoad, scenAt(11, 12, 15),
			scenC, scenAt(11, 18, 12), scenRoad, scenAt(11, 18, 16), scenA)}
	camera := scenUser{name: "B 的摄像头", where: legs(scenRoad, scenAt(11, 10, 14), scenB), using: func(time.Time) bool { return true },
		perTick: 20 << 10}
	w := newScenWorld(t, laptop, camera).withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(11, 24, 0))
	w.expectNoLongBlock("笔记本", 150)
}

// 出差 9 天（额度之内），中间有一天多没上线（第 6 天上午用完，到第 7 天傍晚才再用），总开关关着（升级后的默认）。
// 通行证在这一天多里不能断：断了之后「近 7 天在家用过」已不满足、发不出新的，又没有长住改地区，人到回家前都会被
// 拦着（续期曾改成「家里静默时长内仍在用」，就是这么锁死的）。
func TestScenarioIdleDayMidTripMasterOff(t *testing.T) {
	using := func(t time.Time) bool {
		if t.After(scenAt(11, 16, 10)) && t.Before(scenAt(11, 17, 18)) {
			return false
		}
		return scenDaytime(t)
	}
	w := newScenWorld(t, scenUser{name: "用户", using: using,
		where: legs(scenA, scenAt(11, 10, 10), scenRoad, scenAt(11, 10, 14), scenB, scenAt(11, 19, 12), scenRoad, scenAt(11, 19, 16), scenA)})
	w.masterOn = false
	w.withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(11, 25, 0))
	w.expectNoLongBlock("用户", 30)
}

// 去 B 三天，接着出境六天（境外来源一律被拦、查不到省份，不会被自动放行）再回家；或者接着一直没上线。没有别的省
// 在被拦，「换了地方」不会触发，只有「人此刻还在那」那道门槛挡得住：改动前第 5 天地区就被改成 B（人在境外或
// 离线），回家以后被一直拦着。现在最近的动静不在 B（或根本没有），先不改，回家照常。
func TestScenarioTripThenAbroadOrOffline(t *testing.T) {
	for _, c := range []struct {
		name string
		then scenProv
	}{{"出境", scenAbroad}, {"一直没上线", scenRoad}} {
		t.Run(c.name, func(t *testing.T) {
			w := newScenWorld(t, scenUser{name: "用户", using: scenDaytime,
				where: legs(scenA, scenAt(11, 10, 10), scenRoad, scenAt(11, 10, 14), scenB, scenAt(11, 13, 12), c.then, scenAt(11, 19, 15), scenA)})
			w.withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
			w.run(scenAt(11, 10, 0), scenAt(12, 5, 0))
			w.expectNoBlockFrom("用户", "11-20")
			w.expectRegion(scenA.name)
		})
	}
}

// 搬去 B 常住，手机是双栈的：IPv6 来连在地区限制下一律被拦（查不到省份），人在哪都会留下。它不能被当成「去了别处」
// 否决长住改地区（代码审查指出）。坐夜车走：家里最后一次在用是 11-10 22 点那一小时，判定点是 11-15 23:00——笔记本那时
// 已经停了，只有手机的 IPv6 还在来连；算进「别处」的话要拖到第二天早上笔记本再用时才改。
func TestScenarioMoveWithDualStackPhone(t *testing.T) {
	laptop := scenUser{name: "笔记本", using: scenDaytime,
		where: legs(scenA, scenAt(11, 10, 22).Add(30*time.Minute), scenRoad, scenAt(11, 11, 8), scenB)}
	phone6 := scenUser{name: "手机 IPv6", where: onlyAt(scenIPv6), using: func(time.Time) bool { return true }}
	w := newScenWorld(t, laptop, phone6).withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(11, 24, 0))
	w.expectNoLongBlock("笔记本", 30)
	w.expectRegion(scenB.name)
	if len(w.relocated) == 0 || w.relocated[0].After(scenAt(11, 15, 23).Add(30*time.Minute)) {
		w.failf("判定点 11-15 23:00 一到就该改成 B，实际改地区的时刻：%v", w.relocated)
	}
}

// 本月两次 4 天出差（已用约 8 天额度）后搬家：改动前通行证只给到剩下的 1.8 天，在新地方攒不够 3 天，先因额度满、
// 再因「近 7 天在家用过」不满足被一直拦着，下月也不恢复。现在预支到长住判定点，第 5 天改成新地方，额度退回。
func TestScenarioMoveWithQuotaAlmostUsed(t *testing.T) {
	w := newScenWorld(t, scenUser{name: "用户", using: scenDaytime,
		where: legs(scenA, scenAt(11, 2, 10), scenRoad, scenAt(11, 2, 13), scenB, scenAt(11, 6, 12), scenRoad, scenAt(11, 6, 15),
			scenA, scenAt(11, 7, 10), scenRoad, scenAt(11, 7, 13), scenC, scenAt(11, 11, 12), scenRoad, scenAt(11, 11, 15),
			scenA, scenAt(11, 15, 6), scenRoad, scenAt(11, 15, 8), scenB)})
	w.withHome(scenA, scenAt(10, 25, 0), scenAt(11, 2, 0))
	w.run(scenAt(11, 2, 0), scenAt(12, 12, 0))
	w.expectNoLongBlock("用户", 30)
	w.expectRegion(scenB.name)
	var history []model.InboundRegionPass
	for _, p := range w.passes {
		history = append(history, *p)
	}
	if got := autoDaysThisMonth(history, scenAt(11, 30, 23), scenLoc); got > float64(w.params.MonthlyDays) {
		w.failf("改了地区之后预支的那几天应退回额度，11 月自动放行占用 %.1f 天，超过每月 %d 天", got, w.params.MonthlyDays)
	}
}

// 坐 30 分钟快车去邻省（上海→苏州这种）：10:40 还在家里用，11:10 在 B 连上。改动前「被拦那一小时或前一小时家里
// 在用」就算另一个人：B 整个冷却期（7 天）发不出通行证，4 天的出差每天被拦约 15 小时。现在要被拦之后家里还在接着
// 用才算（2026-11-11），等家里静默满 2 小时就放行。
func TestScenarioFastTrainToNeighbourProvince(t *testing.T) {
	w := newScenWorld(t, scenUser{name: "出差的人", using: scenDaytime,
		where: legs(scenA, scenAt(11, 10, 10).Add(40*time.Minute), scenRoad, scenAt(11, 10, 11).Add(10*time.Minute), scenB,
			scenAt(11, 14, 12), scenRoad, scenAt(11, 14, 13), scenA)})
	w.withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(11, 16, 0))
	w.expectNoLongBlock("出差的人", 150)
}

// 坐 4 小时车，路上手机走流量（流量的地址判在号码归属省，也就是家里的省），14:25 到酒店连上 Wi-Fi（B）：家里「最后
// 一次在用」与第一次被拦落在同一小时。改动前同样整趟被拦；只放宽「前一小时」也救不了这一种。
func TestScenarioPhoneDataThenHotelWiFi(t *testing.T) {
	w := newScenWorld(t, scenUser{name: "出差的人", using: scenDaytime,
		where: legs(scenA, scenAt(11, 10, 14).Add(25*time.Minute), scenB, scenAt(11, 14, 12), scenRoad, scenAt(11, 14, 16), scenA)})
	w.withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(11, 16, 0))
	w.expectNoLongBlock("出差的人", 180)
}

// 防共享：号主白天在家用，外省的人每天夜里错开时间用。错峰与出差同形，只能靠「刚发不久就因家里回来而收回」
// 那道门槛压制：三周里外省的人最多拿到几个晚上，之后这个省停 30 天；地区不会被改过去。
func TestScenarioStaggeredSharerIsContained(t *testing.T) {
	night := func(t time.Time) bool { return t.Hour() >= 23 || t.Hour() < 6 }
	owner := scenUser{name: "号主", where: onlyAt(scenA), using: func(t time.Time) bool { return t.Hour() >= 8 && t.Hour() < 22 }}
	sharer := scenUser{name: "外省的人", where: onlyAt(scenB), using: night}
	w := newScenWorld(t, owner, sharer).withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(12, 1, 0))
	w.expectNoLongBlock("号主", 0)
	// 每晚最多 6 小时（00:00 家里静默满 2 小时才发）：三个晚上约 18 小时。
	if got := w.served["外省的人"]; got > 3*6*60 {
		w.failf("外省的人三周里被放行 %d 分钟，超过 3 个晚上", got)
	}
	w.expectRegion(scenA.name)
}

// 防共享（交接班式错峰）：号主 22:00 停，外省的人 22:10 接着用到早上 6 点。「本人在用时这个省也来连过」改成要求被拦
// 之后家里还在接着用（2026-11-11）以后，这一条不再挡交接班——与坐快车出门同形——只能靠短命通行证门槛压制：三周里
// 外省的人最多拿到几个晚上，之后这个省停掉；号主不受影响，地区不会被改过去。
func TestScenarioHandoverSharerIsContained(t *testing.T) {
	owner := scenUser{name: "号主", where: onlyAt(scenA), using: func(t time.Time) bool { return t.Hour() >= 8 && t.Hour() < 22 }}
	sharer := scenUser{name: "外省的人", where: onlyAt(scenB),
		using: func(t time.Time) bool { return (t.Hour() == 22 && t.Minute() >= 10) || t.Hour() >= 23 || t.Hour() < 6 }}
	w := newScenWorld(t, owner, sharer).withHome(scenA, scenAt(11, 1, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(12, 1, 0))
	w.expectNoLongBlock("号主", 0)
	// 每晚最多 6 小时（00:00 家里静默满 2 小时才发）：三个晚上约 18 小时。
	if got := w.served["外省的人"]; got > 3*6*60 {
		w.failf("外省的人三周里被放行 %d 分钟，超过 3 个晚上", got)
	}
	for _, key := range []string{"11-24", "11-25", "11-26", "11-27", "11-28", "11-29", "11-30"} {
		if w.servedDay["外省的人"][key] > 0 {
			w.failf("%s 外省的人还用得上：交接班式错峰没有被停掉，每天：%v", key, w.servedDay["外省的人"])
			return
		}
	}
	w.expectRegion(scenA.name)
}

// 防共享：号主不再用，两个外省的人一个白天、一个夜里轮流用。换省只在新的省此刻发得出通行证时才换，每周次数用完就
// 不再换，通行证留在其中一人手上，长住改地区会把他的省合法化——那是「搬家 ≡ 转手」这条已记录的取舍，与改动前一样。
// 之后另一个人就是错峰共享者，只能靠短命通行证门槛压制：改动前推演里他还能拿到 3 天左右，这里要求不多于 3 天，
// 并且最后被停掉（30 天内三张短命通行证）。
func TestScenarioTwoSharersAlternateWithoutOwner(t *testing.T) {
	day := scenUser{name: "白天的人", where: onlyAt(scenB), using: func(t time.Time) bool { return t.Hour() >= 9 && t.Hour() < 21 }}
	night := scenUser{name: "夜里的人", where: onlyAt(scenC), using: func(t time.Time) bool { return t.Hour() >= 23 || t.Hour() < 7 }}
	w := newScenWorld(t, day, night).withHome(scenA, scenAt(11, 3, 0), scenAt(11, 10, 0))
	w.run(scenAt(11, 10, 0), scenAt(12, 5, 0))
	both := 0
	for d := scenAt(11, 17, 0); d.Before(scenAt(12, 5, 0)); d = d.Add(24 * time.Hour) {
		key := d.Format("01-02")
		if w.servedDay["白天的人"][key] > 0 && w.servedDay["夜里的人"][key] > 0 {
			both++
		}
	}
	if both > 3 {
		w.failf("一周之后还有 %d 天两个外省的人都用上了（不该多于 3 天）", both)
	}
	for _, key := range []string{"12-01", "12-02", "12-03", "12-04"} {
		if w.servedDay["白天的人"][key] > 0 && w.servedDay["夜里的人"][key] > 0 {
			w.failf("%s 两个人还都用得上：错峰共享者没有被停掉", key)
			return
		}
	}
}
