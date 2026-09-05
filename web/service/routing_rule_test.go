package service

import (
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
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
	err := s.Add(&model.RoutingRule{DomainGroupId: 999, DomainGroupIds: mustEncodeGroupIds(t, []int{999}), Action: model.ActionBlock, Enable: true})
	if err == nil {
		t.Error("expected error when domain group does not exist")
	}
}

func TestAddRuleRejectsProxyWithoutOutbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionProxy, OutboundId: 0, Enable: true})
	if err == nil {
		t.Error("expected error when proxy rule has no outbound")
	}
}

func TestAddRuleRejectsUnknownAction(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: "drop", Enable: true})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestAddBlockRuleWithGlobalInbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	s := RoutingRuleService{}
	r := &model.RoutingRule{Remark: "全局封禁", InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true}
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
	if err := s.Add(&model.RoutingRule{DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true}); err != nil {
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
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckOutboundRefs(node.Id); err == nil {
		t.Error("expected error: outbound is referenced by a rule")
	}
}

func TestGetEnabledRulesSortedByPriorityThenId(t *testing.T) {
	setupDB(t)
	s := RoutingRuleService{}
	// 故意乱序插入。三条规则挂三个不同的域名组：同一个域名组下每个入站至多
	// 被一条规则覆盖，而本测试关心的是排序，不该被冲突校验挡住。
	for i, p := range []int{20, 10, 10} {
		g := newTestGroup(t, "组 "+strconv.Itoa(i))
		if err := s.Add(&model.RoutingRule{
			DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Priority: p, Enable: true,
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
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
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
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
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
		Remark: "甲的 ChatGPT", InboundIds: mustEncodeIds(t, []int{in.Id}), DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
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

// 空的 InboundIds（「所有用户」）不指向任何具体入站，不该阻塞任何入站的删除。
func TestDelInboundAllowedWhenOnlyGlobalRuleExists(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10002)
	g := newTestGroup(t, "违规域名")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "全员封禁", InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("a global rule must not block deleting an unrelated inbound: %v", err)
	}
}

func TestEncodeInboundIdsSortsAndDedupes(t *testing.T) {
	got, err := EncodeInboundIds([]int{5, 3, 5, 1})
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	if got != "[1,3,5]" {
		t.Errorf("got %q, want [1,3,5]", got)
	}
}

// 非正数会被丢弃，于是 [0] 编码后是 []——而 [] 的语义是「所有用户」。
// 严格版必须报错，绝不能让一条本该覆盖某个人的规则被静默放大到全体。
func TestEncodeInboundIdsStrictRejectsAllInvalid(t *testing.T) {
	if _, err := EncodeInboundIdsStrict([]int{0, -1}); err == nil {
		t.Error("expected error: non-empty input with no valid id must not become []")
	}
	// 空输入是前端显式选了「所有用户」，必须放行
	got, err := EncodeInboundIdsStrict(nil)
	if err != nil {
		t.Fatalf("empty input must be accepted: %v", err)
	}
	if got != "[]" {
		t.Errorf("got %q, want []", got)
	}
}

func TestDecodeInboundIdsTreatsBlankAsAllUsers(t *testing.T) {
	for _, raw := range []string{"", "   ", "null"} {
		got, err := DecodeInboundIds(raw)
		if err != nil {
			t.Fatalf("DecodeInboundIds(%q): %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("DecodeInboundIds(%q) = %v, want empty", raw, got)
		}
	}
}

// 真正的语法错误必须返回 error，由 buildRule 整条丢弃该规则——
// 当成空数组就等于把规则放大到所有用户。
func TestDecodeInboundIdsRejectsCorruptData(t *testing.T) {
	if _, err := DecodeInboundIds("{not json"); err == nil {
		t.Error("expected error for corrupt data")
	}
}

// 引用守卫必须能看穿多入站规则：SQLite 会复用被删除的自增 id，
// 一条覆盖 [甲, 乙] 的规则在甲被删掉后，会静默重绑到捡到甲旧 id 的新入站上。
func TestCheckInboundRefsSeesIdInTheMiddleOfAMultiInboundRule(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	c := newTestInbound(t, 10003)
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		InboundIds:    mustEncodeIds(t, []int{a.Id, b.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckInboundRefs(b.Id); err == nil {
		t.Error("expected error: inbound b is referenced by a multi-inbound rule")
	}
	if err := s.CheckInboundRefs(c.Id); err != nil {
		t.Errorf("unreferenced inbound should be deletable, got %v", err)
	}
}

// 「所有用户」规则不指向任何具体入站，不算引用——与旧语义 InboundId = 0 一致。
// 否则一旦建了全局封禁规则，所有入站都删不掉了。
func TestCheckInboundRefsIgnoresAllUsersRule(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	in := newTestInbound(t, 10001)
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckInboundRefs(in.Id); err != nil {
		t.Errorf("an all-users rule must not pin any specific inbound, got %v", err)
	}
}

// newConflictFixture 建一个域名组和三个入站，供冲突测试复用。
func newConflictFixture(t *testing.T) (*model.DomainGroup, *model.Inbound, *model.Inbound, *model.Inbound) {
	t.Helper()
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	return g, newTestInbound(t, 10001), newTestInbound(t, 10002), newTestInbound(t, 10003)
}

func addRuleWith(t *testing.T, groupId int, ids []int, remark string) *model.RoutingRule {
	t.Helper()
	r := &model.RoutingRule{
		Remark: remark, InboundIds: mustEncodeIds(t, ids),
		DomainGroupId: groupId, DomainGroupIds: mustEncodeGroupIds(t, []int{groupId}), Action: model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add %s: %v", remark, err)
	}
	return r
}

func TestConflictRejectsOverlappingInbounds(t *testing.T) {
	g, a, b, c := newConflictFixture(t)
	addRuleWith(t, g.Id, []int{a.Id, b.Id}, "甲乙走 B")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "乙丙走 C", InboundIds: mustEncodeIds(t, []int{b.Id, c.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: inbound b is already covered in this domain group")
	}
}

func TestConflictAllowsDisjointInbounds(t *testing.T) {
	g, a, b, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, []int{a.Id}, "甲")

	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "乙", InboundIds: mustEncodeIds(t, []int{b.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("disjoint inbounds must be accepted: %v", err)
	}
}

// 严格互斥：一个域名组一旦有了「所有用户」规则，就不能再对它加任何规则。
func TestConflictAllUsersBlocksSpecificUser(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, nil, "所有用户")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: an all-users rule already covers this domain group")
	}
}

// 反方向同样要挡：已有指定用户的规则时，「所有用户」也勾不上。
func TestConflictSpecificUserBlocksAllUsers(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, []int{a.Id}, "甲")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "所有用户", InboundIds: "[]",
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: a specific-user rule already exists in this domain group")
	}
}

func TestConflictAllUsersBlocksAnotherAllUsers(t *testing.T) {
	g, _, _, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, nil, "所有用户")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "所有用户 2", InboundIds: "[]",
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: two all-users rules in the same domain group")
	}
}

// 不同域名组永不冲突，即使域名内容重叠——那种重叠由 Priority 决定先后，
// 是既有语义，本功能不动它。
func TestConflictIgnoresOtherDomainGroups(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	other := newTestGroup(t, "另一个组")
	addRuleWith(t, g.Id, []int{a.Id}, "甲在 ChatGPT 组")

	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲在另一个组", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: other.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{other.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("different domain groups must never conflict: %v", err)
	}
}

// 禁用的规则同样占位，否则会出现「保存时没问题、一启用才发现撞车」。
func TestConflictCountsDisabledRules(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	r := addRuleWith(t, g.Id, []int{a.Id}, "甲（将被禁用）")
	r.Enable = false
	if err := (&RoutingRuleService{}).Update(r); err != nil {
		t.Fatalf("Update: %v", err)
	}

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲走别处", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: a disabled rule still holds its slot")
	}
}

// 编辑自己不能算和自己冲突，否则任何一条规则都改不动了。
func TestConflictExcludesTheRuleBeingUpdated(t *testing.T) {
	g, a, b, _ := newConflictFixture(t)
	r := addRuleWith(t, g.Id, []int{a.Id}, "甲")

	r.InboundIds = mustEncodeIds(t, []int{a.Id, b.Id})
	if err := (&RoutingRuleService{}).Update(r); err != nil {
		t.Fatalf("updating a rule must not conflict with itself: %v", err)
	}
}

// 冲突报错必须点名到人和规则。只说「存在冲突」等于让管理员自己去翻规则表，
// 而规则一多就根本找不出是哪一条挡住了。
func TestConflictErrorNamesTheUserAndTheRule(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	a.Remark = "甲"
	if err := database.GetDB().Save(a).Error; err != nil {
		t.Fatalf("save inbound remark: %v", err)
	}
	addRuleWith(t, g.Id, []int{a.Id}, "甲的 ChatGPT 走 B")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲的 ChatGPT 走 C", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected a conflict")
	}
	msg := err.Error()
	for _, want := range []string{"甲的 ChatGPT 走 B", "用户「甲」", "ChatGPT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict error must mention %q, got: %s", want, msg)
		}
	}
	// NewError 走 fmt.Sprintln，会在参数之间插空格，拼出「「 甲 」」这种带
	// 空隙的句子。这条断言把消息钉在 NewErrorf 的一次成型上。
	if strings.Contains(msg, "「 ") || strings.Contains(msg, " 」") {
		t.Errorf("conflict error has stray spaces inside the quotes: %s", msg)
	}
}

// mustEncodeGroupIds 是测试夹具共用的域名组编码器。用非 Strict 版本，
// 因为部分用例要故意造出「空集合」这种非法状态来验证下游会拒绝它。
func mustEncodeGroupIds(t *testing.T, ids []int) string {
	t.Helper()
	encoded, err := EncodeDomainGroupIds(ids)
	if err != nil {
		t.Fatalf("EncodeDomainGroupIds: %v", err)
	}
	return encoded
}

func TestEncodeDomainGroupIdsSortsAndDedupes(t *testing.T) {
	got, err := EncodeDomainGroupIds([]int{3, 1, 3, 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "[1,2,3]" {
		t.Errorf("got %q, want [1,2,3]", got)
	}
}

// 与 EncodeInboundIdsStrict 的关键分歧：入站那边「原始列表本来就空」是
// 合法的（= 所有用户），域名组这边必须报错——空的 domain 条件会让规则
// 劫持该用户的全部流量。
func TestEncodeDomainGroupIdsStrictRejectsEmptyInput(t *testing.T) {
	if _, err := EncodeDomainGroupIdsStrict(nil); err == nil {
		t.Error("empty input must be rejected: [] would make the domain condition empty")
	}
	if _, err := EncodeDomainGroupIdsStrict([]int{}); err == nil {
		t.Error("empty slice must be rejected")
	}
}

func TestEncodeDomainGroupIdsStrictRejectsAllInvalid(t *testing.T) {
	if _, err := EncodeDomainGroupIdsStrict([]int{0, -1}); err == nil {
		t.Error("all-invalid input must be rejected, not silently collapse to []")
	}
}

func TestEncodeDomainGroupIdsStrictAcceptsValid(t *testing.T) {
	got, err := EncodeDomainGroupIdsStrict([]int{2, 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "[1,2]" {
		t.Errorf("got %q, want [1,2]", got)
	}
}

// 空串/null 不报错：迁移回填前、直接改库、并发写入都可能留下空值，
// 在解码这一层报错会让整份配置生成失败。交给 validate / buildRule
// 各自按空集合处理（都会拒绝）。
func TestDecodeDomainGroupIdsTreatsBlankAsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "null"} {
		got, err := DecodeDomainGroupIds(raw)
		if err != nil {
			t.Errorf("DecodeDomainGroupIds(%q) returned error: %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("DecodeDomainGroupIds(%q) = %v, want empty", raw, got)
		}
	}
}

func TestDecodeDomainGroupIdsRejectsCorruptData(t *testing.T) {
	if _, err := DecodeDomainGroupIds("{oops"); err == nil {
		t.Error("corrupt JSON must return an error so the rule is dropped whole")
	}
}

// intersectGroups 绝不能复用 intersectInbounds：后者把空切片当全集，
// 而域名组的空集合是非法值。复用会让两条各自损坏的规则被判成互相冲突，
// 把管理员锁在门外——既修不了旧规则，也建不了新规则。
func TestIntersectGroupsTreatsEmptyAsEmptyNotUniversal(t *testing.T) {
	if ok, _ := intersectGroups(nil, []int{1}); ok {
		t.Error("empty set must not intersect anything")
	}
	if ok, _ := intersectGroups([]int{1}, nil); ok {
		t.Error("empty set must not intersect anything")
	}
	if ok, _ := intersectGroups(nil, nil); ok {
		t.Error("two empty sets must not intersect")
	}
}

func TestIntersectGroupsReportsSmallestSharedId(t *testing.T) {
	ok, who := intersectGroups([]int{2, 5, 9}, []int{5, 9})
	if !ok {
		t.Fatal("expected intersection")
	}
	// b 已升序，取到的是最小的相交 id，保证错误信息稳定可测。
	if who != 5 {
		t.Errorf("who = %d, want 5", who)
	}
}

func TestIntersectGroupsDisjoint(t *testing.T) {
	if ok, _ := intersectGroups([]int{1, 2}, []int{3, 4}); ok {
		t.Error("disjoint sets must not intersect")
	}
}

// addMultiGroupRule 建一条引用多个域名组的规则。
func addMultiGroupRule(t *testing.T, groupIds []int, inboundIds []int, remark string) error {
	t.Helper()
	return (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark:         remark,
		InboundIds:     mustEncodeIds(t, inboundIds),
		DomainGroupIds: mustEncodeGroupIds(t, groupIds),
		Action:         model.ActionBlock,
		Enable:         true,
	})
}

func TestAddRuleRejectsEmptyDomainGroups(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "没选域名组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: "[]", Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("空域名组集合必须被拒绝：domain 条件为空会劫持该用户的全部流量")
	}
}

func TestAddRuleRejectsAnyMissingDomainGroup(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	err := addMultiGroupRule(t, []int{g.Id, 999}, []int{in.Id}, "一个存在一个不存在")
	if err == nil {
		t.Fatal("引用了不存在的域名组必须被拒绝")
	}
}

func TestAddRuleAcceptsMultipleDomainGroups(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id, chatgpt.Id}, []int{in.Id}, "两组"); err != nil {
		t.Fatalf("多域名组规则必须被接受: %v", err)
	}
}

// spec §2.3 的表格逐行落成用例。冲突判定的单位是「域名组 × 用户」的组合。
func TestConflictGroupSetsPartiallyOverlapSameUser(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id, chatgpt.Id}, []int{in.Id}, "两组走 A"); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := addMultiGroupRule(t, []int{chatgpt.Id}, []int{in.Id}, "ChatGPT 走 B")
	if err == nil {
		t.Fatal("组集合在 ChatGPT 上相交且用户相同，必须拒绝")
	}
	if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误信息必须含「冲突」二字（importRules 靠它归类）: %v", err)
	}
	if !strings.Contains(err.Error(), "ChatGPT") {
		t.Errorf("错误信息必须点名相交的那个域名组: %v", err)
	}
}

func TestConflictAllowsDisjointGroupSetsSameUser(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id}, []int{in.Id}, "Claude 走 A"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := addMultiGroupRule(t, []int{chatgpt.Id}, []int{in.Id}, "ChatGPT 走 B"); err != nil {
		t.Fatalf("组集合不相交必须被接受: %v", err)
	}
}

func TestConflictAllowsSameGroupSetDifferentUsers(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	if err := addMultiGroupRule(t, []int{claude.Id}, []int{a.Id}, "甲"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := addMultiGroupRule(t, []int{claude.Id}, []int{b.Id}, "乙"); err != nil {
		t.Fatalf("同组不同人必须被接受: %v", err)
	}
}

// 引用守卫：解码失败时必须拦住删除。SQLite 复用自增 id，孤儿规则会静默
// 绑到新建的域名组上，那时引用不再悬空，生成期的跳过防线也拦不住。
func TestCheckDomainGroupRefsSeesIdInTheMiddleOfAMultiGroupRule(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	banned := newTestGroup(t, "违规")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id, chatgpt.Id, banned.Id}, []int{in.Id}, "三组"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := (&RoutingRuleService{}).CheckDomainGroupRefs(chatgpt.Id); err == nil {
		t.Error("被引用的域名组（位于数组中间）必须拦住删除")
	}
}

func TestCheckDomainGroupRefsBlocksDeletionOnCorruptData(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{g.Id}, []int{in.Id}, "好规则"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 绕过 service 直接写坏数据，模拟直接改库 / 并发写入留下的残骸。
	err := database.GetDB().Exec(
		"UPDATE routing_rules SET domain_group_ids = '{oops' WHERE remark = ?", "好规则").Error
	if err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if err := (&RoutingRuleService{}).CheckDomainGroupRefs(g.Id); err == nil {
		t.Error("解码失败时必须拦住删除，不能放行")
	}
}

// Update 必须让 DomainGroupId 跟上 DomainGroupIds 的变化。buildRule
// （routing_inject.go）、listRules（controller/routing.go）、toPortableRule
// （routing_portable.go）三个消费者眼下仍只读 DomainGroupId（过渡桥接，见
// routing_rule.go 的 Update）；编辑一条规则的域名组若不同步这个字段，生成
// 的 xray 配置、规则列表、导出文件会静默停在改动前的域名组上，且不报任何
// 错——这条路径此前零测试覆盖，正是这个缺陷能溜过去的原因。
func TestUpdateSyncsLegacyDomainGroupIdToFirstGroup(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	r := &model.RoutingRule{
		Remark: "改域名组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id}), Action: model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	r.DomainGroupIds = mustEncodeGroupIds(t, []int{chatgpt.Id})
	if err := (&RoutingRuleService{}).Update(r); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := (&RoutingRuleService{}).Get(r.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	groupIds, err := DecodeDomainGroupIds(got.DomainGroupIds)
	if err != nil {
		t.Fatalf("DecodeDomainGroupIds: %v", err)
	}
	if len(groupIds) == 0 || groupIds[0] != chatgpt.Id {
		t.Fatalf("DomainGroupIds = %v, want first id %d", groupIds, chatgpt.Id)
	}
	if got.DomainGroupId != groupIds[0] {
		t.Errorf("DomainGroupId = %d, want %d（等于 DomainGroupIds 的首个 id）——"+
			"过渡桥接的三个消费者会静默用错域名组", got.DomainGroupId, groupIds[0])
	}
}
