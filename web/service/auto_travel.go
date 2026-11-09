package service

import (
	"fmt"
	"net"
	"sync"
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
	autoTravelHistoryWindow   = 35 * 24 * time.Hour
)

// autoTravelParams 是管理员可调的阈值，来自 SettingService.GetAutoTravelParams。
type autoTravelParams struct {
	HomeSilence  time.Duration // 家里静默多久才算「人走了」
	StableDays   int           // 近 7 天里至少几天有家庭流量才算「有稳定的家」
	PassDuration time.Duration // 单张自动通行证的时长，续期也续这么长
	MonthlyDays  int           // 每个自然月自动放行的天数上限
	WeeklyGrants int           // 近 7 天自动发放次数上限
	Cooldown     time.Duration // 并存或管理员收回后暂停自动放行多久；0 表示不暂停
}

// defaultAutoTravelParams 与 defaultValueMap 里的默认值一致。
func defaultAutoTravelParams() autoTravelParams {
	return autoTravelParams{
		HomeSilence: 2 * time.Hour, StableDays: 3, PassDuration: 72 * time.Hour,
		MonthlyDays: 10, WeeklyGrants: 3, Cooldown: 7 * 24 * time.Hour,
	}
}

// AutoTravelStatus.Action 的取值。
const (
	autoTravelGranted = "granted"
	autoTravelDenied  = "denied"
	autoTravelRenewed = "renewed"
	autoTravelEnded   = "ended"
	autoTravelIdle    = "idle"
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
	// HomeRows 是近 7 天、IP 落在存储的允许集里的小时行。
	HomeRows []model.InboundIPHour
	// PassRows 是近 24 小时、IP 落在活动通行证省份里（且不在家里）的小时行。
	PassRows []model.InboundIPHour
	// HomeLiveActive / PassLiveActive 是在线表此刻有没有活跃（非闲置）的来源。
	HomeLiveActive, PassLiveActive bool
	// Blocked 是近 10 分钟被地区规则拦下、且已验证候选省份的 IPv4 来源。
	Blocked []autoTravelCandidate
	Active  *model.InboundRegionPass
	// History 是近 35 天该入站的通行证记录。
	History   []model.InboundRegionPass
	RiskLevel string
	Params    autoTravelParams
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
		if !p.Auto {
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

// decideAutoTravel 是自动放行的全部判定，纯函数。
func decideAutoTravel(in autoTravelInput) autoTravelDecision {
	now := in.Now
	p := in.Params
	if in.Active != nil {
		if !in.Active.Auto {
			return autoTravelDecision{Action: autoTravelIdle, Reason: "已有手动通行证"}
		}
		if in.HomeLiveActive || homeActiveSince(in.HomeRows, now.Add(-time.Hour)) {
			reason := model.RegionPassHome
			text := "家里恢复使用，收回通行证"
			if in.HomeLiveActive && in.PassLiveActive {
				reason = model.RegionPassCoexist
				text = "家里与外地同时在用，疑似共享，收回通行证并暂停自动放行 7 天"
			}
			return autoTravelDecision{Action: autoTravelEnded, Reason: text, EndReason: reason}
		}
		if in.Active.Until-now.UnixMilli() < autoTravelRenewWithin.Milliseconds() &&
			hasSubstantialRow(in.PassRows) && autoDaysThisMonth(in.History, now, in.Loc) < float64(p.MonthlyDays) {
			return autoTravelDecision{Action: autoTravelRenewed, Reason: "家里仍未使用，外地仍在用，续期",
				Until: now.Add(p.PassDuration).UnixMilli()}
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
	for _, h := range in.History {
		if p.Cooldown > 0 && h.EndReason == model.RegionPassCoexist && h.EndedAt >= cooldownFrom {
			return deny(cooldownText + "内发现过家里与外地同时在用，暂停自动放行")
		}
		// 管理员手动收回过自动通行证，说明他不认可这次放行。不冷却的话，被拦记录与
		// 「家里静默」都还在，下一分钟就会原样发回去。
		if p.Cooldown > 0 && h.Auto && h.EndReason == model.RegionPassRevoked && h.EndedAt >= cooldownFrom {
			return deny(cooldownText + "内管理员收回过自动通行证，暂停自动放行")
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
	for _, c := range in.Blocked {
		if c.Province != "" {
			return autoTravelDecision{Action: autoTravelGranted, Reason: "家里 2 小时未使用，外地被拦，自动放行",
				Province: c.Province, Until: now.Add(p.PassDuration).UnixMilli()}
		}
	}
	return deny("被拦来源查不到省份（境外或 IPv6），不自动放行")
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
	if err != nil {
		logger.Warning("自动出差放行：读取共享风险失败，本轮按无风险数据判断:", err)
	}
	params := s.settingService.GetAutoTravelParams()
	logEnabled, _ := s.settingService.GetAccessLogEnable()
	idleAfter := idleTimeoutOrZero(s.settingService.GetConcurrencyIdleTimeout())
	matcher := newGeoBlockMatcherWith(inbounds, passes, allowedSetFor)

	changed := false
	for _, in := range targets {
		status := AutoTravelStatus{At: now.UnixMilli()}
		input, reason := s.assemble(in, passes[in.Id], matcher, logEnabled, idleAfter, now, loc)
		if reason != "" {
			status.Action, status.Reason = autoTravelDenied, reason
			next[in.Id] = status
			continue
		}
		input.RiskLevel = risk[in.Id].Level
		input.Params = params
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
			if err := s.regionPassService.Renew(input.Active.Id, d.Until); err != nil {
				status.Action, status.Reason = autoTravelDenied, "续期失败: "+err.Error()
			}
		case autoTravelEnded:
			if ended, err := s.regionPassService.RevokePass(input.Active.Id, d.EndReason, now); err != nil {
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
	logEnabled bool, idleAfter time.Duration, now time.Time, loc *time.Location) (autoTravelInput, string) {
	input := autoTravelInput{Now: now, Loc: loc, Active: active}
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
	for _, e := range onlineTrackerInstance.snapshotIdle(in.Port, noLocate, idleAfter) {
		if e.Idle || e.Conns == 0 {
			continue
		}
		if home.Contains(e.IP) {
			input.HomeLiveActive = true
		} else if passSet.Contains(e.IP) {
			input.PassLiveActive = true
		}
	}
	if logEnabled && active == nil {
		input.Blocked = recentGeoBlocked(in.Id, matcher, now)
	}
	history, err := s.regionPassService.Since(in.Id, now.Add(-autoTravelHistoryWindow).UnixMilli())
	if err != nil {
		return input, "读取通行证记录失败"
	}
	input.History = history
	return input, ""
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
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil || !matcher.Blocks(inboundId, ip) {
			continue
		}
		c := autoTravelCandidate{IP: ip}
		if p := provinceOfIP(ip); p != "" {
			if set, err := allowedSetFor([]string{p}); err == nil && set.Contains(ip) {
				c.Province = p
			}
		}
		out = append(out, c)
	}
	return out
}
