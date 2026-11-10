package service

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
)

// 自动出差放行：客户出差时不会来找管理员，于是由面板在「家里没在用、外地被地区
// 规则拦下」时自动发一张只放那一个省的临时通行证。
//
// 它只**放宽**、永远有到期时间、随时可撤销，绝不自动收紧或封禁——那等于退回
// 共享检测设计里否决过的 TOFU。代价是它与错峰共享在数据上同形（家里停、外地用），
// 只能靠下面的频率上限、并存后冷却和风险等级压制，无法根除。
//
// 与「放行松紧」直接相关的六个阈值由管理员在面板设置里调（autoTravelParams）；
// 下面三个是实现细节，不对外。
const (
	autoTravelBlockedLookback = 10 * time.Minute
	autoTravelRenewWithin     = 24 * time.Hour
	// autoTravelHistoryWindow 要盖住「一个月里第二次」：两次之间最多隔 30 天、最后一次又可能是 30 天前。
	autoTravelHistoryWindow = 65 * 24 * time.Hour
	// autoTravelEscalateWindow：一个窗口里反复出现共享痕迹时，停到最后一次之后满一个窗口。
	autoTravelEscalateWindow = 30 * 24 * time.Hour
	// autoTravelShortPass：发出后不到这么久就因家里恢复使用而收回的自动通行证，算「短命」的。
	autoTravelShortPass = 24 * time.Hour
	// autoTravelObservationFresh：连接采样最近一次成功距今超过这么久，就当观测中断。采样每 30 秒一轮。
	autoTravelObservationFresh = 3 * time.Minute
	// autoTravelLiveIdle 是在线表里「此刻在用」的时间窗：最近这么久内有流量才算。固定值，不跟
	// 并发限制的闲置超时走——那个可以设成 0（永不闲置），一台一直连着的电视盒子就会永远算「家里在用」。
	autoTravelLiveIdle = 2 * time.Minute
)

// autoTravelParams 是管理员可调的阈值，来自 SettingService.GetAutoTravelParams。
type autoTravelParams struct {
	HomeSilence  time.Duration // 家里静默多久才算「人走了」
	StableDays   int           // 近 7 天里至少几天有家庭流量才算「有稳定的家」
	PassDuration time.Duration // 单张自动通行证的时长，续期也续这么长
	MonthlyDays  int           // 每个自然月自动放行的天数上限
	WeeklyGrants int           // 近 7 天自动发放次数上限
	Cooldown     time.Duration // 并存或管理员收回后暂停自动放行多久；0 表示不暂停
	// RelocateDays：家里连续这么多天没用、外地一直在用，就把地区改成外地（只在总开关开着时）。0 表示不改。
	RelocateDays int
}

// defaultAutoTravelParams 与 defaultValueMap 里的默认值一致。
func defaultAutoTravelParams() autoTravelParams {
	return autoTravelParams{
		HomeSilence: 2 * time.Hour, StableDays: 3, PassDuration: 72 * time.Hour,
		MonthlyDays: 10, WeeklyGrants: 3, Cooldown: 7 * 24 * time.Hour, RelocateDays: 5,
	}
}

const (
	authLessReason         = "入站不需要认证，扫描器的连接也会被当成被拦的用户，不自动放行"
	observationStaleReason = "观测数据中断（连接采样最近没有成功），看不到家里是否在用"
)

// AutoTravelStatus.Action 的取值。
const (
	autoTravelGranted = "granted"
	autoTravelDenied  = "denied"
	autoTravelRenewed = "renewed"
	autoTravelEnded   = "ended"
	autoTravelIdle    = "idle"
	// autoTravelRelocated：长住外地，地区已改成通行证的那个省。
	autoTravelRelocated = "relocated"
)

// AutoTravelStatus 是某入站最近一次自动放行判定的结果，经「需要留意」下发。
type AutoTravelStatus struct {
	At     int64  `json:"at"`
	Action string `json:"action"`
	Reason string `json:"reason"`
	Region string `json:"region,omitempty"`
}

var (
	autoTravelMu       sync.Mutex
	autoTravelStatuses = map[int]AutoTravelStatus{}
)

// AutoTravelStatuses 返回最近一次判定结果的副本。
func AutoTravelStatuses() map[int]AutoTravelStatus {
	autoTravelMu.Lock()
	defer autoTravelMu.Unlock()
	out := make(map[int]AutoTravelStatus, len(autoTravelStatuses))
	for k, v := range autoTravelStatuses {
		out[k] = v
	}
	return out
}

type autoTravelCandidate struct {
	IP       string
	Province string
}

// autoTravelInput 是 decideAutoTravel 的全部输入，由 AutoTravelService 组装。
type autoTravelInput struct {
	Now time.Time
	Loc *time.Location
	// GeoClosed：核心对这个入站的地区规则是锁死的（IP 归属地库不可用、也没有旧允许集），通行证放不进任何人。
	GeoClosed bool
	// HomeRows 是近 7 天、IP 落在存储的允许集里的小时行。
	HomeRows []model.InboundIPHour
	// PassRows 是近 24 小时、IP 落在活动通行证省份里（且不在家里）的小时行。
	PassRows []model.InboundIPHour
	// ServedAway 是近 7 天、在某张单省通行证生效期间落在那个省里的小时行（长住改地区的依据，见 servedAwayRows）。
	ServedAway []servedAwayRow
	// HomeLiveActive / PassLiveActive 是在线表此刻有没有活跃（非闲置）的来源。
	HomeLiveActive, PassLiveActive bool
	// Blocked 是近 10 分钟被地区规则拦下、且已验证候选省份的 IPv4 来源。
	Blocked []autoTravelCandidate
	Active  *model.InboundRegionPass
	// History 是近 35 天该入站的通行证记录。
	History   []model.InboundRegionPass
	RiskLevel string
	// RiskUnknown 表示这一轮读不出共享风险：不能当成「没有风险」。
	RiskUnknown bool
	Params      autoTravelParams
	// SuspectProvinces 是冷却窗口内「家里正在实质使用的那一小时（或前一小时）也被地区规则拦过」的省份：
	// 同一个人不可能一边在家里用、一边从外省连进来，那更像另一个人。只在即将发放时才查。
	SuspectProvinces map[string]bool
	// ObservationStale 表示连接采样最近没有成功：家里看起来「一直没在用」只是因为没采到。
	ObservationStale bool
	// AuthLess 表示入站不需要认证（socks noauth、http 无账号、dokodemo-door、ss none/plain）：
	// 扫描器的连接在访问日志里同样是「已接受」，会被当成被拦的用户。
	AuthLess bool
}

// liveSignals 判断在线表里家里与通行证省份此刻有没有真的在用的来源。
//
// 只认本次在线已传过至少 coexistMinActiveBytes 的来源：在线表对第一次见到的 IP 一律记为活跃，
// 一条只传了几百字节的连接（端口扫描、主动探测）会在接下来的闲置窗口里一直算「在用」——落在
// 家里的省就能把出差中的人的通行证收回，同时落在两边还会让自动放行暂停一周。
func liveSignals(entries []OnlineIP, home, passSet *cidrSet) (homeLive, passLive bool) {
	for _, e := range entries {
		if e.Idle || e.Conns == 0 || e.Up+e.Down < coexistMinActiveBytes {
			continue
		}
		if home.Contains(e.IP) {
			homeLive = true
		} else if passSet.Contains(e.IP) {
			passLive = true
		}
	}
	return
}

type autoTravelDecision struct {
	Action    string
	Reason    string
	Province  string
	Until     int64
	EndReason string
}

func hourStartOf(t time.Time) int64 {
	s := t.Unix()
	return s - s%3600
}

// homeActiveSince 报告家里自 since 所在小时起有没有实质流量的小时行。
func homeActiveSince(rows []model.InboundIPHour, since time.Time) bool {
	from := hourStartOf(since)
	for _, r := range rows {
		if r.HourStart >= from && r.ActiveBytes >= coexistMinActiveBytes {
			return true
		}
	}
	return false
}

// hoursText 把时长写成「N 小时」或「N 天」，给判定原因用。
func hoursText(d time.Duration) string {
	h := int(d / time.Hour)
	if h >= 24 && h%24 == 0 {
		return fmt.Sprintf("%d 天", h/24)
	}
	return fmt.Sprintf("%d 小时", h)
}

func hasSubstantialRow(rows []model.InboundIPHour) bool {
	for _, r := range rows {
		if r.ActiveBytes >= coexistMinActiveBytes {
			return true
		}
	}
	return false
}

func stableHomeDays(rows []model.InboundIPHour, loc *time.Location) int {
	days := map[string]bool{}
	for _, r := range rows {
		if r.ActiveBytes >= coexistMinActiveBytes {
			days[time.Unix(r.HourStart, 0).In(loc).Format("2006-01-02")] = true
		}
	}
	return len(days)
}

// autoDaysThisMonth 是本自然月（面板时区）内自动通行证实际生效的累计天数。
func autoDaysThisMonth(history []model.InboundRegionPass, now time.Time, loc *time.Location) float64 {
	n := now.In(loc)
	monthStart := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc).UnixMilli()
	nowMs := now.UnixMilli()
	var total int64
	for _, p := range history {
		// relocated：改了地区之后这几张算「住在那里」，不是出差，不占本月额度。
		if !p.Auto || p.EndReason == model.RegionPassRelocated {
			continue
		}
		end := p.EndedAt
		if end == 0 {
			end = min(p.Until, nowMs)
		}
		start := max(p.GrantedAt, monthStart)
		end = min(end, nowMs)
		if end > start {
			total += end - start
		}
	}
	return float64(total) / float64(24*time.Hour/time.Millisecond)
}

// passDurationLeft 是这一次发放或续期能给的时长：单张时长，但不超过本月剩下的自动放行额度。
// 每月天数是上限，不是「低于上限就再给一整张」——改动前用了 9.99 天还能再拿 3 天，单张时长
// 设成 30 天的话一张就是一整个月。
func passDurationLeft(in autoTravelInput, p autoTravelParams) time.Duration {
	left := float64(p.MonthlyDays) - autoDaysThisMonth(in.History, in.Now, in.Loc)
	if left <= 0 {
		return 0
	}
	return min(p.PassDuration, time.Duration(left*float64(24*time.Hour)))
}

// decideAutoTravel 是自动放行的全部判定，纯函数。
func decideAutoTravel(in autoTravelInput) autoTravelDecision {
	now := in.Now
	p := in.Params
	if in.Active != nil {
		if !in.Active.Auto {
			return autoTravelDecision{Action: autoTravelIdle, Reason: "已有手动通行证"}
		}
		if in.AuthLess {
			// 通行证打开的是整个省：在一个不需要认证的入站上，等于给那个省开了一个公开代理。
			return autoTravelDecision{Action: autoTravelEnded, Reason: authLessReason + "，收回自动通行证",
				EndReason: model.RegionPassAutoOff}
		}
		if in.HomeLiveActive || homeActiveSince(in.HomeRows, now.Add(-time.Hour)) {
			reason := model.RegionPassHome
			text := "家里恢复使用，收回通行证"
			if in.HomeLiveActive && in.PassLiveActive {
				reason = model.RegionPassCoexist
				text = "家里与外地同时在用，疑似共享，收回通行证"
				if p.Cooldown > 0 {
					text += "并暂停自动放行 " + hoursText(p.Cooldown)
				}
			}
			return autoTravelDecision{Action: autoTravelEnded, Reason: text, EndReason: reason}
		}
		// 续期窗口不超过通行证时长的一半：固定 24 小时的话，短于一天的通行证每分钟都在续。
		renewWithin := min(autoTravelRenewWithin, p.PassDuration/2)
		if in.Active.Until-now.UnixMilli() < renewWithin.Milliseconds() && hasSubstantialRow(in.PassRows) {
			switch {
			case in.ObservationStale:
				// 采样断了，家里回来了也看不见，上面那道「家里恢复使用就收回」就失效了：不续期，
				// 让它按时到期。
				return autoTravelDecision{Action: autoTravelIdle, Reason: observationStaleReason + "，不续期"}
			case in.RiskUnknown || in.RiskLevel == RiskLevelHigh || in.RiskLevel == RiskLevelSevere:
				return autoTravelDecision{Action: autoTravelIdle, Reason: "共享风险偏高或读不出来，不再续期"}
			}
			if d := passDurationLeft(in, p); d > 0 {
				return autoTravelDecision{Action: autoTravelRenewed, Reason: "家里仍未使用，外地仍在用，续期",
					Until: now.Add(d).UnixMilli()}
			}
			return autoTravelDecision{Action: autoTravelIdle, Reason: fmt.Sprintf("本月自动放行已满 %d 天，不再续期", p.MonthlyDays)}
		}
		return autoTravelDecision{Action: autoTravelIdle, Reason: "自动通行证生效中"}
	}
	if len(in.Blocked) == 0 {
		return autoTravelDecision{Action: autoTravelIdle, Reason: "近 10 分钟没有被地区规则拦下的来源"}
	}
	deny := func(reason string) autoTravelDecision {
		return autoTravelDecision{Action: autoTravelDenied, Reason: reason}
	}
	switch {
	case in.GeoClosed:
		return deny("地区规则锁死中（IP 归属地库不可用），通行证不起作用，不自动放行")
	case in.AuthLess:
		return deny(authLessReason)
	case in.ObservationStale:
		return deny(observationStaleReason + "，本轮不自动放行")
	case in.RiskUnknown:
		return deny("读不出共享风险，本轮不自动放行")
	case in.RiskLevel == RiskLevelHigh || in.RiskLevel == RiskLevelSevere:
		return deny("共享风险等级偏高，不自动放行")
	case stableHomeDays(in.HomeRows, in.Loc) < p.StableDays:
		return deny(fmt.Sprintf("还没有稳定的常用地区（近 7 天少于 %d 天的使用记录）", p.StableDays))
	case in.HomeLiveActive || homeActiveSince(in.HomeRows, now.Add(-p.HomeSilence)):
		return deny(fmt.Sprintf("家里 %s内仍在使用，外地来源不像出差", hoursText(p.HomeSilence)))
	}
	weekAgo := now.Add(-7 * 24 * time.Hour).UnixMilli()
	grants := 0
	cooldownFrom := now.Add(-p.Cooldown).UnixMilli()
	cooldownText := hoursText(p.Cooldown)
	if p.Cooldown > 0 {
		// 一次并存可能是出差回来那一刻的重叠，冷却一段时间就够；一个月里第二次就不是巧合了。
		// 停到第二次之后满 30 天，自动恢复，不需要管理员再去打开。
		var coexistEnds []int64
		for _, h := range in.History {
			if h.EndReason == model.RegionPassCoexist {
				coexistEnds = append(coexistEnds, h.EndedAt)
			}
		}
		if until := escalatedUntil(coexistEnds, 2); until > now.UnixMilli() {
			return deny(fmt.Sprintf("%s内两次发现家里与外地同时在用，暂停自动放行到 %s",
				hoursText(autoTravelEscalateWindow), time.UnixMilli(until).In(in.Loc).Format("01-02 15:04")))
		}
	}
	for _, h := range in.History {
		if p.Cooldown > 0 && h.EndReason == model.RegionPassCoexist && h.EndedAt >= cooldownFrom {
			return deny(cooldownText + "内发现过家里与外地同时在用，暂停自动放行")
		}
		// 管理员收回过通行证（不论是不是自动发的），说明他此刻不想放行。不冷却的话，被拦记录与
		// 「家里静默」都还在，下一分钟就会原样发回去——收回手动通行证也一样：改动前只认自动的，
		// 管理员发了手动通行证（顶掉自动的）再收回，一分钟后又被自动放行顶回来。
		if p.Cooldown > 0 && h.EndReason == model.RegionPassRevoked && h.EndedAt >= cooldownFrom {
			return deny(cooldownText + "内管理员收回过通行证，暂停自动放行")
		}
		if h.Auto && h.GrantedAt >= weekAgo {
			grants++
		}
	}
	if grants >= p.WeeklyGrants {
		return deny(fmt.Sprintf("一周内已自动放行 %d 次，频繁切换不像出差", grants))
	}
	if autoDaysThisMonth(in.History, now, in.Loc) >= float64(p.MonthlyDays) {
		return deny(fmt.Sprintf("本月自动放行已满 %d 天", p.MonthlyDays))
	}
	d := passDurationLeft(in, p)
	if d <= 0 {
		return deny(fmt.Sprintf("本月自动放行已满 %d 天", p.MonthlyDays))
	}
	firstRefusal := ""
	for _, c := range in.Blocked {
		if c.Province == "" {
			continue
		}
		if reason := provinceRefusal(in, c.Province); reason != "" {
			if firstRefusal == "" {
				firstRefusal = reason
			}
			continue
		}
		return autoTravelDecision{Action: autoTravelGranted,
			Reason:   fmt.Sprintf("家里 %s未使用，外地被拦，自动放行", hoursText(p.HomeSilence)),
			Province: c.Province, Until: now.Add(d).UnixMilli()}
	}
	if firstRefusal != "" {
		return deny(firstRefusal)
	}
	return deny("被拦来源查不到可放行的省份（境外、IPv6，或各归属地库判断不一），不自动放行")
}

// 发放前按省份判断的两道防共享门槛。冷却设为 0（不暂停）时一起关闭。
//
// 错峰共享与出差在网络层同形（家里停、外地用），共享风险评分对它最多给到「中」，挡不住发放；
// 这两条看的是只有「两个人」才会留下的痕迹：
//   - 家里正在用的时候，这个省也来连过（被地区规则拦下）：一个人不可能同时在两个地方；
//   - 这个省的自动通行证反复刚发出不久，就因为家里恢复使用而收回：真出差至少要在外地待上一阵，
//     白天本人用、夜里另一个人用，才会一次次在几个小时后被收回。
//
// 代价：手机走流量显示家里省、笔记本连外地 Wi-Fi 的那一趟，以及一周两次当天往返同一个邻省的人，
// 会在冷却期内拿不到这个省的自动通行证。
func provinceRefusal(in autoTravelInput, province string) string {
	p := in.Params
	if p.Cooldown <= 0 {
		return ""
	}
	if in.SuspectProvinces[province] {
		return fmt.Sprintf("%s的来源在家里正在用的时候也来连过，像另一个人，%s内不对这个省自动放行",
			province, hoursText(p.Cooldown))
	}
	nowMs := in.Now.UnixMilli()
	var ends []int64
	recent := 0
	for _, h := range in.History {
		if !h.Auto || h.EndReason != model.RegionPassHome || h.EndedAt-h.GrantedAt >= autoTravelShortPass.Milliseconds() {
			continue
		}
		if passProvince(h) != province {
			continue
		}
		ends = append(ends, h.EndedAt)
		if h.EndedAt >= nowMs-p.Cooldown.Milliseconds() {
			recent++
		}
	}
	if until := escalatedUntil(ends, 3); until > nowMs {
		return fmt.Sprintf("%s的自动通行证 %s内三次刚发出不久就因家里恢复使用而收回，暂停对这个省自动放行到 %s",
			province, hoursText(autoTravelEscalateWindow), time.UnixMilli(until).In(in.Loc).Format("01-02 15:04"))
	}
	if recent >= 2 {
		return fmt.Sprintf("%s的自动通行证 %s内两次刚发出不久就因家里恢复使用而收回，像两个人错开时间用，暂停对这个省自动放行",
			province, hoursText(p.Cooldown))
	}
	return ""
}

// escalatedUntil：时刻列表里只要有 n 次落在同一个 autoTravelEscalateWindow 之内，就停到其中最后一次之后
// 满一个窗口（毫秒）；取最晚的那一组。没有这样的一组时返回 0。
func escalatedUntil(ends []int64, n int) int64 {
	if len(ends) < n {
		return 0
	}
	sorted := append([]int64(nil), ends...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	window := autoTravelEscalateWindow.Milliseconds()
	for i := len(sorted) - 1; i >= n-1; i-- {
		if sorted[i]-sorted[i-n+1] <= window {
			return sorted[i] + window
		}
	}
	return 0
}

// passProvince 返回单省通行证的省份；多省或暂停地区规则的通行证返回空串。
func passProvince(p model.InboundRegionPass) string {
	regions, err := DecodeRegions(p.Regions)
	if err != nil || len(regions) != 1 || regions[0] == model.RegionPassAll {
		return ""
	}
	return regions[0]
}

// AutoTravelService 组装输入、执行判定。无状态，判定结果写进包级状态。
type AutoTravelService struct {
	inboundService    InboundService
	settingService    SettingService
	regionPassService RegionPassService
}

// Run 跑一轮自动放行，返回是否改了通行证（调用方据此置重启标志）。
func (s *AutoTravelService) Run(now time.Time) (bool, error) {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return false, err
	}
	var targets []*model.Inbound
	for _, in := range inbounds {
		if in.Enable && in.AutoTravel && model.HasRegions(in.Regions) {
			targets = append(targets, in)
		}
	}
	next := map[int]AutoTravelStatus{}
	defer func() {
		autoTravelMu.Lock()
		autoTravelStatuses = next
		autoTravelMu.Unlock()
	}()
	if len(targets) == 0 {
		return false, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return false, err
	}
	passes, err := s.regionPassService.Active()
	if err != nil {
		return false, err
	}
	risk, err := (&SharingRiskService{}).Summary()
	riskUnknown := err != nil
	if err != nil {
		logger.Warning("自动出差放行：读取共享风险失败，本轮不发放、不续期:", err)
	}
	params := s.settingService.GetAutoTravelParams()
	masterOn, _ := s.settingService.GetAutoRegionParams()
	logEnabled, _ := s.settingService.GetAccessLogEnable()
	matcher := newGeoBlockMatcherWith(inbounds, passes, allowedSetFor)

	changed := false
	for _, in := range targets {
		status := AutoTravelStatus{At: now.UnixMilli()}
		input, reason := s.assemble(in, passes[in.Id], matcher, logEnabled, now, loc, params.Cooldown)
		if reason != "" {
			status.Action, status.Reason = autoTravelDenied, reason
			next[in.Id] = status
			continue
		}
		input.RiskLevel = risk[in.Id].Level
		input.RiskUnknown = riskUnknown
		input.Params = params
		// 入站打开了「不自动设置地区限制」：长住外地也不改地区，只按出差处理。
		if masterOn && params.RelocateDays > 0 && !in.NoAutoRegion {
			// 通行证到期了也要判：额度先用完、通行证没撑到第 N 天的人，凭通行证期间的实质使用照样改。
			st, stErr := (&AutoRegionService{}).stateOf(in.Id)
			rd := relocateDecision{}
			if stErr != nil {
				// 读不出状态行就判断不了「30 天内只改去一个新省」：这一轮不改。
				logger.Warning("长住改地区：读取自动设置状态失败, 入站", in.Id, ":", stErr)
			} else {
				rd = decideRelocation(relocateInput{Now: now, Loc: loc, Enabled: masterOn, Days: params.RelocateDays,
					Active: input.Active, HomeRows: input.HomeRows, HomeLiveActive: input.HomeLiveActive,
					ServedAway: input.ServedAway, History: input.History, State: st,
					RiskLevel: input.RiskLevel, RiskUnknown: riskUnknown,
					ObservationStale: input.ObservationStale, AuthLess: input.AuthLess})
			}
			if rd.Province != "" {
				ok, err := relocate(in, input.Active, rd.Province, rd.Reason, now)
				switch {
				case err != nil:
					logger.Warning("长住改地区失败, 入站", in.Id, ":", err)
				case ok:
					changed = true
					logger.Info("长住改地区：入站", in.Id, rd.Reason)
					status.Action, status.Reason, status.Region = autoTravelRelocated, rd.Reason, rd.Province
					next[in.Id] = status
					continue
				}
				// 没改成（管理员刚改过地区或收回了通行证）：照常走下面的判定。
			}
		}
		d := decideAutoTravel(input)
		if d.Action == autoTravelIdle && !logEnabled && input.Active == nil {
			d = autoTravelDecision{Action: autoTravelDenied, Reason: "访问日志未开启，无法发现被拦的来源"}
		}
		status.Action, status.Reason, status.Region = d.Action, d.Reason, d.Province
		switch d.Action {
		case autoTravelGranted:
			if _, err := s.regionPassService.GrantAuto(in.Id, d.Province, d.Until, now); err != nil {
				status.Action, status.Reason = autoTravelDenied, "发放失败: "+err.Error()
			} else {
				changed = true
				logger.Info("自动出差放行：入站", in.Id, "放行", d.Province, "至", time.UnixMilli(d.Until).In(loc).Format("01-02 15:04"))
			}
		case autoTravelRenewed:
			if renewed, err := s.regionPassService.Renew(in.Id, input.Active.Id, d.Until); err != nil {
				status.Action, status.Reason = autoTravelDenied, "续期失败: "+err.Error()
			} else if !renewed {
				status.Action, status.Reason = autoTravelIdle, "通行证已经结束，没有续期"
			}
		case autoTravelEnded:
			if ended, err := s.regionPassService.RevokePass(in.Id, input.Active.Id, d.EndReason, now); err != nil {
				status.Action, status.Reason = autoTravelDenied, "收回失败: "+err.Error()
			} else if ended {
				changed = true
				logger.Info("自动出差放行：入站", in.Id, d.Reason)
			}
		}
		next[in.Id] = status
	}
	return changed, nil
}

// assemble 组装 decideAutoTravel 的输入；返回非空 reason 表示本轮无法判断。
func (s *AutoTravelService) assemble(in *model.Inbound, active *model.InboundRegionPass, matcher *geoBlockMatcher,
	logEnabled bool, now time.Time, loc *time.Location, cooldown time.Duration) (autoTravelInput, string) {
	input := autoTravelInput{Now: now, Loc: loc, Active: active,
		AuthLess: !inboundRequiresAuth(in), ObservationStale: !observationFresh(now)}
	stored, err := DecodeRegions(in.Regions)
	if err != nil {
		return input, "地区数据损坏"
	}
	home, err := allowedSetFor(stored)
	if err != nil {
		return input, "IP 归属地库未加载"
	}
	var passSet *cidrSet
	if active != nil {
		if regions, err := DecodeRegions(active.Regions); err == nil && !(len(regions) == 1 && regions[0] == model.RegionPassAll) {
			passSet, _ = allowedSetFor(regions)
		}
	}
	rows, err := (&SharingService{}).windowRows(in.Id, 7, now)
	if err != nil {
		return input, "读取用量记录失败"
	}
	dayAgo := now.Add(-24 * time.Hour).Unix()
	for _, r := range rows {
		if home.Contains(r.IP) {
			input.HomeRows = append(input.HomeRows, r)
		} else if passSet.Contains(r.IP) && r.HourStart >= dayAgo {
			input.PassRows = append(input.PassRows, r)
		}
	}
	input.HomeLiveActive, input.PassLiveActive = liveSignals(
		onlineTrackerInstance.snapshotIdle(in.Port, noLocate, autoTravelLiveIdle), home, passSet)
	input.GeoClosed = GeoDegradedInbounds()[in.Id].Mode == geoDegradeClosed
	if logEnabled && active == nil {
		input.Blocked = dropStoredCandidates(recentGeoBlocked(in.Id, matcher, now), home)
		if !idcExempt(in) {
			input.Blocked = dropIDCCandidates(input.Blocked, idcBlockedSet(&SettingService{}, &IPDBService{}))
		}
		if len(input.Blocked) > 0 && cooldown > 0 {
			// 只在可能发放时才查：一周的被拦记录按 IP×小时聚合，量不大，但没必要每分钟每个入站都查。
			// 判据要的家里小时行只有近 7 天，冷却设得更长也只看 7 天。
			since := now.Add(-min(cooldown, time.Duration(sharingWindowDays)*24*time.Hour))
			blocked, err := blockedHoursSince(in.Id, since, func(ip string) bool { return matcher.Blocks(in.Id, ip) })
			if err != nil {
				return input, "读取被拦记录失败"
			}
			sets := map[string]*cidrSet{}
			input.SuspectProvinces = suspectProvinces(blocked, rows, home, func(ip string) string {
				return enforceableProvince(ip, provinceOfIP(ip), allowedSetFor, sets)
			})
		}
	}
	history, err := s.regionPassService.Since(in.Id, now.Add(-autoTravelHistoryWindow).UnixMilli())
	if err != nil {
		return input, "读取通行证记录失败"
	}
	input.History = history
	input.ServedAway = servedAwayRows(rows, home, history, allowedSetFor, now)
	return input, ""
}

// dropStoredCandidates 去掉落在存储地区里的来源：锁死的入站对所有来源都判「被拦」，家里的来源也会在里面，
// 给它们所在的省（就是家）发通行证什么也放不进来，只白占每周次数与每月天数。
func dropStoredCandidates(cands []autoTravelCandidate, home *cidrSet) []autoTravelCandidate {
	out := cands[:0:0]
	for _, c := range cands {
		if !home.Contains(c.IP) {
			out = append(out, c)
		}
	}
	return out
}

// dropIDCCandidates 去掉机房来源（「拦截机房 / 云服务器来源」开着时 idc 非 nil）：它们被那条规则挡着，
// 给所在的省发通行证也放不进来。
func dropIDCCandidates(cands []autoTravelCandidate, idc *cidrSet) []autoTravelCandidate {
	if idc == nil {
		return cands
	}
	out := cands[:0:0]
	for _, c := range cands {
		if !idc.Contains(c.IP) {
			out = append(out, c)
		}
	}
	return out
}

// recentGeoBlocked 取近 autoTravelBlockedLookback 内被地区规则拦下的 IPv4 来源，
// 并验证候选省份：按执行同口径的允许集，这个 IP 确实落在该省里才算。访问日志里
// 带路由结果的行已经过协议鉴权，扫描器不会出现在这里。
func recentGeoBlocked(inboundId int, matcher *geoBlockMatcher, now time.Time) []autoTravelCandidate {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil
	}
	var ips []string
	err := db.Model(&model.AccessLog{}).Distinct("source_ip").
		Where("inbound_id = ? and time >= ? and route = ? and accepted = ?",
			inboundId, now.Add(-autoTravelBlockedLookback).UnixMilli(), model.BlockOutboundTag, true).
		Order("source_ip").Pluck("source_ip", &ips).Error
	if err != nil {
		logger.Warning("自动出差放行：读取访问日志失败:", err)
		return nil
	}
	var out []autoTravelCandidate
	sets := map[string]*cidrSet{}
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil || !matcher.Blocks(inboundId, ip) {
			continue
		}
		out = append(out, autoTravelCandidate{IP: ip, Province: enforceableProvince(ip, provinceOfIP(ip), allowedSetFor, sets)})
	}
	return out
}

// sharingObservedAt 是连接采样最近一次成功（采到并写进库）的时刻，毫秒。0 表示从没成功过。
var sharingObservedAt atomic.Int64

// markObserved 在 SharingService.Sample 成功时调用。库没打开、平台不支持、采样或写库失败都不算。
func markObserved(now time.Time) { sharingObservedAt.Store(now.UnixMilli()) }

// observationFresh 报告观测是否在正常进行。采样断了，家里看起来就是「一直没在用」，被拦的人会被
// 当成出差放行；生效中的通行证在家里回来时也收不回。
func observationFresh(now time.Time) bool {
	last := sharingObservedAt.Load()
	return last != 0 && now.UnixMilli()-last <= autoTravelObservationFresh.Milliseconds()
}

// blockedHour 是访问日志里一个来源在某个整点小时被地区规则拦过。
type blockedHour struct {
	IP     string
	HourMs int64
}

// blockedHoursSince 取 since 以来进了黑洞、且确实是被地区规则拦下（geo 为真）的来源，按 IP×小时去重。
// 带路由结果的行已经过协议鉴权。geo 为 nil 时一行都不算。
func blockedHoursSince(inboundId int, since time.Time, geo func(ip string) bool) ([]blockedHour, error) {
	db := database.GetAccessLogDB()
	if db == nil || geo == nil {
		return nil, nil
	}
	rows, err := db.Model(&model.AccessLog{}).Select("source_ip, time / 3600000 AS h").
		Where("inbound_id = ? AND time >= ? AND route = ? AND accepted = ?",
			inboundId, since.UnixMilli(), model.BlockOutboundTag, true).
		Group("source_ip, h").Order("source_ip, h").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blockedHour
	for rows.Next() {
		var ip string
		var h int64
		if err := rows.Scan(&ip, &h); err != nil {
			return nil, err
		}
		if geo(ip) {
			out = append(out, blockedHour{IP: ip, HourMs: h * 3600000})
		}
	}
	return out, rows.Err()
}

// suspectProvinces 是「家里那一小时（或前一小时）在实质使用、这个来源同时被地区规则拦下」的来源所在省份，
// 判据与「需要留意」的「疑似共享」标注（classifyBlocked）相同。provinceOf 返回空串的来源不计。
func suspectProvinces(blocked []blockedHour, rows []model.InboundIPHour, home *cidrSet, provinceOf func(ip string) string) map[string]bool {
	out := map[string]bool{}
	for _, b := range blocked {
		if classifyBlocked(b.HourMs, rows, home) != geoBlockedCoexist {
			continue
		}
		if p := provinceOf(b.IP); p != "" {
			out[p] = true
		}
	}
	return out
}
