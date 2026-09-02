# 当前工作状态

最后更新：2026-09-02（最终审查发现的 7 条 finding 修复完成后）
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

## 最终审查的 7 条 finding：已全部修复

一轮修复处理全部 findings，工作区尚未提交。修复过程中变异测试又暴露出**一个审查未发现的真实缺陷**（见下方 C1-b）。

| 编号 | 修复内容 | 落点 |
|---|---|---|
| C1 | 保留 tag 判定收敛为 `model.IsReservedTag()`，三个消费点共用 | `database/model/routing.go`、`allocTag`、`buildOutbounds`、`removeOutboundByTag` |
| I1 | 校验改为**完整生成配置**（`validateWithFullConfig`），新增 `ValidateOutboundReplacing` 供编辑路径 | `web/service/routing_validate.go` |
| I2 | 新增 `CheckInboundRefs`，三条引用边守卫对称 | `routing_rule.go`、`InboundService.DelInbound` |
| I3 | 两处 nil map 守卫（`"null"` 可通过 Unmarshal 却留下 nil map） | `OutboundNodeService.Update`、`buildOutbounds` |
| I4 | `buildRule` 第三个返回值回报跳过原因，`buildRules` 记 `logger.Warning` | `routing_inject.go` |
| I5 | 出站节点行补编辑图标，弹窗区分新增/编辑，tag 只读回显 | `web/html/xui/routing.html` |
| I6 | 三处删除均加 `this.$confirm`，沿用 `inbounds.html` 的既有写法 | 同上 |

### C1-b（修复过程中新发现，已修）：校验早于 tag 分配，完整配置校验看不见真正的 tag

`persist` 原本**先 `ValidateOutbound(ob)`、后 `allocTag`**，校验时 `ob["tag"]` 还是解析器给的旧值。这意味着 I1 的完整配置校验在**新增路径上根本看不到真正会写进配置的 tag**——组合冲突全部漏检，I1「修好 C1 隐形根因」的说法只成立一半。

发现方式：撤掉 C1 两处排除做变异测试时，日志里出现 `config was already invalid before this change, allowing the save: ... existing tag found: a-ui-block`，顺藤摸到调用顺序。

已把顺序改为「先 `allocTag` 定下 tag → 写入 `ob["tag"]` → 再校验」。`allocTag` 只读不写，前置无副作用。

对应回归测试 `TestAddRejectsTagCollidingWithATemplateOutbound` 复刻的是另一个真实场景：用户在 xray 模板里手写了 tag 为 `a-ui-hk` 的出站（`a-ui-` 前缀只是约定，模板用户可编辑，拦不住），此时新建备注为 `hk` 的节点，`allocTag` 查表发现没人占用就会分配 `a-ui-hk`，与模板撞名导致 xray 拒绝启动。修复前该测试失败，修复后通过。

### 两道防线已验证互相独立

变异测试确认（每项都实跑过）：

| 变异 | 结果 |
|---|---|
| 撤掉 `allocTag` 的保留 tag 排除 | 完整配置校验独立拦下：`xray 校验未通过: ... existing tag found: a-ui-block`，保存被拒 |
| 撤掉 `buildOutbounds` 的保留 tag 排除 | `TestInjectSkipsOutboundNodeCarryingReservedTag` 失败（出现 2 个 `a-ui-block`） |
| 撤掉 `removeOutboundByTag` 的保留 tag 排除 | `TestValidateOutboundReplacingNeverRemovesTheInjectedBlackhole` 失败 |
| 去掉「基线本来就不合法则放行」 | `TestValidateOutboundAllowsWhenBaselineTemplateIsAlreadyInvalid` 失败 |
| `CheckInboundRefs` 把全局规则也算作引用 | `TestDelInboundAllowedWhenOnlyGlobalRuleExists` 失败 |

两道防线现在的分工：`allocTag` 排除给出好的 UX（备注写 block 会拿到 `a-ui-block-2` 而非报错），完整配置校验是安全网。

### fail open 的三条边界（不可收紧成拒绝）

1. xray 自身故障——二进制缺失、老版本不认 `run -test`、执行超时
2. 取不到完整配置（模板损坏、库不可用）——退回最小配置校验，不是放弃校验
3. **改动之前配置就已经不合法**——那不是本次改动的错，拒绝会把管理员锁在门外，连修复用的操作都做不了

### 已更新的文档

`CLAUDE.md` 中三处描述因本次修复而失效，已同步改写：保留 tag 段（改为描述 `IsReservedTag` 的三个消费点）、两道防线段（第一道防线现在是三条边而非两条）、输入侧校验段（最小配置 → 完整配置，并补上 fail open 的三条边界）。同时消除了改写后产生的两处自相矛盾。

### 新发现但未处理（不阻塞，留作跟进）

**出站节点的「启用」开关不过校验。** `toggleNode` 发送 `config: ''`，`Update` 因此跳过校验分支。若节点 A 处于禁用状态、用户随后在模板里手写了与 A 同 tag 的出站，再启用 A 就会撞名，而新增路径已能拦住同样的冲突——两条路径不对称。触发需要用户在模板里违反 `a-ui-` 命名空间约定，可能性低，故按「发现的额外问题可以报告，但不自行处理」记录在此。

## 用户反馈修复（分流页按钮失灵 + 切页动画）

用户在真实面板上发现分流页所有「添加 / 编辑」按钮点了没反应，并指出侧栏切页动画生硬。

### 按钮失灵：根因是 Vue 指令写在根元素之外

三个 `<a-modal>` 整块落在 `<a-layout id="app">` 之后。Vue 2 只编译 `el` 指向的子树，所以它们从未被编译：点按钮把 `groupModal.visible` 置为 true，但没有任何东西绑定这个数据，界面毫无反应，控制台也不报错。

排查手段：浏览器扩展未连接，改用本机 Chrome 无头模式 + 项目自带的 Vue/antd 资源 + 桩接口搭隔离环境（端口 87xx，未触碰用户运行中的 54321）。实测证据：

```
raw_uncompiled_a-modal=3     三个 a-modal 原样躺在 DOM 里，从未被编译
compiled_ant-modal_nodes=0   点击后依然是 0
after_click_visible_flag=true 数据确实变了，只是没人渲染
```

修复：把三个弹窗移进 `#app`，并加注释说明为什么不能挪出去。

**删除图标其实是好的。** 同一套环境实测 `A_icon_click_fired_request=true`，请求确实发出。用户看到「没反应」是因为那几个域名组 / 出站节点正被分流规则引用，被第一道引用守卫拒绝了，只弹了一条错误提示（实测提示能正常显示：「该域名组仍被 1 条分流规则引用，请先删除这些规则」）。这是设计内行为，未改动。

### 新增回归守卫 `web/html_test.go`

两个测试，都守 `go build` 查不出的东西：

- `TestAllTemplatesParse`——`getHtmlTemplate` 把 `ParseFS` 错误 `// ignore` 掉了，这个测试不忽略。
- `TestVueDirectivesLiveInsideAVueRoot`——解析每个顶层页面的渲染结果，断言带 `v-*` / `@*` / `:*` 的元素都落在某个 `new Vue({el:'#...'})` 的根元素内。修复前只有 `routing.html` 失败（22 处），另外 4 个页面通过，说明检查不过宽。

代价：`golang.org/x/net` 从 indirect 提为 direct（同版本，go.sum 已有，无新增下载）。用它做 HTML 解析而非手写字符串匹配，是因为这条不变量要对所有页面成立。

### 切页动画

菜单点击是 `location.href = key` 整页跳转，不是前端路由。所以「生硬」由两段构成：跳转间隙的白闪，加上 `[v-cloak]{display:none}` 解除后整个界面瞬间出现。改 `web/assets/css/custom.css`：

- `html, body` 底色设为布局灰，消除白闪。**没有**去画侧栏那条深色区域——侧栏宽度随折叠状态变化，画死了折叠后会露出对不齐的深色带。
- `#app` 淡入 0.22s，`#content-layout` 再多 6px 位移、延迟 0.04s，读起来是浮现而非弹出。
- 菜单项补背景色与选中条的过渡（antd 只给了 color）。
- `prefers-reduced-motion: reduce` 下全部关闭，已实测（`--force-prefers-reduced-motion` 下 `animation=none`）。

**这次改了 `web/assets/css/`**，浏览器对它是 `max-age=31536000` 强缓存、缓存键为 `config/version`（当前 `0.3.2`，未变），所以必须硬刷新（Cmd+Shift+R）才能看到效果。

## 当前测试状态

`go build ./...` / `go vet ./...` 干净；`go test ./...` 全绿。

```
util/link     33 个测试
database       2 个测试
web            2 个测试（模板解析 / Vue 根元素不变量）
web/service   51 个测试
合计          88 个（0 失败，0 跳过）
```

本项目在此分支之前零测试。上述均为本次新增的正式回归测试，不得删除。

**此前的覆盖缺口已补上**：`web/service/routing_e2e_test.go` 把 `GetXrayConfig()` 的产物 `json.Marshal` 后交给真实 xray 判定，三个场景——完整目标形态（2 入站 / 2 域名组 / 2 出站 / 全局 block + 按用户 proxy）、脏数据下仍产出合法配置、以及直接复刻事故路径的「备注写 block」。二进制缺失时 `requireXrayBinary` 会 skip。

该文件里的 `newE2EInbound` 给出真实 xray 接受的入站配置（VLESS 必须有 `decryption:"none"`，实测 `Settings: "{}"` 会被拒）。`routing_inject_test.go` 里原有的 `newTestInbound` 保持不变——它只用于不接触真实 xray 的注入器单元测试。

`assertRealXrayAccepts` **刻意不做 fail open**：`runXrayTest` 的 fail open 是给生产路径用的，测试里拿不到判定必须失败，否则这条覆盖会静默失效。

## 下一步

1. **对本轮修复做一次 scoped 复审**（按流程：一轮修复 → 一次复审）。复审范围是尚未提交的工作区改动，重点看 I1 改成完整配置校验后的行为边界，以及 C1-b 那个调用顺序问题是否还有同类。
2. 复审通过后提交。本轮改动尚未 commit。
3. 可选跟进：上面「新发现但未处理」的启用开关不过校验。

### 本轮修复的验证证据

全部在本机实跑，非推断：

| 检查 | 命令 | 结果 |
|---|---|---|
| 构建 | `go build ./...` | exit 0 |
| 发布构建 | `CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o ... main.go` | exit 0（gopsutil 的 cgo deprecation warning 为既有） |
| 静态检查 | `go vet ./...` | exit 0 |
| 格式 | `gofmt -l <本次改动的 .go>` | 无输出 |
| 测试 | `go test ./... -count=1` | 86 PASS / 0 FAIL / 0 SKIP |
| 模板可解析 | 自写 `ParseFS` 遍历（`web.go` 的 `getHtmlTemplate` 吞错误，`go build` 查不出） | 62 个具名模板全部解析通过 |
| 页面渲染 | 离线 `ExecuteTemplate("routing.html")` | 零个未渲染的 `{{`，I5/I6 的 7 处改动全部命中 |
| JS 语法 | `node --check` | 内联 script 与 `model/routing.js` 均通过 |

未做的验证及原因：

- **浏览器交互未验证**——扩展未连接，按用户指示不动运行中的面板，改用上面的模板渲染 + JS 语法检查做 HTTP/模板层等价验证。编辑弹窗的实际交互（点开编辑图标能否正确回填、保存后列表是否刷新）**未经真人点击确认**。
- **未重启面板、未清理库里 Task 12 的验证数据**——按用户指示保持现状。
- 本次没有改 `web/assets/js|css`，因此不涉及 `cur_ver` 强缓存问题。

## 继续工作时必须注意

- **不要 push，不要动 `main` 分支，不要改写已有历史。** `main` 严格等于 `origin/main`，全部提交只在本地。远程 `github.com/SienFeng/AetherUI`。
- **面板当前正在运行**（重启后的实例，占用 54321）。不要擅自杀死用户的进程。
- **库里留有 Task 12 的验证数据**：2 个入站（端口 10011 / 10002）、2 个域名组（ChatGPT / 违规域名）、3 个出站节点、3 条规则。若要干净环境需先清理。
- 本机 `go` 可能不在默认 PATH：`export PATH="/opt/homebrew/bin:$PATH"`（Go 1.27.1）。
- 排查「面板说正常但用不了」时，**不要相信面板首页的 xray 状态**，以 `pgrep` + `xray run -test -c bin/config.json` 为准（原因见 CLAUDE.md）。
- 改完 `web/html/**` 后 `go build` 查不出模板语法错误，需自行 `ParseFS` 验证。
- 改完 `web/assets/js/**` 后浏览器要硬刷新（Cmd+Shift+R）。
- 浏览器扩展当前未连接，界面交互只能用 HTTP / 模板层等价验证。验证模板改动的两个手法（自写 `ParseFS` 遍历、离线 `ExecuteTemplate` 后查未渲染的 `{{`）见「本轮修复的验证证据」。
- **`gofmt -l $VAR` 在 zsh 下不会分词**（zsh 默认不对未加引号的参数展开做 word splitting），会把整串当成一个文件名。要么写 `${=VAR}`，要么直接列出文件。
- `web/service/inbound.go`、`panel.go`、`server.go`、`setting.go`、`user.go`、`xray.go` **在本分支之前就未通过 `gofmt`**（import 分组问题）。不要顺手格式化，那属于无关改动。
- 本次流程的完整 ledger（含每条裁决与代价评估）在 `.superpowers/sdd/2026-09-02-domain-routing/progress.md`，该目录已被 `.gitignore` 排除。

## Git / 工作区状态

```
分支      feature/domain-routing
HEAD      5a2c395（文档提交；最后一个功能提交是 1d3c7ca）
基线      c4c29b0 (= origin/main，未被改动)
提交数    24（4 前置 + 18 功能 + 2 文档）
工作区    有未提交改动 —— 本轮 7 条 finding 的修复，尚未 commit
远程      origin = github.com/SienFeng/AetherUI（从未 push）
```

本轮改动涉及的文件（`git status`）：

```
M  CLAUDE.md                             三处失效描述同步改写
M  database/model/routing.go             + IsReservedTag
M  web/html/xui/routing.html             I5 编辑入口 + I6 三处确认框
M  web/service/inbound.go                DelInbound 接上 CheckInboundRefs
M  web/service/routing_inject.go         C1 生成端排除、I3 nil map、I4 跳过原因
M  web/service/routing_outbound.go       C1 分配端排除、I3 nil map、C1-b 校验顺序
M  web/service/routing_rule.go           + CheckInboundRefs
M  web/service/routing_validate.go       I1 完整配置校验
M  web/service/routing_*_test.go         新增回归测试
?? web/service/routing_e2e_test.go       新增：完整生成配置交真实 xray
```

共 14 个新增测试。本次没有改 `web/assets/**`，无需处理 `cur_ver` 强缓存。
