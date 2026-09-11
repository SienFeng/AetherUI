package service

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/ipdb"
	"a-ui/util/netdiag"
)

// 共享风险评分。把画像、距离、并存、错峰合成一个 0~100 的**可解释**分数。
//
// 三条贯穿全文的取向，改动时不要削弱：
//
//  1. **「测不了」绝不显示成「低风险」。** 非 TCP 传输、非 Linux 平台上一条
//     采样都没有；纯 IPv6 来源拿不到任何地理信息。这两种状态各有各的出口
//     （unobservable / degraded），绝不映射成 low——把「测不了」报成「干净」
//     是一个确信的错误结论，比空白严重得多。
//  2. **错峰单独不能定罪。** 跨省错峰与「每周两地通勤的同一个人」在网络层
//     完全同形。除了让它的分值上限低于并存，还有一道显式钳制（见 §8.2 的
//     riskNoCoexistCeiling），因为权重将来一定会按生产分布回标，而这条不变量
//     不该取决于有没有人记得那个隐含前提。
//  3. **只有距离 ≥ 2（跨省/跨国）才计分。** 距离 0、1 与 unknown 一律不计分，
//     理由见 network_profile.go 顶部。

// sharingRiskEngineVersion 是评分引擎的版本。
//
// 随 Snapshot 落库，Job 发现不匹配时**无条件重算**。这样将来调权重不需要任何
// 数据迁移——也正是 InboundIPHour 坚持存原始特征而不是存 ProfileID 的兑现。
const sharingRiskEngineVersion = 1

// 四种状态。判定顺序固定为 unobservable → degraded → learning → ready，
// 先命中先返回。
const (
	RiskStateReady        = "ready"
	RiskStateLearning     = "learning"
	RiskStateDegraded     = "degraded"
	RiskStateUnobservable = "unobservable"
)

// 风险等级。**只在 State == ready 时才计算和显示**，其余状态一律为空串。
const (
	RiskLevelLow    = "low"
	RiskLevelWatch  = "watch"
	RiskLevelMedium = "medium"
	RiskLevelHigh   = "high"
	RiskLevelSevere = "severe"
)

const (
	riskCoexistCap  = 50
	riskTemporalCap = 35

	// riskNoCoexistCeiling 是没有任何地理并存证据时的分数上限。
	//
	// 当前权重下无并存路径最高只有 65（35 + 10 + 10 + 10），本来就够不到 70，
	// 所以这是一道**冗余**防线。保留它是为了让「错峰不能单独上高风险」这条
	// 不变量在将来回标权重之后仍然成立，而且它是可测试的。
	riskNoCoexistCeiling = 69

	// riskDegradedCoexistCap 是降级口径（拿不到地理信息、只能按网络族判）下
	// 并存信号的上限。降级口径误报率高得多——同一个人的手机与宽带就是两个
	// 网络族。
	riskDegradedCoexistCap = 10

	// riskReadyMinConfidence 是进入正式等级所需的置信度。
	riskReadyMinConfidence = 60

	// riskDegradedMaxGeoCoverage 是判定降级的地理覆盖率下限（百分比）。
	riskDegradedMinGeoCoverage = 50
)

// RiskEvidence 是一条风险依据。
//
// Code 必须保留：文案要能改、要能被测试断言，而不是靠比对中文字符串。
// Points 可以为 0——那是**上下文证据**，列出来供管理员参考但不计分。
type RiskEvidence struct {
	Code     string         `json:"code"`
	Points   int            `json:"points"`
	Profiles []string       `json:"profiles"`
	Metrics  map[string]any `json:"metrics"`
	Title    string         `json:"title"`
	Detail   string         `json:"detail"`
}

// GeoCoexist 是 Risk 层自己算的地理并存统计。
//
// **不复用 computeCoexist**：那个只认 Province，境外来源会整体退化成
// 网络族口径，而「US + JP 长期并存」其实完全测得出来。两者的 1 MB 门槛
// 共用同一个常量，不会出现「共享检测说没事、风险评分说高危」那种无从
// 解释的分歧。
type GeoCoexist struct {
	Hours int `json:"hours"`
	// Days 是并存发生在几个不同的日历天（按面板时区切）。持续性比总时长
	// 更能区分「一次出差重叠」与「长期两地同用」。
	Days int `json:"days"`
	// Regions 是并存涉及的地区标识，升序去重。中国段是省，境外段是国家。
	Regions []string `json:"regions"`
	// Degraded 为真表示走的是网络族口径，不是地理口径。
	Degraded bool `json:"degraded"`
}

// RiskResult 是一次完整分析的产物。
type RiskResult struct {
	Score       int    `json:"score"`
	Confidence  int    `json:"confidence"`
	Level       string `json:"level"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
	GeoCoverage int    `json:"geoCoverage"`

	AnalysisStart int64 `json:"analysisStart"`
	LastDataHour  int64 `json:"lastDataHour"`
	EngineVersion int   `json:"engineVersion"`

	Evidence []RiskEvidence   `json:"evidence"`
	Profiles []NetworkProfile `json:"profiles"`
	Coexist  GeoCoexist       `json:"coexist"`
	Temporal []TemporalPair   `json:"temporal"`
}

// riskInput 是 analyzeRisk 的全部输入。做成结构体是因为参数不少，而且
// PlatformSupported 必须由调用方注入——netdiag.Supported 是编译期常量，
// 就地引用会让状态机在开发机上无法被测到。
type riskInput struct {
	Inbound           *model.Inbound
	Rows              []model.InboundIPHour
	Now               time.Time
	Loc               *time.Location
	PlatformSupported bool
}

// identityEpoch 返回可分析区间的起点：窗口内**最后一个**旧版本小时之后的
// 下一个小时。窗口内没有旧行时返回最早那个小时。
//
// 为什么不是「旧行占比低于某个数就整段一起算」：行数不等于时间覆盖（一个
// 被扫的小时能塞进 50 行，占比会被少数几个小时拽过门槛），而且达标之后那
// 一段里仍然混着旧行——同一个风险分里一半信号基于带画像的数据、一半基于
// 不带的，界面上看不出来。Epoch 切出来的区间天然连续、语义单一。
func identityEpoch(rows []model.InboundIPHour) int64 {
	if len(rows) == 0 {
		return 0
	}
	var lastOld int64 = -1
	earliest := rows[0].HourStart
	for _, r := range rows {
		if r.HourStart < earliest {
			earliest = r.HourStart
		}
		if r.IdentityVersion < sharingIdentityVersion && r.HourStart > lastOld {
			lastOld = r.HourStart
		}
	}
	if lastOld < 0 {
		return earliest
	}
	return lastOld + 3600
}

func rowsFrom(rows []model.InboundIPHour, start int64) []model.InboundIPHour {
	out := make([]model.InboundIPHour, 0, len(rows))
	for _, r := range rows {
		if r.HourStart >= start {
			out = append(out, r)
		}
	}
	return out
}

// regionOf 返回一行观测的**地区标识**，无法判定时返回空串。
//
// 中国段用省，境外段用国家——两个不同的标识必然对应距离 ≥ 2（省不同是
// 跨省，国不同是跨国），所以「同一小时里出现两个不同标识」就等价于
// 「存在一对距离 ≥ 2 的来源在同时使用」。
func regionOf(r model.InboundIPHour) string {
	if r.Country == ipdb.ChinaCountry {
		if r.Province == "" {
			return ""
		}
		return "CN|" + r.Province
	}
	return r.Country
}

// computeGeoCoexist 算地理并存。地理覆盖率不足时退回网络族口径。
func computeGeoCoexist(rows []model.InboundIPHour, loc *time.Location, degraded bool) GeoCoexist {
	if loc == nil {
		loc = time.UTC
	}
	byHour := map[int64]map[string]bool{}
	for _, r := range rows {
		if !effectiveRow(r) {
			continue
		}
		key := regionOf(r)
		if degraded {
			key = networkFamilyOrIP(r.IP)
		}
		if key == "" {
			continue
		}
		if byHour[r.HourStart] == nil {
			byHour[r.HourStart] = map[string]bool{}
		}
		byHour[r.HourStart][key] = true
	}

	stat := GeoCoexist{Degraded: degraded, Regions: []string{}}
	seen := map[string]bool{}
	days := map[string]bool{}
	for hour, set := range byHour {
		if len(set) < 2 {
			continue
		}
		stat.Hours++
		days[dayKey(hour, loc)] = true
		for k := range set {
			seen[k] = true
		}
	}
	stat.Days = len(days)
	stat.Regions = sortedKeys(seen)
	return stat
}

// geoCoverage 是有效行里能解析出地区的比例（0~100），第二个返回值是有效
// 行数。
//
// 判据用 Country 而不是 Province：ipdb 对境外段只保留国家，按 Province 判
// 会把整个境外来源群误判成「没有地理信息」。
//
// 必须同时返回行数：**没有数据不等于定位不出来**。覆盖率对空输入是 0，
// 若据此判降级，一个刚建好、还没人连过的入站会显示成「降级」，而它的真实
// 状态是「还在学习」。这与第一期 computeCoexist 开头那句「没有数据不等于
// 归属地库未加载」是同一个坑。
func geoCoverage(rows []model.InboundIPHour) (int, int) {
	total, known := 0, 0
	for _, r := range rows {
		if !effectiveRow(r) {
			continue
		}
		total++
		if r.Country != "" {
			known++
		}
	}
	if total == 0 {
		return 0, 0
	}
	return known * 100 / total, total
}

// analyzeRisk 是整个引擎的入口，**纯函数**。
func analyzeRisk(in riskInput) RiskResult {
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}
	res := RiskResult{
		EngineVersion: sharingRiskEngineVersion,
		Evidence:      []RiskEvidence{},
		Profiles:      []NetworkProfile{},
		Temporal:      []TemporalPair{},
		Coexist:       GeoCoexist{Regions: []string{}},
	}

	// 第一道：能不能观测。**必须在读数据之前判**，否则一个零行的 mKCP 入站
	// 会一路走到「没数据 → learning」，而它的真实状态是「这条路上永远不会
	// 有数据」。
	if ok, reason := countabilityOf(in.Inbound, in.PlatformSupported); !ok {
		res.State = RiskStateUnobservable
		res.Reason = reason
		return res
	}

	res.AnalysisStart = identityEpoch(in.Rows)
	rows := rowsFrom(in.Rows, res.AnalysisStart)
	for _, r := range rows {
		if r.HourStart > res.LastDataHour {
			res.LastDataHour = r.HourStart
		}
	}

	coverage, effective := geoCoverage(rows)
	res.GeoCoverage = coverage
	degraded := effective > 0 && coverage < riskDegradedMinGeoCoverage

	res.Profiles = buildProfiles(rows, loc)
	res.Coexist = computeGeoCoexist(rows, loc, degraded)
	res.Temporal = computeTemporal(rows, res.Profiles, loc)
	contextPairs := computeTemporalSameProvince(rows, res.Profiles, loc)

	res.Score, res.Evidence = scoreRisk(res.Profiles, res.Coexist, res.Temporal, contextPairs, degraded)
	res.Confidence = confidenceOf(rows, res.AnalysisStart, in.Now, res.GeoCoverage)

	switch {
	case degraded:
		res.State = RiskStateDegraded
	case res.Confidence < riskReadyMinConfidence:
		res.State = RiskStateLearning
	default:
		res.State = RiskStateReady
		res.Level = levelOf(res.Score)
	}
	return res
}

// scoreRisk 把各路信号合成分数与依据。
//
// 依据的产出顺序是固定的（按下面的书写顺序），不遍历 map——否则同一份数据
// 每次渲染出来的依据次序都不一样。
func scoreRisk(profiles []NetworkProfile, coexist GeoCoexist, temporal, contextPairs []TemporalPair, degraded bool) (int, []RiskEvidence) {
	ev := []RiskEvidence{}
	add := func(e RiskEvidence) {
		if e.Profiles == nil {
			e.Profiles = []string{}
		}
		if e.Metrics == nil {
			e.Metrics = map[string]any{}
		}
		ev = append(ev, e)
	}

	coexistPoints := 0
	if coexist.Hours >= coexistDisplayMinHours {
		base := 15
		switch {
		case coexist.Hours >= 12:
			base = 35
		case coexist.Hours >= 6:
			base = 25
		}
		coexistPoints += base
		add(RiskEvidence{
			Code: "coexist_geo", Points: base, Profiles: coexist.Regions,
			Metrics: map[string]any{"hours": coexist.Hours, "degraded": coexist.Degraded},
			Title:   "异地同时实质使用",
			Detail: fmt.Sprintf("最近的可分析区间内有 %d 小时，不同地区的来源在同时实质使用这个节点。",
				coexist.Hours),
		})

		if coexist.Days >= 3 {
			coexistPoints += 10
			add(RiskEvidence{
				Code: "coexist_multi_day", Points: 10,
				Metrics: map[string]any{"days": coexist.Days},
				Title:   "并存不是一次性的",
				Detail: fmt.Sprintf("并存发生在 %d 个不同的日子，不是一次出差造成的短暂重叠。",
					coexist.Days),
			})

			if len(coexist.Regions) >= 3 {
				coexistPoints += 5
				add(RiskEvidence{
					Code: "coexist_multi_region", Points: 5, Profiles: coexist.Regions,
					Metrics: map[string]any{"regions": len(coexist.Regions)},
					Title:   "并存涉及三个以上地区",
					Detail:  fmt.Sprintf("并存中出现过 %d 个不同地区。", len(coexist.Regions)),
				})
			}
		}
	}
	coexistCap := riskCoexistCap
	if degraded {
		coexistCap = riskDegradedCoexistCap
	}
	coexistPoints = minInt(coexistPoints, coexistCap)

	temporalPoints := 0
	if len(temporal) > 0 {
		temporalPoints += 20
		add(RiskEvidence{
			Code: "temporal_geo", Points: 20, Profiles: pairKeys(temporal),
			Metrics: map[string]any{"pairs": len(temporal)},
			Title:   "异地错峰使用",
			Detail: "两个长期稳定的异地网络几乎从不在同一小时出现。" +
				"注意：这也可能是同一个人在两地之间规律往返，网络层分不开这两种情况。",
		})
		if anyPersistent(temporal) {
			temporalPoints += 15
			add(RiskEvidence{
				Code: "temporal_persistent", Points: 15, Profiles: pairKeys(temporal),
				Title:  "错峰模式持续存在",
				Detail: "错峰不是偶发：连续多天成立，且几乎没有任何重叠。",
			})
		}
	}
	temporalPoints = minInt(temporalPoints, riskTemporalCap)

	// 「第二网络占比高」与「跨地区画像多」都是**放大器，不是独立证据**：
	// 没有并存也没有错峰时，它们描述的恰恰是一次搬家或一趟长差——第二个省
	// 承担四成流量、七天里去过三个省，对一个旅行的人完全正常。单独计分会让
	// 「出差一周」这个必须为零的标定场景拿到 15 分。
	//
	// access_plus_hosting 自己内部已经要求并存或错峰，不必在这里再挡一次。
	hasPrimaryEvidence := coexistPoints > 0 || len(temporal) > 0

	sharePoints, multiPoints := 0, 0
	if hasPrimaryEvidence {
		var shareEv, multiEv *RiskEvidence
		sharePoints, shareEv = secondaryShareSignal(profiles)
		if shareEv != nil {
			add(*shareEv)
		}
		multiPoints, multiEv = multiGeoProfileSignal(profiles)
		if multiEv != nil {
			add(*multiEv)
		}
	}
	hostingPoints, hostingEv := accessPlusHostingSignal(profiles, coexist, temporal)
	if hostingEv != nil {
		add(*hostingEv)
	}

	// 上下文证据：同省不同 ISP 的错峰。**0 分**，只列出来供管理员参考。
	// 它会对几乎每个双设备用户命中（家宽 + 手机就是这个形态），给分等于
	// 给所有正常用户一个固定底分。
	if len(contextPairs) > 0 {
		add(RiskEvidence{
			Code: "temporal_same_province", Points: 0, Profiles: pairKeys(contextPairs),
			Metrics: map[string]any{"pairs": len(contextPairs)},
			Title:   "同省不同运营商的错峰使用（不计分）",
			Detail: "同一个省内两个运营商轮流使用。家用宽带加手机流量就是这个形态，" +
				"因此只作背景信息列出，不计入分数。",
		})
	}

	score := coexistPoints + temporalPoints + sharePoints + multiPoints + hostingPoints
	if coexistPoints == 0 {
		// 没有任何地理并存证据时的硬上限，见 riskNoCoexistCeiling。
		score = minInt(score, riskNoCoexistCeiling)
	}
	return minInt(score, 100), ev
}

// primaryProfile 返回流量最大的稳定画像。没有稳定画像时第二个返回值为 false。
func primaryProfile(profiles []NetworkProfile) (NetworkProfile, bool) {
	stable := stableProfiles(profiles)
	if len(stable) == 0 {
		return NetworkProfile{}, false
	}
	// buildProfiles 已按 Bytes 降序、Key 升序排好，取首个即可。
	return stable[0], true
}

// geoDistantStable 返回与主画像距离 ≥ 2 的稳定画像，保持原顺序。
func geoDistantStable(profiles []NetworkProfile) []NetworkProfile {
	primary, ok := primaryProfile(profiles)
	if !ok {
		return nil
	}
	var out []NetworkProfile
	for _, p := range stableProfiles(profiles) {
		if p.Key == primary.Key {
			continue
		}
		if profileDistance(primary, p) >= distanceScorable {
			out = append(out, p)
		}
	}
	return out
}

func secondaryShareSignal(profiles []NetworkProfile) (int, *RiskEvidence) {
	distant := geoDistantStable(profiles)
	if len(distant) == 0 {
		return 0, nil
	}
	second := distant[0]
	points := 0
	switch {
	case second.TrafficShare >= 0.25:
		points = 10
	case second.TrafficShare >= 0.10:
		points = 5
	}
	if points == 0 {
		return 0, nil
	}
	return points, &RiskEvidence{
		Code: "secondary_share", Points: points, Profiles: []string{second.Key},
		Metrics: map[string]any{"share": second.TrafficShare},
		Title:   "第二个异地网络承担了相当比例的流量",
		Detail: fmt.Sprintf("%s 占该入站可分析流量的 %.0f%%。一次出差或临时热点通常远低于这个比例。",
			second.Key, second.TrafficShare*100),
	}
}

func multiGeoProfileSignal(profiles []NetworkProfile) (int, *RiskEvidence) {
	distant := geoDistantStable(profiles)
	if len(distant) < 2 {
		return 0, nil
	}
	points := minInt(5*(len(distant)-1), 10)
	keys := make([]string, 0, len(distant))
	for _, p := range distant {
		keys = append(keys, p.Key)
	}
	return points, &RiskEvidence{
		Code: "multi_geo_profile", Points: points, Profiles: keys,
		Metrics: map[string]any{"count": len(distant) + 1},
		Title:   "存在三个以上长期稳定的异地网络",
		Detail: fmt.Sprintf("除主要网络外，还有 %d 个与它跨地区的长期稳定网络。",
			len(distant)),
	}
}

func accessPlusHostingSignal(profiles []NetworkProfile, coexist GeoCoexist, temporal []TemporalPair) (int, *RiskEvidence) {
	primary, ok := primaryProfile(profiles)
	if !ok || primary.Hosting {
		// 主画像本身就是机房时不报：用户可能本来就在服务器上使用，那是
		// 他自己的用法，不是共享信号。
		return 0, nil
	}
	for _, p := range geoDistantStable(profiles) {
		if !p.Hosting {
			continue
		}
		// 必须同时存在并存或错峰证据，否则一个「上周用过一次云主机」就会
		// 无端加分。
		if coexist.Hours == 0 && !pairExists(temporal, primary.Key, p.Key) {
			continue
		}
		return 10, &RiskEvidence{
			Code: "access_plus_hosting", Points: 10,
			Profiles: []string{primary.Key, p.Key},
			Title:    "普通接入网络与机房网络长期共处",
			Detail: fmt.Sprintf("%s 是机房 / 云厂商网络，与主要网络 %s 跨地区且持续共处。",
				p.Key, primary.Key),
		}
	}
	return 0, nil
}

// confidenceOf 算置信度。与 Score 分开：一个刚建两小时的入站可能信号很强
// （江苏 + 广东同时出现），但样本太少，直接报「高风险」是拿噪声当结论。
func confidenceOf(rows []model.InboundIPHour, analysisStart int64, now time.Time, coverage int) int {
	score := 0

	span := now.Unix() - analysisStart
	if analysisStart == 0 {
		span = 0
	}
	switch {
	case span >= 72*3600:
		score += 30
	case span >= 24*3600:
		score += 15
	}

	hours := map[int64]bool{}
	var bytes int64
	for _, r := range rows {
		if !effectiveRow(r) {
			continue
		}
		hours[r.HourStart] = true
		bytes += r.ActiveBytes
	}
	switch {
	case len(hours) >= 24:
		score += 25
	case len(hours) >= 6:
		score += 12
	}
	switch {
	case bytes >= 100<<20:
		score += 20
	case bytes >= 10<<20:
		score += 10
	}
	switch {
	case coverage >= 80:
		score += 25
	case coverage >= 50:
		score += 12
	}
	return minInt(score, 100)
}

func levelOf(score int) string {
	switch {
	case score >= 85:
		return RiskLevelSevere
	case score >= 70:
		return RiskLevelHigh
	case score >= 45:
		return RiskLevelMedium
	case score >= 30:
		return RiskLevelWatch
	default:
		return RiskLevelLow
	}
}

func pairKeys(pairs []TemporalPair) []string {
	seen := map[string]bool{}
	for _, p := range pairs {
		seen[p.KeyA] = true
		seen[p.KeyB] = true
	}
	return sortedKeys(seen)
}

func anyPersistent(pairs []TemporalPair) bool {
	for _, p := range pairs {
		if p.Persistent {
			return true
		}
	}
	return false
}

func pairExists(pairs []TemporalPair, a, b string) bool {
	for _, p := range pairs {
		if (p.KeyA == a && p.KeyB == b) || (p.KeyA == b && p.KeyB == a) {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- 服务层：读写 Snapshot ----------

// SharingRiskService 负责风险评估的预计算、查询与清理。
//
// 与其它 service 一样是无状态空结构体。
type SharingRiskService struct {
	sharingService SharingService
	inboundService InboundService
	settingService SettingService
}

// Analyze 对单个入站现场跑一次完整分析。
//
// Detail 接口与 Job 共用这一条路径，保证界面上的分数与依据永远同源。Detail
// 现场算而不是读 Snapshot，是因为它是单入站按需请求、跑一次完全够快，而且
// 拿到的比 Snapshot 新（最多差一个 Job 周期）。
func (s *SharingRiskService) Analyze(inbound *model.Inbound, now time.Time) (RiskResult, error) {
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return RiskResult{}, err
	}
	var rows []model.InboundIPHour
	// 不可观测的入站不必查库——那条路上永远不会有数据。
	if ok, _ := countabilityOf(inbound, netdiag.Supported); ok {
		rows, err = s.sharingService.windowRows(inbound.Id, sharingWindowDays, now)
		if err != nil {
			return RiskResult{}, err
		}
	}
	return analyzeRisk(riskInput{
		Inbound:           inbound,
		Rows:              rows,
		Now:               now,
		Loc:               loc,
		PlatformSupported: netdiag.Supported,
	}), nil
}

// Evaluate 跑一轮全量评估并写回 Snapshot。
//
// **按入站驱动，绝不从数据行反推要处理哪些入站。** 一个 mKCP / QUIC 入站
// 一行数据都没有，从行分组里根本不会出现它——于是 unobservable 这个状态
// 永远发不出来，而它恰恰是这个功能最要紧的一条。非 Linux 平台同理
// （netdiag.Supported 是编译期常量，那里全部入站都零行）。
func (s *SharingRiskService) Evaluate(now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil {
		// 库没打开：面板启动时 InitTrafficDB 失败就是这个状态。风险评估
		// 不可用不该让这个任务每 10 分钟报一次错。
		return nil
	}
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return err
	}
	for _, in := range inbounds {
		res, err := s.Analyze(in, now)
		if err != nil {
			logger.Warning("共享风险评估失败, 入站", in.Id, ":", err)
			continue
		}
		if err := s.upsertSnapshot(db, in.Id, res, now); err != nil {
			logger.Warning("写入共享风险快照失败, 入站", in.Id, ":", err)
		}
	}
	return nil
}

func (s *SharingRiskService) upsertSnapshot(db *gorm.DB, inboundId int, res RiskResult, now time.Time) error {
	evidence, err := json.Marshal(res.Evidence)
	if err != nil {
		return err
	}
	row := &model.InboundRiskSnapshot{
		InboundId: inboundId, EngineVersion: res.EngineVersion,
		Score: res.Score, Confidence: res.Confidence,
		Level: res.Level, State: res.State, Reason: res.Reason,
		GeoCoverage:   res.GeoCoverage,
		AnalysisStart: res.AnalysisStart, LastDataHour: res.LastDataHour,
		EvaluatedAt:  now.Unix(),
		EvidenceJSON: string(evidence),
	}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "inbound_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"engine_version", "score", "confidence", "level", "state", "reason", "geo_coverage", "analysis_start", "last_data_hour", "evaluated_at", "evidence_json"}),
	}).Create(row).Error
}

// Summary 返回各入站的风险快照，供入站列表页使用。
//
// 只读 Snapshot，不做任何计算：它每次加载入站列表都跑，必须廉价。
//
// **版本不匹配的快照按「尚未评估」返回，绝不显示它的旧分数。** Job 下一轮会
// 无条件重算。
func (s *SharingRiskService) Summary() (map[int]RiskSummaryEntry, error) {
	out := map[int]RiskSummaryEntry{}
	db := database.GetTrafficDB()
	if db == nil {
		return out, nil
	}
	var rows []model.InboundRiskSnapshot
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.EngineVersion != sharingRiskEngineVersion {
			out[r.InboundId] = RiskSummaryEntry{State: RiskStateLearning}
			continue
		}
		out[r.InboundId] = RiskSummaryEntry{
			Score: r.Score, Confidence: r.Confidence, Level: r.Level,
			State: r.State, Reason: r.Reason, GeoCoverage: r.GeoCoverage,
			EvaluatedAt: r.EvaluatedAt,
		}
	}
	return out, nil
}

// RiskSummaryEntry 是列表页需要的最小集合。
type RiskSummaryEntry struct {
	Score       int    `json:"score"`
	Confidence  int    `json:"confidence"`
	Level       string `json:"level"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
	GeoCoverage int    `json:"geoCoverage"`
	EvaluatedAt int64  `json:"evaluatedAt"`
}

// DeleteByInbound 删除某入站的风险快照。
//
// 必须在删除入站时调用：SQLite 会复用被删除的自增 id，残留的快照会绑到下一个
// 建出来的入站上，那时引用不再悬空，界面会渲染得非常合理，只是显示的是别人的
// 风险分。
func (s *SharingRiskService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.InboundRiskSnapshot{}).Error
}

// PruneOrphans 删除已不存在的入站遗留的快照，返回删除行数。兜底第二道。
func (s *SharingRiskService) PruneOrphans() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.Inbound{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	tx := db.Where("inbound_id != 0")
	if len(ids) > 0 {
		tx = tx.Where("inbound_id not in ?", ids)
	}
	result := tx.Delete(&model.InboundRiskSnapshot{})
	return result.RowsAffected, result.Error
}
