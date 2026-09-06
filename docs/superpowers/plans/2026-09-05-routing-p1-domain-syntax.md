# P1：域名写法放宽 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让分流组的手工域名框接受 xray 实际支持的全部域名写法，并消掉「手工路径比订阅路径更严」这处既有不一致。

**Architecture:** 只改一个纯函数 `ParseDomains` 与它的一个兄弟分支 `convertSubscriptionLine`。前缀白名单补齐到 xray 的全集；无前缀的裸串按「是否含点」分岔——不含点归一成 `keyword:`，含点因两个生态语义相反而拒绝；对 `domain:`/`full:`/`keyword:` 的值做大小写归一。不动数据模型、不动注入器、不动校验层。

**Tech Stack:** Go 1.27（`go.mod`），标准库 `strings`；测试用 `go test`；门禁 `make verify`。

**Spec:** `docs/superpowers/specs/2026-09-05-routing-ip-and-dns-design.md`（本计划实现其 §4，事实依据见 §2 的 F1/F2/F3）

## Global Constraints

- **升级后默认行为零变化。** 本期唯一有意的行为变化是 Task 2 的订阅存储形态，其影响已在该任务内说明。
- **生成逐字节确定。** `ParseDomains` 按输入行序输出，不排序、不遍历 map。顺序一抖动，`Config.Equals` 恒为 false，10 秒的重启 cron 会不停重启 xray。
- **不得为了让测试通过而弱化测试。** Task 2 会修改两处既有断言，因为预期行为确实改变了；修改处必须写明理由。
- **构建必须 `CGO_ENABLED=1`**（`gorm.io/driver/sqlite` 依赖 `mattn/go-sqlite3`）。`make build` 已带。
- **提交前门禁是 `make verify`**（vet + test + build）。
- 注释解释非显然的原因与约束，不重复代码表面含义；沿用文件现有的中文注释风格。

---

### Task 1: `ParseDomains` 接受 xray 的全部域名写法

**Files:**
- Modify: `web/service/routing_domain.go:15-42`（`domainPrefixes` 上方的注释块、`domainPrefixes`、`ParseDomains`）
- Modify: `web/html/xui/routing.html:262-265`（手工域名 placeholder）
- Test: `web/service/routing_domain_test.go`

**Interfaces:**
- Consumes: `isValidKeyword(s string) bool`（已存在于同包的 `web/service/routing_subscription.go`）
- Produces: `ParseDomains(raw string) ([]string, error)` 签名不变；新增包内私有函数 `normalizeDomainRule(item string) (string, error)`；新增包级变量 `lowercaseValuePrefixes map[string]bool`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_domain_test.go`：

```go
func TestParseDomainsAcceptsAllXrayPrefixes(t *testing.T) {
	raw := "domain:openai.com\nfull:chat.openai.com\nkeyword:openai\n" +
		"regexp:.*\\.oaistatic\\.com\ndotless:localhost\n" +
		"geosite:openai\next:geoip.dat:cn\next-domain:x.dat:tag\next-site:y.dat:tag"
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 9 {
		t.Fatalf("len = %d, want 9: %v", len(got), got)
	}
	if got[2] != "keyword:openai" {
		t.Errorf("got[2] = %q, want keyword:openai", got[2])
	}
}

// 管理员从小火箭配置里整段粘贴过来的原文，逐行都必须能过。
// 最后两行是 DOMAIN-KEYWORD 的转写，改动前正是它们让整段粘贴失败。
func TestParseDomainsAcceptsPastedShadowrocketBlock(t *testing.T) {
	raw := `domain:openai.com
domain:chatgpt.com
domain:chatgpt.site
domain:chat.com
domain:ai.com
domain:sora.com
domain:oaistatic.com
domain:oaiusercontent.com
domain:oaistatsig.com
domain:openaicom.imgix.net
domain:openaimerge.com
domain:crixet.com
domain:openaiapi-site.azureedge.net
domain:client-api.arkoselabs.com
full:openai-api.arkoselabs.com
full:chat.openai.com.cdn.cloudflare.net
full:openaicom-api-bdcpf8c6d2e9atf6.z01.azurefd.net
full:openaicomproductionae4b.blob.core.windows.net
full:production-openaicom-storage.azureedge.net
openai
chatgpt-async-webps`
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 21 {
		t.Fatalf("len = %d, want 21", len(got))
	}
	if got[19] != "keyword:openai" {
		t.Errorf("got[19] = %q, want keyword:openai", got[19])
	}
	if got[20] != "keyword:chatgpt-async-webps" {
		t.Errorf("got[20] = %q, want keyword:chatgpt-async-webps", got[20])
	}
}

// 含点的裸串在 xray 的 routing 规则里是子串匹配，在 geosite 数据文件里却是
// 后缀匹配。放行等于让从 geosite 列表复制来的 openai.com 静默变成能命中
// notopenai.com.evil.net 的规则，而没有任何一层会报错。
func TestParseDomainsRejectsAmbiguousBareDomain(t *testing.T) {
	_, err := ParseDomains("openai.com")
	if err == nil {
		t.Fatal("expected error for bare dotted string")
	}
	for _, want := range []string{"domain:openai.com", "full:openai.com", "keyword:openai.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

// xray 只把目标域名转小写、不归一化配置里的模式
// （app/router/condition.go:59），所以大写的模式是永不命中的哑规则。
func TestParseDomainsLowercasesMatchableValues(t *testing.T) {
	got, err := ParseDomains("domain:OpenAI.COM\nfull:Chat.OpenAI.com\nkeyword:OpenAI\nOpenAI")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"domain:openai.com", "full:chat.openai.com", "keyword:openai", "keyword:openai"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// regexp: 与 dotless: 会被编译成正则，转小写会把 \D 变成 \d 这种
// 意义完全相反的东西。geosite:/ext: 的 code 由 xray 自己 ToUpper。
func TestParseDomainsKeepsCaseSensitiveForms(t *testing.T) {
	raw := "regexp:^API\\D+\\.Example\\.COM$\ndotless:LocalHost\ngeosite:OpenAI"
	got, err := ParseDomains(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"regexp:^API\\D+\\.Example\\.COM$", "dotless:LocalHost", "geosite:OpenAI"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseDomainsRejectsEmptyPrefixValue(t *testing.T) {
	if _, err := ParseDomains("domain:"); err == nil {
		t.Error("expected error for prefix with no value")
	}
}

func TestParseDomainsRejectsKeywordWithSeparators(t *testing.T) {
	if _, err := ParseDomains("open ai"); err == nil {
		t.Error("expected error for keyword containing a space")
	}
}
```

同时给该文件的 import 补上 `"strings"`（`TestParseDomainsRejectsAmbiguousBareDomain` 用到）。

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestParseDomains' -v`
Expected: FAIL。`TestParseDomainsAcceptsAllXrayPrefixes` 报 `域名格式不支持，必须以 domain: / full: / geosite: / regexp: / ext: 开头: keyword:openai`；`TestParseDomainsAcceptsPastedShadowrocketBlock` 在 `openai` 一行同样报错；`TestParseDomainsLowercasesMatchableValues` 因未归一而断言不符。

- [ ] **Step 3: 实现**

把 `web/service/routing_domain.go` 的第 **15-42** 行整段替换为下面的内容。起点是第 15 行的注释块开头
（`// xray 支持的域名匹配前缀。不带前缀的裸域名 xray 也接受……`），不是第 17 行的 `var`——
那两行注释描述的是改动前的行为（「这里要求显式前缀」），留着会与新代码直接矛盾：

```go
// xray 支持的域名匹配前缀，见 common/geodata/rule_parser.go:226
// parseCustomDomainRule 与 parseGeoSiteRule。
// ext-domain: / ext-site: 是 ext: 的别名。
//
// 顺序无关：各前缀互不为前缀（ext: 与 ext-domain: 在第 4 个字符上就分岔）。
var domainPrefixes = []string{
	"domain:", "full:", "keyword:", "regexp:", "dotless:",
	"geosite:", "ext:", "ext-domain:", "ext-site:",
}

// lowercaseValuePrefixes 里的前缀，其值必须转小写才可能命中。
//
// xray 只把「目标域名」转小写，不归一化配置里的模式
// （app/router/condition.go:59），所以 domain:OpenAI.com 是一条永不命中的
// 哑规则，且没有任何一层会报错——不是洁癖，是防一个静默失效。
//
// regexp: 与 dotless: 刻意不在此列：它们会被编译成正则，转小写会把 \D 变成
// \d 这种意义完全相反的东西。geosite: / ext:* 的 code 由 xray 自己 ToUpper
// （rule_parser.go:211），同样不该在这里动。
var lowercaseValuePrefixes = map[string]bool{
	"domain:": true, "full:": true, "keyword:": true,
}

// ParseDomains 把用户在 textarea 中一行一条录入的域名解析成入库列表。
//
// 按输入行序输出，不排序：顺序是「生成逐字节确定」不变量的一部分。
func ParseDomains(raw string) ([]string, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		normalized, err := normalizeDomainRule(item)
		if err != nil {
			return nil, err
		}
		list = append(list, normalized)
	}
	if len(list) == 0 {
		return nil, common.NewError("域名列表不能为空")
	}
	return list, nil
}

// normalizeDomainRule 把一行录入归一成入库形态，或说明它为什么不合法。
func normalizeDomainRule(item string) (string, error) {
	for _, p := range domainPrefixes {
		if !strings.HasPrefix(item, p) {
			continue
		}
		value := item[len(p):]
		if value == "" {
			return "", common.NewError("域名格式不支持，前缀后面没有内容:", item)
		}
		if lowercaseValuePrefixes[p] {
			return p + strings.ToLower(value), nil
		}
		return item, nil
	}

	// 带冒号却没匹配上任何前缀：几乎必然是前缀拼错。放行的话 xray 会把整串
	// 当子串匹配（infra/conf/router.go:175 传的 defaultType 是 Domain_Substr），
	// 而 SNI/Host 里不含冒号——一条永不命中的哑规则，且 Configuration OK。
	if strings.Contains(item, ":") {
		return "", common.NewError("域名格式不支持，无法识别的前缀:", item,
			"可用前缀:", strings.Join(domainPrefixes, " / "))
	}

	// 无前缀的裸串在 xray 的 routing 规则里是子串匹配，但在 geosite 数据文件
	// （domain-list-community）里是后缀匹配——同一份文本两种含义。含点的裸串
	// 两种解读都说得通，放行等于让从 geosite 列表复制来的 openai.com 静默变成
	// 能命中 notopenai.com.evil.net 的规则。拒绝是廉价的：报错里点名三种写法，
	// 补个前缀即可。
	if strings.Contains(item, ".") {
		return "", common.NewError("域名写法有歧义:", item,
			"——不带前缀时 xray 按子串匹配。请明确写成 domain:"+item+
				"（含子域名）、full:"+item+"（精确匹配）或 keyword:"+item+
				"（确实要子串匹配）")
	}

	// 不含点也不含冒号：不可能是域名，意图唯一是关键词（对应 Surge/Clash 的
	// DOMAIN-KEYWORD）。归一成显式的 keyword: 存库，让域名组列表里的标签
	// 自己说清楚它在做什么，也让手工与订阅两条路径的存储形态一致。
	if !isValidKeyword(item) {
		return "", common.NewError("关键词含有非法字符:", item)
	}
	return "keyword:" + strings.ToLower(item), nil
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestParseDomains' -v`
Expected: 全部 PASS，包括改动前就存在的 `TestParseDomainsAcceptsNativeSyntax`、`TestParseDomainsSkipsBlankLinesAndTrims`、`TestParseDomainsRejectsUnknownPrefix`（`wat:openai.com` 含冒号，仍走「无法识别的前缀」分支）、`TestParseDomainsRejectsEmptyResult`。

- [ ] **Step 5: 更新前端 placeholder**

`web/html/xui/routing.html:263-264`，把 placeholder 换成（`&#10;` 是模板里表示换行的写法，沿用原样）：

```html
                <a-input type="textarea" :rows="8" v-model="groupModal.domains"
                         placeholder="domain:openai.com&#10;full:chat.openai.com&#10;keyword:openai&#10;geosite:openai&#10;可留空，只用订阅内容"></a-input>
```

并在该 `a-form-item` 的 `</a-input>` 之后、`</a-form-item>` 之前插入一行说明：

```html
                <div style="color: #888; font-size: 12px; margin-top: 4px;">
                    可用前缀：domain:（含子域名）/ full:（精确）/ keyword:（子串）/ regexp: / dotless: / geosite: / ext:。
                    不带前缀且不含点的会按 keyword: 处理；含点的必须写前缀。
                </div>
```

- [ ] **Step 6: 跑模板测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: PASS。（`web.go` 的 `getHtmlTemplate` 吞掉 `ParseFS` 错误，光靠 `go build` 发现不了模板语法错误，必须跑这个。）

- [ ] **Step 7: 提交**

```bash
git add web/service/routing_domain.go web/service/routing_domain_test.go web/html/xui/routing.html
git commit -m "feat(routing): 域名写法补齐 keyword:/dotless:/ext-* 与裸关键词

前缀白名单补到 xray 实际支持的全集（rule_parser.go:226）。无前缀的裸串按
是否含点分岔：不含点归一成 keyword:，含点因 xray routing 与 geosite 数据
文件语义相反而拒绝，报错里点名三种写法。

对 domain:/full:/keyword: 的值做大小写归一——xray 只把目标域名转小写、
不归一化配置里的模式（condition.go:59），大写的模式是永不命中的哑规则。
regexp:/dotless: 不归一，转小写会把 \\D 变成 \\d。"
```

---

### Task 2: 订阅侧的 `DOMAIN-KEYWORD` 也吐 `keyword:`

**Files:**
- Modify: `web/service/routing_subscription.go:76-110`（`convertSubscriptionLine` 的 `DOMAIN-KEYWORD` 分支）
- Test: `web/service/routing_subscription_test.go:28`、`:183`（既有断言，预期行为改变）

**Interfaces:**
- Consumes: Task 1 定义的存储形态约定（关键词一律存 `keyword:<小写值>`）
- Produces: 无新签名。`convertSubscriptionLine` 的返回值形态改变

**为什么必须改：** 不改的话同一个关键词在库里有两种形态（手工存 `keyword:openai`、订阅存 `openai`），而 `MergeDomains`（`routing_domain.go:290`）按字符串去重，去不掉重复——同一条 xray 规则的 `domain` 数组里会出现两个语义相同的条目。

**代价（必须接受并记录）：** 下一次订阅刷新会重写所有订阅组的 `SubscribedDomains`，配置字节变化 → 触发一次整进程重启。一次性，之后稳定。

- [ ] **Step 1: 改既有断言，让它先失败**

`web/service/routing_subscription_test.go:28`，把

```go
	want := []string{"domain:qq.com", "full:exact.example.com", "baidu"}
```

改为

```go
	// 关键词存显式的 keyword: 前缀，与手工录入路径（ParseDomains）的存储形态
	// 一致。两条路径形态不一致时 MergeDomains 去不掉重复。
	want := []string{"domain:qq.com", "full:exact.example.com", "keyword:baidu"}
```

`web/service/routing_subscription_test.go:183-185`，把

```go
	if len(domains) != 1 || domains[0] != "baidu" {
		t.Errorf("got = %v, want [baidu]", domains)
	}
```

改为

```go
	if len(domains) != 1 || domains[0] != "keyword:baidu" {
		t.Errorf("got = %v, want [keyword:baidu]", domains)
	}
```

并追加一个新用例，钉住两条路径的形态一致：

```go
// 手工录入与订阅拉取必须产出同一个字符串，否则 MergeDomains 去不掉重复。
func TestSubscriptionKeywordMatchesManualForm(t *testing.T) {
	fromSub, _, err := ParseSubscription("DOMAIN-KEYWORD,openai\n")
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	fromManual, err := ParseDomains("openai")
	if err != nil {
		t.Fatalf("ParseDomains: %v", err)
	}
	if fromSub[0] != fromManual[0] {
		t.Errorf("subscription produced %q but manual produced %q", fromSub[0], fromManual[0])
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestParseSubscription|TestSubscriptionKeyword' -v`
Expected: FAIL，三处都报 `baidu` / `openai` 缺少 `keyword:` 前缀。

- [ ] **Step 3: 实现**

`web/service/routing_subscription.go`，把 `DOMAIN-KEYWORD` 分支替换为：

```go
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
				return "", false
			}
			return "keyword:" + strings.ToLower(value), true
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestParseSubscription|TestSubscriptionKeyword' -v`
Expected: 全部 PASS。

- [ ] **Step 5: 跑全包测试**

Run: `go test ./web/service/`
Expected: PASS。（`routing_inject_test.go` 里若有夹具用了裸关键词，此处会暴露；有的话把夹具改成 `keyword:` 形态，不要放宽断言。）

- [ ] **Step 6: 提交**

```bash
git add web/service/routing_subscription.go web/service/routing_subscription_test.go
git commit -m "fix(routing): 订阅的 DOMAIN-KEYWORD 存显式 keyword: 前缀

改动前手工录入存 keyword:openai、订阅存裸 openai，同一个关键词两种形态，
MergeDomains 按字符串去重去不掉，同一条规则的 domain 数组里会出现两个
语义相同的条目。

代价：下次订阅刷新会重写所有订阅组的 SubscribedDomains，配置字节变化
触发一次整进程重启。一次性。"
```

---

### Task 3: 门禁

**Files:** 无改动

- [ ] **Step 1: 跑完整门禁**

Run: `make verify`
Expected: vet 无输出、全部包测试 PASS、build 成功。

失败时读错误、修问题、重跑，不跳过、不粉饰。区分「本次修改导致的失败」与「任务开始前已存在的失败」——后者不擅自修复，但要在最终报告里说明是否阻碍本期完成。

- [ ] **Step 2: 检查最终 diff**

Run: `git diff main...HEAD --stat && git status --short`
Expected: 只有 `web/service/routing_domain.go`、`web/service/routing_domain_test.go`、`web/service/routing_subscription.go`、`web/service/routing_subscription_test.go`、`web/html/xui/routing.html` 五个文件，工作区干净，没有调试残留与临时文件。
