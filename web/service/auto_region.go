package service

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
)

// 自动管理地区限制：给没有地区限制的入站（新的与存量的）在观察期满后自动设置为**一个省**，并打开
// 自动出差放行；已有地区限制、从没被接管过的入站一次性打开自动出差放行。全部判断自动做，不再交管理员
// 确认——管理员不常看后台，「待确认」等于「永远不设限制」。
//
// 不看第一次连接（那是被否决过的 TOFU），看一段时间的数据。用到多个省时只设最常用的那一个：一起写进
// 允许集会把错峰共享者的省永久合法化，正常的第二个地方交给有防共享门槛的自动出差放行。
// 设计见 docs/superpowers/specs/2026-11-09-auto-region-design.md 与 2026-11-09-region-full-automation-design.md。

// decideAutoRegion 的结论。
const (
	autoRegionWait  = "wait"
	autoRegionApply = "apply"
	// autoRegionUnsure 表示这一轮判定不了（预演出错，多半是 IP 库的临时问题）：
	// 与 wait 不同，它不说明数据退回了观察状态，已有的建议原样保留，下一轮再判。
	autoRegionUnsure = "unsure"
)

// autoRegionAppliedVisible 是「已自动设置」在「需要留意」里保留的时长。
const autoRegionAppliedVisible = 7 * 24 * time.Hour

// autoRegionMultiMinSpan：用到多个省时，至少看这么久的数据再选主省份——观察期设成 1 天时，头一天的
// 数据里谁多谁少常常只是偶然。
const autoRegionMultiMinSpan = 3 * 24 * time.Hour

type autoRegionParams struct {
	ObserveDays       int // 最早一条用量记录距今至少几天
	StableDays        int // 近 7 天至少几天有实质流量（与自动出差放行同一设置）
	MaxBlockedPercent int // 设置后会被拦、而且自动出差放行也救不了的流量占近 7 天的比例上限
}

func defaultAutoRegionParams() autoRegionParams {
	return autoRegionParams{ObserveDays: 1, StableDays: 3, MaxBlockedPercent: 5}
}

type autoRegionInput struct {
	Now  time.Time
	Loc  *time.Location
	Rows []model.InboundIPHour // 近 7 天按来源 IP 的小时行
	// SpanRows 是观察期窗口里的行（见 autoRegionRows），只用来量观察期；nil 时用 Rows。观察天数大于 7 时
	// 只看近 7 天的行，「第一次实质使用距今满 N 天」永远达不到。
	SpanRows  []model.InboundIPHour
	RiskLevel string
	// AccessLogOff：访问日志没开时，自动出差放行发现不了被拦的人，「需要留意」也看不到被拦记录。
	AccessLogOff bool
	Params       autoRegionParams
	// UnplaceablePercent 按给定省份预演，返回近 7 天会被拦、而且自动出差放行也救不了的流量占比（0~100）：
	// 来源在允许集之外、又查不到中国的省份（境外、IPv6、归属地不明）。外省来源被拦了还有自动出差放行。
	UnplaceablePercent func(regions []string) (float64, error)
	// AllowedSet 建给定省份的允许集，与执行、与自动出差放行同口径（allowedSetFor）。
	AllowedSet func(regions []string) (*cidrSet, error)
}

type autoRegionDecision struct {
	Action  string
	Reason  string
	Regions []string
}

// decideAutoRegion 是自动设置的全部判定，纯函数。
func decideAutoRegion(in autoRegionInput) autoRegionDecision {
	p := in.Params
	wait := func(reason string) autoRegionDecision {
		return autoRegionDecision{Action: autoRegionWait, Reason: reason}
	}
	// 观察期按 7 天窗口里的实质使用算：第一次实质使用要在观察期之前，最近也要在用。两头一起
	// 要求，一次会话（比如管理员新建入站后自己测了一会儿）就不可能满足——否则 24 小时后会按
	// 管理员所在的省设置，真正的用户一连上就被拦；库里几周前的旧记录也不能替这次的使用凑观察期。
	spanRows := in.SpanRows
	if spanRows == nil {
		spanRows = in.Rows
	}
	first, last := substantialSpan(spanRows)
	if first == 0 || in.Now.Unix()-first < int64(observeDaysWithinRetention(p))*86400 {
		return wait(fmt.Sprintf("观察中（需要满 %d 天的使用记录）", p.ObserveDays))
	}
	if in.Now.Unix()-last > int64(autoRegionRecent(p)/time.Second) {
		return wait("最近没有在用，等用户再用的时候判断")
	}
	sug := suggestRegions(in.Rows)
	if len(sug.Suggested) == 0 {
		return wait("近 7 天没有可用的归属地数据")
	}
	var signs []string
	if len(sug.Coexisting) > 0 {
		signs = append(signs, "异地同时使用（"+strings.Join(sug.Coexisting, "、")+"）")
	}
	switch in.RiskLevel {
	case RiskLevelMedium, RiskLevelHigh, RiskLevelSevere:
		signs = append(signs, "共享风险偏高")
	}
	target := sug.Suggested
	reason := "使用记录稳定，只在" + target[0] + "使用"
	if len(sug.Suggested) >= 2 {
		if in.Now.Unix()-first < int64(autoRegionMultiMinSpan/time.Second) {
			return wait("用到了 " + strings.Join(sug.Suggested, "、") + "，满 3 天再判断哪个是常用地")
		}
		target = []string{dominantProvince(in.Rows, sug.Suggested, in.Loc)}
		reason = "用到了 " + strings.Join(sug.Suggested, "、") + "，按最常用的" + target[0] + "设置，其余省份靠自动出差放行"
	}
	// 设置的那个省自己要先用满「稳定使用天数」，按执行同口径的允许集数——设置之后自动出差放行的 stableHomeDays
	// 看到的就是这个数。锁得比出差放行可用得早，中间离开的人只能被一直拦着；开通第一天恰好在外地的人还会被锁在
	// 外地省：那个省永远凑不够天数，又没发过通行证，长住改地区也没有依据，回家以后一直进不来。
	// 这条取代了改动前的「稳定天数不超过观察天数、所有省份合计」（观察期 1 天时只要 1 天数据就锁）。
	if in.AllowedSet == nil {
		return autoRegionDecision{Action: autoRegionUnsure, Reason: "无法核对稳定使用天数"}
	}
	set, err := in.AllowedSet(target)
	if err != nil {
		return autoRegionDecision{Action: autoRegionUnsure, Reason: "无法核对稳定使用天数: " + err.Error()}
	}
	var targetRows []model.InboundIPHour
	for _, r := range in.Rows {
		if set.Contains(r.IP) {
			targetRows = append(targetRows, r)
		}
	}
	if days := stableHomeDays(targetRows, in.Loc); days < p.StableDays {
		return wait(fmt.Sprintf("%s近 7 天有实质使用的天数不足 %d 天（现在 %d 天）：自动出差放行要用满这么多天才能用，满了再设置",
			target[0], p.StableDays, days))
	}
	pct, err := in.UnplaceablePercent(target)
	if err != nil {
		return autoRegionDecision{Action: autoRegionUnsure, Reason: "无法预演: " + err.Error()}
	}
	if pct > float64(p.MaxBlockedPercent) && len(signs) == 0 {
		// 没有共享迹象，被拦的又多半是本人（常在境外、用 IPv6）：先不设，等这部分流量滑出 7 天窗口。
		return wait(fmt.Sprintf("设置为%s后，近 7 天会有 %.0f%% 的流量被拦、而且自动出差放行也救不了（境外、IPv6 或归属地不明，上限 %d%%），继续观察",
			target[0], pct, p.MaxBlockedPercent))
	}
	if in.AccessLogOff {
		return wait("访问日志没开：设置之后用户出省被拦，既不会被自动出差放行，也没人看得到被拦记录；先不设置")
	}
	if len(signs) > 0 {
		reason += "；发现" + strings.Join(signs, "、") + "，只放一个省"
	}
	return autoRegionDecision{Action: autoRegionApply, Reason: reason, Regions: target}
}

// dominantProvince 在 candidates 里挑「最常用」的省：有实质流量（单小时 ≥ coexistMinActiveBytes）的天数
// 最多；平局比活跃时长；再平按省名，结果确定。这是用到多个省时的全部防共享押注——本人多半是用得最
// 持续的那个，共享者用得再狠也很难天天都在。
func dominantProvince(rows []model.InboundIPHour, candidates []string, loc *time.Location) string {
	type stat struct {
		days    map[string]bool
		seconds int
	}
	stats := map[string]*stat{}
	for _, c := range candidates {
		stats[c] = &stat{days: map[string]bool{}}
	}
	for _, r := range rows {
		st, ok := stats[r.Province]
		if !ok {
			continue
		}
		st.seconds += r.ActiveSeconds
		if r.ActiveBytes >= coexistMinActiveBytes {
			st.days[time.Unix(r.HourStart, 0).In(loc).Format("2006-01-02")] = true
		}
	}
	best := ""
	for _, c := range candidates {
		if best == "" {
			best = c
			continue
		}
		a, b := stats[c], stats[best]
		switch {
		case len(a.days) != len(b.days):
			if len(a.days) > len(b.days) {
				best = c
			}
		case a.seconds != b.seconds:
			if a.seconds > b.seconds {
				best = c
			}
		case c < best:
			best = c
		}
	}
	return best
}

// unplaceableShare 是按 set 设置之后会被拦、而且自动出差放行也救不了的流量占近 7 天的百分比：来源在允许集
// 之外、又查不到中国的省份（境外、IPv6、归属地不明）。
func unplaceableShare(rows []model.InboundIPHour, set *cidrSet) float64 {
	var total, stuck int64
	for _, r := range rows {
		total += r.ActiveBytes
		if r.Province == "" && !set.Contains(r.IP) {
			stuck += r.ActiveBytes
		}
	}
	if total == 0 {
		return 0
	}
	return float64(stuck) * 100 / float64(total)
}

// eligibleForAutoRegion 判断入站本轮要不要判定：启用、没有地区限制、管理员没接手、
// 也没被自动设置过（自动设置过又变成没有地区，只可能是管理员清掉的）。
// autoRegionAllowedSQL 是自动写地区时附在 UPDATE 上的条件：读完入站到写入之间管理员打开了「不自动设置地区
// 限制」也要让路。按 coalesce 写：老库加列后存量行是 NULL。
const autoRegionAllowedSQL = "coalesce(no_auto_region, 0) = 0"

func eligibleForAutoRegion(in *model.Inbound, st *model.InboundAutoRegion) bool {
	if !in.Enable || in.NoAutoRegion || model.HasRegions(in.Regions) {
		return false
	}
	return st == nil || st.State == model.AutoRegionPending
}

// AutoRegionService 无状态；置重启标志由调用方负责。
type AutoRegionService struct {
	settingService SettingService
}

// States 返回全部入站的自动设置状态。
func (s *AutoRegionService) States() (map[int]*model.InboundAutoRegion, error) {
	var rows []*model.InboundAutoRegion
	if err := database.GetDB().Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int]*model.InboundAutoRegion, len(rows))
	for _, r := range rows {
		out[r.InboundId] = r
	}
	return out, nil
}

func (s *AutoRegionService) stateOf(id int) (*model.InboundAutoRegion, error) {
	var rows []model.InboundAutoRegion
	if err := database.GetDB().Where("inbound_id = ?", id).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ErrAutoRegionNoIPDB 表示总开关开着、IP 归属地库却没加载：给不出建议，也预演不了。
var ErrAutoRegionNoIPDB = errors.New("IP 归属地库未加载，自动设置地区限制暂停；请到「面板设置 → IP 归属地库」更新")

// errAutoRegionStale 表示从读到状态到动手之间，状态或地区已被别处改过：管理员刚做了操作，
// 或者任务刚自动设置了。任务遇到它静默跳过；管理员的操作把它原样报出去，请他刷新再看。
var errAutoRegionStale = errors.New("这个入站的自动设置状态刚刚变了（可能刚被自动设置，或已在别处处理），请刷新页面后再操作")

// MarkManual 把入站标成「管理员接手」。这是管理员亲手改地区时调用的，无条件写入：管理员的决定优先。
func (s *AutoRegionService) MarkManual(tx *gorm.DB, id int, now time.Time) error {
	// 不碰 adopted_at / relocated_at：抹掉了，接管会再打开一次管理员关掉的自动出差放行，「30 天内只自动改去
	// 一个新省」也会被一次手工编辑绕开。
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "inbound_id"}},
		DoUpdates: clause.Assignments(map[string]any{"state": model.AutoRegionManual, "regions": "",
			"prev_regions": "", "reason": "", "updated_at": now.UnixMilli()}),
	}).Create(&model.InboundAutoRegion{InboundId: id, State: model.AutoRegionManual, UpdatedAt: now.UnixMilli()}).Error
}

// moveAutoRegionState 只在状态仍为 from 时写入 st，返回是否写入。allowNew 为真时「还没有行」
// 也算 from——没有行表示还在观察，只有任务的写入走这条。
//
// 这是比较后写入，不是先读再写：任务一轮要逐个入站查库、预演，开头读到的状态快照可能早已过时，
// 管理员在这期间点了忽略、确认，或在表单里改过地区（都会变成 manual）。拿旧快照无条件写，
// 就会把管理员的决定改回 pending / applied。
func moveAutoRegionState(tx *gorm.DB, st *model.InboundAutoRegion, from string, allowNew bool) (bool, error) {
	if !allowNew {
		res := tx.Model(&model.InboundAutoRegion{}).Where("inbound_id = ? AND state = ?", st.InboundId, from).
			Updates(map[string]any{"state": st.State, "regions": st.Regions, "reason": st.Reason, "updated_at": st.UpdatedAt})
		return res.RowsAffected > 0, res.Error
	}
	res := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "inbound_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"state", "regions", "reason", "updated_at"}),
		Where: clause.Where{Exprs: []clause.Expression{
			clause.Expr{SQL: "inbound_auto_regions.state = ?", Vars: []interface{}{from}},
		}},
	}).Create(st)
	return res.RowsAffected > 0, res.Error
}

// whereRegionsStill 限定「入站的地区字段仍是 v」。v 为空串时库里可能是 NULL：地区列是后加的，
// 老行没有值，GORM 读出来是空串。
func whereRegionsStill(q *gorm.DB, v string) *gorm.DB {
	if v == "" {
		return q.Where("(regions IS NULL OR regions = '')")
	}
	return q.Where("regions = ?", v)
}

// checkStaleAutoRegionWrite 拦住「页面停在自动设置之前」的整行写回，在 UpdateInbound 的事务里调用。
//
// 入站列表只在打开页面和管理员自己操作之后刷新；切换启用、重置流量、编辑保存都会把整行写回，
// 带着自动设置之前的空地区。放过去的话，刚设置的地区被清掉、自动出差被关掉，入站还会被当成
// 「管理员改过地区」而永远不再自动设置——管理员其实什么都没做，列表上只是少了一个标签。
//
// 所以自动设置后 autoRegionAppliedVisible 之内（列表上有撤销入口的这段时间），清空地区只能走
// 「撤销自动设置」。过了这段时间撤销入口已经不在列表上，表单是唯一的修改途径，不再拦。
// 带着地区的写回（刷新过的表单，或管理员明确改成了别的省）照常放行。
func checkStaleAutoRegionWrite(tx *gorm.DB, id int, incoming string, now time.Time) error {
	var rows []model.InboundAutoRegion
	if err := tx.Where("inbound_id = ?", id).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 || rows[0].State != model.AutoRegionApplied ||
		now.UnixMilli()-rows[0].UpdatedAt >= autoRegionAppliedVisible.Milliseconds() {
		return nil
	}
	var stored sql.NullString
	if err := tx.Model(&model.Inbound{}).Where("id = ?", id).Select("regions").Row().Scan(&stored); err != nil {
		return err
	}
	if !model.HasRegions(stored.String) || !sameRegions(stored.String, rows[0].Regions) {
		return nil
	}
	// 写回的是自动写入之前的值才是过期页面：没有地区（首次设置之前），或原来的省（长住改地区之前）。
	// 管理员明确改成别的省照常放行。
	prev := rows[0].PrevRegions
	if model.HasRegions(incoming) && !(model.HasRegions(prev) && sameRegions(incoming, prev)) {
		return nil
	}
	regions, _ := DecodeRegions(rows[0].Regions)
	// 与入站列表上的标签同名：长住改地区（有原来的地区）显示「地区：自动改过」，首次设置显示「地区：自动设置」。
	chip := "地区：自动设置"
	if model.HasRegions(prev) {
		chip = "地区：自动改过"
	}
	return common.NewError("这个入站的地区刚被自动设置为「" + strings.Join(regions, "、") +
		"」，页面上还是设置之前的数据。请刷新页面后再操作；要取消自动设置，请点入站行上的「" + chip + "」撤销，" +
		"或在编辑里打开「不自动设置地区限制」再保存")
}

// DeleteByInbound 在删除入站的事务里删掉它的状态。
func (s *AutoRegionService) DeleteByInbound(tx *gorm.DB, id int) error {
	return tx.Where("inbound_id = ?", id).Delete(&model.InboundAutoRegion{}).Error
}

// writeRegions 给一个仍然没有地区限制的入站写入 regions 并打开自动出差放行（需要认证的入站），同一
// 事务里把状态从 pending（升级前的遗留；allowNew 时「还没有行」也算）推进到 st。写之前过保存时的同一道校验。
//
// 两步都是比较后写入：地区必须仍是刚读到的值，状态必须仍是遗留的 pending 或没有行。任何一步对不上就
// 整体回滚、返回 errAutoRegionStale。
func writeRegions(id int, regions []string, st *model.InboundAutoRegion, allowNew bool) error {
	in, err := (&InboundService{}).GetInbound(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errAutoRegionStale // 本轮开头之后被删了
	}
	if err != nil {
		return err
	}
	if model.HasRegions(in.Regions) || in.NoAutoRegion {
		return errAutoRegionStale
	}
	encoded, err := EncodeRegions(regions)
	if err != nil {
		return err
	}
	if err := regionsUsable(encoded, in.Regions); err != nil {
		return err
	}
	st.Regions = encoded
	cols := map[string]any{"regions": encoded}
	if inboundRequiresAuth(in) {
		// 无认证的入站不打开：扫描器的连接也是「已接受」，自动出差放行会给扫描器所在的省发通行证。
		cols["auto_travel"] = true
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		res := whereRegionsStill(tx.Model(&model.Inbound{}).Where("id = ?", id), in.Regions).
			Where(autoRegionAllowedSQL).UpdateColumns(cols)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errAutoRegionStale
		}
		moved, err := moveAutoRegionState(tx, st, model.AutoRegionPending, allowNew)
		if err != nil {
			return err
		}
		if !moved {
			return errAutoRegionStale
		}
		if _, on := cols["auto_travel"]; on {
			// 系统替它打开了自动出差放行：记下来，管理员之后关掉（再改地区写 manual）也不会被接管重新打开。
			return tx.Model(&model.InboundAutoRegion{}).Where("inbound_id = ? AND adopted_at = 0", id).
				UpdateColumn("adopted_at", st.UpdatedAt).Error
		}
		return nil
	})
}

// Undo 撤销一次自动设置：清掉地区、关闭自动出差、结束活动通行证，之后由管理员接手。
func (s *AutoRegionService) Undo(id int, now time.Time) error {
	st, err := s.stateOf(id)
	if err != nil {
		return err
	}
	if st == nil || st.State != model.AutoRegionApplied {
		return common.NewError("这个入站的地区不是自动设置的，请在编辑里修改")
	}
	in, err := (&InboundService{}).GetInbound(id)
	if err != nil {
		return err
	}
	// 首次自动设置撤销后回到没有地区；长住改地区撤销后恢复原来的省。两种都关掉自动出差放行：
	// 管理员撤销就是不要这个入站的自动化，否则过几天又会被放行、又会被改回来。
	restore := "[]"
	if model.HasRegions(st.PrevRegions) {
		restore = st.PrevRegions
	}
	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		res := whereRegionsStill(tx.Model(&model.Inbound{}).Where("id = ?", id), in.Regions).
			UpdateColumns(map[string]any{"regions": restore, "auto_travel": false})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errAutoRegionStale
		}
		moved, err := moveAutoRegionState(tx, &model.InboundAutoRegion{InboundId: id, State: model.AutoRegionManual,
			Reason: "管理员撤销了自动设置", UpdatedAt: now.UnixMilli()}, model.AutoRegionApplied, false)
		if err != nil {
			return err
		}
		if !moved {
			return errAutoRegionStale
		}
		// 撤销关掉了自动出差放行，接管不能再把它打开：长住改地区可能抢在第一次接管之前（AdoptedAt 还是 0），
		// 这一行变成 manual 之后正好符合「manual 且没接管过」。记下 AdoptedAt，算作系统已经替它打开过一次。
		if err := tx.Model(&model.InboundAutoRegion{}).Where("inbound_id = ? AND adopted_at = 0", id).
			UpdateColumn("adopted_at", now.UnixMilli()).Error; err != nil {
			return err
		}
		// 生效中的通行证同一个事务里结束：放到事务外、失败只记日志的话，它会留下来，以后再设
		// 地区时接着放行。
		return endActive(tx, id, model.RegionPassCleared, now)
	})
	if err != nil {
		return err
	}
	return nil
}

// earliestHoursOf 一次查出这些入站各自最早一条用量记录的小时（Unix 秒）；没有记录的不在结果里。
//
// 一轮只查一次：大多数候选入站是还没开始用、或用得很少的，逐个查的话每 10 分钟就是
// 一串查询，而绝大多数结论都是「继续观察」。
func earliestHoursOf(ids []int) (map[int]int64, error) {
	out := map[int]int64{}
	db := database.GetTrafficDB()
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	// 自己 Scan 聚合值：GORM 的 Scan 往 *int64 里写聚合列时什么都不写，曾让最早时刻恒为 0、
	// 所有用户永远停在「观察中」。
	rows, err := db.Model(&model.InboundIPHour{}).Select("inbound_id, min(hour_start)").
		Where("inbound_id IN ?", ids).Group("inbound_id").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var earliest sql.NullInt64
		if err := rows.Scan(&id, &earliest); err != nil {
			return nil, err
		}
		if earliest.Valid {
			out[id] = earliest.Int64
		}
	}
	return out, rows.Err()
}

// autoRegionObserved 是观察期的必要条件：库里最早一条用量记录距今至少 ObserveDays 天。Run 用它
// 跳过明显还没满的入站、省一次读明细；真正的判定在 decideAutoRegion 里按窗口内的实质使用算。
func autoRegionObserved(earliest int64, now time.Time, p autoRegionParams) bool {
	return earliest != 0 && now.Unix()-earliest >= int64(observeDaysWithinRetention(p))*86400
}

// observeDaysWithinRetention 是实际能量出来的观察天数：按来源 IP 的小时行只保留 sharingRetentionDays 天，
// 观察天数设成保留期本身时「第一次实质使用距今满 30 天」永远达不到，按保留期内最长能量到的天数判。
func observeDaysWithinRetention(p autoRegionParams) int {
	return min(p.ObserveDays, sharingRetentionDays-1)
}

// autoRegionRows 读一个入站自动设置要用的两段行：近 7 天（建议、稳定天数、会被拦的占比）与观察期窗口
// （观察天数 + 1 天，至少 7 天，只用来量观察期）。观察期不长于 7 天时两段是同一份。
func autoRegionRows(inboundId int, p autoRegionParams, now time.Time) (rows, spanRows []model.InboundIPHour, err error) {
	days := max(sharingWindowDays, observeDaysWithinRetention(p)+1)
	spanRows, err = (&SharingService{}).windowRows(inboundId, days, now)
	if err != nil || days == sharingWindowDays {
		return spanRows, spanRows, err
	}
	cutoff := now.Add(-time.Duration(sharingWindowDays) * 24 * time.Hour).Unix()
	for _, r := range spanRows {
		if r.HourStart >= cutoff {
			rows = append(rows, r)
		}
	}
	return rows, spanRows, nil
}

// autoRegionRecent 是「最近也在用」的窗口：不超过观察期的一半（最多 24 小时），这样一次会话
// 不可能既早于观察期、又落在这个窗口里。
func autoRegionRecent(p autoRegionParams) time.Duration {
	return min(24*time.Hour, time.Duration(p.ObserveDays)*12*time.Hour)
}

// substantialSpan 返回实质流量（单小时 ≥ coexistMinActiveBytes）的第一与最后一个小时（Unix 秒），
// 没有时都是 0。
func substantialSpan(rows []model.InboundIPHour) (first, last int64) {
	for _, r := range rows {
		if r.ActiveBytes < coexistMinActiveBytes {
			continue
		}
		if first == 0 || r.HourStart < first {
			first = r.HourStart
		}
		last = max(last, r.HourStart)
	}
	return first, last
}

// Run 跑一轮自动设置，返回是否写入了地区（调用方据此置重启标志）。总开关开着而 IP 库
// 没加载时返回 ErrAutoRegionNoIPDB。
//
// 单个入站出错（读不出用量、写入失败）只记日志、继续下一个，不拖累其余入站。
func (s *AutoRegionService) Run(now time.Time) (bool, error) {
	// 入站被删后残留的行（删除恰好插在一轮判定的中间时会留下）：SQLite 会复用 id，留着会被
	// 下一个新建的入站继承。
	if err := database.GetDB().Where("inbound_id NOT IN (?)", database.GetDB().Model(&model.Inbound{}).Select("id")).
		Delete(&model.InboundAutoRegion{}).Error; err != nil {
		logger.Warning("自动设置地区限制：清理残留状态失败:", err)
	}
	// 升级前留下的「待确认」行：现在全部判断自动做，没有人会去确认它，留着只会一直挂在列表上。
	if err := database.GetDB().Where("state = ?", model.AutoRegionPending).Delete(&model.InboundAutoRegion{}).Error; err != nil {
		logger.Warning("自动设置地区限制：清理待确认的遗留状态失败:", err)
	}
	enabled, params := s.settingService.GetAutoRegionParams()
	if !enabled {
		return false, nil
	}
	inbounds, err := (&InboundService{}).GetAllInbounds()
	if err != nil {
		return false, err
	}
	states, err := s.States()
	if err != nil {
		return false, err
	}
	s.adopt(inbounds, states, now)
	if (&IPDBService{}).DB() == nil {
		return false, ErrAutoRegionNoIPDB
	}
	var candidates []*model.Inbound
	var ids []int
	for _, in := range inbounds {
		if eligibleForAutoRegion(in, states[in.Id]) {
			candidates = append(candidates, in)
			ids = append(ids, in.Id)
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return false, err
	}
	earliest, err := earliestHoursOf(ids)
	if err != nil {
		return false, err
	}
	risk, err := (&SharingRiskService{}).Summary()
	if err != nil {
		logger.Warning("自动设置地区：读取共享风险失败，本轮跳过:", err)
		return false, nil
	}
	logOn, err := s.settingService.GetAccessLogEnable()
	if err != nil {
		logger.Warning("自动设置地区：读取访问日志开关失败，按未开启处理:", err)
	}

	// 同一组省份的允许集一轮只建一次：建议多半落在少数几个省上，而建允许集要从 IP 库
	// 合并上千个段。
	sets := map[string]*cidrSet{}
	allowed := func(regions []string) (*cidrSet, error) {
		key := strings.Join(regions, "\x00")
		if set, ok := sets[key]; ok {
			return set, nil
		}
		set, err := allowedSetFor(regions)
		if err != nil {
			return nil, err
		}
		sets[key] = set
		return set, nil
	}

	changed := false
	for _, in := range candidates {
		st := states[in.Id]
		d := autoRegionDecision{Action: autoRegionWait}
		// 观察期没满就不用读明细：结论只能是继续观察。
		if autoRegionObserved(earliest[in.Id], now, params) {
			rows, spanRows, err := autoRegionRows(in.Id, params, now)
			if err != nil {
				logger.Warning("自动设置地区限制：读取用量失败，跳过入站", in.Id, ":", err)
				continue
			}
			d = decideAutoRegion(autoRegionInput{
				Now: now, Loc: loc, Rows: rows, SpanRows: spanRows, AccessLogOff: !logOn,
				RiskLevel: risk[in.Id].Level, Params: params,
				UnplaceablePercent: func(regions []string) (float64, error) {
					set, err := allowed(regions)
					if err != nil {
						return 0, err
					}
					return unplaceableShare(rows, set), nil
				},
				AllowedSet: allowed,
			})
		}
		applied, err := s.applyDecision(in, st, d, now)
		if err != nil {
			logger.Warning("自动设置地区限制失败, 入站", in.Id, ":", err)
		}
		changed = changed || applied
	}
	return changed, nil
}

// applyDecision 把一个入站本轮的判定落库，返回是否写入了地区。wait 与 unsure 什么都不写：没有行就表示
// 还在观察。
//
// 真正的写入一律以库里此刻的状态为准（writeRegions 是比较后写入）：管理员在这期间接手了，就静默跳过。
func (s *AutoRegionService) applyDecision(in *model.Inbound, st *model.InboundAutoRegion, d autoRegionDecision, now time.Time) (bool, error) {
	switch d.Action {
	case autoRegionApply:
		err := writeRegions(in.Id, d.Regions, &model.InboundAutoRegion{InboundId: in.Id,
			State: model.AutoRegionApplied, Reason: d.Reason, UpdatedAt: now.UnixMilli()}, true)
		if errors.Is(err, errAutoRegionStale) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		logger.Info("自动设置地区限制：入站", in.Id, "设置为", strings.Join(d.Regions, "、"))
		return true, nil
	}
	return false, nil // wait / unsure：这一轮不写
}

// adopt 接管总开关打开时已经有地区限制、从没被系统处理过（没有状态行）的启用入站：打开自动出差放行，
// 记一行 adopted。只做一次——管理员之后在表单里关掉它，有状态行就不再改回来。v1.47.0 的地区限制都是
// 手工设置的、自动出差放行默认关着，不接管的话「全程自动」碰不到任何一个存量用户。
//
// 无认证的入站不接管（见 inboundRequiresAuth）。打开自动出差放行不改 xray 配置，不需要重启。
func (s *AutoRegionService) adopt(inbounds []*model.Inbound, states map[int]*model.InboundAutoRegion, now time.Time) {
	for _, in := range inbounds {
		if !in.Enable || !model.HasRegions(in.Regions) || !inboundRequiresAuth(in) {
			continue
		}
		st := states[in.Id]
		// 还没有行，或者是管理员在打开总开关之前改过地区留下的 manual 行（系统还没替它打开过）：接管一次。
		// 其余的行（接管过、自动设置过、或系统替它打开过之后的 manual）都不再碰。
		if st != nil && (st.State != model.AutoRegionManual || st.AdoptedAt != 0) {
			continue
		}
		err := database.GetDB().Transaction(func(tx *gorm.DB) error {
			if st == nil {
				// 管理员同时在编辑里改了地区（写 manual）：他的行在前就什么都不做，下一轮按 manual 行处理。
				res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.InboundAutoRegion{InboundId: in.Id,
					State: model.AutoRegionAdopted, Regions: in.Regions, Reason: "总开关打开时已有地区限制，打开了自动出差放行",
					UpdatedAt: now.UnixMilli(), AdoptedAt: now.UnixMilli()})
				if res.Error != nil || res.RowsAffected == 0 {
					return res.Error
				}
			} else {
				res := tx.Model(&model.InboundAutoRegion{}).
					Where("inbound_id = ? AND state = ? AND adopted_at = 0", in.Id, model.AutoRegionManual).
					UpdateColumn("adopted_at", now.UnixMilli())
				if res.Error != nil || res.RowsAffected == 0 {
					return res.Error
				}
			}
			res := whereRegionsStill(tx.Model(&model.Inbound{}).Where("id = ?", in.Id), in.Regions).
				UpdateColumn("auto_travel", true)
			if res.Error != nil {
				return res.Error
			}
			// 入站在这一轮开头之后被删了（或地区刚被改了）：整个回滚，不留状态行。被删的入站留下的
			// adopted 行会被复用这个 id 的新入站继承，它从此永远不会被自动设置地区。
			if res.RowsAffected == 0 {
				return errAutoRegionStale
			}
			return nil
		})
		if errors.Is(err, errAutoRegionStale) {
			continue
		}
		if err != nil {
			logger.Warning("自动管理地区限制：接管入站", in.Id, "失败:", err)
		} else if !in.AutoTravel {
			logger.Info("自动管理地区限制：接管入站", in.Id, "，打开自动出差放行")
		}
	}
}
