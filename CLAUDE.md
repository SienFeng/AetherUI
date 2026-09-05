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
- `database/model/` — 主库早已不止 3 张表：`model.go` 的 User/Inbound/Setting 之外，还有 `routing.go` 的 DomainGroup/OutboundNode/RoutingRule 与 `ipban.go` 的 IPBan，共 7 张表。另有两张表各自落在**独立库**里——访问日志 `AccessLog`（`accesslog.go`，`database.InitAccessLogDB`）与用量历史 `TrafficBucket`（`traffic.go`，`database.InitTrafficDB`）——理由见「用量历史与图表」一节：高频写入不该和面板的普通操作抢主库那把 SQLite 写锁。Inbound 把 xray 的 `settings`/`streamSettings`/`sniffing` 原样存为 JSON 字符串，Go 侧不解析结构，由前端 `web/assets/js/model/xray.js` 负责建模。

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
RoutingRule   分流规则   InboundIds(JSON 数组) × DomainGroupIds(JSON 数组) → Action(proxy|block) + OutboundId + Priority + Enable
```

**「用户」在本项目里等价于「一个入站」**——前端每个协议表单只绑定 `settings.<protocol>es[0]`，所以一个 inbound 恰好一个用户。因此分流按 `inboundTag` 匹配，不需要 email 维度。

`DomainGroup` 的订阅五件套——`SubscribeUrl` / `SubscribedDomains` / `LastUpdatedAt` / `LastError` / `LastSkipped`——支撑域名组订阅更新（`web/service/routing_domain.go`、`routing_subscription.go`，`SubscriptionJob`）。两条不变量：

- **`Domains`（手工）与 `SubscribedDomains`（订阅）物理隔离，各自只有一个写入方。** 管理员编辑表单只写 `Domains`；`SubscriptionJob`/「立即更新」只写 `SubscribedDomains` 及其伴随的 `LastUpdatedAt`/`LastError`/`LastSkipped`。`buildRule` 用 `MergeDomains(manual, subscribed)` 在生成期合并两者（手工在前、订阅在后，保留首次出现），任何一侧都不会覆盖另一侧。
- **拉取失败保留上一次成功的数据，绝不清空。** 清空 `SubscribedDomains` 会让合并结果可能变空，`buildRule` 因「域名组为空」跳过整条规则——本该走指定节点或被封禁的流量静默退回直连，比规则单纯不生效更危险。同理，写回成功结果时要带 `subscribe_url` 相等的条件（compare-and-set）：拉取耗时可达 30s、批量刷新可达分钟级，这期间管理员可能已经把订阅地址改成了别的，不加条件的话旧地址拉到的内容会被当成新地址的结果写回，界面显示「刚刚更新」但域名其实是错的。

`DomainGroupService.Update` 不用 `Save` 写整行，而是拼一个只含实际要改的列的 `map[string]any`（`updateFieldsFor`），原因正是上一条：整行写入会把 `Get` 那一刻捕获的 `SubscribedDomains`/`LastUpdatedAt` 一并写回，把中间一次刚成功的订阅刷新静默回滚。**代价是这份列名单要手动维护**——将来给 `DomainGroup` 加字段，若不在 `updateFieldsFor` 里加对应的 key，这个字段就会静默地无法通过编辑接口更新（`Get`/展示不受影响，容易被漏测）。

五条不可动摇的字段约定：

- **规则存 `InboundIds`（入站 id 的 JSON 数组）而不是 tag 字符串。** 入站 tag 由端口算出（`UpdateInbound` 里 `Tag = fmt.Sprintf("inbound-%v", Port)`），用户改端口 tag 就变，存字符串会让规则静默失效。数组**升序去重**存储，这是「生成逐字节确定」的一部分。
- **`InboundIds` 为空数组 `[]` 表示对所有入站生效**（全局规则），生成时不输出 `inboundTag`。**注意空数组与「一个都没选」在提交体里无法区分**：写入路径用 `EncodeInboundIdsStrict` 挡住「非空输入被过滤成空」，前端 `saveRule` 也拦一道——否则一条本该覆盖某个人的规则会被静默放大到全体。
- **同一个域名组下，任何一个入站至多被一条规则覆盖**（`RoutingRuleService.checkConflict`，把空数组当全集做集合相交判定）。规则改为可引用多个域名组后，判定推广为「**域名组集合相交**且入站集合相交」，判定单位仍是「域名组 × 入站」的组合——同一个域名组可以被多条规则引用，只要覆盖的入站不重叠。禁用的规则同样占位。只在写入路径校验，生成期不干预：迁移前留下的冲突数据照常生成两条规则，由界面标黄交给管理员处理。
- **出站 `Tag` 一经分配即不可变**，且不能由自增 Id 拼出（unique 约束要求 INSERT 前就确定，那时 Id 尚未分配）。用 `link.SuggestTag("a-ui", remark, idx)` 生成，重名由调用方追加序号——注意 `SuggestTag` 只在 remark 为空时才用 `idx`，remark 非空时对任何 idx 都返回同一个值。
- **`a-ui-block` 是保留 tag，任何用户可控的 tag 分配都必须排除它。** 数据库唯一约束管不到它——注入器发出的 tag 不在 `outbound_nodes` 表里。备注写「block」（含 `Block`/`BLOCK`/`block!`/` block ` 等，`SlugRemark` 会把它们归一到同一个 slug）就会生成 `a-ui-block`，与注入器的黑洞出站撞名，xray 报 `existing tag found: a-ui-block` 并拒绝启动。判定收敛在 `model.IsReservedTag()` 一处，分配端（`allocTag`）、生成端（`buildOutbounds`）与校验端（`removeOutboundByTag`）都只认它，**新增保留 tag 只改这一个函数**。生成端也要排除，是因为修复前分配出去的脏数据仍可能躺在库里。

**`DomainGroupIds` 的空数组 `[]` 与 `InboundIds` 的空数组语义相反。** 入站的 `[]` 是合法的「所有用户」；域名组的 `[]` 是非法值——`domain` 条件为空会让 xray 把规则当作「不限制」，规则从「这批域名走 B」退化成「该用户全部流量走 B」，且返回 `Configuration OK`、面板首页显示 `running`。写入路径用 `EncodeDomainGroupIdsStrict`（对空结果一律报错，无论原始列表是否为空），`intersectGroups` 也**不能**复用 `intersectInbounds`（后者把空切片当全集）。这是本子系统里唯一一处「照抄隔壁的实现就会开洞」的地方。

**回退到旧版本二进制**：`domain_group_id` 列保留，单组规则行为完全正常；多组规则该值为 0，旧代码会整条丢弃——分流范围缩小而非放大，安全侧正确。设计文档在 `docs/superpowers/specs/2026-09-05-rule-multi-domain-group-design.md`。

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

### 配置导出 / 导入

域名组、出站节点、分流规则可分项或整包导出成 JSON 文件下载到本地，再上传导入到另一台机器。设计文档在 `docs/superpowers/specs/2026-09-05-routing-import-export-design.md`。

**导出文件不含任何 id**，跨表引用改写成业务键：出站用 `tag`（unique 且一经分配不可变）、域名组用 `remark`、入站用 `{remark, port}` 二元组（入站没有稳定业务键——`Tag` 由端口算出，`Remark` 可重复，`Id` 跨机器无意义且 SQLite 会复用）。

**域名组重名直接拒绝导出。** `Remark` 没有 unique 约束，重名会让导入端无法确定 `domainGroupRef` 指向哪一个；任何"猜"的策略猜错时都会产生一条指向错误域名组的规则，而规则表会渲染得完全正常、配置也会正常生成，只是流量走错节点——没有任何一层防线会发现。检查覆盖 `domainGroups` 与 `rules` 两个 scope（含 `all`）：前者导出的就是域名组本身，后者虽然不导出域名组、但规则里带着 `domainGroupRef`，重名同样会让导入端无法确定指向哪一个。`scope=outbounds` 与域名组完全无关，不检查——那会把管理员指向一个与他正在做的事毫无关系的地方。**导入侧对称处理这个歧义**：本机自身完全可能已经存在两个同名域名组（这不是导出侧的检查能挡住的，是导入目标机器自己的既有状态），导入侧同样绝不"猜"——取 id 最大或最小的那个都不行。引用了歧义备注的规则整条拒绝，理由与导出侧拒绝的理由完全相同：猜错不会有任何报错，规则表和生成的配置都渲染得完全正常，只是流量走错了节点。

**导入时入站认不全 → 规则导入为禁用，绝不清空 `InboundIds`。** 这是本功能最容易犯、后果最严重的错误：`[]` 表示「对所有入站生效」，一条本该只覆盖某个人的规则会被静默放大到全体，而 xray 对空 `inboundTag` 返回 `Configuration OK`。也不整条丢弃——规则的其余部分都是好的，导入成禁用状态并把缺失的入站点名，管理员勾一下就行。**唯一的例外**是已命中集合为空（一个入站都没认出来）：此时必须整条丢弃，哪怕导入成禁用的也不行。这条例外之所以「唯一」，是因为只有这一种情形会让编码结果落回 `[]` 这个具有特殊全局语义的值——「部分命中」（下面「两级匹配」段）或「两个引用塌缩到同一个入站」（同段）都只是让覆盖范围比预期小一点，禁用状态下并无危害；一旦命中集合真的空了，导入成禁用状态不是安全网，因为管理员一旦手滑启用，这条规则立刻变成对所有入站生效、全员中招。所以宁可整条丢弃让他重建，也不放一条「禁用时看着人畜无害、一启用就出事」的规则进库。

这条防线能立住，前提是导入端能可靠分辨「显式的空数组」与「字段缺失 / null」——**`PortableRule.InboundRefs` 因此是指针类型 `*[]PortableInboundRef`，不是值类型切片**：值类型下 `encoding/json` 把 null、键缺失、显式 `[]` 全部 unmarshal 成 `len()==0` 的 nil 切片，三者在 Go 侧完全无法区分，会把手工改过、别的工具生成的、传输被截断的文件误判成「显式表达的全局规则」而放行，让一条来路不明的规则以 `Enable=true` 落库并覆盖全体入站。导出侧 `toPortableRule` 保证永远输出非 nil 指针；导入侧遇到 nil 指针（对应 null 或字段缺失）一律整条拒绝，绝不当作全局规则处理。**改回值类型就会重新打开这个洞。**

**入站两级匹配**：先按 `remark` 精确匹配（恰好命中 1 条才算，重名视为无法区分），失败再按 `port`（有 unique 约束，命中即唯一）。备注优先是因为换机器后端口很可能改了。**两级匹配可能把两个不同的 ref 撞到同一个本机入站**（ref A 按 remark 命中，ref B 的 remark 失配、退到 port 又刚好撞上同一个本机入站）：`EncodeInboundIds` 会把重复 id 静默去重，`missing` 却是空的——一条本该覆盖 {甲, 乙} 的规则会缩小成只覆盖甲，报告里一个字都不会提。导入侧必须单独检测这种去重塌缩，按与「部分命中」相同的策略处理：导入成禁用状态、要求管理员手工确认，不能因为 `missing` 为空就误判成完全命中。

**冲突一律跳过，绝不覆盖。** 域名组按 `remark`、出站节点按 `tag` 判重。覆盖会静默改掉目标机器上正在跑的节点配置（两台机器的同名节点很可能指向不同落地服务器，这正是多机部署的常态），而界面上什么都不会变；跳过则让导入天然幂等，同一个文件导两次不会变成双份。代价是想更新已有节点只能手工编辑——批量覆盖生产节点配置本就该是比"上传个文件"更重的操作。

**导入的出站节点保留原 tag，但落库前必须 `model.IsReservedTag()` 拦一道。** 保留 tag 不在 `outbound_nodes` 表里，唯一约束看不见它们，撞名会让 xray 报 `existing tag found` 拒绝启动整份配置——全员断网而面板首页仍显示 `running`。这一道要排在 `ValidateOutbound` **之前**。同样要拦空 tag：空 tag 不会被 `ValidateOutbound` 拒绝，但会让 `xray/hot_diff.go` 的 `decodeOutbounds` 对整份配置判「必须重启」，出站热更新从此静默失效。**还必须要求导入的 tag 以 `a-ui-` 开头。** 手工新增路径的 `allocTag` 恒定产出 `a-ui-…`，所以「所有生成的 tag 统一带 `a-ui-` 前缀，与手工模板隔离」这条不变量此前在结构上就是成立的；导入是第一条能把任意 tag 写进 `outbound_nodes` 的路径，这条隔离第一次面临被打破的风险——`web/service/config.json` 模板里有一个 tag 为 `blocked` 的出站，撞名后果与保留 tag 撞名完全同形（`existing tag found`、全员断网、面板首页仍显示 `running`），但防线等级并不对称：保留 tag 有 fail-close 的 `IsReservedTag` 挡着，模板 tag 却只有 fail-open 的 `ValidateOutbound` 兜底。所以导入侧要求前缀，把这道 fail-close 的防线补回来；真实导出文件天然满足这个前缀，不影响正常使用。

**不导出 `SubscribedDomains`**（单个组可达十几万条，生产实例实测 `+111226`）**与 `LastUpdatedAt`/`LastError`/`LastSkipped`**（是本机这一次拉取的状态，搬过去会显示一个假的「刚刚更新」）。导入的订阅组 `LastUpdatedAt = 0`，而 `ShouldUpdateNow` 对 0 直接返回 true，`SubscriptionJob`（每 10 分钟）会自动补上首次拉取——**导入路径本身不同步拉取**，一个慢地址能把 HTTP 请求挂满 30 秒。代价是首次拉取成功前，仅依赖订阅内容的规则会被 `buildRule` 跳过，报告里要明说。

**不用事务。** 出站落库前要 exec 真实 xray 校验，包进事务会长时间持有 SQLite 写锁把整个面板卡住。逐条独立成败 + 逐条报告 + 幂等，重跑即补齐。

**分项导出不隐式扩大范围**：`scope=rules` 就只导规则，不带上它引用的域名组和出站节点——隐式扩大会让 `all` 和 `rules` 的区别消失。

**导入接口有 10MB 请求体上限。** 导入的每个出站节点都会触发一次 `ValidateOutbound`（一次 `GetXrayConfig()` + 1~2 次 exec 真实 xray），开销随条目数线性放大，而这是个同步 HTTP 请求；controller 是不可信输入的边界，与「用量历史与图表」一节 `getTrafficOverview` 对 `top` 的钳制同源。真实导出文件是几十 KB，10MB 极其宽松。

**规则的域名组引用是数组 `domainGroupRefs`，`domainGroupRef` 保留作单组时的兼容字段。** 导出侧单组时两个都写、多组时只写数组——这样新面板导出的文件放进旧面板，单组规则照常可用，多组规则被旧面板明确拒绝（「domainGroupRef 为空 → 整条跳过」），而不是静默产生一条指向错误组的规则。导入侧优先数组字段，为 nil 时回落单值字段；组认不出的策略与入站对称：部分认不出导入成禁用并点名，一个都认不出整条丢弃（编码结果会落回 `[]`，是上面那条非法值）。

### `util/link` 包

移植自 3x-ui（`internal/util/link/`，GPL-3.0，与本项目同许可），负责把分享链接解析成 xray outbound。零第三方依赖，自带测试。支持 vmess / vless / trojan / ss / hysteria2 / wireguard，**socks 是本项目自行补充的**（`socks.go`）。文件头保留了来源声明，从上游同步更新时注意不要覆盖掉 `socks.go`。

## 用量历史与图表

入站列表的展开行、系统状态页底部各有一张分时用量图。设计文档在 `docs/superpowers/specs/2026-09-04-traffic-history-design.md`。

**采集**复用 `XrayTrafficJob` 每 10 秒已经拿到的增量（`reset=true`，取完 xray 侧清零），在 `InboundService.AddTraffic` 累加进 `inbounds.up/down` 之前先记一份（`TrafficHistoryService.Record`）。不走「读累计值做差分」那条路：差分方案里，一次正常的「重置流量」和一次数据损坏长得一模一样。这也意味着**历史桶不受重置流量影响**——重置清的是累计计数器，历史记的是「某小时用了多少」，两者语义无关。

**数据**落在独立的 `/etc/<name>/<name>-traffic.db`（`config.GetTrafficDBPath()`，`database.InitTrafficDB`），与访问日志同样的分库理由：高频写入不该和面板的普通操作抢主库那把 SQLite 写锁。一张表（`model.TrafficBucket`）两级粒度（`Granularity` 字段，`GranularityHour`/`GranularityDay`）：小时桶与日桶**各自独立累加**，日桶不由小时桶汇总而来——汇总方案要处理「小时桶已被清理但日桶还没算」的补算逻辑。零增量不写行，图上的 0 由服务端补零。10 个入站跑满保留期约 1~2 MB。

**桶按面板设置的时区对齐**（`SettingService.GetTimeLocation()`，`model.AlignHour`/`AlignDay`），不是 UTC。UTC+8 下按 UTC 切日，看到的「某天用量」会整体错位 8 小时，且不报任何错。

**改时区后的后果比「交界处错一天」严重得多。** `History`/`Overview`（`web/service/traffic_history.go`）用 `bucket_start` 精确相等去 join `buildSlots` 按**当前**时区重算的刻度，旧桶按当时时区对齐、不会跟着改——两个集合在整小时时区切换（UTC ↔ Asia/Shanghai ↔ America/New_York 这类）下**不相交**：改动前的日桶全部落不到新刻度上，「1 年」档历史数据整体消失；改到/改离半小时或一刻钟时区（Asia/Kolkata、Asia/Tehran 等）连小时桶也全灭，24h/7d/30d 一并清空。`Overview` 排 Top N 的聚合查询不受对齐约束，错位的行照样能把入站顶进 Top 12，管理员看到的是十二条平的 0 线，观感像图坏了。数据没丢，保留期内随新数据自愈。刻意不做补偿——重切历史桶需要早已聚合掉的原始秒级数据，估算等于用假数据覆盖真数据。设计文档 §3.3/§8 有实测数据。

三条不能弱化的约束：

- **删除入站必须连带删除它的桶**（`TrafficHistoryService.DeleteByInbound`，接在 `InboundService.DelInbound` 里），另有 `TrafficCleanupJob`（每小时一次）里的 `PruneOrphans` 兜底。SQLite 会复用自增 id，残留的桶会绑到下一个建出来的入站上，那时引用不再悬空，图会渲染得非常合理，只是画的是别人的数据。
- **清理条件必须带 `granularity`**。两级各有各的保留天数设置项（`trafficHourRetentionDays` 默认 30、`trafficDayRetentionDays` 默认 365），不带的话一次「清理小时桶」会把日桶一起删掉，长期趋势图静默变空。
- **写用量历史失败只告警、不阻断 `AddTraffic`**。`inbounds.up/down` 是限额与到期判定的输入，它停止累加的后果（用户超额不被停用）比图上少一段曲线严重得多。

**前端**用 Chart.js v4.5.0 UMD（`web/assets/chart.js/`，MIT），只在 `inbounds.html` 与 `index.html` 单独引入，**不在 `common/js.html`**——那是所有页面共用的，登录页不该白下载这一份体积不小的库。两个页面共用 `web/assets/js/util/chart-util.js` 里的 `trafficChartOptions(maxTicksLimit)`：两张图除了 x 轴刻度上限（入站页传 12、系统状态页传 14）之外配置完全一致，不各自定义一份——分开定义迟早会分叉，而分叉后两张图的观感差异不会有任何东西提醒你。展开行里的 canvas 必须在 `$nextTick` 里取（`expandedRowRender` 是动态渲染的，指令执行时元素还不存在），折叠时必须 `chart.destroy()`（Chart 实例持有 canvas 引用与 resize 监听，不销毁的话反复展开折叠会一直累积，页面开几小时就会明显吃内存）。系统状态页的图**不挂进那个 2 秒的状态轮询循环**（`index.html` 的 `mounted`）：数据一小时才变一次。

服务端负责补零、对齐刻度、格式化 x 轴标签、排序 Top N，前端只管画；`top` 的上界钳制在 controller（`InboundController.getTrafficOverview`，1~50，越界回落 12）——controller 是不可信输入的边界，请求体里一个失控的数字不该让 service 拉出远超所需的系列。**标签在服务端格式化**是因为时区也在服务端：让浏览器自己格式化，访问者所在时区一变，图上的时间就和面板设置的时区对不上了。

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

### 伪装站：反代目标的三条硬判据

`MASK_SITES` 里的候选与用户自填的网址都要过 `check_mask_site`，三条判据都是被真机打出来的，不能放宽：

1. **地址不能带路径。** Caddy 的 `reverse_proxy` upstream 只接受 scheme/host/port（2.11.4 实测：`for now, URLs for proxy upstreams only support scheme, host, and port components`）。带路径的地址不在这里拦下，就会等到 `write_caddyfile` 才炸——那时旧的 web server 可能已经被停用，80/443 上什么都不监听。
2. **根路径必须直接 2xx，任何跳转（含同域）一律拒绝。** 曾经有过一版"跟随一跳、把解析后的地址拿去反代"的逻辑：它从落地起就跑不通，因为 `%{redirect_url}` 给出的绝对地址几乎必然带 `/`，写进 Caddyfile 直接校验失败；预置候选全在根路径 2xx，所以这条死代码一直没被触发。
3. **未知路径上不能跨域跳转。** `check_mask_site` 会对 `<候选>/aui-probe-<随机串>/` 单独探一次。只探根路径是不够的——伪装站被访问最多的恰恰是非根路径。`www.wikipedia.org` 就栽在这里：根路径干净的 200，任意未知路径回 `301 → https://en.wikipedia.org/<原路径>`，Caddy 把这个 `Location` 原样转发给访客，**一条响应同时说明"这不是那个站的域名"和"它在裸反代那个站"**。它已从 `MASK_SITES` 移除，不要加回来。

`write_caddyfile` 生成的伪装块还有一层防御纵深：`header_down Location "^https?://[^/]+(/.*)?$" "https://{host}$1"`，把上游任何绝对 `Location` 的 host 换成访客请求里的域名。预检只探两个路径、站点策略也会变，这层兜底覆盖剩下的路径。代价是上游若真要把访客送去第三方站点，重写后会形成重定向回环——刻意接受的取舍：一个看起来坏掉的页面，远好过一条把整套伪装当场证伪的响应。

### `a-ui bootstrap` 是脚本写库的唯一入口

安装脚本不直接碰 SQLite。所有落库动作（`webListen`/`webPort`/`webBasePath`/三个 `defaultXxx`、REALITY 模式下的入站）都经 `bootstrap.Run`（`bootstrap/bootstrap.go`），理由和「域名分流管理」里入站必须走 `ValidateInboundReplacing` 是同一个：脚本手拼的 JSON 没有任何校验，写错只会在下次重启 xray 时静默失效，而 `bootstrap` 建 REALITY 入站时走的是 `InboundService.AddInbound`，能拿到真实 xray 的校验与「域名分流管理」同一套 fail-open 策略。**不打印节点分享链接**——链接生成逻辑只存在于前端 `xray.js` 的 `genVLESSLink`，Go 侧重新实现一份必然与之漂移，节点由管理员在面板里创建后随时可复制。

`bootstrap` 靠 `webBasePath != "/"` 判断「是否已配置过」（`alreadyInitialized`，`bootstrap.go`），不靠 `webListen`——空字符串既是它的零值又是「监听所有 IP」的合法配置，两者区分不开。`a-ui update` 会 `rm -rf /usr/local/a-ui/` 重新解压，`install.sh` 每次都会调 `setup_wizard`，全靠这条幂等判断防止升级把管理员已经配好的域名/basePath 覆盖回默认值；主动想重新配置（`a-ui` 菜单「配置域名与伪装站」，即 `install.sh --wizard-only`）才传 `-force` 绕开它。

### 失败不锁面板

`bootstrap.Run` 里 `SetListen` **必须是最后一步**——改成 `127.0.0.1` 之后面板就只能经由 Caddy 访问，前面任何一步（Caddy 装失败、证书 60 秒未签发、伪装站预检全部不过、`a-ui bootstrap` 本身返回非零）失败都必须保持 `webListen` 原样，让面板继续监听所有 IP。真实吃过的亏：`mode=caddy` 分支一度没有清空面板自己的 `webCertFile`/`webKeyFile`——机器上若有历史遗留的证书路径，轻则 `AutoHttpsConn` 把 Caddy 转发来的明文连接误判成非 TLS、对每个请求回 307 造成死循环，重则证书文件已不存在导致 `tls.LoadX509KeyPair` 失败、`Server.Start()` 报错、`main.go` 只 `log` 一行就 `return`——进程以退出码 0 静默退出，而 `a-ui.service` 是 `Type=simple` 且没配 `Restart=`，面板从此彻底不再监听任何地址，且**当时唯一打印过的救援命令救不回来**（它只改 `webListen`，不碰这两个证书字段）。现在 `mode=caddy` 会在写 `webListen` 之前先清空这两项。

清空只堵住了安装向导这一条路径——管理员事后在设置页把证书路径填回去，一样会掉进同一个 307 死循环，而且掉进去之后没有退路（面板打不开改不回来；重装因 `alreadyInitialized` 整体跳过 `bootstrap`，不会重新清空）。所以 `entity.CheckValid` 里还有一道硬校验：**`webListen` 是回环地址且 `webCertFile`/`webKeyFile` 任一非空时，直接拒绝保存**。这条必须排在 `tls.LoadX509KeyPair` **之前**——只填了公钥或只填了密钥时加载必定失败，报出来的 `open : no such file or directory` 指向一个空路径，完全看不出真正的问题是「这个拓扑下根本不该填」。判据只认回环地址（`isLoopbackListen`，空串不算——空串是「监听所有 IP」）：无域名安装的 REALITY 分支不装任何反代（`install.sh` 的 `reality_flow` 里 `caddy` 出现 0 次，`bootstrap` 也不传 `-listen`），面板是公网明文 HTTP，这两项是管理员给它加 HTTPS 的唯一手段，**扩大到全部就是砍掉那条分支唯一的补救途径**。回归测试在 `web/service/setting_panel_tls_test.go`，正反两面都守着。

`install.sh` 侧同样贯彻这条原则：`write_caddyfile`/`wait_for_cert` 失败都直接 `return 1`、不调用 `a-ui bootstrap`；`domain_flow` 里 `a-ui bootstrap` 写完配置后还有一次 `wait_for_panel_alive`（真的对 `127.0.0.1:<port><basepath>` 发请求探活，不只看 `systemctl restart` 的返回码——`Type=simple` 无 `Restart=` 意味着「进程起来了又立刻退出」从 systemctl 的返回码上完全看不出来）。

两条救援命令由 `print_rescue_hint` 统一打印。判据是「此刻 `webListen` 是否可能已经是 `127.0.0.1`」，落成两条：

- **`a-ui bootstrap` 调用点之后**的每一条失败分支无条件打印（`print_result` 成功收尾也打）：要么这次调用本身已经把 `webListen` 改成了 `127.0.0.1`，要么这是一次覆盖已有配置的重装，更早一次成功配置里就已经改过了。
- **调用点之前**的失败分支走 `print_rescue_hint_if_force`：只有本次是 `force` 重跑（`--wizard-only`，即 `a-ui` 菜单「配置域名与伪装站」）时才打印。`force` 意味着面板此前已经被这个向导成功配置过一次，`webListen` 一定已经是 `127.0.0.1`，中途放弃并不会把它改回来。全新安装时 `webListen` 还是默认值、面板照常监听所有 IP，打印只是噪音。

这条判据一度只写了前半条，代价是一个真实的死角：`--wizard-only` 重跑时 `port_user 80` 返回的是**我们自己上一次装的 Caddy**，`handle_existing_web_server` 把它停用并 disable，`install_caddy` 对「已安装」只 `enable` 不 `start`，接着用户在选伪装站时输入 `q`——屏幕上最后一句是「已取消」，而机器上 Caddy 停着、面板在 `127.0.0.1`，**从外网彻底不可达**。所以 `domain_flow` 失败退出前还要做两件事：`restart_own_caddy_if_stopped`（先前停用的若正是我们自己的 Caddy 就把它拉回来；nginx/apache 不动——用户刚明确确认过要停它们），以及 `write_caddyfile` 成功后清掉 `stopped_web_svc` 标记（否则后续失败分支会打印「80/443 上没有任何服务在监听」这句与事实相反的话）。同理，`wait_for_cert` 失败时那句「为避免把你锁在面板外，不修改面板监听地址」在重跑场景里是**主动的错误安慰**，现在按 `force` 分岔成两套文案。

`--wizard-only` 的退出码必须如实透出（`setup_wizard force; exit $?`），`a-ui.sh`/`a-ui_en.sh` 的 `reconfig_domain` 也必须检查它——恒返回 0 会让上面整个状态一路静默回到主菜单：

```
a-ui setting -listen ""                        # 恢复监听所有 IP
ssh -L <端口>:127.0.0.1:<端口> root@<本机IP>    # 或走 SSH 隧道，端口用 a-ui setting -show 查
```

### 证书同步（`/root/cert/`）

Caddy 的证书存储路径含 ACME CA 的目录名，签发机构一换就变，所以面板与各入站引用的是固定路径 `/root/cert/`，由 `a-ui-cert-sync`（systemd timer，每小时）从 Caddy 的存储同步过去。两条不能退回去的写法：

- **先按 mtime 定位最新的证书目录，再从同一个目录取 cert 与 key。** 两次独立的 `find … | head -1` 会在某次续期从 Let's Encrypt 回落到 ZeroSSL（`certificates/` 下多出第二个 CA 目录）时各自命中不同目录：轻则 cert 与 key 配不上对，重则一直命中旧目录，`cmp` 每次都判「没变」，`/root/cert/` 的证书就此冻结到过期。这与 spec §8 记的那次「acme.sh 续期成功但 nginx 从没用上新证书」是同一个形状。同步脚本末尾的 `openssl x509 -checkend` 因此必须**无条件**跑，不能只在「这次真的复制了」时跑——冻结场景的表征恰恰是什么都没变。
- **同步没成功就不写 `defaultCertFile`/`defaultKeyFile`。** `domain_flow` 在调 `bootstrap` 之前用 `[[ -s ... ]]` 验收 `/root/cert/` 下的两个文件，不满足就干脆不传这两个参数。写了一个空路径进去，管理员此后新建任何 TLS 入站都会被 `ValidateInboundReplacing` 用一个他从没输入过的路径拒掉。

### 能力边界：不提升节点自身的抗探测能力

这次改造收窄的是**面板**的暴露面（明文 HTTP、固定端口、默认根路径 `/`），**不是**已创建入站的暴露面。`:2886` 这类 vmess+ws+tls 入站改造前后一样直接监听在公网端口上，浏览器直连会得到 400 或断连，这个特征没有变化，443 上的伪装站也保护不到它们——伪装站只接管 Caddy 自己监听的 80/443。安装完成的提示文案必须如实告知这一点（`print_result` 末尾那段），不能让管理员误以为「配了域名 = 节点也变安全了」。真正把入站收编到 Caddy 之后（明文 ws 监听 127.0.0.1、按随机 path 由 Caddy 分流）是设计文档里明确写的下一期，不在本次范围。

## 面板版本与一键更新

侧栏底部常驻版本号（`config.GetVersion()`），有新版打红点，点开可更新或回退到最近 5 个版本。设计文档在 `docs/superpowers/specs/2026-09-05-panel-version-update-design.md`。

**版本判定不做语义化解析。** 仓库 tag 格式不统一（`0.3.4.4` 与 `v1.2.10` 并存），字符串比较会把 `v1.2.9 > v1.2.10` 判反。改用 GitHub releases 列表的天然顺序：当前版本等于第 0 条即最新，在列表里且下标 > 0 即有更新，**不在列表里则既不打红点也不显示「已是最新」**（本地开发版 `config/version` 就是这种情况）。拉 `per_page=10` 但回退列表只给前 5 条——`KnownCurrent` 用全部 10 条判定，落后 6~10 个版本的管理员恰恰最需要看到红点。

**更新必须经 `systemd-run` 起独立 transient unit 执行，不能直接 `os/exec`。** 面板的子进程与面板同在 `/system.slice/a-ui.service` 这个 cgroup 里（实测确认 xray 就在里面），而 `install.sh` 会 `systemctl stop a-ui`，默认 `KillMode=control-group` 会把更新脚本一起杀掉——脚本死在 `rm -rf /usr/local/a-ui/` 前后，留下一台面板已删一半、服务已停、且因 `Restart=no` 不会自愈的机器，只能 SSH 上去手动重装。2026-09-05 实测：`systemd-run` 的 unit 在父 service 被 stop 后存活，而**加了 `setsid` 的对照组仍被杀死**——`setsid` 改的是会话不是 cgroup，不要拿它替代。

**tag 是命令注入路径，两道防线都必须硬拒绝。** 它会被拼进 `bash -c` 的字符串并以 root 执行：先过 `^[A-Za-z0-9._-]{1,64}$`，再必须精确出现在缓存的发布列表里。第二道顺带实现了「只能回退到最近 5 个版本」。这与 `routing_validate.go` 的 fail open 取向**相反且必须相反**：那里放行的是「没法证明非法」的配置，最坏后果是 xray 拒绝启动；这里放行的是一段以 root 执行的字符串。

**`Updatable` 前置检查挡住 Docker 与本地开发**（非 Linux / 找不到 `/usr/local/a-ui/a-ui` / 找不到 systemd 单元 / 没有 `systemd-run`）。在容器里跑 `install.sh` 是纯粹的破坏。`UnsupportedReason` 要具体到哪一条没过并原样显示给管理员。

**回退有两个后果，必须写进二次确认框**：① xray 核心会跟着回退——`install.sh` 解压的发版包带着 `bin/xray-linux-<arch>`，会覆盖机器上现有的那份（v1.2.8 之前的包里是 Xray 1.4.x 构建，没有 `RoutingService` 符号，配置热更新会静默失效）；② 数据库不回滚，`AutoMigrate` 只加列不删列，数据不丢但新功能失效。另有一条不写进 UI 但要记住的偏差：`install.sh` 无论装哪个版本，它自己和 `/usr/bin/a-ui` 都是从 **main 分支**拉的最新版，所以回退得到的是「旧二进制 + 新管理脚本」——改动 `a-ui bootstrap` / `a-ui setting` 的参数时要考虑这一点。

**回退的目标版本很可能根本没有 `/server/panelVersion`——`pollUpgrade` 必须把 404 与「连不上」分开处理。** 版本管理本身是 v1.6.0 才加的接口，回退列表里更早的版本一条都没有，所以这不是边缘情况而是回退的常态。面板 stop 期间不会出现 404：装了 Caddy 的拓扑下 upstream 拒绝连接是 502，没装 Caddy 的 REALITY 拓扑下连接直接被拒、axios 连 `response` 都拿不到。因此 404 是一个可靠信号——有进程正在这个地址上服务，只是它没有这条路由，据此直接判定「面板已就绪」。把 404 并回「连不上 = 正在重启」那一支（改动前就是如此），界面会白转满 3 分钟再报一句「更新可能失败」，而回退其实早就成功了：实测 v1.6.0 → v1.5.0 从下发到新进程就绪只用 4 秒。此路径下版本号无从核对，`done` 的文案**不能**沿用「更新完成，当前版本 X」——那里显示的 `panelVersion.current` 仍是更新前的旧值，照原样显示等于报一个假版本号，改为报 `upgradeTarget` 并说明侧栏版本入口会随之消失、此后只能 `a-ui update`。

**版本缓存不落库。** 新增设置项要同步改 5 处，漏掉 `models.js` 那一处会让**整个保存配置接口失败**；为一份重启后 10 秒内自愈的缓存付这个代价不划算。`PanelVersionJob` 每 6 小时刷新，`Server.startTask` 里另有一个延迟 10 秒的首次触发——`cron.AddJob` 的首次执行在一个完整周期之后，不做延迟触发新装的面板要等 6 小时才显示版本状态。

**拉取失败保留上一次成功的数据，只写 `LastError`。** 清空会让界面从「有新版可更新」退回「尚未检查」，且 tag 白名单变空会让更新按钮全部失效——一次网络抖动就把功能整个关掉。同理 `CheckedAt` 表示「上次**成功**检查」的时刻，不被失败的刷新改写。

**前端版本区走 Vue mixin（`web/assets/js/util/panel-version.js`）。** `common_sider.html` 被四个页面共用，但每个页面各有一个 `new Vue({el:'#app'})`，data 互不相干——少给一个页面挂 mixin，那个页面就会引用 undefined。mixin 的 `data` 是函数、根实例的 `data` 是对象，Vue 的 `mergeDataOrFn` 会正确合并，不用改现有页面的写法。

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
- **cron 任务的 panic 现在会被截住，但不是所有 job 都有第二层。** `web/web.go` 的 `cron.New(...)` 已配 `cron.WithChain(cron.Recover(cronLogger{}))`：任何挂在这个 cron 实例上的任务（含每 10 秒的 xray 重启消费任务）发生 panic，都会被这层截住、由 `cronLogger` 带完整堆栈记进面板日志，不再杀掉整个面板进程。目前 `access_log_job.go` 的两个任务、`concurrency_job.go`、`shaping_job.go`、`traffic_cleanup_job.go` 在各自 `Run` 的首行加了 `defer common.Recover("<任务名>")`（`util/common/err.go`）作为更早的一层——它抢在 cron 那层之前拿到 panic，日志里能带上具体任务名，而不是只知道「某个 job 挂了」；`check_inbound_job.go`、`check_xray_running_job.go`、`ipdb_update_job.go`、`subscription_job.go`、`xray_traffic_job.go` 还没有加这层，完全依赖 cron 那层通用兜底。**这不是「新 job 才有、旧 job 没有」的演进结果**——`ipdb_update_job.go`（没有这层）与 `concurrency_job.go`、`shaping_job.go`（都有）是同一个提交（`601a344`）加的，谁有谁没有只是各自实现时的疏漏，不代表任何时间线，不要据此推断「后来加的就补齐了」。**新增 job 一律照带 Recover 的写法办理**：`Run` 首行 `defer common.Recover("<任务名>")`。在这些路径上写代码仍要格外注意 nil map、越界等运行时 panic——多一层 recover 挡住的是「杀死整个进程」，挡不住「这一轮任务后续逻辑没跑完」。
