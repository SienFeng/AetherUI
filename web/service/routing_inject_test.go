package service

import (
	"bytes"
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
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
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
	rule := &model.RoutingRule{DomainGroupId: 999, DomainGroupIds: mustEncodeGroupIds(t, []int{999}), Action: model.ActionBlock, Enable: true}
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
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
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
		DomainGroupId: chatgpt.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{chatgpt.Id}), Action: model.ActionProxy, OutboundId: node.Id,
		Priority: 1, Enable: true,
	}); err != nil {
		t.Fatalf("Add proxy rule: %v", err)
	}
	if err := rs.Add(&model.RoutingRule{
		DomainGroupId: banned.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{banned.Id}), Action: model.ActionBlock, Priority: 99, Enable: true,
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
		InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
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
		InboundIds: mustEncodeIds(t, []int{in.Id}), DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
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
	node, _ := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "hk")
	rs := RoutingRuleService{}
	// 五条规则挂五个不同的域名组：同一个域名组下每个入站至多被一条规则覆盖，
	// 而本测试关心的是多条规则的生成顺序是否逐字节稳定。
	for i := 0; i < 5; i++ {
		g := newTestGroup(t, "组 "+strconv.Itoa(i))
		if err := rs.Add(&model.RoutingRule{
			DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
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
	rule := &model.RoutingRule{DomainGroupId: empty.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{empty.Id}), Action: model.ActionBlock, Enable: true}
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
		InboundIds: mustEncodeIds(t, []int{in.Id}), DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
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
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
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
		{"域名组不存在", &model.RoutingRule{DomainGroupId: 999, DomainGroupIds: mustEncodeGroupIds(t, []int{999}), Action: model.ActionBlock}},
		{"域名列表为空", &model.RoutingRule{DomainGroupId: emptyGroup.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{emptyGroup.Id}), Action: model.ActionBlock}},
		{"入站不存在", &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), InboundIds: "[999]", Action: model.ActionBlock}},
		{"出站不存在", &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionProxy, OutboundId: 999}},
		{"未知动作", &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: "definitely-not-an-action"}},
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

func TestBuildRuleMergesManualAndSubscribedDomains(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark:            "国内",
		Domains:           `["domain:my-nas.local"]`,
		SubscribedDomains: `["domain:qq.com","domain:163.com"]`,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	rules := decodeRules(t, cfg)
	last := rules[len(rules)-1]
	domains, ok := last["domain"].([]any)
	if !ok {
		t.Fatalf("domain is not an array: %#v", last["domain"])
	}
	want := []string{"domain:my-nas.local", "domain:qq.com", "domain:163.com"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domain[%d] = %v, want %q (手工在前、订阅在后)", i, domains[i], want[i])
		}
	}
}

func TestBuildRuleWorksWithOnlySubscribedDomains(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "纯订阅", Domains: "[]", SubscribedDomains: `["domain:qq.com"]`,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 2 {
		t.Fatalf("规则应当生成: %v", rules)
	}
}

// 只有订阅、而订阅从未成功拉取过的组，合并结果为空，规则必须整条跳过。
func TestBuildRuleSkipsWhenBothSourcesEmpty(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "空", Domains: "[]", SubscribedDomains: "",
		SubscribeUrl: "http://example.com/list",
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 1 {
		t.Errorf("模板里那条 api 规则之外不应有别的: %v", rules)
	}
}

// 生成必须逐字节确定，否则 Config.Equals 恒为 false，
// 每 10 秒的重启 cron 会不停重启 xray。
func TestInjectIsByteDeterministicWithSubscribedDomains(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark:            "国内",
		Domains:           `["domain:b.com","domain:a.com"]`,
		SubscribedDomains: `["domain:d.com","domain:c.com","domain:a.com"]`,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	first := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(first); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for i := 0; i < 20; i++ {
		again := newTemplateConfig(t)
		if err := (&RoutingInjector{}).Inject(again); err != nil {
			t.Fatalf("Inject %d: %v", i, err)
		}
		if string(again.RouterConfig) != string(first.RouterConfig) {
			t.Fatalf("run %d differs:\n%s\n%s", i, first.RouterConfig, again.RouterConfig)
		}
	}
}

// mustEncodeIds 是测试里的小工具，避免每处都写一遍错误处理。
func mustEncodeIds(t *testing.T, ids []int) string {
	t.Helper()
	encoded, err := EncodeInboundIds(ids)
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	return encoded
}

func TestInjectMultiInboundRuleListsAllTagsInOrder(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundIds:    mustEncodeIds(t, []int{b.Id, a.Id}), // 故意逆序传入
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	generated := decodeRules(t, cfg)[1]
	tags, ok := generated["inboundTag"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("inboundTag = %v, want 2 tags", generated["inboundTag"])
	}
	// 升序存储 -> 生成顺序确定。顺序一抖动 Config.Equals 恒为 false，
	// 那个 10 秒的 cron 会不停重启 xray。
	if tags[0] != a.Tag || tags[1] != b.Tag {
		t.Errorf("inboundTag = %v, want [%s %s]", tags, a.Tag, b.Tag)
	}
}

func TestInjectMultiInboundRuleDropsDeadInboundsButKeepsRule(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	alive := newTestInbound(t, 10001)
	dead := newTestInbound(t, 10002)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundIds:    mustEncodeIds(t, []int{alive.Id, dead.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 禁用其中一个入站：剩下的用户仍应受规则约束
	dead.Enable = false
	if err := database.GetDB().Save(dead).Error; err != nil {
		t.Fatalf("disable inbound: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	generated := decodeRules(t, cfg)[1]
	tags, ok := generated["inboundTag"].([]any)
	if !ok || len(tags) != 1 || tags[0] != alive.Tag {
		t.Errorf("inboundTag = %v, want [%s]", generated["inboundTag"], alive.Tag)
	}
}

// 本次改动最重要的一条测试。
//
// 实测（Xray 26.7.28）确认 inboundTag: [] 的语义是「不限制」而非「不匹配
// 任何入站」：两个入站访问目标域名都被规则命中，对照域名正常放行。所以
// 一条指定入站全部失效的规则，若退而求其次输出空数组，就会从「只覆盖甲」
// 放大成「劫持所有人的这批域名」，而 xray 报 Configuration OK、面板首页
// 照样显示 running，无任何报错。宁可整条丢弃。
func TestInjectSkipsRuleWhenAllInboundsAreGone(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		InboundIds:    mustEncodeIds(t, []int{in.Id}),
		DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	in.Enable = false
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("disable inbound: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, rule := range decodeRules(t, cfg) {
		if tags, ok := rule["inboundTag"].([]any); ok && len(tags) == 0 {
			t.Fatalf("generated a rule with an empty inboundTag: %v", rule)
		}
		if rule["outboundTag"] == model.BlockOutboundTag {
			t.Fatalf("rule whose inbounds are all gone must be dropped entirely: %v", rule)
		}
	}
}

func TestInjectSkipsRuleWithCorruptInboundIds(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	r := &model.RoutingRule{
		InboundIds: "[]", DomainGroupId: g.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}), Action: model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 绕过 service 直接写坏数据，模拟手工改库 / 历史脏数据
	if err := database.GetDB().Model(model.RoutingRule{}).Where("id = ?", r.Id).
		Update("inbound_ids", "{not json").Error; err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, rule := range decodeRules(t, cfg) {
		if rule["outboundTag"] == model.BlockOutboundTag {
			t.Fatalf("rule with corrupt inbound ids must be dropped: %v", rule)
		}
	}
}

// xray 只有在出站带 tag 时才会在访问日志里输出 "[入站 -> 出站]"。模板里的
// freedom 出站是裸的、没有 tag，于是所有走默认出站（直连）的记录都不带方括号，
// 被 accesslog.ParseLine 当作无法归属的行丢弃——访问日志里只剩下命中分流规则
// 的流量，管理员看到的是一份沉默地缺了一大半的记录。
func TestInjectTagsDefaultOutboundSoAccessLogCanAttributeIt(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	if obs[0]["protocol"] != "freedom" {
		t.Fatalf("first outbound = %v, want freedom (it must stay xray's default outbound)", obs[0]["protocol"])
	}
	if obs[0]["tag"] != model.DefaultOutboundTag {
		t.Errorf("default outbound tag = %v, want %s", obs[0]["tag"], model.DefaultOutboundTag)
	}
}

// 用户自己给默认出站起了 tag 时不能覆盖：他的路由规则可能正引用着那个 tag，
// 改掉会让规则指向不存在的出站，而 xray 对悬空 outboundTag 不报错，只会静默直连。
func TestInjectKeepsExistingDefaultOutboundTag(t *testing.T) {
	setupDB(t)
	cfg := &xray.Config{}
	tmpl := `{
	  "outbounds": [
	    {"protocol":"freedom","settings":{},"tag":"my-direct"},
	    {"protocol":"blackhole","settings":{},"tag":"blocked"}
	  ],
	  "routing": {"rules": []}
	}`
	if err := json.Unmarshal([]byte(tmpl), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	if obs[0]["tag"] != "my-direct" {
		t.Errorf("default outbound tag = %v, want my-direct (must not be overwritten)", obs[0]["tag"])
	}
}

// 模板把 outbounds 写成空数组时，首位会是注入器自己生成的出站，它们本来就有
// tag，不该被再补一次。
func TestInjectDoesNotTagGeneratedOutboundsAsDefault(t *testing.T) {
	setupDB(t)
	cfg := &xray.Config{}
	if err := json.Unmarshal([]byte(`{"outbounds":[],"routing":{"rules":[]}}`), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	for _, ob := range obs {
		if ob["tag"] == model.DefaultOutboundTag {
			t.Errorf("generated outbound was tagged %s: %v", model.DefaultOutboundTag, ob)
		}
	}
}

// a-ui-default 必须和 a-ui-block 一样是保留 tag：备注写 "default" 会让
// SuggestTag 生成同名 tag，与注入器补上的默认出站撞名，xray 报
// "existing tag found" 并拒绝启动——全员断网，面板首页却仍显示 running。
func TestDefaultOutboundTagIsReserved(t *testing.T) {
	if !model.IsReservedTag(model.DefaultOutboundTag) {
		t.Errorf("IsReservedTag(%s) = false, want true", model.DefaultOutboundTag)
	}
}

// newTestGroupWithDomains 建一个域名指定的组，供跨组合并用例使用。
func newTestGroupWithDomains(t *testing.T, remark string, domains []string) *model.DomainGroup {
	t.Helper()
	encoded, err := EncodeDomains(domains)
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: remark, Domains: encoded}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("Add group: %v", err)
	}
	return g
}

// firstRuleDomains 从生成的配置里取第一条路由规则的 domain 列表。
func firstRuleDomains(t *testing.T, cfg *xray.Config) []string {
	t.Helper()
	var router struct {
		Rules []struct {
			Domain []string `json:"domain"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &router); err != nil {
		t.Fatalf("unmarshal router: %v", err)
	}
	for _, r := range router.Rules {
		if len(r.Domain) > 0 {
			return r.Domain
		}
	}
	t.Fatal("生成的配置里没有带 domain 的规则")
	return nil
}

func TestBuildRuleMergesDomainsAcrossGroupsInOrder(t *testing.T) {
	setupDB(t)
	claude := newTestGroupWithDomains(t, "Claude", []string{"domain:claude.ai", "domain:shared.example"})
	chatgpt := newTestGroupWithDomains(t, "ChatGPT", []string{"domain:shared.example", "domain:openai.com"})
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark:         "两组合并",
		InboundIds:     mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{chatgpt.Id, claude.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	got := firstRuleDomains(t, cfg)
	// 顺序由 DomainGroupIds 的升序决定（claude.Id < chatgpt.Id），
	// 组内保持录入顺序，跨组保留首次出现。
	want := []string{"domain:claude.ai", "domain:shared.example", "domain:openai.com"}
	if len(got) != len(want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("domains = %v, want %v", got, want)
		}
	}
}

// spec §2.2：部分组失效剔除，剩下的照常生效。整条丢弃对 block 规则尤其
// 糟糕——本该封禁的域名会全部裸奔。
func TestBuildRuleDropsEmptyGroupButKeepsTheRest(t *testing.T) {
	setupDB(t)
	claude := newTestGroupWithDomains(t, "Claude", []string{"domain:claude.ai"})
	// DomainGroupService.Add 只是 db.Save，不校验域名，可以直接建一个空组。
	empty := newTestGroupWithDomains(t, "空组", []string{})
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark:         "一好一空",
		InboundIds:     mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id, empty.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	got := firstRuleDomains(t, cfg)
	if len(got) != 1 || got[0] != "domain:claude.ai" {
		t.Errorf("domains = %v, want [domain:claude.ai]（空组剔除，好组保留）", got)
	}
}

func TestBuildRuleDropsWholeRuleWhenAllGroupsEmpty(t *testing.T) {
	setupDB(t)
	// 先用非空域名建组并加规则（validate 只校验组存在，不校验组非空），
	// 再把组清空，造出「规则引用的唯一一组变空了」这个真实状态。
	empty := newTestGroupWithDomains(t, "空组", []string{"domain:placeholder.example"})
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark:         "全空",
		InboundIds:     mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{empty.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := database.GetDB().Exec(
		"UPDATE domain_groups SET domains = '[]' WHERE id = ?", empty.Id).Error; err != nil {
		t.Fatalf("empty the group: %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	var router struct {
		Rules []struct {
			Domain      []string `json:"domain"`
			OutboundTag string   `json:"outboundTag"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &router); err != nil {
		t.Fatalf("unmarshal router: %v", err)
	}
	// 只检查 outboundTag 为 model.BlockOutboundTag 的规则——那才是本用例这条
	// block 规则若被误生成会带的 tag。模板自带的 api/私网/BT 三条静态规则
	// 天然就有 inboundTag 而无 domain（它们本就不是域名规则），若按
	// inboundTag 是否非空来判断会把它们也算作误报，与本用例要验证的问题无关。
	for _, r := range router.Rules {
		if r.OutboundTag == model.BlockOutboundTag && len(r.Domain) == 0 {
			t.Fatal("绝不能输出 domain 为空的规则：xray 会把它当作「不限制」，" +
				"规则从「这批域名走 B」退化成「该用户全部流量走 B」")
		}
	}
}

// DomainGroupIds 为 "[]" 是迁移对脏数据（domain_group_id <= 0）唯一会产出的
// 形态，也是 validate 挡不住的形态（直接改库、并发写入都能造出来）。这一支
// 与「组存在但域名为空」是 buildRule 里两道不同的检查，必须各测各的。
func TestBuildRuleDropsRuleWithEmptyDomainGroupIds(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	// 绕过 service 直接写库：Add 的 validate 会拒绝空的域名组集合，
	// 而这里要造的正是「绕过了 validate 的脏数据」。
	err := database.GetDB().Exec(`INSERT INTO routing_rules
		(remark, inbound_ids, domain_group_id, domain_group_ids, action, outbound_id, priority, enable)
		VALUES ('脏数据', ?, 0, '[]', 'block', 0, 0, 1)`, mustEncodeIds(t, []int{in.Id})).Error
	if err != nil {
		t.Fatalf("insert dirty rule: %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	var router struct {
		Rules []struct {
			Domain      []string `json:"domain"`
			OutboundTag string   `json:"outboundTag"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &router); err != nil {
		t.Fatalf("unmarshal router: %v", err)
	}
	// 同上一条用例：只认 outboundTag 为 model.BlockOutboundTag 的规则，
	// 避开模板自带的 api/私网/BT 三条静态规则（它们有 inboundTag 但无
	// domain，与本用例要验证的问题无关）。
	for _, r := range router.Rules {
		if r.OutboundTag == model.BlockOutboundTag && len(r.Domain) == 0 {
			t.Fatal("DomainGroupIds 为 [] 的规则必须整条丢弃，绝不能输出 domain 为空的规则")
		}
	}
}

// 「生成逐字节确定」是 Config.Equals 能正确判断配置是否变化的前提；
// 顺序一抖动，那个 10 秒的重启 cron 会不停重启 xray。
func TestBuildRuleGenerationIsByteDeterministic(t *testing.T) {
	setupDB(t)
	a := newTestGroupWithDomains(t, "A", []string{"domain:a1.example", "domain:a2.example"})
	b := newTestGroupWithDomains(t, "B", []string{"domain:b1.example"})
	c := newTestGroupWithDomains(t, "C", []string{"domain:c1.example"})
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark:         "三组",
		InboundIds:     mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{c.Id, a.Id, b.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	first, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("first GetXrayConfig: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := (&XrayService{}).GetXrayConfig()
		if err != nil {
			t.Fatalf("GetXrayConfig #%d: %v", i, err)
		}
		if !bytes.Equal(first.RouterConfig, again.RouterConfig) {
			t.Fatalf("生成不确定，第 %d 次与首次不同:\n%s\n%s",
				i, first.RouterConfig, again.RouterConfig)
		}
	}
}

// 一个组同时有域名与 IP 时，必须生成两条独立的 xray 规则。
//
// 绝不能合成一条：同一条规则内的条件是 AND（app/router/config.go:33 的
// BuildCondition + condition.go:35 的 ConditionChan.Apply），「这批域名或这批
// IP 走 B」会变成「域名命中且解析出的 IP 也命中」，几乎永不命中，
// 而 xray 返回 Configuration OK、面板首页照样显示 running。
func TestBuildRuleSplitsDomainAndIP(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "混合", Domains: `["domain:openai.com"]`, Cidrs: `["1.2.3.0/24"]`,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3（模板 1 条 + 生成 2 条）: %v", len(rules), rules)
	}
	if _, ok := rules[1]["domain"]; !ok {
		t.Errorf("rules[1] 应当带 domain 条件: %v", rules[1])
	}
	if _, ok := rules[2]["ip"]; !ok {
		t.Errorf("rules[2] 应当带 ip 条件: %v", rules[2])
	}
}

// 不变量 N1：任何一条生成的规则都不得同时含 domain 与 ip。
func TestBuildRuleNeverCombinesDomainAndIP(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "混合", Domains: `["domain:openai.com"]`,
		Cidrs: `["1.2.3.0/24","geoip:cn"]`,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for i, r := range decodeRules(t, cfg) {
		_, hasDomain := r["domain"]
		_, hasIP := r["ip"]
		if hasDomain && hasIP {
			t.Errorf("rules[%d] 同时带 domain 与 ip（AND 语义，几乎永不命中）: %v", i, r)
		}
	}
}

// 不变量 N2：绝不发出空的条件数组。空数组在 protobuf 里长度为 0，
// 条件整个消失，规则退化成只由剩下的条件约束——范围被静默放大。
func TestBuildRuleNeverEmitsEmptyConditionArray(t *testing.T) {
	setupDB(t)
	for _, f := range []struct {
		remark  string
		domains string
		cidrs   string
	}{
		{"只有域名", `["domain:openai.com"]`, "[]"},
		{"只有 IP", "[]", `["1.2.3.0/24"]`},
	} {
		t.Run(f.remark, func(t *testing.T) {
			setupDB(t)
			group := &model.DomainGroup{Remark: f.remark, Domains: f.domains, Cidrs: f.cidrs}
			if err := database.GetDB().Save(group).Error; err != nil {
				t.Fatalf("save group: %v", err)
			}
			rule := &model.RoutingRule{
				DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
				Action:         model.ActionBlock, Enable: true,
			}
			if err := database.GetDB().Save(rule).Error; err != nil {
				t.Fatalf("save rule: %v", err)
			}
			cfg := newTemplateConfig(t)
			if err := (&RoutingInjector{}).Inject(cfg); err != nil {
				t.Fatalf("Inject: %v", err)
			}
			for i, r := range decodeRules(t, cfg) {
				for _, key := range []string{"domain", "ip", "inboundTag"} {
					v, ok := r[key]
					if !ok {
						continue
					}
					list, isList := v.([]any)
					if isList && len(list) == 0 {
						t.Errorf("rules[%d][%q] 是空数组: %v", i, key, r)
					}
				}
			}
		})
	}
}

func TestBuildRuleOnlyDomainsProducesOneRule(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{Remark: "只有域名", Domains: `["domain:openai.com"]`, Cidrs: "[]"}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2（模板 1 条 + 生成 1 条）: %v", len(rules), rules)
	}
	if _, ok := rules[1]["ip"]; ok {
		t.Errorf("组里没有 IP 段时不得生成 ip 规则: %v", rules[1])
	}
}

func TestBuildRuleOnlyCidrsProducesOneRule(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{Remark: "只有 IP", Domains: "[]", Cidrs: `["1.2.3.0/24"]`}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2（模板 1 条 + 生成 1 条）: %v", len(rules), rules)
	}
	if _, ok := rules[1]["domain"]; ok {
		t.Errorf("组里没有域名时不得生成 domain 规则: %v", rules[1])
	}
}

// 域名与 IP 两侧都空 → 整条丢弃。宁可规则不生效让管理员察觉，
// 也绝不输出条件残缺的规则。
//
// 与既有的 TestBuildRuleSkipsWhenBothSourcesEmpty（:489，管的是「手工与订阅
// 两个来源都空」）是不同的判定，名字刻意区分开。
func TestBuildRuleSkipsWhenNeitherDomainsNorCidrs(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{Remark: "全空", Domains: "[]", Cidrs: "[]"}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	rule := &model.RoutingRule{
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := database.GetDB().Save(rule).Error; err != nil {
		t.Fatalf("save rule: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	if len(rules) != 1 {
		t.Errorf("模板里那条 api 规则之外不应有别的: %v", rules)
	}
}

// 默认关：升级后行为零变化，模板里没有 domainStrategy 就保持没有。
func TestInjectLeavesDomainStrategyAloneByDefault(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var routing map[string]any
	if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
		t.Fatalf("unmarshal routing: %v", err)
	}
	if _, ok := routing["domainStrategy"]; ok {
		t.Errorf("domainStrategy must not be written when the switch is off: %v", routing)
	}
}

func TestInjectWritesDomainStrategyWhenEnabled(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setInt("ipRuleResolveDomain", 1); err != nil {
		t.Fatalf("setInt: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var routing map[string]any
	if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
		t.Fatalf("unmarshal routing: %v", err)
	}
	if routing["domainStrategy"] != "IPIfNonMatch" {
		t.Errorf("domainStrategy = %v, want IPIfNonMatch", routing["domainStrategy"])
	}
}
