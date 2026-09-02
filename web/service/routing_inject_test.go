package service

import (
	"encoding/json"
	"strconv"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

const testTemplate = `{
  "outbounds": [
    {"protocol":"freedom","settings":{}},
    {"protocol":"blackhole","settings":{},"tag":"blocked"}
  ],
  "routing": {"rules": [
    {"type":"field","inboundTag":["api"],"outboundTag":"api"}
  ]}
}`

func newTemplateConfig(t *testing.T) *xray.Config {
	t.Helper()
	cfg := &xray.Config{}
	if err := json.Unmarshal([]byte(testTemplate), cfg); err != nil {
		t.Fatalf("unmarshal template: %v", err)
	}
	return cfg
}

func decodeOutbounds(t *testing.T, cfg *xray.Config) []map[string]any {
	t.Helper()
	var raw []map[string]any
	if err := json.Unmarshal(cfg.OutboundConfigs, &raw); err != nil {
		t.Fatalf("decode outbounds: %v", err)
	}
	return raw
}

func decodeRules(t *testing.T, cfg *xray.Config) []map[string]any {
	t.Helper()
	var routing struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
		t.Fatalf("decode routing: %v", err)
	}
	return routing.Rules
}

// newTestInbound 建一个启用的入站，tag 按现网规则由端口算出。
func newTestInbound(t *testing.T, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true,
		Tag:      "inbound-" + strconv.Itoa(port),
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	return in
}

func TestInjectAppendsBlockOutboundAndKeepsFreedomFirst(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	if obs[0]["protocol"] != "freedom" {
		t.Errorf("first outbound = %v, want freedom (it is xray's default outbound)", obs[0]["protocol"])
	}
	last := obs[len(obs)-1]
	if last["tag"] != model.BlockOutboundTag {
		t.Errorf("last outbound tag = %v, want %s", last["tag"], model.BlockOutboundTag)
	}
}

func TestInjectSkipsRuleWhenDomainGroupMissing(t *testing.T) {
	setupDB(t)
	// 直接建一条引用不存在域名组的规则，绕过 service 校验，模拟脏数据
	rule := &model.RoutingRule{DomainGroupId: 999, Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 1 {
		t.Fatalf("rule count = %d, want 1 (only the template rule); a rule with no domains "+
			"would hijack all traffic for its inbound", len(rules))
	}
}

func TestInjectSkipsProxyRuleWhenOutboundDisabled(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}
	node.Enable = false
	if err := (&OutboundNodeService{}).Update(node); err != nil {
		t.Fatalf("Update node: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := len(decodeRules(t, cfg)); got != 1 {
		t.Errorf("rule count = %d, want 1; rules pointing at a disabled outbound must be skipped", got)
	}
}

func TestInjectBlockRulesComeBeforeProxyRules(t *testing.T) {
	setupDB(t)
	banned := newTestGroup(t, "违规域名")
	chatgpt := newTestGroup(t, "ChatGPT")
	node, _ := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	rs := RoutingRuleService{}
	// 先插 proxy 规则，priority 更小，以证明排序不是靠插入顺序或 priority
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: chatgpt.Id, Action: model.ActionProxy, OutboundId: node.Id,
		Priority: 1, Enable: true,
	}); err != nil {
		t.Fatalf("Add proxy rule: %v", err)
	}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: banned.Id, Action: model.ActionBlock, Priority: 99, Enable: true,
	}); err != nil {
		t.Fatalf("Add block rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 3 {
		t.Fatalf("rule count = %d, want 3", len(rules))
	}
	if rules[1]["outboundTag"] != model.BlockOutboundTag {
		t.Errorf("rules[1] = %v, want the block rule (block must outrank proxy)", rules[1])
	}
	if rules[2]["outboundTag"] != node.Tag {
		t.Errorf("rules[2] = %v, want the proxy rule", rules[2])
	}
}

func TestInjectGlobalRuleOmitsInboundTag(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundId: 0, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	generated := rules[len(rules)-1]
	if _, exists := generated["inboundTag"]; exists {
		t.Errorf("global rule must not carry inboundTag, got %v", generated["inboundTag"])
	}
}

func TestInjectPerInboundRuleUsesCurrentTag(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundId: in.Id, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	generated := decodeRules(t, cfg)[1]
	tags, ok := generated["inboundTag"].([]any)
	if !ok || len(tags) != 1 || tags[0] != in.Tag {
		t.Errorf("inboundTag = %v, want [%s]", generated["inboundTag"], in.Tag)
	}
}

// 这是最关键的一条：生成必须逐字节稳定，否则 Config.Equals 恒为 false，
// 那个 10 秒的 cron 会不停重启 xray。
func TestInjectIsDeterministic(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, _ := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	rs := RoutingRuleService{}
	for i := 0; i < 5; i++ {
		if err := rs.Add(&model.RoutingRule{
			DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	first := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(first); err != nil {
		t.Fatalf("Inject #1: %v", err)
	}
	second := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(second); err != nil {
		t.Fatalf("Inject #2: %v", err)
	}
	if string(first.OutboundConfigs) != string(second.OutboundConfigs) {
		t.Error("outbounds are not byte-identical across two runs")
	}
	if string(first.RouterConfig) != string(second.RouterConfig) {
		t.Error("routing is not byte-identical across two runs")
	}
	if !first.Equals(second) {
		t.Error("Config.Equals must report the two generated configs as equal")
	}
}

// 域名组存在但域名列表为空：规则必须整条丢弃。
// 若退而求其次输出一条 domain 为空的规则，xray 会把「缺失的条件」当作「不限制」，
// 规则就从「访问这批域名走某节点」退化成「该入站全部流量走某节点」，且不报错。
func TestInjectSkipsRuleWhenDomainListEmpty(t *testing.T) {
	setupDB(t)
	empty := &model.DomainGroup{Remark: "empty", Domains: "[]"}
	if err := database.GetDB().Save(empty).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{DomainGroupId: empty.Id, Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := len(decodeRules(t, cfg)); got != 1 {
		t.Errorf("rule count = %d, want 1 (only the template rule)", got)
	}
}

// 规则指向的入站已被删除：规则必须整条丢弃。
// InboundService 没有引用检查，管理员通过正常界面就能删掉被规则引用的入站。
func TestInjectSkipsRuleWhenInboundDeleted(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundId: in.Id, DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 先确认规则本来是会生成的，否则本测试可能因为规则从未生成而假通过
	before := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(before); err != nil {
		t.Fatalf("Inject before: %v", err)
	}
	if got := len(decodeRules(t, before)); got != 2 {
		t.Fatalf("before deletion: rule count = %d, want 2", got)
	}

	if err := database.GetDB().Delete(model.Inbound{}, in.Id).Error; err != nil {
		t.Fatalf("delete inbound: %v", err)
	}

	after := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(after); err != nil {
		t.Fatalf("Inject after: %v", err)
	}
	if got := len(decodeRules(t, after)); got != 1 {
		t.Errorf("after deletion: rule count = %d, want 1 (the orphaned rule must be dropped)", got)
	}
}

// 节点的 Config 损坏：该节点不进 outbounds，引用它的规则也必须一并丢弃，
// 否则规则会带着一个未写入配置的 outboundTag，形成悬空引用。
func TestInjectSkipsRuleWhenOutboundConfigCorrupt(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 绕过服务层校验，直接把库里的 Config 弄坏
	if err := database.GetDB().Model(model.OutboundNode{}).
		Where("id = ?", node.Id).UpdateColumn("config", "{not json").Error; err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// 该节点不应出现在 outbounds 里
	for _, ob := range decodeOutbounds(t, cfg) {
		if ob["tag"] == node.Tag {
			t.Errorf("outbound %q was emitted despite a corrupt config", node.Tag)
		}
	}
	// 引用它的规则也不应出现——否则就是一个悬空 outboundTag，
	// xray 不会报错，流量会静默回落到默认出站（直连）。
	for _, r := range decodeRules(t, cfg) {
		if r["outboundTag"] == node.Tag {
			t.Errorf("rule referencing %q was emitted, leaving a dangling outboundTag", node.Tag)
		}
	}
}

// 历史脏数据：C1 修复之前分配到 a-ui-block 的节点仍可能躺在库里。生成端必须
// 同样排除保留 tag，否则输出两个 a-ui-block，xray 报 "existing tag found"
// 并拒绝启动 —— 而面板首页不会显示任何异常。
func TestInjectSkipsOutboundNodeCarryingReservedTag(t *testing.T) {
	setupDB(t)
	// 绕过 service 直接写库，模拟修复前留下的脏数据
	node := &model.OutboundNode{
		Tag: model.BlockOutboundTag, Remark: "block", Protocol: "socks", Enable: true,
		Config: `{"tag":"` + model.BlockOutboundTag + `","protocol":"socks",` +
			`"settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`,
	}
	if err := database.GetDB().Save(node).Error; err != nil {
		t.Fatalf("save node: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	count := 0
	for _, ob := range decodeOutbounds(t, cfg) {
		if ob["tag"] != model.BlockOutboundTag {
			continue
		}
		count++
		if ob["protocol"] != "blackhole" {
			t.Errorf("%s must be the injector's blackhole, got protocol %v",
				model.BlockOutboundTag, ob["protocol"])
		}
	}
	if count != 1 {
		t.Errorf("%s appears %d times in the generated outbounds, want exactly 1",
			model.BlockOutboundTag, count)
	}
}

// Config 为 "null" 时 json.Unmarshal 不报错却留下 nil map，紧接着的赋值 panic。
// 这条路径由每 10 秒的重启 cron 走到，而 cron 没有 panic 恢复 —— 整个面板进程会死。
func TestInjectSkipsOutboundNodeWithNullConfig(t *testing.T) {
	setupDB(t)
	node := &model.OutboundNode{
		Tag: "a-ui-null-node", Remark: "null", Protocol: "socks", Enable: true, Config: "null",
	}
	if err := database.GetDB().Save(node).Error; err != nil {
		t.Fatalf("save node: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if ob["tag"] == "a-ui-null-node" {
			t.Errorf("a node whose config is null must be skipped, got %v", ob)
		}
	}
}

// 设计 §5.3 接受「生成期跳过」这道防线的理由是「宁可规则不生效，用户能察觉」。
// 但跳过如果不说明原因，用户其实察觉不到：规则表照常渲染，配置里却没有这条规则。
// buildRule 必须回报跳过原因，由 buildRules 记进日志。
func TestBuildRuleReportsWhyItSkipped(t *testing.T) {
	setupDB(t)
	inj := &RoutingInjector{}

	group := newTestGroup(t, "ChatGPT")
	emptyGroup := &model.DomainGroup{Remark: "空组", Domains: "[]"}
	if err := database.GetDB().Save(emptyGroup).Error; err != nil {
		t.Fatalf("save empty group: %v", err)
	}

	cases := []struct {
		name string
		rule *model.RoutingRule
	}{
		{"域名组不存在", &model.RoutingRule{DomainGroupId: 999, Action: model.ActionBlock}},
		{"域名列表为空", &model.RoutingRule{DomainGroupId: emptyGroup.Id, Action: model.ActionBlock}},
		{"入站不存在", &model.RoutingRule{DomainGroupId: group.Id, InboundId: 999, Action: model.ActionBlock}},
		{"出站不存在", &model.RoutingRule{DomainGroupId: group.Id, Action: model.ActionProxy, OutboundId: 999}},
		{"未知动作", &model.RoutingRule{DomainGroupId: group.Id, Action: "definitely-not-an-action"}},
	}
	for _, tc := range cases {
		generated, _, skip := inj.buildRule(tc.rule, map[int]string{}, map[int]string{})
		if generated != nil {
			t.Errorf("%s: expected the rule to be skipped, got %v", tc.name, generated)
		}
		if skip == nil {
			t.Errorf("%s: the rule was skipped without reporting a reason", tc.name)
		}
	}
}
