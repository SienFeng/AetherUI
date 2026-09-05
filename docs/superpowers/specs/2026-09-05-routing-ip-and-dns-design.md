# 分流能力扩展：域名写法、IP 分流与内置 DNS 设计文档

日期：2026-09-05
状态：待评审

> 本文扩展 `2026-09-02-domain-routing-design.md` 的 §3（数据模型）、§5（两道防线）与 §6（配置注入的不变量），扩展 `2026-09-02-domain-subscription-design.md` 的订阅解析，扩展 `2026-09-05-routing-import-export-design.md` 的 `PortableDomainGroup`。其余内容继续有效。
>
> 凡与既有设计结论一致之处，本文直接引用而不重述理由；凡引入**新的不变量**之处，逐条给出它防的是哪一类静默事故。

## 1. 背景与目标

三件事，来自同一次管理员反馈：

1. **域名写法支持不全。** 从小火箭配置里整段复制过来的域名列表，`openai`、`chatgpt-async-webps` 这类 `DOMAIN-KEYWORD` 转写被面板拒绝，整段粘贴失败。
2. **不支持 IP 分流。** 订阅一份 Surge 规则集时，其中的 `IP-CIDR` 全部被跳过（一份真实规则集里有 1587 条）。
3. **没有 DNS 设置。** 面板不提供任何入口控制 VPS 上那次域名解析走谁。

目标：把这三条补齐，且**升级后默认行为零变化**——不配置就等于现在。

### 非目标

- 不做按客户端 IP 分流（xray 的 `source` 条件）。那是「谁能连进来」，已由入站地区限制覆盖，与「往哪里出去」是两件事。
- 不做 ASN 匹配。xray 没有这个能力，不猜、不近似。
- 不做 DNS 的分域名解析（`dns.servers` 的对象形态 + `domains` 过滤）。见 §8 的可扩展性说明。
- 不改动出站节点、动作、优先级、冲突判定的语义。
- 不把入站收编到 Caddy（那是 `2026-09-04-caddy-domain-bootstrap-design.md` 的下一期）。

## 2. 上游事实核实

以下全部由 `go.mod` 锁定的 `xray-core v1.260327.1-0.20260728075948-5ca6f4b7d4dc` 源码确认，不是推断。本子系统的每一条设计决策都挂在这张表上。

| # | 事实 | 出处 |
|---|---|---|
| F1 | 域名写法共 8 种前缀：`regexp:` `domain:` `full:` `keyword:` `dotless:`，以及 `geosite:` `ext:` `ext-domain:` `ext-site:` | `common/geodata/rule_parser.go:226` |
| F2 | 无前缀的裸字符串取 `defaultType`，而 routing 传的是 `Domain_Substr`，即**子串匹配** | `infra/conf/router.go:175,183` |
| F3 | 域名匹配只把**目标域名**转小写，不归一化配置里的模式 → 大写的模式永不命中 | `app/router/condition.go:59,68` |
| F4 | 同一条规则内的各类条件是 **AND**（`ConditionChan` 全真才通过） | `app/router/config.go:33`、`app/router/condition.go:35` |
| F5 | 一条规则一个条件都没有时 xray 才报 `this rule has no effective fields`；只要还剩一个条件（比如 `inboundTag`）就照常加载 | `app/router/config.go:114` |
| F6 | `domainStrategy: IPIfNonMatch` 的实现是「先按原样跑一遍全部规则，**一条都没命中**才解析域名跑第二遍」 | `app/router/router.go:245,261` |
| F7 | 第二遍解析失败时 `GetTargetIPs()` 返回 nil，IP 规则不匹配，最终返回 `ErrNoClue` → 默认出站。**DNS 挂了不会断网** | `features/routing/dns/context.go:21` |
| F8 | `freedom` 只在自己的 `domainStrategy.HasStrategy()` 为真时才调 `internet.LookupForIP`；默认 `AsIs` 不走这条路 | `proxy/freedom/freedom.go:290` |
| F9 | `internet.LookupForIP` 打的是 xray **内置 DNS 客户端**，即 `dns` 段 | `transport/internet/dialer.go:87` |
| F10 | `UseIP*` 是 strategy code 1，解析失败只记日志、回落按域名直连；`ForceIP*` 是 code 2，解析失败直接断连接 | `transport/internet/config.go:13,106`、`proxy/freedom/freedom.go:298` |
| F11 | 没有 `dns` 段时 xray 注册 `localdns.New()`（系统解析器）作为默认 DNS 客户端 | `core/xray.go:216` |
| F12 | IP 条件的语法：CIDR、裸 IP（等价 `/32`、`/128`）、`geoip:xx`、`geoip:!xx`、`ext:file:tag`，`!` 前缀取反 | `common/geodata/rule_parser.go:16,102` |

两条推论，是本文大部分不变量的来源：

- **F4 决定了 IP 条件不能与域名条件共处一条规则。** 「这批域名**或**这批 IP 走 B」必须拆成两条规则；写成一条会变成「域名命中**且**解析出的 IP 也命中」，几乎永不命中，而 xray 返回 `Configuration OK`、面板首页照样显示 `running`。这与既有设计文档记录的「xray 会静默接受错误配置」是同一族事故。
- **F5 决定了 `ip: []` 与 `domain: []`、`inboundTag: []` 完全同构。** 空数组在 protobuf 里长度为 0，条件整个消失，规则退化成只由剩下的条件约束——范围被静默放大。

## 3. 分期与依赖

| 期 | 内容 | 依赖 |
|---|---|---|
| **P1** | 域名写法放宽（§4） | 无 |
| **P2** | 分流组的 IP 段、ip 规则生成、订阅收 IP、导入导出、`ipRuleResolveDomain` 开关（§5–§7） | 无 |
| **P3** | DNS 设置（§8） | 无 |

三期互不依赖，可独立发布、独立回退。

`ipRuleResolveDomain` 归入 P2 而非 P3：由 F11，`IPIfNonMatch` 在没有 `dns` 段时用系统解析器，**不依赖 DNS 设置**。把它留到 P3 会让 P2 交付一个只覆盖 IP 字面量的半成品。

## 4. P1：域名写法放宽

### 4.1 现状与缺口

`web/service/routing_domain.go:17` 的 `domainPrefixes` 只放行 5 种，缺 F1 里的 `keyword:` `dotless:` `ext-domain:` `ext-site:` 与裸字符串。

已经存在一处**两条路径不一致**：`convertSubscriptionLine`（`web/service/routing_subscription.go:76`）处理 `DOMAIN-KEYWORD` 时返回的正是裸关键词，落库后生成配置完全正常。即同一个 `openai`，从订阅进来能用，管理员手输被拒。手工路径比订阅路径更严，本身就是缺陷。

### 4.2 决策

前缀白名单补齐到 F1 的全集。裸字符串按**歧义与否**分岔：

| 输入 | 处理 | 理由 |
|---|---|---|
| 不含点、不含 `:`（`openai`、`chatgpt-async-webps`） | 放行，归一存成 `keyword:openai` | 不可能是域名，意图唯一 |
| 含点（`openai.com`） | **拒绝**，报错点名 `domain:` / `full:` / `keyword:` | 见 4.3 |
| 含 `:` 但前缀不认识（`wat:openai.com`） | **拒绝**，列出全部可用前缀 | 由 F2 它会变成子串匹配，而 SNI/Host 不含冒号 → 永不命中的哑规则 |

### 4.3 为什么含点的裸串必须拒绝

裸串在两个生态里语义相反：xray 的 routing 规则里是**子串**（F2），而 geosite 数据文件（domain-list-community）里是**后缀**。管理员从 geosite 列表复制来的 `openai.com`，放行等于静默生成一条能命中 `notopenai.com.evil.net` 的规则，且没有任何一层会报错。

拒绝是廉价的：报错里点名三种写法，管理员补个前缀即可；放行的代价是一条看不出问题的错规则。

### 4.4 大小写归一

由 F3，`domain:OpenAI.com` 是永不命中的哑规则。落库前对**值**转小写，但只限 `domain:` / `full:` / `keyword:` / 裸串。

`regexp:` 与 `dotless:` **原样透传**——它们会被编译成正则，转小写会把 `\D` 变成 `\d` 这种意义完全相反的东西。`geosite:` / `ext:*` 的 code 由 xray 自己 `ToUpper`（`rule_parser.go:211`），不碰。

### 4.5 顺带消掉订阅侧的不一致

`convertSubscriptionLine` 的 `DOMAIN-KEYWORD` 分支改为吐 `keyword:`。不改则同一个关键词在库里有两种形态，`MergeDomains`（`routing_domain.go:290`）按字符串去重，去不掉。

代价：下一次订阅刷新会重写所有订阅组的 `SubscribedDomains`，配置字节变化 → **触发一次整进程重启**。一次性，之后稳定。

## 5. P2：分流组的 IP 段

### 5.1 数据模型

`model.DomainGroup` 加两列，与域名侧严格对称：

```go
// Cidrs 是管理员手工录入的 IP 段，JSON 字符串数组，元素为 xray 原生 ip 语法：
// 1.2.3.0/24 / 2001:db8::/32 / 8.8.8.8 / geoip:cn / geoip:!cn / ext:file:tag
Cidrs string `json:"cidrs" form:"cidrs"`
// SubscribedCidrs 是上一次成功拉取解析出的 IP 段。与 Cidrs 物理隔离，
// 理由与 SubscribedDomains 完全相同：两个字段各自只有一个写入方，永不交叉。
SubscribedCidrs string `json:"subscribedCidrs" form:"subscribedCidrs"`
```

「域名组」升级为「分流组」：一个组同时装域名和 IP。这样一份 Surge 规则集（域名与 IP-CIDR 本就混排）订阅一次同时喂两边，管理员不必手工拆成两个组再分别引用。规则的引用关系、冲突判定、`Check*Refs` 守卫**全部不变**。

订阅五件套（`SubscribeUrl`/`LastUpdatedAt`/`LastError`/`LastSkipped`）共享。`LastSkipped` 语义扩展为「既非域名也非 IP 的条数」。

### 5.2 三条必须照搬的既有不变量

1. **`updateFieldsFor`（`routing_domain.go:123`）必须加 `cidrs`。** 既有注释已警告：漏加会让该字段静默地无法通过编辑接口更新，而 `Get` 与展示完全正常，极易漏测。
2. **订阅地址变更时 `subscribed_cidrs` 与 `subscribed_domains` 一并清空**、`last_updated_at` 一并置 0。旧地址拉来的 IP 继续参与分流，是「用错误的数据生效」。
3. **拉取失败绝不动这两个字段**（`recordFailure` 的既有约束）。

### 5.3 一条新的不变量：成功时两个字段都写

订阅成功时**必须同时写 `subscribed_domains` 与 `subscribed_cidrs`，哪怕其中一个是空字符串**。

这不与 5.2 第 3 条冲突：那条约束的是**失败**路径。成功路径上，订阅源真的不再列 IP 了，保留上一次的 IP 就是拿过期数据分流——比 IP 条件消失更危险。

后果要接受：一个只靠订阅提供 IP 的组，在上游删光 IP 后其 ip 规则会被 `buildRule` 跳过并记 Warning。这是正确行为。

### 5.4 IP 段的校验

镜像 F12 的语法。落库前另走一遍 `ValidateCidrs`（真实 xray，与 `ValidateDomains`（`routing_validate.go:208`）同构：把候选值挂在一条追加到末尾的探针规则的 `ip` 条件上，出站指向注入器始终会注入的 `a-ui-block`，因此不引入悬空引用）。沿用 `validateWithFullConfig` 的 fail open 三条边界，不收紧。

### 5.5 内联，不写 dat 文件

生成的 `ip` 数组直接内联进配置，不复用 `util/geodat` 写 `bin/a-ui-geo.dat`。

两个理由：域名侧现在就把十万条订阅域名直接内联（既有文档记录过 `+111226` 的生产实例），IP 侧没有理由更保守；且 `bin/a-ui-geo.dat` 已有一个写入方（`geo.go` 的 `buildGeoRules`，且它在没有入站配地区限制时直接返回、根本不写文件），再加一个写入方就要处理互相覆盖与「谁负责写」的时序。内联把这个风险整个消掉。

将来若确实撞到配置体积问题，再迁到 dat 是纯生成端改动，不影响数据模型。

## 6. P2：配置注入

### 6.1 一条数据库规则 → 0~2 条 xray 规则

`buildRule`（`routing_inject.go:211`）的返回值从「一条规则」改成「0~2 条规则」：

```
合并所有被引用组的 domains 与 cidrs
  跨组按 DomainGroupIds 升序，组内手工在前、订阅在后，均保留首次出现
两者皆空 → 整条跳过并记 Warning（沿用既有第二道防线）
domains 非空 → 发一条带 domain 的规则
cidrs   非空 → 发一条带 ip   的规则
两条共用同一份 inboundTag 与 outboundTag
```

### 6.2 两条新的不变量

**N1：绝不发出同时含 `domain` 与 `ip` 的规则。** 由 F4，那是 AND。把两个条件塞进一条规则看起来更简洁，是很自然会犯的错，而错了不会有任何报错——只是那条规则几乎永不命中。

**N2：绝不发出 `ip: []`。** 由 F5，与 `domain: []`、`inboundTag: []` 完全同构：条件消失，规则范围被静默放大。

两条都要有直接断言的单元测试，见 §10。

### 6.3 顺序

- block 组在前、proxy 组在后（既有第二条不变量，与 `Priority` 无关）。
- 同一条数据库规则内：**domain 规则在前，ip 规则在后**，固定顺序。
- 「生成逐字节确定」不受影响：新增的两个列表都按上面的确定顺序合并，`encoding/json` 对 map key 排序。禁止遍历 map 产生数组顺序。

### 6.4 冲突判定不变

`checkConflict` 的判定单位仍是「组 × 入站」。一个组现在可能产出两条 xray 规则，但这不改变哪些数据库规则互斥。不加新维度。

### 6.5 `ipRuleResolveDomain` 开关

新设置项，`int`，默认 `0`。

- 为 `0`：**不碰** `routing.domainStrategy`，保留模板里管理员手写的值（模板默认没有这个键）。升级后行为零变化。
- 为 `1`：生成期写入 `routing.domainStrategy = "IPIfNonMatch"`，覆盖模板里的值。设置项说明里必须写明「会覆盖模板中的同名字段」。

打开后的两个行为变化，要写进设置项说明：

1. 每条未被域名规则命中的连接会在 VPS 上多一次 DNS 解析（有缓存）。
2. 模板里 `geoip:private → blocked` 那条规则会开始对域名目标生效（解析到私网/回环 IP 的域名被封）。这通常是想要的（反 SSRF），但确实是行为变化。

由 F7，解析失败不会断网，只是 IP 规则不生效。

## 7. P2：订阅与导入导出

### 7.1 订阅解析

`ParseSubscription`（`routing_subscription.go:30`）返回值改为 `(domains, cidrs []string, skipped int, err error)`。新增转换：

| 规则类型 | 转成 | 说明 |
|---|---|---|
| `IP-CIDR` / `IP-CIDR6` | CIDR 原值 | 丢掉 `,no-resolve` 之类的尾巴，与现有取第一个值的逻辑一致 |
| `GEOIP` | `geoip:<code 小写>` | 由 `ValidateCidrs` 的真实 xray 兜底；类别不存在时整组拒绝 |
| `IP-ASN` | **仍然跳过并计数** | xray 没有 ASN 匹配能力，不猜、不近似 |
| `SRC-IP-CIDR` | **仍然跳过并计数** | 那是 `source`（按客户端 IP），塞进 `ip` 是语义错误 |

「解析不出任何域名就报错」的既有防线放宽为「**域名和 IP 都为空**才报错」。纯 IP 列表（如中国 IP 段）从此是合法订阅源。

### 7.2 导入导出

`PortableDomainGroup`（`routing_portable.go:35`）加 `Cidrs []string` 字段，json tag 为 `cidrs`。

**不导出 `SubscribedCidrs`**，理由与不导出 `SubscribedDomains` 完全相同（体积、以及搬过去会显示一个假的「刚刚更新」）。

跨版本行为：

- 旧面板读新文件：忽略 `cidrs` → 组只剩域名 → 分流范围缩小，安全侧正确。
- 新面板读旧文件：`cidrs` 缺失 → 空 → 行为不变。

`Cidrs` 用值类型切片即可，不需要 `PortableRule.InboundRefs` 那种指针类型：那里区分「显式 `[]`」与「字段缺失」是因为 `[]` 有「对所有入站生效」的特殊全局语义；`cidrs` 的空与缺失都只表示「这个组没有 IP 段」，两者同义。

## 8. P3：DNS 设置

### 8.1 现状

面板**今天已经能配 DNS**：`xray.Config` 的 `DNSConfig` 字段（`xray/config.go:11`）json tag 就是 `dns`，而 `xrayTemplateConfig` 是整份模板的原文 JSON，手写一段 `"dns": {...}` 进去就会生效。

所以本功能的价值不在「让它成为可能」，而在三件事：

1. 不用手改那份一旦写错就全员断网的模板。
2. **自动把默认 freedom 出站的 `domainStrategy` 设成 `UseIP`。** 由 F8/F9，不做这一步 `dns` 段对直连流量完全是空转，而手写模板的人几乎不会知道这一点。
3. 保证 `servers` 末位有 `localhost` 兜底。

### 8.2 一个诚实的边界

这个拓扑下**用户的 DNS 查询不经过 VPS**：客户端自己解析，隧道里发给面板的是连接目标而不是 DNS 查询。所以本功能**不能**改善客户端侧的 DNS 泄露。

它改善的是**服务端那次解析**：现在走 `/etc/resolv.conf`（云厂商解析器，明文 UDP 53），云厂商能看到全部用户访问的域名。UI 文案必须如实这么说，不能写成「防止 DNS 泄露」。

如果目标仅仅是「VPS 别用云厂商的解析器」，改 `/etc/resolv.conf` 或装 dnscrypt-proxy 就够了，不必动面板。面板 DNS 段不可替代的价值是「不碰模板」与「自动联动 `UseIP`」。

### 8.3 设置项

新设置项 `dnsServers`，`string`，换行分隔，默认 `""`。

**列表为空 = 功能关闭 = 模板完全不动。** 不额外加布尔开关——一个配了却静默不生效的设置项，比没有这个设置项更糟。

非空时生成期做两件事：

1. `cfg.DNSConfig = {"servers": [用户的..., "localhost"]}`。`localhost` 末位固定兜底（用户已写则不重复追加）。配置的解析器全挂时退化成系统解析器，而不是断网。覆盖模板里已有的 `dns` 段，设置项说明写明这一点。
2. 给 `outbounds[0]` 加 `"domainStrategy": "UseIP"`。

### 8.4 三条安全约束

**只用 `UseIP` 系列，永不用 `ForceIP` 系列。** 由 F10，`UseIP*` 解析失败只记日志、回落按域名直连；`ForceIP*` 直接断连接。这一条把「DNS 配错 = 全员断网」整个消掉，是本功能敢做的前提。

**`outbounds[0]` 不是 `freedom` 时不动它，记 Warning。** 管理员可能改过模板。

**一切在生成期注入，绝不写回 `xrayTemplateConfig`。** 写回的话，回退到旧二进制后模板里还留着 `UseIP` 与 `dns` 段，而旧代码不知道它们从哪来、也不会清理。

### 8.5 注入位置与顺序

新建 `web/service/dns_inject.go`，`DNSInjector.Inject(cfg *xray.Config)`，在 `XrayService.GetXrayConfig`（`web/service/xray.go:79`）里 **`routingInjector.Inject` 之后**调用。

顺序必须固定：`RoutingInjector.buildOutbounds` 会把整个 outbounds 数组反序列化再重新序列化，并由 `tagDefaultOutbound` 保证 `outbounds[0]` 存在且有 tag。DNS 注入器排在它之后，只需往那个 map 加一个键，不必自己处理数组不存在的情况。

`encoding/json` 对 map key 排序，加键不破坏逐字节确定性。

### 8.6 校验

`entity.CheckValid` 逐行检查语法：`localhost`、裸 IP、`IP:port`、或 `udp://` `tcp://` `tls://` `https://` `h2c://` `quic://` 开头的地址。**只查语法，不测可达性。**

域名型 DoH 端点（`https://dns.google/dns-query`）**警告但不拒绝**：它需要 bootstrap 解析自己的域名，配错会很难排查，但拒绝会挡住合法用法。IP 型端点（`https://8.8.8.8/dns-query`）零 bootstrap 依赖，UI 里推荐它。

保存时另走一遍 `validateWithFullConfig`（真实 xray），fail open 边界不变。

### 8.7 可扩展性

`dns.servers` 的数组元素在 xray 里既可以是字符串也可以是对象（带 `domains` / `expectIPs` 过滤）。本期只发字符串。将来要做分域名解析时，往同一个数组里发对象即可，不需要改数据形态。

## 9. 改动文件清单

### P1

| 文件 | 改动 |
|---|---|
| `web/service/routing_domain.go` | `domainPrefixes` 补全；`ParseDomains` 重写（歧义分岔 + 大小写归一） |
| `web/service/routing_subscription.go` | `DOMAIN-KEYWORD` 分支改吐 `keyword:` |
| `web/html/xui/routing.html` | 手工域名 placeholder 补 `keyword:openai`；前缀说明 |

### P2

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `DomainGroup` 加 `Cidrs` / `SubscribedCidrs` |
| `web/service/routing_domain.go` | `ParseCidrs` / `EncodeCidrs` / `DecodeCidrs` / `DecodeSubscribedCidrs`；`updateFieldsFor` 加 `cidrs`；订阅地址变更时清空 |
| `web/service/routing_validate.go` | `ValidateCidrs` |
| `web/service/routing_subscription.go` | `ParseSubscription` 返回值；`convertSubscriptionLine` 新增 IP 类型；`isValidCIDR` |
| `web/service/routing_inject.go` | `buildRule` 返回 0~2 条规则；`buildRules` 消费；`Inject` 写 `domainStrategy` |
| `web/service/routing_portable.go` | `PortableDomainGroup.Cidrs`；导出与导入两侧 |
| `web/service/setting.go` | `defaultValueMap` 加 `ipRuleResolveDomain`；getter |
| `web/entity/entity.go` | `AllSetting` 加字段；`CheckValid` 校验 |
| `web/controller/routing.go` | `domainGroupForm` 加 `Cidrs`；`encodeCidrsFromForm` |
| `web/assets/js/model/models.js` | `AllSetting` 构造函数加 `ipRuleResolveDomain` |
| `web/html/xui/routing.html` | 分流组弹窗加 IP 段 textarea；列表加 IP 列 |
| `web/html/xui/setting.html` | 「让 IP 规则也匹配域名目标」开关 |

### P3

| 文件 | 改动 |
|---|---|
| `web/service/dns_inject.go` | **新建**，`DNSInjector` |
| `web/service/xray.go` | `GetXrayConfig` 里调用 `dnsInjector.Inject`，排在 `routingInjector.Inject` 之后 |
| `web/service/setting.go` | `defaultValueMap` 加 `dnsServers`；getter |
| `web/entity/entity.go` | `AllSetting` 加字段；`CheckValid` 逐行语法校验 |
| `web/assets/js/model/models.js` | `AllSetting` 构造函数加 `dnsServers` |
| `web/html/xui/setting.html` | DNS 服务器 textarea + 边界说明文案 |

> `web/assets/js/model/models.js` 这一行不是收尾工作。既有文档已记录：漏掉它会让 `ObjectUtil.cloneProps` 丢弃服务端返回值、提交体里缺字段，**整个保存配置接口失败**，且报错只指向新字段。

## 10. 测试策略

| 文件 | 用例 |
|---|---|
| `web/service/routing_domain_test.go` | **P1**：管理员反馈里那 20 行原文整段通过；`keyword:` / `dotless:` / `ext-domain:` / `ext-site:` 放行；不含点裸串归一成 `keyword:`；含点裸串被拒且错误里点名三种写法；`wat:openai.com` 被拒；`domain:OpenAI.COM` 归一成小写；`regexp:` 与 `dotless:` **不**被转小写。**P2**：`ParseCidrs` 接受 CIDR/裸 IP/IPv6/`geoip:cn`/`geoip:!cn`，拒绝域名与空段；`updateFieldsFor` 能更新 `cidrs`；订阅地址变更时 `subscribed_cidrs` 被清空 |
| `web/service/routing_subscription_test.go` | `IP-CIDR` / `IP-CIDR6` / `GEOIP` 正确转换并落进 cidrs；`IP-ASN` / `SRC-IP-CIDR` 仍跳过并计数；纯 IP 列表不再报错；混合规则集两侧都有产出；`DOMAIN-KEYWORD` 吐 `keyword:` |
| `web/service/routing_inject_test.go` | 一条规则生成两条 xray 规则；只有域名时**不生成 ip 规则**（而非 `ip: []`）；只有 IP 时同理；两者皆空整条跳过并记 Warning；**遍历全部生成规则，断言不存在同时含 `domain` 与 `ip` 的规则**（N1）；**断言不存在空的条件数组**（N2）；同输入两次生成结果逐字节相同；`ipRuleResolveDomain=0` 时不写 `domainStrategy`，`=1` 时写入 |
| `web/service/dns_inject_test.go` | **新建**。空列表时 `DNSConfig` 与 outbounds 都不变；非空时写 `dns` 且 `servers` 末位为 `localhost`；用户已写 `localhost` 时不重复追加；`outbounds[0]` 非 freedom 时不动并记 Warning；同输入两次生成逐字节相同 |
| `web/service/routing_portable_test.go` | 带 `cidrs` 的往返；旧格式（无 `cidrs`）能导入且行为不变；不导出 `SubscribedCidrs`；重跑幂等 |
| `web/service/setting_*_test.go` | 两个新设置项的默认值、`CheckValid` 正反面（合法 DNS 地址各形态、非法前缀被拒、域名型 DoH 放行） |
| `web/service/routing_e2e_test.go` | 真实 xray `run -test` 覆盖三份生成配置：域名+IP 混合组、纯 IP 组、开了 DNS 的 |
| `web/html_test.go` | 既有 `TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` 跑通（新表单项都在既有 modal 内） |

每期结束跑 `make verify`（vet + test + build），这是提交前的门禁。

## 11. 风险、回退与边界

### 11.1 回退到旧版本二进制

| 改动 | 旧二进制读到 | 后果 |
|---|---|---|
| `domain_groups.cidrs` / `subscribed_cidrs` 新列 | 忽略（`AutoMigrate` 只加列不删列） | IP 分流失效，域名分流完全正常。范围缩小而非放大，安全侧正确 |
| 两个新设置项 | `GetAllSetting` 按 `entity.AllSetting` 的字段反射取值，表里多余的 key 不影响 | 无 |
| `xrayTemplateConfig` | **从未被写过**（§8.4 第三条） | 无 |

### 11.2 `Config.Equals` 不需要扩展

新增内容全部落在 `RouterConfig` / `DNSConfig` / `OutboundConfigs` 这三个按字节比较的 `json_util.RawMessage` 字段上。代价是生成必须逐字节确定——见 §6.3 与 §8.5。

`Cidrs` 是 `DomainGroup` 的列，不进 `xray.Config`，同样不需要动 `Equals`。

### 11.3 热更新

`dns`、数组首位的默认出站、`routing.domainStrategy` 三者都在 `xray/hot_diff.go:48` 的 `static` 列表里 → 改这两个设置项必然触发整进程重启。这是既有行为，无需改代码，但要在设置项说明里让管理员有预期。

新增的 ip 规则本身走 routing 的热应用路径，与域名规则完全相同。

### 11.4 已知边界

- **服务端解析结果可能与客户端不同。** 开了 `IPIfNonMatch` 后，IP 规则匹配的是 VPS 解析出的 IP；客户端解析到的可能是另一个 CDN 边缘。这是服务端代理的固有性质，不做补偿。
- **`geoip:private` 开始对域名生效**（§6.5）。
- **IP 分流依赖客户端发什么。** 客户端直接连 IP 字面量时无需任何 DNS；发域名时只有开了 `ipRuleResolveDomain` 才会被 IP 规则看到。UI 文案要说清楚，否则管理员会以为「填了 IP 段就一定生效」。
- **不做 ASN 匹配**（§1 非目标）。订阅里的 `IP-ASN` 条数会计入 `LastSkipped`，界面照常显示。
