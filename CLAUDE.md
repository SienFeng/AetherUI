# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

AetherUI（二进制与模块名均为 `a-ui`）是一个基于 Xray-core 的 Web 管理面板：Go + Gin + GORM/SQLite 后端，Vue 2 + ant-design-vue 的服务端模板前端。面板进程通过 `os/exec` 拉起并托管 `bin/xray-*` 子进程，通过 xray 的 gRPC Stats API 采集流量。

代码谱系为 vaxilu/x-ui → FranzKafkaYu/x-ui 的早期分支，README 直接继承自上游。

## 常用命令

没有 lint 配置或前端构建流程。测试是标准 `go test`；目前有 14 个包带测试（`database`、`database/model`、`util/link`、`web/service`、`xray` 等，`make verify` 的输出即是当前清单），其余包仍无测试。`Makefile` 提供 `make build` / `make test` / `make vet` / `make verify`（vet + test + build，提交前的门禁）/ `make clean`；`.github/workflows/ci.yml` 在 push/PR 时跑 `make verify`，Go 版本从 `go.mod` 读取，不在工作流里硬编码。

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
./a-ui setting -listen 127.0.0.1         # 改面板监听地址（-listen "" 恢复监听所有 IP，救援用）
./a-ui setting -basepath /xxx/           # 改面板 url 根路径
./a-ui setting -show                     # 只读打印当前端口/监听地址/根路径/账号密码，不写库
./a-ui setting -reset                    # 清空 settings 表（回落到默认值）
./a-ui v2-ui -db /etc/v2-ui/v2-ui.db     # 从 v2-ui 迁移 inbound
./a-ui bootstrap -mode caddy ...         # 安装脚本用：写入面板配置并按需创建入站，见「安装向导与 Caddy 拓扑」
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

路由树：`basePath` → `/`（登录）、`/server/*`（状态、xray 版本管理）、`/aui/*`（需登录，下挂 `/aui/inbound/*` 与 `/aui/setting/*`）。

### xray 进程与配置合成

`XrayService.GetXrayConfig()` 把 settings 里的 `xrayTemplateConfig`（默认值 `//go:embed` 自 `web/service/config.json`）反序列化为 `xray.Config`，再把数据库中 `enable=true` 的 inbound 逐条 append 到 `InboundConfigs`。模板里预置了 tag 为 `api` 的 dokodemo-door inbound（127.0.0.1:62789），`Process.refreshAPIPort()` 就是靠这个 tag 找到 gRPC Stats 端口。**改模板时不能删掉 `api` inbound / `stats` / `policy`，否则流量统计静默失效。**

`Process.Start()` 会把合成结果写到 `bin/config.json` 再 `exec bin/xray-<GOOS>-<GOARCH>`——**全是相对路径**，因此进程的工作目录必须是安装根目录（systemd 单元里的 `WorkingDirectory=/usr/local/a-ui/`）。

合成的最后一步是 `RoutingInjector.Inject(cfg)`，它把出站节点与分流规则追加进 `OutboundConfigs` 与 `RouterConfig`（见「域名分流管理」）。

重启去抖机制：任何改动 inbound 的 controller 调用 `xrayService.SetToNeedRestart()` 置原子标志；`InboundController.startTask()` 注册的 10 秒 cron 用 `IsNeedRestartAndSetFalse()` 消费该标志并调 `RestartXray(false)`；`RestartXray` 再用 `Config.Equals()`（`xray/config.go` 逐字段 `bytes.Equal`）判断配置是否真的变了。所以**新增会影响 xray 配置的字段时，必须同步扩展 `Config.Equals` / `InboundConfig.Equals`**，否则改动不会生效——而且热更新上线后漏改的后果比改动前更重：改动前漏改只是**延迟生效**，配置别处一变就整进程重启，新字段照样能进核心；改动后，`diffInbounds` 用 `oldIb.Equals(newIb)` 判「这个入站没变，跳过」，随后 `SetConfig(newCfg)` 把新配置记成已应用，于是该字段**永远进不了核心，也不会有后续重启来纠正**——这是一个静默且持久的错误，不再是「早晚会被下一次改动带上」。

判断「配置变了」之后，`RestartXray` 不再直接重启：先试 `tryHotApply`（`web/service/xray.go`），它用 `xray.ComputeHotDiff`（`xray/hot_diff.go`）比较新旧配置，能靠核心的 gRPC 控制面（`xray/api.go`，入站/出站增删 + 整体替换路由规则）追平的就走热应用，不重启进程；`ComputeHotDiff` 判断不了或涉及没有运行时重载接口的段（log / dns / transport / policy / api / stats / reverse / fakeDns，以及数组首位的默认出站、Reality 入站、routing 里 `domainStrategy`/`domainMatcher` 等）就返回「不能热应用」，退回原来的整进程重启。**新增会影响 xray 配置的字段时，除了按上一段扩展 `Equals`，还要判断它是走运行时重载接口热更新，还是必须重启**——归入 `ComputeHotDiff` 里的 `static` 列表还是新的 diff 分支，判断错了不会报错，只会让核心与面板的配置认知从此不一致。`web/service/xray_hot_reload_e2e_test.go`（`TestHotReloadEndToEndAgainstRealXray`）用真实 xray 进程验证了这条链路：改分流规则走热应用且不重启进程，改访问日志开关（改 `log` 段）仍会触发整进程重启。

出站的增删（数组首位那个默认出站不算）能走热应用：`decodeOutbounds`（`xray/hot_diff.go`）只对数组首位放宽「tag 非空」的要求，因为 `RoutingInjector.buildOutbounds` 原样保留模板（`web/service/config.json`）里的出站数组再往后追加，而模板首位的 `freedom` 出站天然没有 tag——修复前 `decodeOutbounds` 对任何空 tag 一律判定「必须重启」，导致每一份真实生成的配置在出站层面必然走整进程重启，出站热更新事实上从未生效过。编辑一个出站（同 tag 换内容）在 `tryHotApply` 里走的是「先删后加」两条独立 RPC——xray 的 `AddHandler` 对已存在的 tag 会报 `existing tag found`，先加后删走不通。两条 RPC 之间有一个短窗口：引用该 tag 的规则在核心里是悬空的，按「域名分流管理 → xray 会静默接受错误配置」表「规则引用已删除的出站」一行的实测结论，xray 对此不报错，运行时**静默回落默认出站（直连）**。正常情况这个窗口只有一次回环 RPC（毫秒级）；若 `AddOutbound` 失败，窗口会拉长到退回重启完成（1~2 秒）。这是刻意接受、不做补偿的窗口——任何补偿逻辑（比如换临时 tag 分两步搬）都在增加新的失败面，与本子系统「失败即退回重启，不做部分回滚」的设计原则冲突。

对 `OutboundConfigs` / `RouterConfig` 这类 `json_util.RawMessage` 字段则不必改 `Equals`——它们按字节比较，内容变化天然被察觉。代价是**生成必须逐字节确定**，否则 `Equals` 恒为 false，上面那个 10 秒 cron 会不停重启 xray。

### 定时任务（`web/job/`，均注册在 `Server.startTask`）

- `CheckXrayRunningJob`（30s）— 连续 2 次检测到未运行才置重启标志，避开重启窗口。
- `XrayTrafficJob`（10s，启动后延迟 5s 注册）— 拉取流量并按 tag 累加到 Inbound 的 up/down（`reset=true`，xray 侧计数清零）。
- `CheckInboundJob`（30s）— 把超流量或已过期的 inbound 置 `enable=false` 并触发重启。
- `SubscriptionJob`（`@every 10m`）— 自检是否到了配置的订阅更新时刻（`entity.AllSetting.SubscriptionUpdateTime`），到点则拉取该刷新的域名组订阅，成功后置重启标志（`XrayService.SetToNeedRestart`），交给 `InboundController` 那个 10 秒 cron 消费。管理员改「订阅更新时间」或等它自然触发，都**最多 10 分钟内生效**，不需要重启面板。

另有两个在 controller 里注册的任务：`ServerController` 每 2 秒刷新系统状态（前端 3 分钟无请求即停刷）、`InboundController` 的重启消费任务。

### 前端

无打包工具。`web/assets/` 下是本地化的第三方库，模板通过 `{{ .base_path }}assets/...?{{ .cur_ver }}` 引用；`cur_ver` 取自 `config.GetVersion()`，**改了 `web/assets/js|css` 而版本号没变，浏览器会命中 `max-age=31536000` 的强缓存**。

模板用 `[[ ]]` 作为 Vue 插值分隔符以避开 Go 模板的 `{{ }}`。`web/html/xui/form/` 下按协议 / 传输方式拆分为局部模板，新增协议需要同时改 `xray.js`（模型与分享链接生成）和对应 `form/protocol/*.html`。

### 调试模式与嵌入资源

`config.IsDebug()` 为真时：gin 用 DebugMode，模板与静态资源**从磁盘的 `web/html`、`web/assets` 读取**（相对当前工作目录，所以必须在仓库根目录启动）；为假时全部走 `//go:embed`。因此改完模板要么开 `XUI_DEBUG=true`，要么重新编译。

`config/name` 与 `config/version` 是被 embed 的裸文本文件。`GetName()` 决定数据库路径 `/etc/<name>/<name>.db`，改动会导致已有部署找不到数据。

### 设置系统

settings 是 key-value 表。`SettingService` 用反射把 `entity.AllSetting` 的 `json` tag 与 key 对应（`GetAllSetting` / `UpdateAllSetting`），未落库的 key 回落到 `defaultValueMap`。**新增设置项 = 在 `defaultValueMap` 加默认值 + 在 `entity.AllSetting` 加字段（仅当需要前端可改）+ 在 `entity.CheckValid` 加校验 + 加对应 getter + 在 `web/assets/js/model/models.js` 的 `AllSetting` 构造函数里加同名字段**。反射只支持 `int` 和 `string` 两种字段类型。

最后这一步（前端 JS 模型）不是可省的收尾工作，漏掉的后果不是「新设置项不生效」这么轻：`ObjectUtil.cloneProps`（`web/assets/js/util/utils.js`）只克隆目标对象已经拥有的 key，服务端返回的值会被直接丢弃，输入框永远回落到硬编码的初始值；而 `updateAllSetting` 提交的是这个 JS 对象，新字段在提交体里根本不存在，Gin 绑定成零值，若后端校验恰好拒绝零值（比如时间字符串要求非空），**整个保存配置接口都会失败**，端口、证书路径、time zone 等一切无关字段一起遭殃，报错信息还只指向新字段，具有很强的误导性。`ObjectUtil.equals` 同理只按旧对象的 key 做比较，新字段的改动不会点亮「保存」按钮。

三个由安装向导写入的设置项——`defaultDomain` / `defaultCertFile` / `defaultKeyFile`（`entity.DefaultDomain`/`DefaultCertFile`/`DefaultKeyFile`）——只是**新建入站表单的默认填充值**，面板自己从不加载它们。`entity.CheckValid` 对面板自己要用的 `WebCertFile`/`WebKeyFile` 会调 `tls.LoadX509KeyPair` 实际打开证书文件，但这三个新字段**刻意只校验路径格式**（非空时须以 `/` 开头），不做同样的加载校验：证书尚未签发、只是先把将来的路径填进去，是这三项的正常状态（`bootstrap -mode caddy` 就是在证书由 Caddy 异步申请期间提前写入这三项），若在这里也 `LoadX509KeyPair`，会让整个设置页保存接口在证书签发前必然失败，且报错只指向这三个字段，误导管理员去查一个其实不相关的地方。

`secret`（session 加密密钥）的默认值是随机生成的，`GetSecret()` 检测到仍是默认值时会立刻落库固化，避免每次重启导致会话全部失效。

### i18n

`web/translation/*.toml` 仅有 11 行，只覆盖登录页与几个通用按钮。**绝大部分界面文案和后端返回消息（`jsonMsg(c, "添加", err)` 拼成「添加成功/添加失败: ...」）是硬编码简体中文。** 语言由请求头 `Accept-Language` 决定，与安装包的中英文版本无关（两个 tar.gz 的二进制完全相同，只有 SSH 管理脚本不同）。

注意 `initI18n` 中的 `localizer` 是**闭包捕获的包外单变量**，被中间件按请求覆写——存在并发竞态，这是既有实现，改动相关代码时留意。

## 域名分流管理

让管理员配置「**哪个用户**访问**哪批域名**时走**哪个落地节点**，或直接黑洞掉」。设计文档在 `docs/superpowers/specs/2026-09-02-domain-routing-design.md`，是本子系统的约束来源，改动前先读它。

### 数据模型（`database/model/routing.go`）

三张表，规则是一条把前两者连起来的连线：

```
DomainGroup   域名组     Remark + Domains(JSON 字符串数组) + 订阅五件套（见下）
OutboundNode  出站节点   Tag(unique) + Remark + Protocol + Config(完整 outbound JSON) + Enable
RoutingRule   分流规则   InboundIds(JSON 数组) × DomainGroupId → Action(proxy|block) + OutboundId + Priority + Enable
```

**「用户」在本项目里等价于「一个入站」**——前端每个协议表单只绑定 `settings.<protocol>es[0]`，所以一个 inbound 恰好一个用户。因此分流按 `inboundTag` 匹配，不需要 email 维度。

`DomainGroup` 的订阅五件套——`SubscribeUrl` / `SubscribedDomains` / `LastUpdatedAt` / `LastError` / `LastSkipped`——支撑域名组订阅更新（`web/service/routing_domain.go`、`routing_subscription.go`，`SubscriptionJob`）。两条不变量：

- **`Domains`（手工）与 `SubscribedDomains`（订阅）物理隔离，各自只有一个写入方。** 管理员编辑表单只写 `Domains`；`SubscriptionJob`/「立即更新」只写 `SubscribedDomains` 及其伴随的 `LastUpdatedAt`/`LastError`/`LastSkipped`。`buildRule` 用 `MergeDomains(manual, subscribed)` 在生成期合并两者（手工在前、订阅在后，保留首次出现），任何一侧都不会覆盖另一侧。
- **拉取失败保留上一次成功的数据，绝不清空。** 清空 `SubscribedDomains` 会让合并结果可能变空，`buildRule` 因「域名组为空」跳过整条规则——本该走指定节点或被封禁的流量静默退回直连，比规则单纯不生效更危险。同理，写回成功结果时要带 `subscribe_url` 相等的条件（compare-and-set）：拉取耗时可达 30s、批量刷新可达分钟级，这期间管理员可能已经把订阅地址改成了别的，不加条件的话旧地址拉到的内容会被当成新地址的结果写回，界面显示「刚刚更新」但域名其实是错的。

`DomainGroupService.Update` 不用 `Save` 写整行，而是拼一个只含实际要改的列的 `map[string]any`（`updateFieldsFor`），原因正是上一条：整行写入会把 `Get` 那一刻捕获的 `SubscribedDomains`/`LastUpdatedAt` 一并写回，把中间一次刚成功的订阅刷新静默回滚。**代价是这份列名单要手动维护**——将来给 `DomainGroup` 加字段，若不在 `updateFieldsFor` 里加对应的 key，这个字段就会静默地无法通过编辑接口更新（`Get`/展示不受影响，容易被漏测）。

五条不可动摇的字段约定：

- **规则存 `InboundIds`（入站 id 的 JSON 数组）而不是 tag 字符串。** 入站 tag 由端口算出（`UpdateInbound` 里 `Tag = fmt.Sprintf("inbound-%v", Port)`），用户改端口 tag 就变，存字符串会让规则静默失效。数组**升序去重**存储，这是「生成逐字节确定」的一部分。
- **`InboundIds` 为空数组 `[]` 表示对所有入站生效**（全局规则），生成时不输出 `inboundTag`。**注意空数组与「一个都没选」在提交体里无法区分**：写入路径用 `EncodeInboundIdsStrict` 挡住「非空输入被过滤成空」，前端 `saveRule` 也拦一道——否则一条本该覆盖某个人的规则会被静默放大到全体。
- **同一个域名组下，任何一个入站至多被一条规则覆盖**（`RoutingRuleService.checkConflict`，把空数组当全集做集合相交判定）。禁用的规则同样占位。只在写入路径校验，生成期不干预：迁移前留下的冲突数据照常生成两条规则，由界面标黄交给管理员处理。
- **出站 `Tag` 一经分配即不可变**，且不能由自增 Id 拼出（unique 约束要求 INSERT 前就确定，那时 Id 尚未分配）。用 `link.SuggestTag("a-ui", remark, idx)` 生成，重名由调用方追加序号——注意 `SuggestTag` 只在 remark 为空时才用 `idx`，remark 非空时对任何 idx 都返回同一个值。
- **`a-ui-block` 是保留 tag，任何用户可控的 tag 分配都必须排除它。** 数据库唯一约束管不到它——注入器发出的 tag 不在 `outbound_nodes` 表里。备注写「block」（含 `Block`/`BLOCK`/`block!`/` block ` 等，`SlugRemark` 会把它们归一到同一个 slug）就会生成 `a-ui-block`，与注入器的黑洞出站撞名，xray 报 `existing tag found: a-ui-block` 并拒绝启动。判定收敛在 `model.IsReservedTag()` 一处，分配端（`allocTag`）、生成端（`buildOutbounds`）与校验端（`removeOutboundByTag`）都只认它，**新增保留 tag 只改这一个函数**。生成端也要排除，是因为修复前分配出去的脏数据仍可能躺在库里。

### xray 会静默接受错误配置——这是本子系统的全部设计动机

以下均由真实 xray 26.7.28 实测确认，不是推断：

| 情形 | xray 的反应 | 实际后果 |
|---|---|---|
| 规则引用已删除的**出站** | `Configuration OK` | 运行时静默回落默认出站（**直连**）。以为封禁/分流了，其实裸奔 |
| 规则的 `domain` 为**空数组** | `Configuration OK` | xray 把缺失条件当作「不限制」，规则从「访问这批域名走 B」**退化成「该用户全部流量走 B」** |
| 规则的 `inboundTag` 为**空数组** | `Configuration OK` | 与上一行同构：规则从「只覆盖甲」**放大成覆盖所有入站**。Xray 26.7.28 实测：两个入站访问目标域名都被命中，对照域名正常放行 |
| 规则引用已删除的**入站** | `Configuration OK` | 规则永不命中（无害） |
| tag 含中文 | `Configuration OK` | 合法，无需转写 |

因此有**两道防线**，改动本子系统时都不能削弱：

1. **删除时拒绝**——三条引用边都有守卫：`DomainGroupService.Del` / `OutboundNodeService.Del` / `InboundService.DelInbound` 分别调 `RoutingRuleService` 的 `CheckDomainGroupRefs` / `CheckOutboundRefs` / `CheckInboundRefs`。入站这条边**不能只靠第二道防线**：SQLite 会复用被删除的自增 id（见「已知偏差」），孤儿规则会绑到新入站上，那时引用不再悬空，跳过防线拦不住，规则列表还会渲染得很合理。`CheckInboundRefs` 不把 `InboundIds` 为空数组的全局规则算作引用；它读出全部规则逐条解码判断，不能再用 `WHERE inbound_id = ?` 交给 SQL 去数。
2. **生成期跳过**——`buildRule` 第三个返回值非 nil 即整条规则丢弃。域名组不存在或域名为空、出站不存在或已禁用、规则指定的入站**全部**不存在或已禁用，一律跳过。**宁可规则不生效让用户察觉，也绝不输出条件残缺的规则。**跳过必须带原因并由 `buildRules` 记进 `logger.Warning`——否则这道防线对用户是隐形的：规则表照常渲染，配置里却没有它。

### 配置注入的四条不变量（`web/service/routing_inject.go`）

1. **一律 append 到末尾。** 出站追加到末尾，模板里的 `freedom` 才继续是 xray 的默认出站；规则追加到末尾，模板原有的安全规则（屏蔽私网、屏蔽 BT）与用户手写规则才保持更高优先级。
2. **block 规则排在 proxy 规则之前**（两个独立切片先后 append，与 `Priority` 无关）。违规域名封禁是硬约束，不能被某条分流规则绕过。
3. **绝不输出条件残缺的规则**（见上）。域名组挂上订阅后，`buildRule` 生成的 `domain` 条件是 `MergeDomains(手工, 订阅)` 的结果，「条件残缺」的空检查（`len(domains) == 0`）针对的是这个合并后的列表，不是任一单独字段——只要两者合起来非空，规则就照常生成。入站条件同理：`buildRule` 会剔除已删除/已禁用的入站，**剔完为空则整条丢弃**，绝不输出空的 `inboundTag`；只剔掉一部分时规则照常生成，被剔掉的记 `logger.Warning`。
4. **生成逐字节确定**：规则按 `priority asc, id asc`、出站按 `id asc`，`encoding/json` 对 map key 排序。**禁止遍历 map 来产生数组顺序**。

黑洞出站 `a-ui-block` 由注入器**始终自行注入**，不复用模板里的 `blocked`——用户可能把模板里那个删掉，而悬空 `outboundTag` 不报错，block 会静默变直连。所有生成的 tag 统一带 `a-ui-` 前缀，与手工模板隔离。

`buildOutbounds` 返回它**实际写入配置的** id→tag 映射，`buildRules` 必须消费这个映射而不是自己再查一次——否则一个 `Config` 损坏而被跳过的节点，其 tag 仍会被规则引用，形成悬空引用。

### 域名分流依赖 sniffing

路由要靠 xray 嗅探拿到 SNI/Host。入站若关掉 sniffing 或 `destOverride` 不含 `http`/`tls`，**域名规则永远不会命中且无任何报错**。新建入站默认 `enabled=true, destOverride=['http','tls']`（`web/assets/js/model/xray.js` 的 `Sniffing` 类），页面的规则列表会对不满足的入站打黄色警告图标。

### 输入侧校验（`web/service/routing_validate.go`）

出站配置与域名列表在**落库之前**交给真实 xray 校验，因此不需要事务回滚。出站的两个入口都接了：`OutboundNodeService.persist`（新建）与 `Update`（编辑）——**只堵一条等于没堵**，用户可以先建合法节点再编辑成坏的；域名组的新建与编辑共用 `encodeDomainsFromForm` 这一个收口。

**入站也走同一套校验**（`ValidateInboundReplacing`，接在 `InboundService.AddInbound` / `UpdateInbound`）。起因是一次真实事故：管理员在入站表单里开了 TLS 却没填证书路径，保存时一切正常，直到某次重启才发现——**xray 加载配置是全有或全无的**，这一个入站让整份 `bin/config.json` 加载失败，机器上所有用户一起断网，而面板首页只显示一个 `error`，看不出是哪个入站的问题。编辑时要传**旧** tag（改端口 tag 会变），否则候选对象会和库里那份自己撞名而被误拒；停用的入站同样校验，否则问题只是被推迟到它被启用的那一刻。

策略是 **fail open**：只有 xray 明确判定配置非法才拒绝；二进制缺失、老版本不认 `run -test` 参数、执行超时，一律放行并记日志。校验是辅助手段，不能因它自身故障就把用户锁在门外。

校验针对的是**完整生成配置**（设计 §5.4.2）：`validateWithFullConfig` 先 `GetXrayConfig()` 拿到「不做本次改动的话 xray 会拿到的那份配置」，把候选对象应用上去再送检。只包住单个对象的最小配置在原理上发现不了组合层面的冲突（与注入器发出的 tag 撞名、指向不存在出站的 `proxySettings.tag` 等），`a-ui-block` 撞名事故正是因此才一路没被拦住。

编辑出站走 `ValidateOutboundReplacing(ob, old.Tag)`，它会先把完整配置里那份旧的同 tag 出站摘掉，否则候选对象会和它自己撞名而被误拒；但**保留 tag 绝不摘**，否则一个 tag 为 `a-ui-block` 的脏数据节点会「校验通过」。

fail open 有三条边界，都不能收紧成拒绝：xray 自身故障（二进制缺失/老版本/超时）、取不到完整配置时退回最小配置校验、以及**改动之前配置就已经不合法时放行**——那不是本次改动的错，拒绝会把管理员锁在门外，连修复用的操作都做不了。

### `util/link` 包

移植自 3x-ui（`internal/util/link/`，GPL-3.0，与本项目同许可），负责把分享链接解析成 xray outbound。零第三方依赖，自带测试。支持 vmess / vless / trojan / ss / hysteria2 / wireguard，**socks 是本项目自行补充的**（`socks.go`）。文件头保留了来源声明，从上游同步更新时注意不要覆盖掉 `socks.go`。

## 安装向导与 Caddy 拓扑

`install.sh` 的向导（`setup_wizard`）在装好面板后问一次「有没有已解析到本机的域名」，答案决定面板此后的暴露方式；改这条链路前先读 `docs/superpowers/specs/2026-09-04-caddy-domain-bootstrap-design.md`，本节只讲落地后的约束和踩过的坑。

### 端口拓扑

有域名分支（`domain_flow`）：

```
:80/:443    Caddy   ACME 证书 + 80→443 跳转（308，不是 301）+ TLS 终止
              ├─ /<随机 basePath>/*  → 127.0.0.1:<面板端口>
              └─ 其它                → 伪装站（反代或本地静态页）
127.0.0.1:<面板端口>   a-ui   明文 HTTP，外网不可达
:2886 :2996 …          xray   各入站自己终止 TLS，证书路径来自 §「设置系统」的三个 defaultXxx 项
```

无域名分支（`reality_flow`）：面板监听随机高位端口 + 随机 basePath（不装 Caddy）；443 由 xray 的 VLESS+Vision+REALITY 入站独占，伪装目标是外部大站（`REALITY_TARGETS` / `xray.js` 的 `REALITY_TARGET_PRESETS`）。

两个分支都**不对外暴露面板本来的默认端口/路径**，但管理员在面板里创建的入站端口（`:2886` 这类）该开多少还是开多少——见下方「能力边界」。

### `a-ui bootstrap` 是脚本写库的唯一入口

安装脚本不直接碰 SQLite。所有落库动作（`webListen`/`webPort`/`webBasePath`/三个 `defaultXxx`、REALITY 模式下的入站）都经 `bootstrap.Run`（`bootstrap/bootstrap.go`），理由和「域名分流管理」里入站必须走 `ValidateInboundReplacing` 是同一个：脚本手拼的 JSON 没有任何校验，写错只会在下次重启 xray 时静默失效，而 `bootstrap` 建 REALITY 入站时走的是 `InboundService.AddInbound`，能拿到真实 xray 的校验与「域名分流管理」同一套 fail-open 策略。**不打印节点分享链接**——链接生成逻辑只存在于前端 `xray.js` 的 `genVLESSLink`，Go 侧重新实现一份必然与之漂移，节点由管理员在面板里创建后随时可复制。

`bootstrap` 靠 `webBasePath != "/"` 判断「是否已配置过」（`alreadyInitialized`，`bootstrap.go`），不靠 `webListen`——空字符串既是它的零值又是「监听所有 IP」的合法配置，两者区分不开。`a-ui update` 会 `rm -rf /usr/local/a-ui/` 重新解压，`install.sh` 每次都会调 `setup_wizard`，全靠这条幂等判断防止升级把管理员已经配好的域名/basePath 覆盖回默认值；主动想重新配置（`a-ui` 菜单「配置域名与伪装站」，即 `install.sh --wizard-only`）才传 `-force` 绕开它。

### 失败不锁面板

`bootstrap.Run` 里 `SetListen` **必须是最后一步**——改成 `127.0.0.1` 之后面板就只能经由 Caddy 访问，前面任何一步（Caddy 装失败、证书 60 秒未签发、伪装站预检全部不过、`a-ui bootstrap` 本身返回非零）失败都必须保持 `webListen` 原样，让面板继续监听所有 IP。真实吃过的亏：`mode=caddy` 分支一度没有清空面板自己的 `webCertFile`/`webKeyFile`——机器上若有历史遗留的证书路径，轻则 `AutoHttpsConn` 把 Caddy 转发来的明文连接误判成非 TLS、对每个请求回 307 造成死循环，重则证书文件已不存在导致 `tls.LoadX509KeyPair` 失败、`Server.Start()` 报错、`main.go` 只 `log` 一行就 `return`——进程以退出码 0 静默退出，而 `a-ui.service` 是 `Type=simple` 且没配 `Restart=`，面板从此彻底不再监听任何地址，且**当时唯一打印过的救援命令救不回来**（它只改 `webListen`，不碰这两个证书字段）。现在 `mode=caddy` 会在写 `webListen` 之前先清空这两项。

`install.sh` 侧同样贯彻这条原则：`write_caddyfile`/`wait_for_cert` 失败都直接 `return 1`、不调用 `a-ui bootstrap`；`domain_flow` 里 `a-ui bootstrap` 写完配置后还有一次 `wait_for_panel_alive`（真的对 `127.0.0.1:<port><basepath>` 发请求探活，不只看 `systemctl restart` 的返回码——`Type=simple` 无 `Restart=` 意味着「进程起来了又立刻退出」从 systemctl 的返回码上完全看不出来）。

两条救援命令由 `print_rescue_hint` 统一打印，`print_result`（成功收尾）内部调用它，`domain_flow`/`reality_flow` 里 `a-ui bootstrap` 调用点**之后**的每一条失败分支也各自调用一次——判据是「此刻 `webListen` 是否可能已经是 `127.0.0.1`」：`bootstrap` 调用之前的失败（域名为空、Caddy 装失败、伪装站预检不过等）不打印，那时 `webListen` 还没被动过，打印反而是噪音；`bootstrap` 已经跑过之后的失败（写入配置本身失败、写完配置后面板重启探活失败）都打印，因为要么这次调用本身已经把 `webListen` 改成了 `127.0.0.1`，要么这是一次覆盖已有配置的重装、`webListen` 在更早一次成功配置里就已经是 `127.0.0.1` 了——两种情况在脚本里区分不出来，宁可多打印一次不需要的提示，也不能在真正需要救援命令的时候把它吞掉：

```
a-ui setting -listen ""                        # 恢复监听所有 IP
ssh -L <端口>:127.0.0.1:<端口> root@<本机IP>    # 或走 SSH 隧道，端口用 a-ui setting -show 查
```

### 能力边界：不提升节点自身的抗探测能力

这次改造收窄的是**面板**的暴露面（明文 HTTP、固定端口、默认根路径 `/`），**不是**已创建入站的暴露面。`:2886` 这类 vmess+ws+tls 入站改造前后一样直接监听在公网端口上，浏览器直连会得到 400 或断连，这个特征没有变化，443 上的伪装站也保护不到它们——伪装站只接管 Caddy 自己监听的 80/443。安装完成的提示文案必须如实告知这一点（`print_result` 末尾那段），不能让管理员误以为「配了域名 = 节点也变安全了」。真正把入站收编到 Caddy 之后（明文 ws 监听 127.0.0.1、按随机 path 由 Caddy 分流）是设计文档里明确写的下一期，不在本次范围。

## 运维脚本

四个 shell 脚本必须成对维护，改一个就要同步另一个：

- `install.sh` / `install_en.sh` — 一键安装，从 GitHub Release 下载 tar.gz 解压到 `/usr/local/a-ui/`，安装管理脚本到 `/usr/bin/a-ui`，注册 systemd。有域名分支（`setup_wizard` → `domain_flow`）还会装 Caddy（官方源优先）并让它接管 80/443。若这两个端口已被 nginx/apache/caddy 占用，脚本走 `handle_existing_web_server`：列出对方当前服务的站点、备份其配置目录到 `/root/<name>-backup-<时间戳>.tar.gz`、**停用而非卸载**（`systemctl stop`+`disable`，软件包与配置全部保留，`systemctl enable --now <name>` 一条命令即可回滚），且必须输入完整的 `yes`（不是 `y`）才继续；占用者是未识别的进程则直接中止，不做任何猜测性操作。

**`a-ui update` 会把仓库里的 `bin/xray-*` 覆盖到用户机器上，所以那两个二进制的版本是发版内容的一部分。** `update()` 直接调 `install.sh`，而 `install.sh` 在解压前先 `rm -rf /usr/local/a-ui/`，再把发版包整个铺开——发版包里就带着 `bin/xray-linux-<arch>`（见 `release.yml` 的打包步骤）。后果是：管理员先前通过面板「安装 xray」升级过的核心，会在每次面板更新后被**降级回仓库里那一份**。

这条一直是隐性的，直到配置热更新上线才暴露：仓库里的两个 Linux xray 二进制从 `first commit` 起就没动过，是 Xray 1.4.x 时代（go1.16.2）的构建，**里面根本没有 `RoutingService` 符号**，路由热下发在它上面必然连不上、退回整进程重启（`tryHotApply` 的失败兜底按预期工作，所以不报错、只是功能静默失效）。v1.2.8 起已把它们更新到与 `go.mod` 里 `xray-core` 同 commit 的 26.7.28。

**因此升级 `xray-core` 依赖时，必须同时把 `bin/xray-linux-amd64` 与 `bin/xray-linux-arm64` 换成同版本的官方构建**，否则面板内的 `infra/conf` 与用户机器上实际运行的核心会错版。`web/service/xray_hot_reload_e2e_test.go` 的 `requireXrayRoutingService` 守着这条：核心不提供 `RoutingService` 时它跳过并说明原因，而不是以「PID 变了」这种和真实缺陷无法区分的形式失败。核对版本用 `go version -m bin/xray-linux-arm64`（读 Go 构建信息，不需要在目标平台上执行）。
- `a-ui.sh` / `a-ui_en.sh` — 安装后的管理菜单（0-17 项：安装/更新/卸载、重置账号密码、端口、启停、开机自启、BBR、acme 申请 SSL、定时任务）。同时支持 `a-ui start|stop|restart|status|log|update|clear|geo|cron` 等直接子命令。

脚本里硬编码了仓库地址 `SienFeng/AetherUI`，fork 后需一并修改。

## 已知偏差与注意事项

- **上游文档宣称的许多功能并不存在于本代码库**：无 Telegram bot（全仓库无任何相关代码）、无 Reality、无 `xtls-rprx-vision`、无客户端级流量统计与到期限制、无设备/IP 并发限制。前端每个协议表单只编辑 `settings.xxxes[0]`，即**一个 inbound 一个用户**。README 已于本仓库删除，**判断功能是否存在一律以代码为准**。
- 用户密码在数据库中**明文存储**，登录失败日志还会打印明文用户名密码（`web/controller/index.go`）。这是既有行为，涉及认证的改动请提高审查标准。
- `go.mod` 声明 `go 1.27.0`，CI（`.github/workflows/ci.yml`）用 `actions/setup-go` 的 `go-version-file: go.mod` 读同一个版本构建，不在工作流里另行硬编码。`xray-core` 锁定在 `v1.260327.1-0.20260728075948-5ca6f4b7d4dc`，与 `bin/xray-*` 的 26.7.28 是同一个 commit，**必须与 `bin/xray-*` 保持同版本**——它不再只是 gRPC stats 客户端：`xray/api.go` 的控制面热应用还依赖它的 `infra/conf` 把面板发出的 JSON 编译成 typed message 再下发给 gRPC，用旧版本的解析器编译不出新协议/新字段的配置。gin 仍是 1.7.1。代价是二进制体积从约 24 MB 增长到约 40 MB（darwin/arm64 本地实测约 39 MB）。
- `bin/xray-darwin-arm64` 在 `.gitignore` 中，macOS 本地跑面板需自行下载对应 Xray 二进制放入 `bin/`，否则 `RestartXray` 必然失败（面板本身仍可访问）。
- **`web.go` 的 `getHtmlTemplate` 吞掉 `ParseFS` 错误**（`// ignore`）。一个语法错误的模板会被静默跳过，直到渲染时才报 "template not found"。所以改完 `web/html/**` 光靠 `go build` 无法发现问题。`web/html_test.go` 的 `TestAllTemplatesParse` 走同样的遍历但不忽略错误，改完模板跑它即可。
- **Vue 指令写在根元素之外是死代码，且完全静默。** Vue 2 只编译 `el` 指向的那棵子树。分流页的三个 `<a-modal>` 曾整块落在 `<a-layout id="app">` 之后——页面渲染完全正常、数据也照常加载，但所有「添加 / 编辑」按钮点了毫无反应（`visible = true` 改的是没有任何绑定的数据），控制台不报任何错。弹窗要么留在 `#app` 内，要么照 `inbound_modal.html` 的做法给它自己的根元素和 `new Vue({el:'#xxx'})`。`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 对所有顶层页面守这条不变量（用 `golang.org/x/net/html` 解析渲染结果，比对 `v-*` / `@*` / `:*` 属性的位置）。
- **`a-tabs` 的非活动面板仍在 DOM 里**，只是被隐藏。写选择器或做页面自动化时必须限定到 `.ant-tabs-tabpane-active`，否则会命中隐藏面板里的同名元素。
- **面板里的「安装 xray」会连带覆盖 `bin/geoip.dat` 与 `bin/geosite.dat`**（`ServerService.UpdateXray` 从 zip 里一并解出），而这两个文件是仓库跟踪的。仓库当前这份来自 Xray 26.7.28，**含 OPENAI 类别**；更早的版本不含，会让 `geosite:openai` 直接报错。不要把它们还原成更旧的版本。
- **测试的工作目录**：`xray.GetBinaryPath()` 返回相对路径 `bin/xray-<GOOS>-<GOARCH>`，而 `go test` 的 cwd 是包目录。`web/service` 的 `TestMain` 因此 `os.Chdir` 到仓库根（这也与生产一致，systemd 的 `WorkingDirectory=/usr/local/a-ui/`）。这是**进程级副作用**：若今后在该包新增依赖包内相对路径（如 `testdata/`）的测试，请改用 `t.TempDir()` 或绝对路径。
- **面板报告的 xray 状态可能是假的，`bin/config.json` 现在也可能是假的。** `Process.Start()` 把 `cmd.Run()` 丢进 goroutine 后直接返回 nil，所以 xray 启动失败**不会**回传到面板。实测过一次配置冲突：xray 已经退出、全员断网，而 `/server/status` 仍返回 `state=running`、`errorMsg=""`。排查「面板说正常但用不了」这类问题时，**以 `pgrep` 为准，不要相信面板首页**。此前这里还建议「对 `bin/config.json` 跑一次 `xray run -test`」，热更新上线后这条不再可靠：`tryHotApply` 成功时只通过 gRPC 控制面把改动下发进正在跑的核心，**不会重写 `bin/config.json`**——该文件仍是上一次整进程重启（或面板启动）时写的那份，只有下一次真正触发重启才会被重新生成，中间这段时间它不反映核心的真实配置。确认核心当前真实状态，只能靠它的 gRPC 控制面（本项目目前没有现成的查询工具，`RoutingService.TestRoute` 落地前只能靠重启换回一份准确的 `bin/config.json`，或读面板日志里 `热应用：` 开头的 Debug 行辅助判断）。
- **SQLite 的自增主键 id 会被复用。** GORM 的 sqlite 驱动对 `primaryKey;autoIncrement` 生成的是 rowid 别名而非 `AUTOINCREMENT`，删掉最大 id 的行后，新插入的行会拿到同一个 id。任何「存 id 外键 + 被引用方可删除」的组合都要考虑这一点——旧引用会静默绑到新记录上，而且因为引用不再悬空，生成期的跳过防线也拦不住。分流子系统靠三个 `Check*Refs` 守卫堵住了这条路，**新增任何存 id 外键的表都要照做**。
- **cron 任务没有 panic 恢复。** `Server.Start()` 里 `cron.New(...)` 未配 `cron.WithChain(cron.Recover(...))`，`robfig/cron/v3` 自身也不 recover。定时任务（含每 10 秒的 xray 重启消费任务）里的任何 panic 都会**杀掉整个面板进程**。在这些路径上写代码要格外注意 nil map、越界等运行时 panic。
