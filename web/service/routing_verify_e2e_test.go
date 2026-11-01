package service

import (
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// 用真实 xray 走一遍路由验证：规则下发后核心实时路由与期望一致才给 verified，
// 改了规则还没下发时必须是 pending，推不出样例的规则必须是 unverifiable。
func TestRouteVerifyAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	requireXrayRoutingService(t)
	requireXrayAPIPortFree(t)
	setupDB(t)

	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})

	newSocks := func(remark string) *model.Inbound {
		port := freePort(t)
		in := &model.Inbound{
			UserId: 1, Port: port, Protocol: model.Protocol("socks"), Enable: true, Remark: remark,
			Tag:            "inbound-" + strconv.Itoa(port),
			Listen:         "127.0.0.1",
			Settings:       `{"auth":"noauth","udp":false}`,
			StreamSettings: `{"network":"tcp","security":"none"}`,
			Sniffing:       "{}",
		}
		if err := database.GetDB().Save(in).Error; err != nil {
			t.Fatalf("save inbound: %v", err)
		}
		return in
	}
	a := newSocks("e2e-A")
	b := newSocks("e2e-B")

	group := newTestGroupWithDomains(t, "verify-localhost", []string{"full:localhost"})
	rs := &RoutingRuleService{}
	rule := &model.RoutingRule{
		Remark: "verify-e2e", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := rs.Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, a.Port)
	waitForPort(t, b.Port)

	vs := &RoutingVerifyService{}
	verify := func(id int) *RuleVerifyResult {
		t.Helper()
		res, err := vs.Verify(id)
		if err != nil {
			t.Fatalf("Verify(%d): %v", id, err)
		}
		return res
	}
	onlyUser := func(res *RuleVerifyResult) UserVerify {
		t.Helper()
		if len(res.Users) != 1 || res.Users[0].InboundId != a.Id {
			t.Fatalf("规则只覆盖 A，Users 应只有 A: %+v", res.Users)
		}
		return res.Users[0]
	}

	// 1. block 规则已下发：核心对 A 访问 localhost 应走 a-ui-block。
	res := verify(rule.Id)
	if res.Core.State != coreStateOK {
		t.Fatalf("启动后 Core = %+v", res.Core)
	}
	u := onlyUser(res)
	if u.Verdict != verdictVerified || len(u.Samples) != 1 || u.Samples[0].ActualTag != model.BlockOutboundTag {
		t.Fatalf("block 规则应 verified 且实际 tag 为 %s: %+v", model.BlockOutboundTag, u)
	}

	// 2. 改成走 freedom 节点但不下发：核心仍是旧规则，必须 pending 而不是 mismatch。
	node, err := (&OutboundNodeService{}).AddFromJSON(`{"protocol":"freedom","settings":{}}`, "verify-freedom")
	if err != nil {
		t.Fatalf("AddFromJSON: %v", err)
	}
	if !strings.HasPrefix(node.Tag, model.OutboundTagPrefix+"-") {
		t.Fatalf("节点 tag 应以 a-ui- 开头: %q", node.Tag)
	}
	rule.Action = model.ActionProxy
	rule.OutboundId = node.Id
	if err := rs.Update(rule); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	res = verify(rule.Id)
	if res.Core.State != coreStatePending {
		t.Fatalf("未下发时 Core 应为 pending: %+v", res.Core)
	}
	if u := onlyUser(res); u.Verdict != verdictPending {
		t.Fatalf("未下发时 A 应为 pending: %+v", u)
	}

	// 3. 下发（热应用）之后：A 访问 localhost 应走节点 tag。
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	res = verify(rule.Id)
	if res.Core.State != coreStateOK || res.Expected.Tag != node.Tag {
		t.Fatalf("下发后 Core=%+v Expected=%+v", res.Core, res.Expected)
	}
	u = onlyUser(res)
	if u.Verdict != verdictVerified || u.Samples[0].ActualTag != node.Tag {
		t.Fatalf("下发后 A 应 verified 且实际 tag 为 %s: %+v", node.Tag, u)
	}

	// 4. 只有 keyword: 的规则推不出样例：即使已下发也只能 unverifiable。
	kwGroup := newTestGroupWithDomains(t, "verify-keyword", []string{"keyword:zzz"})
	kwRule := &model.RoutingRule{
		Remark: "verify-e2e-keyword", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{kwGroup.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := rs.Add(kwRule); err != nil {
		t.Fatalf("add keyword rule: %v", err)
	}
	// 先下发，否则核心处于 pending，用户行会是 pending 而不是要验证的 unverifiable。
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	res = verify(kwRule.Id)
	if res.Core.State != coreStateOK {
		t.Fatalf("keyword 规则下发后 Core = %+v", res.Core)
	}
	if u := onlyUser(res); u.Verdict != verdictUnverifiable || len(u.Samples) != 0 {
		t.Fatalf("keyword 规则 A 应 unverifiable 且无样例: %+v", u)
	}

	// 5. ErrNoClue：新加一条 direct 规则但不下发，核心里没有任何规则命中它的样例。
	// 真实核心经 gRPC 返回的 ErrNoClue 必须被认成 noMatch（不是 error）：标签是
	// 「未命中」，HitByRule=false；direct 的期望就是默认出站，所以 Match=true。
	// 核心此时是 pending，用户行仍是 pending——未下发的规则不能变绿。
	ncGroup := newTestGroupWithDomains(t, "verify-noclue", []string{"full:verify-noclue.example"})
	ncRule := &model.RoutingRule{
		Remark: "verify-e2e-noclue", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{ncGroup.Id}),
		Action:         model.ActionDirect, Enable: true,
	}
	if err := rs.Add(ncRule); err != nil {
		t.Fatalf("add noclue rule: %v", err)
	}
	res = verify(ncRule.Id)
	if res.Core.State != coreStatePending {
		t.Fatalf("未下发的 direct 规则 Core 应为 pending: %+v", res.Core)
	}
	u = onlyUser(res)
	if len(u.Samples) != 1 {
		t.Fatalf("noclue 规则应有 1 个样例: %+v", u)
	}
	s0 := u.Samples[0]
	if s0.Error != "" || s0.ActualLabel != labelNoMatch || s0.HitByRule || !s0.Match {
		t.Fatalf("核心未命中任何规则应是 noMatch（标签=%q、HitByRule=false、direct 下 Match=true）: %+v", labelNoMatch, s0)
	}
	if u.Verdict != verdictPending {
		t.Fatalf("核心 pending 时用户行应为 pending: %+v", u)
	}

	// 6. 下发后跑一轮全量：共用一个真实控制面连接，三条启用规则都有结果且与单条口径一致。
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	all, gen, err := vs.VerifyAll()
	if err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	if gen != XrayApplyGeneration() || gen == 0 {
		t.Fatalf("VerifyAll 代次 = %d，当前代次 %d", gen, XrayApplyGeneration())
	}
	if len(all) != 3 {
		t.Fatalf("应有三条启用规则的结果: %+v", all)
	}
	want := map[int]string{rule.Id: verdictVerified, kwRule.Id: verdictUnverifiable, ncRule.Id: verdictVerified}
	for id, verdict := range want {
		res := all[id]
		if res == nil || res.Core.State != coreStateOK || res.ApplyGen != gen {
			t.Fatalf("规则 %d 全量结果 = %+v", id, res)
		}
		if u := onlyUser(res); u.Verdict != verdict {
			t.Fatalf("规则 %d 全量结论应为 %s: %+v", id, verdict, u)
		}
	}
	if s0 := onlyUser(all[rule.Id]).Samples[0]; s0.ActualTag != node.Tag {
		t.Fatalf("全量验证里 A 访问 localhost 应走 %s: %+v", node.Tag, s0)
	}
}

// 用真实 xray 走一遍后台审计：RunAudit 写缓存，Summaries 给出 verified 且代次与当前一致；
// 热应用改规则后代次 +1，再审计一轮仍是 verified（旧代次的结论不会被沿用）。
func TestRouteAuditAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	requireXrayRoutingService(t)
	requireXrayAPIPortFree(t)
	setupDB(t)
	resetAuditCache(t)

	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})

	port := freePort(t)
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.Protocol("socks"), Enable: true, Remark: "audit-e2e",
		Tag:            "inbound-" + strconv.Itoa(port),
		Listen:         "127.0.0.1",
		Settings:       `{"auth":"noauth","udp":false}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	group := newTestGroupWithDomains(t, "audit-localhost", []string{"full:localhost"})
	rs := &RoutingRuleService{}
	rule := &model.RoutingRule{
		Remark: "audit-e2e", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := rs.Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, in.Port)

	svc := &RoutingAuditService{}
	summaryOf := func() RuleAuditSummary {
		t.Helper()
		gen, rules := svc.Summaries()
		sum, ok := rules[rule.Id]
		if !ok {
			t.Fatalf("Summaries 缺少规则 %d: %+v", rule.Id, rules)
		}
		if gen != XrayApplyGeneration() {
			t.Fatalf("Summaries 代次 %d ≠ 当前 %d", gen, XrayApplyGeneration())
		}
		return sum
	}

	gen1, err := svc.RunAudit()
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}
	sum := summaryOf()
	if sum.Verdict != verdictVerified || sum.Stale || sum.ApplyGen != XrayApplyGeneration() || sum.ApplyGen != gen1 {
		t.Fatalf("block 规则审计后应为 verified 且代次一致（gen=%d）: %+v", gen1, sum)
	}

	node, err := (&OutboundNodeService{}).AddFromJSON(`{"protocol":"freedom","settings":{}}`, "audit-freedom")
	if err != nil {
		t.Fatalf("AddFromJSON: %v", err)
	}
	rule.Action = model.ActionProxy
	rule.OutboundId = node.Id
	if err := rs.Update(rule); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	if XrayApplyGeneration() != gen1+1 {
		t.Fatalf("热应用后代次应为 %d，实际 %d", gen1+1, XrayApplyGeneration())
	}
	gen2, err := svc.RunAudit()
	if err != nil {
		t.Fatalf("RunAudit(second): %v", err)
	}
	sum = summaryOf()
	if gen2 != gen1+1 || sum.Verdict != verdictVerified || sum.Stale || sum.ApplyGen != gen2 {
		t.Fatalf("改规则热应用后应仍 verified 且代次 +1（gen1=%d gen2=%d）: %+v", gen1, gen2, sum)
	}
}
