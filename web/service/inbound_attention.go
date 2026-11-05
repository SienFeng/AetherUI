package service

import (
	"sort"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// 入站列表上「需要管理员留意」的只读汇总：停用原因与自动恢复时刻、近 24 小时的地区拦截。
//
// 停用原因（DisabledByTraffic）是服务端状态，刻意不随入站列表下发、也不接受前端提交
// （见 CLAUDE.md「流量自动重置」）；这里只读地给出它的含义，前端拿不到、也改不了标记本身。

// 停用原因（InboundAttention.DisabledReason）。
const (
	disabledReasonExpired = "expired" // 已到期
	disabledReasonTraffic = "traffic" // 因超流量被自动停用
	disabledReasonManual  = "manual"  // 管理员手动停用（或超流量停用后被管理员改过启用状态）
)

// geoBlockedWindow 是地区拦截统计回看的时长，与流量走向验证的日志窗口一致。
const geoBlockedWindow = 24 * time.Hour

// expiringSoonWindow 是「即将到期」的提前量，与入站列表的到期过滤、汇总卡同口径。
const expiringSoonWindow = 7 * 24 * time.Hour

// geoBlockedTopIPs 是每个入站列出的被拦来源 IP 数上限。
const geoBlockedTopIPs = 5

// InboundAttention 是一个入站需要留意的状态；全部字段为零值的入站不出现在汇总里。
type InboundAttention struct {
	// DisabledReason 只对停用的入站给出。
	DisabledReason string `json:"disabledReason,omitempty"`
	// AutoRestoreAt 是因超流量停用的入站会在哪一刻被流量重置任务自动拉回来（毫秒）。
	// 0 表示不会自动恢复：没开流量重置、重置周期推不出来，或那一刻之前就已到期。
	AutoRestoreAt int64 `json:"autoRestoreAt,omitempty"`
	// ExpiresSoon 表示启用中的入站将在 7 天内到期。侧栏提醒靠它：其他页面拿不到入站列表。
	ExpiresSoon bool `json:"expiresSoon,omitempty"`
	// GeoBlocked 是近 24 小时被入站地区限制拦下的连接；没有则为 nil。
	GeoBlocked *GeoBlockedSummary `json:"geoBlocked,omitempty"`
}

// GeoBlockedSummary 是一个入站近 24 小时的地区拦截统计。
type GeoBlockedSummary struct {
	Count  int64          `json:"count"`
	LastAt int64          `json:"lastAt"` // 毫秒
	IPs    []GeoBlockedIP `json:"ips"`    // 按次数降序，最多 geoBlockedTopIPs 个
	// MoreIPs 是没列出来的其余来源 IP 数。
	MoreIPs int `json:"moreIps"`
}

// GeoBlockedIP 是一个被拦来源。
type GeoBlockedIP struct {
	IP     string `json:"ip"`
	Count  int64  `json:"count"`
	LastAt int64  `json:"lastAt"`
}

type InboundAttentionService struct {
	settingService SettingService
}

// Summary 返回需要留意的入站（入站 id → 状态）。地区拦截读不出（访问日志关闭、库不可用）
// 时只是没有那一部分，停用原因照常给出；读库出错则返回 error。
func (s *InboundAttentionService) Summary(now time.Time) (map[int]*InboundAttention, error) {
	inbounds, err := (&InboundService{}).GetAllInbounds()
	if err != nil {
		return nil, err
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	out := map[int]*InboundAttention{}
	get := func(id int) *InboundAttention {
		if out[id] == nil {
			out[id] = &InboundAttention{}
		}
		return out[id]
	}
	for _, in := range inbounds {
		if in.Enable {
			if in.ExpiryTime > now.UnixMilli() && in.ExpiryTime-now.UnixMilli() <= expiringSoonWindow.Milliseconds() {
				get(in.Id).ExpiresSoon = true
			}
			continue
		}
		a := get(in.Id)
		a.DisabledReason, a.AutoRestoreAt = disabledReasonOf(in, now, loc)
	}

	geo, err := geoBlockedByInbound(inbounds, now)
	if err != nil {
		return nil, err
	}
	for id, g := range geo {
		get(id).GeoBlocked = g
	}
	return out, nil
}

// disabledReasonOf 给出停用原因，以及因超流量停用时的自动恢复时刻。判定与执行同口径：
// 到期优先于超流量（DisableInvalidInbounds 先停到期的，ResetDueTraffic 不拉回已到期的）。
func disabledReasonOf(in *model.Inbound, now time.Time, loc *time.Location) (string, int64) {
	if expiredAt(in, now) {
		return disabledReasonExpired, 0
	}
	if !in.DisabledByTraffic {
		return disabledReasonManual, 0
	}
	day, ok := resetDayFor(in, loc)
	if !ok {
		return disabledReasonTraffic, 0
	}
	next := nextResetInstantFor(now, day, loc)
	// 那一刻之前就已到期的，重置任务不会拉回它。
	if in.ExpiryTime > 0 && in.ExpiryTime <= next {
		return disabledReasonTraffic, 0
	}
	return disabledReasonTraffic, next
}

// nextResetInstantFor 返回 now 之后（不含当刻）最近的一次「每月 day 号 00:00」，与
// resetInstantFor 同一套钳制规则。重置任务每小时跑一次，实际恢复会在这一刻之后一小时内。
func nextResetInstantFor(now time.Time, day int, loc *time.Location) int64 {
	n := now.In(loc)
	candidate := monthlyInstant(n.Year(), n.Month(), day, loc)
	if !candidate.After(n) {
		year, month := n.Year(), n.Month()+1
		if month > time.December {
			year, month = year+1, time.January
		}
		candidate = monthlyInstant(year, month, day, loc)
	}
	return candidate.UnixMilli()
}

// geoBlockedByInbound 统计配了地区限制的启用入站近 24 小时被地区规则拦下的连接。
// 判定复用 geoBlockMatcher（与流量走向验证同一套），只看出站为 a-ui-block 的行。
func geoBlockedByInbound(inbounds []*model.Inbound, now time.Time) (map[int]*GeoBlockedSummary, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil, nil
	}
	enabled, err := (&SettingService{}).GetAccessLogEnable()
	if err != nil || !enabled {
		return nil, err
	}
	var ids []int
	for _, in := range inbounds {
		if !in.Enable {
			continue
		}
		if regions, err := DecodeRegions(in.Regions); err == nil && len(regions) > 0 {
			ids = append(ids, in.Id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []struct {
		InboundId int
		SourceIp  string
		Count     int64
		LastAt    int64
	}
	err = db.Model(&model.AccessLog{}).
		Select("inbound_id, source_ip, count(*) as count, max(time) as last_at").
		Where("inbound_id in ? and time >= ? and route = ? and accepted = ?",
			ids, now.Add(-geoBlockedWindow).UnixMilli(), model.BlockOutboundTag, true).
		Group("inbound_id, source_ip").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	matcher := newGeoBlockMatcher(inbounds)
	byInbound := map[int][]GeoBlockedIP{}
	for _, r := range rows {
		if !matcher.Blocks(r.InboundId, r.SourceIp) {
			continue // 允许集内来源被封禁，是分流规则的事，不是地区限制
		}
		byInbound[r.InboundId] = append(byInbound[r.InboundId], GeoBlockedIP{IP: r.SourceIp, Count: r.Count, LastAt: r.LastAt})
	}
	out := make(map[int]*GeoBlockedSummary, len(byInbound))
	for id, list := range byInbound {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Count != list[j].Count {
				return list[i].Count > list[j].Count
			}
			return list[i].IP < list[j].IP
		})
		g := &GeoBlockedSummary{}
		for _, ip := range list {
			g.Count += ip.Count
			g.LastAt = max(g.LastAt, ip.LastAt)
		}
		if len(list) > geoBlockedTopIPs {
			g.MoreIPs = len(list) - geoBlockedTopIPs
			list = list[:geoBlockedTopIPs]
		}
		g.IPs = list
		out[id] = g
	}
	return out, nil
}
