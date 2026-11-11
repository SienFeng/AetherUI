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
	// autoTravelMoved：换了地方，同一轮里以 moved 结束旧通行证、给 Province 发一张到 Until 的新通行证。
	autoTravelMoved = "moved"
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
	// SuspectProvinces 是冷却窗口内在家里一段实质使用的中间（homeInUseAround）也被地区规则拦过的省份：
	// 同一个人不可能一边在家里用、一边从外省连进来，那更像另一个人。只在即将发放时才查。
	SuspectProvinces map[string]bool
	// ObservationStale 表示连接采样最近没有成功：家里看起来「一直没在用」只是因为没采到。
	ObservationStale bool
	// AuthLess 表示入站不需要认证（socks noauth、http 无账号、dokodemo-door、ss none/plain）：
	// 扫描器的连接在访问日志里同样是「已接受」，会被当成被拦的用户。
	AuthLess bool
	// Recent 惰性取近 24 小时家里之外被地区规则拦下的来源动静，给长住改地区确认人在哪（relocateInput.Recent）。
	// 访问日志关着时为 nil。
	Recent func() ([]recentSource, error)
	// Relocation 非 nil 表示长住改地区会对这个入站生效（总开关开、长住天数 > 0、入站没排除），决定要不要预支额度。
	Relocation *autoTravelRelocation
}

// autoTravelRelocation 是预支额度要用到的长住改地区设置与这个入站的自动设置状态行（可能为 nil）。
type autoTravelRelocation struct {
	Days  int
	State *model.InboundAutoRegion
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

// relocationBorrowMargin：预支到长住判定点之后再多给的余量——长住判定一分钟一轮，判定点到了也要等这一轮。
const relocationBorrowMargin = time.Hour

// relocationBorrow 是本月额度撑不到长住改地区判定点时，这张通行证可以预支的时长（2026-11-10 管理员选定）；
// 不预支时为 0。
//
// 搬家的人额度只剩 1~2 天时，通行证撑不到判定那天，在新地方攒不够 min(3, N) 天被放行的实质使用：之后先因额度满、
// 再因「近 7 天在家里用过」不满足被一直拦着，长住改地区也没有依据，不会自愈。所以在长住改地区对这个省可能成立时
// （总开关开、入站需要认证、风险评分已就绪且低于「中」、这个省最近一张通行证没被管理员收回、没碰上「30 天内只改去
// 一个新省」），通行证可以超出本月额度，给到判定点为止：家里连续静默满 N 天的那一刻，与这个省攒够 min(3, N) 天的
// 那一刻（按此后每天在用估算）取较晚者，再加余量。改了地区之后这些天算「住在那里」、额度照常退回
// （relabelRelocatedPasses）；人提前回家没改成的话才真的多占——本月总占用封顶在「每月天数 + 长住天数」。
// 判定点已经过了（条件都满足却没改成，多半是人不在那里了）不预支。
func relocationBorrow(in autoTravelInput, province string, used float64) time.Duration {
	rc := in.Relocation
	if rc == nil || rc.Days <= 0 || province == "" || in.AuthLess || in.RiskUnknown || !relocationRiskOK(in.RiskLevel) {
		return 0
	}
	if lastPassRevoked(in.History, province) || recentlyRelocatedElsewhere(rc.State, province, in.Now) {
		return 0
	}
	window := time.Duration(rc.Days) * 24 * time.Hour
	point := in.Now
	var lastHome int64
	for _, r := range in.HomeRows {
		if r.ActiveBytes >= coexistMinActiveBytes && r.HourStart > lastHome {
			lastHome = r.HourStart
		}
	}
	if lastHome > 0 {
		// homeActiveSince(HomeRows, 此刻−N 天) 在「此刻−N 天」所在的小时晚于最后一次实质使用那一小时时才为假。
		point = time.Unix(lastHome+3600, 0).Add(window)
	}
	need := min(3, rc.Days)
	if served := servedDays(in.ServedAway, province, in.Now.Add(-window), in.Loc); served < need {
		if enough := in.Now.Add(time.Duration(need-served) * 24 * time.Hour); enough.After(point) {
			point = enough
		}
	}
	// 长住要求近 N 天里只有这一个省被放行过：之前待过的省（换省结束的那张）也要滑出窗口，否则到了终点还是改不了
	// （代码审查指出）。别的省在这个省开始用之后还被放行过，是两地交替（通勤、两个人），不是搬家，长住改不了，不预支。
	var targetFirst, otherLast int64
	for _, r := range in.ServedAway {
		if r.ActiveBytes < coexistMinActiveBytes {
			continue
		}
		if r.Province == province {
			if targetFirst == 0 || r.HourStart < targetFirst {
				targetFirst = r.HourStart
			}
		} else if r.HourStart > otherLast {
			otherLast = r.HourStart
		}
	}
	if otherLast > 0 {
		if targetFirst > 0 && otherLast > targetFirst {
			return 0
		}
		if out := time.Unix(otherLast+3600, 0).Add(window); out.After(point) {
			point = out
		}
	}
	if !point.After(in.Now) {
		return 0
	}
	borrow := point.Add(relocationBorrowMargin).Sub(in.Now)
	limit := time.Duration((float64(in.Params.MonthlyDays+rc.Days) - used) * float64(24*time.Hour))
	return max(0, min(borrow, limit))
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
		if cands := moveOnCandidates(in, passInUse(in)); len(cands) > 0 {
			// 换过去之前确认新的省此刻发得出通行证（发放的全部门槛），发不出就保留手上这张：只换不看的话，一个人
			// 两台设备分属两个省交替闲置，来回换两次撞上每周次数，旧的已结束、新的发不出，两台都被拦（推演复现过）。
			probe := in
			probe.Active, probe.Blocked = nil, cands
			if g := decideGrant(probe); g.Action == autoTravelGranted {
				// 收回与发放在同一轮（Run 先收回再发）：留到下一轮的话，候选按来源 IP 的顺序挑，旧省还有设备在连时
				// 会被原样发回旧省（推演复现过）。
				return autoTravelDecision{Action: autoTravelMoved, EndReason: model.RegionPassMoved,
					Province: g.Province, Until: g.Until,
					Reason: fmt.Sprintf("%s %s没有使用，%s的来源在被拦，换了地方，通行证换成%s",
						passProvince(*in.Active), hoursText(moveOnIdle(in, g.Province)), g.Province, g.Province)}
			}
		}
		// 续期窗口不超过通行证时长的一半：固定 24 小时的话，短于一天的通行证每分钟都在续。
		renewWithin := min(autoTravelRenewWithin, p.PassDuration/2)
		if in.Active.Until-now.UnixMilli() < renewWithin.Milliseconds() {
			switch {
			case in.ObservationStale:
				// 采样断了，家里回来了也看不见，上面那道「家里恢复使用就收回」就失效了：不续期，
				// 让它按时到期。
				return autoTravelDecision{Action: autoTravelIdle, Reason: observationStaleReason + "，不续期"}
			case !in.PassLiveActive && !hasSubstantialRow(in.PassRows):
				// 24 小时内用过就续，容忍一天左右不上线：曾改成「家里静默时长内仍在用」，出差中途歇一天的人通行证就断了，
				// 而那时「近 7 天在家用过」已不满足、发不出新的。人走了还续的害处由换了地方（moveOnCandidates）、家里恢复使用就收回、
				// 长住的「人还在那」兜着。
				return autoTravelDecision{Action: autoTravelIdle, Reason: passProvince(*in.Active) + " 24 小时内没有实质使用，不续期"}
			case in.RiskUnknown || in.RiskLevel == RiskLevelHigh || in.RiskLevel == RiskLevelSevere:
				return autoTravelDecision{Action: autoTravelIdle, Reason: "共享风险偏高或读不出来，不再续期"}
			}
			// left 已被单张时长截过：给出的时长 d 比它长，才是真的超出了本月额度（预支）。
			left := passDurationLeft(in, p)
			borrow := relocationBorrow(in, passProvince(*in.Active), autoDaysThisMonth(in.History, now, in.Loc))
			if d := min(p.PassDuration, max(left, borrow)); d > 0 {
				until := now.Add(d).UnixMilli()
				if until <= in.Active.Until+time.Minute.Milliseconds() {
					// 到期时刻已被本月额度（或预支的终点）钉住，续期窗口里只有它们能让到期时刻不后移：改动前这里
					// 每一轮都报「续期」、写一次库，管理员看到的是「续期」，其实它马上就要结束。
					at := time.UnixMilli(in.Active.Until).In(in.Loc).Format("01-02 15:04")
					if d > left {
						return autoTravelDecision{Action: autoTravelIdle, Reason: "已预支额度到 " + at + "（长住改地区的判定点或本月预支上限），届时结束"}
					}
					return autoTravelDecision{Action: autoTravelIdle, Reason: "本月自动放行额度将于 " + at + " 用完，届时结束"}
				}
				reason := "家里仍未使用，外地仍在用，续期"
				if d > left {
					reason += "（本月额度撑不到长住改地区判定，预支）"
				}
				return autoTravelDecision{Action: autoTravelRenewed, Reason: reason, Until: until}
			}
			return autoTravelDecision{Action: autoTravelIdle, Reason: fmt.Sprintf("本月自动放行已满 %d 天，不再续期", p.MonthlyDays)}
		}
		return autoTravelDecision{Action: autoTravelIdle, Reason: "自动通行证生效中"}
	}
	return decideGrant(in)
}

// decideGrant 是没有生效中的通行证时的发放判定；换了地方时也拿它试探新的省此刻发不发得出（moveOnCandidates 之后）。
func decideGrant(in autoTravelInput) autoTravelDecision {
	now := in.Now
	p := in.Params
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
	// 管理员收回后的暂停至少一张通行证的时长，暂停天数设成 0 也一样：那个 0 关掉的是共享痕迹的判断，
	// 不是管理员亲手做的决定——改动前 0 时收回后下一分钟就原样发回，只有每周次数上限挡得住。
	revokePause := max(p.Cooldown, p.PassDuration)
	revokeFrom := now.Add(-revokePause).UnixMilli()
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
		if h.EndReason == model.RegionPassRevoked && h.EndedAt >= revokeFrom {
			return deny(hoursText(revokePause) + "内管理员收回过通行证，暂停自动放行")
		}
		if h.Auto && h.GrantedAt >= weekAgo {
			grants++
		}
	}
	if grants >= p.WeeklyGrants {
		return deny(fmt.Sprintf("一周内已自动放行 %d 次，频繁切换不像出差", grants))
	}
	monthFull := fmt.Sprintf("本月自动放行已满 %d 天", p.MonthlyDays)
	used := autoDaysThisMonth(in.History, now, in.Loc)
	left := passDurationLeft(in, p)
	if left <= 0 && in.Relocation == nil {
		return deny(monthFull) // 不可能预支（长住改地区不会发生）
	}
	firstRefusal := ""
	full := false
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
		borrow := relocationBorrow(in, c.Province, used)
		d := min(p.PassDuration, max(left, borrow))
		if d <= 0 {
			full = true
			continue
		}
		until := now.Add(d)
		reason := fmt.Sprintf("家里 %s未使用，外地被拦，自动放行", hoursText(p.HomeSilence))
		if d > left { // left 已被单张时长截过：比它长才是真的超出了本月额度
			reason += "（本月额度撑不到长住改地区判定，预支到 " + until.In(in.Loc).Format("01-02 15:04") + "）"
		}
		return autoTravelDecision{Action: autoTravelGranted, Reason: reason, Province: c.Province, Until: until.UnixMilli()}
	}
	if full {
		return deny(monthFull)
	}
	if firstRefusal != "" {
		return deny(firstRefusal)
	}
	return deny("被拦来源查不到可放行的省份（境外、IPv6，或各归属地库判断不一），不自动放行")
}

// passInUse：通行证的省「此刻仍在用」——在线在用，或家里静默时长内有实质使用（PassRows 只含这个省、不在家里的行）。
func passInUse(in autoTravelInput) bool {
	return in.PassLiveActive || homeActiveSince(in.PassRows, in.Now.Add(-in.Params.HomeSilence))
}

// movedBackIdle：换回一周内（movedBackWindow）刚换走的那个省之前，手上这个省要静默这么久，而不只是家里静默时长。
// 两个省的设备交替闲置（笔记本白天在 C 用；落在 B 的摄像头整夜在连）时，按 2 小时来回换会把通行证在两省之间翻来
// 覆去，撞上每周次数时停在哪边就锁死哪边（推演复现：笔记本在 C 连着 5 天被拦）。真换地方是去新的省，不受影响；
// 夜里的空档一般短于 12 小时；A→B→C 再回 B 的人在 B 最多多等半天。
const (
	movedBackIdle   = 12 * time.Hour
	movedBackWindow = 7 * 24 * time.Hour
)

// moveOnIdle：换去 province 之前，手上这个省要静默多久。
func moveOnIdle(in autoTravelInput, province string) time.Duration {
	since := in.Now.Add(-movedBackWindow).UnixMilli()
	for _, h := range in.History {
		if h.Auto && h.EndReason == model.RegionPassMoved && h.EndedAt >= since && passProvince(h) == province {
			return max(in.Params.HomeSilence, movedBackIdle)
		}
	}
	return in.Params.HomeSilence
}

// moveOnCandidates：人已经离开通行证的省、去了别的省时，返回可以换过去的被拦来源。要求通行证发出已满家里静默时长
// （刚发出时这个省还没来得及留下用量）、这个省此刻不在用（passInUse 为假；换回刚换走的省还要静默满 movedBackIdle）、
// 近 10 分钟有别的可放行省份的来源被地区规则拦下；观测中断时看不清这个省有没有在用，不判。调用方还要拿这些候选跑
// 一遍 decideGrant，新的省此刻发得出才换。
//
// 改动前生效中的通行证会把下一站一直挡到它到期（被拦来源只在没有通行证时才看），人走了它还可能再续一整张。
// 代价：手机与笔记本分别显示两个省的人，笔记本那边停用满家里静默时长后通行证会换到手机那个省（换回来要等半天，
// 每周次数用完后不再换）。
func moveOnCandidates(in autoTravelInput, inUse bool) []autoTravelCandidate {
	if in.ObservationStale || inUse || in.Now.UnixMilli()-in.Active.GrantedAt < in.Params.HomeSilence.Milliseconds() {
		return nil
	}
	current := passProvince(*in.Active)
	if current == "" {
		return nil
	}
	var out []autoTravelCandidate
	for _, c := range in.Blocked {
		if c.Province == "" || c.Province == current {
			continue
		}
		if idle := moveOnIdle(in, c.Province); idle > in.Params.HomeSilence && homeActiveSince(in.PassRows, in.Now.Add(-idle)) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// 发放前按省份判断的两道防共享门槛。冷却设为 0（不暂停）时一起关闭。
//
// 错峰共享与出差在网络层同形（家里停、外地用），共享风险评分对它最多给到「中」，挡不住发放；
// 这两条看的是只有「两个人」才会留下的痕迹：
//   - 家里正在用的时候，这个省也来连过（被地区规则拦下）：一个人不可能同时在两个地方；
//   - 这个省的自动通行证反复刚发出不久，就因为家里恢复使用（或换到了别的省）而结束：真出差至少要在
//     外地待上一阵，白天本人用、夜里另一个人用，才会一次次在几个小时后被收回。
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
		// 换省结束（moved）的也算：两个人在两个省错开时间用，每次都会在几个小时后因「换了地方」结束。
		shortEnd := h.EndReason == model.RegionPassHome || h.EndReason == model.RegionPassMoved
		if !h.Auto || !shortEnd || h.EndedAt-h.GrantedAt >= autoTravelShortPass.Milliseconds() {
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
		return fmt.Sprintf("%s的自动通行证 %s内三次刚发出不久就因家里恢复使用或换了地方而结束，暂停对这个省自动放行到 %s",
			province, hoursText(autoTravelEscalateWindow), time.UnixMilli(until).In(in.Loc).Format("01-02 15:04"))
	}
	if recent >= 2 {
		return fmt.Sprintf("%s的自动通行证 %s内两次刚发出不久就因家里恢复使用或换了地方而结束，像两个人错开时间用，暂停对这个省自动放行",
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
		input, reason := s.assemble(in, passes[in.Id], matcher, logEnabled, now, loc, params)
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
				// 读不出状态行就判断不了「30 天内只改去一个新省」：这一轮不改，也不预支额度。
				logger.Warning("长住改地区：读取自动设置状态失败, 入站", in.Id, ":", stErr)
			} else {
				input.Relocation = &autoTravelRelocation{Days: params.RelocateDays, State: st}
				rd = decideRelocation(relocateInput{Now: now, Loc: loc, Enabled: masterOn, Days: params.RelocateDays,
					Active: input.Active, HomeRows: input.HomeRows, HomeLiveActive: input.HomeLiveActive,
					ServedAway: input.ServedAway, History: input.History, State: st,
					RiskLevel: input.RiskLevel, RiskUnknown: riskUnknown,
					ObservationStale: input.ObservationStale, AuthLess: input.AuthLess, Recent: input.Recent})
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
		case autoTravelMoved:
			// 先收回旧的（比较后写入：管理员刚收回或替换了就作罢），再发新的；新的发不出时旧的已结束，下一轮按常规判断。
			ended, err := s.regionPassService.RevokePass(in.Id, input.Active.Id, model.RegionPassMoved, now)
			switch {
			case err != nil:
				status.Action, status.Reason = autoTravelDenied, "收回失败: "+err.Error()
			case !ended:
				status.Action, status.Reason = autoTravelIdle, "通行证已经结束，没有换省"
			default:
				changed = true
				if _, err := s.regionPassService.GrantAuto(in.Id, d.Province, d.Until, now); err != nil {
					status.Action, status.Reason = autoTravelDenied, "已结束旧通行证，发放新的失败: "+err.Error()
				} else {
					logger.Info("自动出差放行：入站", in.Id, d.Reason, "至", time.UnixMilli(d.Until).In(loc).Format("01-02 15:04"))
				}
			}
		}
		next[in.Id] = status
	}
	return changed, nil
}

// assemble 组装 decideAutoTravel 的输入；返回非空 reason 表示本轮无法判断。
func (s *AutoTravelService) assemble(in *model.Inbound, active *model.InboundRegionPass, matcher *geoBlockMatcher,
	logEnabled bool, now time.Time, loc *time.Location, params autoTravelParams) (autoTravelInput, string) {
	input := autoTravelInput{Now: now, Loc: loc, Active: active,
		AuthLess: !inboundRequiresAuth(in), ObservationStale: !observationFresh(now)}
	cooldown := params.Cooldown
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
	// 通行证历史先读：判断能不能换省（moveOnCandidates 要看一周内换走过哪些省）也要用。
	history, err := s.regionPassService.Since(in.Id, now.Add(-autoTravelHistoryWindow).UnixMilli())
	if err != nil {
		return input, "读取通行证记录失败"
	}
	input.History = history
	// 机房网段（「拦截机房 / 云服务器来源」开着且入站没豁免时非 nil）：被拦来源与近期动静都要去掉它们，只建一次。
	var idc *cidrSet
	if logEnabled && !idcExempt(in) {
		idc = idcBlockedSet(&SettingService{}, &IPDBService{})
	}
	// 没有通行证时，被拦来源决定要不要发放；持有自动通行证时，用它们判断人是不是换了地方（moveOnCandidates）。
	// 通行证自己那个省的来源不算（通行证期间撞上别的拦截规则同样进黑洞）。手动通行证不归自动放行管。
	if logEnabled && (active == nil || active.Auto) {
		input.Blocked = dropIDCCandidates(dropStoredCandidates(recentGeoBlocked(in.Id, matcher, now), home), idc)
		needSuspect := active == nil
		if active != nil {
			input.Blocked = dropStoredCandidates(input.Blocked, passSet)
			// 持有自动通行证时，只有换了地方的前提都成立、要拿 decideGrant 试探新的省时才需要它；与 decideAutoTravel
			// 用同一个 moveOnCandidates 判断。不这么挡的话，另一台设备长期在别的省被拦的人，每分钟都要做一次一周的
			// 聚合查询（代码审查指出）。
			probe := autoTravelInput{Now: now, Active: active, Params: params, Blocked: input.Blocked, History: history,
				PassRows: input.PassRows, PassLiveActive: input.PassLiveActive, ObservationStale: input.ObservationStale}
			needSuspect = len(moveOnCandidates(probe, passInUse(probe))) > 0
		}
		if needSuspect && len(input.Blocked) > 0 && cooldown > 0 {
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
	input.ServedAway = servedAwayRows(rows, home, history, allowedSetFor, now)
	if logEnabled {
		input.Recent = func() ([]recentSource, error) {
			sources, err := recentBlockedSources(in.Id, matcher, home, idc, now)
			if err != nil {
				// 读不出就不改地区（stillThere 返回假）：记下来，不然访问日志库一直出错时，长住改地区悄悄停了，
				// 状态与日志里都看不出原因（代码审查指出）。
				logger.Warning("长住改地区：读取近期被拦来源失败，这一轮不改地区, 入站", in.Id, ":", err)
			}
			return sources, err
		}
	}
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

// suspectProvinces 是在家里一段实质使用的中间被地区规则拦下（homeInUseAround）的来源所在省份。
// provinceOf 返回空串的来源不计。
func suspectProvinces(blocked []blockedHour, rows []model.InboundIPHour, home *cidrSet, provinceOf func(ip string) string) map[string]bool {
	out := map[string]bool{}
	for _, b := range blocked {
		if !homeInUseAround(b.HourMs, rows, home) {
			continue
		}
		if p := provinceOf(b.IP); p != "" {
			out[p] = true
		}
	}
	return out
}

// homeInUseAround：被拦那一小时或前一小时家里有实质流量，并且后一小时家里还在接着用——被拦落在家里一段使用的
// 中间，一个人不可能同时在两个地方（2026-11-11 管理员选定）。
//
// 改动前只看前面（与「需要留意」的「疑似共享」标注 classifyBlocked 同一判据）：离家后一两个小时内在外地连上的人都
// 被当成另一个人——30 分钟快车去邻省、路上手机走流量（地址判在家里的省）到站连酒店 Wi-Fi——这个省整个冷却期发不出
// 通行证，整趟被拦。代价：交接班式错峰（号主停下后一小时内外省接着用）与只在号主当天最后一小时重叠的共享，这一条
// 不再挡，只靠短命通行证门槛压制（TestScenarioHandoverSharerIsContained）。标注不跟着改：它看的是最近一次被拦，
// 后一小时还没到，用这个口径会让正在共享的人一直显示成疑似出差。
//
// 即将发放（或换省试探）时家里已静默满家里静默时长：最近一两个小时的被拦，后一小时家里本来就没在用；之后家里再用，
// 由并存收回接手。
func homeInUseAround(hourMs int64, rows []model.InboundIPHour, home *cidrSet) bool {
	sec := hourMs / 1000
	hour := sec - sec%3600
	var before, after bool
	for _, r := range rows {
		if r.ActiveBytes < coexistMinActiveBytes || !home.Contains(r.IP) {
			continue
		}
		switch r.HourStart {
		case hour, hour - 3600:
			before = true
		case hour + 3600:
			after = true
		}
	}
	return before && after
}
