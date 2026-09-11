package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// tcpInbound 造一个可观测的入站：streamSettings 为空时 transportObservable
// 按 tcp 处理。
func tcpInbound() *model.Inbound {
	return &model.Inbound{Id: 1, Port: 10001, Enable: true}
}

func riskOf(rows []model.InboundIPHour) RiskResult {
	// now 取数据末尾再加一小时，让观测跨度按数据本身算，不受跑测试的时刻影响。
	var last int64
	for _, r := range rows {
		if r.HourStart > last {
			last = r.HourStart
		}
	}
	return analyzeRisk(riskInput{
		Inbound:           tcpInbound(),
		Rows:              rows,
		Now:               time.Unix(last+3600, 0),
		Loc:               time.UTC,
		PlatformSupported: true,
	})
}

func evidenceCodes(res RiskResult) []string {
	out := make([]string, 0, len(res.Evidence))
	for _, e := range res.Evidence {
		out = append(out, e.Code)
	}
	return out
}

func hasCode(res RiskResult, code string) bool {
	for _, e := range res.Evidence {
		if e.Code == code {
			return true
		}
	}
	return false
}

// ---------- §8.3 的六个标定场景 ----------
//
// 断言的是**区间**而不是精确值：这些分值全是量级判断，跑过一段生产数据之后
// 一定要回标，届时精确值会变，而这几条区间约束不该跟着松。

// 场景 1：家用宽带 + 手机流量，同省不同运营商，天然错峰。
//
// 这是整套设计最要紧的一条：它与真正的跨省错峰共享在「两个稳定画像 + 几乎
// 不重叠」这几个特征上完全一致，唯一的区别就是距离。
func TestRiskHomeBroadbandPlusPhoneScoresZero(t *testing.T) {
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, night, 5<<20),
		spread(cn("江苏省", "中国移动"), "117.136.1.1", 7, daytime, 5<<20)...,
	)
	res := riskOf(rows)
	if res.Score != 0 {
		t.Errorf("Score = %v, want 0；依据 = %v", res.Score, evidenceCodes(res))
	}
	if res.State == RiskStateReady && res.Level != RiskLevelLow {
		t.Errorf("Level = %q, want low", res.Level)
	}
	// 这个形态仍然要作为上下文证据列出来——管理员看得见它，只是不计分。
	if !hasCode(res, "temporal_same_province") {
		t.Errorf("缺少同省错峰的上下文证据: %v", evidenceCodes(res))
	}
	for _, e := range res.Evidence {
		if e.Code == "temporal_same_province" && e.Points != 0 {
			t.Errorf("同省错峰计了 %v 分，它必须是 0 分的上下文证据", e.Points)
		}
	}
}

// 场景 2：出差一周（江苏 → 广东），位置迁移而非并存。
//
// 第二个省承担了四成流量、两个省都「稳定」，但没有任何并存或错峰证据。
// 「第二网络占比高」「跨地区画像多」这两个**放大器**在这里必须不生效，
// 否则一次正常出差就会拿到 15 分。
func TestRiskBusinessTripScoresZero(t *testing.T) {
	var rows []model.InboundIPHour
	for d := 0; d <= 4; d++ {
		rows = append(rows, usage(cn("江苏省", "中国电信"), "49.86.1.1", d, daytime, 10<<20)...)
	}
	for d := 4; d <= 8; d++ {
		rows = append(rows, usage(cn("广东省", "中国移动"), "120.86.1.1", d, night, 10<<20)...)
	}
	res := riskOf(rows)
	if res.Score != 0 {
		t.Errorf("Score = %v, want 0；依据 = %v", res.Score, evidenceCodes(res))
	}
	if hasCode(res, "secondary_share") || hasCode(res, "multi_geo_profile") {
		t.Errorf("放大器在没有主证据时生效了: %v", evidenceCodes(res))
	}
}

// 场景 3：跨省错峰，第二画像承担大量流量。这是第一期明确漏检的那个形态。
func TestRiskCrossProvinceOffPeakLandsInMedium(t *testing.T) {
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, daytime, 12<<20),
		spread(cn("广东省", "中国移动"), "120.86.1.1", 7, night, 9<<20)...,
	)
	res := riskOf(rows)
	if res.Score < 45 || res.Score > 69 {
		t.Errorf("Score = %v, want 45~69（中）；依据 = %v", res.Score, evidenceCodes(res))
	}
	if !hasCode(res, "temporal_geo") || !hasCode(res, "temporal_persistent") {
		t.Errorf("缺少错峰证据: %v", evidenceCodes(res))
	}
}

// 场景 4：错峰信号全部拉满、但一小时并存都没有。
//
// **这是钳制不变量的用例。** 跨省错峰与「每周两地通勤的同一个人」在网络层
// 完全同形，所以无论错峰信号叠得多高，都不许把风险推到「高」。
func TestRiskOffPeakAloneNeverReachesHigh(t *testing.T) {
	hoursA := []int{0, 1, 2, 3, 4, 5}
	hoursB := []int{6, 7, 8, 9, 10, 11}
	hoursC := []int{12, 13, 14, 15, 16, 17}
	hoursD := []int{18, 19, 20, 21, 22, 23}
	var rows []model.InboundIPHour
	rows = append(rows, spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, hoursA, 10<<20)...)
	rows = append(rows, spread(cn("广东省", "中国移动"), "120.86.1.1", 7, hoursB, 9<<20)...)
	rows = append(rows, spread(cn("四川省", "中国联通"), "171.8.1.1", 7, hoursC, 8<<20)...)
	rows = append(rows, spread(NetworkMeta{Country: "美国", ISP: "Amazon"}, "3.3.3.3", 7, hoursD, 7<<20)...)

	res := riskOf(rows)
	if res.Coexist.Hours != 0 {
		t.Fatalf("用例没造对：并存 %v 小时，本该为 0", res.Coexist.Hours)
	}
	if res.Score > riskNoCoexistCeiling {
		t.Errorf("Score = %v，超过了无并存证据时的上限 %v；依据 = %v",
			res.Score, riskNoCoexistCeiling, evidenceCodes(res))
	}
	if res.State == RiskStateReady && (res.Level == RiskLevelHigh || res.Level == RiskLevelSevere) {
		t.Errorf("Level = %q：没有并存证据时绝不允许到高/严重", res.Level)
	}
	if !hasCode(res, "access_plus_hosting") {
		t.Errorf("缺少「接入网 + 机房」证据: %v", evidenceCodes(res))
	}
}

// 场景 5：跨省长期并存。这是本系统能拿到的最强证据形态。
//
// 注意它只到「中」而不是「高」：12 小时跨省并存与「本人出差、家中设备保持
// 在线」仍然同形。要压到高以上，需要并存在多日、多地区重复发生（场景 6）。
func TestRiskCrossProvinceCoexistLandsInMedium(t *testing.T) {
	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	other := []int{2, 3, 4, 5, 6, 7}
	var rows []model.InboundIPHour
	rows = append(rows, spread(cn("江苏省", "中国电信"), "49.86.1.1", 5, hours, 57<<20)...)
	rows = append(rows, spread(cn("广东省", "中国移动"), "120.86.1.1", 5, hours, 35<<20)...)
	// 第三个画像刻意放在别的小时、占比低于错峰门槛，只为凑出「三个画像」。
	rows = append(rows, spread(cn("四川省", "中国联通"), "171.8.1.1", 5, other, 17<<20)...)

	res := riskOf(rows)
	if res.Coexist.Hours < 12 {
		t.Fatalf("用例没造对：并存 %v 小时，本该 ≥12", res.Coexist.Hours)
	}
	if res.Score < 45 || res.Score > 69 {
		t.Errorf("Score = %v, want 45~69（中）；依据 = %v", res.Score, evidenceCodes(res))
	}
	if !hasCode(res, "coexist_geo") || !hasCode(res, "coexist_multi_day") {
		t.Errorf("缺少并存证据: %v", evidenceCodes(res))
	}
}

// 场景 6：并存 + 三个以上地区 + 四个画像，这才到「高」。
func TestRiskCoexistAcrossManyRegionsReachesHigh(t *testing.T) {
	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	other := []int{2, 3, 4, 5, 6, 7}
	var rows []model.InboundIPHour
	rows = append(rows, spread(cn("江苏省", "中国电信"), "49.86.1.1", 5, hours, 46<<20)...)
	rows = append(rows, spread(cn("广东省", "中国移动"), "120.86.1.1", 5, hours, 30<<20)...)
	rows = append(rows, spread(cn("上海市", "中国联通"), "101.80.1.1", 5, hours, 15<<20)...)
	// 四川刻意落在别的小时、占比在「稳定」（5%）与「错峰门槛」（10%）之间：
	// 它要算作第四个跨地区画像，但不该自己再产出一条错峰证据。
	rows = append(rows, spread(cn("四川省", "中国电信"), "171.8.1.1", 5, other, 18<<20)...)

	res := riskOf(rows)
	if res.Score < 70 {
		t.Errorf("Score = %v, want ≥70（高）；依据 = %v", res.Score, evidenceCodes(res))
	}
	if !hasCode(res, "coexist_multi_region") {
		t.Errorf("缺少多地区并存证据: %v", evidenceCodes(res))
	}
}

// ---------- 状态机 ----------

// 本期最要紧的一条回归测试：mKCP / QUIC 入站**零行**时仍然必须产出
// unobservable，而不是一路走到「没数据 → 低风险」。把「测不了」显示成
// 「干净」是一个确信的错误结论，比空白严重得多。
func TestRiskUnobservableWithZeroRows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inbound   *model.Inbound
		supported bool
	}{
		{"mKCP 入站", &model.Inbound{Id: 1, StreamSettings: `{"network":"kcp"}`}, true},
		{"QUIC 入站", &model.Inbound{Id: 1, StreamSettings: `{"network":"quic"}`}, true},
		{"非 Linux 平台", tcpInbound(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := analyzeRisk(riskInput{
				Inbound: tc.inbound, Rows: nil, Now: time.Now(),
				Loc: time.UTC, PlatformSupported: tc.supported,
			})
			if res.State != RiskStateUnobservable {
				t.Errorf("State = %q, want %q", res.State, RiskStateUnobservable)
			}
			if res.Level != "" {
				t.Errorf("Level = %q, want 空串——不可评估绝不能映射成任何等级", res.Level)
			}
			if res.Reason == "" {
				t.Error("必须给出人话原因，否则管理员看到的是一个没有解释的空状态")
			}
		})
	}
}

// 可观测但还没有数据 → learning，不是 low。
func TestRiskObservableButEmptyIsLearning(t *testing.T) {
	res := analyzeRisk(riskInput{
		Inbound: tcpInbound(), Rows: nil, Now: time.Now(),
		Loc: time.UTC, PlatformSupported: true,
	})
	if res.State != RiskStateLearning {
		t.Errorf("State = %q, want %q", res.State, RiskStateLearning)
	}
	if res.Level != "" {
		t.Errorf("Level = %q, want 空串", res.Level)
	}
}

// 拿不到地理信息（纯 IPv6 来源）→ degraded，等级为空，且并存信号被压到上限。
func TestRiskDegradedWhenGeoCoverageIsLow(t *testing.T) {
	rows := append(
		spread(NetworkMeta{}, "240e:3b1:2:abcd::1", 7, daytime, 20<<20),
		spread(NetworkMeta{}, "240e:3b1:2:ef01::1", 7, daytime, 20<<20)...,
	)
	res := riskOf(rows)
	if res.State != RiskStateDegraded {
		t.Fatalf("State = %q, want %q（GeoCoverage=%v）", res.State, RiskStateDegraded, res.GeoCoverage)
	}
	if res.Level != "" {
		t.Errorf("Level = %q, want 空串——降级状态不输出普通等级", res.Level)
	}
	if !res.Coexist.Degraded {
		t.Error("并存统计应标明走的是降级口径")
	}
	if res.Score > riskDegradedCoexistCap {
		t.Errorf("Score = %v，降级口径下并存信号上限是 %v", res.Score, riskDegradedCoexistCap)
	}
}

// 境外来源只有国家、没有省份，但跨国并存完全测得出来。
// 旧的 computeCoexist 在这里会整体退化成 IP 口径，Risk 层必须不退化。
func TestRiskDetectsCrossCountryCoexistWithoutProvince(t *testing.T) {
	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	rows := append(
		spread(NetworkMeta{Country: "美国", ISP: "Amazon"}, "3.3.3.3", 5, hours, 30<<20),
		spread(NetworkMeta{Country: "日本", ISP: "Google"}, "35.1.1.1", 5, hours, 30<<20)...,
	)
	res := riskOf(rows)
	if res.State == RiskStateDegraded {
		t.Fatalf("境外来源有 Country，不该判成降级（GeoCoverage=%v）", res.GeoCoverage)
	}
	if res.Coexist.Degraded {
		t.Error("跨国并存不该走降级口径")
	}
	if res.Coexist.Hours < 12 {
		t.Errorf("跨国并存 = %v 小时, want ≥12", res.Coexist.Hours)
	}
	if !hasCode(res, "coexist_geo") {
		t.Errorf("跨国并存没有产出证据: %v", evidenceCodes(res))
	}
}

// ---------- Identity Epoch ----------

func TestIdentityEpochSkipsOldRows(t *testing.T) {
	const h = 3600
	old := profRow(1, 10*h, "1.1.1.1", cn("江苏省", "中国电信"), 5<<20)
	old.IdentityVersion = 0
	old.Country, old.City, old.ISP = "", "", ""

	rows := []model.InboundIPHour{
		old,
		profRow(1, 11*h, "2.2.2.2", cn("广东省", "中国移动"), 5<<20),
		profRow(1, 12*h, "3.3.3.3", cn("广东省", "中国移动"), 5<<20),
	}
	if got, want := identityEpoch(rows), int64(11*h); got != want {
		t.Errorf("identityEpoch = %v, want %v（最后一个旧行之后的下一个小时）", got, want)
	}

	// 老行绝不能参与画像——那是没有画像可参与的行，混进来会让同一个分数
	// 一半基于带画像的数据、一半基于不带的。
	res := riskOf(rows)
	for _, p := range res.Profiles {
		if p.Province == "江苏省" {
			t.Errorf("老行参与了画像: %+v", res.Profiles)
		}
	}
}

func TestIdentityEpochUsesEarliestHourWhenAllRowsAreCurrent(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		profRow(1, 20*h, "1.1.1.1", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 10*h, "2.2.2.2", cn("江苏省", "中国电信"), 5<<20),
	}
	if got, want := identityEpoch(rows), int64(10*h); got != want {
		t.Errorf("identityEpoch = %v, want %v", got, want)
	}
}

// 不按行数占比切。一个被扫的小时能塞进 50 行，占比会被少数几个小时拽过门槛，
// 而那几个小时里没有任何有效使用。
func TestIdentityEpochIsNotDrivenByRowCount(t *testing.T) {
	const h = 3600
	var rows []model.InboundIPHour
	// 50 行老数据挤在一个小时里。
	for i := 0; i < 50; i++ {
		r := profRow(1, 5*h, "10.0.0."+string(rune('a'+i%26)), NetworkMeta{}, 5<<20)
		r.IdentityVersion = 0
		rows = append(rows, r)
	}
	// 两行新数据在后面。
	rows = append(rows,
		profRow(1, 6*h, "1.1.1.1", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 7*h, "2.2.2.2", cn("江苏省", "中国电信"), 5<<20),
	)
	if got, want := identityEpoch(rows), int64(6*h); got != want {
		t.Errorf("identityEpoch = %v, want %v——行数占比不该影响切点", got, want)
	}
}

// ---------- 其它不变量 ----------

func TestRiskLevelThresholds(t *testing.T) {
	cases := []struct {
		score int
		want  string
	}{
		{0, RiskLevelLow}, {29, RiskLevelLow},
		{30, RiskLevelWatch}, {44, RiskLevelWatch},
		{45, RiskLevelMedium}, {69, RiskLevelMedium},
		{70, RiskLevelHigh}, {84, RiskLevelHigh},
		{85, RiskLevelSevere}, {100, RiskLevelSevere},
	}
	for _, c := range cases {
		if got := levelOf(c.score); got != c.want {
			t.Errorf("levelOf(%v) = %q, want %q", c.score, got, c.want)
		}
	}
}

// 依据的产出顺序必须确定，否则同一份数据每次渲染出来的次序都不一样。
func TestRiskEvidenceOrderIsDeterministic(t *testing.T) {
	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 5, hours, 46<<20),
		spread(cn("广东省", "中国移动"), "120.86.1.1", 5, hours, 30<<20)...,
	)
	first := evidenceCodes(riskOf(rows))
	for i := 0; i < 20; i++ {
		again := evidenceCodes(riskOf(rows))
		if len(first) != len(again) {
			t.Fatalf("第 %v 次依据条数不同", i)
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("第 %v 次依据顺序不同: %v vs %v", i, first, again)
			}
		}
	}
}

// ---------- Snapshot 层 ----------

func saveSnapshot(t *testing.T, inboundId int, res RiskResult) {
	t.Helper()
	s := SharingRiskService{}
	if err := s.upsertSnapshot(database.GetTrafficDB(), inboundId, res, time.Now()); err != nil {
		t.Fatalf("upsertSnapshot: %v", err)
	}
}

// 版本不匹配的快照绝不能显示它的旧分数。权重改过之后旧分数与新证据对不上，
// 而管理员没有任何办法看出这个数是哪一版算的。
func TestRiskSummaryTreatsStaleEngineVersionAsLearning(t *testing.T) {
	setupSharingTest(t)
	svc := SharingRiskService{}

	saveSnapshot(t, 1, RiskResult{
		EngineVersion: sharingRiskEngineVersion,
		Score:         82, Confidence: 90, Level: RiskLevelHigh, State: RiskStateReady,
	})
	saveSnapshot(t, 2, RiskResult{
		EngineVersion: sharingRiskEngineVersion - 1,
		Score:         82, Confidence: 90, Level: RiskLevelHigh, State: RiskStateReady,
	})

	got, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got[1].Level != RiskLevelHigh || got[1].Score != 82 {
		t.Errorf("当前版本的快照 = %+v, want 原样返回", got[1])
	}
	stale := got[2]
	if stale.State != RiskStateLearning {
		t.Errorf("过期版本 State = %q, want %q", stale.State, RiskStateLearning)
	}
	if stale.Level != "" || stale.Score != 0 {
		t.Errorf("过期版本泄露了旧结果: %+v", stale)
	}
}

// 快照缺失时 Summary 里根本没有这一项——前端据此显示「学习中」，
// **绝不能因为取不到就渲染成低风险**。
func TestRiskSummaryOmitsInboundsWithoutSnapshot(t *testing.T) {
	setupSharingTest(t)
	got, err := (&SharingRiskService{}).Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空库 Summary = %+v, want 空", got)
	}
}

// SQLite 会复用被删除的自增 id：残留快照会绑到下一个建出来的入站上，
// 而它带的是一个红色的「严重」标记。
func TestRiskSnapshotDeleteByInbound(t *testing.T) {
	setupSharingTest(t)
	svc := SharingRiskService{}
	saveSnapshot(t, 7, RiskResult{EngineVersion: sharingRiskEngineVersion, Score: 90, State: RiskStateReady, Level: RiskLevelSevere})

	if err := svc.DeleteByInbound(7); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	got, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if _, ok := got[7]; ok {
		t.Errorf("删除后仍能读到快照: %+v", got)
	}
}

func TestRiskSnapshotPruneOrphans(t *testing.T) {
	setupSharingTest(t)
	svc := SharingRiskService{}

	live := &model.Inbound{UserId: 1, Port: 10001, Protocol: "vmess", Enable: true, Tag: "inbound-10001"}
	if err := database.GetDB().Create(live).Error; err != nil {
		t.Fatalf("建入站: %v", err)
	}
	saveSnapshot(t, live.Id, RiskResult{EngineVersion: sharingRiskEngineVersion, Score: 10, State: RiskStateReady, Level: RiskLevelLow})
	saveSnapshot(t, live.Id+999, RiskResult{EngineVersion: sharingRiskEngineVersion, Score: 90, State: RiskStateReady, Level: RiskLevelSevere})

	pruned, err := svc.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Errorf("清理了 %v 条, want 1", pruned)
	}
	got, _ := svc.Summary()
	if _, ok := got[live.Id]; !ok {
		t.Error("在用入站的快照被误删了")
	}
	if _, ok := got[live.Id+999]; ok {
		t.Error("孤儿快照没被清掉")
	}
}

// upsert 必须覆盖而不是插第二行——主键是 inbound_id，插不进去会静默失败，
// 快照就此冻结在第一次评估的结果上。
func TestRiskSnapshotUpsertOverwrites(t *testing.T) {
	setupSharingTest(t)
	svc := SharingRiskService{}

	saveSnapshot(t, 1, RiskResult{EngineVersion: sharingRiskEngineVersion, Score: 10, State: RiskStateReady, Level: RiskLevelLow})
	saveSnapshot(t, 1, RiskResult{EngineVersion: sharingRiskEngineVersion, Score: 88, State: RiskStateReady, Level: RiskLevelSevere})

	got, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("快照数 = %v, want 1", len(got))
	}
	if got[1].Score != 88 || got[1].Level != RiskLevelSevere {
		t.Errorf("= %+v, want 覆盖成 88/severe", got[1])
	}
}

// 升级后的老行，新列在库里是 **NULL 而不是空串/0**——AutoMigrate 加列就是
// 这个结果，生产上 100% 会遇到（2026-09-11 在美国节点实测：518 行老数据
// 的 country / identity_version 全部 IS NULL）。
//
// 而本文件其余用例都是显式写 IdentityVersion: 0 造的数据，走不到这条路。
// 若 NULL 扫不进非指针的 int / string 字段，windowRows 会整个报错 →
// Analyze 返回 error → Evaluate 记一行 warning 后 continue → **该入站永远
// 没有快照，界面永远显示「学习中」**，而除了一行日志之外没有任何表征。
func TestWindowRowsReadsNullIdentityColumnsFromUpgradedDB(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()

	// 模拟升级前写入、随后被 AutoMigrate 加上新列的行：新列一律 NULL。
	const h = 3600
	err := db.Exec(`INSERT INTO inbound_ip_hours
		(inbound_id, ip, hour_start, province, active_seconds, active_bytes, active_up, active_down,
		 country, city, isp, identity_version)
		VALUES (1, '1.1.1.1', ?, '江苏省', 1800, ?, 0, 0, NULL, NULL, NULL, NULL)`,
		10*h, 5<<20).Error
	if err != nil {
		t.Fatalf("造老行: %v", err)
	}
	// 再加一行升级后的新行。
	if err := upsertIPHour(db, sharingFlush{
		InboundId: 1, IP: "2.2.2.2", HourStart: 12 * h, ActiveSeconds: 1800, ActiveBytes: 5 << 20,
		Meta: NetworkMeta{Country: "中国", Province: "广东省", ISP: "中国移动"},
	}); err != nil {
		t.Fatalf("造新行: %v", err)
	}

	rows, err := (&SharingService{}).windowRows(1, 3650, time.Unix(13*h, 0))
	if err != nil {
		t.Fatalf("windowRows 读不出带 NULL 的行: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("读到 %v 行, want 2", len(rows))
	}

	var old model.InboundIPHour
	for _, r := range rows {
		if r.IP == "1.1.1.1" {
			old = r
		}
	}
	if old.IdentityVersion != 0 {
		t.Errorf("NULL 的 identity_version 读成了 %v, want 0", old.IdentityVersion)
	}
	if old.Country != "" || old.ISP != "" {
		t.Errorf("NULL 的身份列读成了 country=%q isp=%q, want 空串", old.Country, old.ISP)
	}
	// 第一期就在写的 province 必须原样保留——老行仍要服务 /sharing/* 两个接口。
	if old.Province != "江苏省" {
		t.Errorf("老行的 Province = %q, want 江苏省", old.Province)
	}

	// NULL 必须被当成「旧版本」，Epoch 切到它之后。
	if got, want := identityEpoch(rows), int64(11*h); got != want {
		t.Errorf("identityEpoch = %v, want %v（NULL 要算作旧版本行）", got, want)
	}
}
