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
