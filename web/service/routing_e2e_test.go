package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// assertRealXrayAccepts 把一份配置交给真实 xray 判定，且**不做 fail open**。
// runXrayTest 的 fail open 是给生产路径用的（校验器故障不该锁住管理员），
// 测试里必须相反：拿不到判定就要让测试失败，否则这条覆盖会静默失效。
func assertRealXrayAccepts(t *testing.T, data []byte) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "a-ui-e2e-*.json")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatalf("write config: %v", err)
	}
	file.Close()

	output, runErr := exec.Command(xray.GetBinaryPath(), "run", "-test", "-c", file.Name()).CombinedOutput()
	if runErr != nil && !strings.Contains(string(output), "Configuration OK") {
		t.Fatalf("real xray rejected the generated config: %v\n%s\n--- config ---\n%s",
			runErr, output, data)
	}
}

// newE2EInbound 建一个真实 xray 会接受的入站。GenXrayInboundConfig 原样透传
// Settings/StreamSettings/Sniffing 三段 JSON，所以这里必须给出真实内容——
// VLESS 少了 decryption:"none" 会被 xray 直接拒绝。
func newE2EInbound(t *testing.T, port int, uuid string) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true,
		Tag:            "inbound-" + strconv.Itoa(port),
		Settings:       `{"clients":[{"id":"` + uuid + `"}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       `{"enabled":true,"destOverride":["http","tls"]}`,
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	return in
}

// newE2EGroup 建一个域名组，域名按录入顺序序列化——顺序是生成确定性的前提。
func newE2EGroup(t *testing.T, remark string, domains ...string) *model.DomainGroup {
	t.Helper()
	encoded, err := EncodeDomains(domains)
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	g := &model.DomainGroup{Remark: remark, Domains: encoded}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("add group %q: %v", remark, err)
	}
	return g
}

// newE2EGroupWithCidrs 建一个域名组，同时（可选）带手工域名与 IP 段。
// newE2EGroup 只覆盖纯域名场景，这里补上本期新增的 IP 段路径——domains
// 与 cidrs 任一个可以传 nil，用来构造「只有 IP、没有域名」这类场景。
func newE2EGroupWithCidrs(t *testing.T, remark string, domains, cidrs []string) *model.DomainGroup {
	t.Helper()
	encodedDomains, err := EncodeDomains(domains)
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	encodedCidrs, err := EncodeCidrs(cidrs)
	if err != nil {
		t.Fatalf("EncodeCidrs: %v", err)
	}
	g := &model.DomainGroup{Remark: remark, Domains: encodedDomains, Cidrs: encodedCidrs}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("add group %q: %v", remark, err)
	}
	return g
}

// 这是本子系统此前唯一缺失的覆盖：没有任何测试把**完整生成配置**交给真实 xray。
// C1（保留 tag 撞名）正是因此才能通过全部审查——孤立校验发现不了组合冲突。
//
// 覆盖的是设计文档 §2「完整目标形态」那一行：模板 + 多入站 + 多出站 +
// 全局 block + 按用户 proxy，一次性交给真实 xray 判定。
func TestGeneratedConfigIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	// 原始需求场景：甲、乙两个用户访问同一批 ChatGPT 域名走不同落地节点，
	// 另有一批违规域名对所有人封禁。
	jia := newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	yi := newE2EInbound(t, 10002, "b831381d-6324-4d53-ad4f-8cda48b30812")

	chatgpt := newE2EGroup(t, "ChatGPT", "geosite:openai", "domain:openai.com")
	banned := newE2EGroup(t, "违规域名", "domain:example-banned.com")

	nodeService := OutboundNodeService{}
	nodeB, err := nodeService.AddFromLink("socks5://alice:secret@1.2.3.4:1080", "B 节点 香港")
	if err != nil {
		t.Fatalf("add node B: %v", err)
	}
	nodeC, err := nodeService.AddFromLink("socks5://5.6.7.8:1080", "C 节点 东京")
	if err != nil {
		t.Fatalf("add node C: %v", err)
	}

	ruleService := RoutingRuleService{}
	for _, rule := range []*model.RoutingRule{
		{Remark: "全员封禁违规域名", InboundIds: "[]", DomainGroupId: banned.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{banned.Id}),
			Action: model.ActionBlock, Priority: 0, Enable: true},
		{Remark: "甲的 ChatGPT 走 B", InboundIds: mustEncodeIds(t, []int{jia.Id}), DomainGroupId: chatgpt.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{chatgpt.Id}),
			Action: model.ActionProxy, OutboundId: nodeB.Id, Priority: 1, Enable: true},
		{Remark: "乙的 ChatGPT 走 C", InboundIds: mustEncodeIds(t, []int{yi.Id}), DomainGroupId: chatgpt.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{chatgpt.Id}),
			Action: model.ActionProxy, OutboundId: nodeC.Id, Priority: 2, Enable: true},
	} {
		if err := ruleService.Add(rule); err != nil {
			t.Fatalf("add rule %q: %v", rule.Remark, err)
		}
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)
}

// 脏数据不该让整份配置作废：任何一条残缺的规则或损坏的节点都必须被跳过，
// 剩下的配置仍然要是一份真实 xray 接受的合法配置。
func TestGeneratedConfigStaysValidWithDirtyData(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	in := newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	group := newE2EGroup(t, "ChatGPT", "geosite:openai")

	// 绕过 service 直接写库，模拟直接改库/并发删除/历史脏数据留下的残骸
	db := database.GetDB()
	for _, node := range []*model.OutboundNode{
		{Tag: model.BlockOutboundTag, Remark: "撞名保留 tag", Protocol: "socks", Enable: true,
			Config: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`},
		{Tag: "a-ui-null", Remark: "配置为 null", Protocol: "socks", Enable: true, Config: "null"},
		{Tag: "a-ui-broken", Remark: "配置损坏", Protocol: "socks", Enable: true, Config: "{not json"},
	} {
		if err := db.Save(node).Error; err != nil {
			t.Fatalf("save dirty node %q: %v", node.Tag, err)
		}
	}
	for _, rule := range []*model.RoutingRule{
		{Remark: "引用不存在的域名组", DomainGroupId: 999, DomainGroupIds: mustEncodeGroupIds(t, []int{999}), Action: model.ActionBlock, Enable: true},
		{Remark: "引用不存在的出站", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionProxy,
			OutboundId: 999, Enable: true},
		{Remark: "引用不存在的入站", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), InboundIds: "[999]",
			Action: model.ActionBlock, Enable: true},
		{Remark: "动作未知", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: "definitely-not-an-action", Enable: true},
		{Remark: "唯一一条好规则", DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), InboundIds: mustEncodeIds(t, []int{in.Id}),
			Action: model.ActionBlock, Enable: true},
	} {
		if err := db.Save(rule).Error; err != nil {
			t.Fatalf("save dirty rule %q: %v", rule.Remark, err)
		}
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)

	// 反向确认：那条唯一合法的规则确实还在，否则本测试可能只是因为
	// 什么都没生成而通过，从而失去区分力。
	rules := decodeRules(t, cfg)
	found := false
	for _, r := range rules {
		if r["outboundTag"] == model.BlockOutboundTag {
			found = true
		}
	}
	if !found {
		t.Error("the one well-formed rule disappeared; the test would pass vacuously")
	}
}

// 直接复刻事故路径：管理员把出站节点备注命名为「block」。
// 修复前 allocTag 会分配到 a-ui-block，与注入器始终注入的黑洞出站撞名，
// 生成的配置里出现两个 a-ui-block，xray 报 "existing tag found" 并拒绝启动，
// 全员断网，而面板首页仍显示 running、errorMsg 为空。
//
// CheckXrayRunningJob 每 30 秒重试都撞同一个冲突，不会自愈；Tag 设计上不可变，
// 唯一补救是禁用或删除该节点。所以这条路径必须在分配阶段就走不通。
func TestNodeRemarkedBlockStillProducesAValidConfig(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "block")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if node.Tag == model.BlockOutboundTag {
		t.Fatalf("node was given the reserved tag %q", node.Tag)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)

	// 这个节点必须真的进了配置，否则测试只是因为它被跳过而通过
	if !strings.Contains(string(data), node.Tag) {
		t.Errorf("node tag %q is missing from the generated config", node.Tag)
	}
}

// containsString 判断一条已解码规则里某个条件字段是否含目标值。
//
// decodeRules 把配置整体解成 map[string]any，其中的条件数组解码后是
// []any（元素为 string），不是 []string——对着 any 直接断言成 []string
// 会 panic。这里显式按 []any 遍历再逐个断言成 string。
func containsString(field any, want string) bool {
	arr, ok := field.([]any)
	if !ok {
		return false
	}
	for _, v := range arr {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

// 本期新增：一个组同时挂域名与 IP 时，注入器必须拆成两条独立的 xray 规则。
// 这不是单元测试就能完全覆盖的地方——buildRule 里的注释已经指出，若退化成
// 把两个条件塞进同一条规则，同一条规则内的条件是 AND，「这批域名或这批 IP
// 走 B」会变成「域名命中且解析出的 IP 也命中」，几乎永不命中；而这仍然是一份
// 语法合法的配置，真实 xray 只会判定 Configuration OK，不会有任何报错替
// 管理员发现这个退化。
func TestGeneratedConfigWithMixedDomainAndIPGroupIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	in := newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	group := newE2EGroupWithCidrs(t, "混合组", []string{"domain:openai.com"}, []string{"1.2.3.0/24"})

	rule := &model.RoutingRule{
		Remark: "混合组封禁", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)

	// 反向确认：确实生成了两条独立规则（一条 domain、一条 ip），且没有任何
	// 一条同时携带两个键——这正是本用例要守住的不变量，不能只靠「xray 接受」
	// 就满足，退化成单条 AND 规则同样会被 xray 判定合法。
	//
	// 判定「ip 规则确实生成了」不能只看是否存在某条带 ip 键的规则：模板
	// 自带一条屏蔽私网的 ip 规则（config.json 里的 geoip:private），任何时候
	// 都会存在，不能证明本组的 IP 条件真的被注入了。必须找本组的具体值。
	var domainRule, ipRule map[string]any
	for _, r := range decodeRules(t, cfg) {
		_, hasDomain := r["domain"]
		_, hasIP := r["ip"]
		if hasDomain && hasIP {
			t.Fatalf("一条规则同时带 domain 与 ip 条件（AND 语义，几乎永不命中）: %v", r)
		}
		if containsString(r["domain"], "domain:openai.com") {
			domainRule = r
		}
		if containsString(r["ip"], "1.2.3.0/24") {
			ipRule = r
		}
	}
	if domainRule == nil {
		t.Fatalf("没有找到携带 domain:openai.com 的 domain 规则: %v", decodeRules(t, cfg))
	}
	if ipRule == nil {
		t.Fatalf("没有找到携带 1.2.3.0/24 的 ip 规则: %v", decodeRules(t, cfg))
	}
}

// 本期新增：一个组只有 IP、没有域名。domains 长度为 0 不该连带把 IP 条件也
// 跳过——buildRule 对两类条件分别判空，但只有真实 xray 才能确认最终生成的
// ip 规则本身是一份 xray 认识的合法配置（例如 xray 原生的 geoip:xx 语法）。
func TestGeneratedConfigWithIPOnlyGroupIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	in := newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	group := newE2EGroupWithCidrs(t, "纯 IP 组", nil, []string{"1.2.3.0/24", "geoip:cn"})

	rule := &model.RoutingRule{
		Remark: "纯 IP 组封禁", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action:         model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)

	// 反向确认：ip 规则确实生成了，否则本测试可能只是因为规则被整条跳过
	// （域名组「全空」的另一条防线误伤了纯 IP 组）而通过，从而失去区分力。
	//
	// 必须找本组的具体值，不能只看是否存在某条带 ip 键的规则：模板自带一条
	// 屏蔽私网的 ip 规则（config.json 里的 geoip:private），它与本组的 IP
	// 条件是否被注入毫无关系。
	var ipRule map[string]any
	for _, r := range decodeRules(t, cfg) {
		if containsString(r["ip"], "1.2.3.0/24") {
			ipRule = r
			if _, hasDomain := r["domain"]; hasDomain {
				t.Errorf("纯 IP 组不该生成带 domain 条件的规则: %v", r)
			}
		}
	}
	if ipRule == nil {
		t.Fatal("没有找到携带 1.2.3.0/24 的 ip 规则；纯 IP 组的规则可能被静默跳过了")
	}
	if !containsString(ipRule["ip"], "geoip:cn") {
		t.Errorf("ip 规则应同时含组内第二个 CIDR geoip:cn: %v", ipRule)
	}
}

// 本期新增设置项 ipRuleResolveDomain：开启后生成期要写
// routing.domainStrategy = "IPIfNonMatch"。这是三个新增点里最需要真实 xray
// 判定的一个——domainStrategy 是一个 xray 按精确字符串匹配的枚举值，拼错
// 或拼进一个 xray 不认识的取值会让整份配置被拒绝，而这在 Go 侧的字符串比较
// 或 JSON 断言里完全看不出来。
func TestGeneratedConfigWithIPRuleResolveDomainIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	if err := (&SettingService{}).setInt("ipRuleResolveDomain", 1); err != nil {
		t.Fatalf("setInt(ipRuleResolveDomain): %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}

	var routing struct {
		DomainStrategy string `json:"domainStrategy"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
		t.Fatalf("decode routing: %v", err)
	}
	if routing.DomainStrategy != "IPIfNonMatch" {
		t.Fatalf("domainStrategy = %q, want IPIfNonMatch", routing.DomainStrategy)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)
}

// 本期新增设置项 dnsServers：非空时接管 dns 段并给默认出站的 settings 写
// domainStrategy。这条 e2e 覆盖的是「形状是否被真实 xray 接受」，不是「键
// 写在哪一层」——后者已由 dns_inject_test.go 的
// TestDNSInjectorSetsFreedomDomainStrategy(ThroughGetXrayConfig) 断言死了。
// 之所以两者都要留：DNSInjector 的第一版实现把 domainStrategy 写在了
// outbound 对象本身而不是它的 settings 里，而 xray 的 infra/conf 从不用
// DisallowUnknownFields，多出来的键被静默丢弃，run -test 照样判
// Configuration OK——单靠真实 xray 这一关，那个 bug 会被放过。
func TestGeneratedConfigWithDNSServersIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	// 一个 IP 形态的 DoH 端点 + 一个裸 IP，覆盖 servers 数组里不止一种写法。
	if err := (&SettingService{}).setString("dnsServers", "https://8.8.8.8/dns-query\n1.1.1.1"); err != nil {
		t.Fatalf("setString(dnsServers): %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}

	// 断言 dns 段确实生成且带上了兜底的 localhost——纯做「真实 xray 接受」
	// 的检查发现不了「整段 dns 没写出来」这类回归：一份完全没有 dns 段的
	// 配置同样会被 xray 判定 Configuration OK。
	var dns struct {
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(cfg.DNSConfig, &dns); err != nil {
		t.Fatalf("decode dns: %v", err)
	}
	want := []string{"https://8.8.8.8/dns-query", "1.1.1.1", "localhost"}
	if len(dns.Servers) != len(want) {
		t.Fatalf("servers = %v, want %v", dns.Servers, want)
	}
	for i := range want {
		if dns.Servers[i] != want[i] {
			t.Errorf("servers[%d] = %q, want %q", i, dns.Servers[i], want[i])
		}
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)
}

// dnsServers 与 ipRuleResolveDomain 是本期同时新增的两个设置项，各自往配置
// 的不同角落写一个同名字段：前者写 outbounds[0].settings.domainStrategy
// （UseIP），后者写 routing.domainStrategy（IPIfNonMatch）。分开测都已被
// 上面两条 e2e 与 dns_inject_test.go 的单测覆盖；这条补的是两者同时打开、
// 走同一条 GetXrayConfig 流水线时，注入顺序不会互相覆盖或串位——例如若
// DNSInjector 或 RoutingInjector 未来被改成共享同一个 map 再各自写键，两个
// 同名字段就可能被写到同一个对象上，产生一份语法合法但语义错误、又只有真实
// xray 才可能拒绝或管理员事后才能发现的配置。
func TestGeneratedConfigWithDNSServersAndIPRuleResolveDomainIsAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	newE2EInbound(t, 10001, "b831381d-6324-4d53-ad4f-8cda48b30811")
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString(dnsServers): %v", err)
	}
	if err := (&SettingService{}).setInt("ipRuleResolveDomain", 1); err != nil {
		t.Fatalf("setInt(ipRuleResolveDomain): %v", err)
	}

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}

	var routing struct {
		DomainStrategy string `json:"domainStrategy"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
		t.Fatalf("decode routing: %v", err)
	}
	if routing.DomainStrategy != "IPIfNonMatch" {
		t.Fatalf("routing.domainStrategy = %q, want IPIfNonMatch", routing.DomainStrategy)
	}

	first := decodeOutbounds(t, cfg)[0]
	settings, ok := first["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing or not an object: %v", first)
	}
	if settings["domainStrategy"] != "UseIP" {
		t.Errorf("outbounds[0].settings.domainStrategy = %v, want UseIP", settings["domainStrategy"])
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	assertRealXrayAccepts(t, data)
}
