# IP 目标纳入计量 实施计划（第三期 · 改动 A）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让纯 IP 目标（客户端直连 IP 字面量）能进计量池并被计量，把一台生产机上 0.014% 的上传归因率提到 90%+。

**Architecture:** 计量池的准入判定从「只收注册域名」放宽到「注册域名 + IP 字面量」；生成期对 IP 成员发 `ip` 条件的计量规则（而非 `domain`），且**不带** ip 守卫；域名规则排在 IP 规则之前。数据模型、池容量策略、tag 形态、排序换池逻辑一律不动。

**Tech Stack:** Go 1.27（`go.mod` 声明，CI 用 `go-version-file` 读同一个值）、GORM + SQLite（CGO 必须开）、xray-core `v1.260327.1-0.20260728075948-5ca6f4b7d4dc`。无前端构建流程。

**Spec:** `docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md`（本计划只实现其中的**改动 A**，§4 全节；改动 B 另出计划）

## Global Constraints

- **不动数据模型。** `DomainStat.Domain` 本来就「IP 字面量原样」存（`database/model/domain_stat.go` 该字段注释），`MeterDomain` 的唯一索引 `(inbound_id, domain)` 对 IP 同样成立。spec §6.1 那个加 `Kind` 列 + 扩索引的方案属于改动 B，本期**不做**——顺带避开了 GORM `AutoMigrate` 不修改已存在索引那个静默失败。「类型」在查询时用 `net.ParseIP` 推导。
- **出站数量不许增加。** IP 目标顶替本来会被选中的小流量域名，占用同一批槽位。`meterOutboundBudget = 200`、`meterPoolCapMax = 60` 一个字节都不改。
- **生成必须逐字节确定。** `Config.Equals` 对 `OutboundConfigs`/`RouterConfig` 按字节比较，顺序一抖动就会让那个 10 秒的消费任务不停重启 xray。域名规则全部排在 IP 规则之前，各自内部保持 `Pool()` 的 `(inboundId asc, domain asc)` 顺序。
- **IP 规则不带 ip 守卫。** 守卫（`"ip":["0.0.0.0/0","::/0"]`）是给**域名**规则躲开第一遍匹配用的（spec §4.2）；IP 规则本身就是 IP 条件，加了是同义反复。域名规则的守卫行为一个字节不改。
- **CIDR 掩码必须补齐**（`/32`、`/128`）。`infra/conf` 接受裸 IP，但两种写法产生不同的配置字节，补齐才能让生成逐字节确定。
- 测试的工作目录：`web/service` 的 `TestMain` 会 `os.Chdir` 到仓库根（`xray.GetBinaryPath()` 返回相对路径）。这是进程级副作用，新增测试不要依赖包内相对路径。
- 提交信息用仓库既有风格：`fix(scope): 中文描述` / `feat(scope): 中文描述`，正文说明「为什么」。每个 commit 末尾附：
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
  ```
- **提交一律 `git add <具体路径>`，绝不用 `git add -A`** —— 这个仓库常有另一个会话并行开发。
- 全部完成后跑 `make verify`（vet + test + build），它是提交前门禁。

## File Structure

| 文件 | 责任 | 本期动作 |
|---|---|---|
| `util/domain/domain.go` | 目标归并与准入判定（纯函数） | 新增 `IsMeterable` |
| `util/domain/domain_test.go` | 上者的单测 | 新增用例 |
| `web/service/meter_pool.go` | 池的候选、排序、换池 | 一处准入判定换函数 |
| `web/service/routing_inject.go` | 配置生成（出站 + 规则） | 一处过滤换函数；`buildMeterRules` 分两类 |
| `web/service/domain_stat.go` | 榜单查询 | `TopDomainRow` 加 `Kind`（推导值） |
| `web/html/xui/access_log_modal.html` | 榜单 UI | 加「类型」列 |
| `web/service/routing_inject_meter_test.go` | 注入器的计量测试 | 新增用例 |
| `web/service/meter_pool_test.go` | 池的测试 | 新增用例 |
| `web/service/meter_rule_e2e_test.go` | 真实 xray 的 e2e | 新增用例 |

---

### Task 1: `domain.IsMeterable` 准入判定

**Files:**
- Modify: `util/domain/domain.go`
- Test: `util/domain/domain_test.go`

**Interfaces:**
- Consumes: 无
- Produces: `func IsMeterable(d string) bool` —— 供 Task 2、Task 3 使用

- [ ] **Step 1: 写失败测试**

追加到 `util/domain/domain_test.go`：

```go
// IsMeterable 是计量池的准入闸门，比 IsRegistrable 多放行一类：IP 字面量。
// 第三期给 IP 目标发 ip 条件的计量规则（设计 §4.1），所以它们必须能进池。
func TestIsMeterable(t *testing.T) {
	yes := []string{
		"doubleclick.net",
		"example.co.uk",
		"1.2.3.4",         // IPv4 字面量：第三期新放行
		"72.235.209.83",   // 触发本期立项的那个目标
		"2001:db8::1",     // IPv6 字面量
		"::1",             // IPv6 简写
	}
	for _, d := range yes {
		if !IsMeterable(d) {
			t.Errorf("IsMeterable(%q) = false，期望 true", d)
		}
	}
	no := []string{
		"",                // 空
		"com",             // 公共后缀本身：domain:com 会命中全部 .com
		"co.uk",           // 多级公共后缀本身
		"localhost",       // 不含点的主机名
		"www.example.com", // 子域名不是注册域名，池里只放归并后的结果
		"example.com.",    // 带末尾点：Registrable 已经剥过
		"EXAMPLE.COM",     // 大写：domain:EXAMPLE.COM 是永不命中的哑规则
	}
	for _, d := range no {
		if IsMeterable(d) {
			t.Errorf("IsMeterable(%q) = true，期望 false", d)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./util/domain/ -run TestIsMeterable -v
```

预期：编译失败，`undefined: IsMeterable`

- [ ] **Step 3: 实现**

追加到 `util/domain/domain.go`（放在 `IsRegistrable` 之后）：

```go
// IsMeterable 判断一个归并后的目标能否进计量池。
//
// 比 IsRegistrable 多放行一类：IP 字面量。第二期把它们挡在外面是因为当时
// 计量规则只有 domain: 一种形态，而 domain 条件对 IP 目标永不命中、白占一个
// 槽位；第三期给 IP 成员发 ip 条件的规则（设计 §4.2），它们因此有了资格。
// 这不是一个可有可无的放宽：立项时那台生产机上，某入站 24 小时 2.53 GB 的
// 上传里绝大部分打向一个没有域名的目标，域名池再大也抓不到它。
//
// 仍然拒绝公共后缀本身（"com"）与不含点的主机名（"localhost"）：前者会生成
// domain:com 这种命中全部 .com 的规则，把该入站几乎全部流量吸进一个计量出站，
// 榜单从此只有一行。
//
// 与 IsRegistrable 并列而不是改它的语义：那个函数回答「是不是注册域名」，
// 池以外的地方也在用（filterMeterPool 之外还有将来的调用方），改它会在
// 看不见的地方产生副作用。
func IsMeterable(d string) bool {
	if d == "" {
		return false
	}
	// 不做归一化，只判定——与 IsRegistrable 同规。Registrable 已经在
	// 上游把大小写和末尾点处理过了。
	if net.ParseIP(d) != nil {
		return true
	}
	return IsRegistrable(d)
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./util/domain/ -run TestIsMeterable -v
```

预期：PASS

- [ ] **Step 5: 提交**

```bash
git add util/domain/domain.go util/domain/domain_test.go
git commit -m "$(cat <<'EOF'
feat(domain): 新增 IsMeterable，让 IP 字面量有资格进计量池

第二期把 IP 挡在池外，是因为当时计量规则只有 domain: 一种形态，而
domain 条件对 IP 目标永不命中、白占槽位。第三期要给 IP 成员发 ip 条件
的规则，它们因此有了资格。

与 IsRegistrable 并列而不是改它：那个函数回答「是不是注册域名」，
池以外也在用，改语义会在看不见的地方产生副作用。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 2: 池的候选准入放开 IP

**Files:**
- Modify: `web/service/meter_pool.go`（`buildMeterCandidates` 内一处判定）
- Test: `web/service/meter_pool_test.go`

**Interfaces:**
- Consumes: `domain.IsMeterable`（Task 1）
- Produces: 池表中可能出现 `Domain` 为 IP 字面量的行，供 Task 3、Task 4 消费

- [ ] **Step 1: 写失败测试**

追加到 `web/service/meter_pool_test.go`。参照同文件 `TestRecomputeColdStartRanksByCount` 的建库与调用方式（照抄它的 setup，不要自创）：

```go
// 冷启动时按访问次数选池，IP 字面量必须与域名一起参与竞争——立项时那台
// 生产机上，2.53 GB 上传里绝大部分打向一个没有域名的目标，它进池只需要
// 一个槽位，但第二期的准入判定让它连参选资格都没有。
func TestRecomputeAdmitsIPLiterals(t *testing.T) {
	setupDB(t)
	svc := &MeterPoolService{}
	now := time.Now()

	// 访问次数：IP 远多于域名，冷启动应当优先选中 IP。
	seedDomainStats(t, 1, now, map[string]int64{
		"72.235.209.83": 2495,
		"acspubs.org":   131,
	})

	if err := svc.Recompute(now); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	pool, err := svc.Pool(now)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	got := make(map[string]bool, len(pool))
	for _, e := range pool {
		got[e.Domain] = true
	}
	if !got["72.235.209.83"] {
		t.Errorf("IP 字面量没能进池，池内容：%v", pool)
	}
	if !got["acspubs.org"] {
		t.Errorf("域名不应被挤掉，池内容：%v", pool)
	}
}
```

> **实现者注意**：`seedDomainStats` 是这里假定的辅助函数名。先在
> `meter_pool_test.go` 里找现成的种子辅助（`TestRecomputeColdStartRanksByCount`
> 用的那个），**用它实际的名字和签名**，不要新写一个。若确实没有可复用的，
> 才在测试文件内新增一个最小辅助。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run TestRecomputeAdmitsIPLiterals -v
```

预期：FAIL，报「IP 字面量没能进池」——因为 `buildMeterCandidates` 里的
`domain.IsRegistrable` 把它过滤掉了。

- [ ] **Step 3: 实现**

`web/service/meter_pool.go`，`buildMeterCandidates` 内（约 166 行）：

```go
		// 收注册域名与 IP 字面量两类：前者生成 domain: 规则，后者生成 ip 规则
		//（设计 §4.2）。仍然拒绝 domain:com 那种命中全部 .com 的公共后缀，
		// 它会把该入站几乎全部流量吸进一个计量出站，榜单从此只有一行。
		if !domain.IsMeterable(d) {
			continue
		}
```

（把原来的 `if !domain.IsRegistrable(d)` 连同它上方那段注释一起替换。）

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestRecomputeAdmitsIPLiterals|TestRecompute|TestPool' -v
```

预期：新用例 PASS，且同文件既有的 `TestRecomputeColdStartRanksByCount`、
`TestRecomputeIsIdempotent`、`TestRecomputeRetiresZeroByteDomainAndCoolsItDown`
等全部仍然 PASS（准入放宽不该改变它们的行为）。

- [ ] **Step 5: 提交**

```bash
git add web/service/meter_pool.go web/service/meter_pool_test.go
git commit -m "$(cat <<'EOF'
feat(meter): 池的候选准入放开 IP 字面量

生产机实测：某入站 24 小时 2.53 GB 上传里，绝大部分打向 72.235.209.83
这个没有域名的目标（2171 次直连、全天不间断）。它进池只需要一个槽位，
但第二期的准入判定让它连参选资格都没有——所以覆盖率是 0.014%，而扩池
到 600 也只能抬到 0.02%（池不是不够大，是装错了东西）。

排序、换池、冷却、死计数器上限一律不动，IP 作为普通候选参与同一套竞争。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 3: 生成期的二次过滤放开 IP

**Files:**
- Modify: `web/service/routing_inject.go`（`filterMeterPool` 内一处判定，约 198 行）
- Test: `web/service/routing_inject_meter_test.go`

**Interfaces:**
- Consumes: `domain.IsMeterable`（Task 1）、池中的 IP 行（Task 2）
- Produces: IP 池行能活着走到 `appendMeterOutbounds` 与 `buildMeterRules`

- [ ] **Step 1: 写失败测试**

追加到 `web/service/routing_inject_meter_test.go`。参照同文件
`TestInjectSkipsMeterRowsOfDisabledInbounds` 的建库与注入调用方式：

```go
// 生成期的二次过滤必须与池的准入判定保持同一套标准。两处一旦漂移，
// IP 行会进池、占着槽位、却在生成期被静默丢掉——池表看着满，配置里没有它，
// 而且没有任何一层会报错。
func TestInjectKeepsIPLiteralPoolRows(t *testing.T) {
	setupDB(t)
	seedEnabledInbound(t, 1, 2210)
	seedMeterPool(t, 1, "72.235.209.83")

	cfg := injectAndDecode(t)

	var found bool
	for _, ob := range cfg.Outbounds {
		if ob.Tag == model.MeterTag(1, "72.235.209.83") {
			found = true
		}
	}
	if !found {
		t.Errorf("IP 池行在生成期被丢掉了，出站列表：%v", cfg.Outbounds)
	}
}
```

> **实现者注意**：`seedEnabledInbound` / `seedMeterPool` / `injectAndDecode`
> 是这里假定的辅助名。**先在该测试文件里找现成的**（那几个既有用例一定有
> 等价的辅助），用它们实际的名字与签名。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run TestInjectKeepsIPLiteralPoolRows -v
```

预期：FAIL，报「IP 池行在生成期被丢掉了」

- [ ] **Step 3: 实现**

`web/service/routing_inject.go`，`filterMeterPool` 内（约 198 行），把
`if !domain.IsRegistrable(e.Domain)` 换成：

```go
		// 与 buildMeterCandidates 的准入判定必须是同一个函数：两处漂移会让
		// IP 行进得了池、占得住槽位，却在生成期被静默丢掉，而池表看着是满的。
		if !domain.IsMeterable(e.Domain) {
			continue
		}
```

保留该判定原有的上下文注释（关于 publicsuffix 表更新导致域名不再是注册域名
的那段仍然成立），只替换函数名与紧邻的说明。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestInject' -v
```

预期：新用例 PASS，同文件既有的 9 个 `TestInject*` 全部仍然 PASS。

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_inject.go web/service/routing_inject_meter_test.go
git commit -m "$(cat <<'EOF'
fix(meter): 生成期的二次过滤与池准入用同一个判定

两处漂移的后果是静默的：IP 行进得了池、占得住槽位，却在生成期被丢掉，
池表看着是满的，配置里没有它，没有任何一层会报错。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 4: IP 成员生成 `ip` 条件的计量规则

这是本计划的核心任务。

**Files:**
- Modify: `web/service/routing_inject.go`（`buildMeterRules`，约 294-313 行）
- Test: `web/service/routing_inject_meter_test.go`

**Interfaces:**
- Consumes: 过滤后的池（Task 3）
- Produces: `buildMeterRules` 的返回值形态变化 —— 域名规则在前、IP 规则在后；
  IP 规则形如 `{"type":"field","inboundTag":[tag],"ip":["1.2.3.4/32"],"outboundTag":...}`，**不带** ip 守卫

- [ ] **Step 1: 写失败测试**

追加到 `web/service/routing_inject_meter_test.go`：

```go
// IP 成员发 ip 条件的规则，且不带 ip 守卫。
//
// 守卫（"ip":["0.0.0.0/0","::/0"]）是给**域名**规则用的：它让计量规则在
// 第一遍匹配必然不命中（域名目标此时还没有 IP），从而不屏蔽掉模板里
// geoip:private → blocked 这类只能在第二遍命中的 CIDR 规则。IP 规则本身
// 就是 IP 条件，加守卫是同义反复。
func TestInjectIPMeterRuleUsesIPConditionWithoutGuard(t *testing.T) {
	setupDB(t)
	seedEnabledInbound(t, 1, 2210)
	seedMeterPool(t, 1, "72.235.209.83")
	setIPRuleResolveDomain(t, 1) // 打开两遍匹配，域名规则此时会带守卫

	cfg := injectAndDecode(t)

	rule := findRuleByOutboundTag(t, cfg, model.MeterTag(1, "72.235.209.83"))
	if got := rule["domain"]; got != nil {
		t.Errorf("IP 计量规则不该有 domain 条件，得到 %v", got)
	}
	ips, _ := rule["ip"].([]any)
	if len(ips) != 1 || ips[0] != "72.235.209.83/32" {
		t.Errorf("ip 条件 = %v，期望 [\"72.235.209.83/32\"]（掩码必须补齐，"+
			"否则生成结果不逐字节确定）", ips)
	}
}

// IPv6 补 /128。
func TestInjectIPv6MeterRuleCarries128Mask(t *testing.T) {
	setupDB(t)
	seedEnabledInbound(t, 1, 2210)
	seedMeterPool(t, 1, "2001:db8::1")

	cfg := injectAndDecode(t)

	rule := findRuleByOutboundTag(t, cfg, model.MeterTag(1, "2001:db8::1"))
	ips, _ := rule["ip"].([]any)
	if len(ips) != 1 || ips[0] != "2001:db8::1/128" {
		t.Errorf("ip 条件 = %v，期望 [\"2001:db8::1/128\"]", ips)
	}
}

// 域名规则必须全部排在 IP 规则之前。
//
// 两遍匹配下，域名目标在第一遍先命中域名规则、归到域名行；只有归不到
// 域名的才落到 IP 行（设计 §4.3）。顺序反了会让 IP 行吸走本该归到域名的
// 流量，而两边的数字都还是「对的」，没有任何一层会报错。
func TestInjectMeterDomainRulesComeBeforeIPRules(t *testing.T) {
	setupDB(t)
	seedEnabledInbound(t, 1, 2210)
	seedMeterPool(t, 1, "72.235.209.83") // 排序上 "7..." 在 "acspubs.org" 之后
	seedMeterPool(t, 1, "acspubs.org")

	cfg := injectAndDecode(t)

	domainIdx := indexOfRuleWithOutboundTag(t, cfg, model.MeterTag(1, "acspubs.org"))
	ipIdx := indexOfRuleWithOutboundTag(t, cfg, model.MeterTag(1, "72.235.209.83"))
	if domainIdx > ipIdx {
		t.Errorf("域名规则(下标 %d)排在了 IP 规则(下标 %d)之后", domainIdx, ipIdx)
	}
}
```

> **实现者注意**：`setIPRuleResolveDomain` / `findRuleByOutboundTag` /
> `indexOfRuleWithOutboundTag` 是假定的辅助名。前者在
> `TestInjectMeterRulesCarryIPGuardWhenCoreResolvesDomains` 里一定有等价物；
> 后两个若没有，在测试文件内新增最小实现（找不到就 `t.Fatalf`，不要返回零值）。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestInjectIPMeterRule|TestInjectIPv6MeterRule|TestInjectMeterDomainRulesComeBefore' -v
```

预期：三条全 FAIL —— 当前实现给所有池成员一律发 `domain:` 规则。

- [ ] **Step 3: 实现**

`web/service/routing_inject.go`，把 `buildMeterRules` 整体替换为：

```go
// buildMeterRules 生成计量规则。调用方必须把它们追加在所有其它规则之后。
//
// 返回值里**域名规则全部排在 IP 规则之前**（设计 §4.3）：两遍匹配下，
// 域名目标在第一遍先命中域名规则、归到域名行，只有归不到域名的才落到 IP 行。
// 顺序反了会让 IP 行吸走本该归到域名的流量，而两边的数字看上去都还是对的。
//
// 域名规则这里刻意把 domain 与 ip 并进同一条，看上去违反了「绝不把两类条件
// 并进同一条（那是 AND）」那条不变量——必须解释清楚，否则将来一定会有人来
// 「修」它。那条不变量约束的是**管理员表达的**规则：管理员说「这批域名**或**
// 这批 IP 走 B」，写成一条就变成 AND、几乎永不命中。这里的 AND 是刻意要的
// ——「域名是 X **且** 目标已经解析出 IP」，第二个合取项不是匹配条件，
// 是一个遍次闸门。buildRule 生成管理员规则时仍然严格拆成两条。
//
// IP 规则则不带守卫：守卫的作用就是让规则在第一遍必然不命中，而 IP 规则
// 本身就是 IP 条件、第一遍对 IP 字面量目标直接命中——那正是要的行为。
func buildMeterRules(pool []MeterEntry, inboundTagById map[int]string, guard bool) []any {
	domainRules := make([]any, 0, len(pool))
	ipRules := make([]any, 0, len(pool))
	for _, e := range pool {
		// pool 已由 filterMeterPool 过滤过，这里必然取得到。
		tag := inboundTagById[e.InboundId]
		outboundTag := model.MeterTag(e.InboundId, e.Domain)

		if cidr, ok := meterIPCondition(e.Domain); ok {
			ipRules = append(ipRules, map[string]any{
				"type":        "field",
				"inboundTag":  []string{tag},
				"ip":          []string{cidr},
				"outboundTag": outboundTag,
			})
			continue
		}

		rule := map[string]any{
			"type":       "field",
			"inboundTag": []string{tag},
			// 一律带显式 domain: 前缀。含点的裸串在 xray 里是子串匹配
			//（infra/conf/router.go:175 的 defaultType 是 Domain_Substr），
			// doubleclick.net 会命中 notdoubleclick.net.evil。
			"domain":      []string{"domain:" + e.Domain},
			"outboundTag": outboundTag,
		}
		if guard {
			rule["ip"] = []string{"0.0.0.0/0", "::/0"}
		}
		domainRules = append(domainRules, rule)
	}
	return append(domainRules, ipRules...)
}

// meterIPCondition 把池成员翻译成 ip 条件里的 CIDR，第二个返回值说明它
// 是不是 IP 字面量。
//
// 掩码必须补齐：infra/conf 两种写法都收，但它们产生不同的配置字节，
// 而 Config.Equals 对 RouterConfig 是逐字节比较的——不补齐就等于把
// 「生成逐字节确定」这条不变量交给上游的实现细节去保证。
func meterIPCondition(d string) (string, bool) {
	ip := net.ParseIP(d)
	if ip == nil {
		return "", false
	}
	if ip.To4() != nil {
		return d + "/32", true
	}
	return d + "/128", true
}
```

`routing_inject.go` 的 import 里加 `"net"`（若尚未引入）。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestInject' -v
```

预期：三条新用例 PASS；既有的
`TestInjectMeterRulesComeLastWithoutGuardByDefault`、
`TestInjectMeterRulesCarryIPGuardWhenCoreResolvesDomains`、
`TestInjectMeterIsByteStable` 全部仍然 PASS（纯域名池的生成结果必须逐字节不变）。

若 `TestInjectMeterIsByteStable` 挂了，说明纯域名场景下的输出被改动了 ——
那是回归，不是预期，**不要改测试**，回去看实现。

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_inject.go web/service/routing_inject_meter_test.go
git commit -m "$(cat <<'EOF'
feat(meter): IP 池成员发 ip 条件的计量规则，域名规则排在其前

domain 条件对 IP 字面量目标永不命中，所以第二期的 IP 目标即使进了池
也拿不到任何字节。改成按成员形态二选一：IP 发 ip 条件、域名维持原样。

IP 规则不带 ip 守卫——守卫的作用是让规则在第一遍匹配必然不命中（域名
目标此时还没有 IP），IP 规则本身就是 IP 条件，加了是同义反复。

顺序固定为「域名规则在前、IP 规则在后」：两遍匹配下域名目标在第一遍
先命中域名规则，只有归不到域名的才落到 IP 行。反过来会让 IP 行吸走本该
归到域名的流量，而两边的数字看上去都还是对的。

CIDR 掩码显式补齐（/32、/128）：infra/conf 两种写法都收，但产生不同的
配置字节，而 Config.Equals 是逐字节比较的。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 5: 榜单区分域名与 IP

**Files:**
- Modify: `web/service/domain_stat.go`（`TopDomainRow` 加字段 + 填值）
- Modify: `web/html/xui/access_log_modal.html`（加一列）
- Test: `web/service/domain_stat_top_test.go`

**Interfaces:**
- Consumes: 无（`Domain` 列里 IP 早就存着）
- Produces: `TopDomainRow.Kind` 字段，取值 `"domain"` 或 `"ip"`，前端据此渲染标签

- [ ] **Step 1: 写失败测试**

追加到 `web/service/domain_stat_top_test.go`：

```go
// 榜单要能区分域名与 IP。类型是推导值不是存储值——DomainStat.Domain 从第一期
// 起就「IP 字面量原样」存，加一列 kind 只会多一处需要与推导保持一致的真相源。
func TestTopDomainsMarksIPLiterals(t *testing.T) {
	setupDB(t)
	now := time.Now()
	seedDomainStats(t, 1, now, map[string]int64{
		"72.235.209.83": 2495,
		"acspubs.org":   131,
	})

	got, err := (&DomainStatService{}).TopDomains(1, TopRange24h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	kinds := make(map[string]string, len(got.List))
	for _, row := range got.List {
		kinds[row.Domain] = row.Kind
	}
	if kinds["72.235.209.83"] != "ip" {
		t.Errorf("IP 行的 Kind = %q，期望 \"ip\"", kinds["72.235.209.83"])
	}
	if kinds["acspubs.org"] != "domain" {
		t.Errorf("域名行的 Kind = %q，期望 \"domain\"", kinds["acspubs.org"])
	}
}
```

> **实现者注意**：`setupDB` / `seedDomainStats` 用该测试文件里现成的。
> `TopDomains` 需要入站存在（它会先 `GetInbound` 校验），照抄同文件既有用例的建库方式。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run TestTopDomainsMarksIPLiterals -v
```

预期：编译失败，`row.Kind undefined`

- [ ] **Step 3: 实现**

`web/service/domain_stat.go`，`TopDomainRow` 加字段：

```go
type TopDomainRow struct {
	Domain string `json:"domain"`
	// Kind 是 "domain" 或 "ip"，由 Domain 的形态推导，不落库。
	//
	// 不加数据库列：DomainStat.Domain 从第一期起就「IP 字面量原样」存，
	// 形态本身已经是完备的判据；加一列只会多出一处需要与推导保持一致的
	// 真相源，而它们一旦漂移，界面上的类型标签会和实际生成的规则形态对不上。
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}
```

在 `TopDomains` 里 `result.List = rows` 之前填值：

```go
	if rows != nil {
		for i := range rows {
			rows[i].Kind = topDomainKind(rows[i].Domain)
		}
		result.List = rows
	}
```

并加辅助函数：

```go
// topDomainKind 由目标的形态推导它的类型，与 buildMeterRules 选择规则形态
// 用的是同一个判据（net.ParseIP），两处因此不会漂移。
func topDomainKind(d string) string {
	if net.ParseIP(d) != nil {
		return "ip"
	}
	return "domain"
}
```

`domain_stat.go` 的 import 里加 `"net"`（若尚未引入）。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestTopDomains' -v
```

预期：新用例 PASS，同文件既有用例全部 PASS。

- [ ] **Step 5: 前端加一列**

`web/html/xui/access_log_modal.html`，在榜单的列定义里（`domain` 那列之前）插入：

```javascript
{
    title: "类型", align: 'center', width: 64,
    scopedSlots: { customRender: 'topKind' },
},
```

并在表格模板里加对应的 slot：

```html
<template slot="topKind" slot-scope="text, row">
    <a-tag :color="row.kind === 'ip' ? 'orange' : 'blue'">
        [[ row.kind === 'ip' ? 'IP' : '域名' ]]
    </a-tag>
</template>
```

> 注意模板用 `[[ ]]` 作为 Vue 插值分隔符（避开 Go 模板的 `{{ }}`）。
> 弹窗必须留在它自己的 Vue 根元素内 —— 写到根元素之外是完全静默的死代码。

- [ ] **Step 6: 验证模板仍能解析**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

预期：PASS。`web.go` 的 `getHtmlTemplate` 会**吞掉** `ParseFS` 错误，
所以光靠 `go build` 发现不了模板语法错误，这两条测试是唯一的防线。

- [ ] **Step 7: 提交**

```bash
git add web/service/domain_stat.go web/service/domain_stat_top_test.go web/html/xui/access_log_modal.html
git commit -m "$(cat <<'EOF'
feat(meter): 榜单区分域名与 IP 两类目标

类型是推导值不是存储值：DomainStat.Domain 从第一期起就「IP 字面量原样」
存，形态本身已经是完备的判据。加一列 kind 只会多出一处需要与推导保持
一致的真相源，两者一旦漂移，界面上的类型标签会和实际生成的规则形态对不上。

推导用的 net.ParseIP 与 buildMeterRules 选规则形态用的是同一个判据。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 6: 差额分解，让账「平」

spec §7.1。改动 A 之后精确值有了，但总用量减去已归因仍有一段差额；现在它无声消失在
一句「约 0% 的流量已归因」里，那句话除了让人以为系统坏了之外没有任何信息量。
本任务给每一块差额一个名字。

**Files:**
- Modify: `web/service/accesslog.go`（新增 `CountByRoute`）
- Modify: `web/service/domain_stat.go`（`TopDomainResult` 加 `Breakdown`）
- Modify: `web/html/xui/access_log_modal.html`（榜单下方渲染分解）
- Test: `web/service/domain_stat_top_test.go`

**Interfaces:**
- Consumes: `model.BlockOutboundTag`（现有常量 `"a-ui-block"`）
- Produces: `TopDomainResult.Breakdown *TopDomainBreakdown`，字段见下

- [ ] **Step 1: 写失败测试**

追加到 `web/service/domain_stat_top_test.go`：

```go
// 差额分解必须把总用量拆完：已归因 + 协议开销(估) + 未归因 = 总量。
// 未归因是余项、用减法得出，它天然吸收全部估算误差——这正是它作为
// 最后一项的意义，把它做成「精确测量值」反而是假的。
func TestTopDomainsBreakdownAccountsForEveryByte(t *testing.T) {
	setupDB(t)
	now := time.Now()
	seedTrafficBucket(t, 1, now, 1000, 0)          // 入站计数器：总量 1000
	seedDomainStatBytes(t, 1, now, "acspubs.org", 600, 0) // 已归因 600

	got, err := (&DomainStatService{}).TopDomains(1, TopRange24h, TopOrderUp, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	b := got.Breakdown
	if b == nil {
		t.Fatal("Breakdown 为 nil")
	}
	if b.TotalBytes != 1000 || b.AttributedBytes != 600 {
		t.Fatalf("总量/已归因 = %d/%d，期望 1000/600", b.TotalBytes, b.AttributedBytes)
	}
	if sum := b.AttributedBytes + b.OverheadBytes + b.UnattributedBytes; sum != b.TotalBytes {
		t.Errorf("三项之和 %d != 总量 %d，差额没被拆完", sum, b.TotalBytes)
	}
}

// 已归因超过总量时（口径差的方向并不固定），未归因必须钳到 0 而不是负数。
// 显示一个负的「未归因」会让整块数据当场失去可信度。
func TestTopDomainsBreakdownClampsNegativeRemainder(t *testing.T) {
	setupDB(t)
	now := time.Now()
	seedTrafficBucket(t, 1, now, 100, 0)
	seedDomainStatBytes(t, 1, now, "acspubs.org", 990, 0) // 已归因 > 总量

	got, err := (&DomainStatService{}).TopDomains(1, TopRange24h, TopOrderUp, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Breakdown.UnattributedBytes < 0 {
		t.Errorf("未归因 = %d，必须钳到 0", got.Breakdown.UnattributedBytes)
	}
}
```

> **实现者注意**：`seedTrafficBucket` / `seedDomainStatBytes` 用该测试文件或
> `domain_stat_test.go` 里现成的辅助（`coverage` 的既有用例一定有等价物），
> 用它们实际的名字与签名。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestTopDomainsBreakdown' -v
```

预期：编译失败，`got.Breakdown undefined`

- [ ] **Step 3: 实现服务端**

`web/service/accesslog.go` 新增：

```go
// CountByRoute 统计某入站在 since（Unix 秒）之后、经由某个出站的连接数。
//
// 给差额分解里「被封禁丢弃」那一行用。只能给出连接数：blackhole 不调用
// Dial，出站计数器从未被装上（proxy/blackhole/blackhole.go 的 Process
// 对 TCP 直接 common.Interrupt(link.Reader)），所以那部分字节永远拿不到。
// 它们确实发生过、也确实计在入站计数器里，只是无法单独摘出来。
//
// 注意 AccessLog.Time 是**毫秒**，而调用方给的 since 是秒（与 DomainStat
// 的 BucketStart 同单位），这里负责换算——两处单位不一致是这张表的老坑。
func (s *AccessLogService) CountByRoute(inboundId int, route string, since int64) (int64, error) {
	db := database.GetAccessLogDB()
	if db == nil {
		return 0, nil
	}
	var n int64
	err := db.Model(&model.AccessLog{}).
		Where("inbound_id = ? and route = ? and time >= ?", inboundId, route, since*1000).
		Count(&n).Error
	return n, err
}
```

`web/service/domain_stat.go` 新增类型与常量：

```go
// meterOverheadRatio 是协议封装开销的估算比例。
//
// 入站计数器量的是 VMess+WS+TLS 封装后的加密流，出站计数器量的是解封装后
// 的明文流，两者结构性地差 3~8%。取中值 5%。
//
// 做成常量而不是设置项：新增设置项要同步改 5 处（漏掉 models.js 那处会让
// 整个保存配置接口失败），而这个数只影响一行展示文字，不值得那个代价。
// 不同协议的封装开销差别不小（vless+vision+reality 与 vmess+ws+tls 不是
// 一个量级），所以 UI 上必须标明它是估算。
const meterOverheadRatio = 0.05

// TopDomainBreakdown 把总用量拆成有名字的几块，而不是让差额无声消失在
// 一句「约 0% 已归因」里。
//
// 只有 TotalBytes 与 AttributedBytes 是精确值（都直接来自计数器）；
// OverheadBytes 是按比例估的，UnattributedBytes 是减法余项。前端必须在
// 视觉上把估算值与精确值分开，否则整份数据的可信度会被那个估算拖下水。
type TopDomainBreakdown struct {
	TotalBytes      int64 `json:"totalBytes"`      // 入站计数器，精确
	AttributedBytes int64 `json:"attributedBytes"` // 计量出站合计，精确
	BlockedConns    int64 `json:"blockedConns"`    // 被封禁的连接数；字节不可得
	OverheadBytes   int64 `json:"overheadBytes"`   // 协议封装开销，估算
	UnattributedBytes int64 `json:"unattributedBytes"` // 余项，减法得出
}
```

在 `TopDomainResult` 里加字段：

```go
	// Breakdown 在 Metered 为 false 时为 nil（没有字节数就无从分解）。
	Breakdown *TopDomainBreakdown `json:"breakdown"`
```

在 `TopDomains` 里，紧接 `result.Coverage = coverage` 之后：

```go
		breakdown, err := s.breakdown(db, g, inboundId, since, coverage)
		if err != nil {
			return nil, err
		}
		result.Breakdown = breakdown
```

并新增方法：

```go
// breakdown 由 coverage 已经算好的两个精确值再拆出估算项与余项，
// 不重新查库——两处独立取数会在并发写入下给出对不上的两组数字。
func (s *DomainStatService) breakdown(
	db *gorm.DB, g model.TrafficGranularity, inboundId int, since int64,
	coverage *TopDomainCoverage,
) (*TopDomainBreakdown, error) {
	out := &TopDomainBreakdown{
		TotalBytes:      coverage.TotalBytes,
		AttributedBytes: coverage.MeteredBytes,
	}
	// 被封禁的连接数取不到不算失败：访问日志是独立库，它不可用时
	// 分解的其余部分仍然成立，显示一个 0 比整块不显示要好。
	if n, err := (&AccessLogService{}).CountByRoute(inboundId, model.BlockOutboundTag, since); err == nil {
		out.BlockedConns = n
	} else {
		logger.Warning("差额分解取不到封禁连接数:", err)
	}
	out.OverheadBytes = int64(float64(out.TotalBytes) * meterOverheadRatio)
	out.UnattributedBytes = out.TotalBytes - out.AttributedBytes - out.OverheadBytes
	if out.UnattributedBytes < 0 {
		// 口径差的方向并不固定，已归因偶尔会超过总量。显示一个负的
		// 「未归因」会让整块数据当场失去可信度，钳到 0 并把溢出量
		// 让给开销那一项——它本来就是估算。
		out.OverheadBytes = out.TotalBytes - out.AttributedBytes
		if out.OverheadBytes < 0 {
			out.OverheadBytes = 0
		}
		out.UnattributedBytes = 0
	}
	return out, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestTopDomains' -v
```

预期：两条新用例 PASS，既有 `TestTopDomains*` 与 coverage 相关用例全部 PASS。

- [ ] **Step 5: 前端渲染分解**

`web/html/xui/access_log_modal.html`，在榜单表格**下方**加：

```html
<div v-if="topResult.breakdown" style="margin-top: 12px; font-size: 12px; line-height: 1.9">
    <div style="color: rgba(0,0,0,.45); margin-bottom: 4px">用量分解（总量 [[ sizeFormat(topResult.breakdown.totalBytes) ]]）</div>
    <div>已归因合计　<b>[[ sizeFormat(topResult.breakdown.attributedBytes) ]]</b>
        <span style="color: rgba(0,0,0,.45)">（上表各行之和，精确）</span></div>
    <div>被封禁丢弃　<b>[[ topResult.breakdown.blockedConns ]] 次连接</b>
        <span style="color: rgba(0,0,0,.45)">（字节数不可得，已计入总量）</span></div>
    <div style="color: rgba(0,0,0,.65)">协议封装开销　约 [[ sizeFormat(topResult.breakdown.overheadBytes) ]]
        <span style="color: rgba(0,0,0,.45)">（估算：入站计加密流、出站计明文流，结构性差 3~8%）</span></div>
    <div style="color: rgba(0,0,0,.65)">未归因　　　　[[ sizeFormat(topResult.breakdown.unattributedBytes) ]]
        <span style="color: rgba(0,0,0,.45)">（余项：池外的长尾目标）</span></div>
</div>
```

三条约束：

1. **精确值用 `<b>`、估算值不用**，并在括号里点明哪一项是估的 —— 混在一起会让整块数据的可信度一起塌掉。
2. `topResult` 是这里假定的数据属性名，**用该文件里榜单实际使用的那个**。
3. `sizeFormat` 是项目里现成的字节格式化函数，确认它在这个页面可用（`common/js.html` 引入的工具里）；不可用就用该文件已有的等价函数。

- [ ] **Step 6: 验证模板**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

预期：PASS。`getHtmlTemplate` 会吞掉 `ParseFS` 错误，这两条是唯一防线。

- [ ] **Step 7: 提交**

```bash
git add web/service/accesslog.go web/service/domain_stat.go web/service/domain_stat_top_test.go web/html/xui/access_log_modal.html
git commit -m "$(cat <<'EOF'
feat(meter): 用量差额分解，让每一块都有名字

覆盖率永远到不了 100%（被封禁的流量拿不到字节、入站的加密流与出站的
明文流差 3~8%、池外还有长尾），但差额不该无声消失在一句「约 0% 已归因」
里——那句话除了让人以为系统坏了之外没有任何信息量。

拆成四项：已归因（精确）、被封禁的连接数（字节不可得，blackhole 不调用
Dial，出站计数器从未被装上）、协议封装开销（估算 5%）、未归因（余项）。
余项用减法得出，天然吸收全部估算误差；已归因超过总量时钳到 0，
显示一个负数会让整块数据当场失去可信度。

前端把精确值与估算值在视觉上分开，并逐项标注来源。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 7: 真实 xray 的 e2e 验证

本任务验证 spec §4.4 里那个**未经实测的假设**：IPv6 的计量 tag 含冒号，xray 是否接受。

**Files:**
- Modify: `web/service/meter_rule_e2e_test.go`

**Interfaces:**
- Consumes: Task 4 的规则生成
- Produces: 无（纯验证）

- [ ] **Step 1: 写测试**

追加到 `web/service/meter_rule_e2e_test.go`，参照同文件
`TestMeterRuleShapeAgainstRealXray` 的写法（它已有 `requireXrayBinary` 式的跳过逻辑，照抄）：

```go
// IP 计量规则与 IPv6 的计量 tag 必须能被真实 xray 接受。
//
// IPv6 那条是设计 §4.4 标记的未验证假设：tag 会含冒号
//（a-ui-meter-7-2001:db8::1）。xray 对 tag 字符集很宽松（含中文都
// Configuration OK），但冒号此前没有实测过。这条测试就是那个假设的验收。
//
// 若它失败，退路是对 IPv6 做一次确定性转写——**绝不能**把冒号换成短横线：
// ParseMeterTag 按第一个短横线切分，那会让反查静默错位。
func TestIPMeterRulesAreAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	seedEnabledInbound(t, 1, 2210)
	seedMeterPool(t, 1, "72.235.209.83")
	seedMeterPool(t, 1, "2001:db8::1")
	seedMeterPool(t, 1, "acspubs.org")

	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatalf("GetXrayConfig: %v", err)
	}
	if err := runXrayTestOnConfig(t, cfg); err != nil {
		t.Fatalf("真实 xray 拒绝了含 IP 计量规则的配置: %v", err)
	}
}
```

> **实现者注意**：`runXrayTestOnConfig` 是假定的辅助名。同文件
> `TestMeterRuleShapeAgainstRealXray` 一定有等价的「把配置交给真实 xray 跑
> run -test」的辅助，用它实际的名字。若它只做断言不返回 error，直接调用它即可。

- [ ] **Step 2: 运行测试**

```bash
go test ./web/service/ -run TestIPMeterRulesAreAcceptedByRealXray -v
```

预期：PASS。

**若因为 IPv6 tag 含冒号而失败**：停下来，不要自行改 tag 形态。
把 xray 的报错原文记下来，按 spec §4.4 的退路重新设计转写方案并**先与人确认**
——这个决定会影响 `ParseMeterTag` 的反查，改错是静默的。

- [ ] **Step 3: 提交**

```bash
git add web/service/meter_rule_e2e_test.go
git commit -m "$(cat <<'EOF'
test(meter): 用真实 xray 验证 IP 计量规则与 IPv6 tag

IPv6 的计量 tag 含冒号（a-ui-meter-7-2001:db8::1），设计文档 §4.4 把它
标为未验证的假设。这条 e2e 是那个假设的验收：xray 对 tag 字符集很宽松
（含中文都 Configuration OK），但冒号此前没有实测过。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

### Task 8: 全量验证与文档同步

**Files:**
- Modify: `docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md`（§6 标注分期）

- [ ] **Step 1: 跑门禁**

```bash
make verify
```

预期：exit 0。这会跑 `go vet ./...` + `go test ./...` + `go build`。

若 `web/service` 的 e2e 因为 xray 控制面端口被占用而 SKIP，**如实说明**，
不要当成跑过了（那个端口可能被并行会话的 xray 占着）。

- [ ] **Step 2: 检查最终 diff**

```bash
git diff --stat HEAD~7..HEAD
git status --short
```

确认：只动了计划里列出的文件；没有调试残留；没有 `make build` 产生的 `a-ui` 二进制
（它在 `.gitignore` 里，但仍要确认工作区干净）。

```bash
rm -f a-ui
```

- [ ] **Step 3: 同步设计文档**

`docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md`
的 §6「数据模型」开头加一段：

```markdown
> **分期说明（2026-09-09 实施时修订）**：本节的 `Kind` 列与索引扩展**只属于改动 B**。
> 改动 A 实施后确认不需要它们——`DomainStat.Domain` 从第一期起就「IP 字面量原样」存，
> `MeterDomain` 的唯一索引 `(inbound_id, domain)` 对 IP 同样成立，
> 而榜单的类型标签用 `net.ParseIP` 推导即可（多一处存储就是多一处会漂移的真相源）。
> 因此改动 A **零数据模型变更、零迁移风险**，本节描述的 GORM `AutoMigrate`
> 不修改已存在索引那个静默失败，只在改动 B 实施时才需要面对。
```

- [ ] **Step 4: 提交文档**

```bash
git add docs/superpowers/specs/2026-09-09-ip-and-proxied-traffic-metering-design.md
git commit -m "$(cat <<'EOF'
docs(spec): 标注 Kind 列只属于改动 B，改动 A 零数据模型变更

实施改动 A 时确认：DomainStat.Domain 本来就「IP 字面量原样」存，
MeterDomain 的 (inbound_id, domain) 唯一索引对 IP 同样成立，榜单的类型
标签用 net.ParseIP 推导即可。所以 A 阶段一行数据模型都不用改，
连带避开了 GORM AutoMigrate 不修改已存在索引那个静默失败。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01EsFfrgSijLYpV8mb7Qv5BB
EOF
)"
```

---

## 上线后的验收

部署到生产机之后，判据是**入站 7 的上传归因率**：

```bash
# 在服务器上（只读，零开销）
python3 - <<'PY'
import sqlite3, time
c = sqlite3.connect("file:/etc/a-ui/a-ui-traffic.db?mode=ro", uri=True)
now = int(time.time()); d1 = (now - 24*3600) // 3600 * 3600
tot = list(c.execute(
    "select sum(up) from traffic_buckets where inbound_id=7 and granularity=1 and bucket_start>=?", (d1,)))[0][0]
att = list(c.execute(
    "select sum(up) from domain_stats where inbound_id=7 and granularity=1 and bucket_start>=?", (d1,)))[0][0] or 0
print("上传归因率 %.2f%%  (%.2f MB / %.2f GB)" % (att/tot*100, att/2**20, tot/2**30))
PY
```

改动前是 **0.014%**。预期 **90% 以上**。

池要一个整点周期（`MeterPoolJob` 每小时）才会把 IP 纳进来，所以**部署后至少等一小时**再测。
池变动走 gRPC 热应用，不重启 xray。

---

## Self-Review

**1. Spec 覆盖（只针对改动 A，即 spec §4 全节）**

| spec 条目 | 对应任务 |
|---|---|
| §4.1 准入放开（三处调用点区别对待） | Task 1（新函数）、Task 2（池准入）、Task 3（生成期过滤）；`domain_stat.go:186` 的 `Registrable` 按 spec 要求**不动** |
| §4.2 IP 用 ip 条件、不带守卫、掩码补齐 | Task 4 |
| §4.3 域名规则排在 IP 规则之前 | Task 4 第三条测试 |
| §4.4 tag 形态与 IPv6 未验证假设 | Task 6 |
| §4.5 池排序/换池/死计数器不变 | 无代码改动；Task 2 Step 4 要求既有用例全绿来守它 |
| §4.6 出站数量不增 | 无代码改动；Global Constraints 写死 |
| §6 数据模型 | **确认改动 A 不需要**，Task 7 Step 3 回写 spec |
| §7 UI | Task 5（类型列）、Task 6（§7.1 差额分解）。**初稿把 §7.1 误划给改动 B，已修正**——它需要的四项数据（入站总量、已归因、封禁连接数、开销估算）在 A 阶段全部现成 |

**2. 占位符扫描**：无 TBD/TODO。三处「实现者注意」是**明确的指令**（去找现成辅助函数的实际名字），不是占位符——测试辅助的名字必须以仓库实际代码为准，写死一个猜的名字反而会误导。

**3. 类型一致性**：`IsMeterable(string) bool` 在 Task 1 定义，Task 2、3 使用；`meterIPCondition(string) (string, bool)` 与 `topDomainKind(string) string` 均在定义它们的任务内使用；`TopDomainRow.Kind` 在 Task 5 定义并在同任务的前端消费。`model.MeterTag` / `MeterEntry` 沿用现有签名，未改动。
