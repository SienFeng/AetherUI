# 域名分流管理 设计文档

日期：2026-09-02
状态：待评审

## 1. 背景与目标

当前面板只能管理入站（inbound）。所有流量统一走 `xrayTemplateConfig` 模板里的默认出站 `freedom`（直连），无法按用户区分去向。

要解决的场景：

> A 服务器上装了面板，有多个用户。管理员维护一批 ChatGPT 相关域名。
> 希望用户甲访问这批域名时走 B 节点，用户乙访问**同一批**域名时走 C 节点。
> 另有一批违规域名，对所有人直接黑洞掉。

目标是新增一个「分流管理」功能，让上述配置可以在 Web 界面完成，不必手写 xray 配置。

### 非目标

- 不做单端口多用户改造（见 §3.1 的维度选择）
- 不做出站流量统计
- 不做负载均衡 / 故障转移（balancer）
- 不做订阅 URL 自动拉取节点

## 2. 可行性验证

以下结论均由 Xray-core 26.3.27 实测得出（`xray run -test -c <config>`），非推断。

| 验证项 | 结果 |
|---|---|
| `inboundTag` + `domain` → `outboundTag` 三元匹配 | Configuration OK |
| `user`(email) + `domain` → `outboundTag` | Configuration OK |
| `geosite:openai` 内置域名集 | 可用 |
| socks 出站（带认证 / 免认证） | Configuration OK |
| 规则引用**已删除的入站** tag | Configuration OK，规则永不命中，无害 |
| 规则引用**已删除的出站** tag | Configuration OK，运行时静默回落默认出站 |
| 规则的 `domain` 为空数组 | Configuration OK，规则退化为匹配该入站**全部流量** |
| 完整目标形态（模板 + 2 入站 + 3 出站 + 全局 block + 按用户 block + 按用户 proxy） | Configuration OK |

**中间三行是本设计最重要的输入。** Xray 对悬空引用与空条件一律不报错，引用完整性只能由面板自己保证，否则会出现两类静默失败：

- 出站被删 → 规则回落默认出站，「以为封禁了，其实直连出去」
- 域名组被删 → 规则条件是 AND 关系，未指定的条件不参与匹配，`domain: []` 等于没有域名限制，规则从「该用户访问某批域名走 B 节点」**退化成「该用户全部流量走 B 节点」**

后者尤其危险：删一个域名组，等于把引用它的用户整个劫持到某个节点上。

另一个前提也已确认：`web/assets/js/model/xray.js:573` 中 `Sniffing` 默认 `enabled=true, destOverride=['http','tls']`。域名路由依赖嗅探拿到 SNI/Host，新建入站默认满足。

## 3. 数据模型

新增三张表，均走现有的 `db.AutoMigrate` 路径。

### 3.1 用户维度的选择

前端每个协议表单只绑定 `settings.<protocol>es[0]`（见 `web/html/xui/form/protocol/vless.html`），因此**当前架构下一个入站恰好对应一个用户**。分流按 `inboundTag` 匹配即可，无需引入 email 维度，也就不必改动入站的数据结构。

若将来做单端口多用户，规则表增加一个可选的 `client_email` 字段即可平滑扩展，本设计预留了这个空间但不实现。

### 3.2 表结构

```go
// 域名组：一批域名的可复用集合
type DomainGroup struct {
    Id      int    `json:"id" gorm:"primaryKey;autoIncrement"`
    Remark  string `json:"remark"`   // "ChatGPT"
    Domains string `json:"domains"`  // JSON 数组，元素为 xray 原生域名语法
}

// 出站节点：一个落地代理服务器
type OutboundNode struct {
    Id       int    `json:"id" gorm:"primaryKey;autoIncrement"`
    Tag      string `json:"tag" gorm:"unique"`  // 生成，不可编辑，见 §5.2
    Remark   string `json:"remark"`             // "B 节点 - 香港"
    Protocol string `json:"protocol"`           // vmess/vless/trojan/ss/socks/...
    Config   string `json:"config"`             // 完整 outbound JSON
    Enable   bool   `json:"enable"`
}

// 分流规则：一条连线
type RoutingRule struct {
    Id            int    `json:"id" gorm:"primaryKey;autoIncrement"`
    Remark        string `json:"remark"`        // "甲的 ChatGPT 走香港"
    InboundId     int    `json:"inboundId"`     // 0 = 所有入站（全局规则）
    DomainGroupId int    `json:"domainGroupId"`
    Action        string `json:"action"`        // "proxy" | "block"
    OutboundId    int    `json:"outboundId"`    // action=proxy 时有效
    Priority      int    `json:"priority"`
    Enable        bool   `json:"enable"`
}
```

### 3.3 三个关键设计点

**规则存 `InboundId` 而非 tag。** `InboundService.UpdateInbound` 里有：

```go
oldInbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)
```

入站 tag 由端口算出，用户改一次端口 tag 就变。存字符串 tag 会导致规则在改端口后静默失效。存 id，合成配置时实时查出当前 tag。

**`InboundId = 0` 表示全局规则。** 违规域名封禁通常对所有人生效，不该逐个用户配。合成时该字段为 0 就不输出 `inboundTag`，规则自然匹配所有入站。这对 proxy 同样有用（「所有人的 ChatGPT 都走 B 节点」）。

**域名组元素沿用 xray 原生语法**，不自造格式：`domain:openai.com`、`full:chat.openai.com`、`geosite:openai`、`regexp:.*\.oaistatic\.com`。好处是内置域名集直接可用，一行 `geosite:openai` 顶一批手工域名。

## 4. 配置合成

`xrayTemplateConfig` 手工模板**保持不动**，只在 `XrayService.GetXrayConfig()` 里做增量注入。该函数目前已经在把 inbounds 追加进模板，这里再加两步：

```
模板 JSON
  ├─ inbounds       ← 追加 enable 的入站            （现有逻辑）
  ├─ outbounds      ← 追加 enable 的出站节点 + a-ui-block  （新增，append 末尾）
  └─ routing.rules  ← 追加 block 规则、再追加 proxy 规则     （新增，append 末尾）
```

`a-ui-block` **始终注入**，与当前是否存在 block 规则无关。让出站集合只取决于「出站节点表的内容」这一个变量，生成逻辑和确定性判断都更简单；多一个未被引用的黑洞出站没有任何副作用。

### 4.1 为什么都追加到末尾

**出站追加到末尾**：Xray 取 outbounds 数组的第一个作为默认出站。模板里第一个是 `freedom`。若插到前面，所有未匹配流量会突然全部走某个节点。

**规则追加到末尾**：模板原有的安全规则（`geoip:private` → blocked、bittorrent → blocked）与用户手写规则保持更高优先级。本功能纯增量，不改变任何现有行为。

最终规则顺序：

```
[模板原有规则]  →  [block 规则]  →  [proxy 规则]
```

block 强制优先于 proxy：违规域名封禁是硬约束，不应被某条分流规则绕过。同类规则内部按 `Priority, Id` 排序。

### 4.2 重启检测

`xray.Config.Equals()` **不需要修改**。`OutboundConfigs` 和 `RouterConfig` 都是 `json_util.RawMessage`，注入后字节内容改变，现有的 `bytes.Equal` 天然能察觉并触发重启。

代价是**生成过程必须确定性**：规则按 `Priority, Id` 稳定排序，域名保持录入顺序，map 序列化依赖 `encoding/json` 对 key 的排序保证。否则每次生成的字节都不同，`Equals` 恒为 false，那个 10 秒 cron 会反复重启 xray。这是实现时必须锁死的不变量，需有测试覆盖。

## 5. 安全设计

### 5.1 block 不复用模板里的 `blocked`

模板中的黑洞出站 tag 为 `blocked`，但模板是用户可在「xray 相关设置」中自由编辑的。一旦被删除，所有 `outboundTag: "blocked"` 成为悬空引用——而 §2 已实测 Xray 对此不报错，运行时回落到默认出站 `freedom`，即**直连**。

后果是最坏的失败模式：违规域名看似被封禁，实际畅通无阻，且无任何报错。

因此面板**自行注入专属黑洞出站 `a-ui-block`**，block 能力完全自洽，不受用户改模板影响。

### 5.2 tag 命名空间隔离与生成方式

所有生成的出站 tag 统一加 `a-ui-` 前缀，与手工模板的 tag 彻底隔离，杜绝撞名。

tag **不能基于自增 Id 生成**：`Tag` 有 unique 约束，必须在 INSERT 时就确定，而此时 Id 尚未分配。改用移植包提供的 `SuggestTag("a-ui", remark, idx)`——由 remark 转 slug 加序号得出，与 Id 无关，撞名时递增 idx 重试。

tag 一经生成即固定，不随 remark 改名而变。否则改一次备注就会让所有引用它的规则悬空——而 §2 已确认 Xray 对此不报错。

### 5.3 引用完整性：两道防线

§2 证明 Xray 不会替我们兜底，所以采用「入口拦截 + 生成期兜底」双保险。

**第一道 · 删除时拒绝**

- 删除**出站节点**前检查是否被规则引用，被引用则拒绝并列出规则，避免 §5.1 的静默降级为直连。
- 删除**域名组**前同样检查，避免规则退化为「劫持该用户全部流量」。

**第二道 · 生成期跳过**

即便第一道被绕过（直接改库、并发删除、历史脏数据），合成配置时必须再兜一次：

- 规则的域名组不存在，或展开后域名列表为空 → **整条规则跳过，不输出**
- 规则的出站节点不存在或已禁用 → **整条规则跳过，不输出**

宁可规则不生效（用户能察觉），也绝不输出一条条件残缺的规则（静默劫持全部流量，用户无从察觉）。这条不变量需有测试覆盖。

### 5.4 另外两个防呆

1. **入站 sniffing 关闭时警告**。`destOverride` 不含 `http`/`tls` 时路由拿不到域名，域名规则形同虚设。规则列表对这类入站打标提示。
2. **保存前配置试跑**。生成完整配置后先用 `xray -test` 校验再落库，避免写坏配置导致 xray 起不来。

## 6. 代码复用

移植 3x-ui（`/Users/caryallen/Desktop/3x-ui-main`）的 `internal/util/link/` 包，用于分享链接解析。

**移植依据**（已实测）：

- 零第三方依赖，在独立模块中 `go mod tidy` 不引入任何东西
- 自带测试（`outbound_test.go` 424 行 + `outbound_helpers_test.go` 246 行 + fuzz）在 Go 1.21 独立模块下全部通过
- 输出即 xray outbound 的 wire format，包注释明确说明可直接注入
- 双方均为 GPL-3.0，许可证兼容

**入口**：`ParseLink(link string) (*ParseResult, error)`，附带 `SuggestTag(prefix, remark, idx)` 与 `SlugRemark()`，正好配合 §5.2 的前缀方案。

**已覆盖**：vmess / vless / trojan / ss / hysteria2 / wireguard；传输 tcp / kcp / ws / grpc / httpupgrade / xhttp + tls / reality。

**需要补充**：`parseSocks`。原包不支持 socks。链接格式无统一标准，解析按以下顺序回退：

1. `socks://<base64(user:pass)>@host:port#remark`（v2rayN 格式）
2. `socks://<base64(user:pass@host:port)>#remark`（旧格式）
3. `socks5://user:pass@host:port` / `socks://host:port`（裸格式，免认证时无凭据部分）

产出 xray socks 出站结构（已实测通过）：

```json
{ "tag": "a-ui-node-3", "protocol": "socks",
  "settings": { "servers": [{ "address": "1.2.3.4", "port": 1080,
    "users": [{ "user": "alice", "pass": "secret" }] }] } }
```

免认证时省略 `users`。

**移植代价**：`go.mod` 的 go 指令由 `1.16` 提升至 `1.21`（包内 84 处 `any` 属 Go 1.18 语法，1 处 `maps.Copy` 属 1.21）。CI 使用 Go 1.22（`.github/workflows/release.yml`），本地为 1.27，均满足。提升 go 指令只影响本模块的语言版本，不影响既有依赖的编译。

**不复用的部分**：

- 前端全部。3x-ui v3 为 Vue3 + TypeScript + Vite，AetherUI 为 Vue2 + Go 服务端模板，技术栈不可通约。
- `internal/xray/geodata`。依赖 `xtls/xray-core/common/geodata` 与 protobuf，而 AetherUI 锁定 xray-core v1.4.2，版本过老；且非核心需求，用户直接输入 `geosite:xxx` 字符串即可。
- 3x-ui 的路由管理方式。它不建独立表，把 outbounds/routing 直接存模板 JSON，靠可视化编辑器操作。移植到 Vue2 工作量巨大，且不满足域名组复用需求。

## 7. UI 结构

侧边栏在「入站列表」下方新增一级菜单「分流管理」，页面内分三个 tab。

| tab | 内容 |
|---|---|
| 域名组 | 列表 + 增删改。域名用 textarea 一行一条，保存时校验语法前缀 |
| 出站节点 | 列表 + 增删改。新增时粘贴分享链接自动解析；另有 JSON 高级模式兜底 |
| 分流规则 | 列表。每行一条连线：入站（含「所有用户」选项）× 域名组 × 动作（走节点 / 阻断），可调顺序 |

沿用现有的 ant-design-vue 1.7.2 + Vue 2 写法，模板放 `web/html/xui/`，遵循既有的 `[[ ]]` 插值分隔符约定。

## 8. 后端接口

仿照现有 `InboundController` 的写法，挂在 `/xui/routing/` 下：

```
POST /xui/routing/domain-group/list
POST /xui/routing/domain-group/add
POST /xui/routing/domain-group/update/:id
POST /xui/routing/domain-group/del/:id

POST /xui/routing/outbound/list
POST /xui/routing/outbound/add
POST /xui/routing/outbound/update/:id
POST /xui/routing/outbound/del/:id
POST /xui/routing/outbound/parse      // 分享链接 → outbound JSON 预览

POST /xui/routing/rule/list
POST /xui/routing/rule/add
POST /xui/routing/rule/update/:id
POST /xui/routing/rule/del/:id
```

所有写操作成功后调用 `xrayService.SetToNeedRestart()`，复用既有的「标志位 + 10 秒 cron 消费」机制，不新增定时任务。

## 9. 改动文件清单

新增：

```
util/link/outbound.go            移植自 3x-ui，保留原版权声明与出处
util/link/socks.go               新增 parseSocks
util/link/*_test.go              移植自 3x-ui
database/model/routing.go        三个模型
web/service/routing.go           三组 CRUD + 配置片段生成
web/controller/routing.go        路由注册与参数绑定
web/html/xui/routing.html        页面
web/html/xui/form/routing_*.html 三个 tab 的表单片段
```

修改（均为增量，不改既有行为）：

```
go.mod                       go 指令 1.16 → 1.21
database/db.go               +3 个 AutoMigrate
web/service/xray.go          GetXrayConfig 注入 outbounds 与 routing.rules
web/web.go                   注册 RoutingController
web/controller/xui.go        +页面路由
web/html/xui/common_sider.html  +菜单项
```

## 10. 测试策略

项目当前零测试。本功能建议破例补上，理由是配置合成一旦出错就是**静默的**——Xray 不报错，流量默默走错节点或直连出去。

可测且值得测的部分：

- **配置合成**（`web/service/routing.go`）。本质是纯函数：模板 + 三张表数据 → JSON。覆盖：出站追加位置、规则顺序（block 先于 proxy）、`InboundId=0` 不输出 `inboundTag`、生成的确定性（同样输入两次生成字节相同）。
- **引用完整性**（§5.3 两道防线）。删除被引用的出站节点 / 域名组应被拒绝；且当规则的域名组为空、出站不存在或已禁用时，生成的配置中**不得出现该规则**——这是防止静默劫持全部流量的关键不变量。
- **socks 解析**。新写的 `parseSocks` 三种格式回退。
- **移植包自带测试**原样保留。

`database.InitDB(dbPath)` 接受任意路径（只有 `config.GetDBPath()` 硬编码），因此用 `t.TempDir()` 即可做集成测试，无需先重构。

## 11. 风险

| 风险 | 处置 |
|---|---|
| 域名组/出站被删，规则退化为劫持全部流量或直连 | §5.3 双防线：删除时拒绝 + 生成期跳过残缺规则 |
| 生成非确定性导致 xray 被反复重启 | 稳定排序 + 「同输入两次生成字节相同」的测试 |
| 用户改坏模板导致注入失败 | 注入前校验模板可解析，失败则保持原配置并报错 |
| 用户装了老版 xray，不支持 hysteria2/wireguard 出站 | §5.4 的 `xray -test` 试跑会拦截 |
| go 指令提升影响构建 | CI 已是 1.22；本地 1.27。不影响既有依赖 |
| 移植代码的许可证合规 | 双方均 GPL-3.0；文件头保留原始版权声明与来源链接 |

## 12. 参考

- 上游复用来源：3x-ui（MHSanaei/3x-ui，GPL-3.0），`internal/util/link/`
- Xray 路由文档：`routing.rules` 的 `inboundTag` / `domain` / `outboundTag` 字段
- 实测环境：Xray-core 26.3.27 darwin/arm64
