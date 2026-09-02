# 当前工作状态

最后更新：2026-09-02（Task 12 与最终审查完成后）
分支：`feature/domain-routing`（基于 `origin/main` 的 `c4c29b0`）

## 任务目标

给 AetherUI 面板新增「域名分流管理」：让管理员配置**哪个用户**访问**哪批域名**时走**哪个落地节点**，或直接黑洞掉。

原始需求场景：一批 ChatGPT 域名，用户甲访问时走 B 节点，用户乙访问**同一批**域名时走 C 节点；另有一批违规域名对所有人封禁。

约束来源文档（改动前必读）：

- 设计文档：`docs/superpowers/specs/2026-09-02-domain-routing-design.md`
- 实现计划：`docs/superpowers/plans/2026-09-02-domain-routing.md`

## 已完成工作

**计划的 12 个任务全部完成**，共 18 个功能提交，每个任务都经过独立审查（9 个任务各修复 1 轮）。

| 任务 | 内容 | 提交 |
|---|---|---|
| 1 | 移植 3x-ui 的 `util/link` 解析包，`go.mod` 提至 1.21 | `fb502ef` |
| 2 | 新增 socks 链接解析 | `93a01d0` `d77b117` |
| 3 | 三张表与迁移 | `1d2932e` |
| 4 | 域名组服务与域名语法校验 | `bc20c30` |
| 5 | 出站节点服务（分享链接 / JSON 两种录入） | `fb42b34` `7f60980` |
| 6 | 分流规则服务与引用完整性检查 | `e25cacb` `7d91a57` |
| 7 | 配置注入器（核心） | `25006a9` `5edc396` |
| 8 | 13 个 HTTP 接口 + 侧边栏入口 | `db66a73` |
| 9 | 分流管理页面 | `0afa7ec` `7bf1fd9` |
| 10 | 保存前真实 xray 校验（fail open） | `c279354` `6e5ade7` |
| 11 | 链接解析预览 + sniffing 警告 | `b4854ca` `1d3c7ca` |
| 12 | 端到端验证（纯验证，无代码改动） | — |

另有 4 个前置提交（`c4c29b0..a4ed9e3`）与 1 个文档提交（`7d65307`）。

### Task 12 端到端验证结果：全部通过

在真实运行的面板上、通过真实 HTTP API 执行，非单元测试：

| 步骤 | 结果 |
|---|---|
| 1 自动化检查 | build / vet / test 全绿，72 个测试 |
| 2 造数据 | 2 入站 / 2 域名组 / 3 出站 / 3 规则，全部经真实 API |
| 3 xray 校验生成配置 | `Configuration OK` |
| 4 四条不变量 | 全部满足 |
| 5 引用完整性拦截 | 两类删除均被拒，错误信息准确，零副作用 |
| 6 非法输入拒绝 | 四种非法输入全被拦，零脏数据 |
| 7 改端口规则跟随 | 自动跟随到 `inbound-10011`，旧 tag 消失 |
| 8 **生成确定性** | **130 秒 / 13 个 cron 周期，零重启零重写** |

补充验证（浏览器扩展未连接，改用 HTTP 层等价验证）：

- 页面 HTML 正常渲染，**零个未渲染的 `{{`**（证明没有 Vue 绑定被误写成 Go 模板语法）
- Task 9 / 11 的三处修复项都在生效（DOCTYPE、`HttpUtil.post`、placeholder）
- 解析预览接口对 socks（自写）与 vless（移植）均正确，不支持的 scheme 正确报错

**验证中的关键确认**：block 规则的 `priority` 设为 99（最大）、proxy 设为 0，block 仍排在前面——证明顺序来自 block/proxy 分组机制而非 priority 排序，符合设计。

## 尚未解决：最终审查发现 1 个 Critical + 6 个 Important

整支最终审查（`a4ed9e3..1d3c7ca`）已完成。**功能主链路可用，但有一个特定输入触发的严重缺陷。**

### C1（Critical，已由控制方独立复现）：保留 tag 冲突导致全员断网

管理员把出站节点备注命名为「block」（或 `Block`/`BLOCK`/`block!`/` block `，`SlugRemark` 会归一到同一 slug），`allocTag` 会生成 `a-ui-block`——与注入器的黑洞出站撞名。`allocTag` 只查 `outbound_nodes` 表，而注入器发出的 tag 不在表里，所以放行。

实测复现结果：

```
生成的 outbounds tags: [None,'blocked','a-ui-b-节点','a-ui-c-节点','a-ui-东京-vless','a-ui-block','a-ui-block']
a-ui-block 出现次数  : 2
xray -test          : Failed to start: existing tag found: a-ui-block
xray 子进程          : 已死（全员断网）
面板报告的状态        : state=running, errorMsg=''   <- 面板完全不知道
```

**面板状态显示错误这一点是控制方实测发现、审查报告未提及的**，它让「不会自愈」升级为「不会自愈且看不见」。根因：`Process.Start()` 把 `cmd.Run()` 丢进 goroutine 后返回 nil，启动失败不回传。

不会自愈：`CheckXrayRunningJob` 每 30 秒重试，每次都撞同一个冲突。因 `Tag` 设计上不可变，唯一补救是禁用或删除该节点。删除后 20 秒内自动恢复（已验证）。

### I1（Important）：校验只包住单个对象，看不见组合冲突

设计 §5.4.2 要求校验**完整生成配置**，但计划的 Task 10 实现成「最小配置包住单个 outbound / 域名列表」。这正是 C1 隐形的原因——孤立校验在原理上发现不了重复 tag。审查实测 `xray run -test`（含加载 10MB geosite）仅约 **18ms**，成本理由不成立。

### I2（Important）：SQLite id 复用导致规则静默错绑

GORM 的 sqlite 驱动对自增主键生成 rowid 别名而非 `AUTOINCREMENT`，id 会被复用（审查已实测）。删除用户 A 的入站后新建用户 C 的入站可能拿到同一 id，「A 的 ChatGPT 走 B 节点」这条孤儿规则会**静默重绑到 C 身上**，规则列表还会渲染得很合理。生成期跳过防线拦不住——引用不再悬空，只是指错了人。建议补 `CheckInboundRefs` 使三条引用边对称。

### I3（Important）：`config: "null"` 触发 nil map panic

`json.Unmarshal([]byte("null"), &map)` 不报错但留下 nil map，下一行赋值 panic。走 API 是 500（gin 有 Recovery），**走 cron 会杀掉整个面板进程**（cron 未配 Recover）。两处：`OutboundNodeService.Update`、`RoutingInjector.buildOutbounds`。

### I4（Important）：六处 rule-skip 全部静默

设计 §5.3 接受第二道防线的理由是「宁可规则不生效，用户能察觉」，但 `buildRule` 的六个 `return nil` 无一记日志。禁用一个出站节点，引用它的规则会从生成配置里消失，而规则表照常渲染。用户实际察觉不到。

### I5 / I6（Important，纯 UI）

- I5：出站节点的「编辑」在界面上不可达（只有删除图标），设计 §7 明确要求「列表 + 增删改」。
- I6：三个删除动作均无确认框，与 `inbounds.html` 的既有做法不一致；规则删除不可恢复且无引用保护。

### 审查对工作方法的批评（已接受）

C1、I1、I5、I6 **全部源自计划里逐字提供的成品代码**，实现者严格照做。根因是计划内联了写好的代码，而不是陈述代码必须满足的约束（如「生成的 tag 不得与注入器发出的任何 tag 冲突」）。**后续写任务简报应改为约束优先，而非代码优先**——代码优先的简报让这类问题一直隐形到组合阶段。

### 延后 Minor 的分诊结论

审查对 21 条延后项逐条给了判断，其中三条被判定为**「不是问题」**：`ParseResult.Identity` 在本项目从未被读取；`SlugRemark` 小写化是 slug 的正常行为；`runXrayTest` 的 `json.Marshal` 失败分支不可达。其余多为「可以延后」。完整分诊表见审查报告（会话内产出，未落盘）。

另有一条被推翻的怀疑记录在此以免重复排查：**`parseVless` 输出扁平 `settings`（无 `vnext` 包裹）不是缺陷**。控制方实测两种结构运行时行为一致（xray 正确读出 address/port 并发起连接），仅结构风格与 `parseVmess`/`parseTrojan` 不一致。

## 当前测试状态

`go build ./...` / `go vet ./...` 干净；`go test ./...` 全绿。

```
util/link     33 个测试
database       2 个测试
web/service   37 个测试
合计          72 个
```

本项目在此分支之前零测试。上述均为本次新增的正式回归测试，不得删除。

**已知测试覆盖缺口**：没有任何测试把**完整生成配置**交给真实 xray（这正是 C1 得以通过全部审查的原因）。`newTestInbound` 的 `Settings: "{}"` 真实 xray 会拒绝，需先修好它，才能加这个端到端测试。

## 下一步

按流程应派**一轮**修复（一次性处理全部 findings，而非每条一个 fixer），然后做**一次** scoped 复审。建议优先级：

1. **必须修**：C1（保留 tag 排除，分配端 + 生成端都要）、I3（nil map）、I1（改用完整配置校验——它是 C1 隐形的根因）
2. **应该修**：I2（`CheckInboundRefs`）、I4（skip 记日志）
3. **可跟进**：I5、I6（纯 UI，不影响正确性）

审查还建议加一个端到端测试：`GetXrayConfig()` → `json.Marshal` → `xray run -test`，二进制缺失时 skip。这能把 I1 从「设计偏离」变成「有覆盖的不变量」，并防止 C1 这类组合失败再次发生。

## 继续工作时必须注意

- **不要 push，不要动 `main` 分支，不要改写已有历史。** `main` 严格等于 `origin/main`，全部提交只在本地。远程 `github.com/SienFeng/AetherUI`。
- **面板当前正在运行**（重启后的实例，占用 54321）。不要擅自杀死用户的进程。
- **库里留有 Task 12 的验证数据**：2 个入站（端口 10011 / 10002）、2 个域名组（ChatGPT / 违规域名）、3 个出站节点、3 条规则。若要干净环境需先清理。
- 本机 `go` 可能不在默认 PATH：`export PATH="/opt/homebrew/bin:$PATH"`（Go 1.27.1）。
- 排查「面板说正常但用不了」时，**不要相信面板首页的 xray 状态**，以 `pgrep` + `xray run -test -c bin/config.json` 为准（原因见 CLAUDE.md）。
- 改完 `web/html/**` 后 `go build` 查不出模板语法错误，需自行 `ParseFS` 验证。
- 改完 `web/assets/js/**` 后浏览器要硬刷新（Cmd+Shift+R）。
- 浏览器扩展当前未连接，界面交互只能用 HTTP 层等价验证。
- 本次流程的完整 ledger（含每条裁决与代价评估）在 `.superpowers/sdd/2026-09-02-domain-routing/progress.md`，该目录已被 `.gitignore` 排除。

## Git / 工作区状态

```
分支      feature/domain-routing
HEAD      7d65307（文档提交；最后一个功能提交是 1d3c7ca）
基线      c4c29b0 (= origin/main，未被改动)
提交数    23（4 前置 + 18 功能 + 1 文档）
工作区    干净
远程      origin = github.com/SienFeng/AetherUI（从未 push）
```
