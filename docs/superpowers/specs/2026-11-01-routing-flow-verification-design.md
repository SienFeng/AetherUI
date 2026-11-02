# 分流规则「流量走向」图的真实性验证 设计

日期：2026-11-01
范围：`web/html/xui/routing.html` 规则详情里的「流量走向」图，及其所需的后端接口。

## 1. 起因

「流量走向」图目前只是把规则配置画成图：左列用户来自 `ruleEditor.rule` 的 `inboundIds`，
中列来自 `domainGroupIds`，右列来自 `action/outboundId`，连线是纯装饰 SVG。它不读核心、
不读日志，所以**不能证明流量真的这样走**。管理员的诉求是「确保 100%」。

物理上 100% 做不到（客户端可能先解析成 IP、走 QUIC、或根本没连这台机器），本设计的目标是
退一步的可操作版本：**图上每个用户都附带来自核心或日志的证据；证据与配置不符时直接标红；
拿不到证据时如实说「无法验证」以及原因，绝不默认显示绿色。**

## 2. 目标 / 非目标 / 反目标

目标

- G1 主动验证：对图上每个用户，向**正在运行的** xray 核心问「这个用户访问本规则的域名，
  你会把它送去哪个出站」，与配置期望比对。
- G2 事后核对：对图上每个用户，统计访问日志里**近 24 小时**访问本规则域名组的真实连接，
  分别走了哪些出站。
- G3 每个用户行给出一个明确的状态，四种之一：已验证 / 与配置不符 / 无法验证 / 等待下发。

非目标

- 不做客户端侧验证（客户端出口 IP、DNS 泄漏等）。
- 不验证节点自身的落地出口（那是现有「测试出站节点」的职责，`routing_probe.go`）。
- 不新增数据库列或表；不改生成出的 xray 配置（不给规则加 `ruleTag` 等）。
- 不对未保存的修改做验证。

反目标（做出来但触犯任一条即视为失败）

- A1 **任何情况下都不能在没有证据时显示绿色。** 核心没跑、配置未下发、样例取不出、访问日志
  没开——一律灰色/黄色并写明原因。
- A2 验证动作不得改变核心状态：`TestRoute` 的 `PublishResult` 必须为 false，不触发重启、
  不写配置。
- A3 不得在面板里另写一份域名匹配逻辑。判断「某个日志目标是否属于这个域名组」必须复用
  xray 自己的匹配器（同一个锁定版本的 `common/geodata` 与 `app/router`），否则面板与核心的
  判定迟早漂移，而漂移之后图会对着错误的结论打绿勾。
- A4 不能把访问日志里**规则修改之前**的连接当成「现在仍不符」的证据而不加提示。

## 3. 证据一：核心实时路由（`RoutingService.TestRoute`）

### 3.1 已确认的前提（只读核实过）

- 模板 `web/service/config.json` 的 `api.services` 已包含 `RoutingService`；
  `xray/api.go` 已建 `RoutingServiceClient`（热应用在用）。不需要改模板。
- xray-core（锁定版本）`app/router/command/command.go:92` `TestRoute` 直接调
  `router.PickRoute`，用的是核心**内存里**当前的规则表——包括热应用下发的结果，不依赖
  `bin/config.json`（CLAUDE.md 已说明热应用后该文件可能是旧的）。
- 没有任何规则命中时 `pickRouteInternal` 返回 `common.ErrNoClue`（`app/router/router.go`），
  经 gRPC 变成一个错误；实际行为是落到默认出站。
- 开了 `ipRuleResolveDomain`（`IPIfNonMatch`）时，`TestRoute` 与真实连接一样会走第二遍并
  在核心所在机器上解析域名。这是期望行为：结果与真实流量同口径。

### 3.2 请求构造

对每个「用户 × 样例目标」发一次 `TestRoute`：

- `InboundTag` = 该入站当前的 tag（`inbound-<port>`，以库里 `Inbound.Tag` 为准）。
- 样例目标是域名时填 `TargetDomain`；是 IP 时填 `TargetIPs`。
- `Network` = TCP，`TargetPort` = 443。（模板里 `protocol: bittorrent` 之类的规则不会被误触。）
- `FieldSelectors` = `["outbound"]`，`PublishResult` = false（A2）。

### 3.3 样例目标从哪来

从规则引用的每个域名组的**合并后**条目（`MergeDomains(手工, 订阅)`，与 `buildRule` 同源）里取：

| 条目形态 | 样例 |
|---|---|
| `full:x` / `domain:x` | `x` |
| `keyword:` / `regexp:` / `dotless:` / `geosite:` / `ext:*` | 推不出可靠样例，跳过 |
| IP 段 `a.b.c.d/n`（含 IPv6） | 段内第一个可用地址 |
| `geoip:` / `ext:` IP、带 `!` 取反的段 | 跳过 |

每个域名组最多取 2 个样例（手工优先于订阅，各取首个），每个用户最多 6 个样例。
**跳过的条目不影响已取到样例的判定**，但若某个用户一个样例都取不到，该用户的核心验证结果为
「无法验证：本规则的域名都是关键词/正则/geosite 写法，推不出可测试的域名」。

### 3.4 期望出站

- `proxy` → 节点的 `Tag`（`OutboundNode.Tag`）。
- `block` → `model.BlockOutboundTag`（`a-ui-block`）。
- `direct` → 当前生成配置里默认出站的实际 tag（与 `tagDefaultOutbound` 同源，从
  `GetXrayConfig()` 结果的出站数组首位读取，不硬编码 `a-ui-default`）。

实际出站的归类：

- `ErrNoClue` → 默认出站（展示为「默认出站（未命中任何规则）」）。
- `a-ui-meter-*` 计量 tag → 语义上是默认出站（计量规则排在最后，只接住本来会走默认出站的流量）。
- 判定：`proxy`/`block` 要求实际 tag **严格等于**期望 tag；`direct` 要求实际出口是默认出站
  （默认 tag、计量 tag 或 `ErrNoClue` 都算出口一致），但若实际不是由本规则命中（`ErrNoClue`
  或计量 tag）要在明细里注明「出口一致，但不是由本规则命中」——比如规则被冲突规则抢先或根本没
  下发时，直连结果凑巧一致，管理员应当知道。

### 3.5 核心级前置状态（先于逐用户判定）

| 状态 | 判据 | 展示 |
|---|---|---|
| 核心未运行 | `!XrayService.IsXrayRunning()` 或拿不到 API 端口 | 所有用户灰色「无法验证：xray 未运行」 |
| 等待下发 | 运行中核心的配置快照 `p.GetConfig()` 与 `GetXrayConfig()` 不 `Equals` | 黄色「配置已保存，正在等待下发到核心（最多约 10 秒）」，仍然执行 TestRoute 并展示结果，但整体状态不得为绿 |
| 核心忙 | 拿不到包级 `lock`（热应用/重启进行中，用 `TryLock`） | 灰色「核心正在更新配置，请稍后重新验证」 |
| 本规则不会写进配置 | 前端 `ruleIssue(savedRule)` 非空，或规则已停用 | 不调用接口，沿用现有黄色提示 |

`lock` 只在读取进程快照与 API 端口的瞬间持有，**不在整批 RPC 期间持有**（TestRoute 可能触发 DNS 解析，
不能把热应用卡住）。由此带来的竞态（验证过程中恰好发生热应用）可以接受：结果带 `checkedAt`，
用户可重新验证。

### 3.6 规模与超时

- 全局规则（`inboundIds = []`）对所有**已启用**入站逐个验证，上限 50 个入站；超出部分不验证，
  响应里带 `truncated` 并在界面说明。
- 单次 RPC 超时 3 秒，整次请求总预算 20 秒（`context`），到时未完成的条目记「超时，无法验证」。
- 同一时刻只允许一个验证请求在跑（包级互斥，`TryLock` 失败直接返回「已有验证在进行」），防止
  管理员连点把核心打满。

## 4. 证据二：访问日志里的真实连接

### 4.1 已确认的前提

- `AccessLog` 有 `InboundId`、`Target`（`host:port`）、`Route`（实际出站 tag；经中间出站时为
  `"a -> b"` 整串）、`Accepted`、`Time`（毫秒）。
- 访问日志由 `accessLogEnable` 控制，默认 0（存量部署），新装为 1；保留期默认 7 天。

### 4.2 统计口径

对图上每个用户：

1. 取 `inbound_id = ? AND time >= now-24h`，`GROUP BY target, route`，得到（目标, 出站, 次数, 最近时间）。
   SQL 端最多返回 5000 组，超出标记 `truncated`。
2. 用 xray 的匹配器判断每个目标是否属于本规则的域名组：
   - 去掉端口；是 IP 字面量的走 IP 匹配（本规则所有组合并后的 IP 段），否则走域名匹配。
   - 匹配器用 `geodata.ParseDomainRules(entries, Domain_Substr)` + `router.NewDomainMatcher`
     构造（与 `infra/conf/router.go` 相同的默认类型），IP 侧用 `geodata.ParseIPRules` +
     `router.NewIPMatcher(rules, router.MatcherAsType_Target)`（均为锁定版本里已导出的函数）。涉及 `geosite:`/`ext:` 时要先 `ensureXrayAssetLocation()`。
   - 匹配器每次请求按规则构造一次，不跨请求缓存。
3. 对属于本规则的那些行，按出站是否符合期望（与 §3.4 相同的判定；`Route` 取 `->`/`>>` 之前的
   第一段比对）分为「符合」与「其他出站」两类，汇总次数；「其他出站」按出站分组，带最多 5 个示例
   目标和该组**最近一次**发生时间。
4. `Accepted = false` 的行（被拒绝的连接）单列计数，不计入符合/不符。

### 4.3 对 A4 的处理

库里没有规则修改时间，本设计不为此加列（我的判断：改模型会牵动 CLAUDE.md 记录的迁移/回退约束，
收益不足）。替代：每个「其他出站」分组都带最近发生时间，界面写成
「近 24 小时有 3 次走了直连（最近一次 10:42）」。管理员刚改完规则时能自己判断那是不是改之前的。

### 4.4 访问日志的前置状态

- `accessLogEnable = 0` → 该证据整体显示「未开启访问日志，无法核对真实连接」，并给出指向
  设置页的提示文字（不自动开启：开启会改 `log` 段并整进程重启）。
- 已开启但 24 小时内该用户没有任何属于本规则的连接 → 「近 24 小时没有访问过这些域名」，
  中性灰色，不算失败也不算验证通过。

## 5. 用户行的最终状态

组合规则（从上到下，命中即停）：

| 最终状态 | 条件 | 颜色 + 图标 + 文字（不只靠颜色） |
|---|---|---|
| 与配置不符 | 核心有任一样例不符，**或**访问日志有「其他出站」连接 | 红 `close-circle`「与配置不符」 |
| 等待下发 | 核心级为「等待下发」 | 黄 `clock-circle`「等待下发」 |
| 已验证 | 核心所有样例都符合 | 绿 `check-circle`「已验证」；日志有符合的连接时追加「近 24h N 次真实连接」 |
| 无法验证 | 其余（核心不可用、取不到样例、超时） | 灰 `question-circle`「无法验证」+ 原因 |

访问日志单独不会让一个用户变绿：它只证明「过去」，不证明「现在」。它只能把用户推向红色，
或者给绿色补充真实连接数。

## 6. 界面

沿用 `routing.html` 现有的设计基线，不新增样式体系：

- 用户行 `.rv-urow` 右侧加一个状态徽章，复用 `custom.css` 的 `.status-badge` +
  `status-ok/status-warn/status-danger/status-neutral`，图标 + 文字，满足「不只靠颜色」。
- 连线颜色跟随该用户的状态：已验证用现有绿/蓝；不符用 `--aui-danger`；其余用中性灰。
  连线仍是 `aria-hidden` 的装饰，信息全部在文字里。
- 「流量走向」标题右侧：
  - 一行汇总文字，例如「已验证 2/2 · 14:05」，放在 `role="status" aria-atomic="true"` 的元素里，
    只播报这一处，不让每个徽章各自成为 live region。
  - 一个「重新验证」按钮（`a-button size="small"`），请求进行中禁用并显示 loading。
- 点击用户行（或其徽章，须为可聚焦的 `button`）展开明细：每个样例的「目标 → 实际出站」，以及
  访问日志的「符合 N 次 / 其他出站分组 / 最近时间 / 示例目标」。展开状态用 `aria-expanded`。
- 全局规则只显示一行「所有用户」：徽章显示汇总（如「47/48 已验证」），展开后只列出**不是「已验证」**的用户；
  `truncated` 时写明「只验证了前 50 个已启用入站」。
- 触发时机：选中一条已保存、启用、`ruleIssue` 为空且 `!ruleDirty` 的规则时自动验证一次；
  规则保存成功后自动再验证一次（此时大概率先看到「等待下发」，界面在 12 秒后自动再验证一次，仅一次）。
  有未保存修改时不调用，徽章区域显示「保存后验证」。
- 窄视口：徽章在用户名下方换行，不截断用户名；明细列表纵向堆叠。

## 7. 接口

`POST /aui/routing/rule/verify/:id`（挂在现有 `RoutingController` 的 `rule` 分组下）。
只读，不改任何状态；走现有登录与 CSRF 头校验。

响应 `jsonObj`（字段名为本设计的约定，实现可微调但前后端须一致）：

```json
{
  "ruleId": 2,
  "checkedAt": 1793520000000,
  "expected": { "tag": "a-ui-us-home", "label": "美区家宽（IProyal）", "action": "proxy" },
  "core": { "state": "ok|pending|not_running|busy|error", "message": "" },
  "accessLog": { "enabled": true, "windowHours": 24, "error": "" },   // error 可省略；非空 = 读取失败，不是「没有数据」
  "truncated": false,
  "users": [
    {
      "inboundId": 3, "name": "Xy Qin",
      "verdict": "verified|mismatch|pending|unverifiable",
      "reason": "",
      "samples": [
        { "target": "chatgpt.com", "actualTag": "a-ui-us-home", "actualLabel": "美区家宽（IProyal）",
          "match": true, "hitByRule": true, "error": "" }
      ],
      "log": {
        "matched": 128, "rejected": 0, "truncated": false, "error": "",   // error 可省略；非空 = 该用户日志查询失败
        "others": [ { "route": "a-ui-default", "label": "直连", "count": 3, "lastAt": 1793519000000,
                      "examples": ["api.openai.com:443"] } ]
      }
    }
  ]
}
```

规则不存在 → 错误；规则停用或 `buildRule` 会跳过它 → 返回 `core.state="error"` 与原因，不发 RPC。

## 8. 代码落点

- `xray/api.go`：新增 `TestRoute(ctx, inboundTag, domain string, ip net.IP) (tag string, noMatch bool, err error)`，
  把 `ErrNoClue` 识别为 `noMatch=true`（gRPC 错误按消息文本 `common.ErrNoClue.Error()` 比对，
  这是唯一可行的识别方式，需在注释写明并有测试）。
- `web/service/xray.go`：新增只读的 `RuntimeSnapshot()`，用 `lock.TryLock()` 返回
  `{running, apiPort, pending, defaultTag}`；拿不到锁返回 busy。
- `web/service/routing_verify.go`（新）：样例提取、期望出站计算、调用 TestRoute、日志聚合与匹配、
  组合最终状态。纯函数部分（样例提取、判定组合、Route 首段解析）与 IO 分开，便于单测。
- `web/service/accesslog.go`：新增 `GroupByTargetRoute(inboundId int, sinceMs int64, limit int)`。
- `web/controller/routing.go`：注册路由，参数校验。
- `web/html/xui/routing.html`：状态徽章、汇总、重新验证、明细展开、连线着色、触发时机。

## 9. 验证方式

- 单测：样例提取各形态；判定组合表（§5）逐行；`direct` 的「出口一致但非本规则命中」；Route 首段解析；
  `ErrNoClue` 识别。
- 端到端（仿照 `xray_hot_reload_e2e_test.go`，用真实 xray，缺 `RoutingService` 时跳过）：
  一条 proxy 规则对指定入站 → verified；把规则改成另一个节点但不触发下发 → pending；
  另一个未被覆盖的入站 → 在规则覆盖范围外不出现；关键词-only 组 → unverifiable。
- 日志匹配单测：用 xray 匹配器对 `domain:`/`full:`/`keyword:`/`regexp:`/IP 段的命中与不命中。
- 前端：`go test ./web/...`（模板解析、自闭合、Vue 根等既有守卫）；浏览器实际打开分流页，
  宽/窄视口截图，检查四种状态的渲染、键盘展开、汇总播报、无新增控制台错误。

## 10. 实现者自行决定的部分

- 样例上限、超时等常量的最终取值（本文给的是起点）。
- 明细面板的具体排版，只要遵守第 6 节的约束。
- 由我（设计者）替管理员做的取舍，审阅时请重点看：§4.3 不加规则修改时间列；§3.6 全局规则 50 个入站上限；
  §5 访问日志单独不能变绿；§6 保存后自动重验一次（12 秒）。
