# 按来源 IP 的分时段用量 设计文档

日期：2026-09-10
状态：待评审

## 1. 背景与目标

入站列表展开行里每个来源 IP 有一列「本次上线用量↑|↓」。它来自 `onlineTracker`（`web/service/online.go`）的纯内存状态，含义是「自面板首次观测到这个 IP 起累计的字节」——面板一重启归零，IP 一断开重连也归零（`web/service/online_test.go:171` 的 `TestFirstSeenRestartsAfterReconnect` 钉住了后一条）。

于是这个数字**没有稳定的时间基准**：同一行的值在两次刷新之间可能变小，两个用户之间不可比，也回答不了管理员真正要问的那个问题——「这个人今天用了多少」。

目标是把这一列换成可选窗口的用量（今日 / 近 3 日 / 近 7 日 / 近 30 日 / 近 1 年 / 自定义日期区间），分上下行显示，并在同一个展开行里给出同窗口的**入站合计**用量。

### 非目标

- 不改共享检测（`computeCoexist` / `hasActiveBytes` / `suggestRegions`）的任何判定行为。本次只在它的数据表上加列。
- 不改系统状态页的总览图（`TrafficHistoryService.Overview`）。它不在本次需求里，改它是无关变更。
- 不试图让按 IP 的分项之和等于入站总量。§2.1 说明为什么这在物理上做不到。
- 不改 `InboundIPHour` 的 UTC 小时对齐。理由见 §10.1。
- 不新建第二张按 IP 的用量表。理由见 §3.1。

## 2. 现状核实

### 2.1 按来源 IP 的用量永远对不上入站总量——这是物理约束

xray 的 Stats API 只有 `inbound` / `outbound` / `user(email)` 三类计数器（`xray/process.go:276` 的 `parseTraffics`，正则只认 `inbound>>>` 与 `outbound>>>`），**没有来源 IP 维度**。而本项目一个入站恰好一个用户（前端每个协议表单只绑定 `settings.<protocol>es[0]`），user 维度也拆不出 IP。

所以按来源 IP 的用量只有一个可能的来源：Linux 内核连接表（`util/netdiag`）。两者口径不同：

| | 数据源 | 统计的是什么 |
|---|---|---|
| 入站累计 / `TrafficBucket` | xray Stats API | 代理转发的**应用层**字节 |
| 本次上线用量 / `InboundIPHour` | 内核连接表 | socket 的**链路层**字节，含 TLS 记录层与 WS 帧开销、TCP 重传 |

两者天生差一个量级不小的比例（TLS + WebSocket 封装开销，经验值 5~15%），**任何改法都消不掉**。

这个差异现在被「本次上线」这个模糊窗口掩盖着——没有人会拿一个随时归零的数去对账。一旦改成「今日」，管理员会立刻把各 IP 的分项相加去和「今日入站用量」比，差异当场暴露。因此本设计在界面上**主动标注口径**，而不是假装两者可比。

### 2.2 `InboundIPHour` 已经有按 IP 按小时的字节量，但只有合计

`database/model/sharing.go` 的 `InboundIPHour` 是 `(入站, IP, UTC小时)` 三元组唯一索引，已有 `ActiveBytes`（上下行之和），保留 30 天（`sharingRetentionDays`，硬编码常量）。

它的采集侧有三条为**并存判定**服务的门槛，对用量统计来说都是系统性低估：

- **每小时漏掉开头一个采样间隔**：`sharingCell` 在每个整点被 `rolloverLocked` 清空，新建那一轮只设 `lastUp`/`lastDown` 基线、不计字节（`web/service/sharing_accumulator.go:41-43` 的注释说明了原因）。采样间隔 30 秒，一小时 3600 秒，约 0.83%。
- **60 秒落库门槛，且不跨小时结转**：单个 `(入站, IP, 小时)` 累计活跃不满 `sharingFlushThreshold = 60` 秒就整段丢弃。一个每小时只活跃 50 秒的来源，一整天一个字节都不会被记录。
- **单入站单小时 50 个 IP 上限**（`sharingMaxRowsPerHour`）：超出的**新**来源整条忽略。正常场景一小时 2~3 行，不触发。

本设计**不修这三条**（§9），它们相对 §2.1 的口径差是次要项，而修改它们要动共享检测的判定数据。

### 2.3 展开行的在线明细每 2 秒轮询一次

`web/html/xui/inbounds.html:736-738`：

```js
this.onlineTimer = setInterval(() => {
    this.expandedKeys.forEach(id => this.loadOnlines(id));
}, 2000);
```

频率是 2 秒，且**按展开的入站数逐个请求**。用量数据因此绝不能塞进 `/aui/inbound/onlines/:id` 的返回体：那等于每 2 秒对一张 30 天的表做一次聚合查询、再乘以展开的入站数。

### 2.4 `TrafficBucket` 一张表两级粒度，聚合查询必须带 `granularity`

小时桶与日桶**各自独立累加**，日桶不由小时桶汇总而来。任何跨桶聚合不带 `granularity` 条件，同一段时间的流量会被算两遍。现有 `History` 的查询是带的（`web/service/traffic_history.go:314`），新增的聚合必须照做。

## 3. 数据层

### 3.1 `InboundIPHour` 加两列，不新建表

新增两列：

```go
// ActiveUp/ActiveDown 是本小时该来源 IP 的上行、下行字节。
//
// 与 ActiveBytes 并列而不是取代它：ActiveBytes 是并存判定的门槛判据
// （coexistMinActiveBytes），而升级前写入的行这两个新列恒为 0、ActiveBytes
// 有值。若把 ActiveBytes 改成由两列相加得出，那批历史行的判据会当场失效，
// 共享检测的结论会在升级瞬间整体改变，而界面上没有任何东西说明发生了什么。
ActiveUp   int64 `json:"activeUp"`
ActiveDown int64 `json:"activeDown"`
```

**不新建第二张「入站×IP×小时用量表」**。新建表能摆脱 §2.2 那三条门槛，但只能消除约 1% 的采集偏差，消除不了 §2.1 那个 5~15% 的口径差——而磁盘占用、清理任务、`DeleteByInbound` 连带删除、`PruneOrphans` 兜底、AutoMigrate 全都要再写一遍。收益与代价严重不成比例。

代价是 `InboundIPHour` 从「只有一个消费者」变成两个。`database/model/sharing.go` 顶部那段注释必须同步更新，否则下一个改这张表的人会按旧前提行事。

### 3.2 累加器并行维护三个计数器

`sharingCell`（`web/service/sharing_accumulator.go`）加 `upBytes` / `downBytes`，与现有 `bytes` 并行累加：

```go
du := int64(deltaBytes(uint64(o.Up), uint64(cell.lastUp), true))
dd := int64(deltaBytes(uint64(o.Down), uint64(cell.lastDown), true))
cell.upBytes += du
cell.downBytes += dd
cell.bytes += du + dd
```

`bytes` 仍然独立累加而不是改成 `upBytes + downBytes`——两者数值恒等，但保留独立赋值让「共享检测的输入逐字节不变」这件事在 diff 上可见，也避免将来某次重构顺手把 `bytes` 删掉。

`sharingFlush` 与 `upsertIPHour` 的 `DoUpdates` 列表同步加这两列。

### 3.3 回退兼容

回退到旧二进制：新列被 GORM 忽略，共享检测只读 `ActiveBytes`，行为完全不变。AutoMigrate 只加列不删列，再次升级时数据仍在。**回退安全**。

## 4. 时间区间的表达与钳制

### 4.1 档位翻译在服务端

前端传的是档位名（`today` / `3d` / `7d` / `30d` / `1y`）或自定义的**起止日期字符串**（`YYYY-MM-DD`），不是时间戳。服务端按面板时区（`SettingService.GetTimeLocation()`）把它解释成本地 0 点。

理由与「x 轴标签在服务端格式化」（`docs/superpowers/specs/2026-09-04-traffic-history-design.md`）同源：**时区的权威在服务端**。让浏览器算时间戳的话，访问者换个时区，同一个「今日」就指向不同的绝对时间，而面板设置的时区没变。

窗口统一为左闭右开的 `[start, end)` Unix 秒：

| 档位 | start | end |
|---|---|---|
| `today` | 本地今天 0 点 | 现在 |
| `3d` / `7d` / `30d` | 本地 (今天-N+1) 日 0 点 | 现在 |
| `1y` | 本地 (今天-364) 日 0 点 | 现在 |
| 自定义 | 起始日本地 0 点 | 结束日本地 24 点（不超过现在） |

「近 N 日」按**日历**计算而非「往回数 N×24 小时」：用户说的「近 7 日」是「包含今天在内的 7 个自然日」。这与现有图表那排 `24小时/7天/30天/1年` 的滑动语义不同，是刻意的替换（§8.1）。

### 4.2 controller 钳制

区间来自请求体，是不可信输入。钳制放在 controller（与 `getTrafficOverview` 对 `top` 的钳制同源，`web/controller/inbound.go:365-371`）：

- 认不出的档位 → 回落 `today`
- 自定义日期不可解析 → 回落 `today`
- `start > end` → 交换
- 跨度 > 366 天 → 把 start 抬到 `end - 366 天`
- `end` 超过今天 → 截到现在

四条都不报错、只钳制并照常返回——与 `rangeSpec` 那句「前端传错时给一张能看的图，比报错或空图有用」同一个取向。

### 4.3 粒度按跨度选

跨度 ≤ 30 天用小时桶，> 30 天用日桶。与现有 `rangeSpec` 的行为一致（`30d` → 小时 × 720，`1y` → 日 × 365）。

小时桶的保留期是设置项 `trafficHourRetentionDays`（默认 30，管理员可改小）。改小之后「近 30 日」会出现前半段空白——这是**既有行为**（现有 `Range30d` 同样如此），不在本次范围。

这一节**只针对 `TrafficBucket`**（入站合计与图表）。`InboundIPHour` 只有 UTC 小时一种粒度，不存在粒度选择；窗口超过 30 天时它整块降级（§7.a），不会去读不存在的日桶。

## 5. 接口

### 5.1 按 IP 的分项：`POST /aui/inbound/ipUsage/:id`

入参与 `getTrafficHistory` 的绑定方式保持一致（`web/controller/inbound.go:330-340` 的注释说明了为什么用 `form` tag 而不是 `json` tag）：

```go
form := struct {
    Range string `form:"range"`  // today/3d/7d/30d/1y/custom
    Start string `form:"start"`  // YYYY-MM-DD，range=custom 时必填
    End   string `form:"end"`
}{}
```

返回：

```go
type IPUsageEntry struct {
    IP   string `json:"ip"`
    Up   int64  `json:"up"`
    Down int64  `json:"down"`
    // 归属地五件套与 OnlineIP 同名同义，由同一个 locateWithIPDB 产出，
    // 离线行因此能和在线行一样显示「存疑」标记。
    Location    string             `json:"location"`
    LocationAlt string             `json:"locationAlt"`
    ISP         string             `json:"isp"`
    ISPAlt      string             `json:"ispAlt"`
    Sources     []ipSourceLocation `json:"sources"`
    LastSeen    int64              `json:"lastSeen"` // 窗口内最后一个有记录的小时（毫秒）
}

type IPUsageResult struct {
    Entries         []IPUsageEntry `json:"entries"`
    Split           bool           `json:"split"`           // 见 §7 降级 b
    BeyondRetention bool           `json:"beyondRetention"` // 见 §7 降级 a
    // OtherCount/OtherUp/OtherDown 是被长尾截断合并掉的那部分，见下。
    OtherCount int   `json:"otherCount"`
    OtherUp    int64 `json:"otherUp"`
    OtherDown  int64 `json:"otherDown"`
    Reason     string `json:"reason"`
}
```

查询：`WHERE inbound_id = ? AND hour_start >= ? AND hour_start < ?`，按 IP 分组求和。排序：用量降序，同量时按 IP 字节序（与 `online.go:325` 的排序同源，保证确定性）。

归属地**直接复用 `locateWithIPDB`**（`web/service/online.go:419`，包级函数）——离线 IP 不在连接表快照里，必须重新查，而那个函数的注释原话是「在线明细与访问日志的来源列表共用同一套判定，避免三处结论对不上」，这里是第四处。复用它而不是自己写一次单值 lookup，离线行就能和在线行一样显示「存疑」标记，也不会出现同一个 IP 在表格里换一行就换个归属地的情形。

**返回条数上限 200，长尾合并成一行。** `sharingMaxRowsPerHour` 限的是「单入站单小时 50 个 IP」，30 天窗口下去重后的 IP 总数没有上界——一次持续的端口扫描能攒出几千个。表格渲染几千行会把页面卡死，而管理员真正要看的永远是用量最大的那几个。超出的部分不丢弃，合计进 `OtherCount`/`OtherUp`/`OtherDown`，界面渲染成末尾一行「其余 N 个来源共 X GB」——**总量不能静默缩水**，这与 §6 拒绝「只列在线 IP」是同一条理由。上限钳在 controller，与 `getTrafficOverview` 对 `top` 的钳制同源。

### 5.2 入站合计：扩展 `History`

`TrafficHistoryResult` 加：

```go
Total TrafficPoint `json:"total"`
```

**`Total` 是 `Points` 各点之和，不是一次独立的 SQL `SUM`。**

这一条是刻意的。`History` 用 `bucket_start` 精确相等去 join `buildSlots` 按当前时区重算的刻度，而独立的 `SUM` 聚合不受对齐约束。管理员改过时区之后（`docs/superpowers/specs/2026-09-04-traffic-history-design.md` §3.3 记录了实测：整小时时区切换会让旧桶与新刻度完全不相交），独立 `SUM` 会让**图是一条平的 0 线、数字却有值**——这是最难解释的一种不一致。让 `Total` 等于图上那些点的和，两者永远自洽，且数据在保留期内随新数据自愈。

`History` 的签名扩展为同时接受旧档位与新区间：旧档位入参走原路径（保持 `Overview` 与任何未改的调用方零变化），新区间入参走新路径。

### 5.3 刷新频率

两个接口都**不进 2 秒轮询**（§2.3）。切换档位时各拉一次，之后不自动刷新——`InboundIPHour` 每 60 秒才可能写一次、`TrafficBucket` 一小时才变一次，而系统状态页那张图不挂进 2 秒轮询也是同一个理由。

## 6. 表格语义扩大：窗口内活跃过的 IP

展开行那张表现在只列**当前在线**的 IP。窗口一旦拉长，这个语义与数据脱节：看「近 30 日」时表里可能只有 1 个在线 IP，而这 30 天有 5 个 IP 用过，另外 4 个一行都不出现。管理员看到的是「这 30 天只有一个来源」，而这恰恰对「是不是被共享了」给出了反向误导的答案，且界面渲染得完全合理、没有任何一层会提示。

因此表格语义扩大为「**窗口内活跃过的来源 IP**」：

- 数据来源是两个：`/onlines/:id`（当前在线，2 秒轮询）与 `/ipUsage/:id`（窗口内用量，低频），前端按 IP 字符串做 outer merge。
- **归属地不存在 merge 冲突**：两个接口都走 `locateWithIPDB`，对同一个 IP、同一份库必然给出同一个结果（§5.1）。前端取任意一侧即可，不必分情况——这正是复用那个函数而不是各写一次 lookup 换来的好处。
- 排序：**在线的在前**（保持现有 IP 字节序），离线的在后（按用量降序）。在线与离线之间不混排——管理员一眼要能分出「现在有几个人在连」。
- 离线行的列降级：`连接数` 与 `实时↑|↓` 显示 `—`（不是 0）；`上线时间` 改显示窗口内最后活跃的小时；「踢下线」按钮隐藏（对已断开的连接无意义），「访问日志」保留。
- 离线行仍然显示「已封禁」标记——`IPBan` 是管理员显式设的状态，与在不在线无关。

## 7. 降级：五处，每一处都必须显示原因而不是 0

「看不到」与「没有」必须能被区分开，这是本项目一以贯之的原则（`trafficDBUnavailable`、`emptyOnlineCount`、`onlineUnsupportedReason` 都是同一条）。

| 情形 | 显示 | 说明 |
|---|---|---|
| **a. 窗口超出 30 天**（选「近 1 年」或跨度 > 30 天的自定义区间） | IP 分项整块显示 `—` + 「按来源 IP 的明细只保留 30 天，更长的区间只有入站合计」 | `sharingRetentionDays` 是硬编码的 30，而入站日桶默认保留 365。两个主体的可查范围不对称，必须说出来 |
| **b. 升级前的老行**（`ActiveBytes > 0` 但两个新列均为 0） | 显示合计数 + 「该时段为升级前数据，无上下行拆分」，**不显示 `↑0 ↓0`** | 判据**按整批而非逐行**，与 `web/service/sharing_stat.go:91` 那条注释完全同构：逐行判会让同一张表里两种口径混排。`IPUsageResult.Split` 承载这个判定 |
| **c. 在线但尚未落库**（新上线不满 60 秒的 IP） | 用量列显示「统计中」+ tooltip「活跃满 1 分钟后开始计入」 | 显示 0 会让管理员以为这个正在跑满带宽的连接没有流量 |
| **d. 非 Linux / 连接表读不到** | 沿用现有 `onlineUnsupportedReason` | 在线列表本来就走这条 |
| **e. 用量历史库不可用** | 沿用现有 `trafficDBUnavailable` | 两个接口共用一个库 |

## 8. 前端

### 8.1 一个时间控件驱动三个数据区

展开行顶部一排单选按钮 + 一个 `a-range-picker`（ant-design-vue 1.7.2 自带，`moment` 已在 `common/js.html` 引入，`renew_modal.html` 与 `form/inbound.html` 已有 `a-date-picker` 的先例）：

```
[今日][近3日][近7日][近30日][近1年][📅 自定义]

本段入站用量  ↑ 2.31 GB   ↓ 18.4 GB   合 20.7 GB    （xray 口径）
────────────────────────────────────────────────────
上线时间 │ 来源IP │ 归属地 │ 运营商 │ 连接 │ 实时↑↓ │ 本段用量 ⓘ │ 操作
────────────────────────────────────────────────────
[图表：同一段时间的曲线]
```

**现有那排 `24小时/7天/30天/1年` 被替换掉**，图表随之从滑动窗口变成日历窗口。这是接受的行为变化：一个折叠区里放两个时间控件，「调了上面以为下面也变了」是必然会发生的误解，而那正是本项目一贯要防的静默不一致。

代价是「今日」在凌晨看只有一两个小时的曲线。这是「今日」的正确含义，要连续曲线选「近 3 日」。

### 8.2 列头 tooltip

「本段用量」列头带 ⓘ，内容固定为：

> 来自内核连接表，含 TLS 与 WebSocket 协议开销，**与上方入站合计用量不相等**（后者是 xray 统计的应用层字节）。各来源 IP 分项之和会略大于或小于入站合计。

这是 §2.1 那个物理约束在界面上的唯一出口。**不要因为觉得啰嗦而删掉它**——删掉之后管理员对不上账时唯一的解释来源就没有了。

### 8.3 图表实例的生命周期

沿用现有约定：canvas 在 `$nextTick` 里取（`expandedRowRender` 动态渲染），折叠时 `chart.destroy()`。切换档位时复用同一个实例更新 data，不重建。

## 9. 明确不做

- **不修 §2.2 那三条采集门槛**。修「每小时漏首轮」要让 `rolloverLocked` 跨小时保留 `lastUp`/`lastDown` 基线（对持续连接是对的，对首次观测是错的，两种情形要分开处理）；修「60 秒门槛不结转」会打开一条让扫描器攒够门槛落库的路（`sharing_accumulator.go:156-157` 明确写了为什么不结转）。收益是约 1% 的偏差，风险是共享检测的判定数据，不划算。
- **不改 `Overview`**（系统状态页总览图）。它仍用旧的 `TrafficRange` 四档滑动窗口。后果是面板里同时存在两套时间语义——已知且接受，见 §10.3。
- **不做跨入站的 IP 用量排行**。本次只在单个入站的展开行里做。

## 10. 已知偏差

### 10.1 半小时偏移时区下「今日」切不准

`InboundIPHour` 按 `AlignHourUTC` 对齐到 UTC 整点（`database/model/sharing.go:48-54`），而窗口边界按面板时区算。整小时偏移的时区（含 UTC+8）下本地 0 点必然落在 UTC 整点上，能精确切；半小时或一刻钟偏移的时区（Asia/Kolkata UTC+5:30、Asia/Tehran 等）下本地 0 点是 UTC 的半点，**边界那一个小时的字节会整块算进或算出**。

**不改对齐方式**。`sharing.go:48-54` 明确写了当初选 UTC 的理由：按本地时区对齐会重蹈 `TrafficBucket` 那个坑——管理员改一次时区，旧桶与重算出的新刻度不相交，历史整段消失。为了半小时时区下一个小时的边界精度，去换「改时区后共享检测历史全灭」，方向反了。

误差上界是窗口边界上的一个小时。对「今日」是 1/24，对「近 7 日」是 1/168。

### 10.2 按 IP 的分项之和不等于入站合计

§2.1 的物理约束叠加 §2.2 的三条采集门槛。前者无法消除，后者刻意不修。界面上靠 §8.2 的 tooltip 说明。

### 10.3 面板里同时存在两套时间语义

入站展开行是日历语义（今日 / 近 N 日），系统状态页总览图是滑动语义（24 小时 / 7 天 / 30 天 / 1 年）。这是 §9「不改 `Overview`」的直接后果。两个页面之间不会互相引用同一个数字，所以不会产生对不上账的观感，但确实是不一致。将来若把系统状态页也改成日历语义，`rangeSpec` 与 `Range24h` 等常量可以一并退役。

### 10.4 「近 1 年」档下 IP 分项整块不可用

`sharingRetentionDays` 是硬编码的 30 天，而入站日桶默认保留 365 天。选「近 1 年」时上方的入站合计有值、下方的 IP 分项全是 `—`。这是 §7.a 那条降级，不是缺陷，但它会让「近 1 年」这个档位看起来只做了一半。

## 11. 测试

- **数据层**：`InboundIPHour` 加列后的 upsert 往返（含覆盖式更新只改本小时的值）；`sharingCell` 的 `upBytes + downBytes == bytes` 恒等；`deltaBytes` 回退（客户端重连、计数器归零）时两个新列都不产生负增量。
- **共享检测不变**：一条回归测试，对同一份输入，`computeCoexist` / `hasActiveBytes` / `suggestRegions` 在加列前后结果逐字段一致。
- **区间钳制**：认不出的档位、不可解析的日期、`start > end`、跨度 > 366 天、`end` 超过今天，五种越界入参各一条，断言钳制结果而非报错。
- **窗口计算**：`today` / `3d` / `7d` 在 UTC+8 下的 `[start, end)`；跨月与跨年边界；`1y` 选日粒度而 `30d` 选小时粒度。
- **降级**：窗口超 30 天时 `BeyondRetention` 为 true 且 `Entries` 为空；整批老行时 `Split` 为 false；库不可用时 `Reason` 非空而不是返回空列表。
- **合计自洽**：`Total` 严格等于 `Points` 各点之和（而不是一次独立 `SUM`）。
- **表格合并**：在线 IP 与离线 IP 的 outer merge，在线在前、离线按用量降序在后；只在线不在库（降级 c）与只在库不在线（离线行）两种单边情形。
- **长尾截断**：超过 200 个 IP 时 `Entries` 恰好 200 条，且 `OtherUp + 各条 Up` 等于截断前的总和——**截断不能让总量缩水**。
