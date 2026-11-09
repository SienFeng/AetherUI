# 地区限制：失败收窄、预演、出差通行证与自动出差放行 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一个入站的地区规则故障只影响它自己，并让地区限制可预演、可临时放行、可在客户出差时自动放行。

**Architecture:** 生成链改为按入站降级（last-known-good / fail closed），降级状态放在包级变量里，供「需要留意」读取。通行证单独建表，生成期只读 `ended_at = 0`，不读时间；到期与自动放行都由任务改表，再置重启标志、走路由热应用。预演与自动放行共用一个按执行口径构造的内存 CIDR 集合。

**Tech Stack:** Go（Gin、GORM/SQLite），Vue2 + ant-design-vue 模板，xray-core 路由。

**Spec:** `docs/superpowers/specs/2026-11-09-region-flexibility-auto-travel-design.md`

## Global Constraints

- 不 commit、不 push：用户全局规则要求每次提交都要明确授权。各任务的「Commit」步骤替换为 `git status --short` 自查。
- 生成必须逐字节确定：生成期禁止读 `time.Now`，禁止遍历 map 决定输出顺序。
- 凡是按 `inbound_id` 存的新表，必须在 `DelInbound` 的事务里删除，失败即阻断（SQLite 会复用 id）。
- service 无状态；置重启标志只在 controller / job 里做。
- 测试中的 IP 一律用 RFC 5737 地址（192.0.2.0/24、198.51.100.0/24、203.0.113.0/24），不得出现生产 IP。
- 前端：改了 `web/assets/js` 就要同步提升 `cur_ver`（发版由 tag 写入，开发期只需在 debug 模式下验证）；新加的 `model.Inbound` json 字段必须在 `models.js` 的 `DBInbound` 里有同名属性（`web/inbound_model_test.go` 守着）。
- 门禁：`make verify`（vet + test + build）。

## Review Focus

1. 两个入站共用同一个省份集合（同一个 tag）时，其中一个入站有通行证 → 它的有效 tag 变了，另一个不能被连带改变。测试见 Task 6。
2. `*` 通行证期间，`newGeoBlockMatcher` 不能再认为该入站的来源会被拦（否则「需要留意」会误报）。测试见 Task 6。
3. IP 库未加载、但所有地区入站都持有 `*` 通行证 → 不报错，也不生成地区规则。测试见 Task 2/6。
4. 自动放行：家里在线表此刻活跃，但小时行里没有 → 仍视为家里在用，不发放。测试见 Task 9。
5. 编辑入站（改备注）不得影响活动通行证：通行证不在 Inbound 表上，测试见 Task 6 `TestUpdateInboundKeepsPass`。

---

### Task 1: geodat.Decode

**Files:**
- Modify: `util/geodat/geodat.go`（更新包注释「只做编码」，新增 `Decode`）
- Test: `util/geodat/geodat_test.go`

**Interfaces:**
- Produces: `func Decode(data []byte) ([]Entry, error)`。CIDR 以 `ip/prefix` 字符串返回，IPv4 为点分形式。

- [ ] **Step 1: 写失败测试** `TestDecodeRoundTrip`：`Encode([]Entry{{Tag:"A",CIDRs:{"192.0.2.0/24","2001:db8::/32"}},{Tag:"B",CIDRs:{"198.51.100.0/25"}}})` 再 `Decode`，`reflect.DeepEqual` 等于原值（tag 已是大写）。`TestDecodeRejectsTruncated`：截掉最后 3 字节后返回 error。
- [ ] **Step 2:** `go test ./util/geodat/ -run Decode` → FAIL（未定义）
- [ ] **Step 3: 实现**：用 varint + 长度前缀逆向解析三层消息；遇到未知字段号返回 error（文件只由本包生成）。
- [ ] **Step 4:** `go test ./util/geodat/` → PASS
- [ ] **Step 5:** `git status --short` 自查

### Task 2: 地区规则按入站降级（spec §3.1）

**Files:**
- Create: `web/service/geo_degrade.go`
- Modify: `web/service/geo.go`（`geoPlan`、`buildGeoPlan`、`rulesFromGeoPlan`）、`web/service/routing_inject.go:643-670`（`buildGeoRules`）
- Test: `web/service/geo_degrade_test.go`；改写 `web/service/geo_test.go` 中 `TestInjectFailsLoudlyWhenIPDBMissingButRegionsConfigured`，以及所有断言「整份失败」的用例

**Interfaces:**
- Produces:
  - `type GeoDegradation struct { Mode string `json:"mode"`; Reason string `json:"reason"` }`，其中 Mode 为 `geoDegradeStale = "stale"` 或 `geoDegradeClosed = "closed"`。
  - `func GeoDegradedInbounds() map[int]GeoDegradation`：返回快照副本。
  - `geoPlan` 新增字段 `FailClosed map[int]string`（入站 id → 原因）与 `Degraded map[int]GeoDegradation`。
  - `func buildGeoPlanWith(inbounds []*model.Inbound, db provinceCIDRSource, passes map[int]*model.InboundRegionPass, lastGood map[string][]string) *geoPlan`：不再返回 error。`db == nil` 表示 IP 库未加载。`passes` 在本任务一律传 nil（由 Task 6 接上）。原 `buildGeoPlan(inbounds, db) (*geoPlan, error)` 保留为薄包装，供既有测试使用：任一入站 FailClosed 时返回第一个原因作为 error，以保持旧测试语义。
  - 包级 `geoLastGood map[string][]string`（带锁）：成功生成后更新；为空时从 `geodat.Decode(os.ReadFile(geoDatPath))` 做种子。
- Consumes: `geodat.Decode`（Task 1）

规则：
- `rulesFromGeoPlan`：FailClosed 的入站生成一条规则，`ruleTag: "a-ui-geo-closed-<id>"`、`source: ["0.0.0.0/0","::/0"]`、`outboundTag: a-ui-block`。
- `buildGeoRules`：IP 库为 nil 时不再报错，`db` 传 nil 进计划。`writeGeoDat` 失败时，解码现有文件：现有文件里有的 tag 沿用，哈希取现有文件内容的 sha256 前 4 字节；缺的入站并入 FailClosed（原因「写入地区数据文件失败」）；文件读不出时，所有地区入站都 FailClosed。
- 每次生成结束，用 `setGeoDegraded(plan.Degraded)` 整体替换包级状态。状态有变化时才记 Warning，记录内容包括入站 id、Mode 和原因。

- [ ] **Step 1: 写失败测试**（`geo_degrade_test.go`，用测试里的假 `provinceCIDRSource`）：
  - `TestPlanUsesLastGoodWhenProvinceYieldsEmpty`：A 入站 {江苏省}，源返回空，lastGood 里有该 tag → `plan.Entries` 含该 tag 的 lastGood CIDR，`Degraded[A].Mode=="stale"`，FailClosed 为空。
  - `TestPlanFailsClosedOnlyThatInbound`：A 空集且无 lastGood，B {浙江省} 正常 → FailClosed 只有 A，B 的 tag 在 Entries 里。
  - `TestPlanCorruptRegionsFailsClosed`：A.Regions=`"[bad"` → FailClosed[A]，不 panic、不返回 error。
  - `TestPlanNilDBUsesLastGoodOrCloses`：db=nil，A 有 lastGood、B 没有 → A 为 stale，B 为 closed。
  - `TestRulesFailClosedRule`：FailClosed 的入站规则 source == `["0.0.0.0/0","::/0"]`，ruleTag 前缀 `a-ui-geo-closed-`。
  - 改写后的 `TestInjectDegradesWhenIPDBMissing`：IP 库被清空、入站 {江苏省}、没有 dat → `Inject` 返回 nil，规则里有 closed 规则，`GeoDegradedInbounds()[id].Mode=="closed"`。
- [ ] **Step 2:** `go test ./web/service/ -run 'Plan|FailClosed|Degrades'` → FAIL
- [ ] **Step 3: 实现** 上述接口
- [ ] **Step 4:** `go test ./web/service/` → PASS（地区相关的旧用例也要全绿）
- [ ] **Step 5:** `git status --short`

### Task 3: IP 库更新后置重启标志（spec §3.2）

**Files:**
- Modify: `web/job/ipdb_update_job.go`（`Run`、`RunInitial`：`updated > 0` 时 `j.xrayService.SetToNeedRestart()`，结构体加 `xrayService service.XrayService`）、`web/controller/setting.go:173-190`（`updateIPDB` 成功且 `updated > 0` 时置标志；controller 需要能拿到 `XrayService`，按该 controller 现有字段风格添加）
- Test: 无新单测（job 与 controller 没有测试基础设施）。验证方式是 `go build ./...`，并 grep 确认两处调用都在。

- [ ] **Step 1:** 改代码，注释写明原因（核心里的允许集会落后于库）
- [ ] **Step 2:** `go vet ./web/... && go build ./...` → 无输出
- [ ] **Step 3:** `git status --short`

### Task 4: 保存时校验省名（spec §3.3）

**Files:**
- Modify: `web/service/inbound.go`（`normalizeInboundRegions` 之后、`validateInbound` 之前，在 AddInbound 和 UpdateInbound 两条路径上调用）
- Test: `web/service/geo_test.go` 中靠近 `TestAddAndUpdateInboundNormalizeRegions` 的位置

**Interfaces:**
- Produces: `func checkRegionsUsable(newRegions, oldRegions string, db *ipdb.Multi, src provinceCIDRSource) error`。两值规范化后相等时返回 nil；新值为空时返回 nil；`db == nil` 时返回「IP 归属地库未加载，无法设置地区限制」；有不在 `db.Provinces()` 中的省名时，错误信息要列出这些省名；`src.CIDRsOfProvinces` 为空时返回「这些地区在当前合并口径下没有任何 IP 段」。
- 调用方：AddInbound 传 `old=""`；UpdateInbound 传 `oldInbound.Regions`（要在覆盖之前取）。`src` 用 `regionCIDRSource(db, &SettingService{})`。

- [ ] **Step 1: 写失败测试** `TestCheckRegionsUsable`（表驱动，用假 Multi 或 `requireIPDB`，参照 geo_test 的既有做法）：未变 → nil；含「火星省」→ error 且包含「火星省」；db nil 且新值非空 → error；新值为空 → nil。
- [ ] **Step 2:** 运行 → FAIL
- [ ] **Step 3:** 实现并接入两条写路径
- [ ] **Step 4:** `go test ./web/service/` → PASS
- [ ] **Step 5:** `git status --short`

### Task 5: 「超额被拒」标记过期（spec §3.4）

**Files:**
- Modify: `web/service/online.go`（新增方法）、`web/service/concurrency.go:108-147`（`Enforce`）
- Test: `web/service/concurrency_test.go`

**Interfaces:**
- Produces: `func (t *onlineTracker) retainRejectedPorts(ports map[int]bool)`：删掉 `rejectedNow` 中端口不在 `ports` 里的项；`rejectedAt` 保持不动（它有自己的 5 分钟保留期，用于显示已断开的来源）。
- `Enforce`：`len(inbounds)==0` 的提前返回之前调 `retainRejectedPorts(nil)`；循环结束后，用本轮遍历过的端口集合调用一次。

- [ ] **Step 1: 写失败测试** `TestRetainRejectedPortsDropsStale`：`setRejected(1001, ["192.0.2.1"])` → `retainRejectedPorts({})` → `snapshotIdle(1001, ...)` 中该 IP（先用测试里既有的方式注入一个在线条目）的 `Blocked == false`。
- [ ] **Step 2:** FAIL → **Step 3:** 实现 → **Step 4:** PASS
- [ ] **Step 5:** `git status --short`

### Task 6: 通行证表、有效地区与手动接口（spec §4.1）

**Files:**
- Create: `database/model/region_pass.go`、`web/service/region_pass.go`
- Modify: `database/db.go`（主库 AutoMigrate 加 `&model.InboundRegionPass{}`）、`web/service/geo.go`（`effectiveRegions`）、`web/service/routing_inject.go`（`buildGeoRules` 读活动通行证）、`web/service/routing_geo_evidence.go:41-66`（`newGeoBlockMatcher` 用有效地区）、`web/service/inbound.go`（`DelInbound` 事务内删除）、`web/job/check_inbound_job.go`、`web/controller/inbound.go`（三个路由）
- Test: `web/service/region_pass_test.go`

**Interfaces:**
- Produces:
  ```go
  type InboundRegionPass struct {
      Id        int    `json:"id" gorm:"primaryKey;autoIncrement"`
      InboundId int    `json:"inboundId" gorm:"index"`
      Regions   string `json:"regions"` // JSON 数组；`["*"]` = 暂停地区限制
      Auto      bool   `json:"auto"`
      GrantedAt int64  `json:"grantedAt"` // ms
      Until     int64  `json:"until"`     // ms
      EndedAt   int64  `json:"endedAt"`   // ms，0 = 活动
      EndReason string `json:"endReason"` // expired|revoked|replaced|home|coexist
  }
  const RegionPassAll = "*"
  ```
  `RegionPassService`：
  - `Grant(inboundId int, regions []string, until int64, auto bool, now time.Time) (*model.InboundRegionPass, error)`：校验入站存在且配了地区；regions 只能是 `["*"]` 或全部在 `db.Provinces()` 里；同一事务内以 `replaced` 结束旧的活动通行证。
  - `Revoke(inboundId int, reason string, now time.Time) (bool, error)`
  - `Active() (map[int]*model.InboundRegionPass, error)`：所有 `ended_at = 0` 的记录。
  - `ActiveOf(inboundId int) (*model.InboundRegionPass, error)`
  - `List(inboundId, limit int) ([]model.InboundRegionPass, error)`：按 id 降序。
  - `ExpireDue(now time.Time) (int64, error)`
  - `DeleteByInbound(tx *gorm.DB, inboundId int) error`
  - `func effectiveRegions(in *model.Inbound, pass *model.InboundRegionPass) (provinces []string, suspended bool, err error)`
- `buildGeoPlanWith` 在 Task 2 里留的 `passes` 参数改为真正使用。`newGeoBlockMatcher(inbounds)` 内部调 `RegionPassService.Active()`；读失败时记 Warning、按无通行证处理。`suspended` 的入站在两处都跳过。
- 路由（参数用 form 绑定，`id` 取自 path）：
  - `POST /inbound/regions/pass/grant/:id`，参数 `regions`（JSON 串）、`hours`（1–720）；
  - `POST /inbound/regions/pass/revoke/:id`；
  - `POST /inbound/regions/pass/list/:id`，返回最近 30 条。
  - grant 和 revoke 成功后都调 `a.xrayService.SetToNeedRestart()`。
- `CheckInboundJob.Run`：在既有逻辑之后调 `ExpireDue(time.Now())`；`n > 0` 时置标志，出错时记 Warning（说明失败方向是放宽）。

- [ ] **Step 1: 写失败测试**：
  - `TestEffectiveRegionsUnion`：{江苏省} + 通行证 {上海市} → [上海市 江苏省]；`["*"]` → suspended。
  - `TestGrantReplacesActive`：先后 Grant 两次 → `Active()` 只有第二条，第一条 `EndReason=="replaced"`。
  - `TestGrantRejectsUnknownProvince`、`TestGrantRejectsInboundWithoutRegions`。
  - `TestExpireDue`：until 在 now 之前 → 返回 1，记录变为 `expired`；再跑一次返回 0。
  - `TestPlanPassChangesOnlyThatInboundTag`（Review Focus 1）：A、B 都是 {江苏省}，A 有 {上海市} 通行证 → A 与 B 的 tag 不同，B 的 tag 等于 `regionTag([江苏省])`。
  - `TestPlanSuspendedPassSkipsAndToleratesNilDB`（Review Focus 3）：唯一的地区入站持 `*`，db=nil → 计划为空，无 FailClosed。
  - `TestGeoBlockMatcherSkipsSuspended`（Review Focus 2）。
  - `TestDelInboundDeletesPasses`。
  - `TestUpdateInboundKeepsPass`（Review Focus 5）：Grant 后调 UpdateInbound 改备注 → `ActiveOf` 仍存在。
- [ ] **Step 2:** FAIL → **Step 3:** 实现 → **Step 4:** `go test ./web/service/ ./database/...` PASS
- [ ] **Step 5:** `go build ./... && git status --short`

### Task 7: CIDR 集合与预演（spec §4.2）

**Files:**
- Create: `web/service/region_preview.go`
- Modify: `web/controller/inbound.go`（路由 `POST /inbound/regions/preview`）
- Test: `web/service/region_preview_test.go`

**Interfaces:**
- Produces:
  - `type cidrSet struct{...}`，`func newCIDRSet(cidrs []string) *cidrSet`（IPv4 区间排序合并后二分查找；IPv6 前缀用线性列表即可），`func (s *cidrSet) Contains(ip string) bool`。
  - `func allowedSetFor(provinces []string) (*cidrSet, error)`：IP 库为 nil 时返回 error；CIDR 来自 `regionCIDRSource(db, &SettingService{}).CIDRsOfProvinces`。
  - ```go
    type RegionPreview struct {
        TotalHours, BlockedHours int; TotalBytes, BlockedBytes int64
        Blocked []PreviewSource `json:"blocked"` // 按字节降序，最多 20
        MoreBlocked int `json:"moreBlocked"`
        IPv6 []PreviewSource `json:"ipv6"`
        Suggestion RegionSuggestion `json:"suggestion"`
    }
    type PreviewSource struct { IP, Province, City, ISP string; Hours int; Bytes int64; LastHour int64 }
    func (s *RegionPreviewService) Preview(inboundId int, regions []string, now time.Time) (*RegionPreview, error)
    ```
    行数据取自 `SharingService.windowRows(inboundId, 7, now)`。按 IP 聚合；`Hours` 为不同 HourStart 的个数；Province/City/ISP 取该 IP 最新一行的快照。regions 为空时返回 error「请先选择地区」。
  - `func previewFromRows(rows []model.InboundIPHour, allowed *cidrSet) RegionPreview`：纯函数，供测试直接调用。

- [ ] **Step 1: 写失败测试**：`TestCIDRSetContains`（v4 边界：192.0.2.0/24 包含 .0 和 .255，不包含 198.51.100.1；v6 2001:db8::/32）；`TestPreviewFromRows`：两个 v4 IP（一个在集合内）加一个 v6 行 → Blocked 只有集合外的那个 IP，IPv6 单列，BlockedBytes 正确，TotalHours 是不同小时数。
- [ ] **Step 2–4:** FAIL → 实现 → PASS
- [ ] **Step 5:** `git status --short`

### Task 8: 「需要留意」：拦截标注、降级与通行证（spec §4.3、§3.1 可见性）

**Files:**
- Modify: `web/service/inbound_attention.go`
- Test: `web/service/inbound_attention_test.go`（文件已存在则追加）

**Interfaces:**
- `GeoBlockedIP` 新增 `Province string`（多数票显示省，复用 `locateWithIPDB` 的口径；查不到时为空）和 `Kind string`（取值 `geoBlockedCoexist="coexist"` 或 `geoBlockedTravel="travel"`）。
- `InboundAttention` 新增：
  - `GeoDegraded *GeoDegradation`（来自 `GeoDegradedInbounds()`），
  - `RegionPass *model.InboundRegionPass`（活动通行证），
  - `AutoTravel *AutoTravelStatus`（Task 9 定义；本任务先留 nil）。
- `func classifyBlocked(lastAt int64, rows []model.InboundIPHour, home *cidrSet) string`（纯函数）：lastAt 所在小时或前一小时内，有 home 集合内的行 `ActiveBytes >= coexistMinActiveBytes` → coexist，否则 travel。`home` 用**存储的** Regions 构造（不含通行证）；构造失败时 Kind 留空。
- 侧栏提醒计数口径不变（只按入站计数）；降级的入站也计入提醒。前端计数在 Task 10 调整。

- [ ] **Step 1: 写失败测试** `TestClassifyBlocked`：同小时 home 行 2MB → coexist；只有 500KB → travel；只在前一小时 → coexist；两小时前 → travel。
- [ ] **Step 2–4:** FAIL → 实现并接入 Summary → PASS
- [ ] **Step 5:** `git status --short`

### Task 9: 自动出差放行（spec §4.4）

**Files:**
- Modify: `database/model/model.go`（`AutoTravel bool `json:"autoTravel" form:"autoTravel"``）、`web/service/inbound.go`（UpdateInbound 白名单加 `oldInbound.AutoTravel = inbound.AutoTravel`）、`web/assets/js/model/models.js`（`DBInbound` 加 `this.autoTravel = false;`）、`web/html/xui/inbounds.html:1135` 与 `:1158` 两处提交体加 `autoTravel: dbInbound.autoTravel`
- Create: `web/service/auto_travel.go`、`web/job/auto_travel_job.go`
- Modify: `web/web.go`（`s.cron.AddJob("@every 1m", job.NewAutoTravelJob())`）、`web/service/inbound_attention.go`（填 `AutoTravel`）
- Test: `web/service/auto_travel_test.go`

**Interfaces:**
- 常量（spec §4.4 原值）：`autoTravelHomeSilence = 2*time.Hour`、`autoTravelStableDays = 3`、`autoTravelBlockedLookback = 10*time.Minute`、`autoTravelPassDuration = 72*time.Hour`、`autoTravelRenewWithin = 24*time.Hour`、`autoTravelMonthlyDays = 10`、`autoTravelWeeklyGrants = 3`、`autoTravelCooldown = 7*24*time.Hour`。
- `type AutoTravelStatus struct { At int64; Action string; Reason string; Region string }`，Action 取值为 `granted` / `denied` / `renewed` / `ended` / `idle`。`func AutoTravelStatuses() map[int]AutoTravelStatus` 返回内存快照。
- 纯决策函数，即全部输入都由调用方给出：
  ```go
  type autoTravelInput struct {
      Now time.Time; Loc *time.Location
      HomeRows []model.InboundIPHour      // 近 7 天、IP 在 home 集合内的行
      PassRows []model.InboundIPHour      // 近 24h、IP 在活动通行证省份集合内且不在 home 内的行
      HomeLiveActive, PassLiveActive bool // 在线表此刻是否有活跃来源（非闲置）
      Blocked []autoTravelCandidate       // 近 10 分钟被地区拦下的 v4 来源 + 已验证的候选省
      Active *model.InboundRegionPass
      History []model.InboundRegionPass    // 近 35 天该入站的通行证
      RiskLevel string
  }
  type autoTravelCandidate struct { IP, Province string }
  type autoTravelDecision struct { Action, Reason, Province string; Until int64; EndReason string }
  func decideAutoTravel(in autoTravelInput) autoTravelDecision
  ```
  判定顺序：
  1. 有活动的**自动**通行证时：
     - 家里活跃（HomeRows 中当前小时或前一小时有 ≥1MB 的行，或 HomeLiveActive）→ `ended`；EndReason 在 `HomeLiveActive && PassLiveActive` 时为 `coexist`，否则为 `home`。
     - 否则，剩余时间 < RenewWithin、PassRows 非空、当月已用天数 < 上限 → `renewed`，Until = now+72h。
     - 其余情况 → `idle`。
  2. 有活动的手动通行证 → `idle`（原因「已有手动通行证」）。
  3. Blocked 为空 → `idle`。
  4. 依次检查，第一个不满足的作为 `denied` 的原因：
     - 风险等级为 high / severe；
     - 稳定的家（HomeRows 中 ≥1MB 的行覆盖不同日期数 ≥ 3）；
     - 家里静默（HomeRows 中没有 HourStart ≥ 当前小时起点 − 2h 且 ≥1MB 的行，且 !HomeLiveActive）；
     - 冷却期（History 里最近 7 天没有 `coexist` 结束的记录）；
     - 周次数（近 7 天 Auto 通行证 GrantedAt 个数 < 3）；
     - 月天数（本自然月内 Auto 通行证实际生效时长累计 < 10 天）。
  5. 取第一个有省份的候选 → `granted`，Until = now+72h。
- `AutoTravelService.Run(now)`：遍历「启用 + 有存储 Regions + AutoTravel」的入站，组装输入后调 `decideAutoTravel`，再执行：granted 调 `RegionPassService.Grant(..., auto=true)`；renewed 用 UPDATE 改 until；ended 调 `Revoke(id, EndReason)`。返回 `changed bool`。
  - 访问日志未开启：记状态 `denied`「访问日志未开启」，并跳过所有发放。
  - 候选省份：`locateWithIPDB` 的多数票省份，再用 `allowedSetFor([省]).Contains(ip)` 验证；不包含则丢弃。IPv6 与无省份的来源直接丢弃。
  - 在线活跃：`onlineTrackerInstance.snapshotIdle(port, noLocate, idleAfter)` 中 `!Idle && Conns > 0` 的条目，按 home 集合与通行证集合分类。
- Job：`changed` 时 `SetToNeedRestart()`；出错时记 Warning。

- [ ] **Step 1: 写失败测试**（全部针对 `decideAutoTravel`）：
  - `TestAutoTravelGrantsWhenHomeSilent`
  - `TestAutoTravelDeniesWhenHomeRecentlyActive`（2 小时内有 2MB 的行）
  - `TestAutoTravelDeniesWhenHomeLiveActive`（Review Focus 4：没有行，但 HomeLiveActive）
  - `TestAutoTravelDeniesWithoutStableHome`
  - `TestAutoTravelDeniesHighRisk`
  - `TestAutoTravelDeniesInCooldown`
  - `TestAutoTravelDeniesWeeklyCap`（近 7 天已有 3 张）
  - `TestAutoTravelDeniesMonthlyCap`（当月累计 10 天）
  - `TestAutoTravelEndsCoexist`、`TestAutoTravelEndsHome`、`TestAutoTravelRenews`
  - `TestAutoTravelIgnoresManualPass`
  - 测试 IP 一律用 RFC 5737 地址。
- [ ] **Step 2–4:** FAIL → 实现 → PASS；另外 `go test ./web/` 中的 `inbound_model_test` 要通过（models.js 已同步）
- [ ] **Step 5:** `go build ./... && git status --short`

### Task 10: 前端

**Files:**
- Modify: `web/html/xui/form/inbound.html`（地区限制区域：预演按钮与结果、填入建议、自动出差开关）、`web/html/xui/inbounds.html`（通行证 chip、降级 chip、通行证弹窗、「需要留意」里拦截 IP 的省份与疑似共享 / 出差标签及「发通行证」按钮）、`web/assets/js/util/inbound-attention.js`（降级计入提醒）
- 新组件只有在现有 `inbounds.html` 体量确实承受不住时，才放进 `web/html/xui/region_pass_modal.html`，并参照 `sharing_modal.html` 的引入方式。

要求：
- 开始前调用 `ui-ux-pro-max` 技能（局部调整只做定向检索），沿用 ant-design-vue 与现有 chip、tooltip、modal 风格。
- 文案：通行证 chip「出差通行证 · 上海市 · 至 11-12 14:00」，自动发放的加「自动」；降级 chip 红色，stale 显示「地区数据沿用旧版」，closed 显示「地区规则异常，已全部拦截」，tooltip 给出原因；自动出差开关的说明写明「家里 2 小时没用、外地被拦时自动放行 72 小时；每月最多 10 天」。
- 时间显示走 `DateUtil`。
- 状态：预演要有加载中、空（近 7 天没有数据）、错误三种状态；发放时禁止重复提交。
- 浏览器验收：`XUI_DEBUG=true go run main.go`，数据库用临时库，不碰生产。宽、窄两个视口各截图，检查控制台没有新增报错。

- [ ] **Step 1:** 调用 ui-ux-pro-max
- [ ] **Step 2:** 实现
- [ ] **Step 3:** 本地运行，造数据（加一个入站、配地区、发一张通行证），截图验收
- [ ] **Step 4:** `go test ./web/ ./web/service/` PASS
- [ ] **Step 5:** `git status --short`

### Task 11: 文档

**Files:**
- Modify: `CLAUDE.md`：在「地区限制的两种合并口径」附近新增「地区限制的降级、通行证与自动出差」一节；在「定时任务」里补 `AutoTravelJob`（1m）；在「入站『需要留意』摘要」里补新增字段。

- [ ] **Step 1:** 写文档（只写不能从代码直接看出的约束：生成期不读时间、通行证不在 Inbound 表、降级语义、自动放行的判定顺序）
- [ ] **Step 2:** `make verify` → 全绿
- [ ] **Step 3:** `git status --short`，核对改动清单
