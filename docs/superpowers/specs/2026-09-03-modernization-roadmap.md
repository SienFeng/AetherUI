# AetherUI 现代化改造 总路线图

日期：2026-09-03
状态：待评审

本文档是对 `3x-ui`（`mhsanaei/3x-ui` v3 分支，本地副本 `/Users/caryallen/Desktop/3x-ui-main`）逐包研读后，为 AetherUI 制定的能力补齐路线。它是后续 6 份实施计划共同的 spec：每份计划都从本文档取需求、约束和验收口径。

参考实现的引用一律给到 `3x-ui-main` 内的相对路径与行号，便于对照。**引用不等于照抄**：3x-ui 是 React 19 + AntD 6 的单页前端、多节点架构、Client 表多用户模型，与 AetherUI 的 Vue 2 服务端模板、单机、「一个 inbound 一个用户」模型不兼容。本路线图只取其**后端逻辑与踩坑结论**。

---

## 1. 现状基线

以下为 2026-09-03 在 `main` 分支（`601a344`）实测确认，不是推断：

| 项 | 值 |
|---|---|
| 本地 Go | 1.27.1 darwin/arm64 |
| `go.mod` 声明 | `go 1.21` |
| CI 构建 Go | `1.22`（`.github/workflows/release.yml:44` 硬编码） |
| `CGO_ENABLED=1 go build` | 通过 |
| `go test ./...` | 全部通过（8 个包有测试，19 个包无测试） |
| `bin/` | `xray-darwin-arm64` / `xray-linux-amd64` / `xray-linux-arm64` / geoip / geosite / ipdb 均在位 |
| 源文件规模 | 82 个源文件 / 41 个测试文件（3x-ui：248 / 366） |

`xray-core` 依赖的**唯一使用点**是 `xray/process.go:11` 的 `statsservice "github.com/xtls/xray-core/app/stats/command"`，只用于 `GetTraffic` 的 gRPC stats 查询。

---

## 2. 决定性技术前提（已查证）

这四条决定了计划的排序与边界，每条都经过实际验证。

### 2.1 热更新硬依赖 xray-core 升级

3x-ui 的 `XrayAPI.AddInbound`（`internal/xray/api.go:134-158`）走的是：

```
JSON → conf.InboundDetourConfig → .Build() → typed message → HandlerService.AddInbound
```

也就是**面板进程内必须有一份与运行中核心同版本的 `infra/conf` 解析器**。AetherUI 现在锁的 `xray-core v1.4.2` 是 2021 年的解析器，编译不出 Reality / XHTTP / 现代传输的入站配置。因此配置热更新绕不开把 `xray-core` 升到与 `bin/xray-*`（26.7.28）匹配的版本。

同时确认：`v1.4.2` 的 `app/router/command` 只有 `SubscribeRoutingStats` 与 `TestRoute` 两个 RPC，**没有 `AddRule`**——路由规则的热重载在 v1.4.2 上无论如何做不到。

**决策：升级。** 详见计划 01。

### 2.2 Reality 不依赖任何升级

AetherUI 的 Go 侧把 `settings` / `streamSettings` / `sniffing` 原样存为 JSON 字符串，不解析结构（`database/model/model.go` 的 `Inbound`）。Reality 因此纯粹是**前端建模 + 分享链接生成**的事，落库前的合法性由现有的 `ValidateInboundReplacing`（`web/service/routing_validate.go`，实调 `bin/xray run -test`）兜住。

**推论：计划 04（Reality）不阻塞于计划 01，可并行。**

### 2.3 `//go:linkname` 可以直接消除，而不是"想办法保住"

`util/sys/psutil.go` 用 `//go:linkname` 侵入 `github.com/shirou/gopsutil/internal/common.HostProc`。查证 gopsutil v3.21.3 的实现（`internal/common/common.go:331`）：

```go
func HostProc(combineWith ...string) string {
	return GetEnv("HOST_PROC", "/proc", combineWith...)
}
```

整个函数就是"读 `HOST_PROC` 环境变量，缺省 `/proc`，再 `filepath.Join`"。调用点只有两处（`util/sys/sys_linux.go:43` 和 `:58`，都是拼 `/proc/net/tcp` 这类路径）。

**推论：与其在升级时维护这个 hack，不如用十几行标准库代码原地替换掉，永久消除 CLAUDE.md 里"升级 gopsutil 时会断"这条已知偏差。**

### 2.4 `xray.Config` 是有损结构体

`xray/config.go` 的 `Config` 只定义了 11 个字段（log / routing / dns / inbounds / outbounds / transport / policy / api / stats / reverse / fakeDns）。`GetXrayConfig()` 把模板反序列化进这个结构体再序列化出去，**模板里任何不在这 11 个字段中的顶层键会被静默丢弃**（例如 `observatory`、`burstObservatory`、`metrics`）。

这是既有行为，本路线图不修改它（属于用户可见行为变更）。但计划 01 的热更新必须知道这一点：diff 只需覆盖这 11 个字段，因为别的键根本到不了 `bin/config.json`。

另外注意 `Config.OutboundConfigs` 是**整块 `json_util.RawMessage`**，不像 3x-ui 那样是 `[]OutboundConfig`。出站级别的 diff 需要自己把这块 blob 解成 `[]json.RawMessage` 再按 tag 索引。

---

## 3. 计划划分

6 份计划，每份自成一个可独立上线、可独立验证的交付。**顺序是依赖顺序，不是重要性顺序。**

```
计划 01  依赖与工具链升级 + 配置热更新        ← 阶段一可单独上线
   │
   ├──────────────► 计划 03  分流子系统增强     （需要 RoutingService）
   │
   └──────────────► 计划 06  可观测性、告警与对外接口（部分）

计划 02  安全基线                              （无依赖，可与 01 并行）
计划 04  Reality 与现代入站能力                （无依赖，可与 01 并行）
计划 05  面板运维能力                          （无依赖，可与 01 并行）
```

### 计划 01 — 依赖与工具链升级 + 配置热更新

**覆盖**：第一梯队 #2。

两个阶段：

- **阶段一（可独立上线）**：建立验证门禁 → 消除 `//go:linkname` → 抬升 Go 版本声明与 CI → 升级 `xray-core` 至 26.x 及连带的 grpc/protobuf。交付物是"行为完全不变，但跑在现代工具链上"。
- **阶段二**：接入 `HandlerService` / `RoutingService` gRPC 客户端 → `ComputeHotDiff` → `tryHotApply` 接入 `RestartXray`。交付物是"改分流规则、增删入站不再全员断线"。

**验收**：阶段一，`go test ./...` 与升级前同样全绿，面板实跑、流量统计数字继续增长；阶段二，改一条分流规则后 `pgrep xray` 的 PID 不变，且 `RoutingService.TestRoute` 能验证新规则已生效。

详见 `docs/superpowers/plans/2026-09-03-01-toolchain-and-hot-reload.md`。

### 计划 02 — 安全基线

**覆盖**：第二梯队的密码哈希、登录限速、CSRF、两步验证、cron panic 恢复。

| 项 | 参考实现 | AetherUI 现状 |
|---|---|---|
| bcrypt 密码 + 登录时自动升级旧明文行 | `internal/util/crypto/crypto.go` | `database/model/model.go` 的 `User.Password` 明文；`web/service/user.go:27` 直接 `where username = ? and password = ?` |
| 登录失败限速 | `internal/web/controller/login_limiter.go` | 无 |
| CSRF | `internal/web/session/csrf.go` + `middleware.CSRFMiddleware` | 无 |
| 两步验证 TOTP | `twoFactorEnable` / `twoFactorToken` 设置项 | 无 |
| cron panic 恢复 | `internal/web/web.go:514` 的 `cron.WithChain(cron.Recover(...))` | `web/web.go:344` 裸 `cron.New(...)` |

三个必须写进计划的约束：

1. **登录失败日志目前打印明文用户名和密码**（`web/controller/index.go`）。这条要和 bcrypt 一起改掉，否则哈希了存储却仍在日志里泄露。
2. `web/session/session.go:22` 把整个 `model.User`（含密码字段）gob 进 session。改成哈希后 session 里存的是哈希，仍不该存——应只存 id 与用户名。
3. 限速器的 key 必须包含用户名且**带记录数上限**（3x-ui 用 10000），否则攻击者轮换用户名就能把 map 撑爆。

### 计划 03 — 分流子系统增强

**覆盖**：第一梯队 #1（路由测试）、#3（出站连通性探测）、#4（geodata 浏览器）、#5（出站订阅）。

| 项 | 参考实现 |
|---|---|
| 路由测试 | `internal/xray/api.go:343`（`TestRoute`）+ `internal/web/controller/xray_setting.go:406` |
| geodata 浏览器 + token 校验 | `internal/xray/geodata/`（流式读 protobuf wire format，不解码成 Go struct）+ `internal/web/controller/xray_setting.go:61-64` |
| 出站连通性探测 | `internal/web/service/outbound/probe_http.go`（一批只起一个临时 xray 实例，冷/热两次请求分离握手耗时与稳态 RTT） |
| 出站订阅 | `internal/web/service/outbound_subscription.go` + `internal/web/job/outbound_subscription_job.go` |

三个必须写进计划的约束：

1. **`web/service/config.json:3` 的 `api.services` 只有 `HandlerService` / `LoggerService` / `StatsService`，没有 `RoutingService`。** 路由测试依赖它。存量部署的模板已落库到 `settings` 表，需要一次幂等迁移（补 `RoutingService` 并写回），否则新功能在老机器上静默不可用。
2. 出站订阅必须复用域名组订阅已经建立的三条不变量（手工与订阅字段物理隔离、失败保留上次成功数据、写回带 `subscribe_url` 相等的 compare-and-set），见 `docs/superpowers/specs/2026-09-02-domain-subscription-design.md`。
3. 出站订阅要抄 `filterOutboundsRejectedByCore`：逐个用核心的 config loader 验一遍，扔掉核心拒绝的那些。一份合并配置里只要有一个坏 outbound，整个 xray 起不来。

### 计划 04 — Reality 与现代入站能力

**覆盖**：第三梯队 a（Reality / `xtls-rprx-vision`）、f（fallback 配置）。

`web/assets/js/model/xray.js:44-45` 现在只有 `xtls-rprx-origin` / `xtls-rprx-direct`，`security` 只有 `none` / `tls` / `xtls`。这三个都在 Xray 1.8 被移除了。

**这里有一个必须在计划里正面处理的既有缺陷**：任何用 `security: "xtls"` 保存过的入站，今天生成的 `streamSettings` 里带 `xtlsSettings`，而 26.7.28 的核心不认这个键。按 CLAUDE.md 记载的"xray 加载配置是全有或全无"，这样一个入站会让整份 `bin/config.json` 加载失败，全机断网。计划 04 必须包含：识别这类存量数据、给出迁移或明确的界面警告，不能只是"新增 Reality 选项"。

参考：`internal/web/service/reality_scan.go`（扫描候选 SNI：连上去看 TLS1.3 + H2 + 证书链）、`internal/web/service/fallback.go`。

### 计划 05 — 面板运维能力

**覆盖**：第二梯队的数据库备份/恢复、面板自更新、日志维护、端口冲突检测；第三梯队 d（面板内看 xray 日志）。

| 项 | 参考实现 | 要点 |
|---|---|---|
| 数据库备份 / 恢复 | `internal/web/controller/server.go:342`（`getDb`）、`:379`（`importDB`） | `keepHostSettings` 开关区分"迁移到新机"与"整机克隆" |
| 面板自更新 | `internal/web/service/panel/panel.go:131`（`GetUpdateInfo`）、`:200`（`GetUpdateStatus`） | AetherUI 现在只能 SSH 跑 `a-ui update` |
| xray 日志截断 | `internal/web/job/clear_logs_job.go` 的 `PruneXrayLogsJob`（超 64MB 截断） | AetherUI **依赖** xray 的 access log 文件（`util/accesslog`），日志涨爆磁盘是真实风险 |
| 内存回收 | `internal/web/job/free_os_memory.go` | 一个 `debug.FreeOSMemory()`，小内存 VPS 上很实在 |
| 端口冲突检测 | `internal/web/service/port_conflict.go` | 按 TCP/UDP 分别判定；AetherUI 现在 `Inbound.Port` 上一个 `gorm:"unique"` 一刀切，TCP 入站与 QUIC 入站同端口本不冲突却会被拒 |
| 面板内看 xray 日志 | `internal/web/controller/server.go:76`（`/xraylogs/:count`） | 直接消掉 CLAUDE.md 里"面板说 running 其实已经死了，排查要靠 pgrep"这条 |

端口冲突那条要注意：改 `unique` 约束是 schema 变更，且**放宽约束**意味着已有的重复检测逻辑要接管。必须在计划里写清迁移方式与回滚路径。

### 计划 06 — 可观测性、告警与对外接口

**覆盖**：第三梯队 b（订阅服务器）、c（监控历史曲线）、e（事件总线 + 告警）、g（REST API + API Token）。

这份最大，计划内部按三个阶段推进，每阶段可独立上线：

- **阶段一 · 监控历史**：`internal/web/service/metric_history.go` 的 RRDtool 式分层降采样（2s×1800 = 1 小时 / 60s×2880 = 48 小时 / 600s×1008 = 7 天，每序列约 5700 点 ≈ 90 KiB，落 gob 文件）。AetherUI 已有每 2 秒的状态采集，接上去成本低。
- **阶段二 · 事件总线与告警**：`internal/eventbus/`（事件类型 `xray.crash` / `cpu.high` / `memory.high` / `login.attempt`）。**告警通道用标准库实现**：Telegram 走 `net/http` POST `sendMessage`，邮件走 `net/smtp`，不引入 telego 等第三方库。`xray.crash` 这个事件对 AetherUI 尤其有意义——`xray/process.go` 已经有 `p.exitErr` 字段，现在没人读。
- **阶段三 · 对外接口**：订阅服务器（`internal/sub/`，raw / JSON / Clash 三种格式）与 REST API + API Token（`internal/web/service/panel/api_token.go`，token 存 SHA-256）。

**硬前置**：阶段三必须排在计划 02（安全基线）之后。订阅服务器和 API Token 都是新增的、面向公网的非 session 认证入口，在明文密码 + 无 CSRF 的现状上叠加它们会放大攻击面。

---

## 4. 全局约束

以下约束对 6 份计划全部生效，各计划的 Global Constraints 章节直接引用本节。

### 4.1 依赖

- 目标 Go 版本 **1.27.0**（`go.mod` 的 `go` 指令与 CI 保持一致，CI 改用 `go-version-file: go.mod`，不再硬编码版本号）。
- `xray-core` 升到与 `bin/xray-*` 实际版本（26.7.28）匹配的发行版；版本号在计划 01 执行时用 `go list -m -versions github.com/xtls/xray-core` 与 `./bin/xray-darwin-arm64 -version` 对照选定，**不凭记忆写死**。
- **除 `xray-core` 及其强制连带（grpc / protobuf / golang.org/x/\*）外，不主动升级其他依赖。** gin 1.7.1、gorm 1.21.9、gopsutil v3.21.3 保持原样，除非在新工具链下确实编译失败——那时才升，且单独成任务、单独提交。
- 告警通道（计划 06）**不引入第三方库**：Telegram 用 `net/http`，SMTP 用 `net/smtp`。
- 新增任何其他依赖前，先说明标准库与现有依赖为何不足。

### 4.2 必须保持的既有不变量

这些是 CLAUDE.md 已经记录、且被现有测试守着的约束，任何计划都不得削弱：

- **生成逐字节确定**：出站按 id 升序、规则按 `priority asc, id asc`、`InboundIds` 升序去重。禁止遍历 map 产生数组顺序。破坏它会让 `Config.Equals` 恒为 false，那个 10 秒的重启 cron 会不停重启 xray。
- **新增会影响 xray 配置的字段时，必须同步扩展 `Config.Equals` / `InboundConfig.Equals`**（`xray/config.go`）。`json_util.RawMessage` 字段按字节比较，不必改。
- **绝不输出条件残缺的规则**：`buildRule` 的空域名 / 空 `inboundTag` 检查（`web/service/routing_inject.go`）。
- **三条引用边的删除守卫**：`CheckDomainGroupRefs` / `CheckOutboundRefs` / `CheckInboundRefs`。SQLite 会复用自增 id，新增任何"存 id 外键 + 被引用方可删"的表都要照做。
- **保留 tag 判定收敛在 `model.IsReservedTag()` 一处**。
- **入站校验 fail open 的三条边界不得收紧**：xray 自身故障、取不到完整配置、改动前配置就已非法——一律放行并记日志。
- **Vue 指令必须写在 `el` 指向的根元素之内**（`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着）。
- **`a-tabs` 的非活动面板仍在 DOM 里**，选择器必须限定到 `.ant-tabs-tabpane-active`。

### 4.3 新增设置项的五步

`SettingService` 用反射把 `entity.AllSetting` 的 `json` tag 与 settings 表的 key 对应，**反射只支持 `int` 和 `string` 两种字段类型**。新增一个设置项必须完成全部五步：

1. `web/service/setting.go` 的 `defaultValueMap` 加默认值
2. `web/entity/entity.go` 的 `AllSetting` 加字段（仅当需要前端可改）
3. `entity.CheckValid` 加校验
4. `web/service/setting.go` 加对应 getter
5. **`web/assets/js/model/models.js` 的 `AllSetting` 构造函数里加同名字段**

第 5 步不是收尾工作。`ObjectUtil.cloneProps` 只克隆目标对象已有的 key，漏掉会让服务端返回值被直接丢弃；而 `updateAllSetting` 提交的就是这个 JS 对象，新字段在提交体里根本不存在，Gin 绑成零值——若后端校验拒绝零值，**整个保存配置接口都会失败**，端口、证书路径、时区一起遭殃，报错信息还只指向新字段。

### 4.4 前端

- 无打包工具。改了 `web/assets/js|css` 而 `config/version` 没变，浏览器会命中 `max-age=31536000` 的强缓存。开发期用 `XUI_DEBUG=true` 从磁盘读。
- 模板用 `[[ ]]` 作为 Vue 插值分隔符。
- 新增协议 / 传输方式要同时改 `web/assets/js/model/xray.js`（模型与分享链接生成）和 `web/html/xui/form/` 下对应的局部模板。
- 改完 `web/html/**` 光靠 `go build` 发现不了问题（`getHtmlTemplate` 吞掉 `ParseFS` 错误），必须跑 `web/html_test.go` 的 `TestAllTemplatesParse`。

### 4.5 测试

- 标准库 `testing`，表驱动 + `t.Run` 子测试。断言确切值 / 确切错误，不只断言 `err != nil`。
- **一个测试必须在没有其修复时失败。** 写完测试先revert实现看它变红，再恢复。两边都过的测试比没有测试更糟——它什么都不保证，却会被当成修复有效的证据。
- `web/service` 的 `TestMain` 会 `os.Chdir` 到仓库根（因为 `xray.GetBinaryPath()` 返回相对路径）。这是**进程级副作用**：该包内新增依赖包内相对路径的测试要改用 `t.TempDir()` 或绝对路径。
- 临时验证产物一律放 `/private/tmp/claude-501/-Users-caryallen-Desktop-AetherUI-AetherUI-main/c74ccce5-9df3-4252-a475-129aaea8caf7/scratchpad`，不进仓库；任务完成前用 `git status` 核对最终 diff。

### 4.6 提交

Conventional Commits，中文正文说明「为什么」。类型：`feat` / `fix` / `refactor` / `perf` / `docs` / `chore` / `test`。

---

## 5. 明确不做的事

写在这里是为了防止执行期范围蔓延：

- **多节点 master/sub（mTLS）**、AmneziaWG、MTProto、PIA、LDAP。与 AetherUI 的单机定位不符，每一个都是独立子系统级复杂度。
- **多客户端 per inbound（Client 表）**。「一个 inbound 一个用户」是刻意设计，分流按 `inboundTag` 匹配、不需要 email 维度这条不变量整个建立在它之上。改它等于重写分流子系统。
- **PostgreSQL / dialect 抽象**。单机面板用不上，反而会让 SQLite id 复用那三条 `Check*Refs` 守卫的前提复杂化。
- **前端换 React**。现有 Vue 2 服务端模板有 `web/html_test.go` 两个不变量测试守着，是能用的。
- **主动升级 gin / gorm / gopsutil**。除非在新工具链下编译失败。
- **修复 `xray.Config` 丢弃未知顶层键的问题**（§2.4）。这是用户可见行为变更，不在本路线图范围。
