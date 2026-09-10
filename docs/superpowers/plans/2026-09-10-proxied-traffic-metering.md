# 分流流量按「入站 × 规则」计量 实施计划（第三期 · 改动 B）

> ⛔ **本计划已实施、作为 v1.26.0 发布，随后于 2026-09-10 整体撤销（`git revert -m 1 a36f57d`），GitHub 上的 v1.26.0 release 已删除。不要照着它重做。**
>
> **撤销原因：按入站展开规则时，每一份克隆都带着完整的域名列表，而域名列表可以来自订阅。**
>
> 香港生产实例（16 入站 / 7 条分流规则 / 972 MB 单核 VPS）打开开关后全员断网约 4 分钟：
>
> | | 规则数 | 域名条目 | `bin/config.json` |
> |---|---|---|---|
> | 开关关（v1.25.1） | 1 | 111,489 | 3.4 MB |
> | 开关开（v1.26.0） | 16 | **1,783,824** | **58.6 MB** |
>
> 罪魁是一条全局规则「中国域名列表」——域名组挂了订阅、合并后 111,489 条域名，被展开成 16 份。
> xray 编译这 178 万条域名匹配器耗时约 4 分钟（实测：进程存活 509 秒才把 17 个端口 bind 上，
> 纯 CPU 225 秒），常驻内存 469 MB / 972 MB，swap 吃掉 293 MB。
>
> **这期间面板首页仍显示 `running`**（`Process.Start` 不回传启动失败），xray 进程确实活着、
> CPU 93%、却一个端口都没监听，没有任何一层报错。排查这类现象唯一可靠的判据是 `ss -lntp`，
> 不是面板状态、也不是 `pgrep`。
>
> 同机其余 5 条规则展开的净新增合计只有 **3,904 条**：99.76% 的代价来自那一条，
> 而它恰恰给不出任何域名级明细（11 万个域名塌成一个桶，弹窗里只多一行总量）。
>
> **重做时必须先解决的约束：** 展开一条规则的代价是 `域名数 × (入站数 - 1)`——净新增，
> 入站数为 1 时为 0，因为那只是把原规则换个 tag，再大的列表也免费。任何重做都必须在展开**之前**
> 算出这个数并设上限，超限的规则退回单条形态并记 `logger.Warning`（照「生成期跳过」那道防线的
> 既有写法，跳过必须带原因，否则对管理员是隐形的）。**这个上限不是可选的优化，
> 是这个方案能不能成立的前提。**


> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 被管理员分流规则带走的流量（走出站节点的那部分）也能按「入站 × 规则」计量，让 `chatgpt.com 280 次、0 B` 这类行有真实字节数。

**Architecture:** 开关打开时，每条非 block 的分流规则按入站展开成 N 条，各自的 `outboundTag` 从真实出站改成一个计量出站 `a-ui-meter-r-<入站id>-<规则id>`；计量出站是真实出站的深拷贝、只换 tag，流量走向一个字节不变。计量字节以 `rule:<规则id>` 为键落进 `DomainStat.Domain`，与 A 的 IP 字面量同一套推导式类型判定，**不加数据模型列**。开关关时生成结果与现在逐字节相同。

**Tech Stack:** Go 1.27、GORM + SQLite（CGO）、xray-core `v1.260327.1-0.20260728075948-5ca6f4b7d4dc`。无前端构建。

**Spec:** `docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md` §5、§7（本计划实现改动 B）

## 相对 spec 的两处修订（实施前定案）

**① 不加 `Kind` 列，`rule:<id>` 直接存进 `DomainStat.Domain`。** spec §6 要给 `DomainStat` / `MeterDomain` 加 `Kind` 列并扩唯一索引。改动 A 实施时已确认类型可以从 `Domain` 的形态推导（`net.ParseIP`）；B 沿用同一思路：规则类的键写成 `rule:9`，推导时先判这个前缀再判 IP。它不可能与真实目标撞车——`Domain` 列的值全部来自 `domain.Registrable()`，那个函数的输出永远是注册域名或 IP，不会以 `rule:` 开头。`MeterDomain` 池表完全不动：B 类计量不进池，它由规则直接产生，没有候选、排序、换池这些概念。**spec §6 整节作废**，Task 8 回写。

**② `ParseMeterTag` 对 B 类 tag 返回 `(入站id, "rule:<规则id>", true)`，`RecordMetered` 因此一行都不用改。** 它拿到的第二个返回值直接当 `DomainStat.Domain` 写，A 类是域名或 IP、B 类是 `rule:9`，同一条采集链路自动分流。spec §5.6 说「必须同时更新 `RecordMetered` 的归因分支」，实施后确认不必——归因逻辑没有任何域名语义，它只是把 tag 反查出来的键原样写库。

## Global Constraints

- **开关 `meterProxiedTraffic` 默认 0（关）。关着时 `GetXrayConfig()` 的输出与本计划实施前逐字节相同。** 这是「升级后行为零变化」唯一可验证的形式，Task 3 有测试钉住。
- **计量出站是真实出站的深拷贝，只换 `tag`。** 不用 `proxySettings` 链式（spec §5.4 的取舍）。深拷贝走 `json.Marshal` → `json.Unmarshal`，与 `appendMeterOutbounds` 同一写法。
- **block 规则不拆、不计量。** 黑洞出站字节恒为 0，拆了只有连接数。
- **全局规则（`InboundIds = []`）按当前全部启用入站展开，入站 id 升序。** 不展开 B 对全局规则无效，而这台生产机 7 条规则里 5 条是全局的。
- **顺序逐字节确定**：规则之间仍按 `priority asc, id asc`（`GetEnabled` 已保证），同一条规则展开出的 N 条按入站 id 升序，`domain` 条件在前、`ip` 条件在后（`buildRule` 原有约定）。违反任何一条都会让 `Config.Equals` 恒为 false，10 秒 cron 不停重启 xray。
- **克隆时找不到目标出站 → 整份配置生成失败**（fail-close）。规则已经改成引用计量出站了，出站却没克隆出来就是悬空引用——xray 对悬空 `outboundTag` 静默回落默认出站，本该走 IProyal 的 ChatGPT 会静默走直连。生成失败时 xray 保持原状继续跑，是安全的一侧。
- **规则被删除时连带删除它的计量数据；`TrafficCleanupJob` 里另有兜底。** SQLite 复用自增 id，残留的 `rule:9` 行会绑到下一条新建的规则上，而引用不再悬空、生成期防线拦不住。
- 新增设置项要动 **5 处**（`defaultValueMap` / `entity.AllSetting` / `CheckValid` / getter / `models.js`），漏掉 `models.js` 那处会让整个保存配置接口失败（CLAUDE.md「设置系统」）。
- 测试的工作目录、提交风格、按路径 `git add`、`make verify` 门禁，与改动 A 的计划相同。每个 commit 末尾附：
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
  ```

## File Structure

| 文件 | 本期动作 |
|---|---|
| `database/model/meter.go` | 新增 `MeterRuleTag` / `RuleStatKey` / `IsRuleStatKey`；扩展 `ParseMeterTag` |
| `database/model/meter_test.go` | 新增用例 |
| `web/service/setting.go` | `defaultValueMap` 加 key；新增 `GetMeterProxiedTraffic` |
| `web/entity/entity.go` | `AllSetting` 加字段；`CheckValid` 加校验 |
| `web/assets/js/model/models.js` | `AllSetting` 加同名字段 |
| `web/html/xui/setting.html` | 加开关 |
| `web/service/routing_validate.go` | `settingsAffectXrayConfig` 加判定 |
| `web/service/routing_inject.go` | `buildRule` 拆分；新增 `appendProxiedMeterOutbounds`；`Inject` 挪动出站序列化 |
| `web/service/routing_inject_meter_test.go` | 新增用例 |
| `web/service/routing_rule.go` | `Del` 连带清理 |
| `web/service/domain_stat.go` | `DeleteByRule` / `PruneOrphanRules`；`topDomainKind` 加 rule；`TopDomainRow` 加 `Label` |
| `web/service/domain_stat_top_test.go` | 新增用例 |
| `web/job/traffic_cleanup_job.go` | 调用 `PruneOrphanRules` |
| `web/html/xui/access_log_modal.html` | 规则行的渲染 |
| `web/service/meter_rule_e2e_test.go` | 真实 xray 验证分流结果不变 |

---

### Task 1: tag 形态与 `rule:<id>` 键

**Files:**
- Modify: `database/model/meter.go`
- Test: `database/model/meter_test.go`

**Interfaces:**
- Produces: `MeterRuleTag(inboundId, ruleId int) string`、`RuleStatKey(ruleId int) string`、`IsRuleStatKey(s string) bool`、`ParseRuleStatKey(s string) (int, bool)`；`ParseMeterTag` 对 B 类 tag 返回 `(inboundId, RuleStatKey(ruleId), true)`

- [ ] **Step 1: 写失败测试**

追加到 `database/model/meter_test.go`：

```go
// B 类计量 tag 的形态与反查。ParseMeterTag 对它返回的第二个值是 rule:<id>，
// 这样 RecordMetered 不用改一行——它只是把反查出来的键原样写进 DomainStat.Domain。
func TestMeterRuleTagRoundTrip(t *testing.T) {
	tag := MeterRuleTag(7, 9)
	if tag != "a-ui-meter-r-7-9" {
		t.Fatalf("MeterRuleTag = %q", tag)
	}
	if !IsMeterTag(tag) {
		t.Error("B 类 tag 必须仍被 IsMeterTag 认作计量 tag，否则四道 fail-close 防线漏掉它")
	}
	id, key, ok := ParseMeterTag(tag)
	if !ok || id != 7 || key != "rule:9" {
		t.Errorf("ParseMeterTag(%q) = (%d, %q, %v)，期望 (7, \"rule:9\", true)", tag, id, key, ok)
	}
	if !IsRuleStatKey(key) {
		t.Errorf("IsRuleStatKey(%q) = false", key)
	}
	if rid, ok := ParseRuleStatKey(key); !ok || rid != 9 {
		t.Errorf("ParseRuleStatKey(%q) = (%d, %v)", key, rid, ok)
	}
}

// A 类 tag 的反查一个字节不变——这是回归守卫。
func TestParseMeterTagStillHandlesDomainAndIP(t *testing.T) {
	cases := []struct{ tag, wantKey string }{
		{"a-ui-meter-7-google.com", "google.com"},
		{"a-ui-meter-7-some-cdn.example.com", "some-cdn.example.com"}, // 域名含短横线
		{"a-ui-meter-7-72.235.209.83", "72.235.209.83"},
		{"a-ui-meter-7-2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		id, key, ok := ParseMeterTag(c.tag)
		if !ok || id != 7 || key != c.wantKey {
			t.Errorf("ParseMeterTag(%q) = (%d, %q, %v)，期望 (7, %q, true)", c.tag, id, key, ok, c.wantKey)
		}
	}
}

// 形态不对的 B 类 tag 一律拒绝，绝不猜。
func TestParseMeterTagRejectsMalformedRuleTags(t *testing.T) {
	for _, tag := range []string{
		"a-ui-meter-r-7",       // 缺规则 id
		"a-ui-meter-r-7-",      // 规则 id 为空
		"a-ui-meter-r-7-x",     // 规则 id 非数字
		"a-ui-meter-r-0-9",     // 入站 id 为 0
		"a-ui-meter-r-7-0",     // 规则 id 为 0
		"a-ui-meter-r--9",      // 入站 id 为空
	} {
		if _, _, ok := ParseMeterTag(tag); ok {
			t.Errorf("ParseMeterTag(%q) 应该拒绝", tag)
		}
	}
}

// rule: 前缀不可能与真实目标撞车：Domain 列的值全部来自 domain.Registrable，
// 它的输出是注册域名或 IP。这条测试守的是 IsRuleStatKey 对真实目标的否定判定。
func TestIsRuleStatKeyRejectsRealTargets(t *testing.T) {
	for _, s := range []string{"google.com", "72.235.209.83", "2001:db8::1", "rule", "rule:", "rule:x", ""} {
		if IsRuleStatKey(s) {
			t.Errorf("IsRuleStatKey(%q) = true", s)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./database/model/ -run 'TestMeterRuleTag|TestParseMeterTag|TestIsRuleStatKey' -v
```

预期：编译失败，`undefined: MeterRuleTag`

- [ ] **Step 3: 实现**

`database/model/meter.go`：在 `MeterTag` 之后加：

```go
// meterRuleTagInfix 是 B 类（按分流规则计量）tag 在前缀之后的标记段。
// 它把 a-ui-meter-r-7-9 与 a-ui-meter-7-google.com 区分开：后者前缀之后
// 直接是数字，前者是字母 r。
const meterRuleTagInfix = "r-"

// MeterRuleTag 拼出 (入站, 分流规则) 对应的计量出站 tag，形如 a-ui-meter-r-7-9。
//
// 带的是规则 id 而不是规则备注：备注可空、可重复、可改，id 才是稳定的键。
// SQLite 会复用被删除的自增 id，所以 RoutingRuleService.Del 必须连带删掉
// 该规则的计量数据（DomainStatService.DeleteByRule），否则残留的 rule:9 行
// 会绑到下一条新建的规则上。
func MeterRuleTag(inboundId, ruleId int) string {
	return MeterOutboundTagPrefix + meterRuleTagInfix + strconv.Itoa(inboundId) + "-" + strconv.Itoa(ruleId)
}

// ruleStatKeyPrefix 是 B 类计量在 DomainStat.Domain 里的键前缀。
//
// 不加数据库列，用键的形态区分类型，与 A 类的 IP 字面量同一套推导式判定。
// 它不可能与真实目标撞车：Domain 列的值全部来自 domain.Registrable，
// 那个函数的输出是注册域名或 IP，不会以 rule: 开头。
const ruleStatKeyPrefix = "rule:"

// RuleStatKey 是分流规则在 DomainStat.Domain 里的键。
func RuleStatKey(ruleId int) string {
	return ruleStatKeyPrefix + strconv.Itoa(ruleId)
}

// IsRuleStatKey 判断一个 DomainStat.Domain 的值是不是 B 类计量的键。
func IsRuleStatKey(s string) bool {
	_, ok := ParseRuleStatKey(s)
	return ok
}

// ParseRuleStatKey 从 rule:<id> 反查规则 id。形态不对一律拒绝。
func ParseRuleStatKey(s string) (int, bool) {
	rest, ok := strings.CutPrefix(s, ruleStatKeyPrefix)
	if !ok || rest == "" {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
```

把 `ParseMeterTag` 整体替换为：

```go
// ParseMeterTag 从计量出站 tag 反查出 (入站 id, 键)。
//
// 两种形态：
//   - A 类 a-ui-meter-<入站id>-<目标>：键是注册域名或 IP 字面量。按**第一个**
//     短横线切开——inboundId 是十进制数字不含短横线，其后全部是目标。目标本身
//     可以含短横线（some-cdn.example.com），所以绝不能从右边切。
//   - B 类 a-ui-meter-r-<入站id>-<规则id>：键是 rule:<规则id>（RuleStatKey）。
//     两个数字之间恰好一个短横线。
//
// 返回的键直接被 RecordMetered 当 DomainStat.Domain 写，所以 B 类返回的是
// rule:9 这个键而不是裸的规则 id——采集链路因此不需要知道两类的区别。
//
// 拒绝一切形态不对的输入而不是尽力猜：采集路径上一个猜错的 tag 会把字节
// 静默记到别的目标头上，而榜单会渲染得完全正常。
func ParseMeterTag(tag string) (int, string, bool) {
	rest, ok := strings.CutPrefix(tag, MeterOutboundTagPrefix)
	if !ok {
		return 0, "", false
	}
	if ruleRest, isRule := strings.CutPrefix(rest, meterRuleTagInfix); isRule {
		idStr, ruleStr, ok := strings.Cut(ruleRest, "-")
		if !ok || idStr == "" || ruleStr == "" {
			return 0, "", false
		}
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			return 0, "", false
		}
		ruleId, err := strconv.Atoi(ruleStr)
		if err != nil || ruleId <= 0 {
			return 0, "", false
		}
		return id, RuleStatKey(ruleId), true
	}
	idStr, dom, ok := strings.Cut(rest, "-")
	if !ok || idStr == "" || dom == "" {
		return 0, "", false
	}
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	return id, dom, true
}
```

- [ ] **Step 4: 运行确认通过**

```bash
go test ./database/model/ -v -run 'Meter|RuleStat'
```

预期：新增 4 条 PASS，既有的 `meter_test.go` 用例全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add database/model/meter.go database/model/meter_test.go
git commit -m "$(cat <<'EOF'
feat(meter): B 类计量 tag（a-ui-meter-r-<入站>-<规则>）与 rule:<id> 键

ParseMeterTag 对 B 类 tag 返回的第二个值是 rule:<规则id>，RecordMetered
因此一行都不用改——它只是把反查出来的键原样写进 DomainStat.Domain，
A 类是域名或 IP、B 类是 rule:9，同一条采集链路自动分流。

不加数据模型列：Domain 列的值全部来自 domain.Registrable，输出永远是
注册域名或 IP，不会以 rule: 开头，用键的形态区分类型与 A 类的 IP 同一套。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 2: 总开关 `meterProxiedTraffic`

**Files:**
- Modify: `web/service/setting.go`、`web/entity/entity.go`、`web/assets/js/model/models.js`、`web/html/xui/setting.html`、`web/service/routing_validate.go`
- Test: `web/entity/entity_test.go`（若不存在则新建）、`web/service/setting_defaults_test.go`

**Interfaces:**
- Produces: `(*SettingService).GetMeterProxiedTraffic() (bool, error)`；`entity.AllSetting.MeterProxiedTraffic int`

- [ ] **Step 1: 写失败测试**

追加到 `web/service/setting_defaults_test.go`（参照同文件既有用例的 `setupDB` 方式）：

```go
// 开关默认关：升级后行为零变化靠这一条。
func TestMeterProxiedTrafficDefaultsToOff(t *testing.T) {
	setupDB(t)
	on, err := (&SettingService{}).GetMeterProxiedTraffic()
	if err != nil {
		t.Fatalf("GetMeterProxiedTraffic: %v", err)
	}
	if on {
		t.Error("默认必须是关")
	}
	if err := (&SettingService{}).setString("meterProxiedTraffic", "1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	if on, _ := (&SettingService{}).GetMeterProxiedTraffic(); !on {
		t.Error("写 1 之后应为开")
	}
}

// 只接受 0/1：反射只支持 int，前端 switch 只送这两个值。
func TestCheckValidRejectsBadMeterProxiedTraffic(t *testing.T) {
	s := validAllSettingForTest(t)
	s.MeterProxiedTraffic = 2
	if err := s.CheckValid(); err == nil {
		t.Error("MeterProxiedTraffic = 2 必须被拒绝")
	}
}
```

> **实现者注意**：`validAllSettingForTest` 是假定的辅助名——找同文件里既有的
> 「构造一份能通过 CheckValid 的 AllSetting」的辅助，用它实际的名字；没有就
> 在测试文件里写一个最小的（从 `defaultValueMap` 填字段）。`setupDB` 同理。

- [ ] **Step 2: 运行确认失败**

```bash
go test ./web/service/ -run 'TestMeterProxiedTraffic|TestCheckValidRejectsBadMeterProxied' -v
```

预期：编译失败，`GetMeterProxiedTraffic undefined` / `MeterProxiedTraffic undefined`

- [ ] **Step 3: 实现 5 处 + 校验接入**

`web/service/setting.go` 的 `defaultValueMap`（紧接 `"ipRuleResolveDomain": "0",`）：

```go
	"meterProxiedTraffic":      "0",
```

同文件 `GetIPRuleResolveDomain` 之后加：

```go
// GetMeterProxiedTraffic 报告是否对被分流规则带走的流量也做计量（第三期改动 B）。
//
// 为真时生成期把每条非 block 分流规则按入站展开，outboundTag 改成克隆自真实
// 出站的计量出站。默认 0：这一项打破了「计量不能改变分流结果」的原有不变量，
// 出错后果从「统计不准」升级为「用户断网」，所以必须由管理员显式打开，
// 且关掉即恢复——下一个生成周期走热应用，不重启。
func (s *SettingService) GetMeterProxiedTraffic() (bool, error) {
	v, err := s.getInt("meterProxiedTraffic")
	if err != nil {
		return false, err
	}
	return v != 0, nil
}
```

`web/entity/entity.go` 的 `AllSetting`（紧接 `IPRuleResolveDomain` 字段）：

```go
	MeterProxiedTraffic int `json:"meterProxiedTraffic" form:"meterProxiedTraffic"`
```

同文件 `CheckValid` 里紧接 `IPRuleResolveDomain` 的校验之后：

```go
	if s.MeterProxiedTraffic != 0 && s.MeterProxiedTraffic != 1 {
		return common.NewError("「计量被分流带走的流量」只能是 0 或 1:", s.MeterProxiedTraffic)
	}
```

`web/assets/js/model/models.js` 的 `AllSetting` 构造函数（紧接 `this.ipRuleResolveDomain = 0;`）：

```javascript
        this.meterProxiedTraffic = 0;
```

`web/html/xui/setting.html`，紧接「让 IP 规则也匹配域名目标」那个 `setting-list-item` 之后：

```html
                                <setting-list-item type="switch" title="计量被分流带走的流量"
                                                   desc="关闭时访问日志的「Top 域名」只对走直连的流量给出字节数，被你的分流规则带去出站节点的部分只有访问次数、字节恒为 0。打开后按「入站 × 分流规则」计量（如「ChatGPT 组 → IProyal」一行），流量走向不变。代价：每条分流规则按入站展开，每晚那次 xray 整进程重启多花约 10~15 秒。出问题关掉即恢复"
                                                   v-model="allSetting.meterProxiedTraffic"></setting-list-item>
```

`web/service/routing_validate.go` 的 `settingsAffectXrayConfig`：

```go
func settingsAffectXrayConfig(candidate *entity.AllSetting) bool {
	settingService := SettingService{}
	oldServers, err := settingService.GetDNSServers()
	if err != nil {
		return true
	}
	oldResolve, err := settingService.GetIPRuleResolveDomain()
	if err != nil {
		return true
	}
	oldProxied, err := settingService.GetMeterProxiedTraffic()
	if err != nil {
		return true
	}
	return candidate.DNSServers != oldServers ||
		(candidate.IPRuleResolveDomain == 1) != oldResolve ||
		(candidate.MeterProxiedTraffic == 1) != oldProxied
}
```

并在该文件 `ValidateSettings` 上方的注释里把「只对真正会进入生成配置的设置项做这一步：dnsServers 与 ipRuleResolveDomain」改成「…：dnsServers、ipRuleResolveDomain 与 meterProxiedTraffic」。`apply` 回调**不**为它做任何事：开关翻转会重排整段 routing 和克隆一批出站，正确地重算得把整条注入链再走一遍，那正是 `xrayTemplateConfig` 刻意不走这一步的同一个理由（两条生成链一旦漂移比不校验更危险）。这里校验的是「当前生成的配置」——克隆出来的出站与已经通过校验的真实出站只差一个 tag，不会引入新的非法性。

- [ ] **Step 4: 运行确认通过**

```bash
go test ./web/service/ ./web/entity/ -run 'MeterProxied|CheckValid|Setting' 2>&1 | tail -3
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' 2>&1 | tail -1
```

预期：全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add web/service/setting.go web/entity/entity.go web/assets/js/model/models.js web/html/xui/setting.html web/service/routing_validate.go web/service/setting_defaults_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 总开关 meterProxiedTraffic，默认关

它打破了「计量不能改变分流结果」的原有不变量：计量出站从此承载真实的
分流流量，出错后果从「统计不准」升级为「用户断网」。所以必须由管理员
显式打开，且关掉即恢复——下一个生成周期走热应用，不重启。

按项目约定动了 5 处（defaultValueMap / AllSetting / CheckValid / getter /
models.js），并接进 settingsAffectXrayConfig 让保存时过一遍真实 xray。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 3: `buildRule` 按入站展开并指向计量出站

本计划的核心任务。开关关时输出**逐字节不变**是第一条断言。

**Files:**
- Modify: `web/service/routing_inject.go`（`buildRules` / `buildRule` 签名与实现）
- Test: `web/service/routing_inject_meter_test.go`

**Interfaces:**
- Produces: `type proxiedMeterNeed struct{ InboundId, RuleId int; TargetTag string }`；`buildRules` 第三个返回值变为 `[]proxiedMeterNeed`（原 error 顺延为第四个）；`buildRule` 新增参数 `expand bool, allInboundIds []int`，返回值新增 `[]proxiedMeterNeed`

- [ ] **Step 1: 写失败测试**

追加到 `web/service/routing_inject_meter_test.go`。先看同文件 / `routing_inject_test.go` 里「建域名组 + 建出站节点 + 建规则」的现成辅助（`TestInject*` 的规则用例一定有），下面用 `seedGroup` / `seedOutbound` / `seedRule` 指代，**用它们实际的名字与签名**：

```go
// 开关关时生成结果逐字节不变——「升级后行为零变化」唯一可验证的形式。
func TestInjectProxiedMeterOffIsByteIdentical(t *testing.T) {
	setupMeterPoolTest(t)
	inA := newTestInbound(t, 32031)
	inB := newTestInbound(t, 32032)
	g := seedGroup(t, "ChatGPT", []string{"domain:chatgpt.com"})
	ob := seedOutbound(t, "a-ui-relay", `{"protocol":"freedom","settings":{}}`)
	seedRule(t, []int{inA.Id, inB.Id}, []int{g.Id}, model.ActionProxy, ob.Id, 1)

	before := injectToBytes(t)
	// 开关显式写 0（与默认值相同）再生成一次，必须逐字节相同。
	if err := (&SettingService{}).setString("meterProxiedTraffic", "0"); err != nil {
		t.Fatal(err)
	}
	after := injectToBytes(t)
	if !bytes.Equal(before, after) {
		t.Fatalf("开关关时两次生成不一致:\n%s\n---\n%s", before, after)
	}
	// 且没有任何 B 类出站或规则。
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatal(err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); strings.HasPrefix(tag, "a-ui-meter-r-") {
			t.Errorf("开关关时不该有 B 类出站 %q", tag)
		}
	}
}

// 开关开：一条覆盖两个入站的规则拆成两条，各指向自己的计量出站，
// 计量出站是真实出站的克隆、只换 tag。
func TestInjectProxiedMeterSplitsRulePerInbound(t *testing.T) {
	setupMeterPoolTest(t)
	inA := newTestInbound(t, 32033)
	inB := newTestInbound(t, 32034)
	g := seedGroup(t, "ChatGPT", []string{"domain:chatgpt.com"})
	ob := seedOutbound(t, "a-ui-relay", `{"protocol":"freedom","settings":{"domainStrategy":"UseIP"}}`)
	rule := seedRule(t, []int{inA.Id, inB.Id}, []int{g.Id}, model.ActionProxy, ob.Id, 1)
	if err := (&SettingService{}).setString("meterProxiedTraffic", "1"); err != nil {
		t.Fatal(err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatal(err)
	}

	// 规则：两条，按入站 id 升序，outboundTag 是各自的计量 tag，domain 条件原样。
	var got []map[string]any
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); strings.HasPrefix(tag, "a-ui-meter-r-") {
			got = append(got, r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("B 类规则 %d 条，期望 2：%v", len(got), got)
	}
	wantTags := []string{model.MeterRuleTag(inA.Id, rule.Id), model.MeterRuleTag(inB.Id, rule.Id)}
	for i, r := range got {
		if r["outboundTag"] != wantTags[i] {
			t.Errorf("第 %d 条 outboundTag = %v，期望 %s", i, r["outboundTag"], wantTags[i])
		}
		ib, _ := r["inboundTag"].([]any)
		if len(ib) != 1 {
			t.Errorf("第 %d 条 inboundTag = %v，期望恰好一个", i, ib)
		}
		if d, _ := r["domain"].([]any); len(d) != 1 || d[0] != "domain:chatgpt.com" {
			t.Errorf("第 %d 条 domain = %v，条件不该被改动", i, d)
		}
	}
	// 原来指向真实出站的那条规则不该再存在。
	for _, r := range decodeRules(t, cfg) {
		if r["outboundTag"] == "a-ui-relay" {
			t.Error("开关开时不该再有直接指向真实出站的分流规则")
		}
	}
	// 出站：两个计量出站，是真实出站的克隆，只换 tag。
	outbounds := map[string]map[string]any{}
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); tag != "" {
			outbounds[tag] = ob
		}
	}
	for _, tag := range wantTags {
		clone, ok := outbounds[tag]
		if !ok {
			t.Fatalf("缺计量出站 %s", tag)
		}
		if clone["protocol"] != "freedom" {
			t.Errorf("%s protocol = %v，克隆不完整", tag, clone["protocol"])
		}
		settings, _ := clone["settings"].(map[string]any)
		if settings["domainStrategy"] != "UseIP" {
			t.Errorf("%s settings 没有克隆到（%v）", tag, settings)
		}
	}
	// 真实出站仍在（它可能被别的东西引用，比如模板里手写的规则）。
	if _, ok := outbounds["a-ui-relay"]; !ok {
		t.Error("真实出站不该被移除")
	}
}

// 全局规则（InboundIds 为空）按当前全部启用入站展开，入站 id 升序。
func TestInjectProxiedMeterExpandsGlobalRule(t *testing.T) {
	setupMeterPoolTest(t)
	inA := newTestInbound(t, 32035)
	inB := newTestInbound(t, 32036)
	inC := newTestInbound(t, 32037)
	inC.Enable = false
	if err := database.GetDB().Save(inC).Error; err != nil {
		t.Fatal(err)
	}
	g := seedGroup(t, "Claude", []string{"domain:claude.ai"})
	ob := seedOutbound(t, "a-ui-relay", `{"protocol":"freedom","settings":{}}`)
	rule := seedRule(t, nil, []int{g.Id}, model.ActionProxy, ob.Id, 1) // nil = 全局
	if err := (&SettingService{}).setString("meterProxiedTraffic", "1"); err != nil {
		t.Fatal(err)
	}

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); strings.HasPrefix(tag, "a-ui-meter-r-") {
			tags = append(tags, tag)
		}
	}
	want := []string{model.MeterRuleTag(inA.Id, rule.Id), model.MeterRuleTag(inB.Id, rule.Id)}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("展开结果 %v，期望 %v（停用的入站 C 不该出现，且按 id 升序）", tags, want)
	}
}

// block 规则不拆、不计量。
func TestInjectProxiedMeterLeavesBlockRulesAlone(t *testing.T) {
	setupMeterPoolTest(t)
	inA := newTestInbound(t, 32038)
	g := seedGroup(t, "Netflix", []string{"domain:netflix.com"})
	seedRule(t, []int{inA.Id}, []int{g.Id}, model.ActionBlock, 0, 0)
	if err := (&SettingService{}).setString("meterProxiedTraffic", "1"); err != nil {
		t.Fatal(err)
	}
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatal(err)
	}
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); strings.HasPrefix(tag, "a-ui-meter-r-") {
			t.Errorf("block 规则不该被拆成 B 类计量规则：%v", r)
		}
	}
}

// direct 动作克隆的是默认出站实际生效的那个（tagDefaultOutbound），不硬编码。
func TestInjectProxiedMeterClonesRenamedDefaultForDirect(t *testing.T) {
	setupMeterPoolTest(t)
	inA := newTestInbound(t, 32039)
	g := seedGroup(t, "直连组", []string{"domain:example.org"})
	seedRule(t, []int{inA.Id}, []int{g.Id}, model.ActionDirect, 0, 1)
	if err := (&SettingService{}).setString("meterProxiedTraffic", "1"); err != nil {
		t.Fatal(err)
	}
	cfg := newTemplateConfig(t)
	// 把模板首位出站改个名并加个可辨识的字段。
	var obs []map[string]any
	if err := json.Unmarshal(cfg.OutboundConfigs, &obs); err != nil {
		t.Fatal(err)
	}
	obs[0]["tag"] = "my-direct"
	obs[0]["sendThrough"] = "0.0.0.0"
	raw, _ := json.Marshal(obs)
	cfg.OutboundConfigs = raw

	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatal(err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); strings.HasPrefix(tag, "a-ui-meter-r-") {
			if ob["sendThrough"] != "0.0.0.0" {
				t.Errorf("direct 的计量出站没有克隆自改过名的默认出站：%v", ob)
			}
			return
		}
	}
	t.Error("没有生成 direct 的计量出站")
}
```

`injectToBytes` 若不存在，在测试文件里加：

```go
// injectToBytes 跑一次注入并把出站与路由序列化，供逐字节比较。
func injectToBytes(t *testing.T) []byte {
	t.Helper()
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	return append(append([]byte{}, cfg.OutboundConfigs...), cfg.RouterConfig...)
}
```

测试文件 import 加 `"bytes"`、`"reflect"`、`"strings"`（缺哪个加哪个）。

- [ ] **Step 2: 运行确认失败**

```bash
go test ./web/service/ -run 'TestInjectProxiedMeter' -v 2>&1 | grep -E "^(--- |ok|FAIL|.*undefined)"
```

预期：`TestInjectProxiedMeterOffIsByteIdentical` 可能已 PASS（现状本来就没有 B 类），其余四条 FAIL（找不到 B 类规则/出站）。第一条现在就过是**正常的**——它是回归守卫，实现之后仍然必须过。

- [ ] **Step 3: 实现**

`web/service/routing_inject.go`：

在 `buildRules` 之前加类型：

```go
// proxiedMeterNeed 是 buildRule 在开关打开时提出的「需要一个计量出站」的请求：
// 把 TargetTag 那个出站深拷贝一份、tag 换成 MeterRuleTag(InboundId, RuleId)。
//
// 由 buildRule 提需求、appendProxiedMeterOutbounds 统一满足，而不是在 buildRule
// 里直接改出站数组：规则生成与出站生成的顺序是「先出站后规则」（规则要引用
// 出站的 tag），B 类计量反过来要求「先知道规则才知道要克隆谁」，所以出站数组
// 的最终序列化被挪到了 buildRules 之后。
type proxiedMeterNeed struct {
	InboundId int
	RuleId    int
	TargetTag string
}
```

`buildRules` 改为：

```go
func (s *RoutingInjector) buildRules(
	inboundTagById map[int]string,
	outboundTagById map[int]string,
	defaultOutboundTag string,
) ([]any, []any, []proxiedMeterNeed, error) {
	rules, err := s.ruleService.GetEnabled()
	if err != nil {
		return nil, nil, nil, err
	}
	if len(rules) == 0 {
		return nil, nil, nil, nil
	}
	expand, err := s.settingService.GetMeterProxiedTraffic()
	if err != nil {
		return nil, nil, nil, err
	}
	// 全局规则展开用的入站列表，按 id 升序——这是「生成逐字节确定」的一部分，
	// 禁止改用遍历 map 产生顺序。
	allInboundIds := make([]int, 0, len(inboundTagById))
	for id := range inboundTagById {
		allInboundIds = append(allInboundIds, id)
	}
	sort.Ints(allInboundIds)

	blockRules := make([]any, 0)
	routeRules := make([]any, 0)
	needs := make([]proxiedMeterNeed, 0)
	for _, rule := range rules {
		generated, isBlock, ruleNeeds, skip := s.buildRule(rule, inboundTagById, outboundTagById,
			defaultOutboundTag, expand, allInboundIds)
		if skip != nil {
			logger.Warning("skip routing rule, id:", rule.Id, "remark:", rule.Remark,
				"reason:", skip)
			continue
		}
		for _, g := range generated {
			if isBlock {
				blockRules = append(blockRules, g)
			} else {
				routeRules = append(routeRules, g)
			}
		}
		needs = append(needs, ruleNeeds...)
	}
	return blockRules, routeRules, needs, nil
}
```

`buildRule` 签名与结尾改为（前面取域名组、入站、出站的逻辑**一个字节不动**，只改签名和 `emit` 之后的部分）：

```go
func (s *RoutingInjector) buildRule(
	rule *model.RoutingRule,
	inboundTagById map[int]string,
	outboundTagById map[int]string,
	defaultOutboundTag string,
	expand bool,
	allInboundIds []int,
) ([]map[string]any, bool, []proxiedMeterNeed, error) {
	// …… 原有逻辑到「var outboundTag string / isBlock / switch」为止不变，
	// 只是每个 return nil, false, err 改成 return nil, false, nil, err ……

	// 开关关，或 block 动作：走原来的形态，一个字节不变。
	if !expand || isBlock {
		generated := emitRules(domains, cidrs, inboundTags, outboundTag)
		return generated, isBlock, nil, nil
	}

	// 开关开：按入站展开。每个入站一份规则（domain 一条 + ip 一条），
	// outboundTag 换成该入站专属的计量出站，真实出站的 tag 作为克隆来源
	// 通过 needs 交给 appendProxiedMeterOutbounds。
	//
	// 展开顺序：显式指定入站的按 inboundTags（已由 InboundIds 升序保证）；
	// 全局规则按 allInboundIds（调用方已排序）。两者都是逐字节确定的。
	targetIds := inboundIdsOf(inboundTags, inboundTagById, allInboundIds)
	generated := make([]map[string]any, 0, len(targetIds)*2)
	needs := make([]proxiedMeterNeed, 0, len(targetIds))
	for _, id := range targetIds {
		tag := inboundTagById[id]
		meterTag := model.MeterRuleTag(id, rule.Id)
		generated = append(generated, emitRules(domains, cidrs, []string{tag}, meterTag)...)
		needs = append(needs, proxiedMeterNeed{InboundId: id, RuleId: rule.Id, TargetTag: outboundTag})
	}
	return generated, false, needs, nil
}

// emitRules 把条件与出站组装成 0~2 条 xray 规则，domain 在前、ip 在后。
//
// 从 buildRule 里抽出来是因为展开时要对每个入站各调一次；形态与抽出前
// 逐字节相同（空 inboundTags 不输出该键，见 buildRule 的注释）。
func emitRules(domains, cidrs, inboundTags []string, outboundTag string) []map[string]any {
	generated := make([]map[string]any, 0, 2)
	emit := func(conditionKey string, values []string) {
		g := map[string]any{
			"type":        "field",
			conditionKey:  values,
			"outboundTag": outboundTag,
		}
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
	return generated
}

// inboundIdsOf 把 buildRule 算出的 inboundTags 还原成入站 id 列表；
// 空（全局规则）时返回全部启用入站。两种来源都已经是升序。
func inboundIdsOf(inboundTags []string, inboundTagById map[int]string, allInboundIds []int) []int {
	if len(inboundTags) == 0 {
		return allInboundIds
	}
	tagToId := make(map[string]int, len(inboundTagById))
	for id, tag := range inboundTagById {
		tagToId[tag] = id
	}
	ids := make([]int, 0, len(inboundTags))
	for _, tag := range inboundTags {
		ids = append(ids, tagToId[tag])
	}
	return ids
}
```

把原 `buildRule` 末尾那段 `generated := make(...)` / `emit := func(...)` / `if len(domains) > 0 {...}` / `return generated, isBlock, nil` **删掉**，由上面的分支替代。`routing_inject.go` 的 import 补 `"sort"`。

`Inject` 里对应改动（本任务只改调用形态，出站克隆在 Task 4）：

```go
	blockRules, routeRules, proxiedNeeds, err := s.buildRules(inboundTagById, usableOutboundTags, defaultOutboundTag)
	if err != nil {
		return err
	}
	_ = proxiedNeeds // Task 4 消费
```

- [ ] **Step 4: 运行确认通过（除克隆相关）**

```bash
go test ./web/service/ -run 'TestInject' -v 2>&1 | grep -E "^(--- |ok|FAIL)"
```

预期：`OffIsByteIdentical`、`LeavesBlockRulesAlone` PASS；`SplitsRulePerInbound`、`ExpandsGlobalRule`、`ClonesRenamedDefaultForDirect` 因为**出站还没克隆**仍 FAIL（报「缺计量出站」）——这是预期的，Task 4 让它们过。**既有的全部 `TestInject*` 必须 PASS**，尤其 `TestInjectMeterIsByteStable` 和 `TestInjectWithEmptyPoolChangesNothing`。

- [ ] **Step 5: 提交（半成品也提交，Task 4 紧接着）**

```bash
git add web/service/routing_inject.go web/service/routing_inject_meter_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 开关打开时分流规则按入站展开并指向 B 类计量出站

buildRule 在开关开时对每个入站各生成一份规则，outboundTag 换成
MeterRuleTag(入站, 规则)，并把真实出站的 tag 作为克隆来源通过
proxiedMeterNeed 交出去；开关关或 block 动作走原来的形态，逐字节不变。
全局规则按全部启用入站升序展开——不展开 B 对全局规则无效，而生产机
7 条规则里 5 条是全局的。

出站克隆在下一个提交：规则生成与出站生成的顺序要反过来一次。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 4: 克隆计量出站，`Inject` 挪动出站序列化

**Files:**
- Modify: `web/service/routing_inject.go`
- Test: `web/service/routing_inject_meter_test.go`（Task 3 的三条待过用例 + 一条新用例）

**Interfaces:**
- Consumes: `[]proxiedMeterNeed`（Task 3）
- Produces: `appendProxiedMeterOutbounds(outbounds []any, needs []proxiedMeterNeed) ([]any, error)`

- [ ] **Step 1: 写失败测试（新增一条）**

```go
// 找不到克隆来源必须让整份配置生成失败，绝不跳过：规则已经改成引用计量
// 出站了，出站却没克隆出来就是悬空引用——xray 对悬空 outboundTag 静默回落
// 默认出站，本该走 IProyal 的 ChatGPT 会静默走直连，而面板首页显示 running。
// 生成失败时 xray 保持原状继续跑，是安全的一侧。
func TestAppendProxiedMeterOutboundsFailsOnMissingTarget(t *testing.T) {
	outbounds := []any{
		map[string]any{"protocol": "freedom", "tag": "a-ui-default"},
	}
	_, err := appendProxiedMeterOutbounds(outbounds, []proxiedMeterNeed{
		{InboundId: 7, RuleId: 9, TargetTag: "a-ui-gone"},
	})
	if err == nil {
		t.Fatal("克隆来源不存在时必须返回错误")
	}
}

// 克隆是深拷贝：改克隆体不影响原出站，反之亦然。
func TestAppendProxiedMeterOutboundsDeepCopies(t *testing.T) {
	src := map[string]any{"protocol": "vmess", "tag": "a-ui-relay",
		"settings": map[string]any{"vnext": []any{map[string]any{"address": "1.2.3.4"}}}}
	outbounds := []any{map[string]any{"protocol": "freedom", "tag": "a-ui-default"}, src}
	got, err := appendProxiedMeterOutbounds(outbounds, []proxiedMeterNeed{
		{InboundId: 7, RuleId: 9, TargetTag: "a-ui-relay"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("出站数 %d，期望 3", len(got))
	}
	clone := got[2].(map[string]any)
	if clone["tag"] != model.MeterRuleTag(7, 9) {
		t.Errorf("tag = %v", clone["tag"])
	}
	clone["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["address"] = "changed"
	if src["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["address"] != "1.2.3.4" {
		t.Error("不是深拷贝：改克隆体影响了原出站")
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./web/service/ -run 'TestAppendProxiedMeterOutbounds' -v 2>&1 | tail -3
```

预期：编译失败，`undefined: appendProxiedMeterOutbounds`

- [ ] **Step 3: 实现**

`routing_inject.go`，紧接 `appendMeterOutbounds` 之后：

```go
// appendProxiedMeterOutbounds 按 needs 把真实出站深拷贝成 B 类计量出站追加到末尾。
//
// 克隆而不是 proxySettings 链式（设计 §5.4）：与 appendMeterOutbounds「深拷贝
// 默认出站」完全同构，转发路径与原出站一字不差、无额外转发层；配置不会漂移，
// 生成期每次重新从 outbounds 里取最新的那份来拷。
//
// 找不到来源时返回错误让整份配置生成失败，绝不跳过——见 Global Constraints。
// 同一份 (入站, 规则) 只克隆一次：同一条规则在 buildRule 里对同一入站不会
// 提两次需求，但这里仍按 tag 去重，防线不依赖上游的调用纪律。
func appendProxiedMeterOutbounds(outbounds []any, needs []proxiedMeterNeed) ([]any, error) {
	if len(needs) == 0 {
		return outbounds, nil
	}
	byTag := make(map[string][]byte, len(outbounds))
	for _, item := range outbounds {
		ob, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := ob["tag"].(string)
		if tag == "" {
			continue
		}
		encoded, err := json.Marshal(ob)
		if err != nil {
			return nil, err
		}
		byTag[tag] = encoded
	}
	seen := make(map[string]bool, len(needs))
	for _, n := range needs {
		meterTag := model.MeterRuleTag(n.InboundId, n.RuleId)
		if seen[meterTag] {
			continue
		}
		seen[meterTag] = true
		encoded, ok := byTag[n.TargetTag]
		if !ok {
			return nil, common.NewError("B 类计量出站的克隆来源不存在, 入站:", n.InboundId,
				"规则:", n.RuleId, "目标 tag:", n.TargetTag,
				"（规则已改成引用计量出站，来源缺失会造成悬空引用，整份配置拒绝生成）")
		}
		var clone map[string]any
		if err := json.Unmarshal(encoded, &clone); err != nil {
			return nil, err
		}
		clone["tag"] = meterTag
		outbounds = append(outbounds, clone)
	}
	return outbounds, nil
}
```

`Inject` 的流程改为（把出站的序列化挪到 `buildRules` 之后）：

```go
	outbounds, usableOutboundTags, defaultOutboundTag, err := s.buildOutbounds(cfg.OutboundConfigs)
	if err != nil {
		return err
	}
	outbounds, meterPool, err = appendMeterOutbounds(outbounds, meterPool)
	if err != nil {
		return err
	}

	// 规则要在出站序列化之前生成：B 类计量的克隆来源由规则决定（开关打开时
	// 每条规则对每个入站要一份真实出站的拷贝），所以「先出站后规则」的顺序
	// 在这里反过来一次。出站数组在下面追加完克隆体之后再序列化。
	blockRules, routeRules, proxiedNeeds, err := s.buildRules(inboundTagById, usableOutboundTags, defaultOutboundTag)
	if err != nil {
		return err
	}
	outbounds, err = appendProxiedMeterOutbounds(outbounds, proxiedNeeds)
	if err != nil {
		return err
	}
	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)
```

（删掉原来紧跟 `appendMeterOutbounds` 之后那段 `encodedOutbounds` 序列化，以及 Task 3 留下的 `_ = proxiedNeeds`。）

- [ ] **Step 4: 运行确认通过**

```bash
go test ./web/service/ -run 'TestInject|TestAppendProxiedMeter' -v 2>&1 | grep -E "^(--- |ok|FAIL)"
```

预期：Task 3 的五条 + 本任务两条全部 PASS；**既有的全部 `TestInject*` PASS**（`TestInjectMeterIsByteStable`、`TestInjectWithEmptyPoolChangesNothing`、`TestInjectAppendsMeterOutboundsAsDefaultCopies` 尤其）。

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_inject.go web/service/routing_inject_meter_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 克隆真实出站为 B 类计量出站，出站序列化挪到规则生成之后

克隆而不是 proxySettings 链式：与 appendMeterOutbounds「深拷贝默认出站」
完全同构，转发路径与原出站一字不差；配置不会漂移，生成期每次重新拷。

找不到克隆来源时整份配置拒绝生成而不是跳过：规则已经改成引用计量
出站了，出站没克隆出来就是悬空引用，xray 对它静默回落默认出站，
本该走 IProyal 的 ChatGPT 会静默走直连而面板显示 running。生成失败时
xray 保持原状继续跑，是安全的一侧。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 5: 规则删除连带清理 + 孤儿兜底

**Files:**
- Modify: `web/service/domain_stat.go`（`DeleteByRule` / `PruneOrphanRules`）、`web/service/routing_rule.go`（`Del`）、`web/job/traffic_cleanup_job.go`
- Test: `web/service/domain_stat_test.go`

**Interfaces:**
- Produces: `(*DomainStatService).DeleteByRule(ruleId int) error`、`(*DomainStatService).PruneOrphanRules() (int64, error)`

- [ ] **Step 1: 写失败测试**

追加到 `web/service/domain_stat_test.go`（用同文件的 `setupDomainStatTest` / `putDomainStat`）：

```go
// 删规则必须连带删它的计量数据：SQLite 复用自增 id，残留的 rule:9 行会
// 绑到下一条新建的规则上，而引用不再悬空、生成期防线拦不住。
func TestDeleteByRuleRemovesOnlyThatRule(t *testing.T) {
	setupDomainStatTest(t)
	putDomainStat(t, 1, model.RuleStatKey(9), 1000, 0, 100, 200)
	putDomainStat(t, 1, model.RuleStatKey(10), 1000, 0, 100, 200)
	putDomainStat(t, 1, "google.com", 1000, 5, 0, 0)

	if err := (&DomainStatService{}).DeleteByRule(9); err != nil {
		t.Fatal(err)
	}
	var left []string
	database.GetTrafficDB().Model(&model.DomainStat{}).Order("domain").Pluck("domain", &left)
	want := []string{"google.com", model.RuleStatKey(10)}
	if !reflect.DeepEqual(left, want) {
		t.Errorf("剩余 %v，期望 %v", left, want)
	}
}

// 兜底：规则表里已经不存在的 rule:<id> 行被清掉，其余不动。
func TestPruneOrphanRulesDropsRowsOfDeletedRules(t *testing.T) {
	setupDomainStatTest(t)
	g := seedGroup(t, "组", []string{"domain:a.com"})
	ob := seedOutbound(t, "a-ui-x", `{"protocol":"freedom","settings":{}}`)
	alive := seedRule(t, nil, []int{g.Id}, model.ActionProxy, ob.Id, 1)
	putDomainStat(t, 1, model.RuleStatKey(alive.Id), 1000, 0, 1, 1)
	putDomainStat(t, 1, model.RuleStatKey(alive.Id+1000), 1000, 0, 1, 1) // 不存在的规则
	putDomainStat(t, 1, "google.com", 1000, 5, 0, 0)

	n, err := (&DomainStatService{}).PruneOrphanRules()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("清理了 %d 行，期望 1", n)
	}
	var left []string
	database.GetTrafficDB().Model(&model.DomainStat{}).Order("domain").Pluck("domain", &left)
	if len(left) != 2 {
		t.Errorf("剩余 %v，期望 google.com 与存活规则各一行", left)
	}
}
```

- [ ] **Step 2: 运行确认失败**

```bash
go test ./web/service/ -run 'TestDeleteByRule|TestPruneOrphanRules' -v 2>&1 | tail -3
```

预期：编译失败，`DeleteByRule undefined`

- [ ] **Step 3: 实现**

`domain_stat.go`，紧接 `DeleteByInbound` 之后：

```go
// DeleteByRule 删除某条分流规则的全部 B 类计量数据（两级都删）。
//
// 必须在删除规则时调用。SQLite 会复用被删除的自增 id，不删的话下一条新建
// 的规则会继承上一条的字节数，而且因为引用不再悬空，任何「跳过悬空引用」
// 式的防线都拦不住它。
func (s *DomainStatService) DeleteByRule(ruleId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("domain = ?", model.RuleStatKey(ruleId)).Delete(&model.DomainStat{}).Error
}

// PruneOrphanRules 清掉规则表里已不存在的规则遗留的 B 类计量行，返回行数。
//
// DeleteByRule 的兜底：面板崩溃在删规则与删计量之间、或直接改库删规则，
// 都会留下孤儿。挂在 TrafficCleanupJob 里每小时跑一次。
func (s *DomainStatService) PruneOrphanRules() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.RoutingRule{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, model.RuleStatKey(id))
	}
	tx := db.Where("domain like ?", "rule:%")
	if len(keys) > 0 {
		tx = tx.Where("domain not in ?", keys)
	}
	result := tx.Delete(&model.DomainStat{})
	return result.RowsAffected, result.Error
}
```

`routing_rule.go` 的 `Del`：

```go
func (s *RoutingRuleService) Del(id int) error {
	// 先删计量数据再删规则，与 DomainGroupService.Del「先删子行再删组」同序：
	// 反过来的话两步之间崩溃会留下一条已删规则的孤儿计量行，等着被复用的 id
	// 认领。PruneOrphanRules 是兜底，不是替代。
	if err := (&DomainStatService{}).DeleteByRule(id); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Delete(model.RoutingRule{}, id).Error
}
```

`traffic_cleanup_job.go` 的 `Run`，紧接 `domainStatService.PruneOrphans()` 那段之后：

```go
	if pruned, err := j.domainStatService.PruneOrphanRules(); err != nil {
		logger.Warning("清理孤儿规则计量数据失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除分流规则遗留的计量数据", pruned)
	}
```

- [ ] **Step 4: 运行确认通过**

```bash
go test ./web/service/ ./web/job/ -run 'TestDeleteByRule|TestPruneOrphan|TestTrafficCleanup' 2>&1 | tail -3
```

- [ ] **Step 5: 提交**

```bash
git add web/service/domain_stat.go web/service/routing_rule.go web/job/traffic_cleanup_job.go web/service/domain_stat_test.go
git commit -m "$(cat <<'EOF'
fix(meter): 删分流规则时连带删它的 B 类计量数据，另加孤儿兜底

SQLite 复用自增 id：不删的话下一条新建的规则会继承上一条的字节数，
而引用不再悬空、生成期防线拦不住。与 InboundService.DelInbound 连带删
TrafficHistory / DomainStat 同构，是本仓库第 4 条「存 id 外键 + 被引用方
可删除」的组合。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 6: 榜单显示规则行

**Files:**
- Modify: `web/service/domain_stat.go`（`topDomainKind` / `TopDomainRow.Label` / `TopDomains` 填标签）、`web/html/xui/access_log_modal.html`
- Test: `web/service/domain_stat_top_test.go`

**Interfaces:**
- Produces: `TopDomainRow.Kind` 新增取值 `"rule"`；`TopDomainRow.Label string`（规则行为「备注 → 出站备注」，其余为空）

- [ ] **Step 1: 写失败测试**

```go
// 规则行：Kind 是 rule，Label 是「规则备注 → 出站备注」；规则已删则退化成
// 「规则 #9（已删除）」而不是整行消失——字节是真实发生过的。
func TestTopDomainsLabelsRuleRows(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31831, "甲")
	now := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)
	g := seedGroup(t, "ChatGPT", []string{"domain:chatgpt.com"})
	ob := seedOutboundWithRemark(t, "a-ui-relay", "美区家宽", `{"protocol":"freedom","settings":{}}`)
	rule := seedRuleWithRemark(t, "AI 分流", []int{in.Id}, []int{g.Id}, model.ActionProxy, ob.Id, 1)
	putDomainStat(t, in.Id, model.RuleStatKey(rule.Id), bucket, 0, 1000, 5000)
	putDomainStat(t, in.Id, model.RuleStatKey(rule.Id+1000), bucket, 0, 1, 1) // 已删规则
	putPoolRow(t, in.Id, "acspubs.org", 0)

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderDown, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	byDomain := map[string]TopDomainRow{}
	for _, r := range got.List {
		byDomain[r.Domain] = r
	}
	r := byDomain[model.RuleStatKey(rule.Id)]
	if r.Kind != "rule" {
		t.Errorf("Kind = %q，期望 rule", r.Kind)
	}
	if r.Label != "AI 分流 → 美区家宽" {
		t.Errorf("Label = %q", r.Label)
	}
	gone := byDomain[model.RuleStatKey(rule.Id+1000)]
	if gone.Kind != "rule" || !strings.Contains(gone.Label, "已删除") {
		t.Errorf("已删规则的行 = %+v，期望 Kind=rule 且 Label 含「已删除」", gone)
	}
}
```

> `seedOutboundWithRemark` / `seedRuleWithRemark` 是假定名；若现成辅助不带
> remark 参数，就在测试里建完再 `database.GetDB().Model(...).Update("remark", ...)`。

- [ ] **Step 2: 运行确认失败**

```bash
go test ./web/service/ -run TestTopDomainsLabelsRuleRows -v 2>&1 | tail -3
```

预期：编译失败（`r.Label undefined`）

- [ ] **Step 3: 实现**

`domain_stat.go`：

```go
type TopDomainRow struct {
	Domain string `json:"domain"`
	// Kind 是 "domain" / "ip" / "rule"，由 Domain 的形态推导，不落库。
	// …（保留原注释）…
	Kind string `json:"kind"`
	// Label 只对 rule 行非空：「规则备注 → 出站备注」。规则或出站已删时退化成
	// 带「已删除」的说明，行不消失——字节是真实发生过的，隐藏它等于把差额
	// 塞回「未归因」。
	Label string `json:"label"`
	Count int64  `json:"count"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}

// topDomainKind 由目标的形态推导它的类型。先判 rule: 前缀再判 IP：
// IPv6 含冒号，顺序反了 rule:9 不会误判但意图不清。
func topDomainKind(d string) string {
	if model.IsRuleStatKey(d) {
		return "rule"
	}
	if net.ParseIP(d) != nil {
		return "ip"
	}
	return "domain"
}
```

`TopDomains` 里填值那段改为：

```go
	if rows != nil {
		labels := s.ruleLabels(rows)
		for i := range rows {
			rows[i].Kind = topDomainKind(rows[i].Domain)
			if rows[i].Kind == "rule" {
				rows[i].Label = labels[rows[i].Domain]
			}
		}
		result.List = rows
	}
```

并加方法：

```go
// ruleLabels 为榜单里的规则行生成「规则备注 → 出站备注」。一次把涉及的
// 规则与出站都查出来，不在循环里逐行查库。
func (s *DomainStatService) ruleLabels(rows []TopDomainRow) map[string]string {
	labels := make(map[string]string)
	ids := make([]int, 0)
	for _, r := range rows {
		if id, ok := model.ParseRuleStatKey(r.Domain); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return labels
	}
	var rules []model.RoutingRule
	if err := database.GetDB().Where("id in ?", ids).Find(&rules).Error; err != nil {
		logger.Warning("榜单取规则备注失败:", err)
	}
	var nodes []model.OutboundNode
	if err := database.GetDB().Find(&nodes).Error; err != nil {
		logger.Warning("榜单取出站备注失败:", err)
	}
	nodeRemark := make(map[int]string, len(nodes))
	for _, n := range nodes {
		nodeRemark[n.Id] = n.Remark
	}
	byId := make(map[int]model.RoutingRule, len(rules))
	for _, r := range rules {
		byId[r.Id] = r
	}
	for _, id := range ids {
		key := model.RuleStatKey(id)
		rule, ok := byId[id]
		if !ok {
			labels[key] = fmt.Sprintf("规则 #%d（已删除）", id)
			continue
		}
		name := rule.Remark
		if name == "" {
			name = fmt.Sprintf("规则 #%d", id)
		}
		var target string
		switch rule.Action {
		case model.ActionDirect:
			target = "直连"
		case model.ActionProxy:
			target = nodeRemark[rule.OutboundId]
			if target == "" {
				target = fmt.Sprintf("出站 #%d（已删除）", rule.OutboundId)
			}
		default:
			target = rule.Action
		}
		labels[key] = name + " → " + target
	}
	return labels
}
```

import 补 `"fmt"`。

- [ ] **Step 4: 前端**

`access_log_modal.html`：

类型标签的 slot 改为三态：

```html
                <template slot="topKind" slot-scope="text, row">
                    <a-tag :color="row.kind === 'rule' ? 'purple' : (row.kind === 'ip' ? 'orange' : 'blue')">
                        [[ row.kind === 'rule' ? '规则' : (row.kind === 'ip' ? 'IP' : '域名') ]]
                    </a-tag>
                </template>
```

目标 slot：规则行显示 Label、不可点击（明细是按目标关键字过滤的，`rule:9` 查不到东西）：

```html
                <template slot="domain" slot-scope="text, row">
                    <span v-if="row.kind === 'rule'">[[ row.label ]]</span>
                    <a v-else style="cursor: pointer" @click="accessLogModal.drillDown(row.domain)">[[ row.domain ]]</a>
                </template>
```

访问次数列改用 slot，规则行显示 `—`：把列定义里 `dataIndex: "count"` 那一项改成 `scopedSlots: { customRender: 'topCount' }`，并加：

```html
                <template slot="topCount" slot-scope="text, row">
                    [[ row.kind === 'rule' ? '—' : row.count ]]
                </template>
```

覆盖率下方那段说明改成：

```html
                <div>
                    字节数统计<b>走直连出去</b>的流量；打开「面板设置 → 计量被分流带走的流量」后，被你的分流规则带去出站节点的部分也会以「规则」行给出字节数（按规则合计，不拆到单个域名）。
                    被封禁的部分见下方分解。计量的目标集合每小时自动调整一次，刚上线时排序主要来自访问次数，运行一段时间后才由实测流量主导。
                </div>
```

「未归因」的说明改回：

```html
<span style="color: rgba(0,0,0,.45)">余项：计量池外的长尾 + 未打开分流计量时被规则带走的部分</span>
```

- [ ] **Step 5: 运行确认通过**

```bash
go test ./web/service/ -run 'TestTopDomains' 2>&1 | tail -2
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' 2>&1 | tail -1
```

- [ ] **Step 6: 提交**

```bash
git add web/service/domain_stat.go web/service/domain_stat_top_test.go web/html/xui/access_log_modal.html
git commit -m "$(cat <<'EOF'
feat(meter): 榜单显示「规则」行，标签为「规则备注 → 出站备注」

规则或出站已删时退化成带「已删除」的说明，行不消失——字节是真实发生
过的，隐藏它等于把差额塞回「未归因」。规则行没有访问次数（那是按目标
归并的另一个维度）、不可点击（明细按目标关键字过滤，rule:9 查不到东西）。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 7: 真实 xray 验证分流结果不变

这是 spec §5.5 那个风险的**唯一防线**：开关打开后，被分流的流量必须仍从原来的出站出去。

**Files:**
- Modify: `web/service/meter_rule_e2e_test.go`

- [ ] **Step 1: 写测试**

参照同文件 `TestMeterRuleShapeAgainstRealXray` 的起 xray / socks 读取方式（`waitForPort` / `socksReadDomain` 等辅助直接复用）：

```go
// 开关打开后分流结果必须一个字节不变：被规则带走的流量仍从原出站出去，
// 且字节记在 B 类计量出站的计数器上。
//
// 判据：真实出站是 freedom 指向本地 listener（回 HELLO），默认出站是黑洞。
// 「读到 HELLO」= 流量确实走了那条路；开关开与关都必须读到 HELLO。
// 再用 statsquery 确认开关开时 B 类出站的 uplink/downlink 都非零。
func TestProxiedMeterKeepsRoutingAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	target, targetPort := listenHello(t) // 复用同文件的本地 HELLO 服务辅助
	defer target.Close()

	run := func(t *testing.T, proxied bool) (string, map[string]int64) {
		relayTag := "a-ui-relay"
		ruleOut := relayTag
		outbounds := []any{
			map[string]any{"tag": "a-ui-default", "protocol": "blackhole", "settings": map[string]any{}},
			map[string]any{"tag": relayTag, "protocol": "freedom", "settings": map[string]any{"domainStrategy": "UseIP"}},
		}
		if proxied {
			ruleOut = model.MeterRuleTag(1, 9)
			outbounds = append(outbounds, map[string]any{"tag": ruleOut, "protocol": "freedom",
				"settings": map[string]any{"domainStrategy": "UseIP"}})
		}
		apiPort := freePort(t)
		socksPort := freePort(t)
		cfg := map[string]any{
			"log": map[string]any{"loglevel": "warning"},
			"api": map[string]any{"tag": "api", "services": []any{"StatsService"}},
			"stats": map[string]any{},
			"policy": map[string]any{"system": map[string]any{"statsOutboundUplink": true, "statsOutboundDownlink": true}},
			"dns": map[string]any{"hosts": map[string]any{"meter.test": "127.0.0.1"}, "servers": []any{"localhost"}},
			"inbounds": []any{
				map[string]any{"tag": "in", "listen": "127.0.0.1", "port": socksPort, "protocol": "socks",
					"settings": map[string]any{"auth": "noauth", "udp": false}},
				map[string]any{"tag": "api", "listen": "127.0.0.1", "port": apiPort, "protocol": "dokodemo-door",
					"settings": map[string]any{"address": "127.0.0.1"}},
			},
			"outbounds": outbounds,
			"routing": map[string]any{"rules": []any{
				map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"},
				map[string]any{"type": "field", "inboundTag": []string{"in"}, "domain": []string{"domain:meter.test"}, "outboundTag": ruleOut},
			}},
		}
		encoded, _ := json.Marshal(cfg)
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(xray.GetBinaryPath(), "run", "-c", cfgPath)
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 xray: %v", err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		waitForPort(t, socksPort)
		got := socksReadDomain(fmt.Sprintf("127.0.0.1:%d", socksPort), "meter.test", targetPort)
		stats := queryOutboundStats(t, apiPort) // tag -> uplink+downlink
		return got, stats
	}

	t.Run("开关关：走真实出站", func(t *testing.T) {
		got, _ := run(t, false)
		if got != "HELLO" {
			t.Fatalf("读到 %q，期望 HELLO", got)
		}
	})
	t.Run("开关开：仍走真实出站的克隆，且计量出站有字节", func(t *testing.T) {
		got, stats := run(t, true)
		if got != "HELLO" {
			t.Fatalf("读到 %q，期望 HELLO——开关打开改变了分流结果，这是本期最严重的失败模式", got)
		}
		if stats[model.MeterRuleTag(1, 9)] == 0 {
			t.Errorf("B 类计量出站没有字节，统计：%v", stats)
		}
	})
}
```

> `listenHello` / `freePort` / `queryOutboundStats` 若同文件没有，参照
> `TestMeterRuleShapeAgainstRealXray` 里起本地 listener 与 `xray/api.go`
> 的 gRPC 统计查询各写一个最小版本。这条测试起两次真实 xray，跑起来约 3~5 秒。

- [ ] **Step 2: 运行**

```bash
go test ./web/service/ -run TestProxiedMeterKeepsRoutingAgainstRealXray -v 2>&1 | tail -8
```

预期：两个子用例 PASS。**「开关开」那条若读不到 HELLO，停下来** ——那意味着克隆出站没有把流量送到原来的地方，是本期设计的根本性失败，不要试着在测试里绕。

- [ ] **Step 3: 提交**

```bash
git add web/service/meter_rule_e2e_test.go
git commit -m "$(cat <<'EOF'
test(meter): 真实 xray 验证开关打开后分流结果不变

这是「计量出站承载真实分流流量」那个风险的唯一防线：被规则带走的流量
必须仍从原出站的克隆出去（读到 HELLO），且字节落在 B 类计量出站的
计数器上。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 8: 全量验证与文档同步

- [ ] **Step 1: 门禁**

```bash
make verify; echo exit=$?; rm -f a-ui
```

预期 exit=0。e2e 因端口被占而 SKIP 要如实说明。

- [ ] **Step 2: 检查 diff 与工作区**

```bash
git status --short
git diff --stat main..HEAD
```

只动计划里列出的文件；无调试残留。

- [ ] **Step 3: 回写 spec**

`docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md`：

- §6 顶部那段「分期说明」改成：「**本节作废（2026-09-10 实施改动 B 后确认）**：A、B 两期都不需要 `Kind` 列。B 类计量以 `rule:<规则id>` 为 `DomainStat.Domain` 的键，类型由键的形态推导（`topDomainKind`：先判 `rule:` 前缀，再判 IP，否则域名）；`MeterDomain` 池表不参与 B 类计量。GORM `AutoMigrate` 不修改已存在索引那个风险因此不再存在。」
- §5.6 里「必须同时更新 `RecordMetered` 的归因分支」改成「`RecordMetered` 不需要改：`ParseMeterTag` 对 B 类 tag 返回的第二个值就是 `rule:<id>`，采集链路原样写库」。
- §5.3 补一句：「实施时把规则组装抽成 `emitRules`，展开时对每个入站各调一次；形态与抽出前逐字节相同。」

- [ ] **Step 4: 提交文档**

```bash
git add docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md
git commit -m "$(cat <<'EOF'
docs(spec): 回写改动 B 的实施结论——Kind 列整节作废

A、B 两期都不需要数据模型变更。B 类计量以 rule:<规则id> 为键，类型由
键的形态推导；RecordMetered 一行没改。GORM AutoMigrate 不修改已存在
索引那个风险因此不复存在。

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

## 上线后的验收

1. 升级后在「面板设置」打开「计量被分流带走的流量」，保存。这会走一次真实 xray 校验（约 4 秒）。
2. 日志里应出现 `xray 配置改动已通过控制面下发，无需重启`——开关切换走热应用。**若出现 `connection refused`（xray 重启了），说明热应用没扛住一次性增删约 98 个出站**，记下来，那是 spec §10 第 9 条要验的。
3. 等 10 分钟（`XrayTrafficJob` 采集），看 Xy Qin（入站 2）1 小时档：应出现一行紫色「规则」标签 `<规则备注> → 美区家宽（IProyal）`，带字节数；`chatgpt.com` 那行仍是 0 B（它由规则行计量了）；「未归因」从 13.9 MB 降到 1 MB 左右。
4. 凌晨 04:05 那次整进程重启的时长：`journalctl -u a-ui --since 04:00` 里两条 `connection refused` 的间隔，预期从 15 秒涨到 25~30 秒。

## Self-Review

**Spec 覆盖（改动 B = spec §5 全节 + §7 规则行）**

| spec 条目 | 任务 |
|---|---|
| §5.1 必须改分流规则本身 | Task 3 |
| §5.2 粒度按规则 | Task 1（键是规则 id）、Task 3 |
| §5.3 规则拆分、全局展开、block 不拆、顺序确定 | Task 3 |
| §5.4 克隆不用链式 | Task 4 |
| §5.5 总开关默认关、关时逐字节相同 | Task 2、Task 3 第一条测试 |
| §5.6 tag 形态、id 复用、删规则清理 | Task 1、Task 5 |
| §5.7 A/B 交互（被规则覆盖的域名白占池槽位） | 无代码，接受（spec 已定） |
| §5.8 不改 Config.Equals | 无代码；Task 3 逐字节测试守顺序 |
| §6 数据模型 | **作废**，Task 8 回写 |
| §7 规则行、无访问次数 | Task 6 |
| §10 第 9 条热应用扛批量 | **无自动化测试**，上线验收第 2 条人工看 |
| §10 第 11 条 e2e 分流不变 | Task 7 |

**占位符扫描**：无 TBD/TODO。「假定辅助名」处均有明确指令。

**类型一致性**：`proxiedMeterNeed{InboundId, RuleId int; TargetTag string}` 在 Task 3 定义、Task 4 消费；`MeterRuleTag(int, int) string` / `RuleStatKey(int) string` / `ParseRuleStatKey(string) (int, bool)` / `IsRuleStatKey(string) bool` 在 Task 1 定义，Task 3/5/6 使用；`buildRules` 四返回值、`buildRule` 六参数四返回值在 Task 3 定义并在同任务的 `Inject` 调用处更新；`emitRules(domains, cidrs, inboundTags []string, outboundTag string)` 在 Task 3 内定义与使用。
