# 入站共享检测与地区建议 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让面板报出「这个入站有 N 小时里，不同省份的 IP 在同时使用」这一事实，并据此给出地区限制的建议值，由管理员确认后采纳。

**Architecture:** 复用 `OnlineService` 已有的内核连接表采样（它已经拿到每个入站的来源 IP 与活跃状态），在内存里按「入站 × IP × UTC 小时」累计活跃时长，满门槛写进 traffic 库的一张新表。判定用「同一小时内是否有两个不同省份同时活跃」——这个「并存」判据是区分「用户旅游」（位置迁移，不并存）与「节点被转卖」（位置并存）的唯一可靠信号。检测**只产出事实，不做任何自动处置**。

**Tech Stack:** Go 1.27 + GORM/SQLite（CGO 必须开启）、Vue 2 + ant-design-vue 服务端模板、无前端打包工具。

**Spec:** `docs/superpowers/specs/2026-09-05-inbound-sharing-detection-design.md`

## Global Constraints

- **构建与验证命令**：`make verify`（= `go vet ./...` + `go test ./...` + `go build`）是提交前门禁。单包测试用 `go test ./web/service/ -run TestXxx -v`。**不要猜命令**，`Makefile` 是唯一依据。
- **CGO 必须开启**：`gorm.io/driver/sqlite` 依赖 `mattn/go-sqlite3`。
- **不新增任何设置项。** 所有参数是包级常量。新增设置项需同步改 5 处，漏掉 `web/assets/js/model/models.js` 会让**整个保存配置接口失败**（端口、证书路径一起遭殃），为这个功能不值得。
- **不做任何自动处置**：不自动封禁、不自动限速、不自动写入地区限制。违反这一条就违反了整份设计。
- **绝不遍历 map 产生数组顺序**：所有对外输出的切片必须显式排序，否则同一份输入会产生不同结果。
- **新 job 的 `Run` 首行必须 `defer common.Recover("<任务名>")`**（`util/common/err.go`）。
- **前端 Vue 指令必须写在某个 Vue 根元素内部**，否则是静默死代码——页面正常渲染、按钮点了毫无反应、控制台不报错。
- **改了 `web/assets/js` 或 `css` 而版本号没变**，浏览器会命中 `max-age=31536000` 强缓存。本计划不改 `web/assets/`，只改 `web/html/`（模板不走那套缓存）。
- **时间单位**：`HourStart` 是 **Unix 秒**（与 `model.TrafficBucket.BucketStart` 同单位），不是毫秒。
- 提交信息用中文，结尾附：
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M
  ```

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `database/model/sharing.go`（新建） | `InboundIPHour` 表结构 + `AlignHourUTC` |
| `database/db.go`（改 `:204` 附近） | 把新表加进 traffic 库的 `AutoMigrate` |
| `web/service/sharing_stat.go`（新建） | 纯函数：并存判定 `computeCoexist`、地区建议 `suggestRegions` |
| `web/service/sharing_accumulator.go`（新建） | 纯内存累加器：门槛、上限、跨小时冲刷 |
| `web/service/sharing.go`（新建） | `SharingService`：采集落库、查询、清理 |
| `web/job/sharing_sample_job.go`（新建） | `@every 30s` 采样任务 |
| `web/web.go`（改） | 注册新 job |
| `web/job/traffic_cleanup_job.go`（改） | 加入新表的保留期清理与孤儿清理 |
| `web/service/inbound.go`（改 `DelInbound`） | 第四处连带删除 |
| `web/controller/inbound.go`（改） | 两个新接口 |
| `web/html/xui/sharing_modal.html`（新建） | 明细 modal，自带 Vue 根元素 |
| `web/html/xui/inbounds.html`（改） | 「地区」列追加标记、引入 modal |

拆成 `sharing_stat.go` / `sharing_accumulator.go` / `sharing.go` 三个文件而不是一个：前两者是纯函数与纯内存结构，不碰数据库，测试不需要建库；混在一起会让核心判定逻辑的测试被迫拖上一套 SQLite 初始化。

---

### Task 1: 数据模型与 UTC 小时对齐

**Files:**
- Create: `database/model/sharing.go`
- Create: `database/model/sharing_test.go`
- Modify: `database/db.go`（`InitTrafficDB` 内，`tdb.AutoMigrate(&model.TrafficBucket{})` 之后）

**Interfaces:**
- Consumes: 无（第一个任务）
- Produces: `model.InboundIPHour{Id int64, InboundId int, IP string, HourStart int64, Province string, ActiveSeconds int}`；`model.AlignHourUTC(t time.Time) int64`

- [ ] **Step 1: 写失败的测试**

创建 `database/model/sharing_test.go`：

```go
package model

import (
	"testing"
	"time"
)

// 同一时刻的两种时区表示必须落进同一个桶。这是「按 UTC 对齐」相对
// AlignHour 的全部价值：管理员改面板时区不会让历史桶与新刻度错开。
// AlignHour 那边的教训是改时区后「1 年」档历史整体消失。
func TestAlignHourUTCIsTimezoneIndependent(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	utc := time.Date(2026, 9, 5, 7, 30, 0, 0, time.UTC)
	local := utc.In(shanghai) // 同一时刻，本地表示为 15:30 +08:00

	if got, want := AlignHourUTC(local), AlignHourUTC(utc); got != want {
		t.Errorf("AlignHourUTC 受时区影响: local=%v utc=%v", got, want)
	}
}

func TestAlignHourUTCGroupsWithinTheSameHour(t *testing.T) {
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if AlignHourUTC(base) != AlignHourUTC(base.Add(59*time.Minute+59*time.Second)) {
		t.Error("同一小时内的两个时刻应落进同一个桶")
	}
	if AlignHourUTC(base) == AlignHourUTC(base.Add(time.Hour)) {
		t.Error("跨小时的两个时刻应落进不同的桶")
	}
	if got, want := AlignHourUTC(base), base.Unix(); got != want {
		t.Errorf("整点对齐结果 = %v, want %v（Unix 秒）", got, want)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./database/model/ -run TestAlignHourUTC -v`
Expected: FAIL，编译错误 `undefined: AlignHourUTC`

- [ ] **Step 3: 写实现**

创建 `database/model/sharing.go`：

```go
package model

import "time"

// InboundIPHour 是某入站的某个来源 IP 在某个 UTC 整点小时内的活跃时长，
// 存在**独立的 SQLite 库**里（与 TrafficBucket 同库，见 database.InitTrafficDB）。
//
// 分库理由与 TrafficBucket 相同：这张表每 30 秒写一批，清理时又是大批量
// DELETE，而 SQLite 一个库只有一把写锁——混在主库里会让面板的每一次普通
// 操作都去和它抢锁。
//
// 它只有一个消费者：判定「同一小时内，不同省份的 IP 是否在同时使用这个
// 入站」。这个「并存」判据是区分「用户旅游」（位置迁移，不并存）与「节点
// 被转卖」（位置并存）的唯一可靠信号，见设计文档 §1。
type InboundIPHour struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变。相应地，删除入站时必须连带删掉这些行——SQLite 会复用被删除
	// 的自增 id，不删的话下一个建出来的入站会继承上一个用户的并存记录，
	// 而且因为引用不再悬空，任何「跳过悬空引用」式的防线都拦不住它。
	InboundId int    `json:"inboundId" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:1"`
	IP        string `json:"ip" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:2"`
	HourStart int64  `json:"t" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:3"`

	// Province 是主判定省份，空串表示归属地未知（IPv6 来源、归属地库未加载、
	// 或库中查无此段）。空串的行照常入库：IP 维度的并存信息仍有价值，只是
	// 判定会降级成 IP 口径（见 service.computeCoexist）。
	Province string `json:"province"`

	ActiveSeconds int `json:"activeSeconds"`
}

// AlignHourUTC 把时刻对齐到它所在 UTC 小时的起点，返回 Unix 秒。
//
// 刻意**不用**面板时区，与 AlignHour 相反。这张表唯一的消费者是「同一
// 小时内是否并存」，该判定只关心两条记录落不落进同一个桶，桶的绝对位置
// 无关——UTC 与本地时区在此完全等价。既然等价，就不该背上 TrafficBucket
// 那个包袱：按本地时区对齐时，管理员改一次时区会让旧桶与重算出的新刻度
// 不相交，历史整段消失。展示时再按面板时区格式化标签即可。
func AlignHourUTC(t time.Time) int64 {
	return t.UTC().Truncate(time.Hour).Unix()
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./database/model/ -run TestAlignHourUTC -v`
Expected: PASS（两个用例）

- [ ] **Step 5: 加进 AutoMigrate**

在 `database/db.go` 的 `InitTrafficDB` 里，找到这一行（约 `:204`）：

```go
	if err := tdb.AutoMigrate(&model.TrafficBucket{}); err != nil {
		return err
	}
```

紧随其后插入：

```go
	// 共享检测的小时桶与用量桶同库：分库理由相同（高频写入不该抢主库
	// 那把写锁），再单开一个库只是多一个要初始化、要清理、要判空的句柄。
	if err := tdb.AutoMigrate(&model.InboundIPHour{}); err != nil {
		return err
	}
```

- [ ] **Step 6: 确认全仓库仍编译且测试通过**

Run: `go build ./... && go test ./database/...`
Expected: 全部 PASS

- [ ] **Step 7: 提交**

```bash
git add database/model/sharing.go database/model/sharing_test.go database/db.go
git commit -m "feat(sharing): 新增 InboundIPHour 小时桶与 UTC 对齐

按 UTC 而非面板时区对齐，与 TrafficBucket 刻意相反：并存判定只关心两条
记录是否落进同一个桶，UTC 与本地时区在此等价，因此不必背上「改时区导致
历史桶与新刻度不相交」那个包袱。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 2: 并存判定（纯函数）

这是整个功能的支点。四条判定行为各有一个用例，尤其是「旅游不误报」。

**Files:**
- Create: `web/service/sharing_stat.go`
- Create: `web/service/sharing_stat_test.go`

**Interfaces:**
- Consumes: `model.InboundIPHour`（Task 1）
- Produces:
  - `type CoexistStat struct { Hours int; Provinces []string; ByIP bool; IPs int }`
  - `func (s CoexistStat) Flagged() bool`
  - `func computeCoexist(rows []model.InboundIPHour) CoexistStat`
  - 测试辅助 `func hourRow(inboundId int, hour int64, ip, province string, seconds int) model.InboundIPHour`（Task 3 复用）

- [ ] **Step 1: 写失败的测试**

创建 `web/service/sharing_stat_test.go`：

```go
package service

import (
	"reflect"
	"testing"

	"a-ui/database/model"
)

// hourRow 造一条小时桶记录。Task 3 的测试也用它。
func hourRow(inboundId int, hour int64, ip, province string, seconds int) model.InboundIPHour {
	return model.InboundIPHour{
		InboundId: inboundId, HourStart: hour, IP: ip,
		Province: province, ActiveSeconds: seconds,
	}
}

// 整份设计的支点：并存判的是「同一小时内两个省份同时活跃」，不是
// 「出现过新省份」。旅游是位置迁移（旧的停了新的才开始），转卖是位置
// 并存（两地长期各自活跃）——只有按并存判，旅游才不会被误报。
func TestCoexistCountsOnlySimultaneousProvinces(t *testing.T) {
	const h = 3600
	cases := []struct {
		name  string
		rows  []model.InboundIPHour
		hours int
	}{
		{
			name: "转卖：两省长期并存",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 0*h, "2.2.2.2", "上海", 3600),
				hourRow(1, 1*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 1*h, "2.2.2.2", "上海", 3600),
			},
			hours: 2,
		},
		{
			name: "旅游：位置迁移，旧省停了新省才开始",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 1*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 2*h, "2.2.2.2", "上海", 3600),
				hourRow(1, 3*h, "2.2.2.2", "上海", 3600),
			},
			hours: 0,
		},
		{
			name: "错峰使用：不同小时，已知漏检（设计文档 §9）",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 1*h, "2.2.2.2", "上海", 3600),
			},
			hours: 0,
		},
		{
			name: "同一个人的手机加宽带：同省多 IP",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 0*h, "2.2.2.2", "江苏", 3600),
			},
			hours: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeCoexist(c.rows).Hours; got != c.hours {
				t.Errorf("Hours = %v, want %v", got, c.hours)
			}
		})
	}
}

func TestCoexistReportsProvincesSorted(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "2.2.2.2", "江苏", 3600),
		hourRow(1, 0*h, "1.1.1.1", "上海", 3600),
	}
	got := computeCoexist(rows).Provinces
	want := []string{"上海", "江苏"} // 升序，与输入顺序无关
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Provinces = %v, want %v", got, want)
	}
}

// 全部归属地未知（IPv6 来源，或归属地库未加载）时降级为 IP 口径。
func TestCoexistFallsBackToIPWhenNoProvinceKnown(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "", 3600),
		hourRow(1, 0*h, "2.2.2.2", "", 3600),
	}
	stat := computeCoexist(rows)
	if !stat.ByIP {
		t.Fatal("全部省份为空时应降级为 IP 口径")
	}
	if stat.Hours != 1 || stat.IPs != 2 {
		t.Errorf("Hours=%v IPs=%v, want 1/2", stat.Hours, stat.IPs)
	}
}

// 两种口径绝不混用：只要有一条记录带上了省份，就以省份口径为准。混用会让
// 同一个数字时而是省、时而是 IP，管理员无从判断自己在看什么。
func TestCoexistPrefersProvinceWheneverAnyIsKnown(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
		hourRow(1, 0*h, "2.2.2.2", "", 3600),
	}
	stat := computeCoexist(rows)
	if stat.ByIP {
		t.Fatal("有已知省份时不应降级为 IP 口径")
	}
	if stat.Hours != 0 {
		t.Errorf("Hours = %v, want 0（只有一个已知省份，不构成并存）", stat.Hours)
	}
}

func TestCoexistFlaggedNeedsDisplayMinimum(t *testing.T) {
	if (CoexistStat{Hours: coexistDisplayMinHours - 1}).Flagged() {
		t.Error("低于显示下限不该打标")
	}
	if !(CoexistStat{Hours: coexistDisplayMinHours}).Flagged() {
		t.Error("达到显示下限就该打标")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run TestCoexist -v`
Expected: FAIL，编译错误 `undefined: computeCoexist`

- [ ] **Step 3: 写实现**

创建 `web/service/sharing_stat.go`：

```go
package service

import (
	"sort"

	"a-ui/database/model"
)

// coexistDisplayMinHours 是并存标记的显示下限。
//
// 它只用来挡掉旅游迁移交界处那 1~2 小时的噪声（旧地区的设备还挂着、新
// 地区已经开始用），**不承担判断职责**：面板显示的是「并存 37 小时 / 3 省」
// 这样的事实，不做疑似分级。分级等于面板替管理员下判断，而阈值一旦调不好，
// 告警就退化成噪声或漏报。
const coexistDisplayMinHours = 3

// CoexistStat 是某入站在判定窗口内的并存统计。
type CoexistStat struct {
	// Hours 是并存小时数：某个 HourStart 下存在至少两条 Province 不同且
	// 都非空的记录，这一小时就计一次。
	Hours int `json:"hours"`
	// Provinces 是并存中出现过的省份，升序去重。
	Provinces []string `json:"provinces"`
	// ByIP 为 true 表示窗口内所有记录的 Province 都为空（IPv6 来源或归属地
	// 库未加载），判定降级为 IP 口径：Hours 变成「同一小时有 ≥2 个不同 IP」
	// 的小时数，IPs 是涉及的 IP 数。
	//
	// 降级口径误报率高得多——同一个人的手机和宽带就是两个 IP。界面必须
	// 明说当前是降级状态，别让管理员以为「3 IP 并存」是抓到了转卖。
	ByIP bool `json:"byIp"`
	IPs  int  `json:"ips"`
}

// Flagged 判断这份统计是否达到显示下限。
func (s CoexistStat) Flagged() bool { return s.Hours >= coexistDisplayMinHours }

// computeCoexist 从窗口内的行算出并存统计。
//
// 判定单位是「小时」而不是「是否出现过新省份」，这是整个功能的支点：
//   - 旅游是位置迁移——旧省停止活跃后新省才开始，两者落在不同小时，不并存
//   - 转卖是位置并存——两地长期各自活跃，持续落在同一批小时里
//
// 已知漏检：错峰共享（白天甲、晚上乙）落在不同小时，检测不到。抓它需要
// 「窗口内去重活跃 IP 数」这类指标，而正常用户 7 天十几个 IP、横跨 2~3 个省
// 是常态，误报率高到没法用。见设计文档 §9。
func computeCoexist(rows []model.InboundIPHour) CoexistStat {
	hasProvince := false
	for _, r := range rows {
		if r.Province != "" {
			hasProvince = true
			break
		}
	}

	byHourProvince := map[int64]map[string]bool{}
	byHourIP := map[int64]map[string]bool{}
	for _, r := range rows {
		if byHourIP[r.HourStart] == nil {
			byHourIP[r.HourStart] = map[string]bool{}
		}
		byHourIP[r.HourStart][r.IP] = true
		if r.Province == "" {
			continue
		}
		if byHourProvince[r.HourStart] == nil {
			byHourProvince[r.HourStart] = map[string]bool{}
		}
		byHourProvince[r.HourStart][r.Province] = true
	}

	stat := CoexistStat{ByIP: !hasProvince}
	group := byHourProvince
	if stat.ByIP {
		group = byHourIP
	}

	seen := map[string]bool{}
	for _, set := range group {
		if len(set) < 2 {
			continue
		}
		stat.Hours++
		for v := range set {
			seen[v] = true
		}
	}

	// 显式排序：上面遍历的是 map，顺序不定。不排的话同一份数据每次
	// 渲染出来的省份次序都不一样。
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	if stat.ByIP {
		stat.IPs = len(values)
	} else {
		stat.Provinces = values
	}
	return stat
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run TestCoexist -v`
Expected: PASS（5 个测试函数，其中第一个含 4 个子用例）

- [ ] **Step 5: 提交**

```bash
git add web/service/sharing_stat.go web/service/sharing_stat_test.go
git commit -m "feat(sharing): 并存判定

以「同一小时内两个省份同时活跃」为判据，而非「出现过新省份」：旅游是
位置迁移、转卖是位置并存，只有按并存判旅游才不会被误报。四条行为各有
一个回归用例，旅游那条是整份设计的支点。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 3: 地区建议（纯函数）

**Files:**
- Modify: `web/service/sharing_stat.go`（追加）
- Modify: `web/service/sharing_stat_test.go`（追加）

**Interfaces:**
- Consumes: `computeCoexist`、`hourRow`（Task 2）
- Produces: `type RegionSuggestion struct { Suggested []string; Coexisting []string }`；`func suggestRegions(rows []model.InboundIPHour) RegionSuggestion`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/sharing_stat_test.go`：

```go
func TestSuggestRegionsCoversNinetyFivePercent(t *testing.T) {
	const h = 3600
	// 江苏 900 秒（90%）、上海 80（8%）、浙江 20（2%）。
	// 90 不够 95，加上海到 98 够了，浙江这条长尾被切掉——一次出差、
	// 一次连错网络不该让建议退化成「几乎不限制」。
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 900),
		hourRow(1, 1*h, "2.2.2.2", "上海", 80),
		hourRow(1, 2*h, "3.3.3.3", "浙江", 20),
	}
	got := suggestRegions(rows).Suggested
	want := []string{"上海", "江苏"} // 升序；UTF-8 下 "上" < "江"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Suggested = %v, want %v", got, want)
	}
}

// 若该入站正在被共享，建议值会把买家的省份一并算进去。**不自动剔除**，
// 只标出来：面板分不清「买家的省」和「用户老家常挂的设备」，猜错两个
// 方向都是错的。直接采纳等于把转卖合法化，而这一步完全静默——界面正常、
// 配置正常生成，只是流量走对了买家。
func TestSuggestRegionsFlagsCoexistingWithoutRemoving(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 1800),
		hourRow(1, 0*h, "2.2.2.2", "上海", 1800),
	}
	got := suggestRegions(rows)
	want := []string{"上海", "江苏"}
	if !reflect.DeepEqual(got.Suggested, want) {
		t.Errorf("Suggested = %v, want %v（不得自动剔除并存省份）", got.Suggested, want)
	}
	if !reflect.DeepEqual(got.Coexisting, want) {
		t.Errorf("Coexisting = %v, want %v", got.Coexisting, want)
	}
}

func TestSuggestRegionsIgnoresUnknownProvince(t *testing.T) {
	const h = 3600
	// 归属地未知的行不参与建议：拿空串当省份填进地区限制会让整份配置
	// 生成失败（EncodeRegionsStrict 会拒），或者更糟——静默变成不限制。
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
		hourRow(1, 0*h, "2.2.2.2", "", 3600),
	}
	got := suggestRegions(rows).Suggested
	want := []string{"江苏"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Suggested = %v, want %v", got, want)
	}
}

func TestSuggestRegionsEmptyWhenNoProvinceAtAll(t *testing.T) {
	rows := []model.InboundIPHour{hourRow(1, 0, "1.1.1.1", "", 3600)}
	got := suggestRegions(rows)
	if len(got.Suggested) != 0 || len(got.Coexisting) != 0 {
		t.Errorf("全未知时应给空建议, got %+v", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run TestSuggestRegions -v`
Expected: FAIL，编译错误 `undefined: suggestRegions`

- [ ] **Step 3: 写实现**

追加到 `web/service/sharing_stat.go`（文件末尾）：

```go
// regionSuggestCoverage 是建议集合要覆盖的活跃时长占比。
//
// 不取 100%：一次出差、一次连错网络都会在列表里留下一个只占千分之几的
// 省份，全收进来等于建议「不限制」。95% 能盖住常住地与常去地，又能把
// 长尾切掉。
const regionSuggestCoverage = 0.95

// RegionSuggestion 是地区限制的建议值。
type RegionSuggestion struct {
	// Suggested 是按活跃时长降序累计、覆盖到 95% 的省份，升序输出。
	Suggested []string `json:"suggested"`

	// Coexisting 是 Suggested 里同时出现在并存记录中的省份。
	//
	// **刻意不从 Suggested 里剔除。** 面板分不清「买家的省」和「用户老家
	// 常挂的设备」，猜错两个方向都是错的：剔错了管理员采纳后把自己的用户
	// 挡在门外，不剔则可能把买家放行。标出来交给管理员判断是唯一诚实的
	// 做法——而且这一步若猜错完全静默：界面正常、xray 返回 Configuration
	// OK、面板显示 running，只是流量走对了不该走的人。
	Coexisting []string `json:"coexisting"`
}

// suggestRegions 从窗口内的行算出地区限制的建议值。
func suggestRegions(rows []model.InboundIPHour) RegionSuggestion {
	total := 0
	byProvince := map[string]int{}
	for _, r := range rows {
		// 归属地未知的行不参与建议：空串既不是合法省份，填进地区限制
		// 也没有任何意义。
		if r.Province == "" {
			continue
		}
		byProvince[r.Province] += r.ActiveSeconds
		total += r.ActiveSeconds
	}
	if total == 0 {
		return RegionSuggestion{}
	}

	type entry struct {
		province string
		seconds  int
	}
	list := make([]entry, 0, len(byProvince))
	for p, s := range byProvince {
		list = append(list, entry{p, s})
	}
	// 时长降序；时长相同按省份名升序。第二级排序不是可省的——遍历 map
	// 的顺序不定，没有它时两个时长相同的省份谁进 95% 会随机变化。
	sort.Slice(list, func(i, j int) bool {
		if list[i].seconds != list[j].seconds {
			return list[i].seconds > list[j].seconds
		}
		return list[i].province < list[j].province
	})

	acc := 0
	picked := make([]string, 0, len(list))
	for _, e := range list {
		picked = append(picked, e.province)
		acc += e.seconds
		if float64(acc) >= regionSuggestCoverage*float64(total) {
			break
		}
	}
	sort.Strings(picked)

	inCoexist := map[string]bool{}
	for _, p := range computeCoexist(rows).Provinces {
		inCoexist[p] = true
	}
	flagged := make([]string, 0, len(picked))
	for _, p := range picked {
		if inCoexist[p] {
			flagged = append(flagged, p)
		}
	}
	return RegionSuggestion{Suggested: picked, Coexisting: flagged}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run "TestSuggestRegions|TestCoexist" -v`
Expected: 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add web/service/sharing_stat.go web/service/sharing_stat_test.go
git commit -m "feat(sharing): 地区限制建议值

按活跃时长累计覆盖 95%，长尾切掉。并存省份只标出不剔除——面板分不清
买家的省和用户老家常挂的设备，猜错两个方向都是错的，且猜错完全静默。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 4: 采集累加器（纯内存）

**Files:**
- Create: `web/service/sharing_accumulator.go`
- Create: `web/service/sharing_accumulator_test.go`

**Interfaces:**
- Consumes: `model.AlignHourUTC`（Task 1）
- Produces:
  - 常量 `sharingFlushThreshold = 60`、`sharingMaxRowsPerHour = 50`
  - `type sharingObservation struct { InboundId int; IP string; Province string }`
  - `type sharingFlush struct { InboundId int; IP string; Province string; HourStart int64; ActiveSeconds int }`
  - `func newSharingAccumulator() *sharingAccumulator`
  - `func (a *sharingAccumulator) observe(now time.Time, obs []sharingObservation, step int) []sharingFlush`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/sharing_accumulator_test.go`：

```go
package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database/model"
)

func oneObs() []sharingObservation {
	return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Province: "江苏"}}
}

// 入站端口在公网上会被扫。若每个建立过连接的来源都落一行，一次端口扫描
// 就能在一小时内塞进几千行。60 秒门槛把一次性探测挡在外面。
func TestAccumulatorRequiresThresholdBeforeFlush(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	if got := a.observe(base, oneObs(), 30); len(got) != 0 {
		t.Fatalf("累计 30 秒就落库了: %+v", got)
	}
	got := a.observe(base.Add(30*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 60 {
		t.Fatalf("累计 60 秒 = %+v, want 1 条 ActiveSeconds=60", got)
	}
	if got := a.observe(base.Add(60*time.Second), oneObs(), 30); len(got) != 0 {
		t.Fatalf("距上次落库只多 30 秒，不该再落库: %+v", got)
	}
	got = a.observe(base.Add(90*time.Second), oneObs(), 30)
	if len(got) != 1 || got[0].ActiveSeconds != 120 {
		t.Fatalf("累计 120 秒 = %+v, want 1 条 ActiveSeconds=120", got)
	}
}

// 不满门槛的余量跨小时直接丢弃，不结转——结转会让一个每小时只用 30 秒的
// 扫描器攒几轮就绕开门槛。
func TestAccumulatorDiscardsSubThresholdRemainderAcrossHours(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 59, 30, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	if got := a.observe(h10, oneObs(), 30); len(got) != 0 {
		t.Fatalf("不该落库: %+v", got)
	}
	if got := a.observe(h11, oneObs(), 30); len(got) != 0 {
		t.Fatalf("跨小时结转了不满门槛的余量: %+v", got)
	}
}

// 已过门槛但最后一段还没落库的，跨小时时要补一次收尾写入，否则那段时长
// 永远丢失。
func TestAccumulatorFlushesCompletedHourOnRollover(t *testing.T) {
	a := newSharingAccumulator()
	h10 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)

	a.observe(h10, oneObs(), 30)                        // 30
	a.observe(h10.Add(30*time.Second), oneObs(), 30)    // 60，落库
	a.observe(h10.Add(60*time.Second), oneObs(), 30)    // 90，未落库

	got := a.observe(h11, oneObs(), 30)
	if len(got) != 1 {
		t.Fatalf("跨小时 = %+v, want 1 条收尾记录", got)
	}
	if got[0].HourStart != model.AlignHourUTC(h10) {
		t.Errorf("收尾记录落在 %v, want 上一小时 %v", got[0].HourStart, model.AlignHourUTC(h10))
	}
	if got[0].ActiveSeconds != 90 {
		t.Errorf("收尾记录 ActiveSeconds = %v, want 90", got[0].ActiveSeconds)
	}
}

// 上限不是为正常场景设的（正常一小时 2~3 行），是为被针对性刷时让表的
// 大小有确定天花板。
func TestAccumulatorCapsRowsPerInboundPerHour(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	obs := make([]sharingObservation, 0, sharingMaxRowsPerHour+10)
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		obs = append(obs, sharingObservation{
			InboundId: 1,
			IP:        fmt.Sprintf("10.0.%v.%v", i/256, i%256),
			Province:  "江苏",
		})
	}
	a.observe(base, obs, 30)
	got := a.observe(base.Add(30*time.Second), obs, 30)
	if len(got) != sharingMaxRowsPerHour {
		t.Errorf("落库 %v 条, want 上限 %v 条", len(got), sharingMaxRowsPerHour)
	}
}

// 上限只挡新来源，已在累计的继续累计——否则一次扫描就能把真实用户从
// 表里挤掉。
func TestAccumulatorCapDoesNotEvictExistingSources(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	real := sharingObservation{InboundId: 1, IP: "1.1.1.1", Province: "江苏"}

	a.observe(base, []sharingObservation{real}, 30)

	flood := []sharingObservation{real}
	for i := 0; i < sharingMaxRowsPerHour+10; i++ {
		flood = append(flood, sharingObservation{
			InboundId: 1,
			IP:        fmt.Sprintf("10.0.%v.%v", i/256, i%256),
			Province:  "江苏",
		})
	}
	got := a.observe(base.Add(30*time.Second), flood, 30)

	found := false
	for _, f := range got {
		if f.IP == real.IP && f.ActiveSeconds == 60 {
			found = true
		}
	}
	if !found {
		t.Error("真实用户被扫描流量挤掉了")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run TestAccumulator -v`
Expected: FAIL，编译错误 `undefined: newSharingAccumulator`

- [ ] **Step 3: 写实现**

创建 `web/service/sharing_accumulator.go`：

```go
package service

import (
	"sort"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
)

// sharingFlushThreshold 是落库门槛（秒）：单个 (入站, IP, 小时) 累计活跃满
// 这么久才写一行，此后每再满这么久更新一次。
//
// 入站端口在公网上会被扫。若每个建立过 TCP 连接的来源都落一行，一次端口
// 扫描就能在一小时内塞进几千行，保留期压不住，明细页也会被垃圾淹没。
// 一次性探测、误连到不了 60 秒，真实使用轻松超过——这道门槛既压行数，也
// 提高信号质量：并存判定要的本就是「两边都在实质使用」。
const sharingFlushThreshold = 60

// sharingMaxRowsPerHour 是单入站单小时的行数上限。
//
// 不是为正常场景设的（正常一小时 2~3 行），是为被针对性刷时让表的大小有
// 一个确定的天花板：50 × 24 小时 × 30 天 × 约 100 字节 ≈ 3.6 MB/入站。
const sharingMaxRowsPerHour = 50

type sharingKey struct {
	inboundId int
	ip        string
}

type sharingCell struct {
	province  string
	seconds   int // 本小时累计活跃秒数
	flushedAt int // 上次落库时 seconds 的值
}

// sharingObservation 是一轮采样里的一条「这个 IP 此刻正在实质使用这个入站」。
type sharingObservation struct {
	InboundId int
	IP        string
	Province  string
}

// sharingFlush 是一条待写入的记录。
//
// ActiveSeconds 是本小时的**绝对**活跃秒数而非增量，落库走覆盖式 upsert：
// 绝对值天然幂等——一轮写失败下一轮补上即可，不会因为重试把时长记成两倍。
type sharingFlush struct {
	InboundId     int
	IP            string
	Province      string
	HourStart     int64
	ActiveSeconds int
}

// sharingAccumulator 在内存里累计各来源 IP 的活跃时长，满门槛才产出落库项。
//
// 纯内存、不做持久化恢复：内存累计每满门槛就落一次库，面板重启最多丢当前
// 那一分钟，为这点精度引入一套恢复逻辑不划算。
type sharingAccumulator struct {
	mu    sync.Mutex
	hour  int64
	cells map[sharingKey]*sharingCell
}

func newSharingAccumulator() *sharingAccumulator {
	return &sharingAccumulator{cells: map[sharingKey]*sharingCell{}}
}

// observe 记一轮采样：obs 里的每个 IP 都算作在本轮的 step 秒内持续活跃。
// 返回本轮需要落库的记录（可能为空）。
func (a *sharingAccumulator) observe(now time.Time, obs []sharingObservation, step int) []sharingFlush {
	a.mu.Lock()
	defer a.mu.Unlock()

	hour := model.AlignHourUTC(now)
	var out []sharingFlush
	if hour != a.hour {
		out = a.rolloverLocked(hour)
	}

	perInbound := map[int]int{}
	for k := range a.cells {
		perInbound[k.inboundId]++
	}

	for _, o := range obs {
		key := sharingKey{inboundId: o.InboundId, ip: o.IP}
		cell := a.cells[key]
		if cell == nil {
			// 上限只挡新来源，已在累计的继续累计——否则一次扫描就能把
			// 真实用户从表里挤掉。
			if perInbound[o.InboundId] >= sharingMaxRowsPerHour {
				logger.Warningf("入站 %v 本小时来源 IP 已达 %v 个上限，忽略 %v",
					o.InboundId, sharingMaxRowsPerHour, o.IP)
				continue
			}
			cell = &sharingCell{province: o.Province}
			a.cells[key] = cell
			perInbound[o.InboundId]++
		}
		// 省份以最近一次判定为准：归属地库更新后同一个 IP 的判定可能变，
		// 用新的比留着旧的合理。空串不覆盖已知值——一次查库失败不该把
		// 已经判定出来的省份抹掉。
		if o.Province != "" {
			cell.province = o.Province
		}
		cell.seconds += step
		if cell.seconds-cell.flushedAt >= sharingFlushThreshold {
			cell.flushedAt = cell.seconds
			out = append(out, sharingFlush{
				InboundId: key.inboundId, IP: key.ip, Province: cell.province,
				HourStart: hour, ActiveSeconds: cell.seconds,
			})
		}
	}
	return out
}

// rolloverLocked 结束上一个小时：把已过门槛、但最后一段还没落库的单元补写
// 一次，然后清空。
//
// **不满门槛的余量直接丢弃，不跨小时结转**——结转会让一个每小时只用 30 秒
// 的扫描器攒几轮就攒够门槛落库，正好绕开 sharingFlushThreshold。
func (a *sharingAccumulator) rolloverLocked(newHour int64) []sharingFlush {
	var out []sharingFlush
	for key, cell := range a.cells {
		if cell.seconds >= sharingFlushThreshold && cell.seconds > cell.flushedAt {
			out = append(out, sharingFlush{
				InboundId: key.inboundId, IP: key.ip, Province: cell.province,
				HourStart: a.hour, ActiveSeconds: cell.seconds,
			})
		}
	}
	// 上面遍历的是 map，顺序不定。调用方要把这批依次写进库，排序让同一份
	// 输入永远产生同一个写入次序，测试才好断言。
	sort.Slice(out, func(i, j int) bool {
		if out[i].InboundId != out[j].InboundId {
			return out[i].InboundId < out[j].InboundId
		}
		return out[i].IP < out[j].IP
	})
	a.cells = map[sharingKey]*sharingCell{}
	a.hour = newHour
	return out
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run TestAccumulator -v`
Expected: PASS（5 个用例）

- [ ] **Step 5: 提交**

```bash
git add web/service/sharing_accumulator.go web/service/sharing_accumulator_test.go
git commit -m "feat(sharing): 采集累加器

60 秒门槛挡住端口扫描，50 行上限给表的大小一个确定天花板，且只挡新来源
不挤掉已在累计的真实用户。跨小时的余量直接丢弃不结转——结转会让每小时
只用 30 秒的扫描器攒几轮绕开门槛。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 5: 采集落库与采样任务

**Files:**
- Create: `web/service/sharing.go`
- Create: `web/service/sharing_test.go`
- Create: `web/job/sharing_sample_job.go`
- Modify: `web/web.go`（`startTask` 内，`ConcurrencyJob` 注册之后）

**Interfaces:**
- Consumes: `sharingAccumulator`（Task 4）、`model.InboundIPHour`（Task 1）
- Produces:
  - `type SharingService struct{ ... }`
  - 常量 `sharingSampleStep = 30`、`sharingWindowDays = 7`、`sharingRetentionDays = 30`
  - `var sharingAccumulatorInstance *sharingAccumulator`
  - `func (s *SharingService) Sample(now time.Time) error`
  - `func upsertIPHour(db *gorm.DB, f sharingFlush) error`
  - `func NewSharingSampleJob() *SharingSampleJob`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/sharing_test.go`：

```go
package service

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// setupSharingTest 建一对全新的临时库，与 setupTrafficTest 同一个模式。
func setupSharingTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
}

func listIPHours(t *testing.T) []model.InboundIPHour {
	t.Helper()
	var rows []model.InboundIPHour
	err := database.GetTrafficDB().
		Order("hour_start asc, inbound_id asc, ip asc").
		Find(&rows).Error
	if err != nil {
		t.Fatalf("查询小时桶: %v", err)
	}
	return rows
}

// upsert 必须是覆盖而不是累加：sharingFlush 带的是绝对值，累加会让同一
// 小时被写两次后时长翻倍。
func TestUpsertIPHourOverwritesInsteadOfAccumulating(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()

	f := sharingFlush{
		InboundId: 1, IP: "1.1.1.1", Province: "江苏",
		HourStart: 3600, ActiveSeconds: 60,
	}
	if err := upsertIPHour(db, f); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	f.ActiveSeconds = 120
	if err := upsertIPHour(db, f); err != nil {
		t.Fatalf("二次写入: %v", err)
	}

	rows := listIPHours(t)
	if len(rows) != 1 {
		t.Fatalf("行数 = %v, want 1（唯一索引应让第二次写入落到同一行）", len(rows))
	}
	if rows[0].ActiveSeconds != 120 {
		t.Errorf("ActiveSeconds = %v, want 120（覆盖而非累加）", rows[0].ActiveSeconds)
	}
}

func TestUpsertIPHourKeepsDistinctHoursSeparate(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()

	base := sharingFlush{InboundId: 1, IP: "1.1.1.1", Province: "江苏", ActiveSeconds: 60}
	base.HourStart = 3600
	if err := upsertIPHour(db, base); err != nil {
		t.Fatalf("写入第一小时: %v", err)
	}
	base.HourStart = 7200
	if err := upsertIPHour(db, base); err != nil {
		t.Fatalf("写入第二小时: %v", err)
	}
	if got := len(listIPHours(t)); got != 2 {
		t.Errorf("行数 = %v, want 2", got)
	}
}

// 库没打开时静默返回：面板启动时 InitTrafficDB 失败就是这个状态，
// 共享检测不可用不该让采样任务每 30 秒报一次错。
func TestSampleReturnsNilWhenTrafficDBUnavailable(t *testing.T) {
	database.ResetTrafficDBForTest()
	svc := SharingService{}
	if err := svc.Sample(time.Now()); err != nil {
		t.Errorf("库不可用时 Sample 应返回 nil, got %v", err)
	}
}
```

> **注意**：`TestSampleReturnsNilWhenTrafficDBUnavailable` 会把包级的 traffic 库句柄置空，可能影响同包内后续测试。`database.ResetTrafficDBForTest()` 已存在（`database/db.go:232`），而其它测试都在自己的 `setup*` 里重新 `InitTrafficDB`，所以安全。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run "TestUpsertIPHour|TestSampleReturnsNil" -v`
Expected: FAIL，编译错误 `undefined: upsertIPHour` / `undefined: SharingService`

- [ ] **Step 3: 写 service**

创建 `web/service/sharing.go`：

```go
package service

import (
	"net"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/netdiag"
)

// sharingSampleStep 是采样间隔（秒）。**必须与 SharingSampleJob 的 cron
// 表达式一致**：累加器按它折算活跃时长，两边不一致会让记录的时长系统性
// 偏大或偏小，而没有任何一层会报错。
const sharingSampleStep = 30

// sharingWindowDays 是并存判定的窗口。
//
// 够长能看出持续并存，够短不会让三个月前的一次旅游一直挂在告警上。
const sharingWindowDays = 7

// sharingRetentionDays 是行的保留期，比判定窗口长。
//
// 多出来的部分供明细页回溯，判断「一直在共享」还是「上个月出了趟差」。
// 是常量而非设置项——这张表的消费者只有本功能，多一个设置项就要同步改 5 处，
// 漏掉 models.js 会让整个保存配置接口失败，为一份自愈的辅助数据不值得。
const sharingRetentionDays = 30

// sharingAccumulatorInstance 是包级累加器，与 onlineTrackerInstance 同理：
// 采集是跨请求的持续过程，状态不能挂在无状态的 service 上。
var sharingAccumulatorInstance = newSharingAccumulator()

// SharingService 负责共享检测的采集、查询与清理。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type SharingService struct {
	onlineService  OnlineService
	inboundService InboundService
	ipdbService    IPDBService
	settingService SettingService
}

// Sample 采一轮，把到门槛的活跃时长写进库。
//
// 它自己驱动 OnlineService.sample()，**不依赖并发判定**：
// ConcurrencyService.Enforce 在「无人设并发额度且无封禁」时提前返回，一次
// 系统调用都不做（web/service/concurrency.go:108）——把采集挂在它后面的话，
// 检测在最常见的默认配置下会无声失效。sample() 内部有最小采样间隔去重，
// 两条路径共存不会重复读连接表。
func (s *SharingService) Sample(now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil {
		// 库没打开：面板启动时 InitTrafficDB 失败就是这个状态。共享检测
		// 不可用不该让这个任务每 30 秒报一次错。
		return nil
	}
	if !netdiag.Supported {
		return netdiag.ErrUnsupported
	}
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return err
	}
	if err := s.onlineService.sample(); err != nil {
		return err
	}

	idle := time.Duration(sharingSampleStep) * time.Second
	var obs []sharingObservation
	for _, in := range inbounds {
		if !in.Enable {
			continue
		}
		for _, e := range onlineTrackerInstance.snapshotIdle(in.Port, noLocate, idle) {
			// 只收实质活跃的来源。Idle 表示连接还在但这段时间没有任何
			// 字节往来——纯扫描、失败握手不产生字节增长，天然被挡在外面。
			if e.Idle {
				continue
			}
			obs = append(obs, sharingObservation{
				InboundId: in.Id,
				IP:        e.IP,
				Province:  s.provinceOf(e.IP),
			})
		}
	}

	flushes := sharingAccumulatorInstance.observe(now, obs, sharingSampleStep)
	if len(flushes) == 0 {
		return nil
	}
	// 包成一个事务，理由与 TrafficHistoryService.Record 相同：GORM 的
	// SkipDefaultTransaction 默认为 false，不包的话每次 Create 自带一个
	// BEGIN...COMMIT，一轮就是 N 次独立提交，而这块盘还要同时服务主库
	// 和访问日志库。
	return db.Transaction(func(tx *gorm.DB) error {
		for _, f := range flushes {
			if err := upsertIPHour(tx, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// provinceOf 返回主判定省份，查不到时返回空串。
//
// 多个数据源对同一个 IP 可能给出不同省份，取第一个非空的：Sources() 的
// 顺序是固定的，所以同一份库对同一个 IP 永远给出同一个答案。这里不能用
// Multi 的并集语义——那是给地区限制放行用的，而一个 IP 不可能同时属于
// 两个省，并集在这里没有意义。
//
// IPv6 恒返回空串：ipdb 只收录 IPv4（util/ipdb/ipdb.go:64）。
func (s *SharingService) provinceOf(ipStr string) string {
	db := s.ipdbService.DB()
	if db == nil {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	for _, sl := range db.Lookup(ip) {
		if sl.Location.Region != "" {
			return sl.Location.Region
		}
	}
	return ""
}

// upsertIPHour 覆盖式写入一行。
//
// DoUpdates 用覆盖而不是 gorm.Expr 累加（与 upsertBucket 相反）：sharingFlush
// 带的是本小时的**绝对**活跃秒数。覆盖天然幂等——一轮写失败下一轮补上即可，
// 不会因为重试把时长记成两倍。
func upsertIPHour(db *gorm.DB, f sharingFlush) error {
	row := &model.InboundIPHour{
		InboundId: f.InboundId, IP: f.IP, HourStart: f.HourStart,
		Province: f.Province, ActiveSeconds: f.ActiveSeconds,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "inbound_id"}, {Name: "ip"}, {Name: "hour_start"},
		},
		DoUpdates: clause.AssignmentColumns([]string{"province", "active_seconds"}),
	}).Create(row).Error
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run "TestUpsertIPHour|TestSampleReturnsNil" -v`
Expected: PASS

- [ ] **Step 5: 写采样任务**

创建 `web/job/sharing_sample_job.go`：

```go
package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/netdiag"
	"a-ui/web/service"
)

// SharingSampleJob 每轮采一次各入站的活跃来源 IP，喂给共享检测。
//
// 独立于 ConcurrencyJob：后者在「无人设并发额度且无封禁」时一次系统调用
// 都不做，把采集挂在它后面的话，检测在最常见的默认配置下会无声失效。
type SharingSampleJob struct {
	sharingService service.SharingService

	// 平台不支持时只提示一次，否则每 30 秒刷一条日志。与 ConcurrencyJob
	// 同一个处理方式。
	unsupportedWarned bool
}

func NewSharingSampleJob() *SharingSampleJob {
	return new(SharingSampleJob)
}

func (j *SharingSampleJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层：日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("共享检测采样任务")

	err := j.sharingService.Sample(time.Now())
	if err == nil {
		return
	}
	if err == netdiag.ErrUnsupported {
		if !j.unsupportedWarned {
			j.unsupportedWarned = true
			logger.Warning("当前系统不支持内核连接表，共享检测不会生效:", err)
		}
		return
	}
	// 采集失败只告警，绝不阻断任何既有流程：检测是辅助手段。
	logger.Warning("共享检测采样失败:", err)
}
```

- [ ] **Step 6: 注册任务**

在 `web/web.go` 的 `startTask` 里，找到并发判定的注册（约 `:323`）：

```go
	s.cron.AddJob("@every 1s", job.NewConcurrencyJob())
```

在它之后插入：

```go
	// 每 30 秒采一次活跃来源 IP 供共享检测使用。刻意不复用并发判定那次
	// 采样：Enforce 在无人设并发额度时一次系统调用都不做，挂在它后面
	// 会让检测在默认配置下静默失效。间隔必须与 service.sharingSampleStep
	// 一致，累加器按它折算活跃时长。
	s.cron.AddJob("@every 30s", job.NewSharingSampleJob())
```

- [ ] **Step 7: 全量验证**

Run: `make verify`
Expected: vet 干净、测试全过、构建成功

- [ ] **Step 8: 提交**

```bash
git add web/service/sharing.go web/service/sharing_test.go web/job/sharing_sample_job.go web/web.go
git commit -m "feat(sharing): 采集落库与 30 秒采样任务

采样任务独立于 ConcurrencyJob：后者在无人设并发额度且无封禁时一次系统
调用都不做，把采集挂在它后面会让检测在最常见的默认配置下静默失效。

upsert 走覆盖而非累加：flush 带的是绝对活跃秒数，覆盖天然幂等，重试不会
把时长记成两倍。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 6: 清理三道

**Files:**
- Modify: `web/service/sharing.go`（追加三个方法）
- Modify: `web/service/sharing_test.go`（追加）
- Modify: `web/service/inbound.go`（`DelInbound`，约 `:158-160` 之后）
- Modify: `web/job/traffic_cleanup_job.go`

**Interfaces:**
- Consumes: `SharingService`（Task 5）
- Produces:
  - `func (s *SharingService) Cleanup(now time.Time) (int64, error)`
  - `func (s *SharingService) PruneOrphans() (int64, error)`
  - `func (s *SharingService) DeleteByInbound(inboundId int) error`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/sharing_test.go`：

```go
func mkSharingInbound(t *testing.T, port int, remark string) *model.Inbound {
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

func TestCleanupDropsOnlyExpiredRows(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	fresh := now.Add(-24 * time.Hour)
	stale := now.Add(-(sharingRetentionDays + 1) * 24 * time.Hour)
	for _, at := range []time.Time{fresh, stale} {
		f := sharingFlush{
			InboundId: 1, IP: "1.1.1.1", Province: "江苏",
			HourStart: model.AlignHourUTC(at), ActiveSeconds: 60,
		}
		if err := upsertIPHour(db, f); err != nil {
			t.Fatalf("写入: %v", err)
		}
	}

	svc := SharingService{}
	deleted, err := svc.Cleanup(now)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 1 {
		t.Errorf("删除 %v 行, want 1", deleted)
	}
	rows := listIPHours(t)
	if len(rows) != 1 || rows[0].HourStart != model.AlignHourUTC(fresh) {
		t.Errorf("剩余行 = %+v, want 只剩窗口内那一行", rows)
	}
}

// SQLite 会复用被删除的自增 id。残留的行会绑到下一个建出来的入站上，那时
// 引用不再悬空，界面会渲染得非常合理——只是显示的是别人的并存记录。
// 这一道必须在 DelInbound 里同步做，不能只靠每小时一次的兜底。
func TestDelInboundRemovesItsSharingRows(t *testing.T) {
	setupSharingTest(t)
	in := mkSharingInbound(t, 31001, "甲")
	db := database.GetTrafficDB()
	f := sharingFlush{
		InboundId: in.Id, IP: "1.1.1.1", Province: "江苏",
		HourStart: 3600, ActiveSeconds: 60,
	}
	if err := upsertIPHour(db, f); err != nil {
		t.Fatalf("写入: %v", err)
	}

	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	if got := len(listIPHours(t)); got != 0 {
		t.Errorf("删除入站后残留 %v 行并存记录", got)
	}
}

func TestPruneOrphansRemovesRowsOfDeletedInbounds(t *testing.T) {
	setupSharingTest(t)
	in := mkSharingInbound(t, 31002, "甲")
	db := database.GetTrafficDB()
	for _, id := range []int{in.Id, in.Id + 999} { // 后者是不存在的入站
		f := sharingFlush{
			InboundId: id, IP: "1.1.1.1", Province: "江苏",
			HourStart: 3600, ActiveSeconds: 60,
		}
		if err := upsertIPHour(db, f); err != nil {
			t.Fatalf("写入: %v", err)
		}
	}

	svc := SharingService{}
	pruned, err := svc.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Errorf("清理 %v 行, want 1", pruned)
	}
	rows := listIPHours(t)
	if len(rows) != 1 || rows[0].InboundId != in.Id {
		t.Errorf("剩余行 = %+v, want 只剩存在的那个入站", rows)
	}
}
```

同时在 `web/service/sharing_test.go` 的 import 里补上 `"fmt"`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run "TestCleanupDrops|TestDelInboundRemovesItsSharing|TestPruneOrphansRemovesRows" -v`
Expected: FAIL，编译错误 `svc.Cleanup undefined`

- [ ] **Step 3: 写清理方法**

追加到 `web/service/sharing.go` 末尾：

```go
// Cleanup 删除早于保留期的行，返回删除行数。
//
// 条件只有 hour_start 一个——这张表只有一种粒度，不存在 TrafficBucket 那个
// 「不带 granularity 就会把日桶一起删掉」的坑。
func (s *SharingService) Cleanup(now time.Time) (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(sharingRetentionDays) * 24 * time.Hour).Unix()
	result := db.Where("hour_start < ?", cutoff).Delete(&model.InboundIPHour{})
	return result.RowsAffected, result.Error
}

// PruneOrphans 删除已不存在的入站遗留的行，返回删除行数。
//
// 这是第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留的行会绑到下一个建出来的入站上，那时
// 引用不再悬空，界面会渲染得非常合理，只是显示的是别人的并存记录。
func (s *SharingService) PruneOrphans() (int64, error) {
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
	result := tx.Delete(&model.InboundIPHour{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部并存记录。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *SharingService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.InboundIPHour{}).Error
}
```

- [ ] **Step 4: 接进 DelInbound**

在 `web/service/inbound.go` 的 `DelInbound` 里，找到用量历史那一段（约 `:158-160`）：

```go
	if err := (&TrafficHistoryService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的用量历史失败, 将由定时清理兜底, id:", id, "err:", err)
	}
```

在它之后插入：

```go
	// 共享检测的并存记录同样按入站 id 存，同样会被 id 复用坑到：不清的话
	// 下一个建出来的入站会继承上一个用户的并存记录，被标成「疑似共享」。
	// 失败只告警不阻断，理由同上，残留由每小时一次的 PruneOrphans 兜底。
	if err := (&SharingService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的共享检测记录失败, 将由定时清理兜底, id:", id, "err:", err)
	}
```

- [ ] **Step 5: 接进清理任务**

在 `web/job/traffic_cleanup_job.go` 中：

先给结构体加一个字段：

```go
type TrafficCleanupJob struct {
	trafficService service.TrafficHistoryService
	settingService service.SettingService
	sharingService service.SharingService
}
```

再在 `Run` 的 `PruneOrphans` 那一段**之前**插入：

```go
	// 共享检测的行与用量桶同库，清理挂在同一个任务里：再开一个每小时任务
	// 只是多一份注册与一份 panic 面。保留期是常量，不读设置。
	if deleted, err := j.sharingService.Cleanup(now); err != nil {
		logger.Warning("清理过期共享检测记录失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期共享检测记录", deleted)
	}

	if pruned, err := j.sharingService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿共享检测记录失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的共享检测记录", pruned)
	}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./web/service/ -run "TestCleanupDrops|TestDelInboundRemovesItsSharing|TestPruneOrphansRemovesRows" -v`
Expected: PASS（3 个用例）

- [ ] **Step 7: 全量验证**

Run: `make verify`
Expected: 全绿

- [ ] **Step 8: 提交**

```bash
git add web/service/sharing.go web/service/sharing_test.go web/service/inbound.go web/job/traffic_cleanup_job.go
git commit -m "feat(sharing): 保留期清理、孤儿兜底与删除入站连带清理

三道缺一不可。DelInbound 那道不能只靠每小时的兜底：SQLite 复用自增 id，
这一小时内新建的入站会继承上一个用户的并存记录，而且引用不再悬空，
界面会渲染得完全合理，只是显示的是别人的数据。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 7: 查询与 HTTP 接口

**Files:**
- Modify: `web/service/sharing.go`（追加查询方法与结果类型）
- Modify: `web/service/sharing_test.go`（追加）
- Modify: `web/controller/inbound.go`

**Interfaces:**
- Consumes: `computeCoexist`、`suggestRegions`（Task 2/3）、`SharingService`（Task 5）
- Produces:
  - `func (s *SharingService) Summary(now time.Time) (map[int]CoexistStat, error)`
  - `func (s *SharingService) Detail(inboundId int, now time.Time) (*SharingDetail, error)`
  - `type SharingDetail struct { Stat CoexistStat; Suggestion RegionSuggestion; Hours []SharingDetailHour }`
  - `type SharingDetailHour struct { HourStart int64; Label string; Entries []SharingDetailEntry }`
  - `type SharingDetailEntry struct { IP, Province string; ActiveSeconds int }`
  - 路由 `POST /aui/inbound/sharing/summary`、`POST /aui/inbound/sharing/detail/:id`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/sharing_test.go`：

```go
// Summary 只返回达到显示下限的入站：低于下限的是旅游迁移交界处的噪声，
// 报出来会让告警变成满屏黄标。
func TestSummaryOnlyReturnsFlaggedInbounds(t *testing.T) {
	setupSharingTest(t)
	loud := mkSharingInbound(t, 31010, "并存很多")
	quiet := mkSharingInbound(t, 31011, "只有一小时")
	db := database.GetTrafficDB()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	// loud：coexistDisplayMinHours 个小时都有两省并存
	for i := 0; i < coexistDisplayMinHours; i++ {
		at := now.Add(-time.Duration(i+1) * time.Hour)
		for ip, province := range map[string]string{"1.1.1.1": "江苏", "2.2.2.2": "上海"} {
			f := sharingFlush{
				InboundId: loud.Id, IP: ip, Province: province,
				HourStart: model.AlignHourUTC(at), ActiveSeconds: 3600,
			}
			if err := upsertIPHour(db, f); err != nil {
				t.Fatalf("写入: %v", err)
			}
		}
	}
	// quiet：只有一个小时并存，低于下限
	at := now.Add(-time.Hour)
	for ip, province := range map[string]string{"3.3.3.3": "江苏", "4.4.4.4": "上海"} {
		f := sharingFlush{
			InboundId: quiet.Id, IP: ip, Province: province,
			HourStart: model.AlignHourUTC(at), ActiveSeconds: 3600,
		}
		if err := upsertIPHour(db, f); err != nil {
			t.Fatalf("写入: %v", err)
		}
	}

	svc := SharingService{}
	got, err := svc.Summary(now)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if _, ok := got[quiet.Id]; ok {
		t.Error("低于显示下限的入站不该出现在 Summary 里")
	}
	stat, ok := got[loud.Id]
	if !ok {
		t.Fatal("达到下限的入站应出现在 Summary 里")
	}
	if stat.Hours != coexistDisplayMinHours {
		t.Errorf("Hours = %v, want %v", stat.Hours, coexistDisplayMinHours)
	}
}

// 窗口外的行不参与判定，但仍在保留期内供明细回溯。
func TestSummaryIgnoresRowsOutsideWindow(t *testing.T) {
	setupSharingTest(t)
	in := mkSharingInbound(t, 31012, "旧数据")
	db := database.GetTrafficDB()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-(sharingWindowDays + 1) * 24 * time.Hour)

	for ip, province := range map[string]string{"1.1.1.1": "江苏", "2.2.2.2": "上海"} {
		f := sharingFlush{
			InboundId: in.Id, IP: ip, Province: province,
			HourStart: model.AlignHourUTC(old), ActiveSeconds: 3600,
		}
		if err := upsertIPHour(db, f); err != nil {
			t.Fatalf("写入: %v", err)
		}
	}

	svc := SharingService{}
	got, err := svc.Summary(now)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if _, ok := got[in.Id]; ok {
		t.Error("窗口外的行不该参与判定")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run TestSummary -v`
Expected: FAIL，`svc.Summary undefined`

- [ ] **Step 3: 写查询方法**

追加到 `web/service/sharing.go` 末尾：

```go
// SharingDetailEntry 是明细里的一条：某个 IP 在某小时的活跃情况。
type SharingDetailEntry struct {
	IP            string `json:"ip"`
	Province      string `json:"province"`
	ActiveSeconds int    `json:"activeSeconds"`
}

// SharingDetailHour 是明细里的一个小时。
type SharingDetailHour struct {
	HourStart int64  `json:"t"`
	// Label 在**服务端**按面板时区格式化。让浏览器自己格式化的话，访问者
	// 所在时区一变，明细上的时间就和面板设置的时区对不上了——用量图表那边
	// 也是这个理由。
	Label    string               `json:"label"`
	Coexists bool                 `json:"coexists"`
	Entries  []SharingDetailEntry `json:"entries"`
}

// SharingDetail 是某入站的共享检测明细。
type SharingDetail struct {
	Stat       CoexistStat         `json:"stat"`
	Suggestion RegionSuggestion    `json:"suggestion"`
	// Hours 按时间倒序，只含发生过并存的小时——全都列出来的话，一个正常
	// 用户 30 天有几百个小时，管理员要找的那几行会被淹掉。
	Hours []SharingDetailHour `json:"hours"`
	// WindowDays 与 RetentionDays 下发给前端做文案，避免两边各写一份常量
	// 然后慢慢漂移。
	WindowDays    int `json:"windowDays"`
	RetentionDays int `json:"retentionDays"`
}

// windowRows 读某入站在给定天数内的行。inboundId 为 0 表示读全部入站。
func (s *SharingService) windowRows(inboundId, days int, now time.Time) ([]model.InboundIPHour, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return nil, nil
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	tx := db.Where("hour_start >= ?", cutoff)
	if inboundId != 0 {
		tx = tx.Where("inbound_id = ?", inboundId)
	}
	var rows []model.InboundIPHour
	err := tx.Order("hour_start asc, ip asc").Find(&rows).Error
	return rows, err
}

// Summary 返回各入站的并存统计，只含达到显示下限的。
//
// 低于下限的不返回：那是旅游迁移交界处的噪声，报出来会让入站列表变成
// 满屏黄标，告警就此失去意义。
func (s *SharingService) Summary(now time.Time) (map[int]CoexistStat, error) {
	rows, err := s.windowRows(0, sharingWindowDays, now)
	if err != nil {
		return nil, err
	}
	byInbound := map[int][]model.InboundIPHour{}
	for _, r := range rows {
		byInbound[r.InboundId] = append(byInbound[r.InboundId], r)
	}
	out := map[int]CoexistStat{}
	for id, list := range byInbound {
		if stat := computeCoexist(list); stat.Flagged() {
			out[id] = stat
		}
	}
	return out, nil
}

// Detail 返回某入站的明细：判定窗口内的统计与建议，加上保留期内的并存时段。
func (s *SharingService) Detail(inboundId int, now time.Time) (*SharingDetail, error) {
	windowRows, err := s.windowRows(inboundId, sharingWindowDays, now)
	if err != nil {
		return nil, err
	}
	// 明细比判定窗口看得更远：多出来的那段是用来判断「一直在共享」还是
	// 「上个月出了趟差」的。
	historyRows, err := s.windowRows(inboundId, sharingRetentionDays, now)
	if err != nil {
		return nil, err
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}

	detail := &SharingDetail{
		Stat:          computeCoexist(windowRows),
		Suggestion:    suggestRegions(windowRows),
		Hours:         []SharingDetailHour{},
		WindowDays:    sharingWindowDays,
		RetentionDays: sharingRetentionDays,
	}

	byHour := map[int64][]model.InboundIPHour{}
	order := make([]int64, 0)
	for _, r := range historyRows {
		if _, seen := byHour[r.HourStart]; !seen {
			order = append(order, r.HourStart)
		}
		byHour[r.HourStart] = append(byHour[r.HourStart], r)
	}
	// historyRows 已按 hour_start 升序，这里倒过来给前端：最近的排最前。
	for i := len(order) - 1; i >= 0; i-- {
		hour := order[i]
		list := byHour[hour]
		// 只列发生过并存的小时。全都列出来的话，一个正常用户 30 天有几百
		// 个小时，管理员要找的那几行会被淹掉。
		if computeCoexist(list).Hours == 0 {
			continue
		}
		entries := make([]SharingDetailEntry, 0, len(list))
		for _, r := range list {
			entries = append(entries, SharingDetailEntry{
				IP: r.IP, Province: r.Province, ActiveSeconds: r.ActiveSeconds,
			})
		}
		detail.Hours = append(detail.Hours, SharingDetailHour{
			HourStart: hour,
			Label:     time.Unix(hour, 0).In(loc).Format("2006-01-02 15:04"),
			Coexists:  true,
			Entries:   entries,
		})
	}
	return detail, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run TestSummary -v`
Expected: PASS（2 个用例）

- [ ] **Step 5: 加接口**

在 `web/controller/inbound.go` 中：

给结构体加字段（在 `trafficHistoryService` 那一行之后）：

```go
	sharingService service.SharingService
```

在 `initRouter` 里，紧跟 `g.POST("/traffic/overview", a.getTrafficOverview)` 之后加两行：

```go
	g.POST("/sharing/summary", a.getSharingSummary)
	g.POST("/sharing/detail/:id", a.getSharingDetail)
```

在文件末尾追加两个 handler：

```go
// getSharingSummary 返回各入站的并存统计。
//
// 刻意不塞进入站列表主接口：不改已有接口契约，而且这样天然 fail open——
// 这个查询失败时列表照常渲染，只是没有并存标记。反过来把聚合塞进列表主
// 接口，一次慢查询就能让整个入站列表打不开。
func (a *InboundController) getSharingSummary(c *gin.Context) {
	result, err := a.sharingService.Summary(time.Now())
	if err != nil {
		jsonMsg(c, "获取共享检测统计", err)
		return
	}
	jsonObj(c, result, nil)
}

// getSharingDetail 返回某入站的共享检测明细。
func (a *InboundController) getSharingDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取共享检测明细", err)
		return
	}
	result, err := a.sharingService.Detail(id, time.Now())
	if err != nil {
		jsonMsg(c, "获取共享检测明细", err)
		return
	}
	jsonObj(c, result, nil)
}
```

> 若 `strconv` / `time` 尚未 import，补上。参照同文件的 `getTrafficHistory` 怎么解析 `:id`，保持一致。

- [ ] **Step 6: 全量验证**

Run: `make verify`
Expected: 全绿

- [ ] **Step 7: 提交**

```bash
git add web/service/sharing.go web/service/sharing_test.go web/controller/inbound.go
git commit -m "feat(sharing): 并存统计与明细接口

两个新接口而不是往入站列表主接口塞字段：不改已有契约，且天然 fail open
——聚合查询失败时列表照常渲染，只是没有标记。时间标签在服务端按面板时区
格式化，让浏览器格式化的话访问者换个时区就和面板对不上了。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

### Task 8: 面板呈现

**Files:**
- Create: `web/html/xui/sharing_modal.html`
- Modify: `web/html/xui/inbounds.html`

**Interfaces:**
- Consumes: `POST /aui/inbound/sharing/summary`、`POST /aui/inbound/sharing/detail/:id`（Task 7）
- Produces: 全局对象 `sharingModal`，由 `inbounds.html` 调 `sharingModal.show(dbInbound)` 打开

- [ ] **Step 1: 写 modal**

创建 `web/html/xui/sharing_modal.html`。照 `access_log_modal.html` 的模式：自带根元素与 `new Vue`。

```html
{{define "sharingModal"}}
<a-modal id="sharing-modal" v-model="sharingModal.visible" :title="sharingModal.title"
         :closable="true" :mask-closable="true" width="900px" :footer="null">
    <a-alert v-if="sharingModal.stat.byIp" type="warning" show-icon style="margin-bottom: 12px"
             message="当前只能按 IP 判定，不是按地区"
             description="IP 归属地库未加载，或该入站的来源都是 IPv6（归属地库只收录 IPv4）。这种情况下同一个人的手机与宽带会被算成两个来源，误报率远高于按省份判定。">
    </a-alert>

    <a-descriptions size="small" bordered :column="2" style="margin-bottom: 12px">
        <a-descriptions-item label="并存时长">
            <a-tag color="orange">[[ sharingModal.stat.hours ]] 小时</a-tag>
            <span style="color: rgba(0,0,0,.45)">最近 [[ sharingModal.windowDays ]] 天</span>
        </a-descriptions-item>
        <a-descriptions-item :label="sharingModal.stat.byIp ? '涉及来源' : '涉及地区'">
            <template v-if="sharingModal.stat.byIp">[[ sharingModal.stat.ips ]] 个 IP</template>
            <template v-else>
                <a-tag v-for="p in sharingModal.stat.provinces" :key="p" color="red">[[ p ]]</a-tag>
            </template>
        </a-descriptions-item>
    </a-descriptions>

    <a-card size="small" title="地区限制建议" style="margin-bottom: 12px"
            v-if="sharingModal.suggestion.suggested.length > 0">
        <template slot="extra">
            <a-button type="primary" size="small" @click="sharingModal.adopt()">填入地区限制</a-button>
        </template>
        <p style="margin-bottom: 8px">
            按最近 [[ sharingModal.windowDays ]] 天的活跃时长，覆盖 95% 的地区是：
            <a-tag v-for="p in sharingModal.suggestion.suggested" :key="p"
                   :color="sharingModal.isCoexisting(p) ? 'red' : 'purple'">[[ p ]]</a-tag>
        </p>
        <a-alert v-if="sharingModal.suggestion.coexisting.length > 0" type="error" show-icon
                 message="标红的地区出现在并存记录里，请先判断再采纳"
                 description="如果这个节点确实正在被共享，直接采纳会把对方所在的地区一并放行——等于把共享合法化，而面板不会有任何提示。面板分不清这是买家还是你自己老家常挂的设备，所以只标出来，由你决定删掉哪个。">
        </a-alert>
        <a-alert v-else type="info" show-icon
                 message="采纳后该入站将只监听 IPv4"
                 description="启用地区限制会让入站强制监听 0.0.0.0，通过 IPv6 连接的客户端会立刻连不上。确认保存前请留意这一点。">
        </a-alert>
    </a-card>

    <a-table :columns="sharingModal.columns" :data-source="sharingModal.hours"
             :row-key="h => h.t" :pagination="{ pageSize: 10 }" size="small">
        <template slot="label" slot-scope="text, h">[[ h.label ]]</template>
        <template slot="entries" slot-scope="text, h">
            <a-tag v-for="e in h.entries" :key="e.ip" :color="e.province ? 'red' : 'default'">
                [[ e.province || '归属地未知' ]] · [[ e.ip ]] · [[ sharingModal.minutes(e.activeSeconds) ]]
            </a-tag>
        </template>
    </a-table>
    <p style="margin-top: 8px; color: rgba(0,0,0,.45)">
        只列出发生过并存的小时，最多回溯 [[ sharingModal.retentionDays ]] 天。时间按面板设置的时区显示。
    </p>

    <!-- 设计文档 §8/§9 要求这几条必须出现在界面上，不能只写在文档里：
         管理员据此判断的是要不要停掉一个人的服务，判据的边界必须和判据
         一起摆在他面前。 -->
    <a-collapse :bordered="false" style="margin-top: 8px">
        <a-collapse-panel key="limits" header="这个判定能说明什么、不能说明什么">
            <p><b>并存不等于转卖。</b>运营商出口会漂移——手机在一小时内从一个省的出口切到另一个省，
                是真实存在的误报源。你自己在老家常挂一台设备、人在外地用手机，看起来也和转卖一模一样。
                所以这里只报事实，不做任何自动处置，判断权在你。</p>
            <p><b>错峰共享检测不到。</b>白天你用、晚上他用，落在不同小时，不构成并存。
                抓这种情况需要「一段时间内去重 IP 数」之类的指标，而正常用户 7 天十几个 IP、
                横跨两三个省是常态，误报率高到没法用。</p>
            <p><b>更根本的一条：</b>vmess / vless 的凭证就是一个 UUID，拿到就能复制，
                技术上做不到「确保只有你自己用」。能做的是抬高共享成本、让共享无法隐藏：
                并发限制让共享难用，限速让共享不值，这个检测让共享被看见。
                任何声称能「杜绝」的做法，实际效果都只是给不懂技术的人制造麻烦，
                代价却是你自己的正常用户被误伤。</p>
        </a-collapse-panel>
    </a-collapse>
</a-modal>
<script>

    const sharingModal = {
        visible: false,
        title: '',
        inboundId: 0,
        stat: { hours: 0, provinces: [], byIp: false, ips: 0 },
        suggestion: { suggested: [], coexisting: [] },
        hours: [],
        windowDays: 7,
        retentionDays: 30,
        columns: [
            { title: '时段', align: 'left', width: 160, scopedSlots: { customRender: 'label' } },
            { title: '同时活跃的来源', align: 'left', scopedSlots: { customRender: 'entries' } },
        ],

        isCoexisting(province) {
            return this.suggestion.coexisting.indexOf(province) >= 0;
        },

        minutes(seconds) {
            const m = Math.round(seconds / 60);
            return m > 0 ? m + ' 分钟' : '不足 1 分钟';
        },

        async show(dbInbound) {
            this.inboundId = dbInbound.id;
            this.title = '共享检测 - ' + dbInbound.remark;
            this.hours = [];
            this.visible = true;
            const msg = await HttpUtil.post('/aui/inbound/sharing/detail/' + dbInbound.id);
            if (!msg.success) {
                return;
            }
            this.stat = msg.obj.stat;
            this.suggestion = msg.obj.suggestion;
            this.hours = msg.obj.hours;
            this.windowDays = msg.obj.windowDays;
            this.retentionDays = msg.obj.retentionDays;
        },

        // 只把建议填进编辑框，不直接保存：管理员确认后走现有的保存流程，
        // 复用 EncodeRegionsStrict 的校验与整条配置生成链，不新开写入路径。
        adopt() {
            this.visible = false;
            window.dispatchEvent(new CustomEvent('sharing-adopt-regions', {
                detail: { inboundId: this.inboundId, regions: this.suggestion.suggested.slice() },
            }));
        },
    };

    new Vue({
        delimiters: ['[[', ']]'],
        el: '#sharing-modal',
        data: { sharingModal },
    });

</script>
{{end}}
```

- [ ] **Step 2: 引入 modal 并加列标记**

在 `web/html/xui/inbounds.html` 末尾的模板引入区（约 `:821-827`），在 `{{template "accessLogModal"}}` 之后加：

```
{{template "sharingModal"}}
```

把「地区」列的 slot（`:204-209`）改成：

```html
                            <template slot="regions" slot-scope="text, dbInbound">
                                <template v-if="dbInbound.regionList.length > 0">
                                    <a-tag v-for="r in dbInbound.regionList" :key="r" color="purple">[[ r ]]</a-tag>
                                </template>
                                <a-tag v-else color="green">不限制</a-tag>
                                <!-- 并存标记只报事实、不做分级：分级等于面板替管理员
                                     下判断，而阈值调不好告警就变噪声。 -->
                                <a-tooltip v-if="coexistOf(dbInbound.id)">
                                    <template slot="title">
                                        最近 7 天有 [[ coexistOf(dbInbound.id).hours ]] 小时，
                                        <template v-if="coexistOf(dbInbound.id).byIp">
                                            不同 IP 在同时使用这个节点（归属地库未加载，只能按 IP 判定）
                                        </template>
                                        <template v-else>
                                            不同地区的 IP 在同时使用这个节点
                                        </template>
                                        。点击查看明细。
                                    </template>
                                    <a-tag color="orange" style="cursor: pointer"
                                           @click="sharingModal.show(dbInbound)">
                                        并存 [[ coexistOf(dbInbound.id).hours ]]h /
                                        <template v-if="coexistOf(dbInbound.id).byIp">
                                            [[ coexistOf(dbInbound.id).ips ]] IP
                                        </template>
                                        <template v-else>
                                            [[ coexistOf(dbInbound.id).provinces.length ]] 省
                                        </template>
                                    </a-tag>
                                </a-tooltip>
                            </template>
```

- [ ] **Step 3: 在主 Vue 实例里接上数据**

在 `inbounds.html` 的 `const app = new Vue({...})`（约 `:383`）里：

`data` 中加一个字段：

```javascript
            coexistMap: {},
```

`methods` 中加两个方法：

```javascript
            coexistOf(inboundId) {
                return this.coexistMap[inboundId] || null;
            },
            // 单独拉一次，不塞进入站列表主接口：这个聚合查询失败时列表要
            // 照常渲染，只是没有并存标记。
            async loadCoexist() {
                const msg = await HttpUtil.post('/aui/inbound/sharing/summary');
                if (msg.success) {
                    this.coexistMap = msg.obj || {};
                }
            },
```

在 `async getDBInbounds()`（`:411`）的末尾追加一行：

```javascript
                this.loadCoexist();
```

**不要 `await`**：这个聚合查询失败或变慢都不该拖住入站列表的渲染。

在 `mounted()`（`:778`）里，`this.getDBInbounds();` 之后注册采纳事件：

```javascript
            // sharing_modal.html 有自己的 Vue 根实例（照 access_log_modal 的
            // 做法），两棵树之间靠事件通信。
            window.addEventListener('sharing-adopt-regions', (e) => {
                const dbInbound = this.dbInbounds.find(i => i.id === e.detail.inboundId);
                if (!dbInbound) return;
                // openEditInbound 内部走 inModal.show，它会 new 一份 DBInbound，
                // 所以要在 show 之后再改 inModal.dbInbound，改传进去的那个无效。
                this.openEditInbound(dbInbound);
                this.$nextTick(() => {
                    inModal.dbInbound.regionList = e.detail.regions.slice();
                });
            });
```

填完只是把值放进表单，管理员按「修改」才真正保存——走的是现有的
`updateInbound` 流程，复用 `EncodeRegionsStrict` 的校验与整条配置生成链。

- [ ] **Step 4: 验证模板能解析、指令没写到根元素外**

Run: `go test ./web/ -run "TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot" -v`
Expected: PASS

> 这两条测试不能跳过：`web.go` 的 `getHtmlTemplate` 会**吞掉** `ParseFS` 错误，一个语法错误的模板会被静默跳过直到渲染时才报 "template not found"；而 Vue 指令写在根元素外是完全静默的死代码。

- [ ] **Step 5: 手工验证**

```bash
XUI_DEBUG=true go run main.go
```

在浏览器打开入站列表，确认：
1. 没有并存记录时「地区」列与改动前完全一致
2. 有并存记录时出现橙色标记，点击能打开 modal
3. modal 里点「填入地区限制」会打开编辑框并把地区填好，**且没有直接保存**

> `XUI_DEBUG=true` 时模板从磁盘读，必须在仓库根目录启动。

- [ ] **Step 6: 全量验证**

Run: `make verify`
Expected: 全绿

- [ ] **Step 7: 提交**

```bash
git add web/html/xui/sharing_modal.html web/html/xui/inbounds.html
git commit -m "feat(sharing): 入站列表并存标记与明细弹窗

标记并进「地区」列而不是新增一列：并存告警本就是地区相关的信息，采纳
建议改的也是这一列，看到问题和处理问题在同一个位置。

采纳只填进编辑框、不直接保存，复用现有保存流程的校验与配置生成链。
弹窗自带 Vue 根元素（照 access_log_modal 的做法），与主实例靠事件通信。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_0168Zzn4Jwojxy6hSp6hFf2M"
```

---

## 完成标准

全部任务做完后：

1. `make verify` 全绿
2. `git status` 干净，没有临时文件、调试输出残留
3. 手工验证过 Task 8 Step 5 的三条
4. **不需要**新增任何设置项——若实现过程中你觉得非加不可，先停下来说明理由，那意味着设计有偏差

## 实现者最容易踩的三个坑

1. **把采样挂在 `ConcurrencyJob` 上省事。** `Enforce()` 在无人设并发额度时一次系统调用都不做，这样做会让检测在最常见的默认配置下静默失效——没有报错、没有日志，只是永远没有数据。
2. **让 `suggestRegions` 自动剔除并存省份。** 看着更"智能"，实际是在替管理员猜，而且猜错完全静默：xray 返回 `Configuration OK`、面板显示 `running`，只是流量走对了买家。
3. **把 modal 写在 `<a-layout id="app">` 之外又不给它自己的 Vue 根实例。** 页面渲染完全正常、数据也照常加载，但所有按钮点了毫无反应，控制台不报任何错。分流页踩过这个坑。
