# 共享风险引擎：网络画像 + 错峰检测 + 风险评分 设计文档

日期：2026-09-11
状态：已实施至 §17 第 6 步（v4，见 §18 修订记录）
前置：`docs/superpowers/specs/2026-09-05-inbound-sharing-detection-design.md`（本文是它的第二期，起点正是该文 §9 的两条已知漏检）

## 1. 背景与目标

第一期把「不同省份的 IP 在同一小时实质使用同一个入站」这个事实做出来了，并且用生产数据验证了它的支点：**并存**区分得开旅游与转卖。但它自己在 §9 里留了两条：

1. **错峰共享检测不到**（白天甲、晚上乙，落在不同小时，不产生并存记录）。
2. **只报事实、不做分级**，管理员拿到的是 `并存 37h / 3省`，得自己判断这是转卖还是出差。

同时，第一期上线后暴露了三条文档里没写的问题：

3. `SharingService.Summary` 是全表扫，而它每次加载入站列表都跑。
4. mKCP / QUIC 入站上功能静默不生效，地区列空白，与「没有共享」不可区分。
5. IPv6 下 `CoexistStat.ByIP` 降级口径的误报率远高于界面文案。

本期目标：把「一个 IP 一条记录」升级成「若干个网络画像」，在画像之上补上错峰检测，并把结论收敛成一个**可解释的** 0–100 风险分。第 3、4、5 条作为前置一并修掉——它们各自独立有价值，不依赖风险引擎。

### 非目标（本期不做，理由见 §14）

- 不做任何自动处置。不自动封禁、不自动限速、不自动写地区限制。
- 不做 Trusted Network（可信网络标记）。
- 不做 GuardMode / 智能保护。
- 不做多 UUID / 每设备一凭证。
- 不把并发拒绝历史纳入评分。
- 不做阈值配置项。判定参数一律写死为常量。
- 不引入任何全局设置项（`entity.AllSetting` 一个字段都不加）。
- 不上机器学习。没有标注数据，黑盒告警比没有告警更糟。

### 一句必须先说清楚的话

**这套引擎做的是诊断，不是防止。** 第一期 §9 已经写死了能力边界：vmess / vless 的凭证就是一个 UUID，拿到即可复制，「确保只有用户自己用」在这个协议栈里技术上做不到。风险分不会让任何一个共享者停下来，它只是让管理员知道该找谁谈。真正抬高共享成本的是并发限制（已有）、限速（已有）、每设备独立凭证（没有，见 §14.4）。界面文案不得偏离这一条。

## 2. 现状核实

以下均为读代码确认的事实，非推断。

| 事实 | 位置 |
|---|---|
| `InboundIPHour` 已有 `Province / ActiveSeconds / ActiveBytes / ActiveUp / ActiveDown` | `database/model/sharing.go` |
| 唯一索引是 `(inbound_id, ip, hour_start)`，**没有以 `hour_start` 打头的索引** | 同上 |
| `Summary` 的 `WHERE hour_start >= ?` 不带入站条件，且把窗口内全部行 hydrate 进内存 | `web/service/sharing.go` `windowRows` / `Summary` |
| `Summary` 每次加载入站列表都被调一次 | `web/html/xui/inbounds.html:603` → `web/controller/inbound.go:447` |
| 降级口径按 **原始 IP** 分桶：`byHourIP[r.HourStart][r.IP]` | `web/service/sharing_stat.go` `computeCoexistGated` |
| 「实质使用」门槛是 `coexistMinActiveBytes = 1 << 20`（1 MB/小时） | `web/service/sharing_stat.go:30` |
| 判据必须**对整批数据算一次**，不能逐行/逐小时各算各的 | `sharing_stat.go` `hasActiveBytes` 注释；`ip_usage.go:93` 同构 |
| 落库门槛 60 秒、单入站单小时 50 行上限 | `web/service/sharing_accumulator.go:19,25` |
| `provinceOf` 是多源按固定顺序取第一个非空 Region | `web/service/sharing.go` |
| `ipdb.DB.Lookup` **只收录 IPv4，IPv6 一律返回 false** | `util/ipdb/ipdb.go:85` |
| **境外 IPv4 只保留 Country 与 IDC 名，Region / City 一律丢弃** | `util/ipdb/ipdb.go:114-117` `normalize()` |
| `ipdb.Location` 含 `Country / Region / City / ISP` 四个字段 | `util/ipdb/ipdb.go:55` |
| `CanonicalISP` 已归一三大运营商 + 广电 + 教育网/科技网，并按子串识别 35 个关键词、归一到 21 个云厂商/IDC 名 | `util/ipdb/isp_name.go` |
| `CanonicalISP(raw, foreign=true)` 对境外**非 IDC** 一律返回空串 | `util/ipdb/isp_name.go:97` |
| **`transportObservable(streamSettings) (bool, string)` 已存在**，识别 mKCP/QUIC 并返回人话原因 | `web/service/online.go:476` |
| 在线明细列已经在用它显示「—」加原因 | `web/service/online.go:558,647` |
| `netdiag.Supported` 是**编译期常量**，Linux 为 true，其余平台为 false | `util/netdiag/netdiag_linux.go:12`、`netdiag_other.go:6` |
| `util/netdiag` 只读内核 **TCP** 连接表 | `util/netdiag/netdiag.go:1` |
| 活跃判定已带 1 KB/s 速率门槛（`minActiveRate = 1024`），来自香港节点 120 秒逐秒实测 | `web/service/online.go:36-47` |
| `SharingSampleJob` `@every 30s`，独立于 `ConcurrencyJob` | `web/web.go:339` |
| `ConcurrencyService.limitedInbounds` 的 WHERE 是 `concurrency_limit > 0 or id in (有封禁的)` | `web/service/concurrency.go:64-77` |
| `Enforce()` 在「无人设额度且无封禁」时提前返回，一次系统调用都不做 | `web/service/concurrency.go:108-117` |
| `model.IPBan` 没有 `Source` / `Reason` 字段 | `database/model/ipban.go` |
| `DelInbound` 已有一处引用检查（分流规则）加六处连带删除 | `web/service/inbound.go:151-200` |
| `TrafficCleanupJob` `@every 1h`，承担保留期清理 + `PruneOrphans` | `web/web.go:354` |
| traffic 库的 `AutoMigrate` 已有 **5** 张表（`TrafficBucket` / `InboundIPHour` / `DomainStat` / `DomainStatCursor` / `MeterDomain`） | `database/db.go:207-225` |
| `cron.AddJob` 的首次执行在一个完整周期之后；已有 job 靠 `startTask` 里的延迟 goroutine 做首轮 | `web/web.go:366-372` |
| **`startTask` 里的延迟 goroutine 不经过 cron 的 Recover**，`PanelVersionJob` 那条至今没有 `common.Recover` | CLAUDE.md「已知偏差」；`web/web.go:369` |
| `sharing_modal.html` 有**自己的 Vue 根实例**（`#sharing-modal`），不在 `#app` 内 | `web/html/xui/sharing_modal.html:2`；`inbounds.html:1133` |
| 前端只编辑 `settings.<protocol>es[0]`，链接生成同样取 `[0]` | `web/assets/js/model/xray.js:858,1157,1261` |
| `util/accesslog.Entry` **已解析 `Email`**，与 `SourceIP` 在同一行 | `util/accesslog/accesslog.go:27,94,157`；样本 `accesslog_test.go:59` |
| `model.AccessLog` **没有** Email 列 | `database/model/accesslog.go` |
| `accessLogEnable` 默认关闭，保留期由另一个设置项控制 | 第一期 spec §2 |
| xray 的 user 级字节计数存在，但 `trafficRegex` 只认 `inbound\|outbound` | `xray/process.go:24,272-275` |
| 模板 `policy` 只有 `system` 段，**没有 `levels`** | `web/service/config.json` |
| 新增设置项需同步改 5 处，漏掉 `models.js` 会让**整个保存配置接口失败** | CLAUDE.md「设置系统」 |
| SQLite 复用被删除的自增 id | CLAUDE.md「已知偏差」 |
| 低对比度提示等于没有提示：v1.23.0 修过一次 `rgba(0,0,0,.25)` 虚线，对比度 **1.84:1** | v1.23.0 复盘 |

### 2.1 由现状推出的五条硬约束

**约束一：地理信号的可用性是分层的，不是有无二元。** `normalize()` 对境外 IPv4 只留 Country，对中国 IPv4 留到 Province + City，对 IPv6 什么都没有。所以「Province 为空」**不等于**「没有地理信息」——一个全境外用户群 Province 全空，但跨国并存完全测得出来。判据必须建在**可用字段的层级**上（§6），不能建在某一个字段空不空上。

**约束二：非 TCP 传输与非 Linux 平台上一条采样都没有。** `netdiag` 只读 TCP 连接表，`Supported` 还是编译期常量。现在的表现是地区列空白——难看但不骗人；换成 0–100 分之后会变成「风险 0 / 低」，那是一个确信的错误结论。必须有独立的 `unobservable` 状态，**而且产出它的路径必须不依赖该入站有没有数据行**（§10.1）。

**约束三：判据必须整批算，不能逐行。** 仓库里已有两处（`hasActiveBytes`、`hasUnsplitBytes`）用整批降级。本期的 `IdentityVersion` 用连续 Epoch 兑现这一条（§4.2）。

**约束四：不能把设备级检测建在 AccessLog 上。** 第一期 §2.1 已定死：访问日志默认关闭、保留期由另一个设置项控制。本期继续成立，也是 §14.4 推后多 UUID 的主要原因之一。

**约束五：状态一旦定义，就必须有可达的入口。** 定义了 `unobservable` 却不给它渲染入口，等于没定义（§11）。

## 3. 网络族：两个不同的需求，不是一个常数

初稿把「IPv6 用哪一级前缀」写成一个必须先在生产数据上实测才能定的常数，并把它列为第 0 步的前置。那是把**两个不同的需求**混成了一个数。拆开之后，其中一个的答案由协议本身确定，另一个的答案可以用面板自己积累的数据回标——**都不需要单独从生产机取数**。

### 3.1 需求 A：小时内分桶（§17 第 2 步用）

降级口径问的是「这一小时里有几个不同的来源」。它要挡的是 IPv6 privacy extensions：同一台设备按 RFC 8981 轮换接口标识，而**随机的那部分就在地址的后 64 位**——前缀不变。所以：

```
IPv4 → /24
IPv6 → /64
```

`/64` 对这个需求是**协议层面确定够用**的，不需要任何实测。前缀本身变（PPPoE 重拨、DHCPv6-PD 续约）发生在小时之间，小时内极少；即便真的发生，结果也只是这一小时数成 2 个而不是合并成 1 个——与今天按原始 IP 计数完全一致，不会比现状更差。

**这一步是单向改进，没有需要权衡的取舍。**

### 3.2 需求 B：跨天的画像身份（§17 第 5 步用）

画像要回答的是「这几天里这些地址是不是同一个网络」。这里家宽重拨会不会换掉前缀才真正要紧，而这是运营商、地区、CPE 行为相关的经验问题，规范文档答不了，只能看真实数据。

但它**不构成前置阻塞**，因为：

**粗前缀可以从细前缀截出来。** `/64` 的前 56 位就是 `/56`，前 48 位就是 `/48`。所以「按哪一级聚合」是分析层的一个纯函数常量，改它既不需要重新采集，也不需要迁移任何数据。

于是这个决定自然落到 §17 第 5 步之前，用**面板自己已经积累的行**回标，与第 7 步的阈值回标是同一件事、同一批数据、同一个时机。判据：

```
每入站每日去重族数    中位数 ≤ 2   且   p90 ≤ 4
跨日 persistence      同一个族在窗口内出现的天数中位数 ≥ 3
有效 inbound-day 样本 ≥ 200
```

取满足全部三条的**最长（最具体）**前缀——从 `/64` 往回退，第一个满足的就停，`/56` 与 `/48` 都满足时选 `/56`。方向不能反：前缀越短聚合越多、去重族数越少，取「最短」必然选到最粗的一级，把整个省糊成一个画像。`/48` 是硬下限，比它更短会开始跨接入商聚合。

样本不足（v6 有效行占比 < 5% 或 inbound-day < 200）时：画像层的 v6 聚合级别保持 `/64`，并在 §13 记为能力边界。`/64` 在这里是**保守**的一侧——它可能把一个重拨过的用户拆成两个画像，而画像数量本身不计分（§6.2），只有 `distance ≥ 2` 才计分，而同一个人重拨前后的两个 `/64` 地理属性相同、距离为 0。所以拆开的代价是「少一点信息」，不是「多一个误报」。

### 3.3 因此网络族不落库

`networkFamilyKey(ip)` 是 `ip` 列的**纯函数**，而 `ip` 已经在库里了（`net.IP.String()` 的规范形式，v4-mapped 已在 `normalizeIP` 折成四字节）。存一个派生列不但没有收益，还有害：聚合级别一旦调整，存下来的值当场变成错的，而且没有任何一层会报错。

这与 §4.1「不存 `ProfileID`，存原始特征」是同一条原则——快照要存的是**会变的观测事实**（`ipdb` 会更新，同一个 IP 上月判江苏这月判上海），而不是可以随时重算的纯函数结果。

## 4. 数据模型

### 4.1 `InboundIPHour` 扩列

```go
// 新增，全部为观测当时的快照
Country         string  // 归一后的国家；空串表示未知
City            string  // 归一后的城市；境外恒为空（normalize 丢弃）
ISP             string  // CanonicalISP 的结果
IdentityVersion int     // 0 = 升级前写入的行；1 = 本期采集
```

**不存 `NetworkPrefix`。** 它是 `ip` 列的纯函数，现场算即可，理由见 §3.3。

**为什么存快照而不是查询时重算**：`ipdb` 会更新，同一个 IP 上个月判江苏、这个月判上海。重算意味着历史事实跟着数据库更新而改变。这与 `Province` 当初存下来是同一条理由。

**为什么不存 `ProfileID`（与不存 `NetworkPrefix` 同理）**：画像算法将来很可能加 ASN。存固定 ID 意味着算法一改就要迁几十万行；存原始特征则历史数据可以直接重算，这也是 §10.2 `EngineVersion` 自动重算的前提。

**不加 `GeoConflict` 列。** 多源分歧是展示层信息，拿它当风险信号没有依据——v1.23.0 的复盘确认过一次：那次看起来像「地区限制漏了」的事故，真相是 ip2region 判错而并集放行完全正确。把数据源质量问题折进风险分，等于算到用户头上。

**新增两个索引**（修的是现存问题，不是新功能的配套）：

```
idx_iph_inbound_hour  (inbound_id, hour_start)   -- Detail / RiskJob 的按入站取窗口
idx_iph_hour          (hour_start)               -- Summary 的全库取窗口
```

现有唯一索引 `(inbound_id, ip, hour_start)` 保留不动——`ip` 夹在第二位，范围查询用不上它。

**存储代价要重算。** 现有注释按 ~100 字节/行、对抗性上限 `50 × 24 × 30 ≈ 3.6 MB/入站`。新增三个字符串列后按 ~190 字节/行估，上限约 **6.8 MB/入站**；正常规模 30 天约 **2.9 MB/10 入站**。注释里那个数字必须一并改掉。

### 4.2 升级后的老数据：连续 Identity Epoch，不按行数占比

老行的四个新列全为零值、`IdentityVersion = 0`。

- **绝不用当前 `ipdb` 给它们补齐。** 那不是当时的画像，补出来的是假的历史事实。

**Epoch 定义**：在当前 7 天窗口内找出最后一个含 `IdentityVersion = 0` 的小时 `H₀`，取

```
analysisStart = H₀ + 1 小时     （窗口内没有旧行时 = 窗口起点）
```

Risk 引擎**只分析 `[analysisStart, now]`** 这段——这一段内全部行都是当前 `IdentityVersion`，语义单一。

**为什么不用「行占比 ≥ 80%」**（初稿的写法）：

1. **行数不等于时间覆盖。** 一个被扫的小时能塞进 50 行，占比会被少数几个小时拽过门槛，而那几个小时里没有任何有效使用。
2. **两套语义会混用。** 占比达标后整段 7 天一起分析，其中仍混着老行——正是约束三禁止的事。
3. 初稿还据此写了「升级后约 3 天」，**算术也是错的**：7 天的 80% 是 5.6 天。

Epoch 方案没有这些问题：`[analysisStart, now]` 天然连续、语义单一，跨度随时间自然增长，7 天后自动变成完整窗口。跨度不足时由 §8.4 的 Confidence 判成 `learning`，不需要单独的门槛。

**老行仍然照常服务于第一期的 `/sharing/*` 两个接口**（它们只用 `Province`，那一列老行是有的）。本期不改那两个接口的任何行为。

### 4.3 `InboundRiskSnapshot`（新表，落 traffic 库）

```go
type InboundRiskSnapshot struct {
    InboundId     int    `gorm:"primaryKey"`
    EngineVersion int
    Score         int
    Confidence    int
    Level         string  // 仅 State==ready 时非空：low/watch/medium/high/severe
    State         string  // ready / learning / degraded / unobservable
    Reason        string  // unobservable 时的人话原因；其余为空
    GeoCoverage   int     // 0–100
    AnalysisStart int64
    EvaluatedAt   int64
    LastDataHour  int64
    EvidenceJSON  string
}
```

放 traffic 库而不是主库：它是 `InboundIPHour` 的纯派生物，traffic 库丢了风险分就该跟着失效。

**不存 `ProfilesJSON`。** 画像列表只有 Detail 需要，而 Detail 是单入站按需请求，现场算（§10.3）比往每行 Snapshot 里塞一个大 JSON 划算。

加上它之后 traffic 库共 **6** 张表。

## 5. 网络画像（Network Profile）

### 5.1 网络族

由 `networkFamilyKey(ip)` 现场从 `ip` 列算出，不落库（§3.3）：

```
IPv4 → /24
IPv6 → /64 采集；画像层按 §3.2 回标出的级别再截一次
```

**网络族只是画像的子特征，绝不单独作为身份。** 动态家宽换 `/24` 甚至换 `/16` 是常态。它的用途只有两个：在地理信息缺失时作为唯一可用的降级身份（§9）、以及降低第一期 `ByIP` 口径的误报（§17 第 2 步）。

### 5.2 画像键

按可用信息分层，第一个能构造出来的即为该行的画像键：

| 场景 | 画像键 | 说明 |
|---|---|---|
| 中国 IPv4 | `CN\|<省>\|<ISP>` | 城市**只作子特征**，不进键 |
| 中国 IPv4，ISP 未知 | `CN\|<省>\|prefix:<网络族>` | |
| 境外，已识别 IDC | `<国>\|IDC\|<IDC名>` | `normalize()` 对境外只留 Country + IDC，天然如此 |
| 境外普通网络 | `<国>\|prefix:<网络族>` | 境外普通 ISP 库里本就不保留 |
| 无国家信息（v6 / 库未加载） | `prefix:<网络族>` | 参与 `GeoCoverage` 的分母 |

**城市不进键。** 运营商 IP 定位到市的稳定性远不如到省——南通 / 南京 / 苏州判成三个用户是纯粹的误报来源。

### 5.3 画像的度量

```go
type NetworkProfile struct {
    Key      string
    Country, Province, ISP string
    Cities, Prefixes, IPs  []string   // 各自升序去重，仅供展示

    FirstSeen, LastSeen int64
    ActiveDays          int
    ActiveHours         int
    Bytes, Up, Down     int64
    TrafficShare        float64
    Hosting             bool
}
```

**刻意不提供 per-prefix 的字节 / 天数 / 小时数。** 初稿的 S7（同省同 ISP、网络族长期分离且两侧各 ≥30% 流量）需要这个维度，但画像键 `CN|省|ISP` 已经把同省同 ISP 的不同网络族合并进同一个画像，这个数据根本算不出来——那是一条**定义了却无法计算**的信号，已在 v2 删除（§8.1）。将来确有需要时，另立 `PrefixStat` 单独立项，不在本期硬塞。

**「有效小时」的定义与并存判定完全一致：`ActiveBytes >= coexistMinActiveBytes`（1 MB）。** 不另搞门槛。两套门槛会让管理员看到「共享检测：无异常 / 风险评分：高风险」而不知道为什么。

**稳定画像**（下面所有信号只认稳定画像）：

```
ActiveDays >= 3  且  ActiveHours >= 6  且  Bytes >= 10 MB  且  TrafficShare >= 5%
```

### 5.4 聚合必须是纯函数

```go
func buildProfiles(rows []model.InboundIPHour, loc *time.Location) []NetworkProfile
```

不查库。本期阈值几乎必然要按生产分布回标（§17 第 7 步），纯函数才谈得上用真实数据反复重算。输出按 `Bytes desc, Key asc` 排序——**禁止遍历 map 产生数组顺序**。

## 6. 画像距离：本期唯一的新支点

第一期的支点是「并存」。本期加的是画像，而**画像数量本身不是信号**——这是整个设计里最容易做错的一点。

### 6.1 距离的完整定义

```
distance(A, B):
  A.Country == "" 或 B.Country == ""                          → unknown
  A.Country != B.Country                                       → 3
  A.Country == B.Country == CN:
      A.Province 或 B.Province 为空                            → unknown
      A.Province != B.Province                                 → 2
      A.Province == B.Province 且 A.ISP != B.ISP               → 1
      A.Province == B.Province 且 A.ISP == B.ISP               → 0
  A.Country == B.Country != CN（同一个外国）                   → unknown
```

最后一条是有意的保守取值：`normalize()` 对境外只留 Country，同一个外国内部没有任何可用的地理维度。代价是「US Amazon + US Vultr」这种组合不计分，记录为能力边界（§13）。

| 距离 | 关系 | 信息量 |
|---|---|---|
| 0 | 同省同 ISP | 零：动态家宽换段是常态 |
| 1 | 同省不同 ISP | 近乎零：**家宽 + 手机就是这个形态** |
| 2 | 不同省（中国境内） | 有 |
| 3 | 不同国家 | 有 |
| unknown | 信息不足 | 不参与任何计分信号 |

另有一个正交维度：一侧 `Hosting` 而另一侧不是，且 `distance ≥ 2` → 进 S5。

### 6.2 为什么必须这么设计

一个完全正常的单人用户，家里江苏电信宽带、外出用江苏移动蜂窝，在 §5.2 的画像键下就是两个独立稳定画像，而且天然错峰（在家用宽带、在外用流量），三道稳定性门槛全部轻松满足。

若把「两个稳定画像 + 错峰」直接计分，这个用户拿到的分数会与「江苏电信 + 广东移动错峰共享」**完全相同**——区分度为零，而且不是调参能解决的：提高错峰权重，两边一起涨。

所以：**所有计分信号一律以 `distance ≥ 2` 为前置条件。** 距离 0、1、unknown 一律不计分。

## 7. 错峰检测

### 7.1 定义

对每个稳定画像，按面板时区构造每日 24 小时位图（`uint32` 低 24 位）。对任意两个稳定画像 A、B：

```
SharedDays    = 两者都有活跃的日历天数
AHours/BHours = 各自的有效小时总数
OverlapHours  = 同一天同一小时两者都有效的小时数
OverlapRatio  = OverlapHours / min(AHours, BHours)
```

### 7.2 触发门槛（全部同时满足）

```
distance(A, B)   >= 2        ← §6，没有这一条整个信号无意义
A.ActiveDays     >= 3   且   B.ActiveDays     >= 3
A.ActiveHours    >= 6   且   B.ActiveHours    >= 6
A.TrafficShare   >= 10% 且   B.TrafficShare   >= 10%
SharedDays       >= 3
OverlapRatio     <= 10%
```

**`SharedDays >= 3` 专门挡旅游。** 一次出差是「旧省停、新省起」，两个画像各自活跃 5 天但**共同活跃**只有交界的 1–2 天，达不到门槛。

### 7.3 错峰不能单独定罪——这条在 v2 已升级为代码硬约束

**跨省错峰与「每周在两个城市之间通勤的同一个人」在网络层完全同形。** 两个稳定画像、各占相当比例流量、长期几乎不重叠——搬过家的、周末回老家的、异地双城工作的，全是这个形状。没有任何网络层数据能把它们分开。

初稿只把这条写成文案约束，靠权重之和「恰好」达不到 70。v2 改为**显式钳制**（§8.2）：没有强地理并存证据时 `Score = min(Score, 69)`。理由是权重将来一定会按生产分布回标（§17 第 7 步），而「回标之后错峰能不能单独上高风险」不该取决于有没有人记得这条隐含前提。

## 8. 风险评分

### 8.1 全部信号（可解释，无黑盒）

「地理并存」指 `distance ≥ 2` 的两个画像在同一小时都有有效使用（§8.5，Risk 层自算，不复用 `computeCoexist`）。

| 代码 | 信号 | 前置 | 分值 |
|---|---|---|---|
| `coexist_geo` | 跨省 / 跨国并存实质使用 | — | 并存小时 3–5 → 15；6–11 → 25；≥12 → 35 |
| `coexist_multi_day` | 并存发生在 ≥3 个不同日期 | 上一条成立 | +10 |
| `coexist_multi_region` | 并存涉及 ≥3 个不同地区 | 上一条成立 | +5 |
| `temporal_geo` | 跨省 / 跨国错峰（§7） | distance ≥ 2 | +20 |
| `temporal_persistent` | 且 `SharedDays ≥ 5` 且 `OverlapRatio ≤ 5%` | 上一条成立 | +15 |
| `secondary_share` | 第二个 `distance ≥ 2` 稳定画像的流量占比 | **须已有并存或错峰证据** | 10–25% → 5；≥25% → 10 |
| `multi_geo_profile` | 第 3 个及以上 `distance ≥ 2` 稳定画像 | **须已有并存或错峰证据** | 每个 +5，上限 10 |
| `access_plus_hosting` | 接入网画像与 IDC 画像跨地区稳定共处 | distance ≥ 2 | +10 |
| `temporal_same_province` | 同省不同 ISP 的稳定错峰（distance = 1） | — | **0 分，仅作上下文证据** |

`coexist_*` 三条合计上限 50，`temporal_geo` 两条合计上限 35。

**`temporal_same_province` 第一版不计分**，只在证据列表里列出来。它会对几乎每个双设备用户命中（家宽 + 手机就是这个形态），给分等于给所有正常用户一个固定底分，纯噪声。先采集两周分布，再决定要不要赋 +5。

**初稿的 `split_prefix_same_isp`（S7）已删除**，理由见 §5.3：它需要的 per-prefix 数据在当前画像结构里算不出来。

**`secondary_share` 与 `multi_geo_profile` 是放大器，不是独立证据。** 没有并存也没有错峰时，它们描述的恰恰是一次搬家或一趟长差——第二个省承担四成流量、七天里去过三个省，对一个旅行的人完全正常。这两条若不加前置，§8.3 第 2 行那个必须为零的标定场景会拿到 15 分。`access_plus_hosting` 自己内部已经要求并存或错峰，不必再挡一次。

### 8.2 分级与钳制

```
Score = min(100, Σ)

若不存在 coexist_geo（即没有任何 distance ≥ 2 的地理并存证据）：
    Score = min(Score, sharingRiskNoCoexistCeiling)   // = 69
```

```
0–29   低      30–44  观察      45–69  中      70–84  高      85–100 严重
```

**`Level` 只在 `State == ready` 时才计算和显示。** `learning` / `degraded` / `unobservable` 三种状态一律不映射成 `low`——把「测不了」「还没测够」显示成「低风险」，是本期要防的首要错误（约束二、约束五）。

界面写「共享风险：高」，**不写「该用户正在共享」**。第一期「只报事实、不替管理员下判断」在本期演化成「报分数 + 报依据 + 报另一种解释」。

### 8.3 对六个标定场景的验算

设计时必须能算出这六个数，实现后由测试钉住（§15）：

| 场景 | 命中信号 | 分 | 档 |
|---|---|---|---|
| 家宽 + 手机（同省，电信 + 移动，错峰） | `temporal_same_province`（0 分） | **0** | 低 |
| 出差一周（江苏 → 广东，迁移，交界并存 2h） | 并存 < 3h 不计；`SharedDays` 2 < 3 不计 | **0** | 低 |
| 跨省错峰（江苏电信白天 / 广东移动夜间，7 天，第二画像 40%） | 20 + 15 + 10 | **45** | 中 |
| 错峰拉满（4 个跨省画像 + IDC，但零并存） | 35 + 10 + 10 + 10 = 65，钳制不触发 | **65** | 中 |
| 跨省长期并存（12h+、5 天、第二画像 35%、3 个画像） | 35 + 10 + 10 + 5 | **60** | 中 |
| 上一条再加第 3 个地区并存 + 第 4 个画像 | 35 + 10 + 5 + 10 + 10 | **70** | 高 |

**注意第 5 行只到「中」而不是「高」。** 这是刻意的：12 小时跨省并存确实是本系统能拿到的最强证据，但它与「本人出差、家中设备保持在线」仍然同形。要压到「高」以上，需要并存在多日、多地区重复发生。这条取向与第一期「误报冤枉用户比漏报严重」一致。

第 4 行验证钳制是一道**冗余**防线：当前权重下无并存路径最高 65（35 + 10 + 10 + 10），本来就 < 69。保留它是为了让这条不变量在将来调权重时仍然成立，并且它是可测试的。

### 8.4 Confidence 与 Score 分开

```
观测跨度（analysisStart → now）             ≥72h   → 30；≥24h  → 15
有效活跃小时（过 1 MB 门槛）                 ≥24    → 25；≥6    → 12
有效流量                                     ≥100MB → 20；≥10MB → 10
GeoCoverage                                  ≥80%   → 25；≥50%  → 12
```

`Ready = Confidence >= 60`。未 Ready → `State = learning`，显示进度，不显示等级。

一个刚建 2 小时的入站可能信号很强（江苏 + 广东同时出现），但样本太少。`Score 75 / Confidence 20` 必须显示成「检测到异常，数据仍在积累」，而不是红色的「高风险」。

### 8.5 Risk 层自算地理并存，不复用 `computeCoexist`

`computeCoexist` 只认 `Province`，所以「US + JP 长期并存」在它眼里是两条 Province 为空的记录，整体退化成 `ByIP` 口径。Risk 层已经有 Country，应当按 §6.1 的距离判定并存，覆盖跨省与跨国两种。

**旧的 `computeCoexist` 与 `/sharing/*` 两个接口的契约一个字节都不改。** 新逻辑写成 Risk 层内部的 `computeGeoCoexist(rows, profiles)`，与旧函数共用同一个 1 MB 门槛常量。

由此引出一个必须处理的展示后果：弹窗里若同时渲染旧 `stat.hours`（可能是 0）和新的地理并存小时（可能是 12），两个数字会当场打架。**解法：弹窗的并存概要与并存时段表统一改由 Risk Detail 提供**（§10.3），旧接口保留供契约兼容与将来可能的外部调用，但弹窗不再用它渲染。

## 9. 四种状态，缺一不可

`GeoCoverage` = 窗口内**有效行**（过 1 MB 门槛）中能解析出非空 `Country` 的比例。用 Country 而不是 Province，理由见约束一：境外 IPv4 天然没有 Province，用 Province 会把整个境外用户群误判成降级。

| State | 触发 | 界面 |
|---|---|---|
| `unobservable` | `!netdiag.Supported`，或 `!transportObservable(inbound.StreamSettings)` | 不显示分数与等级。显示 `Reason` 里的人话原因 |
| `degraded` | **有有效数据**且 `GeoCoverage < 50%` | 显示分数与 GeoCoverage，**不显示普通等级**。明说跨省/跨国信号不可用，当前只有网络族口径 |
| `learning` | `Confidence < 60`，或该入站尚无 Snapshot，或 `EngineVersion` 与当前值不匹配 | 显示「学习中」与进度，不显示分数等级 |
| `ready` | 其余 | 正常显示 |

判定顺序固定为 `unobservable → degraded → learning → ready`，先命中先返回。

**`degraded` 必须带「有有效数据」这个前提。** 覆盖率对空输入是 0，只按覆盖率判的话，一个刚建好、还没人连过的入站会显示成「降级」，而它的真实状态是「还在学习」。这与第一期 `computeCoexist` 开头那句「没有数据不等于归属地库未加载」是同一个坑。

**`unobservable` 是本期新增状态里最要紧的一个。** 没有它，mKCP / QUIC 入站会稳定显示「风险 0 / 低」——一个确信的错误结论。实现上几乎是白捡的：`transportObservable` 已存在于 `web/service/online.go:476`，在线明细列已在用它，本期复用同一个函数与同一套原因文案，两处口径天然一致。

**`degraded` 下 `coexist_geo` 仍可能有值**：地理距离算不出来时退回网络族口径（两个不同网络族在同一小时都有有效使用），但该口径的 `coexist_*` 三条合计**上限 10**。第一期已经写明降级口径误报率高得多——同一个人的手机和宽带就是两个 IP/两个族。

## 10. 预计算与接口

### 10.1 `SharingRiskJob`：**按入站驱动，不从数据行反推**

这是 v2 修掉的一个硬缺陷。初稿写的是「读最近 7 天 → 按入站分组」，而 mKCP / QUIC 入站**一行数据都没有**，分组里根本不会出现它们——于是 §9 定义的 `unobservable` 永远发不出来，这个状态等于没定义。非 Linux 平台同理（`netdiag.Supported` 为编译期 false，全部入站零行）。

正确流程，`@every 10m`：

```
inbounds := inboundService.GetAllInbounds()
for each inbound:
    c := countabilityOf(inbound, netdiag.Supported)
    if c.unobservable:
        upsert Snapshot{State: unobservable, Reason: c.reason}   // 零行也必须写
        continue
    rows := 该入站 7 天窗口的行            // 走 idx_iph_inbound_hour
    analysisStart := identityEpoch(rows)   // §4.2
    if rows 为空 或 跨度不足:
        upsert Snapshot{State: learning, AnalysisStart: analysisStart}
        continue
    upsert Analyze(inbound, rows, analysisStart)
```

`countabilityOf(inbound, supported)` 是一个纯函数：`!supported` → 不可观测（原因「当前系统不支持内核连接表」）；否则转交 `transportObservable(inbound.StreamSettings)`，原因串原样透出。

**取舍**：逐入站查询是 N 次带索引的范围查询，而不是一次全表扫再分组。100 个入站每 10 分钟 100 次索引查询，代价可以忽略，而它是唯一能让零行入站也产出 Snapshot 的写法。

`Run` 首行 `defer common.Recover("共享风险评分任务")`。失败只告警不阻断。

### 10.2 首轮、`EngineVersion` 与 stale

```go
const sharingRiskEngineVersion = 1
```

`cron.AddJob` 的首次执行在一个完整周期之后，所以面板刚启动的 10 分钟内 Snapshot 表是空的。**空 Snapshot 绝不能映射成 `score=0/low`**——那和 `unobservable` 显示成低风险是同一类错误。两道：

1. **延迟首轮**：`Server.startTask` 里起一个 goroutine，`time.Sleep(15 * time.Second)` 后跑一次 `SharingRiskJob.Run()`。15 秒是为了避开面板启动时和 xray 抢资源。
   **该 goroutine 首行必须 `defer common.Recover("共享风险评分首轮")`。** CLAUDE.md 明确记录：`startTask` 里这种 goroutine **不经过 cron 的 Recover**，`PanelVersionJob` 那条至今没有这层保护，一个 panic 会杀掉整个面板进程。不要照抄它。
2. **缺失与版本不匹配一律当 `learning`**：controller 层读 Snapshot 时，`记录不存在` 或 `EngineVersion != sharingRiskEngineVersion` 都返回 `State = learning`。版本不匹配的旧分数不显示——Job 下一轮会无条件重算（这也是 §4.1 坚持存原始特征而非 `ProfileID` 的兑现）。

### 10.3 接口：新增两个，不动现有的

```
POST /aui/inbound/sharing/risk/summary      → {inboundId: {score, confidence, level, state, reason, geoCoverage}}
POST /aui/inbound/sharing/risk/detail/:id   → 风险详情
```

- **Summary 读 Snapshot**，一次查询，不做任何计算。它每次加载入站列表都跑，必须廉价。
- **Detail 现场调用 `Analyze()`**——与 Job 共用的同一个纯分析路径——返回最新的 `profiles / evidence / score / confidence / state`，加上并存概要与并存时段表（§8.5）。单入站按需请求，跑一次分析完全够快，而且拿到的比 Snapshot 新（最多差 10 分钟）。这也是 §4.3 不往 Snapshot 里塞 `ProfilesJSON` 的前提。

现有 `/sharing/summary` 与 `/sharing/detail/:id` 的契约**一个字节都不改**。

`detail/:id` 的 `id` 必须 `> 0`——`windowRows` 把 0 当作「读全部入站」的哨兵值，不挡住会静默返回跨所有入站聚合的结果。第一期 `Detail` 已有这道检查，新接口照抄。

### 10.4 Evidence 是结构化的

```go
type RiskEvidence struct {
    Code     string           // 与 §8.1 表格第一列一一对应
    Points   int              // 可以为 0（上下文证据）
    Profiles []string
    Metrics  map[string]any
    Title    string
    Detail   string
}
```

`Code` 必须保留——文案要能改、要能被测试断言，而不是靠比对中文字符串。文案在服务端生成，理由与用量图表的 x 轴标签一致：时区在服务端。

## 11. 面板呈现

### 11.1 每个入站都要有一个可点击的状态 chip

约束五：定义了状态却不给入口，等于没定义。初稿写的是「`unobservable` / `learning` 不打标」，而弹窗入口就是那个标签——这两个状态因此永远打不开。

「地区」列固定渲染一个 chip，统一 `@click` 打开现有 `sharing_modal`，**不新增任何菜单入口**：

| State / Level | chip 文案 | 视觉 |
|---|---|---|
| ready · low | `风险 低` | 中性灰 |
| ready · watch | `风险 观察` | 中性偏暖 |
| ready · medium | `风险 中` | 橙 |
| ready · high | `风险 高` | 红 |
| ready · severe | `风险 严重` | 红（加重） |
| learning | `学习中` | 中性灰 |
| degraded | `降级` | 中性偏暖 |
| unobservable | `不可评估` | 中性灰 |

**所有 chip 的文字对比度必须 ≥ 4.5:1，中性灰那几个尤其。** v1.23.0 修过一次同类问题：分歧提示用了 `rgba(0,0,0,.25)` 的虚线，对比度 **1.84:1**，等于看不见。「低风险」不是「不重要到可以看不见」，它是这个入站唯一的弹窗入口。

### 11.2 弹窗内容

升级现有 `sharing_modal.html`，保持它自己的 Vue 根实例（`#sharing-modal`，已是仓库认可的模式），五段：

1. **状态区** —— 四种状态各有各的写法（§9）。非 `ready` 时这一段是全部内容的主角，不显示分数等级
2. **风险依据** —— 逐条 `+N` 加一句人话，0 分的上下文证据单独成组、明确标注「不计分」，最后一行是总分（触发钳制时明说「因无地理并存证据，上限 69」）
3. **网络画像表** —— 画像 / 活跃天数·小时 / 流量 / 占比 / 网络族数 / 最近活跃
4. **并存明细** —— 数据来自 Risk Detail（§8.5），不再用旧接口渲染
5. **地区限制建议**（原样保留，含「并存省份单独标出、不自动剔除」那条）

前端约束：新增内容仍留在 `#sharing-modal` 这棵子树内。`TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` 自动覆盖。

**不加任何设置项。** 本期一个 `entity.AllSetting` 字段都不加，因此不触发那条「同步改 5 处、漏掉 `models.js` 会让整个保存配置接口失败」的风险。

## 12. 清理

`InboundRiskSnapshot` 是第七张按入站 id 存的表，必须补齐两道：

1. `InboundService.DelInbound` 加一处连带删除，照现有六处的写法：**失败只告警不阻断**，由兜底收尾。
2. `TrafficCleanupJob` 的 `PruneOrphans` 加一处。

理由与前六处完全相同：SQLite 复用被删除的自增 id，残留的 Snapshot 会绑到下一个建出来的入站上，那时引用不再悬空，界面会渲染得非常合理，只是显示的是别人的风险分——而这次显示的还是一个带「严重」字样的红色标记。

Snapshot 不设保留期：每入站一行，随入站删除而删除。`InboundIPHour` 的保留期清理不变（30 天）。

## 13. 失败模式与能力边界

必须写进界面、不能只写在文档里的五条：

1. **IPv6 拿不到任何地理信息**（`ipdb.go:85`），只能靠网络族 → `degraded`。
2. **非 TCP 传输与非 Linux 平台采不到数据** → `unobservable`，绝不显示成低风险。
3. **运营商出口会漂移。** 手机一小时内从江苏出口切到上海出口是真实误报源，这是「只告警不处置」正确的又一个理由。
4. **跨省错峰与两地通勤同形**（§7.3）。
5. **跨省并存与「出差 + 家中设备在线」同形**（§8.3 第 5 行）。

其余边界：

- **同省同运营商之间的共享抓不到。** 画像键完全相同，距离为 0，一条计分信号都不会命中。这不是实现缺陷，是方法的天花板——真正能分开的只有每设备独立凭证（§14.4）。
- **同一个外国内部的两个网络分不开**（§6.1 最后一条）。`normalize()` 对境外只留 Country，「US Amazon + US Vultr」距离为 unknown、不计分。要修需要 ASN 库，不在本期。
- **IPv6 家宽重拨换前缀时，同一个人可能被拆成两个画像。** 这是 §3.2 选 `/64` 作保守默认的已知代价，落在安全侧：画像数量本身不计分，而重拨前后两个 `/64` 的地理属性相同、距离为 0，不会命中任何计分信号。回标出更短的级别后这条自然收敛。
- 面板重启最多丢一分钟累计；采集与评分失败一律只 `logger.Warning`，绝不阻断既有流程。

## 14. 明确不采用 / 推后的方案

### 14.1 Trusted Network（可信网络标记）——推后

不是不做，是**不能在阈值回标之前做**。它一上线就会变成误报的止痛药：管理员遇到一个 45 分的正常用户，标一下「可信」就过去了，于是没有人再去看那 45 分本身是不是算错了。而本期几乎所有阈值都需要用生产分布回标（§17 第 7 步）。

将来实现时要守住的一条：**Trust 抑制告警，不改分数。** `Raw Risk` 与 `Alert State` 分开，否则风险分失去事实意义。表放主库（信任关系是管理员配置），并因此必须进 `DelInbound` 与孤儿清理。

### 14.2 GuardMode / 自动处置——不做

1. **系统根本不知道哪个是本人。** 自动封禁只能封「后来出现的」，而先来后到与「谁是买家」没有必然关系。
2. **原方案自己给出的触发条件已经保守到几乎不会命中**（需同时满足：有手工标记的可信画像、可信画像当前在线、新 IP 属于另一个明确画像、无 Geo 冲突、本小时 ≥5MB、强并存证据、Risk ≥85、Confidence ≥70）。在那个前提下，一条告警加管理员手动 `IPBan` 效果完全相同，代价小一个数量级。
3. **有一个具体的静默失效陷阱**：`ConcurrencyService.limitedInbounds` 的 WHERE 是 `concurrency_limit > 0 or id in (有封禁的)`，`Enforce()` 在没有命中时一次系统调用都不做。新增一张 `GuardBlock` 表而不把它并进这个 WHERE，结果就是「开了智能保护但一次都不触发」，且完全静默。真要做，这条必须写在实现清单第一行。

另外：**不要往 `IPBan` 里塞自动封禁**。它没有 `Source` / `Reason` 字段，混进去之后界面分不清「管理员封的」与「系统自动封的」，而解封的判断完全不同。

### 14.3 并发拒绝历史作为风险信号——不做

权重低（原方案自己只给 +5~10 且要求必须有其他信号共存），但实现要动那个每秒跑一次的 job，还要新增一张表和一套「只记状态变化」的去重逻辑。收益与代价不匹配。更根本的是它的误报源很硬：管理员把 `ConcurrencyLimit` 设成 1，而用户自己 iPhone + MacBook，就会反复触发。

### 14.4 多 UUID / 每设备一凭证——单独立项

**这是唯一一个可能真正「提高共享成本」而不只是「发现共享」的方案**，但它不能挂在本期后面顺手做。

先澄清一个误解：原方案说「需要先做 Spike 验证 Xray 能不能给出 UUID ↔ source IP」。**这个问题代码里已经有答案**：`util/accesslog.ParseLine` 已经在解析 `email`，而日志行的形状是 `from <源IP>:<端口> accepted tcp:... [inbound-xxx >> direct] email: <email>`——**源 IP 与 email 在同一行**。剩下只需确认「你这个 xray 版本对 VLESS / VMess / Trojan 各协议是否都写这一段」，那是一次 5 分钟的验证，不是一个 Spike。

真正的难点是另外三条：

1. **访问日志给 IP 不给字节，而 1 MB/小时那个门槛是整个判定的支点。** 好消息是：第一期定下这个门槛的那次生产实测里，真正的判别式其实是「访问日志条数为 0」——**连接条数在这里是比字节数更直接的代理**，而访问日志恰好给得出。
2. **访问日志默认关闭**（约束四）。另一条路是 xray 的 user 级字节计数（`xray/process.go:272` 确认 stats 里会有 `user>>>xxx>>>traffic>>>uplink`），但需要：给模板 `policy` 加 `levels` 段（`policy` 在 `hot_diff` 的 static 名单里 → 一次整进程重启）、给每个 client 配 `email` 与 `level`（前端现在连 email 字段都没有）、扩 `trafficRegex`。它给得出 email→字节，但给不出 email→IP。**两条数据源各缺一半，必须拼起来用**，这正是立项要写清楚的第一件事。
3. **「一个入站 = 一个用户」在仓库里到处都是**：分流按 `inboundTag` 匹配、地区限制、并发限制、流量统计、分享链接生成（`xray.js` 十余处 `[0]`）、`web/inbound_model_test.go` 的模型不变量。这是一次真正的业务模型重构。

## 15. 测试

**距离函数**（§6.1，本期支点，单独钉）：0 / 1 / 2 / 3 / unknown 五个取值各一例，**含「同一个外国 → unknown」与「Country 为空 → unknown」两条**。

**画像聚合**（纯函数）：
- 同省同 ISP 换 IP / 换 `/24` → **不拆成两个画像**
- 同省不同 ISP → 两个画像，距离 1
- 不同省同 ISP → 两个画像，距离 2
- 境外 IDC → `Hosting = true`
- 输出顺序确定：同一份输入两次调用逐字段相等

**Identity Epoch**（§4.2）：
- 窗口内混有 `IdentityVersion = 0` 的行 → `analysisStart` 落在最后一个旧小时之后
- 窗口内全是新行 → `analysisStart` = 窗口起点
- **断言老行不参与画像计算**（不是「占比够了就整段一起算」）

**错峰**：
- `SharedDays = 2` 的旅游场景 → 不触发
- 距离 1 的家宽 + 手机 → 不进 `temporal_geo`，只产出 0 分的 `temporal_same_province`
- 跨省、7 天、`OverlapRatio = 0` → 触发且命中 `temporal_persistent`

**评分**（§8.3 六个标定场景各一条，断言**分数区间**而非精确值，留出回标空间）：
- 家宽 + 手机同省 → `< 30`
- 出差一周 → `< 30`
- 跨省错峰 → `45–69`
- 错峰拉满零并存 → `<= 69`（**钳制不变量，必须单独断言**）
- 跨省长期并存 → `45–69`
- 并存 + 多地区 + 多画像 → `>= 70`

**地理并存**（§8.5）：
- 境外 `US + JP` 长期并存 → `coexist_geo` **正常计分，不退化成降级口径**
- 旧 `computeCoexist` 对同一份输入的输出**逐字段不变**（契约回归）

**状态机**（§9，四种各一例）：
- mKCP / QUIC 入站 **零行** → 仍产出 `State = unobservable` 的 Snapshot，且 `Level` 为空。**本期最要紧的回归测试**
- 非 Linux（`netdiag.Supported = false`）→ 全部入站 `unobservable`
- 可观测但零行 → `learning`，**不是** `low`
- `GeoCoverage < 50%` → `degraded`，`Level` 为空
- Snapshot 不存在 / `EngineVersion` 不匹配 → controller 返回 `learning`，**不映射为 `low`**

**清理**：`DelInbound` 连带删 Snapshot；`PruneOrphans` 清孤儿。专门测——它防的是 SQLite 复用 id，而那个 bug 的表征是「界面渲染得完全合理，只是数据是别人的」。

**降级口径网络族**（§17 第 2 步）：
- 同一台设备的多个 IPv6 privacy 地址（同 `/64`、不同接口标识）→ `ByIP` 口径数成 1 个，不是 3 个
- 不同 `/64` 的两个 v6 地址 → 仍然数成 2 个
- `networkFamilyKey` 算不出来时（非法 IP）→ 回退原始 IP，不吞掉这一条
- IPv4 同 `/24` 的两个地址 → 数成 1 个（`ipdb` 未加载时的降级路径）

**性能**：构造对抗性上限 `50 × 24 × 30 = 36,000 行 / 入站`，`buildProfiles` + `Analyze` 单入站控制在几十毫秒量级。

**前端**：`TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` 自动覆盖。

全程 `make verify`（`go vet ./...` + `go test ./...` + `go build`）。

## 16. 改动清单

| 文件 | 改动 |
|---|---|
| `database/model/sharing.go` | `InboundIPHour` 加 4 列 + 2 个新索引；改注释里的存储上限数字 |
| `database/model/sharing_risk.go` | **新增** `InboundRiskSnapshot` |
| `database/db.go` | traffic 库 `AutoMigrate` 加第 6 张表 |
| `util/ipdb/isp_name.go` | 暴露 `IsKnownIDC(isp string) bool`（判据：是否落在 `idcKeywords` 的 name 集合里，**不另建名单**） |
| `web/service/network_identity.go` | **新增** `networkFamilyKey`（纯函数，不落库）+ `NetworkMeta` 构造 |
| **`web/service/sharing_stat.go`** | **降级口径改用网络族分桶**：`byHourIP[hour][r.IP]` → 优先 `networkFamilyKey(r.IP)`，算不出再回退原 IP（§17 第 2 步，初稿漏列） |
| `web/service/sharing_accumulator.go` | cell / observation / flush 携带 `NetworkMeta` |
| `web/service/sharing.go` | `Sample` 写入身份快照；`provinceOf` 改为 `networkMetaOf(...).Province`（现有测试全部继续成立） |
| `web/service/network_profile.go` | **新增** 画像聚合（纯函数）+ 画像距离 |
| `web/service/sharing_temporal.go` | **新增** 错峰分析（纯函数） |
| `web/service/sharing_risk.go` | **新增** `Analyze()` + 地理并存 + 评分 + Confidence + Evidence + 状态机（`countabilityOf` 已存在于 `online.go:643`，直接复用） |
| `web/job/sharing_risk_job.go` | **新增** `@every 10m`，按入站驱动 |
| `web/web.go` | 注册新 job；延迟 15 秒的首轮 goroutine（**首行 `defer common.Recover`**） |
| `web/job/traffic_cleanup_job.go` | Snapshot 的孤儿清理 |
| `web/service/inbound.go` | `DelInbound` 加第七处连带删除 |
| `web/controller/inbound.go` | 两个新接口；Snapshot 缺失 / 版本不匹配 → `learning` |
| `web/html/xui/sharing_modal.html` | 状态区 / 依据 / 画像三段，并存明细改用 Risk Detail |
| `web/html/xui/inbounds.html` | 「地区」列改为常驻可点击状态 chip（对比度 ≥ 4.5:1） |
| `web/service/*_test.go` | §15 |

**不改**：`entity.AllSetting`、`web/assets/js/model/models.js` 的入站字段、现有两个 sharing 接口的契约、`computeCoexist` 的行为、`ConcurrencyService`、`IPBan`、地区限制、封禁。

## 17. 落地顺序

前三步各自独立有价值，**不依赖风险引擎**，可以先合并上线：

1. **两个索引。** 修的是现存的全表扫（`Summary` 每次加载入站列表都跑）。
2. **改 `sharing_stat.go` 的降级口径分桶：`byHourIP[hour][r.IP]` → `networkFamilyKey(r.IP)`（v4 `/24`、v6 `/64`）。** 不改这个文件这一步就没有任何效果。
   **无前置、不需要实测**：`/64` 对「小时内有几个来源」这个问题由协议确定够用（§3.1）。网络族全程从 `ip` 现场算，不落库，所以这一步也不依赖第 4 步。
3. **`unobservable` 状态。** `countabilityOf(inbound, platformSupported)` **已存在**（`online.go:643`，在线明细列在用），无需新写；本步只是确认它可复用并补齐测试，界面透出随第 6 步的 chip 一起落地——分两次改同一个模板不划算。
4. **身份快照列 + `NetworkMeta` 采集。** 纯加数据，不改任何判定。
5. **画像 + 距离 + 错峰**（三个纯函数，先不接 UI，只写测试）。
   开写之前先做 §3.2 的回标：用第 4 步以来面板自己积累的行，定下画像层的 v6 聚合级别。数据不够就保持 `/64`（保守侧，§3.2 末段），不阻塞。
6. **`Analyze()` + Snapshot + Job + 首轮 + 接口 + modal 与 chip。**
7. **生产只观察 ≥2 周**，用三台机器的真实分布回标阈值。重点看 `ActiveDays` / `TrafficShare` / `OverlapRatio` / `GeoCoverage` / `Score` 五个量的分布，以及 `temporal_same_province` 的命中率（决定它要不要从 0 分改成 +5）。**这一步不能省，也不能和第 6 步合并**——§8 的分值全是量级判断而非实测标定，和 `coexistMinActiveBytes = 1 MB` 当初的处境完全一样。

第 7 步之后再决定 §14.1 的 Trusted Network 要不要做。

每一步独立过 `make verify`。

## 18. 修订记录

**v2（2026-09-11）**，评审意见并入。修掉 v1 的六处错误与自相矛盾：

1. **§3 判据方向写反**——前缀越短聚合越多，「满足条件的最短前缀」必然选到最粗的一级。改为取满足稳定性条件的**最长**前缀，并补上 p90、样本量、跨日 persistence 三项判据；统计改用 `net.IP` 掩码而非 SQL 字符串截取。
2. **§9 `degraded` 判据用「所有 Province 为空」是错的**——`normalize()` 对境外 IPv4 本来就只留 Country。改为基于 `GeoCoverage`（有 Country 的有效行占比），并把距离定义补全为含 `unknown` 的全函数（§6.1）。
3. **§10.1 从数据行反推入站，导致 `unobservable` 永远发不出来**——零行的 mKCP / QUIC 入站不会出现在分组里。改为按 `GetAllInbounds()` 驱动，零行也 upsert。
4. **§4.2 的「行占比 ≥ 80%」换成连续 Identity Epoch**——行数不等于时间覆盖，且达标后仍混着老行，违反约束三。原文「约 3 天」的算术也错了（7 天的 80% 是 5.6 天）。
5. **删除 S7 `split_prefix_same_isp`**——它需要 per-prefix 的字节/天数，而画像键已把同省同 ISP 的不同网络族合并，该数据算不出来。`temporal_same_province` 同时降为 0 分的上下文证据，两周后再定。
6. **§11 `unobservable` / `learning` 不打标，导致弹窗打不开**——改为每个入站常驻一个可点击 chip，并补上 ≥ 4.5:1 的对比度硬要求（v1.23.0 踩过 1.84:1）。

以及六处实质改进：Risk 层自算地理并存（§8.5，跨国场景不再退化）、错峰上限改为代码钳制（§8.2）、首轮与版本过期不得映射为 low（§10.2）、Detail 走共享的 `Analyze()` 而不往 Snapshot 塞 `ProfilesJSON`（§10.3）、`sharing_stat.go` 补进改动清单（§16）、traffic 库表数由 4 更正为 5（新增后 6）。

另补三条仓库特有约束（评审未覆盖）：延迟首轮 goroutine 必须自带 `common.Recover`（CLAUDE.md 明确记录 cron 那层覆盖不到它，`PanelVersionJob` 至今没有）；状态 chip 的对比度下限；§8.5 引出的「弹窗里两个并存数字会打架」必须由 Risk Detail 统一供数解决。

**v3（2026-09-11）**，一处自查修正，触发于「为什么要取生产机数据」这个问题：

**v2 的 §3 把两个不同的需求混成了一个必须先实测的常数，并据此把「取生产数据」列为第 2 步的前置阻塞。拆开之后这个阻塞不存在：**

- **小时内分桶**（第 2 步）要挡的是 IPv6 privacy extensions，而按 RFC 8981 轮换的是**后 64 位的接口标识**，前缀不变——`/64` 由协议层面确定够用，不需要任何实测。前缀本身变（重拨）发生在小时之间，即便小时内发生，结果也只是数成 2 个而非合并，与现状完全一致、不会更差。
- **跨天的画像身份**（第 5 步）才真正需要经验数据，但它**不构成前置**：粗前缀可以从细前缀截出来（`/64` 的前 56 位就是 `/56`），所以聚合级别是分析层的纯函数常量，改它不需要重新采集或迁移数据。这个回标因此落到第 5 步之前，用面板自己积累的行做，与第 7 步的阈值回标同一件事、同一批数据。

由此连带删掉 `NetworkPrefix` 列（v2 的第 5 个新列）：它是 `ip` 列的纯函数，而 `ip` 已经在库里。存派生列没有收益且有害——聚合级别一旦调整，存下来的值当场变错且没有任何一层会报错。这正是 §4.1「不存 `ProfileID`，存原始特征」那条原则，v2 在 §3 没有贯彻。新列由 5 个减为 4 个，存储上限估算相应下调。

`/64` 作为画像层的保守默认也补进了 §13 能力边界：它可能把重拨过的用户拆成两个画像，但画像数量本身不计分，而重拨前后两个 `/64` 地理属性相同、距离为 0，不命中任何计分信号——代价是少一点信息，不是多一个误报。

**v4（2026-09-11）**，实施过程中发现的两处修正，均已随代码落地：

1. **`secondary_share` 与 `multi_geo_profile` 必须以「已有并存或错峰证据」为前置。** 写 §8.3 第 2 行（出差一周 → 0 分）的测试时发现：v3 的这两条信号没有任何前置，而一个出差一周的人第二个省天然承担四成流量、两个省都够「稳定」，于是拿到 15 分。它们描述的是**放大**已有嫌疑的事实，不是嫌疑本身。加上前置后六个标定场景全部落回预期区间。

2. **`degraded` 必须带「有有效数据」这个前提。** `geoCoverage` 对空输入返回 0，v3 只按 `< 50%` 判降级，于是一个刚建好、还没人连过的入站显示成「降级」而不是「学习中」。`geoCoverage` 改为同时返回有效行数。

另两条实施中确认的事实：

- **§17 第 3 步不需要写新代码。** `countabilityOf(inbound, platformSupported) (bool, string)` 已存在于 `web/service/online.go:643`，签名、注入 `platformSupported` 以便开发机可测的考虑、以及原因文案全都现成，在线明细列已经在用。本期直接复用，两处口径天然一致。§16 里把它标成「新增」是错的，已更正。
- **入站列表页不再调 `/sharing/summary`。** 并存统计已含在风险评估里，弹窗那一份来自 `risk/detail`。**接口本身按契约保留不动**，只是列表页不再需要它。

§17 第 7 步（生产观察 ≥2 周后回标阈值）尚未开始，也无法在开发环境完成。
