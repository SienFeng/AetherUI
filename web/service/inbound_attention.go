package service

import (
	"sort"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
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

// AutoRegionAttention 是「需要留意」里的自动设置地区状态。
type AutoRegionAttention struct {
	State   string   `json:"state"`
	Regions []string `json:"regions"`
	// PrevRegions 是自动写入之前的地区：首次自动设置时为空，长住外地自动改地区时是原来的省（撤销会恢复它）。
	PrevRegions []string `json:"prevRegions"`
	Reason      string   `json:"reason"`
	At          int64    `json:"at"`
}

// 被拦来源的标注（GeoBlockedIP.Kind）。只做提示，不自动放行——自动放行由
// AutoTravelService 按更严的条件单独负责。
const (
	// geoBlockedCoexist：被拦的那一小时或前一小时，家里（存储的允许集）也在实质使用，
	// 两地同时在用，疑似共享。
	geoBlockedCoexist = "coexist"
	// geoBlockedTravel：同一时段家里没有实质流量，更像本人出差。
	geoBlockedTravel = "travel"
)

// classifyBlocked 按 lastAt（毫秒）所在小时与前一小时里，家里有没有实质流量判断。
// 「实质」与并存判定同一门槛（coexistMinActiveBytes）。自动出差放行的防共享门槛另用更严的 homeInUseAround
// （还要后一小时家里接着用）：这里是看最近一次被拦的实时提示，等不到后一小时，两者不要合并。
func classifyBlocked(lastAt int64, rows []model.InboundIPHour, home *cidrSet) string {
	sec := lastAt / 1000
	hour := sec - sec%3600
	for _, r := range rows {
		if (r.HourStart == hour || r.HourStart == hour-3600) &&
			r.ActiveBytes >= coexistMinActiveBytes && home.Contains(r.IP) {
			return geoBlockedCoexist
		}
	}
	return geoBlockedTravel
}

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
	// GeoDegraded 是本入站地区规则的降级状态（沿用旧数据 / 已全部拦截），见 geo_degrade.go。
	GeoDegraded *GeoDegradation `json:"geoDegraded,omitempty"`
	// RegionPass 是本入站的活动出差通行证。
	RegionPass *model.InboundRegionPass `json:"regionPass,omitempty"`
	// AutoTravel 是自动出差放行最近一次的判定结果（只对开了自动出差的入站）。
	AutoTravel *AutoTravelStatus `json:"autoTravel,omitempty"`
	// AutoRegion 是 7 天内的自动设置（首次设置，或长住外地改地区）；不再有「待确认」。
	AutoRegion *AutoRegionAttention `json:"autoRegion,omitempty"`
	// Throttle 是生效中的流量暴涨自动限速（见 SpikeThrottleService）。
	Throttle *model.InboundThrottle `json:"throttle,omitempty"`
	// PortConflict：端口被别的程序占着，这个入站被临时摘出了核心配置（每 30 秒复查，端口空出来自动恢复）。
	PortConflict *PortConflict `json:"portConflict,omitempty"`
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
	// Province 是多数票省份，供一键发通行证；境外、IPv6 或查不到时为空。
	Province string `json:"province"`
	// Kind 是 geoBlockedCoexist 或 geoBlockedTravel；判断不了（IP 库未加载）时为空。
	Kind string `json:"kind"`
}

type InboundAttentionService struct {
	settingService SettingService
}

// Summary 返回需要留意的入站（入站 id → 状态）。读不出入站列表或时区才返回 error。
//
// 地区拦截、自动设置、通行证这几部分是附属的：其中某一块读不出（访问日志关闭、库出错）
// 时只是没有那一部分并记日志，到期提醒、停用原因和侧栏提醒点照常给出——曾经任何一块
// 出错都让整个摘要失败，侧栏和列表上的提醒一起消失。
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
		logger.Warning("需要留意：读取地区拦截失败，本次不显示这一部分:", err)
	}
	for id, g := range geo {
		get(id).GeoBlocked = g
	}
	enabled := map[int]bool{}
	for _, in := range inbounds {
		enabled[in.Id] = in.Enable
	}
	for id, d := range GeoDegradedInbounds() {
		if enabled[id] {
			d := d
			get(id).GeoDegraded = &d
		}
	}
	for id, st := range AutoTravelStatuses() {
		if enabled[id] && st.Action != autoTravelIdle {
			st := st
			get(id).AutoTravel = &st
		}
	}
	states, err := (&AutoRegionService{}).States()
	if err != nil {
		logger.Warning("需要留意：读取自动设置地区状态失败，本次不显示这一部分:", err)
	}
	regionsOf := map[int]string{}
	for _, in := range inbounds {
		regionsOf[in.Id] = in.Regions
	}
	for id, st := range states {
		// 7 天内的自动设置（首次设置或长住改地区）不论启用与否都给出：那段时间里把地区改回去只能点它
		// 撤销（见 checkStaleAutoRegionWrite），停用的入站也一样。全部判断都自动做，没有「待确认」。
		if st.State != model.AutoRegionApplied || now.UnixMilli()-st.UpdatedAt > autoRegionAppliedVisible.Milliseconds() {
			continue
		}
		if _, ok := regionsOf[id]; !ok {
			continue
		}
		regions, _ := DecodeRegions(st.Regions)
		if regions == nil {
			regions = []string{}
		}
		prev, _ := DecodeRegions(st.PrevRegions)
		if prev == nil {
			prev = []string{}
		}
		get(id).AutoRegion = &AutoRegionAttention{State: st.State, Regions: regions, PrevRegions: prev,
			Reason: st.Reason, At: st.UpdatedAt}
	}
	passes, err := (&RegionPassService{}).Active()
	if err != nil {
		logger.Warning("需要留意：读取出差通行证失败，本次不显示这一部分:", err)
	}
	for id, p := range passes {
		if enabled[id] {
			get(id).RegionPass = p
		}
	}
	var throttles []model.InboundThrottle
	if err := database.GetDB().Where("until > ?", now.UnixMilli()).Find(&throttles).Error; err != nil {
		logger.Warning("需要留意：读取流量暴涨限速失败，本次不显示这一部分:", err)
	}
	for i := range throttles {
		if enabled[throttles[i].InboundId] {
			get(throttles[i].InboundId).Throttle = &throttles[i]
		}
	}
	conflicts := (&XrayService{}).PortConflicts()
	if len(conflicts) > 0 {
		idByTag := make(map[string]int, len(inbounds))
		for _, in := range inbounds {
			idByTag[in.Tag] = in.Id
		}
		for i := range conflicts {
			if id, ok := idByTag[conflicts[i].Tag]; ok && enabled[id] {
				get(id).PortConflict = &conflicts[i]
			}
		}
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
	regionsOf := map[int]string{}
	for _, in := range inbounds {
		regionsOf[in.Id] = in.Regions
	}
	out := make(map[int]*GeoBlockedSummary, len(byInbound))
	for id, list := range byInbound {
		annotateBlocked(list, id, regionsOf[id], now)
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

// enforceableProvince 返回 province，前提是按执行口径放行这个省确实放得进 ip；否则返回空。
// 多数票选出的省份（界面上显示的那个）在按多数判据合并时不一定包含这个 IP（各库平票），那时
// 一键发的通行证放行不了它，宁可标成未知、不给按钮。cache 让同一个省的允许集只建一次。
func enforceableProvince(ip, province string, allowed func([]string) (*cidrSet, error), cache map[string]*cidrSet) string {
	if province == "" {
		return ""
	}
	set, ok := cache[province]
	if !ok {
		var err error
		if set, err = allowed([]string{province}); err != nil {
			set = nil
		}
		cache[province] = set
	}
	if set == nil || !set.Contains(ip) {
		return ""
	}
	return province
}

// annotateBlocked 给被拦来源补上省份与「疑似共享 / 疑似出差」标注。家里按**存储的**
// 地区判断（不含通行证）：通行证放进来的省份不是「家」。IP 库未加载时不标注。
func annotateBlocked(list []GeoBlockedIP, inboundId int, storedRegions string, now time.Time) {
	provinces, err := DecodeRegions(storedRegions)
	if err != nil || len(provinces) == 0 {
		return
	}
	home, err := allowedSetFor(provinces)
	if err != nil {
		return
	}
	rows, err := (&SharingService{}).windowRows(inboundId, 2, now)
	if err != nil {
		return
	}
	sets := map[string]*cidrSet{}
	for i := range list {
		list[i].Province = enforceableProvince(list[i].IP, provinceOfIP(list[i].IP), allowedSetFor, sets)
		list[i].Kind = classifyBlocked(list[i].LastAt, rows, home)
	}
}
