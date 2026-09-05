# P3：面板内置 DNS 设置 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让管理员在面板里配置一组 DNS 服务器，接管 VPS 上那次域名解析，而不必手改一旦写错就全员断网的 xray 模板。

**Architecture:** 新增设置项 `dnsServers`（换行分隔，**非空即启用**，为空则一个字节都不改）。新建 `DNSInjector`，在生成期做两件事：写 `dns.servers`（末尾保证有 `localhost` 兜底），以及给数组首位的默认 freedom 出站加 `domainStrategy: "UseIP"`——不做后者，`dns` 段对直连流量完全是空转。一切在生成期注入，绝不写回 `xrayTemplateConfig`。

**Tech Stack:** Go 1.27，标准库 `encoding/json` / `net` / `strings`；Vue 2 + ant-design-vue 服务端模板；测试 `go test`，门禁 `make verify`。

**Spec:** `docs/superpowers/specs/2026-09-05-routing-ip-and-dns-design.md`（本计划实现其 §8，事实依据见 §2 的 F8/F9/F10/F11）

## Global Constraints

- **`dnsServers` 为空 = 模板完全不动。** 升级后行为零变化。不额外加布尔开关——一个配了却静默不生效的设置项，比没有这个设置项更糟。
- **只用 `UseIP` 系列，永不用 `ForceIP` 系列。** `transport/internet/config.go:13` 的矩阵里 `UseIP*` 是 strategy code 1，`proxy/freedom/freedom.go:298` 只在 `ForceIP()`（code 2）时把解析失败变成连接失败；`UseIP` 下解析失败只记日志、回落按域名直连。这一条把「DNS 配错等于全员断网」整个消掉，是本功能敢做的前提。**改成 `ForceIP` 会让一次上游抖动打穿所有用户。**
- **`outbounds[0]` 不是 `freedom` 时不动它，记 Warning。** 管理员可能改过模板。只有 freedom 认 `domainStrategy`。
- **一切在生成期注入，绝不写回 `xrayTemplateConfig`。** 写回的话，回退到旧二进制后模板里还留着 `UseIP` 与 `dns` 段，而旧代码不知道它们从哪来、也不会清理。
- **生成逐字节确定。** `servers` 数组保持管理员录入的顺序（DNS 有优先级语义，排序会改变行为）；`encoding/json` 对 map key 排序，加键不破坏确定性。
- **新增设置项必须改 5 处**：`defaultValueMap` + `entity.AllSetting` + `entity.CheckValid` + getter + `web/assets/js/model/models.js` 的 `AllSetting` 构造函数。漏掉最后一处会让**整个保存配置接口失败**，且报错只指向新字段。
- **UI 文案不得写成「防止 DNS 泄露」。** 这个拓扑下用户的 DNS 查询不经过 VPS（客户端自己解析，隧道里发来的是连接目标）。本功能改善的是服务端那次解析，必须如实这么说。
- **提交前门禁是 `make verify`**（vet + test + build）。

---

### Task 1: `dnsServers` 设置项

**Files:**
- Modify: `web/service/setting.go:25-47`（`defaultValueMap`）、getter 区
- Modify: `web/entity/entity.go:30-59`（`AllSetting`）、`CheckValid`
- Modify: `web/assets/js/model/models.js:177-209`（`AllSetting` 构造函数）
- Modify: `web/html/xui/setting.html`
- Test: `web/service/setting_defaults_test.go`、`web/service/setting_baseline_test.go` 所在包的设置用例

**Interfaces:**
- Produces: `SettingService.GetDNSServers() (string, error)`；`entity.AllSetting.DNSServers string`；包内私有 `checkDNSServer(item string) error`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/setting_defaults_test.go`：

```go
func TestDNSServersDefaultsToEmpty(t *testing.T) {
	setupDB(t)
	all, err := (&SettingService{}).GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.DNSServers != "" {
		t.Errorf("DNSServers = %q, want empty", all.DNSServers)
	}
}

func TestCheckValidAcceptsSupportedDNSForms(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = "8.8.8.8\n1.1.1.1:53\nlocalhost\n" +
		"udp://223.5.5.5\ntcp://223.5.5.5\ntls://8.8.8.8\n" +
		"https://8.8.8.8/dns-query\nh2c://223.5.5.5/dns-query\nquic://8.8.8.8\n" +
		"2001:4860:4860::8888"
	if err := all.CheckValid(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckValidRejectsBareDomainDNS(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = "dns.google"
	if err := all.CheckValid(); err == nil {
		t.Error("expected error for a bare domain: xray needs a scheme or an IP")
	}
}

func TestCheckValidRejectsSchemeWithoutHost(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = "https://"
	if err := all.CheckValid(); err == nil {
		t.Error("expected error for a scheme with no host")
	}
}

// 空值必须放行：这是「不启用」的正常状态。
func TestCheckValidAcceptsEmptyDNSServers(t *testing.T) {
	all := validBaseSetting()
	all.DNSServers = ""
	if err := all.CheckValid(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
```

（`validBaseSetting()` 是 `web/service/setting_baseline_test.go:12` 里既有的辅助函数，返回一份能通过 `CheckValid` 的 `*entity.AllSetting`，不接受参数。）

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestDNSServers|TestCheckValid.*DNS' -v`
Expected: 编译失败，`all.DNSServers undefined`。

- [ ] **Step 3: 加设置项（5 处）**

3.1 `web/service/setting.go` 的 `defaultValueMap`，在 `"ipRuleResolveDomain"` 之后加一行：

```go
	"dnsServers":               "",
```

3.2 同文件的 getter 区追加：

```go
// GetDNSServers 返回管理员配置的 DNS 服务器原文（换行分隔）。
//
// 空字符串表示不启用：此时 DNSInjector 一个字节都不改，xray 会用它自己的
// 默认解析器 localdns（core/xray.go:216），即系统的 /etc/resolv.conf。
func (s *SettingService) GetDNSServers() (string, error) {
	return s.getString("dnsServers")
}
```

3.3 `web/entity/entity.go` 的 `AllSetting`，在 `IPRuleResolveDomain` 之后加：

```go
	DNSServers string `json:"dnsServers" form:"dnsServers"`
```

3.4 同文件，在 `CheckValid` 之外新增一个包内私有的校验函数（放在 `checkIPDBSourceUrl` 附近）：

```go
// dnsServerSchemes 是 xray 的 dns.servers 支持的地址前缀。
var dnsServerSchemes = []string{
	"udp://", "tcp://", "tls://", "https://", "h2c://", "quic://",
}

// checkDNSServer 只查语法，不测可达性。
//
// 可达性交给运行时：配错的最坏后果已经被「只用 UseIP 系列」兜住——解析
// 失败时 freedom 回落按域名直连（proxy/freedom/freedom.go:298 只在
// ForceIP() 时才把失败变成断连），而路由侧的 IPIfNonMatch 解析失败也只是
// IP 规则不命中（features/routing/dns/context.go:21）。在保存这一刻做网络
// 探测，换来的是一次网络抖动就把管理员挡在门外。
//
// 裸域名（dns.google）拒绝：xray 要先解析这个域名本身才能用它，而此时还
// 没有可用的解析器，是个鸡生蛋问题。IP 型端点（https://8.8.8.8/dns-query）
// 零 bootstrap 依赖，是唯一稳妥的写法。
func checkDNSServer(item string) error {
	if item == "localhost" {
		return nil
	}
	for _, scheme := range dnsServerSchemes {
		if !strings.HasPrefix(item, scheme) {
			continue
		}
		if len(item) == len(scheme) {
			return common.NewError("DNS 服务器地址缺少主机名:", item)
		}
		return nil
	}
	host := item
	if h, _, err := net.SplitHostPort(item); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		return common.NewError("DNS 服务器地址不支持:", item,
			"——应为 IP（8.8.8.8）、IP:端口、localhost，或 "+
				strings.Join(dnsServerSchemes, " ")+"开头的地址。"+
				"域名型端点（dns.google）不支持：xray 要先解析它本身才能用它")
	}
	return nil
}
```

3.5 同文件 `CheckValid`，在 `IPRuleResolveDomain` 校验之后加：

```go
	// 空表示不启用，是正常状态。
	for _, line := range strings.Split(s.DNSServers, "\n") {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		if err := checkDNSServer(item); err != nil {
			return err
		}
	}
```

`web/entity/entity.go` 已经 import 了 `net` 与 `strings`，不需要加 import。

3.6 `web/assets/js/model/models.js` 的 `AllSetting` 构造函数，在 `this.ipRuleResolveDomain = 0;` 之后加：

```javascript
        this.dnsServers = "";
```

- [ ] **Step 4: 加前端表单**

`web/html/xui/setting.html`，在「分流」相关的 `a-list`（`ipRuleResolveDomain` 那个开关所在处）之后追加：

```html
                                <setting-list-item type="textarea" title="DNS 服务器（一行一条）"
                                                   desc="留空表示不启用，由系统的 /etc/resolv.conf 解析。填写后，本机上 xray 的域名解析改走这里配置的服务器，云服务商的解析器不再看得到用户访问的域名。注意：这改善的是服务端那一次解析，客户端自己的 DNS 不经过本机，不受影响。推荐 IP 型端点（https://8.8.8.8/dns-query），域名型端点需要先解析自身、无法使用。列表末尾会自动补一个 localhost 兜底：配置的解析器全部不可用时退回系统解析，不会断网。改动需重启 xray（约 10 秒后自动完成）"
                                                   v-model="allSetting.dnsServers"></setting-list-item>
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestDNSServers|TestCheckValid' -v && go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add web/service/setting.go web/entity/entity.go web/assets/js/model/models.js web/html/xui/setting.html web/service/setting_defaults_test.go
git commit -m "feat(dns): 新增 dnsServers 设置项，非空即启用

只查语法不测可达性：配错的最坏后果已被「只用 UseIP 系列」兜住（解析失败
回落按域名直连），而在保存这一刻做网络探测，换来的是一次网络抖动就把
管理员挡在门外。

裸域名端点拒绝：xray 要先解析这个域名本身才能用它，是鸡生蛋。

按规范改满 5 处，含 models.js。UI 文案如实说明这改善的是服务端那次解析，
客户端 DNS 不经过本机。"
```

---

### Task 2: `DNSInjector`

**Files:**
- Create: `web/service/dns_inject.go`
- Modify: `web/service/xray.go:18-24`（`XrayService` 加字段）、`:79-81`（调用点）
- Test: `web/service/dns_inject_test.go`（新建）

**Interfaces:**
- Consumes: `SettingService.GetDNSServers()`（Task 1）、`xray.Config`、`json_util.RawMessage`
- Produces: `DNSInjector.Inject(cfg *xray.Config) error`、`ParseDNSServers(raw string) []string`

- [ ] **Step 1: 写失败的测试**

新建 `web/service/dns_inject_test.go`：

**复用同包既有的辅助函数，不要新造**：`newTemplateConfig(t)`（`routing_inject_test.go:24`）
与 `decodeOutbounds(t, cfg)`（`routing_inject_test.go:33`）已经存在于 `package service`，
在新文件里重新定义 `decodeOutbounds` 会直接编译失败（同包重复声明）。


```go
package service

import (
	"encoding/json"
	"testing"
)

// 不配置 = 一个字节都不改。升级后行为零变化靠的就是这一条。
func TestDNSInjectorNoopWhenUnset(t *testing.T) {
	setupDB(t)
	cfg := newTemplateConfig(t)
	beforeDNS := string(cfg.DNSConfig)
	beforeOut := string(cfg.OutboundConfigs)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if string(cfg.DNSConfig) != beforeDNS {
		t.Errorf("DNSConfig changed: %q -> %q", beforeDNS, cfg.DNSConfig)
	}
	if string(cfg.OutboundConfigs) != beforeOut {
		t.Errorf("OutboundConfigs changed")
	}
}

func TestDNSInjectorWritesServersAndFallback(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "https://8.8.8.8/dns-query\n1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var dns struct {
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(cfg.DNSConfig, &dns); err != nil {
		t.Fatalf("unmarshal dns: %v", err)
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
}

// 管理员自己写了 localhost 就不再补一个：重复项无害但会让界面与配置对不上。
func TestDNSInjectorDoesNotDuplicateFallback(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "localhost\n1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var dns struct {
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(cfg.DNSConfig, &dns); err != nil {
		t.Fatalf("unmarshal dns: %v", err)
	}
	count := 0
	for _, s := range dns.Servers {
		if s == "localhost" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("localhost appears %d times in %v, want 1", count, dns.Servers)
	}
}

// 不设这一项，dns 段对直连流量完全是空转：freedom 只在
// domainStrategy.HasStrategy() 为真时才走 xray 的内置 DNS 客户端。
func TestDNSInjectorSetsFreedomDomainStrategy(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	first := decodeOutbounds(t, cfg)[0]
	if first["domainStrategy"] != "UseIP" {
		t.Errorf("domainStrategy = %v, want UseIP", first["domainStrategy"])
	}
}

// 管理员改过模板、首位不是 freedom 时不动它。只有 freedom 认 domainStrategy。
func TestDNSInjectorLeavesNonFreedomDefaultOutboundAlone(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	cfg.OutboundConfigs = []byte(`[{"protocol":"socks","tag":"custom","settings":{}}]`)
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	first := decodeOutbounds(t, cfg)[0]
	if _, ok := first["domainStrategy"]; ok {
		t.Errorf("must not set domainStrategy on a non-freedom outbound: %v", first)
	}
}

// 生成必须逐字节确定，否则 Config.Equals 恒为 false，10 秒的重启 cron
// 会不停重启 xray。
func TestDNSInjectorIsDeterministic(t *testing.T) {
	setupDB(t)
	if err := (&SettingService{}).setString("dnsServers", "https://8.8.8.8/dns-query\n1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	first := newTemplateConfig(t)
	second := newTemplateConfig(t)
	if err := (&DNSInjector{}).Inject(first); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := (&DNSInjector{}).Inject(second); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if string(first.DNSConfig) != string(second.DNSConfig) {
		t.Errorf("dns not byte-identical:\n%s\n%s", first.DNSConfig, second.DNSConfig)
	}
	if string(first.OutboundConfigs) != string(second.OutboundConfigs) {
		t.Errorf("outbounds not byte-identical")
	}
}

func TestParseDNSServersTrimsAndDedupes(t *testing.T) {
	got := ParseDNSServers("  1.1.1.1  \n\n1.1.1.1\n 8.8.8.8 \n")
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "8.8.8.8" {
		t.Errorf("got = %v, want [1.1.1.1 8.8.8.8]", got)
	}
}
```

（若 `SettingService` 没有导出/可用的 `setString`，改用该包既有测试里写设置的写法。）

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestDNSInjector|TestParseDNSServers' -v`
Expected: 编译失败，`undefined: DNSInjector`。

- [ ] **Step 3: 新建 `web/service/dns_inject.go`**

```go
package service

import (
	"encoding/json"
	"strings"

	"a-ui/logger"
	"a-ui/util/json_util"
	"a-ui/xray"
)

// dnsFallbackServer 是保证出现在 servers 列表里的兜底解析器。
// xray 的 "localhost" 表示走系统解析器（/etc/resolv.conf）。
//
// 存在的理由：管理员配置的解析器全部不可用时，退化成「和不配这个功能一样」，
// 而不是断网。没有它的话，一次上游 DoH 故障就是全员解析失败。
const dnsFallbackServer = "localhost"

// freedomDomainStrategy 只能是 UseIP 系列，绝不能改成 ForceIP 系列。
//
// transport/internet/config.go:13 的矩阵里 UseIP* 是 strategy code 1、
// ForceIP* 是 code 2；proxy/freedom/freedom.go:298 只在 ForceIP() 为真时
// 把解析失败变成连接失败，UseIP 下只记日志并回落按域名直连。
// 这一条把「DNS 配错等于全员断网」整个消掉，是本功能敢做的前提。
const freedomDomainStrategy = "UseIP"

// DNSInjector 在管理员配置了 DNS 服务器时，接管 xray 的 dns 段与默认出站的
// domainStrategy。
//
// 必须排在 RoutingInjector.Inject 之后调用：那一步会把整个 outbounds 数组
// 反序列化再重新序列化，并由 tagDefaultOutbound 保证首个出站存在且带 tag；
// 本注入器只往那个对象上加一个键，不必自己处理数组不存在的情况。
type DNSInjector struct {
	settingService SettingService
}

// Inject 列表为空时一个字节都不改——升级后行为零变化靠的就是这一条。
func (s *DNSInjector) Inject(cfg *xray.Config) error {
	raw, err := s.settingService.GetDNSServers()
	if err != nil {
		return err
	}
	servers := ParseDNSServers(raw)
	if len(servers) == 0 {
		return nil
	}

	// 保证列表里有系统解析器兜底。管理员自己写了就不再补——他把它放在
	// 第一位是有意为之（优先系统解析），补第二个只会让界面与配置对不上。
	hasFallback := false
	for _, item := range servers {
		if item == dnsFallbackServer {
			hasFallback = true
			break
		}
	}
	if !hasFallback {
		servers = append(servers, dnsFallbackServer)
	}

	// 顺序原样保留：DNS 有优先级语义，排序会改变行为。
	encoded, err := json.Marshal(map[string]any{"servers": servers})
	if err != nil {
		return err
	}
	cfg.DNSConfig = json_util.RawMessage(encoded)

	return s.applyFreedomStrategy(cfg)
}

// applyFreedomStrategy 给数组首位的默认出站加上 domainStrategy。
//
// 不做这一步，dns 段对直连流量完全是空转：freedom 只在自己的
// domainStrategy.HasStrategy() 为真时才调 internet.LookupForIP
// （proxy/freedom/freedom.go:290），而只有那个函数打的是 xray 的内置 DNS
// 客户端（transport/internet/dialer.go:87）；默认的 AsIs 走系统解析器。
//
// 手写模板的人几乎不会知道这一点，这正是本功能相对「自己往模板里塞一段
// dns」的主要价值。
func (s *DNSInjector) applyFreedomStrategy(cfg *xray.Config) error {
	outbounds := make([]any, 0)
	if len(cfg.OutboundConfigs) > 0 {
		if err := json.Unmarshal(cfg.OutboundConfigs, &outbounds); err != nil {
			return err
		}
	}
	if len(outbounds) == 0 {
		logger.Warning("dns servers configured but the generated config has no outbound; " +
			"direct traffic keeps using the system resolver")
		return nil
	}
	first, ok := outbounds[0].(map[string]any)
	if !ok || first == nil {
		logger.Warning("dns servers configured but the first outbound is not an object; " +
			"direct traffic keeps using the system resolver")
		return nil
	}
	// 管理员可能改过模板，把首位换成别的协议。只有 freedom 认 domainStrategy，
	// 给别的协议加这个键要么被忽略、要么让整份配置非法——后者会全员断网。
	if first["protocol"] != "freedom" {
		logger.Warning("dns servers configured but the default outbound is not freedom (protocol:",
			first["protocol"], "); direct traffic keeps using the system resolver")
		return nil
	}
	first["domainStrategy"] = freedomDomainStrategy

	encoded, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encoded)
	return nil
}

// ParseDNSServers 把 textarea 原文切成有序、去重的服务器列表。
//
// 只做切分与去重，语法校验在 entity.CheckValid（保存那一刻）：生成期再拒绝
// 已经落库的值，只会让整份配置生成失败、xray 保持旧配置，而管理员在界面上
// 看不到任何线索。
func ParseDNSServers(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
```

- [ ] **Step 4: 接进配置合成**

`web/service/xray.go`，`XrayService` 结构体加字段：

```go
	dnsInjector     DNSInjector
```

在 `GetXrayConfig` 里 `routingInjector.Inject(xrayConfig)` 那个 if 块之后插入：

```go
	// 必须排在 routingInjector 之后：那一步重写了整个 outbounds 数组，
	// 本注入器要往它的首位加 domainStrategy。
	if err := s.dnsInjector.Inject(xrayConfig); err != nil {
		return nil, err
	}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestDNSInjector|TestParseDNSServers|TestGetXrayConfig' -v`
Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add web/service/dns_inject.go web/service/dns_inject_test.go web/service/xray.go
git commit -m "feat(dns): 新增 DNSInjector，写 dns 段并联动 freedom 的 UseIP

只加 dns 段是空转：freedom 只在 domainStrategy.HasStrategy() 为真时才走
xray 的内置 DNS 客户端（freedom.go:290 → dialer.go:87），默认 AsIs 走
系统解析器。这正是本功能相对「自己往模板里塞一段 dns」的主要价值。

固定用 UseIP 而非 ForceIP：解析失败时回落按域名直连而不是断连接
（config.go:13 的矩阵 + freedom.go:298）。servers 末尾保证有 localhost。

排在 RoutingInjector 之后：那一步重写整个 outbounds 数组。"
```

---

### Task 3: e2e 与门禁

**Files:**
- Modify: `web/service/routing_e2e_test.go`

- [ ] **Step 1: 加 e2e 用例**

照该文件既有用例的写法（用真实 xray 跑 `run -test`），加一份「配置了 dnsServers 之后生成的完整配置」的验证，断言 `Configuration OK`。沿用该文件既有的「找不到 xray 二进制就 `t.Skip` 并说明原因」的守卫写法——不要让缺二进制表现成失败。

这一条 e2e 是本期最重要的一道防线：`dns` 段与 freedom 的 `domainStrategy` 都是「写错了 xray 直接拒绝加载整份配置」的位置，而面板首页在那种情况下仍会显示 `running`。

- [ ] **Step 2: 跑 e2e**

Run: `go test ./web/service/ -run 'E2E' -v`
Expected: PASS，或在没有 `bin/xray-darwin-arm64` 的机器上明确 SKIP 并打印原因。

- [ ] **Step 3: 跑完整门禁**

Run: `make verify`
Expected: vet 无输出、全部包测试 PASS、build 成功。失败时读错误、修问题、重跑，不跳过、不粉饰。

- [ ] **Step 4: 手工验收**

Run: `XUI_DEBUG=true go run main.go`（必须在仓库根目录）

在「面板设置」填 `https://8.8.8.8/dns-query`，保存；等约 10 秒后确认 xray 重启且面板可用。用 `pgrep -a xray` 确认进程活着——**不要相信面板首页的 `running`**：`Process.Start()` 把 `cmd.Run()` 丢进 goroutine 后直接返回 nil，xray 启动失败不会回传到面板。再把该项清空，确认恢复原状。

- [ ] **Step 5: 检查最终 diff**

Run: `git diff main...HEAD --stat && git status --short`
Expected: 只有本计划列出的文件，工作区干净，没有调试残留与临时文件。

- [ ] **Step 6: 提交**

```bash
git add web/service/routing_e2e_test.go
git commit -m "test(dns): e2e 覆盖配置了 dnsServers 之后的完整配置"
```
