# 用量历史记录与图表 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给面板加两张分时用量图（单入站展开行内、系统状态页底部），并配套时序数据的采集、持久化与自动清理。

**Architecture:** 采集复用 `XrayTrafficJob` 每 10 秒已经拿到的 xray 增量，写进一个与主库物理分开的 SQLite 库；小时桶与日桶两级独立累加（不做汇总），按各自的保留期由每小时一次的清理任务删除。前端引入本地化的 Chart.js 4，服务端负责补零、对齐刻度与格式化标签，前端只管画。

**Tech Stack:** Go 1.27 / Gin 1.7.1 / GORM v1.21.9 + `gorm.io/driver/sqlite` v1.1.4（CGO 必需）/ Vue 2.6.12 + ant-design-vue 1.7.2 / Chart.js v4.5.0 UMD

**Spec:** `docs/superpowers/specs/2026-09-04-traffic-history-design.md`

## Global Constraints

- **CGO 必须开启**：`gorm.io/driver/sqlite` 依赖 `mattn/go-sqlite3`。所有 `go test` / `go build` 命令都要带 `CGO_ENABLED=1`。
- **提交前门禁**：`make verify`（= `go vet ./...` + `go test ./...` + `go build`），与 `.github/workflows/ci.yml` 一致。
- **`web/service` 包的测试工作目录是仓库根**（该包的 `TestMain` 会 `os.Chdir`）。新增测试若需要临时文件，一律用 `t.TempDir()` 或绝对路径，不要用包内相对路径。
- **不新增任何 Go 依赖。** `gorm.io/gorm/clause` 已在 `web/service/ipban.go` 中使用，直接用即可。
- **唯一的新第三方前端资源**：Chart.js v4.5.0 UMD，MIT，208,341 字节，sha256 `2f27bcf471b2d69dd78494f6e2172fb28470eb843820e2f96bb85d39f9618d30`。本地化到 `web/assets/`，不走 CDN。
- **前端模板用 `[[ ]]` 作为 Vue 插值分隔符**（避开 Go 模板的 `{{ }}`）。
- **Vue 指令（`v-*` / `@*` / `:*`）必须写在 `#app` 根元素内部**，写在外面是完全静默的死代码。`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条。
- **改动 `web/assets/**` 后**，浏览器强缓存靠 `?{{ .cur_ver }}` 失效，而 `cur_ver` 由 CI 从 git tag 写入 `config/version`。本地验证一律用 `XUI_DEBUG=true`（从磁盘读模板与静态资源，必须在仓库根目录启动）。
- **所有面向用户的文案用简体中文**，与现有界面一致。
- **新增的 cron 任务首行必须 `defer common.Recover("<任务名>")`**，与现有 job 保持一致。

---

## File Structure

**新建**

| 文件 | 职责 |
|---|---|
| `database/model/traffic.go` | `TrafficBucket` 表结构、`TrafficGranularity` 常量、`AlignHour` / `AlignDay` 两个纯函数 |
| `database/model/traffic_test.go` | 桶对齐的测试（纯函数，不碰数据库） |
| `web/service/traffic_history.go` | 采集写入、清理、查询聚合。本子系统唯一的业务逻辑文件 |
| `web/service/traffic_history_test.go` | 上者的测试 |
| `web/job/traffic_cleanup_job.go` | 每小时一次的清理任务 |
| `web/controller/traffic_test.go` | 两个 HTTP 接口的测试 |
| `web/assets/chart.js/chart.umd.min.js` | Chart.js v4.5.0 UMD（第三方，本地化） |

**修改**

| 文件 | 改什么 |
|---|---|
| `config/config.go` | 加 `GetTrafficDBPath()` |
| `database/db.go` | 加 `trafficDB` 变量、`InitTrafficDB()`、`GetTrafficDB()` |
| `web/web.go` | 初始化用量库 + 启动时 `PruneOrphans` + 注册清理任务 |
| `web/service/inbound.go` | `AddTraffic` 里记一份历史；`DelInbound` 里连带删除 |
| `web/service/setting.go` | `defaultValueMap` 两个默认值 + 两个 getter |
| `web/entity/entity.go` | `AllSetting` 两个字段 + `CheckValid` 两条校验 |
| `web/service/setting_baseline_test.go` | `validBaseSetting()` 补两个合法值 |
| `web/controller/inbound.go` | 两个路由 + 两个薄方法 |
| `web/assets/js/model/models.js` | `AllSetting` 构造函数两个同名字段 |
| `web/html/xui/setting.html` | 「用量图表」设置项 |
| `web/html/xui/inbounds.html` | 展开行内的单入站图 |
| `web/html/xui/index.html` | 系统状态页底部的全局图 |
| `CLAUDE.md` | 新增本子系统一节 + 修正两处过时描述 |

**为什么业务逻辑集中在一个 `traffic_history.go` 而不按层拆分**：采集、清理、查询共用同一套桶语义与同一个库句柄，拆开会让「桶怎么对齐」「零流量不写行」这类约束散落在三个文件里，改一处漏两处。该文件预计 300 行左右，与 `web/service/` 下的 `accesslog.go`、`routing_rule.go` 量级相当，符合现有惯例。

---

## Task 1: 数据模型、桶对齐与独立库

**Files:**
- Create: `database/model/traffic.go`
- Create: `database/model/traffic_test.go`
- Modify: `config/config.go`（在 `GetAccessLogDBPath` 之后，约 57 行）
- Modify: `database/db.go`（`accessDB` 声明旁 + `InitAccessLogDB` 之后）
- Modify: `web/web.go`（`InitAccessLogDB` 那一块之后，约 413 行）

**Interfaces:**
- Consumes: 无（本计划的第一个任务）
- Produces:
  - `model.TrafficBucket`（字段 `Id int64` / `Granularity model.TrafficGranularity` / `InboundId int` / `BucketStart int64` / `Up int64` / `Down int64`）
  - `model.TrafficGranularity`（`int8`）、常量 `model.GranularityHour = 1`、`model.GranularityDay = 2`
  - `model.AlignHour(t time.Time, loc *time.Location) int64`
  - `model.AlignDay(t time.Time, loc *time.Location) int64`
  - `config.GetTrafficDBPath() string`
  - `database.InitTrafficDB(dbPath string) error`
  - `database.GetTrafficDB() *gorm.DB`（未初始化成功时返回 nil）

- [ ] **Step 1: 写失败的测试 —— 桶对齐**

创建 `database/model/traffic_test.go`：

```go
package model

import (
	"testing"
	"time"
)

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestAlignHourTruncatesToHourStart(t *testing.T) {
	sh := mustLoadLocation(t, "Asia/Shanghai")
	got := AlignHour(time.Date(2026, 9, 4, 17, 43, 21, 500, sh), sh)
	want := time.Date(2026, 9, 4, 17, 0, 0, 0, sh).Unix()
	if got != want {
		t.Errorf("AlignHour = %d，期望 %d", got, want)
	}
}

func TestAlignHourIsIdempotentOnExactHour(t *testing.T) {
	sh := mustLoadLocation(t, "Asia/Shanghai")
	exact := time.Date(2026, 9, 4, 17, 0, 0, 0, sh)
	if got, want := AlignHour(exact, sh), exact.Unix(); got != want {
		t.Errorf("整点对齐后 = %d，期望不变 %d", got, want)
	}
}

func TestAlignDayUsesPanelTimezoneNotUTC(t *testing.T) {
	sh := mustLoadLocation(t, "Asia/Shanghai")
	// 当地 9 月 4 日 01:30。按 UTC 切日会落到 9 月 3 日，整整差一天——
	// 图上「9 月 4 日用了多少」会装进 9 月 3 日那根柱子里，且不报任何错。
	in := time.Date(2026, 9, 4, 1, 30, 0, 0, sh)
	got := AlignDay(in, sh)
	want := time.Date(2026, 9, 4, 0, 0, 0, 0, sh).Unix()
	if got != want {
		t.Errorf("AlignDay = %d，期望当地 9/4 00:00 即 %d", got, want)
	}
	if AlignDay(in, time.UTC) == got {
		t.Error("上海时区与 UTC 的日桶起点不该相同，这个测试没有真正区分时区")
	}
}

func TestAlignConvertsInputLocationFirst(t *testing.T) {
	sh := mustLoadLocation(t, "Asia/Shanghai")
	// 传进来的时刻可能是 UTC 的（容器里 time.Now() 常是 UTC）。对齐必须
	// 先换算到面板时区再取字段，直接读输入的年月日会错一整天。
	utcMoment := time.Date(2026, 9, 3, 17, 30, 0, 0, time.UTC) // = 上海 9/4 01:30
	want := time.Date(2026, 9, 4, 0, 0, 0, 0, sh).Unix()
	if got := AlignDay(utcMoment, sh); got != want {
		t.Errorf("AlignDay(UTC 输入) = %d，期望 %d", got, want)
	}
}

func TestAlignDayCrossesMonthBoundary(t *testing.T) {
	sh := mustLoadLocation(t, "Asia/Shanghai")
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, sh).Unix()
	if got := AlignDay(time.Date(2026, 10, 1, 0, 0, 1, 0, sh), sh); got != want {
		t.Errorf("AlignDay = %d，期望 %d", got, want)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
CGO_ENABLED=1 go test ./database/model/ -run TestAlign -v
```

期望：编译失败，`undefined: AlignHour` / `undefined: AlignDay`。

- [ ] **Step 3: 写实现**

创建 `database/model/traffic.go`：

```go
package model

import "time"

// TrafficGranularity 是用量桶的粒度。两级共用一张表，清理时按它套不同的
// 保留期：小时桶供近期看细节，日桶供长期看趋势。
type TrafficGranularity int8

const (
	GranularityHour TrafficGranularity = 1
	GranularityDay  TrafficGranularity = 2
)

// TrafficBucket 是某个入站在某个时间桶内的用量，存在**独立的 SQLite 库**里
//（见 database.InitTrafficDB）。
//
// 分库的理由与 AccessLog 相同：这张表每 10 秒写一次，清理时又是大批量
// DELETE，而 SQLite 一个库只有一把写锁——混在主库里会让面板的每一次普通
// 操作都去和它抢锁。
type TrafficBucket struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	Granularity TrafficGranularity `json:"-" gorm:"uniqueIndex:idx_traffic_bucket,priority:1"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变，存 tag 会让历史在改端口那一刻断掉。
	//
	// 相应地，删除入站时必须连带删掉它的桶——SQLite 会复用被删除的自增 id，
	// 不删的话下一个建出来的入站会看到上一个用户的曲线，而且因为引用不再
	// 悬空，任何「跳过悬空引用」式的防线都拦不住它。
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_traffic_bucket,priority:2"`

	// BucketStart 是桶起始时刻的 Unix 秒，按面板设置的时区对齐（见 AlignHour）。
	BucketStart int64 `json:"t" gorm:"uniqueIndex:idx_traffic_bucket,priority:3"`

	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// AlignHour 把时刻对齐到它所在小时的起点，返回 Unix 秒。
//
// 用面板设置的时区而不是 UTC。日桶尤其敏感：UTC+8 的管理员按 UTC 切日，
// 看到的「9 月 4 日用量」装的其实是 9 月 3 日 08:00 到 9 月 4 日 08:00 的
// 流量。这类错误不会报错，只会让人根据错的数据做判断。
func AlignHour(t time.Time, loc *time.Location) int64 {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, loc).Unix()
}

// AlignDay 把时刻对齐到它所在日的起点（当地 00:00:00），返回 Unix 秒。
func AlignDay(t time.Time, loc *time.Location) int64 {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc).Unix()
}
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
CGO_ENABLED=1 go test ./database/model/ -run TestAlign -v
```

期望：5 个测试全部 PASS。

- [ ] **Step 5: 加库路径**

在 `config/config.go` 的 `GetAccessLogDBPath()` 函数之后插入：

```go
// GetTrafficDBPath 是用量历史库的路径。与主库同目录但分文件，理由同访问
// 日志库：高频写入不该和面板的普通操作抢同一把写锁。
func GetTrafficDBPath() string {
	return fmt.Sprintf("/etc/%s/%s-traffic.db", GetName(), GetName())
}
```

- [ ] **Step 6: 加库初始化**

在 `database/db.go` 中，`accessDB` 的声明之后加：

```go
// trafficDB 是用量历史专用的库，与主库物理分开，原因见 model.TrafficBucket。
var trafficDB *gorm.DB
```

在 `InitAccessLogDB` 函数之后加：

```go
// InitTrafficDB 打开（必要时创建）用量历史库。
//
// 独立于 InitDB：即使这里失败，面板其余功能也必须照常可用——图表坏了不该
// 让人登不上面板，更不该影响计费用的累计流量。
func InitTrafficDB(dbPath string) error {
	dir := path.Dir(dbPath)
	if err := os.MkdirAll(dir, fs.ModeDir); err != nil {
		return err
	}
	var gormLogger logger.Interface
	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}
	tdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: gormLogger})
	if err != nil {
		return err
	}
	if err := tdb.AutoMigrate(&model.TrafficBucket{}); err != nil {
		return err
	}
	trafficDB = tdb
	return nil
}
```

在 `GetAccessLogDB` 之后加：

```go
// GetTrafficDB 返回用量历史库；未初始化成功时为 nil，调用方必须判空。
func GetTrafficDB() *gorm.DB {
	return trafficDB
}
```

- [ ] **Step 7: 在面板启动时初始化**

在 `web/web.go` 中，`InitAccessLogDB` 那一整块 `if / else if / else if` 之后、`s.startTask()` 之前插入：

```go
	// 用量历史同样用独立的库。打不开只影响图表，不影响累计流量与限额判定。
	if err := database.InitTrafficDB(config.GetTrafficDBPath()); err != nil {
		logger.Warning("open traffic history database failed, 用量图表将不可用:", err)
	}
```

- [ ] **Step 8: 全量验证**

```bash
CGO_ENABLED=1 go build -o /dev/null . && CGO_ENABLED=1 go vet ./config/ ./database/... ./web/
```

期望：无输出（构建与 vet 都通过）。

- [ ] **Step 9: 提交**

```bash
git add database/model/traffic.go database/model/traffic_test.go config/config.go database/db.go web/web.go
git commit -m "feat(traffic): 用量历史的表结构、桶对齐与独立库

桶按面板设置的时区对齐而不是 UTC：UTC+8 下按 UTC 切日，看到的
「某天用量」会整体错位 8 小时，且不报任何错。

库与主库物理分开，理由同访问日志：SQLite 一个库一把写锁，这张表
每 10 秒写一次，混进主库会和面板的每次普通操作抢锁。"
```

---

## Task 2: 采集写入

把 `XrayTrafficJob` 每 10 秒已经拿到的增量，在累加进 `inbounds.up/down` 之前先记一份分时历史。

**Files:**
- Create: `web/service/traffic_history.go`
- Create: `web/service/traffic_history_test.go`
- Modify: `web/service/inbound.go`（`AddTraffic`，约 216 行）

**Interfaces:**
- Consumes: `model.TrafficBucket` / `model.GranularityHour` / `model.GranularityDay` / `model.AlignHour` / `model.AlignDay` / `database.GetTrafficDB`（Task 1）；`inboundTagToId() (map[string]int, error)`（已存在于 `web/service/accesslog.go:150`，同包私有，直接调用）；`xray.Traffic{IsInbound bool, Tag string, Up int64, Down int64}`（已存在）
- Produces:
  - `TrafficHistoryService`（空结构体，内嵌 `settingService SettingService`，按值使用）
  - `(*TrafficHistoryService).Record(traffics []*xray.Traffic, now time.Time) error`
  - `upsertBucket(db *gorm.DB, g model.TrafficGranularity, inboundId int, start, up, down int64) error`（包内私有）

- [ ] **Step 1: 写失败的测试**

创建 `web/service/traffic_history_test.go`：

```go
package service

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// setupTrafficTest 建一对全新的临时库。两个库句柄都是包级变量，
// 每个测试重新 Init 一次即可互不干扰。
func setupTrafficTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
}

func mkTrafficInbound(t *testing.T, port int, remark string) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS,
		Tag:      fmt.Sprintf("inbound-%v", port),
		Remark:   remark,
		Enable:   true,
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("创建入站: %v", err)
	}
	return in
}

// countBuckets 返回某粒度下的全部桶，按 bucket_start 升序。
func listBuckets(t *testing.T, g model.TrafficGranularity) []model.TrafficBucket {
	t.Helper()
	var rows []model.TrafficBucket
	err := database.GetTrafficDB().
		Where("granularity = ?", g).
		Order("bucket_start asc, inbound_id asc").
		Find(&rows).Error
	if err != nil {
		t.Fatalf("查询桶: %v", err)
	}
	return rows
}

func TestRecordWritesBothGranularities(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30001, "甲")
	svc := TrafficHistoryService{}
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)

	err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 100, Down: 900},
	}, now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	hours := listBuckets(t, model.GranularityHour)
	if len(hours) != 1 {
		t.Fatalf("小时桶行数 = %d，期望 1", len(hours))
	}
	if hours[0].Up != 100 || hours[0].Down != 900 || hours[0].InboundId != in.Id {
		t.Errorf("小时桶 = %+v，期望 up=100 down=900 inboundId=%d", hours[0], in.Id)
	}
	days := listBuckets(t, model.GranularityDay)
	if len(days) != 1 {
		t.Fatalf("日桶行数 = %d，期望 1（日桶独立累加，不依赖后续汇总）", len(days))
	}
	if days[0].Up != 100 || days[0].Down != 900 {
		t.Errorf("日桶 = %+v，期望 up=100 down=900", days[0])
	}
}

func TestRecordAccumulatesWithinSameBucket(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30002, "甲")
	svc := TrafficHistoryService{}
	base := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)

	for i, d := range []int64{10, 20, 30} {
		// 同一小时内的三轮采样，间隔 10 秒，与真实的 XrayTrafficJob 一致。
		at := base.Add(time.Duration(i) * 10 * time.Second)
		if err := svc.Record([]*xray.Traffic{
			{IsInbound: true, Tag: in.Tag, Up: d, Down: d * 2},
		}, at); err != nil {
			t.Fatalf("Record 第 %d 轮: %v", i, err)
		}
	}

	hours := listBuckets(t, model.GranularityHour)
	if len(hours) != 1 {
		t.Fatalf("小时桶行数 = %d，期望 1（同一小时应该 UPSERT 累加，不是插新行）", len(hours))
	}
	if hours[0].Up != 60 || hours[0].Down != 120 {
		t.Errorf("累加结果 = up %d / down %d，期望 60 / 120", hours[0].Up, hours[0].Down)
	}
}

func TestRecordSkipsZeroDelta(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30003, "甲")
	svc := TrafficHistoryService{}

	err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 0, Down: 0},
	}, time.Now())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	// 挂机用户大部分小时没有任何流量。存 0 行只是把磁盘填满，
	// 图上的 0 由前端补零画出来。
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("零增量写了 %d 行，期望一行都不写", len(rows))
	}
}

func TestRecordIgnoresOutboundAndUnknownTags(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30004, "甲")
	svc := TrafficHistoryService{}

	err := svc.Record([]*xray.Traffic{
		{IsInbound: false, Tag: in.Tag, Up: 500, Down: 500},   // 出站，不是本子系统的维度
		{IsInbound: true, Tag: "api", Up: 500, Down: 500},     // 模板里的 api 入站，库里没有
		{IsInbound: true, Tag: "inbound-59999", Up: 7, Down: 8}, // 已删除的入站
	}, time.Now())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	// 落成 inbound_id=0 只会在图上多出一条没人认领的曲线。
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写了 %d 行，期望全部忽略：%+v", len(rows), rows)
	}
	_ = in
}

func TestRecordMergesDuplicateTagsInOneRound(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30005, "甲")
	svc := TrafficHistoryService{}

	err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 1, Down: 2},
		{IsInbound: true, Tag: in.Tag, Up: 3, Down: 4},
	}, time.Now())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	hours := listBuckets(t, model.GranularityHour)
	if len(hours) != 1 {
		t.Fatalf("小时桶行数 = %d，期望 1", len(hours))
	}
	if hours[0].Up != 4 || hours[0].Down != 6 {
		t.Errorf("合并结果 = up %d / down %d，期望 4 / 6", hours[0].Up, hours[0].Down)
	}
}

func TestRecordIsNoOpWhenDatabaseUnavailable(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30006, "甲")
	// 模拟库没打开：面板启动时 InitTrafficDB 失败就是这个状态。
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "x.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	database.ResetTrafficDBForTest()

	svc := TrafficHistoryService{}
	if err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 1, Down: 1},
	}, time.Now()); err != nil {
		t.Errorf("库不可用时 Record 应静默返回 nil，实际返回 %v", err)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
CGO_ENABLED=1 go test ./web/service/ -run TestRecord -v
```

期望：编译失败，`undefined: TrafficHistoryService`、`undefined: database.ResetTrafficDBForTest`。

- [ ] **Step 3: 加测试用的库重置钩子**

最后一个测试要验证「库没打开」这条分支。在 `database/db.go` 末尾加：

```go
// ResetTrafficDBForTest 把用量库句柄清空，用于测试「库没打开」这条分支。
// 生产代码不调用它——面板启动时 InitTrafficDB 失败就是这个状态，而那条
// 分支上的每一个调用方都必须判空，不能靠运气。
func ResetTrafficDBForTest() {
	trafficDB = nil
}
```

- [ ] **Step 4: 写实现**

创建 `web/service/traffic_history.go`：

```go
package service

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// TrafficHistoryService 负责用量历史的采集、清理与查询。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type TrafficHistoryService struct {
	settingService SettingService
}

// Record 把一轮流量增量写进用量历史库。
//
// 传进来的是 XrayTrafficJob 每 10 秒从 xray gRPC Stats 取回的**增量**
//（reset=true，取完 xray 侧清零），因此恒为非负：xray 重启后计数从 0
// 重新开始，不会出现负值。这也是不走「读累计值做差分」那条路的原因——
// 差分方案里，一次正常的「重置流量」和一次数据损坏长得一模一样。
//
// 库不可用时静默返回 nil：图表不可用不该让调用方出错，而调用方
//（InboundService.AddTraffic）承担的是计费用的累计流量。
func (s *TrafficHistoryService) Record(traffics []*xray.Traffic, now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil || len(traffics) == 0 {
		return nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	tagToId, err := inboundTagToId()
	if err != nil {
		return err
	}

	// 先按入站聚合。同一轮里同一个 tag 出现多次时应当相加，而不是发两次
	// UPSERT——结果虽然一样，但白白多一倍写入。
	type delta struct{ up, down int64 }
	deltas := make(map[int]*delta, len(traffics))
	for _, t := range traffics {
		if !t.IsInbound {
			continue
		}
		// 零增量不写行：挂机用户大部分小时没有任何流量，跳过它们能砍掉
		// 一多半行数。图上的 0 由前端补零画出来，而补零逻辑无论如何都要
		// 有——新建的入站在它存在之前同样没有行。
		if t.Up == 0 && t.Down == 0 {
			continue
		}
		// 找不到对应入站的 tag（模板里的 api 入站、已删除的入站）直接丢弃。
		// 落成 inbound_id=0 只会在图上多出一条没人认领的曲线。
		id, ok := tagToId[t.Tag]
		if !ok || id == 0 {
			continue
		}
		d := deltas[id]
		if d == nil {
			d = &delta{}
			deltas[id] = d
		}
		d.up += t.Up
		d.down += t.Down
	}
	if len(deltas) == 0 {
		return nil
	}

	hour := model.AlignHour(now, loc)
	day := model.AlignDay(now, loc)
	for id, d := range deltas {
		if err := upsertBucket(db, model.GranularityHour, id, hour, d.up, d.down); err != nil {
			return err
		}
		// 日桶独立累加，不由小时桶汇总而来：汇总方案要处理「小时桶已被
		// 清理但日桶还没算」的补算逻辑，独立累加天生免疫。日桶一年才
		// 365 行，多一次 UPSERT 的代价可以忽略。
		if err := upsertBucket(db, model.GranularityDay, id, day, d.up, d.down); err != nil {
			return err
		}
	}
	return nil
}

// upsertBucket 把增量累加进目标桶，桶不存在时创建。
//
// DoUpdates 用 gorm.Expr 做累加而不是 clause.AssignmentColumns（那是覆盖）：
// 同一个桶一小时会被写 360 次，覆盖会让每个桶只剩最后 10 秒的量。
func upsertBucket(db *gorm.DB, g model.TrafficGranularity, inboundId int, start, up, down int64) error {
	bucket := &model.TrafficBucket{
		Granularity: g, InboundId: inboundId, BucketStart: start, Up: up, Down: down,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "granularity"}, {Name: "inbound_id"}, {Name: "bucket_start"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"up":   gorm.Expr("traffic_buckets.up + ?", up),
			"down": gorm.Expr("traffic_buckets.down + ?", down),
		}),
	}).Create(bucket).Error
}
```

- [ ] **Step 5: 运行测试，确认通过**

```bash
CGO_ENABLED=1 go test ./web/service/ -run TestRecord -v
```

期望：6 个测试全部 PASS。

- [ ] **Step 6: 接进 AddTraffic**

在 `web/service/inbound.go` 的 `AddTraffic` 中，`if len(traffics) == 0 { return nil }` 之后、`db := database.GetDB()` 之前插入：

```go
	// 先记一份分时历史，再走累加。两者写的是不同的库，主库的事务包不住
	// 时序库的写入，所以不放进同一个事务——硬凑只会得到一个原子性的假象。
	//
	// 失败只告警不阻断：inbounds.up/down 是限额与到期判定的输入，它停止
	// 累加的后果（用户超额不被停用）比图上少一段曲线严重得多。
	if err := (&TrafficHistoryService{}).Record(traffics, time.Now()); err != nil {
		logger.Warning("记录用量历史失败:", err)
	}
```

`time` 与 `logger` 都已在该文件的 import 中，无需改动 import。

- [ ] **Step 7: 验证整个 service 包没被打破**

```bash
CGO_ENABLED=1 go test ./web/service/ 2>&1 | tail -5
```

期望：`ok  a-ui/web/service`。

- [ ] **Step 8: 提交**

```bash
git add web/service/traffic_history.go web/service/traffic_history_test.go web/service/inbound.go database/db.go
git commit -m "feat(traffic): 采集分时用量

复用 XrayTrafficJob 每 10 秒已经拿到的增量，不另做差分——差分方案
里一次正常的重置流量和一次数据损坏长得一模一样。

不与 AddTraffic 共用事务：两者写的是不同的库，硬凑只会得到一个
原子性的假象。写失败只告警，因为累计流量是限额判定的输入。"
```

---

## Task 3: 保留期设置项

按 `CLAUDE.md` 记的五步流程加两个设置项。任何一步漏掉都会造成静默故障，其中前端那步有测试守着。

**Files:**
- Modify: `web/service/setting.go`（`defaultValueMap` 约 39 行；getter 加在 `GetAccessLogRetentionDays` 之后，约 471 行）
- Modify: `web/entity/entity.go`（`AllSetting` 约 48 行；`CheckValid` 约 210 行）
- Modify: `web/service/setting_baseline_test.go`（`validBaseSetting()`）
- Modify: `web/assets/js/model/models.js`（`AllSetting` 构造函数，约 195 行）
- Modify: `web/html/xui/setting.html`（约 118-127 行的「访问日志」tab 之后）

**Interfaces:**
- Consumes: 无
- Produces:
  - `(*SettingService).GetTrafficHourRetentionDays() (int, error)`
  - `(*SettingService).GetTrafficDayRetentionDays() (int, error)`
  - `entity.AllSetting` 新字段 `TrafficHourRetentionDays int` / `TrafficDayRetentionDays int`（json/form tag 分别为 `trafficHourRetentionDays` / `trafficDayRetentionDays`）

- [ ] **Step 1: 写失败的测试**

在 `web/service/traffic_history_test.go` 末尾追加：

```go
func TestTrafficRetentionDefaults(t *testing.T) {
	setupTrafficTest(t)
	svc := SettingService{}

	// 默认值直接影响磁盘占用与图能拉多远，改动要有意识。
	if got, err := svc.GetTrafficHourRetentionDays(); err != nil || got != 30 {
		t.Errorf("小时桶保留天数默认 = %d (err %v)，期望 30", got, err)
	}
	if got, err := svc.GetTrafficDayRetentionDays(); err != nil || got != 365 {
		t.Errorf("日桶保留天数默认 = %d (err %v)，期望 365", got, err)
	}
}
```

在 `web/entity/` 下新建 `web/entity/traffic_setting_test.go`：

```go
package entity

import "testing"

func TestCheckValidRejectsOutOfRangeTrafficRetention(t *testing.T) {
	cases := []struct {
		name  string
		hour  int
		day   int
		valid bool
	}{
		{"默认值", 30, 365, true},
		{"下界", 1, 1, true},
		{"上界", 365, 3650, true},
		{"小时桶为 0", 0, 365, false},
		{"小时桶超上界", 366, 365, false},
		{"日桶为 0", 30, 0, false},
		{"日桶超上界", 30, 3651, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validBaseSettingForEntityTest()
			s.TrafficHourRetentionDays = c.hour
			s.TrafficDayRetentionDays = c.day
			err := s.CheckValid()
			if c.valid && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
			if !c.valid && err == nil {
				t.Error("期望被拒绝，实际通过了")
			}
		})
	}
}

// validBaseSettingForEntityTest 与 service 包的 validBaseSetting 同一用意：
// CheckValid 是逐条串行校验的，前面任何一项不合法都会让后面的项根本走不到。
func validBaseSettingForEntityTest() *AllSetting {
	return &AllSetting{
		WebPort:                  54321,
		WebBasePath:              "/",
		TimeLocation:             "Asia/Shanghai",
		XrayTemplateConfig:       "{}",
		SubscriptionUpdateTime:   "04:00",
		IPDBSourceUrl:            "https://example.com/ipv4_source.txt",
		QQWrySourceUrl:           "https://example.com/qqwry.dat",
		IPDBUpdateTime:           "",
		AccessLogEnable:          0,
		AccessLogRetentionDays:   7,
		TCInterface:              "",
		TrafficHourRetentionDays: 30,
		TrafficDayRetentionDays:  365,
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
CGO_ENABLED=1 go test ./web/entity/ ./web/service/ -run "TrafficRetention|CheckValidRejectsOutOfRange" -v 2>&1 | tail -20
```

期望：编译失败，`unknown field TrafficHourRetentionDays`、`undefined: GetTrafficHourRetentionDays`。

- [ ] **Step 3: 加默认值（第 1 步）**

在 `web/service/setting.go` 的 `defaultValueMap` 中，`"accessLogRetentionDays": "7",` 之后加：

```go
	"trafficHourRetentionDays": "30",
	"trafficDayRetentionDays":  "365",
```

- [ ] **Step 4: 加实体字段（第 2 步）**

在 `web/entity/entity.go` 的 `AllSetting` 中，`AccessLogRetentionDays` 那一行之后加：

```go
	TrafficHourRetentionDays int `json:"trafficHourRetentionDays" form:"trafficHourRetentionDays"`
	TrafficDayRetentionDays  int `json:"trafficDayRetentionDays" form:"trafficDayRetentionDays"`
```

- [ ] **Step 5: 加校验（第 3 步）**

在 `web/entity/entity.go` 的 `CheckValid` 中，`AccessLogRetentionDays` 那条校验之后加：

```go
	// 小时桶是图上「近期看细节」的那一级，行数随天数线性增长；日桶一年
	// 才 365 行，上界给得宽。两者都不允许为 0：0 会让清理任务把全部历史
	// 一次删光，而这不是任何人想通过「填 0」表达的意思。
	if s.TrafficHourRetentionDays < 1 || s.TrafficHourRetentionDays > 365 {
		return common.NewError("用量小时数据保留天数应在 1 ~ 365 天之间:", s.TrafficHourRetentionDays)
	}
	if s.TrafficDayRetentionDays < 1 || s.TrafficDayRetentionDays > 3650 {
		return common.NewError("用量每日数据保留天数应在 1 ~ 3650 天之间:", s.TrafficDayRetentionDays)
	}
```

- [ ] **Step 6: 加 getter（第 4 步）**

在 `web/service/setting.go` 的 `GetAccessLogRetentionDays` 之后加：

```go
// GetTrafficHourRetentionDays 返回用量小时桶的保留天数。
func (s *SettingService) GetTrafficHourRetentionDays() (int, error) {
	return s.getInt("trafficHourRetentionDays")
}

// GetTrafficDayRetentionDays 返回用量日桶的保留天数。
func (s *SettingService) GetTrafficDayRetentionDays() (int, error) {
	return s.getInt("trafficDayRetentionDays")
}
```

- [ ] **Step 7: 补测试基线**

在 `web/service/setting_baseline_test.go` 的 `validBaseSetting()` 返回值里加两行：

```go
		TrafficHourRetentionDays: 30,
		TrafficDayRetentionDays:  365,
```

不补的话，所有针对单个字段的校验测试会一起失效，报错还指向一个与它们无关的字段——该文件的注释里写着这个坑已经踩过三次。

- [ ] **Step 8: 加前端模型字段（第 5 步）**

在 `web/assets/js/model/models.js` 的 `AllSetting` 构造函数中，`this.accessLogRetentionDays = 7;` 之后加：

```javascript
        this.trafficHourRetentionDays = 30;
        this.trafficDayRetentionDays = 365;
```

这一步不是可省的收尾工作。`ObjectUtil.cloneProps` 只克隆目标对象已经拥有的 key，漏掉的话服务端返回值会被直接丢弃、输入框永远显示硬编码初始值；而 `updateAllSetting` 提交的正是这个 JS 对象，新字段在提交体里根本不存在，Gin 绑定成零值，被上面那条「不允许为 0」的校验拒绝——**整个保存配置接口都会失败**，端口、证书路径等一切无关字段一起遭殃，报错还只指向新字段。

- [ ] **Step 9: 加设置页界面**

在 `web/html/xui/setting.html` 中，`key="5"` 的「访问日志」`</a-tab-pane>` 之后插入一个新 tab（沿用现有的 `setting-list-item` 组件；key 取 `9`，现有已用到 8）：

```html
                        <a-tab-pane key="9" tab="用量图表">
                            <a-list item-layout="horizontal" style="background: white">
                                <setting-list-item type="number" title="小时数据保留天数"
                                                   desc="入站列表与系统状态页的用量图，「24 小时 / 7 天 / 30 天」三档用的就是这一级数据。超过该天数的记录每小时自动清除。可填 1 ~ 365，默认 30"
                                                   v-model.number="allSetting.trafficHourRetentionDays"></setting-list-item>
                                <setting-list-item type="number" title="每日数据保留天数"
                                                   desc="用量图「1 年」这一档用的数据，一个入站一年只占 365 行。可填 1 ~ 3650，默认 365"
                                                   v-model.number="allSetting.trafficDayRetentionDays"></setting-list-item>
                            </a-list>
                        </a-tab-pane>
```

- [ ] **Step 10: 运行测试，确认通过**

```bash
CGO_ENABLED=1 go test ./web/entity/ ./web/service/ ./web/ -run "TrafficRetention|CheckValidRejectsOutOfRange|TestAllSettingFieldsExistInFrontendModel|TestAllTemplatesParse" -v 2>&1 | tail -30
```

期望：全部 PASS。`TestAllSettingFieldsExistInFrontendModel` 会自动验证第 5 步没漏，`TestAllTemplatesParse` 会验证第 9 步的模板没写坏。

- [ ] **Step 11: 提交**

```bash
git add web/service/setting.go web/entity/entity.go web/entity/traffic_setting_test.go \
        web/service/setting_baseline_test.go web/service/traffic_history_test.go \
        web/assets/js/model/models.js web/html/xui/setting.html
git commit -m "feat(traffic): 用量数据保留天数设置项

小时桶默认 30 天、日桶默认 365 天，均可在面板设置页调整。
两者都不允许填 0：0 会让清理任务把全部历史一次删光，而这不是
任何人想通过填 0 表达的意思。"
```

---

## Task 4: 清理、孤儿清理与定时任务

**Files:**
- Modify: `web/service/traffic_history.go`（追加三个方法）
- Modify: `web/service/traffic_history_test.go`（追加测试）
- Create: `web/job/traffic_cleanup_job.go`
- Modify: `web/service/inbound.go`（`DelInbound`，约 148-151 行之后）
- Modify: `web/web.go`（`Server` 结构体加字段；`InitTrafficDB` 那块加 `PruneOrphans`；`startTask` 注册任务）

**Interfaces:**
- Consumes: `(*SettingService).GetTrafficHourRetentionDays` / `GetTrafficDayRetentionDays`（Task 3）；`model.GranularityHour` / `GranularityDay`（Task 1）
- Produces:
  - `(*TrafficHistoryService).Cleanup(g model.TrafficGranularity, retentionDays int, now time.Time) (int64, error)`
  - `(*TrafficHistoryService).PruneOrphans() (int64, error)`
  - `(*TrafficHistoryService).DeleteByInbound(inboundId int) error`
  - `job.NewTrafficCleanupJob() *job.TrafficCleanupJob`

- [ ] **Step 1: 写失败的测试**

在 `web/service/traffic_history_test.go` 末尾追加：

```go
// writeBucket 直接往库里塞一个桶，用于构造清理与查询测试的初始状态。
func writeBucket(t *testing.T, g model.TrafficGranularity, inboundId int, start, up, down int64) {
	t.Helper()
	row := &model.TrafficBucket{
		Granularity: g, InboundId: inboundId, BucketStart: start, Up: up, Down: down,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入桶: %v", err)
	}
}

func TestCleanupAppliesRetentionPerGranularity(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30101, "甲")
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := TrafficHistoryService{}

	old := now.Add(-40 * 24 * time.Hour).Unix()   // 40 天前
	fresh := now.Add(-10 * 24 * time.Hour).Unix() // 10 天前
	for _, g := range []model.TrafficGranularity{model.GranularityHour, model.GranularityDay} {
		writeBucket(t, g, in.Id, old, 1, 1)
		writeBucket(t, g, in.Id, fresh, 2, 2)
	}

	// 小时桶保留 30 天：40 天前的该删，10 天前的该留。
	deleted, err := svc.Cleanup(model.GranularityHour, 30, now)
	if err != nil {
		t.Fatalf("Cleanup 小时桶: %v", err)
	}
	if deleted != 1 {
		t.Errorf("删除了 %d 行小时桶，期望 1", deleted)
	}
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 1 || rows[0].BucketStart != fresh {
		t.Errorf("剩余小时桶 = %+v，期望只剩 10 天前那条", rows)
	}
	// 日桶保留期更长，同一时刻的日桶不该被上面那次清理带走。
	if rows := listBuckets(t, model.GranularityDay); len(rows) != 2 {
		t.Errorf("日桶剩 %d 行，期望 2——清理必须按 granularity 隔离", len(rows))
	}
}

func TestPruneOrphansRemovesDeletedInboundBuckets(t *testing.T) {
	setupTrafficTest(t)
	alive := mkTrafficInbound(t, 30102, "在")
	svc := TrafficHistoryService{}

	writeBucket(t, model.GranularityHour, alive.Id, 1000, 5, 5)
	// 一个库里已经不存在的入站 id。SQLite 会复用自增 id，留着它的话，
	// 下一个建出来的入站会看到上一个用户的曲线，而且引用不再悬空，
	// 生成期的任何跳过防线都拦不住。
	writeBucket(t, model.GranularityHour, 9999, 1000, 7, 7)

	pruned, err := svc.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Errorf("清理了 %d 行，期望 1", pruned)
	}
	rows := listBuckets(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].InboundId != alive.Id {
		t.Errorf("剩余 = %+v，期望只剩存活入站那条", rows)
	}
}

func TestDeleteByInboundOnlyTouchesTarget(t *testing.T) {
	setupTrafficTest(t)
	a := mkTrafficInbound(t, 30103, "甲")
	b := mkTrafficInbound(t, 30104, "乙")
	svc := TrafficHistoryService{}

	writeBucket(t, model.GranularityHour, a.Id, 1000, 1, 1)
	writeBucket(t, model.GranularityDay, a.Id, 1000, 1, 1)
	writeBucket(t, model.GranularityHour, b.Id, 1000, 2, 2)

	if err := svc.DeleteByInbound(a.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 1 || rows[0].InboundId != b.Id {
		t.Errorf("小时桶剩余 = %+v，期望只剩乙的", rows)
	}
	if rows := listBuckets(t, model.GranularityDay); len(rows) != 0 {
		t.Errorf("日桶剩余 = %+v，期望甲的两级都被删掉", rows)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
CGO_ENABLED=1 go test ./web/service/ -run "TestCleanup|TestPruneOrphans|TestDeleteByInbound" -v 2>&1 | tail -10
```

期望：编译失败，`svc.Cleanup undefined` 等。

- [ ] **Step 3: 写实现**

在 `web/service/traffic_history.go` 末尾追加：

```go
// Cleanup 删除某一级中早于保留期的桶，返回删除行数。
//
// 两级各有各的保留期，所以条件里必须带 granularity——不带的话，一次
// 「清理小时桶」会把同样早于该时刻的日桶一起删掉，长期趋势图会静默变空。
func (s *TrafficHistoryService) Cleanup(g model.TrafficGranularity, retentionDays int, now time.Time) (int64, error) {
	db := database.GetTrafficDB()
	if db == nil || retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	result := db.Where("granularity = ? and bucket_start < ?", g, cutoff).
		Delete(&model.TrafficBucket{})
	return result.RowsAffected, result.Error
}

// PruneOrphans 删除已不存在的入站遗留的桶，返回删除行数。
//
// 这是第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。第一道在
// DelInbound 内。两道都要有：SQLite 会复用被删除的自增 id，残留的桶会绑到
// 下一个建出来的入站上，那时引用不再悬空，图会渲染得非常合理，只是画的是
// 别人的数据。
func (s *TrafficHistoryService) PruneOrphans() (int64, error) {
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
	result := tx.Delete(&model.TrafficBucket{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部用量历史（两级都删）。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *TrafficHistoryService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.TrafficBucket{}).Error
}
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
CGO_ENABLED=1 go test ./web/service/ -run "TestCleanup|TestPruneOrphans|TestDeleteByInbound" -v 2>&1 | tail -10
```

期望：3 个测试全部 PASS。

- [ ] **Step 5: 写定时任务**

创建 `web/job/traffic_cleanup_job.go`：

```go
package job

import (
	"time"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// TrafficCleanupJob 按各自的保留期清理两级用量数据，并顺带清掉已删除入站
// 遗留的孤儿桶。
//
// 不并进 AccessLogCleanupJob：那会让它名不副实，也会让两种数据的清理失败
// 互相牵连——访问日志库出问题不该连带停掉用量数据的清理。
type TrafficCleanupJob struct {
	trafficService service.TrafficHistoryService
	settingService service.SettingService
}

func NewTrafficCleanupJob() *TrafficCleanupJob {
	return new(TrafficCleanupJob)
}

func (j *TrafficCleanupJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层。
	defer common.Recover("用量历史清理任务")

	now := time.Now()

	if days, err := j.settingService.GetTrafficHourRetentionDays(); err != nil {
		logger.Warning("读取用量小时数据保留天数失败:", err)
	} else if deleted, err := j.trafficService.Cleanup(model.GranularityHour, days, now); err != nil {
		logger.Warning("清理过期用量小时数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期用量小时数据", deleted)
	}

	if days, err := j.settingService.GetTrafficDayRetentionDays(); err != nil {
		logger.Warning("读取用量每日数据保留天数失败:", err)
	} else if deleted, err := j.trafficService.Cleanup(model.GranularityDay, days, now); err != nil {
		logger.Warning("清理过期用量每日数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期用量每日数据", deleted)
	}

	if pruned, err := j.trafficService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿用量数据失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的用量数据", pruned)
	}
}
```

- [ ] **Step 6: 删除入站时连带清理**

在 `web/service/inbound.go` 的 `DelInbound` 中，`accessLogService.DeleteByInbound` 那个 `if` 块之后插入：

```go
	// 用量历史同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来
	// 的入站会看到上一个用户的用量曲线。失败只告警不阻断，理由同访问日志，
	// 残留由每小时一次的 PruneOrphans 兜底。
	if err := (&TrafficHistoryService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的用量历史失败, 将由定时清理兜底, id:", id, "err:", err)
	}
```

- [ ] **Step 7: 注册任务与启动时扫描**

在 `web/web.go` 的 `Server` 结构体中，`accessLogService` 那一行之后加：

```go
	trafficHistoryService service.TrafficHistoryService
```

把 Task 1 加的那段 `InitTrafficDB` 改成带孤儿扫描的形式：

```go
	// 用量历史同样用独立的库。打不开只影响图表，不影响累计流量与限额判定。
	if err := database.InitTrafficDB(config.GetTrafficDBPath()); err != nil {
		logger.Warning("open traffic history database failed, 用量图表将不可用:", err)
	} else if pruned, err := s.trafficHistoryService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿用量数据失败:", err)
	} else if pruned > 0 {
		// 与访问日志同理：删除入站时若用量库恰好不可写，桶会留下来。启动时
		// 先扫一遍，把窗口从「最多一小时」缩到「最多到下次重启」。
		logger.Warning("清理了", pruned, "条已删除入站遗留的用量数据")
	}
```

在 `startTask()` 中，`NewAccessLogCleanupJob` 那一行之后加：

```go
	// 每小时按各自的保留期清理用量历史
	s.cron.AddJob("@every 1h", job.NewTrafficCleanupJob())
```

- [ ] **Step 8: 全量验证**

```bash
CGO_ENABLED=1 go build -o /dev/null . && CGO_ENABLED=1 go test ./web/... ./database/... 2>&1 | tail -10
```

期望：构建通过，所有包 `ok` 或 `no test files`。

- [ ] **Step 9: 提交**

```bash
git add web/service/traffic_history.go web/service/traffic_history_test.go \
        web/job/traffic_cleanup_job.go web/service/inbound.go web/web.go
git commit -m "feat(traffic): 保留期清理与孤儿清理

两级各有各的保留期，清理条件必须带 granularity——不带的话一次
清理小时桶会把日桶一起删掉，长期趋势图静默变空。

删除入站时连带清理，另有 PruneOrphans 兜底：SQLite 复用自增 id，
残留的桶会绑到下一个入站上，那时引用不再悬空，图会渲染得非常
合理，只是画的是别人的数据。"
```

---

## Task 5: 查询聚合（补零、Top N、格式化标签）

服务端把数据整成前端可以直接喂给 Chart.js 的形状：刻度稠密、标签已格式化、系列已排序。前端不做任何时间计算——时区在服务端，前端算会算错。

**Files:**
- Modify: `web/service/traffic_history.go`（追加查询部分）
- Modify: `web/service/traffic_history_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 1 与 Task 2 的全部产物
- Produces:
  - `TrafficRange`（`string` 别名）与常量 `Range24h = "24h"` / `Range7d = "7d"` / `Range30d = "30d"` / `Range1y = "1y"`
  - `TrafficPoint{T int64, Up int64, Down int64}`（json: `t` / `up` / `down`）
  - `TrafficSeries{InboundId int, Remark string, Points []int64}`（json: `inboundId` / `remark` / `points`）
  - `TrafficHistoryResult{Granularity string, Labels []string, Points []TrafficPoint, Reason string}`
  - `TrafficOverviewResult{Granularity string, Labels []string, Series []TrafficSeries, Reason string}`
  - `(*TrafficHistoryService).History(inboundId int, r TrafficRange, now time.Time) (*TrafficHistoryResult, error)`
  - `(*TrafficHistoryService).Overview(r TrafficRange, topN int, now time.Time) (*TrafficOverviewResult, error)`

- [ ] **Step 1: 写失败的测试**

在 `web/service/traffic_history_test.go` 末尾追加：

```go
func TestHistoryPadsMissingBucketsWithZero(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30201, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	// 只写当前小时这一个桶，其余 23 个小时库里根本没有行（零流量不写行）。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 111, 222)

	res, err := svc.History(in.Id, Range24h, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(res.Points) != 24 {
		t.Fatalf("点数 = %d，期望 24（缺失的桶必须补零，否则图上会缺一段而不是显示 0）", len(res.Points))
	}
	if len(res.Labels) != len(res.Points) {
		t.Fatalf("labels %d 与 points %d 不等长，Chart.js 会错位", len(res.Labels), len(res.Points))
	}
	last := res.Points[len(res.Points)-1]
	if last.Up != 111 || last.Down != 222 {
		t.Errorf("最后一个点 = %+v，期望 up=111 down=222（当前小时应在最右）", last)
	}
	for i, p := range res.Points[:len(res.Points)-1] {
		if p.Up != 0 || p.Down != 0 {
			t.Errorf("第 %d 个点 = %+v，期望补零", i, p)
		}
	}
	if res.Granularity != "hour" {
		t.Errorf("granularity = %q，期望 hour", res.Granularity)
	}
}

func TestHistoryOneYearUsesDayBuckets(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30202, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	writeBucket(t, model.GranularityDay, in.Id, model.AlignDay(now, sh), 9, 9)
	// 同一天的小时桶不该混进 1 年这一档。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 500, 500)

	res, err := svc.History(in.Id, Range1y, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if res.Granularity != "day" || len(res.Points) != 365 {
		t.Fatalf("granularity=%q 点数=%d，期望 day / 365", res.Granularity, len(res.Points))
	}
	if last := res.Points[364]; last.Up != 9 || last.Down != 9 {
		t.Errorf("最后一个点 = %+v，期望取日桶的 9/9 而不是小时桶的 500/500", last)
	}
}

func TestHistoryExcludesOtherInbounds(t *testing.T) {
	setupTrafficTest(t)
	a := mkTrafficInbound(t, 30203, "甲")
	b := mkTrafficInbound(t, 30204, "乙")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	writeBucket(t, model.GranularityHour, b.Id, model.AlignHour(now, sh), 999, 999)

	res, err := svc.History(a.Id, Range24h, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, p := range res.Points {
		if p.Up != 0 || p.Down != 0 {
			t.Fatalf("甲的图里出现了乙的数据: %+v", p)
		}
	}
}

func TestHistoryUnknownRangeFallsBackTo24h(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30205, "甲")
	svc := TrafficHistoryService{}

	res, err := svc.History(in.Id, TrafficRange("不认识的档位"), time.Now())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// 前端传错档位时给一张能看的图，而不是报错或空图。
	if res.Granularity != "hour" || len(res.Points) != 24 {
		t.Errorf("granularity=%q 点数=%d，期望回落到 24 小时档", res.Granularity, len(res.Points))
	}
}

func TestHistoryReportsReasonWhenDatabaseUnavailable(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30206, "甲")
	database.ResetTrafficDBForTest()
	svc := TrafficHistoryService{}

	res, err := svc.History(in.Id, Range24h, time.Now())
	if err != nil {
		t.Fatalf("库不可用时不该返回错误，实际: %v", err)
	}
	// 「看不到」和「没有」必须能被区分开，否则管理员会以为这个人没用流量。
	if res.Reason == "" {
		t.Error("库不可用时 Reason 应说明原因，不能返回一张看起来正常的空图")
	}
}

func TestOverviewRanksByTotalAndTruncates(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)
	slot := model.AlignHour(now, sh)

	// 三个入站，用量 300 / 100 / 200，取 Top 2 应得到 300 和 200。
	big := mkTrafficInbound(t, 30301, "大")
	small := mkTrafficInbound(t, 30302, "小")
	mid := mkTrafficInbound(t, 30303, "中")
	writeBucket(t, model.GranularityHour, big.Id, slot, 150, 150)
	writeBucket(t, model.GranularityHour, small.Id, slot, 50, 50)
	writeBucket(t, model.GranularityHour, mid.Id, slot, 100, 100)

	res, err := svc.Overview(Range24h, 2, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(res.Series) != 2 {
		t.Fatalf("系列数 = %d，期望 2", len(res.Series))
	}
	if res.Series[0].Remark != "大" || res.Series[1].Remark != "中" {
		t.Errorf("排序 = %q, %q，期望 大, 中（按总量降序）", res.Series[0].Remark, res.Series[1].Remark)
	}
	if got := res.Series[0].Points[len(res.Series[0].Points)-1]; got != 300 {
		t.Errorf("最大系列的最后一个点 = %d，期望 300（up+down）", got)
	}
	for _, s := range res.Series {
		if len(s.Points) != len(res.Labels) {
			t.Errorf("系列 %q 的点数 %d 与 labels %d 不等长", s.Remark, len(s.Points), len(res.Labels))
		}
	}
}

func TestOverviewReturnsAllWhenFewerThanTopN(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	in := mkTrafficInbound(t, 30304, "唯一")
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 1, 1)

	res, err := svc.Overview(Range24h, 12, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(res.Series) != 1 {
		t.Errorf("系列数 = %d，期望 1", len(res.Series))
	}
}

func TestOverviewFallsBackToIdWhenRemarkEmpty(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	in := mkTrafficInbound(t, 30305, "")
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 1, 1)

	res, err := svc.Overview(Range24h, 12, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	// 图例上留一个空标签，管理员分不出这条线是谁的。
	want := fmt.Sprintf("#%d", in.Id)
	if len(res.Series) != 1 || res.Series[0].Remark != want {
		t.Errorf("备注 = %q，期望回落成 %q", res.Series[0].Remark, want)
	}
}

func TestOverviewIgnoresBucketsOutsideRange(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	in := mkTrafficInbound(t, 30306, "甲")
	// 48 小时前的桶落在 24 小时档之外，既不该出现在点里，
	// 也不该让这个入站因为它而挤进 Top N。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now.Add(-48*time.Hour), sh), 9999, 9999)

	res, err := svc.Overview(Range24h, 12, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(res.Series) != 0 {
		t.Errorf("系列数 = %d，期望 0——范围外的桶不该把入站带进 Top N", len(res.Series))
	}
}

// mustLoadShanghai 与面板的默认时区一致（defaultValueMap 里的 timeLocation）。
func mustLoadShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
CGO_ENABLED=1 go test ./web/service/ -run "TestHistory|TestOverview" -v 2>&1 | tail -10
```

期望：编译失败，`undefined: Range24h`、`svc.History undefined` 等。

- [ ] **Step 3: 写实现**

在 `web/service/traffic_history.go` 末尾追加：

```go
// TrafficRange 是用量图上的时间范围档位。
type TrafficRange string

const (
	Range24h TrafficRange = "24h"
	Range7d  TrafficRange = "7d"
	Range30d TrafficRange = "30d"
	Range1y  TrafficRange = "1y"
)

// trafficDBUnavailable 是库没打开时给界面的统一说明。「看不到」和「没有」
// 必须能被区分开——返回一张看起来正常的空图，管理员会以为这个人没用流量。
const trafficDBUnavailable = "用量历史库不可用，图表暂时无法显示。请检查面板日志与磁盘空间"

// TrafficPoint 是单入站图上的一个点。
type TrafficPoint struct {
	T    int64 `json:"t"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// TrafficSeries 是全局图上的一条线。Points 与 TrafficOverviewResult.Labels
// 等长，每个元素是该刻度上的 up+down。
type TrafficSeries struct {
	InboundId int     `json:"inboundId"`
	Remark    string  `json:"remark"`
	Points    []int64 `json:"points"`
}

type TrafficHistoryResult struct {
	Granularity string         `json:"granularity"`
	Labels      []string       `json:"labels"`
	Points      []TrafficPoint `json:"points"`
	Reason      string         `json:"reason"`
}

type TrafficOverviewResult struct {
	Granularity string          `json:"granularity"`
	Labels      []string        `json:"labels"`
	Series      []TrafficSeries `json:"series"`
	Reason      string          `json:"reason"`
}

// rangeSpec 把档位翻译成粒度与点数。认不出的档位回落到 24 小时：前端传错
// 时给一张能看的图，比报错或空图有用。
func rangeSpec(r TrafficRange) (model.TrafficGranularity, int) {
	switch r {
	case Range7d:
		return model.GranularityHour, 24 * 7
	case Range30d:
		return model.GranularityHour, 24 * 30
	case Range1y:
		return model.GranularityDay, 365
	default:
		return model.GranularityHour, 24
	}
}

func granularityName(g model.TrafficGranularity) string {
	if g == model.GranularityDay {
		return "day"
	}
	return "hour"
}

// buildSlots 生成范围内全部刻度的桶起点，升序，最后一个是当前所在的桶。
//
// 小时用算术递减（小时桶按定义就是对齐到整小时的，UTC 偏移含半小时的时区
// 也保持一致）；日必须用 AddDate 递减，因为一天不总是 86400 秒。
func buildSlots(g model.TrafficGranularity, now time.Time, loc *time.Location, count int) []int64 {
	slots := make([]int64, count)
	if g == model.GranularityDay {
		day := time.Unix(model.AlignDay(now, loc), 0).In(loc)
		for i := count - 1; i >= 0; i-- {
			slots[i] = day.Unix()
			day = day.AddDate(0, 0, -1)
		}
		return slots
	}
	end := model.AlignHour(now, loc)
	for i := 0; i < count; i++ {
		slots[i] = end - int64(count-1-i)*3600
	}
	return slots
}

// formatLabels 在服务端把刻度格式化成 x 轴文字。放在服务端是因为时区也在
// 服务端：让前端拿时间戳自己格式化，浏览器所在时区一变，图上的时间就和
// 面板设置的时区对不上了。
func formatLabels(g model.TrafficGranularity, slots []int64, loc *time.Location) []string {
	layout := "01-02 15:00"
	if g == model.GranularityDay {
		layout = "2006-01-02"
	}
	labels := make([]string, len(slots))
	for i, s := range slots {
		labels[i] = time.Unix(s, 0).In(loc).Format(layout)
	}
	return labels
}

// History 返回单个入站在指定范围内的分时用量，刻度稠密（缺失的桶补零）。
func (s *TrafficHistoryService) History(inboundId int, r TrafficRange, now time.Time) (*TrafficHistoryResult, error) {
	g, count := rangeSpec(r)
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	slots := buildSlots(g, now, loc, count)
	result := &TrafficHistoryResult{
		Granularity: granularityName(g),
		Labels:      formatLabels(g, slots, loc),
		Points:      make([]TrafficPoint, count),
	}
	for i, start := range slots {
		result.Points[i] = TrafficPoint{T: start}
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}
	var rows []model.TrafficBucket
	err = db.Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, slots[0]).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	index := make(map[int64]int, len(slots))
	for i, start := range slots {
		index[start] = i
	}
	for _, row := range rows {
		if i, ok := index[row.BucketStart]; ok {
			result.Points[i].Up = row.Up
			result.Points[i].Down = row.Down
		}
	}
	return result, nil
}

// Overview 返回范围内用量最大的前 topN 个入站的分时曲线，按总量降序。
func (s *TrafficHistoryService) Overview(r TrafficRange, topN int, now time.Time) (*TrafficOverviewResult, error) {
	g, count := rangeSpec(r)
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	slots := buildSlots(g, now, loc, count)
	result := &TrafficOverviewResult{
		Granularity: granularityName(g),
		Labels:      formatLabels(g, slots, loc),
		Series:      []TrafficSeries{},
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}
	if topN <= 0 {
		topN = 12
	}

	// 先只算出 Top N 的 id，再取这几个的明细。一次把范围内所有行拉进内存
	// 也能算，但入站变多之后那是一个没有上限的读取量。
	type topRow struct {
		InboundId int
		Total     int64
	}
	var tops []topRow
	err = db.Model(&model.TrafficBucket{}).
		Select("inbound_id, sum(up + down) as total").
		Where("granularity = ? and bucket_start >= ?", g, slots[0]).
		Group("inbound_id").
		// 次级排序按 id：总量相同时顺序也要稳定，否则每次刷新图例都在跳。
		Order("total desc, inbound_id asc").
		Limit(topN).
		Scan(&tops).Error
	if err != nil {
		return nil, err
	}
	if len(tops) == 0 {
		return result, nil
	}

	ids := make([]int, 0, len(tops))
	for _, t := range tops {
		ids = append(ids, t.InboundId)
	}

	var inbounds []*model.Inbound
	if err := database.GetDB().Model(model.Inbound{}).Where("id in ?", ids).Find(&inbounds).Error; err != nil {
		return nil, err
	}
	remarks := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		remarks[in.Id] = in.Remark
	}

	var rows []model.TrafficBucket
	err = db.Where("granularity = ? and bucket_start >= ? and inbound_id in ?", g, slots[0], ids).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	index := make(map[int64]int, len(slots))
	for i, start := range slots {
		index[start] = i
	}
	byInbound := make(map[int][]int64, len(ids))
	for _, id := range ids {
		byInbound[id] = make([]int64, count)
	}
	for _, row := range rows {
		points, ok := byInbound[row.InboundId]
		if !ok {
			continue
		}
		if i, ok := index[row.BucketStart]; ok {
			points[i] = row.Up + row.Down
		}
	}

	// 按 tops 的顺序输出，Top N 的排序结果就是图例的顺序。
	for _, t := range tops {
		remark := remarks[t.InboundId]
		if remark == "" {
			// 图例上留一个空标签，管理员分不出这条线是谁的。
			remark = fmt.Sprintf("#%d", t.InboundId)
		}
		result.Series = append(result.Series, TrafficSeries{
			InboundId: t.InboundId,
			Remark:    remark,
			Points:    byInbound[t.InboundId],
		})
	}
	return result, nil
}
```

在该文件的 import 中加入 `"fmt"`（`Overview` 里的备注回落用到）。

- [ ] **Step 4: 运行测试，确认通过**

```bash
CGO_ENABLED=1 go test ./web/service/ -run "TestHistory|TestOverview" -v 2>&1 | tail -20
```

期望：9 个测试全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add web/service/traffic_history.go web/service/traffic_history_test.go
git commit -m "feat(traffic): 用量查询聚合

服务端负责补零、对齐刻度、格式化标签、排序 Top N，前端只管画。
标签在服务端格式化是因为时区也在服务端：让浏览器自己格式化，
访问者所在时区一变，图上的时间就和面板设置的时区对不上了。

Top N 的次级排序按 id：总量相同时顺序也要稳定，否则每次刷新
图例都在跳。"
```

---

## Task 6: HTTP 接口

**Files:**
- Modify: `web/controller/inbound.go`（结构体约 14-20 行；import；`initRouter` 约 29-44 行；文件末尾加两个方法）
- Create: `web/controller/traffic_test.go`

**Interfaces:**
- Consumes: `service.TrafficRange` / `Range24h` / `(*TrafficHistoryService).History` / `.Overview`（Task 5）；测试辅助 `newRenewRouter(t)` / `createInbound(t, expiryTime, enable, up, down)` / `postForm(t, r, path, body)` / `itoa(int)`（已存在于 `web/controller/inbound_renew_test.go`，同包）
- Produces:
  - 路由 `POST /aui/inbound/traffic/history/:id`（form 参数 `range`）
  - 路由 `POST /aui/inbound/traffic/overview`（form 参数 `range`、`top`）

- [ ] **Step 1: 写失败的测试**

创建 `web/controller/traffic_test.go`：

```go
package controller

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"a-ui/database"
)

// newTrafficRouter 在 newRenewRouter 的基础上再开一个用量库。
// newRenewRouter 只开主库，用量接口在那种状态下走的是「库不可用」分支。
func newTrafficRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r := newRenewRouter(t)
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	return r
}

func decodeTrafficObj(t *testing.T, obj interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("重新编码响应体: %v", err)
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应体 %q: %v", raw, err)
	}
	return out
}

func TestTrafficHistoryEndpointBindsRangeFromUrlencodedForm(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	// 前端发的是 urlencoded，绑定标签必须是 form 而不是 json，
	// 否则 range 永远是空串、图永远只显示 24 小时那一档。
	msg := postForm(t, r, "/aui/inbound/traffic/history/"+itoa(in.Id), "range=1y")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if got := obj["granularity"]; got != "day" {
		t.Errorf("granularity = %v，期望 day（range=1y 没有被绑定进来）", got)
	}
	points, ok := obj["points"].([]interface{})
	if !ok {
		t.Fatalf("响应里没有 points 数组: %v", obj)
	}
	if len(points) != 365 {
		t.Errorf("点数 = %d，期望 365", len(points))
	}
}

func TestTrafficHistoryEndpointDefaultsWithoutRange(t *testing.T) {
	r := newTrafficRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/traffic/history/"+itoa(in.Id), "")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if got := obj["granularity"]; got != "hour" {
		t.Errorf("granularity = %v，期望回落到 hour", got)
	}
}

func TestTrafficHistoryEndpointRejectsNonNumericId(t *testing.T) {
	r := newTrafficRouter(t)

	msg := postForm(t, r, "/aui/inbound/traffic/history/abc", "range=24h")
	if msg.Success {
		t.Error("id 不是数字时应当失败")
	}
}

func TestTrafficOverviewEndpointReturnsLabelsAndSeries(t *testing.T) {
	r := newTrafficRouter(t)
	createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/traffic/overview", "range=24h&top=12")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	labels, ok := obj["labels"].([]interface{})
	if !ok || len(labels) != 24 {
		t.Fatalf("labels = %v，期望 24 个", obj["labels"])
	}
	// 没有任何用量时 series 必须是空数组而不是 null：
	// 前端会对它做 .map，null 会直接报 TypeError 把整张图打掉。
	if _, ok := obj["series"].([]interface{}); !ok {
		t.Errorf("series = %v，期望空数组而不是 null", obj["series"])
	}
}

func TestTrafficOverviewEndpointSurvivesUnavailableDatabase(t *testing.T) {
	r := newTrafficRouter(t)
	createInbound(t, 0, true, 0, 0)
	database.ResetTrafficDBForTest()

	msg := postForm(t, r, "/aui/inbound/traffic/overview", "range=24h")
	// 库不可用是一种要如实告知的状态，不是接口错误：
	// 报 500 会让整个系统状态页看起来是坏的。
	if !msg.Success {
		t.Fatalf("库不可用时接口不该失败, msg = %q", msg.Msg)
	}
	obj := decodeTrafficObj(t, msg.Obj)
	if obj["reason"] == "" {
		t.Error("库不可用时必须给出 reason")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
CGO_ENABLED=1 go test ./web/controller/ -run TestTraffic -v 2>&1 | tail -10
```

期望：全部失败，路由 404（`msg.Success = false`）。

- [ ] **Step 3: 加控制器字段与导入**

在 `web/controller/inbound.go` 的 `InboundController` 结构体中，`geoService` 之后加：

```go
	trafficHistoryService service.TrafficHistoryService
```

在 import 块中加 `"time"`。

- [ ] **Step 4: 注册路由**

在 `initRouter` 中，`g.POST("/provinces", a.getProvinces)` 之前加：

```go
	g.POST("/traffic/history/:id", a.getTrafficHistory)
	g.POST("/traffic/overview", a.getTrafficOverview)
```

- [ ] **Step 5: 写两个方法**

在 `web/controller/inbound.go` 末尾追加：

```go
func (a *InboundController) getTrafficHistory(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	// 同其它接口：前端发的是 urlencoded，绑定标签必须是 form。
	form := struct {
		Range string `form:"range"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	result, err := a.trafficHistoryService.History(id, service.TrafficRange(form.Range), time.Now())
	if err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	jsonObj(c, result, nil)
}

func (a *InboundController) getTrafficOverview(c *gin.Context) {
	form := struct {
		Range string `form:"range"`
		Top   int    `form:"top"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取用量总览", err)
		return
	}
	result, err := a.trafficHistoryService.Overview(service.TrafficRange(form.Range), form.Top, time.Now())
	if err != nil {
		jsonMsg(c, "获取用量总览", err)
		return
	}
	jsonObj(c, result, nil)
}
```

- [ ] **Step 6: 运行测试，确认通过**

```bash
CGO_ENABLED=1 go test ./web/controller/ -run TestTraffic -v 2>&1 | tail -15
```

期望：5 个测试全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add web/controller/inbound.go web/controller/traffic_test.go
git commit -m "feat(traffic): 用量历史与总览的 HTTP 接口

库不可用时返回 success + reason 而不是 500：那是一种要如实告知的
状态，报错会让整个系统状态页看起来是坏的。

series 恒为数组不为 null——前端会对它做 .map，null 会直接
TypeError 把整张图打掉。"
```

---

## Task 7: 前端 —— 引入 Chart.js 与单入站图

**Files:**
- Create: `web/assets/chart.js/chart.umd.min.js`
- Modify: `web/html/xui/inbounds.html`（`expandedRowRender` 模板约 75-134 行；`data` 约 372 行；`methods`；`onExpand` 约 579-585 行；页面底部脚本引入）

**Interfaces:**
- Consumes: `POST /aui/inbound/traffic/history/:id`（Task 6）；全局 `Chart`（Chart.js UMD）；已有的 `HttpUtil.post`、`sizeFormat`
- Produces: 展开行内的用量图；`trafficCharts` 实例表与 `destroyTrafficChart(id)`（后者在 Task 8 无需复用，仅本页内部）

- [ ] **Step 1: 下载并校验 Chart.js**

```bash
mkdir -p web/assets/chart.js
curl -sL -o web/assets/chart.js/chart.umd.min.js \
  https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.5.0/chart.umd.min.js
shasum -a 256 web/assets/chart.js/chart.umd.min.js
```

期望输出的哈希必须是：

```
2f27bcf471b2d69dd78494f6e2172fb28470eb843820e2f96bb85d39f9618d30
```

**哈希不符就停下**，不要继续——这个文件会被 `//go:embed` 打进二进制发给所有用户。

- [ ] **Step 2: 确认它是预期的版本与许可**

```bash
head -c 200 web/assets/chart.js/chart.umd.min.js
```

期望看到 `Chart.js v4.5.0` 与 `Released under the MIT License`。

- [ ] **Step 3: 在入站页引入**

在 `web/html/xui/inbounds.html` 中找到 `{{template "js" .}}`，在它之后加一行：

```html
<script src="{{ .base_path }}assets/chart.js/chart.umd.min.js?{{ .cur_ver }}"></script>
```

**不要加进 `web/html/common/js.html`**：那是所有页面共用的，加进去会让登录页也白下载 203 KB，而登录页是唯一一个未认证用户能打到的页面。

- [ ] **Step 4: 在展开行里加图**

在 `web/html/xui/inbounds.html` 的 `expandedRowRender` 模板中，在线连接表那个 `</a-table>` 之后、`</template>` 之前插入：

```html
                                <div style="margin-top: 16px">
                                    <a-radio-group size="small" :value="trafficRangeOf(dbInbound.id)"
                                                   @change="e => changeTrafficRange(dbInbound.id, e.target.value)">
                                        <a-radio-button value="24h">24 小时</a-radio-button>
                                        <a-radio-button value="7d">7 天</a-radio-button>
                                        <a-radio-button value="30d">30 天</a-radio-button>
                                        <a-radio-button value="1y">1 年</a-radio-button>
                                    </a-radio-group>
                                    <a-alert v-if="trafficReasonOf(dbInbound.id)" type="info" show-icon
                                             style="margin-top: 12px"
                                             :message="trafficReasonOf(dbInbound.id)"></a-alert>
                                    <div style="position: relative; height: 220px; margin-top: 12px">
                                        <canvas :id="'traffic-chart-' + dbInbound.id"></canvas>
                                    </div>
                                </div>
```

这段必须留在 `<a-layout id="app">` 内部。Vue 2 只编译 `el` 指向的那棵子树，写到外面页面照常渲染、数据照常加载，但所有指令都是死的，控制台不报任何错。`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条。

- [ ] **Step 5: 加共享的图表配置与实例表**

在 `web/html/xui/inbounds.html` 页面脚本的最前面（`new Vue({` 之前）加：

```javascript
    // Chart 实例不放进 Vue 的 data：它是个庞大的对象，做成响应式既没有
    // 意义，还会拖慢每一次重绘。
    const trafficCharts = {};

    function trafficChartOptions() {
        return {
            responsive: true,
            maintainAspectRatio: false,
            interaction: { mode: 'index', intersect: false },
            plugins: {
                legend: { position: 'top' },
                tooltip: {
                    callbacks: {
                        label: ctx => ctx.dataset.label + ': ' + sizeFormat(ctx.parsed.y),
                    },
                },
            },
            scales: {
                y: {
                    beginAtZero: true,
                    ticks: { callback: value => sizeFormat(value) },
                },
                x: {
                    ticks: { maxRotation: 45, minRotation: 0, autoSkip: true, maxTicksLimit: 12 },
                },
            },
        };
    }
```

- [ ] **Step 6: 加数据与方法**

在 `data` 中，`expandedKeys: []` 之后加：

```javascript
            trafficRanges: {},
            trafficReasons: {},
```

在 `methods` 中加（`trafficCharts` 不放进 `data`：Chart 实例是个庞大的对象，交给 Vue 做响应式代理既没有意义，还会拖慢每一次重绘）：

```javascript
            trafficRangeOf(id) {
                return this.trafficRanges[id] || '24h';
            },
            trafficReasonOf(id) {
                return this.trafficReasons[id] || '';
            },
            changeTrafficRange(id, range) {
                // Vue 2 无法侦测新增的对象属性，必须走 $set。
                this.$set(this.trafficRanges, id, range);
                this.loadTrafficChart(id);
            },
            async loadTrafficChart(id) {
                const range = this.trafficRangeOf(id);
                const msg = await HttpUtil.post('/aui/inbound/traffic/history/' + id, { range: range });
                if (!msg.success) {
                    return;
                }
                const data = msg.obj;
                this.$set(this.trafficReasons, id, data.reason || '');
                // 等 Vue 把 canvas 渲染进 DOM 再取它：expandedRowRender 是
                // 动态渲染的，指令执行时元素还不存在。
                this.$nextTick(() => {
                    const canvas = document.getElementById('traffic-chart-' + id);
                    if (!canvas) {
                        return;
                    }
                    this.destroyTrafficChart(id);
                    trafficCharts[id] = new Chart(canvas, {
                        type: 'line',
                        data: {
                            labels: data.labels,
                            datasets: [
                                {
                                    label: '上传',
                                    data: data.points.map(p => p.up),
                                    borderColor: '#52c41a',
                                    backgroundColor: 'rgba(82, 196, 26, 0.1)',
                                    borderWidth: 2, pointRadius: 0, tension: 0.3, fill: true,
                                },
                                {
                                    label: '下载',
                                    data: data.points.map(p => p.down),
                                    borderColor: '#1890ff',
                                    backgroundColor: 'rgba(24, 144, 255, 0.1)',
                                    borderWidth: 2, pointRadius: 0, tension: 0.3, fill: true,
                                },
                            ],
                        },
                        options: trafficChartOptions(),
                    });
                });
            },
            destroyTrafficChart(id) {
                // Chart 实例持有 canvas 引用与 resize 监听。不销毁的话，反复
                // 展开折叠会一直累积，页面开几小时就会明显吃内存。
                if (trafficCharts[id]) {
                    trafficCharts[id].destroy();
                    delete trafficCharts[id];
                }
            },
```

- [ ] **Step 7: 接进展开与折叠**

把 `onExpand` 改成：

```javascript
            onExpand(expanded, dbInbound) {
                if (expanded) {
                    this.expandedKeys = this.expandedKeys.concat([dbInbound.id]);
                    this.loadOnlines(dbInbound.id);
                    this.loadTrafficChart(dbInbound.id);
                } else {
                    this.expandedKeys = this.expandedKeys.filter(id => id !== dbInbound.id);
                    this.destroyTrafficChart(dbInbound.id);
                }
            },
```

改之前先读一遍现有的 `onExpand`（约 579 行）：它在展开分支里可能已经调了 `loadOnlines`，保留原有调用，只增不改。

- [ ] **Step 8: 验证模板没写坏**

```bash
CGO_ENABLED=1 go test ./web/ -run "TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot" -v
```

期望：两个测试都 PASS。`getHtmlTemplate` 会吞掉 `ParseFS` 错误，所以光靠 `go build` 发现不了模板语法错误，这一步不能跳过。

- [ ] **Step 9: 人工验证**

```bash
XUI_DEBUG=true go run main.go
```

打开面板 → 入站列表 → 点开那一行。确认：

1. 在线连接表下方出现范围切换与折线图
2. 切换 `7 天` / `1 年`，x 轴刻度数量与标签格式随之变化
3. 反复展开折叠 10 次，浏览器 DevTools 的 Memory 面板里 Chart 实例数不增长
4. 控制台无报错

- [ ] **Step 10: 提交**

```bash
git add web/assets/chart.js/chart.umd.min.js web/html/xui/inbounds.html
git commit -m "feat(traffic): 入站展开行内的分时用量图

Chart.js v4.5.0 UMD 本地化，只在入站页与系统状态页引入，不进
common/js.html——那是所有页面共用的，登录页不该白下载 203KB。

canvas 在 \$nextTick 里取，折叠时 destroy()：expandedRowRender 是
动态渲染的，且 Chart 实例持有 canvas 引用与 resize 监听，不销毁
会随反复展开折叠累积。"
```

---

## Task 8: 前端 —— 系统状态页的全用户图

**Files:**
- Modify: `web/html/xui/index.html`（`a-layout-content` 内最后一个 `a-row` 之后；`data`；`methods`；`mounted`；脚本引入）

**Interfaces:**
- Consumes: `POST /aui/inbound/traffic/overview`（Task 6）；全局 `Chart`；已有的 `HttpUtil.post`、`sizeFormat`
- Produces: 无（页面终点）

- [ ] **Step 1: 引入 Chart.js**

在 `web/html/xui/index.html` 的 `{{template "js" .}}` 之后加：

```html
<script src="{{ .base_path }}assets/chart.js/chart.umd.min.js?{{ .cur_ver }}"></script>
```

- [ ] **Step 2: 加图表卡片**

在 `web/html/xui/index.html` 中，`<a-layout-content>` 内、现有内容的最后一个 `</a-row>` 之后、`</a-layout-content>` 之前插入：

```html
                <a-row style="margin-top: 10px">
                    <a-card hoverable>
                        <div slot="title">
                            最近使用 (Top [[ overviewSeriesCount ]])
                        </div>
                        <a-radio-group size="small" :value="overviewRange" @change="changeOverviewRange">
                            <a-radio-button value="24h">24 小时</a-radio-button>
                            <a-radio-button value="7d">7 天</a-radio-button>
                            <a-radio-button value="30d">30 天</a-radio-button>
                            <a-radio-button value="1y">1 年</a-radio-button>
                        </a-radio-group>
                        <a-alert v-if="overviewReason" type="info" show-icon style="margin-top: 12px"
                                 :message="overviewReason"></a-alert>
                        <div style="position: relative; height: 320px; margin-top: 12px">
                            <canvas id="overview-chart"></canvas>
                        </div>
                    </a-card>
                </a-row>
```

必须在 `<a-layout id="app">` 内部，理由同 Task 7 Step 4。

- [ ] **Step 3: 加调色板与图表配置**

在 `index.html` 页面脚本的最前面（`new Vue({` 之前）加：

```javascript
    let overviewChart = null;

    // 12 个区分度足够的颜色，与截图里的参考图一致的用法：按系列顺序取，
    // 超过 12 个入站时循环（Top N 默认就是 12，正常取不到循环）。
    const OVERVIEW_COLORS = [
        '#1890ff', '#52c41a', '#faad14', '#f5222d', '#722ed1', '#eb2f96',
        '#13c2c2', '#fa8c16', '#2f54eb', '#a0d911', '#08979c', '#c41d7f',
    ];

    function overviewChartOptions() {
        return {
            responsive: true,
            maintainAspectRatio: false,
            interaction: { mode: 'index', intersect: false },
            plugins: {
                legend: { position: 'top' },
                tooltip: {
                    callbacks: {
                        label: ctx => ctx.dataset.label + ': ' + sizeFormat(ctx.parsed.y),
                    },
                },
            },
            scales: {
                y: {
                    beginAtZero: true,
                    ticks: { callback: value => sizeFormat(value) },
                },
                x: {
                    ticks: { maxRotation: 45, minRotation: 0, autoSkip: true, maxTicksLimit: 14 },
                },
            },
        };
    }
```

- [ ] **Step 4: 加数据与方法**

在 `data` 中加：

```javascript
            overviewRange: '24h',
            overviewReason: '',
            overviewSeriesCount: 0,
```

在 `methods` 中加：

```javascript
            changeOverviewRange(e) {
                this.overviewRange = e.target.value;
                this.loadOverviewChart();
            },
            async loadOverviewChart() {
                const msg = await HttpUtil.post('/aui/inbound/traffic/overview', {
                    range: this.overviewRange,
                    top: 12,
                });
                if (!msg.success) {
                    return;
                }
                const data = msg.obj;
                this.overviewReason = data.reason || '';
                this.overviewSeriesCount = data.series.length;
                this.$nextTick(() => {
                    const canvas = document.getElementById('overview-chart');
                    if (!canvas) {
                        return;
                    }
                    if (overviewChart) {
                        overviewChart.destroy();
                        overviewChart = null;
                    }
                    overviewChart = new Chart(canvas, {
                        type: 'line',
                        data: {
                            labels: data.labels,
                            datasets: data.series.map((s, i) => ({
                                label: s.remark,
                                data: s.points,
                                borderColor: OVERVIEW_COLORS[i % OVERVIEW_COLORS.length],
                                backgroundColor: OVERVIEW_COLORS[i % OVERVIEW_COLORS.length],
                                borderWidth: 2, pointRadius: 2, tension: 0.3, fill: false,
                            })),
                        },
                        options: overviewChartOptions(),
                    });
                });
            },
```

- [ ] **Step 5: 在 mounted 里拉一次**

`index.html` 的 `mounted` 现在是一个 `while (true)` 的 2 秒轮询循环。**图不能挂进那个循环**——它的数据一小时才变一次，跟着 2 秒刷新是纯粹的浪费，还会让图每 2 秒重建一次、鼠标悬停的 tooltip 一直被打断。

把 `mounted` 改成先拉一次图，再进原有循环：

```javascript
        async mounted() {
            this.loadOverviewChart();
            while (true) {
                try {
                    await this.getStatus();
                } catch (e) {
                    console.error(e);
                }
                await PromiseUtil.sleep(2000);
            }
        },
```

`loadOverviewChart()` 不加 `await`：它失败或慢都不该拖住状态轮询的启动。

- [ ] **Step 6: 验证模板**

```bash
CGO_ENABLED=1 go test ./web/ -run "TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot" -v
```

期望：两个测试都 PASS。

- [ ] **Step 7: 人工验证**

```bash
XUI_DEBUG=true go run main.go
```

打开系统状态页，确认：

1. 页面底部出现「最近使用 (Top N)」卡片与折线图
2. 点击图例可以隐藏/显示对应的线
3. 切换范围，x 轴随之变化
4. 上方的 CPU / 内存等指标仍在每 2 秒刷新（图没有把轮询打断）
5. 控制台无报错

- [ ] **Step 8: 提交**

```bash
git add web/html/xui/index.html
git commit -m "feat(traffic): 系统状态页的全用户用量图

图不挂进那个 2 秒的状态轮询循环：数据一小时才变一次，跟着刷新
是纯粹的浪费，还会让 tooltip 一直被打断。"
```

---

## Task 9: 文档与门禁

**Files:**
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: 前八个任务的全部产物
- Produces: 无

- [ ] **Step 1: 跑完整门禁**

```bash
make verify
```

期望：`go vet` 无输出，所有测试 `ok`，`go build` 成功。**有任何失败就停下修复**，不要带着红灯继续。

- [ ] **Step 2: 检查工作区没有多余产物**

```bash
git status --short
```

期望：只有本次要提交的文件，没有临时脚本、调试文件或 `.db` 文件。

- [ ] **Step 3: 在 CLAUDE.md 加一节**

在「域名分流管理」一节之后、「安装向导与 Caddy 拓扑」之前插入：

```markdown
## 用量历史与图表

入站列表的展开行、系统状态页底部各有一张分时用量图。设计文档在
`docs/superpowers/specs/2026-09-04-traffic-history-design.md`。

**采集**复用 `XrayTrafficJob` 每 10 秒已经拿到的增量（`reset=true`，取完 xray 侧清零），
在 `AddTraffic` 累加进 `inbounds.up/down` 之前先记一份。不走「读累计值做差分」那条路：
差分方案里，一次正常的「重置流量」和一次数据损坏长得一模一样。这也意味着
**历史桶不受重置流量影响**——重置清的是累计计数器，历史记的是「某小时用了多少」，
两者语义无关。

**数据**落在独立的 `/etc/<name>/<name>-traffic.db`，与访问日志同样的分库理由。
一张表两级粒度（`granularity` 字段）：小时桶与日桶**各自独立累加**，日桶不由小时桶
汇总而来——汇总方案要处理「小时桶已被清理但日桶还没算」的补算逻辑。零增量不写行，
图上的 0 由服务端补零。10 个入站跑满保留期约 1~2 MB。

**桶按面板设置的时区对齐**（`GetTimeLocation()`），不是 UTC。UTC+8 下按 UTC 切日，
看到的「某天用量」会整体错位 8 小时，且不报任何错。时区改变后旧桶边界不变，交界处
会有一天错位，刻意不做补偿。

三条不能弱化的约束：

- **删除入站必须连带删除它的桶**（`TrafficHistoryService.DeleteByInbound`，接在
  `DelInbound` 里），另有每小时一次的 `PruneOrphans` 兜底。SQLite 会复用自增 id，
  残留的桶会绑到下一个建出来的入站上，那时引用不再悬空，图会渲染得非常合理，
  只是画的是别人的数据。
- **清理条件必须带 `granularity`**。两级各有各的保留期，不带的话一次「清理小时桶」
  会把日桶一起删掉，长期趋势图静默变空。
- **写用量历史失败只告警、不阻断 `AddTraffic`**。`inbounds.up/down` 是限额与到期
  判定的输入，它停止累加的后果（用户超额不被停用）比图上少一段曲线严重得多。

**前端**用 Chart.js v4.5.0 UMD（`web/assets/chart.js/`，MIT），只在 `inbounds.html`
与 `index.html` 单独引入，**不在 `common/js.html`**——那是所有页面共用的，登录页不该
白下载 203 KB。展开行里的 canvas 必须在 `$nextTick` 里取（`expandedRowRender` 是动态
渲染的），折叠时必须 `chart.destroy()`（Chart 实例持有 canvas 引用与 resize 监听）。
系统状态页的图**不挂进那个 2 秒的状态轮询循环**：数据一小时才变一次。

服务端负责补零、对齐刻度、格式化 x 轴标签、排序 Top N，前端只管画。**标签在服务端
格式化**是因为时区也在服务端：让浏览器自己格式化，访问者所在时区一变，图上的时间就
和面板设置的时区对不上了。
```

- [ ] **Step 4: 修正 CLAUDE.md 两处过时描述**

这两处在实现过程中发现，与本功能无直接关系，但会误导后续改动：

1. 「分层」一节里的 `database/model/model.go — 仅 3 张表`，改为说明实际已有 `User`/`Inbound`/`Setting` 之外的 `AccessLog`、`IPBan`、`DomainGroup`、`OutboundNode`、`RoutingRule`、`TrafficBucket`，且访问日志与用量历史在**独立的库**里。
2. 「已知偏差与注意事项」里的 `**cron 任务没有 panic 恢复。**` 整条，改为：`web/web.go` 的 `cron.New(...)` 已配 `cron.WithChain(cron.Recover(cronLogger{}))`，且现有 job 一律自带 `defer common.Recover(...)` 再挡一层；新增 job 照此办理。

- [ ] **Step 5: 再跑一次门禁并提交**

```bash
make verify && git add CLAUDE.md && git commit -m "docs: 用量历史子系统说明，并修正两处过时描述

仓库早已不是「仅 3 张表」，cron 也已配了 Recover。这两处会误导
后续改动，顺手改掉。"
```

- [ ] **Step 6: 发版（需要用户确认后再执行）**

```bash
git log --oneline main..HEAD   # 确认本次全部提交
git push origin <分支名>
```

打 tag 触发 `release.yml`（matrix 构建 amd64/arm64 → 打包中英两个 tar.gz → `gh release create`）：

```bash
git tag v<新版本号>
git push origin v<新版本号>
```

**版本号必须递增**：`cur_ver` 取自 `config/version`（由 CI 从 tag 名写入），assets 的强缓存是 `max-age=31536000`，版本号不变的话浏览器不会去取新的 `chart.umd.min.js`，图表会因为 `Chart is not defined` 完全不出现。

CI 跑完后在服务器上：

```bash
a-ui update
```

**`a-ui update` 会 `rm -rf /usr/local/a-ui/` 再重新解压**，但数据库在 `/etc/<name>/` 下，不受影响。首次启动会 `AutoMigrate` 建出 `traffic_buckets` 表，图上要等约一小时才有第一批小时桶数据（日桶同理，当天即可见）。

- [ ] **Step 7: 部署后验证**

```bash
ssh ubuntu@140.245.92.141 'systemctl is-active a-ui && pgrep -a xray | head -2'
ssh ubuntu@140.245.92.141 'ls -la /etc/a-ui/'
```

期望：面板 `active`，xray 进程在（**以 `pgrep` 为准，不要相信面板首页的状态**——`Process.Start()` 把 `cmd.Run()` 丢进 goroutine 后直接返回 nil，xray 启动失败不会回传到面板），`/etc/a-ui/` 下多出 `a-ui-traffic.db`。

然后打开面板确认两张图都在，等一小时后再看一次，确认曲线开始有数据。
