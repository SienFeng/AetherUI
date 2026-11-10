# 自动设置地区限制 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为没有地区限制的入站（新的和存量的）在观察期满、条件全部满足时自动设置地区限制，否则交给管理员一键确认。

**Architecture:** 判定是纯函数 `decideAutoRegion`，`AutoRegionService.Run` 负责组装输入并执行；每入站一行状态存在主库 `inbound_auto_regions`；定时任务 10 分钟一轮；管理员操作走三个接口；「需要留意」下发状态，入站列表显示标签与弹窗。

**Tech Stack:** Go（Gin、GORM/SQLite）、Vue2 + ant-design-vue 1.x。

**Spec:** `docs/superpowers/specs/2026-11-09-auto-region-design.md`

## Global Constraints

- 提交、合并、发版需要管理员单独授权：各任务只做 `git status --short` 自查。
- 新表必须在 `DelInbound` 的事务里删除（SQLite 会复用 id）。
- 新设置项按 CLAUDE.md「设置系统」改 5 处，包括 models.js；两个测试 helper（`validBaseSetting`、`validBaseSettingForEntityTest`）要同步补默认值。
- service 无状态，置重启标志只在 controller / job 里做。
- 测试 IP 用 RFC 5737 地址。
- 门禁：`make verify`。

## Review Focus

1. 管理员在编辑表单里清空了自动设置的地区，之后不能再被自动设回去（`UpdateInbound` 要把状态标成 manual）。Task 1 测试。
2. 停用的入站、已有地区的入站，不进判定。Task 1 测试（`eligibleForAutoRegion`）。
3. 总开关关闭时，任务什么都不写。Task 3（Run 开头直接返回）。
4. `pending` 在条件变好后要能转成 `applied`，不能卡在 pending。Task 1 测试。
5. 撤销要同时关闭 AutoTravel、结束活动通行证。Task 1 测试。

---

### Task 1: 状态表、判定与操作

**Files:** Create `database/model/auto_region.go`、`web/service/auto_region.go`、`web/service/auto_region_test.go`；Modify `database/db.go`、`web/service/inbound.go`（UpdateInbound 标 manual、DelInbound 级联删除）

**Interfaces（Produces）：**
- `model.InboundAutoRegion{InboundId int pk; State string; Regions string; Reason string; UpdatedAt int64}`，以及常量 `AutoRegionPending="pending"`、`AutoRegionApplied="applied"`、`AutoRegionManual="manual"`。
- `type autoRegionParams struct{ ObserveDays, StableDays, MaxBlockedPercent int }`、`defaultAutoRegionParams()`（观察 1 天 / 稳定 3 天，取两者较小值 / 误拦上限 5%）。
- `type autoRegionInput struct{ Now time.Time; Loc *time.Location; EarliestHour int64; Rows []model.InboundIPHour; RiskLevel string; Params autoRegionParams; BlockedPercent func([]string) (float64, error) }`
- `type autoRegionDecision struct{ Action string /* wait|pending|apply */; Reason string; Regions []string }`
- `func decideAutoRegion(in autoRegionInput) autoRegionDecision`
- `func eligibleForAutoRegion(in *model.Inbound, st *model.InboundAutoRegion) bool`
- `AutoRegionService`：`States() (map[int]*model.InboundAutoRegion, error)`、`Confirm(id)`、`Dismiss(id)`、`Undo(id, now)`、`MarkManual(tx, id)`、`DeleteByInbound(tx, id)`、`Run(now) (changed bool, err error)`。

- [ ] 测试：`TestAutoRegionWaitsDuringObservation`、`TestAutoRegionWaitsWithoutStableDays`、`TestAutoRegionAppliesWhenClean`、`TestAutoRegionPendingOnCoexist`、`TestAutoRegionPendingOnRisk`、`TestAutoRegionPendingOnBlockedShare`、`TestEligibleForAutoRegion`、`TestUpdateInboundRegionsMarksManual`、`TestUndoClearsRegionsAutoTravelAndPass`、`TestDelInboundDeletesAutoRegionState` → 先跑一遍确认失败 → 实现 → 通过

### Task 2: 设置项

**Files:** `web/entity/entity.go`、`web/service/setting.go`（defaultValueMap 加三项，并新增 `GetAutoRegionParams() (enabled bool, p autoRegionParams)`）、`web/assets/js/model/models.js`、两个测试 helper

- [ ] 测试：`TestCheckValidAutoRegionRanges` → 先跑一遍确认失败 → 实现 → 通过

### Task 3: 任务、接口与「需要留意」

**Files:**
- Create `web/job/auto_region_job.go`（`defer common.Recover`；`changed` 为真时置重启标志）
- Modify `web/web.go`（`@every 10m`）、`web/controller/inbound.go`（confirm / dismiss / undo 三个接口，成功后置重启标志，dismiss 除外）、`web/service/inbound_attention.go`（下发 `AutoRegion *AutoRegionAttention{State, Regions []string, Reason, At}`；applied 只在 7 天内下发）

- [ ] `go vet` + 全部测试

### Task 4: 前端与渲染验收

**Files:** `web/html/xui/inbounds.html`（limits 列的按钮与标签、#app 内的小弹窗、提醒计数）、`web/assets/js/util/inbound-attention.js`（pending 计入提醒）、`web/html/xui/setting.html`（新分组与 pane key）、`web/assets/css/custom.css`（如有需要）

- [ ] 在隔离副本里渲染验收：一个 pending 入站、一个 applied 入站，各自走一遍确认 / 撤销

### Task 5: 文档与门禁

- [ ] 在 CLAUDE.md 的地区限制相关小节补一段、补上定时任务一行；`make verify` 通过
