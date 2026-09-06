# 域名维度流量归因与 Top 榜单 设计文档

- 日期：2026-09-06
- 状态：设计已确认，待实现
- 相关：`2026-09-04-traffic-history-design.md`（分时桶）、`2026-09-02-domain-routing-design.md`（分流注入器）、`2026-09-05-routing-ip-and-dns-design.md`（DNS 注入器）

## 1. 背景与目标

管理员的原始诉求：**「有的人用节点上传很多东西，不清楚具体哪些域名上传的」**。

现状能回答「这个入站一共上传了 5 GB」（`inbounds.up`、用量图），也能回答「这个入站访问过哪些域名」（访问日志明细），
但两者之间没有桥——**没有任何一处能把字节数归因到域名**。

目标：

1. 按用户（入站）× 周期（1h / 6h / 12h / 24h / 7d / 15d）给出 Top 域名榜单。
2. 榜单最终要能按**上传字节**和**下载字节**排序，而不只是访问次数。
3. 全过程自动，管理员不需要事先圈定要监控哪些域名。
4. 访问日志弹窗的「自动刷新」默认打开。

### 非目标

- **不在访问日志明细表格里加「本次访问消耗流量」列。** §2 证明这个数在 xray 里不存在，加这一列只能靠估算，
  而估算等于用假数据覆盖真数据（与 `2026-09-04-traffic-history-design.md` §8 拒绝为改时区做补偿是同一条原则）。
- 不做全局（跨入站合并）榜单。诉求是「针对每个用户」，全局榜没有对应的使用场景。
- 不计量被管理员自己的分流规则带走的流量（见 §6.6）。
- 不修改 xray-core。`bin/xray-*` 必须保持与 `go.mod` 里 `xray-core` 同版本的官方构建。

## 2. 已核实的事实：xray 不提供按域名的流量

两条都从锁定版本 `xray-core v1.260327.1-0.20260728075948-5ca6f4b7d4dc` 的源码确认，不是推测。

### 2.1 访问日志里没有字节数

`common/log/access.go` 的 `AccessMessage` 全部字段是 `From / To / Status / Reason / Email / Detour`，
`String()` 拼出来的就是 `util/accesslog/accesslog.go` 在解析的那一行。没有任何长度或字节字段。

而且这条日志在**连接建立时**写出，连接关闭时不会再写第二条——即便将来上游加了字节字段，
也拿不到「这条连接最终传了多少」。

结论：`util/accesslog` 的解析器无论怎么改都解析不出流量。

### 2.2 stats 计数器只有三个维度

全仓库 grep `>>>` 的结果（排除测试）只有三种命名：

| 计数器 | 产生位置 |
|---|---|
| `inbound>>><tag>>>>traffic>>>{uplink,downlink}` | `app/proxyman/inbound/always.go:28,36` |
| `outbound>>><tag>>>>traffic>>>{uplink,downlink}` | `app/proxyman/outbound/handler.go:41,49` |
| `user>>><email>>>>traffic>>>{uplink,downlink}` / `user>>><email>>>>online` | `app/dispatcher/default.go:164,173,202,208,225` |

**没有域名维度，也没有「域名 × 用户」的组合维度。** `app/metrics/metrics.go:182` 只是把同一批计数器
换成 Prometheus 格式暴露，不新增维度。

推论：要拿到按域名的真实字节数，**唯一途径是让每个「要计量的域名」拥有独立的出站 tag**。
这就是 §6 的全部设计动机。

### 2.3 出站统计当前是关闭的

模板 `web/service/config.json` 的 `policy.system` 只有 `statsInboundUplink` / `statsInboundDownlink`，
**没有 `statsOutboundUplink` / `statsOutboundDownlink`**。第二期必须补上这两项。

`policy` 在 `xray/hot_diff.go:55` 的 static 名单里 → 补上它会触发**一次**整进程重启。这是一次性代价，
之后计量池的增删走出站热应用，不重启。

### 2.4 出站流量已经在手边，只是被丢掉了

`xray/process.go:241` 的 `GetTraffic(reset)` 返回的 `[]*Traffic` 里 `IsInbound` 两种都有；
`XrayTrafficJob` 每 10 秒调它一次（`reset=true`），而 `InboundService.AddTraffic`（`web/service/inbound.go:269`）
的循环只处理 `traffic.IsInbound`，出站条目被静默丢弃。

**第二期的采集必须挂在这同一次拉取上。** `reset=true` 会清零计数器，另起一次独立拉取会让两条链路互相偷数据。

### 2.5 保留期：原始访问日志只有 7 天

`web/service/setting.go:39` 的 `accessLogRetentionDays` 默认 `"7"`（`entity.CheckValid` 限 1~365）。
「最近半个月」直接查 `access_log` 在默认配置下必然是空的。§4 的预聚合顺带解决这个问题，
管理员不需要为了看 15 天榜单去调大日志保留期、多存几百万行原始记录。

### 2.6 域名归并的依赖已经在了

`golang.org/x/net v0.57.0` 是 `go.mod:19` 的**直接依赖**（`web/html_test.go` 用它的 `html` 子包），
其 `publicsuffix` 子包提供 `EffectiveTLDPlusOne(domain string) (string, error)`。**零新增依赖。**

## 3. 分期

| 期 | 交付 | 数据 | 风险 |
|---|---|---|---|
| 一 | 自动刷新默认开 + Top **访问次数**榜 | 真实（连接次数） | 低：不碰 xray 配置，不重启 |
| 二 | 榜单增加**上传/下载字节**排序 | 真实（xray 自己数的字节） | 高：改分流注入器与默认出站语义 |

两期共用 §4 的同一张表和同一块 UI。第一期就把时区对齐、清理、删除入站连带清理、
查询接口、榜单渲染一次做对；第二期只是往同一行补 `Up`/`Down` 两列，加一个排序维度。

**第一期不得为了「先跑起来」而绕开第二期需要的结构**——那会让第二期推翻第一期，
分期的唯一理由（早点看到东西且不返工）就没了。

## 4. 数据模型

### 4.1 `model.DomainStat`

落在**已有的用量库** `/etc/<name>/<name>-traffic.db`（`database.InitTrafficDB`），
与 `TrafficBucket` 同库同理由：高频写入不该和面板的普通操作抢主库那把 SQLite 写锁。

```go
type DomainStat struct {
    Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

    // 复用 TrafficBucket 那套粒度常量（GranularityHour / GranularityDay），
    // 不另定义一套 —— 两张表的清理都要按它套保留期。
    Granularity model.TrafficGranularity `json:"-" gorm:"uniqueIndex:idx_domain_stat,priority:1"`

    // InboundId 而不是 tag：入站改端口 tag 就变，存 tag 会让历史在那一刻断掉。
    InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_domain_stat,priority:2"`

    // BucketStart 是桶起始时刻的 **Unix 秒**，与 TrafficBucket 一致
    //（注意 AccessLog.Time 是毫秒，聚合时要转换）。按面板时区对齐。
    BucketStart int64 `json:"t" gorm:"uniqueIndex:idx_domain_stat,priority:3"`

    // Domain 是归并后的注册域名（eTLD+1），IP 字面量目标原样保留。
    Domain string `json:"domain" gorm:"uniqueIndex:idx_domain_stat,priority:4"`

    Count int64 `json:"count"` // 连接次数 —— 第一期写
    Up    int64 `json:"up"`    // 上传字节 —— 第二期写
    Down  int64 `json:"down"`  // 下载字节 —— 第二期写
}
```

唯一索引 `idx_domain_stat = (granularity, inbound_id, bucket_start, domain)`。**没有**照抄
`TrafficBucket` 的 `idx_traffic_bucket`（那个索引是 `domain` 在 `bucket_start` 之前）：
榜单查询是 `WHERE granularity=? AND inbound_id=? AND bucket_start>=?` 再 `GROUP BY domain`，
`bucket_start` 的范围条件必须排在等值条件之后、`domain` 之前才能走索引前缀——`EXPLAIN QUERY PLAN`
实测确认，`bucket_start` 排第 4 位时这个范围条件用不上索引，1h 档查询要扫掉该入站 30 天的全部
小时行（选择性只有 1/720）。代价是 `GROUP BY domain` 需要一棵临时 B 树（这个列序下 `domain` 不再
自然有序，不能像 `TrafficBucket` 那样流式分组）——这是权衡后的取舍，省下的扫描量远大于多一棵
临时 B 树的开销。`upsertDomainStat` 里 `clause.OnConflict` 的 `Columns` 顺序不需要跟着换：
SQLite 的 `ON CONFLICT` 按列的**集合**匹配唯一索引，不要求顺序一致，`TestAggregateIsIdempotent`
在改列序后原样通过验证了这一点。

小时桶与日桶**各自独立累加**，日桶不由小时桶汇总而来——汇总方案要处理「小时桶已被清理但日桶还没算」的补算逻辑
（与 `2026-09-04-traffic-history-design.md` 同一个判断）。

### 4.2 自动继承的三条约束

`DomainStat` 是分时桶，`TrafficBucket` 那三条不能弱化的约束原样适用：

1. **删除入站必须连带删除它的行**（接在 `InboundService.DelInbound`），另有 `TrafficCleanupJob` 里的
   `PruneOrphans` 兜底。SQLite 会复用自增 id，残留行会绑到下一个建出来的入站上，榜单会渲染得非常合理，
   只是画的是别人的数据。
2. **清理条件必须带 `granularity`**。复用现有的 `trafficHourRetentionDays`(30) / `trafficDayRetentionDays`(365)
   两个设置项——它们本来就是「分时桶的保留期」，不新增设置项（理由见 §5.3）。不带 granularity 的话，
   一次「清理小时桶」会把日桶一起删掉，15 天榜单静默变空。
3. **桶按面板设置的时区对齐**（`SettingService.GetTimeLocation()`），不是 UTC。

**改时区后的已知偏差同样适用**：旧桶按当时时区对齐，查询按当前时区重算刻度，整小时时区切换下两个集合不相交，
榜单在保留期内随新数据自愈。**刻意不做补偿**——重切历史桶需要早已被清理的原始访问日志。
这一条要在实现的注释里写明，不要在将来被当成 bug 去「修」。

### 4.3 `model.DomainStatCursor`

单行元数据表，同库，记「上次聚合到 `access_log` 的哪条 id」：

```go
type DomainStatCursor struct {
    Id        int   `gorm:"primaryKey"` // 恒为 1
    LastLogId int64
}
```

**位点不进 settings。** 新增设置项要同步改 5 处（`defaultValueMap` / `entity.AllSetting` /
`entity.CheckValid` / getter / `models.js` 的 `AllSetting` 构造函数），漏掉最后一处会让**整个保存配置接口失败**，
端口、证书路径一起遭殃。一个纯内部的聚合位点不值得付这个代价——与
`2026-09-05-panel-version-update-design.md` 决定「版本缓存不落库」是同一个判断。

**位点用 id 而不是时间戳。** `AccessLogService.Query` 已经依赖 `id desc` 保证翻页稳定（同一毫秒内多条记录
靠 id 定序），`Store` 是批量插入、id 单调递增。用 id 做位点意味着面板停机再久也只是补算，
既不会重复计算也不会跳过；而按时间窗重算需要「重算最近 N 小时」的启发式，停机超过 N 小时就静默丢数据。

**前提是 `access_log` 的 id 单调不回退，而这个前提在表被清空过之后不成立。**
`access_log` 的保留期清理（`AccessLogCleanupJob`）不看访问日志开关，无条件按保留期删行；
管理员关闭访问日志超过保留期再打开，或手工删库腾空间，都会让整张表清空。GORM 的 sqlite 驱动对
`primaryKey;autoIncrement` 生成的是裸 `integer`（rowid 别名），**没有** `AUTOINCREMENT`，
表清空后新行的 id 会从 1 重新开始，而位点还停在被清空前的高位——`Where("id > 位点")` 从此恒为空，
聚合永久停摆且没有任何一行日志（`DomainStatJob` 只在消费条数 > 0 时才打日志）。原先这里写的
「面板停机再久也只是补算，不会跳过」在这个前提被打破时不成立：不是补算延迟，而是永久停摆。

`DomainStatService.Aggregate` 现在有一段自愈逻辑兜底这个情形：某一轮批次读到 0 行、且位点 > 0 时，
额外查一次 `access_log` 的 `max(id)`；`max(id) < 位点`（含表空时 `max` 为 0）说明表确实被清空重排过，
而不是单纯追平，此时把位点归零并在本次调用内重试，而不是等下一个 10 分钟周期。只在读到空批次那一轮
多这一次查询，正常运行时每轮都读得到数据，没有额外开销。回归测试见
`web/service/domain_stat_test.go` 的 `TestAggregateRecoversAfterAccessLogIdsReset`。

## 5. 第一期：Top 访问次数榜

### 5.1 域名归并

`AccessLog.Target` 形如 `www.speedtest.net:443`。归并步骤：

1. 剥掉端口（复用 `util/accesslog` 里 `hostOf` 的同款逻辑，注意 IPv6 的方括号）。
2. 是 IP 字面量（`net.ParseIP` 成功）→ **原样保留**，不归并。
3. 否则 `publicsuffix.EffectiveTLDPlusOne`：`googleads.g.doubleclick.net` → `doubleclick.net`。
4. 该函数返回错误（域名本身就是一个公共后缀，如 `com`；或空串）→ 原样保留，不丢弃。

归并到注册域名而不是完整域名，有三个理由：管理员的心智是「传到哪个网站」而不是「传到哪台机器」；
截图那一屏的十几条广告域名会收敛成两三个词；**以及第二期的规则用 `domain:doubleclick.net` 一条就覆盖全部子域名**，
让计量池的容量花在刀刃上。

归并在**聚合时**做一次并落库，不在查询时做——查询要扫的是桶不是原始日志，那里已经没有完整域名了。

### 5.2 `DomainStatJob`

`@every 10m`，注册在 `Server.startTask`。首行 `defer common.Recover("域名统计聚合")`
（CLAUDE.md 已明确要求新增 job 一律照带 Recover 的写法办理）。

一轮的动作：

1. 读位点 `LastLogId`。
2. 从访问日志库取 `id > LastLogId` 的行，按 id 升序，单轮最多 20000 行，防止一次把大量数据读进内存。
3. 逐行归并域名，按 `(inbound_id, domain, hour_bucket)` 与 `(inbound_id, domain, day_bucket)` 在内存里累加。
4. 用 upsert（`ON CONFLICT ... DO UPDATE SET count = count + excluded.count`）写回。
5. 位点推进到本轮最大 id。**位点必须在写回成功之后才推进**，否则一次写失败会永久丢掉那一段。
6. 本轮读满 20000 行说明还有积压，**在同一次 `Run` 里继续下一轮**，直到某轮读不满或累计达到 20 轮
   （40 万行）为止。不这么做的话，首次启用时库里已有的几十万条积压在 10 分钟一轮、一轮两万行的节奏下
   要跑几个小时才追平，而这段时间里榜单是残缺的；20 轮的上限则防止单次 `Run` 长时间占住 CPU 与日志库。

`inbound_id = 0` 的行跳过：它们写入时就没匹配上任何入站（`api` tag 的记录就是这样），界面查不到，
榜单里也不该出现。

### 5.3 查询

```go
type TopDomainQuery struct {
    InboundId int
    Since     time.Duration // 1h / 6h / 12h / 24h / 7d / 15d
    OrderBy   string        // "count" | "up" | "down"（第一期只有 count）
    Limit     int           // controller 钳制在 1~50，越界回落 10
}
```

- `Since <= 24h` → 走小时桶；更长 → 走日桶。
- 起始刻度按当前时区对齐后回退，与 `TrafficHistoryService.buildSlots` 同一套算法。
- 服务端排序、求和、格式化字节数与时间标签；前端只管渲染。**标签在服务端格式化**是因为时区也在服务端——
  让浏览器自己格式化，访问者所在时区一变，榜单的时间范围就和面板设置的时区对不上了。
- `Limit` 的上界钳制在 controller。controller 是不可信输入的边界，与
  `InboundController.getTrafficOverview` 对 `top` 的钳制、导入接口的 10MB 上限同源。

返回体除了榜单本身，还要带**覆盖度信息**（§6.7），第一期该字段恒为「未计量」，前端据此隐藏字节列。

### 5.4 自动刷新默认打开

`web/html/xui/access_log_modal.html:108` 的 `autoRefresh: false` 改为 `true`。

同时**必须改掉 105-107 行那段注释**——它现在写着「默认开着等于替管理员选了其中一种」，
改完值不改注释，就留下一条与代码相反的说明，比不写注释更糟。新注释应记录真实理由：
管理员的主要用法是盯实时，复盘是次要用法，且开关本来就能关掉、状态还会跨弹窗保留。

`show()` 里第 144 行那句「按当前状态重新起定时器」的逻辑不变，改默认值天然生效。

## 6. 第二期：真实字节数计量

### 6.1 计量池

面板自动维护一个**全局的**「被计量的注册域名集合」（所有入站共用同一批域名，出站按笛卡尔积展开）。
全局一份而不是每入站一份，是为了让池的维护逻辑只有一处。

来源：`DomainStat` 里最近 24 小时出现过的域名，按 `(Up+Down) desc, Count desc, Domain asc` 排序取前 K。
**排序必须完全确定**（末位用域名字典序兜底）——`Config.Equals` 对 `OutboundConfigs` / `RouterConfig`
按字节比较，顺序一抖动就恒不相等，那个 10 秒的 cron 会不停重启 xray。

容量自适应，保证生成配置的规模有界：

```
K = min(poolCapMax, meterOutboundBudget / max(1, 已启用入站数))
poolCapMax        = 60
meterOutboundBudget = 200
```

2 个入站 → K=60（120 个计量出站）；10 个入站 → K=20（200 个）。

**冷启动的排序依据只有 `Count`**（还没有任何字节数据），所以第一轮池是按访问次数选的。
运行一段时间后 `Up+Down` 会主导排序，真正的大流量域名被稳定留在池里。
这个「先按次数、后按流量」的收敛过程要在 UI 上说清楚，不能让管理员以为第一小时的榜单就是全量真值。

**滞后（hysteresis）**：只有当新池与旧池的对称差达到 5 个域名时才真正换池。
否则排在容量边界上的两个域名会每轮互换，导致出站被反复热增删。池的重算频率取 `@every 1h`。

### 6.2 计量出站

每个 `(入站 i, 池内域名 d)` 生成一个出站，tag 为：

```
a-ui-meter-<inboundId>-<domain>      例：a-ui-meter-3-doubleclick.net
```

反查无歧义：剥掉固定前缀 `a-ui-meter-`，按**第一个** `-` 切开——`inboundId` 是十进制数字不含 `-`，
其后全部是域名（域名可以含 `-`，所以不能从右边切）。

tag 里直接带域名而不是用「槽位序号 + 映射表」，是为了消掉**槽位复用的归因错乱**：
槽位从域名 A 换成 B 时，那一轮采集里属于 A 的残余流量会被算到 B 头上，而且没有任何一层会报错。
tag 带域名则换池就是换计数器，旧 tag 被删时 xray 侧的计数器随之消失。

**`model.IsReservedTag()` 必须扩展为也拒绝 `a-ui-meter-` 前缀。** 保留 tag 不在 `outbound_nodes` 表里，
数据库唯一约束看不见它们；管理员把出站节点备注写成 `meter-3-x.com` 时 `allocTag` 就可能撞上，
xray 报 `existing tag found` 并拒绝启动整份配置——全员断网而面板首页仍显示 `running`。
CLAUDE.md 已定：新增保留 tag **只改这一个函数**，分配端 / 生成端 / 校验端都只认它。
导入路径（`routing_portable.go`）对保留 tag 的 fail-close 检查因此自动覆盖新前缀。

**出站内容必须是默认出站的完整副本**，除 tag 外逐字节相同。默认出站由 `tagDefaultOutbound` 那套逻辑确定
（管理员手写模板给首个出站起过名字时要原样沿用），**绝不硬编码 freedom**。

### 6.3 与 DNS 注入器的交互（必须处理）

`DNSInjector` 只给**数组首位**那个 freedom 出站的 `settings` 加 `domainStrategy: "UseIP"`
（`web/service/dns_inject.go`）。计量出站是 `routingInjector` 追加到数组末尾的，
不处理的话**被计量的直连流量会绕过内置 DNS**——`dns` 段对它们完全空转，没有报错、没有日志。

解法：`DNSInjector` 同时给所有 `a-ui-meter-` 前缀的出站加同样的 `domainStrategy`。

`GetXrayConfig` 里「先 `routingInjector.Inject` 再 `dnsInjector.Inject`」的顺序**不能反**
（反了 routing 那步会把 DNS 写的键无声冲掉），而这个顺序恰好让 DNSInjector 能看到计量出站。
守着这条顺序的 `TestDNSInjectorSetsFreedomDomainStrategyThroughGetXrayConfig` 要相应扩展，
新增一条断言：计量出站也拿到了 `domainStrategy`。

### 6.4 计量规则

追加在**所有现有规则之后**（模板规则 → block 组 → proxy/direct 组 → 计量组）。这样只有
「本来会走默认出站」的流量才进计量，已被管理员规则命中的流量不受影响。

每条形如：

```json
{"inboundTag": ["inbound-2886"], "domain": ["domain:doubleclick.net"], "outboundTag": "a-ui-meter-3-doubleclick.net"}
```

三个条件全部非空，不触碰「绝不输出条件残缺的规则」那条不变量。

**不生成兜底规则。** 早期设计想给每个入站加一条无 `domain` 条件的兜底规则
（`inboundTag` + `outboundTag: a-ui-meter-<i>-other`）来度量「池子漏了多少」，
这个方案必须放弃，理由是它会静默破坏 IP 规则：

> 开启 `ipRuleResolveDomain` 后 xray 走两遍规则（`app/router/router.go:245-273`），
> 只有整个第一遍都没命中才会解析域名再走第二遍。兜底规则没有 domain 条件，**第一遍必然命中**，
> 于是第二遍永远不会发生——所有用 CIDR 表达的分流与封禁规则从此静默失效，`Configuration OK`，
> 面板显示 `running`。

覆盖度改用 §6.7 的差值算法，不需要任何额外规则。

同样因为不生成兜底规则，**默认出站的语义完全不变**：没命中计量规则的流量照旧走 `OutboundConfigs[0]`。
这消掉了第二期原本最大的风险面。

**IP 字面量目标不计量**，归入「未计量」。它们需要 `ip` 条件而不是 `domain` 条件，
而管理员关心的是「哪些网站」。

### 6.5 采集

在 `InboundService.AddTraffic`（或它之前的一层）把 tag 以 `a-ui-meter-` 开头的出站条目解析成
`(inboundId, domain)`，累加进 `DomainStat` 的 `Up` / `Down`（小时桶与日桶各一次 upsert）。

必须复用 `XrayTrafficJob` 那**同一次** `GetTraffic(reset=true)` 的结果（§2.4）。

失败只告警不阻断，与 `TrafficHistoryService.Record` 同一条原则：`inbounds.up/down` 是限额与到期判定的输入，
它停止累加的后果（用户超额不被停用）比榜单少一段数据严重得多。

### 6.6 只计量走默认出站的流量

这是必须写进 UI 的边界，不是可以含糊过去的实现细节。

被管理员自己的分流规则带走的流量（走代理节点、被 block）不在计量范围内——要覆盖它们，
就得为每个「域名 × 目标出站」的组合再建一份计量出站副本，组合爆炸。

界面上这部分归入「未计量」并单独标注，**绝不混进榜单假装是 0**。

### 6.7 覆盖度：用差值，不用兜底规则

```
未计量字节 ≈ 入站总字节 − Σ(计量出站字节) − Σ(其他出站字节)
```

三项都已经有：入站来自 `inbounds.up/down` 的同一次采集，两类出站都来自同一次 `GetTraffic`。

它是**近似值**（协议开销、握手失败的连接会让入站与出站对不齐），UI 必须如实标注为「约」，
不能显示成精确数字。它的用途只有一个：告诉管理员这份榜单覆盖了多大比例的流量。
比例低时榜单不可信，比例高时榜单就是答案。

### 6.8 policy 注入

`statsOutboundUplink` / `statsOutboundDownlink` **不能只改 `web/service/config.json`**。
那是 `xrayTemplateConfig` 的默认值，管理员一旦在设置页保存过配置，模板就已落库，改 embed 文件对他静默无效。

必须像 `injectAccessLog` 那样在生成期注入：读出 `policy` 段，**只设置这两个 key**，
其余（管理员自己配的 `levels`、`handshake` 等）原样保留，再用 map 中转 Marshal 保证 key 有序、生成逐字节确定。

## 7. 前端

访问日志弹窗 `web/html/xui/access_log_modal.html` 拆成两个 tab：

- **明细** —— 现在这张表，一个字段都不改。
- **Top 域名** —— 周期按钮组（1h / 6h / 12h / 24h / 7d / 15d）+ 榜单表格。
  第一期只有「访问次数」一列；第二期增加「上传 / 下载」两列与排序切换，并在表头上方显示覆盖度。

两个既有陷阱：

- **`a-tabs` 的非活动面板仍在 DOM 里**，只是被隐藏。写选择器或做自动化时必须限定
  `.ant-tabs-tabpane-active`，否则会命中隐藏面板里的同名元素。
- **Vue 指令写在根元素之外是死代码，且完全静默。** 这个弹窗**有自己的 Vue 根实例**
  （`new Vue({el: '#access-log-modal'})`，即 CLAUDE.md 说的「照 `inbound_modal.html` 的做法」），
  所以新增的 tab 内容必须留在 `<a-modal id="access-log-modal">` 这棵子树里，**不是 `#app`**。
  `web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条。

改了 `web/assets/js|css` 而版本号没变，浏览器会命中 `max-age=31536000` 的强缓存——
本次改动集中在模板里，如果确实动了 `web/assets`，发版时要留意 `cur_ver`。

## 8. 接口

```
POST /aui/inbound/topDomains/:id
  body: { since: "1h"|"6h"|"12h"|"24h"|"7d"|"15d", orderBy: "count"|"up"|"down", limit: int }
  resp: {
    list: [{ domain, count, up, down }],
    metered: bool,        // 第一期恒 false，前端据此隐藏字节列
    coverage: {           // metered 为 false 时不显示
      totalBytes, meteredBytes, unmeteredBytes
    },
    since, orderBy, limit  // 实际生效值，前端据此回显
  }
```

沿用 `getAccessLogs` 的鉴权与路由位置（`web/controller/inbound.go:44` 附近）。
`limit` 与 `since` 在 controller 校验并钳制，非法值回落默认而不是报错。

## 9. 已确认的决策

| 决策 | 理由 |
|---|---|
| 不在明细表加「本次流量」列 | §2 证明数据不存在，估算即假数据 |
| 两期共用一张 `DomainStat` | 第一期不返工，第二期只补两列 |
| 归并到注册域名（eTLD+1） | 契合管理员心智；一条 `domain:` 规则覆盖全部子域名 |
| 位点存独立单行表，不进 settings | 新增设置项要改 5 处，漏一处整个保存配置接口失败 |
| 位点用 access_log 的 id | 停机再久只是补算，不重复不跳过 |
| 复用现有两个分时桶保留期设置 | 语义一致（都是分时桶），且避开新增设置项的代价 |
| 计量 tag 内嵌域名，不用槽位序号 | 消掉槽位复用导致的归因错乱 |
| 不生成无 domain 条件的兜底规则 | 会让 `ipRuleResolveDomain` 的第二遍规则永不发生，IP 规则静默失效 |
| 覆盖度用差值估算 | 不需要任何额外规则，风险为零；如实标注为近似值 |
| 榜单放访问日志弹窗的 tab | 入口就在排查现场，看完榜单能切回明细查具体记录 |

## 10. 改动文件清单

### 第一期

| 文件 | 改动 |
|---|---|
| `database/model/domain_stat.go` | 新增 `DomainStat`、`DomainStatCursor` |
| `database/db.go` | 用量库 `AutoMigrate` 加两张表 |
| `web/service/domain_stat.go` | 归并、聚合、查询 |
| `web/job/domain_stat_job.go` | `@every 10m`，首行 `defer common.Recover(...)` |
| `web/web.go` | `startTask` 注册新 job |
| `web/service/inbound.go` | `DelInbound` 连带删 `DomainStat` |
| `web/job/traffic_cleanup_job.go` | 按 granularity 清理 + `PruneOrphans` |
| `web/controller/inbound.go` | `POST /topDomains/:id`，钳制 `limit`/`since` |
| `web/html/xui/access_log_modal.html` | 拆 tab、榜单、`autoRefresh: true` 及注释 |

### 第二期

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `IsReservedTag` 增加 `a-ui-meter-` 前缀 |
| `web/service/meter_pool.go` | 池的计算与滞后 |
| `web/job/meter_pool_job.go` | `@every 1h` |
| `web/service/routing_inject.go` | 生成计量出站与计量规则（追加在最末） |
| `web/service/dns_inject.go` | 给 `a-ui-meter-*` 出站补 `domainStrategy` |
| `web/service/xray.go` | policy 注入 `statsOutbound*` |
| `web/service/inbound.go` | `AddTraffic` 分流出计量出站条目 |
| `web/service/domain_stat.go` | 写 `Up`/`Down`、覆盖度计算 |

## 11. 测试策略

第一期：

- 域名归并的表驱动测试：普通域名、多级子域名、IP 字面量（v4/v6）、公共后缀本身（`com`）、空串。
- 聚合的位点语义：写失败不推进位点；重复跑一轮不重复累加；停机后补算。
- 时区对齐：与 `TrafficBucket` 同一套断言。
- 删除入站连带清理 + `PruneOrphans` 兜底（守 SQLite id 复用）。
- 清理必须带 granularity（构造小时桶与日桶，断言清理小时桶后日桶还在）。
- `web/html_test.go` 的两条模板不变量在改完模板后必须跑。

第二期：

- **生成逐字节确定**：同一份池连续生成两次配置，`Config.Equals` 为真。
- 计量 tag 的反查：域名含 `-`、含多级、极长域名。
- `IsReservedTag` 对 `a-ui-meter-*` 的拒绝，覆盖 `allocTag` 与导入路径两个入口。
- DNS 注入器给计量出站加上了 `domainStrategy`（扩展既有那条测试）。
- **真实 xray 的 e2e**：沿用 `web/service/xray_hot_reload_e2e_test.go` 的形状——换池走热应用且不重启进程；
  改 policy 触发整进程重启。核心不提供 `RoutingService` 时跳过并说明原因，不以「PID 变了」这种
  和真实缺陷无法区分的形式失败。
- 覆盖度差值为负时（协议开销导致）不显示负数。

`make verify`（vet + test + build）是提交前的门禁。

## 12. 风险

| 风险 | 缓解 |
|---|---|
| 第二期上线触发一次全员断线重连 | 无法避免（policy 是 static 段）。发版说明里写明，并放在第二期而不是第一期 |
| 计量池收敛期榜单不准 | UI 明示「运行 N 小时后数据趋于完整」，并显示覆盖度 |
| 一个访问次数极少但流量极大的域名长期进不了池 | 覆盖度会持续偏低，管理员据此知道榜单不可信；`poolCapMax` 可调 |
| 计量出站副本与默认出站不一致 | 副本除 tag 外逐字节相同；e2e 断言两者内容相等 |
| 入站多时生成配置膨胀 | `meterOutboundBudget` 硬上限 200，K 自适应 |
| 榜单聚合拖慢面板 | 聚合在独立库、独立 job、单轮有行数上限；查询走桶不走原始日志 |

## 13. 参考

- `common/log/access.go`、`app/proxyman/{inbound/always.go,outbound/handler.go}`、
  `app/dispatcher/default.go`（xray-core v1.260327.1-0.20260728075948-5ca6f4b7d4dc）
- `docs/superpowers/specs/2026-09-04-traffic-history-design.md`
- `docs/superpowers/specs/2026-09-05-routing-ip-and-dns-design.md`
