# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

AetherUI（二进制与模块名均为 `a-ui`）是一个基于 Xray-core 的 Web 管理面板：Go + Gin + GORM/SQLite 后端，Vue 2 + ant-design-vue 的服务端模板前端。面板进程通过 `os/exec` 拉起并托管 `bin/xray-*` 子进程，通过 xray 的 gRPC Stats API 采集流量。

代码谱系为 vaxilu/x-ui → FranzKafkaYu/x-ui 的早期分支，README 直接继承自上游。

## 常用命令

没有 Makefile、lint 配置或前端构建流程。测试只有标准 `go test`，集中在 `util/link`、`database`、`web/service` 三个包（其余包仍无测试）。

```bash
# 构建（CGO 必须开启：gorm.io/driver/sqlite 依赖 mattn/go-sqlite3）
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o a-ui main.go

# 交叉编译 linux/arm64（需 gcc-aarch64-linux-gnu）
CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o a-ui main.go

# 本地开发运行（见下方「调试模式」的目录要求）
XUI_DEBUG=true go run main.go

# 依赖整理
go mod tidy && go vet ./...
```

二进制子命令（`main.go`）：

```bash
./a-ui                                   # 无参数等价于 run
./a-ui run                               # 启动面板
./a-ui -v                                # 打印版本
./a-ui setting -port 54321               # 改面板端口
./a-ui setting -username x -password y   # 改首个用户的账号密码
./a-ui setting -reset                    # 清空 settings 表（回落到默认值）
./a-ui v2-ui -db /etc/v2-ui/v2-ui.db     # 从 v2-ui 迁移 inbound
```

环境变量：`XUI_DEBUG=true`（调试模式，见下）、`XUI_LOG_LEVEL=debug|info|warn|error`。变量名沿用上游的 `XUI_` 前缀，未随品牌改名。

### 发版

打 tag 推送即触发 `.github/workflows/release.yml`：matrix 构建 amd64/arm64 → 打包中/英两个 tar.gz → `gh release create`。CI 会在编译前把 tag 名写入 `config/version`（该文件由 `//go:embed` 读入），因此**版本号不在代码里维护**。`workflow_dispatch` 可试构建但不建 release。

## 架构要点

### 启动与生命周期

`main.go` → `database.InitDB`（`AutoMigrate` User/Inbound/Setting，无 User 时创建 `admin/admin`）→ `web.NewServer().Start()`。

`web.Server.Start()`（`web/web.go`）是理解整个系统的入口，它按序完成：从 settings 读时区并创建 cron → `initRouter`（session、basePath 中间件、i18n、模板与静态资源、注册三个顶层 controller）→ 读证书决定 HTTP/HTTPS 监听 → `startTask` 注册定时任务 → `httpServer.Serve`。

`main.go` 监听 `SIGHUP` 时会 `Stop()` 旧 Server 并 `NewServer().Start()`——面板「重启」（`PanelService.RestartPanel`）就是给自己发 SIGHUP，配置变更无需重启进程。

### 分层

- `web/controller/` — 只做参数绑定、鉴权（`BaseController.checkLogin`）和 `jsonMsg/jsonObj` 响应包装（`web/controller/util.go`）。
- `web/service/` — 业务逻辑，所有 service 都是**无状态空结构体**，按值嵌入使用；跨请求状态（xray 进程 `p`、`isNeedXrayRestart`）是包级变量。
- `database/model/model.go` — 仅 3 张表。Inbound 把 xray 的 `settings`/`streamSettings`/`sniffing` 原样存为 JSON 字符串，Go 侧不解析结构，由前端 `web/assets/js/model/xray.js` 负责建模。

路由树：`basePath` → `/`（登录）、`/server/*`（状态、xray 版本管理）、`/xui/*`（需登录，下挂 `/xui/inbound/*` 与 `/xui/setting/*`）。

### xray 进程与配置合成

`XrayService.GetXrayConfig()` 把 settings 里的 `xrayTemplateConfig`（默认值 `//go:embed` 自 `web/service/config.json`）反序列化为 `xray.Config`，再把数据库中 `enable=true` 的 inbound 逐条 append 到 `InboundConfigs`。模板里预置了 tag 为 `api` 的 dokodemo-door inbound（127.0.0.1:62789），`Process.refreshAPIPort()` 就是靠这个 tag 找到 gRPC Stats 端口。**改模板时不能删掉 `api` inbound / `stats` / `policy`，否则流量统计静默失效。**

`Process.Start()` 会把合成结果写到 `bin/config.json` 再 `exec bin/xray-<GOOS>-<GOARCH>`——**全是相对路径**，因此进程的工作目录必须是安装根目录（systemd 单元里的 `WorkingDirectory=/usr/local/a-ui/`）。

合成的最后一步是 `RoutingInjector.Inject(cfg)`，它把出站节点与分流规则追加进 `OutboundConfigs` 与 `RouterConfig`（见「域名分流管理」）。

重启去抖机制：任何改动 inbound 的 controller 调用 `xrayService.SetToNeedRestart()` 置原子标志；`InboundController.startTask()` 注册的 10 秒 cron 用 `IsNeedRestartAndSetFalse()` 消费该标志并调 `RestartXray(false)`；`RestartXray` 再用 `Config.Equals()`（`xray/config.go` 逐字段 `bytes.Equal`）判断配置是否真的变了。所以**新增会影响 xray 配置的字段时，必须同步扩展 `Config.Equals` / `InboundConfig.Equals`**，否则改动不会生效。

对 `OutboundConfigs` / `RouterConfig` 这类 `json_util.RawMessage` 字段则不必改 `Equals`——它们按字节比较，内容变化天然被察觉。代价是**生成必须逐字节确定**，否则 `Equals` 恒为 false，上面那个 10 秒 cron 会不停重启 xray。

### 定时任务（`web/job/`，均注册在 `Server.startTask`）

- `CheckXrayRunningJob`（30s）— 连续 2 次检测到未运行才置重启标志，避开重启窗口。
- `XrayTrafficJob`（10s，启动后延迟 5s 注册）— 拉取流量并按 tag 累加到 Inbound 的 up/down（`reset=true`，xray 侧计数清零）。
- `CheckInboundJob`（30s）— 把超流量或已过期的 inbound 置 `enable=false` 并触发重启。

另有两个在 controller 里注册的任务：`ServerController` 每 2 秒刷新系统状态（前端 3 分钟无请求即停刷）、`InboundController` 的重启消费任务。

### 前端

无打包工具。`web/assets/` 下是本地化的第三方库，模板通过 `{{ .base_path }}assets/...?{{ .cur_ver }}` 引用；`cur_ver` 取自 `config.GetVersion()`，**改了 `web/assets/js|css` 而版本号没变，浏览器会命中 `max-age=31536000` 的强缓存**。

模板用 `[[ ]]` 作为 Vue 插值分隔符以避开 Go 模板的 `{{ }}`。`web/html/xui/form/` 下按协议 / 传输方式拆分为局部模板，新增协议需要同时改 `xray.js`（模型与分享链接生成）和对应 `form/protocol/*.html`。

### 调试模式与嵌入资源

`config.IsDebug()` 为真时：gin 用 DebugMode，模板与静态资源**从磁盘的 `web/html`、`web/assets` 读取**（相对当前工作目录，所以必须在仓库根目录启动）；为假时全部走 `//go:embed`。因此改完模板要么开 `XUI_DEBUG=true`，要么重新编译。

`config/name` 与 `config/version` 是被 embed 的裸文本文件。`GetName()` 决定数据库路径 `/etc/<name>/<name>.db`，改动会导致已有部署找不到数据。

### 设置系统

settings 是 key-value 表。`SettingService` 用反射把 `entity.AllSetting` 的 `json` tag 与 key 对应（`GetAllSetting` / `UpdateAllSetting`），未落库的 key 回落到 `defaultValueMap`。**新增设置项 = 在 `defaultValueMap` 加默认值 + 在 `entity.AllSetting` 加字段（仅当需要前端可改）+ 在 `entity.CheckValid` 加校验 + 加对应 getter**。反射只支持 `int` 和 `string` 两种字段类型。

`secret`（session 加密密钥）的默认值是随机生成的，`GetSecret()` 检测到仍是默认值时会立刻落库固化，避免每次重启导致会话全部失效。

### i18n

`web/translation/*.toml` 仅有 11 行，只覆盖登录页与几个通用按钮。**绝大部分界面文案和后端返回消息（`jsonMsg(c, "添加", err)` 拼成「添加成功/添加失败: ...」）是硬编码简体中文。** 语言由请求头 `Accept-Language` 决定，与安装包的中英文版本无关（两个 tar.gz 的二进制完全相同，只有 SSH 管理脚本不同）。

注意 `initI18n` 中的 `localizer` 是**闭包捕获的包外单变量**，被中间件按请求覆写——存在并发竞态，这是既有实现，改动相关代码时留意。

## 域名分流管理

让管理员配置「**哪个用户**访问**哪批域名**时走**哪个落地节点**，或直接黑洞掉」。设计文档在 `docs/superpowers/specs/2026-09-02-domain-routing-design.md`，是本子系统的约束来源，改动前先读它。

### 数据模型（`database/model/routing.go`）

三张表，规则是一条把前两者连起来的连线：

```
DomainGroup   域名组     Remark + Domains(JSON 字符串数组)
OutboundNode  出站节点   Tag(unique) + Remark + Protocol + Config(完整 outbound JSON) + Enable
RoutingRule   分流规则   InboundId × DomainGroupId → Action(proxy|block) + OutboundId + Priority + Enable
```

**「用户」在本项目里等价于「一个入站」**——前端每个协议表单只绑定 `settings.<protocol>es[0]`，所以一个 inbound 恰好一个用户。因此分流按 `inboundTag` 匹配，不需要 email 维度。

三条不可动摇的字段约定：

- **规则存 `InboundId` 外键，不存 tag 字符串。** 入站 tag 由端口算出（`UpdateInbound` 里 `Tag = fmt.Sprintf("inbound-%v", Port)`），用户改端口 tag 就变，存字符串会让规则静默失效。
- **`InboundId = 0` 表示对所有入站生效**（全局规则），生成时不输出 `inboundTag`。
- **出站 `Tag` 一经分配即不可变**，且不能由自增 Id 拼出（unique 约束要求 INSERT 前就确定，那时 Id 尚未分配）。用 `link.SuggestTag("a-ui", remark, idx)` 生成，重名由调用方追加序号——注意 `SuggestTag` 只在 remark 为空时才用 `idx`，remark 非空时对任何 idx 都返回同一个值。
- **`a-ui-block` 是保留 tag，任何用户可控的 tag 分配都必须排除它。** 数据库唯一约束管不到它——注入器发出的 tag 不在 `outbound_nodes` 表里。备注写「block」（含 `Block`/`BLOCK`/`block!`/` block ` 等，`SlugRemark` 会把它们归一到同一个 slug）就会生成 `a-ui-block`，与注入器的黑洞出站撞名，xray 报 `existing tag found: a-ui-block` 并拒绝启动。**将来若增加第二个保留 tag，必须同时更新分配端与生成端的排除逻辑**（建议做成 `IsReservedTag()` 之类的单一判定，避免两处各自维护）。

### xray 会静默接受错误配置——这是本子系统的全部设计动机

以下均由真实 xray 26.7.28 实测确认，不是推断：

| 情形 | xray 的反应 | 实际后果 |
|---|---|---|
| 规则引用已删除的**出站** | `Configuration OK` | 运行时静默回落默认出站（**直连**）。以为封禁/分流了，其实裸奔 |
| 规则的 `domain` 为**空数组** | `Configuration OK` | xray 把缺失条件当作「不限制」，规则从「访问这批域名走 B」**退化成「该用户全部流量走 B」** |
| 规则引用已删除的**入站** | `Configuration OK` | 规则永不命中（无害） |
| tag 含中文 | `Configuration OK` | 合法，无需转写 |

因此有**两道防线**，改动本子系统时都不能削弱：

1. **删除时拒绝**——`DomainGroupService.Del` / `OutboundNodeService.Del` 先调 `RoutingRuleService.CheckDomainGroupRefs` / `CheckOutboundRefs`。
2. **生成期跳过**——`buildRule` 返回 `nil` 即整条规则丢弃。域名组不存在或域名为空、出站不存在或已禁用、入站不存在，一律跳过。**宁可规则不生效让用户察觉，也绝不输出条件残缺的规则。**

入站没有引用守卫（管理员可以正常删掉被规则引用的入站），全靠第二道防线兜住。

### 配置注入的四条不变量（`web/service/routing_inject.go`）

1. **一律 append 到末尾。** 出站追加到末尾，模板里的 `freedom` 才继续是 xray 的默认出站；规则追加到末尾，模板原有的安全规则（屏蔽私网、屏蔽 BT）与用户手写规则才保持更高优先级。
2. **block 规则排在 proxy 规则之前**（两个独立切片先后 append，与 `Priority` 无关）。违规域名封禁是硬约束，不能被某条分流规则绕过。
3. **绝不输出条件残缺的规则**（见上）。
4. **生成逐字节确定**：规则按 `priority asc, id asc`、出站按 `id asc`，`encoding/json` 对 map key 排序。**禁止遍历 map 来产生数组顺序**。

黑洞出站 `a-ui-block` 由注入器**始终自行注入**，不复用模板里的 `blocked`——用户可能把模板里那个删掉，而悬空 `outboundTag` 不报错，block 会静默变直连。所有生成的 tag 统一带 `a-ui-` 前缀，与手工模板隔离。

`buildOutbounds` 返回它**实际写入配置的** id→tag 映射，`buildRules` 必须消费这个映射而不是自己再查一次——否则一个 `Config` 损坏而被跳过的节点，其 tag 仍会被规则引用，形成悬空引用。

### 域名分流依赖 sniffing

路由要靠 xray 嗅探拿到 SNI/Host。入站若关掉 sniffing 或 `destOverride` 不含 `http`/`tls`，**域名规则永远不会命中且无任何报错**。新建入站默认 `enabled=true, destOverride=['http','tls']`（`web/assets/js/model/xray.js` 的 `Sniffing` 类），页面的规则列表会对不满足的入站打黄色警告图标。

### 输入侧校验（`web/service/routing_validate.go`）

出站配置与域名列表在**落库之前**用一份最小配置交给真实 xray 校验，因此不需要事务回滚。两个入口都接了：`OutboundNodeService.persist`（新建）与 `Update`（编辑）——**只堵一条等于没堵**，用户可以先建合法节点再编辑成坏的。

策略是 **fail open**：只有 xray 明确判定配置非法才拒绝；二进制缺失、老版本不认 `run -test` 参数、执行超时，一律放行并记日志。校验是辅助手段，不能因它自身故障就把用户锁在门外。

**当前实现是「用最小配置包住单个 outbound / 域名列表」，只能发现该对象自身的错误，发现不了任何组合层面的冲突**（重复 tag、指向不存在出站的 `proxySettings.tag` 等）。设计文档 §5.4.2 要求的是校验**完整生成配置**——实测 `xray run -test` 含加载 10MB geosite 仅约 18ms，成本不构成理由。改动此处时应优先补上完整配置校验。

### `util/link` 包

移植自 3x-ui（`internal/util/link/`，GPL-3.0，与本项目同许可），负责把分享链接解析成 xray outbound。零第三方依赖，自带测试。支持 vmess / vless / trojan / ss / hysteria2 / wireguard，**socks 是本项目自行补充的**（`socks.go`）。文件头保留了来源声明，从上游同步更新时注意不要覆盖掉 `socks.go`。

## 运维脚本

四个 shell 脚本必须成对维护，改一个就要同步另一个：

- `install.sh` / `install_en.sh` — 一键安装，从 GitHub Release 下载 tar.gz 解压到 `/usr/local/a-ui/`，安装管理脚本到 `/usr/bin/a-ui`，注册 systemd。
- `a-ui.sh` / `a-ui_en.sh` — 安装后的管理菜单（0-17 项：安装/更新/卸载、重置账号密码、端口、启停、开机自启、BBR、acme 申请 SSL、定时任务）。同时支持 `a-ui start|stop|restart|status|log|update|clear|geo|cron` 等直接子命令。

脚本里硬编码了仓库地址 `SienFeng/AetherUI`，fork 后需一并修改。

## 已知偏差与注意事项

- **上游文档宣称的许多功能并不存在于本代码库**：无 Telegram bot（全仓库无任何相关代码）、无 Reality、无 `xtls-rprx-vision`、无客户端级流量统计与到期限制、无设备/IP 并发限制。前端每个协议表单只编辑 `settings.xxxes[0]`，即**一个 inbound 一个用户**。README 已于本仓库删除，**判断功能是否存在一律以代码为准**。
- 用户密码在数据库中**明文存储**，登录失败日志还会打印明文用户名密码（`web/controller/index.go`）。这是既有行为，涉及认证的改动请提高审查标准。
- `go.mod` 声明 `go 1.21`（`util/link` 用到 `any` 与 `maps.Copy`），CI 用 Go 1.22 构建；依赖版本较老（gin 1.7.1、xray-core v1.4.2 仅用于 gRPC stats 客户端，与实际运行的 `bin/xray-*` 版本无关）。
- `bin/xray-darwin-arm64` 在 `.gitignore` 中，macOS 本地跑面板需自行下载对应 Xray 二进制放入 `bin/`，否则 `RestartXray` 必然失败（面板本身仍可访问）。
- `util/sys/psutil.go` 用 `//go:linkname` 侵入 gopsutil 内部包，升级 gopsutil 时会断。
- **`web.go` 的 `getHtmlTemplate` 吞掉 `ParseFS` 错误**（`// ignore`）。一个语法错误的模板会被静默跳过，直到渲染时才报 "template not found"。所以改完 `web/html/**` 光靠 `go build` 无法发现问题——需要自己 `ParseFS` 一遍让错误暴露出来。
- **面板里的「安装 xray」会连带覆盖 `bin/geoip.dat` 与 `bin/geosite.dat`**（`ServerService.UpdateXray` 从 zip 里一并解出），而这两个文件是仓库跟踪的。仓库当前这份来自 Xray 26.7.28，**含 OPENAI 类别**；更早的版本不含，会让 `geosite:openai` 直接报错。不要把它们还原成更旧的版本。
- **测试的工作目录**：`xray.GetBinaryPath()` 返回相对路径 `bin/xray-<GOOS>-<GOARCH>`，而 `go test` 的 cwd 是包目录。`web/service` 的 `TestMain` 因此 `os.Chdir` 到仓库根（这也与生产一致，systemd 的 `WorkingDirectory=/usr/local/a-ui/`）。这是**进程级副作用**：若今后在该包新增依赖包内相对路径（如 `testdata/`）的测试，请改用 `t.TempDir()` 或绝对路径。
- **面板报告的 xray 状态可能是假的。** `Process.Start()` 把 `cmd.Run()` 丢进 goroutine 后直接返回 nil，所以 xray 启动失败**不会**回传到面板。实测过一次配置冲突：xray 已经退出、全员断网，而 `/server/status` 仍返回 `state=running`、`errorMsg=""`。排查「面板说正常但用不了」这类问题时，**以 `pgrep` 和 `bin/config.json` 跑一次 `xray run -test` 为准，不要相信面板首页**。
- **SQLite 的自增主键 id 会被复用。** GORM 的 sqlite 驱动对 `primaryKey;autoIncrement` 生成的是 rowid 别名而非 `AUTOINCREMENT`，删掉最大 id 的行后，新插入的行会拿到同一个 id。任何「存 id 外键 + 被引用方可删除」的组合都要考虑这一点——旧引用会静默绑到新记录上，而且因为引用不再悬空，生成期的跳过防线也拦不住。
- **cron 任务没有 panic 恢复。** `Server.Start()` 里 `cron.New(...)` 未配 `cron.WithChain(cron.Recover(...))`，`robfig/cron/v3` 自身也不 recover。定时任务（含每 10 秒的 xray 重启消费任务）里的任何 panic 都会**杀掉整个面板进程**。在这些路径上写代码要格外注意 nil map、越界等运行时 panic。
