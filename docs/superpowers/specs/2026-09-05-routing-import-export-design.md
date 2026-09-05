# 分流配置导出 / 导入 设计文档

- 状态：设计
- 日期：2026-09-05
- 关联：`docs/superpowers/specs/2026-09-02-domain-routing-design.md`（本子系统的约束来源）、`2026-09-02-routing-multi-inbound-design.md`、`2026-09-02-domain-subscription-design.md`

## 1. 背景与目标

管理员在多台机器上跑 AetherUI 时，域名组、出站节点、分流规则要在每台机器上从头填一遍。域名组尤其痛：一个订阅组要填备注、订阅地址，手工域名可能几十条；出站节点要一条条粘分享链接；规则要重新勾选入站、选组、选节点、排优先级。

目标：一台机器配好之后，导出成一个 JSON 文件下载到本地，在另一台装了 AetherUI 的机器上传导入。

- 域名组、出站节点、分流规则各自可单独导出/导入。
- 三者可一次性整包导出/导入。
- 导出文件落到本地电脑，导入从本地电脑上传。

### 非目标

- **不导出入站。** 入站牵扯端口占用、证书路径、UUID、限速、地区限制，全是机器强相关的东西。跨机器搬入站是另一个问题。
- **不做在线同步 / 主从推送。** 只有文件这一种载体。
- **不做导出文件加密。** 见 §8 决策 2。
- **不做覆盖式导入。** 见 §5.2。
- 不导出订阅已拉取的域名内容。见 §4.3。

## 2. 核心难点：跨机器的引用

`RoutingRule` 存的是 **id 外键**：

```go
InboundIds    string  // JSON 整数数组，升序去重；空数组 [] = 所有入站
DomainGroupId int
OutboundId    int
```

三个 id 在另一台机器上**必然指向别的东西或不存在**。而这个子系统对错误引用的容忍度极低——`2026-09-02-domain-routing-design.md` 里那张实测表说得很清楚：

| 情形 | xray 的反应 | 后果 |
|---|---|---|
| 规则的 `inboundTag` 为空数组 | `Configuration OK` | 规则从「只覆盖甲」**放大成覆盖所有入站** |
| 规则的 `domain` 为空数组 | `Configuration OK` | 规则从「访问这批域名走 B」**退化成该用户全部流量走 B** |
| 规则引用已删除的出站 | `Configuration OK` | 运行时**静默回落直连**，以为封禁了其实裸奔 |

所以导入绝不能出现「id 对不上就置零 / 清空」这类处理。**`InboundIds` 清空成 `[]` 是这个功能最容易犯、后果最严重的错误**：一条本该只覆盖某个人的规则会被静默放大到全体，且 xray 不报任何错。

## 3. 解法：导出零 id，全部改写成业务键

```json
{
  "kind": "a-ui-routing-export",
  "version": 1,
  "exportedAt": 1757030400000,
  "exportedBy": "a-ui v1.5.0",
  "scope": ["domainGroups", "outbounds", "rules"],

  "domainGroups": [
    {
      "remark": "中国域名列表",
      "domains": ["domain:example.com", "geosite:cn"],
      "subscribeUrl": "https://example.com/cn.txt"
    }
  ],

  "outbounds": [
    {
      "tag": "a-ui-hk-01",
      "remark": "香港01",
      "protocol": "vless",
      "config": "{\"tag\":\"a-ui-hk-01\",\"protocol\":\"vless\",...}",
      "enable": true
    }
  ],

  "rules": [
    {
      "remark": "国内直连",
      "domainGroupRef": "中国域名列表",
      "outboundRef": "a-ui-hk-01",
      "inboundRefs": [ { "remark": "用户甲", "port": 2886 } ],
      "action": "proxy",
      "priority": 10,
      "enable": true
    }
  ]
}
```

**文件里不出现任何 `Id`。**

### 3.1 三个引用键的选择

| 引用 | 键 | 理由 |
|---|---|---|
| 出站节点 | `tag` | 数据库 `gorm:"unique"`，唯一且**一经分配不可变**（`OutboundNodeService.Update` 明确拒绝改 tag）。天然的跨机器主键 |
| 域名组 | `remark` | 唯一可用的业务键，但**没有 unique 约束**，见 §3.2 |
| 入站 | `{remark, port}` 二元组 | 入站没有稳定业务键，见 §3.3 |

### 3.2 域名组重名 → 导出直接拒绝

`DomainGroup.Remark` 上没有唯一约束，两个组都叫「国内域名」在库里是合法的。

一旦重名，导入端无法确定 `domainGroupRef: "国内域名"` 指向哪一个。任何"猜"的策略（取第一个、取 id 最小的）都会在猜错时产生一条**指向错误域名组的规则**——而规则表会渲染得完全正常，配置也会正常生成，只是流量走错了节点。这类错误没有任何一层防线会发现。

**处置：导出时检测重名，整体拒绝，错误信息点名是哪几个组，要求管理员先改名。**

这是「宁可不生效让用户察觉，也绝不输出条件残缺的规则」在导出侧的延伸。拒绝一次导出的代价，远小于在另一台机器上产生一条静默走错的规则。

只在**导出了 rules 的场景**下才做这个检查？不——**只要导出了 domainGroups 就检查**。分项导出的域名组文件将来会被拿去和分项导出的规则文件配套使用，重名问题一样存在。检查条件统一，行为才可预测。

### 3.3 入站为什么是二元组

入站的三个候选键都不可靠：

- `Id` — 跨机器无意义，且 SQLite 会复用被删除的自增 id。
- `Tag` — 由端口算出（`UpdateInbound` 里 `Tag = fmt.Sprintf("inbound-%v", Port)`），改端口 tag 就变。存 tag 等于存端口，多绕一层。
- `Remark` — 可重复，没有唯一约束。

所以存 `{remark, port}` 两个，导入时两级匹配（§5.3）。这不是"更可靠的键"，而是"把两个都不可靠的线索都给导入端，让它自己判断并在判断不了时诚实地说出来"。

## 4. 导出

### 4.1 接口

```
POST /aui/routing/export   body: scope=all | domainGroups | outbounds | rules
```

返回 `entity.Msg{ Obj: <上面那个结构> }`。前端拿到 `obj` 自己 `JSON.stringify` 成 Blob 下载——不走 `Content-Disposition`，因为现有前端全部是 axios POST + session cookie，改成 GET 下载要另开一条不带 `X-Requested-With` 的鉴权路径，得不偿失。

文件名：`a-ui-routing-<scope>-<yyyyMMdd-HHmm>.json`。

### 4.2 scope 的语义

- `all` — 三者都导。
- `rules` — **只导规则本身**，不隐式带上它引用的域名组和出站节点。导入端若缺引用，规则会被跳过并报告。这是刻意的：分项导出的语义就该是"只导这一项"，隐式扩大范围会让 `all` 和 `rules` 的区别消失。
- `domainGroups` / `outbounds` — 同理。

前端在导出规则时提示一句「规则依赖域名组和出站节点，跨机器搬迁建议用「导出全部」」。

### 4.3 不导出的字段

| 字段 | 为什么 |
|---|---|
| 所有 `Id` | §3 |
| `SubscribedDomains` | 单个组可达十几万条（生产实例实测 `+111226`），JSON 化后几 MB 到几十 MB。浏览器一次性 `JSON.stringify` + Blob + 上传端 `FileReader` 读回来会卡死 |
| `LastUpdatedAt` / `LastError` / `LastSkipped` | 是本机这一次拉取的状态，搬到另一台机器毫无意义，还会让新机器显示一个假的「刚刚更新」 |

**代价**：导入的订阅组在首次拉取成功前是空的，引用它们的规则会被 `buildRule` 因「域名组为空」整条跳过，那部分流量暂时走直连。这个窗口由 §5.5 收敛到 10 分钟内，并在导入报告里明说。

若目标机器**拉不到订阅源**（被墙、源已失效），窗口是无限的。此时域名组列表里那一行会显示红色的「拉取失败」标签和具体错误——现有 UI 已经覆盖了这个状态，不需要新做什么。

## 5. 导入

```
POST /aui/routing/import   body: data=<JSON 字符串>
```

axios 全局把 body 转 form-urlencoded（`web/assets/js/axios-init.js`），后端 `c.PostForm("data")` 接，与 `getNewEchCert` 等现有接口一致。

### 5.1 不用事务

出站节点落库前要 exec 真实 xray 校验（`ValidateOutbound`），一次几百毫秒，且策略是 fail open。把整个导入包进事务，会在校验期间长时间持有 SQLite 那把写锁，把整个面板（含每 10 秒的流量统计、每秒的并发判定）一起卡住。

逐条独立成败、最后汇总报告。这与 `routing_validate.go` 现有注释里「出站配置与域名列表在**落库之前**交给真实 xray 校验，因此不需要事务回滚」的取向一致。

代价是导入可能"成功一半"。这是可接受的：每一条的成败都在报告里逐条列出，管理员看得见；而且导入是幂等的（§5.2），重跑一次就补齐了。

### 5.2 冲突：跳过已存在，只补新的

- 域名组按 `remark` 判重
- 出站节点按 `tag` 判重
- 规则不做判重，交给 `RoutingRuleService.Add` 里既有的 `checkConflict`

**已存在的一律原封不动**，规则里的引用直接指向本机那一份。

不做覆盖，理由有二：

1. **覆盖会静默改掉目标机器上正在跑的节点配置。** 两台机器的同名节点很可能指向不同的落地服务器（这正是多机部署的常态），覆盖后那台机器的流量会悄悄换一条出口，而界面上什么都不会变。
2. 跳过让导入**天然幂等**：同一个文件导两次不会变成双份，也不会有第二次的副作用。

代价：想更新已有节点的配置只能手工编辑。这是有意的——批量覆盖生产节点配置应该是一个比"上传个文件"更重的操作。

### 5.3 入站匹配：两级，认不全就禁用

```
对 inboundRefs 里的每一项：
  1. 按 remark 在本机入站中精确匹配 —— 恰好命中 1 条才算成功
     （命中 0 条或 ≥2 条都算失败，重名视为无法区分）
  2. 失败则按 port 匹配 —— port 有 unique 约束，命中即唯一

全部命中          → 规则按文件里的 enable 导入
有任何一项没命中  → 规则照常导入，但强制 enable = false，并逐条报告
inboundRefs 是空数组 → 原样保留为全局规则（这是文件里明确表达的语义，不是"认不出来"）
```

**绝不把认不出的入站从数组里剔掉。** 剔到最后变成空数组 = 对所有入站生效（§2 那张表第二行），一条本该只覆盖某个人的规则会被静默放大到全体。

**也绝不整条丢弃。** 丢弃的话管理员要从零重建这条规则，而规则的其余部分（域名组、出站、优先级、动作）都是好的。导入成禁用状态、把缺失的入站在报告里点名，管理员打开编辑弹窗勾一下就好。

导入后的禁用规则在界面上是可见的（规则列表有 enable 开关），不会变成隐形的坑。

### 5.4 出站节点：保留原 tag，但仍要校验

- **用文件里的原 `tag` 落库，不重新 `allocTag`。** 规则靠 tag 对上引用，重新分配会让所有引用它的规则失效。
- **落库前必须 `model.IsReservedTag(tag)` 拦一道。** `a-ui-block` / `a-ui-default` 是注入器自己发出的 tag，不在 `outbound_nodes` 表里，数据库的唯一约束看不见它们。撞名会让 xray 报 `existing tag found: a-ui-block` 并**拒绝启动整份配置**——全员断网，而面板首页仍显示 `running`（`Process.Start()` 把 `cmd.Run()` 丢进 goroutine，启动失败不回传）。判定统一走 `model.IsReservedTag()`，与分配端 / 生成端 / 校验端同一个函数。
- **`tag` 必须非空，长度 ≤ 128。** 空 tag 不会被 `ValidateOutbound` 拒绝，但会让 `xray/hot_diff.go` 的 `decodeOutbounds` 对整份配置判定「必须重启」（数组首位那个模板出站是唯一豁免），出站热更新从此失效——而这不报错，只是静默变慢。字符集不另设白名单：`link.SlugRemark` 生成的 tag 本来就保留中文，xray 实测接受非 ASCII tag。
- **仍走 `ValidateOutbound`（真实 xray 校验）。** 导入文件同样是不可信输入，一个坏配置会让整份 `bin/config.json` 加载失败、全员断网。fail open 策略照旧：xray 二进制缺失 / 超时一律放行并记日志。
- 写库时把 `config` 里的 `tag` 字段强制改写成本行的 `Tag`，与 `persist` / `Update` 的既有做法一致。

### 5.5 域名组：走既有校验路径，不同步拉订阅

- `domains` 走 `service.ParseDomains` → `ValidateDomains` → `EncodeDomains`，与 controller 的 `encodeDomainsFromForm` 同一条路径。
- `subscribeUrl` 非空时走 `service.ValidateSubscribeURL`。
- **不在导入路径里同步拉取订阅。** 一个慢地址能把 HTTP 请求挂满 30 秒，N 个组就是 N × 30 秒。

新建的域名组 `LastUpdatedAt = 0`，而 `ShouldUpdateNow`（`routing_subscription.go:224`）对 0 直接返回 true，`SubscriptionJob` 每 10 分钟跑一次会自动补上首次拉取。**最迟 10 分钟内自愈，不需要任何额外代码。**

导入报告里明说：「N 个域名组已加入订阅，最迟 10 分钟内完成首次拉取；在此之前，仅依赖订阅内容的规则不会写进配置。」

### 5.6 顺序与收尾

```
1. 解析 JSON；校验 kind == "a-ui-routing-export" 且 version == 1，否则整体拒绝
2. 域名组 → 出站节点 → 规则（规则必须最后，它依赖前两者）
3. 有任何实际写入 → xrayService.SetToNeedRestart()
```

第 3 步复用既有链路：置原子标志 → `InboundController` 的 10 秒 cron 消费 → `RestartXray(false)` → `Config.Equals` 发现 `RouterConfig` / `OutboundConfigs` 变了 → 尝试热应用，不行才重启。管理员不需要做任何额外操作。

### 5.7 报告

```go
type ImportCounts struct {
    Created int `json:"created"`
    Skipped int `json:"skipped"`  // 已存在
    Failed  int `json:"failed"`
}

type ImportReport struct {
    DomainGroups ImportCounts `json:"domainGroups"`
    Outbounds    ImportCounts `json:"outbounds"`
    Rules        ImportCounts `json:"rules"`
    Messages     []string     `json:"messages"`
}
```

`Messages` 是人话，逐条说明每一个非 Created 的结果：

```
域名组「中国域名列表」已存在，跳过
出站节点「a-ui-block」的 tag 是系统保留 tag，拒绝导入
规则「国内直连」的入站「用户乙 (2887)」在本机未找到，已导入但保持禁用，请手工指定入站后启用
规则「广告拦截」引用的域名组「广告域名」不存在，整条跳过
规则「日本节点」与已有规则「JP」冲突（同一域名组下入站重叠），跳过
2 个域名组已加入订阅，最迟 10 分钟内完成首次拉取
```

前端用 `a-modal` 展示（**不是 `$message`** —— 可能几十行）。

## 6. 前端

### 6.1 按钮落点

- 三个 tab 各自 `<a-card>` 的 `<div slot="title">`，在「添加域名组」/「添加出站节点」/「添加分流规则」按钮右侧加「导出 / 导入」。
- 顶部统计卡片右侧加「导出全部 / 导入全部」。

### 6.2 导出

```
POST → 拿 obj → JSON.stringify(obj, null, 2) → Blob → URL.createObjectURL → <a download> → revokeObjectURL
```

**导出前弹二次确认**，文案：「导出文件包含出站节点的 UUID / 密码等凭据，任何拿到该文件的人都可以直接使用这些节点。请妥善保管，不要发到公开渠道。」

这不是形式主义——出站节点的 `Config` 里就是完整可用的凭据，一个随手扔进群里的 JSON 等于把落地服务器送出去。

### 6.3 导入

隐藏的 `<input type="file" accept=".json">` → `FileReader.readAsText` → POST → 报告弹窗。

前端先做一次 `JSON.parse` 和 `kind` 检查，格式明显不对就不发请求，报错更快也更具体。

### 6.4 Vue 指令必须在 `#app` 内

新增的两个 `a-modal`（导入报告、导出确认）必须留在 `<a-layout id="app">` 内。分流页的三个弹窗曾整块落在 `#app` 之后——**页面渲染完全正常、数据照常加载，但所有按钮点了毫无反应，控制台不报错**。`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条。

### 6.5 强缓存

改了 `web/assets/js/model/routing.js` 而 `config/version` 没变，浏览器会命中 `max-age=31536000` 的强缓存拿旧文件。本地开发用 `XUI_DEBUG=true`。

## 7. 改动文件清单

新增：
```
web/service/routing_portable.go        导出/导入的编解码与业务逻辑
web/service/routing_portable_test.go
```

修改：
```
web/controller/routing.go        2 个新路由与 handler
web/html/xui/routing.html        6 个按钮 + 2 个 modal + 前端逻辑
CLAUDE.md                        新增小节
```

不改数据模型，不加表，不加列。**`web/assets/js/model/routing.js` 不动**——导出/导入不需要新的模型类，它处理的是一次性的传输结构，不是要在界面上持续渲染的对象；逻辑写在 `routing.html` 的 Vue 实例里。

## 8. 已确认的决策

1. **不导出入站。** 超出「域名、节点、分流」三项，且入站是机器强相关的。
2. **导出文件明文。** 加密要引入密码管理（记不住的密码 = 打不开的备份），而文件导出后立刻落到管理员本地磁盘，加密的收益主要在传输途中——但传输途径由管理员自己决定。改为在界面上明确警示（§6.2）。
3. **跳过而非覆盖。** §5.2。
4. **域名组重名拒绝导出。** §3.2。
5. **不隐式扩大 scope。** §4.2。

## 9. 测试策略

`web/service/routing_portable_test.go`：

| 用例 | 断言 |
|---|---|
| 导出结果 | 序列化后的 JSON 里不出现 `"id"` 字段 |
| 导出结果 | 不含 `subscribedDomains` / `lastUpdatedAt` / `lastError` / `lastSkipped` |
| 两个同名域名组 | 导出返回错误，错误信息含两个组的名字 |
| **入站认不全** | 规则 `Enable == false`，且 `InboundIds` **不是** `"[]"`（回归测试，守 §2 那条不变量） |
| 入站全部命中 | 规则按文件里的 `enable` 导入 |
| `inboundRefs` 为空数组 | 原样导入为全局规则，不被误判为"认不出" |
| 入站 remark 重名 | 不按 remark 匹配，退到 port 匹配 |
| tag 为 `a-ui-block` / `a-ui-default` | 拒绝该条，`Failed` 计数 +1，报告里点名 |
| 重复导入同一文件 | 第二次全部 Skipped，库里数量不变（幂等） |
| 规则引用的域名组不存在 | 整条跳过，报告里点名 |
| `kind` / `version` 不对 | 整体拒绝 |
| `config` 为字符串 `"null"` | 拒绝该条，不 panic |

最后这条不是假想：`OutboundNodeService.Update` 里就有一条注释记着 `"null"` 能通过 `json.Unmarshal` 却留下一个 nil map，紧接着那行 `ob["tag"] = ...` 直接 panic。导入路径同样要给 `config` 强制改写 tag，会走进同一个形状。

模板：`TestAllTemplatesParse` + `TestVueDirectivesLiveInsideAVueRoot`。

端到端（真机）：A 机配好域名组 + 节点 + 规则 → 导出全部 → B 机导入 → 核对生成的 `bin/config.json` 里 `routing.rules` 与 `outbounds` 与 A 机一致（tag 相同、域名条件相同、`inboundTag` 指向 B 机对应入站）。

## 10. 风险

| 风险 | 等级 | 处置 |
|---|---|---|
| 导入把规则放大到所有入站 | **高** | §5.3 硬约束 + 专门的回归测试 |
| 保留 tag 撞名导致 xray 拒绝启动 | **高** | `model.IsReservedTag` 拦截 |
| 导出文件凭据泄露 | 中 | 导出前二次确认 + 明确警示 |
| 域名组重名导致规则绑错 | 中 | 导出侧直接拒绝 |
| 订阅未拉取期间规则不生效 | 低 | 10 分钟内自愈，报告里明说 |
| 导入"成功一半" | 低 | 逐条报告 + 幂等，重跑即补齐 |
| 大文件卡浏览器 | 低 | 不导 `SubscribedDomains`，文件降到几 KB |

## 11. 参考

- `docs/superpowers/specs/2026-09-02-domain-routing-design.md` §5 — 引用完整性的两道防线、xray 静默接受错误配置的实测表
- `docs/superpowers/specs/2026-09-02-routing-multi-inbound-design.md` §4/§5 — `InboundIds` 的编解码与空数组语义
- `web/service/routing_validate.go` — fail open 策略与完整配置校验
- `database/model/routing.go` — `IsReservedTag` 及其三个消费点
