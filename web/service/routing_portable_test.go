package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// newPortableTestInbound 建一个最小可用的入站。导入侧要按 remark+port 匹配它。
func newPortableTestInbound(t *testing.T, remark string, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId:   1,
		Remark:   remark,
		Enable:   true,
		Port:     port,
		Protocol: model.VMess,
		Tag:      "inbound-" + strconv.Itoa(port),
		Settings: "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	return in
}

func newTestOutbound(t *testing.T, tag, remark string) *model.OutboundNode {
	t.Helper()
	ob := &model.OutboundNode{
		Tag:      tag,
		Remark:   remark,
		Protocol: "socks",
		Config:   `{"tag":"` + tag + `","protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
		Enable:   true,
	}
	if err := database.GetDB().Save(ob).Error; err != nil {
		t.Fatalf("save outbound: %v", err)
	}
	return ob
}

// 导出文件是跨机器的，任何 id 都是本机私有的、到了对面必然指向别的东西。
func TestExportContainsNoIds(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	ob := newTestOutbound(t, "a-ui-hk", "香港")
	in := newPortableTestInbound(t, "用户甲", 2886)
	ids, err := EncodeInboundIds([]int{in.Id})
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	rule := &model.RoutingRule{
		Remark: "走香港", InboundIds: ids, DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionProxy, OutboundId: ob.Id, Priority: 10, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"id"`) {
		t.Errorf("导出文件里出现了 id 字段: %s", raw)
	}
	if strings.Contains(string(raw), `"domainGroupId"`) || strings.Contains(string(raw), `"outboundId"`) {
		t.Errorf("导出文件里出现了 id 外键: %s", raw)
	}
}

func TestExportRewritesReferencesToBusinessKeys(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	ob := newTestOutbound(t, "a-ui-hk", "香港")
	in := newPortableTestInbound(t, "用户甲", 2886)
	ids, _ := EncodeInboundIds([]int{in.Id})
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "走香港", InboundIds: ids, DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionProxy, OutboundId: ob.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("Rules len = %d, want 1", len(f.Rules))
	}
	r := f.Rules[0]
	if r.DomainGroupRef != "ChatGPT" {
		t.Errorf("DomainGroupRef = %q, want ChatGPT", r.DomainGroupRef)
	}
	if r.OutboundRef != "a-ui-hk" {
		t.Errorf("OutboundRef = %q, want a-ui-hk", r.OutboundRef)
	}
	if r.InboundRefs == nil || len(*r.InboundRefs) != 1 {
		t.Fatalf("InboundRefs = %v, want 长度 1 的非 nil 指针", r.InboundRefs)
	}
	refs := *r.InboundRefs
	if refs[0].Remark != "用户甲" || refs[0].Port != 2886 {
		t.Errorf("InboundRefs[0] = %+v", refs[0])
	}
	if f.Kind != ExportKind || f.Version != ExportVersion {
		t.Errorf("Kind/Version = %q/%d", f.Kind, f.Version)
	}
}

// 空数组是「对所有入站生效」，是用户显式表达的语义，必须原样导出。
func TestExportKeepsGlobalRuleAsEmptyRefs(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "全局封禁", InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("Rules len = %d", len(f.Rules))
	}
	// InboundRefs 是指针：nil 对应 JSON 的 null/字段缺失，导入端会整条拒绝；
	// 必须是一个指向空切片的非 nil 指针，才能表达「对所有入站生效」这个
	// 用户显式表达过的语义。
	if f.Rules[0].InboundRefs == nil {
		t.Fatal("全局规则的 InboundRefs 不该是 nil —— nil 会被导入端当成「字段缺失」整条拒绝")
	}
	if len(*f.Rules[0].InboundRefs) != 0 {
		t.Errorf("InboundRefs = %+v, want 空", *f.Rules[0].InboundRefs)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"inboundRefs":[]`) {
		t.Errorf("全局规则序列化后应是 inboundRefs:[] 而不是 null: %s", raw)
	}
}

// 订阅域名可达十几万条（生产实例实测 +111226），JSON 化后几 MB 到几十 MB，
// 浏览器一次性 stringify + Blob + 上传端 FileReader 读回来会卡死。
func TestExportOmitsSubscribedDomains(t *testing.T) {
	setupDB(t)
	subscribed, err := EncodeDomains([]string{"domain:sub1.com", "domain:sub2.com"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	manual, _ := EncodeDomains([]string{"domain:manual.com"})
	g := &model.DomainGroup{
		Remark: "订阅组", Domains: manual, SubscribeUrl: "https://example.com/list.txt",
		SubscribedDomains: subscribed, LastUpdatedAt: 1757030400000,
		LastError: "上次失败了", LastSkipped: 7,
	}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("Add group: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw, _ := json.Marshal(f)
	for _, forbidden := range []string{"sub1.com", "sub2.com", "lastUpdatedAt", "lastError", "lastSkipped", "subscribedDomains"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("导出文件不该含 %q: %s", forbidden, raw)
		}
	}
	if len(f.DomainGroups) != 1 {
		t.Fatalf("DomainGroups len = %d", len(f.DomainGroups))
	}
	if f.DomainGroups[0].SubscribeUrl != "https://example.com/list.txt" {
		t.Errorf("SubscribeUrl 应保留: %q", f.DomainGroups[0].SubscribeUrl)
	}
	if len(f.DomainGroups[0].Domains) != 1 || f.DomainGroups[0].Domains[0] != "domain:manual.com" {
		t.Errorf("手工域名应保留: %+v", f.DomainGroups[0].Domains)
	}
}

// remark 上没有 unique 约束，两个组重名在库里是合法的。一旦重名，导入端
// 无法确定 domainGroupRef 指向哪一个，猜错会产生一条指向错误域名组的规则
// ——而规则表会渲染得完全正常，配置也会正常生成，只是流量走错了节点，
// 没有任何一层防线会发现。
func TestExportRejectsDuplicateGroupRemarks(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "国内域名")
	newTestGroup(t, "国内域名")
	_, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err == nil {
		t.Fatal("域名组重名时应拒绝导出")
	}
	if !strings.Contains(err.Error(), "国内域名") {
		t.Errorf("错误信息应点名是哪个组: %v", err)
	}
}

// 只要导出了 domainGroups 就检查重名，不管有没有一起导出 rules——
// 分项导出的域名组文件将来会被拿去和分项导出的规则文件配套使用。
func TestExportRejectsDuplicateRemarksEvenWithoutRules(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "国内域名")
	newTestGroup(t, "国内域名")
	if _, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups); err == nil {
		t.Error("只导域名组时同样应拒绝")
	}
}

// 分项导出不隐式扩大范围：scope=rules 就只导规则，不带上它引用的域名组
// 和出站节点。隐式扩大会让 all 和 rules 的区别消失。
func TestExportScopeDoesNotWiden(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	ob := newTestOutbound(t, "a-ui-hk", "香港")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "走香港", InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action: model.ActionProxy, OutboundId: ob.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeRules)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Errorf("Rules len = %d, want 1", len(f.Rules))
	}
	if len(f.DomainGroups) != 0 {
		t.Errorf("scope=rules 不该带出域名组: %+v", f.DomainGroups)
	}
	if len(f.Outbounds) != 0 {
		t.Errorf("scope=rules 不该带出出站节点: %+v", f.Outbounds)
	}
	if len(f.Scope) != 1 || f.Scope[0] != ExportScopeRules {
		t.Errorf("Scope = %+v", f.Scope)
	}
}

func TestExportRejectsUnknownScope(t *testing.T) {
	setupDB(t)
	if _, err := (&RoutingPortableService{}).Export("everything"); err == nil {
		t.Error("未知 scope 应报错")
	}
}

func inboundsFixture() []*model.Inbound {
	return []*model.Inbound{
		{Id: 1, Remark: "用户甲", Port: 2886},
		{Id: 2, Remark: "用户乙", Port: 2887},
		{Id: 3, Remark: "重名", Port: 2888},
		{Id: 4, Remark: "重名", Port: 2889},
	}
}

func TestResolveInboundRefsMatchesByRemark(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "用户甲", Port: 9999}}, inboundsFixture())
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty", missing)
	}
	// 备注优先于端口：换机器后端口很可能改了，备注才是管理员认得的东西
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("ids = %v, want [1]", ids)
	}
}

func TestResolveInboundRefsFallsBackToPort(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "对面才有的备注", Port: 2887}}, inboundsFixture())
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Errorf("ids = %v, want [2]", ids)
	}
}

// 备注重名时无法区分是哪一个，不能猜——退到端口匹配。
func TestResolveInboundRefsSkipsAmbiguousRemark(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "重名", Port: 2889}}, inboundsFixture())
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if len(ids) != 1 || ids[0] != 4 {
		t.Errorf("ids = %v, want [4] —— 备注重名应退到端口匹配", ids)
	}
}

// 备注重名且端口也对不上，就是真的认不出来了。
func TestResolveInboundRefsReportsMissing(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "重名", Port: 7777}}, inboundsFixture())
	if len(missing) != 1 {
		t.Fatalf("missing = %v, want 1 项", missing)
	}
	if !strings.Contains(missing[0], "重名") || !strings.Contains(missing[0], "7777") {
		t.Errorf("missing 描述应同时含备注与端口，便于管理员对号入座: %q", missing[0])
	}
	if len(ids) != 0 {
		t.Errorf("认不出时不该返回任何 id: %v", ids)
	}
}

// 部分命中时返回已命中的 id 和缺失清单。调用方据此决定禁用整条规则——
// 绝不能拿这个部分列表当作完整覆盖集去启用规则。
func TestResolveInboundRefsPartialMatch(t *testing.T) {
	ids, missing := resolveInboundRefs([]PortableInboundRef{
		{Remark: "用户甲", Port: 2886},
		{Remark: "不存在的人", Port: 7777},
	}, inboundsFixture())
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("ids = %v, want [1]", ids)
	}
	if len(missing) != 1 {
		t.Errorf("missing = %v, want 1 项", missing)
	}
}

// 空 refs 是「对所有入站生效」，是合法且必须原样保留的语义，不是「认不出」。
func TestResolveInboundRefsEmptyMeansGlobal(t *testing.T) {
	ids, missing := resolveInboundRefs([]PortableInboundRef{}, inboundsFixture())
	if len(missing) != 0 {
		t.Errorf("空 refs 不该产生 missing: %v", missing)
	}
	if len(ids) != 0 {
		t.Errorf("空 refs 应返回空 ids: %v", ids)
	}
}

func exportJSON(t *testing.T, f *ExportFile) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func baseExportFile() *ExportFile {
	return &ExportFile{
		Kind: ExportKind, Version: ExportVersion,
		Scope:        []string{ExportScopeDomainGroups, ExportScopeOutbounds, ExportScopeRules},
		DomainGroups: []PortableDomainGroup{},
		Outbounds:    []PortableOutbound{},
		Rules:        []PortableRule{},
	}
}

// refsPtr 构造 PortableRule.InboundRefs 需要的指针字面量。该字段是指针而
// 不是值类型切片，为的是能把「显式空数组」（对所有入站生效）与「null/
// 字段缺失」（必须整条拒绝的不可信输入）区分开，构造测试数据时需要取地址。
// 不传参数时返回指向空切片的非 nil 指针，对应文件里显式写的 []。
func refsPtr(refs ...PortableInboundRef) *[]PortableInboundRef {
	list := append([]PortableInboundRef{}, refs...)
	return &list
}

func TestImportRejectsWrongKind(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Kind = "something-else"
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err == nil {
		t.Error("Kind 不对应整体拒绝")
	}
}

func TestImportRejectsWrongVersion(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Version = 999
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err == nil {
		t.Error("Version 不认识应整体拒绝")
	}
}

func TestImportRejectsMalformedJSON(t *testing.T) {
	setupDB(t)
	if _, err := (&RoutingPortableService{}).Import("{not json"); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

func TestImportCreatesDomainGroupAndOutbound(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{
		{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}, SubscribeUrl: ""},
	}
	f.Outbounds = []PortableOutbound{
		{Tag: "a-ui-hk", Remark: "香港", Protocol: "socks",
			Config: `{"tag":"a-ui-hk","protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
			Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.DomainGroups.Created != 1 {
		t.Errorf("DomainGroups.Created = %d, want 1 (%v)", rep.DomainGroups.Created, rep.Messages)
	}
	if rep.Outbounds.Created != 1 {
		t.Errorf("Outbounds.Created = %d, want 1 (%v)", rep.Outbounds.Created, rep.Messages)
	}
	groups, _ := (&DomainGroupService{}).GetAll()
	if len(groups) != 1 || groups[0].Remark != "ChatGPT" {
		t.Errorf("库里的域名组不对: %+v", groups)
	}
	nodes, _ := (&OutboundNodeService{}).GetAll()
	if len(nodes) != 1 || nodes[0].Tag != "a-ui-hk" {
		t.Errorf("出站节点的 tag 必须原样保留，否则规则引用会失效: %+v", nodes)
	}
}

// 同一个文件导两次不该变成双份，也不该有第二次的副作用。
func TestImportIsIdempotent(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Outbounds = []PortableOutbound{
		{Tag: "a-ui-hk", Remark: "香港", Protocol: "socks",
			Config: `{"tag":"a-ui-hk","protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
			Enable: true},
	}
	raw := exportJSON(t, f)
	s := RoutingPortableService{}
	if _, err := s.Import(raw); err != nil {
		t.Fatalf("首次 Import: %v", err)
	}
	rep, err := s.Import(raw)
	if err != nil {
		t.Fatalf("二次 Import: %v", err)
	}
	if rep.DomainGroups.Created != 0 || rep.DomainGroups.Skipped != 1 {
		t.Errorf("域名组应全部跳过: %+v", rep.DomainGroups)
	}
	if rep.Outbounds.Created != 0 || rep.Outbounds.Skipped != 1 {
		t.Errorf("出站节点应全部跳过: %+v", rep.Outbounds)
	}
	groups, _ := (&DomainGroupService{}).GetAll()
	if len(groups) != 1 {
		t.Errorf("域名组变成了 %d 份", len(groups))
	}
}

// a-ui-block / a-ui-default 是注入器自己发的 tag，不在 outbound_nodes 表里，
// 数据库唯一约束看不见它们。撞名会让 xray 报 existing tag found 并拒绝启动
// 整份配置——全员断网，而面板首页仍显示 running。
func TestImportRejectsReservedTag(t *testing.T) {
	setupDB(t)
	for _, tag := range []string{model.BlockOutboundTag, model.DefaultOutboundTag} {
		f := baseExportFile()
		f.Outbounds = []PortableOutbound{
			{Tag: tag, Remark: "坏节点", Protocol: "socks",
				Config: `{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
				Enable: true},
		}
		rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if rep.Outbounds.Failed != 1 {
			t.Errorf("tag %s 应被拒绝: %+v %v", tag, rep.Outbounds, rep.Messages)
		}
		nodes, _ := (&OutboundNodeService{}).GetAll()
		if len(nodes) != 0 {
			t.Errorf("保留 tag 的节点不该落库: %+v", nodes)
		}
	}
}

func TestImportRejectsEmptyTag(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Outbounds = []PortableOutbound{
		{Tag: "", Remark: "没 tag", Protocol: "socks",
			Config: `{"protocol":"socks","settings":{}}`, Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Outbounds.Failed != 1 {
		t.Errorf("空 tag 应被拒绝: %+v", rep.Outbounds)
	}
}

// "null" 能通过 json.Unmarshal 却留下一个 nil map，紧接着给它赋值直接
// panic（routing_outbound.go 的 Update 里就记着这个坑）。
func TestImportHandlesNullConfig(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Outbounds = []PortableOutbound{
		{Tag: "a-ui-bad", Remark: "坏配置", Protocol: "socks", Config: "null", Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import 不该 panic 或整体失败: %v", err)
	}
	if rep.Outbounds.Failed != 1 {
		t.Errorf("null config 应被拒绝: %+v", rep.Outbounds)
	}
}

// ===== 本文件最重要的一条：入站认不全时必须禁用，绝不清空 =====
func TestImportDisablesRuleWhenInboundMissing(t *testing.T) {
	setupDB(t)
	newPortableTestInbound(t, "用户甲", 2886)

	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "走香港", DomainGroupRef: "ChatGPT", OutboundRef: "",
		InboundRefs: refsPtr(
			PortableInboundRef{Remark: "用户甲", Port: 2886},
			PortableInboundRef{Remark: "对面才有的用户", Port: 9999},
		),
		Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Created != 1 {
		t.Fatalf("规则应被导入（禁用状态）: %+v %v", rep.Rules, rep.Messages)
	}
	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d", len(rules))
	}
	r := rules[0]
	if r.Enable {
		t.Error("入站认不全的规则必须导入为禁用")
	}
	if r.InboundIds == "[]" {
		t.Fatal("InboundIds 被清空成了 []，这等于「对所有入站生效」—— " +
			"一条本该只覆盖某个人的规则被静默放大到全体，而 xray 对此返回 Configuration OK")
	}
	ids, err := DecodeInboundIds(r.InboundIds)
	if err != nil {
		t.Fatalf("DecodeInboundIds: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("应保留已命中的那一个入站: %v", ids)
	}
	joined := strings.Join(rep.Messages, "\n")
	if !strings.Contains(joined, "对面才有的用户") {
		t.Errorf("报告里应点名缺失的入站: %v", rep.Messages)
	}
}

func TestImportKeepsEnabledWhenAllInboundsMatch(t *testing.T) {
	setupDB(t)
	newPortableTestInbound(t, "用户甲", 2886)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "封禁", DomainGroupRef: "ChatGPT",
		InboundRefs: refsPtr(PortableInboundRef{Remark: "用户甲", Port: 2886}),
		Action:      model.ActionBlock, Enable: true,
	}}
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err != nil {
		t.Fatalf("Import: %v", err)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 1 || !rules[0].Enable {
		t.Errorf("全部命中时应保持文件里的 enable: %+v", rules)
	}
}

func TestImportKeepsGlobalRuleGlobal(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "违规", Domains: []string{"domain:bad.com"}}}
	f.Rules = []PortableRule{{
		Remark: "全局封禁", DomainGroupRef: "违规",
		InboundRefs: refsPtr(), // 显式的「所有入站」
		Action:      model.ActionBlock, Enable: true,
	}}
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err != nil {
		t.Fatalf("Import: %v", err)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d", len(rules))
	}
	if rules[0].InboundIds != "[]" {
		t.Errorf("全局规则应保持 []: %q", rules[0].InboundIds)
	}
	if !rules[0].Enable {
		t.Error("全局规则不该被误判成「认不出」而禁用")
	}
}

func TestImportSkipsRuleWithMissingGroupRef(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Rules = []PortableRule{{
		Remark: "孤儿规则", DomainGroupRef: "本机没有的组",
		InboundRefs: refsPtr(), Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Failed != 1 {
		t.Errorf("引用不到域名组的规则应整条跳过: %+v", rep.Rules)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 0 {
		t.Errorf("不该落库: %+v", rules)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "本机没有的组") {
		t.Errorf("报告应点名: %v", rep.Messages)
	}
}

// 新导入的订阅组 LastUpdatedAt = 0，ShouldUpdateNow 对 0 直接返回 true，
// SubscriptionJob 每 10 分钟一次会自动补上首次拉取。导入路径本身不拉——
// 一个慢地址能把 HTTP 请求挂满 30 秒。
func TestImportSubscribedGroupStartsUnfetched(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{
		{Remark: "订阅组", Domains: []string{}, SubscribeUrl: "https://example.com/list.txt"},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	groups, _ := (&DomainGroupService{}).GetAll()
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].LastUpdatedAt != 0 {
		t.Errorf("LastUpdatedAt = %d, want 0（0 才会被 ShouldUpdateNow 立即拉取）", groups[0].LastUpdatedAt)
	}
	if groups[0].SubscribedDomains != "" {
		t.Errorf("SubscribedDomains 应为空: %q", groups[0].SubscribedDomains)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "订阅") {
		t.Errorf("报告应提示订阅组还没拉取: %v", rep.Messages)
	}
}

func TestImportRejectsBadSubscribeUrl(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{
		{Remark: "坏订阅", Domains: []string{}, SubscribeUrl: "ftp://example.com/x"},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.DomainGroups.Failed != 1 {
		t.Errorf("非 http(s) 订阅地址应被拒: %+v", rep.DomainGroups)
	}
}

// C1：inboundRefs 为 null 或字段缺失时，与显式的空数组 []（对所有入站
// 生效，用户明确表达的语义）在语义上天差地别，绝不能当成后者放行——
// 手工改过、别的工具生成的、传输被截断的文件都可能命中这条。
func TestImportRejectsRuleWithNilInboundRefs(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "缺字段", DomainGroupRef: "ChatGPT",
		// InboundRefs 留零值 nil：序列化后是 "inboundRefs":null，模拟文件
		// 里该字段为 null 或整个键缺失（两者反序列化结果完全相同）。
		Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Failed != 1 {
		t.Errorf("inboundRefs 为 null/缺失应整条拒绝: %+v %v", rep.Rules, rep.Messages)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 0 {
		t.Errorf("不该落库: %+v", rules)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "inboundRefs") {
		t.Errorf("报告应点名缺少 inboundRefs 字段: %v", rep.Messages)
	}
}

// C2：DomainGroup.Remark 没有唯一约束，本机存在两个同名组是完全可达的
// 状态。导出侧对此是硬拒绝，导入侧面对同一个歧义绝不能猜一个——猜错会
// 产生一条指向错误域名组的规则，规则表渲染得完全正常、配置也正常生成，
// 只是流量走错了节点，没有任何一层防线会发现。
func TestImportRejectsRuleWhenDomainGroupRemarkAmbiguous(t *testing.T) {
	setupDB(t)
	if err := (&DomainGroupService{}).Add(&model.DomainGroup{Remark: "国内域名", Domains: "[]"}); err != nil {
		t.Fatalf("Add group 1: %v", err)
	}
	if err := (&DomainGroupService{}).Add(&model.DomainGroup{Remark: "国内域名", Domains: "[]"}); err != nil {
		t.Fatalf("Add group 2: %v", err)
	}
	f := baseExportFile()
	f.Rules = []PortableRule{{
		Remark: "走哪个", DomainGroupRef: "国内域名",
		InboundRefs: refsPtr(), Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Failed != 1 {
		t.Errorf("本机域名组重名时应拒绝，不能猜: %+v %v", rep.Rules, rep.Messages)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 0 {
		t.Errorf("不该落库: %+v", rules)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "国内域名") {
		t.Errorf("报告应点名: %v", rep.Messages)
	}
}

// I1：一个入站都没认出来时必须整条丢弃——删掉这道判断，其余测试依然
// 全部通过，而删掉它的后果正是本功能最高危的路径：InboundIds 被编码成
// []，等于把一条本该只覆盖某几个人的规则放大成「对所有入站生效」。
// 顺带覆盖 I2：报告不该先说「已导入但保持禁用」再说「整条跳过」——
// 两句自相矛盾，管理员会去规则列表里找一条根本不存在的禁用规则。
func TestImportDropsRuleWhenNoInboundMatched(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "全认不出", DomainGroupRef: "ChatGPT",
		InboundRefs: refsPtr(
			PortableInboundRef{Remark: "甲", Port: 11111},
			PortableInboundRef{Remark: "乙", Port: 22222},
		),
		Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Failed != 1 {
		t.Errorf("一个都没认出来应整条丢弃: %+v %v", rep.Rules, rep.Messages)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 0 {
		t.Errorf("不该落库，哪怕是禁用状态: %+v", rules)
	}
	joined := strings.Join(rep.Messages, "\n")
	if strings.Contains(joined, "已导入但保持禁用") {
		t.Errorf("不该同时出现「已导入但保持禁用」与「整条跳过」这两条自相矛盾的消息: %v", rep.Messages)
	}
}

// I3：手工新增路径的 allocTag 恒定产出 a-ui-<...>，结构上不可能与模板
// 撞名——「所有生成的 tag 统一带 a-ui- 前缀」这条不变量就是这么成立的。
// 导入保留原 tag 打破了这条隔离：web/service/config.json 模板里有一个
// tag 为 blocked 的出站，撞名会让 xray 报 existing tag found 并拒绝启动
// 整份配置——全员断网，而面板首页仍显示 running。
func TestImportRejectsOutboundTagWithoutAUIPrefix(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Outbounds = []PortableOutbound{
		{Tag: "blocked", Remark: "撞模板", Protocol: "socks",
			Config: `{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
			Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Outbounds.Failed != 1 {
		t.Errorf("非 a-ui- 前缀的 tag 应被拒绝，可能与模板出站撞名: %+v %v", rep.Outbounds, rep.Messages)
	}
	nodes, _ := (&OutboundNodeService{}).GetAll()
	if len(nodes) != 0 {
		t.Errorf("不该落库: %+v", nodes)
	}
}

// I4：两级匹配可能把两个不同的 ref 撞到同一个本机入站——一个 ref 按
// remark 精确命中，另一个 ref 的 remark 对不上、退到 port 又刚好命中了
// 同一个入站。此时 resolveInboundRefs 不会产生 missing（两个 ref 都
// 「命中」了），但 EncodeInboundIds 会把重复 id 静默去重，覆盖范围因此
// 缩小——这是消费端能零成本察觉的塌缩，必须报告并禁用。
func TestImportDisablesRuleWhenInboundRefsCollapseToSameInbound(t *testing.T) {
	setupDB(t)
	newPortableTestInbound(t, "老张", 2886)

	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "覆盖两人", DomainGroupRef: "ChatGPT",
		InboundRefs: refsPtr(
			PortableInboundRef{Remark: "老张", Port: 2886},
			PortableInboundRef{Remark: "对面的老王", Port: 2886}, // remark 对不上，退到 port，刚好撞上老张
		),
		Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Created != 1 {
		t.Fatalf("规则应被导入（禁用状态）: %+v %v", rep.Rules, rep.Messages)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d", len(rules))
	}
	if rules[0].Enable {
		t.Error("入站引用塌缩到同一个入站时必须导入为禁用，覆盖范围已经缩小")
	}
	ids, err := DecodeInboundIds(rules[0].InboundIds)
	if err != nil {
		t.Fatalf("DecodeInboundIds: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("塌缩后应只剩一个 id: %v", ids)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "同一个入站") {
		t.Errorf("报告应提示覆盖范围已缩小: %v", rep.Messages)
	}
}

// M2：Skipped 的定义是「本机已存在」。重复导入被 checkConflict 拒下的
// 规则语义上正是「已存在」，不该计入 Failed——否则一次完全正常的重跑会
// 显示成「规则：失败 N」，与「导入是幂等的」这条设计前提自相矛盾。
func TestImportRuleConflictCountsAsSkippedNotFailed(t *testing.T) {
	setupDB(t)
	newPortableTestInbound(t, "用户甲", 2886)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark:         "封禁",
		DomainGroupRef: "ChatGPT",
		InboundRefs:    refsPtr(PortableInboundRef{Remark: "用户甲", Port: 2886}),
		Action:         model.ActionBlock, Enable: true,
	}}
	raw := exportJSON(t, f)
	s := RoutingPortableService{}
	if _, err := s.Import(raw); err != nil {
		t.Fatalf("首次 Import: %v", err)
	}
	rep, err := s.Import(raw)
	if err != nil {
		t.Fatalf("二次 Import: %v", err)
	}
	if rep.Rules.Failed != 0 {
		t.Errorf("重复导入的规则不该计入 Failed: %+v %v", rep.Rules, rep.Messages)
	}
	if rep.Rules.Skipped != 1 {
		t.Errorf("重复导入的规则应计入 Skipped: %+v", rep.Rules)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 1 {
		t.Errorf("规则不该变成两份: %+v", rules)
	}
}

// F1：空 remark 不是「认不出来」的候补键。DBInbound.remark 的默认值就是
// 空串，入站表单也不强制填，两台机器各自有一个未命名入站是完全可达的
// 组合。resolveInboundRefs 必须把 ref.Remark == "" 当成「没有可用的
// remark 线索」直接退到 port，绝不能去 byRemark[""] 里查——否则会静默
// 命中本机那个同样没起名的入站，端口线索被完全绕过，且因为「命中了」
// 而连「认不全就禁用」这道兜底都不会触发。
func TestResolveInboundRefsDoesNotMatchEmptyRemark(t *testing.T) {
	inbounds := []*model.Inbound{
		{Id: 1, Remark: "", Port: 2886},
		{Id: 2, Remark: "用户乙", Port: 2887},
	}

	// 端口也对不上：不能落到 id 1 头上，必须诚实地报告认不出来。
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "", Port: 9999}}, inbounds)
	if len(ids) != 0 {
		t.Errorf("空 remark 不该命中任何入站: ids = %v", ids)
	}
	if len(missing) != 1 {
		t.Fatalf("missing = %v, want 1 项", missing)
	}

	// 端口对得上：空 remark 应该正常退到 port 匹配。
	ids, missing = resolveInboundRefs(
		[]PortableInboundRef{{Remark: "", Port: 2886}}, inbounds)
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty", missing)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("ids = %v, want [1] —— 空 remark 应能退到 port 匹配", ids)
	}
}

// F1：同一个问题在域名组侧重演——DomainGroup.Remark 没有唯一约束，本机
// 存在一个备注为空的域名组是完全可达的状态（导出侧遇到备注为空的组会
// 忠实保留 domainGroupRef=""）。groupByRemark[""] 若参与匹配，会在本机
// 恰好也有一个空备注组时静默命中它，产生一条指向错误域名组、看起来完全
// 正常的规则。必须在查表之前就把空字符串当成「没有指定域名组」拒绝。
func TestImportRejectsRuleWithEmptyDomainGroupRef(t *testing.T) {
	setupDB(t)
	// 本机存在一个备注同样为空的域名组——若不拦截，会被静默命中。
	if err := (&DomainGroupService{}).Add(&model.DomainGroup{Remark: "", Domains: "[]"}); err != nil {
		t.Fatalf("Add group: %v", err)
	}
	f := baseExportFile()
	f.Rules = []PortableRule{{
		Remark: "没组的规则", DomainGroupRef: "",
		InboundRefs: refsPtr(), Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Failed != 1 {
		t.Errorf("domainGroupRef 为空应整条拒绝，不能命中本机同样没起名的域名组: %+v %v", rep.Rules, rep.Messages)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 0 {
		t.Errorf("不该落库: %+v", rules)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "没组的规则") {
		t.Errorf("报告应点名: %v", rep.Messages)
	}
}

// F2：checkDuplicateGroupRemarks 在分发 scope 之前无条件执行时，管理员
// 只想导出出站节点也会因库里两个与本次导出毫无关系的同名域名组而整体
// 失败。scope=outbounds 与域名组完全无关，不该被挡住。
func TestExportOutboundsOnlyIgnoresDuplicateGroupRemarks(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "国内域名")
	newTestGroup(t, "国内域名")
	ob := newTestOutbound(t, "a-ui-hk", "香港")

	f, err := (&RoutingPortableService{}).Export(ExportScopeOutbounds)
	if err != nil {
		t.Fatalf("scope=outbounds 不该被无关的域名组重名挡住: %v", err)
	}
	if len(f.Outbounds) != 1 || f.Outbounds[0].Tag != ob.Tag {
		t.Errorf("Outbounds = %+v, want 含 %s", f.Outbounds, ob.Tag)
	}
	if len(f.DomainGroups) != 0 {
		t.Errorf("scope=outbounds 不该带出域名组: %+v", f.DomainGroups)
	}
}

// F2：scope=rules 虽不导出域名组本身，但规则里带着 domainGroupRef，重名
// 同样会让导入端无法确定指向哪一个，必须继续拒绝——不能把 outbounds 的
// 豁免连带扩大到 rules。
func TestExportRulesScopeStillRejectsDuplicateGroupRemarks(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "国内域名")
	newTestGroup(t, "国内域名")
	if _, err := (&RoutingPortableService{}).Export(ExportScopeRules); err == nil {
		t.Error("scope=rules 时域名组重名仍应拒绝导出")
	}
}

// 真实写入路径（ruleFromForm / importRules）只写 DomainGroupIds，DomainGroupId
// 恒为 0。本仓库另有一批同时写两个字段的测试夹具，那种形态现实中已不存在，
// 会掩盖一整类只在「新字段有值、旧字段为 0」时才暴露的缺陷。本用例刻意只写
// 新字段，复现真实写入路径的产出，守住「导出不会静默漏掉这类规则」。
//
// 症状之所以必须测：toPortableRule 拒绝整条时 Export 只 logger.Warning 后
// continue，规则从导出文件里静默消失，界面上没有任何信号。
func TestExportRuleWithOnlyDomainGroupIds(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	// RoutingRuleService.Add 原样落库，不补旧字段，所以这条规则的
	// DomainGroupId 会是 0——正是面板里新建一条规则之后的真实形态。
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "只有新字段", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export("rules")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("只写 DomainGroupIds 的规则被导出漏掉了（导出 %d 条，want 1）", len(f.Rules))
	}
	if f.Rules[0].DomainGroupRef != "Claude" {
		t.Errorf("domainGroupRef = %q, want Claude", f.Rules[0].DomainGroupRef)
	}
}

// 多组规则导出时 domainGroupRef 必须留空：旧面板见到空值会明确拒绝，
// 好过让它在多个同名候选里猜一个，产生一条指向错误组的规则——那种规则
// 在规则表和生成的配置里都渲染得完全正常，只是流量走错节点。
func TestExportMultiGroupRuleLeavesLegacyRefEmpty(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "两组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id, chatgpt.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export("rules")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("导出了 %d 条规则，want 1", len(f.Rules))
	}
	if f.Rules[0].DomainGroupRef != "" {
		t.Errorf("多组规则的 domainGroupRef 必须为空，got %q", f.Rules[0].DomainGroupRef)
	}
	if f.Rules[0].DomainGroupRefs == nil || len(*f.Rules[0].DomainGroupRefs) != 2 {
		t.Fatalf("domainGroupRefs = %v, want 两个组名", f.Rules[0].DomainGroupRefs)
	}
}

func TestExportSingleGroupRuleFillsLegacyRef(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "一组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export("rules")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if f.Rules[0].DomainGroupRef != "Claude" {
		t.Errorf("单组规则必须填 domainGroupRef 以兼容旧面板，got %q", f.Rules[0].DomainGroupRef)
	}
}

// v1.7.0 及更早导出的文件只有 domainGroupRef。
func TestImportAcceptsLegacySingleGroupRef(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	f := &ExportFile{
		Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
		Rules: []PortableRule{{
			Remark: "旧格式", DomainGroupRef: "Claude", InboundRefs: &refs,
			Action: model.ActionBlock, Enable: true,
		}},
	}
	report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 1 {
		t.Fatalf("旧格式必须能导入: %+v", report)
	}
}

func TestImportRejectsRuleWithoutAnyGroupRef(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	empty := []string{}
	// 两种情形都拒绝，但报告文案必须不同——这正是 DomainGroupRefs 用指针
	// 类型换来的东西。「显式提交了空数组」管理员得去改文件里那一行；
	// 「字段根本不在」说明文件被截断或压根不是本工具导出的。混成同一句话，
	// 指针类型就白用了，而结构体上的注释也就成了一句不成立的声称。
	for name, tc := range map[string]struct {
		rule    PortableRule
		wantMsg string
	}{
		"两个字段都缺": {
			rule:    PortableRule{Remark: "A", InboundRefs: &refs, Action: model.ActionBlock, Enable: true},
			wantMsg: "缺少 domainGroupRefs 字段",
		},
		"显式空数组": {
			rule:    PortableRule{Remark: "B", DomainGroupRefs: &empty, InboundRefs: &refs, Action: model.ActionBlock, Enable: true},
			wantMsg: "是一个空数组",
		},
	} {
		f := &ExportFile{
			Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
			Rules: []PortableRule{tc.rule},
		}
		report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
		if err != nil {
			t.Fatalf("%s：Import: %v", name, err)
		}
		if report.Rules.Created != 0 {
			t.Errorf("%s：一个域名组都没有的规则必须整条拒绝，got created=%d",
				name, report.Rules.Created)
		}
		joined := strings.Join(report.Messages, "\n")
		if !strings.Contains(joined, tc.wantMsg) {
			t.Errorf("%s：报告应含 %q，实际为 %v", name, tc.wantMsg, report.Messages)
		}
	}
}

// 与入站对称：部分组认不出 → 导入成禁用；全部认不出 → 整条丢弃。
func TestImportPartialGroupMatchImportsDisabled(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	groups := []string{"Claude", "本机没有的组"}
	f := &ExportFile{
		Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
		Rules: []PortableRule{{
			Remark: "部分命中", DomainGroupRefs: &groups, InboundRefs: &refs,
			Action: model.ActionBlock, Enable: true,
		}},
	}
	report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 1 {
		t.Fatalf("部分命中应导入成禁用而不是丢弃: %+v", report)
	}
	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 || rules[0].Enable {
		t.Errorf("部分命中的规则必须导入成禁用状态, got enable=%v", rules[0].Enable)
	}
}

func TestImportDropsRuleWhenNoGroupMatches(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	groups := []string{"本机没有的组"}
	f := &ExportFile{
		Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
		Rules: []PortableRule{{
			Remark: "全不命中", DomainGroupRefs: &groups, InboundRefs: &refs,
			Action: model.ActionBlock, Enable: true,
		}},
	}
	report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 0 {
		t.Errorf("一个组都认不出必须整条丢弃，got created=%d", report.Rules.Created)
	}
}

// spec §10 的「新格式往返」：多组规则导出、再导入到另一台机器，规则引用的
// 域名组必须【恰好】还原成那两个组，且升序。
//
// 只断言 report.Rules.Created 或 len(domainGroupRefs) 挡不住 importRules 里的
// 部分映射缺陷（2 个 ref 进、1 个 id 出）：截断后的集合在 ruleService.Add →
// validate 眼里完全合法（validate 只拒绝不存在的 id），规则表渲染正常、
// 生成的配置也合法，唯一的症状是流量走错节点。
func TestRoundTripMultiGroupRuleRestoresExactGroupSet(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	srcInbound := newPortableTestInbound(t, "甲", 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "两组封禁", InboundIds: mustEncodeIds(t, []int{srcInbound.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id, chatgpt.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export(ExportScopeRules)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw := exportJSON(t, f)

	// 换一台机器：setupDB 重建一个全新的库。故意先建一个无关的组、再按【相反】
	// 的顺序建那两个组，好让目标机器上的 id 与源机器完全不同——两边 id 恰好
	// 相同的话，一个「把源 id 原样照抄过来」的实现也能通过，用例就白写了。
	setupDB(t)
	newTestGroup(t, "无关组")
	dstChatGPT := newTestGroup(t, "ChatGPT")
	dstClaude := newTestGroup(t, "Claude")
	newPortableTestInbound(t, "甲", 10001)

	report, err := (&RoutingPortableService{}).Import(raw)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 1 {
		t.Fatalf("规则没有导入成功: %+v", report)
	}

	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("规则条数 = %d, want 1", len(rules))
	}
	if !rules[0].Enable {
		t.Errorf("组与入站全部命中时不该被降级成禁用: %v", report.Messages)
	}
	got, err := DecodeDomainGroupIds(rules[0].DomainGroupIds)
	if err != nil {
		t.Fatalf("DecodeDomainGroupIds: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("DomainGroupIds = %v, want 恰好两个组（本机 Claude=%d, ChatGPT=%d）",
			got, dstClaude.Id, dstChatGPT.Id)
	}
	if got[0] >= got[1] {
		t.Errorf("DomainGroupIds = %v, 必须升序去重（生成逐字节确定的前提）", got)
	}
	inSet := map[int]bool{got[0]: true, got[1]: true}
	if !inSet[dstClaude.Id] || !inSet[dstChatGPT.Id] {
		t.Errorf("DomainGroupIds = %v, want 恰好 {Claude=%d, ChatGPT=%d}",
			got, dstClaude.Id, dstChatGPT.Id)
	}
}

// 导出与 buildRule 对齐：一条规则引用的组里有一个已经悬空时，buildRule 只剔掉
// 那一个、规则照常写进配置，所以这条规则在本机【正在生效】。导出必须照样导出
// 它，否则一条正在生效的封禁规则会从导出文件里静默消失——Export 对跳过只记
// logger.Warning，导入报告和导出结果里一个字都没有。
func TestExportDropsDanglingGroupButKeepsRule(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	ghost := newTestGroup(t, "马上要被删的组")
	in := newPortableTestInbound(t, "甲", 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "两组封禁", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id, ghost.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 绕过 DomainGroupService.Del 的引用守卫直接删行。守卫有洞时（直接改库、
	// 并发写入、迁移前留下的残骸）库里留下的正是这个形态。
	if err := database.GetDB().Exec("DELETE FROM domain_groups WHERE id = ?", ghost.Id).Error; err != nil {
		t.Fatalf("delete group: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeRules)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("导出了 %d 条规则，want 1（一个组悬空不该让整条规则消失）", len(f.Rules))
	}
	if f.Rules[0].DomainGroupRefs == nil {
		t.Fatal("domainGroupRefs 不能是 nil 指针")
	}
	refs := *f.Rules[0].DomainGroupRefs
	if len(refs) != 1 || refs[0] != "Claude" {
		t.Errorf("domainGroupRefs = %v, want [Claude]（悬空的组剔除，其余保留）", refs)
	}
	// 剔到只剩一个组之后，兼容旧面板的单值字段照常填。
	if f.Rules[0].DomainGroupRef != "Claude" {
		t.Errorf("domainGroupRef = %q, want Claude", f.Rules[0].DomainGroupRef)
	}
}

// 剔到一个组都不剩时才拒绝整条：那时导出的 domainGroupRefs 会是空数组，
// 等于 domain 条件为空——导入端见到它必须拒绝，导出端不该先把这颗雷埋进去。
func TestExportSkipsRuleWhenEveryGroupIsDangling(t *testing.T) {
	setupDB(t)
	ghost := newTestGroup(t, "马上要被删的组")
	in := newPortableTestInbound(t, "甲", 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "只引用了一个组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{ghost.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := database.GetDB().Exec("DELETE FROM domain_groups WHERE id = ?", ghost.Id).Error; err != nil {
		t.Fatalf("delete group: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeRules)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 0 {
		t.Errorf("一个组都不剩的规则必须整条跳过，实际导出 %+v", f.Rules)
	}
}

// 手工录入的 IP 段与手工域名走同一条导出路径，往返后顺序与内容必须原样保留。
func TestExportImportRoundTripsCidrs(t *testing.T) {
	setupDB(t)
	encodedCidrs, err := EncodeCidrs([]string{"1.2.3.0/24", "geoip:cn"})
	if err != nil {
		t.Fatalf("EncodeCidrs: %v", err)
	}
	if err := (&DomainGroupService{}).Add(&model.DomainGroup{
		Remark: "g", Domains: `["domain:openai.com"]`, Cidrs: encodedCidrs,
	}); err != nil {
		t.Fatalf("add group: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.DomainGroups) != 1 {
		t.Fatalf("groups = %d, want 1", len(f.DomainGroups))
	}
	got := f.DomainGroups[0].Cidrs
	if len(got) != 2 || got[0] != "1.2.3.0/24" || got[1] != "geoip:cn" {
		t.Errorf("cidrs = %v", got)
	}
}

// 旧格式（没有 cidrs 字段）必须能导入，且行为与改动前一致：组落库后没有
// 任何 IP 段，而不是导入失败或整体报错。
func TestImportOldFormatWithoutCidrs(t *testing.T) {
	setupDB(t)
	// kind/version 是真实旧文件也会带的字段（导入侧对它们做硬校验），
	// 唯独没有 cidrs 键——这正是升级前导出的文件的真实形态。
	raw := `{"kind":"a-ui-routing-export","version":1,"scope":["domainGroups"],"domainGroups":[{"remark":"g","domains":["domain:openai.com"],"subscribeUrl":""}],"outbounds":[],"rules":[]}`
	report, err := (&RoutingPortableService{}).Import(raw)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.DomainGroups.Created != 1 {
		t.Fatalf("created = %d, want 1: %v", report.DomainGroups.Created, report.Messages)
	}
	g, err := (&DomainGroupService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	cidrs, err := DecodeCidrs(g[0].Cidrs)
	if err != nil {
		t.Fatalf("DecodeCidrs: %v", err)
	}
	if len(cidrs) != 0 {
		t.Errorf("cidrs = %v, want empty", cidrs)
	}
}

// 不导出订阅拉来的 IP 段：单个组可达十几万条，且它是本机这一次拉取的状态，
// 搬过去会显示一个假的「刚刚更新」——与 TestExportOmitsSubscribedDomains 同理。
func TestExportOmitsSubscribedCidrs(t *testing.T) {
	setupDB(t)
	if err := (&DomainGroupService{}).Add(&model.DomainGroup{
		Remark: "g", Domains: "[]", Cidrs: "[]",
		SubscribeUrl:    "https://example.com/x.list",
		SubscribedCidrs: `["9.9.9.0/24"]`,
	}); err != nil {
		t.Fatalf("add group: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw := exportJSON(t, f)
	if strings.Contains(raw, "9.9.9.0/24") {
		t.Errorf("export must not contain subscribed cidrs: %s", raw)
	}
}
