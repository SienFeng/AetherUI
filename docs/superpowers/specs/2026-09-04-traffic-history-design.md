# 用量历史记录与图表 设计文档

日期：2026-09-04
状态：待评审

## 1. 背景与目标

面板当前只保存**累计**流量：`inbounds.up` / `inbounds.down` 两个计数器，由 `XrayTrafficJob` 每 10 秒累加一次。这份数据只能回答「这个用户一共用了多少」，回答不了「他什么时候在用」。

管理员实际需要的是后者：

> 某个用户是不是在半夜跑批量下载？这台机器的流量高峰在几点？
> 这个月哪几个用户占掉了大部分带宽？

目标是新增两处图表：

1. **单入站图** —— 入站列表展开行内，在线连接表下方，画该用户的分时上传 / 下载曲线
2. **全局图** —— 系统状态页底部，画所有用户的分时用量，Top N 多线叠加

以及支撑它们的时序数据采集、持久化与自动清理。

### 非目标

- 不做出站维度的流量统计（xray 侧的出站 stats 与入站是两套 tag，与「一个入站一个用户」的模型对不上）
- 不做单个来源 IP 的分时统计（在线明细已经给了实时速率；按 IP 存历史会让行数乘上一个不可控的系数）
- 不做流量告警 / 阈值通知
- 不做数据导出（CSV / API 对外开放）
- 不改动现有的累计流量、限额、到期逻辑

## 2. 现状核实

以下均为读代码确认的事实，非推断。设计的每一条都建立在它们之上。

| 事实 | 位置 |
|---|---|
| `XrayTrafficJob` 每 10 秒调一次 xray gRPC Stats，`reset=true` 取回的是**增量**，取完 xray 侧清零 | `web/job/xray_traffic_job.go`、`web/service/xray.go` |
| `InboundService.AddTraffic` 把这份增量累加进 `inbounds.up/down`，累加后增量即被丢弃 | `web/service/inbound.go:216` |
| 访问日志已经**独立分库**，理由是 SQLite 一个库一把写锁 | `database.InitAccessLogDB`、`model.AccessLog` 的注释 |
| `GetAccessLogDB()` 未初始化成功时返回 nil，调用方必须判空 | `database/db.go` |
| 保留期清理已有完整先例：设置项 + `@every 1h` 的清理任务 + 孤儿清理 | `accessLogRetentionDays`、`AccessLogCleanupJob` |
| 删除入站时连带删除其访问日志，防 SQLite 复用自增 id | `AccessLogService.DeleteByInbound` |
| tag → inbound_id 的映射已有现成实现，`service` 包内私有函数 | `web/service/accesslog.go:150` `inboundTagToId()` |
| 面板时区设置可读 | `SettingService.GetTimeLocation()`，`web/service/setting.go:411` |
| cron 已配 `cron.WithChain(cron.Recover(cronLogger{}))` | `web/web.go:347` |
| `entity.AllSetting` 与前端 `models.js` 的字段同步**有测试守着** | `web/setting_model_test.go` |
| 模板解析错误被 `getHtmlTemplate` 吞掉，但有测试兜底 | `web/html_test.go` `TestAllTemplatesParse` |
| Vue 指令写在根元素外是静默死代码，有测试守着 | `web/html_test.go` `TestVueDirectivesLiveInsideAVueRoot` |
| `web/assets/` 下**没有任何图表库** | 全目录搜索无 chart / echart / plot |
| `common/js.html` 是**所有页面**共用的脚本引入点，含登录页 | `web/html/common/js.html` |
| 展开行由 `expandedRowRender` 动态渲染，当前放的是在线连接表 | `web/html/xui/inbounds.html:75` |

第三方库核实（`curl` 实测，非查记忆）：

| 库 | 版本 | UMD 压缩体积 | 许可 | 依赖 |
|---|---|---|---|---|
| Chart.js | v4.5.0 | 208,341 字节（203 KB） | MIT | 无，UMD 自包含，暴露全局 `Chart` |

MIT 与本项目的 GPL-3.0 兼容。

## 3. 采集

### 3.1 数据源的选择

考虑过两条路：

**A. 复用 `XrayTrafficJob` 已经拿到的增量**（采纳）

`AddTraffic` 拿到的 `[]*xray.Traffic` 本身就是过去约 10 秒的增量，写进时序表之后再走原有的累加逻辑。

- 零额外采集开销，不多一次 gRPC 调用
- 增量恒为非负。xray 重启后计数从 0 重新开始，不存在负值
- **不受「重置流量」影响**。管理员点重置流量清的是 `inbounds.up/down` 这个累计计数器；历史桶记的是「某小时用了多少」，两者语义无关

**B. 独立 job 定期读 `inbounds.up/down` 做差分**（否决）

改动面更小，但重置流量会产生一个巨大的负差，删改入站同样。要正确处理就得记录上次快照、判负、丢弃异常值——比 A 复杂，且判负逻辑本身会静默吃掉真实数据：一次正常的重置和一次数据损坏在差分层面长得一模一样。

### 3.2 写入点

在 `AddTraffic` 的现有事务**之外**、进入事务之前调用 `TrafficHistoryService.Record(traffics)`。

不放进同一个事务，是因为两者写的是不同的库——主库的事务包不住时序库的写入，硬凑只会得到一个看起来是原子的假象。

**时序写入失败只记 `logger.Warning`，不影响 `AddTraffic` 的返回值。** 图挂了不能让计费流量跟着挂：`inbounds.up/down` 是限额与到期判定的输入，它停止累加的后果（用户超额不被停用）比图上少一段曲线严重得多。

### 3.3 桶的对齐

`bucket_start` 是桶起始时刻的 Unix 秒。对齐用**面板设置里的时区**（`GetTimeLocation()`，cron 用的同一个），不是 UTC：

- 小时桶：该时区本地时间的整点
- 日桶：该时区本地时间的 00:00:00

按 UTC 切日会让中国管理员看到的「某天用量」整体错位 8 小时——图上 9 月 4 日那根柱子里，装的是 9 月 3 日 08:00 到 9 月 4 日 08:00 的流量。这类错误不会报错，只会让人根据错的数据做判断。

**时区改变后的行为**：旧桶的 `bucket_start` 不变（历史按当时的时区切），新桶按新时区切，交界处会有一天的错位。刻意不做补偿——重切历史桶需要原始的秒级数据，而那份数据早已聚合掉了；任何「估算着搬」的补偿逻辑都是在用假数据覆盖真数据。

### 3.4 零流量不写行

增量为 0 的入站直接跳过，不写行。

参考图上大片的 0 是前端补零画出来的，不是存出来的。挂机用户大部分小时没有任何流量，这一条能砍掉一多半行数，而代价只是前端多一步补零——而补零逻辑无论如何都要有（新建的入站在它存在之前的时间段同样没有行）。

## 4. 数据模型

### 4.1 独立分库

新增 `/etc/<name>/<name>-traffic.db`，与访问日志同样的分库理由（见 `model.AccessLog` 注释）：这张表每 10 秒写一次，清理时又是大批量 DELETE，混进主库会去和面板的每一次普通操作抢同一把写锁。

初始化照 `InitAccessLogDB` 的形状：独立于 `InitDB`，失败只记日志、面板其余功能照常可用；`GetTrafficDB()` 未初始化成功时返回 nil，**所有调用方判空**。

### 4.2 表结构

```go
// database/model/traffic.go

type TrafficGranularity int8

const (
    GranularityHour TrafficGranularity = 1
    GranularityDay  TrafficGranularity = 2
)

// TrafficBucket 是某个入站在某个时间桶内的用量，存在独立的 SQLite 库里
// （见 database.InitTrafficDB）。
type TrafficBucket struct {
    Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

    // Granularity 决定这一行属于哪一级：小时桶供近期看细节，日桶供长期看趋势。
    // 两级共用一张表，清理时按它套不同的保留期。
    Granularity TrafficGranularity `json:"-" gorm:"uniqueIndex:idx_bucket,priority:1"`

    // InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，改端口就变，
    // 存 tag 会让历史在改端口那一刻断掉。相应地，删除入站必须连带删掉它的桶
    // ——SQLite 会复用自增 id，不删的话新建的入站会看到上一个用户的曲线。
    InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_bucket,priority:2"`

    // BucketStart 是桶起始时刻的 Unix 秒，按面板设置的时区对齐（见 §3.3）。
    BucketStart int64 `json:"t" gorm:"uniqueIndex:idx_bucket,priority:3"`

    Up   int64 `json:"up"`
    Down int64 `json:"down"`
}
```

四个刻意的决定：

1. **一张表 + `granularity` 字段**，不是两张表。清理时按 granularity 套不同保留期，查询时 `WHERE granularity = ?`，省掉一整套重复的建表、清理、查询代码。
2. **唯一索引 `(granularity, inbound_id, bucket_start)`** 既是 UPSERT 的冲突目标，也是两个查询走的索引。列序按选择性排：全局图按时间范围扫某一级的全部入站，前缀 `(granularity, inbound_id)` 之后仍能用上第三列做范围。
3. **日桶独立累加，不由小时桶汇总而来。** 汇总方案要处理「小时桶已被清理但日桶还没算」的补算逻辑，两者独立累加则天生免疫。代价是每次采样多一次 UPSERT——日桶一年才 365 行，可以忽略。
4. **不存 `total` 字段。** `up + down` 在查询时算，存下来只是给自己制造一个可能与两个分量不一致的第三份真相。

### 4.3 写入方式

GORM 的 `clause.OnConflict` UPSERT：

```go
clause.OnConflict{
    Columns:   []clause.Column{{Name: "granularity"}, {Name: "inbound_id"}, {Name: "bucket_start"}},
    DoUpdates: clause.Assignments(map[string]any{
        "up":   gorm.Expr("traffic_buckets.up + ?", up),
        "down": gorm.Expr("traffic_buckets.down + ?", down),
    }),
}
```

写入频率：10 秒一次 × 有流量的入站数 × 2（小时桶 + 日桶）。10 个活跃入站也就 720 次 UPSERT/小时，对一个独立的、没有读竞争的 SQLite 库微不足道。

考虑过在内存里累加、每分钟 flush 一次以减少写入，**否决**：省下的是一个本来就不构成瓶颈的开销，换来的是面板重启丢最多一分钟数据，以及一份需要考虑并发的内存状态。

### 4.4 磁盘占用

一行含索引约 55~60 字节。10 个入站跑满保留期上限：

| | 行数 | |
|---|---|---|
| 小时桶 30 天 | 10 × 24 × 30 = 7,200 | 零流量不写行，实际更少 |
| 日桶 365 天 | 10 × 365 = 3,650 | |
| 合计 | ≈ 10,850 行 | **库文件 1~2 MB 量级** |

50 个入站也就 5~10 MB。这个量级不需要为它做任何优化，也不需要 VACUUM。

## 5. 保留与自动清理

### 5.1 两个设置项

| key | 默认 | 校验范围 | 含义 |
|---|---|---|---|
| `trafficHourRetentionDays` | 30 | 1 ~ 365 | 小时桶保留天数 |
| `trafficDayRetentionDays` | 365 | 1 ~ 3650 | 日桶保留天数 |

按 `CLAUDE.md` 记的五步流程走全套：`defaultValueMap` 加默认值 → `entity.AllSetting` 加字段 → `entity.CheckValid` 加校验 → 加 getter → `web/assets/js/model/models.js` 的 `AllSetting` 构造函数加同名字段。

最后一步有 `web/setting_model_test.go` 自动守着，漏掉会直接测试失败，不会退化成「保存配置接口对所有字段一起失败」那种误导性极强的线上故障。另需在 `web/service/setting_baseline_test.go` 的 `validBaseSetting()` 里补两个合法值，否则所有针对单字段的校验测试会一起失效。

### 5.2 清理任务

清理逻辑**不塞进**已存在的 `AccessLogCleanupJob`——那会让它名不副实，也会让两种数据的清理失败互相牵连。新增一个同频率的 `TrafficCleanupJob`，与它并列注册：

```go
s.cron.AddJob("@every 1h", job.NewTrafficCleanupJob())
```

任务体照 `AccessLogCleanupJob` 的形状，三件事：

1. `Cleanup(GranularityHour, trafficHourRetentionDays)` —— 删除超期小时桶
2. `Cleanup(GranularityDay, trafficDayRetentionDays)` —— 删除超期日桶
3. `PruneOrphans()` —— 删除库里已不存在的 inbound_id 对应的行

首行 `defer common.Recover("用量历史清理任务")`。cron 虽已配 `cron.Recover`，但现有 job 一律自带，保持一致。

### 5.3 孤儿清理不是可选项

`PruneOrphans` 与「删除入站时同步删除」两条都要有，理由是 `CLAUDE.md` 里那条 SQLite 复用自增主键 id 的坑：

删掉入站不清历史，下一个建出来的入站会拿到同一个 id，于是看到上一个用户的曲线。而且因为此时引用**不再悬空**，任何「跳过悬空引用」式的防线都拦不住它——图会渲染得非常合理，只是画的是别人的数据。

因此 `InboundService.DelInbound` 里要加一行 `trafficHistoryService.DeleteByInbound(id)`，照 `AccessLogService.DeleteByInbound` 的先例。`PruneOrphans` 是第二道防线，兜住删除路径漏调或中途失败的情况。

## 6. 后端接口

新增 `web/service/traffic_history.go` 承载业务逻辑，`InboundController` 只加两个薄方法。不新建顶层 controller：数据源就是入站流量，且 service 层要用同包的 `inboundTagToId()` 与入站 remark。

```
POST /aui/inbound/traffic/history/:id
     { range: "24h" | "7d" | "30d" | "1y" }
  → { granularity, points: [{ t, up, down }, ...] }

POST /aui/inbound/traffic/overview
     { range: "24h" | "7d" | "30d" | "1y" }
  → { granularity, labels: [...], series: [{ inboundId, remark, points: [total, ...] }, ...] }
```

`24h` / `7d` / `30d` 走小时桶，`1y` 走日桶。

两个接口的响应形状刻意不同：单入站只有两条线，点带上自己的时间戳最直白；全局图有最多 12 个系列、共享同一套刻度，把 `labels` 提出来单独放、系列只给纯数值数组，省掉 12 份重复的时间戳，也正好是 Chart.js category 轴要的形状。

三条服务端职责，都是为了让前端只管画：

1. **补零与刻度对齐**。返回的点数组在所选范围内刻度完全稠密，缺失的桶补 0。前端不做任何时间计算——时区在服务端，前端算会算错。
2. **Top N 排序与截断**。全局图按所选时间段内的 `up + down` 总量排序取前 12。入站不足 12 个时全部返回。
3. **x 轴标签由服务端格式化**（`2026-09-04 17:00` / `2026-09-04`）。这样前端用 Chart.js 的 category 轴即可，**完全不需要 date adapter**，少一个第三方依赖。

`GetTrafficDB()` 为 nil 时返回空点集与一条说明，不返回错误：图空着并附上原因，比整个页面报错好。这与在线明细 `OnlineResult.Supported / Reason` 的处理是同一个思路——「看不到」和「没有」必须能被区分开。

## 7. 前端

### 7.1 图表库引入

Chart.js v4.5.0 UMD 落到 `web/assets/chart.js/chart.umd.min.js`，与 `web/assets/` 下其余第三方库一样本地化，不走 CDN。

**只在 `index.html` 和 `inbounds.html` 各自单独引入，不加进 `common/js.html`。** 后者是所有页面共用的，加进去会让登录页也白下载 203 KB——登录页是唯一一个未认证用户能打到的页面，它的体积应该保持最小。

引用带 `?{{ .cur_ver }}`（`max-age=31536000` 的强缓存靠它失效）。版本号由 CI 从 tag 写入，发版时自然变；本地调试用 `XUI_DEBUG=true` 走磁盘读取。

### 7.2 单入站图

位置：`expandedRowRender` 内，在线连接表下方。

- 上传 / 下载两条折线，`24小时 / 7天 / 30天 / 1年` 的 `a-radio-group` 切换
- 展开时才发请求，折叠即停

**两个必须处理的坑**：

- **canvas 要在 `$nextTick` 里挂载**。`expandedRowRender` 是动态渲染的，指令执行时 DOM 还没有。
- **折叠时必须 `chart.destroy()`**。Chart.js 实例持有 canvas 引用与 resize 监听，不销毁会随着反复展开折叠累积泄漏，页面开几小时就会明显吃内存。展开行的 chart 实例按 inboundId 存进一个 map，`onExpand` 的折叠分支里逐个销毁。

不用 `a-tabs` 做范围切换：`a-tabs` 的非活动面板仍在 DOM 里，会给页面留下一堆隐藏的 canvas，也会让选择器命中隐藏面板里的同名元素（`CLAUDE.md` 记过这个坑）。

### 7.3 全局图

位置：系统状态页 `index.html` 底部，一张 `a-card`。

- Top 12 多线叠加，图例可点击隐藏（Chart.js 默认行为）
- 同样的 `24小时 / 7天 / 30天 / 1年` 切换
- 系列名用入站 remark；remark 为空时回落 `#<id>`

**图表与它的 Vue 指令必须留在 `#app` 内**，`TestVueDirectivesLiveInsideAVueRoot` 守着这条。写到外面是完全静默的死代码：页面照常渲染，图就是不出来，控制台也不报错。

系统状态页现有一个每 2 秒轮询 `/server/status` 的循环。**图不挂进这个循环**——它的数据一小时才变一次，跟着 2 秒刷新是纯粹的浪费。图单独在 `mounted` 拉一次，切换范围时重拉。

## 8. 已知取舍

| 取舍 | 说明 |
|---|---|
| 时区改变后交界处有一天错位 | 见 §3.3。重切历史需要早已聚合掉的原始数据，估算补偿等于用假数据覆盖真数据 |
| 面板停机期间无数据 | 面板不跑就采不到。图上表现为一段 0，与「真的没用流量」无法区分。这是采样式统计的固有属性，不做标注（标注需要另存一份面板在线时段表，复杂度不成比例） |
| 最多丢 10 秒数据 | 面板重启时 xray 侧已清零的那一轮增量会丢。累计流量本来就有同样的窗口，不新增问题 |
| 图与累计流量可能有微小差异 | 历史桶不受重置流量影响，累计计数器受。这是刻意的语义差别，不是 bug |
| xray 未运行时不采集 | `XrayTrafficJob` 首行就 `IsXrayRunning()` 判空返回，沿用 |

## 9. 测试

按项目现有分布，测试跟着被测代码所在的包走。

**`database/model` 或 `web/service`（桶对齐与聚合，纯函数优先）**

- 小时桶 / 日桶对齐：正常时刻、整点边界、跨日、跨月
- 非 UTC 时区（`Asia/Shanghai`）下日桶起点是本地 00:00，不是 UTC 00:00
- 零增量不写行
- UPSERT 累加：同一桶写两次，值相加而不是覆盖

**`web/service`（服务）**

- `Cleanup` 按 granularity 分别套保留期：小时桶该删的删了，同时间的日桶不受影响
- `PruneOrphans` 删掉已不存在入站的行，保留存在入站的行
- `DeleteByInbound` 只删指定入站
- 补零：范围内缺失的桶补 0，返回的刻度数与范围一致
- Top N：按总量排序，不足 N 个时全返回
- `GetTrafficDB()` 为 nil 时不 panic，返回空结果

**`web`（模板与前端模型）**

- `TestAllTemplatesParse`、`TestVueDirectivesLiveInsideAVueRoot` 已存在，改完模板跑一次即可覆盖
- `TestAllSettingFieldsExistInFrontendModel`（`web/setting_model_test.go`）自动覆盖两个新设置项的前端同步

**门禁**：`make verify`（vet + test + build），与 CI 一致。

## 10. 发布

按项目现有流程，走正式发版：

1. 本地实现 + `make verify` 通过
2. 提交（不推送，等确认）
3. 打 tag 推 GitHub → `release.yml` matrix 构建 amd64/arm64 → 打包 → `gh release create`
4. 服务器上 `a-ui update`

**版本号不在代码里维护**，CI 会把 tag 名写进 `config/version`。这一点对本功能尤其重要：`cur_ver` 决定 assets 的强缓存是否失效，新 tag 会让新加的 `chart.umd.min.js` 与改动过的页面脚本正常生效。

同步更新 `CLAUDE.md`，新增一节说明本子系统。**另需修正 `CLAUDE.md` 中已经过时的两处**（在本次实现中发现，与本功能无直接关系但会误导后续改动）：

- 「`database/model/model.go` — 仅 3 张表」：实际已有 `AccessLog`、`IPBan`、`DomainGroup`、`OutboundNode`、`RoutingRule` 等
- 「cron 任务没有 panic 恢复」：`web/web.go:347` 已配 `cron.WithChain(cron.Recover(cronLogger{}))`
