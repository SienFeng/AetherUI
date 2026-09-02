# 域名组订阅更新 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让域名组挂一个订阅 URL，面板每天定时拉取远程规则集、解析成 xray 域名语法并入库，分流规则自动生效，全程无需重启面板或 xray。

**Architecture:** 订阅域名与手工域名在 `DomainGroup` 上分两个字段物理隔离，注入器生成配置时合并去重。拉取由一个每 10 分钟自检的 cron job 驱动，更新成功后调用现有的 `SetToNeedRestart()`，复用既有的 10 秒重启链路。拉取失败或解析为空一律保留旧数据。

**Tech Stack:** Go 1.21（标准库 `net/http`，不新增依赖）、GORM/SQLite、robfig/cron v3、Vue 2 + ant-design-vue 服务端模板。

**Spec:** `docs/superpowers/specs/2026-09-02-domain-subscription-design.md`

## Global Constraints

以下约束来自 spec 与 `CLAUDE.md`，**每个任务都隐含包含**：

- **不新增第三方依赖。** 拉取用标准库 `net/http`，解析手写。
- **生成必须逐字节确定。** 合并、排序、去重的顺序必须是确定的。任何遍历 map 产生数组顺序的写法都是 bug——`Config.Equals()` 会因此恒为 false，每 10 秒的重启 cron 将不停重启 xray。
- **绝不输出条件残缺的规则。** 域名为空的规则会被 xray 当作「不限制」，劫持该入站全部流量。合并结果为空时 `buildRule` 必须整条跳过并记 `logger.Warning`。
- **拉取失败或解析为空 → 保留上一次成功的数据**，只写 `LastError`。绝不清空 `SubscribedDomains`。
- **cron 没有 panic 恢复。** `web/job/` 下的代码里任何 nil map 解引用、切片越界都会杀掉整个面板进程。
- **测试工作目录**：`web/service` 包的 `TestMain`（在 `routing_validate_test.go:21`）已 `os.Chdir` 到仓库根。不要新增依赖包内相对路径的测试。
- **响应体上限 10 MB，超时 30 秒。**
- **URL 只接受 `http://` 与 `https://`。**
- 构建命令：`CGO_ENABLED=1 go build -trimpath -o a-ui main.go`
- 测试命令：`go test ./web/service/ ./web/ -v`

---

### Task 1: 数据模型与域名合并

给 `DomainGroup` 加订阅相关字段，并实现合并去重的纯函数。这是后续所有任务的地基。

**Files:**
- Modify: `database/model/routing.go`（`DomainGroup` 结构体）
- Modify: `web/service/routing_domain.go`（新增 `MergeDomains`）
- Test: `web/service/routing_domain_test.go`

**Interfaces:**
- Produces:
  - `model.DomainGroup` 新增字段 `SubscribeUrl string`、`SubscribedDomains string`、`LastUpdatedAt int64`、`LastError string`、`LastSkipped int`
  - `service.MergeDomains(manual, subscribed []string) []string`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_domain_test.go`：

```go
func TestMergeDomainsKeepsManualFirstAndDeduplicates(t *testing.T) {
	manual := []string{"domain:my-nas.local", "domain:qq.com"}
	subscribed := []string{"domain:qq.com", "domain:163.com"}
	got := MergeDomains(manual, subscribed)
	want := []string{"domain:my-nas.local", "domain:qq.com", "domain:163.com"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 合并结果必须逐字节确定，否则 Config.Equals 恒为 false，
// 每 10 秒的重启 cron 会不停重启 xray。
func TestMergeDomainsIsDeterministic(t *testing.T) {
	manual := []string{"domain:b.com", "domain:a.com"}
	subscribed := []string{"domain:c.com", "domain:a.com", "domain:d.com"}
	first := MergeDomains(manual, subscribed)
	for i := 0; i < 50; i++ {
		again := MergeDomains(manual, subscribed)
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("run %d differs at %d: %q vs %q", i, j, first[j], again[j])
			}
		}
	}
}

func TestMergeDomainsHandlesEmptyInputs(t *testing.T) {
	if got := MergeDomains(nil, nil); len(got) != 0 {
		t.Errorf("both empty: got %v, want empty", got)
	}
	if got := MergeDomains([]string{"domain:a.com"}, nil); len(got) != 1 {
		t.Errorf("subscribed empty: got %v", got)
	}
	if got := MergeDomains(nil, []string{"domain:a.com"}); len(got) != 1 {
		t.Errorf("manual empty: got %v", got)
	}
}

func TestMergeDomainsDropsEmptyStrings(t *testing.T) {
	got := MergeDomains([]string{"", "domain:a.com"}, []string{"", ""})
	if len(got) != 1 || got[0] != "domain:a.com" {
		t.Errorf("got = %v, want [domain:a.com]", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run TestMergeDomains -v
```

预期：编译失败，`undefined: MergeDomains`

- [ ] **Step 3: 加模型字段**

修改 `database/model/routing.go` 的 `DomainGroup`，在 `Domains` 之后追加：

```go
// DomainGroup 是一批可复用的域名集合。
type DomainGroup struct {
	Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Remark string `json:"remark" form:"remark"`
	// Domains 是管理员手工录入的域名，JSON 字符串数组，元素为 xray 原生域名语法：
	// domain:openai.com / full:chat.openai.com / geosite:openai / regexp:.*\.oaistatic\.com
	Domains string `json:"domains" form:"domains"`

	// SubscribeUrl 为空表示这个组不订阅，行为与本功能上线前完全一致。
	SubscribeUrl string `json:"subscribeUrl" form:"subscribeUrl"`
	// SubscribedDomains 是上一次成功拉取并解析出的域名，JSON 字符串数组。
	// 与 Domains 物理隔离：订阅更新绝不覆盖管理员手工补的条目，
	// 两个字段各自只有一个写入方，永不交叉。
	SubscribedDomains string `json:"subscribedDomains" form:"subscribedDomains"`
	// LastUpdatedAt 是上一次「成功」更新的时刻，Unix 毫秒。0 表示从未成功过，
	// 调度会据此立即拉取一次，见 SubscriptionJob。
	LastUpdatedAt int64 `json:"lastUpdatedAt" form:"lastUpdatedAt"`
	// LastError 是上一次尝试的失败原因，成功时清空。必须显示在界面上——
	// 只进日志的话，管理员看到的是一个域名数量停在两周前却毫无提示的组。
	LastError string `json:"lastError" form:"lastError"`
	// LastSkipped 是上一次成功解析时跳过的非域名规则条数（IP-CIDR 等）。
	LastSkipped int `json:"lastSkipped" form:"lastSkipped"`
}
```

`database/db.go` 的 `initRouting()` 已经对 `DomainGroup` 调了 `AutoMigrate`，GORM 会自动加列，**无需改动**。

- [ ] **Step 4: 实现 MergeDomains**

追加到 `web/service/routing_domain.go`：

```go
// MergeDomains 把手工域名与订阅域名合并去重。
//
// 顺序是确定的：手工在前、订阅在后，各自保持原顺序，重复项保留首次出现。
// 这一点不能含糊——注入器的第四条不变量要求生成逐字节确定，顺序一旦不稳定，
// Config.Equals 恒为 false，每 10 秒的重启 cron 会不停重启 xray。
func MergeDomains(manual, subscribed []string) []string {
	merged := make([]string, 0, len(manual)+len(subscribed))
	seen := make(map[string]bool, len(manual)+len(subscribed))
	for _, list := range [][]string{manual, subscribed} {
		for _, d := range list {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			merged = append(merged, d)
		}
	}
	return merged
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./web/service/ -run TestMergeDomains -v
```

预期：4 个测试全部 PASS

- [ ] **Step 6: 确认既有测试没被破坏**

```bash
go build ./... && go test ./web/service/ -v
```

预期：全部 PASS（`ValidateDomains` 相关测试需要 `bin/xray-*`，缺失时会 skip，属正常）

- [ ] **Step 7: 提交**

```bash
git add database/model/routing.go web/service/routing_domain.go web/service/routing_domain_test.go
git commit -m "feat(routing): 域名组增加订阅字段与域名合并函数"
```

---

### Task 2: 订阅内容解析器

把订阅文件的文本解析成 xray 域名语法。纯函数，不碰网络、不碰数据库，是本功能覆盖率最该高的部分。

**Files:**
- Create: `web/service/routing_subscription.go`
- Test: `web/service/routing_subscription_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `service.ParseSubscription(raw string) ([]string, int, error)` — 返回（域名列表, 跳过条数, 错误）

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_subscription_test.go`：

```go
package service

import "testing"

func TestParseSubscriptionSurgeFormat(t *testing.T) {
	raw := `# ChinaMax
DOMAIN-SUFFIX,qq.com
DOMAIN,exact.example.com
DOMAIN-KEYWORD,baidu
IP-CIDR,1.1.1.1/32,no-resolve
PROCESS-NAME,Telegram
`
	domains, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:qq.com", "full:exact.example.com", "baidu"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}

func TestParseSubscriptionPlainDomainList(t *testing.T) {
	raw := ".360.cn\n163.com\n\n# comment\n.qq.com\n"
	domains, skipped, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:360.cn", "domain:163.com", "domain:qq.com"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
}

func TestParseSubscriptionClashYaml(t *testing.T) {
	raw := "payload:\n  - '+.qq.com'\n  - \"163.com\"\n  - '.baidu.com'\n"
	domains, _, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:qq.com", "domain:163.com", "domain:baidu.com"}
	if len(domains) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(domains), len(want), domains)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, domains[i], want[i])
		}
	}
}

func TestParseSubscriptionHandlesCRLF(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\r\nDOMAIN-SUFFIX,163.com\r\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" || domains[1] != "domain:163.com" {
		t.Errorf("got = %v", domains)
	}
}

// 一条域名都解析不出来必须报错，绝不能返回空数组。
// 空的域名组会让 buildRule 跳过整条规则，流量静默退回直连——
// 这正是订阅源改格式或 URL 失效返回 404 页面时会走到的路径。
func TestParseSubscriptionRejectsEmptyResult(t *testing.T) {
	cases := map[string]string{
		"全是 IP 规则":  "IP-CIDR,1.1.1.1/32\nIP-CIDR6,::1/128\n",
		"全是注释":      "# nothing here\n# really\n",
		"空文本":       "   \n\n  \n",
		"404 HTML": "<!DOCTYPE html>\n<html><body>404: Not Found</body></html>\n",
	}
	for name, raw := range cases {
		if _, _, err := ParseSubscription(raw); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseSubscriptionDeduplicates(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\n.qq.com\nqq.com\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("got = %v, want [domain:qq.com]", domains)
	}
}

func TestParseSubscriptionIgnoresTrailingPolicyField(t *testing.T) {
	domains, _, err := ParseSubscription("DOMAIN-SUFFIX,qq.com,DIRECT\nDOMAIN,a.com,PROXY,no-resolve\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" || domains[1] != "full:a.com" {
		t.Errorf("got = %v", domains)
	}
}

func TestParseSubscriptionRejectsGarbageEntries(t *testing.T) {
	// 含空格、斜杠、协议头的都不是域名，必须跳过而不是原样放进配置
	domains, skipped, err := ParseSubscription("DOMAIN-SUFFIX,qq.com\nhttps://evil.com/path\nhas space\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(domains) != 1 || domains[0] != "domain:qq.com" {
		t.Errorf("got = %v, want [domain:qq.com]", domains)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run TestParseSubscription -v
```

预期：编译失败，`undefined: ParseSubscription`

- [ ] **Step 3: 实现解析器**

创建 `web/service/routing_subscription.go`：

```go
package service

import (
	"strings"

	"a-ui/util/common"
)

// 逐行解析，不做全局格式识别。Surge/Clash classical、Clash YAML、纯域名列表
// 三种格式的行特征互不冲突，逐行判断比先猜格式更健壮：真实订阅文件里混着
// 注释、YAML 头和规则行，全局识别一旦猜错就整份文件解析失败。
//
// 已知的非域名规则类型一律跳过并计数。不尝试翻译成 xray 的其他条件——
// 域名组这个概念只承载域名，把 IP 规则塞进来需要动整个数据模型。
var skippedRuleTypes = map[string]bool{
	"IP-CIDR": true, "IP-CIDR6": true, "IP-ASN": true, "GEOIP": true,
	"SRC-IP-CIDR": true, "SRC-PORT": true, "DST-PORT": true,
	"PROCESS-NAME": true, "PROCESS-PATH": true, "USER-AGENT": true,
	"URL-REGEX": true, "RULE-SET": true, "SUB-DOMAIN": true,
	"DOMAIN-WILDCARD": true, "AND": true, "OR": true, "NOT": true,
	"PROTOCOL": true, "NETWORK": true, "IN-PORT": true,
}

// ParseSubscription 把订阅文件文本解析成 xray 域名语法。
// 返回（域名列表, 跳过的非域名条数, 错误）。
//
// 解析不出任何域名时返回错误而非空列表：调用方据此保留上一次成功的数据。
// 若在这里返回空列表，上游改格式或 URL 失效返回 404 页面时，域名组会被清空，
// 引用它的规则被 buildRule 跳过，流量静默退回直连。
func ParseSubscription(raw string) ([]string, int, error) {
	domains := make([]string, 0, 256)
	seen := make(map[string]bool, 256)
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

		converted, ok := convertSubscriptionLine(item)
		if !ok {
			skipped++
			continue
		}
		if seen[converted] {
			continue
		}
		seen[converted] = true
		domains = append(domains, converted)
	}

	if len(domains) == 0 {
		return nil, skipped, common.NewError(
			"订阅内容里没有解析出任何域名（跳过了", skipped, "条非域名规则）")
	}
	return domains, skipped, nil
}

// convertSubscriptionLine 把一行转成 xray 域名语法。第二个返回值为 false
// 表示这行应当被跳过并计数。
func convertSubscriptionLine(item string) (string, bool) {
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
			return domainRule("domain:", value)
		case "DOMAIN":
			return domainRule("full:", value)
		case "DOMAIN-KEYWORD":
			// xray 的裸域名就是子串匹配，与 DOMAIN-KEYWORD 语义一致。
			// 会误伤（ads 命中 downloads.example.com），但那是这个规则类型
			// 在 Shadowrocket/Clash 里的固有行为，不是本实现引入的偏差。
			if !isValidKeyword(value) {
				return "", false
			}
			return value, true
		default:
			if skippedRuleTypes[ruleType] {
				return "", false
			}
			// 不认识的类型一律跳过，绝不猜测
			return "", false
		}
	}
	// 纯域名列表：.example.com / +.example.com / *.example.com / example.com
	// 这类列表的惯例是后缀匹配
	return domainRule("domain:", item)
}

func domainRule(prefix, value string) (string, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "+.")
	value = strings.TrimPrefix(value, "*.")
	value = strings.TrimPrefix(value, ".")
	value = strings.TrimSuffix(value, ".")
	if !isValidDomain(value) {
		return "", false
	}
	return prefix + strings.ToLower(value), true
}

// isValidDomain 只做防呆，不追求 RFC 完备：拦住 URL、带空格的说明文字、
// HTML 片段这些明显不是域名的东西，避免它们原样进入 xray 配置。
func isValidDomain(s string) bool {
	if s == "" || len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	if strings.ContainsAny(s, " \t/\\:?#@<>\"'()[]{}|,") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" {
			return false
		}
		for _, r := range label {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			// 允许非 ASCII：xray 接受中文等国际化域名
			if !isAlnum && r != '-' && r != '_' && r < 128 {
				return false
			}
		}
	}
	return true
}

// isValidKeyword 比域名宽松（关键词不含点也合法），但仍要拦住空白与分隔符。
func isValidKeyword(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	return !strings.ContainsAny(s, " \t/\\:?#@<>\"'()[]{}|,")
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run TestParseSubscription -v
```

预期：8 个测试全部 PASS

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_subscription.go web/service/routing_subscription_test.go
git commit -m "feat(routing): 订阅内容解析器，支持 Surge/Clash/纯域名三种格式"
```

---

### Task 3: HTTP 拉取

带超时与体积上限的拉取。用 `httptest` 测，不依赖外网。

**Files:**
- Modify: `web/service/routing_subscription.go`
- Test: `web/service/routing_subscription_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `service.ValidateSubscribeURL(raw string) error`
  - `service.fetchSubscription(rawURL string) (string, error)`（包内私有）

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_subscription_test.go`（顶部 import 补 `net/http`、`net/http/httptest`、`strings`）：

```go
func TestValidateSubscribeURLAcceptsHttpAndHttps(t *testing.T) {
	for _, u := range []string{"http://example.com/a.list", "https://example.com/a.list"} {
		if err := ValidateSubscribeURL(u); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
}

func TestValidateSubscribeURLRejectsOtherSchemes(t *testing.T) {
	for _, u := range []string{"ftp://example.com/a", "file:///etc/passwd", "example.com/a", ""} {
		if err := ValidateSubscribeURL(u); err == nil {
			t.Errorf("%s: expected error, got nil", u)
		}
	}
}

func TestFetchSubscriptionReadsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer srv.Close()

	body, err := fetchSubscription(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, "qq.com") {
		t.Errorf("body = %q", body)
	}
}

func TestFetchSubscriptionRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if _, err := fetchSubscription(srv.URL); err == nil {
		t.Error("expected error for 404, got nil")
	}
}

// 不设上限的话一个大文件就能把面板打爆，而 cron 没有 panic 恢复，
// OOM 会杀掉整个面板进程。
func TestFetchSubscriptionRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 11; i++ {
			w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	if _, err := fetchSubscription(srv.URL); err == nil {
		t.Error("expected error for oversized body, got nil")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestValidateSubscribeURL|TestFetchSubscription' -v
```

预期：编译失败，`undefined: ValidateSubscribeURL` / `undefined: fetchSubscription`

- [ ] **Step 3: 实现拉取**

追加到 `web/service/routing_subscription.go`（import 补 `io`、`net/http`、`net/url`、`time`）：

```go
const (
	subscriptionTimeout  = 30 * time.Second
	subscriptionMaxBytes = 10 << 20 // 10 MB，约 30 万条域名
)

// ValidateSubscribeURL 在保存表单时就拦住非法地址，不必等到拉取时才报错。
//
// 只限制 scheme。不拦内网地址：本面板是单管理员系统，管理员本就有 shell 级
// 权限，拦截 SSRF 换不来实际的安全收益，却会挡住「订阅局域网里自建的列表」
// 这种合理用法。
func ValidateSubscribeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return common.NewError("订阅地址无法解析:", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return common.NewError("订阅地址必须以 http:// 或 https:// 开头:", raw)
	}
	if u.Host == "" {
		return common.NewError("订阅地址缺少主机名:", raw)
	}
	return nil
}

// fetchSubscription 拉取订阅内容。超时与体积上限都是硬限制。
func fetchSubscription(rawURL string) (string, error) {
	if err := ValidateSubscribeURL(rawURL); err != nil {
		return "", err
	}

	client := &http.Client{Timeout: subscriptionTimeout}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", common.NewError("拉取订阅失败:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", common.NewError("订阅地址返回状态码:", resp.StatusCode)
	}

	// 多读 1 字节用于判断是否超限：LimitReader 本身只会静默截断，
	// 截断后的内容解析出来是残缺的域名列表，比直接失败更危险。
	body, err := io.ReadAll(io.LimitReader(resp.Body, subscriptionMaxBytes+1))
	if err != nil {
		return "", common.NewError("读取订阅内容失败:", err)
	}
	if len(body) > subscriptionMaxBytes {
		return "", common.NewError("订阅内容超过 10MB 上限")
	}
	return string(body), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestValidateSubscribeURL|TestFetchSubscription' -v
```

预期：5 个测试全部 PASS

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_subscription.go web/service/routing_subscription_test.go
git commit -m "feat(routing): 订阅拉取，带 30s 超时与 10MB 体积上限"
```

---
### Task 4: 更新编排与失败策略

把拉取、解析、校验、落库串起来。**本任务是整个功能的安全核心**——失败时必须保留旧数据。

**Files:**
- Modify: `web/service/routing_domain.go`（`Refresh`、`Update`、`decodeSubscribedDomains`）
- Test: `web/service/routing_subscription_test.go`

**Interfaces:**
- Consumes: `fetchSubscription`、`ParseSubscription`（Task 2、3）、`ValidateDomains`（现有，`routing_validate.go`）、`EncodeDomains`/`DecodeDomains`（现有）
- Produces:
  - `(*DomainGroupService).Refresh(id int) error`
  - `service.DecodeSubscribedDomains(encoded string) ([]string, error)`
  - `(*DomainGroupService).Update` 行为变更：`SubscribeUrl` 改变时清空订阅数据

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_subscription_test.go`（import 补 `a-ui/database`、`a-ui/database/model`）：

```go
func TestRefreshWritesSubscribedDomainsOnSuccess(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\nDOMAIN-SUFFIX,163.com\nIP-CIDR,1.1.1.1/32\n"))
	}))
	defer srv.Close()

	group := &model.DomainGroup{Remark: "国内", Domains: "[]", SubscribeUrl: srv.URL}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	if err := s.Refresh(group.Id); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	got, err := s.Get(group.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	domains, err := DecodeSubscribedDomains(got.SubscribedDomains)
	if err != nil {
		t.Fatalf("DecodeSubscribedDomains: %v", err)
	}
	if len(domains) != 2 || domains[0] != "domain:qq.com" {
		t.Errorf("domains = %v", domains)
	}
	if got.LastUpdatedAt == 0 {
		t.Error("LastUpdatedAt should be set")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
	if got.LastSkipped != 1 {
		t.Errorf("LastSkipped = %d, want 1", got.LastSkipped)
	}
}

// 失败时清空订阅域名会让合并结果为空、规则被 buildRule 跳过、
// 流量静默退回直连。这是本功能最危险的失败模式，必须锁死。
func TestRefreshKeepsOldDataOnFailure(t *testing.T) {
	setupDB(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }},
		{"解析为空", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<!DOCTYPE html><html>404: Not Found</html>"))
		}},
		{"空响应", func(w http.ResponseWriter, r *http.Request) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			const oldData = `["domain:old.com"]`
			group := &model.DomainGroup{
				Remark: tc.name, Domains: "[]", SubscribeUrl: srv.URL,
				SubscribedDomains: oldData, LastUpdatedAt: 1234567890,
			}
			if err := database.GetDB().Save(group).Error; err != nil {
				t.Fatalf("save group: %v", err)
			}

			s := &DomainGroupService{}
			if err := s.Refresh(group.Id); err == nil {
				t.Fatal("expected error, got nil")
			}

			got, err := s.Get(group.Id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.SubscribedDomains != oldData {
				t.Errorf("SubscribedDomains = %q, want %q (旧数据必须保留)",
					got.SubscribedDomains, oldData)
			}
			if got.LastUpdatedAt != 1234567890 {
				t.Errorf("LastUpdatedAt = %d, 失败不应改动成功时间", got.LastUpdatedAt)
			}
			if got.LastError == "" {
				t.Error("LastError 必须写入，否则管理员看不到订阅已经坏了")
			}
		})
	}
}

func TestRefreshRejectsGroupWithoutUrl(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{Remark: "无订阅", Domains: `["domain:a.com"]`}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}
	if err := (&DomainGroupService{}).Refresh(group.Id); err == nil {
		t.Error("expected error for group without subscribe url")
	}
}

// 从「国内域名合集」改成「广告拦截列表」之后，旧域名继续按新规则的动作生效
// 是一次用错误的数据分流，比规则暂时不生效更危险。
func TestUpdateClearsSubscribedDataWhenUrlChanges(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "组", Domains: "[]", SubscribeUrl: "http://a.example.com/list",
		SubscribedDomains: `["domain:old.com"]`, LastUpdatedAt: 111, LastSkipped: 5,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	err := s.Update(&model.DomainGroup{
		Id: group.Id, Remark: "组", Domains: "[]",
		SubscribeUrl: "http://b.example.com/list",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(group.Id)
	if got.SubscribedDomains != "" {
		t.Errorf("SubscribedDomains = %q, want empty", got.SubscribedDomains)
	}
	if got.LastUpdatedAt != 0 {
		t.Errorf("LastUpdatedAt = %d, want 0 (触发立即重新拉取)", got.LastUpdatedAt)
	}
	if got.LastSkipped != 0 {
		t.Errorf("LastSkipped = %d, want 0", got.LastSkipped)
	}
}

func TestUpdateKeepsSubscribedDataWhenUrlUnchanged(t *testing.T) {
	setupDB(t)
	group := &model.DomainGroup{
		Remark: "组", Domains: "[]", SubscribeUrl: "http://a.example.com/list",
		SubscribedDomains: `["domain:old.com"]`, LastUpdatedAt: 111,
	}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	s := &DomainGroupService{}
	err := s.Update(&model.DomainGroup{
		Id: group.Id, Remark: "改了备注", Domains: `["domain:manual.com"]`,
		SubscribeUrl: "http://a.example.com/list",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(group.Id)
	if got.SubscribedDomains != `["domain:old.com"]` {
		t.Errorf("SubscribedDomains = %q, 地址没变不该清空", got.SubscribedDomains)
	}
	if got.LastUpdatedAt != 111 {
		t.Errorf("LastUpdatedAt = %d, want 111", got.LastUpdatedAt)
	}
	if got.Remark != "改了备注" || got.Domains != `["domain:manual.com"]` {
		t.Errorf("备注与手工域名应当被更新: %+v", got)
	}
}

func TestDecodeSubscribedDomainsToleratesEmpty(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		got, err := DecodeSubscribedDomains(raw)
		if err != nil {
			t.Errorf("%q: unexpected error %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("%q: got %v, want empty", raw, got)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestRefresh|TestUpdateClears|TestUpdateKeeps|TestDecodeSubscribed' -v
```

预期：编译失败，`undefined: DecodeSubscribedDomains` 等

- [ ] **Step 3: 实现 DecodeSubscribedDomains**

追加到 `web/service/routing_domain.go`：

```go
// DecodeSubscribedDomains 容忍空字符串——没订阅过的组这个字段本来就是空的，
// 直接交给 DecodeDomains 会得到一个 json 语法错误，进而被 buildRule 当成
// 「数据损坏」丢弃整条规则。
func DecodeSubscribedDomains(encoded string) ([]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	return DecodeDomains(encoded)
}
```

- [ ] **Step 4: 实现 Refresh**

追加到 `web/service/routing_domain.go`（import 补 `sync`、`time`、`a-ui/logger`）：

```go
// subscriptionMu 串行化所有订阅更新。定时任务与管理员点「立即更新」可能同时
// 发生。更新是分钟级的低频操作，不值得做更细的按组加锁。
var subscriptionMu sync.Mutex

// Refresh 立即更新一个域名组的订阅内容。
func (s *DomainGroupService) Refresh(id int) error {
	subscriptionMu.Lock()
	defer subscriptionMu.Unlock()

	group, err := s.Get(id)
	if err != nil {
		return err
	}
	return s.refreshLocked(group)
}

// refreshLocked 假定调用方已持有 subscriptionMu。
func (s *DomainGroupService) refreshLocked(group *model.DomainGroup) error {
	if group.SubscribeUrl == "" {
		return common.NewError("该域名组没有配置订阅地址, id:", group.Id)
	}

	raw, err := fetchSubscription(group.SubscribeUrl)
	if err != nil {
		return s.recordFailure(group, err)
	}
	domains, skipped, err := ParseSubscription(raw)
	if err != nil {
		return s.recordFailure(group, err)
	}
	// 落库前过真实 xray 校验。ValidateDomains 自身是 fail open 的：
	// 二进制缺失、超时等一律放行，只有 xray 明确判定非法才拦。
	if err := ValidateDomains(domains); err != nil {
		return s.recordFailure(group, err)
	}
	encoded, err := EncodeDomains(domains)
	if err != nil {
		return s.recordFailure(group, err)
	}

	// 用 map 而不是 struct：GORM 的 struct 更新会跳过零值，
	// LastError 与 LastSkipped 清不掉。
	return database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Updates(map[string]any{
			"subscribed_domains": encoded,
			"last_updated_at":    time.Now().UnixMilli(),
			"last_error":         "",
			"last_skipped":       skipped,
		}).Error
}

// recordFailure 只写失败原因，绝不动 SubscribedDomains 与 LastUpdatedAt。
//
// 清空订阅域名会让合并结果为空 → buildRule 跳过整条规则 → 本该走指定节点或被
// 封禁的流量静默退回直连。上游改格式、URL 失效返回 404 页面、CDN 返回空响应
// 都会走到这里，而它们都不该导致分流失效。宁可用旧数据。
func (s *DomainGroupService) recordFailure(group *model.DomainGroup, cause error) error {
	err := database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Update("last_error", cause.Error()).Error
	if err != nil {
		logger.Warning("record subscription failure err:", err)
	}
	logger.Warning("refresh subscription failed, id:", group.Id,
		"remark:", group.Remark, "err:", cause)
	return cause
}
```

- [ ] **Step 5: 改 Update 处理订阅地址变更**

替换 `web/service/routing_domain.go` 里现有的 `Update`：

```go
func (s *DomainGroupService) Update(group *model.DomainGroup) error {
	old, err := s.Get(group.Id)
	if err != nil {
		return err
	}
	old.Remark = group.Remark
	old.Domains = group.Domains

	// 订阅地址变了：旧订阅内容来自另一个来源，继续拿它分流是「用错误的数据
	// 生效」，比规则暂时不生效更危险。清空并把 LastUpdatedAt 置 0，
	// SubscriptionJob 的「从未成功过」分支会在下一个检查窗口拉取新地址。
	if old.SubscribeUrl != group.SubscribeUrl {
		old.SubscribeUrl = group.SubscribeUrl
		old.SubscribedDomains = ""
		old.LastUpdatedAt = 0
		old.LastError = ""
		old.LastSkipped = 0
	}

	return database.GetDB().Save(old).Error
}
```

- [ ] **Step 6: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestRefresh|TestUpdateClears|TestUpdateKeeps|TestDecodeSubscribed' -v
```

预期：全部 PASS（`TestRefreshKeepsOldDataOnFailure` 的 3 个子测试都要过）

- [ ] **Step 7: 提交**

```bash
git add web/service/routing_domain.go web/service/routing_subscription_test.go
git commit -m "feat(routing): 订阅更新编排，失败时保留上次成功的数据"
```

---

### Task 5: 注入器合并订阅域名

让生成的 xray 配置真正用上订阅内容。

**Files:**
- Modify: `web/service/routing_inject.go`（`buildRule`）
- Test: `web/service/routing_inject_test.go`

**Interfaces:**
- Consumes: `MergeDomains`（Task 1）、`DecodeSubscribedDomains`（Task 4）
- Produces: 无新导出符号，`buildRule` 行为变更

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_inject_test.go`：

```go
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
	rule := &model.RoutingRule{DomainGroupId: group.Id, Action: model.ActionBlock, Enable: true}
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
	rule := &model.RoutingRule{DomainGroupId: group.Id, Action: model.ActionBlock, Enable: true}
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
	rule := &model.RoutingRule{DomainGroupId: group.Id, Action: model.ActionBlock, Enable: true}
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
	rule := &model.RoutingRule{DomainGroupId: group.Id, Action: model.ActionBlock, Enable: true}
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestBuildRuleMerges|TestBuildRuleWorksWithOnly|TestBuildRuleSkipsWhenBoth|TestInjectIsByteDeterministic' -v
```

预期：`TestBuildRuleMergesManualAndSubscribedDomains` FAIL（只有 1 条手工域名），其余可能已通过

- [ ] **Step 3: 改 buildRule**

修改 `web/service/routing_inject.go` 的 `buildRule`，把取域名那三行替换掉：

```go
	group, err := s.domainGroupService.Get(rule.DomainGroupId)
	if err != nil {
		return nil, false, common.NewError("域名组不存在, id:", rule.DomainGroupId)
	}
	manual, err := DecodeDomains(group.Domains)
	if err != nil {
		return nil, false, common.NewError("域名组数据损坏, id:", rule.DomainGroupId, "err:", err)
	}
	subscribed, err := DecodeSubscribedDomains(group.SubscribedDomains)
	if err != nil {
		return nil, false, common.NewError("域名组订阅数据损坏, id:", rule.DomainGroupId, "err:", err)
	}
	// 合并顺序确定（手工在前、订阅在后、保留首次出现），
	// 这是「生成逐字节确定」不变量的一部分。
	domains := MergeDomains(manual, subscribed)
	if len(domains) == 0 {
		return nil, false, common.NewError("域名组为空, id:", rule.DomainGroupId,
			"（域名条件为空会让规则退化成劫持该入站全部流量）")
	}
```

同时更新 `buildRule` 上方的文档注释，说明域名来自两个来源的合并。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestBuildRule|TestInject' -v
```

预期：全部 PASS，包括既有的跳过防线测试

- [ ] **Step 5: 跑整包回归**

```bash
go build ./... && go test ./web/service/ -v
```

预期：全部 PASS

- [ ] **Step 6: 提交**

```bash
git add web/service/routing_inject.go web/service/routing_inject_test.go
git commit -m "feat(routing): 注入器合并手工域名与订阅域名"
```

---

### Task 6: 订阅更新时间设置项

**Files:**
- Modify: `web/service/setting.go`（`defaultValueMap`、getter）
- Modify: `web/entity/entity.go`（`AllSetting`、`CheckValid`）
- Test: `web/service/routing_subscription_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - 设置 key `subscriptionUpdateTime`，默认 `"04:00"`
  - `(*SettingService).GetSubscriptionUpdateTime() (string, error)`
  - `entity.AllSetting.SubscriptionUpdateTime string`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_subscription_test.go`（import 补 `a-ui/web/entity`）：

```go
func TestSubscriptionUpdateTimeDefault(t *testing.T) {
	setupDB(t)
	got, err := (&SettingService{}).GetSubscriptionUpdateTime()
	if err != nil {
		t.Fatalf("GetSubscriptionUpdateTime: %v", err)
	}
	if got != "04:00" {
		t.Errorf("got = %q, want 04:00", got)
	}
}

func TestAllSettingRejectsBadUpdateTime(t *testing.T) {
	base := func(v string) *entity.AllSetting {
		return &entity.AllSetting{
			WebPort: 54321, WebBasePath: "/", TimeLocation: "Asia/Shanghai",
			// CheckValid 会先 json.Unmarshal 这个字段，空字符串会让它在
			// 到达时间格式校验之前就报错，测出来的就不是我们要测的东西了。
			XrayTemplateConfig:     "{}",
			SubscriptionUpdateTime: v,
		}
	}
	for _, bad := range []string{"25:00", "4:00pm", "0400", "", "04:60", "abc"} {
		if err := base(bad).CheckValid(); err == nil {
			t.Errorf("%q: expected error, got nil", bad)
		}
	}
	for _, good := range []string{"04:00", "00:00", "23:59"} {
		if err := base(good).CheckValid(); err != nil {
			t.Errorf("%q: unexpected error %v", good, err)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestSubscriptionUpdateTime|TestAllSettingRejects' -v
```

预期：编译失败，`undefined: GetSubscriptionUpdateTime`

- [ ] **Step 3: 加默认值与 getter**

在 `web/service/setting.go` 的 `defaultValueMap` 里追加一行：

```go
	"timeLocation":           "Asia/Shanghai",
	"subscriptionUpdateTime": "04:00",
}
```

在文件里其他 getter 旁边追加：

```go
// GetSubscriptionUpdateTime 返回域名组订阅的每日更新时刻，格式 HH:MM。
func (s *SettingService) GetSubscriptionUpdateTime() (string, error) {
	return s.getString("subscriptionUpdateTime")
}
```

- [ ] **Step 4: 加设置字段与校验**

在 `web/entity/entity.go` 的 `AllSetting` 里追加字段：

```go
	TimeLocation string `json:"timeLocation" form:"timeLocation"`

	SubscriptionUpdateTime string `json:"subscriptionUpdateTime" form:"subscriptionUpdateTime"`
}
```

在 `CheckValid()` 末尾 `return nil` 之前追加（import 补 `time`）：

```go
	// 用 time.Parse 而不是手写正则：标准库负责格式与范围，
	// 25:00 / 04:60 这类越界值它会直接拒绝。
	if _, err := time.Parse("15:04", s.SubscriptionUpdateTime); err != nil {
		return common.NewError("订阅更新时间格式不正确，应为 HH:MM:", s.SubscriptionUpdateTime)
	}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestSubscriptionUpdateTime|TestAllSettingRejects' -v
```

预期：2 个测试 PASS

- [ ] **Step 6: 提交**

```bash
git add web/service/setting.go web/entity/entity.go web/service/routing_subscription_test.go
git commit -m "feat(setting): 新增订阅更新时间设置项"
```

---

### Task 7: 调度判断与定时任务

**Files:**
- Modify: `web/service/routing_domain.go`（`RefreshDue`）
- Modify: `web/service/routing_subscription.go`（`ShouldUpdateNow`）
- Create: `web/job/subscription_job.go`
- Modify: `web/web.go`（`startTask`）
- Test: `web/service/routing_subscription_test.go`

**Interfaces:**
- Consumes: `Refresh`（Task 4）、`GetSubscriptionUpdateTime`（Task 6）
- Produces:
  - `service.ShouldUpdateNow(now time.Time, lastUpdatedAt int64, hour, minute int) bool`
  - `(*DomainGroupService).RefreshDue() (int, error)`
  - `job.NewSubscriptionJob() *SubscriptionJob`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_subscription_test.go`（import 补 `time`）：

```go
func TestShouldUpdateNow(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	today := func(h, m int) time.Time {
		return time.Date(2026, 9, 2, h, m, 0, 0, loc)
	}
	ms := func(tm time.Time) int64 { return tm.UnixMilli() }

	cases := []struct {
		name          string
		now           time.Time
		lastUpdatedAt int64
		want          bool
	}{
		{"从未成功过，未到时间点也要更新", today(1, 0), 0, true},
		{"已过时间点且今天没更新过", today(5, 0), ms(today(4, 0).AddDate(0, 0, -1)), true},
		{"已过时间点但今天更新过", today(5, 0), ms(today(4, 30)), false},
		{"未到时间点", today(3, 0), ms(today(0, 0).AddDate(0, 0, -1)), false},
		{"恰好到点", today(4, 0), ms(today(0, 0).AddDate(0, 0, -1)), true},
		{"今天更晚时候手动更新过", today(23, 0), ms(today(10, 0)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldUpdateNow(tc.now, tc.lastUpdatedAt, 4, 0)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefreshDueOnlyTouchesGroupsWithUrl(t *testing.T) {
	setupDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer srv.Close()

	withURL := &model.DomainGroup{Remark: "订阅组", Domains: "[]", SubscribeUrl: srv.URL}
	plain := &model.DomainGroup{Remark: "手工组", Domains: `["domain:a.com"]`}
	for _, g := range []*model.DomainGroup{withURL, plain} {
		if err := database.GetDB().Save(g).Error; err != nil {
			t.Fatalf("save group: %v", err)
		}
	}

	s := &DomainGroupService{}
	count, err := s.RefreshDue()
	if err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	got, _ := s.Get(withURL.Id)
	if got.SubscribedDomains == "" {
		t.Error("订阅组应当被更新")
	}
	gotPlain, _ := s.Get(plain.Id)
	if gotPlain.SubscribedDomains != "" || gotPlain.LastUpdatedAt != 0 {
		t.Error("没有订阅地址的组不应被碰")
	}
}

// 一个组拉取失败不能拖垮其余组。
func TestRefreshDueContinuesAfterFailure(t *testing.T) {
	setupDB(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,qq.com\n"))
	}))
	defer good.Close()

	g1 := &model.DomainGroup{Remark: "坏", Domains: "[]", SubscribeUrl: bad.URL}
	g2 := &model.DomainGroup{Remark: "好", Domains: "[]", SubscribeUrl: good.URL}
	for _, g := range []*model.DomainGroup{g1, g2} {
		if err := database.GetDB().Save(g).Error; err != nil {
			t.Fatalf("save group: %v", err)
		}
	}

	s := &DomainGroupService{}
	count, err := s.RefreshDue()
	if err != nil {
		t.Fatalf("RefreshDue 不应因单个组失败而整体报错: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (只有好的那个算成功)", count)
	}
	if got, _ := s.Get(g2.Id); got.SubscribedDomains == "" {
		t.Error("好的组应当被更新")
	}
	if got, _ := s.Get(g1.Id); got.LastError == "" {
		t.Error("坏的组应当记录失败原因")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestShouldUpdateNow|TestRefreshDue' -v
```

预期：编译失败，`undefined: ShouldUpdateNow` / `undefined: RefreshDue`

- [ ] **Step 3: 实现 ShouldUpdateNow**

追加到 `web/service/routing_subscription.go`：

```go
// ShouldUpdateNow 判断某个域名组现在是否该更新。
//
// 这里刻意不按配置时间去建 cron entry，而是固定间隔跑、每次自己判断。
// 换来两个收益：改更新时间立即生效，不必走 SIGHUP 重启面板（cron entry 是在
// Server.Start 里注册的）；面板重启若恰好跨过时间点，也会在下一个检查窗口补上。
//
// lastUpdatedAt == 0 表示从未成功过，此时立即更新、不等时间点——新建的订阅组
// 否则会一直空到第二天凌晨，而空域名组会让引用它的规则被 buildRule 跳过。
func ShouldUpdateNow(now time.Time, lastUpdatedAt int64, hour, minute int) bool {
	if lastUpdatedAt == 0 {
		return true
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if now.Before(target) {
		return false
	}
	// 上次成功早于今天的时间点，说明今天这一轮还没跑过
	return time.UnixMilli(lastUpdatedAt).In(now.Location()).Before(target)
}
```

- [ ] **Step 4: 实现 RefreshDue**

追加到 `web/service/routing_domain.go`：

```go
// RefreshDue 更新所有到点的订阅域名组，返回成功更新的个数。
//
// 单个组失败不影响其余组：失败原因已由 recordFailure 落库，返回 error 会让
// 一个坏掉的订阅地址把整批更新都挡住。只有取配置这种全局性失败才返回 error。
func (s *DomainGroupService) RefreshDue() (int, error) {
	settingService := SettingService{}
	raw, err := settingService.GetSubscriptionUpdateTime()
	if err != nil {
		return 0, err
	}
	at, err := time.Parse("15:04", raw)
	if err != nil {
		return 0, common.NewError("订阅更新时间格式不正确:", raw, "err:", err)
	}
	loc, err := settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}

	groups, err := s.GetAll()
	if err != nil {
		return 0, err
	}

	subscriptionMu.Lock()
	defer subscriptionMu.Unlock()

	now := time.Now().In(loc)
	updated := 0
	for _, group := range groups {
		if group.SubscribeUrl == "" {
			continue
		}
		if !ShouldUpdateNow(now, group.LastUpdatedAt, at.Hour(), at.Minute()) {
			continue
		}
		if err := s.refreshLocked(group); err != nil {
			continue // 失败原因已落库并记日志
		}
		updated++
	}
	return updated, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestShouldUpdateNow|TestRefreshDue' -v
```

预期：全部 PASS

- [ ] **Step 6: 写 job**

创建 `web/job/subscription_job.go`：

```go
package job

import (
	"a-ui/logger"
	"a-ui/web/service"
)

// SubscriptionJob 每 10 分钟检查一次有没有到点该更新的订阅域名组。
//
// 注意：cron 没有 panic 恢复（Server.Start 里的 cron.New 未配 cron.Recover），
// 这个 Run 里的任何 panic 都会杀掉整个面板进程。所有可能失败的操作都必须
// 走 error 返回，不能依赖 recover 兜底。
type SubscriptionJob struct {
	domainGroupService service.DomainGroupService
	xrayService        service.XrayService
}

func NewSubscriptionJob() *SubscriptionJob {
	return new(SubscriptionJob)
}

func (j *SubscriptionJob) Run() {
	count, err := j.domainGroupService.RefreshDue()
	if err != nil {
		logger.Warning("refresh domain group subscriptions err:", err)
		return
	}
	if count > 0 {
		logger.Debugf("refreshed %v domain group subscriptions", count)
		// 复用既有链路：置标志 → InboundController 的 10 秒 cron 消费 →
		// RestartXray(false) → Config.Equals 发现 RouterConfig 变了 → 重启 xray。
		// 管理员不需要重启面板，也不需要重启 xray。
		j.xrayService.SetToNeedRestart()
	}
}
```

- [ ] **Step 7: 注册 job**

在 `web/web.go` 的 `startTask()` 末尾追加：

```go
	// 每 10 秒检查一次 inbound 流量超出和到期的情况
	s.cron.AddJob("@every 30s", job.NewCheckInboundJob())

	// 每 10 分钟检查一次域名组订阅是否到了更新时间
	s.cron.AddJob("@every 10m", job.NewSubscriptionJob())
}
```

- [ ] **Step 8: 编译并跑整包回归**

```bash
go build ./... && go vet ./... && go test ./web/service/ -v
```

预期：编译通过，测试全部 PASS

- [ ] **Step 9: 提交**

```bash
git add web/service/routing_subscription.go web/service/routing_domain.go web/job/subscription_job.go web/web.go web/service/routing_subscription_test.go
git commit -m "feat(routing): 订阅定时更新任务，每 10 分钟自检是否到点"
```

---
### Task 8: 后端接口

列表接口摘要化，新增 `detail` 与 `refresh`，表单接受订阅地址。

**Files:**
- Modify: `web/controller/routing.go`

**Interfaces:**
- Consumes: `Refresh`（Task 4）、`MergeDomains`（Task 1）、`DecodeSubscribedDomains`（Task 4）、`ValidateSubscribeURL`（Task 3）
- Produces:
  - `POST /xui/routing/domain-group/detail/:id` → `domainGroupDetail`
  - `POST /xui/routing/domain-group/refresh/:id` → `jsonMsg`
  - `POST /xui/routing/domain-group/list` 返回 `[]domainGroupSummary`（**响应结构变更**，Task 9 的前端必须同步）

- [ ] **Step 1: 加表单字段与响应结构**

在 `web/controller/routing.go` 的 `domainGroupForm` 加字段，并在其后加两个响应结构：

```go
type domainGroupForm struct {
	Id           int    `json:"id" form:"id"`
	Remark       string `json:"remark" form:"remark"`
	Domains      string `json:"domains" form:"domains"`
	SubscribeUrl string `json:"subscribeUrl" form:"subscribeUrl"`
}

// 列表页只需要摘要。域名组挂上订阅后可能有几万条域名，
// 每次开页面把全量传一遍既没意义，前端渲染几万个 tag 还会卡死浏览器。
const domainGroupPreviewLimit = 5

// 编辑弹窗里订阅域名是只读展示，给个上限避免渲染卡顿。
const subscribedPreviewLimit = 200

type domainGroupSummary struct {
	Id              int      `json:"id"`
	Remark          string   `json:"remark"`
	Preview         []string `json:"preview"`
	EffectiveCount  int      `json:"effectiveCount"`
	ManualCount     int      `json:"manualCount"`
	SubscribedCount int      `json:"subscribedCount"`
	SubscribeUrl    string   `json:"subscribeUrl"`
	LastUpdatedAt   int64    `json:"lastUpdatedAt"`
	LastError       string   `json:"lastError"`
	LastSkipped     int      `json:"lastSkipped"`
}

type domainGroupDetail struct {
	Id                int      `json:"id"`
	Remark            string   `json:"remark"`
	Domains           string   `json:"domains"`
	SubscribeUrl      string   `json:"subscribeUrl"`
	SubscribedPreview []string `json:"subscribedPreview"`
	SubscribedCount   int      `json:"subscribedCount"`
	LastUpdatedAt     int64    `json:"lastUpdatedAt"`
	LastError         string   `json:"lastError"`
	LastSkipped       int      `json:"lastSkipped"`
}

// decodeGroupDomains 解出一个组的手工域名与订阅域名。数据损坏时当作空列表，
// 界面还能显示这个组的其余信息，管理员才有机会去修它。
func decodeGroupDomains(group *model.DomainGroup) (manual, subscribed []string) {
	manual, err := service.DecodeDomains(group.Domains)
	if err != nil {
		manual = nil
	}
	subscribed, err = service.DecodeSubscribedDomains(group.SubscribedDomains)
	if err != nil {
		subscribed = nil
	}
	return manual, subscribed
}
```

- [ ] **Step 2: 改 list 为摘要**

替换 `listDomainGroups`：

```go
func (a *RoutingController) listDomainGroups(c *gin.Context) {
	groups, err := a.domainGroupService.GetAll()
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	summaries := make([]*domainGroupSummary, 0, len(groups))
	for _, group := range groups {
		manual, subscribed := decodeGroupDomains(group)
		merged := service.MergeDomains(manual, subscribed)
		preview := merged
		if len(preview) > domainGroupPreviewLimit {
			preview = preview[:domainGroupPreviewLimit]
		}
		summaries = append(summaries, &domainGroupSummary{
			Id:              group.Id,
			Remark:          group.Remark,
			Preview:         preview,
			EffectiveCount:  len(merged),
			ManualCount:     len(manual),
			SubscribedCount: len(subscribed),
			SubscribeUrl:    group.SubscribeUrl,
			LastUpdatedAt:   group.LastUpdatedAt,
			LastError:       group.LastError,
			LastSkipped:     group.LastSkipped,
		})
	}
	jsonObj(c, summaries, nil)
}
```

- [ ] **Step 3: 加 detail 与 refresh**

追加到 `web/controller/routing.go`：

```go
// detailDomainGroup 供编辑弹窗使用。list 只返回摘要，弹窗要展示的手工域名原文
// 与订阅域名预览没有别的来源。订阅域名全量任何时候都不出现在响应里。
func (a *RoutingController) detailDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	group, err := a.domainGroupService.Get(id)
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	manual, subscribed := decodeGroupDomains(group)
	preview := subscribed
	if len(preview) > subscribedPreviewLimit {
		preview = preview[:subscribedPreviewLimit]
	}
	jsonObj(c, &domainGroupDetail{
		Id:                group.Id,
		Remark:            group.Remark,
		Domains:           strings.Join(manual, "\n"),
		SubscribeUrl:      group.SubscribeUrl,
		SubscribedPreview: preview,
		SubscribedCount:   len(subscribed),
		LastUpdatedAt:     group.LastUpdatedAt,
		LastError:         group.LastError,
		LastSkipped:       group.LastSkipped,
	}, nil)
}

func (a *RoutingController) refreshDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "更新订阅", err)
		return
	}
	err = a.domainGroupService.Refresh(id)
	jsonMsg(c, "更新订阅", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}
```

注册路由，在 `initRouter` 的 `dg` 分组里追加：

```go
	dg := g.Group("/domain-group")
	dg.POST("/list", a.listDomainGroups)
	dg.POST("/detail/:id", a.detailDomainGroup)
	dg.POST("/add", a.addDomainGroup)
	dg.POST("/update/:id", a.updateDomainGroup)
	dg.POST("/del/:id", a.delDomainGroup)
	dg.POST("/refresh/:id", a.refreshDomainGroup)
```

import 补 `strings`。

- [ ] **Step 4: 让 add/update 接受并校验订阅地址**

`addDomainGroup` 与 `updateDomainGroup` 里，在 `encodeDomainsFromForm` 之后、构造 `model.DomainGroup` 之前插入校验，并把 `SubscribeUrl` 带上。

`addDomainGroup` 改为：

```go
func (a *RoutingController) addDomainGroup(c *gin.Context) {
	form := &domainGroupForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "添加域名组", err)
		return
	}
	encoded, err := encodeDomainsFromForm(form.Domains)
	if err != nil {
		jsonMsg(c, "添加域名组", err)
		return
	}
	// 保存时只校验格式，不去拉取——一个慢地址会把这个 HTTP 请求挂满 30 秒。
	// 内容由管理员点「立即更新」或定时任务拉取。
	if form.SubscribeUrl != "" {
		if err := service.ValidateSubscribeURL(form.SubscribeUrl); err != nil {
			jsonMsg(c, "添加域名组", err)
			return
		}
	}
	group := &model.DomainGroup{
		Remark: form.Remark, Domains: encoded, SubscribeUrl: form.SubscribeUrl,
	}
	err = a.domainGroupService.Add(group)
	jsonMsg(c, "添加域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}
```

`updateDomainGroup` 同样插入这段校验，并把传给 `Update` 的结构体改为：

```go
	err = a.domainGroupService.Update(&model.DomainGroup{
		Id: id, Remark: form.Remark, Domains: encoded, SubscribeUrl: form.SubscribeUrl,
	})
```

**注意**：手工域名 textarea 现在可以为空（组里可能只有订阅）。`encodeDomainsFromForm` 走的 `ParseDomains` 会对空输入报「域名列表不能为空」。改 `encodeDomainsFromForm`，允许空输入返回 `"[]"`：

```go
// encodeDomainsFromForm 把 textarea 原文校验并转成入库格式。
// 允许为空：域名组可以只有订阅内容，手工域名一条不填。
// 合并后仍为空的组，其规则会被 buildRule 跳过并记 warning，防线不受影响。
func encodeDomainsFromForm(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "[]", nil
	}
	list, err := service.ParseDomains(raw)
	if err != nil {
		return "", err
	}
	if err := service.ValidateDomains(list); err != nil {
		return "", err
	}
	return service.EncodeDomains(list)
}
```

- [ ] **Step 5: 编译并手动验证接口**

```bash
go build ./... && go vet ./...
```

预期：编译通过。

```bash
XUI_DEBUG=true go run main.go
```

另开一个终端，登录后用浏览器开发者工具或 curl 检查 `POST /xui/routing/domain-group/list` 返回的是摘要结构（含 `effectiveCount`、`subscribeUrl` 字段，不含全量 `domains`）。

- [ ] **Step 6: 提交**

```bash
git add web/controller/routing.go
git commit -m "feat(routing): 域名组接口支持订阅地址、详情与立即更新"
```

---

### Task 9: 分流页前端

**Files:**
- Modify: `web/assets/js/model/routing.js`（`DomainGroup` 类）
- Modify: `web/html/xui/routing.html`
- Test: `web/html_test.go`（既有测试，不改）

**Interfaces:**
- Consumes: Task 8 的 `domainGroupSummary` / `domainGroupDetail` 响应结构

> **调试提醒**：`web/assets/**` 走 `max-age=31536000` 强缓存，版本号不变时浏览器不会重新拉取。本任务改了 `routing.js`，务必用 `XUI_DEBUG=true go run main.go` 启动并在浏览器里硬刷新（macOS: Cmd+Shift+R）。

- [ ] **Step 1: 改 DomainGroup 模型类**

替换 `web/assets/js/model/routing.js` 的 `DomainGroup` 类。它现在承载的是列表摘要，不再是完整域名数组：

```js
// 列表接口返回的是摘要：域名组挂上订阅后可能有几万条域名，
// 全量传给前端既没意义，渲染几万个 tag 还会卡死浏览器。
// 编辑弹窗需要的手工域名原文由 detail 接口单独取。
class DomainGroup {
    constructor(json = {}) {
        this.id = json.id || 0;
        this.remark = json.remark || "";
        this.preview = json.preview || [];
        this.effectiveCount = json.effectiveCount || 0;
        this.manualCount = json.manualCount || 0;
        this.subscribedCount = json.subscribedCount || 0;
        this.subscribeUrl = json.subscribeUrl || "";
        this.lastUpdatedAt = json.lastUpdatedAt || 0;
        this.lastError = json.lastError || "";
        this.lastSkipped = json.lastSkipped || 0;
    }

    static fromJson(json = {}) {
        return new DomainGroup(json);
    }

    get subscribed() {
        return this.subscribeUrl !== "";
    }

    // 订阅状态：未订阅 / 失败 / 等待首次拉取 / 成功
    get subscribeState() {
        if (!this.subscribed) return "none";
        if (this.lastError) return "error";
        if (!this.lastUpdatedAt) return "pending";
        return "ok";
    }
}
```

- [ ] **Step 2: 改域名列渲染**

`web/html/xui/routing.html` 第 64-66 行附近，把 `v-for` 遍历全量域名改为摘要展示：

```html
<template slot="domains" slot-scope="text, group">
    <a-tag v-for="d in group.preview" :key="d">[[ d ]]</a-tag>
    <span v-if="group.effectiveCount > group.preview.length" style="color: #888;">
        +[[ group.effectiveCount - group.preview.length ]]
    </span>
    <span v-if="group.effectiveCount === 0" style="color: #f5222d;">
        空（引用它的规则不会写进配置）
    </span>
</template>
```

- [ ] **Step 3: 加订阅列**

在 `groupColumns` 里，「域名」与「操作」之间插入一列：

```js
groupColumns: [
    { title: '备注', dataIndex: 'remark', width: 160 },
    { title: '域名', scopedSlots: { customRender: 'domains' } },
    { title: '订阅', width: 220, scopedSlots: { customRender: 'subscribe' } },
    { title: '操作', width: 140, scopedSlots: { customRender: 'action' } },
],
```

在域名组表格里加对应的 slot（与 `domains` slot 相邻）：

```html
<template slot="subscribe" slot-scope="text, group">
    <span v-if="group.subscribeState === 'none'" style="color: #bbb;">—</span>
    <a-tooltip v-else-if="group.subscribeState === 'error'" :title="group.lastError">
        <a-tag color="red">拉取失败</a-tag>
    </a-tooltip>
    <a-tag v-else-if="group.subscribeState === 'pending'" color="orange">等待首次拉取</a-tag>
    <a-tooltip v-else :title="'共 ' + group.subscribedCount + ' 条，已忽略 ' + group.lastSkipped + ' 条非域名规则'">
        <a-tag color="green">[[ relativeTime(group.lastUpdatedAt) ]]</a-tag>
    </a-tooltip>
</template>
```

在操作列的 slot 里，「编辑」之前加一个按钮：

```html
<a-button v-if="group.subscribed" type="link" size="small"
          @click="refreshGroup(group)">立即更新</a-button>
```

- [ ] **Step 4: 加 relativeTime 与 refreshGroup**

在 `methods` 里追加：

```js
relativeTime(ms) {
    if (!ms) return '从未';
    const diff = Date.now() - ms;
    if (diff < 60000) return '刚刚';
    if (diff < 3600000) return Math.floor(diff / 60000) + ' 分钟前';
    if (diff < 86400000) return Math.floor(diff / 3600000) + ' 小时前';
    return Math.floor(diff / 86400000) + ' 天前';
},
async refreshGroup(group) {
    const msg = await this.post('xui/routing/domain-group/refresh/' + group.id);
    if (msg.success) await this.loadAll();
},
```

- [ ] **Step 5: 改编辑弹窗**

`openGroup` 现在要走 detail 接口取原文（列表里已经没有全量域名了）：

```js
async openGroup(group) {
    this.groupModal.id = group ? group.id : 0;
    this.groupModal.remark = group ? group.remark : '';
    this.groupModal.domains = '';
    this.groupModal.subscribeUrl = '';
    this.groupModal.subscribedPreview = [];
    this.groupModal.subscribedCount = 0;
    this.groupModal.lastError = '';
    this.groupModal.lastSkipped = 0;
    this.groupModal.lastUpdatedAt = 0;
    if (group) {
        const msg = await this.post('xui/routing/domain-group/detail/' + group.id);
        if (!msg.success) return;
        const d = msg.obj;
        this.groupModal.domains = d.domains || '';
        this.groupModal.subscribeUrl = d.subscribeUrl || '';
        this.groupModal.subscribedPreview = d.subscribedPreview || [];
        this.groupModal.subscribedCount = d.subscribedCount || 0;
        this.groupModal.lastError = d.lastError || '';
        this.groupModal.lastSkipped = d.lastSkipped || 0;
        this.groupModal.lastUpdatedAt = d.lastUpdatedAt || 0;
    }
    this.groupModal.visible = true;
},
```

`groupModal` 的初始值同步扩展：

```js
groupModal: {
    visible: false, id: 0, remark: '', domains: '', subscribeUrl: '',
    subscribedPreview: [], subscribedCount: 0, lastError: '', lastSkipped: 0, lastUpdatedAt: 0,
},
```

`saveGroup` 带上订阅地址：

```js
const data = {
    remark: this.groupModal.remark,
    domains: this.groupModal.domains,
    subscribeUrl: this.groupModal.subscribeUrl,
};
```

- [ ] **Step 6: 弹窗里加订阅区块**

在域名 textarea 之后追加。**必须留在 `<a-layout id="app">` 内部**——Vue 2 只编译 `el` 指向的那棵子树，落在根元素之外的指令是完全静默的死代码：

```html
<a-form-item label="订阅地址">
    <a-input v-model="groupModal.subscribeUrl"
             placeholder="留空表示不订阅，例如 https://example.com/rules.list"></a-input>
    <div style="color: #888; font-size: 12px; margin-top: 4px;">
        支持 Surge/Clash 规则集与纯域名列表。IP-CIDR 等非域名规则会被忽略。
        保存后由定时任务拉取，或在列表里点「立即更新」。
    </div>
</a-form-item>
<a-form-item v-if="groupModal.subscribeUrl" label="订阅内容">
    <a-alert v-if="groupModal.lastError" type="error" :message="groupModal.lastError" show-icon
             style="margin-bottom: 8px;"></a-alert>
    <a-alert v-else-if="!groupModal.lastUpdatedAt" type="warning" show-icon
             message="订阅地址已更改或尚未拉取，等待首次拉取" style="margin-bottom: 8px;"></a-alert>
    <div v-if="groupModal.subscribedCount">
        <div style="color: #888; font-size: 12px; margin-bottom: 4px;">
            共 [[ groupModal.subscribedCount ]] 条，已忽略 [[ groupModal.lastSkipped ]] 条非域名规则。
            <span v-if="groupModal.subscribedCount > groupModal.subscribedPreview.length">
                此处只显示前 [[ groupModal.subscribedPreview.length ]] 条。
            </span>
        </div>
        <div style="max-height: 180px; overflow-y: auto; background: #fafafa; padding: 8px; border-radius: 4px;">
            <a-tag v-for="d in groupModal.subscribedPreview" :key="d">[[ d ]]</a-tag>
        </div>
    </div>
</a-form-item>
```

同时把手工域名那个 `a-form-item` 的 label 改成「手工域名」，placeholder 后补一句「可留空，只用订阅内容」。

- [ ] **Step 7: 修 ruleIssue 与统计条**

`ruleIssue` 里这行现在会报错（`group.domains` 已不存在）：

```js
if (!group.domains.length) return '域名组为空，这条规则不会写进配置';
```

改为：

```js
if (!group.effectiveCount) return '域名组为空，这条规则不会写进配置';
```

统计条追加一项（在「未生效规则」旁边）：

```html
<a-col :span="6">
    订阅异常：
    <a-tag :color="subscriptionErrorCount ? 'red' : 'green'">[[ subscriptionErrorCount ]]</a-tag>
</a-col>
```

在 `computed` 里加：

```js
subscriptionErrorCount() {
    return this.groups.filter(g => g.lastError).length;
},
```

- [ ] **Step 8: 跑模板测试**

```bash
go test ./web/ -v
```

预期：`TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` PASS。

后者尤其重要：新加的弹窗内容若落在 `#app` 之外，页面渲染完全正常、数据照常加载，但按钮点了毫无反应且控制台不报错。

- [ ] **Step 9: 手动验证**

```bash
XUI_DEBUG=true go run main.go
```

浏览器开 `http://localhost:54321/xui/routing`，**硬刷新**（Cmd+Shift+R）绕开 assets 强缓存，然后逐项确认：

1. 新建一个域名组，手工域名留空，订阅地址填一个可访问的规则集 URL → 保存成功
2. 列表里该组显示「等待首次拉取」
3. 点「立即更新」→ 变成「刚刚」，域名列显示前 5 条 + 「+N」
4. 打开编辑弹窗 → 订阅内容区显示条数与前 200 条预览
5. 把订阅地址改成一个 404 地址 → 保存 → 点「立即更新」→ 显示「拉取失败」，且**域名条数保持不变**（旧数据保留）
6. 统计条的「订阅异常」变成 1

- [ ] **Step 10: 提交**

```bash
git add web/assets/js/model/routing.js web/html/xui/routing.html
git commit -m "feat(routing): 分流页支持订阅地址、状态展示与立即更新"
```

---

### Task 10: 设置页

**Files:**
- Modify: `web/html/xui/setting.html`

**Interfaces:**
- Consumes: `entity.AllSetting.SubscriptionUpdateTime`（Task 6）

- [ ] **Step 1: 加设置项**

在 `web/html/xui/setting.html` 的「其他设置」tab（`a-tab-pane key="4"`）里，时区那一项之后追加：

```html
<setting-list-item type="text" title="订阅更新时间"
                   desc="域名组订阅每天在该时刻自动更新，格式 HH:MM，改动立即生效无需重启"
                   v-model="allSetting.subscriptionUpdateTime"></setting-list-item>
```

`desc` 里「改动立即生效无需重启」不是套话：`SubscriptionJob` 每 10 分钟自检一次配置值，不像其他设置那样需要走 SIGHUP 重启面板。

- [ ] **Step 2: 跑模板测试**

```bash
go test ./web/ -v
```

预期：PASS

- [ ] **Step 3: 手动验证**

```bash
XUI_DEBUG=true go run main.go
```

开 `http://localhost:54321/xui/setting` → 其他设置 → 确认「订阅更新时间」显示为 `04:00`，改成 `25:00` 保存应当报错，改成 `05:30` 保存应当成功。

- [ ] **Step 4: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -v
```

预期：全部 PASS。

- [ ] **Step 5: 检查最终 diff**

```bash
git status --short
git diff --stat HEAD
```

确认没有调试残留、没有临时文件、没有与本功能无关的改动。

- [ ] **Step 6: 提交**

```bash
git add web/html/xui/setting.html
git commit -m "feat(setting): 设置页加入订阅更新时间"
```

---

## 完成标准

全部任务完成后，以下命令必须全绿：

```bash
CGO_ENABLED=1 go build -trimpath -o /tmp/a-ui-build main.go
go vet ./...
go test ./... -v
```

并且手动验证过 Task 9 Step 9 的 6 个检查点，尤其是第 5 点——**拉取失败时旧数据必须保留**。这是本功能唯一一个失败后果严重（分流静默失效、流量裸奔）的路径。
