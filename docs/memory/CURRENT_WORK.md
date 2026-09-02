# 当前工作状态

最后更新：2026-09-02
分支：`feature/domain-routing`（基于 `origin/main` 的 `c4c29b0`）

## 任务目标

给 AetherUI 面板新增「域名分流管理」：让管理员配置**哪个用户**访问**哪批域名**时走**哪个落地节点**，或直接黑洞掉。

原始需求场景：一批 ChatGPT 域名，用户甲访问时走 B 节点，用户乙访问**同一批**域名时走 C 节点；另有一批违规域名对所有人封禁。

约束来源文档（改动前必读）：

- 设计文档：`docs/superpowers/specs/2026-09-02-domain-routing-design.md`
- 实现计划：`docs/superpowers/plans/2026-09-02-domain-routing.md`（12 个任务）

## 已完成工作

**12 个任务中的 1-11 已全部完成并通过独立审查**，共 18 个功能提交。

| 任务 | 内容 | 提交 |
|---|---|---|
| 1 | 移植 3x-ui 的 `util/link` 解析包，`go.mod` 提至 1.21 | `fb502ef` |
| 2 | 新增 socks 链接解析 | `93a01d0` `d77b117` |
| 3 | 三张表与迁移 | `1d2932e` |
| 4 | 域名组服务与域名语法校验 | `bc20c30` |
| 5 | 出站节点服务（分享链接 / JSON 两种录入） | `fb42b34` `7f60980` |
| 6 | 分流规则服务与引用完整性检查 | `e25cacb` `7d91a57` |
| 7 | **配置注入器（核心）** | `25006a9` `5edc396` |
| 8 | 13 个 HTTP 接口 + 侧边栏入口 | `db66a73` |
| 9 | 分流管理页面 | `0afa7ec` `7bf1fd9` |
| 10 | 保存前真实 xray 校验（fail open） | `c279354` `6e5ade7` |
| 11 | 链接解析预览 + sniffing 警告 | `b4854ca` `1d3c7ca` |

分支还包含 4 个前置提交（`c4c29b0..a4ed9e3`）：删除 README（仓库所有者确认）、更新 geo 数据、新增 CLAUDE.md 与两份文档、修正计划的 git/xray 前提。

## 关键文件

新增：

```
util/link/                       分享链接解析（移植自 3x-ui + 自写 socks.go）
database/model/routing.go        三个模型与常量
web/service/routing_domain.go    域名组服务
web/service/routing_outbound.go  出站节点服务
web/service/routing_rule.go      分流规则服务 + 引用完整性检查
web/service/routing_inject.go    配置注入器（本功能最核心的文件）
web/service/routing_validate.go  落库前的真实 xray 校验
web/controller/routing.go        13 个 HTTP 接口
web/html/xui/routing.html        页面
web/assets/js/model/routing.js   前端数据类
```

修改：`database/db.go`（+3 个 AutoMigrate）、`web/service/xray.go`（`GetXrayConfig` 末尾调注入器）、`web/controller/xui.go`、`web/html/xui/common_sider.html`、`go.mod`/`go.sum`。

## 已确认的设计决策

这些是**经过实测或明确裁决**的结论，不要在新会话里重新推翻：

1. **「用户」等价于「一个入站」**——前端每协议只编辑 `settings.<proto>es[0]`，故分流按 `inboundTag` 匹配，不引入 email 维度。
2. **规则存 `InboundId` 外键而非 tag 字符串**——入站 tag 由端口算出，改端口 tag 就变。
3. **`InboundId = 0` = 全局规则**，生成时不输出 `inboundTag`。
4. **出站 `Tag` 不可变、不由自增 Id 生成**（unique 约束要求 INSERT 前确定）。
5. **落地节点用分享链接录入**，解析器移植自 3x-ui 而非自写（零依赖、自带测试、GPL-3.0 兼容）。
6. **两道防线**：删除时拒绝 + 生成期跳过残缺规则。入站没有引用守卫，全靠第二道兜。
7. **四条注入不变量**：append 到末尾 / block 先于 proxy / 绝不输出残缺规则 / 逐字节确定。
8. **`a-ui-block` 黑洞出站始终自行注入**，不复用模板里的 `blocked`。
9. **校验器 fail open**——只有 xray 明确判定非法才拒绝，自身故障一律放行。
10. **`Config.Equals` 不需要改**——RawMessage 按字节比较，天然感知变化。

以上细节已固化进 `CLAUDE.md` 的「域名分流管理」章节。

## 当前测试状态

`go build ./...` / `go vet ./...` **干净**；`go test ./...` **全绿**。

```
util/link     33 个测试（含移植包自带的 + 自写的 6 个 socks 测试）
database       2 个测试
web/service   37 个测试
合计          72 个
```

本项目在此分支之前**零测试**，以上均为本次新增，属正式回归测试，不得删除。

`web/service` 的校验测试依赖真实 `bin/xray-darwin-arm64`（当前为 Xray 26.7.28，已就位）。若二进制缺失，这些测试会 SKIP 而非失败。

## 尚未解决 / 未完成

**1. Task 12（端到端验证）完全未做。** 这是计划的最后一个任务，含 9 个验证步骤，需要面板真实运行。前置条件：

- 用户在 06:05 启动的面板实例（PID 65932）跑的是**旧代码**，且占用 54321 端口。
- 需要用户重启面板（`Ctrl+C` 后 `XUI_DEBUG=true go run main.go`），或经授权后由 Claude 重启。
- 未经用户明确同意，**不要杀掉那个进程**。

Task 12 的完整步骤见计划文件的「Task 12」章节，要点：造两个入站两个节点两个域名组三条规则 → 核对生成的 `bin/config.json` → 验证引用完整性拦截 → 验证非法输入被拒 → 验证改端口后规则不失效 → **观察 2 分钟确认 xray 没有被反复重启**（确定性验证）。

**2. 整支最终审查已派发但结果未收到。** 用最强模型审 `a4ed9e3..1d3c7ca`（18 提交 / 27 文件 / 4316 行），审查包在 `.superpowers/sdd/2026-09-02-domain-routing/review-a4ed9e3..1d3c7ca.diff`。会话结束时它仍在后台运行，**结论未知**。新会话需要重新派发或手工完成这次审查。

**3. 20 条延后的 Minor 待分诊。** 全部记录在 ledger（见下），逐条注明了任务来源与理由。合并前应决定哪些必须修。其中比较值得关注的几条：

- `allocTag` 是 check-then-insert，并发新增靠 DB unique 兜底，错误信息不友好
- `CheckXxxRefs` 与 delete 之间无事务，理论上有 TOCTOU 窗口（单管理员面板风险低）
- `validate` 把 `Get` 的任何错误都当作「不存在」，瞬时 DB 错误会被误报
- `updateOutbound`/`updateRule` 的请求体 `id` 会覆盖 URL 的 `id`——**既有约定**，`inbound.go` 同样如此，宜作跨 controller 统一加固
- `buildRule` 的 `default` 分支（未知 action）未测
- `lastMeaningfulLine` 在输出全为横幅时返回空串，错误提示会没有原因

**4. 一个已知的测试 fixture 偏差。** `web/service/routing_inject_test.go` 的 `newTestInbound` 用 `Settings: "{}"` 造 VLESS 入站，**真实 xray 会拒绝**（缺 `decryption`）。单元测试不跑 xray 所以察觉不到。不影响注入逻辑的正确性（入站不归它管），但说明单元测试的 fixture 与真实配置存在偏差——这正是 Task 12 不能省的原因。

## 继续工作时必须注意

- **不要 push，不要动 `main` 分支，不要改写已有历史。** `main` 当前严格等于 `origin/main`，18 个功能提交只在本地。远程是 `github.com/SienFeng/AetherUI`。
- **不要擅自杀死用户的面板进程**，也不要占用 54321 端口。
- 本机 `go` 可能不在默认 PATH：`export PATH="/opt/homebrew/bin:$PATH"`（Go 1.27.1）。
- 改完 `web/html/**` 后 `go build` 查不出模板语法错误，需自行 `ParseFS` 验证（原因见 CLAUDE.md）。
- 改完 `web/assets/js/**` 后浏览器要硬刷新（Cmd+Shift+R），因为静态资源被无条件加了一年强缓存。
- 面板里点「安装 xray」会覆盖仓库跟踪的 `bin/geo*.dat`，当前这份含 OPENAI 类别，别还原成旧版。
- 本次流程的完整 ledger（含每一条裁决与其代价评估）在 `.superpowers/sdd/2026-09-02-domain-routing/progress.md`，该目录已被 `.gitignore` 排除，不会进入提交。

## Git / 工作区状态

```
分支      feature/domain-routing
HEAD      1d3c7ca
基线      c4c29b0 (= origin/main，未被改动)
提交数    22（4 前置 + 18 功能）
工作区    干净，无未提交改动，无未跟踪文件
远程      origin = github.com/SienFeng/AetherUI（从未 push）
```
