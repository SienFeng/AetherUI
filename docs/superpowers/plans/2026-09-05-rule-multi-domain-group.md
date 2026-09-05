# 分流规则多域名组（复选框）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一条分流规则可以同时引用多个域名组，交互与「用户（入站）」的复选框完全对称；生成期把各组域名合并成一份 `domain` 列表，输出一条 xray 规则。

**Architecture:** 在 `RoutingRule` 上新增 JSON 整数数组列 `DomainGroupIds`（升序去重），保留旧列 `DomainGroupId` 不删以支持版本回退；启动时幂等回填。冲突判定从「域名组相同」推广为「域名组集合相交 且 入站集合相交」，SQL 过滤改为逐条解码。生成期逐组取域名后跨组合并去重，失效的组剔除而非整条丢弃。

**Tech Stack:** Go 1.27 + GORM/SQLite + Gin；前端 Vue 2 + ant-design-vue 服务端模板（无打包工具）。测试为标准 `go test`，门禁是 `make verify`（vet + test + build）。

**Spec:** `docs/superpowers/specs/2026-09-05-rule-multi-domain-group-design.md`

## Global Constraints

以下约束适用于**每一个** Task，不再逐条重复：

- **`DomainGroupIds` 的空数组 `[]` 是非法值，不是「所有域名组」。** 与 `InboundIds` 的 `[]`（合法，表示「所有用户」）语义相反。任何一处照抄入站的空值处理，都会让 `domain` 条件为空、xray 把规则当作「不限制」，规则从「这批域名走 B」退化成「该用户全部流量走 B」，且返回 `Configuration OK`、面板首页显示 `running`。见 spec §3。
- **生成必须逐字节确定。** `DomainGroupIds` 升序去重存储；`buildRule` 按该顺序逐组取域名再合并、保留首次出现。**禁止遍历 map 产生数组顺序。** 顺序一抖动，`Config.Equals` 恒为 false，`InboundController` 那个 10 秒 cron 会不停重启 xray。
- **不需要扩展 `xray.Config.Equals` / `InboundConfig.Equals`。** 域名组落在 `RouterConfig` 这个 `json_util.RawMessage` 字段，按字节比较。
- **冲突错误信息里的「冲突」二字必须保留。** `web/service/routing_portable.go` 的 `importRules` 用 `strings.Contains(err.Error(), "冲突")` 把这类错误归进 `Skipped` 而非 `Failed`，是导入幂等性的依赖。
- **`DomainGroupId` 旧列保留，不从结构体删除、不 drop。** 用于版本回退，见 spec §4.1、§11。新代码只读写 `DomainGroupIds`。
- **写入路径一律用 `EncodeDomainGroupIdsStrict`**，不用非 Strict 版本。
- 运行单个测试：`go test ./web/service/ -run TestName -v`（`web/service` 的 `TestMain` 会 `os.Chdir` 到仓库根，命令在仓库根执行即可）。数据库测试：`go test ./database/ -run TestName -v`。
- 每个 Task 结束时工作区必须全绿：`go build ./...` 通过、该 Task 涉及的包测试通过。

---

## 文件结构

| 文件 | 职责 | Task |
|---|---|---|
| `web/service/routing_rule.go` | 编解码、集合相交、校验、冲突、引用守卫 | 1, 4 |
| `web/service/routing_rule_test.go` | 上述的测试 + 共享测试夹具 | 1, 3, 4 |
| `database/model/routing.go` | `RoutingRule.DomainGroupIds` 字段 | 2 |
| `database/db.go` | `migrateRoutingRuleDomainGroupIds` | 2 |
| `database/routing_migrate_test.go` | 迁移测试 | 2 |
| `web/service/routing_domain.go` | `MergeDomains` 改变参 | 5 |
| `web/service/routing_inject.go` | `buildRule` 跨组合并与剔除 | 5 |
| `web/service/routing_inject_test.go` | 生成期测试 + `mustEncodeIds` | 3, 5 |
| `web/controller/routing.go` | 表单 / 视图 / 转换 | 6 |
| `web/service/routing_portable.go` | 导出结构、`toPortableRule`、`importRules` | 7 |
| `web/service/routing_portable_test.go` | 导入导出测试 | 3, 7 |
| `web/assets/js/model/routing.js` | 前端 `RoutingRule` 模型 | 8 |
| `web/html/xui/routing.html` | 弹窗复选框、列表列、四处判定 | 8 |
| `CLAUDE.md` | 记录新不变量与回退风险 | 9 |

---

### Task 1: 编解码函数与集合相交

纯新增函数，不改动任何现有代码路径。做完后现有测试应当全部照常通过。

**Files:**
- Modify: `web/service/routing_rule.go`（在文件末尾 `DecodeInboundIds` 之后追加）
- Test: `web/service/routing_rule_test.go`（在文件末尾追加）

**Interfaces:**
- Produces:
  - `func EncodeDomainGroupIds(ids []int) (string, error)`
  - `func EncodeDomainGroupIdsStrict(ids []int) (string, error)`
  - `func DecodeDomainGroupIds(encoded string) ([]int, error)`
  - `func intersectGroups(a, b []int) (bool, int)`（包内私有）
  - 测试辅助 `func mustEncodeGroupIds(t *testing.T, ids []int) string`（Task 3、5、7 复用）

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_rule_test.go` 末尾：

```go
// mustEncodeGroupIds 是测试夹具共用的域名组编码器。用非 Strict 版本，
// 因为部分用例要故意造出「空集合」这种非法状态来验证下游会拒绝它。
func mustEncodeGroupIds(t *testing.T, ids []int) string {
	t.Helper()
	encoded, err := EncodeDomainGroupIds(ids)
	if err != nil {
		t.Fatalf("EncodeDomainGroupIds: %v", err)
	}
	return encoded
}

func TestEncodeDomainGroupIdsSortsAndDedupes(t *testing.T) {
	got, err := EncodeDomainGroupIds([]int{3, 1, 3, 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "[1,2,3]" {
		t.Errorf("got %q, want [1,2,3]", got)
	}
}

// 与 EncodeInboundIdsStrict 的关键分歧：入站那边「原始列表本来就空」是
// 合法的（= 所有用户），域名组这边必须报错——空的 domain 条件会让规则
// 劫持该用户的全部流量。
func TestEncodeDomainGroupIdsStrictRejectsEmptyInput(t *testing.T) {
	if _, err := EncodeDomainGroupIdsStrict(nil); err == nil {
		t.Error("empty input must be rejected: [] would make the domain condition empty")
	}
	if _, err := EncodeDomainGroupIdsStrict([]int{}); err == nil {
		t.Error("empty slice must be rejected")
	}
}

func TestEncodeDomainGroupIdsStrictRejectsAllInvalid(t *testing.T) {
	if _, err := EncodeDomainGroupIdsStrict([]int{0, -1}); err == nil {
		t.Error("all-invalid input must be rejected, not silently collapse to []")
	}
}

func TestEncodeDomainGroupIdsStrictAcceptsValid(t *testing.T) {
	got, err := EncodeDomainGroupIdsStrict([]int{2, 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "[1,2]" {
		t.Errorf("got %q, want [1,2]", got)
	}
}

// 空串/null 不报错：迁移回填前、直接改库、并发写入都可能留下空值，
// 在解码这一层报错会让整份配置生成失败。交给 validate / buildRule
// 各自按空集合处理（都会拒绝）。
func TestDecodeDomainGroupIdsTreatsBlankAsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "null"} {
		got, err := DecodeDomainGroupIds(raw)
		if err != nil {
			t.Errorf("DecodeDomainGroupIds(%q) returned error: %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("DecodeDomainGroupIds(%q) = %v, want empty", raw, got)
		}
	}
}

func TestDecodeDomainGroupIdsRejectsCorruptData(t *testing.T) {
	if _, err := DecodeDomainGroupIds("{oops"); err == nil {
		t.Error("corrupt JSON must return an error so the rule is dropped whole")
	}
}

// intersectGroups 绝不能复用 intersectInbounds：后者把空切片当全集，
// 而域名组的空集合是非法值。复用会让两条各自损坏的规则被判成互相冲突，
// 把管理员锁在门外——既修不了旧规则，也建不了新规则。
func TestIntersectGroupsTreatsEmptyAsEmptyNotUniversal(t *testing.T) {
	if ok, _ := intersectGroups(nil, []int{1}); ok {
		t.Error("empty set must not intersect anything")
	}
	if ok, _ := intersectGroups([]int{1}, nil); ok {
		t.Error("empty set must not intersect anything")
	}
	if ok, _ := intersectGroups(nil, nil); ok {
		t.Error("two empty sets must not intersect")
	}
}

func TestIntersectGroupsReportsSmallestSharedId(t *testing.T) {
	ok, who := intersectGroups([]int{2, 5, 9}, []int{5, 9})
	if !ok {
		t.Fatal("expected intersection")
	}
	// b 已升序，取到的是最小的相交 id，保证错误信息稳定可测。
	if who != 5 {
		t.Errorf("who = %d, want 5", who)
	}
}

func TestIntersectGroupsDisjoint(t *testing.T) {
	if ok, _ := intersectGroups([]int{1, 2}, []int{3, 4}); ok {
		t.Error("disjoint sets must not intersect")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run 'TestEncodeDomainGroupIds|TestDecodeDomainGroupIds|TestIntersectGroups' -v`
Expected: 编译失败，`undefined: EncodeDomainGroupIds` 等。

- [ ] **Step 3: 实现**

追加到 `web/service/routing_rule.go` 末尾：

```go
// EncodeDomainGroupIds 把域名组 id 列表编成入库格式：丢弃非正数、去重、升序。
//
// 升序去重是「生成逐字节确定」的一部分：buildRule 按这个顺序逐组取域名再
// 合并，顺序一抖动，Config.Equals 恒为 false，那个 10 秒的重启 cron 会不停
// 重启 xray。
//
// 注意本函数会丢弃非正数，因此 [0] 这类输入会得到 "[]"——而空的域名组集合
// 会让规则的 domain 条件为空。写入路径一律用 EncodeDomainGroupIdsStrict。
func EncodeDomainGroupIds(ids []int) (string, error) {
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

// EncodeDomainGroupIdsStrict 是写入路径该用的版本。
//
// 与 EncodeInboundIdsStrict 的关键分歧：那边只在「原始非空却编出 []」时报错，
// 因为入站的空列表是用户通过「所有用户」复选框显式表达的合法语义。域名组
// 没有对应的概念——空的域名组集合意味着 domain 条件为空，xray 会把它当作
// 「不限制」，规则从「这批域名走 B」退化成「该用户全部流量走 B」，且返回
// Configuration OK、面板首页显示 running。所以这里对空结果一律报错，
// 无论原始列表是否为空。
func EncodeDomainGroupIdsStrict(ids []int) (string, error) {
	encoded, err := EncodeDomainGroupIds(ids)
	if err != nil {
		return "", err
	}
	if encoded == "[]" {
		if len(ids) == 0 {
			return "", common.NewError("必须至少指定一个域名组")
		}
		return "", common.NewError("域名组选择非法：提交了", len(ids),
			"个域名组，但没有一个是有效的域名组 id")
	}
	return encoded, nil
}

// DecodeDomainGroupIds 是 EncodeDomainGroupIds 的逆操作。
//
// 空字符串与 "null" 当作空切片且不报错：迁移会回填，但直接改库、并发写入
// 等路径仍可能留下空值，在这里报错会让整份配置生成失败。空切片本身是非法
// 状态，由 validate（拒绝写入）与 buildRule（整条丢弃）各自处理。
// 真正的语法错误仍返回 error，交给调用方整条丢弃该规则。
func DecodeDomainGroupIds(encoded string) ([]int, error) {
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

// intersectGroups 判断两个域名组集合是否相交。
//
// 与 intersectInbounds 的关键分歧：那边把空切片当全集（「所有用户」），
// 这里的空集合是非法值而不是全集，绝不能复用——复用会让两条各自损坏的
// 规则被判成互相冲突，管理员既修不了旧规则也建不了新规则。
//
// 第二个返回值是相交的最小 id（b 已升序），保证错误信息稳定可测。
func intersectGroups(a, b []int) (bool, int) {
	if len(a) == 0 || len(b) == 0 {
		return false, 0
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./web/service/ -run 'TestEncodeDomainGroupIds|TestDecodeDomainGroupIds|TestIntersectGroups' -v`
Expected: 全部 PASS。

Run: `go test ./web/service/`
Expected: PASS（本 Task 未改动任何现有路径）。

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_rule.go web/service/routing_rule_test.go
git commit -m "feat(routing): 域名组 id 数组的编解码与集合相交"
```

---

### Task 2: 数据模型字段与迁移

**Files:**
- Modify: `database/model/routing.go`（`RoutingRule` 结构体）
- Modify: `database/db.go`（新增迁移函数，在 `initRouting()` 末尾调用）
- Test: `database/routing_migrate_test.go`

**Interfaces:**
- Consumes: 无
- Produces: `model.RoutingRule.DomainGroupIds string`；`migrateRoutingRuleDomainGroupIds() error`（包内私有）

- [ ] **Step 1: 写失败的测试**

追加到 `database/routing_migrate_test.go` 末尾：

```go
// 旧库里的规则只有 domain_group_id。迁移必须把它搬到 domain_group_ids，
// 且绝不改变任何一条规则的实际生效范围。
//
// domain_group_id <= 0 是 validate 挡不住的脏数据（直接改库可以造出来），
// 回填成 [] 后 buildRule 会因「合并后域名为空」整条丢弃——与迁移前
// domainGroupService.Get(0) 失败后跳过整条的行为完全一致。
func TestMigrateRoutingRuleDomainGroupIds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := GetDB()

	insert := func(remark string, groupId int, groupIds string) {
		t.Helper()
		err := db.Exec(`INSERT INTO routing_rules
			(remark, inbound_ids, domain_group_id, domain_group_ids, action, outbound_id, priority, enable)
			VALUES (?, '[]', ?, ?, 'block', 0, 0, 1)`, remark, groupId, groupIds).Error
		if err != nil {
			t.Fatalf("insert %s: %v", remark, err)
		}
	}
	insert("普通规则", 7, "")
	insert("脏数据", 0, "")
	insert("已迁移过的", 3, "[3,9]")

	if err := migrateRoutingRuleDomainGroupIds(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	read := func(remark string) string {
		t.Helper()
		var got string
		err := db.Raw("SELECT domain_group_ids FROM routing_rules WHERE remark = ?", remark).
			Scan(&got).Error
		if err != nil {
			t.Fatalf("read %s: %v", remark, err)
		}
		return got
	}
	if got := read("普通规则"); got != "[7]" {
		t.Errorf("普通规则 = %q, want [7]", got)
	}
	if got := read("脏数据"); got != "[]" {
		t.Errorf("脏数据 = %q, want []", got)
	}
	// 已经有值的行绝不能被覆盖，否则多组规则会在每次重启时被压回单组。
	if got := read("已迁移过的"); got != "[3,9]" {
		t.Errorf("已迁移过的 = %q, want [3,9]（不得被覆盖）", got)
	}

	// 幂等：面板每次启动都会跑，重启多少次都必须安全。
	if err := migrateRoutingRuleDomainGroupIds(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if got := read("普通规则"); got != "[7]" {
		t.Errorf("第二次迁移后 普通规则 = %q, want [7]", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./database/ -run TestMigrateRoutingRuleDomainGroupIds -v`
Expected: 编译失败，`undefined: migrateRoutingRuleDomainGroupIds`。

- [ ] **Step 3: 加字段**

在 `database/model/routing.go` 的 `RoutingRule` 里，把 `DomainGroupId` 那一行替换成：

```go
	// DomainGroupIds 是这条规则引用的域名组 id，JSON 整数数组，升序去重存储。
	//
	// 升序去重与 InboundIds 同理，是「生成逐字节确定」的一部分：buildRule
	// 按这个顺序逐组取域名再合并，顺序一抖动，Config.Equals 恒为 false，
	// 那个 10 秒的重启 cron 会不停重启 xray。
	//
	// 与 InboundIds 的空数组语义【相反】：这里的 [] 非法，绝不表示「所有
	// 域名组」。域名条件为空会让 xray 把规则当作「不限制」，从「这批域名走
	// B」退化成「该用户全部流量走 B」，且 Configuration OK、面板显示 running。
	DomainGroupIds string `json:"domainGroupIds" form:"domainGroupIds"`
	// DomainGroupId 是多域名组改造前的单值字段，新代码一律不再读写它。
	//
	// 有意保留不删（GORM 的 sqlite AutoMigrate 本来也不删列）：万一管理员
	// 回滚到旧版本二进制，旧代码读到的还是原值，单组规则行为完全正常；
	// 删掉列则每条规则都读成 0，buildRule 全部丢弃——分流静默全灭，而面板
	// 首页仍显示 running。改造后新建的多组规则该值为 0，旧代码会整条丢弃，
	// 即分流范围缩小而非放大，安全侧正确。
	DomainGroupId int `json:"domainGroupId" form:"domainGroupId"`
```

- [ ] **Step 4: 加迁移**

在 `database/db.go` 的 `migrateRoutingRuleInboundIds` 之后追加：

```go
// migrateRoutingRuleDomainGroupIds 把旧的单域名组字段 domain_group_id
// 搬到 domain_group_ids。
//
// 幂等：只回填 domain_group_ids 为空的行，面板每次启动都会跑，重启多少次
// 都安全。已有值的行绝不覆盖，否则多组规则每次重启都会被压回单组。
//
// domain_group_id <= 0 是 validate 挡不住的脏数据（直接改库可以造出来），
// 回填成 [] 后 buildRule 会因「合并后域名为空」整条丢弃并记 Warning——
// 与迁移前 domainGroupService.Get(0) 失败后跳过整条的行为完全一致。
// 迁移不改变任何一条规则的实际生效范围。
//
// domain_group_id 列有意保留不删，理由见 model.RoutingRule 的字段注释。
func migrateRoutingRuleDomainGroupIds() error {
	if !db.Migrator().HasColumn(&model.RoutingRule{}, "domain_group_id") {
		return nil
	}
	return db.Exec(`
UPDATE routing_rules
SET domain_group_ids = CASE WHEN domain_group_id > 0
                            THEN '[' || domain_group_id || ']' ELSE '[]' END
WHERE domain_group_ids IS NULL OR domain_group_ids = ''`).Error
}
```

把 `initRouting()` 的最后一行 `return migrateRoutingRuleInboundIds()` 改成：

```go
	if err := migrateRoutingRuleInboundIds(); err != nil {
		return err
	}
	return migrateRoutingRuleDomainGroupIds()
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./database/ -v`
Expected: 全部 PASS，含新增的 `TestMigrateRoutingRuleDomainGroupIds`。

Run: `go build ./...`
Expected: 通过（新字段还没有消费者）。

- [ ] **Step 6: 提交**

```bash
git add database/model/routing.go database/db.go database/routing_migrate_test.go
git commit -m "feat(routing): RoutingRule 增加 DomainGroupIds 列与幂等回填迁移"
```

---

### Task 3: 测试夹具双写过渡

把 6 个测试文件里 60 处 `DomainGroupId: X` 改成**同时**设置两个字段。这一步不碰任何生产代码，改完测试仍应全绿——目的是让 Task 4/5 切换实现时，测试在切换前后都能通过，实现改动与夹具改动分成两次可独立回滚的提交。

**Files:**
- Modify: `web/service/routing_rule_test.go`、`web/service/routing_inject_test.go`、`web/service/routing_portable_test.go`、`web/service/routing_e2e_test.go`、`web/service/geo_test.go`、`web/service/xray_hot_reload_e2e_test.go`

**Interfaces:**
- Consumes: `mustEncodeGroupIds(t, ids)`（Task 1）
- Produces: 所有测试夹具同时携带 `DomainGroupId` 与 `DomainGroupIds`

- [ ] **Step 1: 机械替换**

在仓库根执行（macOS 的 `sed -i` 需要备份后缀参数，这里用 perl 避免平台差异）：

```bash
perl -pi -e 's/DomainGroupId: ([A-Za-z0-9_.]+),/DomainGroupId: $1, DomainGroupIds: mustEncodeGroupIds(t, []int{$1}),/g' \
  web/service/routing_rule_test.go \
  web/service/routing_inject_test.go \
  web/service/routing_portable_test.go \
  web/service/routing_e2e_test.go \
  web/service/geo_test.go \
  web/service/xray_hot_reload_e2e_test.go
gofmt -w web/service/*_test.go
```

- [ ] **Step 2: 确认替换数量与残留**

Run:
```bash
grep -c "DomainGroupIds: mustEncodeGroupIds" web/service/*_test.go
grep -n "DomainGroupId: [A-Za-z0-9_.]*," web/service/*_test.go | grep -v DomainGroupIds
```
Expected: 第一条合计 60 处；第二条**无输出**（没有漏网的单写点）。若第二条有输出，多半是跨行的结构体字面量，手工补上同样的 `DomainGroupIds`。

- [ ] **Step 3: 跑全部测试确认仍然全绿**

Run: `go test ./web/service/`
Expected: PASS。生产代码此刻仍只读 `DomainGroupId`，新字段是纯冗余，行为不变。

- [ ] **Step 6: 提交**

```bash
git add web/service/*_test.go
git commit -m "test(routing): 测试夹具同时写入 DomainGroupId 与 DomainGroupIds"
```

---

### Task 4: 服务层切到多域名组

**Files:**
- Modify: `web/service/routing_rule.go:53`（`validate`）、`:119`（`groupLabel`）、`:140`（`checkConflict`）、`:191`（`Update`）、`:221`（`CheckDomainGroupRefs`）
- Test: `web/service/routing_rule_test.go`

**Interfaces:**
- Consumes: `EncodeDomainGroupIdsStrict`、`DecodeDomainGroupIds`、`intersectGroups`（Task 1）；`model.RoutingRule.DomainGroupIds`（Task 2）
- Produces: `validate` / `checkConflict` / `CheckDomainGroupRefs` 全部按域名组集合工作

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_rule_test.go` 末尾：

```go
// addMultiGroupRule 建一条引用多个域名组的规则。
func addMultiGroupRule(t *testing.T, groupIds []int, inboundIds []int, remark string) error {
	t.Helper()
	return (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark:         remark,
		InboundIds:     mustEncodeIds(t, inboundIds),
		DomainGroupIds: mustEncodeGroupIds(t, groupIds),
		Action:         model.ActionBlock,
		Enable:         true,
	})
}

func TestAddRuleRejectsEmptyDomainGroups(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "没选域名组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: "[]", Action: model.ActionBlock, Enable: true,
	})
	if err == nil {
		t.Fatal("空域名组集合必须被拒绝：domain 条件为空会劫持该用户的全部流量")
	}
}

func TestAddRuleRejectsAnyMissingDomainGroup(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	err := addMultiGroupRule(t, []int{g.Id, 999}, []int{in.Id}, "一个存在一个不存在")
	if err == nil {
		t.Fatal("引用了不存在的域名组必须被拒绝")
	}
}

func TestAddRuleAcceptsMultipleDomainGroups(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id, chatgpt.Id}, []int{in.Id}, "两组"); err != nil {
		t.Fatalf("多域名组规则必须被接受: %v", err)
	}
}

// spec §2.3 的表格逐行落成用例。冲突判定的单位是「域名组 × 用户」的组合。
func TestConflictGroupSetsPartiallyOverlapSameUser(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id, chatgpt.Id}, []int{in.Id}, "两组走 A"); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := addMultiGroupRule(t, []int{chatgpt.Id}, []int{in.Id}, "ChatGPT 走 B")
	if err == nil {
		t.Fatal("组集合在 ChatGPT 上相交且用户相同，必须拒绝")
	}
	if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误信息必须含「冲突」二字（importRules 靠它归类）: %v", err)
	}
	if !strings.Contains(err.Error(), "ChatGPT") {
		t.Errorf("错误信息必须点名相交的那个域名组: %v", err)
	}
}

func TestConflictAllowsDisjointGroupSetsSameUser(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id}, []int{in.Id}, "Claude 走 A"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := addMultiGroupRule(t, []int{chatgpt.Id}, []int{in.Id}, "ChatGPT 走 B"); err != nil {
		t.Fatalf("组集合不相交必须被接受: %v", err)
	}
}

func TestConflictAllowsSameGroupSetDifferentUsers(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	a := newTestInbound(t, 10001)
	b := newTestInbound(t, 10002)
	if err := addMultiGroupRule(t, []int{claude.Id}, []int{a.Id}, "甲"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := addMultiGroupRule(t, []int{claude.Id}, []int{b.Id}, "乙"); err != nil {
		t.Fatalf("同组不同人必须被接受: %v", err)
	}
}

// 引用守卫：解码失败时必须拦住删除。SQLite 复用自增 id，孤儿规则会静默
// 绑到新建的域名组上，那时引用不再悬空，生成期的跳过防线也拦不住。
func TestCheckDomainGroupRefsSeesIdInTheMiddleOfAMultiGroupRule(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	banned := newTestGroup(t, "违规")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{claude.Id, chatgpt.Id, banned.Id}, []int{in.Id}, "三组"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := (&RoutingRuleService{}).CheckDomainGroupRefs(chatgpt.Id); err == nil {
		t.Error("被引用的域名组（位于数组中间）必须拦住删除")
	}
}

func TestCheckDomainGroupRefsBlocksDeletionOnCorruptData(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	if err := addMultiGroupRule(t, []int{g.Id}, []int{in.Id}, "好规则"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// 绕过 service 直接写坏数据，模拟直接改库 / 并发写入留下的残骸。
	err := database.GetDB().Exec(
		"UPDATE routing_rules SET domain_group_ids = '{oops' WHERE remark = ?", "好规则").Error
	if err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if err := (&RoutingRuleService{}).CheckDomainGroupRefs(g.Id); err == nil {
		t.Error("解码失败时必须拦住删除，不能放行")
	}
}
```

`routing_rule_test.go` 顶部的 import 需要 `strings` 与 `a-ui/database`；若尚未引入，补上。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run 'TestAddRuleRejectsEmptyDomainGroups|TestConflictGroupSets|TestCheckDomainGroupRefsSeesId' -v`
Expected: FAIL —— `validate` 还在读 `DomainGroupId`，空集合会因「域名组不存在: 0」被拒（碰巧通过），而多组用例会因 `DomainGroupId` 为 0 全部被拒。

- [ ] **Step 3: 改 `validate`**

把 `web/service/routing_rule.go` 的 `validate` 开头那三行替换成：

```go
func (s *RoutingRuleService) validate(rule *model.RoutingRule) error {
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return common.NewError("域名组数据损坏:", err)
	}
	// 空集合绝不放行：domain 条件为空会让规则劫持该用户的全部流量，
	// 而 xray 返回 Configuration OK，没有任何一层会报错。
	if len(groupIds) == 0 {
		return common.NewError("必须至少指定一个域名组")
	}
	for _, id := range groupIds {
		if _, err := s.domainGroupService.Get(id); err != nil {
			return common.NewError("域名组不存在:", id)
		}
	}
	switch rule.Action {
```

- [ ] **Step 4: 改 `groupLabel` 与 `checkConflict`**

`groupLabel` 保持原样（按单个 id 取备注，冲突文案要点名相交的那一个）。

把 `checkConflict` 整个函数体替换成：

```go
func (s *RoutingRuleService) checkConflict(rule *model.RoutingRule) error {
	ids, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return common.NewError("入站数据损坏:", err)
	}
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return common.NewError("域名组数据损坏:", err)
	}

	// 域名组是 JSON 数组列，没法再用 WHERE domain_group_id = ? 交给 SQL 过滤，
	// 只能读出全部规则逐条解码。规则是几十条量级，这点开销换掉一张关联表是
	// 划算的——与 CheckInboundRefs 同一个取舍。
	others, err := s.GetAll()
	if err != nil {
		return err
	}

	for _, other := range others {
		if other.Id == rule.Id {
			continue
		}
		otherGroupIds, decodeErr := DecodeDomainGroupIds(other.DomainGroupIds)
		if decodeErr != nil {
			// 无从判断它覆盖了哪些组。不拦——它自己已经会被 buildRule 整条
			// 丢弃，再拿它去挡别人只会让管理员既修不了旧规则也建不了新规则。
			continue
		}
		sharedGroup, whichGroup := intersectGroups(groupIds, otherGroupIds)
		if !sharedGroup {
			continue
		}
		otherIds, decodeErr := DecodeInboundIds(other.InboundIds)
		if decodeErr != nil {
			continue
		}
		overlap, who := intersectInbounds(ids, otherIds)
		if !overlap {
			continue
		}
		// 用 NewErrorf 而不是 NewError：后者走 fmt.Sprintln，会在每个参数
		// 之间插空格，拼出「与分流规则「 甲 」冲突」这种带空隙的句子。
		//
		// 注意：routing_portable.go 的 importRules 用
		// strings.Contains(err.Error(), "冲突") 把这类错误识别成「本机已存在
		// 同覆盖范围的规则」（计入 Skipped 而不是 Failed，导入才能保持幂等）。
		// 改这句文案（尤其是去掉「冲突」二字）前，先去同步看那一处。
		return common.NewErrorf(
			"与分流规则「%s」冲突：%s在域名组「%s」下已被它覆盖。同一个用户在同一个域名组下只能有一条规则。",
			ruleLabel(other), inboundLabel(who), s.groupLabel(whichGroup))
	}
	return nil
}
```

同时把 `checkConflict` 上方的文档注释里「当且仅当域名组相同且集合相交」改成「当且仅当**域名组集合相交**且入站集合相交」，其余段落不动。

- [ ] **Step 5: 改 `Update` 与 `CheckDomainGroupRefs`**

`Update` 里把 `old.DomainGroupId = rule.DomainGroupId` 改成：

```go
	old.DomainGroupIds = rule.DomainGroupIds
```

（不再写 `DomainGroupId`——它只作为回退用的历史值保留，不该被新代码改动。）

`CheckDomainGroupRefs` 整个替换：

```go
// CheckDomainGroupRefs 在删除域名组前调用。
//
// 域名组一旦消失，引用它的规则会少掉这一组的域名；若它是规则引用的唯一
// 一组，合并结果为空，buildRule 会整条丢弃——本该走指定节点或被封禁的
// 流量静默退回直连。
//
// 更危险的是 SQLite 会复用自增主键 id：删掉 Claude 组再新建 ChatGPT 组
// 可能拿到同一个 id，孤儿规则会静默变成「ChatGPT 的域名走 Claude 的节点」。
// 那时引用不再悬空，生成期的跳过防线拦不住，规则列表还会渲染得完全正常。
//
// DomainGroupIds 是 JSON 数组列，没法交给 SQL 去数，只能读出来逐条解码，
// 与 CheckInboundRefs 同形。
func (s *RoutingRuleService) CheckDomainGroupRefs(groupId int) error {
	if groupId <= 0 {
		return nil
	}
	rules, err := s.GetAll()
	if err != nil {
		return err
	}
	count := 0
	for _, rule := range rules {
		ids, decodeErr := DecodeDomainGroupIds(rule.DomainGroupIds)
		if decodeErr != nil {
			// 数据损坏时无从判断这条规则引用了哪些组。宁可拦住删除：放行
			// 的话，SQLite 复用 id 后这条规则可能静默绑到新建的域名组上。
			return common.NewError("分流规则", rule.Id,
				"的域名组数据已损坏，无法确认引用关系，请先修复或删除该规则")
		}
		for _, id := range ids {
			if id == groupId {
				count++
				break
			}
		}
	}
	if count > 0 {
		return common.NewError("该域名组仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./web/service/ -run 'Rule|Conflict|DomainGroupRefs' -v`
Expected: 全部 PASS。

Run: `go test ./web/service/`
Expected: PASS。Task 3 的夹具双写让所有旧用例继续满足新读法。

- [ ] **Step 7: 提交**

```bash
git add web/service/routing_rule.go web/service/routing_rule_test.go
git commit -m "feat(routing): 校验、冲突判定与引用守卫切到域名组集合"
```

---

### Task 5: 生成期跨组合并

**Files:**
- Modify: `web/service/routing_domain.go:291`（`MergeDomains` 改变参）
- Modify: `web/service/routing_inject.go:211`（`buildRule` 的域名部分）
- Test: `web/service/routing_inject_test.go`

**Interfaces:**
- Consumes: `DecodeDomainGroupIds`（Task 1）
- Produces: `func MergeDomains(lists ...[]string) []string`；`buildRule` 按 spec §6 的流程工作

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_inject_test.go` 末尾：

```go
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
			Domain []string `json:"domain"`
			Inbound []string `json:"inboundTag"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &router); err != nil {
		t.Fatalf("unmarshal router: %v", err)
	}
	for _, r := range router.Rules {
		if len(r.Inbound) > 0 && len(r.Domain) == 0 {
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
			Domain  []string `json:"domain"`
			Inbound []string `json:"inboundTag"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(cfg.RouterConfig, &router); err != nil {
		t.Fatalf("unmarshal router: %v", err)
	}
	for _, r := range router.Rules {
		if len(r.Inbound) > 0 && len(r.Domain) == 0 {
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
```

`routing_inject_test.go` 顶部 import 需要 `bytes`、`encoding/json`、`a-ui/database`、`a-ui/xray`；若尚未引入，补上。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run TestBuildRule -v`
Expected: FAIL —— `buildRule` 还在读 `rule.DomainGroupId`（此时为 0），四个用例都会因「域名组不存在」被跳过，生成的配置里没有带 domain 的规则。

- [ ] **Step 3: `MergeDomains` 改变参**

把 `web/service/routing_domain.go` 的 `MergeDomains` 签名与循环改成：

```go
// MergeDomains 按传入顺序合并多个域名列表，去重并保留首次出现。
//
// 顺序确定是「生成逐字节确定」不变量的一部分：调用方按固定顺序传入
// （手工在前、订阅在后；跨组则按域名组 id 升序），本函数不重排。
func MergeDomains(lists ...[]string) []string {
	total := 0
	for _, l := range lists {
		total += len(l)
	}
	merged := make([]string, 0, total)
	seen := make(map[string]bool, total)
	for _, list := range lists {
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

现有调用点 `MergeDomains(manual, subscribed)` 在 Go 的变参规则下无需修改。

- [ ] **Step 4: 改 `buildRule`**

把 `web/service/routing_inject.go` 里 `buildRule` 开头到 `generated := ...` 之前的那一段（从 `group, err := s.domainGroupService.Get(...)` 到 `if len(domains) == 0 { ... }`）整体替换成：

```go
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return nil, false, common.NewError("规则的域名组数据损坏, id:", rule.Id, "err:", err)
	}
	if len(groupIds) == 0 {
		return nil, false, common.NewError("规则没有指定任何域名组, id:", rule.Id,
			"（域名条件为空会让规则退化成劫持该入站全部流量）")
	}

	// 按 DomainGroupIds 的升序逐组取域名。失效的组剔除而不是整条丢弃：
	// 一个订阅从未拉取成功的空组，不该把同一条规则里本来好好的分流一起
	// 废掉；对 block 规则尤其如此——整条丢弃等于本该封禁的域名全部裸奔，
	// 部分生成至少封住了还在的那部分。
	//
	// 「数据损坏」与「组为空」的后果完全相同（该组贡献 0 条域名），统一
	// 走剔除；剔除的方向是缩小匹配范围，安全侧一致。
	lists := make([][]string, 0, len(groupIds))
	for _, gid := range groupIds {
		group, groupErr := s.domainGroupService.Get(gid)
		if groupErr != nil {
			logger.Warning("routing rule drops a domain group that no longer exists, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		manual, decodeErr := DecodeDomains(group.Domains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a domain group with corrupt manual domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		subscribed, decodeErr := DecodeSubscribedDomains(group.SubscribedDomains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a domain group with corrupt subscribed domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		// 组内合并顺序确定（手工在前、订阅在后、保留首次出现）。
		one := MergeDomains(manual, subscribed)
		if len(one) == 0 {
			logger.Warning("routing rule drops an empty domain group, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		lists = append(lists, one)
	}

	// 跨组按上面的遍历顺序合并去重。禁止改用遍历 map 产生顺序——
	// 那样生成不再逐字节确定，Config.Equals 恒为 false，10 秒的重启 cron
	// 会不停重启 xray。
	domains := MergeDomains(lists...)
	if len(domains) == 0 {
		return nil, false, common.NewError("规则的域名组全部不存在或为空, rule id:", rule.Id,
			"group ids:", groupIds,
			"（域名条件为空会让规则退化成劫持该入站全部流量）")
	}
```

同时把 `buildRule` 上方文档注释里「域名来自两个来源的合并」那一段改为说明跨组合并，保留「绝不能退而求其次生成一条缺少 domain 的规则」那一段不动。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./web/service/ -run TestBuildRule -v`
Expected: 4 个用例全部 PASS。

Run: `go test ./web/service/`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add web/service/routing_domain.go web/service/routing_inject.go web/service/routing_inject_test.go
git commit -m "feat(routing): buildRule 跨域名组合并，失效的组剔除而非整条丢弃"
```

---

### Task 6: Controller 接口

**Files:**
- Modify: `web/controller/routing.go:72`（`routingRuleForm`）、`:84`（`routingRuleView`）、`:98`（`ruleFromForm`）、`:455-475`（`getRules`）

**Interfaces:**
- Consumes: `service.EncodeDomainGroupIdsStrict`、`service.DecodeDomainGroupIds`（Task 1）
- Produces: 接口收发 `domainGroupIds []int` 与 `groupsBroken bool`

- [ ] **Step 1: 改表单与视图结构**

`routingRuleForm` 的 `DomainGroupId int` 那一行改成：

```go
	// DomainGroupIds 至少要有一个元素。空数组【不是】「所有域名组」——
	// 与 InboundIds 的空数组语义相反，见 model.RoutingRule 的字段注释。
	DomainGroupIds []int `json:"domainGroupIds" form:"domainGroupIds"`
```

`routingRuleView` 的 `DomainGroupId int` 那一行改成：

```go
	DomainGroupIds []int `json:"domainGroupIds"`
```

并在 `Broken` 之后追加：

```go
	// GroupsBroken 标记 DomainGroupIds 列解码失败，与 Broken（InboundIds
	// 解码失败）分开而不合并：两者的界面文案不同，合并会让「入站数据损坏」
	// 和「域名组数据损坏」显示成同一句话，管理员照着去修错的地方。
	GroupsBroken bool `json:"groupsBroken"`
```

- [ ] **Step 2: 改 `ruleFromForm`（替换桥接）**

> **注意**：Task 4 在这个函数里留了一段**过渡桥接**——保留 `DomainGroupId: form.DomainGroupId`
> 的同时，用 `service.EncodeDomainGroupIdsStrict([]int{form.DomainGroupId})` 编出
> `DomainGroupIds` 一并写入，并带一句「过渡桥接」注释。本 Step 要**替换掉**它：
> 表单字段本身改成数组之后，桥接就没有存在意义了。连同那句过渡注释一起删除，
> 不要留下两套并存的编码逻辑。


```go
func ruleFromForm(id int, form *routingRuleForm) (*model.RoutingRule, error) {
	encoded, err := service.EncodeInboundIdsStrict(form.InboundIds)
	if err != nil {
		return nil, err
	}
	encodedGroups, err := service.EncodeDomainGroupIdsStrict(form.DomainGroupIds)
	if err != nil {
		return nil, err
	}
	return &model.RoutingRule{
		Id:             id,
		Remark:         form.Remark,
		InboundIds:     encoded,
		DomainGroupIds: encodedGroups,
		Action:         form.Action,
		OutboundId:     form.OutboundId,
		Priority:       form.Priority,
		Enable:         form.Enable,
	}, nil
}
```

- [ ] **Step 3: 改 `getRules` 的视图组装**

在现有 `ids`/`broken` 的处理之后、`views = append(...)` 之前插入：

```go
		groupIds, groupsErr := service.DecodeDomainGroupIds(rule.DomainGroupIds)
		groupsBroken := groupsErr != nil
		if groupsBroken {
			groupIds = nil
		}
		if groupIds == nil {
			// 必须是 []，不能是 null：前端对它做 .length / .includes，
			// null 会在渲染规则列表时抛异常，整页数据都出不来。
			//
			// 与 InboundIds 不同的是，空的域名组数组在前端没有「所有域名组」
			// 这个歧义解读，渲染成红色的「域名组数据损坏」标签即可。
			groupIds = []int{}
		}
```

并把 `views = append(...)` 里的 `DomainGroupId: rule.DomainGroupId,` 换成 `DomainGroupIds: groupIds,`，同时补上 `GroupsBroken: groupsBroken,`。

- [ ] **Step 4: 确认编译与测试**

Run: `go build ./... && go test ./web/... ./database/`
Expected: 全部通过。

- [ ] **Step 5: 提交**

```bash
git add web/controller/routing.go
git commit -m "feat(routing): 规则接口收发域名组数组"
```

---

### Task 7: 导入导出

**Files:**
- Modify: `web/service/routing_portable.go:62`（`PortableRule`）、`:238`（`toPortableRule`）、`:636-760`（`importRules`）
- Test: `web/service/routing_portable_test.go`

**Interfaces:**
- Consumes: `EncodeDomainGroupIdsStrict`（Task 1）
- Produces: 导出字段 `domainGroupRefs *[]string`（主）+ `domainGroupRef string`（兼容）

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_portable_test.go` 末尾：

```go
// 多组规则导出时 domainGroupRef 必须留空：旧面板见到空值会明确拒绝，
// 好过让它在多个同名候选里猜一个，产生一条指向错误组的规则——那种规则
// 在规则表和生成的配置里都渲染得完全正常，只是流量走错节点。
func TestExportMultiGroupRuleLeavesLegacyRefEmpty(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	chatgpt := newTestGroup(t, "ChatGPT")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "两组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id, chatgpt.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export("rules")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("导出了 %d 条规则，want 1", len(f.Rules))
	}
	if f.Rules[0].DomainGroupRef != "" {
		t.Errorf("多组规则的 domainGroupRef 必须为空，got %q", f.Rules[0].DomainGroupRef)
	}
	if f.Rules[0].DomainGroupRefs == nil || len(*f.Rules[0].DomainGroupRefs) != 2 {
		t.Fatalf("domainGroupRefs = %v, want 两个组名", f.Rules[0].DomainGroupRefs)
	}
}

func TestExportSingleGroupRuleFillsLegacyRef(t *testing.T) {
	setupDB(t)
	claude := newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "一组", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id}),
		Action:         model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export("rules")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if f.Rules[0].DomainGroupRef != "Claude" {
		t.Errorf("单组规则必须填 domainGroupRef 以兼容旧面板，got %q", f.Rules[0].DomainGroupRef)
	}
}

// v1.7.0 及更早导出的文件只有 domainGroupRef。
func TestImportAcceptsLegacySingleGroupRef(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	f := &ExportFile{
		Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
		Rules: []PortableRule{{
			Remark: "旧格式", DomainGroupRef: "Claude", InboundRefs: &refs,
			Action: model.ActionBlock, Enable: true,
		}},
	}
	report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 1 {
		t.Fatalf("旧格式必须能导入: %+v", report)
	}
}

func TestImportRejectsRuleWithoutAnyGroupRef(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	empty := []string{}
	for name, rule := range map[string]PortableRule{
		"两个字段都缺": {Remark: "A", InboundRefs: &refs, Action: model.ActionBlock, Enable: true},
		"显式空数组":  {Remark: "B", DomainGroupRefs: &empty, InboundRefs: &refs, Action: model.ActionBlock, Enable: true},
	} {
		f := &ExportFile{
			Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
			Rules: []PortableRule{rule},
		}
		report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
		if err != nil {
			t.Fatalf("%s：Import: %v", name, err)
		}
		if report.Rules.Created != 0 {
			t.Errorf("%s：一个域名组都没有的规则必须整条拒绝，got created=%d",
				name, report.Rules.Created)
		}
	}
}

// 与入站对称：部分组认不出 → 导入成禁用；全部认不出 → 整条丢弃。
func TestImportPartialGroupMatchImportsDisabled(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "Claude")
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	groups := []string{"Claude", "本机没有的组"}
	f := &ExportFile{
		Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
		Rules: []PortableRule{{
			Remark: "部分命中", DomainGroupRefs: &groups, InboundRefs: &refs,
			Action: model.ActionBlock, Enable: true,
		}},
	}
	report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 1 {
		t.Fatalf("部分命中应导入成禁用而不是丢弃: %+v", report)
	}
	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 || rules[0].Enable {
		t.Errorf("部分命中的规则必须导入成禁用状态, got enable=%v", rules[0].Enable)
	}
}

func TestImportDropsRuleWhenNoGroupMatches(t *testing.T) {
	setupDB(t)
	in := newTestInbound(t, 10001)
	refs := []PortableInboundRef{{Remark: in.Remark, Port: in.Port}}
	groups := []string{"本机没有的组"}
	f := &ExportFile{
		Kind: ExportKind, Version: ExportVersion, Scope: []string{"rules"},
		Rules: []PortableRule{{
			Remark: "全不命中", DomainGroupRefs: &groups, InboundRefs: &refs,
			Action: model.ActionBlock, Enable: true,
		}},
	}
	report, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Rules.Created != 0 {
		t.Errorf("一个组都认不出必须整条丢弃，got created=%d", report.Rules.Created)
	}
}
```

签名已核实：`Export(scope string) (*ExportFile, error)`、`Import(raw string) (*ImportReport, error)`（收的是 JSON 字符串，不是结构体）。`exportJSON(t, f)` 是 `routing_portable_test.go` 里已有的辅助函数，把 `*ExportFile` 序列化成 JSON。常量为 `ExportKind` / `ExportVersion`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./web/service/ -run 'TestExportMultiGroup|TestExportSingleGroup|TestImportAcceptsLegacy|TestImportRejectsRuleWithoutAnyGroupRef|TestImportPartialGroup|TestImportDropsRule' -v`
Expected: 编译失败，`PortableRule` 没有 `DomainGroupRefs` 字段。

- [ ] **Step 3: 改 `PortableRule`**

把 `DomainGroupRef string` 那一行替换成：

```go
	// DomainGroupRefs 是指针，理由与 InboundRefs 同构但【结论相反】：
	// null / 键缺失 / 显式 [] 三者全部整条拒绝——域名组一个都没有等于
	// domain 条件为空，xray 会把规则当作「不限制」。用指针不是为了区分
	// 放行与拒绝（都拒绝），而是为了让报错说得准：是「字段缺失」还是
	// 「显式空数组」。
	DomainGroupRefs *[]string `json:"domainGroupRefs"`
	// DomainGroupRef 兼容 v1.7.0 及更早导出的文件，只读，不再作为主字段。
	// 导出侧仍在【单组时】填它：这样新面板导出的文件放进旧面板，单组规则
	// 照常可用，多组规则会被旧面板的「domainGroupRef 为空 → 整条跳过」
	// 明确拒绝——明确报错远好过静默产生一条指向错误组的规则。
	DomainGroupRef string `json:"domainGroupRef,omitempty"`
```

- [ ] **Step 4: 改 `toPortableRule`**

把开头取域名组的三行替换成：

```go
	groupIds, err := DecodeDomainGroupIds(r.DomainGroupIds)
	if err != nil {
		return PortableRule{}, common.NewError("域名组数据损坏:", err)
	}
	if len(groupIds) == 0 {
		return PortableRule{}, common.NewError("规则没有引用任何域名组")
	}
	groupRefs := make([]string, 0, len(groupIds))
	for _, gid := range groupIds {
		g, ok := groupById[gid]
		if !ok {
			// 悬空引用，本机上这条规则的这一部分已经不生效了。整条跳过，
			// 不能只剔掉这一个——剔到最后变成空数组就是「domain 条件为空」。
			return PortableRule{}, common.NewErrorf("域名组 #%d 不存在", gid)
		}
		groupRefs = append(groupRefs, g.Remark)
	}
	legacyRef := ""
	if len(groupRefs) == 1 {
		legacyRef = groupRefs[0]
	}
```

并把 return 里的 `DomainGroupRef: g.Remark,` 换成：

```go
		DomainGroupRefs: &groupRefs,
		DomainGroupRef:  legacyRef,
```

- [ ] **Step 5: 改 `importRules`（含替换桥接）**

> **注意**：Task 4 在本函数构造 `&model.RoutingRule{...}` 的地方留了一段**过渡桥接**——
> 保留 `DomainGroupId: g.Id` 的同时，用 `EncodeDomainGroupIdsStrict([]int{g.Id})` 编出
> `DomainGroupIds` 一并写入，并带一句「过渡桥接」注释。本 Step 的多组解析会取代它：
> 删掉桥接那几行连同过渡注释，改用下面解析出的 `encodedGroups`。
> 不要留下两处都在算 `DomainGroupIds` 的代码。


把 `importRules` 里从 `if item.DomainGroupRef == ""` 到 `g, ok := groupByRemark[...]` 那一整段（含 `ambiguousGroupRemark` 判断）替换成：

```go
		// 优先新字段；nil 指针（null 或键缺失）时回落旧字段，兼容
		// v1.7.0 及更早导出的文件。
		var groupRefs []string
		switch {
		case item.DomainGroupRefs != nil:
			groupRefs = *item.DomainGroupRefs
		case item.DomainGroupRef != "":
			groupRefs = []string{item.DomainGroupRef}
		}
		if len(groupRefs) == 0 {
			report.Rules.Failed++
			report.fail("规则「%s」没有指定域名组，整条跳过（域名条件为空会让它劫持该用户的全部流量）", label)
			continue
		}

		groupIds := make([]int, 0, len(groupRefs))
		missingGroups := make([]string, 0)
		rejected := false
		for _, ref := range groupRefs {
			// 空字符串不参与域名组匹配，理由与 resolveInboundRefs 里空
			// remark 不参与入站匹配完全同构：groupByRemark[""] 会在本机恰好
			// 存在一个备注为空的域名组时静默命中它，产生一条指向错误域名组
			// 的规则，而规则表和生成的配置都渲染得完全正常。
			if ref == "" {
				report.Rules.Failed++
				report.fail("规则「%s」的域名组引用里有空值，整条跳过", label)
				rejected = true
				break
			}
			if ambiguousGroupRemark[ref] {
				report.Rules.Failed++
				report.fail("规则「%s」引用的域名组「%s」在本机有多个同名组，无法确定指向哪一个，整条跳过（请先在域名组页面改名）",
					label, ref)
				rejected = true
				break
			}
			g, ok := groupByRemark[ref]
			if !ok {
				missingGroups = append(missingGroups, ref)
				continue
			}
			groupIds = append(groupIds, g.Id)
		}
		if rejected {
			continue
		}
		// 一个都没认出来必须整条丢弃：编码结果会落回 []，而空的域名组集合
		// 意味着 domain 条件为空。导入成禁用状态也不行——管理员一旦手滑
		// 启用，这条规则立刻变成劫持该用户的全部流量。
		if len(groupIds) == 0 {
			report.Rules.Failed++
			report.fail("规则「%s」引用的域名组在本机一个都没找到（%s），整条跳过",
				label, strings.Join(missingGroups, "、"))
			continue
		}
		encodedGroups, err := EncodeDomainGroupIdsStrict(groupIds)
		if err != nil {
			report.Rules.Failed++
			report.fail("规则「%s」的域名组编码失败：%v", label, err)
			continue
		}
```

在下方 `enable := item.Enable` 之后、`if len(missing) > 0` 之前插入：

```go
		if len(missingGroups) > 0 {
			// 与入站的「部分命中」同策略：规则的其余部分都是好的，导入成
			// 禁用状态并把缺失的组点名，管理员打开编辑弹窗勾一下就行。
			enable = false
			report.say("规则「%s」的域名组 %s 在本机未找到，已导入但保持禁用，请手工确认后启用",
				label, strings.Join(missingGroups, "、"))
		}
```

最后把构造 `rule` 的那一行里的 `DomainGroupId: g.Id,` 换成 `DomainGroupIds: encodedGroups,`。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./web/service/ -run 'Export|Import' -v`
Expected: 全部 PASS。

Run: `go test ./web/service/`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add web/service/routing_portable.go web/service/routing_portable_test.go
git commit -m "feat(routing): 导入导出支持多域名组并兼容旧格式文件"
```

---

### Task 8: 前端复选框

**Files:**
- Modify: `web/assets/js/model/routing.js:63-90`（`RoutingRule`）
- Modify: `web/html/xui/routing.html`（弹窗 366-373、列表 205-206、方法 ~495-520 / ~726-745 / ~815-830 / ~920-940）

**Interfaces:**
- Consumes: 接口字段 `domainGroupIds []int`、`groupsBroken bool`（Task 6）
- Produces: 无（终端消费者）

- [ ] **Step 1: 改前端模型**

`web/assets/js/model/routing.js` 的 `RoutingRule`：

```js
class RoutingRule {
    constructor(id = 0, remark = "", inboundIds = [], domainGroupIds = [],
                action = RULE_ACTION.PROXY, outboundId = 0, priority = 0,
                enable = true, broken = false, groupsBroken = false) {
        this.id = id;
        this.remark = remark;
        // 空数组 = 所有用户（含以后新建的入站）。
        // 注意它与「一个用户都没勾」在提交体里长得一模一样，弹窗必须自己
        // 区分这两种意图，见 routing.html 的 saveRule。
        this.inboundIds = inboundIds;
        // 与 inboundIds 相反：空数组【不是】「所有域名组」，而是非法状态。
        // 域名条件为空会让 xray 把规则当作「不限制」，规则从「这批域名走 B」
        // 退化成「该用户全部流量走 B」。saveRule 必须自己拦住它。
        this.domainGroupIds = domainGroupIds;
        this.action = action;
        this.outboundId = outboundId;
        this.priority = priority;
        this.enable = enable;
        // broken 为真表示服务端解码 inboundIds 失败。这种规则不会写进配置，
        // 但它的 inboundIds 是空数组，看起来和「所有用户」一样，必须区分渲染。
        this.broken = broken;
        // groupsBroken 为真表示服务端解码 domainGroupIds 失败。与 broken 分开
        // 是因为界面文案不同，合并会让管理员照着去修错的地方。
        this.groupsBroken = groupsBroken;
    }

    static fromJson(json = {}) {
        return new RoutingRule(json.id, json.remark, json.inboundIds || [],
            json.domainGroupIds || [], json.action, json.outboundId, json.priority,
            json.enable, json.broken, json.groupsBroken);
    }
}
```

- [ ] **Step 2: 换掉弹窗里的下拉框**

`web/html/xui/routing.html` 的「域名组」`<a-form-item>` 整块替换成（结构照抄上方「用户（入站）」那套）：

```html
            <a-form-item label="域名组">
                <div style="display: flex; align-items: center; margin-bottom: 8px;">
                    <!-- 域名组少的时候搜索框纯属噪音，超过 5 个才出现 -->
                    <a-input v-if="groups.length > 5" v-model="ruleModal.groupFilter"
                             placeholder="搜索域名组" size="small" allow-clear
                             style="width: 160px;">
                        <a-icon slot="prefix" type="search"></a-icon>
                    </a-input>
                    <span style="margin-left: auto; font-size: 12px; color: #999;">
                        已选 [[ ruleModal.rule.domainGroupIds.length ]] 组
                        <a @click="selectAllVisibleGroups" style="margin-left: 8px;">[[ selectAllGroupsLabel ]]</a>
                        <a @click="clearGroupSelection" style="margin-left: 8px;">清空</a>
                    </span>
                </div>
                <!-- 固定高度内部滚动：域名组再多，弹窗总高也不变，
                     「保存」按钮永远留在视口里。 -->
                <div style="max-height: 156px; overflow-y: auto; padding-right: 4px;">
                    <div v-for="g in visibleGroups" :key="g.id" style="margin-bottom: 4px;">
                        <!-- 域名组复选框永不禁用：能不能选取决于当前选了哪些用户，
                             两边互相依赖。被禁用的是「用户」那一侧。 -->
                        <a-checkbox :checked="ruleModal.rule.domainGroupIds.includes(g.id)"
                                    @change="e => toggleDomainGroup(g.id, e.target.checked)">
                            [[ g.remark ]]
                        </a-checkbox>
                    </div>
                    <div v-if="!visibleGroups.length" style="color: #999; font-size: 12px;">
                        没有匹配的域名组
                    </div>
                </div>
            </a-form-item>
```

- [ ] **Step 3: 改 computed 与方法**

`ruleModal` 的初始数据加 `groupFilter: ''`。新增 computed：

```js
            visibleGroups() {
                const kw = (this.ruleModal.groupFilter || '').trim().toLowerCase();
                if (!kw) return this.groups;
                return this.groups.filter(g => (g.remark || '').toLowerCase().includes(kw));
            },
            selectAllGroupsLabel() {
                const ids = this.ruleModal.rule.domainGroupIds;
                return this.visibleGroups.every(g => ids.includes(g.id)) ? '取消全选' : '全选';
            },
```

`occupiedInbounds` 里的两行改成按组集合相交判断：

```js
            occupiedInbounds() {
                const map = {};
                const gids = this.ruleModal.rule.domainGroupIds;
                if (!gids.length) return map;
                for (const r of this.rules) {
                    if (r.id === this.ruleModal.rule.id) continue;
                    // 冲突判定的单位是「域名组 × 用户」的组合：组集合不相交
                    // 就不冲突，同一个域名组可以被多条规则引用。
                    if (!r.domainGroupIds.some(id => gids.includes(id))) continue;
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
```

把 `onDomainGroupChange` 整个替换成：

```js
            // 勾选/取消一个域名组后，占用集合跟着变。已勾选但在新集合下不可用
            // 的用户必须剔除，否则会带着一个必被后端拒绝的选择去点保存。
            //
            // 用复选框自己的 @change 而不是 watch：Vue 的 watcher 在 nextTick
            // 异步触发，openRule 先换 rule 再把 visible 置真，等 watcher 跑起来
            // visible 已经是 true，守卫拦不住它——打开编辑弹窗会顺手把已勾选的
            // 项过滤一遍。这里显式赋值再读 computed，时序完全可控。
            toggleDomainGroup(groupId, checked) {
                const ids = [...this.ruleModal.rule.domainGroupIds];
                const idx = ids.indexOf(groupId);
                if (checked && idx < 0) ids.push(groupId);
                if (!checked && idx >= 0) ids.splice(idx, 1);
                // 升序提交，和后端 EncodeDomainGroupIds 的存储顺序对齐
                this.ruleModal.rule.domainGroupIds = ids.sort((a, b) => a - b);
                this.reconcileInboundSelection();
            },
            selectAllVisibleGroups() {
                const ids = [...this.ruleModal.rule.domainGroupIds];
                const all = this.visibleGroups.every(g => ids.includes(g.id));
                for (const g of this.visibleGroups) {
                    const idx = ids.indexOf(g.id);
                    if (all && idx >= 0) ids.splice(idx, 1);
                    if (!all && idx < 0) ids.push(g.id);
                }
                this.ruleModal.rule.domainGroupIds = ids.sort((a, b) => a - b);
                this.reconcileInboundSelection();
            },
            clearGroupSelection() {
                this.ruleModal.rule.domainGroupIds = [];
                this.reconcileInboundSelection();
            },
            // 按当前域名组集合重算占用，剔除已不可用的用户选择。
            reconcileInboundSelection() {
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

`saveRule` 开头加一道拦截（放在已有的入站拦截旁边）：

```js
                if (!this.ruleModal.rule.domainGroupIds.length) {
                    // 空的域名组集合是非法状态而不是「所有域名组」，没有理由
                    // 把它发出去。见 model.RoutingRule 的字段注释。
                    this.$message.error('请至少选择一个域名组');
                    return;
                }
```

- [ ] **Step 4: 改列表列与三处告警**

规则列表的「域名组」列（`routing.html:205-206`）改成渲染多个 tag：

```html
                                        <template v-if="rule.groupsBroken">
                                            <a-tag color="red">域名组数据损坏</a-tag>
                                        </template>
                                        <template v-else>
                                            <a-tag v-for="gid in rule.domainGroupIds" :key="gid"
                                                   :color="groupMissing(gid) ? 'red' : ''">
                                                [[ groupName(gid) ]]
                                            </a-tag>
                                        </template>
```

`ruleIssue` 里域名组相关的两个分支替换成：

```js
                if (rule.groupsBroken) return '规则的域名组数据损坏，这条规则不会写进配置';
                if (!rule.domainGroupIds.length) return '规则没有指定域名组，这条规则不会写进配置';
                // 部分组失效不再是失效原因——后端只剔除失效的组，剩下的照常
                // 生效。只有【全部】组都没有可用域名时整条规则才会被丢弃。
                const liveGroups = rule.domainGroupIds
                    .map(id => this.groups.find(x => x.id === id))
                    .filter(g => g && g.effectiveCount);
                if (!liveGroups.length) {
                    return '引用的域名组已全部删除或为空，这条规则不会写进配置';
                }
```

`ruleConflict` 里的组判断替换成：

```js
                    const sharedGroups = rule.domainGroupIds
                        .filter(id => other.domainGroupIds.includes(id));
                    if (!sharedGroups.length) continue;
                    const groupLabel = sharedGroups.map(this.groupName).join('、');
```

并把两句冲突文案里的「在同一域名组下」改成 `'在域名组 ' + groupLabel + ' 下'`。

- [ ] **Step 5: 跑模板测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: PASS。`web.go` 的 `getHtmlTemplate` 吞掉 `ParseFS` 错误，光靠 `go build` 发现不了模板语法错误，这两个测试是唯一的守卫。

- [ ] **Step 6: 手工验证**

Run: `XUI_DEBUG=true go run main.go`（必须在仓库根启动：调试模式下模板与静态资源从磁盘的 `web/html`、`web/assets` 相对当前工作目录读取）

在分流管理页逐项确认：
1. 添加规则时「域名组」是复选框，可多选
2. 勾选多个组后保存成功，列表里显示多个 tag
3. 编辑该规则，勾选状态正确回显
4. 建第二条规则、选同一批用户 + 有交集的组 → 保存被拒，错误信息点名相交的组
5. 建第二条规则、选同一批用户 + 完全不同的组 → 保存成功
6. 先勾用户、再勾一个该用户已被占用的组 → 该用户被自动取消勾选
7. 一个组都不勾点保存 → 前端提示「请至少选择一个域名组」

- [ ] **Step 7: 提交**

```bash
git add web/assets/js/model/routing.js web/html/xui/routing.html
git commit -m "feat(routing): 分流规则表单的域名组改为复选框多选"
```

---

### Task 9: 收尾与文档

**Files:**
- Modify: `CLAUDE.md`（「域名分流管理」小节）
- Verify: 全仓库

**Interfaces:**
- Consumes: 前八个 Task 的全部产出
- Produces: 无

- [ ] **Step 1: 删除三处过渡桥接**

Task 4 为了让每个 Task 都以全绿结束，在三个地方留了写 `DomainGroupId` 的**过渡桥接**（均带「过渡桥接」注释）。到本 Task 时 `buildRule`、`listRules`、`toPortableRule` 都已改读 `DomainGroupIds`，桥接必须全部删除：

1. `web/controller/routing.go` 的 `ruleFromForm` —— Task 6 应已替换，在此确认没有残留
2. `web/service/routing_portable.go` 的 `importRules` —— Task 7 应已替换，在此确认没有残留
3. `web/service/routing_rule.go` 的 `Update` —— **本 Step 删除**，去掉同步 `old.DomainGroupId` 的那一行与它的过渡注释

删干净是 spec §11 描述的回退行为成立的前提：删除后新建的多组规则 `domain_group_id` 为 0，回退到旧版二进制时旧代码会整条丢弃它们（分流范围缩小而非放大，安全侧正确）。桥接若残留，旧代码会按第一个组分流——不算危险，但与文档不符。

- [ ] **Step 2: 确认生产代码不再读写 `DomainGroupId`**

Run: `grep -rn "DomainGroupId\b" --include="*.go" --include="*.js" --include="*.html" . | grep -v "_test.go" | grep -v "DomainGroupIds"`
Expected: 只剩两处——`database/model/routing.go` 的字段声明与注释、`database/db.go` 的迁移。其余任何一处都说明有路径没切干净或桥接没删净。

- [ ] **Step 3: `gofmt` 收尾**

Task 2 给 `database/model/routing.go` 插注释时破坏了结构体字段的 gofmt 对齐（改动前该文件是 gofmt-clean 的）。`make verify` 不含 gofmt 步骤，抓不到这个。

Run: `gofmt -l ./database/ ./web/ ./xray/ ./util/ && gofmt -w database/model/routing.go`
Expected: 修掉 `database/model/routing.go`。其余文件若本来就不 gofmt-clean（例如 `database/db.go` 的 import 顺序是既有状态），**不要动**——那不是本次改造引入的。

- [ ] **Step 4: 更新 CLAUDE.md**

在「域名分流管理 → 数据模型」的五条字段约定里，把第三条「同一个域名组下，任何一个入站至多被一条规则覆盖」改成：

```
- **同一个域名组下，任何一个入站至多被一条规则覆盖**（`RoutingRuleService.checkConflict`，把空数组当全集做集合相交判定）。规则改为可引用多个域名组后，判定推广为「**域名组集合相交**且入站集合相交」，判定单位仍是「域名组 × 入站」的组合——同一个域名组可以被多条规则引用，只要覆盖的入站不重叠。禁用的规则同样占位。只在写入路径校验，生成期不干预：迁移前留下的冲突数据照常生成两条规则，由界面标黄交给管理员处理。
```

并在该小节的三张表说明里，把 `RoutingRule` 那一行的 `DomainGroupId` 改成 `DomainGroupIds`，随后追加一段新的不变量：

```
**`DomainGroupIds` 的空数组 `[]` 与 `InboundIds` 的空数组语义相反。** 入站的 `[]` 是合法的「所有用户」；域名组的 `[]` 是非法值——`domain` 条件为空会让 xray 把规则当作「不限制」，规则从「这批域名走 B」退化成「该用户全部流量走 B」，且返回 `Configuration OK`、面板首页显示 `running`。写入路径用 `EncodeDomainGroupIdsStrict`（对空结果一律报错，无论原始列表是否为空），`intersectGroups` 也**不能**复用 `intersectInbounds`（后者把空切片当全集）。这是本子系统里唯一一处「照抄隔壁的实现就会开洞」的地方。

**回退到旧版本二进制**：`domain_group_id` 列保留，单组规则行为完全正常；多组规则该值为 0，旧代码会整条丢弃——分流范围缩小而非放大，安全侧正确。设计文档在 `docs/superpowers/specs/2026-09-05-rule-multi-domain-group-design.md`。
```

在「配置导出 / 导入」小节末尾追加：

```
**规则的域名组引用是数组 `domainGroupRefs`，`domainGroupRef` 保留作单组时的兼容字段。** 导出侧单组时两个都写、多组时只写数组——这样新面板导出的文件放进旧面板，单组规则照常可用，多组规则被旧面板明确拒绝（「domainGroupRef 为空 → 整条跳过」），而不是静默产生一条指向错误组的规则。导入侧优先数组字段，为 nil 时回落单值字段；组认不出的策略与入站对称：部分认不出导入成禁用并点名，一个都认不出整条丢弃（编码结果会落回 `[]`，是上面那条非法值）。
```

- [ ] **Step 5: 全量验证**

Run: `make verify`
Expected: vet 无输出、全部测试 PASS、编译成功。

Run: `git status --short && git diff --stat HEAD~8`
Expected: 工作区干净；改动仅落在计划的文件清单内，没有调试残留与无关格式变化。

- [ ] **Step 4: 提交**

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md 记录多域名组的空数组语义与回退风险"
```
