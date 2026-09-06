# P2：分流组的 IP 段与 ip 规则 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让「域名组」升级成「分流组」——同一个组同时装域名与 IP 段，订阅一次同时喂两边；一条分流规则据此生成最多两条 xray 规则（一条 `domain`、一条 `ip`），并提供一个默认关闭的开关让 IP 规则也能匹配域名目标。

**Architecture:** `DomainGroup` 加 `Cidrs` / `SubscribedCidrs` 两列，与域名侧严格对称（手工与订阅物理隔离、各自只有一个写入方）。IP 段内联进配置，不写 dat 文件。`buildRule` 的返回值从「一条规则」改成「0~2 条规则」——同一条 xray 规则内 `domain` 与 `ip` 是 AND，绝不能并列。新增设置项 `ipRuleResolveDomain`，为 1 时生成期写 `routing.domainStrategy = "IPIfNonMatch"`。

**Tech Stack:** Go 1.27，GORM + SQLite（`AutoMigrate` 只加列不删列），Vue 2 + ant-design-vue 服务端模板；测试 `go test`，门禁 `make verify`。

**Spec:** `docs/superpowers/specs/2026-09-05-routing-ip-and-dns-design.md`（本计划实现其 §5、§6、§7，事实依据见 §2 的 F4/F5/F6/F7/F11/F12）

## Global Constraints

- **升级后默认行为零变化。** 不填 IP 段、不开开关 = 与现在完全一致。
- **绝不发出同时含 `domain` 与 `ip` 的规则（不变量 N1）。** 同一条规则内的条件是 AND（`app/router/config.go:33` + `app/router/condition.go:35`）。并列会让「这批域名**或**这批 IP 走 B」变成「域名命中**且**解析出的 IP 也命中」，几乎永不命中，而 xray 返回 `Configuration OK`、面板首页照样显示 `running`。
- **绝不发出 `ip: []`（不变量 N2）。** 空数组在 protobuf 里长度为 0，条件整个消失，规则退化成只由剩下的条件约束——范围被静默放大。与 `domain: []`、`inboundTag: []` 完全同构。
- **生成逐字节确定。** 跨组按 `DomainGroupIds` 升序、组内手工在前订阅在后、同一条数据库规则内 domain 规则在前 ip 规则在后。禁止遍历 map 产生数组顺序——顺序一抖动，`Config.Equals` 恒为 false，10 秒的重启 cron 会不停重启 xray。
- **`updateFieldsFor` 必须包含每一个要支持编辑的列。** 漏加会让该字段静默地无法通过编辑接口更新，而 `Get` 与展示完全正常。
- **校验一律 fail open。** `ValidateCidrs` 沿用 `validateWithFullConfig` 的三条边界（xray 自身故障、取不到完整配置、改动之前配置就已不合法），一条都不收紧。
- **新增设置项必须改 5 处**：`defaultValueMap` + `entity.AllSetting` + `entity.CheckValid` + getter + `web/assets/js/model/models.js` 的 `AllSetting` 构造函数。漏掉最后一处会让**整个保存配置接口失败**，且报错只指向新字段。
- **提交前门禁是 `make verify`**（vet + test + build）。

---

### Task 1: 数据列与 IP 段编解码

**Files:**
- Modify: `database/model/routing.go:35-56`（`DomainGroup` 加两列）
- Create: `web/service/routing_cidr.go`
- Modify: `web/service/routing_domain.go:123-146`（`updateFieldsFor`）
- Test: `web/service/routing_cidr_test.go`（新建）、`web/service/routing_domain_test.go`

**Interfaces:**
- Produces:
  - `ParseCidrs(raw string) ([]string, error)`
  - `EncodeCidrs(list []string) (string, error)`
  - `DecodeCidrs(encoded string) ([]string, error)`
  - `DecodeSubscribedCidrs(encoded string) ([]string, error)`
  - `isValidCIDR(s string) bool`（包内私有，Task 3 也要用）
  - `model.DomainGroup.Cidrs` / `.SubscribedCidrs`

- [ ] **Step 1: 写失败的测试**

新建 `web/service/routing_cidr_test.go`：

```go
package service

import (
	"strings"
	"testing"
)

func TestParseCidrsAcceptsXraySyntax(t *testing.T) {
	raw := "1.2.3.0/24\n8.8.8.8\n2001:db8::/32\n::1\ngeoip:cn\ngeoip:!cn\n!geoip:cn\next:geoip.dat:cn\next-ip:x.dat:tag"
	got, err := ParseCidrs(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 9 {
		t.Fatalf("len = %d, want 9: %v", len(got), got)
	}
	if got[0] != "1.2.3.0/24" || got[4] != "geoip:cn" {
		t.Errorf("got = %v", got)
	}
}

func TestParseCidrsRejectsDomains(t *testing.T) {
	_, err := ParseCidrs("openai.com")
	if err == nil {
		t.Fatal("expected error for a domain in the CIDR field")
	}
	if !strings.Contains(err.Error(), "域名") {
		t.Errorf("error should point the admin at the domain field, got: %v", err)
	}
}

func TestParseCidrsRejectsOutOfRangePrefix(t *testing.T) {
	if _, err := ParseCidrs("1.2.3.0/33"); err == nil {
		t.Error("expected error for IPv4 prefix > 32")
	}
	if _, err := ParseCidrs("2001:db8::/129"); err == nil {
		t.Error("expected error for IPv6 prefix > 128")
	}
}

func TestParseCidrsRejectsEmptyPrefixValue(t *testing.T) {
	if _, err := ParseCidrs("geoip:"); err == nil {
		t.Error("expected error for geoip: with no code")
	}
	if _, err := ParseCidrs("!"); err == nil {
		t.Error("expected error for a lone negation")
	}
}

func TestParseCidrsSkipsBlankLinesAndTrims(t *testing.T) {
	got, err := ParseCidrs("  1.2.3.0/24  \n\n\n  geoip:cn\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "1.2.3.0/24" || got[1] != "geoip:cn" {
		t.Errorf("got = %v", got)
	}
}

func TestParseCidrsRejectsEmptyResult(t *testing.T) {
	if _, err := ParseCidrs("  \n \n"); err == nil {
		t.Error("expected error for empty list")
	}
}

// nil 必须编码成 []，不能是 null：库里存 null 时列表页与导出侧要多一处
// 分支，而 [] 与「没有 IP 段」语义完全一致。
func TestEncodeCidrsNormalizesNil(t *testing.T) {
	got, err := EncodeCidrs(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "[]" {
		t.Errorf("got = %q, want []", got)
	}
}

// 没订阅过的组这一列本来就是空字符串，交给 DecodeCidrs 会得到 json 语法
// 错误，进而被 buildRule 当成「数据损坏」丢弃整条规则。
func TestDecodeSubscribedCidrsToleratesEmpty(t *testing.T) {
	got, err := DecodeSubscribedCidrs("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", got)
	}
}
```

追加到 `web/service/routing_domain_test.go`：

```go
// updateFieldsFor 的列名单是手工维护的。漏掉一个列会让该字段静默地无法
// 通过编辑接口更新，而 Get 与展示完全正常——极易漏测，所以单独钉一条。
func TestUpdateFieldsForIncludesCidrs(t *testing.T) {
	old := &model.DomainGroup{Id: 1, Remark: "a", Domains: "[]", Cidrs: "[]"}
	next := &model.DomainGroup{Id: 1, Remark: "a", Domains: "[]", Cidrs: `["1.2.3.0/24"]`}
	fields := updateFieldsFor(old, next)
	if fields["cidrs"] != `["1.2.3.0/24"]` {
		t.Errorf("cidrs = %v, want the new value", fields["cidrs"])
	}
}

// 订阅地址变了，旧地址拉来的 IP 段继续参与分流就是「用错误的数据生效」。
func TestUpdateFieldsForClearsSubscribedCidrsWhenUrlChanges(t *testing.T) {
	old := &model.DomainGroup{Id: 1, SubscribeUrl: "https://a.example/x"}
	next := &model.DomainGroup{Id: 1, SubscribeUrl: "https://b.example/y"}
	fields := updateFieldsFor(old, next)
	if fields["subscribed_cidrs"] != "" {
		t.Errorf("subscribed_cidrs = %v, want cleared", fields["subscribed_cidrs"])
	}
	if fields["subscribed_domains"] != "" {
		t.Errorf("subscribed_domains = %v, want cleared", fields["subscribed_domains"])
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestParseCidrs|TestEncodeCidrs|TestDecodeSubscribedCidrs|TestUpdateFieldsFor' -v`
Expected: 编译失败，`undefined: ParseCidrs`、`unknown field Cidrs in struct literal`。

- [ ] **Step 3: 加数据列**

`database/model/routing.go`，在 `DomainGroup` 的 `Domains` 字段之后插入：

```go
	// Cidrs 是管理员手工录入的 IP 段，JSON 字符串数组，元素为 xray 原生
	// ip 语法：1.2.3.0/24 / 8.8.8.8 / 2001:db8::/32 / geoip:cn / geoip:!cn /
	// ext:文件:标签。与 Domains 平行：一个组同时承载域名与 IP 两类条件，
	// 生成期各自成为一条独立的 xray 规则（绝不并列进同一条，那是 AND）。
	Cidrs string `json:"cidrs" form:"cidrs"`
```

在 `SubscribedDomains` 字段之后插入：

```go
	// SubscribedCidrs 是上一次成功拉取并解析出的 IP 段，JSON 字符串数组。
	// 与 Cidrs 物理隔离，理由与 SubscribedDomains 完全相同：两个字段各自
	// 只有一个写入方，永不交叉。
	SubscribedCidrs string `json:"subscribedCidrs" form:"subscribedCidrs"`
```

不需要写迁移函数：GORM 的 `AutoMigrate` 会加列，两列的零值（空字符串）由 `DecodeCidrs` / `DecodeSubscribedCidrs` 当作「没有 IP 段」处理。

- [ ] **Step 4: 新建 `web/service/routing_cidr.go`**

```go
package service

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"

	"a-ui/util/common"
)

// xray 支持的 IP 匹配前缀，见 common/geodata/rule_parser.go:16 ParseIPRules。
// geoip:xx 会被 xray 改写成 ext:geoip.dat:xx。
var cidrPrefixes = []string{"geoip:", "ext:", "ext-ip:"}

// ParseCidrs 把用户在 textarea 中一行一条录入的 IP 段解析成入库列表。
//
// 按输入行序输出，不排序：顺序是「生成逐字节确定」不变量的一部分。
func ParseCidrs(raw string) ([]string, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		normalized, err := normalizeCidrRule(item)
		if err != nil {
			return nil, err
		}
		list = append(list, normalized)
	}
	if len(list) == 0 {
		return nil, common.NewError("IP 段列表不能为空")
	}
	return list, nil
}

// normalizeCidrRule 校验一行录入，或说明它为什么不合法。
//
// 不改写内容：geoip 的 code 由 xray 自己 ToUpper（rule_parser.go:74），
// CIDR 也没有需要归一的大小写。这里只做「拦住明显不是 IP 段的东西」。
func normalizeCidrRule(item string) (string, error) {
	// 前导 ! 是取反，可叠加，作用于整条规则（rule_parser.go:49 cutReversePrefix）。
	body := strings.TrimLeft(item, "!")
	if body == "" {
		return "", common.NewError("IP 段不能只有取反符号:", item)
	}
	for _, p := range cidrPrefixes {
		if !strings.HasPrefix(body, p) {
			continue
		}
		if body[len(p):] == "" {
			return "", common.NewError("IP 段格式不支持，前缀后面没有内容:", item)
		}
		// 类别/文件是否真的存在，交给 ValidateCidrs 的真实 xray 判定：
		// geoip.dat 的内容会随面板「安装 xray」变化，在这里硬编码类别清单
		// 迟早与机器上那份 dat 对不上。
		return item, nil
	}
	if !isValidCIDR(body) {
		return "", common.NewError("IP 段格式不支持:", item,
			"——应为 CIDR（1.2.3.0/24）、单个 IP（8.8.8.8）、"+
				"geoip:cn / geoip:!cn，或 ext:文件:标签。域名请填在「手工域名」框里")
	}
	return item, nil
}

// isValidCIDR 镜像 xray 的 parseCIDR（common/geodata/rule_parser.go:102）：
// 允许裸 IP（等价 /32、/128），前缀长度不得超过地址族上限。
//
// 刻意不用 net.ParseCIDR：它拒绝裸 IP，也拒绝 1.2.3.4/24 这种主机位非零
// 的写法，而 xray 两者都接受。校验比 xray 更严会拦下合法配置。
func isValidCIDR(s string) bool {
	ipStr, prefixStr, hasPrefix := strings.Cut(s, "/")
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	maxPrefix := 128
	if ip.To4() != nil {
		maxPrefix = 32
	}
	if !hasPrefix {
		return true
	}
	n, err := strconv.Atoi(prefixStr)
	return err == nil && n >= 0 && n <= maxPrefix
}

// EncodeCidrs 把 IP 段列表序列化为入库格式。
//
// nil 归一成 []：json.Marshal 对 nil 切片产出 "null"，列表页与导出侧就要
// 多一处分支，而 [] 与「没有 IP 段」语义完全一致。
func EncodeCidrs(list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeCidrs 是 EncodeCidrs 的逆操作。
//
// 空字符串当作「没有 IP 段」：升级前建的组这一列就是空的，在这里报错会让
// buildRule 把引用它的规则整条丢弃——分流静默失效。真正的语法错误仍返回
// error，那是数据损坏，宁可让规则被丢弃也不能当成「没有条件」。
func DecodeCidrs(encoded string) ([]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(encoded), &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DecodeSubscribedCidrs 与 DecodeCidrs 行为一致，单独命名是为了让调用点
// 自解释——与 DecodeDomains / DecodeSubscribedDomains 的分工对称。
func DecodeSubscribedCidrs(encoded string) ([]string, error) {
	return DecodeCidrs(encoded)
}
```

- [ ] **Step 5: 扩展 `updateFieldsFor`**

`web/service/routing_domain.go`，把 `updateFieldsFor` 里的 `fields` 初始化与订阅清空块替换为：

```go
	fields := map[string]any{
		"remark":  next.Remark,
		"domains": next.Domains,
		"cidrs":   next.Cidrs,
	}

	// 订阅地址变了：旧订阅内容来自另一个来源，继续拿它分流是「用错误的数据
	// 生效」，比规则暂时不生效更危险。域名与 IP 两侧必须一起清，只清一侧会
	// 留下一个「域名是新地址的、IP 还是旧地址的」的混合体。
	if old.SubscribeUrl != next.SubscribeUrl {
		fields["subscribe_url"] = next.SubscribeUrl
		fields["subscribed_domains"] = ""
		fields["subscribed_cidrs"] = ""
		fields["last_updated_at"] = 0
		fields["last_error"] = ""
		fields["last_skipped"] = 0
	}
```

并在该函数的文档注释里，把「将来给 DomainGroup 加字段时……」那段保留原样——它正是本次要遵守的约束。

- [ ] **Step 6: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestParseCidrs|TestEncodeCidrs|TestDecodeSubscribedCidrs|TestUpdateFieldsFor|TestDomainGroupCRUD' -v`
Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add database/model/routing.go web/service/routing_cidr.go web/service/routing_cidr_test.go web/service/routing_domain.go web/service/routing_domain_test.go
git commit -m "feat(routing): 分流组加 Cidrs/SubscribedCidrs 两列与编解码

与域名侧严格对称：手工与订阅物理隔离、各自只有一个写入方、订阅地址变更
时两侧一起清空。isValidCIDR 镜像 xray 的 parseCIDR（允许裸 IP 与主机位
非零），刻意不用 net.ParseCIDR——比 xray 更严会拦下合法配置。

updateFieldsFor 同步加 cidrs：漏加会让该字段静默地无法通过编辑接口更新，
而 Get 与展示完全正常。"
```

---

### Task 2: `ValidateCidrs`

**Files:**
- Modify: `web/service/routing_validate.go`（在 `ValidateDomains` 之后新增）
- Test: `web/service/routing_validate_test.go`

**Interfaces:**
- Consumes: `validateWithFullConfig(apply func(cfg map[string]any), minimal map[string]any) error`（已存在）、`model.BlockOutboundTag`
- Produces: `ValidateCidrs(cidrs []string) error`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_validate_test.go`：

```go
// 空列表直接放行，绝不构造探针规则：ip 为空数组时那条探针只剩 outboundTag
// 一个非条件字段，xray 会报 "this rule has no effective fields"
// （app/router/config.go:114）而整份配置被判非法——一个「这个组没有 IP 段」
// 的正常状态会变成保存失败。
func TestValidateCidrsAllowsEmpty(t *testing.T) {
	if err := ValidateCidrs(nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := ValidateCidrs([]string{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateCidrsAcceptsValidList(t *testing.T) {
	setupDB(t)
	if err := ValidateCidrs([]string{"1.2.3.0/24", "geoip:private"}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestValidateCidrs' -v`
Expected: 编译失败，`undefined: ValidateCidrs`。

- [ ] **Step 3: 实现**

`web/service/routing_validate.go`，在 `ValidateDomains` 函数之后追加：

```go
// ValidateCidrs 校验 IP 段列表，能抓出不存在的 geoip 类别（checkFile 会打开
// dat 找 code，见 common/geodata/geodat_loader.go:16）与非法的 CIDR。
// 候选值挂在一条追加到末尾的探针规则上，出站指向注入器始终会注入的黑洞，
// 因此这条探针不会引入悬空引用。
//
// 空列表直接放行：ip 为空数组时探针只剩 outboundTag 一个非条件字段，
// xray 会报 "this rule has no effective fields" 而整份配置被判非法——
// 一个「这个组没有 IP 段」的正常状态会变成保存失败。
func ValidateCidrs(cidrs []string) error {
	if len(cidrs) == 0 {
		return nil
	}
	probe := map[string]any{
		"type": "field", "ip": cidrs, "outboundTag": model.BlockOutboundTag,
	}
	minimal := map[string]any{
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
		},
		"routing": map[string]any{
			"rules": []any{
				map[string]any{"type": "field", "ip": cidrs, "outboundTag": "direct"},
			},
		},
	}
	return validateWithFullConfig(func(cfg map[string]any) {
		routing, _ := cfg["routing"].(map[string]any)
		if routing == nil {
			routing = map[string]any{}
			cfg["routing"] = routing
		}
		rules, _ := routing["rules"].([]any)
		routing["rules"] = append(rules, probe)
	}, minimal)
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestValidateCidrs' -v`
Expected: PASS。（本机没有 `bin/xray-darwin-arm64` 时 `runXrayTest` 走 fail open 直接返回 nil，测试同样通过——这正是 fail open 的设计意图。）

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_validate.go web/service/routing_validate_test.go
git commit -m "feat(routing): 新增 ValidateCidrs，落库前过真实 xray

与 ValidateDomains 同构：探针规则追加到完整配置末尾，出站指向注入器始终
注入的黑洞。空列表直接放行——ip 为空数组时探针只剩 outboundTag 一个非条件
字段，xray 报 'this rule has no effective fields'，会把「这个组没有 IP 段」
这个正常状态变成保存失败。"
```

---

### Task 3: 订阅解析产出 IP 段

**Files:**
- Modify: `web/service/routing_subscription.go:30-118`（`ParseSubscription`、`convertSubscriptionLine`）
- Modify: `web/service/routing_domain.go:155-200`（`refreshLocked`）
- Test: `web/service/routing_subscription_test.go`

**Interfaces:**
- Consumes: `isValidCIDR`（Task 1）、`ValidateCidrs`（Task 2）、`EncodeCidrs`（Task 1）
- Produces: `ParseSubscription(raw string) (domains []string, cidrs []string, skipped int, err error)`（**签名改变**）；`convertSubscriptionLine(item string) (value string, isIP bool, ok bool)`（**签名改变**）

- [ ] **Step 1: 写失败的测试**

改 `web/service/routing_subscription_test.go` 里所有 `ParseSubscription` 调用点以匹配新签名（多接一个返回值），并追加：

```go
func TestParseSubscriptionCollectsIPRules(t *testing.T) {
	raw := `# comment
DOMAIN-SUFFIX,qq.com,DIRECT
IP-CIDR,1.1.1.0/24,PROXY,no-resolve
IP-CIDR6,2001:db8::/32,PROXY
GEOIP,CN,DIRECT
IP-ASN,20473,PROXY
SRC-IP-CIDR,192.168.1.0/24,DIRECT
PROCESS-NAME,Telegram,PROXY
`
	domains, cidrs, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("domains = %v", domains)
	}
	want := []string{"1.1.1.0/24", "2001:db8::/32", "geoip:cn"}
	if len(cidrs) != len(want) {
		t.Fatalf("cidrs = %v, want %v", cidrs, want)
	}
	for i := range want {
		if cidrs[i] != want[i] {
			t.Errorf("cidrs[%d] = %q, want %q", i, cidrs[i], want[i])
		}
	}
	// IP-ASN（xray 没有 ASN 匹配能力）、SRC-IP-CIDR（那是 source，按客户端 IP，
	// 塞进 ip 是语义错误）、PROCESS-NAME 三条必须跳过并计数。
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

// 纯 IP 列表（如中国 IP 段）从此是合法订阅源：改动前它会因「没解析出任何
// 域名」整份报错，而失败路径会保留上一次的数据，管理员看不出问题在哪。
func TestParseSubscriptionAcceptsPlainIPList(t *testing.T) {
	domains, cidrs, _, err := ParseSubscription("1.0.1.0/24\n1.0.2.0/23\n8.8.8.8\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 0 {
		t.Errorf("domains = %v, want empty", domains)
	}
	if len(cidrs) != 3 {
		t.Errorf("cidrs = %v, want 3 entries", cidrs)
	}
}

// 两侧都空才报错。报错是为了让调用方保留上一次成功的数据——上游改格式、
// URL 失效返回 404 页面、CDN 返回空响应都会走到这里。
func TestParseSubscriptionErrorsOnlyWhenBothEmpty(t *testing.T) {
	if _, _, _, err := ParseSubscription("IP-ASN,20473,PROXY\n"); err == nil {
		t.Error("expected error when nothing was parsed")
	}
}

func TestParseSubscriptionDedupesCidrs(t *testing.T) {
	_, cidrs, _, err := ParseSubscription("IP-CIDR,1.1.1.0/24,A\nIP-CIDR,1.1.1.0/24,B\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cidrs) != 1 {
		t.Errorf("cidrs = %v, want 1 entry", cidrs)
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestParseSubscription' -v`
Expected: 编译失败，`assignment mismatch: 4 variables but ParseSubscription returns 3 values`。

- [ ] **Step 3: 改 `ParseSubscription` 与 `convertSubscriptionLine`**

把 `web/service/routing_subscription.go` 里从注释块「逐行解析，不做全局格式识别」到 `convertSubscriptionLine` 结束的整段替换为：

```go
// 逐行解析，不做全局格式识别。Surge/Clash classical、Clash YAML、纯域名列表、
// 纯 IP 列表四种格式的行特征互不冲突，逐行判断比先猜格式更健壮：真实订阅
// 文件里混着注释、YAML 头和规则行，全局识别一旦猜错就整份文件解析失败。
//
// 域名与 IP 分别收集：一个分流组同时承载两类条件，生成期各自成为一条独立的
// xray 规则。不认识的规则类型一律跳过并计数，绝不猜测。
//
// 明确不翻译的两类：IP-ASN（xray 没有 ASN 匹配能力）、SRC-IP-CIDR（那是
// source，按客户端 IP 匹配，塞进 ip 条件是语义错误）。
// 其余仍会遇到的：SRC-PORT、DST-PORT、PROCESS-NAME、PROCESS-PATH、
// USER-AGENT、URL-REGEX、RULE-SET、SUB-DOMAIN、DOMAIN-WILDCARD、AND、OR、
// NOT、PROTOCOL、NETWORK、IN-PORT。

// ParseSubscription 把订阅文件文本解析成 xray 的域名与 IP 语法。
// 返回（域名列表, IP 段列表, 跳过的条数, 错误）。
//
// 两侧都解析不出内容时返回错误而非空列表：调用方据此保留上一次成功的数据。
// 若在这里返回空列表，上游改格式或 URL 失效返回 404 页面时，分流组会被清空，
// 引用它的规则被 buildRule 跳过，流量静默退回直连。
func ParseSubscription(raw string) ([]string, []string, int, error) {
	// 部分订阅源（尤其是 Windows 工具导出的）在文件开头带 UTF-8 BOM，
	// 不去掉的话它会粘在第一行的规则类型前面，导致该行的 switch/前缀匹配
	// 全部失配而被当成一条跳过的规则——只丢一行，不易察觉，但仍是数据损失。
	raw = strings.TrimPrefix(raw, "\uFEFF")

	domains := make([]string, 0, 256)
	cidrs := make([]string, 0, 64)
	seenDomain := make(map[string]bool, 256)
	seenCidr := make(map[string]bool, 64)
	skipped := 0

	for _, line := range strings.Split(raw, "\n") {
		item := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if item == "" || strings.HasPrefix(item, "#") || strings.HasPrefix(item, ";") {
			continue
		}
		// Clash YAML 的头，不是规则
		if item == "payload:" {
			continue
		}
		// Clash YAML 的条目：- '+.example.com'
		if strings.HasPrefix(item, "- ") {
			item = strings.TrimSpace(strings.TrimPrefix(item, "- "))
			item = strings.Trim(item, `'"`)
		}

		converted, isIP, ok := convertSubscriptionLine(item)
		if !ok {
			skipped++
			continue
		}
		if isIP {
			if seenCidr[converted] {
				continue
			}
			seenCidr[converted] = true
			cidrs = append(cidrs, converted)
			continue
		}
		if seenDomain[converted] {
			continue
		}
		seenDomain[converted] = true
		domains = append(domains, converted)
	}

	if len(domains) == 0 && len(cidrs) == 0 {
		return nil, nil, skipped, common.NewError(
			"订阅内容里没有解析出任何域名或 IP 段（跳过了", skipped, "条无法翻译的规则）")
	}
	return domains, cidrs, skipped, nil
}

// convertSubscriptionLine 把一行转成 xray 语法。
// 第二个返回值为 true 表示它是 IP 段而非域名；第三个为 false 表示这行应当
// 被跳过并计数。
func convertSubscriptionLine(item string) (string, bool, bool) {
	if idx := strings.Index(item, ","); idx > 0 {
		ruleType := strings.ToUpper(strings.TrimSpace(item[:idx]))
		rest := item[idx+1:]
		// 丢掉策略段与 no-resolve 之类的尾巴，只取第一个值
		if next := strings.Index(rest, ","); next >= 0 {
			rest = rest[:next]
		}
		value := strings.TrimSpace(rest)

		switch ruleType {
		case "DOMAIN-SUFFIX":
			v, ok := domainRule("domain:", value)
			return v, false, ok
		case "DOMAIN":
			v, ok := domainRule("full:", value)
			return v, false, ok
		case "DOMAIN-KEYWORD":
			// xray 的 keyword: 就是子串匹配，与 DOMAIN-KEYWORD 语义一致。
			// 会误伤（ads 命中 downloads.example.com），但那是这个规则类型在
			// Shadowrocket/Clash 里的固有行为，不是本实现引入的偏差。
			//
			// 必须归一大小写：xray 只把目标域名转小写、不归一化配置里的模式
			// （app/router/condition.go:59），大写关键词永不命中。
			//
			// 必须带显式前缀：手工录入路径（ParseDomains）存的是 keyword:xxx，
			// 两条路径形态不一致的话 MergeDomains 按字符串去重，去不掉重复。
			if !isValidKeyword(value) {
				return "", false, false
			}
			return "keyword:" + strings.ToLower(value), false, true
		case "IP-CIDR", "IP-CIDR6":
			if !isValidCIDR(value) {
				return "", false, false
			}
			return value, true, true
		case "GEOIP":
			// GEOIP,CN → geoip:cn。类别是否真的存在于机器上那份 geoip.dat，
			// 交给 ValidateCidrs 的真实 xray 判定。
			code := strings.ToLower(value)
			if !isValidGeoCode(code) {
				return "", false, false
			}
			return "geoip:" + code, true, true
		default:
			// IP-ASN / SRC-IP-CIDR 等已知但无法忠实翻译的类型，以及一切
			// 不认识的类型，一律跳过，绝不猜测
			return "", false, false
		}
	}
	// 无逗号：纯域名列表或纯 IP 列表。先试 IP——isValidDomain 明确拒绝 IP
	// 字面量，所以顺序反了会让整份中国 IP 段列表被当成垃圾全部跳过。
	if isValidCIDR(item) {
		return item, true, true
	}
	// 纯域名列表：.example.com / +.example.com / *.example.com / example.com
	// 这类列表的惯例是后缀匹配
	v, ok := domainRule("domain:", item)
	return v, false, ok
}

// isValidGeoCode 只做防呆：geoip 的类别是字母数字短串（cn、private、
// telegram），拦住带空格、斜杠、点的说明文字。
func isValidGeoCode(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
```

- [ ] **Step 4: 改 `refreshLocked` 同时写两侧**

`web/service/routing_domain.go`，把 `refreshLocked` 里从 `domains, skipped, err := ParseSubscription(raw)` 到 `Updates(...)` 的整段替换为：

```go
	domains, cidrs, skipped, err := ParseSubscription(raw)
	if err != nil {
		return s.recordFailure(group, err)
	}
	// 落库前过真实 xray 校验。两个 Validate 自身都是 fail open 的：
	// 二进制缺失、超时等一律放行，只有 xray 明确判定非法才拦。
	//
	// 空列表不送检：探针规则的条件为空数组时 xray 会报
	// "this rule has no effective fields"，把「这一侧没有内容」这个正常
	// 状态变成整次刷新失败。ValidateCidrs 自己挡了空，ValidateDomains
	// 没有（它的既有调用点保证了非空），所以在这里显式判。
	if len(domains) > 0 {
		if err := ValidateDomains(domains); err != nil {
			return s.recordFailure(group, err)
		}
	}
	if err := ValidateCidrs(cidrs); err != nil {
		return s.recordFailure(group, err)
	}
	encodedDomains, err := EncodeDomains(domains)
	if err != nil {
		return s.recordFailure(group, err)
	}
	encodedCidrs, err := EncodeCidrs(cidrs)
	if err != nil {
		return s.recordFailure(group, err)
	}

	// 用 map 而不是 struct：GORM 的 struct 更新会跳过零值，
	// LastError 与 LastSkipped 清不掉。
	//
	// 两侧【都】写，哪怕其中一个是空——这不与「失败时绝不清空」冲突，
	// 那条约束的是失败路径。成功路径上，订阅源真的不再列 IP 了，保留上一次
	// 的 IP 就是拿过期数据分流，比 IP 条件消失更危险。
	//
	// Where 里带上 subscribe_url：拉取耗时可达 30s，一批组更是分钟级，
	// 这期间管理员可能已经把订阅地址改成了别的（Update 不取 subscriptionMu）。
	// 不加这个条件，本次用旧地址拉到的内容会被当成新地址的结果写回——
	// 组的 URL 是新的，内容却是旧地址的，界面还显示「刚刚更新」，
	// 比规则单纯不生效更危险（见 spec §5.5）。
	res := database.GetDB().Model(model.DomainGroup{}).
		Where("id = ? AND subscribe_url = ?", group.Id, group.SubscribeUrl).
		Updates(map[string]any{
			"subscribed_domains": encodedDomains,
			"subscribed_cidrs":   encodedCidrs,
			"last_updated_at":    time.Now().UnixMilli(),
			"last_error":         "",
			"last_skipped":       skipped,
		})
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestParseSubscription|TestSubscription|TestDomainGroup' -v`
Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add web/service/routing_subscription.go web/service/routing_subscription_test.go web/service/routing_domain.go
git commit -m "feat(routing): 订阅解析收 IP-CIDR/IP-CIDR6/GEOIP 并写进 SubscribedCidrs

IP-ASN 与 SRC-IP-CIDR 仍然跳过并计数：前者 xray 没有匹配能力，后者是
source（按客户端 IP），塞进 ip 条件是语义错误。

无逗号的行先试 CIDR 再试域名——isValidDomain 明确拒绝 IP 字面量，顺序反了
会让整份中国 IP 段列表被当成垃圾全部跳过。

成功时两侧都写，哪怕其中一个是空：与「失败时绝不清空」不冲突，那条约束的
是失败路径；成功路径上保留上一次的 IP 就是拿过期数据分流。"
```

---

### Task 4: `buildRule` 生成 0~2 条规则

**Files:**
- Modify: `web/service/routing_inject.go:158-268`（`buildRules` 与 `buildRule`）
- Test: `web/service/routing_inject_test.go`

**Interfaces:**
- Consumes: `DecodeCidrs` / `DecodeSubscribedCidrs`（Task 1）、`MergeDomains`（已存在，同时用于域名与 IP 两类值）
- Produces: `buildRule(...) ([]map[string]any, bool, error)`（**返回值改变**：从一条规则改成 0~2 条）

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_inject_test.go`。**复用该文件既有的夹具，不要新造**：
`newTemplateConfig(t)`（:24）、`decodeRules(t, cfg)`（:42）、`mustEncodeGroupIds(t, ids)`
（在 `routing_rule_test.go:495`，同包）。建数据用 `database.GetDB().Save(...)` 直接写库，
与该文件既有的 `TestBuildRuleMergesManualAndSubscribedDomains`（:428）完全一致——
**不要走 `RoutingRuleService.Add`**，它的 `validate`（`routing_rule.go:75`）要求
`OutboundId` 指向真实存在的出站节点，用 `ActionBlock` 建数据就绕开了这层无关的耦合。

注意 `newTemplateConfig` 用的 `testTemplate`（:14）里预置了 **1 条** `api` 规则，
生成的规则追加在它之后，所以断言里的期望条数都要算上它。

```go
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
```


- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestBuildRule' -v`
Expected: FAIL。`TestBuildRuleSplitsDomainAndIP` 只拿到 1 条规则（IP 段被忽略）；`TestBuildRuleOnlyCidrsProducesOneRule` 拿到 0 条（整条被当成「域名组为空」丢弃）。

- [ ] **Step 3: 改 `buildRules` 的消费方式**

`web/service/routing_inject.go`，把 `buildRules` 里的循环体替换为：

```go
	for _, rule := range rules {
		generated, isBlock, skip := s.buildRule(rule, inboundTagById, outboundTagById)
		if skip != nil {
			// 设计 §5.3 接受这道防线的理由是「宁可规则不生效，用户能察觉」。
			// 跳过若不记日志，用户其实察觉不到：规则表照常渲染，生成的配置里
			// 却没有这条规则，流量默默走了默认出站。
			logger.Warning("skip routing rule, id:", rule.Id, "remark:", rule.Remark,
				"reason:", skip)
			continue
		}
		// 一条数据库规则最多产出两条 xray 规则（域名一条、IP 一条）。
		// 顺序由 buildRule 固定（domain 在前），这里原样追加，不重排。
		for _, g := range generated {
			if isBlock {
				blockRules = append(blockRules, g)
			} else {
				proxyRules = append(proxyRules, g)
			}
		}
	}
```

- [ ] **Step 4: 重写 `buildRule`**

把 `buildRule` 的整个函数（含其文档注释）替换为：

```go
// buildRule 生成 0~2 条 xray 规则，或说明为什么必须整条丢弃。
//
// 第三个返回值非 nil 即表示跳过，且必须给出原因——调用方要把它记进日志，
// 否则这道防线对用户是隐形的。
//
// 一条数据库规则最多产出两条 xray 规则：域名条件一条、IP 条件一条。
// **绝不把两者并列进同一条**——同一条规则内的条件是 AND
// （app/router/config.go:33 的 BuildCondition + condition.go:35 的
// ConditionChan.Apply），并列会让「这批域名或这批 IP 走 B」变成「域名命中
// 且解析出的 IP 也命中」，几乎永不命中，而 xray 返回 Configuration OK、
// 面板首页照样显示 running。
//
// 也绝不能退而求其次生成一条条件为空数组的规则：xray 把长度为 0 的条件
// 视为「不限制」，那样的规则会劫持该入站的全部流量，且不会有任何报错。
//
// 条件来自一条规则引用的多个分流组的合并（DomainGroupIds，升序）。每个组内
// 再合并手工录入与订阅更新。两级合并顺序都是确定的，是「生成逐字节确定」
// 不变量的一部分。
func (s *RoutingInjector) buildRule(
	rule *model.RoutingRule,
	inboundTagById map[int]string,
	outboundTagById map[int]string,
) ([]map[string]any, bool, error) {
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return nil, false, common.NewError("规则的分流组数据损坏, id:", rule.Id, "err:", err)
	}
	if len(groupIds) == 0 {
		return nil, false, common.NewError("规则没有指定任何分流组, id:", rule.Id,
			"（条件为空会让规则退化成劫持该入站全部流量）")
	}

	// 按 DomainGroupIds 的升序逐组取条件。失效的组剔除而不是整条丢弃：
	// 一个订阅从未拉取成功的空组，不该把同一条规则里本来好好的分流一起
	// 废掉；对 block 规则尤其如此——整条丢弃等于本该封禁的目标全部裸奔，
	// 部分生成至少封住了还在的那部分。
	//
	// 「数据损坏」与「组为空」的后果完全相同（该组贡献 0 条条件），统一
	// 走剔除；剔除的方向是缩小匹配范围，安全侧一致。
	domainLists := make([][]string, 0, len(groupIds))
	cidrLists := make([][]string, 0, len(groupIds))
	for _, gid := range groupIds {
		group, groupErr := s.domainGroupService.Get(gid)
		if groupErr != nil {
			logger.Warning("routing rule drops a group that no longer exists, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		manualDomains, decodeErr := DecodeDomains(group.Domains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt manual domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		subscribedDomains, decodeErr := DecodeSubscribedDomains(group.SubscribedDomains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt subscribed domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		manualCidrs, decodeErr := DecodeCidrs(group.Cidrs)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt manual cidrs, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		subscribedCidrs, decodeErr := DecodeSubscribedCidrs(group.SubscribedCidrs)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt subscribed cidrs, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		// 组内合并顺序确定（手工在前、订阅在后、保留首次出现）。
		// MergeDomains 只是「有序去重」，两类值都用它。
		oneDomains := MergeDomains(manualDomains, subscribedDomains)
		oneCidrs := MergeDomains(manualCidrs, subscribedCidrs)
		if len(oneDomains) == 0 && len(oneCidrs) == 0 {
			logger.Warning("routing rule drops an empty group, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		if len(oneDomains) > 0 {
			domainLists = append(domainLists, oneDomains)
		}
		if len(oneCidrs) > 0 {
			cidrLists = append(cidrLists, oneCidrs)
		}
	}

	// 跨组按上面的遍历顺序合并去重。禁止改用遍历 map 产生顺序——
	// 那样生成不再逐字节确定，Config.Equals 恒为 false，10 秒的重启 cron
	// 会不停重启 xray。
	domains := MergeDomains(domainLists...)
	cidrs := MergeDomains(cidrLists...)
	if len(domains) == 0 && len(cidrs) == 0 {
		return nil, false, common.NewError("规则的分流组全部不存在或为空, rule id:", rule.Id,
			"group ids:", groupIds,
			"（条件为空会让规则退化成劫持该入站全部流量）")
	}

	inboundIds, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return nil, false, common.NewError("规则的入站数据损坏, id:", rule.Id, "err:", err)
	}
	var inboundTags []string
	if len(inboundIds) > 0 {
		tags := make([]string, 0, len(inboundIds))
		missing := make([]int, 0)
		for _, id := range inboundIds {
			tag, ok := inboundTagById[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			tags = append(tags, tag)
		}
		if len(tags) == 0 {
			// 剩下空数组绝不能输出。实测（Xray 26.7.28）确认 xray 把
			// inboundTag: [] 当作「不限制」而非「不匹配任何入站」——一条本该
			// 只覆盖甲的规则会劫持所有人的流量，且 Configuration OK、
			// 面板首页照样显示 running。
			return nil, false, common.NewError("规则指定的入站全部不存在或已禁用, ids:", inboundIds)
		}
		if len(missing) > 0 {
			// 部分失效不整条丢弃：剩下的入站仍该按规则走。但必须记录，
			// 否则被剔除的那些用户会静默回落直连而无人察觉。
			logger.Warning("routing rule drops inbounds that no longer exist or are disabled, rule id:",
				rule.Id, "inbound ids:", missing)
		}
		// tags 的顺序由 InboundIds 的升序保证，不得改用遍历 inboundTagById
		// 这个 map 来产生顺序——那样生成不再逐字节确定。
		inboundTags = tags
	}

	var outboundTag string
	isBlock := false
	switch rule.Action {
	case model.ActionBlock:
		outboundTag = model.BlockOutboundTag
		isBlock = true
	case model.ActionProxy:
		tag, ok := outboundTagById[rule.OutboundId]
		if !ok {
			return nil, false, common.NewError("出站节点不存在、已禁用或未写入配置, id:",
				rule.OutboundId)
		}
		outboundTag = tag
	default:
		return nil, false, common.NewError("未知的动作:", rule.Action)
	}

	// 顺序固定：domain 在前、ip 在后。这是「生成逐字节确定」的一部分。
	generated := make([]map[string]any, 0, 2)
	emit := func(conditionKey string, values []string) {
		g := map[string]any{
			"type":        "field",
			conditionKey:  values,
			"outboundTag": outboundTag,
		}
		// 空数组绝不写进配置（见函数注释）。inboundTags 为空表示这是一条
		// 全局规则，此时正确的做法是压根不输出 inboundTag 这个键。
		if len(inboundTags) > 0 {
			g["inboundTag"] = inboundTags
		}
		generated = append(generated, g)
	}
	if len(domains) > 0 {
		emit("domain", domains)
	}
	if len(cidrs) > 0 {
		emit("ip", cidrs)
	}
	return generated, isBlock, nil
}
```

同时把 `MergeDomains`（`web/service/routing_domain.go:290`）的文档注释首行改为：

```go
// MergeDomains 按传入顺序合并多个字符串列表，去重并保留首次出现。
// 域名与 IP 段两类规则值都用它——它只是一个有序去重，与值的语义无关。
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestBuildRule|TestInject|TestRouting' -v`
Expected: 全部 PASS，包括该文件既有的确定性用例（同输入两次生成逐字节相同）。

- [ ] **Step 6: 提交**

```bash
git add web/service/routing_inject.go web/service/routing_inject_test.go web/service/routing_domain.go
git commit -m "feat(routing): buildRule 生成 0~2 条规则，域名与 IP 各自成条

绝不并列进同一条：同一条 xray 规则内的条件是 AND（config.go:33 +
condition.go:35），并列会把「这批域名或这批 IP 走 B」变成「域名命中且解析
出的 IP 也命中」，几乎永不命中，而 xray 返回 Configuration OK。

绝不发出空的条件数组：长度为 0 的条件在 protobuf 里整个消失，规则退化成
只由 inboundTag 约束，范围被静默放大。两条不变量各有直接断言的用例。"
```

---

### Task 5: `ipRuleResolveDomain` 开关

**Files:**
- Modify: `web/service/setting.go:25-47`（`defaultValueMap`）、getter 区
- Modify: `web/entity/entity.go:30-59`（`AllSetting`）、`CheckValid`
- Modify: `web/service/routing_inject.go:18-24`（`RoutingInjector` 加 `settingService`）、`Inject`
- Modify: `web/assets/js/model/models.js:177-209`（`AllSetting` 构造函数）
- Modify: `web/html/xui/setting.html`
- Test: `web/service/routing_inject_test.go`、`web/service/setting_defaults_test.go`

**Interfaces:**
- Produces: `SettingService.GetIPRuleResolveDomain() (bool, error)`；`entity.AllSetting.IPRuleResolveDomain int`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_inject_test.go`：

```go
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
```

`newTemplateConfig(t)` 是 `web/service/routing_inject_test.go:24` 里既有的辅助函数，
返回一个基于该文件 `testTemplate`（:14）的 `*xray.Config`——**直接用它，不要新造**。
那份模板里没有 `domainStrategy`，正是这两个用例需要的初始状态。

追加到 `web/service/setting_defaults_test.go`：

```go
func TestIPRuleResolveDomainDefaultsToOff(t *testing.T) {
	setupDB(t)
	all, err := (&SettingService{}).GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.IPRuleResolveDomain != 0 {
		t.Errorf("IPRuleResolveDomain = %d, want 0", all.IPRuleResolveDomain)
	}
}

func TestCheckValidRejectsOutOfRangeIPRuleResolveDomain(t *testing.T) {
	all := validBaseSetting()
	all.IPRuleResolveDomain = 2
	if err := all.CheckValid(); err == nil {
		t.Error("expected error for a value other than 0/1")
	}
}
```

（`validBaseSetting()` 是 `web/service/setting_baseline_test.go:12` 里既有的辅助函数，返回一份能通过 `CheckValid` 的 `*entity.AllSetting`，不接受参数。）

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestInjectLeavesDomainStrategy|TestInjectWritesDomainStrategy|TestIPRuleResolveDomain|TestCheckValidRejectsOutOfRange' -v`
Expected: 编译失败，`all.IPRuleResolveDomain undefined`。

- [ ] **Step 3: 加设置项（5 处）**

3.1 `web/service/setting.go` 的 `defaultValueMap`，在 `"concurrencyIdleTimeout"` 之后加一行：

```go
	"ipRuleResolveDomain":      "0",
```

3.2 同文件的 getter 区（`GetAccessLogEnable` 附近）追加：

```go
// GetIPRuleResolveDomain 报告是否允许 IP 分流规则匹配域名目标。
//
// 为真时生成期写 routing.domainStrategy = IPIfNonMatch，xray 会在第一遍
// 全部规则都没命中时，把域名解析成 IP 再跑第二遍（app/router/router.go:261）。
func (s *SettingService) GetIPRuleResolveDomain() (bool, error) {
	v, err := s.getInt("ipRuleResolveDomain")
	if err != nil {
		return false, err
	}
	return v != 0, nil
}
```

3.3 `web/entity/entity.go` 的 `AllSetting`，在 `ConcurrencyIdleTimeout` 之后加：

```go
	IPRuleResolveDomain int `json:"ipRuleResolveDomain" form:"ipRuleResolveDomain"`
```

3.4 同文件 `CheckValid`，在时间/URL 校验之后加：

```go
	// 只接受 0/1：反射只支持 int，前端的 switch 也只会送这两个值。
	// 放行其他值会让生成期写出一个 xray 不认识的 domainStrategy，
	// 而那会让整份配置加载失败——全员断网。
	if s.IPRuleResolveDomain != 0 && s.IPRuleResolveDomain != 1 {
		return common.NewError("「IP 规则匹配域名目标」只能是 0 或 1:", s.IPRuleResolveDomain)
	}
```

3.5 `web/assets/js/model/models.js` 的 `AllSetting` 构造函数，在 `this.concurrencyIdleTimeout = 120;` 之后加：

```javascript
        this.ipRuleResolveDomain = 0;
```

> 这一行不是收尾工作。`ObjectUtil.cloneProps` 只克隆目标对象已经拥有的 key，漏掉它会让服务端返回值被丢弃、提交体里缺字段，`CheckValid` 拒绝零值时**整个保存配置接口失败**，端口、证书、时区一起遭殃，而报错只指向这个新字段。

- [ ] **Step 4: 注入 `domainStrategy`**

`web/service/routing_inject.go`，给 `RoutingInjector` 结构体加一个字段：

```go
	settingService     SettingService
```

在 `Inject` 里 `routing["rules"] = rules` 之后、`json.Marshal(routing)` 之前插入：

```go
	// 开关为 0 时【不碰】domainStrategy：模板里管理员可能手写过它，
	// 覆盖成默认值是在他不知情时改变分流行为。升级后行为零变化也靠这一条。
	resolveDomain, err := s.settingService.GetIPRuleResolveDomain()
	if err != nil {
		return err
	}
	if resolveDomain {
		routing["domainStrategy"] = "IPIfNonMatch"
	}
```

- [ ] **Step 5: 加前端开关**

`web/html/xui/setting.html`，在分流相关的 tab（订阅更新时刻所在的那个 `a-list`）里追加：

```html
                                <setting-list-item type="switch" title="让 IP 规则也匹配域名目标"
                                                   desc="关闭时，IP 分流规则只对客户端直接连 IP 的连接生效。打开后 xray 会在所有域名规则都没命中时，于本机解析域名再匹配一次 IP 规则；代价是每条未命中的连接多一次解析，且模板里「私网 IP → 拦截」那条规则会开始对域名生效。会覆盖模板中的 routing.domainStrategy，开关变化需重启 xray（约 10 秒后自动完成）"
                                                   v-model="allSetting.ipRuleResolveDomain"></setting-list-item>
```

- [ ] **Step 6: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestInject|TestIPRuleResolveDomain|TestCheckValid' -v && go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add web/service/setting.go web/entity/entity.go web/service/routing_inject.go web/assets/js/model/models.js web/html/xui/setting.html web/service/routing_inject_test.go web/service/setting_defaults_test.go
git commit -m "feat(routing): 新增 ipRuleResolveDomain 开关，默认关

为 1 时生成期写 routing.domainStrategy=IPIfNonMatch，让 IP 规则也能匹配
域名目标；为 0 时完全不碰该字段，保留管理员在模板里手写的值——升级后
行为零变化靠的就是这一条。

按规范改满 5 处，含 models.js：漏掉它会让 cloneProps 丢弃服务端返回值、
提交体缺字段，整个保存配置接口失败。"
```

---

### Task 6: 控制器与前端表单

**Files:**
- Modify: `web/controller/routing.go:22-33`（`domainGroupForm`）、`:42-70`（两个视图结构）、`:271-360`（三个处理函数）
- Modify: `web/assets/js/model/routing.js:14-30`（`DomainGroup`）
- Modify: `web/html/xui/routing.html`（弹窗、列定义、`openGroup`、`saveGroup`）
- Test: 由 Task 8 的 e2e 与模板测试覆盖

**Interfaces:**
- Consumes: `ParseCidrs` / `EncodeCidrs` / `ValidateCidrs`（Task 1、2）
- Produces: `encodeCidrsFromForm(raw string) (string, error)`

- [ ] **Step 1: 后端表单与视图**

`web/controller/routing.go`，`domainGroupForm` 加字段（放在 `Domains` 之后）：

```go
	Cidrs   string `json:"cidrs" form:"cidrs"`
```

`domainGroupSummary` 加字段：

```go
	CidrCount int `json:"cidrCount"`
```

`domainGroupDetail` 加字段：

```go
	Cidrs                 string   `json:"cidrs"`
	SubscribedCidrPreview []string `json:"subscribedCidrPreview"`
	SubscribedCidrCount   int      `json:"subscribedCidrCount"`
```

在 `encodeDomainsFromForm` 之后追加：

```go
// encodeCidrsFromForm 把 textarea 原文校验并转成入库格式。
// 允许为空：一个组可以只有域名，或只有订阅内容。
func encodeCidrsFromForm(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "[]", nil
	}
	list, err := service.ParseCidrs(raw)
	if err != nil {
		return "", err
	}
	if err := service.ValidateCidrs(list); err != nil {
		return "", err
	}
	return service.EncodeCidrs(list)
}
```

`addDomainGroup` 与 `updateDomainGroup` 各加一段（紧跟在 `encodeDomainsFromForm` 之后）：

```go
	encodedCidrs, err := encodeCidrsFromForm(form.Cidrs)
	if err != nil {
		jsonMsg(c, "添加域名组", err) // updateDomainGroup 里写「修改域名组」
		return
	}
```

并把两处 `model.DomainGroup{...}` 字面量补上 `Cidrs: encodedCidrs`。

列表与详情两个组装函数里，`CidrCount` 取 `len(MergeDomains(手工, 订阅))` 的长度（与 `EffectiveCount` 同样的口径），`SubscribedCidrPreview` 截到 `subscribedPreviewLimit`；解码失败时沿用既有的 `Broken` 处理，不新增分支。

- [ ] **Step 2: 前端模型**

`web/assets/js/model/routing.js` 的 `DomainGroup` 构造函数加一行：

```javascript
        this.cidrCount = json.cidrCount || 0;
```

- [ ] **Step 3: 弹窗加 IP 段输入**

`web/html/xui/routing.html`，在「手工域名」`a-form-item` 之后插入：

```html
            <a-form-item label="IP 段（一行一条）">
                <a-input type="textarea" :rows="4" v-model="groupModal.cidrs"
                         placeholder="1.2.3.0/24&#10;8.8.8.8&#10;2001:db8::/32&#10;geoip:cn&#10;可留空"></a-input>
                <div style="color: #888; font-size: 12px; margin-top: 4px;">
                    IP 段与域名各自生成一条独立的分流规则，互不影响。
                    默认只对客户端直接连 IP 的连接生效；要让它也匹配域名目标，
                    到「面板设置」打开「让 IP 规则也匹配域名目标」。
                </div>
            </a-form-item>
```

把订阅地址下方那句说明改为：

```html
                <div style="color: #888; font-size: 12px; margin-top: 4px;">
                    支持 Surge/Clash 规则集、纯域名列表与纯 IP 列表。
                    IP-CIDR / IP-CIDR6 / GEOIP 会进 IP 段；IP-ASN、SRC-IP-CIDR 等无法忠实翻译的规则会被忽略。
                    保存后由定时任务拉取，或在列表里点「立即更新」。
                </div>
```

- [ ] **Step 4: 列表加 IP 列与弹窗状态**

`groupColumns` 里在「域名」之后插入：

```javascript
                { title: 'IP 段', width: 100, scopedSlots: { customRender: 'cidrs' } },
```

并在 `a-table` 的模板区照既有 `domains` 插槽的写法加一个 `cidrs` 插槽，渲染 `[[ group.cidrCount ]] 条`（为 0 时显示 `—`，与「订阅」列的空态一致）。

`groupModal` 的初始值加 `cidrs: '', subscribedCidrPreview: [], subscribedCidrCount: 0,`。

`openGroup` 里，重置段加：

```javascript
                this.groupModal.cidrs = '';
                this.groupModal.subscribedCidrPreview = [];
                this.groupModal.subscribedCidrCount = 0;
```

回填段加：

```javascript
                    this.groupModal.cidrs = d.cidrs || '';
                    this.groupModal.subscribedCidrPreview = d.subscribedCidrPreview || [];
                    this.groupModal.subscribedCidrCount = d.subscribedCidrCount || 0;
```

`saveGroup` 的 `data` 加 `cidrs: this.groupModal.cidrs,`。

- [ ] **Step 5: 跑模板与包测试**

Run: `go test ./web/... -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot|TestDomainGroup' -v`
Expected: PASS。

- [ ] **Step 6: 手工验收**

Run: `XUI_DEBUG=true go run main.go`（必须在仓库根目录，调试模式下模板与静态资源从磁盘读）
在「分流管理 → 域名组」新建一个组，域名填 `domain:openai.com`、IP 段填 `1.2.3.0/24`，保存；再编辑它，确认两个框都回填了刚才的内容（这一步专门验 `updateFieldsFor` 的列名单——漏加 `cidrs` 时保存会「成功」但内容不变）。

- [ ] **Step 7: 提交**

```bash
git add web/controller/routing.go web/assets/js/model/routing.js web/html/xui/routing.html
git commit -m "feat(routing): 分流组表单与列表支持 IP 段"
```

---

### Task 7: 导入导出

**Files:**
- Modify: `web/service/routing_portable.go:35-39`（`PortableDomainGroup`）、`:156-174`（导出）、`:485-545`（导入）
- Test: `web/service/routing_portable_test.go`

**Interfaces:**
- Consumes: `ParseCidrs` / `EncodeCidrs` / `ValidateCidrs` / `DecodeCidrs`
- Produces: `PortableDomainGroup.Cidrs []string`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_portable_test.go`：

```go
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
	data, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var f PortableFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(f.DomainGroups) != 1 {
		t.Fatalf("groups = %d, want 1", len(f.DomainGroups))
	}
	got := f.DomainGroups[0].Cidrs
	if len(got) != 2 || got[0] != "1.2.3.0/24" || got[1] != "geoip:cn" {
		t.Errorf("cidrs = %v", got)
	}
}

// 旧格式（没有 cidrs 字段）必须能导入，且行为与改动前一致。
func TestImportOldFormatWithoutCidrs(t *testing.T) {
	setupDB(t)
	raw := `{"version":1,"scope":["domainGroups"],"domainGroups":[{"remark":"g","domains":["domain:openai.com"],"subscribeUrl":""}],"outbounds":[],"rules":[]}`
	report, err := (&RoutingPortableService{}).Import([]byte(raw))
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

// 不导出订阅拉来的 IP 段：单个组可达十几万条，且它是本机这一次拉取的状态。
func TestExportOmitsSubscribedCidrs(t *testing.T) {
	setupDB(t)
	if err := (&DomainGroupService{}).Add(&model.DomainGroup{
		Remark: "g", Domains: "[]", Cidrs: "[]",
		SubscribeUrl:    "https://example.com/x.list",
		SubscribedCidrs: `["9.9.9.0/24"]`,
	}); err != nil {
		t.Fatalf("add group: %v", err)
	}
	data, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if strings.Contains(string(data), "9.9.9.0/24") {
		t.Errorf("export must not contain subscribed cidrs: %s", data)
	}
}
```

（`RoutingPortableService`、`PortableFile`、`Export`/`Import` 的实际类型名与签名以 `web/service/routing_portable.go` 现有定义为准，照抄该文件既有测试的调用写法。）

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestExportImportRoundTripsCidrs|TestImportOldFormatWithoutCidrs|TestExportOmitsSubscribedCidrs' -v`
Expected: 编译失败，`f.DomainGroups[0].Cidrs undefined`。

- [ ] **Step 3: 结构体加字段**

```go
type PortableDomainGroup struct {
	Remark string   `json:"remark"`
	Domains []string `json:"domains"`
	// Cidrs 用值类型切片即可，不需要 PortableRule.InboundRefs 那种指针。
	// 那里要区分「显式 []」与「字段缺失」，是因为 [] 有「对所有入站生效」的
	// 特殊全局语义；这里的空与缺失都只表示「这个组没有 IP 段」，两者同义。
	Cidrs        []string `json:"cidrs"`
	SubscribeUrl string   `json:"subscribeUrl"`
}
```

- [ ] **Step 4: 导出侧**

在导出循环里，`manual` 之后照同样的写法解一份 IP 段（解码失败当空列表，理由与域名侧相同），并写进结构体：

```go
			manualCidrs, err := DecodeCidrs(g.Cidrs)
			if err != nil {
				manualCidrs = nil
			}
			if manualCidrs == nil {
				manualCidrs = []string{}
			}
			f.DomainGroups = append(f.DomainGroups, PortableDomainGroup{
				Remark:       g.Remark,
				Domains:      manual,
				Cidrs:        manualCidrs,
				SubscribeUrl: g.SubscribeUrl,
			})
```

`SubscribedCidrs` 不导出——与 `SubscribedDomains` 同理（体积，以及搬过去会显示一个假的「刚刚更新」）。

- [ ] **Step 5: 导入侧**

在 `encoded`（域名）那一段之后照抄一份 IP 段的：

```go
		// 走与表单同一条校验路径。导入文件是不可信输入，与管理员在表单里
		// 输入的东西同级。
		encodedCidrs := "[]"
		if len(item.Cidrs) > 0 {
			list, err := ParseCidrs(strings.Join(item.Cidrs, "\n"))
			if err != nil {
				report.DomainGroups.Failed++
				report.fail("域名组「%s」的 IP 段格式有误：%v", item.Remark, err)
				continue
			}
			if err := ValidateCidrs(list); err != nil {
				report.DomainGroups.Failed++
				report.fail("域名组「%s」的 IP 段未通过校验：%v", item.Remark, err)
				continue
			}
			encodedCidrs, err = EncodeCidrs(list)
			if err != nil {
				report.DomainGroups.Failed++
				report.fail("域名组「%s」的 IP 段编码失败：%v", item.Remark, err)
				continue
			}
		}
```

并把落库的字面量改为：

```go
		g := &model.DomainGroup{
			Remark: item.Remark, Domains: encoded, Cidrs: encodedCidrs,
			SubscribeUrl: item.SubscribeUrl,
		}
```

- [ ] **Step 6: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestExport|TestImport' -v`
Expected: 全部 PASS，含既有的往返与幂等用例。

- [ ] **Step 7: 提交**

```bash
git add web/service/routing_portable.go web/service/routing_portable_test.go
git commit -m "feat(routing): 导入导出带上分流组的 IP 段

不导出 SubscribedCidrs（体积，且搬过去会显示假的「刚刚更新」）。
Cidrs 用值类型切片：这里的空与缺失都只表示「没有 IP 段」，不像
InboundRefs 那样需要区分显式 [] 与字段缺失。

旧面板读新文件会忽略 cidrs，组只剩域名——分流范围缩小而非放大。"
```

---

### Task 8: e2e 与门禁

**Files:**
- Modify: `web/service/routing_e2e_test.go`

- [ ] **Step 1: 加 e2e 用例**

照该文件既有用例的写法（用真实 xray 跑 `run -test`），加三份生成配置的验证：域名+IP 混合组、纯 IP 组、开了 `ipRuleResolveDomain` 的。沿用该文件既有的「找不到 xray 二进制就 `t.Skip` 并说明原因」的守卫写法——不要让缺二进制表现成失败。

- [ ] **Step 2: 跑 e2e**

Run: `go test ./web/service/ -run 'E2E' -v`
Expected: PASS，或在没有 `bin/xray-darwin-arm64` 的机器上明确 SKIP 并打印原因。

- [ ] **Step 3: 跑完整门禁**

Run: `make verify`
Expected: vet 无输出、全部包测试 PASS、build 成功。失败时读错误、修问题、重跑，不跳过、不粉饰。

- [ ] **Step 4: 检查最终 diff**

Run: `git diff main...HEAD --stat && git status --short`
Expected: 只有本计划列出的文件，工作区干净，没有调试残留与临时文件。

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_e2e_test.go
git commit -m "test(routing): e2e 覆盖混合组、纯 IP 组与开了 domainStrategy 的配置"
```
