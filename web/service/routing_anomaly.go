package service

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
)

// 访问日志持续扫描（审计设计 §4）：增量读访问日志，把「属于某条规则却没走它期望出站」
// 的连接按规则汇总成最近 24 小时的异常摘要。只在内存里维护，不写库。
//
// 判定口径与流量走向图的日志证据（buildLogEvidence）一致：行的入站在规则覆盖范围内、
// 目标命中规则域名组（xray 自己的匹配器）、routeHead(Route) 经 judgeSample 判为不符，
// 且只看 Accepted=true 的行。被更高优先级规则遮蔽的域名同样记为下层规则的异常——
// 「这条规则在这个域名上没生效」正是要告诉管理员的事，这里不重新推导规则先后。
// 两类不是分流规则造成的连接不计（与 buildLogEvidence 同口径）：被入站地区限制拦下的
// 封禁（geoBlockMatcher），以及核心没命中任何规则（NoRuleHit，核心看的是嗅探出的另一个域名）。
//
// 小机器约束（§4.3）：规则按引用的全部域名组合并后的条目数分两档。小规则的匹配器
// 按代次常驻缓存，每分钟扫描；大规则由每 10 分钟的任务扫描，匹配器取自与路由审计共用的
// 常驻槽（只留一个，条目或 dat 变了才重建，见 routing_large_matcher.go）。两档各有独立的
// (time,id) 游标。
//
// 窗口边界：摘要按小时分桶，丢弃的是早于 now-24h 所在小时的桶，所以列表上的窗口实际是
// 24~25 小时；流量走向图的日志证据从精确的 now-24h 起算。两边在窗口边缘最多差出一小时
// 的连接，这是分桶换来的内存上界，不去对齐。
//
// 游标按 (time, id) 前进：系统时钟向回跳时，回跳后写入的行 time 比游标小，会被跳过
// 直到时钟追过游标——这段时间的走错不会进摘要（流量走向图按需查询不受影响）。
//
// 锁：扫描不取 verifyMu（不与核心验证互斥），也不碰包级 lock（不取运行时快照）。

const (
	auditLargeRuleEntries = 20000 // 合并后条目数超过它即为大规则
	anomalyMaxUsers       = 50    // 每条规则最多展开的用户数
	anomalyMaxRoutes      = 5     // 每个用户最多展开的出站数
	anomalyWindow         = verifyLogWindowHours * time.Hour
	anomalyBucketMs       = int64(time.Hour / time.Millisecond)
)

// auditLargeThreshold 是大小规则的分界，包级变量只为测试能把它调小。
var auditLargeThreshold = auditLargeRuleEntries

// UserAnomaly 是一个用户（入站）在窗口内走错的连接统计；Routes 按 routeHead 计数，
// 最多 anomalyMaxRoutes 个，超出的出站只计入 Count。
type UserAnomaly struct {
	Count  int64            `json:"count"`
	LastAt int64            `json:"lastAt"`
	Routes map[string]int64 `json:"routes"`
}

// RuleAnomaly 是一条规则最近 24 小时的异常摘要。ByUser 最多 anomalyMaxUsers 个，
// 超出的用户只计入规则的 Count/LastAt。
type RuleAnomaly struct {
	Count  int64                `json:"count"`
	LastAt int64                `json:"lastAt"`
	ByUser map[int]*UserAnomaly `json:"byUser"`
}

// logCursor 是访问日志里的一个位置，按 (Time, Id) 字典序比较。
type logCursor struct {
	Time, Id int64
}

func (c logCursor) less(o logCursor) bool {
	return c.Time < o.Time || (c.Time == o.Time && c.Id < o.Id)
}

// 扫描档位，用作游标与扫描锁的下标。
const (
	anomalyClassSmall = 0
	anomalyClassLarge = 1
)

type anomalyUserBucket struct {
	count, lastAt int64
	routes        map[string]int64
}

// anomalyBucket 是一条规则一个小时的累计。写入时就按上限截断（用户、出站），
// 内存因此有界：每条规则至多 25 个桶 × 50 用户 × 5 出站。
type anomalyBucket struct {
	count, lastAt int64
	byUser        map[int]*anomalyUserBucket
}

func newAnomalyBucket() *anomalyBucket {
	return &anomalyBucket{byUser: map[int]*anomalyUserBucket{}}
}

// add 记一条走错的连接。超出用户上限的只计入桶总数；超出出站上限的计入用户总数但不展开。
func (b *anomalyBucket) add(inboundId int, route string, count, lastAt int64) {
	b.count += count
	b.lastAt = max(b.lastAt, lastAt)
	u := b.byUser[inboundId]
	if u == nil {
		if len(b.byUser) >= anomalyMaxUsers {
			return
		}
		u = &anomalyUserBucket{routes: map[string]int64{}}
		b.byUser[inboundId] = u
	}
	u.count += count
	u.lastAt = max(u.lastAt, lastAt)
	if _, ok := u.routes[route]; ok || len(u.routes) < anomalyMaxRoutes {
		u.routes[route] += count
	}
}

// merge 把 src 并进 b，按用户 id 升序，使截断结果确定。
func (b *anomalyBucket) merge(src *anomalyBucket) {
	b.count += src.count
	b.lastAt = max(b.lastAt, src.lastAt)
	ids := make([]int, 0, len(src.byUser))
	for id := range src.byUser {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		su := src.byUser[id]
		u := b.byUser[id]
		if u == nil {
			if len(b.byUser) >= anomalyMaxUsers {
				continue
			}
			u = &anomalyUserBucket{routes: map[string]int64{}}
			b.byUser[id] = u
		}
		u.count += su.count
		u.lastAt = max(u.lastAt, su.lastAt)
		routes := make([]string, 0, len(su.routes))
		for r := range su.routes {
			routes = append(routes, r)
		}
		sort.Strings(routes)
		for _, r := range routes {
			if _, ok := u.routes[r]; ok || len(u.routes) < anomalyMaxRoutes {
				u.routes[r] += su.routes[r]
			}
		}
	}
}

// anomalyBatchRows 是每档每轮最多读的行数，读不完下一轮继续。用 var 仅为让测试调小。
var anomalyBatchRows = 20000

// 包级扫描状态，全部由 anomalyMu 保护；持锁期间只做内存操作，不读库、不建匹配器。
var (
	anomalyMu          sync.Mutex
	anomalySummaries   = map[int]map[int64]*anomalyBucket{} // ruleId → 小时 key → 桶
	anomalyCursors     [2]logCursor                         // 两档各自的游标
	anomalyCursorEpoch uint64                               // 清空时递增，扫描据此丢弃清空前读出的批次
	anomalyWatermarks  = map[int]logCursor{}                // 每条规则已计到的位置
	anomalyIgnore      = map[int]int64{}                    // ruleId → 忽略时刻（毫秒）
	// 最近一次大规则扫描是否读满了一整批（游标可能落后于日志写入）。
	anomalyLargeBacklogged bool
)

// anomalyScanMu 串行化同一档的扫描；两档之间可以并行。
var anomalyScanMu [2]sync.Mutex

// anomalyRulePlan 是一条规则的扫描计划，构建后只读。
type anomalyRulePlan struct {
	ruleId     int
	expected   ExpectedOutbound
	defaultTag string
	coverage   map[int]bool // 规则实际覆盖的启用入站
	groupIds   []int        // 实际参与的组（buildRule 剔掉的坏组、空组不在内）
	groupKey   string
	large      bool
	matcher    *groupMatcher // 只有小规则有；大规则扫描时临时构建
}

// anomalyPlanSet 是按（代次 + 启用规则集合）缓存的全部规则计划。
type anomalyPlanSet struct {
	gen        uint64
	sig        string
	rules      []*anomalyRulePlan
	matchers   map[string]*groupMatcher // 小规则的匹配器，按组序列共享
	enabledIds map[int]bool
	// geo 判断封禁连接是否被入站地区限制拦下。地区设置或允许集变化都会改核心配置、
	// 推进代次，计划随之重建，所以与规则计划同一个缓存口径。
	geo *geoBlockMatcher
}

var (
	anomalyPlanMu sync.Mutex
	anomalyPlans  *anomalyPlanSet
)

// anomalyIgnoreBefore 返回规则的忽略时刻（毫秒），没有则为 0。Verify 的日志窗口起点
// 取 max(now-24h, 它)，与列表上的异常摘要同口径。
func anomalyIgnoreBefore(ruleId int) int64 {
	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	return anomalyIgnore[ruleId]
}

// LargeScanBacklogged 报告最近一次大规则扫描是否读满了一整批。读满说明还有未读的行，
// 调用方应在下一轮直接再扫一次，而不是等满 10 分钟间隔。
func (s *RoutingAuditService) LargeScanBacklogged() bool {
	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	return anomalyLargeBacklogged
}

// ResetAnomalies 是「忽略此前的异常」：清空该规则的摘要并记下忽略时刻。
// 只清内存摘要，不删访问日志。忽略时刻同样只在内存里：面板重启后的首轮回填
// 会重新统计重启前 24 小时的连接，这是「不写库」的代价。
func (s *RoutingAuditService) ResetAnomalies(ruleId int, now time.Time) {
	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	delete(anomalySummaries, ruleId)
	anomalyIgnore[ruleId] = now.UnixMilli()
}

// forgetRuleAuditState 丢掉一条规则在内存里的全部审计状态：异常摘要、水位、忽略时刻
// 与验证缓存。规则删除时、以及新插入的规则拿到 id 时调用（RoutingRuleService.Del / Add）。
//
// SQLite 会复用被删除的自增 id（CLAUDE.md「已知偏差」）。这些状态平时由下一轮扫描
// （pruneAnomaliesLocked）与审计（replaceAuditResults）按「规则已不在」清理；但删掉
// 最新那条规则后紧接着新建一条，新规则在清理之前就占住了同一个 id，清理从此不会
// 发生——新规则一出生就顶着旧规则最多 24 小时的「不符」、旧的忽略时刻与旧的验证结论。
// 新建时也清一次，是因为删除不一定走 Del（DetachInbound 在删入站的事务里整条删规则、
// 手工改库）：一个刚插入的 id，此前的任何状态都只可能属于一条已经不存在的规则。
//
// 推进 anomalyCursorEpoch：正在进行的扫描可能是按旧规则的计划算出的增量，合并时
// 因代次变化整批作废、游标不前进，下一轮按新计划重读这一批，不丢其他规则的数据。
func forgetRuleAuditState(ruleId int) {
	anomalyMu.Lock()
	delete(anomalySummaries, ruleId)
	delete(anomalyWatermarks, ruleId)
	delete(anomalyIgnore, ruleId)
	anomalyCursorEpoch++
	anomalyMu.Unlock()

	auditMu.Lock()
	delete(auditResults, ruleId)
	auditMu.Unlock()
}

func anomalyCutoffKey(now time.Time) int64 {
	return now.Add(-anomalyWindow).UnixMilli() / anomalyBucketMs
}

// Anomaly 汇总一条规则在 now 往前 24 小时内的异常；用户按次数降序、id 升序截到上限，
// 出站按次数降序、名字升序截到上限。返回的 map 永远非 nil。
func (s *RoutingAuditService) Anomaly(ruleId int, now time.Time) RuleAnomaly {
	out := RuleAnomaly{ByUser: map[int]*UserAnomaly{}}
	cutoff := anomalyCutoffKey(now)
	users := map[int]*UserAnomaly{}

	anomalyMu.Lock()
	for key, b := range anomalySummaries[ruleId] {
		if key < cutoff {
			continue
		}
		out.Count += b.count
		out.LastAt = max(out.LastAt, b.lastAt)
		for id, ub := range b.byUser {
			u := users[id]
			if u == nil {
				u = &UserAnomaly{Routes: map[string]int64{}}
				users[id] = u
			}
			u.Count += ub.count
			u.LastAt = max(u.LastAt, ub.lastAt)
			for r, n := range ub.routes {
				u.Routes[r] += n
			}
		}
	}
	anomalyMu.Unlock()

	ids := make([]int, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := users[ids[i]], users[ids[j]]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return ids[i] < ids[j]
	})
	for i, id := range ids {
		if i >= anomalyMaxUsers {
			break
		}
		u := users[id]
		u.Routes = topRoutes(u.Routes, anomalyMaxRoutes)
		out.ByUser[id] = u
	}
	return out
}

// anomalyTotals 只取规则窗口内的总数与最近时刻，给 Summaries 用（不展开用户）。
func anomalyTotals(ruleId int, now time.Time) (count, lastAt int64) {
	cutoff := anomalyCutoffKey(now)
	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	for key, b := range anomalySummaries[ruleId] {
		if key >= cutoff {
			count += b.count
			lastAt = max(lastAt, b.lastAt)
		}
	}
	return count, lastAt
}

func topRoutes(routes map[string]int64, limit int) map[string]int64 {
	if len(routes) <= limit {
		return routes
	}
	names := make([]string, 0, len(routes))
	for r := range routes {
		names = append(names, r)
	}
	sort.Slice(names, func(i, j int) bool {
		if routes[names[i]] != routes[names[j]] {
			return routes[names[i]] > routes[names[j]]
		}
		return names[i] < names[j]
	})
	out := make(map[string]int64, limit)
	for _, r := range names[:limit] {
		out[r] = routes[r]
	}
	return out
}

// clearAnomaliesLocked 清空摘要、游标与水位（访问日志关闭时）。忽略时刻保留。
func clearAnomaliesLocked() {
	anomalySummaries = map[int]map[int64]*anomalyBucket{}
	anomalyCursors = [2]logCursor{}
	anomalyWatermarks = map[int]logCursor{}
	anomalyLargeBacklogged = false
	anomalyCursorEpoch++
}

func (s *RoutingAuditService) accessLogEnabled() (bool, error) {
	if v := s.verifier(); v.accessLogEnabled != nil {
		return v.accessLogEnabled()
	}
	return (&SettingService{}).GetAccessLogEnable()
}

// ScanAccessLog 扫描一轮访问日志。large=false 只处理小规则，large=true 只处理大规则。
// 访问日志关闭时清空摘要与游标并返回 nil；读开关失败返回 error、什么都不清
// （一次库锁不该抹掉 24 小时的摘要）。
func (s *RoutingAuditService) ScanAccessLog(now time.Time, large bool) error {
	class := anomalyClassSmall
	if large {
		class = anomalyClassLarge
	}
	anomalyScanMu[class].Lock()
	defer anomalyScanMu[class].Unlock()

	enabled, err := s.accessLogEnabled()
	if err != nil {
		return fmt.Errorf("读取访问日志开关失败：%w", err)
	}
	if !enabled {
		anomalyMu.Lock()
		clearAnomaliesLocked()
		anomalyMu.Unlock()
		return nil
	}

	plans, existIds, err := s.anomalyPlanSet()
	if err != nil {
		return err
	}
	var classRules []*anomalyRulePlan
	for _, p := range plans.rules {
		if p.large == large {
			classRules = append(classRules, p)
		}
	}

	// 起点：首次（游标为零）或游标已落到窗口之外时从 now-24h 起。
	floor := logCursor{Time: now.Add(-anomalyWindow).UnixMilli() - 1, Id: math.MaxInt64}
	anomalyMu.Lock()
	pruneAnomaliesLocked(plans, existIds, now)
	if len(classRules) == 0 {
		// 这一档没有规则：游标归零，将来有规则进入这一档时从 now-24h 回填。
		anomalyCursors[class] = logCursor{}
		if large {
			anomalyLargeBacklogged = false
		}
		anomalyMu.Unlock()
		return nil
	}
	cursor := anomalyCursors[class]
	if cursor.less(floor) {
		cursor = floor
	}
	epoch := anomalyCursorEpoch
	watermarks := make(map[int]logCursor, len(classRules))
	ignore := make(map[int]int64, len(classRules))
	for _, p := range classRules {
		if wm, ok := anomalyWatermarks[p.ruleId]; ok {
			watermarks[p.ruleId] = wm
		}
		ignore[p.ruleId] = anomalyIgnore[p.ruleId]
	}
	anomalyMu.Unlock()

	rows, err := (&AccessLogService{}).ReadAfter(cursor.Time, cursor.Id, anomalyBatchRows)
	if err != nil {
		return fmt.Errorf("读取访问日志失败：%w", err)
	}

	sc := &anomalyScan{
		geo:        plans.geo,
		rows:       rows,
		cutoff:     anomalyCutoffKey(now),
		watermarks: watermarks,
		ignore:     ignore,
		deltas:     map[int]map[int64]*anomalyBucket{},
	}
	if large {
		sc.scanLarge(classRules)
	} else {
		sc.accumulate(classRules, func(p *anomalyRulePlan) *groupMatcher { return p.matcher })
	}
	deltas := sc.deltas

	next := cursor
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		next = logCursor{Time: last.Time, Id: last.Id}
	}

	anomalyMu.Lock()
	defer anomalyMu.Unlock()
	if anomalyCursorEpoch != epoch {
		return nil // 读的过程中被清空过（访问日志关闭），这一批作废
	}
	anomalyCursors[class] = next
	if large {
		anomalyLargeBacklogged = len(rows) >= anomalyBatchRows
	}
	for _, p := range classRules {
		if wm, ok := anomalyWatermarks[p.ruleId]; !ok || wm.less(next) {
			anomalyWatermarks[p.ruleId] = next
		}
		byKey := deltas[p.ruleId]
		if byKey == nil || anomalyIgnore[p.ruleId] != ignore[p.ruleId] {
			// 扫描期间被「忽略此前的异常」重置过：这一批的增量可能早于忽略时刻，整批不计。
			continue
		}
		sum := anomalySummaries[p.ruleId]
		if sum == nil {
			sum = map[int64]*anomalyBucket{}
			anomalySummaries[p.ruleId] = sum
		}
		for key, b := range byKey {
			if dst := sum[key]; dst != nil {
				dst.merge(b)
			} else {
				sum[key] = b
			}
		}
	}
	return nil
}

// anomalyScan 是一轮扫描在锁外的工作区：已读出的行、扫描开始时的水位与忽略时刻快照，
// 以及本轮累出的增量（合并进摘要时再持锁）。
type anomalyScan struct {
	geo        *geoBlockMatcher
	rows       []model.AccessLog
	cutoff     int64
	watermarks map[int]logCursor
	ignore     map[int]int64
	deltas     map[int]map[int64]*anomalyBucket
}

// candidate 判断一行是否可能是规则 p 的异常：除「目标属于规则域名组」以外的全部条件。
// 先做这些廉价判断，匹配器只对剩下的行调用；大规则据此决定这一轮要不要构建匹配器。
func (sc *anomalyScan) candidate(p *anomalyRulePlan, r *model.AccessLog, head string) bool {
	if !r.Accepted || r.Time/anomalyBucketMs < sc.cutoff {
		return false
	}
	if !p.coverage[r.InboundId] || r.Time < sc.ignore[p.ruleId] {
		return false
	}
	if wm, ok := sc.watermarks[p.ruleId]; ok && !wm.less(logCursor{Time: r.Time, Id: r.Id}) {
		return false // 规则换档前在另一档已计过
	}
	if match, _ := judgeSample(p.expected, head, false, p.defaultTag); match {
		return false
	}
	// 不是本规则造成的两类连接，判定次序与 buildLogEvidence 一致。
	if r.NoRuleHit || sc.geo.isGeoBlocked(r.InboundId, r.Route, r.SourceIP) {
		return false
	}
	return true
}

// accumulate 用给定的匹配器把行累加进 rules 的增量。匹配器为 nil 的规则跳过。
func (sc *anomalyScan) accumulate(rules []*anomalyRulePlan, matcherOf func(*anomalyRulePlan) *groupMatcher) {
	for i := range sc.rows {
		r := &sc.rows[i]
		head := routeHead(r.Route)
		for _, p := range rules {
			m := matcherOf(p)
			if m == nil || !sc.candidate(p, r, head) || !m.Match(r.Target) {
				continue
			}
			byKey := sc.deltas[p.ruleId]
			if byKey == nil {
				byKey = map[int64]*anomalyBucket{}
				sc.deltas[p.ruleId] = byKey
			}
			key := r.Time / anomalyBucketMs
			b := byKey[key]
			if b == nil {
				b = newAnomalyBucket()
				byKey[key] = b
			}
			b.add(r.InboundId, head, 1, r.Time)
		}
	}
}

// anomalyLargeTrace 只给测试观察扫描对大规则匹配器的取用（"acquire"/"release" + 组序列），
// 生产环境为 nil。
var anomalyLargeTrace func(event, groupKey string)

// scanLarge 按组序列逐个处理大规则：从常驻槽取一个匹配器、走完共用它的规则、放手，再取
// 下一个，任一时刻至多占用一个（槽也只留一个，设计 §4.3 按一次构建估算内存）。本轮没有
// 任何候选行的组序列不取，也就不会为它构建。
func (sc *anomalyScan) scanLarge(rules []*anomalyRulePlan) {
	var keys []string
	byKey := map[string][]*anomalyRulePlan{}
	for _, p := range rules {
		if _, ok := byKey[p.groupKey]; !ok {
			keys = append(keys, p.groupKey)
		}
		byKey[p.groupKey] = append(byKey[p.groupKey], p)
	}
	for _, key := range keys {
		sc.scanLargeKey(key, byKey[key])
	}
}

func (sc *anomalyScan) scanLargeKey(key string, rules []*anomalyRulePlan) {
	if !sc.anyCandidate(rules) {
		return
	}
	if anomalyLargeTrace != nil {
		anomalyLargeTrace("acquire", key)
		defer anomalyLargeTrace("release", key)
	}
	m, release := largeMatcherOf(rules[0])
	defer release()
	if m == nil {
		return
	}
	sc.accumulate(rules, func(*anomalyRulePlan) *groupMatcher { return m })
}

func (sc *anomalyScan) anyCandidate(rules []*anomalyRulePlan) bool {
	for i := range sc.rows {
		r := &sc.rows[i]
		head := routeHead(r.Route)
		for _, p := range rules {
			if sc.candidate(p, r, head) {
				return true
			}
		}
	}
	return false
}

// pruneAnomaliesLocked 丢弃过期桶、停用/删除规则的摘要与水位、已删除规则的忽略时刻。
// existIds 含停用规则。
func pruneAnomaliesLocked(plans *anomalyPlanSet, existIds map[int]bool, now time.Time) {
	cutoff := anomalyCutoffKey(now)
	for id, sum := range anomalySummaries {
		if !plans.enabledIds[id] {
			delete(anomalySummaries, id)
			continue
		}
		for key := range sum {
			if key < cutoff {
				delete(sum, key)
			}
		}
		if len(sum) == 0 {
			delete(anomalySummaries, id)
		}
	}
	for id := range anomalyWatermarks {
		if !plans.enabledIds[id] {
			delete(anomalyWatermarks, id)
		}
	}
	// 只在规则被删除后才清忽略时刻：SQLite 会复用 id，留着会作用到下一条新规则上；
	// 停用再启用的规则保留它，否则列表与流量走向图会一边忽略一边不忽略。
	for id := range anomalyIgnore {
		if !existIds[id] {
			delete(anomalyIgnore, id)
		}
	}
}

// largeMatcherOf 从常驻槽取大规则的匹配器与放手函数，条目取法与 planRule 一致（逐组读库
// 合并）：两边得到同样的条目，审计与扫描共用同一个匹配器。刻意按组序列合并成一个，而不是
// 逐组各建一个再取「任一命中」：取反条目在合并列表里是对并集取反（见 verifyShared.matcher）。
// 失败返回 nil 并记日志，该规则本轮不计；放手函数照样要调用。
func largeMatcherOf(p *anomalyRulePlan) (*groupMatcher, func()) {
	var domainLists, cidrLists [][]string
	for _, gid := range p.groupIds {
		domains, cidrs, ok := groupConditionsOf(gid)
		if !ok {
			continue
		}
		domainLists = append(domainLists, domains)
		cidrLists = append(cidrLists, cidrs)
	}
	e, release := largeMatchers.acquire(MergeDomains(domainLists...), MergeDomains(cidrLists...))
	if e.err != nil {
		logger.Warning("路由异常扫描：构造大规则匹配器失败，rule id:", p.ruleId, "err:", e.err)
		return nil, release
	}
	return e.m, release
}

// anomalyPlanSet 返回当前的规则计划与全部规则 id（含停用）；代次或启用规则集合
// （含规则内容）变化时重建计划。
func (s *RoutingAuditService) anomalyPlanSet() (*anomalyPlanSet, map[int]bool, error) {
	gen := XrayApplyGeneration()
	all, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		return nil, nil, fmt.Errorf("读取分流规则失败：%w", err)
	}
	var enabled []*model.RoutingRule
	var sig strings.Builder
	existIds := make(map[int]bool, len(all))
	for _, r := range all {
		existIds[r.Id] = true
		if !r.Enable {
			continue
		}
		enabled = append(enabled, r)
		fmt.Fprintf(&sig, "%d|%s|%s|%s|%d;", r.Id, r.InboundIds, r.DomainGroupIds, r.Action, r.OutboundId)
	}

	anomalyPlanMu.Lock()
	defer anomalyPlanMu.Unlock()
	if p := anomalyPlans; p != nil && p.gen == gen && p.sig == sig.String() {
		return p, existIds, nil
	}
	set, err := s.buildAnomalyPlans(enabled)
	if err != nil {
		return nil, nil, err
	}
	set.gen, set.sig = gen, sig.String()
	anomalyPlans = set
	return set, existIds, nil
}

// buildAnomalyPlans 复用 Verify 的 prepareShared + planRule 推出期望出站与合并条目，
// 与流量走向图同一份判定。传空运行时快照：不连核心控制面、不取包级 lock；
// 默认出站 tag 因此取当前生成配置（与核心持有的是同一个 tagDefaultOutbound 值）。
func (s *RoutingAuditService) buildAnomalyPlans(enabled []*model.RoutingRule) (*anomalyPlanSet, error) {
	v := s.verifier()
	sh, err := v.prepareShared(XrayRuntime{})
	if err != nil {
		return nil, err
	}
	defer sh.close()

	large, smallGroups := classifyAnomalyRules(enabled)
	// 只有小规则需要完整条目（建常驻匹配器）。其余组在 planRule 的组缓存里预置一个
	// 不带条目的占位，planRule 就不会把大组的十几万条连同样例读进来并在整个重建期间持有；
	// 大规则只用到它的期望出站与组序列，条目在扫描时临时读。占位的 skip 与真实取法一致
	// （classifyAnomalyRules 只为 groupConditionsOf 返回 ok 的组给出条目数）。
	for gid, ok := range smallGroups {
		if !ok {
			sh.groups[gid] = &groupConditions{}
		}
	}

	set := &anomalyPlanSet{
		matchers:   map[string]*groupMatcher{},
		enabledIds: make(map[int]bool, len(enabled)),
		geo:        sh.geoBlocker(),
	}
	for _, rule := range enabled {
		set.enabledIds[rule.Id] = true
		plan, reason := v.planRule(sh, rule)
		if reason != "" {
			continue // 不会写进核心配置的规则没有「走错」可言
		}
		p := &anomalyRulePlan{
			ruleId:     rule.Id,
			expected:   plan.expected,
			defaultTag: plan.defaultTag,
			coverage:   map[int]bool{},
			groupKey:   plan.groupKey,
			large:      large[rule.Id],
		}
		for _, part := range strings.Split(plan.groupKey, ",") {
			if gid, err := strconv.Atoi(part); err == nil {
				p.groupIds = append(p.groupIds, gid)
			}
		}
		// 覆盖范围与 buildRule 一致：全局规则覆盖所有启用入站，否则取指定入站里启用的。
		// 不用 plan.users：它对全局规则截到 50 个、还列出停用入站，那是展示用的。
		ids, _ := DecodeInboundIds(rule.InboundIds) // buildRule 已校验过
		if len(ids) == 0 {
			for id := range sh.inboundTags {
				p.coverage[id] = true
			}
		} else {
			for _, id := range ids {
				if _, ok := sh.inboundTags[id]; ok {
					p.coverage[id] = true
				}
			}
		}
		if !p.large {
			m, ok := set.matchers[p.groupKey]
			if !ok {
				if m, err = newGroupMatcher(plan.domains, plan.cidrs); err != nil {
					logger.Warning("路由异常扫描：构造匹配器失败，rule id:", rule.Id, "err:", err)
					m = nil
				}
				set.matchers[p.groupKey] = m
			}
			if m == nil {
				continue
			}
			p.matcher = m
		}
		set.rules = append(set.rules, p)
	}
	return set, nil
}

// classifyAnomalyRules 按「规则引用的全部组合并后的条目数（手工 + 订阅，域名 + IP 段）」
// 把规则分成大小两档，结果与对 planRule 的合并结果取 len(domains)+len(cidrs) 完全一致，
// 但不同时持有所有组的条目：逐组读一次只记条目数、列表随即释放。合并是有序去重，
// 合并后的条目数介于「最大的单组」与「各组之和」之间，只有落在两者之间跨过门槛的规则
// 才需要真的合并一次（同样用完即释放）。
//
// 返回 large（ruleId → 是否大规则）与 groups（可用组 id → 是否被某条小规则引用）。
func classifyAnomalyRules(enabled []*model.RoutingRule) (large map[int]bool, groups map[int]bool) {
	sizes := map[int]int{} // 只含 groupConditionsOf 返回 ok 的组
	seen := map[int]bool{}
	usedOf := make(map[int][]int, len(enabled))
	for _, rule := range enabled {
		gids, err := DecodeDomainGroupIds(rule.DomainGroupIds)
		if err != nil {
			continue // buildRule 会丢弃这条规则
		}
		for _, gid := range gids {
			if !seen[gid] {
				seen[gid] = true
				if domains, cidrs, ok := groupConditionsOf(gid); ok {
					sizes[gid] = len(domains) + len(cidrs)
				}
			}
			if _, ok := sizes[gid]; ok {
				usedOf[rule.Id] = append(usedOf[rule.Id], gid)
			}
		}
	}

	large = make(map[int]bool, len(enabled))
	groups = make(map[int]bool, len(sizes))
	for gid := range sizes {
		groups[gid] = false
	}
	for _, rule := range enabled {
		used := usedOf[rule.Id]
		sum, biggest := 0, 0
		for _, gid := range used {
			sum += sizes[gid]
			biggest = max(biggest, sizes[gid])
		}
		switch {
		case sum <= auditLargeThreshold:
		case biggest > auditLargeThreshold:
			large[rule.Id] = true
		default:
			large[rule.Id] = mergedEntryCount(used) > auditLargeThreshold
		}
		if !large[rule.Id] {
			for _, gid := range used {
				groups[gid] = true
			}
		}
	}
	return large, groups
}

// mergedEntryCount 按 planRule 的取法合并几个组并返回条目数，列表不外传。
func mergedEntryCount(gids []int) int {
	var domainLists, cidrLists [][]string
	for _, gid := range gids {
		if domains, cidrs, ok := groupConditionsOf(gid); ok {
			domainLists = append(domainLists, domains)
			cidrLists = append(cidrLists, cidrs)
		}
	}
	return len(MergeDomains(domainLists...)) + len(MergeDomains(cidrLists...))
}
