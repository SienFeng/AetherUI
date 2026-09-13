package service

import (
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// newDetachFixture 建一个域名组，供本文件的用例复用。
func newDetachRule(t *testing.T, remark string, g *model.DomainGroup, inboundIds string) *model.RoutingRule {
	t.Helper()
	rule := &model.RoutingRule{
		Remark:         remark,
		InboundIds:     inboundIds,
		DomainGroupId:  g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock,
		Enable:         true,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("Add rule %q: %v", remark, err)
	}
	return rule
}

// assertNoRuleReferences 钉住本功能的核心不变量：删完之后，库里不能再有任何
// 一条规则指向这个 id。剩一条就够出事——SQLite 复用 id 后它会绑到下一个新建
// 的入站上，那时引用不再悬空，生成期那道跳过防线也拦不住。
func assertNoRuleReferences(t *testing.T, inboundId int) {
	t.Helper()
	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, r := range rules {
		ids, decodeErr := DecodeInboundIds(r.InboundIds)
		if decodeErr != nil {
			t.Fatalf("rule #%d: DecodeInboundIds: %v", r.Id, decodeErr)
		}
		for _, id := range ids {
			if id == inboundId {
				t.Errorf("rule #%d still references inbound #%d (InboundIds = %q)",
					r.Id, inboundId, r.InboundIds)
			}
		}
	}
}

// 一条还覆盖着别人的规则，只摘掉被删的那个 id，其余原样保留。
func TestDelInboundDetachesIdFromMultiInboundRule(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	c := newTestInbound(t, 10003)
	rule := newDetachRule(t, "三人组", g, mustEncodeIds(t, []int{a.Id, b.Id, c.Id}))

	if err := (&InboundService{}).DelInbound(b.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}

	got, err := (&RoutingRuleService{}).Get(rule.Id)
	if err != nil {
		t.Fatalf("rule must survive: %v", err)
	}
	want := mustEncodeIds(t, []int{a.Id, c.Id})
	if got.InboundIds != want {
		t.Errorf("InboundIds = %q, want %q", got.InboundIds, want)
	}
	assertNoRuleReferences(t, b.Id)
}

// 只覆盖这一个入站的规则必须整条删除。
//
// 绝不能只摘掉 id 留下 InboundIds = "[]"：空数组的语义是「对所有入站生效」，
// 一条本来只管一个人的规则会当场放大到全体，而 xray 会返回 Configuration OK、
// 面板显示 running，没有任何一层会报错。
func TestDelInboundDeletesRuleThatCoveredOnlyThatInbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	only := newTestInbound(t, 10001)
	rule := newDetachRule(t, "测试节点", g, mustEncodeIds(t, []int{only.Id}))

	if err := (&InboundService{}).DelInbound(only.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}

	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, r := range rules {
		if r.Id == rule.Id {
			t.Fatalf("rule survived with InboundIds = %q; an empty array means ALL inbounds", r.InboundIds)
		}
	}
	assertNoRuleReferences(t, only.Id)
}

// 「所有用户」规则不指向任何具体入站，删谁都不该动它一个字节。
// 否则一旦建了全局封禁规则，删掉任何一个入站都会把它误伤掉。
func TestDelInboundLeavesAllUsersRuleUntouched(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	in := newTestInbound(t, 10001)
	rule := newDetachRule(t, "全局封禁", g, "[]")

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}

	got, err := (&RoutingRuleService{}).Get(rule.Id)
	if err != nil {
		t.Fatalf("all-users rule must survive: %v", err)
	}
	if got.InboundIds != "[]" {
		t.Errorf("InboundIds = %q, want %q", got.InboundIds, "[]")
	}
}

// 规则数据损坏时无从判断它引用了谁，必须拒绝删除并保住入站。
// 放行的话，SQLite 复用 id 后这条规则会静默绑到下一个新建的入站上。
func TestDelInboundRefusesWhenRuleDataIsCorrupt(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	rule := newDetachRule(t, "损坏的规则", g, mustEncodeIds(t, []int{in.Id}))
	// 写入路径不会产生这种值，但导入的文件、并发写入、手工改库都可能留下。
	if err := database.GetDB().Model(model.RoutingRule{}).
		Where("id = ?", rule.Id).Update("inbound_ids", "{not json").Error; err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	if err := (&InboundService{}).DelInbound(in.Id); err == nil {
		t.Fatal("expected refusal: rule data is corrupt, references cannot be determined")
	}

	if _, err := (&InboundService{}).GetInbound(in.Id); err != nil {
		t.Errorf("inbound must survive a refused delete: %v", err)
	}
}

// 没有任何规则引用它时，删除照常进行——这是最常见的一条路径。
func TestDelInboundWithoutAnyRuleStillWorks(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}

	if _, err := (&InboundService{}).GetInbound(in.Id); err == nil {
		t.Error("inbound should be gone")
	}
}

// 预检把两类去向分开报给管理员：哪条会被整条删除、哪条只是少掉这个人。
// 它与真正执行共用同一次判定，确认框说的就是实际会做的。
func TestPlanInboundDetachSeparatesRemovedFromDetached(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	sole := newDetachRule(t, "只管甲", g, mustEncodeIds(t, []int{a.Id}))
	shared := newDetachRule(t, "甲乙同组", newTestGroup(t, "Claude"), mustEncodeIds(t, []int{a.Id, b.Id}))

	plan, err := (&RoutingRuleService{}).PlanInboundDetach(a.Id)
	if err != nil {
		t.Fatalf("PlanInboundDetach: %v", err)
	}

	if len(plan.Removed) != 1 || plan.Removed[0].Id != sole.Id {
		t.Fatalf("Removed = %+v, want only rule #%d", plan.Removed, sole.Id)
	}
	if len(plan.Detached) != 1 || plan.Detached[0].Id != shared.Id {
		t.Fatalf("Detached = %+v, want only rule #%d", plan.Detached, shared.Id)
	}
	if plan.Removed[0].Label != "只管甲" {
		t.Errorf("Removed[0].Label = %q, want %q", plan.Removed[0].Label, "只管甲")
	}
}
