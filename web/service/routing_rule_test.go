package service

import (
	"testing"

	"a-ui/database/model"
)

func newTestGroup(t *testing.T, remark string) *model.DomainGroup {
	t.Helper()
	encoded, err := EncodeDomains([]string{"geosite:openai"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: remark, Domains: encoded}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("Add group: %v", err)
	}
	return g
}

func TestAddRuleRejectsMissingDomainGroup(t *testing.T) {
	setupDB(t)
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: 999, Action: model.ActionBlock, Enable: true})
	if err == nil {
		t.Error("expected error when domain group does not exist")
	}
}

func TestAddRuleRejectsProxyWithoutOutbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: 0, Enable: true})
	if err == nil {
		t.Error("expected error when proxy rule has no outbound")
	}
}

func TestAddRuleRejectsUnknownAction(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, Action: "drop", Enable: true})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestAddBlockRuleWithGlobalInbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	s := RoutingRuleService{}
	r := &model.RoutingRule{Remark: "全局封禁", InboundId: 0, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true}
	if err := s.Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if r.Id == 0 {
		t.Fatal("Add did not assign an Id")
	}
}

func TestCheckDomainGroupRefsBlocksDeletion(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckDomainGroupRefs(g.Id); err == nil {
		t.Error("expected error: domain group is referenced by a rule")
	}
	if err := s.CheckDomainGroupRefs(g.Id + 1); err != nil {
		t.Errorf("unreferenced group should be deletable, got %v", err)
	}
}

func TestCheckOutboundRefsBlocksDeletion(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckOutboundRefs(node.Id); err == nil {
		t.Error("expected error: outbound is referenced by a rule")
	}
}

func TestGetEnabledRulesSortedByPriorityThenId(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	// 故意乱序插入
	for _, p := range []int{20, 10, 10} {
		if err := s.Add(&model.RoutingRule{
			DomainGroupId: g.Id, Action: model.ActionBlock, Priority: p, Enable: true,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	rules, err := s.GetEnabled()
	if err != nil {
		t.Fatalf("GetEnabled: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("len = %d, want 3", len(rules))
	}
	if rules[0].Priority != 10 || rules[1].Priority != 10 || rules[2].Priority != 20 {
		t.Fatalf("priorities = %d,%d,%d; want 10,10,20",
			rules[0].Priority, rules[1].Priority, rules[2].Priority)
	}
	if rules[0].Id > rules[1].Id {
		t.Error("rules with equal priority must be ordered by Id ascending")
	}
}

func TestDelDomainGroupRejectsWhenReferenced(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	rs := RoutingRuleService{}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	// 走真实删除入口，而不是内部的 CheckDomainGroupRefs。
	// 域名组一旦被删，引用它的规则 domain 会变成空数组，而 xray 把缺失的
	// 匹配条件当作「不限制」，规则会退化成劫持该入站的全部流量且不报错。
	if err := (&DomainGroupService{}).Del(g.Id); err == nil {
		t.Fatal("Del succeeded on a referenced domain group; it must be refused")
	}
	if _, err := (&DomainGroupService{}).Get(g.Id); err != nil {
		t.Fatalf("domain group was deleted despite the refusal: %v", err)
	}

	// 移除引用后必须能正常删除，否则本测试可能只是因为 Del 恒失败而通过。
	rules, err := rs.GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rule count = %d, want 1", len(rules))
	}
	if err := rs.Del(rules[0].Id); err != nil {
		t.Fatalf("Del rule: %v", err)
	}
	if err := (&DomainGroupService{}).Del(g.Id); err != nil {
		t.Errorf("Del failed even after the referencing rule was removed: %v", err)
	}
}

func TestDelOutboundNodeRejectsWhenReferenced(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	rs := RoutingRuleService{}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	// 出站一旦被删，规则的 outboundTag 会悬空，而 xray 对悬空 outboundTag
	// 不报错，运行时静默回落到默认出站（直连）。
	if err := (&OutboundNodeService{}).Del(node.Id); err == nil {
		t.Fatal("Del succeeded on a referenced outbound; it must be refused")
	}
	if _, err := (&OutboundNodeService{}).Get(node.Id); err != nil {
		t.Fatalf("outbound node was deleted despite the refusal: %v", err)
	}

	// 同样地，移除引用后必须能正常删除。
	rules, err := rs.GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rule count = %d, want 1", len(rules))
	}
	if err := rs.Del(rules[0].Id); err != nil {
		t.Fatalf("Del rule: %v", err)
	}
	if err := (&OutboundNodeService{}).Del(node.Id); err != nil {
		t.Errorf("Del failed even after the referencing rule was removed: %v", err)
	}
}

// SQLite 的自增主键 id 会被复用（GORM 的 sqlite 驱动生成的是 rowid 别名而非
// AUTOINCREMENT）。删掉用户甲的入站后，新建用户丙的入站可能拿到同一个 id，
// 「甲的 ChatGPT 走 B 节点」这条孤儿规则会静默重绑到丙身上，规则列表还会渲染得
// 很合理。生成期跳过那道防线拦不住 —— 引用不再悬空，只是指错了人。
// 三条引用边必须对称：域名组、出站已有守卫，入站也要有。
func TestDelInboundRejectedWhileReferencedByRule(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	g := newTestGroup(t, "ChatGPT")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲的 ChatGPT", InboundId: in.Id, DomainGroupId: g.Id,
		Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	if err := (&InboundService{}).DelInbound(in.Id); err == nil {
		t.Fatal("DelInbound removed an inbound that a routing rule still references")
	}

	// 被拒之后入站必须还在，否则守卫只是「报错但照删」
	if _, err := (&InboundService{}).GetInbound(in.Id); err != nil {
		t.Errorf("inbound was deleted despite the rejection: %v", err)
	}
}

// InboundId = 0 是全局规则，不指向任何具体入站，不该阻塞任何入站的删除。
func TestDelInboundAllowedWhenOnlyGlobalRuleExists(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10002)
	g := newTestGroup(t, "违规域名")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "全员封禁", InboundId: 0, DomainGroupId: g.Id,
		Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("a global rule must not block deleting an unrelated inbound: %v", err)
	}
}
