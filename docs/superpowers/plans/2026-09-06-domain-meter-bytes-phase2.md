# 域名维度真实字节计量（第二期）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让访问日志弹窗的 Top 域名榜单显示每个域名真实的上传/下载字节数，全过程自动，管理员不需要事先圈定要监控哪些域名。

**Architecture:** xray 的 stats 计数器只有 inbound/outbound/user 三个维度，没有域名维度，所以唯一的办法是**给每个要计量的域名一个独立的出站 tag**。面板按小时维护一个「计量池」（每入站各自的 Top-K 注册域名，落库），生成期为池里每个 `(入站, 域名)` 生成一个默认出站的逐字节副本与一条追加在最末的路由规则，然后复用 `XrayTrafficJob` 每 10 秒那次 `GetTraffic(reset=true)` 把出站字节归到域名分时桶上。

**Tech Stack:** Go 1.27 + Gin + GORM/SQLite（CGO 必须开启）；xray-core v1.260327.1-0.20260728075948-5ca6f4b7d4dc；前端是 Vue 2 + ant-design-vue 的服务端模板，无打包工具。

**Spec:** `docs/superpowers/specs/2026-09-06-domain-traffic-attribution-design.md`（第二期是 §6，前端 §7，接口 §8，决策 §9，文件清单 §10，测试 §11，风险 §12。§4 的数据模型与 §5 的第一期已经实现并发版）

**分支：** 从 `main` 新建 `feat/domain-meter-bytes`，不要在 `main` 上直接改。

---

## Global Constraints

以下每一条都对**每个任务**生效，任务正文不再重复。

**并行会话的未提交改动——最高优先级：**

- 这个仓库有另一个会话正在改分流子系统。以下文件**一个字节都不能碰**，也绝不能提交：
  `web/controller/routing.go`、`web/service/routing_cidr.go`、`web/service/routing_cidr_test.go`、
  `web/service/routing_domain.go`、`web/service/routing_domain_test.go`、
  `web/service/routing_portable.go`、`web/service/routing_subscription_test.go`、
  `web/controller/routing_dedup_test.go`。
- **每一次提交都必须用路径限定的 `git add <具体文件>`，绝对禁止 `git add -A` / `git add .` / `git commit -a`。**
- 提交前跑一次 `git status --porcelain`，确认暂存区里只有本任务列出的文件。

**命名与常量（与 spec §6 逐字一致，不要改数值）：**

- 计量出站 tag 前缀：`a-ui-meter-`，完整形态 `a-ui-meter-<inboundId>-<domain>`，例 `a-ui-meter-3-doubleclick.net`
- `meterOutboundBudget = 200`（全部入站共用的计量出站总预算）
- `meterPoolCapMax = 60`（单个入站的池容量上限）
- `meterPoolRound = time.Hour`（一「轮」的长度，与 `MeterPoolJob` 的周期一致）
- `meterPoolWindow = 24 * time.Hour`（排序时回看多久的数据）
- `meterMinHoldRounds = 2`（最小驻留轮数）
- `meterProbeGiveUpRounds = 3`（连续几轮零字节即退场）
- `meterCooldown = 24 * time.Hour`（退场后的冷却时长）
- `meterSwapMargin = 1.25`（在位加成／替换余量）
- `meterStaleCounterLimit = 2000`（观测到的死计数器上限，超过即冻结换池）

**生成逐字节确定（违反会让那个 10 秒的 cron 不停重启 xray）：**

- `Config.Equals` 对 `OutboundConfigs` / `RouterConfig` 按字节比较。任何进入配置的列表都必须有完全确定的顺序。
- **禁止遍历 map 来产生数组顺序。** 一律先取出 key、排序，再遍历。
- 计量出站与计量规则都按 `(inboundId asc, domain asc)` 排序。

**路由与安全：**

- 计量规则一律**追加在所有现有规则之后**（模板规则 → 地区规则 → block 组 → proxy/direct 组 → 计量组）。
- 计量规则的形态按生成配置里 `routing.domainStrategy` 的**最终值**二选一：`ipifnonmatch` / `ipondemand`（不区分大小写）时带 `"ip": ["0.0.0.0/0", "::/0"]` 守卫，其余一律不带。判据不能取 `ipRuleResolveDomain` 这个设置项本身。
- `domainStrategy` 只能是 `UseIP`，**绝不能用 `ForceIP` 系列**。
- 计量出站必须是**默认出站的深拷贝**，除 `tag` 外逐字节相同；默认出站的 tag 取 `tagDefaultOutbound` 的返回值，**绝不硬编码 freedom / `model.DefaultOutboundTag`**。

**数据完整性：**

- 新增任何存入站 id 外键的表，都必须同时接上 `InboundService.DelInbound` 的级联删除**和** `TrafficCleanupJob` 里的 `PruneOrphans` 兜底。SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，而且因为引用不再悬空，任何「跳过悬空引用」式的防线都拦不住。
- 用量库不可用（`database.GetTrafficDB()` 为 nil）时一律 fail-open 到「没有这个功能」，绝不让配置生成失败。

**工程约定：**

- 新增 cron job 的 `Run` 首行必须是 `defer common.Recover("<任务名>")`。
- 面向用户的字符串（日志、报错、界面文案）一律简体中文。
- 注释解释非显然的原因、约束和权衡，不重复代码表面含义；篇幅参照同目录既有文件。
- **不新增任何设置项。** 新增设置项要同步改 5 处，漏一处会让整个保存配置接口失败。
- 每个任务结束前跑 `make verify`（`go vet ./... && go test ./... && go build`）。它是提交前的门禁。
- 提交信息用中文，结尾必须是这两行：

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
```

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `database/model/meter.go`（新建） | `MeterDomain` 表结构；计量 tag 的拼装与反查（`MeterTag` / `ParseMeterTag` / `IsMeterTag`） |
| `database/model/routing.go`（改 1 行体） | `IsReservedTag` 追加对计量 tag 前缀的拒绝 |
| `database/db.go`（改） | 用量库 `AutoMigrate` 加 `MeterDomain` |
| `util/domain/domain.go`（改） | 新增 `IsRegistrable`，判断一个字符串是否本身就是注册域名 |
| `web/service/meter_pool.go`（新建） | 计量池：读取、重算（排序 + 三道闸门）、级联删除、孤儿清理、死计数器观测的读写 |
| `web/service/meter_collect.go`（新建） | `DomainStatService.RecordMetered`：把出站字节归到域名分时桶 |
| `web/service/policy_inject.go`（新建） | `injectOutboundStats`：生成期打开 `policy.system.statsOutbound*` |
| `web/service/routing_inject.go`（改） | 生成计量出站与计量规则，追加在最末 |
| `web/service/dns_inject.go`（改） | 给计量出站补 `domainStrategy` |
| `web/service/xray.go`（改） | `GetXrayConfig` 调 `injectOutboundStats` |
| `web/service/inbound.go`（改） | `AddTraffic` 调 `RecordMetered`；`DelInbound` 级联删池行 |
| `web/service/domain_stat.go`（改） | `TopDomains` 支持 `orderBy`、`Metered`、`Coverage` |
| `web/job/meter_pool_job.go`（新建） | `@every 1h` 重算计量池 |
| `web/job/traffic_cleanup_job.go`（改） | 清理池的孤儿行 |
| `web/web.go`（改） | 注册 `MeterPoolJob` |
| `web/controller/inbound.go`（改） | `orderBy` 参数的校验与钳制 |
| `web/html/xui/access_log_modal.html`（改） | 字节列、排序切换、覆盖度条、收敛期提示 |
| `web/service/config.json`（改） | 模板 `policy.system` 补两个 key（只影响全新安装） |
| `CLAUDE.md`（改） | 记录新的保留 tag 前缀与计量子系统的约束 |

**与 spec §10 文件清单的两处有意偏差**（功能与归属不变，只是落在更合适的文件里）：

- spec 把 `injectOutboundStats` 记在 `web/service/xray.go`。这里单开
  `web/service/policy_inject.go`，与 `dns_inject.go` / `routing_inject.go` 并列；
  `xray.go` 只保留一行调用。`xray.go` 已经承担进程生命周期与热应用两件事，
  再塞一个注入器会让它继续膨胀。
- spec 把 `RecordMetered` 记在 `web/service/domain_stat.go`。这里它仍然是
  `DomainStatService` 的方法（服务归属与 spec 一致），但定义在
  `web/service/meter_collect.go`：`domain_stat.go` 已经约 450 行且主题是
  「访问日志聚合 + 榜单查询」，采集出站字节是计量子系统的事。覆盖度
  （`Coverage`）留在 `domain_stat.go`，它是榜单查询的一部分。

---

## Task 1: 计量 tag 的编解码与保留 tag 扩展

**Files:**
- Create: `database/model/meter.go`
- Modify: `database/model/routing.go`（只改 `IsReservedTag` 的函数体）
- Modify: `util/domain/domain.go`（追加 `IsRegistrable`）
- Test: `database/model/meter_test.go`（新建）
- Test: `util/domain/domain_test.go`（追加用例，文件已存在）

**Interfaces:**
- Produces:
  - `model.MeterOutboundTagPrefix = "a-ui-meter-"`（常量）
  - `model.MeterTag(inboundId int, domain string) string`
  - `model.ParseMeterTag(tag string) (inboundId int, domain string, ok bool)`
  - `model.IsMeterTag(tag string) bool`
  - `domain.IsRegistrable(d string) bool`
- Consumes: 无

- [ ] **Step 1: 写失败的测试（计量 tag 的编解码）**

新建 `database/model/meter_test.go`：

```go
package model

import "testing"

func TestMeterTagRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		inboundId int
		domain    string
	}{
		{"普通域名", 3, "doubleclick.net"},
		{"域名含短横线", 12, "some-cdn.example.com"},
		{"多级注册域名", 7, "example.co.uk"},
		{"入站 id 多位", 12345, "a.io"},
		{"域名首字符是数字", 1, "9gag.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tag := MeterTag(c.inboundId, c.domain)
			gotId, gotDomain, ok := ParseMeterTag(tag)
			if !ok {
				t.Fatalf("ParseMeterTag(%q) 判为不可解析", tag)
			}
			if gotId != c.inboundId || gotDomain != c.domain {
				t.Errorf("反查 = (%d, %q)，期望 (%d, %q)", gotId, gotDomain, c.inboundId, c.domain)
			}
		})
	}
}

func TestParseMeterTagRejectsMalformed(t *testing.T) {
	// 每一条都必须被拒绝：一个形态不对的 tag 是无法归因的，
	// 硬猜只会把字节记到错的域名上。
	bad := []string{
		"",                       // 空
		"a-ui-block",             // 别的保留 tag
		"a-ui-hk",                // 出站节点的 tag
		"inbound-2886",           // 入站 tag
		"a-ui-meter-",            // 只有前缀
		"a-ui-meter-3",           // 缺分隔符与域名
		"a-ui-meter-3-",          // 域名为空
		"a-ui-meter--x.com",      // id 为空
		"a-ui-meter-x-y.com",     // id 不是数字
		"a-ui-meter-0-x.com",     // id 必须为正：0 不是任何入站
		"a-ui-meter--1-x.com",    // 同上：首字符就是分隔符，切出来的 id 仍是空串
	}
	for _, tag := range bad {
		if _, _, ok := ParseMeterTag(tag); ok {
			t.Errorf("ParseMeterTag(%q) 判为可解析，期望拒绝", tag)
		}
	}
}

func TestIsReservedTagRejectsMeterPrefix(t *testing.T) {
	// 保留 tag 不在 outbound_nodes 表里，数据库唯一约束看不见它们；
	// 撞名会让 xray 报 existing tag found 并拒绝启动整份配置——全员断网，
	// 而面板首页仍显示 running。
	reserved := []string{
		BlockOutboundTag,
		DefaultOutboundTag,
		"a-ui-meter-3-doubleclick.net",
		"a-ui-meter-",       // 光是前缀也要拒绝：它不可能是一个合法节点 tag
		"a-ui-meter-乱七八糟", // 形态不对但仍带前缀，同样不能分配出去
	}
	for _, tag := range reserved {
		if !IsReservedTag(tag) {
			t.Errorf("IsReservedTag(%q) = false，期望 true", tag)
		}
	}
	for _, tag := range []string{"a-ui-hk", "a-ui-meterx", "blocked", "meter-3-x.com", ""} {
		if IsReservedTag(tag) {
			t.Errorf("IsReservedTag(%q) = true，期望 false", tag)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./database/model/ -run 'MeterTag|IsReservedTagRejectsMeter' -v`
Expected: 编译失败，`undefined: MeterTag`、`undefined: ParseMeterTag`

- [ ] **Step 3: 实现 `database/model/meter.go` 的 tag 部分**

新建 `database/model/meter.go`：

```go
package model

import (
	"strconv"
	"strings"
)

// MeterOutboundTagPrefix 是计量出站 tag 的固定前缀。
//
// 计量出站是「给每个被计量的注册域名一个独立 tag」这件事的载体：xray 的
// stats 计数器只有 inbound/outbound/user 三个维度，没有域名维度，所以要
// 拿到按域名的字节数，唯一的办法就是让每个域名拥有自己的出站 tag。
const MeterOutboundTagPrefix = "a-ui-meter-"

// MeterTag 拼出 (入站, 注册域名) 对应的计量出站 tag，形如
// a-ui-meter-3-doubleclick.net。
//
// tag 里直接带域名而不是「槽位序号 + 映射表」，是为了消掉槽位复用的归因
// 错乱：槽位从域名 A 换成 B 时，那一轮采集里属于 A 的残余流量会被算到 B
// 头上，而且没有任何一层会报错。tag 带域名则换池就是换计数器。
func MeterTag(inboundId int, domain string) string {
	return MeterOutboundTagPrefix + strconv.Itoa(inboundId) + "-" + domain
}

// ParseMeterTag 从计量出站 tag 反查出 (入站 id, 注册域名)。
//
// 按**第一个**短横线切开：inboundId 是十进制数字不含短横线，其后全部是
// 域名。域名本身可以含短横线（some-cdn.example.com），所以绝不能从右边切。
//
// 拒绝一切形态不对的输入而不是尽力猜：采集路径上一个猜错的 tag 会把字节
// 静默记到别的域名头上，而榜单会渲染得完全正常。
func ParseMeterTag(tag string) (int, string, bool) {
	rest, ok := strings.CutPrefix(tag, MeterOutboundTagPrefix)
	if !ok {
		return 0, "", false
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

// IsMeterTag 判断一个 tag 是否由计量子系统发出。
//
// 判的是**前缀**而不是「能否成功反查」：分配端要拒绝的是整个前缀命名空间，
// 一个形态不对但带着前缀的 tag 同样不能分配给出站节点。
func IsMeterTag(tag string) bool {
	return strings.HasPrefix(tag, MeterOutboundTagPrefix)
}
```

- [ ] **Step 4: 扩展 `IsReservedTag`**

`database/model/routing.go`，把函数体改成（注释也要同步，只改值不改注释会留下一条与代码相反的说明）：

```go
// IsReservedTag 判定一个 tag 是否由注入器自己发出，因而不能分配给出站节点。
//
// 保留 tag 不在 outbound_nodes 表里，数据库的唯一约束管不到它们：备注写成
// 「block」（含 Block/BLOCK/block!/" block "，SlugRemark 会把它们归一到同一个
// slug）会让 SuggestTag 生成 a-ui-block，与注入器始终注入的黑洞出站撞名，
// xray 报 "existing tag found" 并拒绝启动——全员断网，而面板首页仍显示 running。
// 计量出站（a-ui-meter-*）同理：备注写成「meter-3-x.com」就可能撞上。
//
// 四个消费点都只认这一个判定，将来新增保留 tag 只需改这里：
// 分配端 OutboundNodeService.allocTag（不分配出去）、
// 生成端 RoutingInjector.buildOutbounds（修复前的脏数据不写进配置）、
// 校验端 removeOutboundByTag（校验时绝不把注入器的黑洞出站当成旧版本摘掉）、
// 导入端 routing_portable.go（导入文件里的保留 tag 一律拒绝落库）。
func IsReservedTag(tag string) bool {
	return tag == BlockOutboundTag || tag == DefaultOutboundTag || IsMeterTag(tag)
}
```

**注意：`routing.go` 目前没有 import 块。**不要为了 `strings.HasPrefix` 去加一个——`IsMeterTag` 就在同一个包里，调用它即可，`routing.go` 保持零 import。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./database/model/ -v`
Expected: PASS，包括既有用例

- [ ] **Step 6: 写 `IsRegistrable` 的失败测试**

`util/domain/domain_test.go` 末尾追加：

```go
func TestIsRegistrable(t *testing.T) {
	// 计量池只能收真正的注册域名。放行公共后缀本身会生成 domain:com
	// 这种命中全部 .com 的规则，把该入站几乎全部流量吸进一个计量出站，
	// 榜单从此只有一行。
	yes := []string{
		"doubleclick.net",
		"example.co.uk",
		"9gag.com",
		"some-cdn.example.com.cn",
	}
	for _, d := range yes {
		if !IsRegistrable(d) {
			t.Errorf("IsRegistrable(%q) = false，期望 true", d)
		}
	}
	no := []string{
		"",                 // 空
		"com",              // 公共后缀本身
		"co.uk",            // 多级公共后缀本身
		"localhost",        // 不含点的主机名
		"www.example.com",  // 子域名不是注册域名，池里只放归并后的结果
		"1.2.3.4",          // IPv4 字面量：需要 ip 条件而不是 domain 条件
		"2001:db8::1",      // IPv6 字面量
		"example.com.",     // 带末尾点：Registrable 已经剥过，这里不再兼容
		"EXAMPLE.COM",      // 大写：Registrable 已经转过小写，这里不再兼容
	}
	for _, d := range no {
		if IsRegistrable(d) {
			t.Errorf("IsRegistrable(%q) = true，期望 false", d)
		}
	}
}
```

- [ ] **Step 7: 运行测试确认它失败**

Run: `go test ./util/domain/ -run IsRegistrable -v`
Expected: 编译失败，`undefined: IsRegistrable`

- [ ] **Step 8: 实现 `IsRegistrable`**

`util/domain/domain.go` 末尾追加：

```go
// IsRegistrable 判断 d 本身是不是一个注册域名（eTLD+1）。
//
// Registrable 对三类值是「原样返回、不丢弃」——IP 字面量、公共后缀本身
// （"com"）、不含点的主机名（"localhost"）——因为访问次数照样要统计。但第二期
// 的计量池必须把它们挡在外面：计量规则写的是 domain:<值>，而 domain:com 会
// 命中全部 .com，把该入站几乎全部流量吸进一个计量出站；IP 字面量则需要 ip
// 条件，domain 条件对它永不命中，白占一个池槽位。
//
// 判据是「EffectiveTLDPlusOne 成功且返回值等于输入」——只接受已经归并好的
// 结果，不替调用方做归一化（转小写、剥末尾点在 Registrable 里已经做过）。
func IsRegistrable(d string) bool {
	if d == "" || net.ParseIP(d) != nil {
		return false
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(d)
	return err == nil && etld1 == d
}
```

- [ ] **Step 9: 运行测试确认通过**

Run: `go test ./util/domain/ ./database/model/ -v`
Expected: PASS

- [ ] **Step 10: 确认 `allocTag` 与导入路径自动继承了新前缀**

这两条路径本来就只调 `model.IsReservedTag`，不需要改代码，但要有测试钉住。
`web/service/routing_outbound_test.go` 末尾追加：

```go
func TestAllocTagNeverReturnsMeterTag(t *testing.T) {
	setupDB(t)
	// 备注被 SlugRemark 归一成 meter-3-x.com 之后，SuggestTag 会拼出
	// a-ui-meter-3-x.com，与计量出站撞名；xray 会报 existing tag found
	// 并拒绝启动整份配置——全员断网，而面板首页仍显示 running。
	node, err := (&OutboundNodeService{}).AddFromLink("socks5://1.2.3.4:1080", "meter-3-x.com")
	if err != nil {
		t.Fatalf("AddFromLink: %v", err)
	}
	if model.IsReservedTag(node.Tag) {
		t.Fatalf("分配出的 tag = %q，撞上了保留 tag 命名空间", node.Tag)
	}
}
```

需要在该文件的 import 里补 `"a-ui/database/model"`（若尚未导入）。

- [ ] **Step 11: 跑全量门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 12: 提交**

```bash
git status --porcelain
git add database/model/meter.go database/model/meter_test.go database/model/routing.go \
        util/domain/domain.go util/domain/domain_test.go web/service/routing_outbound_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 计量出站 tag 的编解码与保留 tag 扩展

计量出站 tag 形如 a-ui-meter-<inboundId>-<domain>，反查按第一个短横线切开
（域名可以含短横线，不能从右边切）。IsReservedTag 追加对整个前缀命名空间的
拒绝，分配端/生成端/校验端/导入端四处自动继承。

IsRegistrable 把公共后缀本身（com）、IP 字面量、子域名挡在计量池外面：
domain:com 会命中全部 .com，把该入站几乎全部流量吸进一个计量出站。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 2: 计量池的表结构、读取与级联清理

**Files:**
- Modify: `database/model/meter.go`（追加 `MeterDomain` 结构体）
- Modify: `database/db.go`（`InitTrafficDB` 里 `AutoMigrate`）
- Create: `web/service/meter_pool.go`
- Modify: `web/service/inbound.go`（`DelInbound` 级联）
- Modify: `web/job/traffic_cleanup_job.go`（孤儿清理）
- Test: `web/service/meter_pool_test.go`（新建）

**Interfaces:**
- Consumes: `model.MeterTag` / `model.ParseMeterTag`（Task 1）
- Produces:
  - `model.MeterDomain`（表 `meter_domains`）
  - `service.MeterEntry{InboundId int; Domain string}`
  - `(*service.MeterPoolService).Pool(now time.Time) ([]MeterEntry, error)` —— 按 `(inboundId asc, domain asc)` 排序，只含未在冷却期的行
  - `(*service.MeterPoolService).DeleteByInbound(inboundId int) error`
  - `(*service.MeterPoolService).PruneOrphans() (int64, error)`

- [ ] **Step 1: 写失败的测试**

新建 `web/service/meter_pool_test.go`：

```go
package service

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// setupMeterPoolTest 建主库与用量库。计量池落在用量库，与 DomainStat 同库。
func setupMeterPoolTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	// 用量库句柄是包级变量，会跨用例残留——而且 SQLite 在文件被 t.TempDir
	// 清掉之后仍能通过已打开的 fd 读到旧数据。计量池就在这个库里，不清空的话
	// 本用例写进池的行会漏进 routing_inject_test.go 的用例，让 Inject 生成出
	// 无从解释的计量出站，把既有断言打成随机失败。
	t.Cleanup(database.ResetTrafficDBForTest)
}

// putPoolRow 直接写一行池记录，绕过重算逻辑，专测读取与清理。
func putPoolRow(t *testing.T, inboundId int, dom string, cooldownUntil int64) {
	t.Helper()
	row := &model.MeterDomain{
		InboundId: inboundId, Domain: dom,
		EnteredAt: time.Now().Unix(), CooldownUntil: cooldownUntil,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入池行: %v", err)
	}
}

func TestPoolIsSortedAndSkipsCooldown(t *testing.T) {
	setupMeterPoolTest(t)
	now := time.Unix(1_800_000_000, 0)
	// 故意乱序写入：Pool 必须自己排序，生成期靠它保证配置逐字节确定。
	putPoolRow(t, 7, "zeta.com", 0)
	putPoolRow(t, 3, "beta.com", 0)
	putPoolRow(t, 3, "alpha.com", 0)
	// 冷却中的行不参与生成：它已经退池，只是留着记冷却时刻。
	putPoolRow(t, 3, "cooling.com", now.Add(time.Hour).Unix())

	got, err := (&MeterPoolService{}).Pool(now)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	want := []MeterEntry{
		{InboundId: 3, Domain: "alpha.com"},
		{InboundId: 3, Domain: "beta.com"},
		{InboundId: 7, Domain: "zeta.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("Pool 返回 %d 行，期望 %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行 = %+v，期望 %+v", i, got[i], want[i])
		}
	}
}

func TestPoolReturnsEmptyWhenTrafficDBMissing(t *testing.T) {
	setupMeterPoolTest(t)
	putPoolRow(t, 3, "alpha.com", 0)
	database.ResetTrafficDBForTest()
	// 用量库打不开时整个计量功能自动停用，绝不让配置生成失败。
	got, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pool = %+v，期望空", got)
	}
}

func TestDeleteByInboundRemovesOnlyThatInbound(t *testing.T) {
	setupMeterPoolTest(t)
	putPoolRow(t, 3, "alpha.com", 0)
	putPoolRow(t, 7, "zeta.com", 0)
	if err := (&MeterPoolService{}).DeleteByInbound(3); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	got, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(got) != 1 || got[0].InboundId != 7 {
		t.Errorf("Pool = %+v，期望只剩入站 7 的那行", got)
	}
}

func TestPruneOrphansDropsRowsOfDeletedInbounds(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31501, "甲")
	putPoolRow(t, in.Id, "alpha.com", 0)
	putPoolRow(t, in.Id+999, "orphan.com", 0) // 这个入站不存在

	// SQLite 会复用被删除的自增 id：残留行会绑到下一个建出来的入站上，
	// 于是面板会为一个全新用户生成一批别人的域名的计量出站与规则，
	// 而引用不再悬空，生成期没有任何一道防线拦得住。
	pruned, err := (&MeterPoolService{}).PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("清理了 %d 行，期望 1", pruned)
	}
	got, _ := (&MeterPoolService{}).Pool(time.Now())
	if len(got) != 1 || got[0].InboundId != in.Id {
		t.Errorf("Pool = %+v，期望只剩存在的那个入站", got)
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'Pool|DeleteByInbound|PruneOrphansDrops' -v`
Expected: 编译失败，`undefined: MeterPoolService`、`undefined: MeterEntry`、`model.MeterDomain` 未定义

- [ ] **Step 3: 追加 `MeterDomain` 表结构**

`database/model/meter.go` 末尾追加：

```go
// MeterDomain 是「当前正在被计量」的 (入站, 注册域名) 对，落在用量库
// （见 database.InitTrafficDB），与 DomainStat 同库。
//
// 分库理由与 DomainStat / TrafficBucket 相同：高频写入不该和面板的普通操作
// 抢主库那把 SQLite 写锁；而且这张表的孤儿清理挂在同一个每小时任务里。
//
// 池必须落库，不能在 RoutingInjector.Inject 里现算：Inject 由那个 10 秒的
// 重启消费任务反复调用，现算意味着排名一变配置字节就变，Config.Equals 恒
// 判不等，cron 会不停热应用甚至重启 xray。
//
// 没有任何 json tag：这张表是纯服务端状态，既不下发给前端也不接受前端提交
//（与 model.Inbound 的 LastResetAt / DisabledByTraffic 同一条理由）。
//
// 相应地，删除入站时必须连带删掉它的行——SQLite 会复用被删除的自增 id，
// 不删的话下一个建出来的入站会拿到上一个用户的计量域名，生成出一批指向
// 别人域名的计量出站与规则，而引用不再悬空，跳过式的防线拦不住。
type MeterDomain struct {
	Id int64 `gorm:"primaryKey;autoIncrement"`

	InboundId int    `gorm:"uniqueIndex:idx_meter_domain,priority:1"`
	Domain    string `gorm:"uniqueIndex:idx_meter_domain,priority:2"`

	// EnteredAt 是进池时刻的 Unix 秒，供「最小驻留」闸门使用：刚进池的域名
	// 还没来得及产生字节，它的权重必然是 0，不保护就会被自己的 0 权重挤出去。
	EnteredAt int64

	// ProbeZeroRounds 是进池后连续几轮实测字节为 0。
	//
	// 零字节的域名不会在 DomainStat 里留下行（零增量不写行），所以「在池但
	// 实测为 0」这个状态无法从 DomainStat 反推，必须由池表自己记账——这是
	// 这个字段存在的唯一理由。
	ProbeZeroRounds int

	// CooldownUntil 是冷却截止时刻的 Unix 秒，0 表示不在冷却。
	//
	// 它同时是「这一行是否在池内」的判据：> now 表示已退场、正在冷却，
	// 不生成出站与规则，也不参与本轮候选。行不能直接删掉，否则冷却状态
	// 就没地方存，一个只有连接数没有流量的域名会每轮重新入选、每轮退场。
	CooldownUntil int64
}
```

- [ ] **Step 4: 在用量库里建表**

`database/db.go` 的 `InitTrafficDB`，在 `DomainStatCursor` 那段之后追加：

```go
	// 计量池与域名统计同库：重算时要读 DomainStat 的聚合结果，生成期要读
	// 池，两者始终一起用；孤儿清理也挂在同一个每小时任务里。
	if err := tdb.AutoMigrate(&model.MeterDomain{}); err != nil {
		return err
	}
```

- [ ] **Step 5: 实现 `web/service/meter_pool.go` 的读取与清理部分**

新建 `web/service/meter_pool.go`：

```go
package service

import (
	"sort"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// MeterPoolService 维护计量池：哪些 (入站, 注册域名) 对当前正在被计量。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type MeterPoolService struct {
	settingService SettingService
}

// MeterEntry 是池里的一行，只含生成配置需要的两个字段。
type MeterEntry struct {
	InboundId int
	Domain    string
}

// Pool 返回当前在池的全部条目，按 (inboundId asc, domain asc) 排序。
//
// 排序不是可省的整洁工作：生成期用它产出计量出站与计量规则，而
// Config.Equals 对 OutboundConfigs / RouterConfig 按字节比较——顺序一抖动
// 就恒判不等，那个 10 秒的 cron 会不停重启 xray。
//
// 冷却中的行（CooldownUntil > now）不返回：它们已经退场，留在表里只是为了
// 记住冷却截止时刻。
//
// 用量库不可用时返回空切片而不是报错：整个计量功能自动停用，配置照常生成。
func (s *MeterPoolService) Pool(now time.Time) ([]MeterEntry, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return nil, nil
	}
	var rows []model.MeterDomain
	err := db.Where("cooldown_until <= ?", now.Unix()).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]MeterEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, MeterEntry{InboundId: r.InboundId, Domain: r.Domain})
	}
	// 在 Go 侧排序而不是交给 SQL 的 ORDER BY：排序键是生成期的不变量，
	// 让它和读取方式解耦，将来换查询条件也不会悄悄丢掉顺序。
	sort.Slice(out, func(i, j int) bool {
		if out[i].InboundId != out[j].InboundId {
			return out[i].InboundId < out[j].InboundId
		}
		return out[i].Domain < out[j].Domain
	})
	return out, nil
}

// DeleteByInbound 删除某入站的全部池行。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *MeterPoolService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.MeterDomain{}).Error
}

// PruneOrphans 删除已不存在的入站遗留的池行，返回删除行数。
//
// 第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，那时
// 引用不再悬空，面板会为一个全新用户生成一批指向别人域名的计量出站与规则，
// 而配置与界面都渲染得完全正常。
func (s *MeterPoolService) PruneOrphans() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.Inbound{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	tx := db.Where("inbound_id != 0")
	if len(ids) > 0 {
		tx = tx.Where("inbound_id not in ?", ids)
	}
	result := tx.Delete(&model.MeterDomain{})
	return result.RowsAffected, result.Error
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./web/service/ -run 'Pool|DeleteByInbound|PruneOrphansDrops' -v`
Expected: PASS

- [ ] **Step 7: 接上 `DelInbound` 的级联删除**

`web/service/inbound.go` 的 `DelInbound`，在 `DomainStatService` 那段之后追加：

```go
	// 计量池同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来的
	// 入站会继承上一个用户的计量域名，生成出一批指向别人域名的计量出站与
	// 规则。失败只告警不阻断，理由同上，残留由每小时一次的 PruneOrphans 兜底。
	if err := (&MeterPoolService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的计量池失败, 将由定时清理兜底, id:", id, "err:", err)
	}
```

- [ ] **Step 8: 接上 `TrafficCleanupJob` 的孤儿清理**

`web/job/traffic_cleanup_job.go`：结构体加字段 `meterPoolService service.MeterPoolService`，
并在 `domainStatService.PruneOrphans()` 那段之后追加：

```go
	// 计量池与域名统计同库，孤儿清理挂在同一个任务里，理由同上。
	if pruned, err := j.meterPoolService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿计量池记录失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的计量池记录", pruned)
	}
```

- [ ] **Step 9: 写「删除入站会连带清池」的回归测试**

`web/service/meter_pool_test.go` 末尾追加：

```go
func TestDelInboundClearsMeterPool(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31502, "甲")
	putPoolRow(t, in.Id, "alpha.com", 0)

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	got, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pool = %+v，期望空——删除入站必须连带清掉它的池行", got)
	}
}
```

- [ ] **Step 10: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 11: 提交**

```bash
git status --porcelain
git add database/model/meter.go database/db.go web/service/meter_pool.go \
        web/service/meter_pool_test.go web/service/inbound.go web/job/traffic_cleanup_job.go
git commit -m "$(cat <<'EOF'
feat(meter): 计量池表结构、读取与两道清理防线

MeterDomain 落在用量库，与 DomainStat 同库。CooldownUntil 同时是「这一行是否
在池内」的判据：退场的行不能直接删，否则冷却状态没地方存，一个只有连接数没有
流量的域名会每轮重新入选、每轮退场。

Pool 按 (inboundId, domain) 排序——生成期靠它保证配置逐字节确定，顺序一抖动
Config.Equals 就恒判不等，10 秒的 cron 会不停重启 xray。

DelInbound 级联 + PruneOrphans 兜底两道都要有：SQLite 复用自增 id，残留行会
绑到下一个建出来的入站上，那时引用不再悬空，跳过式的防线拦不住。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---
## Task 3: 计量池的排序与三道闸门

**Files:**
- Modify: `web/service/meter_pool.go`（追加常量、纯函数与 `Recompute`）
- Test: `web/service/meter_pool_rank_test.go`（新建，测纯函数）
- Test: `web/service/meter_pool_test.go`（追加，测 `Recompute` 的落库行为）

**Interfaces:**
- Consumes: `domain.IsRegistrable`（Task 1）、`model.MeterDomain`（Task 2）、`model.GranularityHour`、`model.AlignHour`
- Produces:
  - `(*service.MeterPoolService).Recompute(now time.Time) (changed int, err error)`
  - `service.StaleMeterCounters() int64` / `service.SetStaleMeterCounters(n int64)` —— 死计数器观测值的读写，Task 8 负责写
  - 包内纯函数：`meterPoolCapacity(enabled int) int`、`meterAvgBytesPerConn(aggs []meterAgg) float64`、`buildMeterCandidates(...) []meterCandidate`、`pickPool(cands []meterCandidate, k int) []string`

**设计要点（实现前必读，spec §6.2）：**

三道闸门用**一个排序表达式**同时表达，不写成三段互相纠缠的分支：

| 闸门 | 表达方式 |
|---|---|
| 替换余量 | 在池的域名权重乘 `meterSwapMargin`（1.25）。新候选要挤掉在位者，就必须超出它 1.25 倍——这与「候选权重 ≥ 在位末位 × 1.25」完全等价，但不需要维护「谁是末位」。 |
| 最小驻留 | `MustKeep` 标记，在挑选时先占位。刚进池的域名还没产生字节，权重必然是 0，不保护就会被自己的 0 权重挤出去。 |
| 试用退场 | 连续 `meterProbeGiveUpRounds` 轮实测字节为 0 的域名**直接不进候选列表**，并写 24 小时冷却。 |

权重本身要解决另一个问题：**只按实测字节排会让池在第二天就冻死**——没进过池的域名字节恒为 0，永远排在所有已计量域名之后，永远进不了池。所以未计量域名按 `count × 平均每连接字节` 折算。这个折算值**只用于选谁进池，不写库、不出现在任何接口返回体里**——§1 非目标里「估算等于用假数据覆盖真数据」约束的是展示，这里是选择。

- [ ] **Step 1: 写纯函数的失败测试**

新建 `web/service/meter_pool_rank_test.go`：

```go
package service

import (
	"reflect"
	"testing"
	"time"

	"a-ui/database/model"
)

func TestMeterPoolCapacity(t *testing.T) {
	// K = min(meterPoolCapMax, meterOutboundBudget / N)。上限来自实测：
	// 每个计量出站给真实 xray 校验多加约 1.03 ms，200 个约 0.21 秒。
	cases := []struct{ enabled, want int }{
		{0, 60},   // 没有启用入站：按 1 算，不做特殊分支
		{1, 60},
		{2, 60},   // 200/2 = 100 > 60，被 capMax 压住
		{4, 50},   // 200/4 = 50
		{10, 20},
		{200, 1},
		{201, 0},  // 预算被摊薄到 0：整个计量功能自动停用
	}
	for _, c := range cases {
		if got := meterPoolCapacity(c.enabled); got != c.want {
			t.Errorf("meterPoolCapacity(%d) = %d，期望 %d", c.enabled, got, c.want)
		}
	}
}

func TestMeterAvgBytesPerConn(t *testing.T) {
	if got := meterAvgBytesPerConn(nil); got != 0 {
		t.Errorf("空输入 = %v，期望 0", got)
	}
	// 只有连接数没有字节（刚上线第一小时）时也必须是 0，
	// 这样权重退化成 0，排序键自然落到第二项 count desc。
	if got := meterAvgBytesPerConn([]meterAgg{{Domain: "a.com", Count: 10}}); got != 0 {
		t.Errorf("零字节 = %v，期望 0", got)
	}
	aggs := []meterAgg{
		{Domain: "a.com", Bytes: 900, Count: 3},
		{Domain: "b.com", Bytes: 100, Count: 2},
	}
	if got := meterAvgBytesPerConn(aggs); got != 200 {
		t.Errorf("平均 = %v，期望 200（1000 字节 / 5 次连接）", got)
	}
}

func TestBuildMeterCandidatesFiltersNonRegistrable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	aggs := []meterAgg{
		{Domain: "doubleclick.net", Bytes: 100, Count: 1},
		{Domain: "com", Bytes: 999, Count: 9},         // 公共后缀本身：domain:com 会吸走全部 .com
		{Domain: "1.2.3.4", Bytes: 999, Count: 9},     // IP 字面量：domain 条件对它永不命中
		{Domain: "www.example.com", Bytes: 999, Count: 9}, // 子域名：池里只放归并后的注册域名
	}
	cands := buildMeterCandidates(aggs, nil, nil, nil, now)
	if len(cands) != 1 || cands[0].Domain != "doubleclick.net" {
		t.Fatalf("候选 = %+v，期望只剩 doubleclick.net", cands)
	}
}

func TestBuildMeterCandidatesEstimatesUnmeteredDomains(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// 已计量域名 a：1000 字节 / 5 次连接 → 平均 200 字节/连接。
	// 未计量域名 b：只有 4 次连接，折算权重 = 4 × 200 = 800。
	aggs := []meterAgg{
		{Domain: "a.com", Bytes: 1000, Count: 5},
		{Domain: "b.com", Bytes: 0, Count: 4},
	}
	cands := buildMeterCandidates(aggs, nil, nil, nil, now)
	byDom := map[string]meterCandidate{}
	for _, c := range cands {
		byDom[c.Domain] = c
	}
	if byDom["a.com"].Weight != 1000 {
		t.Errorf("a.com 权重 = %v，期望 1000（实测字节直接用）", byDom["a.com"].Weight)
	}
	if byDom["b.com"].Weight != 800 {
		t.Errorf("b.com 权重 = %v，期望 800（4 次 × 平均 200 字节）——"+
			"未计量域名不折算的话字节恒为 0，永远进不了池，池会锁死在上线首日那批",
			byDom["b.com"].Weight)
	}
	// 排序：权重高的在前
	if cands[0].Domain != "a.com" {
		t.Errorf("首位 = %q，期望 a.com", cands[0].Domain)
	}
}

func TestBuildMeterCandidatesGivesIncumbentsTheSwapMargin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-10 * time.Hour).Unix() // 早已过最小驻留期
	inPool := map[string]*model.MeterDomain{
		"a.com": {Domain: "a.com", EnteredAt: old},
	}
	aggs := []meterAgg{
		{Domain: "a.com", Bytes: 100, Count: 1},
		{Domain: "b.com", Bytes: 100, Count: 1},
	}
	cands := buildMeterCandidates(aggs, inPool, nil, nil, now)
	byDom := map[string]meterCandidate{}
	for _, c := range cands {
		byDom[c.Domain] = c
	}
	// 在位加成就是「替换余量」：权重相同的挑战者挤不掉在位者，
	// 否则两个权重相近的域名会每轮互换，出站被反复热增删。
	if byDom["a.com"].Weight != 125 {
		t.Errorf("在位者权重 = %v，期望 125（100 × 1.25）", byDom["a.com"].Weight)
	}
	if byDom["b.com"].Weight != 100 {
		t.Errorf("挑战者权重 = %v，期望 100（无加成）", byDom["b.com"].Weight)
	}
	if cands[0].Domain != "a.com" {
		t.Errorf("首位 = %q，期望在位者 a.com 排前面", cands[0].Domain)
	}
}

func TestBuildMeterCandidatesKeepsPoolDomainsWithNoRows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	inPool := map[string]*model.MeterDomain{
		// 刚进池，窗口内一条 DomainStat 行都没有（零增量不写行）
		"fresh.com": {Domain: "fresh.com", EnteredAt: now.Unix()},
	}
	cands := buildMeterCandidates(nil, inPool, nil, nil, now)
	if len(cands) != 1 || cands[0].Domain != "fresh.com" {
		t.Fatalf("候选 = %+v，期望包含池里那个没有任何行的域名——"+
			"漏掉它会让它绕过最小驻留闸门被静默挤出去", cands)
	}
	if !cands[0].MustKeep {
		t.Error("MustKeep = false，期望 true：刚进池不足两轮的域名必须强制保留")
	}
}

func TestBuildMeterCandidatesExcludesCoolingAndRetired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	aggs := []meterAgg{
		{Domain: "cool.com", Bytes: 999, Count: 9},
		{Domain: "gone.com", Bytes: 999, Count: 9},
		{Domain: "ok.com", Bytes: 1, Count: 1},
	}
	inPool := map[string]*model.MeterDomain{
		"gone.com": {Domain: "gone.com", EnteredAt: now.Add(-10 * time.Hour).Unix()},
	}
	cands := buildMeterCandidates(aggs, inPool,
		map[string]bool{"cool.com": true}, map[string]bool{"gone.com": true}, now)
	if len(cands) != 1 || cands[0].Domain != "ok.com" {
		t.Fatalf("候选 = %+v，期望只剩 ok.com", cands)
	}
}

func TestPickPoolHonoursMinHoldThenWeight(t *testing.T) {
	// 容量 1，权重最高的是 hot.com，但 fresh.com 处于最小驻留期。
	cands := []meterCandidate{
		{Domain: "hot.com", Weight: 10000, Count: 1},
		{Domain: "fresh.com", Weight: 0, Count: 0, MustKeep: true},
	}
	got := pickPool(cands, 1)
	if !reflect.DeepEqual(got, []string{"fresh.com"}) {
		t.Errorf("pickPool = %v，期望 [fresh.com]——刚进池的域名权重必然是 0，"+
			"不保护就会被自己的 0 权重挤出去，永远拿不到实测数据", got)
	}
}

func TestPickPoolFillsByWeightAndReturnsSorted(t *testing.T) {
	cands := []meterCandidate{
		{Domain: "z.com", Weight: 300},
		{Domain: "a.com", Weight: 200},
		{Domain: "m.com", Weight: 100},
	}
	got := pickPool(cands, 2)
	// 选中的是权重前二（z/a），但返回必须按域名字典序——生成期靠它保证
	// 配置逐字节确定。
	if !reflect.DeepEqual(got, []string{"a.com", "z.com"}) {
		t.Errorf("pickPool = %v，期望 [a.com z.com]", got)
	}
}

func TestPickPoolCapacityZeroSelectsNothing(t *testing.T) {
	cands := []meterCandidate{{Domain: "a.com", Weight: 1, MustKeep: true}}
	if got := pickPool(cands, 0); len(got) != 0 {
		t.Errorf("pickPool(k=0) = %v，期望空——预算摊薄到 0 时整个功能停用", got)
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'MeterPoolCapacity|MeterAvg|BuildMeterCandidates|PickPool' -v`
Expected: 编译失败，`undefined: meterPoolCapacity` 等

- [ ] **Step 3: 实现常量、观测值与纯函数**

`web/service/meter_pool.go` 的 import 补上 `"sync/atomic"`、`"a-ui/logger"`、`"a-ui/util/domain"`、`"gorm.io/gorm"`，并在 `MeterPoolService` 定义之前追加：

```go
const (
	// meterOutboundBudget 是全部入站共用的计量出站总预算。
	//
	// 上限来自实测（spec §6.0）：每个计量出站给配置解析加约 1.03 ms、给配置
	// 加约 235 B。这条代价的落点不是 xray 启动（多花 0.2 秒无人察觉），而是
	// routing_validate.go 那套**同步 HTTP 请求里的真实 xray 校验**——新建/编辑
	// 出站节点、新建/编辑入站、保存设置各会 exec 一次 run -test，200 个计量
	// 出站意味着这些操作各多花约 0.21 秒。
	meterOutboundBudget = 200

	// meterPoolCapMax 是单个入站的池容量上限。单入站场景不必把预算吃满：
	// 60 个注册域名已能覆盖绝大多数用户流量的主体。
	meterPoolCapMax = 60

	// meterPoolRound 是「一轮」的长度，必须与 MeterPoolJob 的周期一致。
	meterPoolRound = time.Hour

	// meterPoolWindow 是排序时回看多久的数据。
	meterPoolWindow = 24 * time.Hour

	// meterMinHoldRounds 是最小驻留轮数：刚进池的域名还没来得及产生字节，
	// 权重必然是 0，不保护就会被自己的 0 权重挤出去，永远拿不到实测数据。
	meterMinHoldRounds = 2

	// meterProbeGiveUpRounds 是连续几轮实测字节为 0 就退场。挡的是只有连接数、
	// 没有流量的域名（探测器、失败重连，以及已经被管理员规则分流走的域名）
	// 长期占着槽位。
	meterProbeGiveUpRounds = 3

	// meterCooldown 是退场后多久不再参选。
	meterCooldown = 24 * time.Hour

	// meterSwapMargin 是在位加成，也就是「替换余量」：新候选要挤掉在位者，
	// 权重必须超出它这么多倍。没有它的话，两个权重相近的域名会每轮互换，
	// 出站被反复热增删，而每一次退池都会在核心里留下一对永不回收的计数器。
	meterSwapMargin = 1.25

	// meterStaleCounterLimit 是观测到的死计数器上限，超过即冻结换池。
	//
	// xray 的 RemoveHandler（app/proxyman/outbound/outbound.go:131）只删 handler
	// 不注销 stats 计数器，StatsService 也没有任何注销 RPC，所以每个退过池的
	// tag 会在核心里留到进程退出为止，并且每次 QueryStats 都被返回一遍。
	// 冻结只停止吸纳新域名，已在池内的域名照常计量；xray 任何一次整进程重启
	// 都会清空计数器，观测值归零，冻结自动解除。
	meterStaleCounterLimit = 2000
)

// meterStaleCounters 是上一次采集观测到的「已不在池内的计量计数器」条数。
//
// 观测式而不是记账式：它天然跨 xray 重启自愈，不需要面板去跟踪「上一次重启
// 是什么时候」这种它其实拿不准的状态。写入方是 DomainStatService.RecordMetered
// （每 10 秒一次），读取方是 Recompute（每小时一次）。
var meterStaleCounters atomic.Int64

// StaleMeterCounters 返回上一次采集观测到的死计数器条数。
func StaleMeterCounters() int64 { return meterStaleCounters.Load() }

// SetStaleMeterCounters 由采集路径写入。导出是为了让测试能直接构造这个状态。
func SetStaleMeterCounters(n int64) { meterStaleCounters.Store(n) }

// meterAgg 是某入站在窗口内、某个注册域名的聚合结果。
type meterAgg struct {
	Domain string
	Bytes  int64
	Count  int64
}

// meterCandidate 是排序用的候选。Weight 已经乘过在位加成。
type meterCandidate struct {
	Domain   string
	Count    int64
	Weight   float64
	MustKeep bool
}

// meterPoolCapacity 算出单个入站的池容量。
func meterPoolCapacity(enabled int) int {
	if enabled < 1 {
		enabled = 1
	}
	k := meterOutboundBudget / enabled
	if k > meterPoolCapMax {
		k = meterPoolCapMax
	}
	return k
}

// meterAvgBytesPerConn 算出「每次连接平均多少字节」，用来把未计量域名的
// 连接次数折算成与实测字节可比的权重。
//
// 没有任何字节数据时返回 0——此时所有权重都是 0，排序键自然落到第二项
// count desc，也就是「冷启动按访问次数选池」，不需要单独的代码分支。
func meterAvgBytesPerConn(aggs []meterAgg) float64 {
	var bytes, count int64
	for _, a := range aggs {
		bytes += a.Bytes
		count += a.Count
	}
	if count <= 0 {
		return 0
	}
	return float64(bytes) / float64(count)
}

// buildMeterCandidates 把聚合结果与当前池状态合成一份排好序的候选列表。
//
// 候选集是「窗口内有数据的域名」∪「当前在池的域名」——后者不能漏：零增量
// 不写行，一个刚进池、窗口内一条 DomainStat 行都没有的域名如果不进候选，
// 就会绕过最小驻留闸门被静默挤出去。
//
// 排序键是 (Weight desc, Count desc, Domain asc)。末位用域名字典序兜底是硬
// 要求：生成期靠这个顺序保证配置逐字节确定。
func buildMeterCandidates(
	aggs []meterAgg,
	inPool map[string]*model.MeterDomain,
	cooling map[string]bool,
	retired map[string]bool,
	now time.Time,
) []meterCandidate {
	avg := meterAvgBytesPerConn(aggs)

	byDomain := make(map[string]meterAgg, len(aggs))
	for _, a := range aggs {
		byDomain[a.Domain] = a
	}
	names := make([]string, 0, len(aggs)+len(inPool))
	for _, a := range aggs {
		names = append(names, a.Domain)
	}
	for d := range inPool {
		if _, ok := byDomain[d]; !ok {
			names = append(names, d)
		}
	}
	// 先定序再构造，SliceStable 的结果才是确定的——map 的遍历顺序是随机的。
	sort.Strings(names)

	cands := make([]meterCandidate, 0, len(names))
	for _, d := range names {
		// 只收真正的注册域名：domain:com 会命中全部 .com，把该入站几乎全部
		// 流量吸进一个计量出站，榜单从此只有一行；IP 字面量则需要 ip 条件，
		// domain 条件对它永不命中，白占一个槽位。
		if !domain.IsRegistrable(d) {
			continue
		}
		if cooling[d] || retired[d] {
			continue
		}
		a := byDomain[d]
		w := float64(a.Bytes)
		if a.Bytes == 0 {
			// 只按实测字节排会让池在第二天就冻死：没进过池的域名字节恒为 0，
			// 永远排在所有已计量域名之后。折算值只用于选谁进池，不写库、
			// 不出现在任何接口返回体里。
			w = float64(a.Count) * avg
		}
		row, pooled := inPool[d]
		mustKeep := false
		if pooled {
			w *= meterSwapMargin
			mustKeep = now.Sub(time.Unix(row.EnteredAt, 0)) < meterMinHoldRounds*meterPoolRound
		}
		cands = append(cands, meterCandidate{Domain: d, Count: a.Count, Weight: w, MustKeep: mustKeep})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Weight != cands[j].Weight {
			return cands[i].Weight > cands[j].Weight
		}
		if cands[i].Count != cands[j].Count {
			return cands[i].Count > cands[j].Count
		}
		return cands[i].Domain < cands[j].Domain
	})
	return cands
}

// pickPool 从排好序的候选里挑出容量为 k 的新池，返回按域名字典序排列的结果。
//
// 两轮：先让处于最小驻留期的占位，再按排序键补足。返回值排序是硬要求，
// 生成期靠它保证配置逐字节确定。
func pickPool(cands []meterCandidate, k int) []string {
	result := make([]string, 0, k)
	seen := make(map[string]bool, k)
	for _, c := range cands {
		if len(result) >= k {
			break
		}
		if !c.MustKeep {
			continue
		}
		result = append(result, c.Domain)
		seen[c.Domain] = true
	}
	for _, c := range cands {
		if len(result) >= k {
			break
		}
		if seen[c.Domain] {
			continue
		}
		result = append(result, c.Domain)
		seen[c.Domain] = true
	}
	sort.Strings(result)
	return result
}
```

- [ ] **Step 4: 运行纯函数测试确认通过**

Run: `go test ./web/service/ -run 'MeterPoolCapacity|MeterAvg|BuildMeterCandidates|PickPool' -v`
Expected: PASS

- [ ] **Step 5: 写 `Recompute` 的失败测试**

`web/service/meter_pool_test.go` 末尾追加：

```go
// putDomainStat 直接写一行小时桶，供池的重算读取。
func putDomainStat(t *testing.T, inboundId int, dom string, bucketStart, count, up, down int64) {
	t.Helper()
	row := &model.DomainStat{
		Granularity: model.GranularityHour, InboundId: inboundId,
		Domain: dom, BucketStart: bucketStart, Count: count, Up: up, Down: down,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入域名统计: %v", err)
	}
}

// poolRows 返回某入站的全部池行（含冷却中的），按域名排序。
func poolRows(t *testing.T, inboundId int) []model.MeterDomain {
	t.Helper()
	var rows []model.MeterDomain
	if err := database.GetTrafficDB().Where("inbound_id = ?", inboundId).
		Order("domain asc").Find(&rows).Error; err != nil {
		t.Fatalf("查询池行: %v", err)
	}
	return rows
}

func TestRecomputeColdStartRanksByCount(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31601, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	bucket := now.Add(-time.Hour).Unix()
	// 一个字节都还没有：排序退化成按访问次数。
	putDomainStat(t, in.Id, "a.com", bucket, 5, 0, 0)
	putDomainStat(t, in.Id, "b.com", bucket, 50, 0, 0)

	if _, err := (&MeterPoolService{}).Recompute(now); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	rows := poolRows(t, in.Id)
	if len(rows) != 2 {
		t.Fatalf("池里 %d 行，期望 2：%+v", len(rows), rows)
	}
	// K = min(60, 200/1) = 60，两个都进得去；这里断言的是它们确实都进了池，
	// 且 EnteredAt 被写上（最小驻留闸门要用）。
	for _, r := range rows {
		if r.EnteredAt != now.Unix() {
			t.Errorf("%s 的 EnteredAt = %d，期望 %d", r.Domain, r.EnteredAt, now.Unix())
		}
	}
}

func TestRecomputeIsIdempotent(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31602, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	putDomainStat(t, in.Id, "a.com", now.Add(-time.Hour).Unix(), 5, 1000, 2000)

	svc := &MeterPoolService{}
	if _, err := svc.Recompute(now); err != nil {
		t.Fatalf("首轮 Recompute: %v", err)
	}
	before := poolRows(t, in.Id)
	changed, err := svc.Recompute(now)
	if err != nil {
		t.Fatalf("次轮 Recompute: %v", err)
	}
	if changed != 0 {
		t.Errorf("次轮变动 %d 项，期望 0——数据没变就不该换池，"+
			"每次换池都会在核心里留下一对永不回收的计数器", changed)
	}
	after := poolRows(t, in.Id)
	if len(before) != len(after) || before[0].Domain != after[0].Domain ||
		before[0].EnteredAt != after[0].EnteredAt {
		t.Errorf("池发生了变化：%+v -> %+v", before, after)
	}
}

func TestRecomputeRetiresZeroByteDomainAndCoolsItDown(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31603, "甲")
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// 只有连接数、没有字节：典型的「已经被管理员规则分流走」或「探测器」。
	putDomainStat(t, in.Id, "zero.com", base.Add(-time.Hour).Unix(), 100, 0, 0)

	svc := &MeterPoolService{}
	// 第 0 轮是「进池」，那一轮不产生零轮计数；零轮计数从其后每一轮开始 +1，
	// 所以跑满 meterProbeGiveUpRounds(3) 需要 base+1h / +2h / +3h 三轮，
	// 退场发生在 base+3h —— 一共 4 轮。
	rounds := meterProbeGiveUpRounds + 1
	for i := 0; i < rounds; i++ {
		if _, err := svc.Recompute(base.Add(time.Duration(i) * time.Hour)); err != nil {
			t.Fatalf("第 %d 轮 Recompute: %v", i, err)
		}
	}
	rows := poolRows(t, in.Id)
	if len(rows) != 1 {
		t.Fatalf("池行 %d 条，期望 1（退场后仍保留行以记住冷却时刻）：%+v", len(rows), rows)
	}
	wantCooldown := base.Add(time.Duration(rounds-1) * time.Hour).Add(meterCooldown).Unix()
	if rows[0].CooldownUntil != wantCooldown {
		t.Errorf("CooldownUntil = %d，期望 %d", rows[0].CooldownUntil, wantCooldown)
	}
	// 冷却中的行不参与生成。
	entries, err := svc.Pool(base.Add(time.Duration(rounds) * time.Hour))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Pool = %+v，期望空——退场的域名不该继续生成出站与规则", entries)
	}
}

func TestRecomputeFreezesWhenStaleCountersExceedLimit(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31604, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	putDomainStat(t, in.Id, "a.com", now.Add(-time.Hour).Unix(), 5, 1000, 0)

	SetStaleMeterCounters(meterStaleCounterLimit + 1)
	t.Cleanup(func() { SetStaleMeterCounters(0) })

	changed, err := (&MeterPoolService{}).Recompute(now)
	if err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if changed != 0 {
		t.Errorf("变动 %d 项，期望 0——死计数器过多时必须冻结换池", changed)
	}
	if rows := poolRows(t, in.Id); len(rows) != 0 {
		t.Errorf("池行 = %+v，期望空", rows)
	}
}

func TestRecomputeIgnoresDisabledInbounds(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(0)
	in := mkTrafficInbound(t, 31605, "甲")
	in.Enable = false
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("停用入站: %v", err)
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	putDomainStat(t, in.Id, "a.com", now.Add(-time.Hour).Unix(), 5, 1000, 0)

	if _, err := (&MeterPoolService{}).Recompute(now); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if rows := poolRows(t, in.Id); len(rows) != 0 {
		t.Errorf("池行 = %+v，期望空——停用的入站不生成计量出站，也不该占预算", rows)
	}
}
```

- [ ] **Step 6: 运行测试确认它失败**

Run: `go test ./web/service/ -run Recompute -v`
Expected: 编译失败，`Recompute undefined`

- [ ] **Step 7: 实现 `Recompute`**

`web/service/meter_pool.go` 末尾追加：

```go
// Recompute 按小时重算全部启用入站的计量池，返回本轮变动的条目数
//（进池 + 退池），0 表示池没有任何变化。
//
// 调用方（MeterPoolJob）只在返回值大于 0 时才置 xray 重启标志：池没变就没有
// 配置改动，白置标志只会让那个 10 秒的消费任务空跑一次。
//
// 用量库不可用时静默返回 0：整个计量功能自动停用，不影响配置生成。
func (s *MeterPoolService) Recompute(now time.Time) (int, error) {
	tdb := database.GetTrafficDB()
	if tdb == nil {
		return 0, nil
	}
	// 死计数器过多时冻结换池。只停止吸纳新域名，已在池内的域名照常计量；
	// xray 任何一次整进程重启都会清空计数器，观测值归零，冻结自动解除。
	if stale := StaleMeterCounters(); stale > meterStaleCounterLimit {
		logger.Warningf("核心里已累积 %v 个已退池的计量计数器（上限 %v），"+
			"暂停调整计量池；xray 下一次整进程重启会清空它们并自动恢复",
			stale, meterStaleCounterLimit)
		return 0, nil
	}
	// 冷却期满的行直接删掉：它们已经不在池里，留着只是为了记冷却时刻，
	// 到期后就是普通的「不在池内」，该重新参选了。
	if err := tdb.Where("cooldown_until > 0 and cooldown_until <= ?", now.Unix()).
		Delete(&model.MeterDomain{}).Error; err != nil {
		return 0, err
	}

	inbounds, err := (&InboundService{}).GetAllInbounds()
	if err != nil {
		return 0, err
	}
	enabled := make([]int, 0, len(inbounds))
	for _, in := range inbounds {
		if in.Enable {
			enabled = append(enabled, in.Id)
		}
	}
	sort.Ints(enabled)
	k := meterPoolCapacity(len(enabled))

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}
	since := model.AlignHour(now.Add(-meterPoolWindow), loc)

	changed := 0
	for _, inboundId := range enabled {
		n, err := s.recomputeInbound(tdb, inboundId, k, since, now)
		changed += n
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// recomputeInbound 重算单个入站的池，返回变动条目数。
func (s *MeterPoolService) recomputeInbound(
	tdb *gorm.DB, inboundId, k int, since int64, now time.Time,
) (int, error) {
	var aggs []meterAgg
	err := tdb.Model(&model.DomainStat{}).
		Select("domain, coalesce(sum(up),0) + coalesce(sum(down),0) as bytes, coalesce(sum(count),0) as count").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?",
			model.GranularityHour, inboundId, since).
		Group("domain").
		Scan(&aggs).Error
	if err != nil {
		return 0, err
	}

	var rows []model.MeterDomain
	if err := tdb.Where("inbound_id = ?", inboundId).Find(&rows).Error; err != nil {
		return 0, err
	}
	inPool := make(map[string]*model.MeterDomain, len(rows))
	cooling := make(map[string]bool)
	for i := range rows {
		r := &rows[i]
		if r.CooldownUntil > now.Unix() {
			cooling[r.Domain] = true
			continue
		}
		inPool[r.Domain] = r
	}

	bytesOf := make(map[string]int64, len(aggs))
	for _, a := range aggs {
		bytesOf[a.Domain] = a.Bytes
	}
	// 在池且本轮实测字节为 0 的，零轮数加一；有字节则清零。
	// 零字节不会在 DomainStat 里留下行，所以这个状态只能由池表自己记。
	zeroRounds := make(map[string]int, len(inPool))
	retired := make(map[string]bool)
	for dom, r := range inPool {
		if bytesOf[dom] > 0 {
			zeroRounds[dom] = 0
			continue
		}
		zeroRounds[dom] = r.ProbeZeroRounds + 1
		if zeroRounds[dom] >= meterProbeGiveUpRounds {
			retired[dom] = true
		}
	}

	cands := buildMeterCandidates(aggs, inPool, cooling, retired, now)
	newPool := pickPool(cands, k)
	inNew := make(map[string]bool, len(newPool))
	for _, d := range newPool {
		inNew[d] = true
	}

	changed := 0
	// 进池与轮数更新。newPool 已按域名排序，写入顺序确定。
	for _, d := range newPool {
		if row, ok := inPool[d]; ok {
			if row.ProbeZeroRounds != zeroRounds[d] {
				if err := tdb.Model(&model.MeterDomain{}).Where("id = ?", row.Id).
					Update("probe_zero_rounds", zeroRounds[d]).Error; err != nil {
					return changed, err
				}
			}
			continue
		}
		if err := tdb.Create(&model.MeterDomain{
			InboundId: inboundId, Domain: d, EnteredAt: now.Unix(),
		}).Error; err != nil {
			return changed, err
		}
		changed++
	}
	// 退池。退场的写冷却、保留行；被更强候选挤掉的直接删。
	leaving := make([]string, 0, len(inPool))
	for d := range inPool {
		if !inNew[d] {
			leaving = append(leaving, d)
		}
	}
	sort.Strings(leaving)
	for _, d := range leaving {
		row := inPool[d]
		if retired[d] {
			err := tdb.Model(&model.MeterDomain{}).Where("id = ?", row.Id).
				Updates(map[string]any{
					"cooldown_until":    now.Add(meterCooldown).Unix(),
					"probe_zero_rounds": 0,
				}).Error
			if err != nil {
				return changed, err
			}
		} else if err := tdb.Delete(&model.MeterDomain{}, row.Id).Error; err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./web/service/ -run 'Recompute|MeterPool|PickPool|BuildMeter' -v`
Expected: PASS

- [ ] **Step 9: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 10: 提交**

```bash
git status --porcelain
git add web/service/meter_pool.go web/service/meter_pool_test.go web/service/meter_pool_rank_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 计量池的排序与三道闸门

三道闸门用一个排序表达式同时表达：在位加成（1.25 倍）就是替换余量，
MustKeep 就是最小驻留，连续三轮零字节的域名直接不进候选并写 24 小时冷却。

权重解决的是另一个问题：只按实测字节排会让池在第二天就冻死——没进过池的
域名字节恒为 0，永远排在所有已计量域名之后。未计量域名按 count × 平均每
连接字节折算，折算值只用于选池，不写库、不进任何接口返回体。

死计数器冻结是观测式而不是记账式：xray 的 RemoveHandler 不注销 stats
计数器且没有注销 RPC，观测值跨重启天然自愈。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 4: `MeterPoolJob`

**Files:**
- Create: `web/job/meter_pool_job.go`
- Modify: `web/web.go`（`startTask` 注册）

**Interfaces:**
- Consumes: `(*service.MeterPoolService).Recompute(now)`（Task 3）、`(*service.XrayService).SetToNeedRestart()`
- Produces: `job.NewMeterPoolJob() *MeterPoolJob`

- [ ] **Step 1: 写 job**

新建 `web/job/meter_pool_job.go`：

```go
package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// MeterPoolJob 每小时重算一次计量池。
//
// 一小时而不是更频繁：榜单的最小档位就是 1 小时，而每换一次池都是一批出站
// 与规则的热增删，且每个退池的 tag 会在核心里留下一对永不回收的计数器
//（见 MeterPoolService 里 meterStaleCounterLimit 的注释）。稳一点比新一点重要。
//
// cron.AddJob 的首次执行在一个完整周期之后，这里刻意不做延迟触发：池已经在
// 库里，面板重启后生成期照常读得到，第一个小时不重算不会丢任何东西；而全新
// 安装的机器第一个小时本来也没有数据可排。
type MeterPoolJob struct {
	meterPoolService service.MeterPoolService
	xrayService      service.XrayService
}

func NewMeterPoolJob() *MeterPoolJob {
	return new(MeterPoolJob)
}

func (j *MeterPoolJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层——日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("计量池重算任务")

	changed, err := j.meterPoolService.Recompute(time.Now())
	if err != nil {
		logger.Warning("重算计量池失败:", err)
		return
	}
	if changed == 0 {
		return
	}
	logger.Debugf("计量池变动 %v 项", changed)
	// 池变了才置标志。置了标志之后由 InboundController 那个 10 秒的消费任务
	// 调 RestartXray(false)，走 tryHotApply：出站增删与整段路由替换都有控制面
	// 接口，不会重启进程。池没变就没有配置改动，白置标志只会让消费任务空跑。
	j.xrayService.SetToNeedRestart()
}
```

- [ ] **Step 2: 注册到 `startTask`**

`web/web.go`，在注册 `TrafficCleanupJob`（`@every 1h`）那一行**之前**插入：

```go
	// 计量池重算。见 MeterPoolJob 里关于周期与「为什么不做延迟首次触发」的说明。
	s.cron.AddJob("@every 1h", job.NewMeterPoolJob())
```

- [ ] **Step 3: 编译并跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 4: 人工确认注册顺序**

Run: `grep -n "AddJob" web/web.go`
Expected: 输出里能看到 `@every 1h` 的 `NewMeterPoolJob()` 一行，且在 `NewTrafficCleanupJob()` 之前。

- [ ] **Step 5: 提交**

```bash
git status --porcelain
git add web/job/meter_pool_job.go web/web.go
git commit -m "$(cat <<'EOF'
feat(meter): 每小时重算计量池的定时任务

只在池真的变了时才置 xray 重启标志——池没变就没有配置改动，白置标志只会让
那个 10 秒的消费任务空跑一次。置标志之后走 tryHotApply：出站增删与整段路由
替换都有控制面接口，不重启进程。

刻意不做延迟首次触发：池已经在库里，面板重启后生成期照常读得到。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---
## Task 5: 生成期打开出站流量统计

**Files:**
- Create: `web/service/policy_inject.go`
- Modify: `web/service/xray.go`（`GetXrayConfig` 末尾调用）
- Modify: `web/service/config.json`（模板 `policy.system` 补两个 key）
- Test: `web/service/policy_inject_test.go`（新建）

**Interfaces:**
- Consumes: `xray.Config.Policy`（`json_util.RawMessage`）
- Produces: `service.injectOutboundStats(cfg *xray.Config) error`（包内私有）

**为什么必须在生成期注入（spec §6.6）：**

`app/proxyman/outbound/handler.go:34-56` 的 `getStatCounter` 在
`policy.ForSystem().Stats.OutboundUplink` 为假时**根本不注册计数器**。而只改
`web/service/config.json` 是不够的——那是 `xrayTemplateConfig` 的默认值，管理员一旦在设置页保存过配置，模板就已落库，改 embed 文件对他静默无效。

**无条件注入，不看池是否为空。**`policy` 在 `xray/hot_diff.go:55` 的 static 名单里，改它必然触发一次整进程重启；无条件注入让这次重启发生在**升级后的第一次配置变更**，时间点可预期、可以写进发版说明。若改成「池非空才注入」，重启会推迟到上线约一小时后池第一次形成时，变成一次谁都没预料到的全员断线。

- [ ] **Step 1: 写失败的测试**

新建 `web/service/policy_inject_test.go`：

```go
package service

import (
	"encoding/json"
	"testing"

	"a-ui/xray"
)

func decodePolicySystem(t *testing.T, cfg *xray.Config) map[string]any {
	t.Helper()
	var policy struct {
		System map[string]any `json:"system"`
	}
	if len(cfg.Policy) == 0 {
		return nil
	}
	if err := json.Unmarshal(cfg.Policy, &policy); err != nil {
		t.Fatalf("解析 policy: %v", err)
	}
	return policy.System
}

func TestInjectOutboundStatsAddsBothKeys(t *testing.T) {
	cfg := &xray.Config{}
	if err := injectOutboundStats(cfg); err != nil {
		t.Fatalf("injectOutboundStats: %v", err)
	}
	system := decodePolicySystem(t, cfg)
	if system["statsOutboundUplink"] != true || system["statsOutboundDownlink"] != true {
		t.Errorf("policy.system = %+v，期望两个出站统计开关都为 true——"+
			"getStatCounter 在它们为假时根本不注册计数器，计量出站一个字节都数不到", system)
	}
}

func TestInjectOutboundStatsKeepsAdminKeys(t *testing.T) {
	cfg := &xray.Config{Policy: []byte(
		`{"levels":{"0":{"handshake":8}},"system":{"statsInboundUplink":true,"statsUserUplink":true}}`)}
	if err := injectOutboundStats(cfg); err != nil {
		t.Fatalf("injectOutboundStats: %v", err)
	}
	system := decodePolicySystem(t, cfg)
	// 管理员自己配的 key 一个都不能丢：这里只设两个 key，不是整段替换。
	for _, k := range []string{"statsInboundUplink", "statsUserUplink"} {
		if system[k] != true {
			t.Errorf("policy.system[%q] 丢失，期望原样保留", k)
		}
	}
	var policy map[string]any
	if err := json.Unmarshal(cfg.Policy, &policy); err != nil {
		t.Fatalf("解析 policy: %v", err)
	}
	if _, ok := policy["levels"]; !ok {
		t.Error("policy.levels 丢失，期望原样保留")
	}
}

func TestInjectOutboundStatsIsByteStable(t *testing.T) {
	// 生成逐字节确定：Config.Equals 按字节比 Policy，抖动会让那个 10 秒的
	// cron 判定配置一直在变，不停重启 xray。
	first := &xray.Config{Policy: []byte(`{"system":{"statsInboundUplink":true}}`)}
	if err := injectOutboundStats(first); err != nil {
		t.Fatalf("首次: %v", err)
	}
	second := &xray.Config{Policy: []byte(`{"system":{"statsInboundUplink":true}}`)}
	if err := injectOutboundStats(second); err != nil {
		t.Fatalf("再次: %v", err)
	}
	if string(first.Policy) != string(second.Policy) {
		t.Errorf("两次结果不同：\n%s\n%s", first.Policy, second.Policy)
	}
	// 幂等：对已经注入过的配置再注入一次，结果必须完全一样。
	again := &xray.Config{Policy: first.Policy}
	if err := injectOutboundStats(again); err != nil {
		t.Fatalf("幂等: %v", err)
	}
	if string(again.Policy) != string(first.Policy) {
		t.Errorf("重复注入改变了结果：\n%s\n%s", first.Policy, again.Policy)
	}
}

func TestInjectOutboundStatsRejectsBrokenPolicy(t *testing.T) {
	// 管理员把 policy 写坏时让整份配置生成失败：xray 会保持旧配置继续跑，
	// 是安全的一侧，而错误会出现在面板日志里。
	cfg := &xray.Config{Policy: []byte(`{"system":123}`)}
	if err := injectOutboundStats(cfg); err == nil {
		t.Error("期望报错——policy.system 不是对象时不能静默吞掉")
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run InjectOutboundStats -v`
Expected: 编译失败，`undefined: injectOutboundStats`

- [ ] **Step 3: 实现注入器**

新建 `web/service/policy_inject.go`：

```go
package service

import (
	"encoding/json"

	"a-ui/xray"
)

// injectOutboundStats 在生成期打开出站流量统计。
//
// app/proxyman/outbound/handler.go:34-56 的 getStatCounter 在
// policy.ForSystem().Stats.OutboundUplink 为假时**根本不注册计数器**，
// 所以不打开这两个开关，计量出站一个字节都数不到。
//
// 不能只改 web/service/config.json：那是 xrayTemplateConfig 的默认值，
// 管理员一旦在设置页保存过配置，模板就已落库，改 embed 文件对他静默无效。
// 这与 injectAccessLog 是同一个理由、同一种做法。
//
// **只设置这两个 key**，管理员自己配的 levels / handshake / statsUserUplink
// 等一律原样保留——整段替换会在管理员不知情时改掉他的策略配置。
//
// 无条件注入，不看计量池是否为空：policy 在 xray/hot_diff.go:55 的 static
// 名单里，改它必然触发一次整进程重启；无条件注入让这次重启落在升级后的第一次
// 配置变更，时间点可预期、可以写进发版说明。改成「池非空才注入」的话，重启会
// 推迟到上线约一小时后池第一次形成时，变成一次谁都没预料到的全员断线。
//
// 用 map 中转再 Marshal：encoding/json 对 map key 排序，生成逐字节确定。
// policy 写坏时返回错误让整份配置生成失败——xray 会保持旧配置继续跑，是安全
// 的一侧，而错误会出现在面板日志里。
func injectOutboundStats(cfg *xray.Config) error {
	policy := map[string]json.RawMessage{}
	if len(cfg.Policy) > 0 {
		if err := json.Unmarshal(cfg.Policy, &policy); err != nil {
			return err
		}
	}
	system := map[string]json.RawMessage{}
	if raw, ok := policy["system"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &system); err != nil {
			return err
		}
	}
	system["statsOutboundUplink"] = json.RawMessage("true")
	system["statsOutboundDownlink"] = json.RawMessage("true")

	encodedSystem, err := json.Marshal(system)
	if err != nil {
		return err
	}
	policy["system"] = encodedSystem
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	cfg.Policy = encoded
	return nil
}
```

- [ ] **Step 4: 在 `GetXrayConfig` 里调用**

`web/service/xray.go` 的 `GetXrayConfig`，在 `injectAccessLog` 之后、`return xrayConfig, nil` 之前插入：

```go
	// 打开出站流量统计。没有它，计量出站的计数器根本不会被注册
	//（见 injectOutboundStats 的注释）。与访问日志开关一样，改动会体现在
	// 配置字节里，Config.Equals 能察觉；但 policy 段没有运行时重载接口，
	// 所以升级后的第一次配置变更会触发一次整进程重启。
	if err := injectOutboundStats(xrayConfig); err != nil {
		return nil, err
	}
```

- [ ] **Step 5: 更新模板默认值**

`web/service/config.json` 的 `policy.system` 补两个 key（只影响全新安装；存量部署靠生成期注入）：

```json
  "policy": {
    "system": {
      "statsInboundDownlink": true,
      "statsInboundUplink": true,
      "statsOutboundDownlink": true,
      "statsOutboundUplink": true
    }
  },
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./web/service/ -run 'InjectOutboundStats|GetXrayConfig|DNSInjector' -v`
Expected: PASS

- [ ] **Step 7: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git status --porcelain
git add web/service/policy_inject.go web/service/policy_inject_test.go \
        web/service/xray.go web/service/config.json
git commit -m "$(cat <<'EOF'
feat(meter): 生成期打开出站流量统计

getStatCounter 在 policy.system.statsOutbound* 为假时根本不注册计数器，
不打开这两个开关，计量出站一个字节都数不到。

必须在生成期注入而不是只改模板：模板是 xrayTemplateConfig 的默认值，管理员
保存过一次配置之后它就已落库，改 embed 文件对他静默无效。只设这两个 key，
管理员自己配的 levels/handshake/statsUserUplink 原样保留。

无条件注入，不看池是否为空：policy 在 hot_diff 的 static 名单里，改它必然
触发一次整进程重启，无条件注入让这次重启落在升级后第一次配置变更这个可预期
的时刻，而不是上线一小时后池第一次形成时。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 6: 生成计量出站与计量规则

**Files:**
- Modify: `web/service/routing_inject.go`
- Modify: `web/service/domain_stat_test.go`、`web/service/sharing_test.go`、`web/service/traffic_history_test.go`（各加一行 `t.Cleanup`，见 Step 1）
- Test: `web/service/routing_inject_meter_test.go`（新建）

**Interfaces:**
- Consumes: `(*service.MeterPoolService).Pool(now)`（Task 2）、`model.MeterTag`（Task 1）
- Produces（均为包内私有）：
  - `enabledInboundTags(inbounds []*model.Inbound) map[int]string`
  - `filterMeterPool(pool []MeterEntry, inboundTagById map[int]string) []MeterEntry`
  - `appendMeterOutbounds(outbounds []any, pool []MeterEntry) ([]any, []MeterEntry, error)`
  - `buildMeterRules(pool []MeterEntry, inboundTagById map[int]string, guard bool) []any`
  - `meterRuleNeedsIPGuard(strategy any) bool`
  - `(*RoutingInjector).buildRules` 签名改为 `buildRules(inboundTagById, outboundTagById map[int]string, defaultOutboundTag string)`

**这是本期最关键的一段，实现前必读 spec §6.1：**

计量规则带 `domain` 条件，在两遍匹配模式下会**在第一遍命中并吃掉第二遍**，让模板自带的 `geoip:private → blocked` 与管理员所有 CIDR 规则对池内域名静默失效。加 `"ip": ["0.0.0.0/0", "::/0"]` 守卫可以修好它：它匹配任意 IP，但**要求目标已经有 IP**——域名目标在第一遍没有 IP，规则整条不命中；第二遍解析出 IP 之后守卫恒真。真实 xray 上的 A/B/C/D/E 五个用例已经验证过这一结论。

守卫**只能**在核心会解析域名时加，否则计量完全空转（用例 D）。判据取 `routing.domainStrategy` 的最终值。

- [ ] **Step 1: 先修掉测试之间的用量库串扰**

计量池在用量库里，而 `database.GetTrafficDB()` 是包级变量，会跨用例残留——SQLite 在文件被 `t.TempDir` 清掉之后仍能通过已打开的 fd 读到旧数据。不修的话，别的用例写进池的行会漏进 `routing_inject_test.go`，让 `Inject` 生成出无从解释的计量出站，把既有断言（「最后一个出站是 a-ui-block」）打成随机失败。

在下面三个既有 helper 的**末尾**各加一行（`setupMeterPoolTest` 也加）：

```go
	t.Cleanup(database.ResetTrafficDBForTest)
```

- `web/service/domain_stat_test.go` 的 `setupDomainStatTest`
- `web/service/sharing_test.go` 里那个 `InitTrafficDB` 所在的 helper
- `web/service/traffic_history_test.go` 里那个 `InitTrafficDB` 所在的 helper

（`web/service/meter_pool_test.go` 的 `setupMeterPoolTest` 在 Task 2 创建时
就已经带上这一行，这里不用再动。）

Run: `go test ./web/service/ -count=1`
Expected: PASS（这一步不该改变任何行为，只是让句柄的生命周期与测试对齐）

- [ ] **Step 2: 提交这一步**

```bash
git status --porcelain
git add web/service/domain_stat_test.go web/service/sharing_test.go \
        web/service/traffic_history_test.go
git commit -m "$(cat <<'EOF'
test: 用量库句柄随测试结束一起清空

GetTrafficDB 是包级变量，会跨用例残留；SQLite 在文件被 t.TempDir 清掉之后
仍能通过已打开的 fd 读到旧数据。计量池就在那个库里，不清空的话别的用例写的
池会漏进分流注入器的测试，把断言打成随机失败。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

- [ ] **Step 3: 写失败的测试**

新建 `web/service/routing_inject_meter_test.go`：

```go
package service

import (
	"encoding/json"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// 建库与写池行都复用 meter_pool_test.go 里的 setupMeterPoolTest / putPoolRow，
// 出站与规则的解码复用 routing_inject_test.go 里的 decodeOutbounds / decodeRules，
// 不再重复一份。
func TestInjectWithEmptyPoolChangesNothing(t *testing.T) {
	setupMeterPoolTest(t)
	newTestInbound(t, 32001)

	withPool := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(withPool); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, withPool)
	// 池为空 = 一个计量出站都不生成，最后一个仍然是黑洞出站。
	if obs[len(obs)-1]["tag"] != model.BlockOutboundTag {
		t.Errorf("最后一个出站 = %v，期望 %s", obs[len(obs)-1]["tag"], model.BlockOutboundTag)
	}
	for _, ob := range obs {
		if tag, _ := ob["tag"].(string); model.IsMeterTag(tag) {
			t.Errorf("池为空却生成了计量出站 %q", tag)
		}
	}
}

func TestInjectAppendsMeterOutboundsAsDefaultCopies(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32002)
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	putPoolRow(t, in.Id, "example.com", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)

	// 计量出站追加在最末，顺序按 (inboundId asc, domain asc)。
	n := len(obs)
	wantTags := []string{
		model.MeterTag(in.Id, "doubleclick.net"),
		model.MeterTag(in.Id, "example.com"),
	}
	for i, want := range wantTags {
		got := obs[n-len(wantTags)+i]["tag"]
		if got != want {
			t.Errorf("倒数第 %d 个出站 tag = %v，期望 %q", len(wantTags)-i, got, want)
		}
	}
	// 黑洞出站仍在计量出站之前。
	if obs[n-len(wantTags)-1]["tag"] != model.BlockOutboundTag {
		t.Errorf("计量出站之前应当是黑洞出站，实际是 %v", obs[n-len(wantTags)-1]["tag"])
	}
	// 深拷贝的可观测判据：浅拷贝会让所有克隆共享同一个 map，于是默认出站
	// 会被写上最后一个计量 tag，而 diffOutbounds 要求首位逐字节不变——
	// 每次换池都会退化成整进程重启。
	if firstTag, _ := obs[0]["tag"].(string); model.IsMeterTag(firstTag) {
		t.Fatalf("默认出站的 tag 变成了 %q，说明计量出站是浅拷贝、与它共享了同一个 map", firstTag)
	}

	// 除 tag 外必须与默认出站逐字节相同——它就是默认出站的副本，
	// 换个 tag 只是为了让 xray 单独计数，转发行为一模一样。
	stripTag := func(ob map[string]any) string {
		clone := map[string]any{}
		for k, v := range ob {
			if k != "tag" {
				clone[k] = v
			}
		}
		encoded, err := json.Marshal(clone)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(encoded)
	}
	want := stripTag(obs[0])
	for i := n - len(wantTags); i < n; i++ {
		if got := stripTag(obs[i]); got != want {
			t.Errorf("计量出站内容 = %s，期望与默认出站相同 %s", got, want)
		}
	}
}

func TestInjectMeterRulesComeLastWithoutGuardByDefault(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32003)
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	rules := decodeRules(t, cfg)
	last := rules[len(rules)-1]

	if last["outboundTag"] != model.MeterTag(in.Id, "doubleclick.net") {
		t.Fatalf("最后一条规则 = %+v，期望是计量规则", last)
	}
	if got := last["domain"]; !jsonEq(t, got, []string{"domain:doubleclick.net"}) {
		t.Errorf("domain = %v，期望 [domain:doubleclick.net]——含点的裸串在 xray 里是"+
			"子串匹配，会命中 notdoubleclick.net.evil", got)
	}
	if got := last["inboundTag"]; !jsonEq(t, got, []string{in.Tag}) {
		t.Errorf("inboundTag = %v，期望 [%s]", got, in.Tag)
	}
	// 默认（模板没写 domainStrategy、开关为 0）是单遍匹配：ip 条件规则对
	// 域名目标本来就永不命中，没有第二遍可屏蔽，纯形态是安全的；反过来
	// 带上守卫会让计量完全空转。
	if _, ok := last["ip"]; ok {
		t.Error("单遍匹配下不该带 ip 守卫，否则守卫恒假、计量完全空转")
	}
}

func TestInjectMeterRulesCarryIPGuardWhenCoreResolvesDomains(t *testing.T) {
	// 三种「核心会解析域名」的形态都要带守卫。不带的话计量规则会在第一遍
	// 命中并吃掉第二遍，模板自带的 geoip:private 与管理员所有 CIDR 规则对
	// 池内域名静默失效（真实 xray 上实测复现，spec §6.1 用例 A）。
	cases := []struct {
		name         string
		resolveDomain bool
		template     string
		wantGuard    bool
	}{
		{"开关打开 → IPIfNonMatch", true, "", true},
		{"模板手写 IPOnDemand", false, "IPOnDemand", true},
		{"模板手写小写 ipifnonmatch", false, "ipifnonmatch", true},
		{"模板手写无法识别的值", false, "IPSometimes", false},
		{"模板手写 AsIs", false, "AsIs", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupMeterPoolTest(t)
			in := newTestInbound(t, 32010)
			putPoolRow(t, in.Id, "doubleclick.net", 0)
			if c.resolveDomain {
				if err := (&SettingService{}).setString("ipRuleResolveDomain", "1"); err != nil {
					t.Fatalf("setString: %v", err)
				}
			}
			cfg := newTemplateConfig(t)
			if c.template != "" {
				cfg.RouterConfig = []byte(
					`{"domainStrategy":"` + c.template + `","rules":[]}`)
			}
			if err := (&RoutingInjector{}).Inject(cfg); err != nil {
				t.Fatalf("Inject: %v", err)
			}
			rules := decodeRules(t, cfg)
			last := rules[len(rules)-1]
			ip, hasIP := last["ip"]
			if c.wantGuard {
				if !hasIP || !jsonEq(t, ip, []string{"0.0.0.0/0", "::/0"}) {
					t.Errorf("ip = %v，期望 [0.0.0.0/0 ::/0]——没有守卫，计量规则会在"+
						"第一遍命中并吃掉第二遍，IP 段规则对池内域名静默失效", ip)
				}
			} else if hasIP {
				t.Errorf("ip = %v，期望不带守卫——核心不解析域名时守卫恒假，计量完全空转", ip)
			}
		})
	}
}

func TestInjectSkipsMeterRowsOfDisabledInbounds(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32004)
	in.Enable = false
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("停用入站: %v", err)
	}
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, ob := range decodeOutbounds(t, cfg) {
		if tag, _ := ob["tag"].(string); model.IsMeterTag(tag) {
			t.Errorf("为停用入站生成了计量出站 %q", tag)
		}
	}
	for _, r := range decodeRules(t, cfg) {
		if tag, _ := r["outboundTag"].(string); model.IsMeterTag(tag) {
			t.Errorf("为停用入站生成了计量规则，outboundTag = %q", tag)
		}
	}
}

func TestInjectMeterIsByteStable(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32005)
	for _, d := range []string{"zeta.com", "alpha.com", "mid.com"} {
		putPoolRow(t, in.Id, d, 0)
	}
	first := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(first); err != nil {
		t.Fatalf("首次 Inject: %v", err)
	}
	second := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(second); err != nil {
		t.Fatalf("再次 Inject: %v", err)
	}
	// Config.Equals 按字节比较；顺序一抖动就恒判不等，那个 10 秒的 cron
	// 会不停重启 xray。
	if string(first.OutboundConfigs) != string(second.OutboundConfigs) {
		t.Errorf("出站不逐字节确定：\n%s\n%s", first.OutboundConfigs, second.OutboundConfigs)
	}
	if string(first.RouterConfig) != string(second.RouterConfig) {
		t.Errorf("路由不逐字节确定：\n%s\n%s", first.RouterConfig, second.RouterConfig)
	}
}

func TestInjectMeterCopiesRenamedDefaultOutbound(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32006)
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	cfg := newTemplateConfig(t)
	// 管理员给首个出站起过名字：tagDefaultOutbound 会原样保留它，
	// 计量出站必须是**那个**出站的副本，不能硬编码 freedom。
	cfg.OutboundConfigs = []byte(
		`[{"protocol":"freedom","tag":"我的直连","settings":{"domainStrategy":"UseIP"}},` +
			`{"protocol":"blackhole","settings":{},"tag":"blocked"}]`)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	meter := obs[len(obs)-1]
	if meter["tag"] != model.MeterTag(in.Id, "doubleclick.net") {
		t.Fatalf("最后一个出站 = %+v，期望是计量出站", meter)
	}
	settings, _ := meter["settings"].(map[string]any)
	if settings == nil || settings["domainStrategy"] != "UseIP" {
		t.Errorf("计量出站 settings = %v，期望复制了管理员那份（含 domainStrategy）", meter["settings"])
	}
}

func TestPoolChangeIsHotApplicable(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32007)
	putPoolRow(t, in.Id, "alpha.com", 0)

	before := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(before); err != nil {
		t.Fatalf("首次 Inject: %v", err)
	}
	putPoolRow(t, in.Id, "beta.com", 0)
	after := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(after); err != nil {
		t.Fatalf("换池后 Inject: %v", err)
	}

	// 换池必须能走热应用：出站增删（非首位）与整段路由替换都有控制面接口。
	// 走不了的话每小时换一次池就是每小时掐断一次所有人的连接。
	//
	// 最容易破坏这一点的是「计量出站不是深拷贝」——那会改到数组首位，
	// 而 diffOutbounds 要求默认出站逐字节不变，一变就必须整进程重启。
	diff, ok := xray.ComputeHotDiff(before, after)
	if !ok {
		t.Fatal("ComputeHotDiff 判定必须重启——换池应当能走热应用")
	}
	if len(diff.AddedOutbounds) != 1 {
		t.Errorf("新增出站 %d 个，期望 1", len(diff.AddedOutbounds))
	}
	if len(diff.RemovedOutboundTags) != 0 {
		t.Errorf("删除出站 %v，期望空", diff.RemovedOutboundTags)
	}
	if diff.RoutingConfig == nil {
		t.Error("路由配置未被标记为需要下发——新增的计量规则不会进入核心")
	}
	if len(diff.RemovedInboundTags) != 0 || len(diff.AddedInbounds) != 0 {
		t.Errorf("换池不该触碰入站：removed=%v added=%d",
			diff.RemovedInboundTags, len(diff.AddedInbounds))
	}
}

// jsonEq 比较一个来自 JSON 解码的值（[]any）与期望的字符串切片。
func jsonEq(t *testing.T, got any, want []string) bool {
	t.Helper()
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i := range want {
		if list[i] != want[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'InjectMeter|InjectWithEmptyPool|InjectAppendsMeter|InjectSkipsMeter' -v`
Expected: 计量相关断言全部 FAIL（当前 `Inject` 根本不生成计量出站与规则）

- [ ] **Step 5: 在 `routing_inject.go` 里加纯函数**

`web/service/routing_inject.go` 的 import 补上 `"sort"`（若未导入）、`"strings"`、`"time"`，并在 `tagDefaultOutbound` 之后追加：

```go
// enabledInboundTags 返回启用入站的 id -> tag。
//
// 提到 Inject 里算一次再往下传，是因为计量规则与分流规则都要用它。
// 各自再查一次 GetAllInbounds 的话，每次配置生成就要多读一遍带着
// settings/streamSettings/sniffing 这些 JSON 大字段的整张表。
func enabledInboundTags(inbounds []*model.Inbound) map[int]string {
	byId := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		if in.Enable {
			byId[in.Id] = in.Tag
		}
	}
	return byId
}

// filterMeterPool 剔除指向已停用或已删除入站的池行。
//
// 出站与规则必须消费**同一份**过滤后的列表：只过滤一侧会留下没有规则引用
// 的孤儿出站（无害但无意义），或者引用不存在出站的悬空规则——后者危险得多，
// xray 对悬空 outboundTag 不报错，运行时静默回落默认出站。
func filterMeterPool(pool []MeterEntry, inboundTagById map[int]string) []MeterEntry {
	out := make([]MeterEntry, 0, len(pool))
	for _, e := range pool {
		if _, ok := inboundTagById[e.InboundId]; ok {
			out = append(out, e)
		}
	}
	return out
}

// appendMeterOutbounds 给池里每个 (入站, 域名) 追加一个默认出站的深拷贝，
// 返回新的出站数组与**实际写进配置的**池条目。
//
// 第二个返回值是关键，与 buildOutbounds 返回 usableOutboundTags 是同一个
// 道理：调用方必须只为实际写进配置的那些条目生成规则，否则会形成悬空
// outboundTag，而 xray 对此不报错、静默回落默认出站。
//
// 深拷贝走 JSON 往返：浅拷贝会共享 settings 那个 map，DNSInjector 给计量
// 出站写 domainStrategy 时会同时写进默认出站（或者反过来），两者再也分不开。
//
// 拿不到可复制的默认出站时整个放弃计量并记 Warning，而不是退而求其次自己
// 造一个 freedom：默认出站是管理员可以改的（换协议、加 sendThrough），
// 造一个假的等于让被计量的流量走上一条与其它直连流量不同的路径。
func appendMeterOutbounds(outbounds []any, pool []MeterEntry) ([]any, []MeterEntry, error) {
	if len(pool) == 0 {
		return outbounds, nil, nil
	}
	if len(outbounds) == 0 {
		logger.Warning("生成配置里没有出站，本次不生成计量出站；域名榜单的字节列会保持为空")
		return outbounds, nil, nil
	}
	base, ok := outbounds[0].(map[string]any)
	if !ok || base == nil {
		logger.Warning("模板首位出站不是一个对象，本次不生成计量出站；域名榜单的字节列会保持为空")
		return outbounds, nil, nil
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range pool {
		var clone map[string]any
		if err := json.Unmarshal(encoded, &clone); err != nil {
			return nil, nil, err
		}
		clone["tag"] = model.MeterTag(e.InboundId, e.Domain)
		outbounds = append(outbounds, clone)
	}
	return outbounds, pool, nil
}

// meterRuleNeedsIPGuard 判断计量规则要不要带 ip 守卫。
//
// 守卫是 ip: ["0.0.0.0/0", "::/0"]：匹配任意 IP，但**要求目标已经有 IP**。
// 域名目标在第一遍匹配时没有 IP，规则整条不命中；第二遍挂上 DNS 客户端、
// 解析出 IP 之后守卫恒真。于是所有既有的 ip 条件规则（它们都排在计量规则
// 之前）在第二遍照常拿到它们本来的机会。
//
// 不加守卫的后果在真实 xray 上实测复现过（spec §6.1 用例 A）：计量规则在
// 第一遍命中，第二遍永远不发生，模板自带的 geoip:private 与管理员所有 CIDR
// 规则对池内域名——恰恰是流量最大的那批——静默失效。
//
// 判据必须与 xray 自己的解析完全一致：infra/conf/router.go 的
// getDomainStrategy 就是 strings.ToLower 之后 switch ipifnonmatch /
// ipondemand，其余一律 AsIs。判错的两种后果不对称：该带没带是上面那个安全
// 问题，不该带却带了只是计量空转（用例 D），所以无法识别的值一律按
// 「不解析」处理，落在后者。
func meterRuleNeedsIPGuard(strategy any) bool {
	s, ok := strategy.(string)
	if !ok {
		return false
	}
	switch strings.ToLower(s) {
	case "ipifnonmatch", "ipondemand":
		return true
	default:
		return false
	}
}

// buildMeterRules 生成计量规则。调用方必须把它们追加在所有其它规则之后。
//
// 这里刻意把 domain 与 ip 并进同一条规则，看上去违反了「绝不把两类条件并进
// 同一条（那是 AND）」那条不变量——必须解释清楚，否则将来一定会有人来「修」它。
// 那条不变量约束的是**管理员表达的**规则：管理员说「这批域名**或**这批 IP 走
// B」，写成一条就变成 AND、几乎永不命中。这里的 AND 是刻意要的——「域名是 X
// **且** 目标已经解析出 IP」，第二个合取项不是匹配条件，是一个遍次闸门。
// buildRule 生成管理员规则时仍然严格拆成两条，一个字节都不改。
func buildMeterRules(pool []MeterEntry, inboundTagById map[int]string, guard bool) []any {
	rules := make([]any, 0, len(pool))
	for _, e := range pool {
		// pool 已由 filterMeterPool 过滤过，这里必然取得到。
		tag := inboundTagById[e.InboundId]
		rule := map[string]any{
			"type":       "field",
			"inboundTag": []string{tag},
			// 一律带显式 domain: 前缀。含点的裸串在 xray 里是子串匹配
			//（infra/conf/router.go:175 的 defaultType 是 Domain_Substr），
			// doubleclick.net 会命中 notdoubleclick.net.evil。
			"domain":      []string{"domain:" + e.Domain},
			"outboundTag": model.MeterTag(e.InboundId, e.Domain),
		}
		if guard {
			rule["ip"] = []string{"0.0.0.0/0", "::/0"}
		}
		rules = append(rules, rule)
	}
	return rules
}
```

如果 `sort` 在本文件里没有别的用处就不要导入它——`Pool()` 已经排好序，
`filterMeterPool` 保持相对顺序，这里不需要再排。

- [ ] **Step 6: 改造 `Inject`**

把 `Inject` 的函数体替换成（其余部分保持不变）：

```go
func (s *RoutingInjector) Inject(cfg *xray.Config) error {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return err
	}
	inboundTagById := enabledInboundTags(inbounds)

	// 计量池：出站与规则必须用同一份，理由见 filterMeterPool。
	// 用量库不可用时 Pool 返回空，整个计量功能自动停用，配置照常生成。
	meterPool, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		return err
	}
	meterPool = filterMeterPool(meterPool, inboundTagById)

	outbounds, usableOutboundTags, defaultOutboundTag, err := s.buildOutbounds(cfg.OutboundConfigs)
	if err != nil {
		return err
	}
	outbounds, meterPool, err = appendMeterOutbounds(outbounds, meterPool)
	if err != nil {
		return err
	}
	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)

	blockRules, routeRules, err := s.buildRules(inboundTagById, usableOutboundTags, defaultOutboundTag)
	if err != nil {
		return err
	}

	routing := map[string]any{}
	if len(cfg.RouterConfig) > 0 {
		if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
			return err
		}
	}
	rules, _ := routing["rules"].([]any)
	if rules == nil {
		rules = make([]any, 0)
	}
	geoRules, err := s.buildGeoRules()
	if err != nil {
		return err
	}

	// 开关为 0 时【不碰】domainStrategy：模板里管理员可能手写过它，
	// 覆盖成默认值是在他不知情时改变分流行为。升级后行为零变化也靠这一条。
	//
	// 这一段必须排在计量规则之前：计量规则的形态取决于核心会不会在路由匹配期
	// 解析域名，判据正是这里写完之后的最终 domainStrategy。
	resolveDomain, err := s.settingService.GetIPRuleResolveDomain()
	if err != nil {
		return err
	}
	if resolveDomain {
		routing["domainStrategy"] = "IPIfNonMatch"
	}
	meterRules := buildMeterRules(meterPool, inboundTagById,
		meterRuleNeedsIPGuard(routing["domainStrategy"]))

	// 地区规则排在本项目生成的其余规则之前。这是对「一律 append 到末尾」
	// 的一处受控例外：模板原有的安全规则仍保持更高优先级，但地区限制属于
	// 准入判定，逻辑上必须先于任何分流决策。排在分流之后的话，非允许地区的
	// 用户访问被分流的域名时会先命中分流规则走代理出站，限制被静默绕过。
	//
	// 计量规则永远在最末：只有「本来会走默认出站」的流量才进计量，已被管理员
	// 规则命中的流量完全不受影响。
	rules = append(rules, geoRules...)
	rules = append(rules, blockRules...)
	rules = append(rules, routeRules...)
	rules = append(rules, meterRules...)
	routing["rules"] = rules

	encodedRouting, err := json.Marshal(routing)
	if err != nil {
		return err
	}
	cfg.RouterConfig = json_util.RawMessage(encodedRouting)
	return nil
}
```

- [ ] **Step 7: 改 `buildRules` 的签名**

`buildRules` 改成接收 `inboundTagById`，删掉它内部那次 `GetAllInbounds` 与 map 构造：

```go
func (s *RoutingInjector) buildRules(
	inboundTagById map[int]string,
	outboundTagById map[int]string,
	defaultOutboundTag string,
) ([]any, []any, error) {
	rules, err := s.ruleService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	if len(rules) == 0 {
		return nil, nil, nil
	}
	// inboundTagById 由 Inject 算好传进来：计量规则也要用它，各自查一次
	// GetAllInbounds 会让每次配置生成多读一遍带 JSON 大字段的整张表。
	blockRules := make([]any, 0)
	// ……以下保持原样
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./web/service/ -run 'Inject|PoolChangeIsHotApplicable' -v`
Expected: PASS（包括既有的全部 `TestInject*`）

- [ ] **Step 9: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 10: 提交**

```bash
git status --porcelain
git add web/service/routing_inject.go web/service/routing_inject_meter_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 生成计量出站与计量规则

计量出站是默认出站的深拷贝（JSON 往返，浅拷贝会让 DNS 注入器写 domainStrategy
时污染默认出站），追加在黑洞出站之后；计量规则追加在所有规则之后，只有本来会
走默认出站的流量才进计量。

规则形态按最终 routing.domainStrategy 二选一：核心会解析域名时带
ip: ["0.0.0.0/0","::/0"] 守卫。守卫匹配任意 IP 但要求目标已经有 IP，于是规则
在第一遍必然不命中、第二遍才可能命中——所有既有 ip 条件规则照常拿到它们本来的
机会。不带守卫的后果在真实 xray 上实测复现过：模板自带的 geoip:private 与管理员
所有 CIDR 规则对池内域名静默失效。判据与 xray 的 getDomainStrategy 完全一致，
无法识别的值按「不解析」处理，落在「计量空转」这条安全侧。

出站与规则消费同一份过滤后的池，否则会形成悬空 outboundTag——xray 对此不报错，
运行时静默回落默认出站。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 7: DNS 注入器给计量出站补 `domainStrategy`

**Files:**
- Modify: `web/service/dns_inject.go`（`applyFreedomStrategy` 末尾）
- Test: `web/service/dns_inject_test.go`（追加用例）

**Interfaces:**
- Consumes: `model.IsMeterTag`（Task 1）、计量出站的生成（Task 6）
- Produces: 无新导出

**为什么必须做（spec §6.5）：** `applyFreedomStrategy` 只给**数组首位**那个 freedom 出站加 `domainStrategy: "UseIP"`。计量出站是 `routingInjector` 追加到末尾的，不处理的话**被计量的直连流量会绕过内置 DNS**——`dns` 段对它们完全空转，没有报错、没有日志，正是这个功能存在的理由所描述的那种故障。

- [ ] **Step 1: 写失败的测试**

`web/service/dns_inject_test.go` 末尾追加：

```go
func TestDNSInjectorAlsoCoversMeterOutbounds(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32101)
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	if err := (&SettingService{}).setString("dnsServers", "https://8.8.8.8/dns-query"); err != nil {
		t.Fatalf("setString: %v", err)
	}

	cfg := newTemplateConfig(t)
	// 顺序不能反：routing 那一步会把整个 outbounds 数组反序列化再重新序列化，
	// 反过来的话它会把 DNS 这一步写的键无声冲掉。
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("RoutingInjector.Inject: %v", err)
	}
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("DNSInjector.Inject: %v", err)
	}

	obs := decodeOutbounds(t, cfg)
	meter := obs[len(obs)-1]
	if !model.IsMeterTag(meter["tag"].(string)) {
		t.Fatalf("最后一个出站 = %+v，期望是计量出站", meter)
	}
	settings, _ := meter["settings"].(map[string]any)
	if settings == nil || settings["domainStrategy"] != "UseIP" {
		t.Errorf("计量出站 settings = %v，期望带 domainStrategy=UseIP——"+
			"不补的话被计量的直连流量会绕过内置 DNS，dns 段对它们完全空转，"+
			"没有报错也没有日志", meter["settings"])
	}
	// 补完之后仍然必须与默认出站除 tag 外逐字节相同。
	first, _ := obs[0].(map[string]any)
	firstSettings, _ := first["settings"].(map[string]any)
	if firstSettings["domainStrategy"] != settings["domainStrategy"] {
		t.Errorf("计量出站与默认出站的 domainStrategy 不一致：%v vs %v",
			settings["domainStrategy"], firstSettings["domainStrategy"])
	}
}

func TestDNSInjectorSkipsMeterOutboundsWhenDefaultIsNotFreedom(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32102)
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	if err := (&SettingService{}).setString("dnsServers", "1.1.1.1"); err != nil {
		t.Fatalf("setString: %v", err)
	}
	cfg := newTemplateConfig(t)
	// 管理员把首位换成了别的协议：整个函数早退，计量出站也一同不写——
	// 它们是副本，单独写一个默认出站没有的键，恰恰会打破「除 tag 外逐字节
	// 相同」那条不变量。
	cfg.OutboundConfigs = []byte(
		`[{"protocol":"socks","tag":"我的上游","settings":{}},` +
			`{"protocol":"blackhole","settings":{},"tag":"blocked"}]`)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("RoutingInjector.Inject: %v", err)
	}
	if err := (&DNSInjector{}).Inject(cfg); err != nil {
		t.Fatalf("DNSInjector.Inject: %v", err)
	}
	obs := decodeOutbounds(t, cfg)
	meter := obs[len(obs)-1]
	settings, _ := meter["settings"].(map[string]any)
	if settings != nil {
		if _, ok := settings["domainStrategy"]; ok {
			t.Error("首位不是 freedom 时不该给计量出站写 domainStrategy")
		}
	}
}
```

该文件需要 import `"a-ui/database/model"`（若尚未导入）。

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run DNSInjectorAlsoCoversMeter -v`
Expected: FAIL，计量出站的 `settings` 里没有 `domainStrategy`

- [ ] **Step 3: 实现**

`web/service/dns_inject.go` 的 `applyFreedomStrategy`，在 `settings["domainStrategy"] = freedomDomainStrategy` 之后、`json.Marshal(outbounds)` 之前插入：

```go
	// 计量出站（a-ui-meter-*）是 RoutingInjector 追加到数组末尾的默认出站
	// 副本。不给它们补上同一个键的话，**被计量的直连流量会绕过内置 DNS**——
	// dns 段对它们完全空转，没有报错、没有日志，正是本功能存在的理由所描述
	// 的那种故障。
	//
	// 不需要给它们重跑一遍上面那套判定：它们是首位的逐字节副本，protocol 与
	// targetStrategy 的结论必然相同；而首位判定不通过时函数本来就已经早退，
	// 副本也就一同不写——这正是我们要的，给副本单独写一个默认出站没有的键，
	// 恰恰会打破「除 tag 外逐字节相同」那条不变量。
	for i := 1; i < len(outbounds); i++ {
		ob, ok := outbounds[i].(map[string]any)
		if !ok || ob == nil {
			continue
		}
		tag, _ := ob["tag"].(string)
		if !model.IsMeterTag(tag) {
			continue
		}
		obSettings, _ := ob["settings"].(map[string]any)
		if obSettings == nil {
			obSettings = map[string]any{}
			ob["settings"] = obSettings
		}
		obSettings["domainStrategy"] = freedomDomainStrategy
	}
```

`dns_inject.go` 的 import 补上 `"a-ui/database/model"`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run DNSInjector -v`
Expected: PASS（含既有的 `TestDNSInjectorSetsFreedomDomainStrategyThroughGetXrayConfig`）

- [ ] **Step 5: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git status --porcelain
git add web/service/dns_inject.go web/service/dns_inject_test.go
git commit -m "$(cat <<'EOF'
feat(meter): DNS 注入器给计量出站补 domainStrategy

不补的话被计量的直连流量会绕过内置 DNS——dns 段对它们完全空转，没有报错也
没有日志，正是这个功能存在的理由所描述的那种故障。

不给它们重跑判定：它们是首位的逐字节副本，protocol 与 targetStrategy 的结论
必然相同；首位判定不通过时函数本来就早退，副本一同不写——给副本单独写一个
默认出站没有的键，恰恰会打破「除 tag 外逐字节相同」那条不变量。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---
## Task 8: 采集出站字节并归到域名分时桶

**Files:**
- Create: `web/service/meter_collect.go`
- Modify: `web/service/inbound.go`（`AddTraffic` 里加一行）
- Test: `web/service/meter_collect_test.go`（新建）

**Interfaces:**
- Consumes: `model.ParseMeterTag` / `model.IsMeterTag`（Task 1）、`(*MeterPoolService).Pool`（Task 2）、`SetStaleMeterCounters`（Task 3）、`xray.Traffic{IsInbound, Tag, Up, Down}`
- Produces: `(*service.DomainStatService).RecordMetered(traffics []*xray.Traffic, now time.Time) error`

**必须复用同一次拉取（spec §2.4 / §6.7）：** `XrayTrafficJob` 每 10 秒调一次 `GetTraffic(reset=true)`，取完 xray 侧计数器清零。另起一次独立拉取会让两条链路互相偷数据。

- [ ] **Step 1: 写失败的测试**

新建 `web/service/meter_collect_test.go`：

```go
package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

func TestRecordMeteredWritesBothGranularities(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31701, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)

	// 面板时区默认 Asia/Shanghai：UTC 17:30 是当地次日 01:30，
	// 小时桶落在当地 01:00，日桶落在当地 00:00。
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	traffics := []*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 999, Down: 999},
		{IsInbound: false, Tag: model.MeterTag(in.Id, "doubleclick.net"), Up: 100, Down: 200},
	}
	if err := (&DomainStatService{}).RecordMetered(traffics, now); err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}

	for _, g := range []model.TrafficGranularity{model.GranularityHour, model.GranularityDay} {
		rows := listDomainStats(t, g)
		if len(rows) != 1 {
			t.Fatalf("粒度 %d 有 %d 行，期望 1：%+v", g, len(rows), rows)
		}
		if rows[0].Domain != "doubleclick.net" || rows[0].Up != 100 || rows[0].Down != 200 {
			t.Errorf("粒度 %d 的行 = %+v，期望 doubleclick.net 上传 100 下载 200", g, rows[0])
		}
		// 字节来自计量出站，次数来自访问日志聚合；这一轮没有访问日志，
		// 所以 Count 必须是 0——两个数据源写同一行是设计如此。
		if rows[0].Count != 0 {
			t.Errorf("粒度 %d 的 Count = %d，期望 0", g, rows[0].Count)
		}
	}
}

func TestRecordMeteredAccumulates(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31702, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	tag := model.MeterTag(in.Id, "doubleclick.net")
	svc := &DomainStatService{}

	for i := 0; i < 3; i++ {
		// 同一个桶在一小时内会被 360 轮采集写到，必须累加而不是覆盖。
		if err := svc.RecordMetered([]*xray.Traffic{{Tag: tag, Up: 10, Down: 20}}, now); err != nil {
			t.Fatalf("第 %d 轮: %v", i, err)
		}
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].Up != 30 || rows[0].Down != 60 {
		t.Errorf("累加结果 = %+v，期望上传 30 下载 60", rows)
	}
}

func TestRecordMeteredSkipsZeroAndUnparsable(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31703, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)

	err := (&DomainStatService{}).RecordMetered([]*xray.Traffic{
		// 零增量不写行：xray 对每个已注册的计数器都会返回一行（包括值为 0
		// 的），退过池的 tag 更是会一直返回 0。写进去只会造出一堆空桶。
		{Tag: model.MeterTag(in.Id, "doubleclick.net"), Up: 0, Down: 0},
		// 形态不对的 tag 一律跳过：硬猜只会把字节记到错的域名上。
		{Tag: "a-ui-meter-x-bad.com", Up: 100, Down: 100},
		{Tag: "a-ui-meter-", Up: 100, Down: 100},
		// 入站条目与普通出站条目都不属于这里。
		{IsInbound: true, Tag: in.Tag, Up: 100, Down: 100},
		{Tag: "a-ui-hk", Up: 100, Down: 100},
	}, now)
	if err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写了 %d 行，期望 0：%+v", len(rows), rows)
	}
}

func TestRecordMeteredObservesStaleCounters(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31704, "甲")
	putPoolRow(t, in.Id, "in-pool.com", 0)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	SetStaleMeterCounters(0)
	t.Cleanup(func() { SetStaleMeterCounters(0) })

	// xray 的 RemoveHandler 不注销 stats 计数器，也没有注销 RPC，所以退过池
	// 的 tag 会一直被 QueryStats 返回（值为 0）。数出来供 Recompute 决定
	// 要不要冻结换池。
	err := (&DomainStatService{}).RecordMetered([]*xray.Traffic{
		{Tag: model.MeterTag(in.Id, "in-pool.com"), Up: 1, Down: 1},
		{Tag: model.MeterTag(in.Id, "gone-1.com")},
		{Tag: model.MeterTag(in.Id, "gone-2.com")},
	}, now)
	if err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}
	if got := StaleMeterCounters(); got != 2 {
		t.Errorf("观测到 %d 个死计数器，期望 2", got)
	}
}

func TestRecordMeteredNoopWithoutMeterEntries(t *testing.T) {
	setupMeterPoolTest(t)
	SetStaleMeterCounters(7)
	t.Cleanup(func() { SetStaleMeterCounters(0) })
	// 完全没有计量条目时也要把观测值归零：xray 重启之后计数器全清，
	// 观测值必须跟着归零，冻结才能自动解除。
	err := (&DomainStatService{}).RecordMetered([]*xray.Traffic{
		{IsInbound: true, Tag: "inbound-1", Up: 1, Down: 1},
	}, time.Now())
	if err != nil {
		t.Fatalf("RecordMetered: %v", err)
	}
	if got := StaleMeterCounters(); got != 0 {
		t.Errorf("观测值 = %d，期望 0", got)
	}
}

func TestRecordMeteredSilentWhenTrafficDBMissing(t *testing.T) {
	setupMeterPoolTest(t)
	database.ResetTrafficDBForTest()
	// 库不可用时静默返回：字节数据少一段，比让 AddTraffic 整个失败轻得多。
	err := (&DomainStatService{}).RecordMetered(
		[]*xray.Traffic{{Tag: "a-ui-meter-1-x.com", Up: 1, Down: 1}}, time.Now())
	if err != nil {
		t.Errorf("RecordMetered = %v，期望 nil", err)
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run RecordMetered -v`
Expected: 编译失败，`RecordMetered undefined`

- [ ] **Step 3: 实现**

新建 `web/service/meter_collect.go`：

```go
package service

import (
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/xray"
)

// meterDelta 是一轮采集里某个 (入站, 域名) 的字节增量。
type meterDelta struct {
	inboundId int
	domain    string
	up        int64
	down      int64
}

// RecordMetered 把计量出站的字节增量归到域名分时桶上。
//
// 输入必须是 XrayTrafficJob 那**同一次** GetTraffic(reset=true) 的结果：
// reset=true 会清零 xray 侧的计数器，另起一次独立拉取会让两条链路互相偷数据。
//
// 与 TrafficHistoryService.Record 并列挂在 AddTraffic 上，失败只告警不阻断：
// inbounds.up/down 是限额与到期判定的输入，它停止累加的后果（用户超额不被
// 停用）比榜单少一段字节数据严重得多。
//
// 一个必须记住的语义差异：DomainStat.Count 来自访问日志，记的是**连接建立**
// 时刻；Up/Down 来自这里，记的是**流量发生**时刻。一条 10:59 建立、传到 11:30
// 的连接，它的 1 次计数落在 10 点桶，字节分落在 10 点和 11 点两个桶。所以同一
// 个桶里两者不是同一批连接的统计量，一个域名完全可能出现 Count=0 而 Up=5GB
// 的行。把它们强行对齐需要连接级的字节数，而 xray 不提供（spec §2）。
func (s *DomainStatService) RecordMetered(traffics []*xray.Traffic, now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil || len(traffics) == 0 {
		return nil
	}
	// 先扫一遍。一个没开计量的面板每 10 秒也会收到一整份全零的增量，
	// 提前返回能省掉下面的池查询与 GetTimeLocation（后者每次都要重新读
	// tzdata 文件，Go 不缓存它）。
	//
	// 归零观测值也必须做在这里：xray 整进程重启之后计数器全清，这一支正是
	// 那之后的第一轮，观测值不跟着归零的话冻结永远解不开。
	hasMeter := false
	for _, t := range traffics {
		if !t.IsInbound && model.IsMeterTag(t.Tag) {
			hasMeter = true
			break
		}
	}
	if !hasMeter {
		SetStaleMeterCounters(0)
		return nil
	}

	pool, err := (&MeterPoolService{}).Pool(now)
	if err != nil {
		return err
	}
	inPool := make(map[string]bool, len(pool))
	for _, e := range pool {
		inPool[model.MeterTag(e.InboundId, e.Domain)] = true
	}

	var stale int64
	byKey := make(map[string]*meterDelta, len(traffics))
	for _, t := range traffics {
		if t.IsInbound || !model.IsMeterTag(t.Tag) {
			continue
		}
		// 观测死计数器：xray 的 RemoveHandler（app/proxyman/outbound/outbound.go:131）
		// 只删 handler 不注销 stats 计数器，StatsService 也没有注销 RPC，
		// 所以退过池的 tag 会留到进程退出为止，每轮都被返回一遍（值为 0）。
		// 数出来供 MeterPoolService.Recompute 决定要不要冻结换池。
		if !inPool[t.Tag] {
			stale++
		}
		// 零增量不写行，与 TrafficBucket 同规。上面那些死计数器返回的正是 0，
		// 不跳过的话每轮都会为它们造一堆空桶。
		if t.Up == 0 && t.Down == 0 {
			continue
		}
		id, dom, ok := model.ParseMeterTag(t.Tag)
		if !ok {
			// 形态不对的 tag 无法归因，硬猜只会把字节记到错的域名上。
			logger.Debug("跳过无法解析的计量出站 tag:", t.Tag)
			continue
		}
		d := byKey[t.Tag]
		if d == nil {
			d = &meterDelta{inboundId: id, domain: dom}
			byKey[t.Tag] = d
		}
		d.up += t.Up
		d.down += t.Down
	}
	SetStaleMeterCounters(stale)
	if len(byKey) == 0 {
		return nil
	}

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	hour := model.AlignHour(now, loc)
	day := model.AlignDay(now, loc)

	// 按 tag 排序后再写：map 的遍历顺序是随机的，固定写序让并发下的锁顺序
	// 也固定，且失败时重跑的行为可复现。
	tags := make([]string, 0, len(byKey))
	for tag := range byKey {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	return db.Transaction(func(tx *gorm.DB) error {
		for _, tag := range tags {
			d := byKey[tag]
			if err := upsertDomainStatBytes(tx, model.GranularityHour, d, hour); err != nil {
				return err
			}
			if err := upsertDomainStatBytes(tx, model.GranularityDay, d, day); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertDomainStatBytes 把字节增量累加进目标桶，桶不存在时创建。
//
// 与第一期的 upsertDomainStat 完全同构，只是改的列不同：那边累加 count，
// 这边累加 up/down。两者写的可能是同一行——次数来自访问日志聚合，字节来自
// 这里，谁先到谁建行。
func upsertDomainStatBytes(db *gorm.DB, g model.TrafficGranularity, d *meterDelta, start int64) error {
	row := &model.DomainStat{
		Granularity: g, InboundId: d.inboundId, Domain: d.domain,
		BucketStart: start, Up: d.up, Down: d.down,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "granularity"}, {Name: "inbound_id"}, {Name: "domain"}, {Name: "bucket_start"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"up":   gorm.Expr("domain_stats.up + ?", d.up),
			"down": gorm.Expr("domain_stats.down + ?", d.down),
		}),
	}).Create(row).Error
}
```

本实现用的是 `model.IsMeterTag`，不需要 `strings`——import 里不要写它。

- [ ] **Step 4: 接到 `AddTraffic` 上**

`web/service/inbound.go` 的 `AddTraffic`，在 `TrafficHistoryService.Record` 那段之后追加：

```go
	// 计量出站的字节同样来自这一次拉取（reset=true 已经清零 xray 侧计数器，
	// 另起一次独立拉取会让两条链路互相偷数据）。失败只告警不阻断，理由同上。
	if err := (&DomainStatService{}).RecordMetered(traffics, time.Now()); err != nil {
		logger.Warning("记录域名计量流量失败:", err)
	}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./web/service/ -run 'RecordMetered|AddTraffic' -v`
Expected: PASS

- [ ] **Step 6: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git status --porcelain
git add web/service/meter_collect.go web/service/meter_collect_test.go web/service/inbound.go
git commit -m "$(cat <<'EOF'
feat(meter): 采集计量出站的字节并归到域名分时桶

复用 XrayTrafficJob 那同一次 GetTraffic(reset=true)：reset 会清零 xray 侧
计数器，另起一次独立拉取会让两条链路互相偷数据。失败只告警不阻断——
inbounds.up/down 是限额判定的输入，它停止累加比榜单少一段数据严重得多。

顺带观测死计数器：RemoveHandler 不注销 stats 计数器且没有注销 RPC，退过池的
tag 会一直被返回（值为 0）。数出来供 Recompute 决定要不要冻结换池；没有任何
计量条目时归零，这样 xray 重启后冻结能自动解除。

Count 来自访问日志（连接建立时刻），Up/Down 来自这里（流量发生时刻），同一
个桶里两者不是同一批连接的统计量——一个域名出现 Count=0 而 Up=5GB 是正常的。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 9: 榜单查询支持排序维度与覆盖度

**Files:**
- Modify: `web/service/domain_stat.go`（`TopDomainResult`、`TopDomains`，新增 `TopDomainOrder`、`TopDomainCoverage`）
- Modify: `web/controller/inbound.go`（`getTopDomains` 绑定并钳制 `orderBy`）
- Test: `web/service/domain_stat_top_test.go`（新建）

**Interfaces:**
- Consumes: `model.DomainStat`、`model.TrafficBucket`、`model.MeterDomain`
- Produces:
  - `service.TopDomainOrder`（`TopOrderCount` / `TopOrderUp` / `TopOrderDown`）
  - `service.TopDomainCoverage{MeteredBytes, TotalBytes int64; Ratio *float64}`
  - `(*service.DomainStatService).TopDomains(inboundId int, r TopDomainRange, order TopDomainOrder, limit int, now time.Time) (*TopDomainResult, error)` —— **签名新增第三个参数**
  - `TopDomainResult` 新增 `OrderBy string` 与 `Coverage *TopDomainCoverage`

**覆盖度的口径（spec §6.8）：**

```
该入站该周期已计量字节 = Σ DomainStat.(Up+Down)
该入站该周期总字节     = Σ TrafficBucket.(Up+Down)   （同库、同粒度、同对齐、同一次 GetTraffic 采集）
覆盖率 = 已计量 / 总字节，钳到 [0,1]；分母为 0 时为 null
```

入站计的是客户端与面板之间的**加密流**，出站计的是面板与目标之间的流，两者相差一层协议开销，所以覆盖率结构性地小于 100%——UI 必须标注为「约」。

`Metered` 的判据是「该入站在 `meter_domains` 里至少有一行未在冷却」：池表就是生成端读的那张表，用它做判据两侧永远一致；用「配置里真的有计量规则」做判据则需要查询接口去反推一次配置生成的结果，那条通路不存在，硬造只会多一个会与生成端漂移的真相源。

- [ ] **Step 1: 写失败的测试**

新建 `web/service/domain_stat_top_test.go`：

```go
package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// putTrafficBucket 直接写一行用量桶，供覆盖度的分母使用。
func putTrafficBucket(t *testing.T, inboundId int, bucketStart, up, down int64) {
	t.Helper()
	row := &model.TrafficBucket{
		Granularity: model.GranularityHour, InboundId: inboundId,
		BucketStart: bucketStart, Up: up, Down: down,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入用量桶: %v", err)
	}
}

func TestTopDomainsOrdersByBytes(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31801, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	// few.com 次数少但上传巨大——正是管理员要找的那种「谁在偷偷上传」。
	putDomainStat(t, in.Id, "few.com", bucket, 2, 900, 10)
	putDomainStat(t, in.Id, "many.com", bucket, 500, 5, 900)

	svc := &DomainStatService{}
	byCount, err := svc.TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if byCount.List[0].Domain != "many.com" {
		t.Errorf("按次数排首位 = %q，期望 many.com", byCount.List[0].Domain)
	}
	byUp, err := svc.TopDomains(in.Id, TopRange1h, TopOrderUp, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if byUp.List[0].Domain != "few.com" {
		t.Errorf("按上传排首位 = %q，期望 few.com——这正是「哪些域名上传最多」"+
			"这个诉求要回答的问题", byUp.List[0].Domain)
	}
	byDown, err := svc.TopDomains(in.Id, TopRange1h, TopOrderDown, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if byDown.List[0].Domain != "many.com" {
		t.Errorf("按下载排首位 = %q，期望 many.com", byDown.List[0].Domain)
	}
	if byUp.OrderBy != string(TopOrderUp) {
		t.Errorf("OrderBy = %q，期望 up——前端要靠它回显", byUp.OrderBy)
	}
}

func TestTopDomainsFallsBackToCountOnUnknownOrder(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31802, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)

	// 这是个展示接口，一个拼错的参数不该变成报错弹窗。
	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopDomainOrder("乱写"), 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.OrderBy != string(TopOrderCount) {
		t.Errorf("OrderBy = %q，期望回落 count", got.OrderBy)
	}
}

func TestTopDomainsMeteredFollowsPool(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31803, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	svc := &DomainStatService{}

	got, err := svc.TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Metered {
		t.Error("池为空时 Metered 应为 false——前端据此隐藏字节列，"+
			"显示一列恒为 0 的「上传」会被理解成「他没上传过」")
	}
	if got.Coverage != nil {
		t.Errorf("Coverage = %+v，期望 nil", got.Coverage)
	}

	putPoolRow(t, in.Id, "doubleclick.net", 0)
	got, err = svc.TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if !got.Metered {
		t.Error("池非空时 Metered 应为 true")
	}
}

func TestTopDomainsCoverageRatio(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31804, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	putDomainStat(t, in.Id, "doubleclick.net", bucket, 1, 300, 200) // 已计量 500
	putTrafficBucket(t, in.Id, bucket, 600, 400)                    // 总计 1000

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Coverage == nil {
		t.Fatal("Coverage 为 nil，期望有值")
	}
	if got.Coverage.MeteredBytes != 500 || got.Coverage.TotalBytes != 1000 {
		t.Errorf("Coverage = %+v，期望已计量 500 / 总计 1000", got.Coverage)
	}
	if got.Coverage.Ratio == nil || *got.Coverage.Ratio != 0.5 {
		t.Errorf("Ratio = %v，期望 0.5", got.Coverage.Ratio)
	}
}

func TestTopDomainsCoverageNilWhenNoTotal(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31805, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Coverage == nil || got.Coverage.Ratio != nil {
		t.Errorf("Coverage = %+v，期望 Ratio 为 nil——分母为 0 时不显示，"+
			"显示 0%% 会被理解成「一点都没归因到」", got.Coverage)
	}
}

func TestTopDomainsCoverageClampsAboveOne(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31806, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	// 入站计的是加密流、出站计的是明文流，协议开销通常让覆盖率小于 1；
	// 但极端情况下可能越界，显示 103% 会让整块数据失去可信度。
	putDomainStat(t, in.Id, "doubleclick.net", bucket, 1, 900, 900)
	putTrafficBucket(t, in.Id, bucket, 500, 500)

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Coverage.Ratio == nil || *got.Coverage.Ratio != 1 {
		t.Errorf("Ratio = %v，期望钳到 1", got.Coverage.Ratio)
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run TopDomains -v`
Expected: 编译失败（`TopDomains` 参数个数不对、`TopOrderUp` 未定义）

- [ ] **Step 3: 扩展类型与查询**

`web/service/domain_stat.go`，在 `TopDomainRange` 那组常量之后追加：

```go
// TopDomainOrder 是榜单的排序维度。
type TopDomainOrder string

const (
	TopOrderCount TopDomainOrder = "count"
	TopOrderUp    TopDomainOrder = "up"
	TopOrderDown  TopDomainOrder = "down"
)

// normalizeTopOrder 把档位翻译成 (实际生效值, ORDER BY 子句)。
//
// 未知值回落 count——这是个展示接口，一个拼错的参数不该变成报错弹窗。
// 排序键末位一律用域名字典序兜底：数值相同时顺序抖动会让自动刷新的榜单
// 里的行无端跳动。
func normalizeTopOrder(o TopDomainOrder) (TopDomainOrder, string) {
	switch o {
	case TopOrderUp:
		return TopOrderUp, "up desc, domain asc"
	case TopOrderDown:
		return TopOrderDown, "down desc, domain asc"
	default:
		return TopOrderCount, "count desc, domain asc"
	}
}

// TopDomainCoverage 说明这份榜单覆盖了该入站多大比例的流量。
//
// 分子来自 DomainStat 的字节列（计量出站数出来的），分母来自 TrafficBucket
//（入站计数器数出来的）。两者同库、同粒度、同对齐、同一次 GetTraffic 采集，
// 可比性是结构性的。
//
// 它仍然是**近似值**：入站计的是客户端与面板之间的加密流，出站计的是面板与
// 目标之间的流，两者相差一层协议开销与握手，所以覆盖率结构性地小于 100%。
// UI 必须如实标注为「约」。
//
// 差额有三部分：被管理员自己的分流规则带走的流量、走默认出站但不在计量池里
// 的域名、以及上面那层协议开销。比例低时榜单不可信，比例高时榜单就是答案——
// 这是覆盖度唯一的用途。
type TopDomainCoverage struct {
	MeteredBytes int64 `json:"meteredBytes"`
	TotalBytes   int64 `json:"totalBytes"`
	// Ratio 为 nil 表示分母为 0，界面上整行不显示。显示 0% 会被理解成
	// 「一点都没归因到」，那是另一回事。
	Ratio *float64 `json:"ratio"`
}
```

`TopDomainResult` 增加两个字段：

```go
type TopDomainResult struct {
	// Metered 为 false 表示这批数据只有访问次数，没有字节数。判据是「该入站
	// 在 meter_domains 里至少有一行未在冷却」——池表就是生成端读的那张表，
	// 用它两侧永远一致；用「配置里真的有计量规则」做判据则要反推一次配置生成
	// 的结果，那条通路不存在，硬造只会多一个会与生成端漂移的真相源。
	Metered bool           `json:"metered"`
	Range   string         `json:"range"`
	OrderBy string         `json:"orderBy"`
	Limit   int            `json:"limit"`
	List    []TopDomainRow `json:"list"`
	// Coverage 在 Metered 为 false 时为 nil。
	Coverage *TopDomainCoverage `json:"coverage"`
}
```

`TopDomains` 改成：

```go
func (s *DomainStatService) TopDomains(
	inboundId int, r TopDomainRange, order TopDomainOrder, limit int, now time.Time,
) (*TopDomainResult, error) {
	inboundService := InboundService{}
	if _, err := inboundService.GetInbound(inboundId); err != nil {
		return nil, err
	}
	g, back, effective := topRangeSpec(r)
	effectiveOrder, orderClause := normalizeTopOrder(order)
	if limit <= 0 {
		limit = 10
	}
	result := &TopDomainResult{
		Range:   string(effective),
		OrderBy: string(effectiveOrder),
		Limit:   limit,
		List:    make([]TopDomainRow, 0, limit), // 不能给前端 null
	}
	db := database.GetTrafficDB()
	if db == nil {
		return result, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	var since int64
	if g == model.GranularityHour {
		since = model.AlignHour(now.Add(-back), loc)
	} else {
		since = model.AlignDay(now.Add(-back), loc)
	}

	var rows []TopDomainRow
	err = db.Model(&model.DomainStat{}).
		Select("domain, sum(count) as count, sum(up) as up, sum(down) as down").
		// since 和桶起点一样落在对齐边界上，用 ">=" 会把 since 自身那一桶
		// 也算进来，所以「最近 1 小时」实际覆盖的是 60~120 分钟，不是精确
		// 的 60 分钟。这是刻意的取舍，理由见第一期的说明。
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Group("domain").
		Order(orderClause).
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows != nil {
		result.List = rows
	}

	metered, err := s.inboundIsMetered(db, inboundId, now)
	if err != nil {
		return nil, err
	}
	result.Metered = metered
	if metered {
		coverage, err := s.coverage(db, g, inboundId, since)
		if err != nil {
			return nil, err
		}
		result.Coverage = coverage
	}
	return result, nil
}

// inboundIsMetered 判断这个入站当前是否有域名正在被计量。
//
// 已知的不精确处：入站被停用时不生成计量规则，但池行还在（生成期刻意不删，
// 入站可能只是临时停用），于是这里仍会返回 true。后果只是给一个没有流量的
// 入站显示了字节列与 0% 覆盖率，可以接受；反过来把它做精确，就要在查询路径上
// 引入一次入站启用状态的判断，而那个状态与「历史上这段时间是否被计量过」
// 根本不是一回事——榜单查的是过去 15 天，入站是此刻的状态。
func (s *DomainStatService) inboundIsMetered(db *gorm.DB, inboundId int, now time.Time) (bool, error) {
	var n int64
	err := db.Model(&model.MeterDomain{}).
		Where("inbound_id = ? and cooldown_until <= ?", inboundId, now.Unix()).
		Count(&n).Error
	return n > 0, err
}

// coverage 算出这份榜单覆盖了该入站多大比例的流量。
func (s *DomainStatService) coverage(
	db *gorm.DB, g model.TrafficGranularity, inboundId int, since int64,
) (*TopDomainCoverage, error) {
	var metered struct{ Up, Down int64 }
	err := db.Model(&model.DomainStat{}).
		Select("coalesce(sum(up),0) as up, coalesce(sum(down),0) as down").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Scan(&metered).Error
	if err != nil {
		return nil, err
	}
	var total struct{ Up, Down int64 }
	err = db.Model(&model.TrafficBucket{}).
		Select("coalesce(sum(up),0) as up, coalesce(sum(down),0) as down").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Scan(&total).Error
	if err != nil {
		return nil, err
	}
	out := &TopDomainCoverage{
		MeteredBytes: metered.Up + metered.Down,
		TotalBytes:   total.Up + total.Down,
	}
	if out.TotalBytes > 0 {
		// 钳到 [0,1]：协议开销在极端情况下可能让比值越界，显示一个 103%
		// 或负数会让整块数据失去可信度。
		ratio := float64(out.MeteredBytes) / float64(out.TotalBytes)
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		out.Ratio = &ratio
	}
	return out, nil
}
```

`domain_stat.go` 的 import 需要 `"gorm.io/gorm"`（已有）。

- [ ] **Step 4: 更新 controller**

`web/controller/inbound.go` 的 `getTopDomains`，绑定结构体加一个字段并钳制：

```go
	form := struct {
		Range   string `form:"range"`
		OrderBy string `form:"orderBy"`
		Limit   int    `form:"limit"`
	}{}
```

调用改成：

```go
	result, err := a.domainStatService.TopDomains(id, service.TopDomainRange(form.Range),
		service.TopDomainOrder(form.OrderBy), form.Limit, time.Now())
```

`orderBy` 的非法值由 service 的 `normalizeTopOrder` 回落，controller 不再重复一层——回落规则只有一处，界面回显的也是 service 给出的实际生效值。

- [ ] **Step 5: 修既有调用点与测试**

`TopDomains` 多了一个参数，所有既有调用都要补 `TopOrderCount`。

Run: `go build ./... 2>&1 | head -20`
按输出逐一修正（预期只有 `web/controller/inbound.go` 与 `web/service/domain_stat_test.go` 里的调用）。

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./web/service/ ./web/controller/ -run 'TopDomains|getTopDomains' -v`
Expected: PASS

- [ ] **Step 7: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git status --porcelain
git add web/service/domain_stat.go web/service/domain_stat_top_test.go \
        web/service/domain_stat_test.go web/controller/inbound.go
git commit -m "$(cat <<'EOF'
feat(meter): 榜单支持按上传/下载排序并给出覆盖度

覆盖度 = Σ DomainStat 字节 / Σ TrafficBucket 字节。两者同库、同粒度、同对齐、
同一次 GetTraffic 采集，可比性是结构性的；而且都按入站分桶——原来设想的三项
出站差值算不出每入站的覆盖度，出站计数器没有入站维度。

仍是近似值：入站计的是加密流、出站计的是明文流，覆盖率结构性地小于 100%。
分母为 0 时 Ratio 为 nil（不显示，而不是显示 0%），比值钳到 [0,1]。

Metered 的判据是池表里有没有未冷却的行——池表就是生成端读的那张表，两侧
永远一致。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---
## Task 10: 前端字节列、排序切换与覆盖度

**Files:**
- Modify: `web/html/xui/access_log_modal.html`
- Test: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot'`

**Interfaces:**
- Consumes: `/aui/inbound/topDomains/:id` 的 `metered` / `orderBy` / `coverage`（Task 9）
- Produces: 无

**两个既有陷阱：**

- **`a-tabs` 的非活动面板仍在 DOM 里**，只是被隐藏。写选择器或做自动化时必须限定 `.ant-tabs-tabpane-active`。
- **Vue 指令写在根元素之外是死代码，且完全静默。** 这个弹窗有自己的 Vue 根实例（`new Vue({el: '#access-log-modal'})`，见文件第 366-369 行），所有新增内容必须留在 `<a-modal id="access-log-modal">` 这棵子树里，**不是 `#app`**。

`sizeFormat` 是 `web/assets/js/util/common.js` 里的全局函数，模板里可以直接用（既有的 `DateUtil.formatMillis` 就是同样的用法）。**本任务不改 `web/assets` 下的任何文件**，所以不涉及 `cur_ver` 强缓存问题。

- [ ] **Step 1: 替换 Top 面板的头部区块**

把 `<a-tab-pane key="top" tab="Top 域名">` 里那个 `<div style="margin-bottom: 12px">…</div>` 整块替换成：

```html
            <div style="margin-bottom: 12px">
                <a-radio-group :value="accessLogModal.topRange" size="small" button-style="solid"
                               @change="e => accessLogModal.switchRange(e.target.value)">
                    <a-radio-button value="1h">1 小时</a-radio-button>
                    <a-radio-button value="6h">6 小时</a-radio-button>
                    <a-radio-button value="12h">12 小时</a-radio-button>
                    <a-radio-button value="24h">24 小时</a-radio-button>
                    <a-radio-button value="7d">7 天</a-radio-button>
                    <a-radio-button value="15d">15 天</a-radio-button>
                </a-radio-group>
                <a-radio-group v-if="accessLogModal.topMetered" style="margin-left: 12px"
                               :value="accessLogModal.topOrder" size="small"
                               @change="e => accessLogModal.switchOrder(e.target.value)">
                    <a-radio-button value="count">按次数</a-radio-button>
                    <a-radio-button value="up">按上传</a-radio-button>
                    <a-radio-button value="down">按下载</a-radio-button>
                </a-radio-group>
            </div>
            <div v-if="!accessLogModal.topMetered" style="margin-bottom: 12px; color: rgba(0,0,0,.45)">
                统计的是<b>连接次数</b>，不是流量——上传大文件往往只有很少几条长连接，未必排在前面。
            </div>
            <div v-else style="margin-bottom: 12px; color: rgba(0,0,0,.45)">
                <div v-if="accessLogModal.topCoverageText">[[ accessLogModal.topCoverageText ]]</div>
                <div>
                    字节数只统计<b>走直连出去</b>的流量：被你自己的分流规则带走（走出站节点或被封禁）的部分不在其中。
                    计量的域名集合每小时自动调整一次，刚上线时排序主要来自访问次数，运行一段时间后才由实测流量主导。
                </div>
                <div>
                    「访问次数」按<b>连接建立</b>时刻归类，「上传/下载」按<b>流量发生</b>时刻归类，
                    所以一条跨越整点的长连接可能出现在「0 次访问、却有几个 GB」的行里。
                </div>
            </div>
```

- [ ] **Step 2: 给表格补上两个字节列的插槽**

在 `<a-table :columns="accessLogModal.topColumns" …>` 内部，`domain` 那个 `<template>` 之后追加：

```html
                <template slot="up" slot-scope="text, row">
                    [[ sizeFormat(row.up) ]]
                </template>
                <template slot="down" slot-scope="text, row">
                    [[ sizeFormat(row.down) ]]
                </template>
```

- [ ] **Step 3: 扩展 data**

把 `topRange: '24h',` 起到 `topColumns: […],` 那一段替换成：

```js
        tab: 'detail',
        topRange: '24h',
        // topOrder 只在服务端报告 metered 为真时才可能不是 count：没有字节数据
        // 时按上传排序会得到一张全 0 的表，比不给这个选项更让人困惑。
        topOrder: 'count',
        topList: [],
        topLoading: false,
        topLoaded: false,
        // topMetered 由服务端给出（该入站是否有域名正在被计量）。为假时必须
        // 整列隐藏字节列——显示一列恒为 0 的「上传」会被理解成「他没上传过」，
        // 比不显示更糟。
        topMetered: false,
        topCoverageText: '',
        topColumns: [],
```

- [ ] **Step 4: 加列的构造与排序切换方法**

在 `switchRange(range) { … }` 之后插入：

```js
        switchOrder(order) {
            this.topOrder = order;
            this.loadTop();
        },
        // 列按 metered 现算而不是写死：ant-design-vue 的 columns 是普通数组，
        // 而这个弹窗的状态挂在一个纯对象上（不是 Vue 实例），用不了 computed。
        rebuildTopColumns() {
            const columns = [{
                title: "#", align: 'center', width: 60,
                scopedSlots: { customRender: 'rank' },
            }, {
                title: "域名", align: 'left',
                scopedSlots: { customRender: 'domain' },
            }, {
                title: "访问次数", align: 'right', width: 110, dataIndex: "count",
            }];
            if (this.topMetered) {
                columns.push({
                    title: "上传", align: 'right', width: 110,
                    scopedSlots: { customRender: 'up' },
                }, {
                    title: "下载", align: 'right', width: 110,
                    scopedSlots: { customRender: 'down' },
                });
            }
            this.topColumns = columns;
        },
        // 覆盖度必须如实说成「约」：入站计的是客户端与面板之间的加密流，
        // 出站计的是面板与目标之间的流，两者相差一层协议开销，比值结构性地
        // 小于 100%。服务端给不出比值（该周期没有任何流量）时整行不显示，
        // 显示 0% 会被理解成「一点都没归因到」，那是另一回事。
        formatCoverage(coverage) {
            if (!coverage || coverage.ratio === null || coverage.ratio === undefined) {
                return '';
            }
            const percent = Math.round(coverage.ratio * 100);
            return `本周期该用户约 ${percent}% 的流量已归因到具体域名（`
                + `${sizeFormat(coverage.meteredBytes)} / ${sizeFormat(coverage.totalBytes)}）。`;
        },
```

- [ ] **Step 5: 更新 `loadTop` 与 `show`**

`loadTop` 替换成：

```js
        async loadTop() {
            this.topLoading = true;
            const msg = await HttpUtil.post(`/aui/inbound/topDomains/${this.inboundId}`, {
                range: this.topRange,
                orderBy: this.topOrder,
                limit: 20,
            });
            this.topLoading = false;
            if (!msg.success) {
                return;
            }
            this.topList = msg.obj.list || [];
            // 一律回显服务端给出的实际生效值：档位与排序维度的回落规则只有
            // 服务端那一处，前端自己猜会和它漂移。
            this.topRange = msg.obj.range;
            this.topOrder = msg.obj.orderBy || 'count';
            this.topMetered = !!msg.obj.metered;
            this.topCoverageText = this.formatCoverage(msg.obj.coverage);
            this.rebuildTopColumns();
            this.topLoaded = true;
        },
```

`show()` 里 `this.topList = []; this.topLoaded = false;` 那两行替换成：

```js
            this.topList = [];
            this.topLoaded = false;
            this.topOrder = 'count';
            this.topMetered = false;
            this.topCoverageText = '';
            this.rebuildTopColumns();
```

- [ ] **Step 6: 跑模板不变量测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: PASS

`web.go` 的 `getHtmlTemplate` **吞掉 `ParseFS` 错误**（`// ignore`），一个语法错误的模板会被静默跳过，直到渲染时才报 "template not found"——所以改完模板光靠 `go build` 发现不了问题，这两条测试是唯一的守卫。

- [ ] **Step 7: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git status --porcelain
git add web/html/xui/access_log_modal.html
git commit -m "$(cat <<'EOF'
feat(meter): 榜单显示上传/下载字节、排序切换与覆盖度

metered 为假时整列隐藏字节列——显示一列恒为 0 的「上传」会被理解成
「他没上传过」，比不显示更糟。

三句必须说清楚的边界写进面板：字节只统计走直连出去的流量、计量域名集合每小时
调整一次且刚上线时排序主要来自访问次数、访问次数按连接建立时刻归类而字节按
流量发生时刻归类（所以会出现「0 次访问、几个 GB」的行）。

覆盖度如实说成「约」，服务端给不出比值时整行不显示而不是显示 0%。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 11: 真实 xray 的规则形态回归测试

**Files:**
- Create: `web/service/meter_rule_e2e_test.go`

**Interfaces:**
- Consumes: `xray.GetBinaryPath()`、既有测试辅助 `requireXrayBinary` / `freePort` / `waitForPort`
- Produces: 无

**这是本期唯一一条「测试我们没有做错什么」的用例，不能省。** 它把 spec §6.1 那张交叉实验表固化下来：不带守卫的计量规则会让模板自带的 `geoip:private → blocked` 对池内域名静默失效。四个用例覆盖两个方向——危险确实存在（A）、守卫确实挡住了它（B）、守卫不妨碍正常计量（C）、守卫在单遍模式下确实恒假（D，这正是形态必须按 `domainStrategy` 二选一的原因）。

**判据设计：把默认出站做成黑洞、计量出站做成 freedom。**这样「客户端读到数据」就等价于「计量规则命中了」，不需要连 gRPC 去读 stats，测试没有额外依赖。目标域名 `meter.test` 由 `dns.hosts` 指到 `127.0.0.1`，正好落在 `geoip:private` 覆盖的范围里。

- [ ] **Step 1: 写测试**

新建 `web/service/meter_rule_e2e_test.go`：

```go
package service

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"a-ui/xray"
)

// TestMeterRuleShapeAgainstRealXray 把 spec §6.1 的交叉实验固化成回归测试。
//
// 守的是一件事：**计量规则不能屏蔽 IP 规则的第二遍**。xray 在 IPIfNonMatch
// 下走两遍规则（app/router/router.go:245-273），第二遍只在第一遍一条都没命中
// 时才发生。计量规则带 domain 条件，会参与并可能命中第一遍——一旦命中，第二遍
// 永远不会发生，模板自带的 geoip:private 与管理员所有 CIDR 规则对池内域名
// 静默失效。加上 ip: ["0.0.0.0/0","::/0"] 守卫可以修好它：守卫匹配任意 IP，
// 但要求目标已经有 IP，域名目标在第一遍没有。
//
// 判据：默认出站是黑洞、计量出站是 freedom，所以「读到数据」就等价于
// 「计量规则命中了」。目标 meter.test 由 dns.hosts 指到 127.0.0.1，正好落在
// geoip:private 覆盖的范围里。
func TestMeterRuleShapeAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("HELLO"))
			c.Close()
		}
	}()
	targetPort := target.Addr().(*net.TCPAddr).Port

	run := func(t *testing.T, strategy string, withPrivateRule, guarded bool) string {
		t.Helper()
		socksPort := freePort(t)

		meterRule := map[string]any{
			"type":        "field",
			"inboundTag":  []string{"in"},
			"domain":      []string{"domain:meter.test"},
			"outboundTag": "a-ui-meter-1-meter.test",
		}
		if guarded {
			meterRule["ip"] = []string{"0.0.0.0/0", "::/0"}
		}
		rules := []any{}
		if withPrivateRule {
			// 与 web/service/config.json 模板里那条一模一样。
			rules = append(rules, map[string]any{
				"type": "field", "ip": []string{"geoip:private"}, "outboundTag": "a-ui-block",
			})
		}
		rules = append(rules, meterRule)

		routing := map[string]any{"rules": rules}
		if strategy != "" {
			routing["domainStrategy"] = strategy
		}
		cfg := map[string]any{
			"log": map[string]any{"loglevel": "warning"},
			"dns": map[string]any{
				"hosts":   map[string]any{"meter.test": "127.0.0.1"},
				"servers": []any{"localhost"},
			},
			"inbounds": []any{map[string]any{
				"tag": "in", "listen": "127.0.0.1", "port": socksPort, "protocol": "socks",
				"settings": map[string]any{"auth": "noauth", "udp": false},
			}},
			"outbounds": []any{
				// 默认出站故意做成黑洞：这样「读到数据」就只可能来自计量出站。
				map[string]any{"tag": "a-ui-default", "protocol": "blackhole", "settings": map[string]any{}},
				map[string]any{"tag": "a-ui-block", "protocol": "blackhole", "settings": map[string]any{}},
				map[string]any{"tag": "a-ui-meter-1-meter.test", "protocol": "freedom",
					"settings": map[string]any{"domainStrategy": "UseIP"}},
			},
			"routing": routing,
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(xray.GetBinaryPath(), "run", "-c", cfgPath)
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 xray: %v", err)
		}
		defer func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()
		waitForPort(t, socksPort)

		return socksReadDomain(fmt.Sprintf("127.0.0.1:%d", socksPort), "meter.test", targetPort)
	}

	t.Run("两遍匹配下不带守卫会绕过私网封禁", func(t *testing.T) {
		// 危险本身。计量规则在第一遍命中，第二遍永远不发生，
		// geoip:private 对这个域名完全失效——流量照常送达。
		if got := run(t, "IPIfNonMatch", true, false); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO——这条用例复现的是危险本身，"+
				"它不成立说明整个前提变了，先回去核对 xray 的两遍匹配逻辑", got)
		}
	})

	t.Run("两遍匹配下带守卫则封禁生效", func(t *testing.T) {
		// 守卫让计量规则在第一遍必然不命中，第二遍 geoip:private 先命中，
		// 行为与「没有计量功能」时逐字节一致。
		if got := run(t, "IPIfNonMatch", true, true); got != "" {
			t.Errorf("读到 %q，期望空——带守卫时私网封禁必须照常生效，"+
				"否则计量会静默关掉管理员所有的 IP 段规则", got)
		}
	})

	t.Run("两遍匹配下带守卫不妨碍计量", func(t *testing.T) {
		// 没有别的规则挡路时，守卫形态在第二遍照常命中计量出站。
		if got := run(t, "IPIfNonMatch", false, true); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO——守卫不该让计量本身失效", got)
		}
	})

	t.Run("单遍匹配下守卫恒假", func(t *testing.T) {
		// 这条用例解释了形态为什么必须按 domainStrategy 二选一：AsIs 下
		// 没有第二遍，守卫永远拿不到 IP，计量规则永不命中，流量回落默认出站
		//（这里是黑洞，所以读不到数据）。
		if got := run(t, "", false, true); got != "" {
			t.Errorf("读到 %q，期望空——单遍匹配下守卫恒假，必须改用纯 domain 形态", got)
		}
	})

	t.Run("单遍匹配下纯形态照常计量", func(t *testing.T) {
		if got := run(t, "", false, false); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO", got)
		}
	})
}

// socksReadDomain 通过 socks5 以**域名**形式连到目标并读回内容，读不到就返回空串。
//
// 与 geo_e2e_test.go 里的 socksRead 的唯一区别是地址类型用 0x03（域名）而不是
// 0x01（IPv4）——计量规则匹配的正是域名，用 IP 字面量发起的话 domain 条件永不命中。
func socksReadDomain(proxyAddr, host string, port int) string {
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return ""
	}
	if _, err := c.Read(make([]byte, 2)); err != nil {
		return ""
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return ""
	}
	if _, err := c.Read(make([]byte, 32)); err != nil {
		return ""
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		return ""
	}
	return string(buf[:n])
}
```

- [ ] **Step 2: 运行测试**

Run: `go test ./web/service/ -run TestMeterRuleShapeAgainstRealXray -v`
Expected: 五个子用例全部 PASS。本机没有 `bin/xray-<GOOS>-<GOARCH>` 时整条测试 skip 并说明原因（`requireXrayBinary` 已经这么做）。

- [ ] **Step 3: 跑门禁**

Run: `make verify`
Expected: PASS

- [ ] **Step 4: 提交**

```bash
git status --porcelain
git add web/service/meter_rule_e2e_test.go
git commit -m "$(cat <<'EOF'
test(meter): 用真实 xray 钉住计量规则的形态

五个子用例覆盖两个方向：不带守卫时私网封禁被静默绕过（危险本身）、带守卫后
封禁重新生效（危险已挡住）、守卫不妨碍正常计量、以及守卫在单遍匹配下恒假
（这正是形态必须按 domainStrategy 二选一的原因）。

判据是「默认出站做成黑洞、计量出站做成 freedom」，于是「读到数据」等价于
「计量规则命中了」，不需要连 gRPC 读 stats。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## Task 12: 同步 `CLAUDE.md`

**Files:**
- Modify: `CLAUDE.md`

**Interfaces:** 无

改动了保留 tag 的语义、配置注入的规则顺序、定时任务清单，以及引入了一个新的已知偏差——按项目规范必须同步文档。四处插入，锚点文本逐字给出。

- [ ] **Step 1: 保留 tag 那一条**

在「域名分流管理 → 数据模型」里，找到以 `**\`a-ui-block\` 是保留 tag，任何用户可控的 tag 分配都必须排除它。**` 开头的那一段，把段末的
`**新增保留 tag 只改这一个函数**。` 替换为：

```
**新增保留 tag 只改这一个函数**——第二期加进来的计量出站前缀 `a-ui-meter-`
就是这么加的（`model.IsMeterTag`），分配端、生成端、校验端、导入端四处自动
继承，没有一处需要另外判断。
```

- [ ] **Step 2: 配置注入的不变量**

在「配置注入的四条不变量」一节，第 2 条（block 规则排在其余规则之前）那一大段之后、第 3 条之前，插入一段新的：

```markdown
**计量规则永远排在所有规则的最后，而且在两遍匹配模式下必须带 ip 守卫。**
第二期（`docs/superpowers/specs/2026-09-06-domain-traffic-attribution-design.md` §6）
给每个被计量的注册域名发一个独立出站 tag，配套一条 `domain:` 规则。规则排在
最后是为了「只计量本来会走默认出站的流量」；但**光排在最后是不够的**——开了
`ipRuleResolveDomain` 之后 xray 走两遍规则，计量规则带 `domain` 条件会在**第一遍**
命中，第二遍从此永远不会发生，于是模板自带的 `geoip:private → blocked` 与管理员
所有用 CIDR 表达的规则，对池内域名（恰恰是流量最大的那批）**全部静默失效**。

解法是给计量规则加 `"ip": ["0.0.0.0/0", "::/0"]`：它匹配任意 IP，但要求目标
**已经有 IP**——域名目标在第一遍没有，规则整条不命中；第二遍解析出 IP 之后守卫
恒真。判据取生成配置里 `routing.domainStrategy` 的**最终值**（`ipifnonmatch` /
`ipondemand` 时带守卫，其余不带），与 `infra/conf/router.go` 的 `getDomainStrategy`
用同一个 switch，无法识别的值一律按「不解析」处理——判错的两种后果不对称：
该带没带是上面那个安全问题，不该带却带了只是计量空转。真实 xray 上的五个交叉
用例固化在 `web/service/meter_rule_e2e_test.go`。

**这一条看上去违反了「绝不把 domain 与 ip 并进同一条规则（那是 AND）」，不要去
「修」它。**那条不变量约束的是管理员表达的规则（「这批域名**或**这批 IP」写成
一条会变成几乎永不命中的 AND）；这里的 AND 是刻意要的——「域名是 X **且** 目标
已经解析出 IP」，第二个合取项不是匹配条件，是一个遍次闸门。`buildRule` 生成
管理员规则时仍然严格拆成两条。
```

- [ ] **Step 3: 定时任务清单**

在「定时任务（`web/job/`，均注册在 `Server.startTask`）」的列表里，`TrafficResetJob` 那一行之后插入：

```markdown
- `MeterPoolJob`（1h）— 重算计量池（每个入站当前正在被计量的 Top-K 注册域名），
  只在池真的变了时才置重启标志。刻意不做延迟首次触发：池已经在库里，面板重启后
  生成期照常读得到。
```

- [ ] **Step 4: 已知偏差**

在「已知偏差与注意事项」一节末尾追加：

```markdown
- **退出计量池的 tag，它在 xray 里的 stats 计数器永远不会被回收。**
  `app/proxyman/outbound/outbound.go:131` 的 `RemoveHandler` 只从 handler 表里
  删对象，不注销计数器；`app/stats/command` 的 `StatsService` 也没有任何注销 RPC
  （`command.proto:85-92` 逐条核对过）。所以每个退过池的 `a-ui-meter-*` tag，它的
  两个计数器会留到 xray 进程退出为止，并且每次 `QueryStats` 都被返回一遍（值为 0）。
  好的一面是退池到下一次采集之间的残余字节不会丢；坏的一面是计数器集合只增不减。
  `MeterPoolService` 用**观测式**上限兜底：`RecordMetered` 数出「带计量前缀但不在
  当前池内」的条目数，超过 `meterStaleCounterLimit`(2000) 就冻结换池（已在池内的
  域名照常计量），xray 任何一次整进程重启都会清空计数器、自动解冻。观测式而不是
  记账式，是因为它天然跨重启自愈——记账式要面板去跟踪「上一次重启是什么时候」，
  而那个状态它其实拿不准。
- **`DomainStat` 同一行里的 `Count` 与 `Up`/`Down` 不是同一批连接的统计量。**
  `Count` 来自访问日志，记的是**连接建立**时刻；`Up`/`Down` 来自每 10 秒一次的
  出站计数器采样，记的是**流量发生**时刻。一条 10:59 建立、传到 11:30 的连接，
  它的 1 次计数落在 10 点桶，字节分落在 10 点和 11 点两个桶。所以一个域名完全
  可能出现 `Count=0` 而 `Up=5GB` 的行——这不是 bug，把两者强行对齐需要连接级的
  字节数，而 xray 不提供（`common/log/access.go` 的 `AccessMessage` 没有任何长度
  字段，而且这条日志在连接**建立**时就写出了）。
```

- [ ] **Step 5: 确认没有把别的段落改坏**

Run: `git diff CLAUDE.md | head -80`
Expected: 只有上面四处新增，没有任何既有段落被删改。

- [ ] **Step 6: 提交**

```bash
git status --porcelain
git add CLAUDE.md
git commit -m "$(cat <<'EOF'
docs: CLAUDE.md 同步第二期计量子系统的约束

四处：保留 tag 前缀多了 a-ui-meter-；配置注入多了「计量规则永远最后、两遍匹配
下必须带 ip 守卫」这条不变量（含「为什么它看上去违反 domain/ip 不能并列那条、
但不要去修」）；定时任务多了 MeterPoolJob；已知偏差多了「退池的 tag 计数器
永不回收」与「同一行里 Count 与 Up/Down 不是同一批连接」。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01KF6kjzJYfmFDg1j28N6CGg
EOF
)"
```

---

## 收尾检查（全部任务完成后）

- [ ] **完整门禁**：`make verify`
- [ ] **e2e 确实跑过**：`go test ./web/service/ -run 'E2E|AgainstRealXray' -v`，确认没有因为缺二进制而整片 skip。skip 了就说明这一轮没有验证过最关键的那条不变量，要在最终报告里写明。
- [ ] **越界检查**：`git status --porcelain` 里除了本计划列出的文件，只应剩下并行会话那 8 个未提交文件，且它们的内容与开工前一致（`git diff --stat` 对比）。
- [ ] **最终 diff 自查**：`git diff main...HEAD --stat`，确认没有调试残留、没有无关格式改动。
- [ ] **发版说明要点**（交给人类，不要自己发版）：升级后第一次配置变更会触发**一次整进程重启**（`policy` 段新增 `statsOutbound*`，没有运行时重载接口），全员会断线重连一次。这是一次性的。

---

## 计划自查记录

**spec §6 覆盖情况**（每一小节都能指到任务）：

| spec 小节 | 任务 |
|---|---|
| §6.0 实测数据 | 无需实现；数值作为 Task 3 常量注释的依据 |
| §6.1 ip 守卫 | Task 6（实现与单测）+ Task 11（真实 xray 回归） |
| §6.2.1 容量 | Task 3（`meterPoolCapacity`） |
| §6.2.2 排序与折算权重 | Task 3（`meterAvgBytesPerConn` / `buildMeterCandidates`） |
| §6.2.3 候选过滤 | Task 1（`IsRegistrable`）+ Task 3（过滤） |
| §6.2.4 三道闸门 | Task 3 |
| §6.2.5 死计数器 | Task 3（冻结）+ Task 8（观测） |
| §6.2.6 池落库 | Task 2 |
| §6.3 计量出站 | Task 1（tag 与保留 tag）+ Task 6（生成与深拷贝） |
| §6.4 计量规则 | Task 6 |
| §6.5 DNS 注入器 | Task 7 |
| §6.6 policy 注入 | Task 5 |
| §6.7 采集 | Task 8 |
| §6.8 覆盖度 | Task 9 |
| §6.9 只计量走默认出站 | Task 6（规则次序）+ Task 10（界面上如实说明这条边界） |
| §6.10 会发生几次重启 | Task 5（注释）+ Task 6（`TestPoolChangeIsHotApplicable`）+ 收尾的发版说明 |
| §6.11 回退到旧版本 | **无需代码**——旧二进制的 `TopDomains` 硬编码 `Metered: false`，`meter_domains` 表它压根不读。Task 9 的 `TestTopDomainsMeteredFollowsPool` 覆盖了同形的「池为空 → 隐藏字节列」这一支 |
| §7 前端 | Task 10 |
| §8 接口 | Task 9 |

**spec §11 测试清单里没有单独立项的一条：**「换池走热应用且不重启进程」原计划照
`xray_hot_reload_e2e_test.go` 的形状再写一条真实 xray 的 e2e。这里改成 Task 6 的
`TestPoolChangeIsHotApplicable`——直接对 `xray.ComputeHotDiff` 断言，不需要拉起
真实进程。理由：这条测试要守的性质是**面板算出来的 diff 是否只含出站增删与路由
替换**，那完全是 `ComputeHotDiff` 的判定，拉起真实 xray 只会把一个确定性判断变成
一条又慢又依赖二进制的用例；而「改 policy 必须重启」在 `xray/hot_diff_test.go:121`
已经有既成用例，不重复。

**执行顺序的依赖**：1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9 → 10 → 11 → 12。
Task 4 依赖 3；Task 6 依赖 1、2；Task 7 依赖 6；Task 8 依赖 1、2、3；Task 9 依赖 2；
Task 10 依赖 9；Task 11 只依赖 xray 二进制，可以提前跑。Task 3 里的
`StaleMeterCounters` / `SetStaleMeterCounters` 由 Task 3 定义、Task 8 写入，
两者之间没有循环依赖。
