# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

AetherUI（二进制与模块名均为 `a-ui`）是一个基于 Xray-core 的 Web 管理面板：Go + Gin + GORM/SQLite 后端，Vue 2 + ant-design-vue 的服务端模板前端。面板进程通过 `os/exec` 拉起并托管 `bin/xray-*` 子进程，通过 xray 的 gRPC Stats API 采集流量。

代码谱系为 vaxilu/x-ui → FranzKafkaYu/x-ui 的早期分支，README 直接继承自上游。

## 常用命令

本仓库**没有任何测试文件**，也没有 Makefile、lint 配置或前端构建流程。

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

重启去抖机制：任何改动 inbound 的 controller 调用 `xrayService.SetToNeedRestart()` 置原子标志；`InboundController.startTask()` 注册的 10 秒 cron 用 `IsNeedRestartAndSetFalse()` 消费该标志并调 `RestartXray(false)`；`RestartXray` 再用 `Config.Equals()`（`xray/config.go` 逐字段 `bytes.Equal`）判断配置是否真的变了。所以**新增会影响 xray 配置的字段时，必须同步扩展 `Config.Equals` / `InboundConfig.Equals`**，否则改动不会生效。

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

## 运维脚本

四个 shell 脚本必须成对维护，改一个就要同步另一个：

- `install.sh` / `install_en.sh` — 一键安装，从 GitHub Release 下载 tar.gz 解压到 `/usr/local/a-ui/`，安装管理脚本到 `/usr/bin/a-ui`，注册 systemd。
- `a-ui.sh` / `a-ui_en.sh` — 安装后的管理菜单（0-17 项：安装/更新/卸载、重置账号密码、端口、启停、开机自启、BBR、acme 申请 SSL、定时任务）。同时支持 `a-ui start|stop|restart|status|log|update|clear|geo|cron` 等直接子命令。

脚本里硬编码了仓库地址 `SienFeng/AetherUI`，fork 后需一并修改。

## 已知偏差与注意事项

- **README 描述的功能大部分不存在于本代码库**：无 Telegram bot（全仓库无任何相关代码）、无 Reality、无 `xtls-rprx-vision`、无客户端级流量统计与到期限制、无设备/IP 并发限制。前端每个协议表单只编辑 `settings.xxxes[0]`，即**一个 inbound 一个用户**。README 的「变更记录」继承自上游，**判断功能是否存在一律以代码为准**。
- 用户密码在数据库中**明文存储**，登录失败日志还会打印明文用户名密码（`web/controller/index.go`）。这是既有行为，涉及认证的改动请提高审查标准。
- `go.mod` 声明 `go 1.16`，CI 用 Go 1.22 构建；依赖版本较老（gin 1.7.1、xray-core v1.4.2 仅用于 gRPC stats 客户端，与实际运行的 `bin/xray-*` 版本无关）。
- `bin/xray-darwin-arm64` 在 `.gitignore` 中，macOS 本地跑面板需自行下载对应 Xray 二进制放入 `bin/`，否则 `RestartXray` 必然失败（面板本身仍可访问）。
- `util/sys/psutil.go` 用 `//go:linkname` 侵入 gopsutil 内部包，升级 gopsutil 时会断。
