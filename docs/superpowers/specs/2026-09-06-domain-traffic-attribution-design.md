# 域名维度流量归因与 Top 榜单 设计文档

- 日期：2026-09-06
- 状态：第一期已实现并发版（v1.13.0）；第二期设计已完成，待实现
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
- 不计量被管理员自己的分流规则带走的流量（见 §6.9）。
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
| 二 | 榜单增加**上传/下载字节**排序 | 真实（xray 自己数的字节） | 高：改分流注入器、打开出站统计（一次整进程重启）、会与两遍路由匹配相互作用（§6.1） |

两期共用 §4 的同一张表和同一块 UI。第一期就把时区对齐、清理、删除入站连带清理、
查询接口、榜单渲染一次做对；第二期在数据层面只是往同一行补 `Up`/`Down` 两列、加一个
排序维度，**第一期的结构确实没有返工**——第二期全部的复杂度都在「怎么让 xray 数出这两个
数」这一侧（§6），与第一期的表结构无关。

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

单行元数据表，同库，记「上次聚合到 `access_log` 的哪条 id、那条记录写于何时」：

```go
type DomainStatCursor struct {
    Id          int   `gorm:"primaryKey"` // 恒为 1
    LastLogId   int64
    LastLogTime int64 // 毫秒，与 AccessLog.Time 同单位
}
```

**位点不进 settings。** 新增设置项要同步改 5 处（`defaultValueMap` / `entity.AllSetting` /
`entity.CheckValid` / getter / `models.js` 的 `AllSetting` 构造函数），漏掉最后一处会让**整个保存配置接口失败**，
端口、证书路径一起遭殃。一个纯内部的聚合位点不值得付这个代价——与
`2026-09-05-panel-version-update-design.md` 决定「版本缓存不落库」是同一个判断。

**位点是 `(LastLogTime, LastLogId)` 的复合序，`LastLogTime` 是主序，`LastLogId` 只用来打破
同一毫秒内多条记录的并列。** 这个设计经过了三轮被真实反例推翻的迭代，记录下来是为了不要在
将来重新犯同一类错误：

1. **v1「id 单调递增，位点只会补算不会跳过」**——假前提。`access_log` 的自增 id 是可以被
   复用的 rowid，不是真正单调的序列：GORM 的 sqlite 驱动对 `primaryKey;autoIncrement`
   生成的是裸 `integer`（rowid 别名），**没有** `AUTOINCREMENT`，新行的 id 恒为**当前表内
   `max(rowid) + 1`**。`access_log` 的保留期清理（`AccessLogCleanupJob`，不看访问日志
   开关）、`AccessLogService.DeleteByInbound`（接在 `InboundService.DelInbound`）、
   `PruneOrphans` 都会删行，只要被删掉的行占据了当时最高的那批 id（哪怕只是部分删除、
   表并未清空），后续新写入的行就会复用比位点更小的 id，`Where(id > 位点)` 从此再也读不到
   这批新数据——**位点失效不一定表现为空批次**，只要新增行数没有超过被释放的 id 数，
   `id > 位点` 依然会读到非空结果（哪怕结果里混着一批本该被排除的新数据也不例外），
   这一点直到 v3 才被真正证伪（见下方"缺陷"表）。
2. **v2「空批次时把位点归零重来」**——修了 v1 的停摆问题，但引入了新的错误：部分删除场景
   下，`id > 位点` 读到空批次，归零后下一轮会把删除前就已经聚合过的、另一个未被删除的入站
   的历史行重新读一遍，产生静默的虚高计数。
3. **v3「空批次时对齐到当前 `max(id)`」**——修了 v2 的虚高问题（不再归零、只对齐到当前
   实际存在的最大 id），但被证明在**这一版本最初要解决的场景**（整表清空后有新数据落地）
   下同样会出错：自愈检测发生的时刻，新数据往往已经落地在表里，此时查到的 `max(id)`
   已经包含了这条从未被聚合过的新数据，对齐到它等于直接把这条数据标记成"已处理"，
   永久跳过、不留任何痕迹。用两组互相矛盾的回归测试交叉验证过：`0` 和 `max(id)` 这两个
   候选值，没有一个能同时满足"整表清空 + 有新数据"与"部分删除 + 无新数据"这两类场景。
   根因是 `access_log` 的自增 id 一旦被复用，同一个 id 值在不同时刻可能对应完全不同的
   逻辑行，仅凭"当前 id 集合 + 旧位点"这两个都基于 id 的信息，无法可靠区分。

**v4（当前版本）不再试图找到"正确的 id 候选值"，而是换掉判据本身**：`AccessLog.Time` 是
写入时刻，删除任何行都不会改变其余行的 `Time`，因此以 `Time` 为位点主序之后，"id 是否
失效"这个问题不再存在。查询条件从 `Where(id > 位点)` 换成：

```sql
WHERE time >= ? AND (time > ? OR id > ?)   -- ?, ?, ? = LastLogTime, LastLogTime, LastLogId
ORDER BY time ASC, id ASC
LIMIT ?
```

（这个写法与更直白的 `time > ? OR (time = ? AND id > ?)` 逻辑完全等价，四种真值组合逐一
核对过：`time>位点` 命中；`time=位点 且 id>位点id` 命中；`time=位点 且 id<=位点id` 被内层
排除；`time<位点` 被前导 `time >= ?` 排除——写成带前导范围约束的形式纯粹是性能考虑，见
下方"性能"一节，与语义无关。）

推进逻辑也随之简化：本批（已按 `time asc, id asc` 排序）**最后一条**的 `(Time, Id)` 就是
新位点——排序键与位点字段完全对应，不再需要额外遍历取 max，也不再需要任何自愈或回退逻辑：
新行不管实际拿到的 id 是多少，只要写入时刻在位点之后就会被读到。`id` 退化成只在同一毫秒内
排序用的次要字段。**这不是"两行随机撞进同一毫秒"式的概率论小概率事件**：真正的触发条件是
确定的——"位点尾部恰好停在某一毫秒"与"那一毫秒之内、之后发生过释放高位 id 的删除"同时
成立，此时若新数据又落进同一毫秒、复用了比位点 id 更小的号，会被 `id > 位点id` 排除。
影响有界（只波及与位点尾部共享同一毫秒的那几条）且方向是漏读（不会重复计数），实践中可以
接受，但不应该用"概率可忽略"这种说法掩盖它有确定触发条件这一事实。

四个场景在 v4 下全部自然成立，不需要任何专门代码路径处理：

| 场景 | 现象 | v4 下的结果 |
|---|---|---|
| ① 整表清空后有新数据落地 | 位点 (t3, 3)，新行 time=t_after > t3、id=1（复用） | `time > t3` 直接命中，与 id 无关，正确聚合（`TestAggregateRecoversAfterAccessLogIdsReset`） |
| ② 部分删除（删掉高位 id），之后无新数据 | 位点 (t6, 6)，剩下的行 time ≤ t6 | 剩下的行不满足 `time > t6`，也不满足 `time = t6 且 id > 6`，不重读（`TestAggregateDoesNotDoubleCountAfterPartialDelete`） |
| ③ 混合：删掉高位 id 后又有新数据复用同一段 id 区间（数量相等） | 位点 (t6, 6)，新数据 time=t_new > t6，id 复用为 4/5/6 | `time > t6` 直接命中，与新数据的 id 是否"反超"位点无关（`TestAggregateHandlesMixedPartialDeleteAndNewData`） |
| ③′ 混合，且新数据量**超过**被释放的 id 数（新行的 id 反超旧位点） | 位点 (t6, 6)，新数据 5 条 time=t_new > t6，id 复用为 2~6，其中 5、6 反超了旧位点 4（B 实际只释放了 3 个 id） | v1~v3（凡是靠比较 id 判断是否需要处理）**完全检测不到**这个场景：`id > 6` 天然非空（因为 id=6 存在于新数据里），自愈分支根本不会触发，(2,3,4] 区间的新数据永久跳过、无任何日志。v4 与 id 无关，全部正确聚合（`TestAggregateHandlesNewDataExceedingReleasedIdRange`） |

场景③′是本设计从 v1 到 v3 全部漏掉的格子——前三版都只在"空批次"这个信号上做文章，而这个
场景根本不会产生空批次。这也是为什么 v4 换掉的是判据本身，而不是在同一个判据里换一个候选
值：只要还在比较任何形式的 id，就无法同时满足场景②与场景③′。

**残留代价（刻意接受，不做补偿）：系统时钟异常。** `AccessLog.Time` 是 xray 写日志行时的
系统墙钟，不受本项目控制，有两种严重程度不同的失常：

1. **回拨**（NTP 步进、DST 秋季回拨每年一次）：日志的 `Time` 在同一批、甚至前后两批之间
   倒退。回拨期间写入的日志，其 `Time` 小于当时的位点时间，会被这个查询条件永久跳过。
   后果是**榜单少一段数据，不会重复计数**——方向落在安全侧，与本项目其它地方"宁可漏读
   也不能重复计入"的取舍一致。同一批内 Time 非单调（比如 id 更小的行反而 Time 更晚）
   不会导致任何重复计数，因为同一批内的行不区分顺序、逐条计入一次；只有当"更早时刻的
   记录"是在位点已经推进到"更晚时刻"**之后**才姗姗来迟时，才会被跳过。回归测试见
   `TestAggregateClockRollbackSkipsRatherThanDoubleCounts`——**这条测试断言的 `n == 0`
   守的是一个刻意接受的取舍，不是正确性不变量**：将来如果给这类跳过加缓解措施（比如
   预留一个回看窗口），这条测试会先变红，那不代表改坏了东西，是提醒改动者正在触碰一个
   已知取舍。
2. **超前，且后续被校正**（NTP 故障、误设系统时间、虚拟机快照回滚后再前进）：更严重的
   一类，不是"漏一段"而是**永久停摆**。时钟超前期间写下的日志把位点顶到未来某个时刻；
   时钟校正回来之后，`time >= 位点` 从此永远不可能被真实数据满足——聚合从那一刻起彻底
   不再更新，停摆幅度等于当时时钟前跳的量（可能几小时到几天），没有边界。
   `AccessLogCleanupJob` 的清理条件是 `time < cutoff`，永远删不到这些"来自未来"的行，
   这个状态**不会自愈**。更麻烦的是，`TopDomains` 用 `bucket_start >= since` 圈定榜单
   窗口，故障期间访问过的域名会**永久钉在每一个档位的榜单里**——它们的桶起点本来就是
   未来，永远不会被"最近 N 天"这个滑动窗口排除出去。触发源不止直接改系统时间一种：面板
   与 xray 是两个独立进程，`time.Local` 是进程内缓存，若在两者的重启窗口内改过系统
   时区，两个进程对同一时刻的本地时间解读可能整体错位，最多相差 26 小时（两个时区偏移
   之差的极值）。**明确不做自愈**——下调位点正是 v1→v3 反复栽进去的陷阱，这里只做侦测：
   位点时间超前当前系统时间超过 24 小时即记一条 `logger.Warning`，说清楚发生了什么、
   需要人工清空 `domain_stat_cursors` 表 `id=1` 的那一行才能恢复。24 小时的容差不是
   任意选的：覆盖了上面说的"改时区 + 跨进程重启窗口"这类良性抖动（最多 26 小时，但
   这个场景本身极其罕见）与普通 NTP 抖动，同时仍能及时报出真实故障。回归测试见
   `TestAggregateWarnsAndStopsWhenPositionIsInTheFuture`。

**这两种情形与"正常追平"目前被编码成了同一个返回值 `(0, nil)`，调用方（`DomainStatJob`）
只在消费条数 > 0 时才打日志。** 三者从外部完全无法区分——这不是本次改动引入的新问题，是
整个"批处理 + 位点"模式的固有局限，但值得记录：如果将来要进一步收紧对时钟异常的响应，
这是需要打破的第一个假设。

**批次截断（`domainStatBatchSize`）与同一毫秒内的多条记录：** 若某一轮读到的 20000 条恰好
在某个毫秒的中间被截断，下一轮的 `Where(time >= 位点时间 AND (time > 位点时间 OR
id > 位点id))` 能精确从截断点后一条继续——这就是标准的 keyset 分页（seek method），`id`
只要在同一毫秒内保持唯一（access_log 主键本身保证这一点），跨批次边界既不会漏读也不会
重读。回归测试见 `TestAggregateHandlesBatchBoundaryMidMillisecond`（故意写 20500 条同一
毫秒的记录逼出这个边界）。

**迁移：** `LastLogTime` 是这一版本新加的列，`AutoMigrate` 给已存在的位点行加这一列时，
该列是零值，但 `LastLogId` 已经是一个真实的历史位点。若不处理，`time >= 0` 会命中全表，
把升级前已经聚合过的历史数据重新聚合一遍、计数翻倍——这是"重复计入"，比"漏一段历史"
严重得多。处理方式是"从现在开始，不补算历史"：`Aggregate` 检测到「`LastLogTime == 0` 且
`LastLogId > 0`」这个只可能来自升级的组合时，把位点对齐到当前 `access_log` 里 `(time, id)`
最大的那一行（表当前为空就对齐到 `(0, 0)`，等同一次全新安装），跳过升级前的全部积压，
并记一条 `logger.Warning` 说明发生了什么。宁可欠一段历史数据，也不能让 `time >= 0` 命中
全表、把升级前的数据重复计入。回归测试见
`TestAggregateMigrationAlignsToLatestExistingRowWithoutReaggregating`（表非空）与
`TestAggregateMigrationWithEmptyAccessLogResetsCleanly`（表恰好为空）。

`saveDomainStatCursor` 必须同时写 `LastLogId` 与 `LastLogTime`：只推进其中一个，会让下一次
查询的 `(time, id)` 复合序出现缺口或矛盾。

**性能：查询写法从纯 OR 改成带前导范围约束，原因是绑定参数下的真实查询计划，不是最初
以为的那样。** 这段记录一次判断失误的完整过程，因为它暴露了一类容易重犯的方法论错误。

最初的写法是 `WHERE time > ? OR (time = ? AND id > ?)`。第一次用 `EXPLAIN QUERY PLAN`
检查时，把参数**内联成字面量**执行（`db.Dialector.Explain` 就是做这件事——把绑定参数
替换成字面量再拼出 SQL 文本），得到 `SEARCH TABLE access_logs USING INDEX
idx_access_logs_time (time>?)`，误判为"走定位式索引访问"。这个结论后来被推翻：**生产
执行永远走绑定参数，而 SQLite 只有在能看见两个 `time` 比较项是同一个常量时，才能把这个
OR 折成一个范围约束去做定位式访问；绑定参数下查询规划器看不到这一点，会退化成
`SCAN TABLE access_logs USING INDEX idx_access_logs_time`——按索引顺序从头扫描、逐行用
WHERE 过滤，代价是 O(位点之前的全部历史行数)，不是 O(新增行数)。**

第一次的"验证"用另一种方式重犯了同一个错误：对照实验测的是"位点在稳态、只返回 19 行"
vs "位点=0、返回 20000 行"两种情况的**耗时**，看到约 30 倍的差异就当作"走了定位式访问"
的证据。这个差异只反映**返回行数**（20000 远大于 19），与查询是 `SEARCH` 还是 `SCAN`
无关——真正有区分力的是**固定返回行数、缩放表的总行数**：`SCAN` 的耗时会随表增长线性
上升，`SEARCH` 不会。

缩放实验（绑定参数，每次都只返回 19 行，5 万 / 20 万 / 80 万行的表分别测原写法与纯
`time > ?` 对照组）：

| 表总行数 | 原写法（纯 OR，`SCAN`） | 纯 `time > ?`（对照，`SEARCH`） |
|---|---|---|
| 5 万 | 1.44 ms | 55 µs |
| 20 万 | 5.97 ms | 50 µs |
| 80 万 | 24.7 ms | 56 µs |

原写法的耗时**严格线性于表总行数**（5 万→80 万，行数涨 16 倍，耗时从 1.44ms 涨到
24.7ms、约 17 倍），是 `SCAN` 的特征；纯 `time > ?`（作为"确定能走索引"的对照组）耗时
与表大小基本无关，稳定在 50~56 µs。

改写后的 `time >= ? AND (time > ? OR id > ?)`——与原写法**逻辑完全等价**（四种真值组合
逐一核对过，见上文）——只在 80 万行这一个表规模上测过三种位点情形（与原写法同规模对照）：

| 位点 | 原写法（纯 OR） | 改写后 |
|---|---|---|
| 稳态（尾部 19 行） | 25.1 ms / `SCAN` | 57 µs / `SEARCH` |
| 落在同一毫秒中间 | 24.0 ms / `SCAN` | 17.7 µs / `SEARCH` |
| 位点=0（全量返回 20000 行） | 43.8 ms | 43.3 ms |

三种情形两种写法返回的行数完全一致，差异纯粹来自查询计划。位点=0（全量返回 20000 行）
时两种写法耗时相当，因为这种情况下确实需要读出这么多行，谈不上"定位式"还是"扫描式"的
差异——这也解释了为什么"耗时短就是走了索引"这个直觉会失灵：返回行数才是耗时的主导因素，
只有在返回行数固定时，耗时才能反映访问模式。改写后的写法**没有在 5 万 / 20 万这两个更
小的表规模上重复测过**，缩放行为是从"逻辑完全等价 + `SEARCH` 计划"推出的合理预期，不是
逐一实测的结论，这里如实注明，不要在将来误以为它也被同样测过。

**方法论教训（这个仓库最容易重犯的一类误判，值得记住）：**

1. 判断查询计划必须用**绑定参数**执行 `EXPLAIN QUERY PLAN`，不能把参数内联成字面量——
   两者对同一个 SQL 文本可能给出不同的计划，`db.Dialector.Explain`（或任何"取生产查询
   原文再内联参数"的验证方式）系统性地掩盖这个差异。
2. 不要用"返回行数少、耗时短"当作定位式访问的证据，那只反映返回了多少行。要证明"访问
   模式是 O(匹配行数) 还是 O(表总行数)"，必须固定返回行数、缩放表的总行数，看耗时是否
   随表增长。
3. "用生产代码路径复核"本身不是免死金牌——如果复核时仍然做了参数内联，或者仍然只比较
   耗时而不控制返回行数，复核只会重复同一个错误，产生虚假的"独立验证"信心。

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

1. 读位点 `(LastLogTime, LastLogId)`。
2. 从访问日志库取 `time >= LastLogTime AND (time > LastLogTime OR id > LastLogId)` 的行，
   按 `time asc, id asc` 排序，单轮最多 20000 行，防止一次把大量数据读进内存（§4.3 有
   这个位点为什么以 time 为主序、为什么不能只用 id、以及这个查询写法为什么带前导范围
   约束的完整推演）。
3. 逐行归并域名，按 `(inbound_id, domain, hour_bucket)` 与 `(inbound_id, domain, day_bucket)` 在内存里累加。
4. 用 upsert（`ON CONFLICT ... DO UPDATE SET count = count + excluded.count`）写回。
5. 位点推进到本轮最后一条（按 `time asc, id asc` 排序后的最后一条）的 `(Time, Id)`。
   **位点必须在写回成功之后才推进**，否则一次写失败会永久丢掉那一段。
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

返回体除了榜单本身，还要带**覆盖度信息**（§6.8），第一期该字段恒为「未计量」，前端据此隐藏字节列。

### 5.4 自动刷新默认打开

`web/html/xui/access_log_modal.html:108` 的 `autoRefresh: false` 改为 `true`。

同时**必须改掉 105-107 行那段注释**——它现在写着「默认开着等于替管理员选了其中一种」，
改完值不改注释，就留下一条与代码相反的说明，比不写注释更糟。新注释应记录真实理由：
管理员的主要用法是盯实时，复盘是次要用法，且开关本来就能关掉、状态还会跨弹窗保留。

`show()` 里第 144 行那句「按当前状态重新起定时器」的逻辑不变，改默认值天然生效。

## 6. 第二期：真实字节数计量

### 6.0 实测数据

本节所有容量与代价判断都以下面三组实测为依据，不是估算。环境：darwin/arm64、
Xray 26.7.28（与 `go.mod` 锁定版本同 commit 的仓库内二进制）、全部走回环。绝对值在
低配 VPS 上会更大，但三组数据要说明的都是**随规模如何变化**，那个结论与机器无关。

**（一）配置解析耗时与体积**，每个计量出站附带一条计量规则：

| 计量出站数 | 生成配置体积 | `xray run -test` 耗时 |
|---|---|---|
| 0 | 1.3 KB | 35 ms |
| 50 | 12.7 KB | 74 ms |
| 100 | 24.2 KB | 120 ms |
| 200 | 47.1 KB | 227 ms |
| 400 | 93.6 KB | 449 ms |

严格线性，约 **1.03 ms / 计量出站**、**235 B / 计量出站**。

这条代价的落点**不是 xray 启动**（启动只发生在整进程重启时，多花 0.2 秒无人察觉），
而是 `web/service/routing_validate.go` 那套**同步 HTTP 请求里的真实 xray 校验**：
新建/编辑出站节点、新建/编辑入站、保存设置各会 `exec` 一次 `run -test`，导入路径
每个节点 1~2 次。200 个计量出站意味着这些操作各多花约 0.21 秒，导入 50 个节点多花
10~20 秒。**这才是 `meterOutboundBudget` 的真正约束来源。**

**（二）新连接建立延迟**（SOCKS5 连接 → 收到应答，即 xray 完成路由匹配并建好出站
连接的全部耗时；100 次预热后取 600 次样本，全部规则都用不匹配的域名以强制走完整
规则链）：

| 路由规则数 | 中位 | p95 |
|---|---|---|
| 0 | 0.103 ms | 0.176 ms |
| 100 | 0.104 ms | 0.183 ms |
| 200 | 0.100 ms | 0.186 ms |
| 400 | 0.111 ms | 0.182 ms |

400 条规则相对 0 条只多约 8 µs，**完全淹没在测量噪音里**（200 条那组甚至比 0 条还
快）。xray 的域名匹配走的是专门的匹配器，一次匹配是纳秒级内存操作。

结论要写进发版说明：**计量不会让节点变慢。**吞吐完全不受影响（计量出站是默认出站
的逐字节副本，转发路径一模一样）；新连接建立的额外开销在微秒量级，而建立一条 TCP
连接本身就是毫秒起步。规模的真正代价在上面第（一）项，不在网速。

**（三）计量规则形态的交叉实验**——见 §6.1，那是本期最关键的一组数据。

### 6.1 必须先解决的问题：计量规则会屏蔽 IP 规则的第二遍

**这是第二期真正的技术难点，原设计漏掉了它。**

`ipRuleResolveDomain` 打开时，注入器往 `routing` 写 `domainStrategy: "IPIfNonMatch"`，
核心据此走两遍规则（`app/router/router.go:245-273`，锁定版本源码逐行确认）：

```
第一遍：不带 DNS 客户端遍历全部规则。任何一条命中即返回。
        域名目标此时没有 IP，所以 ip 条件必然不命中。
第二遍：只有整个第一遍一条都没命中时才发生。挂上 DNS 客户端重新遍历，
        ip 条件这时才可能命中。
```

计量规则带 `domain` 条件，**必然参与并可能命中第一遍**。它一旦命中，第二遍就永远
不会发生——于是**每一条用 IP 段表达的规则，对池内域名全部静默失效**，包括：

- 模板 `web/service/config.json` 自带的 `ip: ["geoip:private"] → blocked`（每一台机器都有）；
- 管理员在域名组里配的 `Cidrs`/`SubscribedCidrs` 所生成的那条 ip 规则（封禁或分流）。

而池内域名恰恰是**流量最大的那一批**。这与 §6.4 拒绝兜底规则是同一个失效模式，只是
原设计以为「加了 domain 条件就安全」——不是。CLAUDE.md「配置注入的四条不变量」第 2 条
已经写下过这个危险（「一条写成 IP 段的封禁，是可以被一条写成域名的分流规则绕过的」），
第二期若照原设计实现，就是由面板**自己**去批量制造它。

**解法：给计量规则加一个只有第二遍才可能为真的 ip 守卫。**

```json
{"type":"field","inboundTag":["inbound-2886"],"domain":["domain:doubleclick.net"],
 "ip":["0.0.0.0/0","::/0"],"outboundTag":"a-ui-meter-3-doubleclick.net"}
```

`ip: ["0.0.0.0/0","::/0"]` 匹配任意 IP，但**要求目标已经有 IP**。域名目标在第一遍
没有 IP（`GetTargetIPs()` 返回空），条件不满足，规则整条不命中；第二遍挂上 DNS 客户端
之后域名被解析出 IP，守卫恒真，规则正常命中。于是：

- 所有既有 ip 条件规则（它们都排在计量规则之前）在第二遍**照常拿到它们本来的机会**；
- 只有它们全都没命中时，计量规则才在第二遍末尾接住这条连接——这正是「只计量本来会走
  默认出站的流量」想表达的语义；
- **零额外 DNS 解析**：第二遍本来就已经发生了（第一遍没命中才会走到这里），守卫不制造
  任何新的解析。第一遍就命中管理员规则的连接不受影响，仍然不解析。

**实测交叉验证**（真实 xray 26.7.28，`dns.hosts` 把 `t.test` 指到 127.0.0.1，即
`geoip:private` 覆盖的范围；SOCKS 客户端发 4096 B 并读回；用 `xray api statsquery`
读计量计数器）：

| 用例 | `domainStrategy` | 有 `geoip:private` 规则 | 计量规则形态 | 连接结果 | 计量计数器 |
|---|---|---|---|---|---|
| A | IPIfNonMatch | 有 | 纯 `domain` | **连通** | **5120 B** |
| B | IPIfNonMatch | 有 | `domain` + ip 守卫 | **被 blackhole 掐断** | 0 B |
| C | IPIfNonMatch | 无 | `domain` + ip 守卫 | 连通 | 5120 B |
| D | AsIs（无 `domainStrategy`） | 无 | `domain` + ip 守卫 | 连通 | **0 B** |
| E | AsIs | 无 | 纯 `domain` | 连通 | 5120 B |

- A 复现了危险：私网封禁被静默绕过，流量照常送达，且被计量。
- B 证明守卫修好了它：封禁生效，行为与「没有计量功能」时逐字节一致。
- C 证明守卫形态在第二遍照常计量。
- D 证明守卫形态在**单遍模式下永不命中**——所以形态必须按模式选。
- E 证明单遍模式必须用纯形态；此时 ip 条件规则对域名目标本来就永不命中，没有第二遍
  可屏蔽，纯形态是安全的。

**形态的选择判据是「核心会不会在路由匹配期做 DNS 解析」，也就是生成配置里
`routing.domainStrategy` 的最终值**，不是 `ipRuleResolveDomain` 这个设置项本身——
管理员可能在模板里手写过 `IPOnDemand`，那时开关为 0 但核心仍会解析：

| 最终 `routing.domainStrategy` | 计量规则形态 | 理由 |
|---|---|---|
| `IPIfNonMatch` | 带 ip 守卫 | 两遍匹配，必须让出第一遍 |
| `IPOnDemand` | 带 ip 守卫 | 单遍但按需解析，守卫可满足；排在最后天然不抢先 |
| 其余（`AsIs` / 缺失 / 无法识别） | 纯 `domain` | 不会解析，守卫恒假会让计量完全空转 |

注入器已经把 `routing` 解成 map 并在这一步写 `domainStrategy`，取最终值不需要新增任何
数据通路。**无法识别的值一律按「不解析」处理**：猜错的两种后果不对称——按「会解析」
处理而实际不会，是计量静默全空转；按「不解析」处理而实际会，是回到 A 那一行的危险。
落在前者。

**这一条看上去违反了「绝不把 domain 与 ip 并进同一条规则（那是 AND）」那条不变量，
必须解释清楚，否则将来一定会有人来「修」它。** 那条不变量约束的是**管理员表达的**
规则：管理员说「这批域名**或**这批 IP 走 B」，写成一条就变成 AND、几乎永不命中。这里
的 AND 是**刻意要的**——「域名是 X **且** 目标已经解析出 IP」，第二个合取项不是一个
匹配条件，是一个**遍次闸门**。`buildRule` 生成管理员规则时仍然严格拆成两条，一个字节
都不改；计量规则是注入器自己发出的，走独立的构造函数（§6.4），两条路径不共用代码。

### 6.2 计量池

面板自动维护「被计量的注册域名集合」，**按入站各自计算**。

> **这里修订了原设计的「全局一份池」。**原理由是「让池的维护逻辑只有一处」，但出站是
> 按 `(入站, 域名)` 笛卡尔积展开的，全局池与每入站池产生的出站数、规则数**完全相同**，
> 成本一模一样；而全局池会让「流量被某个别人不访问的站主导」的用户拿到 0 覆盖率，
> 与需求原文「针对每个用户显示出来」直接冲突。同样的成本下每入站池严格更优，没有取舍。

#### 6.2.1 容量

```
启用入站数 N（GetAllInbounds 里 Enable 为真的条数，至少按 1 算）
K = min(meterPoolCapMax, meterOutboundBudget / N)
```

| 常量 | 值 | 依据 |
|---|---|---|
| `meterOutboundBudget` | 200 | §6.0（一）：每次真实 xray 校验多花约 0.21 秒，是可接受的上限 |
| `meterPoolCapMax` | 60 | 单入站场景不必把预算吃满；60 个注册域名已能覆盖绝大多数用户流量的主体 |

2 个入站 → K=60（120 个计量出站）；10 个入站 → K=20（200 个）。入站超过 200 个时整数
除法给出 K=0，整个计量功能自动停用，不生成任何计量出站与规则——这不是需要单独处理的
边界，是预算约束的自然结果。

#### 6.2.2 排序：不能只按已实测字节排

按 `(Up+Down) desc` 排序有一个会让功能在第二天就冻死的缺陷：**没进过池的域名字节数
恒为 0，永远排在所有已计量域名之后，于是永远进不了池。**池会锁死在上线首日抓到的那
一批上，新出现的大流量站永远看不见。原设计的「冷启动按 Count 排」只解决了第一轮。

解法是把两种量放到**同一把尺子**上：用已计量域名算出「每次连接的平均字节数」，给未
计量域名做一个**仅用于排序、绝不进入界面**的折算。

```
该入站最近 24 小时（小时桶）：
  avgBytesPerConn = Σ(up+down) / Σcount      （分母为 0 时取 0）
  权重(d) = bytes(d) > 0 ? bytes(d) : count(d) × avgBytesPerConn
排序键：权重 desc, count desc, domain asc
```

末位用域名字典序兜底是硬要求：**排序必须完全确定**，`Config.Equals` 对
`OutboundConfigs`/`RouterConfig` 按字节比较，顺序一抖动就恒不相等，那个 10 秒的 cron
会不停重启 xray。

折算值只用于**选谁进池**，不写库、不出现在任何接口返回体里——§1 非目标里「估算等于用
假数据覆盖真数据」约束的是**展示**，这里是选择。这个区别要写进实现注释，否则将来会
被误当成违反非目标而删掉。

`avgBytesPerConn` 为 0（该入站还没有任何字节数据，即上线第一小时）时权重退化成
`count × 0 = 0`，排序键自然落到第二项 `count desc`——这正是原设计说的「冷启动按访问
次数选」，不需要单独的代码分支。

注意这个权重**分不出**「在池但实测为 0」与「从没进过池」——两者的 `bytes` 都是 0，都会
拿到折算权重。这是刻意的：排序只负责「谁值得一试」，「试过了确实没流量」由 §6.2.4 的
试用退场闸门负责，两件事不要混在同一个表达式里。

#### 6.2.3 候选过滤

三条，缺一条都会出事：

1. **只收真正的注册域名**：`publicsuffix.EffectiveTLDPlusOne(d)` 必须成功**且等于 `d`
   本身**。第一期的归并对公共后缀本身（`com`）与 IP 字面量是「原样保留、不丢弃」，
   这些值直接进池会生成 `domain:com` —— 一条命中**全部 `.com`** 的规则，把该入站几乎
   全部流量吸进一个计量出站，榜单从此只有一行。为此在 `util/domain` 增加
   `IsRegistrable(d string) bool`，与 `Registrable` 同文件、同一套 publicsuffix 判定。
2. **IP 字面量不进池**（被第 1 条一并挡住）：它们需要 `ip` 条件而不是 `domain` 条件，
   而管理员关心的是「哪些网站」。归入「未计量」。
3. **冷却期内的域名不参选**（见 6.2.4）。

#### 6.2.4 换池：三道闸门

池要稳。每换一次池就是一批出站与规则的增删，而**退池的 tag 在核心里留下的计数器不会
被回收**（见 6.2.5）。三道闸门，都作用在「每入站、每轮」上：

| 闸门 | 规则 | 防的是什么 |
|---|---|---|
| 最小驻留 | 进池不足 `meterMinHoldRounds`(2) 轮的域名本轮强制保留 | 刚进池还没来得及产生字节就被自己的 0 权重挤出去 |
| 替换余量 | 候选权重必须 ≥ 在位末位权重 × `meterSwapMargin`(1.25) 才替换 | 两个权重相近的域名每轮互换，出站被反复热增删 |
| 试用退场 | 在池且连续 `meterProbeGiveUpRounds`(3) 轮实测字节为 0 的域名退池，并设 `CooldownUntil = now + meterCooldown`(24h) | 一个只有连接数、没有流量的域名（探测器、失败重连）长期占着槽位 |

`MeterPoolJob` 频率 `@every 1h`，所以「轮」就是小时。

零字节的域名**不会在 `DomainStat` 里留下行**（零增量不写行，与 `TrafficBucket` 同规），
所以「在池但实测为 0」这个状态无法从 `DomainStat` 反推，必须由池表自己记账——这就是
`MeterDomain.ProbeZeroRounds` 存在的唯一理由。

#### 6.2.5 死计数器：一个必须承认、只能限制不能消除的代价

`app/proxyman/outbound/outbound.go:131` 的 `RemoveHandler` **只从 handler 表里删对象，
不注销 stats 计数器**；`app/stats/command` 的 `StatsService` 也没有任何注销 RPC
（只有 `GetStats`/`QueryStats`/`GetSysStats`/…，逐条核对过 `command.proto:85-92`）。
所以每一个退过池的 `a-ui-meter-*` tag，它的两个计数器会在核心里**留到进程退出为止**，
并且每一次 `QueryStats` 都会把它们（值为 0）返回回来。

两个后果，方向相反：

- **好的一面**：退池到下一次采集之间的残余字节不会丢——计数器还在，下一轮
  `GetTraffic` 照样取到并归到正确的域名上。所以退池不需要任何「先采集再删」的编排。
- **坏的一面**：计数器集合只增不减。按 6.2.4 的三道闸门，稳态下每入站每天退池的域名
  是个位数，但没有上限保证。

**限制手段是观测式的，不记账**：`GetTraffic` 的返回里数出「tag 以 `a-ui-meter-` 开头
但不在当前池内」的条目数，超过 `meterStaleCounterLimit`(2000) 就**冻结换池**（只停止
吸纳新域名，已在池内的域名照常计量）并记一条 `logger.Warning`。观测式而不是记账式，
是因为它天然跨重启自愈：xray 一旦整进程重启（改入站、改设置、升级核心、面板重启都会
触发），计数器全清，观测值归零，冻结自动解除，不需要面板去跟踪「上一次重启是什么时候」
这种它其实拿不准的状态。

2000 个死计数器约合每次 `QueryStats` 多传 120 KB（计数器名约 56 字符，加 protobuf 开销
每条约 60 B），10 秒一次，是可以接受的上界；按三道闸门约束下的稳态 churn 估算，要几周
才会到达。

#### 6.2.6 池落库

池必须**落库**，不能在 `RoutingInjector.Inject` 里现算：`Inject` 由那个 10 秒的重启
消费任务反复调用，现算意味着排名一变配置字节就变，cron 会不停热应用甚至重启。落库之后
`Inject` 只是按确定顺序读一张表。

```go
// MeterDomain 是「当前正在被计量」的 (入站, 注册域名) 对。
// 与 DomainStat 同库（用量库），理由相同：高频写入不抢主库那把写锁，
// 而且它的清理与 DomainStat 挂在同一个每小时任务里。
// 没有任何 json tag：这张表是纯服务端状态，既不下发给前端也不接受前端提交
//（与 model.Inbound 的 LastResetAt/DisabledByTraffic 同一条理由）。
type MeterDomain struct {
    Id        int64  `gorm:"primaryKey;autoIncrement"`
    InboundId int    `gorm:"uniqueIndex:idx_meter_domain,priority:1"`
    Domain    string `gorm:"uniqueIndex:idx_meter_domain,priority:2"`

    // EnteredAt 是进池时刻的 Unix 秒，供「最小驻留」闸门使用。
    EnteredAt int64
    // ProbeZeroRounds 是进池后连续几轮实测字节为 0。零字节不会在 DomainStat
    // 里留下行，这个状态只能由池表自己记。
    ProbeZeroRounds int
    // CooldownUntil 之前该域名不参选，Unix 秒。
    CooldownUntil int64
}
```

**删除入站必须连带删除它的池行**（接在 `InboundService.DelInbound`，与 `DomainStat`、
`TrafficBucket`、`AccessLog` 完全同构），另有 `TrafficCleanupJob` 里的 `PruneOrphans`
兜底。SQLite 会复用被删除的自增 id：残留的池行会绑到下一个建出来的入站上，于是面板会
为一个全新用户生成一批**别人的**域名的计量出站与规则——引用不再悬空，生成期没有任何一
道防线拦得住，而配置与界面都渲染得完全正常。这是 CLAUDE.md「新增任何存 id 外键的表都要
照做」那一条的直接适用。

用量库打不开（`database.GetTrafficDB()` 为 nil）时池为空 → 不生成任何计量出站与规则 →
`Metered` 为 false → 榜单退回第一期形态。整条链路对「库不可用」是 fail-open 到「没有这个
功能」，不是 fail-close 到「配置生成失败」。

### 6.3 计量出站

每个 `(入站 i, 池内域名 d)` 一个出站，tag 为：

```
a-ui-meter-<inboundId>-<domain>        例：a-ui-meter-3-doubleclick.net
```

反查无歧义：剥掉固定前缀 `a-ui-meter-`，按**第一个** `-` 切开——`inboundId` 是十进制
数字不含 `-`，其后全部是域名（域名可以含 `-`，所以不能从右边切）。反查必须校验 id 能解析
成正整数、域名非空，否则跳过并记 Debug：一个形态不对的 tag 是无法归因的，硬猜只会把字节
记到错的域名上。

tag 里直接带域名而不是「槽位序号 + 映射表」，是为了消掉**槽位复用的归因错乱**：槽位从
域名 A 换成 B 时，那一轮采集里属于 A 的残余流量会被算到 B 头上，而且没有任何一层会报错。
tag 带域名则换池就是换计数器（6.2.5 说明了旧计数器还在，残余字节仍归 A）。

**出站内容必须是默认出站的完整副本，除 tag 外逐字节相同。**默认出站由 `tagDefaultOutbound`
确定（管理员手写模板给首个出站起过名字时原样沿用），**绝不硬编码 freedom**。复制要走
JSON 往返做深拷贝——浅拷贝共享 `settings` 那个 map，§6.5 给计量出站写 `domainStrategy` 时
会同时写进默认出站，或者反过来，两者再也分不开。

追加位置在 `a-ui-block` 黑洞出站**之后**、数组最末。首位仍是默认出站（`diffOutbounds`
要求它逐字节不变），黑洞出站的位置也不受影响。

**`model.IsReservedTag()` 必须扩展为也拒绝 `a-ui-meter-` 前缀。**保留 tag 不在
`outbound_nodes` 表里，数据库唯一约束看不见它们；管理员把出站节点备注写成 `meter-3-x.com`
时 `allocTag` 就可能撞上，xray 报 `existing tag found` 并拒绝启动整份配置——全员断网而面板
首页仍显示 `running`。CLAUDE.md 已定：新增保留 tag **只改这一个函数**，分配端（`allocTag`）、
生成端（`buildOutbounds`）、校验端（`removeOutboundByTag`）、导入端（`routing_portable.go`
的 fail-close 检查）都只认它，四处自动覆盖新前缀，不需要各自再加判断。

### 6.4 计量规则

追加在**所有现有规则之后**：模板规则 → 地区规则 → block 组 → proxy/direct 组 → **计量组**。
只有「本来会走默认出站」的流量才进计量，已被管理员规则命中的流量完全不受影响。

组内顺序按 `(inboundId asc, domain asc)`，与出站顺序一致，保证生成逐字节确定。

形态按 §6.1 的判据二选一：纯形态有 `inboundTag`、`domain` 两个条件，守卫形态多一个 `ip`，
每一个都非空，不触碰「绝不输出条件残缺的规则」那条不变量。

`domain` 的值一律带显式 `domain:` 前缀——含点的裸串在 xray 里是**子串**匹配
（`infra/conf/router.go:175` 传给 `ParseDomainRules` 的默认类型是 `geodata.Domain_Substr`），
`doubleclick.net` 会命中 `notdoubleclick.net.evil`；`domain:doubleclick.net` 才是按域名边界
的后缀匹配，也正是第一期归并到 eTLD+1 想要的语义。

只为**启用且存在**的入站生成规则（复用 `buildRules` 已经建好的 `inboundTagById`）。池里
残留的、指向已停用入站的行本轮不生成规则，也不删——入站可能只是被临时停用。

**不生成兜底规则。**早期设计想给每个入站加一条无 `domain` 条件的兜底规则来度量「池子漏了
多少」，必须放弃：兜底规则没有 domain 条件，**第一遍必然命中**，于是第二遍永远不会发生，
所有用 CIDR 表达的分流与封禁从此静默失效，而 `Configuration OK`、面板显示 `running`。
（§6.1 说明了带 domain 条件的计量规则其实是同一个陷阱的弱化版，同样需要处理。）覆盖度改用
§6.8 的口径，不需要任何额外规则。

同样因为不生成兜底规则，**默认出站的语义完全不变**：没命中计量规则的流量照旧走
`OutboundConfigs[0]`。

### 6.5 与 DNS 注入器的交互

`DNSInjector.applyFreedomStrategy` 只给**数组首位**那个 freedom 出站的 `settings` 加
`domainStrategy: "UseIP"`。计量出站是 `routingInjector` 追加到末尾的，不处理的话**被计量的
直连流量会绕过内置 DNS**——`dns` 段对它们完全空转，没有报错、没有日志，正是这个功能存在的
理由所描述的那种故障。

解法：`applyFreedomStrategy` 在处理完首位之后，遍历其余出站，对 tag 以 `a-ui-meter-` 开头
的**同样**加 `domainStrategy`。**不需要给它们重跑一遍判定**：计量出站是默认出站的逐字节
副本，首位的每一条判定（`protocol` 是不是 `freedom`、`targetStrategy` 是否已被占用）对副本
必然给出相同结论；而首位判定不通过时函数本来就早退，副本也就一同不写——这正是我们要的，
给副本单独写一个默认出站没有的键，恰恰会打破「除 tag 外逐字节相同」那条不变量。
仍然**绝不用 `ForceIP` 系列**：`UseIP` 下解析失败只记日志并回落按域名直连，`ForceIP` 会
变成连接失败。

`GetXrayConfig` 里「先 `routingInjector.Inject` 再 `dnsInjector.Inject`」的顺序**不能反**
（反了 routing 那步重建 outbounds 数组时会把 DNS 写的键无声冲掉），而这个顺序恰好让
DNSInjector 能看到计量出站。守着这条顺序的
`TestDNSInjectorSetsFreedomDomainStrategyThroughGetXrayConfig` 要相应扩展：新增一条断言，
计量出站也拿到了 `domainStrategy`，且其内容与默认出站除 tag 外逐字节相同。

### 6.6 policy 注入：打开出站统计

`app/proxyman/outbound/handler.go:34-56` 的 `getStatCounter` 在
`policy.ForSystem().Stats.OutboundUplink` 为假时**根本不注册计数器**（源码逐行确认）。模板
`web/service/config.json` 的 `policy.system` 只有 `statsInboundUplink`/`statsInboundDownlink`，
所以出站字节现在一个都没有。

`statsOutboundUplink` / `statsOutboundDownlink` **不能只改 `web/service/config.json`**：那是
`xrayTemplateConfig` 的默认值，管理员一旦在设置页保存过配置，模板就已落库，改 embed 文件对
他静默无效。必须像 `injectAccessLog` 那样在生成期注入：把 `policy` 解成
`map[string]json.RawMessage`，再把其中的 `system` 解成同样的 map，**只设置这两个 key**，其余
（管理员自己配的 `levels`、`handshake`、`statsUserUplink` 等）原样保留，再逐层 Marshal 回去
（`encoding/json` 对 map key 排序，生成逐字节确定）。

**无条件注入，不看池是否为空。**`policy` 在 `xray/hot_diff.go:55` 的 static 名单里，改它必然
触发一次整进程重启；无条件注入让这次重启发生在**升级后的第一次配置变更**，时间点可预期、
可以写进发版说明。若改成「池非空才注入」，重启会推迟到上线约一小时后池第一次形成时，变成
一次谁都没预料到的全员断线。

`getStatCounter` 在 **handler 创建时**取计数器，而 gRPC `AddHandler` 走的正是同一条创建路径
——所以只要 policy 开关已经打开，**后续热增的计量出站照样有计数器**，换池不需要重启。这是
「一次性代价」这个说法成立的前提，不是想当然。

顺带的好处：出站统计打开之后，`a-ui-default`（默认出站，`tagDefaultOutbound` 保证它有 tag，
而 `getStatCounter` 要求 `len(tag) > 0`）、各出站节点、`a-ui-block` 全都开始有字节数，这正是
§6.8 覆盖度口径的输入之一。

### 6.7 采集

`XrayTrafficJob` 每 10 秒调一次 `GetTraffic(reset=true)`（取完 xray 侧清零），结果里出站条目
现在被 `InboundService.AddTraffic` 静默丢弃（那个循环只处理 `traffic.IsInbound`）。

**必须复用这同一次拉取**：`reset=true` 会清零计数器，另起一次独立拉取会让两条链路互相偷数据。

在 `AddTraffic` 里，紧挨着已有的 `TrafficHistoryService.Record` 再加一行
`DomainStatService.RecordMetered(traffics, time.Now())`，它：

1. 只收 `!IsInbound && strings.HasPrefix(Tag, "a-ui-meter-")` 的条目；
2. 反查出 `(inboundId, domain)`，反查失败的跳过并记 Debug；
3. `Up+Down == 0` 的条目**不写行**（与 `TrafficBucket` 的「零增量不写行」同规，也是 6.2.5
   里那些死计数器不会污染数据的原因）；
4. 按面板时区对齐出小时桶与日桶，各做一次 upsert：
   `up = up + ?, down = down + ?`（`clause.OnConflict` + `gorm.Expr`，与第一期的
   `count = count + ?` 完全同构，两者可以合成同一个 upsert 函数的两种调用）。

**失败只告警不阻断**，与 `TrafficHistoryService.Record` 同一条原则：`inbounds.up/down` 是限额
与到期判定的输入，它停止累加的后果（用户超额不被停用）比榜单少一段数据严重得多。

**一个必须写进注释、也必须写进 UI 的语义差异**：`Count` 来自访问日志，记的是**连接建立**时刻；
`Up`/`Down` 来自 10 秒一次的计数器采样，记的是**流量发生**时刻。一条 10:59 建立、传到 11:30 的
连接，它的 1 次计数落在 10 点桶，它的字节分落在 10 点和 11 点两个桶。所以同一个桶里
`Count` 与 `Up/Down` 不是同一批连接的统计量，**一个域名完全可能出现 `Count=0` 而 `Up=5GB`
的行**（upsert 天然支持，桶不存在就创建）。这不是 bug，是两个数据源的固有差异；把它们强行
对齐需要连接级的字节数，而 §2 已经证明 xray 不提供。

`parseTraffics`（`xray/process.go:276`）按 **tag 单键**建索引，不区分 `IsInbound`——入站 tag 是
`inbound-<端口>`，计量出站 tag 是 `a-ui-meter-*`，两者不可能撞名，现状安全。将来若有人改动 tag
生成规则，这个隐含前提要一起复核。

### 6.8 覆盖度

原设计的三项差值（`入站总字节 − Σ计量出站 − Σ其他出站`）不用了：它是近似值，而且**算不出
每个入站的覆盖度**——出站计数器不带入站维度，`a-ui-default` 把所有入站的直连流量混在一起。

改用同一张表里两个都**按入站分桶**的量：

```
该入站该周期已计量字节 = Σ DomainStat.(Up+Down)   （granularity/inbound_id/bucket_start 同一套条件）
该入站该周期总字节     = Σ TrafficBucket.(Up+Down) （同库、同粒度、同对齐、同一次 GetTraffic 采集）
覆盖率 = 已计量 / 总字节
```

两个序列来自同一次 `GetTraffic`、同一套时区对齐、同一个库，可比性是结构性的，不是巧合。

仍然是**近似值，UI 必须如实标注为「约」**：入站计数的是客户端与面板之间的**加密流**，出站
计数的是面板与目标之间的流，两者相差一层协议开销与握手，所以覆盖率结构性地小于 100%。
分母为 0 时不显示（而不是显示 0%）；比值必须钳到 `[0, 1]` 再展示——协议开销在极端情况下可能
让比值略微越界，显示一个 103% 或负数会让整块数据失去可信度。

差额的构成要在 UI 上说清楚，它有三部分：被管理员自己的分流规则带走的流量（§6.9）、走默认出站
但不在池里的域名、以及上面那层协议开销。**比例低时榜单不可信，比例高时榜单就是答案**——这是
覆盖度唯一的用途。

**`Metered` 的判据是「该入站在 `meter_domains` 里至少有一行」**，不是版本号、也不是「配置里
真的有计量规则」——后者要求查询接口去反推一次配置生成的结果，那条通路不存在，硬造出来只会
多一个会与生成端漂移的真相源。池表就是生成端读的那张表，用它做判据，两侧永远一致。

这个判据有一个已知的不精确处：入站被停用时不生成计量规则，但池行还在（§6.4 刻意不删——入站
可能只是临时停用），于是 `Metered` 仍为 true。后果仅仅是给一个没有流量的入站显示了字节列与
0% 覆盖率，可以接受；反过来把它做精确，就要在查询路径上再引入一次入站启用状态的判断，而那
个状态与「历史上这段时间是否被计量过」根本不是一回事——榜单查的是过去 15 天，入站是此刻的
状态。

`Coverage` 落在 `DomainStatService` 上而不是 `TrafficHistoryService`：它是榜单的一部分，与
榜单同一次查询返回。它读 `TrafficBucket` 这张不属于自己的表，是同库内的只读聚合，不涉及写入
方的所有权。

### 6.9 只计量走默认出站的流量

这是必须写进 UI 的边界，不是可以含糊过去的实现细节。

被管理员自己的分流规则带走的流量（走代理节点、被 block）不在计量范围内——要覆盖它们，就得为
每个「域名 × 目标出站」的组合再建一份计量出站副本，组合爆炸。

界面上这部分归入「未计量」并单独标注，**绝不混进榜单假装是 0**。

一个自然的副作用：管理员已经用规则分流走的域名，即使进了池，其计量出站也永远拿不到字节
（管理员规则排在前面）。6.2.4 的「试用退场」闸门会在 3 轮后把它退池并冷却 24 小时，槽位自动
让给别的域名——不需要为此专门做「池要排除已被规则覆盖的域名」的判断（那需要面板去求解
`geosite:`/`keyword:`/`regexp:` 条件是否覆盖某个域名，做不到也不该做）。

### 6.10 这一期会发生几次重启

| 时刻 | 发生什么 | 为什么 |
|---|---|---|
| 升级后第一次配置生成 | **一次整进程重启** | `policy` 段新增 `statsOutbound*`，在 `hot_diff` 的 static 名单里（§6.6） |
| 每次换池 | 热应用，不重启 | 出站增删（非首位）与整段路由替换都有控制面接口 |
| `ipRuleResolveDomain` 被切换 | 一次整进程重启 | `routing.domainStrategy` 变化，本来就在必须重启的清单里；计量规则形态随之切换（§6.1） |

换池的热应用里有一个已知窗口，与 CLAUDE.md 记的「编辑出站」窗口同源：`tryHotApply` 先
`DelOutbound` 再 `ApplyRoutingConfig`，两者之间旧路由仍引用着已删除的计量 tag，xray 对悬空
`outboundTag` 不报错、**静默回落默认出站**。对计量规则而言回落目标就是直连——与该流量本来的
去向完全一致，只是这一毫秒的字节没被计量。**这是所有悬空引用场景里唯一无害的一个**，刻意不做
补偿，与本子系统「失败即退回重启，不做部分回滚」的原则一致。

### 6.11 回退到旧版本二进制

- `meter_domains` 表保留不删（`AutoMigrate` 本来也不删表），旧代码不认识它，不生成任何计量出站
  与规则。
- `statsOutbound*` 不再被注入 → `policy` 段变化 → 一次整进程重启，之后出站计数器不再产生。
- `DomainStat` 的 `Up`/`Down` 两列第一期就已存在，旧代码读得到、只是恒不写：**历史字节数保留，
  不再更新**。
- 回退后的旧二进制里 `TopDomains` 根本没有 `Metered` 这条逻辑，它硬编码返回 false（第一期的
  实现就是如此），前端据此隐藏字节列——不会出现「显示着一列停在昨天的上传量」这种最难排查
  的形态。`meter_domains` 表里残留的行不影响这一点：旧代码压根不读它。

回退方向上没有数据损坏，也没有「范围被放大」的规则，落在安全侧。

## 7. 前端

访问日志弹窗 `web/html/xui/access_log_modal.html` 拆成两个 tab：

- **明细** —— 现在这张表，一个字段都不改。
- **Top 域名** —— 周期按钮组（1h / 6h / 12h / 24h / 7d / 15d）+ 榜单表格。

第二期在「Top 域名」这个 tab 里增加四样东西，四样都由 `metered` 一个字段统一开关：

1. **上传 / 下载两列**，与「访问次数」并列，用 `sizeFormat` 格式化。
   `metered` 为 false 时**必须整列隐藏**，不能显示一列恒为 0 的「上传」——那会被
   理解成「他没上传过」，比不显示更糟。
2. **排序切换**：次数 / 上传 / 下载三选一，作为 `orderBy` 参数发给服务端。排序在服务端做
   （§5.3 已定：服务端排序、求和、格式化，前端只管渲染）。
3. **覆盖度条**，在表头上方一行：
   「本周期该用户约 **72%** 的流量已归因到具体域名；其余为被分流规则带走的流量、不在计量
   范围内的域名，以及协议开销。」
   `coverage.ratio` 为 null（分母为 0）时整行不显示，而不是显示 0%。
4. **收敛期提示**：计量池按小时重算、按实测字节收敛（§6.2.2），上线第一小时的排序完全来自
   访问次数。这一点必须说出来，不能让管理员以为第一小时的榜单就是全量真值。提示在
   `coverage.ratio` 低于某个阈值或该入站尚无字节数据时显示。

还要在 tab 内显示一句关于 `Count` 与 `Up/Down` 语义差异的说明（§6.7）：次数按**连接建立**
时刻归桶，字节按**流量发生**时刻归桶，所以同一行出现「0 次访问、5 GB 上传」是正常的——那是
一条跨桶的长连接。

三个既有陷阱：

- **`a-tabs` 的非活动面板仍在 DOM 里**，只是被隐藏。写选择器或做自动化时必须限定
  `.ant-tabs-tabpane-active`，否则会命中隐藏面板里的同名元素。
- **Vue 指令写在根元素之外是死代码，且完全静默。** 这个弹窗**有自己的 Vue 根实例**
  （`new Vue({el: '#access-log-modal'})`，即 CLAUDE.md 说的「照 `inbound_modal.html` 的做法」），
  所以新增的 tab 内容必须留在 `<a-modal id="access-log-modal">` 这棵子树里，**不是 `#app`**。
  `web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条。
- 改了 `web/assets/js|css` 而版本号没变，浏览器会命中 `max-age=31536000` 的强缓存——
  本次改动集中在模板里，如果确实动了 `web/assets`，发版时要留意 `cur_ver`。

## 8. 接口

```
POST /aui/inbound/topDomains/:id
  body: {
    range:   "1h"|"6h"|"12h"|"24h"|"7d"|"15d",   // 缺省/非法回落 "24h"
    orderBy: "count"|"up"|"down",                // 缺省/非法回落 "count"；第二期新增
    limit:   int                                  // 缺省/越界回落 10，上界 50
  }
  resp: {
    list:    [{ domain, count, up, down }],
    metered: bool,      // 该入站在 meter_domains 里是否有行（§6.8）；false 时前端隐藏字节列
    range:   string,    // 实际生效值，前端据此回显
    orderBy: string,
    limit:   int,
    coverage: {         // 第二期新增；metered 为 false 时整个对象为 null
      meteredBytes: int64,    // Σ DomainStat.(up+down)
      totalBytes:   int64,    // Σ TrafficBucket.(up+down)，同粒度同窗口
      ratio:        float|null // meteredBytes/totalBytes，钳到 [0,1]；分母为 0 时 null
    }
  }
```

字段名以第一期已落地的实现为准（`range`，不是 `since`）。`orderBy` 与 `coverage` 是第二期
新增，**旧前端读到多出来的字段会忽略，新前端读到缺失的 `coverage` 要按 null 处理**——回退到
旧二进制时后者就是实际形态（§6.11）。

沿用 `getAccessLogs` 的鉴权与路由位置（`web/controller/inbound.go:46`）。`limit` / `range` /
`orderBy` 一律在 controller 校验并钳制，非法值回落默认而不是报错：这是个展示接口，一个拼错的
参数不该变成报错弹窗。钳制放在 controller 是因为它是不可信输入的边界，与
`InboundController.getTrafficOverview` 对 `top` 的钳制、导入接口的 10MB 上限同源。

## 9. 已确认的决策

| 决策 | 理由 |
|---|---|
| 不在明细表加「本次流量」列 | §2 证明数据不存在，估算即假数据 |
| 两期共用一张 `DomainStat` | 第一期不返工，第二期只补两列 |
| 归并到注册域名（eTLD+1） | 契合管理员心智；一条 `domain:` 规则覆盖全部子域名 |
| 位点存独立单行表，不进 settings | 新增设置项要改 5 处，漏一处整个保存配置接口失败 |
| 位点以 access_log 的 Time 为主序、id 只做同毫秒内排序 | 单靠 id 无法区分「已聚合的历史行」与「复用了同一 id 的新数据」（§4.3 完整推演），Time 不受行删除影响 |
| 复用现有两个分时桶保留期设置 | 语义一致（都是分时桶），且避开新增设置项的代价 |
| 计量 tag 内嵌域名，不用槽位序号 | 消掉槽位复用导致的归因错乱 |
| 不生成无 domain 条件的兜底规则 | 会让 `ipRuleResolveDomain` 的第二遍规则永不发生，IP 规则静默失效 |
| **计量规则在会解析域名的模式下带 `ip: ["0.0.0.0/0","::/0"]` 守卫** | 不带守卫会在第一遍命中并吃掉第二遍，让模板的 `geoip:private` 与管理员所有 CIDR 规则对池内域名静默失效；A/B 两个用例在真实 xray 上实测（§6.1） |
| **形态判据取生成配置里 `routing.domainStrategy` 的最终值，不取设置项** | 管理员可能在模板里手写 `IPOnDemand`；无法识别的值按「不解析」处理，因为猜错的两种后果不对称 |
| **计量池按入站各自计算，不用全局池** | 出站数与规则数完全相同（笛卡尔积），但全局池会让流量结构与他人不同的用户拿到 0 覆盖率，与「针对每个用户」冲突 |
| **未计量域名按 `count × 平均每连接字节` 折算权重参与排序** | 纯按实测字节排会让池锁死在上线首日那一批（新域名字节恒为 0，永远进不来）；折算只用于选池，不进任何接口返回体 |
| **池落库，不在 `Inject` 里现算** | `Inject` 由 10 秒的 cron 反复调用，现算会让排名一变配置字节就变，cron 不停热应用甚至重启 |
| **无条件注入 `statsOutbound*`，不看池是否为空** | `policy` 必然触发整进程重启；无条件注入让这次重启落在可预期、可写进发版说明的时刻 |
| **覆盖度改用「Σ DomainStat 字节 / Σ TrafficBucket 字节」，不用三项差值** | 差值算不出每入站的覆盖度（出站计数器不带入站维度），而这两个量本来就同库、同粒度、同对齐、同一次采集 |
| **死计数器用观测式上限冻结，不记账** | `RemoveHandler` 不注销计数器且 `StatsService` 没有注销 RPC；观测式跨 xray 重启天然自愈，记账式要面板跟踪它其实拿不准的重启时刻 |
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
| `web/controller/inbound.go` | `POST /topDomains/:id`，钳制 `limit`/`range` |
| `web/html/xui/access_log_modal.html` | 拆 tab、榜单、`autoRefresh: true` 及注释 |

### 第二期

| 文件 | 改动 |
|---|---|
| `database/model/routing.go` | `IsReservedTag` 增加 `a-ui-meter-` 前缀（一处改动，四个消费点自动覆盖） |
| `database/model/meter.go` | 新增 `MeterDomain`（§6.2.6） |
| `database/db.go` | 用量库 `AutoMigrate` 加 `MeterDomain` |
| `util/domain/domain.go` | 新增 `IsRegistrable(d string) bool`（§6.2.3 第 1 条） |
| `web/service/meter_pool.go` | 池的排序、三道闸门、死计数器观测、落库 |
| `web/job/meter_pool_job.go` | `@every 1h`，首行 `defer common.Recover("计量池重算")` |
| `web/web.go` | `startTask` 注册 `MeterPoolJob` |
| `web/service/routing_inject.go` | `buildMeterOutbounds` / `buildMeterRules`，追加在出站与规则的最末；形态按 `routing.domainStrategy` 二选一 |
| `web/service/dns_inject.go` | `applyFreedomStrategy` 遍历并给 `a-ui-meter-*` 出站补 `domainStrategy` |
| `web/service/xray.go` | `GetXrayConfig` 调 `injectOutboundStats` |
| `web/service/accesslog.go` 或新文件 | `injectOutboundStats(cfg)`：只设 `policy.system` 的两个 key，其余原样保留 |
| `web/service/inbound.go` | `AddTraffic` 增加一行 `RecordMetered`；`DelInbound` 连带删 `MeterDomain` |
| `web/service/domain_stat.go` | `RecordMetered`、`Coverage`、`TopDomains` 支持 `orderBy` 与 `Metered` |
| `web/job/traffic_cleanup_job.go` | `PruneOrphans` 覆盖 `MeterDomain` |
| `web/controller/inbound.go` | `orderBy` 参数的校验与钳制 |
| `web/html/xui/access_log_modal.html` | 字节列、排序切换、覆盖度条、收敛期提示 |
| `web/service/config.json` | `policy.system` 补两个 key（只影响全新安装的默认模板；存量靠生成期注入） |

## 11. 测试策略

第一期：

- 域名归并的表驱动测试：普通域名、多级子域名、IP 字面量（v4/v6）、公共后缀本身（`com`）、空串。
- 聚合的位点语义：写失败不推进位点；重复跑一轮不重复累加；停机后补算；access_log 的 id
  被删除/复用（整表清空、部分删除、部分删除后新数据反超旧位点、批次边界落在同一毫秒中间、
  时钟回拨、AutoMigrate 迁移）等场景下不漏数也不重复计数，完整推演与回归测试对应关系见 §4.3。
- 时区对齐：与 `TrafficBucket` 同一套断言。
- 删除入站连带清理 + `PruneOrphans` 兜底（守 SQLite id 复用）。
- 清理必须带 granularity（构造小时桶与日桶，断言清理小时桶后日桶还在）。
- `web/html_test.go` 的两条模板不变量在改完模板后必须跑。

第二期：

- **规则形态的选择**（纯 Go 单测，不需要真实 xray）：`routing.domainStrategy` 分别为
  `IPIfNonMatch` / `IPOnDemand` / `AsIs` / 缺失 / 无法识别的字符串时，生成的计量规则是否带
  `ip` 守卫。无法识别的值必须落到「纯 domain」那一侧。
- **真实 xray 的规则形态 e2e**：把 §6.1 那张交叉实验表固化成回归测试——带 `geoip:private`
  规则时，纯形态的连接**能连通**（复现危险），守卫形态的连接**被 blackhole 掐断**（危险已挡住）；
  无 `geoip:private` 时守卫形态照常计量。这是本期唯一一条「测试的是我们没有做错什么」的用例，
  不能省。核心不提供所需能力时跳过并说明原因，不以「PID 变了」这种和真实缺陷无法区分的形式失败。
- **生成逐字节确定**：同一份池连续生成两次配置，`Config.Equals` 为真；打乱池表的物理行序后
  生成结果仍逐字节相同（守「禁止遍历 map 产生数组顺序」）。
- **计量出站是默认出站的副本**：断言除 `tag` 外逐字节相同，且管理员把模板首个出站改名/换成
  非 freedom 时行为正确（沿用 `tagDefaultOutbound` 的返回值，不硬编码）。
- **深拷贝**：给计量出站写 `domainStrategy` 不会污染默认出站，反之亦然。
- 计量 tag 的反查：域名含 `-`、多级子域、极长域名、id 非数字、缺少分隔符、域名为空。
- `IsReservedTag` 对 `a-ui-meter-*` 的拒绝，覆盖 `allocTag` 与导入路径两个入口。
- **DNS 注入器给计量出站加上了 `domainStrategy`**（扩展既有那条
  `TestDNSInjectorSetsFreedomDomainStrategyThroughGetXrayConfig`）。
- **policy 注入**：只改两个 key，管理员写的 `levels`/`handshake`/`statsUserUplink` 原样保留；
  连续注入两次逐字节相同；模板里 `policy` 缺失时也能正确建出来。
- **池的三道闸门**：最小驻留（进池 1 轮的域名不被挤出）、替换余量（权重差 1.1 倍不换、1.5 倍换）、
  试用退场（连续 3 轮零字节退池并冷却 24 小时，冷却期内不再入选）。
- **排序权重**：`avgBytesPerConn` 为 0 时退化成按 count 排；有字节数据后新域名靠折算权重能挤进池
  （守「池不会锁死在首日那一批」）。
- **候选过滤**：`com`、IP 字面量、空串一律不进池——这条挡的是 `domain:com` 这种会吸走该入站几乎
  全部流量的规则。
- **死计数器冻结**：构造超过上限的 stale 计量条目，断言换池被冻结且已在池内的域名照常计量。
- **采集**：出站条目被正确拆成 `(inboundId, domain)` 并累加进小时桶与日桶；零字节条目不写行；
  非计量前缀的出站条目不影响 `inbounds.up/down` 的累加；`RecordMetered` 失败只告警不阻断
  `AddTraffic`。
- **覆盖度**：分母为 0 时返回 null 而不是 0；比值越界时钳到 `[0,1]`（协议开销可能让它略微超过 1）。
- **热应用 e2e**：换池走热应用且不重启进程（沿用 `web/service/xray_hot_reload_e2e_test.go` 的形状）；
  改 policy 触发整进程重启。
- **回退形态**：`meter_domains` 有数据但生成端不产出计量规则时，`Metered` 为 false、`coverage`
  为 null，历史 `Up`/`Down` 仍可查询。

`make verify`（vet + test + build）是提交前的门禁。

## 12. 风险

| 风险 | 缓解 |
|---|---|
| 计量规则屏蔽 IP 规则的第二遍，让 `geoip:private` 与管理员 CIDR 规则对池内域名静默失效 | §6.1 的 ip 守卫，真实 xray 上 A/B 交叉验证；判据取 `domainStrategy` 最终值，无法识别时落到安全侧 |
| 第二期上线触发一次全员断线重连 | 无法避免（`policy` 是 static 段）。无条件注入让它落在可预期的时刻，写进发版说明，并放在第二期而不是第一期 |
| 计量池收敛期榜单不准 | UI 明示收敛过程并显示覆盖度；折算权重让新域名能在一小时内进池 |
| 池锁死在上线首日抓到的那批域名 | §6.2.2 的折算权重 + §6.2.4 的试用退场闸门 |
| 退池的计量 tag 在核心里留下永不回收的计数器 | 三道闸门压低 churn；`meterStaleCounterLimit` 观测式冻结；xray 任何一次整进程重启自动清零解冻 |
| 一个访问次数极少但流量极大的域名长期进不了池 | 首次进池后就按实测字节排，会立刻升到前列；在此之前覆盖度持续偏低，管理员据此知道榜单不可信 |
| 计量出站副本与默认出站不一致 | 副本除 tag 外逐字节相同；深拷贝；e2e 断言两者内容相等 |
| 入站多时生成配置膨胀，拖慢每一次真实 xray 校验 | `meterOutboundBudget` 硬上限 200（实测每个计量出站 1.03 ms / 235 B），K 自适应；K 为 0 时功能自动停用 |
| 节点网速变慢 | 实测排除：400 条规则相对 0 条只多约 8 µs，在噪音以内；吞吐完全不受影响（§6.0） |
| 榜单聚合拖慢面板 | 聚合在独立库、独立 job、单轮有行数上限；查询走桶不走原始日志 |
| 用量库打不开导致配置生成失败 | 池为空即不生成任何计量出站与规则，fail-open 到「没有这个功能」，不影响 xray 配置生成 |

## 13. 参考

以下 xray-core 位置全部取自锁定版本 v1.260327.1-0.20260728075948-5ca6f4b7d4dc：

- `common/log/access.go` —— 访问日志没有字节字段（§2.1）
- `app/proxyman/inbound/always.go`、`app/dispatcher/default.go` —— 计数器只有三个维度（§2.2）
- `app/proxyman/outbound/handler.go:34-56` —— 出站计数器由 `policy.system.statsOutbound*` 开关
  控制，且在 handler 创建时注册（§6.6）
- `app/proxyman/outbound/outbound.go:131` —— `RemoveHandler` 不注销计数器（§6.2.5）
- `app/stats/command/command.proto:85-92`、`command.go:164` —— `StatsService` 没有注销 RPC，
  `QueryStats` 会把值为 0 的计数器一并返回（§6.2.5）
- `app/router/router.go:245-273` —— 两遍规则匹配，第二遍只在第一遍全不命中时发生（§6.1）
- `features/routing/dns/context.go:21-50` —— 解析是惰性的、按连接缓存的，解析失败返回 nil（§6.1）
- `infra/conf/router.go:175` —— 裸域名串按 `Domain_Substr` 解析（子串匹配），`domain:` 才是
  按域名边界的后缀匹配（§6.4）

本仓库内的相关设计文档：

- `docs/superpowers/specs/2026-09-04-traffic-history-design.md`（分时桶、时区对齐、保留期）
- `docs/superpowers/specs/2026-09-05-routing-ip-and-dns-design.md`（DNS 注入器、`ipRuleResolveDomain`）
- `docs/superpowers/specs/2026-09-02-domain-routing-design.md`（分流注入器的四条不变量）
