# 分流规则多入站（复选框） Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一条分流规则可以用复选框覆盖多个入站，并从数据模型层面保证「同一个域名组下，任何一个入站至多被一条规则覆盖」。

**Architecture:** `RoutingRule.InboundId int` 换成 `InboundIds string`（JSON 整数数组，升序去重，空数组 = 所有用户）。注入器把它展开成 xray 的 `inboundTag` 数组；写入路径新增一道跨规则的冲突校验；前端弹窗从下拉单选改成复选框并把已被占用的用户置灰。旧数据用一条幂等 SQL 回填，`inbound_id` 老列保留不删。

**Tech Stack:** Go 1.21 / GORM + SQLite（CGO 必开）/ Gin / Vue 2 + ant-design-vue（服务端模板，无打包工具）/ 标准 `go test`

**Spec:** `docs/superpowers/specs/2026-09-02-routing-multi-inbound-design.md`

## Global Constraints

这些约束来自 spec 与项目既有不变量，**每个任务都隐含包含本节**：

- **绝不输出 `inboundTag: []`。** 实测（Xray 26.7.28）确认 xray 把空的 `inboundTag` 当作「不限制」而非「不匹配任何入站」，规则会从「只覆盖甲」放大到「覆盖所有人」，且 `Configuration OK`、面板首页照样显示 running。这与 `domain: []` 是同一类事故。
- **非空的入站选择绝不能被静默过滤成空数组。** `[0]`、`[-1]` 这类输入编码后是 `[]`，而 `[]` 的语义是「所有用户」。写入路径（后端 `EncodeInboundIdsStrict` + 前端 `saveRule`）都必须拦住。
- **生成逐字节确定。** `InboundIds` 升序存储，`inboundTag` 数组按该顺序生成。**禁止遍历 map 产生数组顺序**——否则 `Config.Equals` 恒为 false，那个 10 秒的 cron 会不停重启 xray。
- **一律 append 到末尾；block 规则排在 proxy 规则之前。** 本次改动不碰这两条。
- **冲突校验只在写入路径生效，绝不在生成期干预。** 历史冲突数据照常生成两条规则，行为与上线前一致，只在界面标黄。
- **迁移不改变任何一条规则的实际生效范围。**
- **`inbound_id` 列保留不删。** 回滚到旧二进制时行为退回单选；删了列则每条规则都变成「所有用户」。
- **改了 `web/assets/js` 但版本号没变会命中 `max-age=31536000` 强缓存**，本地验证需硬刷新。
- **`web/html/**` 的语法错误被 `getHtmlTemplate` 吞掉**（`// ignore`），`go build` 发现不了，必须跑 `web/html_test.go`。
- **Vue 指令写在 `el` 指向的根元素之外是完全静默的死代码。** 弹窗必须留在 `<a-layout id="app">` 内。
- **cron 没有 panic 恢复**，注入器路径上的 nil map / 越界会杀掉整个面板进程。
- **commit 步骤需要用户明确授权后才执行**（项目规范：未经授权不执行 commit）。若未获授权，跳过 commit 步骤，把改动留在工作区。

**测试命令：**

```bash
go test ./web/service/... ./database/... ./web/ ./util/...
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o /tmp/a-ui-build main.go && rm -f /tmp/a-ui-build
go vet ./...
```

**中间态说明：** Task 2 完成前，多入站功能不是可用状态。不要在 Task 1 之后部署。

---

## File Structure

| 文件 | 职责 | 任务 |
|---|---|---|
| `database/model/routing.go` | `RoutingRule` 加 `InboundIds`，Task 2 删 `InboundId` | 1, 2 |
| `database/db.go` | `initRouting` 里的一次性回填迁移 | 1 |
| `database/routing_migrate_test.go` | 迁移测试（文件已存在，追加） | 1 |
| `web/service/routing_rule.go` | 编解码、写入校验、冲突判定、引用守卫 | 1, 2, 3 |
| `web/service/routing_rule_test.go` | 编解码 + 冲突 + 引用守卫测试（已存在，改写并追加） | 1, 2, 3 |
| `web/service/routing_inject.go` | `buildRule` 多入站生成 | 2 |
| `web/service/routing_inject_test.go` | 注入测试（已存在，改写并追加） | 2 |
| `web/controller/routing.go` | `routingRuleForm` / `routingRuleView` 转换层 | 2 |
| `web/assets/js/model/routing.js` | 前端 `RoutingRule` 模型 | 4 |
| `web/html/xui/routing.html` | 复选框弹窗、占用计算、列表渲染、`ruleIssue` | 4 |
| `docs/.../2026-09-02-domain-routing-design.md` | 标注被修订的章节 | 5 |
| `CLAUDE.md` | 同步两条字段约定 | 5 |

---

### Task 1: 数据字段、编解码与迁移

纯加法：新增 `InboundIds` 字段和一对编解码函数，把旧的 `inbound_id` 回填过去。`InboundId` 仍然是权威字段，**本任务不改变任何运行时行为**，现有测试必须全绿。

**Files:**
- Modify: `database/model/routing.go`（`RoutingRule` 结构体）
- Modify: `database/db.go`（`initRouting`）
- Modify: `web/service/routing_rule.go`（文件末尾追加三个函数）
- Test: `database/routing_migrate_test.go`（追加）
- Test: `web/service/routing_rule_test.go`（追加）

**Interfaces:**
- Consumes: 无
- Produces:
  - `model.RoutingRule.InboundIds string`
  - `service.EncodeInboundIds(ids []int) (string, error)`
  - `service.EncodeInboundIdsStrict(ids []int) (string, error)`
  - `service.DecodeInboundIds(encoded string) ([]int, error)`
  - `database.migrateRoutingRuleInboundIds() error`（包内私有）

- [ ] **Step 1: 给 `RoutingRule` 加字段**

`database/model/routing.go`，把 `InboundId` 那一段改成：

```go
	// InboundId 为 0 表示对所有入站生效。
	InboundId int `json:"inboundId" form:"inboundId"`
	// InboundIds 是这条规则覆盖的入站 id，JSON 整数数组，升序去重存储。
	// 空数组 [] 表示「所有用户」（含以后新建的入站）。
	//
	// 升序去重不是洁癖：buildRule 直接按这个顺序生成 inboundTag 数组，而
	// 「生成逐字节确定」是 Config.Equals 能正确判断配置是否变化的前提；
	// 顺序一抖动，那个 10 秒的重启 cron 就会不停重启 xray。
	InboundIds string `json:"inboundIds" form:"inboundIds"`
```

- [ ] **Step 2: 写编解码的失败测试**

`web/service/routing_rule_test.go` 末尾追加：

```go
func TestEncodeInboundIdsSortsAndDedupes(t *testing.T) {
	got, err := EncodeInboundIds([]int{5, 3, 5, 1})
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	if got != "[1,3,5]" {
		t.Errorf("got %q, want [1,3,5]", got)
	}
}

// 非正数会被丢弃，于是 [0] 编码后是 []——而 [] 的语义是「所有用户」。
// 严格版必须报错，绝不能让一条本该覆盖某个人的规则被静默放大到全体。
func TestEncodeInboundIdsStrictRejectsAllInvalid(t *testing.T) {
	if _, err := EncodeInboundIdsStrict([]int{0, -1}); err == nil {
		t.Error("expected error: non-empty input with no valid id must not become []")
	}
	// 空输入是前端显式选了「所有用户」，必须放行
	got, err := EncodeInboundIdsStrict(nil)
	if err != nil {
		t.Fatalf("empty input must be accepted: %v", err)
	}
	if got != "[]" {
		t.Errorf("got %q, want []", got)
	}
}

func TestDecodeInboundIdsTreatsBlankAsAllUsers(t *testing.T) {
	for _, raw := range []string{"", "   ", "null"} {
		got, err := DecodeInboundIds(raw)
		if err != nil {
			t.Fatalf("DecodeInboundIds(%q): %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("DecodeInboundIds(%q) = %v, want empty", raw, got)
		}
	}
}

// 真正的语法错误必须返回 error，由 buildRule 整条丢弃该规则——
// 当成空数组就等于把规则放大到所有用户。
func TestDecodeInboundIdsRejectsCorruptData(t *testing.T) {
	if _, err := DecodeInboundIds("{not json"); err == nil {
		t.Error("expected error for corrupt data")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./web/service/ -run 'InboundIds' -v`
Expected: FAIL，`undefined: EncodeInboundIds` 等编译错误

- [ ] **Step 4: 实现编解码**

`web/service/routing_rule.go` 末尾追加（`import` 补上 `encoding/json`、`sort`、`strings`）：

```go
// EncodeInboundIds 把入站 id 列表编成入库格式：丢弃非正数、去重、升序。
//
// 升序是「生成逐字节确定」不变量的一部分，见 model.RoutingRule.InboundIds 的注释。
//
// 注意本函数会丢弃非正数，因此 [0] 这类输入会得到 "[]"——而 "[]" 的语义是
// 「所有用户」。写入路径一律用 EncodeInboundIdsStrict，不要直接用这个。
func EncodeInboundIds(ids []int) (string, error) {
	seen := make(map[int]bool, len(ids))
	cleaned := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
	}
	sort.Ints(cleaned)
	b, err := json.Marshal(cleaned)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// EncodeInboundIdsStrict 是写入路径该用的版本：原始列表非空、却没有任何
// 有效 id 时报错，而不是降级成「所有用户」。
//
// 一次前端 bug 或手工构造的请求，就能把一条本该只覆盖某个人的规则悄悄
// 放大到全体，且一路合法——「所有用户」只能来自前端那个显式的复选框，
// 也就是原始列表本来就是空的情况。
func EncodeInboundIdsStrict(ids []int) (string, error) {
	encoded, err := EncodeInboundIds(ids)
	if err != nil {
		return "", err
	}
	if len(ids) > 0 && encoded == "[]" {
		return "", common.NewError("入站选择非法：提交了", len(ids), "个入站，但没有一个是有效的入站 id")
	}
	return encoded, nil
}

// DecodeInboundIds 是 EncodeInboundIds 的逆操作。
//
// 空字符串与 "null" 当作空数组（= 所有用户）：迁移会回填，但直接改库、
// 并发写入等路径仍可能留下空值，在这里报错会让整份配置生成失败。
// 真正的语法错误仍返回 error，交给调用方整条丢弃该规则。
func DecodeInboundIds(encoded string) ([]int, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var ids []int
	if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./web/service/ -run 'InboundIds' -v`
Expected: PASS（4 个测试）

- [ ] **Step 6: 写迁移的失败测试**

`database/routing_migrate_test.go` 末尾追加：

```go
// 旧库里的规则只有 inbound_id。迁移必须把它搬到 inbound_ids，且
// 绝不改变任何一条规则的实际生效范围：inbound_id = 0 是「所有入站」，
// 对应新语义的空数组 []。
func TestMigrateRoutingRuleInboundIds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()

	insert := func(remark string, inboundId int, inboundIds string) {
		t.Helper()
		err := db.Exec(`INSERT INTO routing_rules
			(remark, inbound_id, inbound_ids, domain_group_id, action, outbound_id, priority, enable)
			VALUES (?, ?, ?, 1, 'block', 0, 0, 1)`, remark, inboundId, inboundIds).Error
		if err != nil {
			t.Fatalf("insert %s: %v", remark, err)
		}
	}
	insert("指定入站", 7, "")
	insert("全局规则", 0, "")
	insert("已迁移过", 7, "[1,2]")

	if err := migrateRoutingRuleInboundIds(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	read := func(remark string) string {
		t.Helper()
		var got string
		err := db.Raw("SELECT inbound_ids FROM routing_rules WHERE remark = ?", remark).Scan(&got).Error
		if err != nil {
			t.Fatalf("read %s: %v", remark, err)
		}
		return got
	}
	if got := read("指定入站"); got != "[7]" {
		t.Errorf("指定入站 inbound_ids = %q, want [7]", got)
	}
	if got := read("全局规则"); got != "[]" {
		t.Errorf("全局规则 inbound_ids = %q, want []", got)
	}
	// 已有值不能被覆盖，否则重启一次就把用户改过的多入站选择打回单选
	if got := read("已迁移过"); got != "[1,2]" {
		t.Errorf("已迁移过 inbound_ids = %q, want [1,2]", got)
	}

	// 幂等：面板每次启动都会跑这条迁移
	if err := migrateRoutingRuleInboundIds(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if got := read("指定入站"); got != "[7]" {
		t.Errorf("after second run: %q, want [7]", got)
	}
}
```

- [ ] **Step 7: 跑测试确认失败**

Run: `go test ./database/ -run TestMigrateRoutingRuleInboundIds -v`
Expected: FAIL，`undefined: migrateRoutingRuleInboundIds`

- [ ] **Step 8: 实现迁移**

`database/db.go`，在 `initRouting` 上方加函数：

```go
// migrateRoutingRuleInboundIds 把旧的单入站字段 inbound_id 搬到 inbound_ids。
//
// 幂等：只回填 inbound_ids 为空的行，面板每次启动都会跑，重启多少次都安全。
//
// inbound_id = 0 在旧语义里是「所有入站」，对应新语义的空数组 []——两者
// 生效范围完全一致，迁移不改变任何一条规则的实际行为。
//
// 全新安装的库没有 inbound_id 列（该字段最终会从结构体删除），直接执行会
// 报 no such column，所以先探测。字段不在结构体上时 GORM 会把传入的字符串
// 直接当列名去查，正是这里需要的行为。
//
// inbound_id 列有意保留不删（GORM 的 sqlite AutoMigrate 本来也不删列）：
// 万一管理员回滚到旧版本二进制，旧代码读到的还是原值，行为退回单选；
// 删掉列则每条规则都会变成「所有用户」，作用域被静默放大到全体。
func migrateRoutingRuleInboundIds() error {
	if !db.Migrator().HasColumn(&model.RoutingRule{}, "inbound_id") {
		return nil
	}
	return db.Exec(`
UPDATE routing_rules
SET inbound_ids = CASE WHEN inbound_id > 0 THEN '[' || inbound_id || ']' ELSE '[]' END
WHERE inbound_ids IS NULL OR inbound_ids = ''`).Error
}
```

`initRouting` 改成：

```go
func initRouting() error {
	if err := db.AutoMigrate(&model.DomainGroup{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.OutboundNode{}); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.RoutingRule{}); err != nil {
		return err
	}
	return migrateRoutingRuleInboundIds()
}
```

- [ ] **Step 9: 跑测试确认通过**

Run: `go test ./database/ -v`
Expected: PASS（含原有两个测试）

- [ ] **Step 10: 全量回归 —— 本任务不得改变任何现有行为**

```bash
go test ./web/service/... ./database/... ./web/ ./util/...
go vet ./...
```
Expected: 全部 PASS

- [ ] **Step 11: Commit（需用户授权）**

```bash
git add database/model/routing.go database/db.go database/routing_migrate_test.go \
        web/service/routing_rule.go web/service/routing_rule_test.go
git commit -m "feat(routing): 分流规则新增 InboundIds 字段与旧数据迁移"
```

---

### Task 2: 切换到多入站

写入路径、读取路径、注入器一次性切换到 `InboundIds`，并删除 `InboundId` 字段。**必须原子完成**：写入与读取分开切换会让中间态的新建规则全部变成「所有用户」。

**Files:**
- Modify: `database/model/routing.go`（删 `InboundId`）
- Modify: `web/service/routing_rule.go`（`Update` 搬运字段、`CheckInboundRefs` 改判定）
- Modify: `web/service/routing_inject.go`（`buildRule`）
- Modify: `web/controller/routing.go`（form / view 转换层）
- Test: `web/service/routing_inject_test.go`
- Test: `web/service/routing_rule_test.go`

**Interfaces:**
- Consumes: Task 1 的 `EncodeInboundIds` / `EncodeInboundIdsStrict` / `DecodeInboundIds`、`model.RoutingRule.InboundIds`
- Produces:
  - `controller.routingRuleForm` / `controller.routingRuleView`（包内私有）
  - `controller.ruleFromForm(id int, form *routingRuleForm) (*model.RoutingRule, error)`
  - `buildRule` 生成的 `inboundTag` 为 `[]string`，顺序与 `InboundIds` 升序一致
  - `RoutingRuleService.CheckInboundRefs` 语义不变：空数组不算引用某个具体入站
  - 测试辅助 `mustEncodeIds(t *testing.T, ids []int) string`（定义在 `routing_inject_test.go`，Task 3 复用）

- [ ] **Step 1: 写注入器的失败测试**

`web/service/routing_inject_test.go` 末尾追加。注意 `newTestInbound`、`newTemplateConfig`、`decodeRules` 都已存在于该文件，`newTestGroup` 在 `routing_rule_test.go`（同包可用）。

```go
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
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
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
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
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

// 本计划最重要的一条测试。
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
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
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
		InboundIds: "[]", DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
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
```

同时把该文件里两个已有测试改成新字段：

- `TestInjectGlobalRuleOmitsInboundTag`：`InboundId: 0` → `InboundIds: "[]"`
- `TestInjectPerInboundRuleUsesCurrentTag`：`InboundId: in.Id` → `InboundIds: mustEncodeIds(t, []int{in.Id})`

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run 'TestInjectMultiInbound|TestInjectSkipsRule' -v`
Expected: FAIL（`InboundIds` 尚未被 `buildRule` 读取，规则会被当成全局规则生成，`inboundTag` 缺失）

- [ ] **Step 3: 改 `buildRule`**

`web/service/routing_inject.go`，把这一段：

```go
	if rule.InboundId > 0 {
		tag, ok := inboundTagById[rule.InboundId]
		if !ok {
			return nil, false, common.NewError("入站不存在或已禁用, id:", rule.InboundId)
		}
		generated["inboundTag"] = []string{tag}
	}
```

替换为（注意函数里已有 `err` 变量，用 `=` 而不是 `:=`，按编译器提示微调）：

```go
	inboundIds, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return nil, false, common.NewError("规则的入站数据损坏, id:", rule.Id, "err:", err)
	}
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
			// 只覆盖甲的规则会劫持所有人的这批域名，且 Configuration OK、
			// 面板首页照样显示 running。与 domain 为空数组是同一类事故。
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
		generated["inboundTag"] = tags
	}
```

- [ ] **Step 4: 跑注入器测试确认通过**

Run: `go test ./web/service/ -run TestInject -v`
Expected: PASS

- [ ] **Step 5: 写 `CheckInboundRefs` 的失败测试**

`web/service/routing_rule_test.go` 末尾追加：

```go
// 引用守卫必须能看穿多入站规则：SQLite 会复用被删除的自增 id，
// 一条覆盖 [甲, 乙] 的规则在甲被删掉后，会静默重绑到捡到甲旧 id 的新入站上。
func TestCheckInboundRefsSeesIdInTheMiddleOfAMultiInboundRule(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	c := newTestInbound(t, 10003)
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		InboundIds:    mustEncodeIds(t, []int{a.Id, b.Id}),
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckInboundRefs(b.Id); err == nil {
		t.Error("expected error: inbound b is referenced by a multi-inbound rule")
	}
	if err := s.CheckInboundRefs(c.Id); err != nil {
		t.Errorf("unreferenced inbound should be deletable, got %v", err)
	}
}

// 「所有用户」规则不指向任何具体入站，不算引用——与旧语义 InboundId = 0 一致。
// 否则一旦建了全局封禁规则，所有入站都删不掉了。
func TestCheckInboundRefsIgnoresAllUsersRule(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	in := newTestInbound(t, 10001)
	s := RoutingRuleService{}
	if err := s.Add(&model.RoutingRule{
		InboundIds: "[]", DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.CheckInboundRefs(in.Id); err != nil {
		t.Errorf("an all-users rule must not pin any specific inbound, got %v", err)
	}
}
```

同时把该文件里已有的 `TestDelInboundRejectedWhileReferencedByRule` 中的 `InboundId: in.Id` 改成 `InboundIds: mustEncodeIds(t, []int{in.Id})`。

还有 `web/service/routing_e2e_test.go` 约第 96-104 行那三条规则，也要换成新字段（这三条分属两个域名组、两个不同入站，互不相交，Task 3 的冲突校验不会挡它们）：

```go
		{Remark: "全员封禁违规域名", InboundIds: "[]", DomainGroupId: banned.Id,
			Action: model.ActionBlock, Priority: 0, Enable: true},
		{Remark: "甲的 ChatGPT 走 B", InboundIds: mustEncodeIds(t, []int{jia.Id}), DomainGroupId: chatgpt.Id,
			Action: model.ActionProxy, OutboundId: nodeB.Id, Priority: 1, Enable: true},
		{Remark: "乙的 ChatGPT 走 C", InboundIds: mustEncodeIds(t, []int{yi.Id}), DomainGroupId: chatgpt.Id,
			Action: model.ActionProxy, OutboundId: nodeC.Id, Priority: 2, Enable: true},
```

- [ ] **Step 6: 跑测试确认失败**

Run: `go test ./web/service/ -run TestCheckInboundRefs -v`
Expected: FAIL（`CheckInboundRefs` 还在查 `inbound_id` 列）

- [ ] **Step 7: 改 `CheckInboundRefs` 与 `Update`**

`web/service/routing_rule.go`：

```go
func (s *RoutingRuleService) CheckInboundRefs(inboundId int) error {
	if inboundId <= 0 {
		return nil
	}
	rules, err := s.GetAll()
	if err != nil {
		return err
	}
	count := 0
	for _, rule := range rules {
		ids, decodeErr := DecodeInboundIds(rule.InboundIds)
		if decodeErr != nil {
			// 数据损坏时无从判断这条规则引用了谁。宁可拦住删除：放行的话，
			// SQLite 复用 id 后这条规则可能静默绑到新建的入站上。
			return common.NewError("分流规则", rule.Id,
				"的入站数据已损坏，无法确认引用关系，请先修复或删除该规则")
		}
		for _, id := range ids {
			if id == inboundId {
				count++
				break
			}
		}
	}
	if count > 0 {
		return common.NewError("该入站仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}
```

`Update` 里 `old.InboundId = rule.InboundId` 改成 `old.InboundIds = rule.InboundIds`。

- [ ] **Step 8: 跑测试确认通过**

Run: `go test ./web/service/ -run 'TestCheckInboundRefs|TestDelInbound' -v`
Expected: PASS

- [ ] **Step 9: 删除 `InboundId` 字段**

`database/model/routing.go` 里删掉这两行：

```go
	// InboundId 为 0 表示对所有入站生效。
	InboundId int `json:"inboundId" form:"inboundId"`
```

`inbound_id` 数据库列**不删**，理由见 Global Constraints。

- [ ] **Step 10: 编译，逐个修掉残留引用**

Run: `go build ./... && go vet ./...`
Expected: 报出所有还在用 `InboundId` 的位置（controller、可能遗漏的测试）。逐个改掉。

- [ ] **Step 11: 加 controller 的转换层**

`web/controller/routing.go`，在 `domainGroupDetail` 下方加：

```go
type routingRuleForm struct {
	Remark string `json:"remark" form:"remark"`
	// InboundIds 为空数组表示「所有用户」。空与「全是非法 id」必须区分开，
	// 转换时走 EncodeInboundIdsStrict——后者报错，前者才是合法的全局规则。
	InboundIds    []int  `json:"inboundIds" form:"inboundIds"`
	DomainGroupId int    `json:"domainGroupId" form:"domainGroupId"`
	Action        string `json:"action" form:"action"`
	OutboundId    int    `json:"outboundId" form:"outboundId"`
	Priority      int    `json:"priority" form:"priority"`
	Enable        bool   `json:"enable" form:"enable"`
}

type routingRuleView struct {
	Id            int    `json:"id"`
	Remark        string `json:"remark"`
	InboundIds    []int  `json:"inboundIds"`
	DomainGroupId int    `json:"domainGroupId"`
	Action        string `json:"action"`
	OutboundId    int    `json:"outboundId"`
	Priority      int    `json:"priority"`
	Enable        bool   `json:"enable"`
	// Broken 标记 InboundIds 列解码失败。这种规则 buildRule 会整条丢弃，
	// 但解码失败得到的空数组在前端看来就是「所有用户」——不带这个标记，
	// 一条已经不生效的规则会在界面上显示成覆盖全员的正常规则。
	Broken bool `json:"broken"`
}

// ruleFromForm 把表单转成待落库的规则。
func ruleFromForm(id int, form *routingRuleForm) (*model.RoutingRule, error) {
	encoded, err := service.EncodeInboundIdsStrict(form.InboundIds)
	if err != nil {
		return nil, err
	}
	return &model.RoutingRule{
		Id:            id,
		Remark:        form.Remark,
		InboundIds:    encoded,
		DomainGroupId: form.DomainGroupId,
		Action:        form.Action,
		OutboundId:    form.OutboundId,
		Priority:      form.Priority,
		Enable:        form.Enable,
	}, nil
}
```

三个 handler 改成：

```go
func (a *RoutingController) listRules(c *gin.Context) {
	rules, err := a.ruleService.GetAll()
	if err != nil {
		jsonMsg(c, "获取分流规则", err)
		return
	}
	views := make([]*routingRuleView, 0, len(rules))
	for _, rule := range rules {
		ids, decodeErr := service.DecodeInboundIds(rule.InboundIds)
		broken := decodeErr != nil
		if broken {
			ids = nil
		}
		if ids == nil {
			// 必须是 []，不能是 null：前端对它做 .length / .includes，
			// null 会在渲染规则列表时抛异常，整页数据都出不来。
			ids = []int{}
		}
		views = append(views, &routingRuleView{
			Id: rule.Id, Remark: rule.Remark, InboundIds: ids,
			DomainGroupId: rule.DomainGroupId, Action: rule.Action,
			OutboundId: rule.OutboundId, Priority: rule.Priority,
			Enable: rule.Enable, Broken: broken,
		})
	}
	jsonObj(c, views, nil)
}

func (a *RoutingController) addRule(c *gin.Context) {
	form := &routingRuleForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "添加分流规则", err)
		return
	}
	rule, err := ruleFromForm(0, form)
	if err != nil {
		jsonMsg(c, "添加分流规则", err)
		return
	}
	err = a.ruleService.Add(rule)
	jsonMsg(c, "添加分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	form := &routingRuleForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	rule, err := ruleFromForm(id, form)
	if err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	err = a.ruleService.Update(rule)
	jsonMsg(c, "修改分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}
```

- [ ] **Step 12: 全量回归**

```bash
go build ./... && go vet ./...
go test ./web/service/... ./database/... ./web/ ./util/...
```
Expected: 全部 PASS

- [ ] **Step 13: Commit（需用户授权）**

```bash
git add database/model/routing.go web/service/routing_rule.go web/service/routing_inject.go \
        web/controller/routing.go web/service/routing_rule_test.go web/service/routing_inject_test.go
git commit -m "feat(routing): 分流规则支持多入站，生成 inboundTag 数组"
```

---

### Task 3: 冲突校验

新增「同一个域名组下，任何一个入站至多被一条规则覆盖」的写入期校验。纯新增逻辑，不碰生成期。

**Files:**
- Modify: `web/service/routing_rule.go`
- Test: `web/service/routing_rule_test.go`

**Interfaces:**
- Consumes: Task 1 的 `DecodeInboundIds`；Task 2 的 `model.RoutingRule.InboundIds` 与 `mustEncodeIds`
- Produces:
  - `RoutingRuleService.checkConflict(rule *model.RoutingRule) error`（包内私有，由 `Add` / `Update` 调用）
  - `intersectInbounds(a, b []int) (bool, int)`（包内私有）

- [ ] **Step 1: 写冲突校验的失败测试**

`web/service/routing_rule_test.go` 末尾追加：

```go
// newConflictFixture 建一个域名组和三个入站，供冲突测试复用。
func newConflictFixture(t *testing.T) (*model.DomainGroup, *model.Inbound, *model.Inbound, *model.Inbound) {
	t.Helper()
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	return g, newTestInbound(t, 10001), newTestInbound(t, 10002), newTestInbound(t, 10003)
}

func addRuleWith(t *testing.T, groupId int, ids []int, remark string) *model.RoutingRule {
	t.Helper()
	r := &model.RoutingRule{
		Remark: remark, InboundIds: mustEncodeIds(t, ids),
		DomainGroupId: groupId, Action: model.ActionBlock, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add %s: %v", remark, err)
	}
	return r
}

func TestConflictRejectsOverlappingInbounds(t *testing.T) {
	g, a, b, c := newConflictFixture(t)
	addRuleWith(t, g.Id, []int{a.Id, b.Id}, "甲乙走 B")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "乙丙走 C", InboundIds: mustEncodeIds(t, []int{b.Id, c.Id}),
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: inbound b is already covered in this domain group")
	}
}

func TestConflictAllowsDisjointInbounds(t *testing.T) {
	g, a, b, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, []int{a.Id}, "甲")

	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "乙", InboundIds: mustEncodeIds(t, []int{b.Id}),
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("disjoint inbounds must be accepted: %v", err)
	}
}

// 严格互斥：一个域名组一旦有了「所有用户」规则，就不能再对它加任何规则。
func TestConflictAllUsersBlocksSpecificUser(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, nil, "所有用户")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: an all-users rule already covers this domain group")
	}
}

// 反方向同样要挡：已有指定用户的规则时，「所有用户」也勾不上。
func TestConflictSpecificUserBlocksAllUsers(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, []int{a.Id}, "甲")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "所有用户", InboundIds: "[]",
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: a specific-user rule already exists in this domain group")
	}
}

func TestConflictAllUsersBlocksAnotherAllUsers(t *testing.T) {
	g, _, _, _ := newConflictFixture(t)
	addRuleWith(t, g.Id, nil, "所有用户")

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "所有用户 2", InboundIds: "[]",
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: two all-users rules in the same domain group")
	}
}

// 不同域名组永不冲突，即使域名内容重叠——那种重叠由 Priority 决定先后，
// 是既有语义，本功能不动它。
func TestConflictIgnoresOtherDomainGroups(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	other := newTestGroup(t, "另一个组")
	addRuleWith(t, g.Id, []int{a.Id}, "甲在 ChatGPT 组")

	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲在另一个组", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: other.Id, Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("different domain groups must never conflict: %v", err)
	}
}

// 禁用的规则同样占位，否则会出现「保存时没问题、一启用才发现撞车」。
func TestConflictCountsDisabledRules(t *testing.T) {
	g, a, _, _ := newConflictFixture(t)
	r := addRuleWith(t, g.Id, []int{a.Id}, "甲（将被禁用）")
	r.Enable = false
	if err := (&RoutingRuleService{}).Update(r); err != nil {
		t.Fatalf("Update: %v", err)
	}

	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "甲走别处", InboundIds: mustEncodeIds(t, []int{a.Id}),
		DomainGroupId: g.Id, Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("expected conflict: a disabled rule still holds its slot")
	}
}

// 编辑自己不能算和自己冲突，否则任何一条规则都改不动了。
func TestConflictExcludesTheRuleBeingUpdated(t *testing.T) {
	g, a, b, _ := newConflictFixture(t)
	r := addRuleWith(t, g.Id, []int{a.Id}, "甲")

	r.InboundIds = mustEncodeIds(t, []int{a.Id, b.Id})
	if err := (&RoutingRuleService{}).Update(r); err != nil {
		t.Fatalf("updating a rule must not conflict with itself: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run TestConflict -v`
Expected: FAIL —— 冲突用例全部「成功添加」而不是报错

- [ ] **Step 3: 实现冲突判定**

`web/service/routing_rule.go`，在 `validate` 下方加（`import` 补 `strconv`）：

```go
// intersectInbounds 判断两个覆盖集合是否相交。空切片代表全集（所有用户）。
//
// 第二个返回值是相交的具体入站 id；任一边是全集时返回 0，此时错误信息该说
// 「所有用户」而不是点某个人的名。b 已升序，取到的是最小的相交 id，
// 保证错误信息稳定可测。
func intersectInbounds(a, b []int) (bool, int) {
	if len(a) == 0 || len(b) == 0 {
		return true, 0
	}
	set := make(map[int]bool, len(a))
	for _, id := range a {
		set[id] = true
	}
	for _, id := range b {
		if set[id] {
			return true, id
		}
	}
	return false, 0
}

// ruleLabel 给规则一个人能认出来的名字，备注为空时退回 id。
func ruleLabel(rule *model.RoutingRule) string {
	if rule.Remark != "" {
		return rule.Remark
	}
	return "规则 #" + strconv.Itoa(rule.Id)
}

// inboundLabel 把相交的入站说清楚。id 为 0 表示冲突来自「所有用户」那一侧，
// 此时没有具体的人可点名。
func inboundLabel(id int) string {
	if id <= 0 {
		return "「所有用户」"
	}
	in, err := (&InboundService{}).GetInbound(id)
	if err != nil || in == nil {
		return "入站 #" + strconv.Itoa(id)
	}
	if in.Remark != "" {
		return "用户「" + in.Remark + "」"
	}
	return "入站「" + in.Tag + "」"
}

// groupLabel 取域名组备注，取不到就退回 id——报错信息本身不该再失败。
func (s *RoutingRuleService) groupLabel(id int) string {
	group, err := s.domainGroupService.Get(id)
	if err != nil || group == nil {
		return "#" + strconv.Itoa(id)
	}
	return group.Remark
}

// checkConflict 保证「同一个域名组下，任何一个入站至多被一条规则覆盖」。
//
// 把每条规则看作它覆盖的入站集合：InboundIds 为空数组表示全集（含以后新建
// 的入站），否则就是那些 id 的集合。两条规则冲突，当且仅当域名组相同且集合
// 相交——全集与任何集合相交，也与另一个全集相交，「所有用户」与「指定用户」
// 的严格互斥就是这条判定的自然结果，不需要额外分支。
//
// 不过滤 Enable：禁用的规则同样占位，否则会出现「保存时没问题、一启用才
// 发现撞车」。想腾位置就得先改掉或删掉旧规则。
//
// 只在写入路径生效，绝不在生成期干预。迁移前写入的冲突数据照常生成两条
// 规则，行为与本功能上线前一致；生成期悄悄丢一条，等于在管理员不知情时
// 改变分流行为。历史冲突由界面标黄暴露，交给人决定改哪条。
func (s *RoutingRuleService) checkConflict(rule *model.RoutingRule) error {
	ids, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return common.NewError("入站数据损坏:", err)
	}

	db := database.GetDB()
	others := make([]*model.RoutingRule, 0)
	err = db.Model(model.RoutingRule{}).
		Where("domain_group_id = ? and id <> ?", rule.DomainGroupId, rule.Id).
		Order("id asc").Find(&others).Error
	if err != nil {
		return err
	}

	for _, other := range others {
		otherIds, decodeErr := DecodeInboundIds(other.InboundIds)
		if decodeErr != nil {
			// 无从判断它覆盖了谁。不拦——它自己已经会被 buildRule 整条丢弃，
			// 再拿它去挡别人只会让管理员既修不了旧规则也建不了新规则。
			continue
		}
		overlap, who := intersectInbounds(ids, otherIds)
		if !overlap {
			continue
		}
		return common.NewError("与分流规则「", ruleLabel(other), "」冲突：",
			inboundLabel(who), "在域名组「", s.groupLabel(rule.DomainGroupId),
			"」下已被它覆盖。同一个用户在同一个域名组下只能有一条规则。")
	}
	return nil
}
```

- [ ] **Step 4: 在 `Add` / `Update` 接入**

```go
func (s *RoutingRuleService) Add(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	if err := s.checkConflict(rule); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Save(rule).Error
}
```

`Update` 同样在 `validate` 之后加 `checkConflict`。`Update` 传进来的 `rule.Id` 必须已经是目标 id，`checkConflict` 靠它排除自身。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./web/service/ -run TestConflict -v`
Expected: PASS（8 个测试）

- [ ] **Step 6: 全量回归**

```bash
go test ./web/service/... ./database/... ./web/ ./util/...
go vet ./...
```
**已知有两个既有测试会被打挂**，它们都往同一个域名组里连插多条「所有用户」规则。把它们改成挂不同的域名组——**不要削弱校验来迁就测试**，这两个测试关心的是排序和生成确定性，与冲突无关。

`web/service/routing_rule_test.go` 的 `TestGetEnabledRulesSortedByPriorityThenId`，把开头的 `g := newTestGroup(t, "ChatGPT")` 和那个循环整体换成（该文件需要 `import "strconv"`）：

```go
	s := RoutingRuleService{}
	// 故意乱序插入。三条规则挂三个不同的域名组：同一个域名组下每个入站至多
	// 被一条规则覆盖，而本测试关心的是排序，不该被冲突校验挡住。
	for i, p := range []int{20, 10, 10} {
		g := newTestGroup(t, "组 "+strconv.Itoa(i))
		if err := s.Add(&model.RoutingRule{
			DomainGroupId: g.Id, Action: model.ActionBlock, Priority: p, Enable: true,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
```

`web/service/routing_inject_test.go` 的 `TestInjectIsDeterministic`，同样把 `g := newTestGroup(t, "ChatGPT")` 删掉并改循环（该文件已 import `strconv`）：

```go
	rs := RoutingRuleService{}
	// 五条规则挂五个不同的域名组，理由同上：本测试关心的是多条规则的生成
	// 顺序是否逐字节稳定。
	for i := 0; i < 5; i++ {
		g := newTestGroup(t, "组 "+strconv.Itoa(i))
		if err := rs.Add(&model.RoutingRule{
			DomainGroupId: g.Id, Action: model.ActionProxy, OutboundId: node.Id, Enable: true,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
```

其余既有用例（`TestInjectBlockRulesComeBeforeProxyRules` 用两个不同域名组、`routing_e2e_test.go` 那三条分属两组两个不同入站、其他都是单条规则）不受影响。改完再全量跑一次确认没有遗漏。

- [ ] **Step 7: Commit（需用户授权）**

```bash
git add web/service/routing_rule.go web/service/routing_rule_test.go
git commit -m "feat(routing): 同一域名组下每个入站至多被一条规则覆盖"
```

---

### Task 4: 前端复选框

**Files:**
- Modify: `web/assets/js/model/routing.js`
- Modify: `web/html/xui/routing.html`
- Test: `web/html_test.go`（已有，只跑不改）

**Interfaces:**
- Consumes: Task 2 的 `routingRuleView`（`inboundIds: []int`、`broken: bool`）与 `routingRuleForm`
- Produces: 无（终端 UI）

- [ ] **Step 1: 改前端模型**

`web/assets/js/model/routing.js`，把 `RoutingRule` 整个替换为：

```js
class RoutingRule {
    constructor(id = 0, remark = "", inboundIds = [], domainGroupId = 0,
                action = RULE_ACTION.PROXY, outboundId = 0, priority = 0,
                enable = true, broken = false) {
        this.id = id;
        this.remark = remark;
        // 空数组 = 所有用户（含以后新建的入站）。
        // 注意它与「一个用户都没勾」在提交体里长得一模一样，弹窗必须自己
        // 区分这两种意图，见 routing.html 的 saveRule。
        this.inboundIds = inboundIds;
        this.domainGroupId = domainGroupId;
        this.action = action;
        this.outboundId = outboundId;
        this.priority = priority;
        this.enable = enable;
        // broken 为真表示服务端解码 inboundIds 失败。这种规则不会写进配置，
        // 但它的 inboundIds 是空数组，看起来和「所有用户」一样，必须区分渲染。
        this.broken = broken;
    }

    static fromJson(json = {}) {
        return new RoutingRule(json.id, json.remark, json.inboundIds || [],
            json.domainGroupId, json.action, json.outboundId, json.priority,
            json.enable, json.broken);
    }
}
```

- [ ] **Step 2: 改规则表的「用户」列**

`web/html/xui/routing.html`，把 `<template slot="inbound" ...>` 整块替换为：

```html
<template slot="inbound" slot-scope="text, rule">
    <a-tag v-if="rule.broken" color="red">数据损坏</a-tag>
    <a-tag v-else-if="!rule.inboundIds.length">所有用户</a-tag>
    <template v-else>
        <a-tag v-for="id in rule.inboundIds.slice(0, 3)" :key="id"
               :color="inboundMissing(id) ? 'red' : ''">
            [[ inboundName(id) ]]
        </a-tag>
        <a-tooltip v-if="rule.inboundIds.length > 3"
                   :title="rule.inboundIds.map(inboundName).join('、')">
            <a-tag>等 [[ rule.inboundIds.length ]] 人</a-tag>
        </a-tooltip>
    </template>
</template>
```

- [ ] **Step 3: 改弹窗的「用户（入站）」表单项**

把原来的 `<a-select v-model="ruleModal.rule.inboundId" ...>` 那整个 `<a-form-item>` 替换为：

```html
<a-form-item label="用户（入站）">
    <a-checkbox :checked="ruleModal.allUsers" :disabled="allUsersDisabled"
                @change="e => toggleAllUsers(e.target.checked)">
        所有用户（含以后新建的入站）
    </a-checkbox>
    <div v-if="allUsersDisabledReason"
         style="color: #faad14; font-size: 12px; margin: 4px 0 0 24px;">
        [[ allUsersDisabledReason ]]
    </div>
    <a-divider style="margin: 8px 0;"></a-divider>
    <div v-if="!inbounds.length" style="color: #999;">还没有任何入站</div>
    <div v-for="i in inbounds" :key="i.id" style="margin-bottom: 4px;">
        <a-checkbox :checked="ruleModal.rule.inboundIds.includes(i.id)"
                    :disabled="ruleModal.allUsers || !!inboundOccupiedBy(i.id)"
                    @change="e => toggleInbound(i.id, e.target.checked)">
            [[ i.remark || i.tag ]]
        </a-checkbox>
        <span v-if="inboundOccupiedBy(i.id)"
              style="color: #999; font-size: 12px; margin-left: 8px;">
            已被规则「[[ inboundOccupiedBy(i.id) ]]」覆盖
        </span>
    </div>
</a-form-item>
```

**这一整块必须留在 `<a-layout id="app">` 内部。** Vue 2 只编译 `el` 指向的那棵子树，写到外面所有 `@change`、`:disabled` 都是死代码，页面渲染正常、点击毫无反应、控制台不报错。

- [ ] **Step 4: 加弹窗状态与占用计算**

`data` 里的 `ruleModal` 改为：

```js
ruleModal: { visible: false, rule: new RoutingRule(), allUsers: true },
```

在 `new Vue({...})` 里、与 `data` / `methods` 平级加 `computed`：

```js
computed: {
    // occupiedInbounds 返回当前域名组下、除正在编辑的这条规则之外，
    // 每个入站已被哪条规则覆盖。key 是入站 id，value 是规则名。
    // key 0 是特殊项：存在「所有用户」规则时用它记录，它占住所有人。
    occupiedInbounds() {
        const map = {};
        const gid = this.ruleModal.rule.domainGroupId;
        if (!gid) return map;
        for (const r of this.rules) {
            if (r.id === this.ruleModal.rule.id) continue;
            if (r.domainGroupId !== gid) continue;
            const label = r.remark || ('规则 #' + r.id);
            if (!r.inboundIds.length) {
                if (map[0] === undefined) map[0] = label;
                continue;
            }
            for (const id of r.inboundIds) {
                if (map[id] === undefined) map[id] = label;
            }
        }
        return map;
    },
    allUsersDisabled() {
        return Object.keys(this.occupiedInbounds).length > 0;
    },
    allUsersDisabledReason() {
        const map = this.occupiedInbounds;
        if (map[0]) {
            return '该域名组已有覆盖所有用户的规则「' + map[0] + '」，不能再对它添加任何规则';
        }
        if (Object.keys(map).length > 0) {
            return '该域名组已有针对具体用户的规则，不能再选「所有用户」';
        }
        return '';
    },
},
```

**不要用 `watch` 监听 `ruleModal.rule.domainGroupId`。** Vue 2 的 watcher 是在 nextTick 异步触发的：`openRule` 先替换 `ruleModal.rule` 再把 `visible` 置真，等 watcher 跑起来时 `visible` 已经是 true，`if (!visible) return` 这种守卫拦不住它——打开编辑弹窗会顺手把已勾选的项过滤一遍。改用域名组下拉框自己的 `@change`，时序完全可控。

把域名组那个 `<a-form-item>` 里的 select 加上 `@change`：

```html
<a-form-item label="域名组">
    <a-select v-model="ruleModal.rule.domainGroupId" style="width: 100%;"
              @change="onDomainGroupChange">
        <a-select-option v-for="g in groups" :key="g.id" :value="g.id">
            [[ g.remark ]]
        </a-select-option>
    </a-select>
</a-form-item>
```

- [ ] **Step 5: 加 methods，改 `openRule` 与 `saveRule`**

`methods` 里加：

```js
inboundOccupiedBy(id) {
    const map = this.occupiedInbounds;
    return map[0] || map[id] || '';
},
toggleAllUsers(checked) {
    this.ruleModal.allUsers = checked;
    if (checked) this.ruleModal.rule.inboundIds = [];
},
toggleInbound(id, checked) {
    const ids = this.ruleModal.rule.inboundIds.slice();
    const idx = ids.indexOf(id);
    if (checked && idx < 0) ids.push(id);
    if (!checked && idx >= 0) ids.splice(idx, 1);
    // 升序提交，和后端 EncodeInboundIds 的存储顺序对齐
    this.ruleModal.rule.inboundIds = ids.sort((a, b) => a - b);
},
// 换了域名组，占用集合跟着变。已勾选但在新组下不可用的项必须剔除，
// 否则会带着一个必被后端拒绝的选择去点保存。
//
// 显式赋值再读 computed，不依赖 v-model 与 @change 的先后——computed 是
// 同步惰性求值，赋值之后立刻读到的就是新域名组的占用集合。
onDomainGroupChange(groupId) {
    this.ruleModal.rule.domainGroupId = groupId;
    const map = this.occupiedInbounds;
    if (map[0]) {
        this.ruleModal.allUsers = false;
        this.ruleModal.rule.inboundIds = [];
        return;
    }
    if (this.ruleModal.allUsers && Object.keys(map).length > 0) {
        this.ruleModal.allUsers = false;
    }
    this.ruleModal.rule.inboundIds =
        this.ruleModal.rule.inboundIds.filter(id => !map[id]);
},
```

`openRule` 改为：

```js
openRule(rule) {
    this.ruleModal.rule = rule
        ? RoutingRule.fromJson(JSON.parse(JSON.stringify(rule)))
        : new RoutingRule();
    this.ruleModal.allUsers = this.ruleModal.rule.inboundIds.length === 0;
    this.ruleModal.visible = true;
},
```

`saveRule` 改为：

```js
async saveRule() {
    const r = this.ruleModal.rule;
    const inboundIds = this.ruleModal.allUsers ? [] : r.inboundIds;
    if (!this.ruleModal.allUsers && !inboundIds.length) {
        // 空数组的语义是「所有用户」。静默提交等于把这条规则的作用域
        // 放大到全体，所以必须在这里拦住，不能让后端去猜用户的意图。
        this.$message.error('请至少勾选一个用户，或勾选「所有用户」');
        return;
    }
    const url = r.id ? 'xui/routing/rule/update/' + r.id : 'xui/routing/rule/add';
    const msg = await this.post(url, Object.assign({}, r, { inboundIds }));
    if (msg.success) {
        this.ruleModal.visible = false;
        await this.loadAll();
    }
},
```

- [ ] **Step 6: 扩展 `ruleIssue` 与 sniffing 检查**

把 `sniffingOff(rule)` 替换为按入站 id 判断的版本，并加两个函数：

```js
// 域名分流依赖 sniffing 拿到 SNI/Host。入站关掉 sniffing 或
// destOverride 不含 http/tls 时，域名规则永远不会命中。
sniffingOff(inboundId) {
    const inbound = this.inbounds.find(x => x.id === inboundId);
    if (!inbound) return false;
    let sniffing;
    try {
        sniffing = JSON.parse(inbound.sniffing || '{}');
    } catch (e) {
        return true;
    }
    if (!sniffing.enabled) return true;
    const dest = sniffing.destOverride || [];
    return !dest.includes('http') && !dest.includes('tls');
},
// 只检查规则显式指定的入站。「所有用户」规则不逐个检查——保持本功能
// 上线前的行为，避免这次改动顺带引入一堆无关的新告警。
sniffingOffNames(rule) {
    if (!rule.inboundIds.length) return [];
    return rule.inboundIds.filter(id => this.sniffingOff(id)).map(this.inboundName);
},
// ruleConflict 检出违反「同一域名组下每个入站至多被一条规则覆盖」的历史
// 数据。迁移不改动这类数据（改了就是在管理员不知情时改变分流行为），
// 只在这里指出来，由管理员决定改哪条。
ruleConflict(rule) {
    for (const other of this.rules) {
        if (other.id === rule.id) continue;
        if (other.domainGroupId !== rule.domainGroupId) continue;
        const label = other.remark || ('规则 #' + other.id);
        if (!rule.inboundIds.length || !other.inboundIds.length) {
            return '与规则「' + label + '」在同一域名组下重叠，实际生效的只有优先级靠前的那条';
        }
        const both = rule.inboundIds.filter(id => other.inboundIds.includes(id));
        if (both.length) {
            return '与规则「' + label + '」在用户 ' + both.map(this.inboundName).join('、') +
                ' 上重叠，实际生效的只有优先级靠前的那条';
        }
    }
    return '';
},
```

`ruleIssue` 替换为：

```js
// ruleIssue 返回空串表示这条规则会正常生效，否则返回失效原因。
//
// 前几种与后端 RoutingInjector.buildRule 的跳过条件一一对应：条件残缺的
// 规则整条不输出，配置里根本没有它，而规则表照常渲染——用户察觉不到。
// sniffing 那种规则会生成，但 xray 在路由阶段拿不到域名，永不命中。
// 最后一种是历史冲突数据，两条规则都会生成，只有一条真正起作用。
ruleIssue(rule) {
    if (!rule.enable) return '';
    if (rule.broken) return '规则的入站数据损坏，这条规则不会写进配置';
    const group = this.groups.find(x => x.id === rule.domainGroupId);
    if (!group) return '域名组已删除，这条规则不会写进配置';
    if (!group.effectiveCount) return '域名组为空，这条规则不会写进配置';

    if (rule.inboundIds.length) {
        const dead = rule.inboundIds.filter(id => {
            const i = this.inbounds.find(x => x.id === id);
            return !i || !i.enable;
        });
        if (dead.length === rule.inboundIds.length) {
            return '指定的入站已全部删除或禁用，这条规则不会写进配置';
        }
        if (dead.length) {
            return '有 ' + dead.length + ' 个入站已删除或已禁用（' +
                dead.map(this.inboundName).join('、') + '），这些用户不受本规则约束';
        }
    }
    if (rule.action === 'proxy') {
        const node = this.nodes.find(x => x.id === rule.outboundId);
        if (!node) return '出站节点已删除，这条规则不会写进配置';
        if (!node.enable) return '出站节点已禁用，这条规则不会写进配置';
    }
    const off = this.sniffingOffNames(rule);
    if (off.length) {
        return '入站 ' + off.join('、') +
            ' 未开启 sniffing（http/tls），xray 在路由阶段拿不到域名，对这些用户本规则永远不会命中';
    }
    return this.ruleConflict(rule);
},
```

- [ ] **Step 7: 跑模板不变量测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: PASS

这两个测试是必跑的：`getHtmlTemplate` 会吞掉 `ParseFS` 错误，模板语法错误 `go build` 发现不了；Vue 指令写到根元素外则完全静默。

- [ ] **Step 8: 起面板手工验证**

```bash
XUI_DEBUG=true go run main.go
```

浏览器打开分流管理页（调试模式从磁盘读模板与静态资源，但 JS 仍可能命中强缓存，**用硬刷新**）。逐项确认：

1. 添加分流规则 → 用户区是复选框，默认勾着「所有用户」
2. 取消「所有用户」、一个都不勾 → 点保存弹出「请至少勾选一个用户」，不发请求
3. 勾甲、乙保存 → 列表「用户」列显示两个 tag
4. 再添加一条规则、选同一个域名组 → 甲、乙置灰并注明被哪条规则覆盖，「所有用户」也置灰
5. 换成另一个域名组 → 置灰消失
6. 编辑第一条规则 → 甲、乙是勾上的且不被自己置灰
7. 禁用一个入站 → 该规则那一行出现黄色警告，说明这些用户不受约束

- [ ] **Step 9: Commit（需用户授权）**

```bash
git add web/assets/js/model/routing.js web/html/xui/routing.html
git commit -m "feat(routing): 分流规则用户改为复选框，冲突项置灰"
```

---

### Task 5: 文档同步

**Files:**
- Modify: `docs/superpowers/specs/2026-09-02-domain-routing-design.md`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: 前四个任务的最终形态
- Produces: 无

- [ ] **Step 1: 在原设计文档标注被修订的章节**

`docs/superpowers/specs/2026-09-02-domain-routing-design.md`：

1. 在文件顶部「状态：待评审」下方加一行：

```markdown
> **部分内容已被 `2026-09-02-routing-multi-inbound-design.md` 修订**：§3.2 的 `RoutingRule` 表结构、§3.3 中 `InboundId` 相关的两条设计点、§5.3 的第二道防线清单。其余内容继续有效。
```

2. §3.2 的 `RoutingRule` 代码块里，把 `InboundId` 那一行改成：

```go
    InboundIds    string `json:"inboundIds"`    // JSON 整数数组，[] = 所有入站；见多入站设计文档
```

3. §3.3 的「规则存 `InboundId` 而非 tag」与「`InboundId = 0` 表示全局规则」两段，段首各加一句：

```markdown
（已修订：字段现为 `InboundIds`，一条规则可覆盖多个入站，空数组表示全局。存 id 不存 tag 的理由不变。）
```

4. §5.3「第二道 · 生成期跳过」的清单末尾加一条：

```markdown
- 规则指定的入站**全部**不存在或已禁用 → **整条规则跳过，不输出**。绝不能退而求其次输出空的 `inboundTag`：实测确认 xray 把它当作「不限制」，规则会从「只覆盖甲」放大成「劫持所有人」。部分入站失效时只剔除失效的那些，规则照常生成。
```

- [ ] **Step 2: 更新 `CLAUDE.md`**

在「域名分流管理 → 数据模型」小节：

1. 三张表的示意里，`RoutingRule` 那一行改成：

```
RoutingRule   分流规则   InboundIds(JSON 数组) × DomainGroupId → Action(proxy|block) + OutboundId + Priority + Enable
```

2. 「三条不可动摇的字段约定」的前两条改写为：

```markdown
- **规则存 `InboundIds`（入站 id 的 JSON 数组）而不是 tag 字符串。** 入站 tag 由端口算出（`UpdateInbound` 里 `Tag = fmt.Sprintf("inbound-%v", Port)`），用户改端口 tag 就变，存字符串会让规则静默失效。数组升序去重存储，这是「生成逐字节确定」的一部分。
- **`InboundIds` 为空数组 `[]` 表示对所有入站生效**（全局规则），生成时不输出 `inboundTag`。**注意空数组与「一个都没选」在提交体里无法区分**，写入路径用 `EncodeInboundIdsStrict` 挡住「非空输入被过滤成空」，前端 `saveRule` 也拦一道——否则一条本该覆盖某个人的规则会被静默放大到全体。
- **同一个域名组下，任何一个入站至多被一条规则覆盖**（`RoutingRuleService.checkConflict`）。禁用的规则同样占位。只在写入路径校验，生成期不干预：历史冲突数据照常生成两条规则，由界面标黄交给管理员处理。
```

3. 在「xray 会静默接受错误配置」那张表里追加一行：

```markdown
| 规则的 `inboundTag` 为**空数组** | `Configuration OK` | 与 `domain` 空数组同构：xray 当作「不限制」，规则从「只覆盖甲」放大成**覆盖所有入站**（Xray 26.7.28 实测：两个入站都被命中，对照域名放行） |
```

4. 「配置注入的四条不变量」第 3 条末尾补一句：

```markdown
入站条件同理：`buildRule` 会剔除已删除/已禁用的入站，**剔完为空则整条丢弃**，绝不输出空的 `inboundTag`。
```

- [ ] **Step 3: 通读确认没有残留的旧描述**

Run: `grep -rn "InboundId\b" CLAUDE.md docs/superpowers/specs/`
Expected: 只剩下带「已修订」标注的历史描述，没有把 `InboundId` 当作现役字段的句子

- [ ] **Step 4: 最终全量验证**

```bash
go build ./... && go vet ./...
go test ./web/service/... ./database/... ./web/ ./util/...
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o /tmp/a-ui-build main.go && rm -f /tmp/a-ui-build
git status --short
git diff
```

确认：没有越界修改、没有调试残留、没有临时文件。

- [ ] **Step 5: Commit（需用户授权）**

```bash
git add CLAUDE.md docs/superpowers/specs/2026-09-02-domain-routing-design.md
git commit -m "docs(routing): 同步多入站改造后的字段约定与不变量"
```
