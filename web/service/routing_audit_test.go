package service

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/json_util"
	"a-ui/xray"
)

// resetAuditCache 清空包级审计缓存：各测试的库都从 id=1 起，残留会串到下一个测试。
func resetAuditCache(t *testing.T) {
	t.Helper()
	auditMu.Lock()
	auditResults = map[int]*RuleVerifyResult{}
	auditMu.Unlock()
	largeMatchers.reset()
	t.Cleanup(func() {
		auditMu.Lock()
		auditResults = map[int]*RuleVerifyResult{}
		auditMu.Unlock()
		largeMatchers.reset()
	})
}

// 后台全量与按需单条必须是同一份判定：同一条规则两边给出的用户结论与样例逐项一致，
// 停用规则不进全量结果。
func TestVerifyAllMatchesVerify(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	proxyRule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	blockRule := f.addRule(t, model.ActionBlock, []int{f.b.Id})
	// 停用规则用另一个组，避免与上面两条在「组 × 入站」上冲突（停用规则同样占位）。
	other := newTestGroupWithDomains(t, "Other", []string{"full:other.example"})
	disabled := &model.RoutingRule{
		Remark: "disabled", InboundIds: mustEncodeIds(t, []int{f.a.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{other.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(disabled); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	if err := database.GetDB().Model(disabled).Update("enable", false).Error; err != nil {
		t.Fatalf("disable rule: %v", err)
	}

	rt := okRuntime()
	rt.ApplyGen = 7
	s := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, rt)
	all, gen, err := s.VerifyAll()
	if err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	if gen != 7 {
		t.Errorf("gen = %d，期望 7", gen)
	}
	if len(all) != 2 {
		t.Fatalf("应只有两条启用规则的结果: %+v", all)
	}
	if _, ok := all[disabled.Id]; ok {
		t.Error("停用规则不应出现在全量结果里")
	}
	for _, id := range []int{proxyRule.Id, blockRule.Id} {
		got, ok := all[id]
		if !ok {
			t.Fatalf("缺少规则 %d 的结果", id)
		}
		if got.ApplyGen != 7 {
			t.Errorf("规则 %d ApplyGen = %d", id, got.ApplyGen)
		}
		want, err := s.Verify(id)
		if err != nil {
			t.Fatalf("Verify(%d): %v", id, err)
		}
		if got.Core != want.Core || got.Expected != want.Expected {
			t.Errorf("规则 %d Core/Expected 不一致: all=%+v/%+v verify=%+v/%+v", id, got.Core, got.Expected, want.Core, want.Expected)
		}
		if len(got.Users) != len(want.Users) || len(got.Users) == 0 {
			t.Fatalf("规则 %d Users 数不一致: all=%+v verify=%+v", id, got.Users, want.Users)
		}
		for i := range got.Users {
			g, w := got.Users[i], want.Users[i]
			if g.InboundId != w.InboundId || g.Verdict != w.Verdict || g.Reason != w.Reason {
				t.Errorf("规则 %d 用户 %d 结论不一致: all=%+v verify=%+v", id, i, g, w)
			}
			if !reflect.DeepEqual(g.Samples, w.Samples) {
				t.Errorf("规则 %d 用户 %d 样例不一致: all=%+v verify=%+v", id, i, g.Samples, w.Samples)
			}
		}
	}
	if v := all[proxyRule.Id].Users[0].Verdict; v != verdictVerified {
		t.Errorf("proxy 规则应 verified: %s", v)
	}
	if v := all[blockRule.Id].Users[0].Verdict; v != verdictMismatch {
		t.Errorf("block 规则实际走了节点，应 mismatch: %s", v)
	}
}

// 一轮内共享的工作只做一次：两条规则都要默认出站 tag 时 GetXrayConfig 只调用一次。
func TestVerifyAllSharesWork(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	first := f.addRule(t, model.ActionDirect, []int{f.a.Id})
	// 同组同入站的第二条规则过不了 checkConflict，直接落库：生成期照常输出两条，
	// 正是迁移前遗留冲突数据的形态。
	second := &model.RoutingRule{
		Remark: "dup", InboundIds: first.InboundIds, DomainGroupIds: first.DomainGroupIds,
		Action: model.ActionDirect, Enable: true, Priority: 1,
	}
	if err := database.GetDB().Save(second).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	calls := 0
	rt := okRuntime()
	rt.DefaultTag = "" // 迫使两条 direct 规则都去配置里取默认出站
	fr := &fakeRoute{fn: returnTag("my-direct")}
	s := newVerifyService(fr, rt)
	s.xrayConfig = func() (*xray.Config, error) {
		calls++
		return &xray.Config{OutboundConfigs: json_util.RawMessage(`[{"protocol":"freedom","tag":"my-direct"}]`)}, nil
	}
	all, _, err := s.VerifyAll()
	if err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	if calls != 1 {
		t.Errorf("GetXrayConfig 调用 %d 次，期望一轮内只调用 1 次", calls)
	}
	for _, id := range []int{first.Id, second.Id} {
		res := all[id]
		if res == nil || res.Expected.Tag != "my-direct" {
			t.Fatalf("规则 %d 的期望出站应取自配置的默认出站: %+v", id, res)
		}
		if u := findUser(t, res, f.a.Id); u.Verdict != verdictVerified {
			t.Errorf("规则 %d 应 verified: %+v", id, u)
		}
	}
	if fr.count() != 4 {
		t.Errorf("两条规则各两个样例，route 应调用 4 次，实际 %d", fr.count())
	}
}

// 核心没就绪、核心忙、已有验证在跑：一律报错且不调用 route，RunAudit 不动缓存。
func TestVerifyAllCoreNotReady(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	old := &RuleVerifyResult{RuleId: rule.Id, CheckedAt: 1, ApplyGen: 3, Users: []UserVerify{}}
	storeVerifyResult(old)

	cases := []struct {
		name string
		svc  func(fr *fakeRoute) *RoutingVerifyService
		lock bool
		want error
	}{
		{"not running", func(fr *fakeRoute) *RoutingVerifyService { return newVerifyService(fr, XrayRuntime{ApplyGen: 9}) }, false, ErrCoreNotReady},
		{"busy", func(fr *fakeRoute) *RoutingVerifyService { return newVerifyService(fr, XrayRuntime{Busy: true}) }, false, ErrVerifyBusy},
		{"verifyMu held", func(fr *fakeRoute) *RoutingVerifyService { return newVerifyService(fr, okRuntime()) }, true, ErrVerifyBusy},
		{"init failure", func(*fakeRoute) *RoutingVerifyService {
			// route 为 nil 走真实控制面，端口 -1 必然连不上。
			return &RoutingVerifyService{runtime: func() XrayRuntime {
				return XrayRuntime{Running: true, APIPort: -1, DefaultTag: model.DefaultOutboundTag, ApplyGen: 9}
			}}
		}, false, ErrCoreNotReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRoute{fn: returnTag(f.node.Tag)}
			if c.lock {
				verifyMu.Lock()
				defer verifyMu.Unlock()
			}
			res, _, err := c.svc(fr).VerifyAll()
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v，期望 %v", err, c.want)
			}
			if res != nil {
				t.Errorf("出错时不应返回结果: %+v", res)
			}
			if fr.count() != 0 {
				t.Errorf("不应调用 route，实际 %d 次", fr.count())
			}

			gen, err := (&RoutingAuditService{verify: c.svc(fr)}).RunAudit()
			if !errors.Is(err, c.want) || gen != 0 {
				t.Fatalf("RunAudit = %d, %v，期望 0, %v", gen, err, c.want)
			}
			if got, ok := cachedVerifyResult(rule.Id); !ok || got != old {
				t.Errorf("RunAudit 失败不应改动缓存: %+v", got)
			}
		})
	}
	if !verifyMu.TryLock() {
		t.Fatal("VerifyAll 返回后 verifyMu 应已释放")
	}
	verifyMu.Unlock()
}

// RunAudit 成功：写入结果、返回本轮代次。
func TestVerifyAllRunAuditStores(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	rt := okRuntime()
	rt.ApplyGen = 11
	gen, err := (&RoutingAuditService{verify: newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, rt)}).RunAudit()
	if err != nil || gen != 11 {
		t.Fatalf("RunAudit = %d, %v", gen, err)
	}
	got, ok := (&RoutingAuditService{}).Cached(rule.Id)
	if !ok || got.ApplyGen != 11 || findUser(t, got, f.a.Id).Verdict != verdictVerified {
		t.Fatalf("缓存 = %+v, %v", got, ok)
	}
}

func TestStoreVerifyResultNewerWins(t *testing.T) {
	resetAuditCache(t)
	storeVerifyResult(&RuleVerifyResult{RuleId: 1, ApplyGen: 5, CheckedAt: 100})
	storeVerifyResult(&RuleVerifyResult{RuleId: 1, ApplyGen: 4, CheckedAt: 200})
	if got, _ := cachedVerifyResult(1); got.ApplyGen != 5 || got.CheckedAt != 100 {
		t.Fatalf("旧代次不应覆盖新代次: %+v", got)
	}
	storeVerifyResult(&RuleVerifyResult{RuleId: 1, ApplyGen: 5, CheckedAt: 50})
	if got, _ := cachedVerifyResult(1); got.CheckedAt != 100 {
		t.Fatalf("同代次更旧的结果不应覆盖: %+v", got)
	}
	storeVerifyResult(&RuleVerifyResult{RuleId: 1, ApplyGen: 5, CheckedAt: 300})
	if got, _ := cachedVerifyResult(1); got.CheckedAt != 300 {
		t.Fatalf("同代次更新的结果应覆盖: %+v", got)
	}
	storeVerifyResult(&RuleVerifyResult{RuleId: 1, ApplyGen: 6, CheckedAt: 10})
	if got, _ := cachedVerifyResult(1); got.ApplyGen != 6 {
		t.Fatalf("新代次应覆盖，哪怕 CheckedAt 更小: %+v", got)
	}
	if _, ok := cachedVerifyResult(2); ok {
		t.Error("没存过的规则不应有缓存")
	}
}

// 手动验证有核心证据时写回缓存；拿不到 verifyMu、核心未运行时不写（不能用没有证据的结果顶掉旧结论）。
func TestStoreVerifyResultFromVerify(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})

	if _, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, XrayRuntime{}).Verify(rule.Id); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, ok := cachedVerifyResult(rule.Id); ok {
		t.Fatal("核心未运行的结果不应写进缓存")
	}

	verifyMu.Lock()
	_, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(rule.Id)
	verifyMu.Unlock()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, ok := cachedVerifyResult(rule.Id); ok {
		t.Fatal("已有验证在进行时的 busy 结果不应写进缓存")
	}

	rt := okRuntime()
	rt.ApplyGen = 4
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, rt).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got, ok := cachedVerifyResult(rule.Id)
	if !ok || got != res || got.ApplyGen != 4 {
		t.Fatalf("有核心证据的结果应写回缓存: %+v, %v", got, ok)
	}
}

func TestAuditCacheDropsDeletedRules(t *testing.T) {
	resetAuditCache(t)
	replaceAuditResults(map[int]*RuleVerifyResult{
		1: {RuleId: 1, ApplyGen: 1, CheckedAt: 1},
		2: {RuleId: 2, ApplyGen: 1, CheckedAt: 1},
	}, []int{1, 2})
	if _, ok := cachedVerifyResult(2); !ok {
		t.Fatal("规则 2 应已写入")
	}
	replaceAuditResults(map[int]*RuleVerifyResult{1: {RuleId: 1, ApplyGen: 2, CheckedAt: 2}}, []int{1})
	if _, ok := cachedVerifyResult(2); ok {
		t.Error("已删除/停用的规则 2 应被移除")
	}
	if got, ok := cachedVerifyResult(1); !ok || got.ApplyGen != 2 {
		t.Errorf("规则 1 应被新结果覆盖: %+v", got)
	}
	// 启用但本轮没有结果（预算用完没轮到）的规则保留旧结果。
	replaceAuditResults(map[int]*RuleVerifyResult{}, []int{1})
	if _, ok := cachedVerifyResult(1); !ok {
		t.Error("本轮没有新结果的启用规则应保留旧结果")
	}
	// 结果里带着不在启用列表里的规则（本轮期间被删）：不写入。
	replaceAuditResults(map[int]*RuleVerifyResult{3: {RuleId: 3, ApplyGen: 3}}, []int{1})
	if _, ok := cachedVerifyResult(3); ok {
		t.Error("不在启用列表里的规则不应写入")
	}
}

// Summaries 遍历启用规则：无缓存的给 unverifiable、CheckedAt=0；有缓存的按 ruleVerdictOf 汇总。
func TestAuditCacheSummaries(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	verified := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	missing := f.addRule(t, model.ActionBlock, []int{f.b.Id})
	gen := XrayApplyGeneration()
	storeVerifyResult(&RuleVerifyResult{RuleId: verified.Id, ApplyGen: gen, CheckedAt: 500, Users: []UserVerify{
		{Verdict: verdictVerified}, {Verdict: verdictMismatch},
	}})
	storeVerifyResult(&RuleVerifyResult{RuleId: 999, ApplyGen: gen, CheckedAt: 500})

	_, rules := (&RoutingAuditService{}).Summaries()
	if len(rules) != 2 {
		t.Fatalf("只应列启用规则: %+v", rules)
	}
	if got := rules[verified.Id]; got.Verdict != verdictMismatch || got.MismatchUsers != 1 || got.CheckedAt != 500 || got.ApplyGen != gen || got.Stale {
		t.Errorf("verified 规则摘要 = %+v", got)
	}
	if got := rules[missing.Id]; got.Verdict != verdictUnverifiable || got.CheckedAt != 0 {
		t.Errorf("无缓存规则摘要 = %+v", got)
	}
}

func TestRuleVerdictOf(t *testing.T) {
	users := func(vs ...string) []UserVerify {
		out := []UserVerify{}
		for _, v := range vs {
			out = append(out, UserVerify{Verdict: v})
		}
		return out
	}
	cases := []struct {
		name     string
		res      *RuleVerifyResult
		verdict  string
		mismatch int
	}{
		{"全部 verified", &RuleVerifyResult{Users: users(verdictVerified, verdictVerified)}, verdictVerified, 0},
		{"unverifiable 压过 verified", &RuleVerifyResult{Users: users(verdictVerified, verdictUnverifiable)}, verdictUnverifiable, 0},
		{"pending 压过 unverifiable", &RuleVerifyResult{Users: users(verdictUnverifiable, verdictPending, verdictVerified)}, verdictPending, 0},
		{"mismatch 压过一切并计数", &RuleVerifyResult{Users: users(verdictPending, verdictMismatch, verdictMismatch, verdictVerified)}, verdictMismatch, 2},
		{"认不出的结论不给绿", &RuleVerifyResult{Users: users(verdictVerified, "weird")}, verdictUnverifiable, 0},
		// 没有任何用户行就没有证据：核心 ok 也不给绿（spec A1）。
		{"无用户 ok", &RuleVerifyResult{Core: CoreState{State: coreStateOK}, Users: users()}, verdictUnverifiable, 0},
		{"无用户 pending", &RuleVerifyResult{Core: CoreState{State: coreStatePending}, Users: users()}, verdictPending, 0},
		{"无用户 error", &RuleVerifyResult{Core: CoreState{State: coreStateError}, Users: users()}, verdictUnverifiable, 0},
		{"无用户 busy", &RuleVerifyResult{Core: CoreState{State: coreStateBusy}}, verdictUnverifiable, 0},
		{"无用户 not_running", &RuleVerifyResult{Core: CoreState{State: coreStateNotRunning}}, verdictUnverifiable, 0},
		{"nil", nil, verdictUnverifiable, 0},
		// 停用用户不参与规则级结论
		{"停用用户不拖累已验证", &RuleVerifyResult{Users: append(users(verdictVerified), UserVerify{Verdict: verdictUnverifiable, Disabled: true})}, verdictVerified, 0},
		{"停用用户不掩盖不符", &RuleVerifyResult{Users: append(users(verdictMismatch), UserVerify{Verdict: verdictUnverifiable, Disabled: true})}, verdictMismatch, 1},
		{"全部停用 ok 不给绿", &RuleVerifyResult{Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictUnverifiable, Disabled: true}}}, verdictUnverifiable, 0},
		{"全部停用 pending", &RuleVerifyResult{Core: CoreState{State: coreStatePending}, Users: []UserVerify{{Verdict: verdictUnverifiable, Disabled: true}}}, verdictPending, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, n := ruleVerdictOf(c.res)
			if v != c.verdict || n != c.mismatch {
				t.Errorf("ruleVerdictOf = %s, %d，期望 %s, %d", v, n, c.verdict, c.mismatch)
			}
		})
	}
}

// 整轮预算用完：被截断的规则不进结果，RunAudit 保留它的旧结果；手动验证同口径不写缓存。
func TestVerifyAllBudgetExhaustedKeepsOldResult(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	first := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	second := f.addRule(t, model.ActionBlock, []int{f.b.Id})
	old := &RuleVerifyResult{RuleId: first.Id, ApplyGen: 1, CheckedAt: 1, Users: []UserVerify{}}
	storeVerifyResult(old)

	fr := &fakeRoute{fn: func(ctx context.Context, _, _ string, _ net.IP) (string, bool, error) {
		<-ctx.Done()
		return "", false, ctx.Err()
	}}
	s := newVerifyService(fr, okRuntime())
	s.rpcTimeout = time.Second
	s.budget = 30 * time.Millisecond

	all, _, err := s.VerifyAll()
	if !errors.Is(err, ErrVerifyTruncated) {
		t.Fatalf("预算用完应返回 ErrVerifyTruncated，实际 %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("预算用完时被截断与没轮到的规则都不应有结果: %+v", all)
	}
	if fr.count() != 1 {
		t.Errorf("预算用完后不应再调 route，实际 %d 次", fr.count())
	}

	// 被截断的一轮不算成功：RunAudit 把错误交给任务，任务据此不推进 lastGen/lastAt。
	if _, err := (&RoutingAuditService{verify: s}).RunAudit(); !errors.Is(err, ErrVerifyTruncated) {
		t.Fatalf("RunAudit 应返回 ErrVerifyTruncated，实际 %v", err)
	}
	if got, ok := cachedVerifyResult(first.Id); !ok || got != old {
		t.Errorf("本轮没有新结果的规则应保留旧结果: %+v", got)
	}
	if _, ok := cachedVerifyResult(second.Id); ok {
		t.Error("从未验证过的规则不应凭空有结果")
	}

	if _, err := s.Verify(first.Id); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got, _ := cachedVerifyResult(first.Id); got != old {
		t.Errorf("手动验证超出预算的结果不应写进缓存: %+v", got)
	}
}

// runningCore 是「核心在跑、持有当前配置」的快照，供只关心别的条件的 Summaries 测试注入。
// 代次取此刻的真实代次：快照代次与 Summaries 开头读到的不同会被当成「中途换过配置」。
func runningCore() XrayRuntime { return XrayRuntime{Running: true, ApplyGen: XrayApplyGeneration()} }

// 代次前进后，旧代次的结果不再代表核心现状：标 stale、结论按 pending，绝不给绿。
func TestSummariesStaleWhenGenerationAdvanced(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	gen := XrayApplyGeneration()
	storeVerifyResult(&RuleVerifyResult{RuleId: rule.Id, ApplyGen: gen, CheckedAt: 500,
		Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictVerified}}})

	svc := &RoutingAuditService{runtime: runningCore}
	if _, rules := svc.Summaries(); rules[rule.Id].Verdict != verdictVerified || rules[rule.Id].Stale {
		t.Fatalf("同代次应为 verified 且不 stale: %+v", rules[rule.Id])
	}

	xrayApplyGen.Inc()
	cur, rules := svc.Summaries()
	if cur != gen+1 {
		t.Fatalf("Summaries 代次 = %d，期望 %d", cur, gen+1)
	}
	got := rules[rule.Id]
	if !got.Stale || got.Verdict != verdictPending || got.ApplyGen != gen || got.CheckedAt != 500 {
		t.Errorf("代次前进后应 stale + pending 且保留旧结果的代次与时间: %+v", got)
	}
}

// 保存后到核心载入新配置之前代次不变，同代次缓存是旧配置的结论：核心 Pending（或正忙于
// 重启/热应用）时本会是 verified 的摘要改标 stale + pending；核心未运行时改标
// stale + unverifiable。核心那一路的不符原样保留。快照整轮只取一次。
func TestSummariesPendingCoreNeverGreen(t *testing.T) {
	cases := []struct {
		name string
		rt   XrayRuntime
		want string // 绿规则此刻的结论
	}{
		{"pending", XrayRuntime{Running: true, Pending: true}, verdictPending},
		{"busy", XrayRuntime{Busy: true}, verdictPending},
		{"current", XrayRuntime{Running: true}, verdictVerified},
		{"not running", XrayRuntime{}, verdictUnverifiable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAuditCache(t)
			f := newVerifyFixture(t)
			green := f.addRule(t, model.ActionProxy, []int{f.a.Id})
			red := f.addRule(t, model.ActionBlock, []int{f.b.Id})
			gen := XrayApplyGeneration()
			storeVerifyResult(&RuleVerifyResult{RuleId: green.Id, ApplyGen: gen, CheckedAt: 500,
				Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictVerified}}})
			storeVerifyResult(&RuleVerifyResult{RuleId: red.Id, ApplyGen: gen, CheckedAt: 500,
				Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictMismatch}}})
			calls := 0
			rt := tc.rt
			if !rt.Busy { // 真实快照 Busy 时代次为零值，其余情形带着当前代次
				rt.ApplyGen = gen
			}
			svc := &RoutingAuditService{runtime: func() XrayRuntime { calls++; return rt }}

			_, rules := svc.Summaries()
			got := rules[green.Id]
			if tc.want == verdictVerified {
				if got.Verdict != verdictVerified || got.Stale {
					t.Errorf("核心持有当前配置时应保留 verified: %+v", got)
				}
			} else if got.Verdict != tc.want || !got.Stale || got.CheckedAt != 500 {
				t.Errorf("不得给绿，应 stale + %s 并保留时间: %+v", tc.want, got)
			}
			if r := rules[red.Id]; r.Verdict != verdictMismatch || r.MismatchUsers != 1 {
				t.Errorf("核心那一路的不符应原样保留: %+v", r)
			}
			if calls != 1 {
				t.Errorf("快照调用次数 = %d，期望整轮一次", calls)
			}
		})
	}
}

// 没有任何用户行的规则（全局规则但没有启用入站）不给绿。
func TestSummariesEmptyRuleUnverifiable(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	storeVerifyResult(&RuleVerifyResult{RuleId: rule.Id, ApplyGen: XrayApplyGeneration(), CheckedAt: 500,
		Core: CoreState{State: coreStateOK}, Users: []UserVerify{}})
	_, rules := (&RoutingAuditService{}).Summaries()
	if got := rules[rule.Id]; got.Verdict != verdictUnverifiable || got.Stale {
		t.Errorf("无用户规则应 unverifiable: %+v", got)
	}
}

// 核心刚拉起、控制面端口还没人听：grpc.NewClient 惰性连接让 Init 照样成功，
// 必须在发样例之前就识别出来报 ErrCoreNotReady，不能把一轮全灰的结果当成功写进缓存。
func TestVerifyAllCoreNotReadyClosedPort(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	old := &RuleVerifyResult{RuleId: rule.Id, CheckedAt: 1, ApplyGen: 3, Users: []UserVerify{}}
	storeVerifyResult(old)

	// 先监听再关闭，拿到一个合法但没人监听的端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	s := &RoutingVerifyService{runtime: func() XrayRuntime {
		return XrayRuntime{Running: true, APIPort: port, DefaultTag: model.DefaultOutboundTag, ApplyGen: 9}
	}}
	start := time.Now()
	res, gen, err := s.VerifyAll()
	if !errors.Is(err, ErrCoreNotReady) || res != nil || gen != 0 {
		t.Fatalf("VerifyAll = %v, %d, %v，期望 nil, 0, ErrCoreNotReady", res, gen, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("连接被拒应很快识别，耗时 %v", d)
	}
	if _, err := (&RoutingAuditService{verify: s}).RunAudit(); !errors.Is(err, ErrCoreNotReady) {
		t.Fatalf("RunAudit err = %v，期望 ErrCoreNotReady", err)
	}
	if got, _ := cachedVerifyResult(rule.Id); got != old {
		t.Errorf("核心未就绪时缓存不应改动: %+v", got)
	}

	// 手动验证同样走这条判定：如实报核心错误、不写缓存。
	one, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if one.Core.State != coreStateError {
		t.Errorf("Core = %+v，期望 error", one.Core)
	}
	if got, _ := cachedVerifyResult(rule.Id); got != old {
		t.Errorf("连不上核心的手动验证不应写缓存: %+v", got)
	}
}

// 连接就绪后核心中途退出：任一 RPC 报 Unavailable，整轮不出结果。
func TestVerifyAllUnavailableMidRound(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	f.addRule(t, model.ActionProxy, []int{f.a.Id})
	fr := &fakeRoute{fn: func(context.Context, string, string, net.IP) (string, bool, error) {
		return "", false, status.Error(codes.Unavailable, "connection refused")
	}}
	res, _, err := newVerifyService(fr, okRuntime()).VerifyAll()
	if !errors.Is(err, ErrCoreNotReady) || res != nil {
		t.Fatalf("VerifyAll = %v, %v，期望 nil, ErrCoreNotReady", res, err)
	}
}

// 预算截断的一轮：已跑完的规则结果写入缓存，但 RunAudit 报 ErrVerifyTruncated；
// 下一轮没有当前代次结果的规则排在前面，不会每轮卡在同一处。
func TestVerifyAllTruncatedStoresPartialAndReorders(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	fast := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	slow := f.addRule(t, model.ActionBlock, []int{f.b.Id})
	fr := &fakeRoute{fn: func(ctx context.Context, inboundTag, _ string, _ net.IP) (string, bool, error) {
		if inboundTag == f.b.Tag {
			<-ctx.Done()
			return "", false, ctx.Err()
		}
		return f.node.Tag, false, nil
	}}
	rt := okRuntime()
	rt.ApplyGen = 21
	s := newVerifyService(fr, rt)
	s.rpcTimeout = time.Second
	s.budget = 200 * time.Millisecond

	gen, err := (&RoutingAuditService{verify: s}).RunAudit()
	if !errors.Is(err, ErrVerifyTruncated) || gen != 21 {
		t.Fatalf("RunAudit = %d, %v，期望 21, ErrVerifyTruncated", gen, err)
	}
	if got, ok := cachedVerifyResult(fast.Id); !ok || got.ApplyGen != 21 {
		t.Fatalf("跑完的规则应写入缓存: %+v", got)
	}
	if _, ok := cachedVerifyResult(slow.Id); ok {
		t.Fatal("被截断的规则不应有结果")
	}

	rules := []*model.RoutingRule{fast, slow}
	orderRulesForRound(rules, 21)
	if rules[0].Id != slow.Id {
		t.Errorf("下一轮应先跑没有当前代次结果的规则，顺序 = [%d %d]", rules[0].Id, rules[1].Id)
	}
}

// 代次在 Summaries 开头读取与取快照之间前进（中途热应用）：同代次缓存是换之前的结论，
// 不能给绿。
func TestSummariesSnapshotGenerationAdvanced(t *testing.T) {
	resetAuditCache(t)
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	gen := XrayApplyGeneration()
	storeVerifyResult(&RuleVerifyResult{RuleId: rule.Id, ApplyGen: gen, CheckedAt: 500,
		Core: CoreState{State: coreStateOK}, Users: []UserVerify{{Verdict: verdictVerified}}})
	svc := &RoutingAuditService{runtime: func() XrayRuntime {
		return XrayRuntime{Running: true, ApplyGen: gen + 1}
	}}
	_, rules := svc.Summaries()
	if got := rules[rule.Id]; got.Verdict != verdictPending || !got.Stale {
		t.Errorf("快照代次已前进时应 stale + pending: %+v", got)
	}
}

// 待生效判定的记忆：同键 ttl 内复用；过期、代次/改动序号/配置对象变化都现算；
// 判定出错不记。
func TestSummariesPendingMemo(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := &pendingMemo{ttl: 5 * time.Second, now: func() time.Time { return now }}
	calls, result := 0, true
	compute := func() (bool, bool) { calls++; return result, true }
	cfg := &xray.Config{}
	key := pendingKey{applyGen: 1, changeSeq: 1, current: cfg}

	if !m.get(key, compute) || calls != 1 {
		t.Fatalf("首次应现算，calls=%d", calls)
	}
	result = false
	now = now.Add(4 * time.Second)
	if !m.get(key, compute) || calls != 1 {
		t.Fatalf("ttl 内同键应复用，calls=%d", calls)
	}
	for name, k := range map[string]pendingKey{
		"代次":   {applyGen: 2, changeSeq: 1, current: cfg},
		"改动序号": {applyGen: 1, changeSeq: 2, current: cfg},
		"配置对象": {applyGen: 1, changeSeq: 1, current: &xray.Config{}},
	} {
		before := calls
		m.get(k, compute)
		if calls != before+1 {
			t.Errorf("%s变化应现算", name)
		}
		m.get(key, compute) // 换回原键同样现算，记忆只有一份
	}
	before := calls
	now = now.Add(6 * time.Second)
	m.get(key, compute)
	if calls != before+1 {
		t.Error("过期应现算")
	}
	// 出错的判定不进记忆：下一次仍现算。
	failing := func() (bool, bool) { calls++; return false, false }
	k2 := pendingKey{applyGen: 9}
	m.get(k2, failing)
	before = calls
	m.get(k2, compute)
	if calls != before+1 {
		t.Error("出错的判定不应被记住")
	}

	// 要求下发改动的两个入口都会让记忆失效。
	seq := xrayChangeSeq.Load()
	(&XrayService{}).SetToNeedRestart()
	(&XrayService{}).IsNeedRestartAndSetFalse()
	if xrayChangeSeq.Load() != seq+1 {
		t.Errorf("SetToNeedRestart 应递增改动序号: %d -> %d", seq, xrayChangeSeq.Load())
	}
}
