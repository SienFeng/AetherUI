# 分流规则自动应用于新增入站 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给分流规则加一个「以后新增的用户自动应用此规则」开关，新建入站时在同一个事务里把它追加进这些规则的 `InboundIds`。

**Architecture:** 写入期扩散，不是生成期推导。数据保持静态，`buildRule` / 配置生成 / 回退契约一个字都不用改。`checkConflict` 的既有不变量不放宽，另加一条「每个域名组至多一条规则声明自动应用」。

**Tech Stack:** Go 1.27 + GORM(sqlite) + Gin；前端 Vue 2 + ant-design-vue 1.7.2 内联模板，无打包工具。

**Spec:** `docs/superpowers/specs/2026-09-07-rule-auto-apply-new-inbounds-design.md`

## Global Constraints

- **本地验证命令是 `make verify`**（= `go vet ./...` + `go test ./...` + `go build`）。不要凭生态惯例猜测其他命令。
- **`InboundIds` 的空数组 `[]` 表示「所有用户」**，是合法语义。任何往空数组里追加 id 的行为都会把规则从「覆盖所有人」降级成「只覆盖一个人」——这是本计划唯一的高危失误模式（spec §4.2）。
- **`InboundIds` 必须升序去重存储**，一律走 `service.EncodeInboundIds`。顺序抖动会让 `Config.Equals` 恒为 false，那个 10 秒的重启 cron 会不停重启 xray。
- **新增的 `checkConflict` 错误文案必须含「冲突」二字**：`routing_portable.go` 的 `importRules` 靠 `strings.Contains(err.Error(), "冲突")` 把这类错误计入 Skipped 而非 Failed。
- **后端零值是 `false`**。「新建默认勾上」只是前端表单初始值，绝不体现为后端默认值——否则导入的旧文件和直接调接口创建的规则都会变成 true，违反「升级后行为零变化」。
- **测试里凡是要走 `InboundService.AddInbound` 的，入站配置必须是合法的 xray 配置**（用 `vlessSettings()` + `plainTCPStream`，定义在 `web/service/inbound_validate_test.go:19-21`）。`AddInbound` 会 exec 真实 xray 校验：本地 macOS 缺 `bin/xray-darwin-arm64` 时 fail open 放行，CI 的 linux 上会真的校验——配置写错会出现「本地过、CI 挂」。
- **端口不能重复**：`AddInbound` 有 `checkPortExist`，同一个测试里每个入站用不同端口。
- `web/service` 的 `TestMain` 会 `os.Chdir` 到仓库根，这是进程级副作用；新增测试不要依赖包内相对路径。

---

### Task 1: 数据模型 + `AttachInbound`

**Files:**
- Modify: `database/model/routing.go`（`RoutingRule` 结构体末尾）
- Modify: `web/service/routing_rule.go`（新增 `AttachInbound`）
- Test: `web/service/routing_rule_test.go`（追加）

**Interfaces:**
- Consumes: `service.EncodeInboundIds(ids []int) (string, error)`、`service.DecodeInboundIds(encoded string) ([]int, error)`（均在 `web/service/routing_rule.go`）
- Produces: `model.RoutingRule.ApplyToNewInbounds bool`；`func (s *RoutingRuleService) AttachInbound(tx *gorm.DB, inboundId int) error`

- [ ] **Step 1: 给模型加字段**

在 `database/model/routing.go` 的 `RoutingRule` 结构体里，`Enable bool` 那一行**之后**加：

```go
	// ApplyToNewInbounds 为真时，以后新建的入站会在创建它的同一个事务里被
	// 追加进这条规则的 InboundIds（见 RoutingRuleService.AttachInbound）。
	//
	// 这是写入期扩散而非生成期推导：数据保持静态，规则弹窗里勾选框显示的
	// 就是实际生效的名单。推导方案会让两者不一致。
	//
	// 零值 false：AutoMigrate 给老库加上它之后没有任何规则会自动扩散，
	// 升级后行为零变化。前端新建表单默认勾上，那是表单初始值，不是这里的
	// 默认值——否则导入的旧文件也会变成 true。
	ApplyToNewInbounds bool `json:"applyToNewInbounds" form:"applyToNewInbounds"`
```

- [ ] **Step 2: 写失败的测试**

追加到 `web/service/routing_rule_test.go` 末尾。注意 `newTestBlockRule` 已存在（签名 `newTestBlockRule(t, remark string, enable bool)`），但它建的是 block 规则且 `InboundIds` 为 `"[]"`；本任务需要能指定 `InboundIds` 与 `ApplyToNewInbounds` 的 helper，所以另写一个：

```go
// newTestRuleWithInbounds 建一条覆盖指定入站的 block 规则。
// 每条规则各配一个独立域名组：checkConflict 不允许同一域名组下入站集合重叠。
func newTestRuleWithInbounds(t *testing.T, remark string, inboundIds []int, applyToNew bool) *model.RoutingRule {
	t.Helper()
	g := newTestGroup(t, remark+"-组")
	encoded, err := EncodeInboundIds(inboundIds)
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	r := &model.RoutingRule{
		Remark:             remark,
		InboundIds:         encoded,
		DomainGroupId:      g.Id,
		DomainGroupIds:     mustEncodeGroupIds(t, []int{g.Id}),
		Action:             model.ActionBlock,
		Enable:             true,
		ApplyToNewInbounds: applyToNew,
	}
	if err := (&RoutingRuleService{}).Add(r); err != nil {
		t.Fatalf("Add rule %s: %v", remark, err)
	}
	return r
}

func mustInboundIds(t *testing.T, ruleId int) []int {
	t.Helper()
	r, err := (&RoutingRuleService{}).Get(ruleId)
	if err != nil {
		t.Fatalf("Get rule %d: %v", ruleId, err)
	}
	ids, err := DecodeInboundIds(r.InboundIds)
	if err != nil {
		t.Fatalf("DecodeInboundIds: %v", err)
	}
	return ids
}

func TestAttachInboundOnlyTouchesRulesThatOptedIn(t *testing.T) {
	setupDB(t)
	opted := newTestRuleWithInbounds(t, "自动纳新", []int{7, 3}, true)
	manual := newTestRuleWithInbounds(t, "手工名单", []int{7, 3}, false)

	if err := (&RoutingRuleService{}).AttachInbound(database.GetDB(), 5); err != nil {
		t.Fatalf("AttachInbound: %v", err)
	}

	// 升序去重是「生成逐字节确定」的前提，顺序一抖动 Config.Equals 恒为
	// false，那个 10 秒的重启 cron 会不停重启 xray。
	got := mustInboundIds(t, opted.Id)
	want := []int{3, 5, 7}
	if len(got) != len(want) {
		t.Fatalf("声明自动应用的规则 inboundIds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("声明自动应用的规则 inboundIds = %v, want %v", got, want)
		}
	}

	if ids := mustInboundIds(t, manual.Id); len(ids) != 2 {
		t.Errorf("未声明的规则被改动了: %v", ids)
	}
}

// 空数组表示「所有用户」。往里追加一个 id 会让规则从「覆盖所有人」降级成
// 「只覆盖这一个人」，其余用户当场失去这条规则、静默走默认出站，而 xray
// 返回 Configuration OK、面板显示 running，没有任何一层会报错。
func TestAttachInboundSkipsAllUsersRule(t *testing.T) {
	setupDB(t)
	all := newTestRuleWithInbounds(t, "所有用户", nil, true)
	if ids := mustInboundIds(t, all.Id); len(ids) != 0 {
		t.Fatalf("前置条件不成立，规则不是空数组: %v", ids)
	}

	if err := (&RoutingRuleService{}).AttachInbound(database.GetDB(), 5); err != nil {
		t.Fatalf("AttachInbound: %v", err)
	}

	if ids := mustInboundIds(t, all.Id); len(ids) != 0 {
		t.Errorf("「所有用户」规则被降级成了具体名单: %v", ids)
	}
}

// 重复调用不应把同一个 id 塞两遍——EncodeInboundIds 会去重，但这条钉住
// 「重跑一次不会改变结果」这个性质。
func TestAttachInboundIsIdempotent(t *testing.T) {
	setupDB(t)
	r := newTestRuleWithInbounds(t, "自动纳新", []int{3}, true)
	s := RoutingRuleService{}
	for i := 0; i < 2; i++ {
		if err := s.AttachInbound(database.GetDB(), 5); err != nil {
			t.Fatalf("AttachInbound #%d: %v", i, err)
		}
	}
	got := mustInboundIds(t, r.Id)
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Errorf("inboundIds = %v, want [3 5]", got)
	}
}
```

- [ ] **Step 3: 运行测试，确认失败**

Run: `go test ./web/service/ -run 'TestAttachInbound' 2>&1 | head -20`
Expected: 编译失败，`s.AttachInbound undefined (type RoutingRuleService has no field or method AttachInbound)`

- [ ] **Step 4: 实现 `AttachInbound`**

加到 `web/service/routing_rule.go` 里 `Reorder` 之后、`Del` 之前：

```go
// AttachInbound 把新建的入站追加进所有声明了「自动应用于新增用户」的规则。
//
// tx 由调用方传入：扩散必须与建入站在同一个事务里。失败若只记日志放行，
// 结果是一个用户静默地没进规则——正是这个功能要消灭的失效，而且比人工
// 遗漏更隐蔽（管理员以为系统已经处理了）。
func (s *RoutingRuleService) AttachInbound(tx *gorm.DB, inboundId int) error {
	if inboundId <= 0 {
		return nil
	}
	rules := make([]*model.RoutingRule, 0)
	err := tx.Model(model.RoutingRule{}).
		Where("apply_to_new_inbounds = ?", true).Find(&rules).Error
	if err != nil {
		return err
	}
	for _, rule := range rules {
		ids, decodeErr := DecodeInboundIds(rule.InboundIds)
		if decodeErr != nil {
			return common.NewError("分流规则", rule.Id, "的入站数据已损坏:", decodeErr)
		}
		// 空数组表示「所有用户」，已经覆盖刚建的这一个。往里追加会让规则
		// 从「覆盖所有人」降级成「只覆盖这一个人」，其余用户当场失去它而
		// 没有任何一层报错。正常情况下表单联动不会产生这种组合，但直接改库、
		// 导入的文件、将来某条新写入路径都可能留下它。
		if len(ids) == 0 {
			continue
		}
		encoded, encodeErr := EncodeInboundIds(append(ids, inboundId))
		if encodeErr != nil {
			return encodeErr
		}
		if encoded == rule.InboundIds {
			continue
		}
		err = tx.Model(model.RoutingRule{}).Where("id = ?", rule.Id).
			Update("inbound_ids", encoded).Error
		if err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestAttachInbound' -v 2>&1 | tail -12`
Expected: 三条全部 PASS

- [ ] **Step 6: 提交**

```bash
git add database/model/routing.go web/service/routing_rule.go web/service/routing_rule_test.go
git commit -m "feat(routing): 规则新增「自动应用于新增入站」标记与 AttachInbound"
```

---

### Task 2: 接进三条建入站路径

**Files:**
- Modify: `web/service/inbound.go:84-104`（`AddInbound`）、`web/service/inbound.go:106+`（`AddInbounds`）
- Test: `web/service/routing_rule_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `(&RoutingRuleService{}).AttachInbound(tx, inboundId)`
- Produces: 无新签名，`AddInbound` / `AddInbounds` 行为变更

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_rule_test.go`。**必须走 `AddInbound` 而不是直接 `db.Save`**——被测的正是那条路径：

```go
// 建入站必须走 AddInbound：被测的正是这条路径上的扩散。入站配置用
// vlessSettings()+plainTCPStream（inbound_validate_test.go），它们能通过
// AddInbound 里那次真实 xray 校验——CI 的 linux 上带着 bin/xray-linux-amd64，
// 配置写错会出现「本地 macOS 过、CI 挂」。
func addTestInboundThroughService(t *testing.T, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true,
		Tag:      "inbound-" + strconv.Itoa(port),
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := (&InboundService{}).AddInbound(in); err != nil {
		t.Fatalf("AddInbound(%d): %v", port, err)
	}
	return in
}

func TestAddInboundAttachesToOptedInRules(t *testing.T) {
	setupDB(t)
	existing := addTestInboundThroughService(t, 20011)
	opted := newTestRuleWithInbounds(t, "自动纳新", []int{existing.Id}, true)
	manual := newTestRuleWithInbounds(t, "手工名单", []int{existing.Id}, false)

	created := addTestInboundThroughService(t, 20012)

	ids := mustInboundIds(t, opted.Id)
	found := false
	for _, id := range ids {
		if id == created.Id {
			found = true
		}
	}
	if !found {
		t.Errorf("新入站 %d 没被加进声明自动应用的规则: %v", created.Id, ids)
	}
	if got := mustInboundIds(t, manual.Id); len(got) != 1 || got[0] != existing.Id {
		t.Errorf("未声明的规则被改动了: %v", got)
	}
}

func TestAddInboundsAttachesToOptedInRules(t *testing.T) {
	setupDB(t)
	existing := addTestInboundThroughService(t, 20021)
	opted := newTestRuleWithInbounds(t, "自动纳新", []int{existing.Id}, true)

	// v2ui 迁移路径。只挂 AddInbound 会漏掉这条。
	batch := []*model.Inbound{
		{UserId: 1, Port: 20022, Protocol: model.VLESS, Enable: true,
			Tag: "inbound-20022", Settings: vlessSettings(),
			StreamSettings: plainTCPStream, Sniffing: "{}"},
		{UserId: 1, Port: 20023, Protocol: model.VLESS, Enable: true,
			Tag: "inbound-20023", Settings: vlessSettings(),
			StreamSettings: plainTCPStream, Sniffing: "{}"},
	}
	if err := (&InboundService{}).AddInbounds(batch); err != nil {
		t.Fatalf("AddInbounds: %v", err)
	}

	ids := mustInboundIds(t, opted.Id)
	if len(ids) != 3 {
		t.Errorf("批量建入站后 inboundIds = %v, want 3 个", ids)
	}
}

// 扩散失败必须让建入站整个失败。放行的话是一个用户静默地没进规则——比
// 人工遗漏更隐蔽，管理员以为系统已经处理了。
func TestAddInboundRollsBackWhenAttachFails(t *testing.T) {
	setupDB(t)
	existing := addTestInboundThroughService(t, 20031)
	rule := newTestRuleWithInbounds(t, "自动纳新", []int{existing.Id}, true)
	// 直接改库写进一段无法解码的 JSON，模拟脏数据让 AttachInbound 报错。
	err := database.GetDB().Model(model.RoutingRule{}).Where("id = ?", rule.Id).
		Update("inbound_ids", "{ 坏掉的 JSON").Error
	if err != nil {
		t.Fatalf("注入脏数据: %v", err)
	}

	in := &model.Inbound{
		UserId: 1, Port: 20032, Protocol: model.VLESS, Enable: true,
		Tag: "inbound-20032", Settings: vlessSettings(),
		StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := (&InboundService{}).AddInbound(in); err == nil {
		t.Fatal("扩散失败时 AddInbound 应当报错")
	}

	var count int64
	if err := database.GetDB().Model(model.Inbound{}).
		Where("port = ?", 20032).Count(&count).Error; err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Error("扩散失败但入站落库了，事务没有回滚")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./web/service/ -run 'TestAddInbound' 2>&1 | head -20`
Expected: `TestAddInboundAttachesToOptedInRules` 报「新入站没被加进声明自动应用的规则」；`TestAddInboundRollsBackWhenAttachFails` 报「扩散失败时 AddInbound 应当报错」

- [ ] **Step 3: 改 `AddInbound` 为事务**

把 `web/service/inbound.go` 的 `AddInbound` 末尾两行

```go
	db := database.GetDB()
	return db.Save(inbound).Error
```

换成：

```go
	db := database.GetDB()
	// 事务包住「落库 + 扩散」：扩散失败若放行，就是一个用户静默地没进规则，
	// 正是「自动应用于新增用户」这个功能要消灭的失效。宁可整个失败让管理员
	// 重试。inbound.Id 是自增的，必须 Save 之后才能拿来扩散。
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(inbound).Error; err != nil {
			return err
		}
		return (&RoutingRuleService{}).AttachInbound(tx, inbound.Id)
	})
```

`web/service/inbound.go` 若还没导入 `gorm.io/gorm`，一并加上。

- [ ] **Step 4: 在 `AddInbounds` 里接上**

`AddInbounds` 已经自带事务（`tx := db.Begin()` + defer commit/rollback）。找到循环里 `err = tx.Save(inbound).Error` 之后的错误处理，在其后补：

```go
		// v2ui 迁移路径。只挂 AddInbound 会漏掉它，spec §4.3 三条路径要齐。
		err = (&RoutingRuleService{}).AttachInbound(tx, inbound.Id)
		if err != nil {
			return err
		}
```

注意这里必须赋值给外层那个 `err` 变量（`defer` 靠它决定 commit 还是 rollback），不要用 `:=` 另起一个。

**不要在 `AttachInbound` 里调 `SetToNeedRestart()`**（spec §4.5）。扩散确实改变了配置，但三条路径都已各自处理：面板新建走 `InboundController.addInbound`，成功后就调了（`web/controller/inbound.go:92`）；`bootstrap` 与 `v2ui` 运行时 xray 尚未由面板托管。在 service 层置重启标志也与本项目「controller 负责触发、service 保持无状态」的分层不符。

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestAddInbound' -v 2>&1 | tail -12`
Expected: 三条全部 PASS

- [ ] **Step 6: 提交**

```bash
git add web/service/inbound.go web/service/routing_rule_test.go
git commit -m "feat(routing): 三条建入站路径接入自动扩散，与落库同事务"
```

---

### Task 3: `checkConflict` 新判定 + `Update` 同步

**Files:**
- Modify: `web/service/routing_rule.go`（`checkConflict` 与 `Update`）
- Test: `web/service/routing_rule_test.go`（追加）

**Interfaces:**
- Consumes: 既有的 `intersectGroups(a, b []int) (bool, int)`、`ruleLabel(*model.RoutingRule) string`、`(s *RoutingRuleService) groupLabel(int) string`
- Produces: 无新签名

- [ ] **Step 1: 写失败的测试**

```go
// 不加这条约束，新建入站会被同时扩散进两条规则，它们随即在同一域名组下
// 覆盖同一个入站——系统自己造出违反核心不变量的数据，而管理员什么都没做错。
func TestCheckConflictRejectsSecondAutoApplyRuleInSameGroup(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	first := &model.RoutingRule{
		Remark: "甲", InboundIds: "[3]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}

	second := &model.RoutingRule{
		Remark: "乙", InboundIds: "[9]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	err := s.Add(second)
	if err == nil {
		t.Fatal("同一域名组下第二条声明自动应用的规则应当被拒绝")
	}
	// importRules 靠 strings.Contains(err, "冲突") 把这类错误计入 Skipped
	// 而非 Failed，导入才能保持幂等。
	if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误文案必须含「冲突」二字，实际: %v", err)
	}
}

// 入站集合不重叠时，两条规则本来可以共存；只有都声明自动应用才冲突。
func TestCheckConflictAllowsSecondRuleWithoutAutoApply(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	s := RoutingRuleService{}
	first := &model.RoutingRule{
		Remark: "甲", InboundIds: "[3]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	second := &model.RoutingRule{
		Remark: "乙", InboundIds: "[9]", DomainGroupId: g.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: false,
	}
	if err := s.Add(second); err != nil {
		t.Errorf("未声明自动应用的规则不该被拒绝: %v", err)
	}
}

// 判定单位是域名组不是规则：一条规则可以引用多个组，任一组撞上即冲突。
func TestCheckConflictAutoApplyIsPerDomainGroup(t *testing.T) {
	setupDB(t)
	g1 := newTestGroup(t, "ChatGPT")
	g2 := newTestGroup(t, "Claude")
	s := RoutingRuleService{}
	first := &model.RoutingRule{
		Remark: "甲", InboundIds: "[3]", DomainGroupId: g1.Id,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g1.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	// 乙引用 {Claude, ChatGPT}，与甲在 ChatGPT 上撞车。
	second := &model.RoutingRule{
		Remark: "乙", InboundIds: "[9]", DomainGroupId: 0,
		DomainGroupIds: mustEncodeGroupIds(t, []int{g1.Id, g2.Id}),
		Action:         model.ActionBlock, Enable: true, ApplyToNewInbounds: true,
	}
	if err := s.Add(second); err == nil {
		t.Error("多组规则与已有声明者在任一组上撞车都应当被拒绝")
	}
}

// 表单里有这个复选框，管理员改了就该落库——与相邻的 priority 结论相反。
func TestUpdateSyncsApplyToNewInbounds(t *testing.T) {
	setupDB(t)
	r := newTestRuleWithInbounds(t, "自动纳新", []int{3}, true)
	s := RoutingRuleService{}
	edited, err := s.Get(r.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	edited.ApplyToNewInbounds = false
	if err := s.Update(edited); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(r.Id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.ApplyToNewInbounds {
		t.Error("取消勾选没有落库")
	}
}
```

`web/service/routing_rule_test.go` 已导入 `strings`，无需改 import。

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./web/service/ -run 'TestCheckConflict|TestUpdateSyncs' 2>&1 | head -20`
Expected: `TestCheckConflictRejectsSecondAutoApplyRuleInSameGroup` 报「应当被拒绝」；`TestUpdateSyncsApplyToNewInbounds` 报「取消勾选没有落库」

- [ ] **Step 3: 在 `checkConflict` 里加判定**

在 `checkConflict` 的 `for _, other := range others` 循环里，**`sharedGroup` 判定之后、`otherIds` 解码之前**插入：

```go
		// 两条规则都声明「自动应用于新增用户」且共享域名组时必须拒绝：
		// 新建入站会被同时扩散进两条，它们随即在同一域名组下覆盖同一个入站，
		// 违反本函数守的核心不变量——而那次写入由 AttachInbound 发起，
		// 不走表单校验，这里拦不住就没有第二道防线了。
		//
		// 判定单位是域名组不是规则：一条规则可以引用多个组。
		if rule.ApplyToNewInbounds && other.ApplyToNewInbounds {
			return common.NewErrorf(
				"与分流规则「%s」冲突：域名组「%s」下已有一条声明了「以后新增用户自动应用」的规则。"+
					"同一个域名组下只能有一条规则自动纳入新用户。",
				ruleLabel(other), s.groupLabel(whichGroup))
		}
```

- [ ] **Step 4: 在 `Update` 里同步字段**

在 `web/service/routing_rule.go` 的 `Update` 里，`old.Enable = rule.Enable` **之前**加：

```go
	// 与紧邻的 priority 结论相反：priority 刻意不同步（表单没有那一项，
	// 照抄零值会把规则弹到列表顶部），而这个字段表单里确实有复选框，
	// 管理员改了就该落库。两者相邻，照着隔壁抄就会抄错。
	old.ApplyToNewInbounds = rule.ApplyToNewInbounds
```

同时在上方 `priority` 那段既有注释末尾补一句：

```go
	// （紧随其后的 ApplyToNewInbounds 结论相反，那一项必须同步。）
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestCheckConflict|TestUpdateSyncs' -v 2>&1 | tail -12`
Expected: 四条全部 PASS

- [ ] **Step 6: 跑全部分流测试确认没打破既有行为**

Run: `go test ./web/service/ 2>&1 | tail -3`
Expected: `ok a-ui/web/service`

- [ ] **Step 7: 提交**

```bash
git add web/service/routing_rule.go web/service/routing_rule_test.go
git commit -m "feat(routing): 每个域名组至多一条规则可声明自动纳新"
```

---

### Task 4: 接口与导入导出

**Files:**
- Modify: `web/controller/routing.go`（`routingRuleForm`、`routingRuleView`、`ruleFromForm`、`listRules`）
- Modify: `web/service/routing_portable.go`（`PortableRule`、`toPortableRule`、`importRules`）
- Test: `web/service/routing_portable_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `model.RoutingRule.ApplyToNewInbounds`
- Produces: JSON 字段 `applyToNewInbounds`（接口与导出文件同名）

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_portable_test.go`。走 `Export(ExportScopeAll)` 这个公开入口，与该文件既有测试一致——**不要直接调 `toPortableRule`**，它的签名是 `toPortableRule(r, groupById, nodeById, inboundById)`，四个参数，直接调要自己拼三个 map。该文件已导入 `encoding/json` / `strings` / `database` / `model`，无需改 import。

```go
// 导出必须带上这个标记。丢掉它，导入端的规则不再自动纳新，而导入报告里
// 一个字都不会提。
func TestExportCarriesApplyToNewInbounds(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	in := newPortableTestInbound(t, "用户甲", 2886)
	ids, err := EncodeInboundIds([]int{in.Id})
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	err = (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "自动纳新", InboundIds: ids, DomainGroupId: g.Id,
		DomainGroupIds:     mustEncodeGroupIds(t, []int{g.Id}),
		Action:             model.ActionBlock,
		Enable:             true,
		ApplyToNewInbounds: true,
	})
	if err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("导出的规则数 = %d, want 1", len(f.Rules))
	}
	if !f.Rules[0].ApplyToNewInbounds {
		t.Error("导出丢掉了 applyToNewInbounds")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"applyToNewInbounds":true`) {
		t.Errorf("导出文件的 JSON 里没有这个键: %s", raw)
	}
}

// 旧导出文件没有这个键，必须解成 false（范围缩小，安全侧正确），
// 而不是被当成缺失值报错或猜成 true。
func TestPortableRuleWithoutApplyToNewInboundsDefaultsToFalse(t *testing.T) {
	raw := `{"remark":"旧文件","domainGroupRefs":["ChatGPT"],"inboundRefs":[],"action":"block"}`
	var decoded PortableRule
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ApplyToNewInbounds {
		t.Error("旧文件没有这个键时应当是 false")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./web/service/ -run 'TestExportCarriesApplyToNewInbounds|TestPortableRuleWithout' 2>&1 | head -15`
Expected: 编译失败，`f.Rules[0].ApplyToNewInbounds undefined (type PortableRule has no field or method ApplyToNewInbounds)`

- [ ] **Step 3: 给 `PortableRule` 加字段**

在 `web/service/routing_portable.go` 的 `PortableRule` 结构体里，`Priority int` 那一行附近加：

```go
	// ApplyToNewInbounds 刻意是值类型 bool，与上面的 InboundRefs 指针相反：
	// 那边必须区分「字段缺失」与「显式 []」，因为 [] 在那里另有「对所有入站
	// 生效」这个特殊含义；这边「键缺失」与 false 都只意味着「不自动纳新」，
	// 是同义词，加指针只会多出一处需要解释的不对称。与 PortableDomainGroup
	// 的 Cidrs 取舍相同。
	ApplyToNewInbounds bool `json:"applyToNewInbounds"`
```

- [ ] **Step 4: 导出侧填上**

在 `toPortableRule` 返回的 `PortableRule{...}` 字面量里，`Enable: r.Enable,` 旁边加：

```go
		ApplyToNewInbounds: r.ApplyToNewInbounds,
```

- [ ] **Step 5: 导入侧填上**

在 `importRules` 构造 `&model.RoutingRule{...}` 的地方（约 `:893`），`Priority: item.Priority, Enable: enable,` 旁边加：

```go
			ApplyToNewInbounds: item.ApplyToNewInbounds,
```

导入不需要额外校验：Task 3 的新约束由 `Add` 里的 `checkConflict` 自动继承，冲突会因文案含「冲突」而正确计入 Skipped。

- [ ] **Step 6: 给接口加字段**

`web/controller/routing.go`：

`routingRuleForm` 里 `OutboundId` 之后、`Enable` 之前加：

```go
	// 表单里有这个复选框（与 Priority 不同，那一项已经移除、改由拖拽决定）。
	ApplyToNewInbounds bool `json:"applyToNewInbounds" form:"applyToNewInbounds"`
```

`routingRuleView` 里 `Priority int` 之后加：

```go
	ApplyToNewInbounds bool `json:"applyToNewInbounds"`
```

`ruleFromForm` 返回的 `&model.RoutingRule{...}` 里加：

```go
		ApplyToNewInbounds: form.ApplyToNewInbounds,
```

`listRules` 里 `views = append(views, &routingRuleView{...})` 的字面量里加：

```go
			ApplyToNewInbounds: rule.ApplyToNewInbounds,
```

- [ ] **Step 7: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestExportCarriesApplyToNewInbounds|TestPortableRuleWithout' -v 2>&1 | tail -8`
Expected: 两条 PASS

- [ ] **Step 8: 全量验证**

Run: `make verify 2>&1 | grep -E "FAIL|vet|build -"`
Expected: 无 FAIL，`go vet` 与 `go build` 两行正常出现

- [ ] **Step 9: 提交**

```bash
git add web/controller/routing.go web/service/routing_portable.go web/service/routing_portable_test.go
git commit -m "feat(routing): 接口与导入导出带上自动纳新标记"
```

---

### Task 5: 前端

**Files:**
- Modify: `web/assets/js/model/routing.js:105-135`（`RoutingRule`）
- Modify: `web/html/xui/routing.html`（弹窗、`ruleModal` 初值、`openRule`、`saveRule`）
- Modify: `web/html/xui/component/routing_rule_table.html`（「用户」列标记）
- Test: 由 `web/html_test.go` 既有的两条不变量自动覆盖

**Interfaces:**
- Consumes: Task 4 的 JSON 字段 `applyToNewInbounds`
- Produces: 无

- [ ] **Step 1: 改前端模型（两处都要改）**

`web/assets/js/model/routing.js` 的 `RoutingRule`：构造函数参数表在 `groupsBroken = false` 之后加 `applyToNewInbounds = false`，函数体里加

```js
        // 为真时，以后新建的入站会被服务端自动加进 this.inboundIds。
        // 构造函数与 fromJson 必须同时改：只改一处，服务端返回的值会被
        // 静默丢弃，界面永远显示这里的初始值。
        this.applyToNewInbounds = applyToNewInbounds;
```

`fromJson` 的 `new RoutingRule(...)` 实参末尾补 `json.applyToNewInbounds`。

> **注意 `web/assets/` 下的文件受强缓存**（`max-age=31536000`，靠 `?{{ .cur_ver }}` 破缓存）。开发时用 `XUI_DEBUG=true` 从磁盘读；发版时版本号会变，用户侧自然更新。

- [ ] **Step 2: 弹窗加复选框**

`web/html/xui/routing.html` 的规则弹窗里，「用户（入站）」标题与「所有用户」复选框之后、搜索框之前插入：

```html
                <a-checkbox v-model="ruleModal.rule.applyToNewInbounds"
                            :disabled="ruleModal.allUsers">
                    以后新增的用户自动应用此规则
                </a-checkbox>
```

- [ ] **Step 3: 选中「所有用户」时联动勾上**

在 Vue 实例的 `watch` 里加（若尚无 `watch` 段则新建一个，与 `computed` 平级）：

```js
        watch: {
            // 空数组本来就含未来新建的入站，让人配出「所有用户 + 不应用于
            // 新增」这种自相矛盾的组合毫无意义。后端不做强制改写——
            // AttachInbound 对空数组一律跳过，两种取值行为完全一致。
            'ruleModal.allUsers'(val) {
                if (val) this.ruleModal.rule.applyToNewInbounds = true;
            },
        },
```

- [ ] **Step 4: 新建时默认勾上**

找到 `openRule()` 方法里新建分支构造 `new RoutingRule()` 的地方，在其后加：

```js
                    // 绝大多数分流规则本来就该覆盖所有人；只给特定几个人的
                    // 规则由管理员手动取消。这是【表单初始值】，后端零值仍是
                    // false——否则导入的旧文件也会变成 true。
                    this.ruleModal.rule.applyToNewInbounds = true;
```

编辑分支不要动，它必须显示服务端返回的值。

- [ ] **Step 5: 提交体带上该字段**

`saveRule()` 里已有 `Object.assign({}, r, { inboundIds })`，`r` 是整个 `RoutingRule` 实例，Step 1 加了属性后自动带上。**确认一遍即可，不需要改代码。**

- [ ] **Step 6: 规则列表显示标记**

`web/html/xui/component/routing_rule_table.html` 的 `<template slot="inbound" ...>` 里，在最外层 `</template>` 之前（即用户标签之后）加：

```html
        <a-tooltip v-if="rule.applyToNewInbounds"
                   title="以后新建的入站会自动加入这条规则">
            <a-tag color="blue">+新用户</a-tag>
        </a-tooltip>
```

不显示的话，管理员从列表完全看不出哪些规则会自动纳新——而那正是他需要一眼看到的信息。

- [ ] **Step 7: 跑模板不变量测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectives' -v 2>&1 | tail -12`
Expected: 全部 PASS。

> 这两条一条守「模板能解析」（`web.go` 的 `getHtmlTemplate` 把 `ParseFS` 错误 `// ignore` 掉了，语法错误会被静默跳过），一条守「Vue 指令落在根元素内」（Vue 2 只编译 `el` 指向的子树，写在外面的指令是完全静默的死代码）。改完模板必须跑。

- [ ] **Step 8: 全量验证**

Run: `make verify 2>&1 | grep -E "FAIL|vet|build -"`
Expected: 无 FAIL

- [ ] **Step 9: 提交**

```bash
git add web/assets/js/model/routing.js web/html/xui/routing.html web/html/xui/component/routing_rule_table.html
git commit -m "feat(routing): 规则弹窗与列表支持自动纳新标记"
```

- [ ] **Step 10: 人工验证清单（无法自动化，交给管理员）**

本仓库无法在本地做 UI 视觉验证。升级后请在面板上确认：

1. 新建规则时复选框默认勾上。
2. 勾「所有用户」时复选框自动勾上并变灰。
3. 编辑一条已保存的规则，复选框状态与保存时一致。
4. 规则列表里勾了该项的规则显示 `+新用户` 标签。
5. **端到端**：给某条规则勾上并保存 → 新建一个入站 → 回到分流页，该规则的用户列表里出现这个新入站。
6. 同一域名组下给第二条规则勾上并保存 → 应当被拒绝，提示含「冲突」。
