# 分流配置导出 / 导入 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 域名组、出站节点、分流规则可以分项或整包导出成一个 JSON 文件下载到本地，再上传导入到另一台装了 AetherUI 的机器。

**Architecture:** 新增一个 `web/service/routing_portable.go`，把三张表的内容序列化成**不含任何 id** 的传输结构——跨表引用改写成业务键（出站用 `tag`、域名组用 `remark`、入站用 `{remark, port}` 二元组）。导入时逐条解析引用、逐条落库、逐条报告，不用事务（出站落库前要 exec 真实 xray 校验，包进事务会长时间持有 SQLite 写锁）。冲突一律跳过而非覆盖，导入因此天然幂等。

**Tech Stack:** Go 1.27 / Gin 1.7.1 / GORM+SQLite / Vue 2.6.12 + ant-design-vue 1.7.2（无打包工具，服务端模板）

**Spec:** `docs/superpowers/specs/2026-09-05-routing-import-export-design.md`

## Global Constraints

- 构建必须开 CGO：`export CGO_ENABLED=1`。
- 提交前门禁：`make verify`。
- 测试只用标准库 `testing`，**不引入断言库**。写法照抄 `web/service/routing_rule_test.go`。
- `web/service` 包的 `TestMain` 已在 `routing_validate_test.go:21` 定义，**不要再写第二个**。测试库用同包已有的 `setupDB(t)`（`routing_domain_test.go:11`，内部走 `database.InitDB(filepath.Join(t.TempDir(), "test.db"))`）。
- 错误一律 `common.NewError` / `common.NewErrorf`；日志用 `a-ui/logger`。
- controller 响应走 `jsonMsg` / `jsonObj`。
- 改 `web/html/**` 后必须跑 `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot'`。
- 改 `web/assets/js/**` 后本地必须 `XUI_DEBUG=true go run main.go` 验证（否则命中一年期强缓存）。
- 模板 Vue 插值分隔符是 `[[ ]]`；**所有 Vue 指令必须在 `<a-layout id="app">` 内**。
- 导出结构的 JSON 字段名一经确定不可改——它是跨版本的文件格式。

### 本子系统不可违反的既有不变量

摘自 `docs/superpowers/specs/2026-09-02-domain-routing-design.md` 的实测表（真实 xray 26.7.28）：

| 情形 | xray 反应 | 后果 |
|---|---|---|
| 规则 `inboundTag` 为空数组 | `Configuration OK` | 规则从「只覆盖甲」**放大成覆盖所有入站** |
| 规则 `domain` 为空数组 | `Configuration OK` | 规则**退化成该用户全部流量走某节点** |
| 规则引用已删除的出站 | `Configuration OK` | 运行时**静默回落直连** |

因此本计划中：**导入时绝不能把认不出的入站从 `InboundIds` 里剔掉**。`model.RoutingRule.InboundIds` 为 `"[]"` 表示「对所有入站生效」，是一个必须由用户显式表达的语义，绝不能由"解析失败"降级而来。

---

## File Structure

新增：

| 文件 | 职责 |
|---|---|
| `web/service/routing_portable.go` | 传输结构定义、导出、导入、报告 |
| `web/service/routing_portable_test.go` | 上述全部的单测 |

修改：

| 文件 | 改动 |
|---|---|
| `web/controller/routing.go` | 2 个新路由与 handler |
| `web/html/xui/routing.html` | 6 个按钮 + 2 个 modal + 前端逻辑 |
| `CLAUDE.md` | 新增小节 |

**不改数据模型，不加表，不加列。** `web/assets/js/model/routing.js` 也不动——导出/导入处理的是一次性的传输结构，不是要在界面上持续渲染的对象。

---

### Task 1: 传输结构与导出

**Files:**
- Create: `web/service/routing_portable.go`
- Test: `web/service/routing_portable_test.go`

**Interfaces:**
- Consumes: `DomainGroupService.GetAll()`、`OutboundNodeService.GetAll()`、`RoutingRuleService.GetAll()`、`InboundService.GetAllInbounds()`、`DecodeDomains`、`DecodeInboundIds`
- Produces:
  - `const ExportKind = "a-ui-routing-export"`、`const ExportVersion = 1`
  - `type ExportFile struct { ... }`、`PortableDomainGroup`、`PortableOutbound`、`PortableRule`、`PortableInboundRef`
  - `type RoutingPortableService struct{}`
  - `func (s *RoutingPortableService) Export(scope string) (*ExportFile, error)`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/routing_portable_test.go`：

```go
package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// newTestInbound 建一个最小可用的入站。导入侧要按 remark+port 匹配它。
func newTestInbound(t *testing.T, remark string, port int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId:   1,
		Remark:   remark,
		Enable:   true,
		Port:     port,
		Protocol: model.VMess,
		Tag:      "inbound-" + strconv.Itoa(port),
		Settings: "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	return in
}

func newTestOutbound(t *testing.T, tag, remark string) *model.OutboundNode {
	t.Helper()
	ob := &model.OutboundNode{
		Tag:      tag,
		Remark:   remark,
		Protocol: "socks",
		Config:   `{"tag":"` + tag + `","protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
		Enable:   true,
	}
	if err := database.GetDB().Save(ob).Error; err != nil {
		t.Fatalf("save outbound: %v", err)
	}
	return ob
}

// 导出文件是跨机器的，任何 id 都是本机私有的、到了对面必然指向别的东西。
func TestExportContainsNoIds(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	ob := newTestOutbound(t, "a-ui-hk", "香港")
	in := newTestInbound(t, "用户甲", 2886)
	ids, err := EncodeInboundIds([]int{in.Id})
	if err != nil {
		t.Fatalf("EncodeInboundIds: %v", err)
	}
	rule := &model.RoutingRule{
		Remark: "走香港", InboundIds: ids, DomainGroupId: g.Id,
		Action: model.ActionProxy, OutboundId: ob.Id, Priority: 10, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"id"`) {
		t.Errorf("导出文件里出现了 id 字段: %s", raw)
	}
	if strings.Contains(string(raw), `"domainGroupId"`) || strings.Contains(string(raw), `"outboundId"`) {
		t.Errorf("导出文件里出现了 id 外键: %s", raw)
	}
}

func TestExportRewritesReferencesToBusinessKeys(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	ob := newTestOutbound(t, "a-ui-hk", "香港")
	in := newTestInbound(t, "用户甲", 2886)
	ids, _ := EncodeInboundIds([]int{in.Id})
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "走香港", InboundIds: ids, DomainGroupId: g.Id,
		Action: model.ActionProxy, OutboundId: ob.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("Rules len = %d, want 1", len(f.Rules))
	}
	r := f.Rules[0]
	if r.DomainGroupRef != "ChatGPT" {
		t.Errorf("DomainGroupRef = %q, want ChatGPT", r.DomainGroupRef)
	}
	if r.OutboundRef != "a-ui-hk" {
		t.Errorf("OutboundRef = %q, want a-ui-hk", r.OutboundRef)
	}
	if len(r.InboundRefs) != 1 {
		t.Fatalf("InboundRefs len = %d, want 1", len(r.InboundRefs))
	}
	if r.InboundRefs[0].Remark != "用户甲" || r.InboundRefs[0].Port != 2886 {
		t.Errorf("InboundRefs[0] = %+v", r.InboundRefs[0])
	}
	if f.Kind != ExportKind || f.Version != ExportVersion {
		t.Errorf("Kind/Version = %q/%d", f.Kind, f.Version)
	}
}

// 空数组是「对所有入站生效」，是用户显式表达的语义，必须原样导出。
func TestExportKeepsGlobalRuleAsEmptyRefs(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "违规域名")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "全局封禁", InboundIds: "[]", DomainGroupId: g.Id,
		Action: model.ActionBlock, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}
	f, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Fatalf("Rules len = %d", len(f.Rules))
	}
	if f.Rules[0].InboundRefs == nil {
		t.Error("全局规则的 InboundRefs 应是空切片而不是 nil —— nil 序列化成 null，导入端无法与「字段缺失」区分")
	}
	if len(f.Rules[0].InboundRefs) != 0 {
		t.Errorf("InboundRefs = %+v, want 空", f.Rules[0].InboundRefs)
	}
}

// 订阅域名可达十几万条（生产实例实测 +111226），JSON 化后几 MB 到几十 MB，
// 浏览器一次性 stringify + Blob + 上传端 FileReader 读回来会卡死。
func TestExportOmitsSubscribedDomains(t *testing.T) {
	setupDB(t)
	subscribed, err := EncodeDomains([]string{"domain:sub1.com", "domain:sub2.com"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	manual, _ := EncodeDomains([]string{"domain:manual.com"})
	g := &model.DomainGroup{
		Remark: "订阅组", Domains: manual, SubscribeUrl: "https://example.com/list.txt",
		SubscribedDomains: subscribed, LastUpdatedAt: 1757030400000,
		LastError: "上次失败了", LastSkipped: 7,
	}
	if err := (&DomainGroupService{}).Add(g); err != nil {
		t.Fatalf("Add group: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw, _ := json.Marshal(f)
	for _, forbidden := range []string{"sub1.com", "sub2.com", "lastUpdatedAt", "lastError", "lastSkipped", "subscribedDomains"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("导出文件不该含 %q: %s", forbidden, raw)
		}
	}
	if len(f.DomainGroups) != 1 {
		t.Fatalf("DomainGroups len = %d", len(f.DomainGroups))
	}
	if f.DomainGroups[0].SubscribeUrl != "https://example.com/list.txt" {
		t.Errorf("SubscribeUrl 应保留: %q", f.DomainGroups[0].SubscribeUrl)
	}
	if len(f.DomainGroups[0].Domains) != 1 || f.DomainGroups[0].Domains[0] != "domain:manual.com" {
		t.Errorf("手工域名应保留: %+v", f.DomainGroups[0].Domains)
	}
}

// remark 上没有 unique 约束，两个组重名在库里是合法的。一旦重名，导入端
// 无法确定 domainGroupRef 指向哪一个，猜错会产生一条指向错误域名组的规则
// ——而规则表会渲染得完全正常，配置也会正常生成，只是流量走错了节点，
// 没有任何一层防线会发现。
func TestExportRejectsDuplicateGroupRemarks(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "国内域名")
	newTestGroup(t, "国内域名")
	_, err := (&RoutingPortableService{}).Export(ExportScopeAll)
	if err == nil {
		t.Fatal("域名组重名时应拒绝导出")
	}
	if !strings.Contains(err.Error(), "国内域名") {
		t.Errorf("错误信息应点名是哪个组: %v", err)
	}
}

// 只要导出了 domainGroups 就检查重名，不管有没有一起导出 rules——
// 分项导出的域名组文件将来会被拿去和分项导出的规则文件配套使用。
func TestExportRejectsDuplicateRemarksEvenWithoutRules(t *testing.T) {
	setupDB(t)
	newTestGroup(t, "国内域名")
	newTestGroup(t, "国内域名")
	if _, err := (&RoutingPortableService{}).Export(ExportScopeDomainGroups); err == nil {
		t.Error("只导域名组时同样应拒绝")
	}
}

// 分项导出不隐式扩大范围：scope=rules 就只导规则，不带上它引用的域名组
// 和出站节点。隐式扩大会让 all 和 rules 的区别消失。
func TestExportScopeDoesNotWiden(t *testing.T) {
	setupDB(t)
	g := newTestGroup(t, "ChatGPT")
	ob := newTestOutbound(t, "a-ui-hk", "香港")
	if err := (&RoutingRuleService{}).Add(&model.RoutingRule{
		Remark: "走香港", InboundIds: "[]", DomainGroupId: g.Id,
		Action: model.ActionProxy, OutboundId: ob.Id, Enable: true,
	}); err != nil {
		t.Fatalf("Add rule: %v", err)
	}

	f, err := (&RoutingPortableService{}).Export(ExportScopeRules)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(f.Rules) != 1 {
		t.Errorf("Rules len = %d, want 1", len(f.Rules))
	}
	if len(f.DomainGroups) != 0 {
		t.Errorf("scope=rules 不该带出域名组: %+v", f.DomainGroups)
	}
	if len(f.Outbounds) != 0 {
		t.Errorf("scope=rules 不该带出出站节点: %+v", f.Outbounds)
	}
	if len(f.Scope) != 1 || f.Scope[0] != ExportScopeRules {
		t.Errorf("Scope = %+v", f.Scope)
	}
}

func TestExportRejectsUnknownScope(t *testing.T) {
	setupDB(t)
	if _, err := (&RoutingPortableService{}).Export("everything"); err == nil {
		t.Error("未知 scope 应报错")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestExport' -v
```

预期：编译失败，`undefined: RoutingPortableService`。

- [ ] **Step 3: 写实现**

创建 `web/service/routing_portable.go`：

```go
package service

import (
	"time"

	"a-ui/config"
	"a-ui/database/model"
	"a-ui/util/common"
)

// 导出文件的格式标识与版本。两者都是跨版本契约，改动前必须想清楚
// 旧文件怎么办——导入端认不出 Kind 或 Version 会整体拒绝。
const (
	ExportKind    = "a-ui-routing-export"
	ExportVersion = 1
)

// 导出范围。分项导出不隐式扩大：scope=rules 就只导规则，不带上它引用的
// 域名组和出站节点——隐式扩大会让 all 和 rules 的区别消失。
const (
	ExportScopeAll          = "all"
	ExportScopeDomainGroups = "domainGroups"
	ExportScopeOutbounds    = "outbounds"
	ExportScopeRules        = "rules"
)

// PortableDomainGroup 不含 SubscribedDomains 与 LastUpdatedAt/LastError/
// LastSkipped：前者单个组可达十几万条，后三者是本机这一次拉取的状态，
// 搬到另一台机器毫无意义，还会让新机器显示一个假的「刚刚更新」。
type PortableDomainGroup struct {
	Remark       string   `json:"remark"`
	Domains      []string `json:"domains"`
	SubscribeUrl string   `json:"subscribeUrl"`
}

// PortableOutbound 保留原 tag：规则靠 tag 对上引用，导入端重新分配会让
// 所有引用它的规则失效。
type PortableOutbound struct {
	Tag      string `json:"tag"`
	Remark   string `json:"remark"`
	Protocol string `json:"protocol"`
	Config   string `json:"config"`
	Enable   bool   `json:"enable"`
}

// PortableInboundRef 是入站的跨机器线索。
//
// 入站的三个候选键都不可靠：Id 跨机器无意义且 SQLite 会复用；Tag 由端口
// 算出（UpdateInbound 里 Tag = "inbound-<port>"），存 tag 等于存端口；
// Remark 可重复。所以两个都给，让导入端自己两级匹配并在判断不了时诚实
// 地说出来。
type PortableInboundRef struct {
	Remark string `json:"remark"`
	Port   int    `json:"port"`
}

type PortableRule struct {
	Remark         string `json:"remark"`
	DomainGroupRef string `json:"domainGroupRef"`
	// OutboundRef 在 action=block 时为空。
	OutboundRef string `json:"outboundRef"`
	// InboundRefs 为空切片表示「对所有入站生效」，是用户显式表达的语义。
	// 必须序列化成 [] 而不是 null——导入端要能把它与「字段缺失」区分开。
	InboundRefs []PortableInboundRef `json:"inboundRefs"`
	Action      string               `json:"action"`
	Priority    int                  `json:"priority"`
	Enable      bool                 `json:"enable"`
}

type ExportFile struct {
	Kind         string                `json:"kind"`
	Version      int                   `json:"version"`
	ExportedAt   int64                 `json:"exportedAt"`
	ExportedBy   string                `json:"exportedBy"`
	Scope        []string              `json:"scope"`
	DomainGroups []PortableDomainGroup `json:"domainGroups"`
	Outbounds    []PortableOutbound    `json:"outbounds"`
	Rules        []PortableRule        `json:"rules"`
}

type RoutingPortableService struct {
	domainGroupService DomainGroupService
	outboundService    OutboundNodeService
	ruleService        RoutingRuleService
	inboundService     InboundService
}

func scopeIncludes(scope, want string) bool {
	return scope == ExportScopeAll || scope == want
}

func validExportScope(scope string) bool {
	switch scope {
	case ExportScopeAll, ExportScopeDomainGroups, ExportScopeOutbounds, ExportScopeRules:
		return true
	}
	return false
}

func (s *RoutingPortableService) Export(scope string) (*ExportFile, error) {
	if !validExportScope(scope) {
		return nil, common.NewErrorf("未知的导出范围：%q", scope)
	}

	groups, err := s.domainGroupService.GetAll()
	if err != nil {
		return nil, err
	}
	// 重名检查不看 scope：规则文件与域名组文件将来会被配套使用，
	// 只在导出规则时才检查会留下一个可被绕开的洞。
	if err := checkDuplicateGroupRemarks(groups); err != nil {
		return nil, err
	}

	f := &ExportFile{
		Kind:         ExportKind,
		Version:      ExportVersion,
		ExportedAt:   time.Now().UnixMilli(),
		ExportedBy:   "a-ui " + config.GetVersion(),
		Scope:        []string{},
		DomainGroups: []PortableDomainGroup{},
		Outbounds:    []PortableOutbound{},
		Rules:        []PortableRule{},
	}
	if scope == ExportScopeAll {
		f.Scope = []string{ExportScopeDomainGroups, ExportScopeOutbounds, ExportScopeRules}
	} else {
		f.Scope = []string{scope}
	}

	if scopeIncludes(scope, ExportScopeDomainGroups) {
		for _, g := range groups {
			// 解码失败当作空列表：这个组本身已经损坏（buildRule 会丢弃引用
			// 它的规则），但组的备注和订阅地址仍是有用的，导出它比整个
			// 导出失败对管理员更有帮助。
			manual, err := DecodeDomains(g.Domains)
			if err != nil {
				manual = nil
			}
			if manual == nil {
				manual = []string{}
			}
			f.DomainGroups = append(f.DomainGroups, PortableDomainGroup{
				Remark:       g.Remark,
				Domains:      manual,
				SubscribeUrl: g.SubscribeUrl,
			})
		}
	}

	if scopeIncludes(scope, ExportScopeOutbounds) {
		nodes, err := s.outboundService.GetAll()
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			f.Outbounds = append(f.Outbounds, PortableOutbound{
				Tag: n.Tag, Remark: n.Remark, Protocol: n.Protocol,
				Config: n.Config, Enable: n.Enable,
			})
		}
	}

	if scopeIncludes(scope, ExportScopeRules) {
		rules, err := s.ruleService.GetAll()
		if err != nil {
			return nil, err
		}
		groupById := make(map[int]*model.DomainGroup, len(groups))
		for _, g := range groups {
			groupById[g.Id] = g
		}
		nodes, err := s.outboundService.GetAll()
		if err != nil {
			return nil, err
		}
		nodeById := make(map[int]*model.OutboundNode, len(nodes))
		for _, n := range nodes {
			nodeById[n.Id] = n
		}
		inbounds, err := s.inboundService.GetAllInbounds()
		if err != nil {
			return nil, err
		}
		inboundById := make(map[int]*model.Inbound, len(inbounds))
		for _, in := range inbounds {
			inboundById[in.Id] = in
		}

		for _, r := range rules {
			pr, skip := s.toPortableRule(r, groupById, nodeById, inboundById)
			if skip != nil {
				// 引用已经悬空的规则本来就不会写进配置，导出它只会在对面
				// 产生同样一条不生效的规则，还占着 checkConflict 的位置。
				logger.Warning("导出跳过规则「", ruleLabel(r), "」：", skip)
				continue
			}
			f.Rules = append(f.Rules, pr)
		}
	}

	return f, nil
}

// checkDuplicateGroupRemarks 是导出侧唯一的硬拒绝。
func checkDuplicateGroupRemarks(groups []*model.DomainGroup) error {
	seen := make(map[string]bool, len(groups))
	dups := make([]string, 0)
	for _, g := range groups {
		if seen[g.Remark] {
			dups = append(dups, g.Remark)
			continue
		}
		seen[g.Remark] = true
	}
	if len(dups) == 0 {
		return nil
	}
	return common.NewErrorf(
		"域名组备注重复：%s。导出文件用备注引用域名组，重名会让导入端无法确定规则指向哪一个，请先改名再导出。",
		strings.Join(dups, "、"))
}

func (s *RoutingPortableService) toPortableRule(
	r *model.RoutingRule,
	groupById map[int]*model.DomainGroup,
	nodeById map[int]*model.OutboundNode,
	inboundById map[int]*model.Inbound,
) (PortableRule, error) {
	g, ok := groupById[r.DomainGroupId]
	if !ok {
		return PortableRule{}, common.NewErrorf("域名组 #%d 不存在", r.DomainGroupId)
	}
	outboundRef := ""
	if r.Action == model.ActionProxy {
		n, ok := nodeById[r.OutboundId]
		if !ok {
			return PortableRule{}, common.NewErrorf("出站节点 #%d 不存在", r.OutboundId)
		}
		outboundRef = n.Tag
	}
	ids, err := DecodeInboundIds(r.InboundIds)
	if err != nil {
		return PortableRule{}, common.NewError("入站数据损坏:", err)
	}
	// 空切片而不是 nil：nil 会被 encoding/json 序列化成 null，导入端就无法
	// 把「对所有入站生效」和「字段缺失」区分开——而这两者的后果天差地别。
	refs := make([]PortableInboundRef, 0, len(ids))
	for _, id := range ids {
		in, ok := inboundById[id]
		if !ok {
			// 悬空引用，本机上这条规则的这一部分已经不生效了。整条跳过，
			// 不能只剔掉这一个——剔到最后变成空数组就是「对所有人生效」。
			return PortableRule{}, common.NewErrorf("入站 #%d 不存在", id)
		}
		refs = append(refs, PortableInboundRef{Remark: in.Remark, Port: in.Port})
	}
	return PortableRule{
		Remark:         r.Remark,
		DomainGroupRef: g.Remark,
		OutboundRef:    outboundRef,
		InboundRefs:    refs,
		Action:         r.Action,
		Priority:       r.Priority,
		Enable:         r.Enable,
	}, nil
}
```

在 import 里补 `"strings"` 与 `"a-ui/logger"`。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestExport' -v
go vet ./web/service/
```

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_portable.go web/service/routing_portable_test.go
git commit -m "feat(routing): 分流配置导出，引用全部改写成业务键

导出文件不含任何 id：出站用 tag（unique 且不可变）、域名组用 remark、
入站用 remark+port 二元组。

域名组重名直接拒绝导出——remark 没有 unique 约束，重名会让导入端无法确定
规则指向哪一个，猜错会产生一条指向错误域名组的规则，而规则表会渲染得完全
正常、配置也会正常生成，只是流量走错节点，没有任何一层防线会发现。

全局规则的 InboundRefs 序列化成 [] 而不是 null：导入端必须能把「对所有入站
生效」与「字段缺失」区分开。"
```

---

### Task 2: 导入的引用解析（纯逻辑）

把最容易出错的入站匹配单独拆出来先钉死，它不碰数据库落库路径，只做查表。

**Files:**
- Modify: `web/service/routing_portable.go`（追加）
- Modify: `web/service/routing_portable_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `PortableInboundRef`
- Produces:
  - `func resolveInboundRefs(refs []PortableInboundRef, inbounds []*model.Inbound) (ids []int, missing []string)`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_portable_test.go`：

```go
func inboundsFixture() []*model.Inbound {
	return []*model.Inbound{
		{Id: 1, Remark: "用户甲", Port: 2886},
		{Id: 2, Remark: "用户乙", Port: 2887},
		{Id: 3, Remark: "重名", Port: 2888},
		{Id: 4, Remark: "重名", Port: 2889},
	}
}

func TestResolveInboundRefsMatchesByRemark(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "用户甲", Port: 9999}}, inboundsFixture())
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty", missing)
	}
	// 备注优先于端口：换机器后端口很可能改了，备注才是管理员认得的东西
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("ids = %v, want [1]", ids)
	}
}

func TestResolveInboundRefsFallsBackToPort(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "对面才有的备注", Port: 2887}}, inboundsFixture())
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Errorf("ids = %v, want [2]", ids)
	}
}

// 备注重名时无法区分是哪一个，不能猜——退到端口匹配。
func TestResolveInboundRefsSkipsAmbiguousRemark(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "重名", Port: 2889}}, inboundsFixture())
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if len(ids) != 1 || ids[0] != 4 {
		t.Errorf("ids = %v, want [4] —— 备注重名应退到端口匹配", ids)
	}
}

// 备注重名且端口也对不上，就是真的认不出来了。
func TestResolveInboundRefsReportsMissing(t *testing.T) {
	ids, missing := resolveInboundRefs(
		[]PortableInboundRef{{Remark: "重名", Port: 7777}}, inboundsFixture())
	if len(missing) != 1 {
		t.Fatalf("missing = %v, want 1 项", missing)
	}
	if !strings.Contains(missing[0], "重名") || !strings.Contains(missing[0], "7777") {
		t.Errorf("missing 描述应同时含备注与端口，便于管理员对号入座: %q", missing[0])
	}
	if len(ids) != 0 {
		t.Errorf("认不出时不该返回任何 id: %v", ids)
	}
}

// 部分命中时返回已命中的 id 和缺失清单。调用方据此决定禁用整条规则——
// 绝不能拿这个部分列表当作完整覆盖集去启用规则。
func TestResolveInboundRefsPartialMatch(t *testing.T) {
	ids, missing := resolveInboundRefs([]PortableInboundRef{
		{Remark: "用户甲", Port: 2886},
		{Remark: "不存在的人", Port: 7777},
	}, inboundsFixture())
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("ids = %v, want [1]", ids)
	}
	if len(missing) != 1 {
		t.Errorf("missing = %v, want 1 项", missing)
	}
}

// 空 refs 是「对所有入站生效」，是合法且必须原样保留的语义，不是「认不出」。
func TestResolveInboundRefsEmptyMeansGlobal(t *testing.T) {
	ids, missing := resolveInboundRefs([]PortableInboundRef{}, inboundsFixture())
	if len(missing) != 0 {
		t.Errorf("空 refs 不该产生 missing: %v", missing)
	}
	if len(ids) != 0 {
		t.Errorf("空 refs 应返回空 ids: %v", ids)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestResolveInboundRefs' -v
```

预期：`undefined: resolveInboundRefs`。

- [ ] **Step 3: 写实现**

追加到 `web/service/routing_portable.go`：

```go
// resolveInboundRefs 把导出文件里的入站线索映射到本机的入站 id。
//
// 两级匹配：先按 remark 精确匹配（**恰好命中 1 条才算**，重名视为无法
// 区分），失败再按 port 匹配（port 有 unique 约束，命中即唯一）。
// 备注优先是因为换机器后端口很可能改了，而备注是管理员认得的东西。
//
// 返回的 missing 非空时，调用方**必须**把整条规则导入成禁用状态，
// 绝不能拿部分命中的 ids 当作完整覆盖集：剔掉认不出的那几个之后，
// 一条本该只覆盖某个人的规则会被缩小或（剔到空时）放大成覆盖全体，
// 而 xray 对空 inboundTag 返回 Configuration OK，不会有任何报错。
func resolveInboundRefs(refs []PortableInboundRef, inbounds []*model.Inbound) ([]int, []string) {
	byRemark := make(map[string][]*model.Inbound, len(inbounds))
	byPort := make(map[int]*model.Inbound, len(inbounds))
	for _, in := range inbounds {
		byRemark[in.Remark] = append(byRemark[in.Remark], in)
		byPort[in.Port] = in
	}

	ids := make([]int, 0, len(refs))
	missing := make([]string, 0)
	for _, ref := range refs {
		if matched := byRemark[ref.Remark]; len(matched) == 1 {
			ids = append(ids, matched[0].Id)
			continue
		}
		if in, ok := byPort[ref.Port]; ok {
			ids = append(ids, in.Id)
			continue
		}
		missing = append(missing, common.NewErrorf("%s (端口 %d)", ref.Remark, ref.Port).Error())
	}
	return ids, missing
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestResolveInboundRefs|TestExport' -v
```

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_portable.go web/service/routing_portable_test.go
git commit -m "feat(routing): 导入侧的入站两级匹配

先按备注精确匹配（恰好一条才算，重名视为无法区分），失败退到端口。
备注优先是因为换机器后端口很可能改了。

missing 非空时调用方必须整条禁用，绝不能拿部分命中的 ids 当完整覆盖集
——剔掉认不出的之后规则会被缩小，剔到空就是放大成覆盖全体，而 xray 对
空 inboundTag 返回 Configuration OK。"
```

---

### Task 3: 导入落库与报告

**Files:**
- Modify: `web/service/routing_portable.go`（追加）
- Modify: `web/service/routing_portable_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的全部类型、Task 2 的 `resolveInboundRefs`、`ParseDomains` / `ValidateDomains` / `EncodeDomains` / `ValidateSubscribeURL` / `ValidateOutbound` / `EncodeInboundIds` / `model.IsReservedTag` / `RoutingRuleService.Add`
- Produces:
  - `type ImportCounts struct { Created, Skipped, Failed int }`
  - `type ImportReport struct { DomainGroups, Outbounds, Rules ImportCounts; Messages []string }`
  - `func (s *RoutingPortableService) Import(raw string) (*ImportReport, error)`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/routing_portable_test.go`：

```go
func exportJSON(t *testing.T, f *ExportFile) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func baseExportFile() *ExportFile {
	return &ExportFile{
		Kind: ExportKind, Version: ExportVersion,
		Scope:        []string{ExportScopeDomainGroups, ExportScopeOutbounds, ExportScopeRules},
		DomainGroups: []PortableDomainGroup{},
		Outbounds:    []PortableOutbound{},
		Rules:        []PortableRule{},
	}
}

func TestImportRejectsWrongKind(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Kind = "something-else"
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err == nil {
		t.Error("Kind 不对应整体拒绝")
	}
}

func TestImportRejectsWrongVersion(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Version = 999
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err == nil {
		t.Error("Version 不认识应整体拒绝")
	}
}

func TestImportRejectsMalformedJSON(t *testing.T) {
	setupDB(t)
	if _, err := (&RoutingPortableService{}).Import("{not json"); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

func TestImportCreatesDomainGroupAndOutbound(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{
		{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}, SubscribeUrl: ""},
	}
	f.Outbounds = []PortableOutbound{
		{Tag: "a-ui-hk", Remark: "香港", Protocol: "socks",
			Config: `{"tag":"a-ui-hk","protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
			Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.DomainGroups.Created != 1 {
		t.Errorf("DomainGroups.Created = %d, want 1 (%v)", rep.DomainGroups.Created, rep.Messages)
	}
	if rep.Outbounds.Created != 1 {
		t.Errorf("Outbounds.Created = %d, want 1 (%v)", rep.Outbounds.Created, rep.Messages)
	}
	groups, _ := (&DomainGroupService{}).GetAll()
	if len(groups) != 1 || groups[0].Remark != "ChatGPT" {
		t.Errorf("库里的域名组不对: %+v", groups)
	}
	nodes, _ := (&OutboundNodeService{}).GetAll()
	if len(nodes) != 1 || nodes[0].Tag != "a-ui-hk" {
		t.Errorf("出站节点的 tag 必须原样保留，否则规则引用会失效: %+v", nodes)
	}
}

// 同一个文件导两次不该变成双份，也不该有第二次的副作用。
func TestImportIsIdempotent(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Outbounds = []PortableOutbound{
		{Tag: "a-ui-hk", Remark: "香港", Protocol: "socks",
			Config: `{"tag":"a-ui-hk","protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
			Enable: true},
	}
	raw := exportJSON(t, f)
	s := RoutingPortableService{}
	if _, err := s.Import(raw); err != nil {
		t.Fatalf("首次 Import: %v", err)
	}
	rep, err := s.Import(raw)
	if err != nil {
		t.Fatalf("二次 Import: %v", err)
	}
	if rep.DomainGroups.Created != 0 || rep.DomainGroups.Skipped != 1 {
		t.Errorf("域名组应全部跳过: %+v", rep.DomainGroups)
	}
	if rep.Outbounds.Created != 0 || rep.Outbounds.Skipped != 1 {
		t.Errorf("出站节点应全部跳过: %+v", rep.Outbounds)
	}
	groups, _ := (&DomainGroupService{}).GetAll()
	if len(groups) != 1 {
		t.Errorf("域名组变成了 %d 份", len(groups))
	}
}

// a-ui-block / a-ui-default 是注入器自己发的 tag，不在 outbound_nodes 表里，
// 数据库唯一约束看不见它们。撞名会让 xray 报 existing tag found 并拒绝启动
// 整份配置——全员断网，而面板首页仍显示 running。
func TestImportRejectsReservedTag(t *testing.T) {
	setupDB(t)
	for _, tag := range []string{model.BlockOutboundTag, model.DefaultOutboundTag} {
		f := baseExportFile()
		f.Outbounds = []PortableOutbound{
			{Tag: tag, Remark: "坏节点", Protocol: "socks",
				Config: `{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":1080}]}}`,
				Enable: true},
		}
		rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if rep.Outbounds.Failed != 1 {
			t.Errorf("tag %s 应被拒绝: %+v %v", tag, rep.Outbounds, rep.Messages)
		}
		nodes, _ := (&OutboundNodeService{}).GetAll()
		if len(nodes) != 0 {
			t.Errorf("保留 tag 的节点不该落库: %+v", nodes)
		}
	}
}

func TestImportRejectsEmptyTag(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Outbounds = []PortableOutbound{
		{Tag: "", Remark: "没 tag", Protocol: "socks",
			Config: `{"protocol":"socks","settings":{}}`, Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Outbounds.Failed != 1 {
		t.Errorf("空 tag 应被拒绝: %+v", rep.Outbounds)
	}
}

// "null" 能通过 json.Unmarshal 却留下一个 nil map，紧接着给它赋值直接
// panic（routing_outbound.go 的 Update 里就记着这个坑）。
func TestImportHandlesNullConfig(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Outbounds = []PortableOutbound{
		{Tag: "a-ui-bad", Remark: "坏配置", Protocol: "socks", Config: "null", Enable: true},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import 不该 panic 或整体失败: %v", err)
	}
	if rep.Outbounds.Failed != 1 {
		t.Errorf("null config 应被拒绝: %+v", rep.Outbounds)
	}
}

// ===== 本文件最重要的一条：入站认不全时必须禁用，绝不清空 =====
func TestImportDisablesRuleWhenInboundMissing(t *testing.T) {
	setupDB(t)
	newTestInbound(t, "用户甲", 2886)

	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "走香港", DomainGroupRef: "ChatGPT", OutboundRef: "",
		InboundRefs: []PortableInboundRef{
			{Remark: "用户甲", Port: 2886},
			{Remark: "对面才有的用户", Port: 9999},
		},
		Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Created != 1 {
		t.Fatalf("规则应被导入（禁用状态）: %+v %v", rep.Rules, rep.Messages)
	}
	rules, err := (&RoutingRuleService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d", len(rules))
	}
	r := rules[0]
	if r.Enable {
		t.Error("入站认不全的规则必须导入为禁用")
	}
	if r.InboundIds == "[]" {
		t.Fatal("InboundIds 被清空成了 []，这等于「对所有入站生效」—— " +
			"一条本该只覆盖某个人的规则被静默放大到全体，而 xray 对此返回 Configuration OK")
	}
	ids, err := DecodeInboundIds(r.InboundIds)
	if err != nil {
		t.Fatalf("DecodeInboundIds: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("应保留已命中的那一个入站: %v", ids)
	}
	joined := strings.Join(rep.Messages, "\n")
	if !strings.Contains(joined, "对面才有的用户") {
		t.Errorf("报告里应点名缺失的入站: %v", rep.Messages)
	}
}

func TestImportKeepsEnabledWhenAllInboundsMatch(t *testing.T) {
	setupDB(t)
	newTestInbound(t, "用户甲", 2886)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "ChatGPT", Domains: []string{"domain:openai.com"}}}
	f.Rules = []PortableRule{{
		Remark: "封禁", DomainGroupRef: "ChatGPT",
		InboundRefs: []PortableInboundRef{{Remark: "用户甲", Port: 2886}},
		Action:      model.ActionBlock, Enable: true,
	}}
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err != nil {
		t.Fatalf("Import: %v", err)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 1 || !rules[0].Enable {
		t.Errorf("全部命中时应保持文件里的 enable: %+v", rules)
	}
}

func TestImportKeepsGlobalRuleGlobal(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{{Remark: "违规", Domains: []string{"domain:bad.com"}}}
	f.Rules = []PortableRule{{
		Remark: "全局封禁", DomainGroupRef: "违规",
		InboundRefs: []PortableInboundRef{}, // 显式的「所有入站」
		Action:      model.ActionBlock, Enable: true,
	}}
	if _, err := (&RoutingPortableService{}).Import(exportJSON(t, f)); err != nil {
		t.Fatalf("Import: %v", err)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 1 {
		t.Fatalf("规则数 = %d", len(rules))
	}
	if rules[0].InboundIds != "[]" {
		t.Errorf("全局规则应保持 []: %q", rules[0].InboundIds)
	}
	if !rules[0].Enable {
		t.Error("全局规则不该被误判成「认不出」而禁用")
	}
}

func TestImportSkipsRuleWithMissingGroupRef(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.Rules = []PortableRule{{
		Remark: "孤儿规则", DomainGroupRef: "本机没有的组",
		InboundRefs: []PortableInboundRef{}, Action: model.ActionBlock, Enable: true,
	}}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.Rules.Failed != 1 {
		t.Errorf("引用不到域名组的规则应整条跳过: %+v", rep.Rules)
	}
	rules, _ := (&RoutingRuleService{}).GetAll()
	if len(rules) != 0 {
		t.Errorf("不该落库: %+v", rules)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "本机没有的组") {
		t.Errorf("报告应点名: %v", rep.Messages)
	}
}

// 新导入的订阅组 LastUpdatedAt = 0，ShouldUpdateNow 对 0 直接返回 true，
// SubscriptionJob 每 10 分钟一次会自动补上首次拉取。导入路径本身不拉——
// 一个慢地址能把 HTTP 请求挂满 30 秒。
func TestImportSubscribedGroupStartsUnfetched(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{
		{Remark: "订阅组", Domains: []string{}, SubscribeUrl: "https://example.com/list.txt"},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	groups, _ := (&DomainGroupService{}).GetAll()
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].LastUpdatedAt != 0 {
		t.Errorf("LastUpdatedAt = %d, want 0（0 才会被 ShouldUpdateNow 立即拉取）", groups[0].LastUpdatedAt)
	}
	if groups[0].SubscribedDomains != "" {
		t.Errorf("SubscribedDomains 应为空: %q", groups[0].SubscribedDomains)
	}
	if !strings.Contains(strings.Join(rep.Messages, "\n"), "订阅") {
		t.Errorf("报告应提示订阅组还没拉取: %v", rep.Messages)
	}
}

func TestImportRejectsBadSubscribeUrl(t *testing.T) {
	setupDB(t)
	f := baseExportFile()
	f.DomainGroups = []PortableDomainGroup{
		{Remark: "坏订阅", Domains: []string{}, SubscribeUrl: "ftp://example.com/x"},
	}
	rep, err := (&RoutingPortableService{}).Import(exportJSON(t, f))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.DomainGroups.Failed != 1 {
		t.Errorf("非 http(s) 订阅地址应被拒: %+v", rep.DomainGroups)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestImport' -v
```

预期：`undefined: ImportReport` 等。

- [ ] **Step 3: 写实现**

追加到 `web/service/routing_portable.go`：

```go
type ImportCounts struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"` // 本机已存在
	Failed  int `json:"failed"`
}

type ImportReport struct {
	DomainGroups ImportCounts `json:"domainGroups"`
	Outbounds    ImportCounts `json:"outbounds"`
	Rules        ImportCounts `json:"rules"`
	// Messages 是人话，逐条说明每一个非 Created 的结果。前端用 modal 展示，
	// 不能用 $message——可能有几十行。
	Messages []string `json:"messages"`
}

func (r *ImportReport) say(format string, a ...any) {
	r.Messages = append(r.Messages, common.NewErrorf(format, a...).Error())
}

// Import 逐条处理、逐条报告，**不用事务**。
//
// 出站节点落库前要 exec 真实 xray 校验，一次几百毫秒且策略是 fail open。
// 包进事务会在校验期间长时间持有 SQLite 那把写锁，把整个面板（含每 10 秒
// 的流量统计、每秒的并发判定）一起卡住。这与 routing_validate.go 里
// 「落库之前校验，因此不需要事务回滚」的取向一致。
//
// 代价是导入可能「成功一半」。可接受：每条的成败都在报告里，而且导入是
// 幂等的（冲突一律跳过），重跑一次就补齐了。
func (s *RoutingPortableService) Import(raw string) (*ImportReport, error) {
	var f ExportFile
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, common.NewError("导入文件不是合法的 JSON:", err)
	}
	if f.Kind != ExportKind {
		return nil, common.NewErrorf("不是 AetherUI 的分流配置文件（kind=%q）", f.Kind)
	}
	if f.Version != ExportVersion {
		return nil, common.NewErrorf(
			"导入文件版本 %d 与当前面板支持的版本 %d 不一致，请用同版本的面板导出",
			f.Version, ExportVersion)
	}

	report := &ImportReport{Messages: []string{}}
	changed := false

	if s.importDomainGroups(f.DomainGroups, report) {
		changed = true
	}
	if s.importOutbounds(f.Outbounds, report) {
		changed = true
	}
	if s.importRules(f.Rules, report) {
		changed = true
	}

	if changed {
		// 复用既有链路：置原子标志 → InboundController 的 10 秒 cron 消费
		// → RestartXray(false) → Config.Equals 察觉 RouterConfig/OutboundConfigs
		// 变了 → 先试热应用，不行才整进程重启。管理员不需要额外操作。
		(&XrayService{}).SetToNeedRestart()
	}
	return report, nil
}

func (s *RoutingPortableService) importDomainGroups(items []PortableDomainGroup, report *ImportReport) bool {
	if len(items) == 0 {
		return false
	}
	existing, err := s.domainGroupService.GetAll()
	if err != nil {
		report.say("读取本机域名组失败：%v", err)
		report.DomainGroups.Failed += len(items)
		return false
	}
	byRemark := make(map[string]bool, len(existing))
	for _, g := range existing {
		byRemark[g.Remark] = true
	}

	changed := false
	subscribedCount := 0
	for _, item := range items {
		if item.Remark == "" {
			report.DomainGroups.Failed++
			report.say("有一个域名组的备注为空，已跳过")
			continue
		}
		if byRemark[item.Remark] {
			report.DomainGroups.Skipped++
			report.say("域名组「%s」已存在，跳过", item.Remark)
			continue
		}
		// 走与表单同一条校验路径。导入文件是不可信输入，与管理员在表单里
		// 输入的东西同级。
		encoded := "[]"
		if len(item.Domains) > 0 {
			list, err := ParseDomains(strings.Join(item.Domains, "\n"))
			if err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」的域名格式有误：%v", item.Remark, err)
				continue
			}
			if err := ValidateDomains(list); err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」的域名未通过校验：%v", item.Remark, err)
				continue
			}
			encoded, err = EncodeDomains(list)
			if err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」编码失败：%v", item.Remark, err)
				continue
			}
		}
		if item.SubscribeUrl != "" {
			if err := ValidateSubscribeURL(item.SubscribeUrl); err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」的订阅地址非法：%v", item.Remark, err)
				continue
			}
		}
		// LastUpdatedAt 留 0：ShouldUpdateNow 对 0 直接返回 true，
		// SubscriptionJob（每 10 分钟）会自动补上首次拉取。这里不同步拉，
		// 一个慢地址能把这个 HTTP 请求挂满 30 秒。
		g := &model.DomainGroup{
			Remark: item.Remark, Domains: encoded, SubscribeUrl: item.SubscribeUrl,
		}
		if err := s.domainGroupService.Add(g); err != nil {
			report.DomainGroups.Failed++
			report.say("域名组「%s」写库失败：%v", item.Remark, err)
			continue
		}
		byRemark[item.Remark] = true
		report.DomainGroups.Created++
		changed = true
		if item.SubscribeUrl != "" {
			subscribedCount++
		}
	}
	if subscribedCount > 0 {
		report.say("%d 个域名组已加入订阅，最迟 10 分钟内完成首次拉取；在此之前，仅依赖订阅内容的规则不会写进配置",
			subscribedCount)
	}
	return changed
}

func (s *RoutingPortableService) importOutbounds(items []PortableOutbound, report *ImportReport) bool {
	if len(items) == 0 {
		return false
	}
	existing, err := s.outboundService.GetAll()
	if err != nil {
		report.say("读取本机出站节点失败：%v", err)
		report.Outbounds.Failed += len(items)
		return false
	}
	byTag := make(map[string]bool, len(existing))
	for _, n := range existing {
		byTag[n.Tag] = true
	}

	changed := false
	for _, item := range items {
		if item.Tag == "" || len(item.Tag) > 128 {
			report.Outbounds.Failed++
			report.say("出站节点「%s」的 tag 为空或过长，已跳过", item.Remark)
			continue
		}
		// 保留 tag 不在 outbound_nodes 表里，数据库唯一约束看不见它们。
		// 撞名会让 xray 报 existing tag found 并拒绝启动整份配置——全员断网，
		// 而面板首页仍显示 running。判定统一走 model.IsReservedTag。
		if model.IsReservedTag(item.Tag) {
			report.Outbounds.Failed++
			report.say("出站节点「%s」的 tag %s 是系统保留 tag，拒绝导入", item.Remark, item.Tag)
			continue
		}
		if byTag[item.Tag] {
			report.Outbounds.Skipped++
			report.say("出站节点 %s 已存在，跳过", item.Tag)
			continue
		}

		var ob map[string]any
		if err := json.Unmarshal([]byte(item.Config), &ob); err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 的配置不是合法 JSON：%v", item.Tag, err)
			continue
		}
		// "null" 能通过 Unmarshal 却留下一个 nil map，下一行赋值直接 panic。
		if ob == nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 的配置为 null", item.Tag)
			continue
		}
		ob["tag"] = item.Tag
		// 与新增/编辑路径同样过真实 xray 校验：一个坏配置会让整份
		// bin/config.json 加载失败、全员断网。fail open 策略照旧——
		// xray 二进制缺失或超时时 ValidateOutbound 会放行并记日志。
		if err := ValidateOutbound(ob); err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 未通过 xray 校验：%v", item.Tag, err)
			continue
		}
		encoded, err := json.Marshal(ob)
		if err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 编码失败：%v", item.Tag, err)
			continue
		}
		protocol := item.Protocol
		if p, ok := ob["protocol"].(string); ok && p != "" {
			protocol = p
		}
		node := &model.OutboundNode{
			Tag: item.Tag, Remark: item.Remark, Protocol: protocol,
			Config: string(encoded), Enable: item.Enable,
		}
		if err := database.GetDB().Save(node).Error; err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 写库失败：%v", item.Tag, err)
			continue
		}
		byTag[item.Tag] = true
		report.Outbounds.Created++
		changed = true
	}
	return changed
}

func (s *RoutingPortableService) importRules(items []PortableRule, report *ImportReport) bool {
	if len(items) == 0 {
		return false
	}
	groups, err := s.domainGroupService.GetAll()
	if err != nil {
		report.say("读取本机域名组失败：%v", err)
		report.Rules.Failed += len(items)
		return false
	}
	groupByRemark := make(map[string]*model.DomainGroup, len(groups))
	for _, g := range groups {
		groupByRemark[g.Remark] = g
	}
	nodes, err := s.outboundService.GetAll()
	if err != nil {
		report.say("读取本机出站节点失败：%v", err)
		report.Rules.Failed += len(items)
		return false
	}
	nodeByTag := make(map[string]*model.OutboundNode, len(nodes))
	for _, n := range nodes {
		nodeByTag[n.Tag] = n
	}
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		report.say("读取本机入站失败：%v", err)
		report.Rules.Failed += len(items)
		return false
	}

	changed := false
	for _, item := range items {
		label := item.Remark
		if label == "" {
			label = "(无备注)"
		}
		g, ok := groupByRemark[item.DomainGroupRef]
		if !ok {
			report.Rules.Failed++
			report.say("规则「%s」引用的域名组「%s」不存在，整条跳过", label, item.DomainGroupRef)
			continue
		}
		outboundId := 0
		if item.Action == model.ActionProxy {
			n, ok := nodeByTag[item.OutboundRef]
			if !ok {
				report.Rules.Failed++
				report.say("规则「%s」引用的出站节点 %s 不存在，整条跳过", label, item.OutboundRef)
				continue
			}
			outboundId = n.Id
		}

		ids, missing := resolveInboundRefs(item.InboundRefs, inbounds)
		enable := item.Enable
		if len(missing) > 0 {
			// 绝不把认不出的入站剔掉后当作完整覆盖集——剔到空就是
			// 「对所有入站生效」。导入成禁用状态，把缺失的点名报告，
			// 管理员打开编辑弹窗勾一下就好。整条丢弃也不对：规则的其余
			// 部分（域名组、出站、优先级、动作）都是好的。
			enable = false
			report.say("规则「%s」的入站 %s 在本机未找到，已导入但保持禁用，请手工指定入站后启用",
				label, strings.Join(missing, "、"))
		}
		encoded, err := EncodeInboundIds(ids)
		if err != nil {
			report.Rules.Failed++
			report.say("规则「%s」的入站编码失败：%v", label, err)
			continue
		}
		// 部分命中却编码成了 []，等于把规则放大到全体。这种情况只可能在
		// 已命中集合为空时出现，此时必须整条丢弃而不是导入一条覆盖全员的
		// 规则——哪怕它是禁用的，管理员一旦启用就会全员中招。
		if encoded == "[]" && len(item.InboundRefs) > 0 {
			report.Rules.Failed++
			report.say("规则「%s」的入站在本机一个都没找到，整条跳过（若照常导入，它会变成对所有用户生效）", label)
			continue
		}

		rule := &model.RoutingRule{
			Remark: item.Remark, InboundIds: encoded, DomainGroupId: g.Id,
			Action: item.Action, OutboundId: outboundId,
			Priority: item.Priority, Enable: enable,
		}
		// 走 Add 而不是直接写库：它自带 validate（域名组/出站存在、动作合法）
		// 与 checkConflict（同一域名组下入站不得重叠）。
		if err := s.ruleService.Add(rule); err != nil {
			report.Rules.Failed++
			report.say("规则「%s」导入失败：%v", label, err)
			continue
		}
		report.Rules.Created++
		changed = true
	}
	return changed
}
```

在 import 里补 `"encoding/json"` 与 `"a-ui/database"`。

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestImport|TestExport|TestResolveInboundRefs' -v
go vet ./web/service/
```

注意：涉及 `ValidateOutbound` 的用例在没有 `bin/xray-<GOOS>-<GOARCH>` 的机器上会因 fail open 而放行，测试仍应通过（断言的是「保留 tag / 空 tag / null config 被拒」，这三条都在 xray 校验**之前**判定）。

- [ ] **Step 5: 提交**

```bash
git add web/service/routing_portable.go web/service/routing_portable_test.go
git commit -m "feat(routing): 分流配置导入，冲突跳过、入站认不全则禁用

不用事务：出站落库前要 exec 真实 xray 校验，包进事务会长时间持有 SQLite
写锁把整个面板卡住。逐条独立成败 + 幂等（冲突跳过），重跑即补齐。

入站认不全时导入为禁用且保留已命中的 id，绝不清空——[] 在 xray 里等于
「对所有入站生效」且返回 Configuration OK。已命中集合为空时整条丢弃，
哪怕禁用也不行：管理员一旦启用就会全员中招。

保留 tag（a-ui-block / a-ui-default）在 xray 校验之前就拒绝：它们不在
outbound_nodes 表里，数据库唯一约束看不见，撞名会让 xray 拒绝启动整份
配置——全员断网而面板首页仍显示 running。"
```

---

### Task 4: 两个 HTTP 接口

**Files:**
- Modify: `web/controller/routing.go`

**Interfaces:**
- Consumes: Task 1 的 `Export`、Task 3 的 `Import`
- Produces:
  - `POST /aui/routing/export`（form: `scope`）→ `ExportFile`
  - `POST /aui/routing/import`（form: `data`）→ `ImportReport`

- [ ] **Step 1: 给 RoutingController 加字段**

在 `web/controller/routing.go` 的 `RoutingController` 结构体里追加：

```go
	portableService service.RoutingPortableService
```

- [ ] **Step 2: 注册路由**

在 `initRouter` 的 `rl` 分组之后追加：

```go
	g.POST("/export", a.exportRouting)
	g.POST("/import", a.importRouting)
```

- [ ] **Step 3: 写 handler**

在文件末尾追加：

```go
// exportRouting 返回导出结构，由前端自己 stringify 成 Blob 下载。
//
// 不走 Content-Disposition：现有前端全部是 axios POST + session cookie，
// 改成 GET 下载要另开一条不带 X-Requested-With 的鉴权路径，得不偿失。
func (a *RoutingController) exportRouting(c *gin.Context) {
	scope := c.PostForm("scope")
	if scope == "" {
		scope = service.ExportScopeAll
	}
	f, err := a.portableService.Export(scope)
	if err != nil {
		jsonMsg(c, "导出分流配置", err)
		return
	}
	jsonObj(c, f, nil)
}

func (a *RoutingController) importRouting(c *gin.Context) {
	data := c.PostForm("data")
	if strings.TrimSpace(data) == "" {
		jsonMsg(c, "导入分流配置", common.NewError("没有收到导入内容"))
		return
	}
	report, err := a.portableService.Import(data)
	if err != nil {
		jsonMsg(c, "导入分流配置", err)
		return
	}
	jsonObj(c, report, nil)
}
```

在 `web/controller/routing.go` 顶部 import 里补 `"a-ui/util/common"`（`strings` 已经 import 了）。

- [ ] **Step 4: 编译与手工验证**

```bash
go build ./... && go vet ./...
XUI_DEBUG=true go run main.go
```

浏览器登录后在控制台：

```js
// 导出
const r = await HttpUtil.post('/aui/routing/export', { scope: 'all' });
console.log(r.obj);   // 应含 kind/version/domainGroups/outbounds/rules，且没有任何 id

// 空内容应被拒
console.log(await HttpUtil.post('/aui/routing/import', { data: '' }));
```

- [ ] **Step 5: 提交**

```bash
git add web/controller/routing.go
git commit -m "feat(routing): 导出/导入两个接口

导出返回结构由前端 stringify 成 Blob 下载，不走 Content-Disposition：
现有前端全是 axios POST + session cookie，改 GET 下载要另开一条不带
X-Requested-With 的鉴权路径，得不偿失。"
```

---

### Task 5: 前端按钮、下载、上传与报告

**Files:**
- Modify: `web/html/xui/routing.html`

**Interfaces:**
- Consumes: Task 4 的两个接口

- [ ] **Step 1: 顶部统计卡片加整包按钮**

在 `routing.html` 那张 `<a-card hoverable style="margin-bottom: 20px;">` 的 `<a-row>` 之后、`</a-card>` 之前插入：

```html
<a-row style="margin-top:12px">
    <a-col :span="24" style="text-align:right">
        <a-button icon="download" @click="confirmExport('all')">导出全部</a-button>
        <a-button icon="upload" style="margin-left:8px" @click="pickImportFile()">导入全部</a-button>
    </a-col>
</a-row>
```

- [ ] **Step 2: 三个 tab 各加一对按钮**

在每个 `<div slot="title">` 里，「添加…」按钮之后追加（三处，scope 分别是 `domainGroups` / `outbounds` / `rules`）：

域名组 tab：

```html
<a-button icon="download" style="margin-left:8px"
          @click="confirmExport('domainGroups')">导出</a-button>
<a-button icon="upload" style="margin-left:8px"
          @click="pickImportFile()">导入</a-button>
```

出站节点 tab：把 `'domainGroups'` 换成 `'outbounds'`。
分流规则 tab：把 `'domainGroups'` 换成 `'rules'`。

- [ ] **Step 3: 加隐藏的文件选择器与报告 modal**

在 `<a-layout id="app">` 内部、`</a-layout>` 之前插入：

```html
<input type="file" ref="importFile" accept=".json,application/json"
       style="display:none" @change="onImportFileChosen">

<a-modal v-model="importReportVisible" title="导入结果" :footer="null" :width="720">
    <a-row style="margin-bottom:12px">
        <a-col :span="8">
            域名组：<a-tag color="green">新增 [[ importReport.domainGroups.created ]]</a-tag>
            <a-tag>跳过 [[ importReport.domainGroups.skipped ]]</a-tag>
            <a-tag v-if="importReport.domainGroups.failed" color="red">
                失败 [[ importReport.domainGroups.failed ]]
            </a-tag>
        </a-col>
        <a-col :span="8">
            出站节点：<a-tag color="green">新增 [[ importReport.outbounds.created ]]</a-tag>
            <a-tag>跳过 [[ importReport.outbounds.skipped ]]</a-tag>
            <a-tag v-if="importReport.outbounds.failed" color="red">
                失败 [[ importReport.outbounds.failed ]]
            </a-tag>
        </a-col>
        <a-col :span="8">
            分流规则：<a-tag color="green">新增 [[ importReport.rules.created ]]</a-tag>
            <a-tag>跳过 [[ importReport.rules.skipped ]]</a-tag>
            <a-tag v-if="importReport.rules.failed" color="red">
                失败 [[ importReport.rules.failed ]]
            </a-tag>
        </a-col>
    </a-row>
    <div v-if="importReport.messages && importReport.messages.length" class="import-messages">
        <div v-for="(m, i) in importReport.messages" :key="i">[[ m ]]</div>
    </div>
    <div v-else style="color:#8c8c8c">全部导入成功，没有需要说明的项。</div>
</a-modal>
```

**必须在 `<a-layout id="app">` 内。** Vue 2 只编译 `el` 指向的子树——分流页的三个弹窗曾整块落在 `#app` 之后，页面渲染完全正常、数据照常加载，但所有按钮点了毫无反应，控制台不报错。

- [ ] **Step 4: 加 data 与 methods**

在 `routing.html` 的 `new Vue({...})` 的 `data` 里追加：

```js
            importReportVisible: false,
            importReport: {
                domainGroups: { created: 0, skipped: 0, failed: 0 },
                outbounds: { created: 0, skipped: 0, failed: 0 },
                rules: { created: 0, skipped: 0, failed: 0 },
                messages: [],
            },
```

在 `methods` 里追加：

```js
            // 导出前必须二次确认。出站节点的 config 里就是完整可用的凭据，
            // 一个随手扔进群里的 JSON 等于把落地服务器送出去。
            confirmExport(scope) {
                const scopeLabel = {
                    all: '全部分流配置', domainGroups: '域名组',
                    outbounds: '出站节点', rules: '分流规则',
                }[scope] || scope;
                const extra = scope === 'rules'
                    ? '规则依赖域名组和出站节点，跨机器搬迁建议用「导出全部」。'
                    : '';
                this.$confirm({
                    title: '导出' + scopeLabel,
                    content: h => h('div', [
                        h('p', '导出文件包含出站节点的 UUID / 密码等凭据，任何拿到该文件的人都可以直接使用这些节点。'),
                        h('p', '请妥善保管，不要发到公开渠道。'),
                        extra ? h('p', { style: 'color:#8c8c8c' }, extra) : null,
                    ]),
                    okText: '导出',
                    cancelText: '取消',
                    onOk: () => this.doExport(scope),
                });
            },
            async doExport(scope) {
                const msg = await HttpUtil.post('/aui/routing/export', { scope });
                if (!msg.success || !msg.obj) return;
                const text = JSON.stringify(msg.obj, null, 2);
                const blob = new Blob([text], { type: 'application/json' });
                const url = URL.createObjectURL(blob);
                const a = document.createElement('a');
                a.href = url;
                a.download = 'a-ui-routing-' + scope + '-' + moment().format('YYYYMMDD-HHmm') + '.json';
                document.body.appendChild(a);
                a.click();
                document.body.removeChild(a);
                // 不 revoke 的话这个 Blob 会一直挂在内存里，反复导出会累积
                URL.revokeObjectURL(url);
            },
            pickImportFile() {
                // 清空 value，否则连续选同一个文件不会触发 change
                this.$refs.importFile.value = '';
                this.$refs.importFile.click();
            },
            async onImportFileChosen(e) {
                const file = e.target.files && e.target.files[0];
                if (!file) return;
                const text = await file.text();
                // 前端先验一道，格式明显不对就不发请求，报错更快也更具体
                let parsed;
                try {
                    parsed = JSON.parse(text);
                } catch (err) {
                    this.$message.error('这不是一个合法的 JSON 文件');
                    return;
                }
                if (!parsed || parsed.kind !== 'a-ui-routing-export') {
                    this.$message.error('这不是 AetherUI 导出的分流配置文件');
                    return;
                }
                this.loading(true);
                const msg = await HttpUtil.post('/aui/routing/import', { data: text });
                this.loading(false);
                if (!msg.success || !msg.obj) return;
                this.importReport = msg.obj;
                this.importReportVisible = true;
                await this.loadAll();
            },
```

⚠️ `this.loadAll()` 是假设的方法名——**先确认页面刷新数据的实际方法名**：

```bash
grep -n "async load\|methods: {" -A 5 web/html/xui/routing.html | head -30
```

把 `this.loadAll()` 换成实际那个（可能是 `this.loadData()` / `this.refresh()` 等）。

⚠️ `file.text()` 需要较新的浏览器。若要兼容旧环境，换成：

```js
const text = await new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result);
    reader.onerror = reject;
    reader.readAsText(file);
});
```

- [ ] **Step 5: 加报告样式**

在 `routing.html` 顶部的 `<style>` 块里追加：

```css
    .import-messages {
        max-height: 360px;
        overflow: auto;
        background: #fafafa;
        border: 1px solid #f0f0f0;
        border-radius: 4px;
        padding: 12px;
        font-size: 13px;
        line-height: 1.8;
    }
```

- [ ] **Step 6: 跑模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

- [ ] **Step 7: 端到端目视验证**

```bash
XUI_DEBUG=true go run main.go
```

在分流管理页面：

1. 建一个域名组、一个出站节点（用 socks 分享链接或 JSON）、一条规则
2. 点「导出全部」→ 确认框出现且提到凭据 → 确认 → 浏览器下载 `a-ui-routing-all-*.json`
3. 打开下载的文件，确认**没有任何 `"id"` 字段**，规则里是 `domainGroupRef` / `outboundRef` / `inboundRefs`
4. 点「导入全部」→ 选同一个文件 → 报告弹窗显示三项全部「跳过」，列表数量不变（幂等）
5. 手工删掉库里的域名组和节点（或换一个干净的库），再导入 → 显示「新增」
6. 手工把文件里某条规则的 `inboundRefs` 改成一个本机不存在的入站，导入 → 该规则出现在列表里且**开关是关的**，报告里点名了缺失的入站
7. 浏览器控制台无报错

- [ ] **Step 8: 提交**

```bash
git add web/html/xui/routing.html
git commit -m "feat(routing): 分流页的导出/导入按钮与结果报告

导出前二次确认并明确警示凭据泄露：出站节点的 config 里就是完整可用的
UUID/密码，一个随手扔进群里的 JSON 等于把落地服务器送出去。

导入结果用 modal 而不是 \$message——逐条报告可能有几十行。

新增的 input 与 modal 都在 <a-layout id=app> 内：Vue 2 只编译 el 指向的
子树，写在外面页面渲染正常、点击毫无反应、控制台不报错。"
```

---

### Task 6: 文档与全量门禁

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: 在 CLAUDE.md 的「域名分流管理」小节末尾追加**

在「### `util/link` 包」之前插入：

```markdown
### 配置导出 / 导入

域名组、出站节点、分流规则可分项或整包导出成 JSON 文件下载到本地，再上传导入到另一台机器。设计文档在 `docs/superpowers/specs/2026-09-05-routing-import-export-design.md`。

**导出文件不含任何 id**，跨表引用改写成业务键：出站用 `tag`（unique 且一经分配不可变）、域名组用 `remark`、入站用 `{remark, port}` 二元组（入站没有稳定业务键——`Tag` 由端口算出，`Remark` 可重复，`Id` 跨机器无意义且 SQLite 会复用）。

**域名组重名直接拒绝导出。** `Remark` 没有 unique 约束，重名会让导入端无法确定 `domainGroupRef` 指向哪一个；任何"猜"的策略猜错时都会产生一条指向错误域名组的规则，而规则表会渲染得完全正常、配置也会正常生成，只是流量走错节点——没有任何一层防线会发现。检查不看 scope：只导域名组时同样检查，否则分项导出会留下一个可绕开的洞。

**导入时入站认不全 → 规则导入为禁用，绝不清空 `InboundIds`。** 这是本功能最容易犯、后果最严重的错误：`[]` 表示「对所有入站生效」，一条本该只覆盖某个人的规则会被静默放大到全体，而 xray 对空 `inboundTag` 返回 `Configuration OK`。也不整条丢弃——规则的其余部分都是好的，导入成禁用状态并把缺失的入站点名，管理员勾一下就行。**唯一的例外**是已命中集合为空（一个入站都没认出来）：此时必须整条丢弃，哪怕导入成禁用的也不行——管理员一旦启用就会全员中招。

**入站两级匹配**：先按 `remark` 精确匹配（恰好命中 1 条才算，重名视为无法区分），失败再按 `port`（有 unique 约束，命中即唯一）。备注优先是因为换机器后端口很可能改了。

**冲突一律跳过，绝不覆盖。** 域名组按 `remark`、出站节点按 `tag` 判重。覆盖会静默改掉目标机器上正在跑的节点配置（两台机器的同名节点很可能指向不同落地服务器，这正是多机部署的常态），而界面上什么都不会变；跳过则让导入天然幂等，同一个文件导两次不会变成双份。代价是想更新已有节点只能手工编辑——批量覆盖生产节点配置本就该是比"上传个文件"更重的操作。

**导入的出站节点保留原 tag，但落库前必须 `model.IsReservedTag()` 拦一道。** 保留 tag 不在 `outbound_nodes` 表里，唯一约束看不见它们，撞名会让 xray 报 `existing tag found` 拒绝启动整份配置——全员断网而面板首页仍显示 `running`。这一道要排在 `ValidateOutbound` **之前**。同样要拦空 tag：空 tag 不会被 `ValidateOutbound` 拒绝，但会让 `xray/hot_diff.go` 的 `decodeOutbounds` 对整份配置判「必须重启」，出站热更新从此静默失效。

**不导出 `SubscribedDomains`**（单个组可达十几万条，生产实例实测 `+111226`）**与 `LastUpdatedAt`/`LastError`/`LastSkipped`**（是本机这一次拉取的状态，搬过去会显示一个假的「刚刚更新」）。导入的订阅组 `LastUpdatedAt = 0`，而 `ShouldUpdateNow` 对 0 直接返回 true，`SubscriptionJob`（每 10 分钟）会自动补上首次拉取——**导入路径本身不同步拉取**，一个慢地址能把 HTTP 请求挂满 30 秒。代价是首次拉取成功前，仅依赖订阅内容的规则会被 `buildRule` 跳过，报告里要明说。

**不用事务。** 出站落库前要 exec 真实 xray 校验，包进事务会长时间持有 SQLite 写锁把整个面板卡住。逐条独立成败 + 逐条报告 + 幂等，重跑即补齐。

**分项导出不隐式扩大范围**：`scope=rules` 就只导规则，不带上它引用的域名组和出站节点——隐式扩大会让 `all` 和 `rules` 的区别消失。
```

- [ ] **Step 2: 跑全量门禁**

```bash
make verify
```

- [ ] **Step 3: 清理**

```bash
make clean
git status --short
```

预期：只有本次改动的文件。

- [ ] **Step 4: 提交**

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md 增加分流配置导出/导入小节

记下三条最容易被后人改坏的约束：导出零 id 全业务键、域名组重名拒绝导出、
入站认不全导入为禁用而非清空 InboundIds。"
```

- [ ] **Step 5: 跨机器端到端验证**

在两台装了本分支的机器上（或同一台机器用两个数据库）：

1. A 机建 2 个域名组（含一个带订阅地址的）、2 个出站节点、3 条规则（一条全局、一条绑定单个入站、一条绑定多个入站）
2. A 机「导出全部」，把文件传到 B 机
3. B 机导入，核对报告：全局规则应正常启用；绑定入站的规则若 B 机没有对应入站，应显示为禁用并点名
4. B 机上核对生成的配置：

```bash
sudo grep -A 40 '"routing"' /usr/local/a-ui/bin/config.json
```

验收点：`outbounds` 里的 tag 与 A 机一致；`rules` 的 `domain` 条件与 A 机一致；启用规则的 `inboundTag` 指向 B 机对应入站的 tag；**没有任何一条规则的 `inboundTag` 是空数组**。

⚠️ 若面板日志里出现「热应用」字样，说明改动是通过 gRPC 控制面下发的，`bin/config.json` **不会被重写**，此时它仍是上一次整进程重启时写的那份。要拿到准确的文件，先 `a-ui restart`。

---

## Self-Review

**Spec 覆盖核对：**

| Spec 章节 | 落在哪个 Task |
|---|---|
| §3 导出格式（零 id、三个业务键） | Task 1 类型定义与 `toPortableRule` |
| §3.2 域名组重名拒绝（且不看 scope） | Task 1 `checkDuplicateGroupRemarks` + 两条测试 |
| §3.3 入站二元组 | Task 1 `PortableInboundRef`、Task 2 `resolveInboundRefs` |
| §4.1 导出接口与文件名 | Task 4 handler、Task 5 `doExport` |
| §4.2 scope 不隐式扩大 | Task 1 `scopeIncludes` + `TestExportScopeDoesNotWiden` |
| §4.3 不导出的字段 | Task 1 `PortableDomainGroup` + `TestExportOmitsSubscribedDomains` |
| §5.1 不用事务 | Task 3 `Import` 注释与结构 |
| §5.2 跳过而非覆盖 | Task 3 三个 import 函数 + `TestImportIsIdempotent` |
| §5.3 入站两级匹配、认不全禁用 | Task 2 + Task 3 `importRules` + `TestImportDisablesRuleWhenInboundMissing` |
| §5.4 保留原 tag、拦保留 tag/空 tag、过 ValidateOutbound | Task 3 `importOutbounds` + 三条测试 |
| §5.5 域名组走既有校验、不同步拉订阅 | Task 3 `importDomainGroups` + `TestImportSubscribedGroupStartsUnfetched` |
| §5.6 顺序与 SetToNeedRestart | Task 3 `Import` |
| §5.7 报告结构与人话 | Task 3 `ImportReport.say`、Task 5 modal |
| §6.1 按钮落点 | Task 5 Step 1/2 |
| §6.2 导出下载 + 凭据警示 | Task 5 `confirmExport` / `doExport` |
| §6.3 导入上传 + 前端预检 | Task 5 `onImportFileChosen` |
| §6.4 指令在 #app 内 | Task 5 Step 3 说明 + Step 6 模板测试 |
| §8 五个决策 | 分散在注释 + Task 6 文档 |
| §9 测试表 | Task 1/2/3 的测试逐条对应 |

Spec §9 测试表中的每一行都有对应用例：无 id ✓、无 subscribedDomains ✓、重名拒绝 ✓、入站认不全 ✓、全部命中 ✓、空数组全局 ✓、remark 重名退端口 ✓、保留 tag ✓、幂等 ✓、域名组不存在 ✓、kind/version ✓、`config` 为 `"null"` ✓。

**类型一致性核对：** `PortableDomainGroup` / `PortableOutbound` / `PortableRule` / `PortableInboundRef` / `ExportFile` 在 Task 1 定义，Task 3、5 一致引用。JSON 字段名（`domainGroupRef` / `outboundRef` / `inboundRefs` / `subscribeUrl`）在 Task 1 定义，Task 3 反序列化、Task 5 前端预检（`kind`）一致。`ImportCounts` 的 `created`/`skipped`/`failed` 在 Task 3 定义，Task 5 模板一致引用。`resolveInboundRefs` 在 Task 2 定义、Task 3 使用，签名一致。

**已知的执行期风险：** Task 5 Step 4 依赖 `routing.html` 里刷新数据的实际方法名与 `file.text()` 的浏览器支持，两处都在同一步内给了 grep 命令与替代写法。Task 3 的 `TestImportRejectsReservedTag` 依赖 `ValidateOutbound` 在缺少 xray 二进制时 fail open 放行——它断言的三条（保留 tag / 空 tag / null config）都在 xray 校验**之前**判定，因此在没有 `bin/xray-darwin-arm64` 的开发机上同样成立。
