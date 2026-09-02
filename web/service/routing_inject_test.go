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

func TestInjectSkipsRuleWithEmptyDomainGroup(t *testing.T) {
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
