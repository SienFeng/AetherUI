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
		Remark: "走香港", InboundIds: ids, DomainGroupId: g.Id,
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
		Remark: "走香港", InboundIds: ids, DomainGroupId: g.Id,
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
	if len(r.InboundRefs) != 1 {
		t.Fatalf("InboundRefs len = %d, want 1", len(r.InboundRefs))
	}
	if r.InboundRefs[0].Remark != "用户甲" || r.InboundRefs[0].Port != 2886 {
		t.Errorf("InboundRefs[0] = %+v", r.InboundRefs[0])
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
		Remark: "全局封禁", InboundIds: "[]", DomainGroupId: g.Id,
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
	if f.Rules[0].InboundRefs == nil {
		t.Error("全局规则的 InboundRefs 应是空切片而不是 nil —— nil 序列化成 null，导入端无法与「字段缺失」区分")
	}
	if len(f.Rules[0].InboundRefs) != 0 {
		t.Errorf("InboundRefs = %+v, want 空", f.Rules[0].InboundRefs)
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
		Remark: "走香港", InboundIds: "[]", DomainGroupId: g.Id,
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
		InboundRefs: []PortableInboundRef{
			{Remark: "用户甲", Port: 2886},
			{Remark: "对面才有的用户", Port: 9999},
		},
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
		InboundRefs: []PortableInboundRef{{Remark: "用户甲", Port: 2886}},
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
		InboundRefs: []PortableInboundRef{}, // 显式的「所有入站」
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
		InboundRefs: []PortableInboundRef{}, Action: model.ActionBlock, Enable: true,
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
