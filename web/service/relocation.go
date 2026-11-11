package service

import (
	"errors"
	"fmt"
	"net"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
)

// 长住外地：自动改地区。
//
// 自动出差放行把一次搬家当成出差：额度用完（默认 10 天）之后外地被拦，而且一直拦着——重新发证要求
// 「近 7 天在家里至少用过几天」，人搬走一周后这一条永远不满足。管理员不常看后台时，用户第 10 天起就
// 再也用不了。所以在总开关开着时，一张一直续着的自动通行证撑到「家里连续 N 天没用、外地至少 3 天在用」
// 时，直接把地区改成那个省。
//
// 代价（管理员选了全自动，知情接受）：搬家与「把账号转给外省的人」在网络层完全同形，改地区会把接手者
// 的省份合法化。门槛因此比发通行证更严：观测必须新鲜、共享风险读得出且低于「中」、入站需要认证，
// 而且只认一张已经持续生效到现在的单省自动通行证——它本身已经过了发放时的全部防共享门槛。
type relocateInput struct {
	Now     time.Time
	Loc     *time.Location
	Enabled bool // 总开关（自动管理地区限制）
	Days    int  // autoTravelRelocateDays，0 表示不自动改
	// Active 是生效中的通行证（可能为 nil）：手动的表示管理员在管，不动；自动的必须就是要改去的那个省。
	Active *model.InboundRegionPass
	// HomeRows 是近 7 天落在存储地区里的小时行。
	HomeRows       []model.InboundIPHour
	HomeLiveActive bool
	// ServedAway 是近 7 天、在某张通行证生效期间落在它的省份里的小时行（servedAwayRows 按执行同口径的
	// CIDR 集合判定）。通行证到期以后这些行还在：额度先用完、通行证没撑到第 N 天的人，凭它们照样能改。
	// 只认被放行过的行——被拦下的连接只说明外地有人在敲门，不说明有人住在那里。
	ServedAway []servedAwayRow
	// History 是近 65 天的通行证记录；State 是这个入站的自动设置状态行（可能为 nil）。
	History          []model.InboundRegionPass
	State            *model.InboundAutoRegion
	RiskLevel        string
	RiskUnknown      bool
	ObservationStale bool
	AuthLess         bool
	// Recent 惰性取近 24 小时家里之外的来源动静（被地区规则拦下的来连）；nil 表示没有这类记录。
	Recent func() ([]recentSource, error)
}

// recentSource 是家里之外某个来源在某个整点小时的一次动静。
type recentSource struct {
	Province string // 可放行的省份；查不到（境外、IPv6、归属地不明）为空串
	Hour     int64  // 整点小时（Unix 秒）
}

type servedAwayRow struct {
	Province    string
	HourStart   int64
	ActiveBytes int64
}

type relocateDecision struct {
	Province string // 空串表示不改
	Reason   string
}

// relocateNewProvinceWindow：同一个入站 30 天内只自动改去一个新省（搬回原来的地区不算）。
const relocateNewProvinceWindow = 30 * 24 * time.Hour

// relocatePresenceWindow：改地区前这么久之内，这个省要有动静（被放行的实质使用，或被地区规则拦下的来连）。
const relocatePresenceWindow = 24 * time.Hour

// decideRelocation 是长住改地区的全部判定，纯函数（Recent 是惰性取数的回调，只在其余条件都满足时调用）。
func decideRelocation(in relocateInput) relocateDecision {
	if !in.Enabled || in.Days <= 0 || in.AuthLess || in.ObservationStale || in.RiskUnknown || !relocationRiskOK(in.RiskLevel) {
		return relocateDecision{}
	}
	if in.Active != nil && !in.Active.Auto {
		return relocateDecision{} // 管理员手动发的通行证：他在管这个入站
	}
	since := in.Now.Add(-time.Duration(in.Days) * 24 * time.Hour)
	if in.HomeLiveActive || homeActiveSince(in.HomeRows, since) {
		return relocateDecision{}
	}
	from := hourStartOf(since)
	days := map[string]map[string]bool{}
	for _, r := range in.ServedAway {
		if r.Province == "" || r.HourStart < from || r.ActiveBytes < coexistMinActiveBytes {
			continue
		}
		if days[r.Province] == nil {
			days[r.Province] = map[string]bool{}
		}
		days[r.Province][time.Unix(r.HourStart, 0).In(in.Loc).Format("2006-01-02")] = true
	}
	// 没有、或者不止一个省：不像搬到了某一个地方。
	if len(days) != 1 {
		return relocateDecision{}
	}
	var province string
	for p := range days {
		province = p
	}
	if len(days[province]) < min(3, in.Days) {
		return relocateDecision{}
	}
	if in.Active != nil && passProvince(*in.Active) != province {
		return relocateDecision{}
	}
	if lastPassRevoked(in.History, province) {
		return relocateDecision{} // 管理员收回过这个省最近的一张通行证：他不想放行这个省
	}
	if recentlyRelocatedElsewhere(in.State, province, in.Now) {
		return relocateDecision{}
	}
	// 放在最后：要查访问日志。
	if !stillThere(in, province) {
		return relocateDecision{}
	}
	return relocateDecision{Province: province,
		Reason: fmt.Sprintf("家里 %d 天没有使用，%s有 %d 天在用，按长住处理，地区改为%s", in.Days, province, len(days[province]), province)}
}

// stillThere：人此刻还在这个省。近 relocatePresenceWindow 内这个省要有动静（通行证期间被放行的实质使用，或被地区
// 规则拦下的来连），别处（别的省、境外、查不到省份的来源）最近的动静也不能比它晚——按整点小时比，同一小时不算离开
// （手机与笔记本可能分属两个省）。
//
// 改动前只看「近 N 天里有 3 天在用」：A→B（3 天）→C 的人在 C 时地区就被改成了 B，回家以后反被锁在门外，不会自愈；
// 去了境外或一直没上线的人也一样。被拦的来连在这里只用来确认或否决人在哪，不算住在那里的天数（那只认被放行的行）。
func stillThere(in relocateInput, province string) bool {
	var last, elsewhere int64
	note := func(p string, hour int64) {
		if p == province {
			last = max(last, hour)
		} else {
			elsewhere = max(elsewhere, hour)
		}
	}
	for _, r := range in.ServedAway {
		if r.ActiveBytes >= coexistMinActiveBytes {
			note(r.Province, r.HourStart)
		}
	}
	if in.Recent != nil {
		recent, err := in.Recent()
		if err != nil {
			return false
		}
		for _, s := range recent {
			note(s.Province, s.Hour)
		}
	}
	return last >= hourStartOf(in.Now.Add(-relocatePresenceWindow)) && elsewhere <= last
}

// recentBlockedSources 取近 relocatePresenceWindow 内被地区规则拦下的 IPv4 来源（访问日志里带路由结果、已过协议鉴权的
// 行），每个来源取最后一次所在的整点小时。家里与机房的来源不算：前者不是「家里之外」，后者不说明人在哪。IPv6 也不算
// （与 recentGeoBlocked 一样）：地区限制下 IPv6 一律被拦、查不到省份，双栈设备人在哪都会留下这种来连，算进「别处」
// 的话会一再否决真搬过去的人（代码审查指出）。省份按执行同口径核对，查不到（境外、归属地不明）为空串。
func recentBlockedSources(inboundId int, matcher *geoBlockMatcher, home, idc *cidrSet, now time.Time) ([]recentSource, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return nil, nil
	}
	rows, err := db.Model(&model.AccessLog{}).Select("source_ip, max(time)").
		Where("inbound_id = ? AND time >= ? AND route = ? AND accepted = ?",
			inboundId, now.Add(-relocatePresenceWindow).UnixMilli(), model.BlockOutboundTag, true).
		Group("source_ip").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sets := map[string]*cidrSet{}
	var out []recentSource
	for rows.Next() {
		var ip string
		var lastMs int64
		if err := rows.Scan(&ip, &lastMs); err != nil {
			return nil, err
		}
		if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
			continue
		}
		if home.Contains(ip) || idc.Contains(ip) || !matcher.Blocks(inboundId, ip) {
			continue
		}
		sec := lastMs / 1000
		out = append(out, recentSource{Province: enforceableProvince(ip, provinceOfIP(ip), allowedSetFor, sets), Hour: sec - sec%3600})
	}
	return out, rows.Err()
}

// relocationRiskOK：共享风险评分已就绪且低于「中」（low / watch）。空串是评分还没就绪（学习中、降级、引擎版本刚变、
// 还没有快照）：改地区会把这个省永久合法化，「测不了」不能当成「低风险」——风险评分自己也是这么约定的
// （sharing_risk.go 顶部第 1 条）。预支额度（relocationBorrow）用同一条口径。
func relocationRiskOK(level string) bool {
	switch level {
	case "", RiskLevelMedium, RiskLevelHigh, RiskLevelSevere:
		return false
	}
	return true
}

// servedDays 是 since 所在小时以来，这个省在通行证期间被放行、有实质使用的日历天数（面板时区）。
func servedDays(rows []servedAwayRow, province string, since time.Time, loc *time.Location) int {
	from := hourStartOf(since)
	days := map[string]bool{}
	for _, r := range rows {
		if r.Province == province && r.HourStart >= from && r.ActiveBytes >= coexistMinActiveBytes {
			days[time.Unix(r.HourStart, 0).In(loc).Format("2006-01-02")] = true
		}
	}
	return len(days)
}

// lastPassRevoked：这个省最近的一张通行证（自动或手动）是被管理员收回的。
func lastPassRevoked(history []model.InboundRegionPass, province string) bool {
	var last *model.InboundRegionPass
	for i := range history {
		h := &history[i]
		if passProvince(*h) != province {
			continue
		}
		if last == nil || h.GrantedAt > last.GrantedAt {
			last = h
		}
	}
	return last != nil && last.EndReason == model.RegionPassRevoked
}

// recentlyRelocatedElsewhere：30 天内已经自动改去过一个新省，而这次要去的也不是原来的地方。
// 搬家一次是常态；把账号一个接一个转给外省的人，每次在网络层都和搬家一样。
func recentlyRelocatedElsewhere(st *model.InboundAutoRegion, province string, now time.Time) bool {
	if st == nil || st.RelocatedAt == 0 || now.UnixMilli()-st.RelocatedAt >= relocateNewProvinceWindow.Milliseconds() {
		return false
	}
	return !regionsContain(st.PrevRegions, province)
}

// regionsContain：编码的地区字段里有这个省。解析不了按「没有」处理。
func regionsContain(encoded, province string) bool {
	regions, err := DecodeRegions(encoded)
	if err != nil {
		return false
	}
	for _, p := range regions {
		if p == province {
			return true
		}
	}
	return false
}

// servedAwayRows 挑出不在家里、而且就在某张单省通行证生效期间落在那个省里的小时行。
func servedAwayRows(rows []model.InboundIPHour, home *cidrSet, passes []model.InboundRegionPass,
	setFor func([]string) (*cidrSet, error), now time.Time) []servedAwayRow {
	sets := map[string]*cidrSet{}
	var out []servedAwayRow
	for _, p := range passes {
		province := passProvince(p)
		if province == "" {
			continue
		}
		set, ok := sets[province]
		if !ok {
			set, _ = setFor([]string{province})
			sets[province] = set
		}
		if set == nil {
			continue
		}
		end := p.EndedAt
		if end == 0 {
			end = min(p.Until, now.UnixMilli())
		}
		for _, r := range rows {
			hourStartMs, hourEndMs := r.HourStart*1000, (r.HourStart+3600)*1000
			if hourEndMs <= p.GrantedAt || hourStartMs >= end || home.Contains(r.IP) || !set.Contains(r.IP) {
				continue
			}
			out = append(out, servedAwayRow{Province: province, HourStart: r.HourStart, ActiveBytes: r.ActiveBytes})
		}
	}
	return out
}

// relocate 把入站的地区改成 province、结束那张自动通行证（pass 为 nil 表示它已经到期）、记下原来的地区，
// 一个事务。返回是否改了。
//
// 全部是比较后写入：地区必须仍是本轮开头读到的值（管理员这期间改过就让路），传进来的通行证必须仍在生效
// （管理员收回了，说明他不想放行这个省）。任何一步对不上就整体回滚、静默跳过。
func relocate(in *model.Inbound, pass *model.InboundRegionPass, province, reason string, now time.Time) (bool, error) {
	encoded, err := EncodeRegions([]string{province})
	if err != nil {
		return false, err
	}
	if err := regionsUsable(encoded, in.Regions); err != nil {
		return false, err
	}
	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		res := whereRegionsStill(tx.Model(&model.Inbound{}).Where("id = ?", in.Id), in.Regions).
			Where(autoRegionAllowedSQL).UpdateColumns(map[string]any{"regions": encoded})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errAutoRegionStale
		}
		if pass != nil {
			ended := tx.Model(&model.InboundRegionPass{}).Where("id = ? AND inbound_id = ? AND ended_at = 0", pass.Id, in.Id).
				Updates(map[string]any{"ended_at": now.UnixMilli(), "end_reason": model.RegionPassRelocated})
			if ended.Error != nil {
				return ended.Error
			}
			if ended.RowsAffected == 0 {
				return errAutoRegionStale
			}
		}
		// 判定窗口里到期的那几张这个省的自动通行证也改记成 relocated：改了地区，它们算「住在那里」，
		// 不再占本月的自动放行额度（autoDaysThisMonth 不计 relocated）。
		if err := relabelRelocatedPasses(tx, in.Id, province, now); err != nil {
			return err
		}
		var cur model.InboundAutoRegion
		if err := tx.Where("inbound_id = ?", in.Id).Limit(1).Find(&cur).Error; err != nil {
			return err
		}
		relocatedAt := now.UnixMilli()
		if cur.InboundId != 0 && regionsContain(cur.PrevRegions, province) {
			// 搬回原来的地方（出差超过 N 天先改过去、回家后再改回来）：不刷新「30 天内只改去一个新省」的时刻。
			relocatedAt = cur.RelocatedAt
		}
		// 无条件写状态行：管理员在编辑里改过地区（manual）也一样——他开着这个入站的自动出差放行，
		// 就是把移动交给了系统。撤销会恢复原来的地区并关掉自动出差放行。
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&model.InboundAutoRegion{
			InboundId: in.Id, State: model.AutoRegionApplied, Regions: encoded, PrevRegions: in.Regions,
			Reason: reason, UpdatedAt: now.UnixMilli(), RelocatedAt: relocatedAt, AdoptedAt: cur.AdoptedAt}).Error
	})
	if errors.Is(err, errAutoRegionStale) {
		return false, nil
	}
	return err == nil, err
}

// relabelRelocatedPasses 把近 7 天到期（或因换了地方结束）的、这个省的自动通行证改记成 relocated。
func relabelRelocatedPasses(tx *gorm.DB, inboundId int, province string, now time.Time) error {
	var passes []model.InboundRegionPass
	since := now.Add(-time.Duration(sharingWindowDays) * 24 * time.Hour).UnixMilli()
	if err := tx.Where("inbound_id = ? AND auto = ? AND end_reason IN ? AND ended_at >= ?",
		inboundId, true, []string{model.RegionPassExpired, model.RegionPassMoved}, since).Find(&passes).Error; err != nil {
		return err
	}
	var ids []int
	for _, p := range passes {
		if passProvince(p) == province {
			ids = append(ids, p.Id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&model.InboundRegionPass{}).Where("id IN ?", ids).
		Update("end_reason", model.RegionPassRelocated).Error
}
