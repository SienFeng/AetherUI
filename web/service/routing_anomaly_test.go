package service

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// resetAnomalyState 清空包级异常扫描状态：各测试的库都从 id=1 起，残留会串到下一个测试。
func resetAnomalyState(t *testing.T) {
	t.Helper()
	clear := func() {
		anomalyMu.Lock()
		anomalySummaries = map[int]map[int64]*anomalyBucket{}
		anomalyCursors = [2]logCursor{}
		anomalyCursorEpoch++
		anomalyWatermarks = map[int]logCursor{}
		anomalyIgnore = map[int]int64{}
		anomalyLargeBacklogged = false
		anomalyMu.Unlock()
		anomalyPlanMu.Lock()
		anomalyPlans = nil
		anomalyPlanMu.Unlock()
		auditLargeThreshold = auditLargeRuleEntries
		anomalyBatchRows = 20000
	}
	clear()
	t.Cleanup(clear)
}

var anomalyNow = time.UnixMilli(1793520000000)

type anomalyFixture struct {
	a, b *model.Inbound
	node *model.OutboundNode
	rule *model.RoutingRule
	svc  *RoutingAuditService
}

// newAnomalyFixture：规则 proxy→a-ui-us，只覆盖入站 A，组 domain:openai.com。
func newAnomalyFixture(t *testing.T, domains []string, inboundsOf func(a, b *model.Inbound) []int) *anomalyFixture {
	t.Helper()
	resetAuditCache(t)
	resetAnomalyState(t)
	setupAccessLogDB(t)
	setAccessLog(t, true)
	f := &anomalyFixture{
		a:   newVerifyInbound(t, 42001, "用户A", true),
		b:   newVerifyInbound(t, 42002, "用户B", true),
		svc: &RoutingAuditService{now: func() time.Time { return anomalyNow }},
	}
	f.node = &model.OutboundNode{
		Tag: "a-ui-us", Remark: "美国节点", Protocol: "socks", Enable: true,
		Config: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`,
	}
	if err := database.GetDB().Save(f.node).Error; err != nil {
		t.Fatalf("save node: %v", err)
	}
	group := newTestGroupWithDomains(t, "OpenAI", domains)
	ids := []int{f.a.Id}
	if inboundsOf != nil {
		ids = inboundsOf(f.a, f.b)
	}
	f.rule = &model.RoutingRule{
		Remark: "anomaly", InboundIds: mustEncodeIds(t, ids),
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionProxy, OutboundId: f.node.Id, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(f.rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	return f
}

func insertLogs(t *testing.T, n int, row model.AccessLog) {
	t.Helper()
	for i := 0; i < n; i++ {
		r := row
		if err := database.GetAccessLogDB().Create(&r).Error; err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}
}

func wrongRow(inboundId int, at time.Time) model.AccessLog {
	return model.AccessLog{Time: at.UnixMilli(), InboundId: inboundId, Target: "api.openai.com:443", Route: "a-ui-default", Accepted: true}
}

func mustScan(t *testing.T, s *RoutingAuditService, now time.Time, large bool) {
	t.Helper()
	if err := s.ScanAccessLog(now, large); err != nil {
		t.Fatalf("ScanAccessLog(large=%v): %v", large, err)
	}
}

func TestAnomalyScanCountsWrongRoute(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	at := anomalyNow.Add(-time.Hour)
	insertLogs(t, 2, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443", Route: "a-ui-us", Accepted: true})
	insertLogs(t, 3, wrongRow(f.a.Id, at))
	insertLogs(t, 5, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "example.com:443", Route: "a-ui-default", Accepted: true})
	insertLogs(t, 1, wrongRow(f.b.Id, at))
	insertLogs(t, 1, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443", Route: "a-ui-default", Accepted: false})

	mustScan(t, f.svc, anomalyNow, false)
	got := f.svc.Anomaly(f.rule.Id, anomalyNow)
	if got.Count != 3 || got.LastAt != at.UnixMilli() {
		t.Fatalf("Anomaly = %+v，期望 Count=3 LastAt=%d", got, at.UnixMilli())
	}
	ua := got.ByUser[f.a.Id]
	if ua == nil || ua.Count != 3 || ua.Routes["a-ui-default"] != 3 || len(ua.Routes) != 1 {
		t.Fatalf("ByUser[A] = %+v", ua)
	}
	if _, ok := got.ByUser[f.b.Id]; ok {
		t.Errorf("规则不覆盖入站 B，不应计入: %+v", got.ByUser)
	}
}

func TestAnomalyScanIncremental(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 2, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(30*time.Second)))
	later := anomalyNow.Add(time.Minute)
	mustScan(t, f.svc, later, false)
	if got := f.svc.Anomaly(f.rule.Id, later); got.Count != 3 {
		t.Fatalf("两次扫描后 Count = %d，期望 3（不重复计）", got.Count)
	}
}

func TestAnomalyScanCursorSurvivesIdReuse(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 3, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	if err := database.GetAccessLogDB().Where("1 = 1").Delete(&model.AccessLog{}).Error; err != nil {
		t.Fatalf("delete logs: %v", err)
	}
	r := wrongRow(f.a.Id, anomalyNow.Add(10*time.Second))
	if err := database.GetAccessLogDB().Create(&r).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if r.Id != 1 {
		t.Fatalf("前提不成立：清空后新行 id = %d，期望复用为 1", r.Id)
	}
	later := anomalyNow.Add(time.Minute)
	mustScan(t, f.svc, later, false)
	if got := f.svc.Anomaly(f.rule.Id, later); got.Count != 4 {
		t.Fatalf("id 复用后的新行应计入: Count = %d，期望 4", got.Count)
	}
}

func TestAnomalyWindowDropsOld(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 3, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	later := anomalyNow.Add(25 * time.Hour)
	mustScan(t, f.svc, later, false)
	if got := f.svc.Anomaly(f.rule.Id, later); got.Count != 0 || len(got.ByUser) != 0 {
		t.Fatalf("超出 24h 的桶应丢弃: %+v", got)
	}
	anomalyMu.Lock()
	n := len(anomalySummaries[f.rule.Id])
	anomalyMu.Unlock()
	if n != 0 {
		t.Errorf("扫描应把过期桶从内存里删掉，剩 %d 个", n)
	}
}

func TestAnomalyDropsDeletedRules(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 3, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	if f.svc.Anomaly(f.rule.Id, anomalyNow).Count != 3 {
		t.Fatal("前提：删除前应有 3 次异常")
	}
	if err := (&RoutingRuleService{}).Del(f.rule.Id); err != nil {
		t.Fatalf("Del: %v", err)
	}
	mustScan(t, f.svc, anomalyNow.Add(time.Minute), false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(time.Minute)); got.Count != 0 {
		t.Errorf("删除的规则应从摘要移除: %+v", got)
	}
	if _, rules := f.svc.Summaries(); len(rules) != 0 {
		t.Errorf("Summaries 不应含已删除规则: %+v", rules)
	}
}

func TestAnomalyDisabledAccessLogClears(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 3, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	setAccessLog(t, false)
	mustScan(t, f.svc, anomalyNow.Add(time.Minute), false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(time.Minute)); got.Count != 0 {
		t.Errorf("访问日志关闭后摘要应清空: %+v", got)
	}
	anomalyMu.Lock()
	cursors := anomalyCursors
	anomalyMu.Unlock()
	if cursors != [2]logCursor{} {
		t.Errorf("访问日志关闭后游标应清零: %+v", cursors)
	}
}

func TestResetAnomaliesAlsoHidesFromVerify(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 3, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	verifier := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	verifyOthers := func() int {
		t.Helper()
		res, err := verifier.Verify(f.rule.Id)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		u := findUser(t, res, f.a.Id)
		if u.Log == nil {
			t.Fatalf("应有日志证据: %+v", u)
		}
		return len(u.Log.Others)
	}
	if verifyOthers() != 1 {
		t.Fatal("前提：重置前 Verify 应看到走错的连接")
	}

	f.svc.ResetAnomalies(f.rule.Id, anomalyNow)
	if _, rules := f.svc.Summaries(); rules[f.rule.Id].AnomalyCount != 0 {
		t.Errorf("重置后 AnomalyCount 应为 0: %+v", rules[f.rule.Id])
	}
	if n := verifyOthers(); n != 0 {
		t.Errorf("重置后 Verify 不应再列出忽略时刻之前的连接，Others=%d", n)
	}
	// 再扫一遍旧行（游标已过）也不应让它们回来。
	mustScan(t, f.svc, anomalyNow.Add(30*time.Second), false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(30*time.Second)); got.Count != 0 {
		t.Errorf("重置后不应重新出现旧异常: %+v", got)
	}

	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(time.Minute)))
	mustScan(t, f.svc, anomalyNow.Add(2*time.Minute), false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(2*time.Minute)); got.Count != 1 {
		t.Errorf("重置后的新异常应出现: %+v", got)
	}
	if n := verifyOthers(); n != 1 {
		t.Errorf("重置后的新连接应出现在 Verify 里，Others=%d", n)
	}
}

// 列表摘要里忽略时刻同样作用于没扫到的旧行：大规则游标落后时，重置前的行之后才被读到也不计。
func TestResetAnomaliesSkipsLaggingRows(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	insertLogs(t, 2, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	f.svc.ResetAnomalies(f.rule.Id, anomalyNow)
	mustScan(t, f.svc, anomalyNow.Add(time.Minute), false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(time.Minute)); got.Count != 0 {
		t.Errorf("忽略时刻之前的行即使之后才扫到也不应计入: %+v", got)
	}
}

func TestSummariesMergeAnomaly(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	storeVerifyResult(&RuleVerifyResult{RuleId: f.rule.Id, ApplyGen: XrayApplyGeneration(), CheckedAt: 500,
		Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictVerified}}})
	if _, rules := f.svc.Summaries(); rules[f.rule.Id].Verdict != verdictVerified {
		t.Fatalf("前提：无异常时应为 verified: %+v", rules[f.rule.Id])
	}
	at := anomalyNow.Add(-time.Hour)
	insertLogs(t, 2, wrongRow(f.a.Id, at))
	mustScan(t, f.svc, anomalyNow, false)
	_, rules := f.svc.Summaries()
	got := rules[f.rule.Id]
	if got.Verdict != verdictMismatch || got.AnomalyCount != 2 || got.AnomalyLastAt != at.UnixMilli() {
		t.Errorf("核心 verified + 有异常应为 mismatch: %+v", got)
	}
}

// 核心待生效时 verified 会改标 pending，但访问日志异常带来的不符不受影响：
// 那是真实流量的证据，与配置是否已下发无关。
func TestSummariesPendingKeepsAnomalyMismatch(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	f.svc.runtime = func() XrayRuntime { return XrayRuntime{Running: true, Pending: true} }
	storeVerifyResult(&RuleVerifyResult{RuleId: f.rule.Id, ApplyGen: XrayApplyGeneration(), CheckedAt: 500,
		Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictVerified}}})
	insertLogs(t, 2, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, false)
	_, rules := f.svc.Summaries()
	if got := rules[f.rule.Id]; got.Verdict != verdictMismatch || got.AnomalyCount != 2 {
		t.Errorf("待生效时异常仍应为 mismatch: %+v", got)
	}
}

func TestAnomalyLargeRuleOnlyInLargeScan(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	auditLargeThreshold = 2
	insertLogs(t, 2, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))

	mustScan(t, f.svc, anomalyNow, false)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow); got.Count != 0 {
		t.Fatalf("小规则扫描不应处理大规则: %+v", got)
	}
	mustScan(t, f.svc, anomalyNow, true)
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow); got.Count != 2 {
		t.Fatalf("大规则扫描应计入: %+v", got)
	}
	anomalyPlanMu.Lock()
	defer anomalyPlanMu.Unlock()
	if anomalyPlans == nil {
		t.Fatal("前提：应有规则计划缓存")
	}
	for _, p := range anomalyPlans.rules {
		if p.ruleId == f.rule.Id && (!p.large || p.matcher != nil) {
			t.Errorf("大规则不应缓存匹配器: %+v", p)
		}
	}
	if len(anomalyPlans.matchers) != 0 {
		t.Errorf("匹配器缓存里不应有大规则的条目: %v", anomalyPlans.matchers)
	}
}

func TestAnomalyCaps(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, func(a, b *model.Inbound) []int { return []int{} })
	at := anomalyNow.Add(-time.Hour)
	ids := []int{f.a.Id, f.b.Id}
	for i := 0; i < 58; i++ {
		ids = append(ids, newVerifyInbound(t, 43000+i, "用户"+strconv.Itoa(i), true).Id)
	}
	for _, id := range ids {
		insertLogs(t, 1, wrongRow(id, at))
	}
	mustScan(t, f.svc, anomalyNow, false)
	got := f.svc.Anomaly(f.rule.Id, anomalyNow)
	if len(got.ByUser) != anomalyMaxUsers || got.Count != 60 {
		t.Fatalf("len(ByUser)=%d Count=%d，期望 50 / 60", len(got.ByUser), got.Count)
	}

	// 一个用户走错到 7 个不同出站。
	for i := 0; i < 7; i++ {
		r := wrongRow(f.a.Id, at.Add(time.Minute))
		r.Route = "a-ui-x" + strconv.Itoa(i)
		insertLogs(t, 1, r)
	}
	mustScan(t, f.svc, anomalyNow.Add(time.Minute), false)
	got = f.svc.Anomaly(f.rule.Id, anomalyNow.Add(time.Minute))
	ua := got.ByUser[f.a.Id]
	if ua == nil || len(ua.Routes) != anomalyMaxRoutes || ua.Count != 8 {
		t.Fatalf("ByUser[A] = %+v，期望 5 个出站、Count=8", ua)
	}
	if got.Count != 67 {
		t.Errorf("Count = %d，期望 67（含超出上限的出站）", got.Count)
	}
}

// traceLargeMatchers 记录大规则匹配器的构建/释放，并统计同一时刻存活的最大个数。
func traceLargeMatchers(t *testing.T) (events *[]string, maxLive *int) {
	t.Helper()
	events, maxLive = &[]string{}, new(int)
	live := 0
	anomalyLargeTrace = func(event, key string) {
		*events = append(*events, event+":"+key)
		if event == "build" {
			live++
		} else {
			live--
		}
		*maxLive = max(*maxLive, live)
	}
	t.Cleanup(func() { anomalyLargeTrace = nil })
	return events, maxLive
}

func addAnomalyRule(t *testing.T, f *anomalyFixture, inboundIds []int, groups ...*model.DomainGroup) *model.RoutingRule {
	t.Helper()
	ids := []int{}
	for _, g := range groups {
		ids = append(ids, g.Id)
	}
	rule := &model.RoutingRule{
		Remark: "anomaly2", InboundIds: mustEncodeIds(t, inboundIds),
		DomainGroupIds: mustEncodeGroupIds(t, ids),
		Action:         model.ActionProxy, OutboundId: f.node.Id, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	return rule
}

// 两条大规则（不同组序列）：匹配器逐个构建、用完释放，任一时刻至多一个存活。
func TestAnomalyLargeMatchersBuiltOneAtATime(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	g2 := newTestGroupWithDomains(t, "Claude", []string{"domain:claude.ai", "domain:anthropic.com", "full:y.ai"})
	rule2 := addAnomalyRule(t, f, []int{f.b.Id}, g2)
	auditLargeThreshold = 2
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	r := wrongRow(f.b.Id, anomalyNow.Add(-time.Hour))
	r.Target = "api.anthropic.com:443"
	insertLogs(t, 1, r)
	events, maxLive := traceLargeMatchers(t)

	mustScan(t, f.svc, anomalyNow, true)
	if *maxLive != 1 || len(*events) != 4 {
		t.Fatalf("应逐个构建并释放（至多 1 个存活），events=%v maxLive=%d", *events, *maxLive)
	}
	for i, e := range *events {
		if want := []string{"build", "release"}[i%2]; !strings.HasPrefix(e, want+":") {
			t.Fatalf("构建与释放应交替: %v", *events)
		}
	}
	if f.svc.Anomaly(f.rule.Id, anomalyNow).Count != 1 || f.svc.Anomaly(rule2.Id, anomalyNow).Count != 1 {
		t.Errorf("两条大规则都应计入 1 次异常")
	}
}

// 没有候选行（只有不覆盖的入站、走对的、被拒绝的、窗口外的行）时不构建大匹配器。
func TestAnomalyLargeScanSkipsBuildWithoutCandidates(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	auditLargeThreshold = 2
	events, _ := traceLargeMatchers(t)

	mustScan(t, f.svc, anomalyNow, true) // 没有任何行
	at := anomalyNow.Add(-time.Hour)
	insertLogs(t, 1, wrongRow(f.b.Id, at))                                                                                                     // 入站不在覆盖范围
	insertLogs(t, 1, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443", Route: "a-ui-us", Accepted: true}) // 走对
	rejected := wrongRow(f.a.Id, at)
	rejected.Accepted = false
	insertLogs(t, 1, rejected)
	mustScan(t, f.svc, anomalyNow.Add(time.Minute), true)
	if len(*events) != 0 {
		t.Fatalf("没有候选行时不应构建匹配器: %v", *events)
	}
}

// 分档按合并（去重）后的条目数：两个组各 2 条、重叠 1 条 → 合并 3 条为大；完全重叠 → 2 条为小。
func TestAnomalyClassifyUsesMergedCount(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	g1 := newTestGroupWithDomains(t, "g1", []string{"domain:a.com", "domain:b.com"})
	g2 := newTestGroupWithDomains(t, "g2", []string{"domain:b.com", "domain:c.com"})
	g3 := newTestGroupWithDomains(t, "g3", []string{"domain:x.com", "domain:y.com"})
	g4 := newTestGroupWithDomains(t, "g4", []string{"domain:y.com", "domain:x.com"})
	overlap := addAnomalyRule(t, f, []int{f.b.Id}, g1, g2)
	same := addAnomalyRule(t, f, []int{f.b.Id}, g3, g4)
	auditLargeThreshold = 2

	plans, _, err := f.svc.anomalyPlanSet()
	if err != nil {
		t.Fatalf("anomalyPlanSet: %v", err)
	}
	got := map[int]*anomalyRulePlan{}
	for _, p := range plans.rules {
		got[p.ruleId] = p
	}
	if p := got[overlap.Id]; p == nil || !p.large || p.matcher != nil || len(p.groupIds) != 2 {
		t.Errorf("合并后 3 条应为大规则: %+v", p)
	}
	if p := got[same.Id]; p == nil || p.large || p.matcher == nil {
		t.Errorf("合并后 2 条应为小规则且有常驻匹配器: %+v", p)
	}
	if p := got[f.rule.Id]; p == nil || p.large || p.matcher == nil {
		t.Errorf("单组 1 条应为小规则: %+v", p)
	}

	// 大规则的组在计划重建时只放了占位，扫描时仍要按真实条目判定。
	r := wrongRow(f.b.Id, anomalyNow.Add(-time.Hour))
	r.Target = "www.c.com:443"
	insertLogs(t, 1, r)
	mustScan(t, f.svc, anomalyNow, true)
	if n := f.svc.Anomaly(overlap.Id, anomalyNow).Count; n != 1 {
		t.Errorf("大规则应按真实条目命中 c.com: Count=%d", n)
	}
}

func TestLargeScanBacklogged(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com"}, nil)
	auditLargeThreshold = 0 // 让这条规则归入大规则一档
	anomalyBatchRows = 3
	insertLogs(t, 5, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))

	mustScan(t, f.svc, anomalyNow, true)
	if !f.svc.LargeScanBacklogged() {
		t.Fatal("读满一批（3/3）后应报告积压")
	}
	mustScan(t, f.svc, anomalyNow, true) // 剩 2 行，读不满
	if f.svc.LargeScanBacklogged() {
		t.Fatal("读不满一批后应清除积压标记")
	}
}
