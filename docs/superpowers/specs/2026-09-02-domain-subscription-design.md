# 域名组订阅更新 设计文档

> 本文是「域名分流管理」子系统的增量设计，前置文档为
> `2026-09-02-domain-routing-design.md`。那份文档确立的约束在此全部继续有效，
> 尤其是「绝不输出条件残缺的规则」与「生成逐字节确定」两条。

## 1. 背景与目标

管理员目前只能在域名组里手工逐条录入域名。像「国内域名合集」这类列表有四五万条、
且每天都在变，手工维护不现实。社区已有成熟的规则集仓库（blackmatrix7、skk.moe 等）
通过 HTTP 发布纯文本列表，Shadowrocket/Clash 这类客户端可以直接订阅。

**目标**：域名组可以挂一个订阅 URL，面板每天定时拉取、解析、更新，更新后分流规则
自动生效，全程不需要管理员干预、不需要重启面板或 xray。

### 非目标

- **不做规则集的完整语义支持。** 订阅文件里的 IP-CIDR、PROCESS-NAME、USER-AGENT
  等非域名规则一律跳过。域名组这个概念本身就只承载域名，把 IP 规则塞进来需要动
  整个数据模型和注入器，不在本次范围。
- **不内置代理拉取。** 面板直连订阅 URL。境内服务器拉不通 GitHub 是部署问题，
  由管理员填镜像地址自行解决（见 §9 风险）。
- **不做订阅源的增删改查界面。** 订阅 URL 是域名组的一个字段，不是独立实体。
- **不支持一个组挂多个 URL。** 需要聚合多个来源时建多个域名组、多条规则。

## 2. 可行性验证

以下均由真实 xray 26.7.28（`bin/xray-darwin-arm64`）实测确认。

### 2.1 规模：四五万条域名内联进配置完全可行

| 域名条数 | config.json 体积 | `xray run -test` 结果 | 耗时 |
|---|---|---|---|
| 5,000 | 164 KB | Configuration OK | 0.04s |
| 50,000 | 1,693 KB | Configuration OK | 0.15s |

结论：**不需要把订阅编译成 dat 文件再用 `ext:` 引用**。直接内联的方案让注入器、
校验器、生成链路全部不用改，是最省的路径。1.7MB 的 `RouterConfig` 参与
`Config.Equals()` 的 `bytes.Equal` 比较，每 10 秒一次，开销可忽略。

### 2.2 xray 不接受 URL，而且不报错

```
domain: ["https://raw.githubusercontent.com/.../ChinaMax_Domain.list"]
→ Configuration OK.
```

xray 把它当成一个裸域名做子串匹配，规则永远不命中。这是本子系统反复遇到的
「xray 静默接受错误配置」模式的又一例，也正是本功能必须在**面板侧**完成
拉取与解析的原因——不存在把 URL 直接交给 xray 的可能。

### 2.3 geosite 类别名写错会被 xray 明确拒绝

```
geosite:cn                     → Configuration OK.
geosite:nonexistent-category   → failed to check code NONEXISTENT-CATEGORY from geosite.dat
```

所以 §5.3 的落库前校验对订阅内容是有效的，不是走过场。

## 3. 数据模型

`DomainGroup` 增加五个字段，不新建表：

```go
type DomainGroup struct {
    Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
    Remark string `json:"remark" form:"remark"`
    // Domains 是管理员手工录入的域名，JSON 字符串数组。语义不变。
    Domains string `json:"domains" form:"domains"`

    // SubscribeUrl 为空表示这个组不订阅，退化成现有行为。
    SubscribeUrl string `json:"subscribeUrl" form:"subscribeUrl"`
    // SubscribedDomains 是上一次成功拉取并解析出的域名，JSON 字符串数组。
    // 与 Domains 物理隔离：订阅更新绝不覆盖管理员手工补的条目。
    SubscribedDomains string `json:"subscribedDomains" form:"subscribedDomains"`
    // LastUpdatedAt 是上一次「成功」更新的时刻，Unix 毫秒。0 表示从未成功过。
    // 调度靠它判断今天是否已经跑过，见 §6。
    LastUpdatedAt int64 `json:"lastUpdatedAt" form:"lastUpdatedAt"`
    // LastError 是上一次尝试的失败原因，成功时清空。必须在界面上显示——
    // 只进日志的话，管理员看到的是一个「域名数量停在两周前」却毫无提示的组。
    LastError string `json:"lastError" form:"lastError"`
    // LastSkipped 是上一次成功解析时跳过的非域名规则条数（IP-CIDR 等）。
    // §5.2 要求把它显示给管理员，因此必须落库——否则页面刷新后这个数字就没了。
    LastSkipped int `json:"lastSkipped" form:"lastSkipped"`
}
```

### 3.1 为什么订阅域名单独存一个字段

把解析结果直接覆盖写进 `Domains` 会**丢数据**：管理员补的
`domain:my-nas.local` 在下一次订阅更新时就没了，而且是静默消失。
两个字段物理隔离之后，各自的写入方永不交叉——`Domains` 只由管理员的表单写，
`SubscribedDomains` 只由订阅任务写。

代价只有一处：注入器要合并两个数组。这是 §4 唯一的改动点。

### 3.2 为什么不建子表

一行一域名的子表要为一次订阅更新做四五万次 INSERT，SQLite 上是秒级操作，
而且删除旧行、插入新行之间存在窗口期——那一刻 `buildRule` 读到的域名可能是空的，
会触发「域名组为空」的跳过防线，规则被丢弃、流量退回直连。
单字段整体替换是一次原子的 UPDATE，没有这个窗口。

## 4. 配置合成的改动

`RoutingInjector.buildRule` 目前这样取域名：

```go
domains, err := DecodeDomains(group.Domains)
```

改为合并两个来源。**合并顺序必须确定**，否则违反「生成逐字节确定」不变量，
`Config.Equals()` 会恒为 false，每 10 秒的重启 cron 将不停重启 xray：

```go
// 手工域名在前（按管理员录入顺序），订阅域名在后（按订阅文件出现顺序），
// 整体去重保留首次出现。三者都是确定的，因此合并结果逐字节确定。
domains := mergeDomains(manual, subscribed)
```

「域名组为空则整条规则跳过」这道防线**语义不变**：判断的是合并后的结果。
一个只有订阅、而订阅从未成功拉取过的组，合并结果为空，规则照常被跳过并记
`logger.Warning`——这正是期望行为。

## 5. 订阅更新流程

### 5.1 拉取

标准库 `net/http`，不引入依赖。三条硬限制：

| 限制 | 值 | 理由 |
|---|---|---|
| 超时 | 30s | 含连接与读取的总时长 |
| 响应体上限 | 10 MB | `io.LimitReader` 截断。不设限的话一个大文件就能打爆内存，而 **cron 没有 panic 恢复**，OOM 会杀掉整个面板进程 |
| 重定向 | 最多 5 次 | `net/http` 默认值，不改 |

只接受 `http://` 和 `https://` 两种 scheme，其余在保存表单时就拒绝。

### 5.2 解析

按文件首个非空非注释行自动识别三种格式：

| 格式 | 判别 | 映射 |
|---|---|---|
| Clash YAML | 首行为 `payload:` | `- '+.example.com'` → `domain:example.com`<br>`- 'example.com'` → `domain:example.com` |
| Surge / Clash classical | 行内含 `,` 且首段是已知规则类型 | `DOMAIN-SUFFIX,x` → `domain:x`<br>`DOMAIN,x` → `full:x`<br>`DOMAIN-KEYWORD,x` → `x`（裸写，xray 的子串匹配语义） |
| 纯域名列表 | 其余 | `.example.com` → `domain:example.com`<br>`example.com` → `domain:example.com` |

`#` 开头的整行是注释。空行跳过。

**跳过并计数**（不尝试翻译）：`IP-CIDR`、`IP-CIDR6`、`GEOIP`、`IP-ASN`、
`PROCESS-NAME`、`USER-AGENT`、`DST-PORT`、`SRC-IP-CIDR`、`RULE-SET`、
`DOMAIN-WILDCARD`、`URL-REGEX` 及一切无法识别的行。跳过总数写进日志，
并在界面显示为「已忽略 N 条非域名规则」——管理员应当知道订阅里有多少内容没被采用。

`DOMAIN-KEYWORD` 翻译成裸域名会带来子串误伤（`ads` 命中 `downloads.example.com`）。
这是该规则类型在 Shadowrocket/Clash 里的固有语义，不是本实现引入的偏差，
因此照原义翻译，不做收窄。

### 5.3 落库前校验

解析结果送 `ValidateDomains`（现有函数，走真实 xray 的完整配置校验）。
沿用其 fail open 语义：xray 二进制缺失、超时等一律放行。

### 5.4 三个必须失败的情形

以下任一发生，**保留上一次成功的 `SubscribedDomains` 不动**，只写 `LastError`：

1. HTTP 请求失败、非 2xx、超时、响应体超限
2. 解析结果为空（拉到了内容但一条域名都没解析出来）
3. `ValidateDomains` 明确判定非法

第 2 条最关键。上游仓库改结构、URL 失效返回一个 404 HTML 页面、CDN 返回空响应，
都会走到这里。若此时把 `SubscribedDomains` 清空，合并结果为空 → `buildRule` 跳过
整条规则 → **本该走指定节点或被封禁的流量静默退回直连**。这正是前置设计文档
§5.3 列为头号事故模式的那件事。宁可用旧数据，也绝不清空。

### 5.5 管理员改掉 SubscribeUrl 时

`Update` 检测到 `SubscribeUrl` 与库中不同时，**清空 `SubscribedDomains`、
`LastError`、`LastSkipped`，并把 `LastUpdatedAt` 置 0**。

保留旧数据是更危险的选项：从「国内域名合集」改成「广告拦截列表」之后，
那批旧域名会继续按新规则的动作被处理，是一次**用错误的数据分流**。清空之后
合并结果为空，规则被 `buildRule` 跳过并记 warning，管理员能察觉——符合本子系统
「宁可规则不生效，也不要它悄悄按错的条件生效」的一贯取舍。

`LastUpdatedAt = 0` 会命中 §6 的「从未成功过」分支，最多 10 分钟后自动拉取新
地址的内容。空窗期由界面提示覆盖：「订阅地址已更改，等待首次拉取」。

## 6. 调度

新增设置项 `subscriptionUpdateTime`，`string`，默认 `"04:00"`，格式 `HH:MM`。
按现有约定四处同步：`defaultValueMap`、`entity.AllSetting`、`entity.CheckValid`
（校验 `HH:MM` 且时分在范围内）、`SettingService` 的 getter。

新增 `web/job/subscription_job.go`，在 `Server.startTask()` 注册：

```go
// 每 10 分钟检查一次是否到了订阅更新时间
s.cron.AddJob("@every 10m", job.NewSubscriptionJob())
```

**不按配置时间建 cron entry**，而是固定间隔跑、job 内部判断是否该更新：

```
对每个 SubscribeUrl 非空的域名组：
    LastUpdatedAt == 0（从未成功过）？
        → 立即更新，不等时间点
    否则：今天的 HH:MM 已经过了吗？    （用 settings 的 timeLocation，与 cron 同一时区）
          且 LastUpdatedAt 不在今天之内？
        → 更新
```

「从未成功过就立即更新」这条分支是为新建的订阅组准备的。没有它，管理员填完
URL 保存后，域名组会一直空到第二天凌晨——而空的域名组会让引用它的规则被
`buildRule` 跳过，流量走直连。管理员当然可以点「立即更新」，但把一个必然要做的
动作留给人去记，本身就是缺陷。

这样换来两个收益：

- **改更新时间立即生效**，不需要重启面板。若按配置时间建 cron entry，entry 是在
  `Server.Start()` 里注册的，改设置后必须走 SIGHUP 重启面板才生效。
- **面板重启不漏更新**。若重启恰好跨过时间点，固定 entry 方案会整天不更新；
  自检方案会在下一个 10 分钟窗口补上。

代价是每 10 分钟一次空转判断，成本可忽略。判断所需的 `LastUpdatedAt` 本来就要存。

### 6.1 更新后如何生效

写库成功后调 `xrayService.SetToNeedRestart()`。此后完全复用现有链路：

```
SetToNeedRestart()
  → InboundController 注册的 10 秒 cron 消费标志
  → RestartXray(false)
  → Config.Equals() 逐字节比较发现 RouterConfig 变了
  → 重启 xray
```

**管理员不需要重启面板，也不需要重启 xray。** 这条链路已经存在且被现有功能验证过，
本功能只是多一个触发点。

### 6.2 并发

`SubscriptionJob` 与手动触发的「立即更新」可能同时跑。用一个包级
`sync.Mutex` 串行化整个更新过程即可——更新是分钟级的低频操作，
不值得做更细的按组加锁。

## 7. UI

### 7.1 域名组列表

现在这样渲染域名（`web/html/xui/routing.html:65`）：

```html
<a-tag v-for="d in group.domains" :key="d">[[ d ]]</a-tag>
```

四五万个 tag 会卡死浏览器，**必须改**。列表页改为摘要：

```
备注        域名                                      订阅              操作
国内直连    48213 条  domain:qq.com domain:163.com …   ✓ 2 小时前        编辑 删除
广告拦截    12094 条  domain:doubleclick.net …         ✗ 拉取失败(超时)  编辑 删除
ChatGPT     2 条      geosite:openai domain:chatgpt.com  —              编辑 删除
```

- 域名列只渲染**前 5 条** tag，其余折叠为「+N」
- 订阅列显示状态：成功时是相对时间，失败时红字显示 `LastError` 摘要，未订阅显示 `—`
- 每个订阅组一个「立即更新」按钮

`/xui/routing/domain-group/list` 目前把完整域名数组返给前端。改为返回
`domainCount`、`subscribedCount` 与前 5 条预览，**不再传全量**——1.7MB 的
JSON 每次开页面传一遍没有意义。

### 7.2 编辑弹窗

- 新增「订阅 URL」输入框，留空即现有行为
- 手工域名 textarea 不变
- 订阅到的域名以**只读折叠区**展示，默认收起，展开后限制渲染前 200 条并提示
  「共 N 条，此处只显示前 200 条」。不进 textarea，避免误编辑，也避免浏览器卡顿。
  数据来自 §8 的 `detail` 接口，打开弹窗时才请求
- 显示上次更新时间、失败原因、已忽略的非域名规则条数

### 7.3 统计条

顶部统计条加一项「订阅异常：N」，N 为 `LastError` 非空的组数，非 0 时红色。

## 8. 后端接口

在现有 `/xui/routing/domain-group` 组下新增一个：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/xui/routing/domain-group/refresh/:id` | 立即更新指定组。同步执行并返回结果 |
| POST | `/xui/routing/domain-group/detail/:id` | 取单个组的完整信息，供编辑弹窗使用 |

`detail` 是必需的：§7.1 把 `list` 改成了只返回摘要，编辑弹窗要展示的订阅域名
预览就没有来源了。`detail` 返回手工域名全量（管理员要编辑它）、订阅域名的
**前 200 条**（只读展示，见 §7.2）、以及订阅状态字段。订阅域名全量任何时候
都不出现在 HTTP 响应里。

`add` 与 `update` 的表单增加 `subscribeUrl` 字段，在 `encodeDomainsFromForm`
所在的收口处一并校验 URL scheme。

**保存表单时不触发拉取**，避免一个慢 URL 把 HTTP 请求挂 30 秒。
新建带订阅的组之后由管理员点「立即更新」，或等定时任务。

## 9. 改动文件清单

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `DomainGroup` 加 5 个字段 |
| `web/service/routing_subscription.go` | **新增**：拉取、格式识别、解析、失败策略 |
| `web/service/routing_subscription_test.go` | **新增**：解析器与失败路径测试 |
| `web/service/routing_domain.go` | 合并函数 `MergeDomains`；service 增加 `Refresh` |
| `web/service/routing_inject.go` | `buildRule` 改为合并两个域名来源 |
| `web/service/setting.go` | `defaultValueMap` 加 `subscriptionUpdateTime`；getter |
| `web/entity/entity.go` | `AllSetting` 加字段；`CheckValid` 校验 `HH:MM` |
| `web/job/subscription_job.go` | **新增**：定时判断与触发 |
| `web/web.go` | `startTask` 注册新 job |
| `web/controller/routing.go` | `refresh` 接口；表单加 `subscribeUrl`；list 改为摘要 |
| `web/assets/js/model/routing.js` | `DomainGroup` 类改为承载列表摘要 |
| `web/html/xui/routing.html` | 列表摘要化、弹窗订阅区、立即更新按钮、统计项 |
| `web/html/xui/setting.html` | 订阅更新时间设置项 |
| `database/db.go` | 无需改动（`AutoMigrate` 自动加列） |

## 10. 测试策略

沿用 `web/service` 包现有测试模式（`TestMain` 已 `os.Chdir` 到仓库根）。

**解析器**（纯函数，最该覆盖）：
- 三种格式各自的正确解析
- `DOMAIN-SUFFIX` / `DOMAIN` / `DOMAIN-KEYWORD` 的映射正确
- 非域名规则被跳过且计数正确
- 注释行、空行、CRLF 行尾
- 全是 IP 规则的文件 → 解析结果为空 → 必须报错而非返回空数组

**失败策略**（本功能的安全核心）：
- HTTP 404 / 超时 / 响应体超限 → `SubscribedDomains` 保持不变，`LastError` 被写入
- 解析结果为空 → 同上
- 用 `httptest.Server` 构造，不依赖外网

**合并**：
- 手工与订阅有重复项 → 去重且顺序确定
- 同一输入多次合并结果逐字节一致（守「生成逐字节确定」不变量）
- 订阅为空但手工非空 → 规则正常生成
- 两者都为空 → `buildRule` 跳过并给出原因

**调度判断**（纯函数，抽出来单独测）：
- 已过时间点且今天没更新过 → 应更新
- 已过时间点但今天更新过 → 不应更新
- 未到时间点 → 不应更新
- `LastUpdatedAt == 0` 且未到时间点 → 仍应更新（新建组的分支）
- 跨时区正确（用 settings 的 `timeLocation`）

**模板**：改完 `routing.html` 跑 `web/html_test.go` 的
`TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot`。
后者尤其重要——新增的弹窗内容若落在 `#app` 之外会完全静默失效。

## 11. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| 面板在境内，拉不通 GitHub | 功能不可用 | 界面明确显示网络错误；管理员可填镜像地址。不内置代理（见非目标） |
| 管理员填内网地址（SSRF） | 面板可探测内网 | 本面板是单管理员系统，管理员本就有 shell 级权限，不构成提权。仅限制 scheme 为 http/https |
| 订阅源改格式导致解析为空 | 规则退回直连 | §5.4 第 2 条：解析为空视为失败，保留旧数据 |
| 上游列表暴涨到几十万条 | 配置膨胀、xray 启动变慢 | 10MB 响应体上限天然封顶（约 30 万条）。超限视为失败并保留旧数据 |
| 订阅内容含非法 geosite 引用 | xray 拒绝启动 | §5.3 落库前过真实 xray 校验 |

## 12. 参考

- 前置设计：`docs/superpowers/specs/2026-09-02-domain-routing-design.md`
- 项目约束：`CLAUDE.md` 的「域名分流管理」与「已知偏差与注意事项」两节
