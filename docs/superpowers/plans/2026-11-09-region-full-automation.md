# 地区限制全自动化（第二期）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. 每一步测试先行（RED → GREEN）。

**Goal:** 管理员不出手：自动设置只设一个省、长住自动改地区、出差放行补防共享门槛、地区库遗留问题全部修掉。

**Spec:** `docs/superpowers/specs/2026-11-09-region-full-automation-design.md`

## Global Constraints

- 总开关 `autoRegionEnable` 默认关：不打开时升级后行为零变化。
- 新设置项按项目五处规则加（defaultValueMap、entity.AllSetting、CheckValid、getter、models.js）。
- 所有自动写入都是比较后写入；管理员的写入优先。
- 不提交、不发版、不碰生产机（未授权）。测试里只用 RFC 5737 地址。
- 前端改动必须渲染验收；模板不得自闭合 antd 组件。

## Review Focus

1. 通行证生效期间观测中断：既不能续期，也不能因「家里静默」而改地区。
2. 改地区与管理员同时编辑：比较后写入，管理员赢。
3. dat 代码按内容寻址后，进程重启 / 校验仍能在文件里找到运行中配置的代码。
4. 降级已生效状态与配置一致时不能每 10 秒置一次重启标志。
5. 存量入站（有地区、AutoTravel=false）在打开总开关后的第一轮被接管。

---

### Task 1: 自动出差放行的防共享门槛

**Files:** `web/service/auto_travel.go`、`web/service/sharing.go`、新 `web/service/inbound_auth.go`；测试 `auto_travel_test.go`、`inbound_auth_test.go`

- 纯函数新输入：`SuspectProvinces map[string]bool`（本人在用时被拦过的省）、`ObservationStale bool`、`AuthLess bool`。
- 规则：`TestAutoTravelDeniesProvinceBlockedWhileHomeActive`、`TestAutoTravelDeniesProvinceAfterShortHomeEndedPasses`、
  `TestAutoTravelPausesThirtyDaysAfterRepeatedShortPasses`、`TestAutoTravelPausesThirtyDaysAfterSecondCoexist`、
  `TestAutoTravelSkipsAuthLessInbound`、`TestAutoTravelHoldsWhenObservationStale`（含续期）、`TestCooldownZeroDisablesSharingGates`。
- `inboundRequiresAuth(in)`：`TestInboundRequiresAuth`（socks noauth / http 无账号 / dokodemo / ss none、plain → false）。
- 观测心跳：`SharingService.Sample` 成功路径写 `sharingObservedAt`；`TestSharingSampleHeartbeatOnlyOnSuccess`。
- `blockedWhileHome(inboundId, home, since)`：访问日志按 IP×小时聚合，复用 `classifyBlocked` 判据；`TestSuspectProvincesFromBlockedWhileHome`。
- U2：`TestRecentGeoBlockedSkipsStoredProvince`。

### Task 2: 设置项与校验

**Files:** `web/service/setting.go`、`web/entity/entity.go`、`web/assets/js/model/models.js`、`web/html/xui/setting.html`

- `autoTravelRelocateDays`（默认 5，0~7）；`GetAutoTravelParams` 带上 `RelocateDays`。
- CheckValid：总开关开而访问日志关 → 拒绝；`RelocateDays > MonthlyDays`（RelocateDays>0）→ 拒绝。
  `TestCheckValidAutoRegionNeedsAccessLog`、`TestCheckValidRelocateWithinMonthlyBudget`。
- 设置页：打开总开关时自动打开访问日志；新项与说明文字。

### Task 3: 长住自动改地区

**Files:** `web/service/auto_travel.go`、`web/service/auto_region.go`、`database/model/auto_region.go`、`database/model/region_pass.go`

- `decideRelocation(in relocateInput) (province string, reason string)` 纯函数：
  `TestRelocationAfterHomeSilentAndStableAway`、`TestRelocationNeedsFreshObservationAndLowRisk`、`TestRelocationOff`。
- `relocate(in, pass, province, now)`：事务内比较后写入地区、结束通行证（`relocated`）、状态行 applied + PrevRegions。
  `TestRelocateRespectsConcurrentAdminEdit`、`TestRunRelocatesWhenMasterSwitchOn`、`TestRunDoesNotRelocateWhenMasterSwitchOff`。
- `Undo` 对改过的地区恢复 PrevRegions：`TestUndoRelocationRestoresPreviousRegions`。
- 过期页面防护：`TestUpdateInboundRejectsStaleWriteBackOfPreviousRegions`。

### Task 4: 自动设置不再待确认

**Files:** `web/service/auto_region.go`、`web/controller/inbound.go`、`web/html/xui/inbounds.html`、`web/assets/js/util/inbound-attention.js`、`web/service/inbound_attention.go`

- `dominantProvince(rows, suggested)`：`TestDominantProvince`。
- `decideAutoRegion` 改写：`TestAutoRegionAppliesDominantProvinceOnly`、`TestAutoRegionWaitsForThreeDaysWhenMultiProvince`、
  `TestAutoRegionUnplaceableTrafficGate`、`TestAutoRegionAppliesDespiteUnplaceableWhenSharing`、`TestAutoRegionWaitsWhenAccessLogOff`；
  原有 pending 用例改为对应的自动结论。
- 接管存量：`TestRunAdoptsExistingRegionInbounds`、`TestAdoptionRespectsLaterAdminChoice`、`TestAdoptionSkipsAuthLess`。
- 删除 pending：Run 开头清理；Confirm/Dismiss（服务、路由、前端）删除；`writeRegions` 对无认证入站不开自动出差。

### Task 5: 多省缺一个（R1-M7）

**Files:** `web/service/geo.go`、`geo_degrade.go`；`TestGeoPlanDetectsMissingProvinceInSet`（有 lastGood → stale；无 → partial）。

### Task 6: 降级状态如实 + 证据匹配器（R1-I2、R1-M5）

**Files:** `web/service/geo_degrade.go`、`routing_inject.go`、`xray.go`、`routing_geo_evidence.go`

- `TestGeoDegradedReportsAppliedNotGenerated`、`TestGeoGenerationDifferentFromAppliedRequestsRestart`、
  `TestGeoEqualConfigCommitsWithoutLoop`、`TestGeoBlockMatcherBlocksClosedInbound`。

### Task 7: dat 代码按内容寻址（R1-C1）

**Files:** `web/service/geo.go`、`geo_degrade.go`、`routing_inject.go`、`routing_geo_evidence.go`

- `TestGeoDatCodeChangesWithContent`、`TestXrayIPSetCacheCannotServeStaleContent`（进程内用 xray 的匹配器复现缓存问题）、
  `TestGeoDatKeepsCodesOfRunningConfig`、`TestLastGoodSeedsFromContentAddressedDat`；既有 geo 测试按新格式更新。

### Task 8: 新入站清掉同 id 残留（R4-M4）

**Files:** `web/service/inbound.go`；`TestAddInboundClearsLeftoversOfReusedId`。

### Task 9: 前端与渲染验收

设置页、入站列表（撤销弹窗、状态标签）、「需要留意」新模式、表单提示。`TestAllTemplatesParse`、`TestVueDirectivesLiveInsideAVueRoot`、
`TestNoSelfClosingAntdComponents`；浏览器实测。

### Task 10: 文档、模拟、门禁

CLAUDE.md 对应段落；隔离副本重跑场景模拟（前后对比）；`make verify`；`-race` 六个包。

### Final

新鲜审查者（最强模型）审本期与第一期全部未提交改动 → 修 Critical/Important（各自 RED→GREEN）→ 报告。
