# 分流规则后台持续审计 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 配置一生效就在后台验证所有启用规则、持续扫描访问日志抓走错的真实连接，把结果放到规则列表、侧栏红点和流量走向图（含"实际去向"）上。

**Architecture:** `RestartXray` 两处成功路径递增一个应用代次；30s 的审计任务在代次变化（或 10 分钟兜底）时调用新的 `VerifyAll`，结果放进内存缓存（每规则一份、新代次覆盖旧代次）；1 分钟的扫描任务用 `(time,id)` 游标增量读访问日志，按 `buildLogEvidence` 同一语义在内存里维护每规则 24h 分桶异常摘要。新接口把两者合成规则级状态给前端。

**Tech Stack:** Go（gin、gorm/sqlite、robfig/cron、go.uber.org/atomic）、xray-core 锁定版本；Vue 2 + ant-design-vue 1.7.2 in-DOM 模板，无构建链。

**Spec:** `docs/superpowers/specs/2026-11-01-routing-flow-audit-design.md`（前置：`docs/superpowers/specs/2026-11-01-routing-flow-verification-design.md`）

## Global Constraints

- 不新增数据库表、不改 xray 配置生成；验证与扫描全程只读（`PublishResult=false`、不重启、不写库）。
- 不得在 `RestartXray` 持有包级 `lock` 期间调验证；`lock` 只在 `RuntimeSnapshot` 里 `TryLock` 一瞬间。
- 审计任务 `@every 30s`；兜底周期 10 分钟；扫描任务 `@every 1m`；每轮最多读 20000 行；异常窗口 24 小时、按小时分桶；`VerifyAll` 整轮预算 60s，单次 RPC 3s。
- 核心未运行 / busy / 连不上控制面 → 本轮不落结果（不得让一次重启在列表上闪红/灰）。
- 用户手动验证占着 `verifyMu` 时后台本轮跳过；手动验证结果写回同一缓存，代次新的覆盖旧的，同代次 `checkedAt` 新的覆盖旧的。
- 异常判定语义 = 现有 `buildLogEvidence`：行入站在规则覆盖范围内、目标被规则域名组匹配器命中、`routeHead(Route)` 不符合期望出站；只看 `Accepted=true`。不按规则顺序重算。
- 访问日志游标条件：`time > lastTime OR (time = lastTime AND id > lastId)`，按 `time, id` 升序。
- 规则级 verdict = 用户里最坏一档（mismatch > pending > unverifiable > verified），再并入异常（有异常即 mismatch）。
- 前端：规则列表与侧栏每 60s 拉一次 `/rule/audit`，`document.hidden` 时暂停；实际去向最多 3 个，超出合并为"另有 N 个去向"；pending 时不画实际去向；图标沿用流量走向图的 ✓ / × / ! / 时钟（`rv-vicon`）。
- 新增 job 的 `Run` 首行 `defer common.Recover("<任务名>")`。
- antd 组件显式闭合；Vue 指令在根内；时间走 `DateUtil`；请求走 `HttpUtil.post`；改 `web/assets/js` 的文件引用必须带 `?{{ .cur_ver }}`。
- 未经用户授权不 push、不合并；本分支 `feat/routing-flow-verification` 上的本地提交已获授权。

## Review Focus

1. **xray 刚被崩溃自愈拉起、控制面还没就绪**：期望审计本轮不落结果、旧结果保留，下一轮正常；不能把所有规则刷成灰/红。→ Task 2 `TestVerifyAllCoreNotReady`、Task 4 `TestRouteAuditJobSkipsWhenNotReady`
2. **访问日志被清理到 0 行后新行 id 从 1 重新开始**：期望扫描不漏行。→ Task 3 `TestAnomalyScanCursorSurvivesIdReuse`
3. **管理员点"忽略此前的异常"后**：列表变绿的同时，打开该规则的流量走向图也不能再因为旧连接显示红色（两处口径一致）。→ Task 3 `TestResetAnomaliesAlsoHidesFromVerify`
4. **规则被删除或停用**：期望缓存与异常摘要里它被移除，`/rule/audit` 不再返回它；SQLite id 复用后新规则不继承旧结果。→ Task 2 `TestAuditCacheDropsDeletedRules`、Task 3 `TestAnomalyDropsDeletedRules`
5. **手动验证与后台审计交错**：后台审计结果（更新代次）不能被一次更早发起的手动验证覆盖回去。→ Task 2 `TestStoreVerifyResultNewerWins`

---

### Task 1: 应用代次信号

**Files:**
- Modify: `web/service/xray.go`（包级变量区；`RestartXray`；`XrayRuntime` 与 `RuntimeSnapshot`）
- Test: `web/service/xray_runtime_test.go`（追加）

**Interfaces:**
- Produces:
  - 包级 `var xrayApplyGen atomic.Uint64`（`go.uber.org/atomic`，已在该文件导入）
  - `func XrayApplyGeneration() uint64`
  - `XrayRuntime` 新增字段 `ApplyGen uint64`：在 `RuntimeSnapshot` 持锁那一瞬间读取

- [ ] **Step 1: 写失败测试** `TestApplyGenerationBumpsOnHotApplyAndStart`：用真实 xray（同 `TestRuntimeSnapshotAgainstRealXray` 的前置与 Cleanup），`g0 := XrayApplyGeneration()`；`RestartXray(true)` 后 `> g0`；启用一条规则后 `RestartXray(false)`（走热应用）后再 `+1`；再调一次 `RestartXray(false)`（配置未变）代次**不变**；`RuntimeSnapshot().ApplyGen == XrayApplyGeneration()`。
- [ ] **Step 2: 运行** `go test ./web/service/ -run TestApplyGenerationBumpsOnHotApplyAndStart -v`。期望结果：编译失败（未定义）。
- [ ] **Step 3: 实现。** `tryHotApply` 返回 true 的两个出口（diff 为空仅同步快照那条**不**递增——核心规则没变）与 `p.Start()` 返回 nil 之后各 `xrayApplyGen.Inc()`。注释写明为什么只在这两处。
- [ ] **Step 4: 运行同上**。期望结果：PASS（RUN，不是 SKIP；若端口 62789 被占先停本地面板）。
- [ ] **Step 5: Commit** `feat(service): xray apply generation signal`

### Task 2: `VerifyAll` 与结果缓存

**Files:**
- Modify: `web/service/routing_verify.go`（把 `Verify` 拆成共享准备 + 单规则判定）
- Create: `web/service/routing_audit.go`（缓存、规则级汇总）
- Test: `web/service/routing_audit_test.go`

**Interfaces:**
- Consumes: Task 1 `XrayRuntime.ApplyGen`、`XrayApplyGeneration()`
- Produces:
  - `RuleVerifyResult` 新增 `ApplyGen uint64 \`json:"applyGen"\``
  - `var ErrVerifyBusy = errors.New("已有验证在进行")`、`var ErrCoreNotReady = errors.New("xray 核心未就绪")`
  - `func (s *RoutingVerifyService) VerifyAll() (map[int]*RuleVerifyResult, uint64, error)`：只验证启用规则；返回结果与本轮使用的代次。`verifyMu.TryLock` 失败 → `ErrVerifyBusy`；`rt.Busy` → `ErrVerifyBusy`；`!rt.Running` 或控制面 `Init` 失败 → `ErrCoreNotReady`。整轮 60s 预算，单 RPC 3s，共享一个 API 连接；`GetXrayConfig`、`buildOutbounds`、每个域名组的样例与匹配器在一轮内各算一次。
  - 内部：`type verifyShared struct{...}`、`func (s *RoutingVerifyService) prepareShared(rt XrayRuntime) (*verifyShared, error)`、`func (s *RoutingVerifyService) verifyOne(ctx context.Context, sh *verifyShared, rule *model.RoutingRule, now time.Time) *RuleVerifyResult`；`Verify(id)` 改为走这两段。
  - `func storeVerifyResult(res *RuleVerifyResult)`：按 `ApplyGen` 新者胜，同代次按 `CheckedAt` 新者胜。
  - `func replaceAuditResults(results map[int]*RuleVerifyResult, enabledIds []int)`：写入并删除不在 `enabledIds` 里的规则。
  - `func cachedVerifyResult(ruleId int) (*RuleVerifyResult, bool)`
  - `type RuleAuditSummary struct { Verdict string \`json:"verdict"\`; CheckedAt int64 \`json:"checkedAt"\`; ApplyGen uint64 \`json:"applyGen"\`; MismatchUsers int \`json:"mismatchUsers"\`; AnomalyCount int64 \`json:"anomalyCount"\`; AnomalyLastAt int64 \`json:"anomalyLastAt"\` }`（AnomalyCount/LastAt 由 Task 3 填，这里先为 0）
  - `func ruleVerdictOf(res *RuleVerifyResult) (verdict string, mismatchUsers int)`：用户最坏一档；无用户时取 core.state 映射（ok/pending → 对应 verdict，其他 → unverifiable）
  - `type RoutingAuditService struct{}`；`func (s *RoutingAuditService) Summaries() (applyGen uint64, rules map[int]RuleAuditSummary)`：遍历启用规则，无缓存的 verdict=`unverifiable`、CheckedAt=0
  - `Verify(id)` 成功产出结果后调用 `storeVerifyResult`
  - `func (s *RoutingAuditService) RunAudit() (uint64, error)`：调用 `VerifyAll`，成功则 `replaceAuditResults` 并返回代次；错误原样返回（`ErrVerifyBusy`/`ErrCoreNotReady` 时不动缓存）
  - `func (s *RoutingAuditService) Cached(ruleId int) (*RuleVerifyResult, bool)`（导出包装）

- [ ] **Step 1: 写失败测试（伪造 route/runtime，复用 `routing_verify_service_test.go` 的建数据辅助）。**
  - `TestVerifyAllMatchesVerify`：两条启用规则 + 一条停用规则；`VerifyAll` 返回 2 条，每条与对同一规则调 `Verify` 的 `Users[*].Verdict`、`Samples` 一致；停用规则不在结果里。
  - `TestVerifyAllSharesWork`：伪造 route 计数；两条规则引用同一组同一入站时，`GetXrayConfig` 只调用 1 次（为此在 `RoutingVerifyService` 加可注入的 `xrayConfig func() (*xray.Config, error)` 字段，nil 用真实实现）。
  - `TestVerifyAllCoreNotReady`：runtime `Running=false` → `ErrCoreNotReady`；runtime `Busy=true` → `ErrVerifyBusy`；测试内持有 `verifyMu` → `ErrVerifyBusy`。
  - `TestStoreVerifyResultNewerWins`：先存 gen=5，再存 gen=4 → 仍是 gen=5；再存 gen=5、checkedAt 更新 → 被替换。
  - `TestAuditCacheDropsDeletedRules`：`replaceAuditResults({1:..,2:..}, [1,2])` 后 `replaceAuditResults({1:..}, [1])` → `cachedVerifyResult(2)` 不存在。
  - `TestRuleVerdictOf`：表驱动覆盖最坏一档顺序与无用户时的映射。
- [ ] **Step 2: 运行** `go test ./web/service/ -run 'TestVerifyAll|TestStoreVerifyResult|TestAuditCache|TestRuleVerdictOf|TestVerify' -v`。期望结果：新测试编译失败；（拆分后）既有 `TestVerify*` 仍须全部通过。
- [ ] **Step 3: 实现上述接口。** 缓存用包级 `sync.Mutex` 保护的 `map[int]*RuleVerifyResult`。
- [ ] **Step 4: 运行同上 + `go test ./web/service/`**。期望结果：PASS。
- [ ] **Step 5: Commit** `feat(service): VerifyAll and in-memory audit cache`

### Task 3: 访问日志异常扫描

**Files:**
- Modify: `web/service/accesslog.go`（追加 `ReadAfter`）
- Create: `web/service/routing_anomaly.go`
- Modify: `web/service/routing_verify.go`（日志证据窗口起点尊重忽略时刻）
- Modify: `web/service/routing_audit.go`（`Summaries` 并入异常）
- Test: `web/service/routing_anomaly_test.go`（复用 `setupAccessLogDB`）

**Interfaces:**
- Consumes: `newGroupMatcher`、`judgeSample`、`routeHead`、`ExpectedOutbound`、Task 1 `XrayApplyGeneration()`、Task 2 `RoutingAuditService`、`RuleAuditSummary`
- Produces:
  - `func (s *AccessLogService) ReadAfter(afterTime, afterId int64, limit int) ([]model.AccessLog, error)`：条件与排序见 Global Constraints
  - `type UserAnomaly struct { Count int64 \`json:"count"\`; LastAt int64 \`json:"lastAt"\`; Routes map[string]int64 \`json:"routes"\` }`
  - `type RuleAnomaly struct { Count int64 \`json:"count"\`; LastAt int64 \`json:"lastAt"\`; ByUser map[int]*UserAnomaly \`json:"byUser"\` }`
  - `func (s *RoutingAuditService) ScanAccessLog(now time.Time) error`：访问日志关闭 → 清空摘要与游标并返回；首次（游标为零）从 `now-24h` 起；每轮最多 20000 行；按小时分桶（key = `time/3600000`）累加，丢弃早于 `now-24h` 所在小时的桶；规则计划（入站集合、匹配器、期望出站）按代次缓存，代次变化或启用规则集合变化时重建；停用/删除的规则从摘要移除。
  - `func (s *RoutingAuditService) Anomaly(ruleId int, now time.Time) RuleAnomaly`
  - `func (s *RoutingAuditService) ResetAnomalies(ruleId int, now time.Time)`：清空该规则摘要，并记录忽略时刻 `ignoreBefore[ruleId] = now.UnixMilli()`
  - `func anomalyIgnoreBefore(ruleId int) int64`；`Verify`/`verifyOne` 的日志查询起点改为 `max(now-24h, anomalyIgnoreBefore(ruleId))`
  - `Summaries` 填入 `AnomalyCount`/`AnomalyLastAt`，有异常时 verdict 置 `mismatch`

- [ ] **Step 1: 写失败测试。**
  - `TestAnomalyScanCountsWrongRoute`：规则 proxy→`a-ui-us` 覆盖入站 A，组 `domain:openai.com`；插入 A 的 `api.openai.com:443` route=`a-ui-us` ×2、route=`a-ui-default` ×3、`example.com:443` route=`a-ui-default` ×5、入站 B 的 `api.openai.com:443` route=`a-ui-default` ×1、Accepted=false ×1。扫描后 `Anomaly(rule).Count==3`，`ByUser[A].Routes["a-ui-default"]==3`，B 不计。
  - `TestAnomalyScanIncremental`：扫描两次，第二次之间新增 1 条走错 → 总数 +1（不重复计）。
  - `TestAnomalyScanCursorSurvivesIdReuse`：扫描后删空 access_logs，再插入一条（id 回到 1、time 更新）走错的行 → 被计入。
  - `TestAnomalyWindowDropsOld`：`now` 前进 25 小时后再扫描 → 旧桶不计。
  - `TestAnomalyDropsDeletedRules`：删除规则后扫描 → `Anomaly(rule).Count==0` 且 `Summaries` 不含它。
  - `TestAnomalyDisabledAccessLogClears`：关闭访问日志 → 扫描后摘要为空。
  - `TestResetAnomaliesAlsoHidesFromVerify`：有 3 次走错后 `ResetAnomalies`；`Summaries` 该规则 `AnomalyCount==0`；`Verify(rule)` 的该用户 `Log.Others` 为空（旧连接早于忽略时刻）；之后新增 1 条走错 → 两处都重新出现。
  - `TestSummariesMergeAnomaly`：核心验证 verified + 有异常 → `Verdict=="mismatch"`。
- [ ] **Step 2: 运行** `go test ./web/service/ -run 'TestAnomaly|TestResetAnomalies|TestSummaries' -v`。期望结果：FAIL（未定义）。
- [ ] **Step 3: 实现。** 摘要、游标、忽略时刻都放包级、互斥保护的内存结构；扫描不持有 `verifyMu`。
- [ ] **Step 4: 运行同上 + `go test ./web/service/`**。期望结果：PASS。
- [ ] **Step 5: Commit** `feat(service): continuous access-log anomaly scan`

### Task 4: 两个后台任务

**Files:**
- Create: `web/job/route_audit_job.go`、`web/job/route_anomaly_job.go`
- Modify: `web/web.go`（`startTask` 注册，紧挨 `AccessLogCollectJob`）
- Test: `web/job/route_audit_job_test.go`

**Interfaces:**
- Consumes: Task 1 `XrayApplyGeneration()`；Task 2 `RunAudit() (uint64, error)`、`ErrVerifyBusy`、`ErrCoreNotReady`；Task 3 `ScanAccessLog`
- Produces:
  - `func NewRouteAuditJob() *RouteAuditJob`，字段 `lastGen uint64`、`lastAt time.Time`、`now func() time.Time`（测试注入）
  - `func shouldAudit(curGen, lastGen uint64, lastAt, now time.Time) bool`：代次不同，或 `now.Sub(lastAt) >= 10*time.Minute`
  - `func NewRouteAnomalyJob() *RouteAnomalyJob`
  - 审计：`ErrVerifyBusy`/`ErrCoreNotReady` → 不更新 `lastGen/lastAt`（下一轮重试），只 `logger.Debug`；其他错误 `logger.Warning`；成功 → 更新 `lastGen` 为 `VerifyAll` 返回的代次、`lastAt=now`。

- [ ] **Step 1: 写失败测试。** `TestShouldAudit`（表驱动：同代次 9 分钟 false、同代次 10 分钟 true、代次变化 true、首次 lastAt 零值 true）；`TestRouteAuditJobSkipsWhenNotReady`：注入一个返回 `ErrCoreNotReady` 的审计函数（在 job 上加 `audit func() (uint64, error)` 字段，nil 用真实实现），运行后 `lastGen`/`lastAt` 不变；返回成功时更新。
- [ ] **Step 2: 运行** `go test ./web/job/ -run 'TestShouldAudit|TestRouteAuditJob' -v`。期望结果：FAIL。
- [ ] **Step 3: 实现两个 job 并在 `web.go` 注册**：`s.cron.AddJob("@every 30s", job.NewRouteAuditJob())`、`s.cron.AddJob("@every 1m", job.NewRouteAnomalyJob())`；两个 `Run` 首行 `defer common.Recover(...)`。
- [ ] **Step 4: 运行同上 + `go test ./web/...`**。期望结果：PASS。
- [ ] **Step 5: Commit** `feat(job): route audit and anomaly scan jobs`

### Task 5: 接口

**Files:**
- Modify: `web/controller/routing.go`（`rl` 分组）
- Test: `web/controller/routing_verify_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 `Summaries`、`Cached`；Task 3 `ResetAnomalies`
- Produces:
  - `POST /aui/routing/rule/audit` → `jsonObj` `{ "applyGen": n, "accessLogEnabled": bool, "rules": { "<ruleId>": RuleAuditSummary } }`
  - `POST /aui/routing/rule/verify/:id?cached=1`：缓存存在且 `ApplyGen == XrayApplyGeneration()` → 直接返回缓存；否则现场 `Verify`
  - `POST /aui/routing/rule/anomaly/reset/:id` → `jsonMsg(c, "忽略此前的异常", err)`；id 非法同样走 `jsonMsg`

- [ ] **Step 1: 写失败测试**（沿用该文件现有的 gin 测试脚手架）：`TestRuleAuditEndpoint` 返回 success 且含 `rules`；`TestAnomalyResetRejectsBadId`；`TestVerifyCachedReturnsCache`（预先 `storeVerifyResult` 一份 gen=当前代次的结果 → 接口原样返回，`CheckedAt` 相同）。
- [ ] **Step 2: 运行** `go test ./web/controller/ -run 'TestRuleAudit|TestAnomalyReset|TestVerifyCached' -v`。期望结果：FAIL（404）。
- [ ] **Step 3: 实现。**
- [ ] **Step 4: 运行同上 + `go test ./web/...`**。期望结果：PASS。
- [ ] **Step 5: Commit** `feat(controller): routing audit endpoints`

### Task 6: 前端：规则列表状态、侧栏红点、缓存秒开、忽略按钮

**Files:**
- Create: `web/assets/js/util/routing-audit.js`
- Modify: `web/html/common/js.html`（在 `panel-version.js` 之后引入，带 `?{{ .cur_ver }}`）
- Modify: `web/html/xui/common_sider.html`（"分流管理"菜单项红点）
- Modify: `web/html/xui/index.html`、`inbounds.html`、`routing.html`、`setting.html`（`mixins` 数组追加 `routingAuditMixin`）
- Modify: `web/html/xui/routing.html`（规则列表行图标；选中时 `cached=1` 秒开；忽略按钮）
- Test: 既有 `web/html_test.go`

**Interfaces:**
- Consumes: Task 5 三个接口
- Produces:
  - `routingAuditMixin`：data `routingAudit: { applyGen: 0, rules: {}, mismatchCount: 0, loadedAt: 0 }`；method `refreshRoutingAudit()`；`mounted` 立即拉一次并每 60s 拉一次，`document.hidden` 时跳过；`beforeDestroy` 清定时器。`mismatchCount` = verdict 为 `mismatch` 的规则数。
  - 侧栏：分流管理菜单项用 `<a-badge :dot="routingAudit.mismatchCount > 0">` 包裹（参照 `common_sider.html:109` 的写法），`title` 为 `routingAudit.mismatchCount + ' 条规则存在走错的流量'`。
  - 规则列表每行（在启用开关左侧）放与流量走向图同款的 `rv-vicon`，依 `routingAudit.rules[rule.id]`：verified ✓、mismatch ×、pending 时钟、其余 !；停用规则不显示。`title` 文案：`核心验证 HH:mm 通过` / `与配置不符` + （有异常时）`；近 24h 有 N 次走错（最近 HH:mm）` / `尚未验证`。图标是 `span role="img"` + `aria-label`，不可点（点规则行即进入详情）。
  - 选中规则时先请求 `verify/:id?cached=1`；拿到的结果按现有 epoch 机制落位；随后不再额外现场验证（缓存不是当前代次时接口已现场验证）。保存后流程不变。
  - 流量走向区域，当 `routingAudit.rules[id].anomalyCount > 0` 时显示按钮「忽略此前的异常」：`HttpUtil.post('aui/routing/rule/anomaly/reset/' + id)` 成功后 `refreshRoutingAudit()` 并 `runVerify(id)`。
- [ ] **Step 1: 实现上述内容。**
- [ ] **Step 2: 运行** `go test ./web/ -run 'TestAllTemplatesParse|TestNoSelfClosingAntdComponents|TestVueDirectivesLiveInsideAVueRoot|TestNoStateChangingRequestBypassesAxios|TestStylesheetsAreCacheBusted'` 与 `go test ./web/...`；并用仓库 vendored 的 vue 2.6.12 对 `routing.html` 的 `#app` 模板做 `Vue.compile`、对新增 js 做 `node --check`。期望结果：全部通过，编译 0 错误。
- [ ] **Step 3: Commit** `feat(routing-ui): audit status in rule list, sidebar dot, cached open, ignore anomalies`

### Task 7: 前端：实际去向节点

**Files:**
- Modify: `web/html/xui/routing.html`（`.rv-dcol-r` 区域、`drawRuleWires`、相关 computed）
- Test: 既有 `web/html_test.go`

**Interfaces:**
- Consumes: 现有 `verify.result`（`users[].samples[].actualTag/actualLabel/match`、`users[].log.others[].route/label`）
- Produces:
  - computed `actualDests`：仅当结果已加载且 `core.state !== 'pending'`；收集 verdict 为 mismatch 的用户里 `match=false` 的样例 `actualTag` 与日志 `others[].route`，按 tag 去重，保留 `{ tag, label, userKeys: [] }`（全局规则时 userKeys 为 `['all']`），按涉及用户数降序，最多 3 个，其余计入 `moreCount`。
  - 模板：在配置去向 `.rv-dest` 下方渲染 `.rv-dest.is-actual`（红色虚线边框），标题 `实际：{label}`；`moreCount>0` 时一行 `另有 N 个去向`。窄视口下随列纵向堆叠。
  - `drawRuleWires`：对每个 actualDest，从其 `userKeys` 对应的用户行画红线（`--aui-danger`）到该节点；配置去向的连线保持现状。尊重 `prefers-reduced-motion`（不加动画点）。
- [ ] **Step 1: 实现上述内容。**
- [ ] **Step 2: 运行** 同 Task 6 Step 2 的模板测试与 `Vue.compile` 检查。期望结果：通过。
- [ ] **Step 3: Commit** `feat(routing-ui): show actual destinations for mismatched users`

### Task 8: 端到端、文档与门禁

**Files:**
- Modify: `web/service/routing_verify_e2e_test.go`（追加用例）
- Modify: `CLAUDE.md`（"流量走向验证"一节）

**Interfaces:**
- Consumes: 全部前序任务

- [ ] **Step 1: 写端到端测试** `TestRouteAuditAgainstRealXray`：真实 xray（前置同既有 e2e，必须 RUN），建 socks 入站 + `full:localhost` 组 + block 规则；`RestartXray(true)`；`(&RoutingAuditService{}).RunAudit()` 后 `Summaries()` 该规则 `Verdict=="verified"`、`ApplyGen==XrayApplyGeneration()`；把规则改成 proxy 指向 freedom 节点、`RestartXray(false)` 后再 `RunAudit()` → 仍 verified 且代次 +1。
- [ ] **Step 2: 运行** `go test ./web/service/ -run TestRouteAuditAgainstRealXray -v`。期望结果：PASS（不是 SKIP）。
- [ ] **Step 3: 更新 CLAUDE.md**：代次信号只在热应用成功与 `p.Start()` 成功两处递增；后台审计不得在 `lock` 内调用、不得与手动验证抢 `verifyMu`；结果与异常摘要只在内存（重启后约 30s/1min 内重建）；异常语义 = `buildLogEvidence`，被遮蔽域名算下层规则走错；游标用 `(time,id)` 防 id 复用；忽略异常同时作用于列表与流量走向图。
- [ ] **Step 4: 运行** `make verify`。期望结果：vet、test、build 全部通过。
- [ ] **Step 5: Commit** `test/docs: routing audit e2e and invariants`
