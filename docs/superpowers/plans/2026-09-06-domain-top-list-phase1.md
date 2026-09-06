# 域名 Top 榜单（第一期）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在访问日志弹窗里增加「Top 域名」tab，按用户 × 周期（1h/6h/12h/24h/7d/15d）给出访问次数最多的域名；同时把「自动刷新」的默认值改为打开。

**Architecture:** 新增一张与 `TrafficBucket` 同构的分时桶表 `DomainStat`（同库、同粒度常量、同时区对齐规则），由一个每 10 分钟的 job 从访问日志库增量聚合而来。第一期只写 `Count` 列，`Up`/`Down` 留给第二期填，两期共用同一张表和同一块 UI。

**Tech Stack:** Go 1.27 / GORM+SQLite / Gin / Vue 2 + ant-design-vue（服务端模板，无打包工具）/ `golang.org/x/net/publicsuffix`

**Spec:** `docs/superpowers/specs/2026-09-06-domain-traffic-attribution-design.md`

## Global Constraints

- **不新增第三方依赖。** `golang.org/x/net v0.57.0` 已是 `go.mod:19` 的直接依赖，`publicsuffix` 是它的子包。
- **不新增设置项。** 保留期复用现有的 `trafficHourRetentionDays`(默认 30) 与 `trafficDayRetentionDays`(默认 365)；聚合位点存独立单行表。新增设置项要同步改 5 处，漏掉 `models.js` 那处会让**整个保存配置接口失败**。
- **桶的 `BucketStart` 是 Unix 秒**（与 `model.TrafficBucket` 一致），而 `model.AccessLog.Time` 是**毫秒**，聚合时必须转换。
- **桶按面板设置的时区对齐**（`SettingService.GetTimeLocation()`，默认 `Asia/Shanghai`），用 `model.AlignHour` / `model.AlignDay`，不是 UTC。
- **清理条件必须带 `granularity`**，两级各有各的保留期。
- **删除入站必须连带删除它的行**：SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，榜单渲染得完全正常，只是画的是别人的数据。
- **新增 job 的 `Run` 首行必须 `defer common.Recover("<任务名>")`**（CLAUDE.md 明确要求）。
- **`web/html/**` 改完必须跑 `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot'`**：`getHtmlTemplate` 吞掉 `ParseFS` 错误，光靠 `go build` 发现不了模板语法错误。
- **访问日志弹窗有自己的 Vue 根实例**（`new Vue({el: '#access-log-modal'})`），新增的模板内容必须留在 `<a-modal id="access-log-modal">` 子树内，**不是 `#app`**。
- **提交步骤需要用户明确授权后才执行**（项目规范：未经授权不执行 commit）。仓库有并行会话，提交时必须**按路径 `git add`**，不要 `git add -A`。
- 全流程门禁：`make verify`（vet + test + build）。

---

### Task 1: 域名归并到注册域名

把 `www.speedtest.net:443` 这样的访问目标归并成 `speedtest.net`。这是聚合与榜单的基础，纯函数，先做。

**Files:**
- Create: `util/domain/domain.go`
- Create: `util/domain/domain_test.go`
- Modify: `util/accesslog/accesslog.go`（`hostOf` → 导出为 `HostOf`）

**Interfaces:**
- Consumes: 无
- Produces:
  - `accesslog.HostOf(addr string) (string, bool)` —— 原 `hostOf` 改名导出
  - `domain.Registrable(target string) string`

**为什么复用而不是各写一份 `hostOf`**：剥端口 + 剥 IPv6 方括号这段逻辑两处完全相同，重复实现意味着将来 IPv6 处理改一处漏一处，而这类 bug 完全静默（榜单里多出一个带方括号的"域名"，没有任何一层会报错）。

- [ ] **Step 1: 把 `hostOf` 改名导出**

`util/accesslog/accesslog.go`：把 `func hostOf(addr string) (string, bool)` 改成 `func HostOf(addr string) (string, bool)`，注释首行相应改为 `// HostOf 去掉地址末尾的端口，...`，并更新包内唯一的调用点 `sourceIP, ok := hostOf(sourceAddr)` → `HostOf(sourceAddr)`。

```bash
grep -rn "hostOf" util/accesslog/
```

确认改完之后没有残留的小写调用。

- [ ] **Step 2: 写归并的失败测试**

创建 `util/domain/domain_test.go`：

```go
package domain

import "testing"

// 期望值全部由 golang.org/x/net/publicsuffix v0.57.0 实测确认，不是推测。
func TestRegistrable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通三段域名", "www.speedtest.net:443", "speedtest.net"},
		{"多级子域名", "googleads.g.doubleclick.net:443", "doubleclick.net"},
		{"两段域名原样", "example.com:443", "example.com"},
		{"多级公共后缀", "example.co.uk:443", "example.co.uk"},
		{"github.io 是公共后缀", "a.b.c.example.github.io:443", "example.github.io"},
		{"大小写归一", "WWW.Example.COM:80", "example.com"},
		// 不剥尾点的话 EffectiveTLDPlusOne 报 "empty label"，会回落成
		// "example.com."，和 "example.com" 分裂成两个桶且没有任何报错。
		{"末尾点要剥掉", "example.com.:443", "example.com"},
		{"IPv4 字面量原样", "1.2.3.4:443", "1.2.3.4"},
		{"IPv6 字面量剥方括号", "[2001:db8::1]:443", "2001:db8::1"},
		{"本身就是公共后缀时原样", "com:443", "com"},
		{"无点主机名原样", "localhost:443", "localhost"},
		{"没有端口也要能处理", "example.com", "example.com"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Registrable(c.in); got != c.want {
				t.Errorf("Registrable(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 3: 运行测试确认它失败**

Run: `go test ./util/domain/ -run TestRegistrable -v`
Expected: FAIL —— `undefined: Registrable`（包还不存在，报的是构建错误）

- [ ] **Step 4: 实现 `Registrable`**

创建 `util/domain/domain.go`：

```go
// Package domain 把访问目标归并到注册域名（eTLD+1）。
//
// 独立成包而不是塞进 web/service：归并是纯函数，可以完整单测，
// 而且第二期的计量池同样要用它。
package domain

import (
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"

	"a-ui/util/accesslog"
)

// Registrable 把访问日志里的目标（host:port 形式）归并成注册域名。
//
// 归并到 eTLD+1 而不是保留完整域名，有三个理由：管理员的心智是「传到哪个
// 网站」而不是「传到哪台机器」；一屏广告子域名会收敛成两三个词；以及第二期
// 的计量规则用 domain:<注册域名> 一条就覆盖它的全部子域名。
//
// 三类值原样返回，都不丢弃——丢弃会让这部分流量在榜单上凭空消失，而管理员
// 看不出少了东西：
//   - IP 字面量目标（客户端直连 IP）。它们在第二期需要 ip 条件而不是 domain
//     条件，不参与计量，但访问次数照样要统计。
//   - 本身就是公共后缀的（"com"）：EffectiveTLDPlusOne 对它返回错误。
//   - 不含点的主机名（"localhost"）：同样返回错误。
func Registrable(target string) string {
	host, ok := accesslog.HostOf(target)
	if !ok || host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	// 转小写并剥掉末尾的点，两件事都必须做在调用 publicsuffix 之前：
	// 它对 "example.com." 报 empty label 而不是自动忽略，回落之后
	// "example.com." 与 "example.com" 会分裂成两个桶，没有任何一层会报错。
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || etld1 == "" {
		return host
	}
	return etld1
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./util/domain/ ./util/accesslog/ -v`
Expected: PASS（`util/accesslog` 的既有测试也必须仍然通过，改名不能改行为）

- [ ] **Step 6: 提交（需用户授权）**

```bash
git add util/domain/domain.go util/domain/domain_test.go util/accesslog/accesslog.go
git commit -m "feat(domain): 新增访问目标到注册域名的归并"
```

---

### Task 2: `DomainStat` 表与增量聚合

建表、建库迁移，并实现「从访问日志增量聚合成分时桶」这条写入路径。

**Files:**
- Create: `database/model/domain_stat.go`
- Modify: `database/db.go`（`InitTrafficDB` 内加两次 `AutoMigrate`，在 `model.InboundIPHour` 那次之后）
- Create: `web/service/domain_stat.go`
- Create: `web/service/domain_stat_test.go`

**Interfaces:**
- Consumes: `domain.Registrable(string) string`（Task 1）
- Produces:
  - `model.DomainStat`、`model.DomainStatCursor`
  - `service.DomainStatService`（无状态空结构体，按现有 service 惯例）
  - `(*DomainStatService).Aggregate(now time.Time) (int, error)` —— 返回本次消费的访问日志行数

- [ ] **Step 1: 写模型**

创建 `database/model/domain_stat.go`：

```go
package model

// DomainStat 是某个入站在某个时间桶内、对某个注册域名的访问统计，
// 存在**独立的 SQLite 库**里（与 TrafficBucket 同库，见 database.InitTrafficDB）。
//
// 分库的理由与 TrafficBucket 相同：高频写入不该和面板的普通操作抢主库
// 那把 SQLite 写锁。
//
// Count 由第一期的访问日志聚合写入；Up/Down 留给第二期的出站计量填，
// 在此之前恒为 0。两期共用一张表，第一期就把时区对齐、清理、孤儿清除
// 一次做对，第二期只补两列。
type DomainStat struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	// 复用 TrafficBucket 那套粒度常量，不另定义一套——两张表的清理都要
	// 按它套各自的保留期。
	Granularity TrafficGranularity `json:"-" gorm:"uniqueIndex:idx_domain_stat,priority:1"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变，存 tag 会让历史在改端口那一刻断掉。
	//
	// 相应地，删除入站时必须连带删掉它的行——SQLite 会复用被删除的自增 id，
	// 不删的话下一个建出来的入站会看到上一个用户访问过哪些网站，而且因为
	// 引用不再悬空，任何「跳过悬空引用」式的防线都拦不住它。
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_domain_stat,priority:2"`

	// Domain 是归并后的注册域名（util/domain.Registrable），IP 字面量原样。
	Domain string `json:"domain" gorm:"uniqueIndex:idx_domain_stat,priority:3"`

	// BucketStart 是桶起始时刻的 Unix **秒**，按面板设置的时区对齐（AlignHour）。
	// 注意 AccessLog.Time 是**毫秒**，聚合时要转换。
	BucketStart int64 `json:"t" gorm:"uniqueIndex:idx_domain_stat,priority:4"`

	Count int64 `json:"count"` // 连接次数
	Up    int64 `json:"up"`    // 上传字节，第二期填
	Down  int64 `json:"down"`  // 下载字节，第二期填
}

// DomainStatCursor 记「聚合任务上次读到访问日志的哪一条」，恒定单行（Id=1）。
//
// 不存进 settings：新增设置项要同步改 5 处（defaultValueMap / entity.AllSetting /
// entity.CheckValid / getter / models.js 的 AllSetting 构造函数），漏掉最后一处
// 会让整个保存配置接口失败，端口、证书路径一起遭殃。一个纯内部的位点不值得
// 付这个代价。
//
// 位点用 access_log 的自增 id 而不是时间戳：id 单调递增（AccessLogService.Query
// 本来就依赖这一点来保证翻页稳定），面板停机再久也只是补算，既不会重复计算
// 也不会跳过；按时间窗重算则需要「重算最近 N 小时」的启发式，停机超过 N 小时
// 就静默丢数据。
type DomainStatCursor struct {
	Id        int   `gorm:"primaryKey"`
	LastLogId int64
}
```

- [ ] **Step 2: 加库迁移**

`database/db.go` 的 `InitTrafficDB` 里，在 `AutoMigrate(&model.InboundIPHour{})` 之后、`trafficDB = tdb` 之前插入：

```go
	// 域名统计与用量桶同库：分库理由相同（高频写入不该抢主库那把写锁），
	// 而且两张表的清理挂在同一个每小时任务里。
	if err := tdb.AutoMigrate(&model.DomainStat{}); err != nil {
		return err
	}
	if err := tdb.AutoMigrate(&model.DomainStatCursor{}); err != nil {
		return err
	}
```

- [ ] **Step 3: 写聚合的失败测试**

创建 `web/service/domain_stat_test.go`：

```go
package service

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/accesslog"
)

// setupDomainStatTest 建三个全新的临时库：主库（入站）、访问日志库（聚合的
// 输入）、用量库（桶的去处）。三个句柄都是包级变量，每个测试重新 Init 一次
// 即可互不干扰。
func setupDomainStatTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitAccessLogDB(filepath.Join(dir, "access.db")); err != nil {
		t.Fatalf("InitAccessLogDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
}

// listDomainStats 返回某粒度下的全部行，顺序确定，便于逐行断言。
func listDomainStats(t *testing.T, g model.TrafficGranularity) []model.DomainStat {
	t.Helper()
	var rows []model.DomainStat
	err := database.GetTrafficDB().
		Where("granularity = ?", g).
		Order("bucket_start asc, inbound_id asc, domain asc").
		Find(&rows).Error
	if err != nil {
		t.Fatalf("查询域名统计: %v", err)
	}
	return rows
}

func TestAggregateWritesBothGranularities(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31001, "甲")

	// 面板时区默认 Asia/Shanghai：UTC 17:30 是当地次日 01:30，
	// 小时桶落在当地 01:00，日桶落在当地 00:00。
	at := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "www.speedtest.net:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "cdn.speedtest.net:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "s22.cnzz.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	n, err := (&DomainStatService{}).Aggregate(at)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 3 {
		t.Fatalf("消费了 %d 条，期望 3", n)
	}

	hours := listDomainStats(t, model.GranularityHour)
	if len(hours) != 2 {
		t.Fatalf("小时桶 %d 行，期望 2（两个注册域名）: %+v", len(hours), hours)
	}
	// 两个 speedtest.net 的子域名必须合并成一行，计数为 2。
	if hours[0].Domain != "cnzz.com" || hours[0].Count != 1 {
		t.Errorf("第一行 = %q/%d，期望 cnzz.com/1", hours[0].Domain, hours[0].Count)
	}
	if hours[1].Domain != "speedtest.net" || hours[1].Count != 2 {
		t.Errorf("第二行 = %q/%d，期望 speedtest.net/2", hours[1].Domain, hours[1].Count)
	}
	if hours[0].InboundId != in.Id {
		t.Errorf("inboundId = %d，期望 %d", hours[0].InboundId, in.Id)
	}

	days := listDomainStats(t, model.GranularityDay)
	if len(days) != 2 {
		t.Fatalf("日桶 %d 行，期望 2", len(days))
	}
	// 日桶独立累加，计数应与小时桶一致（同一天内只有这三条）。
	if days[1].Domain != "speedtest.net" || days[1].Count != 2 {
		t.Errorf("日桶 = %q/%d，期望 speedtest.net/2", days[1].Domain, days[1].Count)
	}
	// 桶按面板时区（Asia/Shanghai）对齐，不是 UTC。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if want := model.AlignHour(at, loc); hours[0].BucketStart != want {
		t.Errorf("小时桶起点 = %d，期望 %d（按 Asia/Shanghai 对齐）", hours[0].BucketStart, want)
	}
	if want := model.AlignDay(at, loc); days[0].BucketStart != want {
		t.Errorf("日桶起点 = %d，期望 %d", days[0].BucketStart, want)
	}
}

// 位点必须推进：再跑一次不能把同一批日志重复计入，否则每 10 分钟一轮，
// 一天下来榜单会虚高 144 倍，而且没有任何一层会报错。
func TestAggregateIsIdempotent(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31002, "甲")
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "example.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	svc := &DomainStatService{}
	if _, err := svc.Aggregate(at); err != nil {
		t.Fatalf("第一次 Aggregate: %v", err)
	}
	n, err := svc.Aggregate(at)
	if err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("第二次消费了 %d 条，期望 0（位点应已推进）", n)
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Errorf("重复聚合后 = %+v，期望恰好一行且 count=1", rows)
	}
}

// inbound_id = 0 的行（模板里 api 入站留下的、已删除入站留下的）不进榜单：
// 界面按入站查，落成 0 只会变成永远看不见的垃圾行。
func TestAggregateSkipsUnknownInbound(t *testing.T) {
	setupDomainStatTest(t)
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "example.com:443", Inbound: "api", Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := (&DomainStatService{}).Aggregate(at); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写入了 %d 行，期望 0: %+v", len(rows), rows)
	}
}
```

- [ ] **Step 4: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'TestAggregate' -v`
Expected: FAIL —— `undefined: DomainStatService`

- [ ] **Step 5: 实现聚合**

创建 `web/service/domain_stat.go`：

```go
package service

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/domain"
)

const (
	// domainStatBatchSize 是单轮从访问日志读取的行数上限，防止一次把大量
	// 数据读进内存。
	domainStatBatchSize = 20000

	// domainStatMaxRounds 是单次 Aggregate 最多连跑几轮。首次启用时库里
	// 可能已有几十万条积压，一轮两万行、十分钟一次的话要跑几个小时才追平，
	// 这期间榜单是残缺的；连跑到追平即可，20 轮（40 万行）的上限则防止
	// 单次调用长时间占住 CPU 与两个库。
	domainStatMaxRounds = 20
)

// DomainStatService 负责域名统计的聚合、清理与查询。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type DomainStatService struct {
	settingService SettingService
}

// Aggregate 把访问日志里位点之后的记录聚合成域名分时桶，返回本次消费的行数。
//
// 库不可用时静默返回 0：榜单不可用不该让调用方出错。
func (s *DomainStatService) Aggregate(now time.Time) (int, error) {
	tdb := database.GetTrafficDB()
	adb := database.GetAccessLogDB()
	if tdb == nil || adb == nil {
		return 0, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}
	tagToId, err := inboundTagToId()
	if err != nil {
		return 0, err
	}
	// 反过来用：访问日志里已经存了 inbound_id，这里只需要它是不是仍然有效。
	validId := make(map[int]bool, len(tagToId))
	for _, id := range tagToId {
		validId[id] = true
	}

	total := 0
	for round := 0; round < domainStatMaxRounds; round++ {
		cursor, err := loadDomainStatCursor(tdb)
		if err != nil {
			return total, err
		}
		var logs []model.AccessLog
		err = adb.Model(&model.AccessLog{}).
			Where("id > ?", cursor).
			Order("id asc").
			Limit(domainStatBatchSize).
			Find(&logs).Error
		if err != nil {
			return total, err
		}
		if len(logs) == 0 {
			return total, nil
		}

		// 先在内存里按 (粒度, 入站, 域名, 桶) 聚合，再逐条 UPSERT。
		// 同一轮里同一个键出现几百次是常态，不合并就是白写几百次。
		type key struct {
			g     model.TrafficGranularity
			id    int
			dom   string
			start int64
		}
		counts := make(map[key]int64, len(logs))
		maxId := cursor
		for i := range logs {
			row := &logs[i]
			if row.Id > maxId {
				maxId = row.Id
			}
			// inbound_id = 0 是写入时就没匹配上任何入站的记录（api 入站
			// 就是这样）；已被删除的入站同样跳过——它的桶马上要被清掉。
			if row.InboundId == 0 || !validId[row.InboundId] {
				continue
			}
			dom := domain.Registrable(row.Target)
			if dom == "" {
				continue
			}
			// AccessLog.Time 是毫秒，桶起点是 Unix 秒。
			at := time.UnixMilli(row.Time)
			counts[key{model.GranularityHour, row.InboundId, dom, model.AlignHour(at, loc)}]++
			counts[key{model.GranularityDay, row.InboundId, dom, model.AlignDay(at, loc)}]++
		}

		// 整轮包进一个事务：GORM 的 SkipDefaultTransaction 默认为 false，
		// 不包的话每条 UPSERT 自带一次 BEGIN/COMMIT，几百次提交对 SQLite
		// 是几百次 fsync。位点的推进也在同一个事务里——先写桶后推位点，
		// 中途失败则整轮不写，下一轮从原位点重来，不会丢也不会重。
		err = tdb.Transaction(func(tx *gorm.DB) error {
			for k, c := range counts {
				if err := upsertDomainStat(tx, k.g, k.id, k.dom, k.start, c); err != nil {
					return err
				}
			}
			return saveDomainStatCursor(tx, maxId)
		})
		if err != nil {
			return total, err
		}
		total += len(logs)

		// 没读满说明已经追平，不必再跑一轮。
		if len(logs) < domainStatBatchSize {
			return total, nil
		}
	}
	return total, nil
}

// upsertDomainStat 把次数累加进目标桶，桶不存在时创建。
//
// DoUpdates 用 gorm.Expr 做累加而不是覆盖：同一个桶在一小时内会被多轮聚合
// 写到，覆盖会让每个桶只剩最后一轮的量。
func upsertDomainStat(db *gorm.DB, g model.TrafficGranularity, inboundId int, dom string, start, count int64) error {
	row := &model.DomainStat{
		Granularity: g, InboundId: inboundId, Domain: dom, BucketStart: start, Count: count,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "granularity"}, {Name: "inbound_id"}, {Name: "domain"}, {Name: "bucket_start"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"count": gorm.Expr("domain_stats.count + ?", count),
		}),
	}).Create(row).Error
}

// loadDomainStatCursor 读位点，没有行时返回 0（从头开始）。
func loadDomainStatCursor(db *gorm.DB) (int64, error) {
	var c model.DomainStatCursor
	err := db.Where("id = ?", 1).First(&c).Error
	if err == gorm.ErrRecordNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return c.LastLogId, nil
}

func saveDomainStatCursor(db *gorm.DB, lastLogId int64) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_log_id"}),
	}).Create(&model.DomainStatCursor{Id: 1, LastLogId: lastLogId}).Error
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./web/service/ -run 'TestAggregate' -v`
Expected: PASS（三条全过）

- [ ] **Step 7: 提交（需用户授权）**

```bash
git add database/model/domain_stat.go database/db.go web/service/domain_stat.go web/service/domain_stat_test.go
git commit -m "feat(domain-stat): 新增域名分时桶与访问日志增量聚合"
```

---

### Task 3: Top 域名查询

**Files:**
- Modify: `web/service/domain_stat.go`（追加查询相关代码）
- Modify: `web/service/domain_stat_test.go`（追加测试）

**Interfaces:**
- Consumes: `model.DomainStat`、`upsertDomainStat`（Task 2）
- Produces:
  - `service.TopDomainRange` 及常量 `TopRange1h/6h/12h/24h/7d/15d`
  - `(*DomainStatService).TopDomains(inboundId int, r TopDomainRange, limit int, now time.Time) (*TopDomainResult, error)`
  - `service.TopDomainResult{ Metered bool; Range string; List []TopDomainRow }`
  - `service.TopDomainRow{ Domain string; Count, Up, Down int64 }`

- [ ] **Step 1: 写查询的失败测试**

追加到 `web/service/domain_stat_test.go`：

```go
func TestTopDomainsOrdersByCountWithinRange(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31003, "甲")
	other := mkTrafficInbound(t, 31004, "乙")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tdb := database.GetTrafficDB()
	// 三个域名落在最近 3 小时；一个落在 30 小时前（1h/6h 档不该看到它）。
	write := func(inboundId int, dom string, hoursAgo int, count int64) {
		t.Helper()
		start := model.AlignHour(now.Add(-time.Duration(hoursAgo)*time.Hour), loc)
		if err := upsertDomainStat(tdb, model.GranularityHour, inboundId, dom, start, count); err != nil {
			t.Fatalf("写桶: %v", err)
		}
	}
	write(in.Id, "speedtest.net", 0, 5)
	write(in.Id, "doubleclick.net", 1, 9)
	write(in.Id, "cnzz.com", 2, 7)
	write(in.Id, "old.example", 30, 100)
	write(other.Id, "notmine.com", 0, 999)

	svc := &DomainStatService{}
	res, err := svc.TopDomains(in.Id, TopRange6h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if res.Metered {
		t.Error("第一期不该声称有计量数据")
	}
	got := make([]string, 0, len(res.List))
	for _, r := range res.List {
		got = append(got, r.Domain)
	}
	want := []string{"doubleclick.net", "cnzz.com", "speedtest.net"}
	if len(got) != len(want) {
		t.Fatalf("返回 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("返回 %v，期望 %v（按次数降序）", got, want)
		}
	}
	if res.List[0].Count != 9 {
		t.Errorf("首位次数 = %d，期望 9", res.List[0].Count)
	}
}

// 1h 档只看当前这一个小时桶；6h 档要把更早的桶算进来。
// 边界算错的表征只是"数字偏小"，没有任何一层会报错。
func TestTopDomainsRespectsRangeBoundary(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31005, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	cur := model.AlignHour(now, loc)
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "a.com", cur, 1); err != nil {
		t.Fatal(err)
	}
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "b.com", cur-3600, 1); err != nil {
		t.Fatal(err)
	}

	svc := &DomainStatService{}
	res, err := svc.TopDomains(in.Id, TopRange1h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 1 || res.List[0].Domain != "a.com" {
		t.Errorf("1h 档返回 %+v，期望只有 a.com", res.List)
	}
	res, err = svc.TopDomains(in.Id, TopRange6h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 2 {
		t.Errorf("6h 档返回 %d 条，期望 2", len(res.List))
	}
}

// 7d / 15d 走日桶。走错粒度会把两级数据混在一起加倍计数。
func TestTopDomainsUsesDayBucketsForLongRanges(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31006, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	day := model.AlignDay(now, loc)
	if err := upsertDomainStat(tdb, model.GranularityDay, in.Id, "d.com", day, 42); err != nil {
		t.Fatal(err)
	}
	// 同一天的小时桶：如果查询粒度写错，这 7 会被一起加进来变成 49。
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "d.com", model.AlignHour(now, loc), 7); err != nil {
		t.Fatal(err)
	}

	res, err := (&DomainStatService{}).TopDomains(in.Id, TopRange7d, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 1 || res.List[0].Count != 42 {
		t.Errorf("返回 %+v，期望恰好一条 count=42", res.List)
	}
}

func TestTopDomainsRejectsUnknownRange(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31007, "甲")
	res, err := (&DomainStatService{}).TopDomains(in.Id, TopDomainRange("99h"), 10, time.Now())
	if err != nil {
		t.Fatalf("非法档位不该报错，应回落默认: %v", err)
	}
	if res.Range != string(TopRange24h) {
		t.Errorf("回落到 %q，期望 %q", res.Range, TopRange24h)
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'TestTopDomains' -v`
Expected: FAIL —— `undefined: TopRange6h`

- [ ] **Step 3: 实现查询**

追加到 `web/service/domain_stat.go`：

```go
// TopDomainRange 是榜单的时间档位。
type TopDomainRange string

const (
	TopRange1h  TopDomainRange = "1h"
	TopRange6h  TopDomainRange = "6h"
	TopRange12h TopDomainRange = "12h"
	TopRange24h TopDomainRange = "24h"
	TopRange7d  TopDomainRange = "7d"
	TopRange15d TopDomainRange = "15d"
)

// TopDomainRow 是榜单里的一行。
//
// Up/Down 在第一期恒为 0，前端靠 TopDomainResult.Metered 决定是否显示这两列——
// 显示一列恒为 0 的「上传」会被当成「他没上传过」，比不显示更糟。
type TopDomainRow struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
}

// TopDomainResult 是榜单接口的返回体。
type TopDomainResult struct {
	// Metered 为 false 表示这批数据只有访问次数，没有字节数。第二期上线后
	// 才为 true。
	Metered bool           `json:"metered"`
	Range   string         `json:"range"` // 实际生效的档位，前端据此回显
	Limit   int            `json:"limit"`
	List    []TopDomainRow `json:"list"`
}

// topRangeSpec 把档位翻译成（粒度, 回溯时长）。未知档位回落 24h——
// 这是个展示接口，一个拼错的参数不该变成报错弹窗。
func topRangeSpec(r TopDomainRange) (model.TrafficGranularity, time.Duration, TopDomainRange) {
	switch r {
	case TopRange1h:
		return model.GranularityHour, time.Hour, r
	case TopRange6h:
		return model.GranularityHour, 6 * time.Hour, r
	case TopRange12h:
		return model.GranularityHour, 12 * time.Hour, r
	case TopRange24h:
		return model.GranularityHour, 24 * time.Hour, r
	case TopRange7d:
		return model.GranularityDay, 7 * 24 * time.Hour, r
	case TopRange15d:
		return model.GranularityDay, 15 * 24 * time.Hour, r
	default:
		return model.GranularityHour, 24 * time.Hour, TopRange24h
	}
}

// TopDomains 返回某入站在给定档位内访问次数最多的域名。
//
// 起点按面板时区对齐后回溯，与用量图的刻度算法一致：不对齐的话，
// 「最近 24 小时」的起点会落在某个小时的中间，而桶是整点的，边界那一桶
// 要么整个漏掉要么整个算进来，取决于当前分钟数——同一个查询在一小时内
// 会给出两种结果。
func (s *DomainStatService) TopDomains(inboundId int, r TopDomainRange, limit int, now time.Time) (*TopDomainResult, error) {
	g, back, effective := topRangeSpec(r)
	if limit <= 0 {
		limit = 10
	}
	result := &TopDomainResult{
		Metered: false, // 第二期上线后改为真实的计量状态
		Range:   string(effective),
		Limit:   limit,
		List:    make([]TopDomainRow, 0, limit), // 不能给前端 null
	}
	db := database.GetTrafficDB()
	if db == nil {
		return result, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	var since int64
	if g == model.GranularityHour {
		since = model.AlignHour(now.Add(-back), loc)
	} else {
		since = model.AlignDay(now.Add(-back), loc)
	}

	var rows []TopDomainRow
	err = db.Model(&model.DomainStat{}).
		Select("domain, sum(count) as count, sum(up) as up, sum(down) as down").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Group("domain").
		// 次数相同时按域名字典序兜底，让同一份数据每次返回的顺序一致——
		// 顺序抖动会让自动刷新时榜单里的行无端跳动。
		Order("count desc, domain asc").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows != nil {
		result.List = rows
	}
	return result, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run 'TestTopDomains|TestAggregate' -v`
Expected: PASS

- [ ] **Step 5: 提交（需用户授权）**

```bash
git add web/service/domain_stat.go web/service/domain_stat_test.go
git commit -m "feat(domain-stat): 新增各周期 Top 域名查询"
```

---

### Task 4: 清理、孤儿清除与删除入站的连带清理

没有这一步，删掉的入站会把它的域名统计留给下一个复用同一个自增 id 的新入站。

**Files:**
- Modify: `web/service/domain_stat.go`
- Modify: `web/service/domain_stat_test.go`
- Modify: `web/service/inbound.go:158-160` 附近（`DelInbound` 内，用量历史那段之后）
- Modify: `web/job/traffic_cleanup_job.go`

**Interfaces:**
- Consumes: Task 2、Task 3 的全部
- Produces:
  - `(*DomainStatService).Cleanup(g model.TrafficGranularity, retentionDays int, now time.Time) (int64, error)`
  - `(*DomainStatService).PruneOrphans() (int64, error)`
  - `(*DomainStatService).DeleteByInbound(inboundId int) error`

- [ ] **Step 1: 写清理的失败测试**

追加到 `web/service/domain_stat_test.go`：

```go
// 清理条件必须带 granularity：不带的话一次「清理小时桶」会把同样早于该
// 时刻的日桶一起删掉，15 天榜单静默变空。
func TestCleanupOnlyTouchesGivenGranularity(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31008, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	old := now.Add(-60 * 24 * time.Hour)
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "a.com", model.AlignHour(old, loc), 1); err != nil {
		t.Fatal(err)
	}
	if err := upsertDomainStat(tdb, model.GranularityDay, in.Id, "a.com", model.AlignDay(old, loc), 1); err != nil {
		t.Fatal(err)
	}

	svc := &DomainStatService{}
	deleted, err := svc.Cleanup(model.GranularityHour, 30, now)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 1 {
		t.Errorf("删了 %d 行，期望 1", deleted)
	}
	if rows := listDomainStats(t, model.GranularityDay); len(rows) != 1 {
		t.Errorf("日桶被误删：剩 %d 行，期望 1", len(rows))
	}
}

// SQLite 会复用被删除的自增 id：不清掉的话，下一个建出来的入站会看到
// 上一个用户访问过哪些网站，而且引用不再悬空，任何跳过式的防线都拦不住。
func TestDeleteByInboundAndPruneOrphans(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31009, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "a.com", model.AlignHour(now, loc), 1); err != nil {
		t.Fatal(err)
	}
	// 一个库里根本不存在的入站 id：模拟 DelInbound 那次清理失败留下的残留。
	if err := upsertDomainStat(tdb, model.GranularityHour, 99999, "b.com", model.AlignHour(now, loc), 1); err != nil {
		t.Fatal(err)
	}

	svc := &DomainStatService{}
	if err := svc.DeleteByInbound(in.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].Domain != "b.com" {
		t.Fatalf("DeleteByInbound 后剩 %+v，期望只剩孤儿行 b.com", rows)
	}
	pruned, err := svc.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Errorf("清了 %d 行孤儿，期望 1", pruned)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("仍剩 %d 行", len(rows))
	}
}
```

- [ ] **Step 2: 运行测试确认它失败**

Run: `go test ./web/service/ -run 'TestCleanupOnlyTouches|TestDeleteByInboundAndPrune' -v`
Expected: FAIL —— `svc.Cleanup undefined`

- [ ] **Step 3: 实现三个方法**

追加到 `web/service/domain_stat.go`：

```go
// Cleanup 删除某一级中早于保留期的行，返回删除行数。
//
// 两级各有各的保留期，所以条件里必须带 granularity——不带的话，一次
// 「清理小时桶」会把同样早于该时刻的日桶一起删掉，长期榜单会静默变空。
func (s *DomainStatService) Cleanup(g model.TrafficGranularity, retentionDays int, now time.Time) (int64, error) {
	db := database.GetTrafficDB()
	if db == nil || retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	result := db.Where("granularity = ? and bucket_start < ?", g, cutoff).
		Delete(&model.DomainStat{})
	return result.RowsAffected, result.Error
}

// PruneOrphans 删除已不存在的入站遗留的行，返回删除行数。
//
// 第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，
// 那时引用不再悬空，榜单会渲染得非常合理，只是列的是别人访问过的网站。
func (s *DomainStatService) PruneOrphans() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.Inbound{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	tx := db.Where("inbound_id != 0")
	if len(ids) > 0 {
		tx = tx.Where("inbound_id not in ?", ids)
	}
	result := tx.Delete(&model.DomainStat{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部域名统计（两级都删）。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *DomainStatService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.DomainStat{}).Error
}
```

- [ ] **Step 4: 接进 `DelInbound`**

`web/service/inbound.go`，在清理用量历史那段（现为 155-160 行）之后、清理共享检测记录之前插入：

```go
	// 域名统计同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来
	// 的入站会看到上一个用户访问过哪些网站。失败只告警不阻断，理由同上，
	// 残留由每小时一次的 PruneOrphans 兜底。
	if err := (&DomainStatService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的域名统计失败, 将由定时清理兜底, id:", id, "err:", err)
	}
```

- [ ] **Step 5: 接进 `TrafficCleanupJob`**

`web/job/traffic_cleanup_job.go`：结构体加字段 `domainStatService service.DomainStatService`，并在 `trafficService.PruneOrphans()` 那段之后追加：

```go
	// 域名统计与用量桶同库、同粒度常量，因此共用同一份保留期设置，
	// 清理也挂在同一个任务里。
	if days, err := j.settingService.GetTrafficHourRetentionDays(); err != nil {
		logger.Warning("读取用量小时数据保留天数失败:", err)
	} else if deleted, err := j.domainStatService.Cleanup(model.GranularityHour, days, now); err != nil {
		logger.Warning("清理过期域名统计小时数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期域名统计小时数据", deleted)
	}

	if days, err := j.settingService.GetTrafficDayRetentionDays(); err != nil {
		logger.Warning("读取用量每日数据保留天数失败:", err)
	} else if deleted, err := j.domainStatService.Cleanup(model.GranularityDay, days, now); err != nil {
		logger.Warning("清理过期域名统计每日数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期域名统计每日数据", deleted)
	}

	if pruned, err := j.domainStatService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿域名统计失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的域名统计", pruned)
	}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./web/service/ -run 'TestCleanupOnlyTouches|TestDeleteByInboundAndPrune' -v && go vet ./web/...`
Expected: PASS，vet 无输出

- [ ] **Step 7: 提交（需用户授权）**

```bash
git add web/service/domain_stat.go web/service/domain_stat_test.go web/service/inbound.go web/job/traffic_cleanup_job.go
git commit -m "feat(domain-stat): 保留期清理、孤儿清除与删除入站连带清理"
```

---

### Task 5: 聚合定时任务

**Files:**
- Create: `web/job/domain_stat_job.go`
- Modify: `web/web.go`（`startTask` 内，`NewTrafficCleanupJob` 那行附近）

**Interfaces:**
- Consumes: `(*DomainStatService).Aggregate(time.Time) (int, error)`（Task 2）
- Produces: `job.NewDomainStatJob() *DomainStatJob`

- [ ] **Step 1: 写 job**

创建 `web/job/domain_stat_job.go`：

```go
package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// DomainStatJob 把访问日志增量聚合成域名分时桶。
//
// 10 分钟一轮而不是跟着 AccessLogCollectJob（5 秒）跑：榜单的最小档位是
// 1 小时，没有必要更频繁；而每一轮都要开事务写两级桶，跟着 5 秒跑等于
// 把写入放大几十倍去换一个没人看得出来的新鲜度。
type DomainStatJob struct {
	domainStatService service.DomainStatService
}

func NewDomainStatJob() *DomainStatJob {
	return new(DomainStatJob)
}

func (j *DomainStatJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层——日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("域名统计聚合任务")

	n, err := j.domainStatService.Aggregate(time.Now())
	if err != nil {
		logger.Warning("聚合域名统计失败:", err)
		return
	}
	if n > 0 {
		logger.Debugf("聚合了 %v 条访问日志到域名统计", n)
	}
}
```

- [ ] **Step 2: 注册到 cron**

`web/web.go` 的 `startTask` 内，在 `s.cron.AddJob("@every 1h", job.NewTrafficCleanupJob())` 那段之前插入：

```go
	// 域名统计聚合。见 DomainStatJob 里关于周期的说明。
	s.cron.AddJob("@every 10m", job.NewDomainStatJob())
```

- [ ] **Step 3: 编译并跑全量测试**

Run: `go build ./... && go test ./web/... ./util/...`
Expected: 全部 PASS

- [ ] **Step 4: 提交（需用户授权）**

```bash
git add web/job/domain_stat_job.go web/web.go
git commit -m "feat(domain-stat): 注册每 10 分钟的聚合任务"
```

---

### Task 6: 榜单接口

**Files:**
- Modify: `web/controller/inbound.go`（`initRouter` 加路由；文件末尾加 handler；结构体加 service 字段）

**Interfaces:**
- Consumes: `(*DomainStatService).TopDomains(...)`（Task 3）
- Produces: `POST /aui/inbound/topDomains/:id`

- [ ] **Step 1: 加 service 字段与路由**

`web/controller/inbound.go`：结构体（`accessLogService` 那一组字段，第 19 行附近）加：

```go
	domainStatService     service.DomainStatService
```

`initRouter` 里 `g.POST("/accessLogs/:id", a.getAccessLogs)` 之后加：

```go
	g.POST("/topDomains/:id", a.getTopDomains)
```

- [ ] **Step 2: 写 handler**

追加到 `web/controller/inbound.go`（放在 `getAccessLogs` 之后，与它相邻）：

```go
// getTopDomains 返回某入站在给定周期内访问次数最多的域名。
//
// 访问日志明细回答「他访问过什么」，这个接口回答「他主要在访问什么」——
// 一屏几十条同一个站的记录，翻页是看不出比重的。
func (a *InboundController) getTopDomains(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取域名榜单", err)
		return
	}
	// 与 getTrafficHistory 一样，前端 axios 实际发的是 JSON；form tag
	// 在两种绑定下都能工作，与既有接口保持一致。
	form := struct {
		Range string `form:"range"`
		Limit int    `form:"limit"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取域名榜单", err)
		return
	}
	// 上界在 controller 钳住——controller 是不可信输入的边界，请求体里一个
	// 失控的数字不该让服务端拉出远超所需的行。与 getTrafficOverview 对 top
	// 的钳制同源。
	if form.Limit <= 0 || form.Limit > 50 {
		form.Limit = 10
	}
	// 入站必须存在：不校验的话，一个不存在的 id 会返回一张空榜单，
	// 看起来像「这个人没访问过任何网站」。
	if _, err := a.inboundService.GetInbound(id); err != nil {
		jsonMsg(c, "获取域名榜单", err)
		return
	}
	result, err := a.domainStatService.TopDomains(id, service.TopDomainRange(form.Range), form.Limit, time.Now())
	if err != nil {
		jsonMsg(c, "获取域名榜单", err)
		return
	}
	jsonObj(c, result, nil)
}
```

- [ ] **Step 3: 编译并跑 vet**

Run: `go build ./... && go vet ./web/...`
Expected: 无错误、无输出

- [ ] **Step 4: 提交（需用户授权）**

```bash
git add web/controller/inbound.go
git commit -m "feat(domain-stat): 新增 Top 域名接口"
```

---

### Task 7: 前端 —— Top 域名 tab 与自动刷新默认打开

**Files:**
- Modify: `web/html/xui/access_log_modal.html`

**Interfaces:**
- Consumes: `POST /aui/inbound/topDomains/:id`（Task 6）
- Produces: 无（终端任务）

- [ ] **Step 1: 把现有内容包进 tabs**

这一步**只搬运、不改写**：当前文件里第 8~89 行（`<a-row :gutter="8" ...>` 筛选行、`近期来源` 的 `<div>`、`<a-table>` 整块，到 `</a-table>` 为止）原样保留，只是被包进一个 tab-pane，内容一个字符都不改。

在第 7 行 `</a-alert>` 之后插入：

```html
    <a-tabs :active-key="accessLogModal.tab" @change="k => accessLogModal.switchTab(k)">
        <a-tab-pane key="detail" tab="明细">
```

在第 89 行 `</a-table>` 之后插入：

```html
        </a-tab-pane>
```

（第二个 tab-pane 与 `</a-tabs>` 在 Step 2 补上，紧跟这个 `</a-tab-pane>`。）

用 `:active-key` 而不是 `v-model`：写入点只留 `switchTab` 一处，与本文件里 `a-switch :checked + @change` 的既有写法一致。两者并用的话 `tab` 会被 v-model 和 `switchTab` 各写一次，将来在 `switchTab` 里加逻辑很容易看漏其中一条路径。

**注意**：`a-tabs` 的非活动面板仍在 DOM 里，只是被隐藏——这不影响本任务，但将来写选择器时必须限定 `.ant-tabs-tabpane-active`。

- [ ] **Step 2: 写 Top 域名面板**

第二个 `a-tab-pane` 的内容：

```html
        <a-tab-pane key="top" tab="Top 域名">
            <div style="margin-bottom: 12px">
                <a-radio-group :value="accessLogModal.topRange" size="small" button-style="solid"
                               @change="e => accessLogModal.switchRange(e.target.value)">
                    <a-radio-button value="1h">1 小时</a-radio-button>
                    <a-radio-button value="6h">6 小时</a-radio-button>
                    <a-radio-button value="12h">12 小时</a-radio-button>
                    <a-radio-button value="24h">24 小时</a-radio-button>
                    <a-radio-button value="7d">7 天</a-radio-button>
                    <a-radio-button value="15d">15 天</a-radio-button>
                </a-radio-group>
                <span style="margin-left: 12px; color: rgba(0,0,0,.45)">
                    统计的是<b>连接次数</b>，不是流量——上传大文件往往只有很少几条长连接，未必排在前面。
                </span>
            </div>
            <a-table :columns="accessLogModal.topColumns" :data-source="accessLogModal.topList"
                     :row-key="row => row.domain" size="small" :loading="accessLogModal.topLoading"
                     :pagination="false"
                     :locale="{ emptyText: '这个周期内没有统计数据' }">
                <template slot="rank" slot-scope="text, row, index">
                    [[ index + 1 ]]
                </template>
                <template slot="domain" slot-scope="text, row">
                    <a style="cursor: pointer" @click="accessLogModal.drillDown(row.domain)">[[ row.domain ]]</a>
                </template>
            </a-table>
        </a-tab-pane>
    </a-tabs>
```

- [ ] **Step 3: 加 data 与 methods**

`const accessLogModal = { ... }` 里，在 `columns: [...]` 之后加：

```javascript
        // tab 与榜单状态。榜单与明细各自独立加载：切到榜单再拉，不给
        // 「只想看明细」的那次打开多发一个请求。
        tab: 'detail',
        topRange: '24h',
        topList: [],
        topLoading: false,
        topLoaded: false,
        topColumns: [{
            title: "#", align: 'center', width: 60,
            scopedSlots: { customRender: 'rank' },
        }, {
            title: "域名", align: 'left',
            scopedSlots: { customRender: 'domain' },
        }, {
            title: "访问次数", align: 'right', width: 120, dataIndex: "count",
        }],
```

`methods` 部分（对象里的函数），在 `toggleAutoRefresh` 之前加：

```javascript
        switchTab(key) {
            // tab 用 :active-key 单向绑定，这里是它唯一的写入点。
            this.tab = key;
            // 首次切到榜单才加载。已经加载过的不重复拉——档位没变时数据
            // 至多也就 10 分钟一变（聚合任务的周期）。
            if (key === 'top' && !this.topLoaded) {
                this.loadTop();
            }
        },
        switchRange(range) {
            this.topRange = range;
            this.loadTop();
        },
        async loadTop() {
            this.topLoading = true;
            const msg = await HttpUtil.post(`/aui/inbound/topDomains/${this.inboundId}`, {
                range: this.topRange,
                limit: 20,
            });
            this.topLoading = false;
            if (!msg.success) {
                return;
            }
            this.topList = msg.obj.list || [];
            this.topRange = msg.obj.range;
            this.topLoaded = true;
        },
        // 点榜单里的域名 = 切回明细并按它过滤。榜单回答「主要在访问什么」，
        // 明细回答「具体哪几次、从哪个 IP」，两步之间不该让管理员手工重打一遍。
        drillDown(domain) {
            this.tab = 'detail';
            this.key = domain;
            this.search();
        },
```

`show({ dbInbound, ip })` 里，在 `this.list = [];` 之后加（每次打开都是一个新入站，榜单状态必须重置，否则会显示上一个用户的榜单）：

```javascript
            this.tab = 'detail';
            this.topList = [];
            this.topLoaded = false;
```

- [ ] **Step 4: 自动刷新默认打开**

把 105-108 行整段替换为：

```javascript
        // 自动刷新默认开：这个弹窗的主要用法是盯「他现在在访问什么」，
        // 默认关意味着每次打开都要先点一下开关。复盘「他昨天访问过什么」
        // 时它确实是干扰，但那时把开关关掉即可，而且状态会跨弹窗保留。
        // 它本来就只在第一页生效（见 tick），翻页复盘时自动暂停。
        autoRefresh: true,
```

- [ ] **Step 5: 跑模板测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`
Expected: PASS

`getHtmlTemplate` 会吞掉 `ParseFS` 错误（`// ignore`），一个语法错误的模板要到渲染时才报 "template not found"，所以**光靠 `go build` 发现不了这里的问题**，这一步不能跳过。

- [ ] **Step 6: 人工验证（需要能跑面板的环境）**

```bash
XUI_DEBUG=true go run main.go
```

`XUI_DEBUG=true` 让模板从磁盘读取，改完不用重新编译。打开任一入站的访问日志，确认：自动刷新开关默认是开的；「Top 域名」tab 能切换、能切档位；点域名会切回明细并带上关键字过滤。

**注意**：本机若没有 `bin/xray-darwin-arm64`（它在 `.gitignore` 里），`RestartXray` 必然失败，但面板本身照常可访问，不影响本项目的验证。

- [ ] **Step 7: 全量门禁**

Run: `make verify`
Expected: vet 无输出、测试全过、构建成功

- [ ] **Step 8: 提交（需用户授权）**

```bash
git add web/html/xui/access_log_modal.html
git commit -m "feat(domain-stat): 访问日志弹窗新增 Top 域名 tab，自动刷新默认打开"
```

---

## 完成标准

- [ ] `make verify` 通过
- [ ] 访问日志弹窗打开即自动刷新
- [ ] 「Top 域名」tab 在 6 个档位下都能返回数据（有数据的档位）
- [ ] 点榜单里的域名能切回明细并按它过滤
- [ ] `git status` 干净，没有本次任务遗留的临时文件
- [ ] 最终 diff 里没有调试输出、没有越界修改

## 第二期入口

第一期完成后，第二期（真实字节数计量）的起点是 spec 的 §6。它会往 `DomainStat` 的 `Up`/`Down` 两列写数据，
并把 `TopDomainResult.Metered` 改成真实状态——前端的字节列与排序切换靠这个字段开关，
第一期的 UI 不需要为此重写。
