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

// Update 必须让持久化的 DomainGroupIds 跟上传入的新值——这是分流规则编辑
// 域名组最基本的路径，回归测试防止 Update 漏赋值导致改动静默不生效。
func TestUpdatePersistsDomainGroupIds(t *testing.T) {
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
	if len(groupIds) != 1 || groupIds[0] != chatgpt.Id {
		t.Fatalf("DomainGroupIds = %v, want [%d]", groupIds, chatgpt.Id)
	}
}

// 回退契约的金丝雀：Add 只写 DomainGroupIds，落库的 DomainGroupId 必须是 0。
//
// 这条断言钉住的是「回退到旧版本二进制后会发生什么」。旧代码只读
// domain_group_id，所以本次改造后新建的规则——不论单组还是多组——回退后
// 都会被旧代码整条丢弃（范围缩小，安全侧正确）。若哪天有人「顺手」在 Add
// 里把它补上，回退行为就会从「整条丢弃」变成「按一个可能已经不在规则里的
// 组分流」，而 xray 返回 Configuration OK、面板首页显示 running，
// 没有任何一层会报错。
func TestAddLeavesLegacyDomainGroupIdZero(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	r := &model.RoutingRule{
		Remark: "单组新规则", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := (&RoutingRuleService{}).Get(r.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DomainGroupId != 0 {
		t.Errorf("DomainGroupId = %d, want 0（Add 不写旧字段，回退契约据此成立）", got.DomainGroupId)
	}
}

// Update 必须把 DomainGroupId 显式置 0，不能让它停在编辑前的旧值。
//
// 反例（改动前的行为）：一条升级前就存在的老规则，domain_group_id 被迁移
// 保留成 3；管理员在面板里把它改成组 9，domain_group_ids 变成 [9] 而
// domain_group_id 停在 3。回退后旧代码按【管理员已经从规则里删掉的】组 3
// 分流——既不是整条丢弃，也不是范围缩小，而是一条谁都没要求过的规则。
func TestUpdateClearsLegacyDomainGroupId(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	r := &model.RoutingRule{
		Remark: "老规则", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 造出「升级前就存在、被迁移回填过」的形态：domain_group_id 有原值。
	err := database.GetDB().Exec(
		"UPDATE routing_rules SET domain_group_id = ? WHERE id = ?", claude.Id, r.Id).Error
	if err != nil {
		t.Fatalf("simulate migrated row: %v", err)
	}

	r.DomainGroupIds = mustEncodeGroupIds(t, []int{chatgpt.Id})
	if err := (&RoutingRuleService{}).Update(r); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := (&RoutingRuleService{}).Get(r.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DomainGroupId != 0 {
		t.Errorf("编辑后 DomainGroupId = %d, want 0（停在旧值会让回退按一个已被删掉的组分流）",
			got.DomainGroupId)
	}
}

// 直连规则不引用任何出站节点，OutboundId 在这条路径上没有意义，
// 不该被要求填写。
func TestAddDirectRuleNeedsNoOutbound(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "国内直连")
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionDirect, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

// 一条从代理改成直连的规则，不该再挡住那个出站节点的删除：它已经不引用
// 那个节点了。CheckOutboundRefs 只数 action = proxy 的行，这条钉住这一点，
// 免得将来有人把条件放宽成「只看 outbound_id」——残留的 OutboundId 会让
// 一个早已没人引用的节点永远删不掉，而界面上找不到任何引用它的规则。
func TestCheckOutboundRefsIgnoresDirectRules(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "国内直连")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	s := RoutingRuleService{}
	// OutboundId 照原样残留，模拟管理员把一条代理规则改成了直连
	if err := s.Add(&model.RoutingRule{
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionDirect, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckOutboundRefs(node.Id); err != nil {
		t.Errorf("直连规则不该算作对出站节点的引用: %v", err)
	}
}

// newTestBlockRule 建一条覆盖全部入站的 block 规则。
//
// 每条规则各配一个独立域名组：checkConflict 不允许同一域名组下的入站集合
// 重叠，而这些规则的 InboundIds 都是「所有入站」。
func newTestBlockRule(t *testing.T, remark string, enable bool) *model.RoutingRule {
	t.Helper()
	g := newTestGroup(t, remark+"-组")
	r := &model.RoutingRule{
		Remark:         remark,
		InboundIds:     "[]",
		DomainGroupId:  g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock,
		Enable:         enable,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add rule %s: %v", remark, err)
	}
	return r
}

func TestReorderRewritesPriorityInGivenOrder(t *testing.T) {
	setupDB(t)
	// 中间那条是禁用的：它在界面列表里照样占一个位置、照样能被拖动，
	// 重排必须一视同仁，否则它被启用的那一刻会跳到一个谁都没安排过的位置。
	a := newTestBlockRule(t, "A", true)
	b := newTestBlockRule(t, "B", false)
	c := newTestBlockRule(t, "C", true)
	s := RoutingRuleService{}

	if err := s.Reorder([]int{c.Id, b.Id, a.Id}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}

	rules, err := s.GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	want := []int{c.Id, b.Id, a.Id}
	if len(rules) != len(want) {
		t.Fatalf("规则条数 = %d, want %d", len(rules), len(want))
	}
	for i, r := range rules {
		if r.Id != want[i] {
			t.Errorf("第 %d 位 id = %d, want %d", i, r.Id, want[i])
		}
		// priority 必须稠密：留空档会让「界面第 n 行」与 priority 脱钩，
		// 下一次重排的结果不再可预测。
		if r.Priority != i {
			t.Errorf("第 %d 位 priority = %d, want %d", i, r.Priority, i)
		}
	}
}

func TestReorderRejectsIdSetMismatch(t *testing.T) {
	setupDB(t)
	a := newTestBlockRule(t, "A", true)
	b := newTestBlockRule(t, "B", true)
	s := RoutingRuleService{}

	// 前端手里的 id 集合可能是陈旧的——另一个管理员刚新建或刚删掉一条规则。
	// 放行会让没列进来的那条规则留着旧 priority，顺序错乱且没有任何一层报错。
	cases := map[string][]int{
		"少一条":       {a.Id},
		"多一条":       {a.Id, b.Id, 9999},
		"重复":        {a.Id, a.Id},
		"条数对但换了 id": {a.Id, 9999},
		"空":         nil,
	}
	for name, ids := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.Reorder(ids); err == nil {
				t.Fatal("expected error for mismatched id set")
			}
			// 拒绝必须是整体的：一条 priority 都不许落库。
			rules, err := s.GetAll()
			if err != nil {
				t.Fatalf("GetAll: %v", err)
			}
			for _, r := range rules {
				if r.Priority != 0 {
					t.Errorf("被拒绝的重排写入了 priority: id=%d priority=%d", r.Id, r.Priority)
				}
			}
		})
	}
}

// 表单不再提交 priority（顺序由拖拽决定），新建的规则必须自己落到末尾。
// 恒为 0 的话它会和被重排成 priority=0 的那条并列，按 id asc 排在它之后——
// 新规则出现在列表第二行，位置既不是顶也不是底，谁都预料不到。
func TestNextPriorityFollowsLargest(t *testing.T) {
	setupDB(t)
	s := RoutingRuleService{}

	got, err := s.NextPriority()
	if err != nil {
		t.Fatalf("NextPriority on empty: %v", err)
	}
	if got != 0 {
		t.Errorf("空库的 NextPriority = %d, want 0", got)
	}

	a := newTestBlockRule(t, "A", true)
	b := newTestBlockRule(t, "B", true)
	if err := s.Reorder([]int{b.Id, a.Id}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}

	got, err = s.NextPriority()
	if err != nil {
		t.Fatalf("NextPriority: %v", err)
	}
	// 重排后最大 priority 是 1，新规则要排到它之后。
	if got != 2 {
		t.Errorf("NextPriority = %d, want 2", got)
	}
}

// 编辑一条规则不能改变它在列表里的位置。
//
// 表单不再提交 priority，ruleFromForm 出来的候选对象那一项恒为零值；Update
// 若照抄它，任何一次「改个备注」都会把规则弹到列表顶部，而管理员完全无从
// 预料——他改的是备注，动的却是分流的先后顺序。
func TestUpdateKeepsPriority(t *testing.T) {
	setupDB(t)
	a := newTestBlockRule(t, "A", true)
	b := newTestBlockRule(t, "B", true)
	s := RoutingRuleService{}
	if err := s.Reorder([]int{a.Id, b.Id}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}

	edited, err := s.Get(b.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	edited.Remark = "B 改过备注"
	edited.Priority = 0 // 表单绑定出来的零值
	if err := s.Update(edited); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(b.Id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Remark != "B 改过备注" {
		t.Errorf("备注没改到: %q", got.Remark)
	}
	if got.Priority != 1 {
		t.Errorf("编辑后 priority = %d, want 1（位置不该变）", got.Priority)
	}
}

// newTestRuleWithInbounds 建一条覆盖指定入站的 block 规则。
// 每条规则各配一个独立域名组：checkConflict 不允许同一域名组下入站集合重叠。
func newTestRuleWithInbounds(t *testing.T, remark string, inboundIds []int, applyToNew bool) *model.RoutingRule {
	t.Helper()
	g := newTestGroup(t, remark+"-组")
	encoded, err := EncodeInboundIds(inboundIds)
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	r := &model.RoutingRule{
		Remark:             remark,
		InboundIds:         encoded,
		DomainGroupId:      g.Id,
		DomainGroupIds:     mustEncodeGroupIds(t, []int{g.Id}),
		Action:             model.ActionBlock,
		Enable:             true,
		ApplyToNewInbounds: applyToNew,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add rule %s: %v", remark, err)
	}
	return r
}

func mustInboundIds(t *testing.T, ruleId int) []int {
	t.Helper()
	r, err := (&RoutingRuleService{}).Get(ruleId)
	if err != nil {
		t.Fatalf("Get rule %d: %v", ruleId, err)
	}
	ids, err := DecodeInboundIds(r.InboundIds)
	if err != nil {
		t.Fatalf("DecodeInboundIds: %v", err)
	}
	return ids
}

func TestAttachInboundOnlyTouchesRulesThatOptedIn(t *testing.T) {
	setupDB(t)
	opted := newTestRuleWithInbounds(t, "自动纳新", []int{7, 3}, true)
	manual := newTestRuleWithInbounds(t, "手工名单", []int{7, 3}, false)

	if err := (&RoutingRuleService{}).AttachInbound(database.GetDB(), 5); err != nil {
		t.Fatalf("AttachInbound: %v", err)
	}

	// 升序去重是「生成逐字节确定」的前提，顺序一抖动 Config.Equals 恒为
	// false，那个 10 秒的重启 cron 会不停重启 xray。
	got := mustInboundIds(t, opted.Id)
	want := []int{3, 5, 7}
	if len(got) != len(want) {
		t.Fatalf("声明自动应用的规则 inboundIds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("声明自动应用的规则 inboundIds = %v, want %v", got, want)
		}
	}

	if ids := mustInboundIds(t, manual.Id); len(ids) != 2 {
		t.Errorf("未声明的规则被改动了: %v", ids)
	}
}

// 空数组表示「所有用户」。往里追加一个 id 会让规则从「覆盖所有人」降级成
// 「只覆盖这一个人」，其余用户当场失去这条规则、静默走默认出站，而 xray
// 返回 Configuration OK、面板显示 running，没有任何一层会报错。
func TestAttachInboundSkipsAllUsersRule(t *testing.T) {
	setupDB(t)
	all := newTestRuleWithInbounds(t, "所有用户", nil, true)
	if ids := mustInboundIds(t, all.Id); len(ids) != 0 {
		t.Fatalf("前置条件不成立，规则不是空数组: %v", ids)
	}

	if err := (&RoutingRuleService{}).AttachInbound(database.GetDB(), 5); err != nil {
		t.Fatalf("AttachInbound: %v", err)
	}

	if ids := mustInboundIds(t, all.Id); len(ids) != 0 {
		t.Errorf("「所有用户」规则被降级成了具体名单: %v", ids)
	}
}

// 重复调用不应把同一个 id 塞两遍——EncodeInboundIds 会去重，但这条钉住
// 「重跑一次不会改变结果」这个性质。
func TestAttachInboundIsIdempotent(t *testing.T) {
	setupDB(t)
	r := newTestRuleWithInbounds(t, "自动纳新", []int{3}, true)
	s := RoutingRuleService{}
	for i := 0; i < 2; i++ {
		if err := s.AttachInbound(database.GetDB(), 5); err != nil {
			t.Fatalf("AttachInbound #%d: %v", i, err)
		}
	}
	got := mustInboundIds(t, r.Id)
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Errorf("inboundIds = %v, want [3 5]", got)
	}
}

// 建入站必须走 AddInbound：被测的正是这条路径上的扩散。入站配置用
// vlessSettings()+plainTCPStream（inbound_validate_test.go），它们能通过
// AddInbound 里那次真实 xray 校验——CI 的 linux 上带着 bin/xray-linux-amd64，
// 配置写错会出现「本地 macOS 过、CI 挂」。
func addTestInboundThroughService(t *testing.T, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true,
		Tag:      "inbound-" + strconv.Itoa(port),
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := (&InboundService{}).AddInbound(in); err != nil {
		t.Fatalf("AddInbound(%d): %v", port, err)
	}
	return in
}

func TestAddInboundAttachesToOptedInRules(t *testing.T) {
	setupDB(t)
	existing := addTestInboundThroughService(t, 20011)
	opted := newTestRuleWithInbounds(t, "自动纳新", []int{existing.Id}, true)
	manual := newTestRuleWithInbounds(t, "手工名单", []int{existing.Id}, false)

	created := addTestInboundThroughService(t, 20012)

	ids := mustInboundIds(t, opted.Id)
	found := false
	for _, id := range ids {
		if id == created.Id {
			found = true
		}
	}
	if !found {
		t.Errorf("新入站 %d 没被加进声明自动应用的规则: %v", created.Id, ids)
	}
	if got := mustInboundIds(t, manual.Id); len(got) != 1 || got[0] != existing.Id {
		t.Errorf("未声明的规则被改动了: %v", got)
	}
}

func TestAddInboundsAttachesToOptedInRules(t *testing.T) {
	setupDB(t)
	existing := addTestInboundThroughService(t, 20021)
	opted := newTestRuleWithInbounds(t, "自动纳新", []int{existing.Id}, true)

	// v2ui 迁移路径。只挂 AddInbound 会漏掉这条。
	batch := []*model.Inbound{
		{UserId: 1, Port: 20022, Protocol: model.VLESS, Enable: true,
			Tag: "inbound-20022", Settings: vlessSettings(),
			StreamSettings: plainTCPStream, Sniffing: "{}"},
		{UserId: 1, Port: 20023, Protocol: model.VLESS, Enable: true,
			Tag: "inbound-20023", Settings: vlessSettings(),
			StreamSettings: plainTCPStream, Sniffing: "{}"},
	}
	if err := (&InboundService{}).AddInbounds(batch); err != nil {
		t.Fatalf("AddInbounds: %v", err)
	}

	ids := mustInboundIds(t, opted.Id)
	if len(ids) != 3 {
		t.Errorf("批量建入站后 inboundIds = %v, want 3 个", ids)
	}
}

// 扩散失败必须让建入站整个失败。放行的话是一个用户静默地没进规则——比
// 人工遗漏更隐蔽，管理员以为系统已经处理了。
func TestAddInboundRollsBackWhenAttachFails(t *testing.T) {
	setupDB(t)
	existing := addTestInboundThroughService(t, 20031)
	rule := newTestRuleWithInbounds(t, "自动纳新", []int{existing.Id}, true)
	// 直接改库写进一段无法解码的 JSON，模拟脏数据让 AttachInbound 报错。
	err := database.GetDB().Model(model.RoutingRule{}).Where("id = ?", rule.Id).
		Update("inbound_ids", "{ 坏掉的 JSON").Error
	if err != nil {
		t.Fatalf("注入脏数据: %v", err)
	}

	in := &model.Inbound{
		UserId: 1, Port: 20032, Protocol: model.VLESS, Enable: true,
		Tag: "inbound-20032", Settings: vlessSettings(),
		StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := (&InboundService{}).AddInbound(in); err == nil {
		t.Fatal("扩散失败时 AddInbound 应当报错")
	}

	var count int64
	if err := database.GetDB().Model(model.Inbound{}).
		Where("port = ?", 20032).Count(&count).Error; err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Error("扩散失败但入站落库了，事务没有回滚")
	}
}

// 不加这条约束，新建入站会被同时扩散进两条规则，它们随即在同一域名组下
// 覆盖同一个入站——系统自己造出违反核心不变量的数据，而管理员什么都没做错。
func TestCheckConflictRejectsSecondAutoApplyRuleInSameGroup(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	first := &model.RoutingRule{
		Remark: "甲", InboundIds: "[3]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}

	second := &model.RoutingRule{
		Remark: "乙", InboundIds: "[9]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	err := s.Add(second)
	if err == nil {
		t.Fatal("同一域名组下第二条声明自动应用的规则应当被拒绝")
	}
	// importRules 靠 strings.Contains(err, "冲突") 把这类错误计入 Skipped
	// 而非 Failed，导入才能保持幂等。
	if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误文案必须含「冲突」二字，实际: %v", err)
	}
}

// 入站集合不重叠时，两条规则本来可以共存；只有都声明自动应用才冲突。
func TestCheckConflictAllowsSecondRuleWithoutAutoApply(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	first := &model.RoutingRule{
		Remark: "甲", InboundIds: "[3]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	second := &model.RoutingRule{
		Remark: "乙", InboundIds: "[9]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: false,
	}
	if err := s.Add(second); err != nil {
		t.Errorf("未声明自动应用的规则不该被拒绝: %v", err)
	}
}

// 判定单位是域名组不是规则：一条规则可以引用多个组，任一组撞上即冲突。
func TestCheckConflictAutoApplyIsPerDomainGroup(t *testing.T) {
	setupDB(t)
	g1 := newTestGroup(t, "ChatGPT")
	g2 := newTestGroup(t, "Claude")
	s := RoutingRuleService{}
	first := &model.RoutingRule{
		Remark: "甲", InboundIds: "[3]", DomainGroupId: g1.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g1.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	// 乙引用 {Claude, ChatGPT}，与甲在 ChatGPT 上撞车。
	second := &model.RoutingRule{
		Remark: "乙", InboundIds: "[9]", DomainGroupId: 0,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g1.Id, g2.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(second); err == nil {
		t.Error("多组规则与已有声明者在任一组上撞车都应当被拒绝")
	}
}

// 表单里有这个复选框，管理员改了就该落库——与相邻的 priority 结论相反。
func TestUpdateSyncsApplyToNewInbounds(t *testing.T) {
	setupDB(t)
	r := newTestRuleWithInbounds(t, "自动纳新", []int{3}, true)
	s := RoutingRuleService{}
	edited, err := s.Get(r.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	edited.ApplyToNewInbounds = false
	if err := s.Update(edited); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(r.Id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.ApplyToNewInbounds {
		t.Error("取消勾选没有落库")
	}
}
