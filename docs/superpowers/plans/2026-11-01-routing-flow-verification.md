# 分流规则「流量走向」验证 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让「流量走向」图上每个用户都带上来自运行中 xray 核心（TestRoute）与访问日志的证据，并给出 已验证 / 与配置不符 / 等待下发 / 无法验证 四种状态之一。

**Architecture:** 后端新增只读接口 `POST /aui/routing/rule/verify/:id`。它先取核心运行时快照，再对每个「用户 × 样例目标」调 `RoutingService.TestRoute`，同时按入站聚合近 24h 访问日志，并用 xray 自己的匹配器筛出属于本规则域名组的连接，最后组合成每个用户的状态。前端在 `routing.html` 的图上渲染状态徽章、汇总、明细和连线颜色。

**Tech Stack:** Go（gin、gorm/sqlite），xray-core（锁定版本的 `app/router/command`、`common/geodata`、`app/router`），Vue 2 + ant-design-vue 1.7.2 in-DOM 模板（无构建链）。

**Spec:** `docs/superpowers/specs/2026-11-01-routing-flow-verification-design.md`

## Global Constraints

- 验证全程只读：`TestRoute` 的 `PublishResult=false`；不重启、不写配置、不写库（spec A2）。
- 没有证据就不能显示绿色（spec A1）。访问日志单独不能让用户变绿（spec §5）。
- 域名/IP 归属判定只能用 xray 的 `geodata.ParseDomainRules(…, geodata.Domain_Substr)` + `router.NewDomainMatcher`，以及 `geodata.ParseIPRules` + `router.NewIPMatcher(…, router.MatcherAsType_Target)`。禁止手写匹配（spec A3）。
- 常量：单次 RPC 超时 3s；整次请求预算 20s；每组最多 2 个样例；每用户最多 6 个样例；全局规则最多 50 个已启用入站；日志窗口 24h；日志分组上限 5000；每个「其他出站」分组最多 5 个示例。
- 请求 `TestRoute` 时：`Network=TCP`，`TargetPort=443`，`FieldSelectors=["outbound"]`。
- 状态枚举：`core.state ∈ {ok, pending, not_running, busy, error}`；`user.verdict ∈ {verified, mismatch, pending, unverifiable}`。
- 包级 `lock` 只在读取快照的瞬间用 `TryLock` 持有，不能在整批 RPC 期间持有。
- 前端：antd 组件一律显式闭合（`TestNoSelfClosingAntdComponents`）；时间显示走 `DateUtil`（面板时区）；状态必须同时有图标和文字；汇总放在唯一一个 `role="status" aria-atomic="true"` 元素里。
- 未经用户明确授权不执行 `git commit`。下文的「Commit」步骤只在授权后执行，否则跳过并在交付时列出改动。

## Review Focus

1. **规则里有已停用的入站**：`buildRule` 会剔除它，核心里本来就没有该入站的规则。期望显示「无法验证：该用户已停用」，不能显示红色的「与配置不符」。→ Task 5 `TestVerifyDisabledInboundIsUnverifiable`
2. **开了 `ipRuleResolveDomain` 且核心解析很慢或超时**：期望该样例记「超时」，用户显示「无法验证」，不能显示红色。→ Task 5 `TestVerifyTimeoutIsUnverifiable`
3. **管理员连点「重新验证」**：第二个并发请求期望立即返回 `core.state=busy`，不能排队压到核心上。→ Task 5 `TestVerifyRejectsConcurrentRun`
4. **访问日志的 Route 是中间出站链**（`"a-ui-us -> a-ui-default"`）：期望按第一段 `a-ui-us` 判定，不能把整串当成「其他出站」。→ Task 3 `TestRouteHead`
5. **`direct` 规则没有命中，但凑巧走了默认出站（`ErrNoClue` 或计量 tag）**：期望 `match=true` 且 `hitByRule=false`，明细里注明，不能报红，也不能当作完全无事。→ Task 3 `TestJudgeSampleDirect`

---

### Task 1: XrayAPI.TestRoute

**Files:**
- Modify: `xray/api.go`（在 `ApplyRoutingConfig` 之后追加）
- Test: `xray/api_test_route_test.go`（新建）

**Interfaces:**
- Produces:
  - `func (x *XrayAPI) TestRoute(ctx context.Context, inboundTag, domain string, ip net.IP) (tag string, noMatch bool, err error)`：`domain` 与 `ip` 二选一，非空/非 nil 的那个生效。
  - `func isNoClueErr(err error) bool`：用 `status.Convert(err).Message()` 是否包含 `common.ErrNoClue.Error()` 来判断。注释说明这是 gRPC 跨进程后唯一可行的识别方式。

- [ ] **Step 1: 写失败测试** `TestIsNoClueErr`：`status.Error(codes.Unknown, common.ErrNoClue.Error())` → true；`status.Error(codes.Unknown, "other")` → false；`nil` → false；`context.DeadlineExceeded` → false。另写 `TestTestRouteRequiresClient`：零值 `XrayAPI{}` 调用返回 err。
- [ ] **Step 2: 运行** `go test ./xray -run 'TestIsNoClueErr|TestTestRouteRequiresClient'`。期望结果：编译失败（函数未定义）。
- [ ] **Step 3: 实现。** 构造 `routerService.TestRouteRequest{RoutingContext:&routerService.RoutingContext{InboundTag, Network: net.Network_TCP, TargetPort:443, TargetDomain 或 TargetIPs:[][]byte{ip}}, FieldSelectors:[]string{"outbound"}, PublishResult:false}`。`isNoClueErr` 为真时返回 `("", true, nil)`。超时由调用方的 `ctx` 控制。
- [ ] **Step 4: 运行同上命令**。期望结果：PASS。
- [ ] **Step 5: Commit**（需授权）`feat(xray): add TestRoute wrapper`

### Task 2: XrayService.RuntimeSnapshot

**Files:**
- Modify: `web/service/xray.go`
- Test: `web/service/xray_runtime_test.go`（新建）

**Interfaces:**
- Produces:
  - `type XrayRuntime struct { Running, Busy, Pending bool; APIPort int; DefaultTag string }`
  - `func (s *XrayService) RuntimeSnapshot() XrayRuntime`：`lock.TryLock()` 失败时返回 `Busy=true`。成功时只读取 `p`、`p.IsRunning()`、`p.GetAPIPort()`、`p.GetConfig()`，然后立即解锁。之后在锁外调 `GetXrayConfig()`，`Pending = !running.Equals(fresh)`；`GetXrayConfig` 出错时 `Pending=false`，并把错误记进 `logger.Warning`。
  - `func defaultOutboundTag(cfg *xray.Config) string`：取 `cfg.OutboundConfigs` 数组首个元素的 `tag`。用**运行中**快照的配置，因为这是核心实际持有的那份。

- [ ] **Step 1: 写失败测试。** `TestRuntimeSnapshotNotRunning`：`p=nil` → `Running=false, Busy=false`。`TestRuntimeSnapshotBusy`：测试里先 `lock.Lock()`，用 `t.Cleanup` 解锁 → `Busy=true`。`TestDefaultOutboundTag`：`[{"protocol":"freedom","tag":"a-ui-default"},{"tag":"x"}]` → `"a-ui-default"`；空数组 → `""`。
- [ ] **Step 2: 运行** `go test ./web/service -run 'TestRuntimeSnapshot|TestDefaultOutboundTag'`。期望结果：FAIL（未定义）。
- [ ] **Step 3: 实现上述两个函数。**
- [ ] **Step 4: 运行同上命令**。期望结果：PASS。
- [ ] **Step 5: Commit**（需授权）`feat(service): xray runtime snapshot for route verification`

### Task 3: 验证的纯函数（样例、期望、判定、组合）

**Files:**
- Create: `web/service/routing_verify.go`
- Test: `web/service/routing_verify_test.go`

**Interfaces:**
- Produces（结果类型的 json tag 必须与 spec §7 一致）:
  - `type RuleVerifyResult struct { RuleId int; CheckedAt int64; Expected ExpectedOutbound; Core CoreState; AccessLog AccessLogState; Truncated bool; Users []UserVerify }`
  - `type ExpectedOutbound struct { Action, Tag, Label string }`，`type CoreState struct { State, Message string }`，`type AccessLogState struct { Enabled bool; WindowHours int }`
  - `type UserVerify struct { InboundId int; Name, Verdict, Reason string; Samples []SampleResult; Log *LogEvidence }`
  - `type SampleResult struct { Target, ActualTag, ActualLabel string; Match, HitByRule bool; Error string }`
  - `type LogEvidence struct { Matched, Rejected int64; Truncated bool; Others []LogOther }`，`type LogOther struct { Route, Label string; Count, LastAt int64; Examples []string }`
  - `type sampleTarget struct { Domain string; IP net.IP; Display string }`
  - `func sampleTargets(domains, cidrs []string, perGroup int) []sampleTarget`：对**一个**组取样，先域名后 IP，各取到 `perGroup` 为止（总数也不超过 `perGroup`）。
  - `func judgeSample(exp ExpectedOutbound, actualTag string, noMatch bool, defaultTag string) (match, hitByRule bool)`
  - `func routeHead(route string) string`：取 `" -> "` / `" >> "` 之前的第一段，并 TrimSpace。
  - `func combineVerdict(coreState string, samples []SampleResult, log *LogEvidence) (verdict, reason string)`

- [ ] **Step 1: 写失败测试（表驱动）。**
  - `TestSampleTargets`：`["full:chatgpt.com","domain:openai.com","keyword:gpt"]` + `["1.2.3.0/24","!10.0.0.0/8","geoip:us"]`，`perGroup=2` → `chatgpt.com`、`openai.com`。只给 cidrs `["1.2.3.0/24"]` → IP `1.2.3.1`。`["2001:db8::/32"]` → `2001:db8::1`。全是 `keyword:`/`regexp:`/`geosite:` → 空。
  - `TestJudgeSampleProxy`：期望 tag `a-ui-us`：实际 `a-ui-us` → (true,true)；实际 `a-ui-default` → (false,true)；noMatch → (false,false)。
  - `TestJudgeSampleBlock`：期望 `a-ui-block`，实际 `a-ui-block` → (true,true)。
  - `TestJudgeSampleDirect`（Review Focus 5）：defaultTag `a-ui-default`：实际 `a-ui-default` → (true,true)；noMatch → (true,false)；实际 `a-ui-meter-openai-com` → (true,false)；实际 `a-ui-us` → (false,true)。
  - `TestRouteHead`（Review Focus 4）：`"a-ui-us -> a-ui-default"` → `a-ui-us`；`"a-ui-us >> x"` → `a-ui-us`；`"direct"` → `direct`；`""` → `""`。
  - `TestCombineVerdict`，逐行对应 spec §5：任一样例 `Match=false` → mismatch；`log.Others` 非空 → mismatch（即使核心全部符合）；`coreState=pending` 且无不符 → pending；全部符合 → verified；样例为空 → unverifiable，reason `本规则的域名都是关键词/正则/geosite 写法，推不出可测试的域名`；样例全部带 `Error` → unverifiable；`coreState=not_running` → unverifiable，reason `xray 未运行`；只有日志 Matched>0、没有样例 → unverifiable（日志单独不能变绿）。
- [ ] **Step 2: 运行** `go test ./web/service -run 'TestSampleTargets|TestJudgeSample|TestRouteHead|TestCombineVerdict'`。期望结果：FAIL。
- [ ] **Step 3: 实现。** IP 段的「首个可用地址」：解析 CIDR 后网络地址 +1；`/32`、`/128` 取地址本身。带 `!` 或含 `:` 前缀的 IP 条目（`geoip:`/`ext:`）跳过。`combineVerdict` 判定顺序严格按 spec §5 自上而下。
- [ ] **Step 4: 运行同上命令**。期望结果：PASS。
- [ ] **Step 5: Commit**（需授权）`feat(service): routing verification verdict logic`

### Task 4: 访问日志聚合与 xray 匹配器

**Files:**
- Modify: `web/service/accesslog.go`（在 `CountByRoute` 之后追加）
- Modify: `xray/api.go`（导出 `EnsureAssetLocation`）
- Modify: `web/service/routing_verify.go`
- Test: `web/service/routing_verify_log_test.go`（新建，复用 `setupAccessLogDB`）

**Interfaces:**
- Consumes: Task 3 的 `LogEvidence`、`LogOther`、`ExpectedOutbound`、`judgeSample`、`routeHead`。
- Produces:
  - `type TargetRouteCount struct { Target, Route string; Accepted bool; Count, LastAt int64 }`
  - `func (s *AccessLogService) GroupByTargetRoute(inboundId int, sinceMs int64, limit int) (rows []TargetRouteCount, truncated bool, err error)`：执行 `GROUP BY target, route, accepted`，`MAX(time)` 作为 LastAt，按 count 降序；查询 `limit+1` 行来判断是否截断；`GetAccessLogDB()==nil` 时返回空。
  - `func EnsureAssetLocation()`（新增于 `xray/api.go`，直接调用包内未导出的 `ensureXrayAssetLocation()`），供 `geosite:`/`ext:` 条目加载 dat 文件。
  - `type groupMatcher struct{...}`；`func newGroupMatcher(domains, cidrs []string) (*groupMatcher, error)`：先调 `xray.EnsureAssetLocation()`；某一侧条目为空时不构造那一侧的匹配器。
  - `func (m *groupMatcher) Match(target string) bool`：用 `net.SplitHostPort` 拆出主机（失败则整串当主机），去掉 IPv6 方括号；是 IP 的走 IP 匹配器，否则走域名匹配器。
  - `func buildLogEvidence(rows []TargetRouteCount, m *groupMatcher, exp ExpectedOutbound, defaultTag string, labelOf func(tag string) string) LogEvidence`：只统计 `m.Match` 为真的行；`Accepted=false` 计入 Rejected；其余用 `judgeSample(exp, routeHead(r.Route), false, defaultTag)` 判断，符合的计入 Matched，不符的按 routeHead 分组进 Others（每组 Count 求和，LastAt 取最大，Examples 最多 5 个），Others 按 Count 降序。

- [ ] **Step 1: 写失败测试。**
  - `TestGroupMatcher`：domains `["domain:openai.com","full:chatgpt.com","keyword:gpt","regexp:^api\\."]`，cidrs `["1.2.3.0/24"]`。期望命中：`api.openai.com:443`、`chatgpt.com:443`、`mygpt.io:443`、`api.x.com:80`、`1.2.3.9:443`、`[2001:db8::1]:443`（仅当加入 `2001:db8::/32` 时）。期望不命中：`notopenai.com:443`、`sub.chatgpt.com:443`、`5.6.7.8:443`。
  - `TestGroupByTargetRoute`：插入 3 条 `chatgpt.com:443/a-ui-us`、1 条 `chatgpt.com:443/a-ui-default`、1 条 24h 前的记录，以及别的入站的记录。期望 2 行，计数分别为 3 和 1；`limit=1` 时 `truncated=true`。
  - `TestBuildLogEvidence`：期望 proxy `a-ui-us`。输入行：`chatgpt.com:443 a-ui-us ×3`、`chatgpt.com:443 "a-ui-us -> a-ui-default" ×1`、`chatgpt.com:443 a-ui-default ×2 LastAt=X`、`example.com:443 a-ui-default ×9`、一条 Accepted=false ×1。期望 `Matched=4`、`Rejected=1`，`Others=[{Route:"a-ui-default",Count:2,LastAt:X,Examples:["chatgpt.com:443"]}]`；`example.com` 不计入。
- [ ] **Step 2: 运行** `go test ./web/service -run 'TestGroupMatcher|TestGroupByTargetRoute|TestBuildLogEvidence'`。期望结果：FAIL。
- [ ] **Step 3: 实现上述函数。**
- [ ] **Step 4: 运行同上命令**。期望结果：PASS。
- [ ] **Step 5: Commit**（需授权）`feat(service): access-log evidence for routing verification`

### Task 5: RoutingVerifyService.Verify 编排（含真实 xray 端到端）

**Files:**
- Modify: `web/service/routing_verify.go`
- Test: `web/service/routing_verify_service_test.go`（新建，用伪造的 route 函数）
- Test: `web/service/routing_verify_e2e_test.go`（新建，复用 `xray_hot_reload_e2e_test.go` 里的 `requireXrayBinary`/`requireXrayRoutingService`/`requireXrayAPIPortFree`/`freePort`/`waitForPort`/`mustEncodeIds`/`mustEncodeGroupIds` 和 Cleanup 写法）

**Interfaces:**
- Consumes: Task 1 `XrayAPI.TestRoute`；Task 2 `RuntimeSnapshot`/`XrayRuntime`；Task 3 全部；Task 4 `GroupByTargetRoute`/`newGroupMatcher`/`buildLogEvidence`；现有 `RoutingRuleService.Get`、`DecodeInboundIds`、`DecodeDomainGroupIds`、`DomainGroupService.Get`、`DecodeDomains`/`DecodeSubscribedDomains`/`DecodeCidrs`/`DecodeSubscribedCidrs`、`MergeDomains`、`OutboundNodeService.Get`、`InboundService.GetAllInbounds`、`SettingService.GetAccessLogEnable`。
- Produces:
  - `type routeFunc func(ctx context.Context, inboundTag, domain string, ip net.IP) (tag string, noMatch bool, err error)`
  - `type RoutingVerifyService struct { route routeFunc; runtime func() XrayRuntime; now func() time.Time; rpcTimeout, budget time.Duration }`：零值可用，nil/0 字段时使用真实实现和默认值（3s / 20s）。真实 route 每次 `Verify` 现开一个 `XrayAPI` 连接，`defer Close`。
  - `func (s *RoutingVerifyService) Verify(ruleId int) (*RuleVerifyResult, error)`
  - 包级 `var verifyMu sync.Mutex`，用 `TryLock`；失败时返回 `Core.State="busy"`、`Message="已有验证在进行，请稍后"`，`Users` 为空。

- [ ] **Step 1: 写失败测试（伪造 route/runtime，使用 `setupDB` + `setupAccessLogDB` 建数据）。**
  - `TestVerifyVerified`：proxy 规则覆盖入站 A，伪造 route 返回节点 tag → A 为 verified，`Expected.Label` 为节点 remark。
  - `TestVerifyMismatch`：route 返回 `a-ui-default` → mismatch，`Samples[0].ActualLabel` 非空。
  - `TestVerifyPending`：runtime `Pending=true` → `Core.State=pending`，用户为 pending。
  - `TestVerifyNotRunning`：runtime `Running=false` → `Core.State=not_running`，未调用 route（计数断言为 0）。
  - `TestVerifyBusySnapshot`：runtime `Busy=true` → `Core.State=busy`。
  - `TestVerifyRejectsConcurrentRun`（Review Focus 3）：测试里先持有 `verifyMu`，然后调用 → `Core.State=busy`。
  - `TestVerifyDisabledInboundIsUnverifiable`（Review Focus 1）：规则覆盖 A（启用）与 B（停用）→ B 为 unverifiable，reason `该用户已停用，规则不会对其生效`，且不对 B 调 route。
  - `TestVerifyTimeoutIsUnverifiable`（Review Focus 2）：route 阻塞直到 ctx 结束 → Sample.Error 为 `超时`，用户为 unverifiable。测试里设 `rpcTimeout: 50*time.Millisecond`。
  - `TestVerifyGlobalRuleTruncates`：`inboundIds=[]`，有 52 个启用入站 → `Truncated=true`，`len(Users)=50`。
  - `TestVerifyAccessLogDisabled`：`accessLogEnable=0` → `AccessLog.Enabled=false`，各用户 `Log=nil`。
  - `TestVerifyDisabledRule`：规则 `Enable=false` → `Core.State=error`，Message 非空，不调用 route。
- [ ] **Step 2: 运行** `go test ./web/service -run 'TestVerify'`。期望结果：FAIL。
- [ ] **Step 3: 实现 `Verify`。** 步骤顺序：取 verifyMu → 读规则（停用直接返回 error 状态）→ 计算 Expected（proxy 用节点 Tag 和 Remark，block 用 `model.BlockOutboundTag`/「封禁」，direct 用 `runtime.DefaultTag`/「直连」）→ 确定用户列表（`[]` 时取全部启用入站，并按 50 截断）→ 每个组用 `sampleTargets(…, 2)` 取样，合并后截到 6 → 判断 runtime 状态 → 对每个启用的用户逐个样例调 route（单次 3s，整体 20s 的 ctx）→ 若访问日志已开启，对每个用户执行 `GroupByTargetRoute(inboundId, now-24h, 5000)` + `buildLogEvidence` → `combineVerdict`。`labelOf` 把节点 tag 映射到 remark，`a-ui-block` 映射为「封禁」，默认 tag 和计量 tag 映射为「直连（默认出站）」，其余原样返回。noMatch 的样例 `ActualLabel` 为 `默认出站（未命中任何规则）`。
- [ ] **Step 4: 运行同上命令**。期望结果：PASS。
- [ ] **Step 5: 写端到端测试** `TestRouteVerifyAgainstRealXray`：用真实 xray（前置条件与 hot reload e2e 相同）、socks 入站 A 与 B、一个 `full:localhost` 组、一条只覆盖 A 的 block 规则。依次断言：
  - `RestartXray(true)` 之后 Verify → A 为 verified，实际 tag 为 `a-ui-block`；
  - 把规则改为 proxy 指向一个 `freedom` 节点并保存，**不调用** RestartXray → `Core.State=pending`；
  - 调用 `RestartXray(false)` 后再 Verify → A 为 verified，tag 为节点 tag；
  - 再新建一条只覆盖 A、组为 `keyword:zzz` 的 block 规则 → 该规则的 Verify 结果中 A 为 unverifiable。
- [ ] **Step 6: 运行** `go test ./web/service -run TestRouteVerifyAgainstRealXray -v`。期望结果：PASS；缺少二进制或 RoutingService 时为 SKIP，并给出原因。
- [ ] **Step 7: Commit**（需授权）`feat(service): verify routing rule against live core and access log`

### Task 6: 接口 `POST /aui/routing/rule/verify/:id`

**Files:**
- Modify: `web/controller/routing.go`（`RoutingController` 增加字段 `verifyService service.RoutingVerifyService`；在 `rl` 分组注册 `rl.POST("/verify/:id", a.verifyRule)`）
- Test: `web/controller/routing_verify_test.go`（新建；若 controller 包没有现成的 gin 测试脚手架，就只测 id 解析：`TestVerifyRuleRejectsBadId` 用 `httptest` + `gin.CreateTestContext`，传 `id=abc` → `success=false`）

**Interfaces:**
- Consumes: Task 5 `RoutingVerifyService.Verify`
- Produces: 响应为 `jsonObj(c, result, err)`，结构同 spec §7。

- [ ] **Step 1: 写失败测试** `TestVerifyRuleRejectsBadId`。
- [ ] **Step 2: 运行** `go test ./web/controller -run TestVerifyRuleRejectsBadId`。期望结果：FAIL。
- [ ] **Step 3: 实现 `verifyRule(c *gin.Context)`**：`strconv.Atoi` 失败时 `jsonMsg(c, "验证分流规则", err)`；否则 `jsonObj(c, result, err)`。
- [ ] **Step 4: 运行同上命令，再跑** `go test ./web/...`。期望结果：PASS（含 `TestNoStateChangingRequestBypassesAxios` 等既有守卫）。
- [ ] **Step 5: Commit**（需授权）`feat(controller): routing rule verify endpoint`

### Task 7: 前端：图上的状态、汇总、明细与触发

**Files:**
- Modify: `web/html/xui/routing.html`（模板 `:1547-1597` 的 `.rv-flow`；`flowUsers` `:2098`；`drawRuleWires` `:2930`；相关 watch `:3310`；保存成功路径）
- Modify: `web/assets/css/custom.css`（只在复用 `.status-badge` / `status-ok|warn|danger|neutral` 不够时，补 `.rv-verify-*` 布局类；不新增颜色变量）
- Test: 现有 `web/html_test.go`

**Interfaces:**
- Consumes: Task 6 接口与 spec §7 的 JSON。
- Produces（前端内部约定）:
  - data `verify: { ruleId: 0, loading: false, result: null, error: '', expanded: {}, retryTimer: null }`
  - methods `runVerify(ruleId)`（`HttpUtil.post('aui/routing/rule/verify/' + id)`；loading 时直接 return）、`userVerify(u)`（按 `flowUsers` 的 key 返回对应的 `UserVerify`；`key==='all'` 时返回汇总对象 `{verified, total, failed: UserVerify[]}`）、`verdictMeta(verdict)` → `{cls, icon, text}`：
    - verified → `status-ok` / `check-circle` / 已验证
    - mismatch → `status-danger` / `close-circle` / 与配置不符
    - pending → `status-warn` / `clock-circle` / 等待下发
    - unverifiable → `status-neutral` / `question-circle` / 无法验证
  - computed `canVerify`：`savedRule && savedRule.enable && !ruleIssue(savedRule) && !ruleDirty`

- [ ] **Step 1: 模板。**
  - 每个 `.rv-urow` 末尾加 `<button type="button" class="status-badge …" :aria-expanded="…" @click="toggleVerify(u.key)">`，内含图标与文字。不可验证时，在 `title` 和展开明细里都写出原因。
  - `.rv-flow` 标题行右侧放一个 `<span role="status" aria-atomic="true">` 汇总（如「已验证 2/2 · HH:mm」，时间用 `DateUtil`），旁边放 `<a-button size="small" :loading="verify.loading" :disabled="!canVerify" @click="runVerify(savedRule.id)">重新验证</a-button>`。
  - `!canVerify && ruleDirty` 时，徽章区统一显示 `status-neutral`「保存后验证」。
  - 明细展开区：样例列表「目标 → 实际出站」；`hitByRule=false` 时注明「出口一致，但不是由本规则命中」。日志部分：「近 24 小时符合 N 次」；对每个 Others 写「近 24 小时有 N 次走了 {label}（最近一次 {DateUtil 时间}）」并列出示例。`AccessLog.Enabled=false` 时写「未开启访问日志，无法核对真实连接（可在面板设置中开启）」。
  - 全局规则行的展开区只列出 verdict 不是 verified 的用户。`Truncated` 时写「只验证了前 50 个已启用入站」。
  - 所有 antd 标签显式闭合。
- [ ] **Step 2: 行为。**
  - 选中规则变化且 `canVerify` 时自动 `runVerify` 一次（只 watch 规则 id，避免编辑时触发）。
  - 规则保存成功后调用 `runVerify`；如果结果 `core.state==='pending'`，设一个 12 秒的一次性 `retryTimer` 再验证一次。切换规则或组件卸载时清掉该定时器。
  - 切换规则时清空 `verify.result`，避免把上一条规则的结果显示在新规则上（用 `verify.ruleId` 校验）。
- [ ] **Step 3: 连线着色。** `drawRuleWires` 按每行 `userVerify` 选色：verified 用现有蓝/绿，mismatch 用 `getComputedStyle` 读到的 `--aui-danger`，其余用中性灰 `#94A0B8`。右侧主线：任一用户 mismatch 时为红，否则沿用现有颜色。`verify.result` 变化时调用 `scheduleRuleWires()`。
- [ ] **Step 4: 运行** `go test ./web/ -run 'TestAllTemplatesParse|TestNoSelfClosingAntdComponents|TestVueDirectivesLiveInsideAVueRoot|TestNoStateChangingRequestBypassesAxios'`。期望结果：PASS。
- [ ] **Step 5: Commit**（需授权）`feat(routing-ui): show live verification on traffic flow diagram`

### Task 8: 浏览器视觉/交互验收与文档同步

**Files:**
- Modify: `CLAUDE.md`（在「域名分流管理」下新增小节「流量走向验证」，写明：TestRoute 的只读约束、`ErrNoClue` 按消息文本识别、匹配必须复用 xray 匹配器、日志单独不变绿、`lock` 只能短持）
- 验收截图：放在 scratchpad，不进仓库

- [ ] **Step 1:** `XUI_DEBUG=true go run main.go`（在仓库根目录运行），需要 macOS 本地的 `bin/xray-darwin-arm64`；在本地测试库里建两个 socks 入站、一个节点和若干规则。不在生产面板上做写操作。
- [ ] **Step 2:** 加载 `ui-ux-pro-max`，并确认 `taste-skill:design-taste-frontend` 的 app-screen 规则是否适用。用 Chrome 打开分流页，在 1440 和 390 两个宽度下分别截图检查四种状态：已验证；改规则后的等待下发；停掉 xray 后的无法验证；故意建一条被更高优先级规则抢先的规则得到不符。检查键盘 Tab 能聚焦徽章、Enter 能展开、`aria-expanded` 会切换，汇总文字随结果更新，控制台没有新增错误，用户名不被截断。
- [ ] **Step 3:** 修复发现的问题，再走一轮「渲染 → 查看 → 对照」。
- [ ] **Step 4:** 运行 `make verify`。期望结果：vet、test、build 全部通过。
- [ ] **Step 5: Commit**（需授权）`docs: record routing flow verification invariants`
