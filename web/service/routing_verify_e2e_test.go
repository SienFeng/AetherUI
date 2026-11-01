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
}
