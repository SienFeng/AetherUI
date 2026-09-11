# 按来源 IP 的分时段用量 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把入站展开行里「本次上线用量」（纯内存、随面板重启与客户端重连归零）换成可选窗口的用量——今日 / 近 3 日 / 近 7 日 / 近 30 日 / 近 1 年 / 自定义区间，分上下行，并在同一区块给出同窗口的入站合计。

**Architecture:** 数据层复用 `InboundIPHour`（共享检测那张表）加 `ActiveUp`/`ActiveDown` 两列，不新建表、不改 `ActiveBytes`。窗口计算抽成纯函数 `ParseWindow`，所有钳制在那里完成。按 IP 的分项走新接口 `/aui/inbound/ipUsage/:id`，入站合计扩展现有 `History`。前端把展开行里现有那排 `24小时/7天/30天/1年` 替换成日历语义的档位，一个控件同时驱动合计数字、IP 分项列、下方图表。

**Tech Stack:** Go 1.27 + Gin 1.7.1 + GORM/SQLite（CGO 必须开启）；前端 Vue 2.6.12 + ant-design-vue 1.7.2 + Chart.js 4.5.0 UMD + moment，无打包工具。

**Spec:** `docs/superpowers/specs/2026-09-10-per-ip-usage-window-design.md`

## Global Constraints

- **构建与验证命令**：`make verify`（= `go vet ./...` + `go test ./...` + `go build`）。单包测试 `go test ./web/service/ -run TestXxx -v`。CGO 必须开启（Makefile 已 `export CGO_ENABLED := 1`），不要另行设置。
- **`web/service` 的测试工作目录是仓库根**（该包 `TestMain` 会 `os.Chdir`）。新增测试若要用临时文件，一律 `t.TempDir()` 或绝对路径，不要用包内相对路径。
- **共享检测的判定行为必须逐字节不变**：`computeCoexist` / `computeCoexistGated` / `hasActiveBytes` / `suggestRegions` 的输入输出不得改变。`ActiveBytes` 保留为独立累加的字段，**不得**改成 `ActiveUp + ActiveDown` 的计算值。
- **「看不到」与「没有」必须能区分**：任何取不到数据的情形都返回非空 `Reason` 或显式降级标志，绝不返回一个看起来正常的 0。
- **不改 `InboundIPHour` 的 `AlignHourUTC` 对齐方式**（spec §10.1）。
- **不改 `TrafficHistoryService.Overview`**（系统状态页总览图）与 `rangeSpec` / `Range24h` 等既有常量（spec §9）。
- **不修 spec §2.2 的三条采集门槛**（每小时漏首轮、60 秒门槛不结转、50 IP/小时上限）。
- 前端改动后 `web/assets` 与 `web/html` 的缓存靠 `config.GetVersion()` 破，本地验证用 `XUI_DEBUG=true go run main.go`（必须在仓库根启动）。
- 中文注释，风格与周围代码一致：解释**为什么**和**不这么做会怎样**，不复述代码表面含义。

### 对 spec 的一处实现层细化

spec §4.2 写「钳制放在 controller」。本计划把钳制逻辑放进纯函数 `ParseWindow`（`web/service/traffic_window.go`），controller 只负责调用它。行为完全一致——`ParseWindow` 就是不可信输入进入 service 的那道边界——但日期解析、区间交换、跨度上限、未来截断这四类逻辑可以脱离 HTTP 单元测试。Task 2 的测试直接覆盖这四类。

---

### Task 1: 数据层——`InboundIPHour` 加上下行两列

**Files:**
- Modify: `database/model/sharing.go`（`InboundIPHour` 结构体 + 顶部消费者注释）
- Modify: `web/service/sharing_accumulator.go`（`sharingCell` 第 31-47 行附近、`sharingFlush` 第 64-71 行附近、`observe` 第 110-150 行附近、`rolloverLocked` 第 158-180 行附近）
- Modify: `web/service/sharing.go`（`upsertIPHour`，第 153-166 行）
- Test: `web/service/sharing_test.go`（追加，不新建文件——该文件已有 `setupSharingTest` / `listIPHours` 两个辅助函数可复用）

**Interfaces:**
- Consumes: 无（本任务是最底层）
- Produces:
  - `model.InboundIPHour.ActiveUp int64` / `.ActiveDown int64`（GORM 列名 `active_up` / `active_down`）
  - `service.sharingFlush.ActiveUp int64` / `.ActiveDown int64`
  - `sharingCell.upBytes int64` / `.downBytes int64`（包内私有）

- [ ] **Step 1: 写失败的测试——加列后的 upsert 往返**

追加到 `web/service/sharing_test.go` 末尾：

```go
// 新增的上下行两列必须能写进去、读回来，且与 ActiveBytes 一致。
//
// 这三个值在采集侧是三个独立累加器（见 sharingCell），数值上恒有
// up + down == bytes。测试钉住这个恒等式，将来谁把 ActiveBytes 改成
// 计算值、或漏掉 DoUpdates 里的某一列，都会在这里变红。
func TestUpsertIPHourRoundTripsSplitBytes(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()

	f := sharingFlush{
		InboundId: 7, IP: "1.2.3.4", Province: "江苏省",
		HourStart: 1000, ActiveSeconds: 120,
		ActiveBytes: 3000, ActiveUp: 1000, ActiveDown: 2000,
	}
	if err := upsertIPHour(db, f); err != nil {
		t.Fatalf("upsertIPHour: %v", err)
	}

	rows := listIPHours(t)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，期望 1", len(rows))
	}
	got := rows[0]
	if got.ActiveUp != 1000 || got.ActiveDown != 2000 {
		t.Errorf("ActiveUp/ActiveDown = %d/%d，期望 1000/2000", got.ActiveUp, got.ActiveDown)
	}
	if got.ActiveUp+got.ActiveDown != got.ActiveBytes {
		t.Errorf("上下行之和 %d 与 ActiveBytes %d 不等",
			got.ActiveUp+got.ActiveDown, got.ActiveBytes)
	}
}

// 覆盖式 upsert 必须把两个新列一起覆盖掉。
//
// 漏掉 DoUpdates 里的某一列，表现是「上行在涨、下行冻结在第一次写入的值」
// ——不会报错，只会让分项永远偏小，而且只在同一小时内被写第二次时才出现。
func TestUpsertIPHourOverwritesSplitBytes(t *testing.T) {
	setupSharingTest(t)
	db := database.GetTrafficDB()

	first := sharingFlush{
		InboundId: 7, IP: "1.2.3.4", HourStart: 1000,
		ActiveSeconds: 60, ActiveBytes: 300, ActiveUp: 100, ActiveDown: 200,
	}
	if err := upsertIPHour(db, first); err != nil {
		t.Fatalf("第一次 upsert: %v", err)
	}
	second := first
	second.ActiveSeconds, second.ActiveBytes = 120, 900
	second.ActiveUp, second.ActiveDown = 300, 600
	if err := upsertIPHour(db, second); err != nil {
		t.Fatalf("第二次 upsert: %v", err)
	}

	rows := listIPHours(t)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，期望 1（覆盖而非新增）", len(rows))
	}
	if rows[0].ActiveUp != 300 || rows[0].ActiveDown != 600 {
		t.Errorf("ActiveUp/ActiveDown = %d/%d，期望 300/600",
			rows[0].ActiveUp, rows[0].ActiveDown)
	}
}

// 累加器必须并行维护三个计数器，且客户端重连（计数器回退）时不产生负增量。
//
// deltaBytes 对回退按全量计入，三个计数器都要走它——只给 bytes 走、
// 给 up/down 直接相减的话，一次重连会让分项变成负数，界面显示成
// 「-2.3 GB」而没有任何一层会拦住它。
func TestAccumulatorTracksSplitBytesAndSurvivesReconnect(t *testing.T) {
	a := newSharingAccumulator()
	base := time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC)

	// 第一轮只设基线，不计字节（见 sharingCell 注释）。
	a.observe(base, []sharingObservation{
		{InboundId: 1, IP: "1.1.1.1", Up: 500, Down: 900},
	}, 30)
	// 第二轮：正常增长。
	a.observe(base.Add(30*time.Second), []sharingObservation{
		{InboundId: 1, IP: "1.1.1.1", Up: 1500, Down: 2900},
	}, 30)
	// 第三轮：客户端重连，累计值从头开始。
	flushes := a.observe(base.Add(60*time.Second), []sharingObservation{
		{InboundId: 1, IP: "1.1.1.1", Up: 100, Down: 200},
	}, 30)

	if len(flushes) != 1 {
		t.Fatalf("落库条数 = %d，期望 1（累计 90 秒已过 60 秒门槛）", len(flushes))
	}
	f := flushes[0]
	// 第二轮 +1000/+2000，第三轮回退按全量 +100/+200。
	if f.ActiveUp != 1100 || f.ActiveDown != 2200 {
		t.Errorf("ActiveUp/ActiveDown = %d/%d，期望 1100/2200", f.ActiveUp, f.ActiveDown)
	}
	if f.ActiveUp+f.ActiveDown != f.ActiveBytes {
		t.Errorf("上下行之和 %d 与 ActiveBytes %d 不等",
			f.ActiveUp+f.ActiveDown, f.ActiveBytes)
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestUpsertIPHourRoundTripsSplitBytes|TestUpsertIPHourOverwritesSplitBytes|TestAccumulatorTracksSplitBytesAndSurvivesReconnect' -v`

Expected: 编译失败，`unknown field ActiveUp in struct literal of type sharingFlush`。

- [ ] **Step 3: 给 `model.InboundIPHour` 加两列**

在 `database/model/sharing.go` 的 `ActiveBytes` 字段之后追加：

```go
	// ActiveUp/ActiveDown 是本小时该来源 IP 的上行、下行字节。
	//
	// 与 ActiveBytes 并列而不是取代它：ActiveBytes 是并存判定的门槛判据
	// （coexistMinActiveBytes），而升级前写入的行这两个新列恒为 0、
	// ActiveBytes 有值。若把 ActiveBytes 改成由两列相加得出，那批历史行的
	// 判据会当场失效，共享检测的结论会在升级瞬间整体改变，而界面上没有
	// 任何东西说明发生了什么。
	//
	// 升级前的行这两列恒为 0，消费侧据此整批降级（见 service.hasSplitBytes），
	// 不逐行判断——理由与 hasActiveBytes 那条完全相同。
	ActiveUp   int64 `json:"activeUp"`
	ActiveDown int64 `json:"activeDown"`
```

同时把该文件顶部注释里的这句：

```
// 它只有一个消费者：判定「同一小时内，不同省份的 IP 是否在同时使用这个
// 入站」。
```

改成：

```
// 它有两个消费者：
//
//  1. 判定「同一小时内，不同省份的 IP 是否在同时使用这个入站」。这个
//     「并存」判据是区分「用户旅游」（位置迁移，不并存）与「节点被转卖」
//     （位置并存）的唯一可靠信号，见设计文档 §1。
//  2. 入站展开行里按来源 IP 的分时段用量（ActiveUp/ActiveDown），见
//     docs/superpowers/specs/2026-09-10-per-ip-usage-window-design.md。
//
// 两个消费者对采集门槛的要求方向相反：并存判定要过滤噪声（60 秒落库门槛、
// 50 IP/小时上限），用量统计希望不漏。冲突以并存判定为准——用量那一侧
// 在界面上标注了口径，见该文档 §2.2 与 §8.2。
```

（原注释里「这个『并存』判据是区分…」那两行并入上面第 1 条，不要重复保留。）

- [ ] **Step 4: 给 `sharingCell` 与 `sharingFlush` 加字段**

`web/service/sharing_accumulator.go`，`sharingCell` 的 `bytes` 字段改为三个：

```go
	// bytes 是本小时累计的上下行字节，upBytes/downBytes 是它的拆分；
	// lastUp/lastDown 是上一轮观测到的累计值——OnlineIP.Up/Down 给的是
	//「本次在线期间的累计量」而不是增量，差分必须在这里做。
	//
	// bytes 独立累加而不是写成 upBytes + downBytes：两者数值恒等，但独立
	// 赋值让「共享检测的输入逐字节不变」这件事在 diff 上看得见，也避免
	// 将来某次重构顺手把 bytes 删掉——它是 ActiveBytes 的唯一来源，而
	// ActiveBytes 是并存判定的门槛判据。
	//
	// cell 新建那一轮只设基线、不计字节：进入观测之前的流量属于上一个小时
	// 或上一次在线，算进来就是把别处的流量挪到本小时。代价是每小时的第一个
	// 采样间隔（30 秒）的流量不计入，相对 1 MB 的判定门槛可以忽略。
	bytes     int64
	upBytes   int64
	downBytes int64
	lastUp    int64
	lastDown  int64
```

`sharingFlush` 追加两个字段：

```go
type sharingFlush struct {
	InboundId     int
	IP            string
	Province      string
	HourStart     int64
	ActiveSeconds int
	ActiveBytes   int64
	ActiveUp      int64
	ActiveDown    int64
}
```

- [ ] **Step 5: 改 `observe` 的差分与两处 flush 构造**

`observe` 里那段差分改成：

```go
			// 复用 online.go 的 deltaBytes：内核计数器只会单调增长，出现
			// 回退只可能是客户端断开重连、计数器从头开始，此时按全量计入，
			// 绝不产生负增量。三个计数器都必须走它——只给 bytes 走、给
			// up/down 直接相减的话，一次重连会让分项变成负数。
			du := int64(deltaBytes(uint64(o.Up), uint64(cell.lastUp), true))
			dd := int64(deltaBytes(uint64(o.Down), uint64(cell.lastDown), true))
			cell.upBytes += du
			cell.downBytes += dd
			cell.bytes += du + dd
			cell.lastUp, cell.lastDown = o.Up, o.Down
```

`observe` 里构造 `sharingFlush` 的那处，以及 `rolloverLocked` 里构造 `sharingFlush` 的那处，**两处都要**加上新字段：

```go
			out = append(out, sharingFlush{
				InboundId: key.inboundId, IP: key.ip, Province: cell.province,
				HourStart: hour, ActiveSeconds: cell.seconds, ActiveBytes: cell.bytes,
				ActiveUp: cell.upBytes, ActiveDown: cell.downBytes,
			})
```

（`rolloverLocked` 那处的 `HourStart` 是 `a.hour` 而不是 `hour`，其余相同。漏掉 `rolloverLocked` 那处的后果是：跨小时补写的那一批分项恒为 0，而 `ActiveBytes` 正常——表现为「每小时最后一段的上下行拆分丢失」，只在跨小时时出现，极难发现。）

- [ ] **Step 6: 改 `upsertIPHour` 写入两列**

`web/service/sharing.go`：

```go
	row := &model.InboundIPHour{
		InboundId: f.InboundId, IP: f.IP, HourStart: f.HourStart,
		Province: f.Province, ActiveSeconds: f.ActiveSeconds, ActiveBytes: f.ActiveBytes,
		ActiveUp: f.ActiveUp, ActiveDown: f.ActiveDown,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "inbound_id"}, {Name: "ip"}, {Name: "hour_start"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"province", "active_seconds", "active_bytes", "active_up", "active_down",
		}),
	}).Create(row).Error
```

- [ ] **Step 7: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestUpsertIPHour|TestAccumulator' -v`

Expected: PASS，含既有的 `TestUpsertIPHourOverwritesInsteadOfAccumulating`。

- [ ] **Step 8: 跑共享检测的全部既有测试，确认行为未变**

Run: `go test ./web/service/ -run 'Sharing|Coexist|IPHour' -v`

Expected: 全部 PASS。**任何一条既有断言变红都意味着改动越界了**——本任务只加列，不改判定。

- [ ] **Step 9: 提交**

```bash
git add database/model/sharing.go web/service/sharing_accumulator.go web/service/sharing.go web/service/sharing_test.go
git commit -m "feat(sharing): InboundIPHour 记录上下行拆分

给 InboundIPHour 加 ActiveUp/ActiveDown 两列，累加器并行维护三个计数器。
ActiveBytes 保留为独立累加的字段而不是改成两列之和：它是并存判定的门槛
判据，而升级前写入的行两个新列恒为 0，改成计算值会让那批历史行的判据
当场失效。

AutoMigrate 只加列不删列，回退到旧二进制时新列被忽略、共享检测只读
ActiveBytes，行为完全不变。"
```

---

### Task 2: 窗口计算——`ParseWindow` 纯函数与全部钳制

**Files:**
- Create: `web/service/traffic_window.go`
- Test: `web/service/traffic_window_test.go`

**Interfaces:**
- Consumes: `model.TrafficGranularity` / `model.GranularityHour` / `model.GranularityDay`（`database/model/traffic.go` 既有）
- Produces:
  - `service.TrafficWindow{Start, End int64; Granularity model.TrafficGranularity; Label string}`
  - `service.ParseWindow(name, startDate, endDate string, loc *time.Location, now time.Time) TrafficWindow`
  - 档位常量 `WindowToday` / `Window3d` / `Window7d` / `Window30d` / `Window1y` / `WindowCustom`
  - `service.ipUsageMaxWindowDays = 30`（Task 3 判断 `BeyondRetention` 时用）

- [ ] **Step 1: 写失败的测试**

新建 `web/service/traffic_window_test.go`：

```go
package service

import (
	"testing"
	"time"

	"a-ui/database/model"
)

// 时区一律用 mustLoadShanghai（同包 traffic_history_test.go 既有）：
// 它就是 defaultValueMap 里 timeLocation 的默认值，整小时偏移，
// 生产实例绝大多数是这个。不要再写一个同样的辅助函数。

// 「今日」是本地今天 0 点到现在，不是「往回数 24 小时」。
func TestParseWindowToday(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow(WindowToday, "", "", loc, now)

	wantStart := time.Date(2026, 9, 10, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart {
		t.Errorf("Start = %d，期望 %d（本地今天 0 点）", w.Start, wantStart)
	}
	if w.End != now.Unix() {
		t.Errorf("End = %d，期望 %d（现在）", w.End, now.Unix())
	}
	if w.Granularity != model.GranularityHour {
		t.Errorf("Granularity = %v，期望小时", w.Granularity)
	}
}

// 「近 N 日」按日历算：包含今天在内的 N 个自然日。
func TestParseWindowRecentDaysIsCalendarNotRolling(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow(Window7d, "", "", loc, now)

	// 包含今天在内的 7 天，起点是 9 月 4 日 0 点，而不是 9 月 3 日 14:30。
	wantStart := time.Date(2026, 9, 4, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart {
		t.Errorf("Start = %d，期望 %d（9/4 0 点）", w.Start, wantStart)
	}
}

// 跨月边界：9 月 2 日看「近 7 日」要回到 8 月 27 日，不能在 9 月 1 日截断。
func TestParseWindowCrossesMonthBoundary(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, loc)

	w := ParseWindow(Window7d, "", "", loc, now)

	wantStart := time.Date(2026, 8, 27, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart {
		t.Errorf("Start = %d，期望 %d（8/27 0 点）", w.Start, wantStart)
	}
}

// 跨年边界：1 月 2 日看「近 7 日」要回到上一年 12 月 27 日。
//
// AddDate 天然处理跨年，这条用例钉住的是「没有人为了『简单』把它改成
// 减去 N*86400 秒」——那在夏令时切换的时区上会差一个小时，而中国不用
// 夏令时，本地永远测不出来。
func TestParseWindowCrossesYearBoundary(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2027, 1, 2, 10, 0, 0, 0, loc)

	w := ParseWindow(Window7d, "", "", loc, now)

	wantStart := time.Date(2026, 12, 27, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart {
		t.Errorf("Start = %d，期望 %d（2026/12/27 0 点）", w.Start, wantStart)
	}
}

// 粒度按跨度选：≤30 天用小时桶，>30 天用日桶。
func TestParseWindowPicksGranularityBySpan(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	if g := ParseWindow(Window30d, "", "", loc, now).Granularity; g != model.GranularityHour {
		t.Errorf("30d 的粒度 = %v，期望小时", g)
	}
	if g := ParseWindow(Window1y, "", "", loc, now).Granularity; g != model.GranularityDay {
		t.Errorf("1y 的粒度 = %v，期望日", g)
	}
}

// 自定义区间：起始日 0 点到结束日 24 点，左闭右开。
func TestParseWindowCustom(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow(WindowCustom, "2026-09-01", "2026-09-05", loc, now)

	wantStart := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).Unix()
	wantEnd := time.Date(2026, 9, 6, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart || w.End != wantEnd {
		t.Errorf("[%d, %d)，期望 [%d, %d)", w.Start, w.End, wantStart, wantEnd)
	}
}

// 钳制一：认不出的档位回落「今日」，不报错。
//
// 与 rangeSpec 那句「前端传错时给一张能看的图，比报错或空图有用」同一取向。
func TestParseWindowUnknownRangeFallsBackToToday(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow("不存在的档位", "", "", loc, now)

	want := ParseWindow(WindowToday, "", "", loc, now)
	if w.Start != want.Start || w.End != want.End {
		t.Errorf("[%d, %d)，期望与「今日」相同 [%d, %d)", w.Start, w.End, want.Start, want.End)
	}
}

// 钳制二：自定义日期不可解析时回落「今日」。
func TestParseWindowBadCustomDateFallsBackToToday(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	for _, c := range []struct{ start, end string }{
		{"不是日期", "2026-09-05"},
		{"2026-09-01", "也不是日期"},
		{"", ""},
		{"2026-13-45", "2026-09-05"},
	} {
		w := ParseWindow(WindowCustom, c.start, c.end, loc, now)
		want := ParseWindow(WindowToday, "", "", loc, now)
		if w.Start != want.Start || w.End != want.End {
			t.Errorf("start=%q end=%q 得到 [%d, %d)，期望回落今日 [%d, %d)",
				c.start, c.end, w.Start, w.End, want.Start, want.End)
		}
	}
}

// 钳制三：start > end 时交换，而不是返回一个空区间。
//
// 空区间会让界面显示一片 0，管理员看不出是自己把日期选反了。
func TestParseWindowSwapsReversedRange(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow(WindowCustom, "2026-09-05", "2026-09-01", loc, now)

	wantStart := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).Unix()
	wantEnd := time.Date(2026, 9, 6, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart || w.End != wantEnd {
		t.Errorf("[%d, %d)，期望交换后的 [%d, %d)", w.Start, w.End, wantStart, wantEnd)
	}
}

// 钳制四：跨度超过 366 天时把起点抬上来，不是拒绝。
func TestParseWindowClampsSpanTo366Days(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow(WindowCustom, "2000-01-01", "2026-09-09", loc, now)

	span := w.End - w.Start
	maxSpan := int64(windowMaxSpanDays) * 86400
	if span > maxSpan {
		t.Errorf("跨度 %d 秒超过上限 %d 秒", span, maxSpan)
	}
	if span < maxSpan-86400 {
		t.Errorf("跨度 %d 秒，期望被钳到接近上限 %d 秒（而不是缩成很小）", span, maxSpan)
	}
}

// 钳制五：End 不得超过现在。
//
// 选一个未来的结束日，右端必须截到现在——否则图表会画出一串未来的空刻度。
func TestParseWindowClampsFutureEnd(t *testing.T) {
	loc := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	w := ParseWindow(WindowCustom, "2026-09-08", "2027-01-01", loc, now)

	if w.End != now.Unix() {
		t.Errorf("End = %d，期望截到现在 %d", w.End, now.Unix())
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run TestParseWindow -v`

Expected: 编译失败，`undefined: ParseWindow`。

- [ ] **Step 3: 实现 `ParseWindow`**

新建 `web/service/traffic_window.go`：

```go
package service

import (
	"time"

	"a-ui/database/model"
)

// 时间窗口的档位名。与前端 inbounds.html 里 a-radio-button 的 value 一一对应，
// 两边改一处就必须改另一处——认不出的档位会静默回落「今日」，不会报错。
//
// **注意 Window7d/Window30d 与 traffic_history.go 里的 Range7d/Range30d
// 字符串相同但语义不同**：这里是日历窗口（包含今天在内的 N 个自然日），
// 那里是滑动窗口（从此刻往回数 N×24 小时）。两套档位分别服务两个接口
// ——ParseWindow 用于 /traffic/history 与 /ipUsage，rangeSpec 用于
// /traffic/overview（系统状态页），互不调用。把它们合并成一套是一次
// 独立的改动，需要连带改掉系统状态页，不在本次范围。
const (
	WindowToday  = "today"
	Window3d     = "3d"
	Window7d     = "7d"
	Window30d    = "30d"
	Window1y     = "1y"
	WindowCustom = "custom"
)

// windowMaxSpanDays 是自定义区间的跨度上限。
//
// 区间来自请求体，是不可信输入：一个 1970→2100 的区间会让服务端拉出天量行。
// 366 而不是 365，是为了让「整整一个闰年」这种合理输入不被截断。
const windowMaxSpanDays = 366

// windowHourGranularityDays 是改用日桶的跨度阈值。
//
// 与既有 rangeSpec 的行为一致：30 天用小时桶（720 点），更长用日桶。
const windowHourGranularityDays = 30

// ipUsageMaxWindowDays 是按来源 IP 的分项能覆盖的最大天数。
//
// 等于 sharingRetentionDays——InboundIPHour 只保留这么久，超出的窗口
// 里那部分数据根本不存在。**故意写成独立常量而不是直接引用
// sharingRetentionDays**：两者语义不同（一个是保留期，一个是查询能力
// 上界），将来保留期若改成设置项，这里要跟着改而不是自动漂移。
const ipUsageMaxWindowDays = sharingRetentionDays

// windowDateLayout 是前端传来的自定义日期格式。只收日期不收时刻：
// 时刻由服务端按面板时区补成 0 点与 24 点，浏览器所在时区因此不影响结果。
const windowDateLayout = "2006-01-02"

// TrafficWindow 是一次用量查询的时间范围，左闭右开的 Unix 秒。
//
// 左闭右开而不是闭区间：桶的起点落在 [Start, End) 里就算命中，边界那个桶
// 归属明确，不会被相邻两个窗口各算一次。
type TrafficWindow struct {
	Start       int64
	End         int64
	Granularity model.TrafficGranularity
	// Label 是给界面显示的范围描述，如「2026-09-04 ~ 2026-09-10」。
	// 在服务端格式化，理由与 formatLabels 相同：时区的权威在服务端。
	Label string
}

// SpanDays 返回窗口覆盖的天数（向上取整）。
func (w TrafficWindow) SpanDays() int {
	span := w.End - w.Start
	if span <= 0 {
		return 0
	}
	return int((span + 86399) / 86400)
}

// ParseWindow 把档位名或自定义日期翻译成窗口，并完成全部钳制。
//
// 这是不可信输入进入 service 的边界：区间来自请求体，越界的值一律钳制而
// 不是报错——与 rangeSpec 那句「前端传错时给一张能看的图，比报错或空图
// 有用」同一取向。五类钳制：认不出的档位、不可解析的日期、start > end、
// 跨度超上限、End 超过现在。
//
// 档位到区间的翻译放在服务端而不是让前端算时间戳：时区的权威在服务端
// （SettingService.GetTimeLocation），前端算的话，访问者换个时区，同一个
//「今日」就指向不同的绝对时间，而面板设置的时区并没有变。
func ParseWindow(name, startDate, endDate string, loc *time.Location, now time.Time) TrafficWindow {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	var start, end time.Time
	switch name {
	case WindowCustom:
		s, errS := time.ParseInLocation(windowDateLayout, startDate, loc)
		e, errE := time.ParseInLocation(windowDateLayout, endDate, loc)
		if errS != nil || errE != nil {
			// 钳制二：日期不可解析，回落「今日」。
			start, end = midnight, now
			break
		}
		if s.After(e) {
			// 钳制三：选反了就交换，而不是返回空区间——空区间会让界面
			// 显示一片 0，管理员看不出是自己把日期选反了。
			s, e = e, s
		}
		// 结束日是**包含**的，所以右端取它的次日 0 点。
		start, end = s, e.AddDate(0, 0, 1)
	case Window3d:
		start, end = midnight.AddDate(0, 0, -2), now
	case Window7d:
		start, end = midnight.AddDate(0, 0, -6), now
	case Window30d:
		start, end = midnight.AddDate(0, 0, -29), now
	case Window1y:
		start, end = midnight.AddDate(0, 0, -364), now
	default:
		// 钳制一：认不出的档位（含 WindowToday 本身）回落「今日」。
		start, end = midnight, now
	}

	// 钳制五：右端不得超过现在。选了未来的结束日时，图表不该画出一串
	// 未来的空刻度。
	if end.After(now) {
		end = now
	}
	// 钳制四：跨度超上限时抬起点，而不是拒绝整个请求。
	if maxSpan := time.Duration(windowMaxSpanDays) * 24 * time.Hour; end.Sub(start) > maxSpan {
		start = end.Add(-maxSpan)
	}
	// 交换与钳制之后仍可能出现空区间（结束日在今天之前且起点被抬到它之后
	// 是不可能的，但未来的起始日会落到这里）。空区间统一退回「今日」，
	// 理由同钳制二。
	if !end.After(start) {
		start, end = midnight, now
	}

	g := model.GranularityHour
	if end.Sub(start) > time.Duration(windowHourGranularityDays)*24*time.Hour {
		g = model.GranularityDay
	}

	return TrafficWindow{
		Start:       start.Unix(),
		End:         end.Unix(),
		Granularity: g,
		Label: start.Format(windowDateLayout) + " ~ " +
			end.Add(-time.Second).Format(windowDateLayout),
	}
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./web/service/ -run TestParseWindow -v`

Expected: 全部 PASS（11 条）。

- [ ] **Step 5: 提交**

```bash
git add web/service/traffic_window.go web/service/traffic_window_test.go
git commit -m "feat(traffic): 用量查询的时间窗口与钳制

ParseWindow 把档位名或自定义日期翻译成左闭右开的 [Start, End)，并在这里
完成全部五类钳制：认不出的档位、不可解析的日期、start > end、跨度超 366
天、End 超过现在。越界一律钳制而不报错，与 rangeSpec 那句「前端传错时给
一张能看的图，比报错或空图有用」同一取向。

翻译放在服务端而不是让前端算时间戳：时区的权威在服务端，前端算的话访问者
换个时区，同一个「今日」就指向不同的绝对时间。"
```

---

### Task 3: `IPUsageService`——按来源 IP 的分项查询

**Files:**
- Create: `web/service/ip_usage.go`
- Test: `web/service/ip_usage_test.go`

**Interfaces:**
- Consumes:
  - `service.TrafficWindow` / `service.ipUsageMaxWindowDays`（Task 2）
  - `model.InboundIPHour.ActiveUp` / `.ActiveDown`（Task 1）
  - `service.locateWithIPDB(svc IPDBService, ip net.IP) ipLocation`（`web/service/online.go:419`，既有包级函数）
  - `service.ipSourceLocation`（`web/service/online.go:400`，既有）
  - `service.trafficDBUnavailable`（`web/service/traffic_history.go:199`，既有常量）
- Produces:
  - `service.IPUsageEntry` / `service.IPUsageResult`
  - `service.IPUsageService{ipdbService IPDBService}` 及其 `Query(inboundId int, w TrafficWindow) (*IPUsageResult, error)`
  - `service.hasSplitBytes(rows []model.InboundIPHour) bool`
  - `service.ipUsageMaxEntries = 200`

- [ ] **Step 1: 写失败的测试**

新建 `web/service/ip_usage_test.go`：

```go
package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// seedIPHour 往库里塞一行，供本文件各用例复用。
func seedIPHour(t *testing.T, inboundId int, ip string, hourStart int64, up, down int64) {
	t.Helper()
	row := model.InboundIPHour{
		InboundId: inboundId, IP: ip, HourStart: hourStart,
		ActiveSeconds: 120, ActiveBytes: up + down,
		ActiveUp: up, ActiveDown: down,
	}
	if err := database.GetTrafficDB().Create(&row).Error; err != nil {
		t.Fatalf("写入 InboundIPHour: %v", err)
	}
}

// 基本聚合：同一个 IP 的多个小时求和，按用量降序排列。
func TestIPUsageAggregatesAndSortsByUsage(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "1.1.1.1", 3600, 100, 200)
	seedIPHour(t, 1, "1.1.1.1", 7200, 300, 400)
	seedIPHour(t, 1, "2.2.2.2", 3600, 5000, 6000)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("条数 = %d，期望 2", len(got.Entries))
	}
	// 用量大的排前面。
	if got.Entries[0].IP != "2.2.2.2" {
		t.Errorf("第一条 = %s，期望用量最大的 2.2.2.2", got.Entries[0].IP)
	}
	if got.Entries[1].Up != 400 || got.Entries[1].Down != 600 {
		t.Errorf("1.1.1.1 的 Up/Down = %d/%d，期望 400/600（两个小时求和）",
			got.Entries[1].Up, got.Entries[1].Down)
	}
	if got.Entries[1].LastSeen != 7200*1000 {
		t.Errorf("LastSeen = %d，期望 %d 毫秒（窗口内最后一个有记录的小时）",
			got.Entries[1].LastSeen, 7200*1000)
	}
}

// 窗口是左闭右开：恰好落在 End 上的桶不算进来。
//
// 不这么做的话，相邻两个窗口会把边界那个小时各算一次，「今日」与「昨日」
// 相加会大于「近 2 日」，而没有任何一层会报错。
func TestIPUsageWindowIsHalfOpen(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 3600, End: 7200, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "1.1.1.1", 3600, 10, 20) // 在窗口内
	seedIPHour(t, 1, "1.1.1.1", 7200, 99, 99) // 恰好在右端，不算

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Up != 10 {
		t.Fatalf("Up = %v，期望只统计窗口内那一行（10）", got.Entries)
	}
}

// 只统计指定入站的行。
//
// SQLite 会复用被删除的自增 id，漏掉 inbound_id 条件会让一个入站看到
// 别人的来源 IP，而表格渲染得完全合理。
func TestIPUsageIsScopedToInbound(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "1.1.1.1", 3600, 10, 20)
	seedIPHour(t, 2, "9.9.9.9", 3600, 10, 20)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].IP != "1.1.1.1" {
		t.Fatalf("得到 %v，期望只有入站 1 的来源", got.Entries)
	}
}

// 升级前的老行（ActiveBytes 有值、两个新列为 0）整批降级，Split 为 false。
//
// 判据按整批而非逐行，与 hasActiveBytes 那条完全同构：逐行判会有歧义
// ——一条恰好在本小时没有字节增量的新行，与升级前写入的老行长得一模一样。
func TestIPUsageSplitIsBatchWideNotPerRow(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	// 全是老行。
	for _, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		row := model.InboundIPHour{
			InboundId: 1, IP: ip, HourStart: 3600,
			ActiveSeconds: 120, ActiveBytes: 1000, // ActiveUp/ActiveDown 为零值
		}
		if err := database.GetTrafficDB().Create(&row).Error; err != nil {
			t.Fatalf("写入老行: %v", err)
		}
	}

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Split {
		t.Error("Split = true，期望 false（整批都是升级前的老行）")
	}
	// 降级时仍要给出合计，不能因为拆不出上下行就返回 0。
	if got.Entries[0].Up+got.Entries[0].Down != 1000 {
		t.Errorf("合计 = %d，期望 1000（降级只影响拆分，不影响总量）",
			got.Entries[0].Up+got.Entries[0].Down)
	}
}

// 只要批里有一行带拆分，整批就按有拆分处理。
func TestIPUsageSplitTrueWhenAnyRowHasSplit(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	old := model.InboundIPHour{
		InboundId: 1, IP: "1.1.1.1", HourStart: 3600,
		ActiveSeconds: 120, ActiveBytes: 1000,
	}
	if err := database.GetTrafficDB().Create(&old).Error; err != nil {
		t.Fatalf("写入老行: %v", err)
	}
	seedIPHour(t, 1, "2.2.2.2", 7200, 300, 700)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !got.Split {
		t.Error("Split = false，期望 true（批里有带拆分的行）")
	}
}

// 窗口超出 InboundIPHour 的保留期时整块降级：BeyondRetention 为 true、
// Entries 为空、Reason 非空。
//
// 返回一张看起来正常的空表会让管理员以为这段时间没人用过。
func TestIPUsageBeyondRetentionDegradesWithReason(t *testing.T) {
	setupSharingTest(t)
	now := time.Now()
	w := TrafficWindow{
		Start:       now.AddDate(0, 0, -200).Unix(),
		End:         now.Unix(),
		Granularity: model.GranularityDay,
	}
	seedIPHour(t, 1, "1.1.1.1", now.Add(-time.Hour).Unix(), 10, 20)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !got.BeyondRetention {
		t.Error("BeyondRetention = false，期望 true")
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries 有 %d 条，期望空", len(got.Entries))
	}
	if got.Reason == "" {
		t.Error("Reason 为空——「看不到」必须和「没有」能区分开")
	}
}

// 长尾截断：超过上限时只返回前 N 条，其余合并成 Other，总量不缩水。
func TestIPUsageTruncatesLongTailWithoutLosingTotal(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 10000000, Granularity: model.GranularityHour}

	var wantUp, wantDown int64
	total := ipUsageMaxEntries + 30
	for i := 0; i < total; i++ {
		up := int64(total - i) // 递减，保证排序稳定可断言
		down := up * 2
		seedIPHour(t, 1, fmt.Sprintf("10.0.%d.%d", i/256, i%256), 3600, up, down)
		wantUp += up
		wantDown += down
	}

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got.Entries) != ipUsageMaxEntries {
		t.Fatalf("条数 = %d，期望 %d", len(got.Entries), ipUsageMaxEntries)
	}
	if got.OtherCount != 30 {
		t.Errorf("OtherCount = %d，期望 30", got.OtherCount)
	}

	var gotUp, gotDown int64
	for _, e := range got.Entries {
		gotUp += e.Up
		gotDown += e.Down
	}
	gotUp += got.OtherUp
	gotDown += got.OtherDown
	if gotUp != wantUp || gotDown != wantDown {
		t.Errorf("截断后总量 %d/%d，期望 %d/%d——截断绝不能让总量缩水",
			gotUp, gotDown, wantUp, wantDown)
	}
}

// 用量相同时按 IP 字节序排，保证同一份输入永远给出同一个次序。
func TestIPUsageOrderIsDeterministicOnTiedUsage(t *testing.T) {
	setupSharingTest(t)
	w := TrafficWindow{Start: 1000, End: 100000, Granularity: model.GranularityHour}

	seedIPHour(t, 1, "2.2.2.2", 3600, 100, 100)
	seedIPHour(t, 1, "1.1.1.1", 3600, 100, 100)

	var svc IPUsageService
	got, err := svc.Query(1, w)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Entries[0].IP != "1.1.1.1" {
		t.Errorf("同量时第一条 = %s，期望字节序在前的 1.1.1.1", got.Entries[0].IP)
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run TestIPUsage -v`

Expected: 编译失败，`undefined: IPUsageService`。

- [ ] **Step 3: 实现 `IPUsageService`**

新建 `web/service/ip_usage.go`：

```go
package service

import (
	"bytes"
	"net"
	"sort"

	"a-ui/database"
	"a-ui/database/model"
)

// ipUsageMaxEntries 是返回给界面的条数上限。
//
// sharingMaxRowsPerHour 限的是「单入站单小时 50 个 IP」，30 天窗口下去重后
// 的 IP 总数没有上界——一次持续的端口扫描能攒出几千个。表格渲染几千行会把
// 页面卡死，而管理员真正要看的永远是用量最大的那几个。超出的部分**不丢弃**，
// 合计进 OtherCount/OtherUp/OtherDown，界面渲染成末尾一行：总量不能静默缩水。
const ipUsageMaxEntries = 200

// ipUsageBeyondRetention 是窗口超出 InboundIPHour 保留期时的说明。
//
// 必须有这句话：返回一张看起来正常的空表，管理员会以为这段时间没人用过。
const ipUsageBeyondRetention = "按来源 IP 的明细只保留 30 天，更长的区间只有入站合计用量"

// ipUsageNoSplitReason 是整批数据都来自升级前时的说明。
//
// 写成常量而不是 fmt.Sprintf 拼一个没有占位符的字符串——后者会被
// go vet 的 printf 检查判为多余，让 make verify 直接失败。
const ipUsageNoSplitReason = "该时段有升级前写入的数据，无法拆分上下行，显示的是合计用量"

// IPUsageEntry 是某个来源 IP 在窗口内的用量。
type IPUsageEntry struct {
	IP   string `json:"ip"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`

	// 归属地五件套与 OnlineIP 同名同义，由同一个 locateWithIPDB 产出。
	// 复用它而不是自己写一次单值 lookup，离线行才能和在线行一样显示
	//「存疑」标记，也不会出现同一个 IP 换一行就换个归属地的情形。
	Location    string             `json:"location"`
	LocationAlt string             `json:"locationAlt"`
	ISP         string             `json:"isp"`
	ISPAlt      string             `json:"ispAlt"`
	Sources     []ipSourceLocation `json:"sources"`

	// LastSeen 是窗口内最后一个有记录的小时（毫秒）。界面用它给离线行
	// 填「上线时间」那一列——对已经断开的来源，「最后活跃」比一个空值有用。
	LastSeen int64 `json:"lastSeen"`
}

// IPUsageResult 是按来源 IP 的分项查询结果。
type IPUsageResult struct {
	Entries []IPUsageEntry `json:"entries"`

	// Split 为 false 表示这批数据拆不出上下行（全是升级前写入的行），
	// 界面应显示合计数而不是「↑0 ↓0」——后者是「没有用量」的意思。
	Split bool `json:"split"`

	// BeyondRetention 为 true 表示窗口超出了保留期，Entries 必为空。
	BeyondRetention bool `json:"beyondRetention"`

	// OtherCount/OtherUp/OtherDown 是被长尾截断合并掉的那部分。
	OtherCount int   `json:"otherCount"`
	OtherUp    int64 `json:"otherUp"`
	OtherDown  int64 `json:"otherDown"`

	Reason string `json:"reason"`
}

// IPUsageService 查询按来源 IP 的分时段用量。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type IPUsageService struct {
	ipdbService IPDBService
}

// hasSplitBytes 判断这批行里有没有上下行拆分，即该不该按拆分口径显示。
//
// 判据是「整批数据里有没有拆分」而不是逐行判断——与 hasActiveBytes 那条
// 完全同构：一条恰好在本小时没有字节增量的新行，与升级前写入的老行长得
// 一模一样，逐行判会让同一张表里两种口径混排。
func hasSplitBytes(rows []model.InboundIPHour) bool {
	for _, r := range rows {
		if r.ActiveUp > 0 || r.ActiveDown > 0 {
			return true
		}
	}
	return false
}

// Query 返回某入站在窗口内各来源 IP 的用量，按用量降序。
//
// 窗口超出保留期时整块降级（BeyondRetention），而不是返回一张空表：
//「看不到」和「没有」必须能区分开。
func (s *IPUsageService) Query(inboundId int, w TrafficWindow) (*IPUsageResult, error) {
	result := &IPUsageResult{Entries: []IPUsageEntry{}}

	if w.SpanDays() > ipUsageMaxWindowDays {
		result.BeyondRetention = true
		result.Reason = ipUsageBeyondRetention
		return result, nil
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}

	var rows []model.InboundIPHour
	err := db.Where("inbound_id = ? and hour_start >= ? and hour_start < ?",
		inboundId, w.Start, w.End).Find(&rows).Error
	if err != nil {
		return nil, err
	}

	result.Split = hasSplitBytes(rows)

	type agg struct {
		up, down int64
		lastSeen int64
	}
	byIP := map[string]*agg{}
	for _, r := range rows {
		a := byIP[r.IP]
		if a == nil {
			a = &agg{}
			byIP[r.IP] = a
		}
		if result.Split {
			a.up += r.ActiveUp
			a.down += r.ActiveDown
		} else {
			// 降级：拆不出上下行时把合计全部计进 Down。界面在 Split 为
			// false 时只显示合计数，不显示箭头，所以计进哪一侧不影响显示；
			// 计进 Down 而不是对半分，是为了让「合计 = Up + Down」这个
			// 恒等式在任何情形下都成立，前端不必为降级另写一套求和。
			a.down += r.ActiveBytes
		}
		if hourMillis := r.HourStart * 1000; hourMillis > a.lastSeen {
			a.lastSeen = hourMillis
		}
	}

	entries := make([]IPUsageEntry, 0, len(byIP))
	for ip, a := range byIP {
		e := IPUsageEntry{IP: ip, Up: a.up, Down: a.down, LastSeen: a.lastSeen}
		// 离线 IP 不在连接表快照里，归属地必须重新查。走 locateWithIPDB
		// 是为了与在线明细、访问日志的来源列表共用同一套判定。
		if parsed := net.ParseIP(ip); parsed != nil {
			loc := locateWithIPDB(s.ipdbService, parsed)
			e.Location, e.LocationAlt = loc.Location, loc.LocationAlt
			e.ISP, e.ISPAlt = loc.ISP, loc.ISPAlt
			e.Sources = loc.Sources
		}
		entries = append(entries, e)
	}

	// 用量降序；同量时按 IP 字节序，保证同一份输入永远给出同一个次序
	//（与 onlineTracker.snapshotAt 的排序同源）。遍历 map 产生的顺序不定，
	// 不排的话每次刷新表格行都会跳。
	sort.Slice(entries, func(i, j int) bool {
		li, lj := entries[i].Up+entries[i].Down, entries[j].Up+entries[j].Down
		if li != lj {
			return li > lj
		}
		return bytes.Compare(
			net.ParseIP(entries[i].IP).To16(),
			net.ParseIP(entries[j].IP).To16(),
		) < 0
	})

	if len(entries) > ipUsageMaxEntries {
		for _, e := range entries[ipUsageMaxEntries:] {
			result.OtherUp += e.Up
			result.OtherDown += e.Down
		}
		result.OtherCount = len(entries) - ipUsageMaxEntries
		entries = entries[:ipUsageMaxEntries]
	}
	result.Entries = entries

	if !result.Split && len(entries) > 0 {
		result.Reason = ipUsageNoSplitReason
	}
	return result, nil
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./web/service/ -run TestIPUsage -v`

Expected: 全部 PASS（8 条）。

- [ ] **Step 5: 跑整包，确认没有踩到别的用例**

Run: `go test ./web/service/`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add web/service/ip_usage.go web/service/ip_usage_test.go
git commit -m "feat(traffic): 按来源 IP 的分时段用量查询

IPUsageService.Query 按窗口聚合 InboundIPHour，用量降序、同量按 IP 字节序
（遍历 map 的顺序不定，不排的话每次刷新表格行都会跳）。

三处降级都带原因而不是返回 0：窗口超保留期整块降级、整批老行按合计口径
显示、库不可用沿用 trafficDBUnavailable。超过 200 条时长尾合并成 Other，
总量不缩水。

归属地复用 locateWithIPDB，与在线明细、访问日志同一套判定，离线行因此
也能显示「存疑」标记。"
```

---

### Task 4: `History` 支持窗口与 `Total`

**Files:**
- Modify: `web/service/traffic_history.go`（`TrafficHistoryResult` 第 216-222 行附近、`History` 第 291-328 行；新增 `buildSlotsInWindow` 与 `HistoryWindow`）
- Test: `web/service/traffic_history_test.go`（追加）

**Interfaces:**
- Consumes: `service.TrafficWindow`（Task 2）
- Produces:
  - `TrafficHistoryResult.Total TrafficPoint`（json `total`）
  - `service.(*TrafficHistoryService).HistoryWindow(inboundId int, w TrafficWindow) (*TrafficHistoryResult, error)`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/traffic_history_test.go` 末尾。**复用该文件既有的
`setupTrafficTest` / `mkTrafficInbound` / `mustLoadShanghai` / `writeBucket`，
不要自建同样的辅助函数**；文件顶部的 import 已含 `time` 与 `a-ui/database/model`，
不用改。

时区一律走 `mustLoadShanghai`：`HistoryWindow` 内部用
`s.settingService.GetTimeLocation()`，而 `defaultValueMap["timeLocation"]`
就是 `Asia/Shanghai`（`web/service/setting.go:33`）。测试若拿 `time.UTC`
构造桶，被测代码按上海时区重算刻度，两边对不上、断言必挂。

```go
// Total 必须严格等于 Points 各点之和，而不是一次独立的 SQL SUM。
//
// History 用 bucket_start 精确相等去 join buildSlots 按当前时区重算的刻度，
// 而独立的 SUM 聚合不受对齐约束。管理员改过面板时区之后（整小时时区切换会
// 让旧桶与新刻度完全不相交，见 2026-09-04-traffic-history-design.md §3.3），
// 独立 SUM 会让「图是一条平的 0 线、数字却有值」——那是最难解释的一种不
// 一致。让 Total 等于图上那些点的和，两者永远自洽。
func TestHistoryWindowTotalEqualsSumOfPoints(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30301, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	for i, hour := range []int{10, 11, 12} {
		at := time.Date(2026, 9, 10, hour, 0, 0, 0, sh)
		writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(at, sh),
			int64(100*(i+1)), int64(200*(i+1)))
	}

	w := ParseWindow(WindowToday, "", "", sh, now)
	res, err := svc.HistoryWindow(in.Id, w)
	if err != nil {
		t.Fatalf("HistoryWindow: %v", err)
	}

	var sumUp, sumDown int64
	for _, p := range res.Points {
		sumUp += p.Up
		sumDown += p.Down
	}
	if res.Total.Up != sumUp || res.Total.Down != sumDown {
		t.Errorf("Total = %d/%d，期望等于各点之和 %d/%d",
			res.Total.Up, res.Total.Down, sumUp, sumDown)
	}
	if res.Total.Up != 600 || res.Total.Down != 1200 {
		t.Errorf("Total = %d/%d，期望 600/1200", res.Total.Up, res.Total.Down)
	}
}

// 落在窗口外的桶不能被算进去。
//
// 「今日」是本地今天 0 点到现在，昨天那个桶必须落在外面——否则相邻两个
// 窗口会把边界重复计算，「今日」加「昨日」会大于「近 2 日」。
func TestHistoryWindowExcludesBucketsOutsideWindow(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30302, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	inside := time.Date(2026, 9, 10, 10, 0, 0, 0, sh)
	outside := time.Date(2026, 9, 9, 10, 0, 0, 0, sh)
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(inside, sh), 100, 200)
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(outside, sh), 999, 999)

	w := ParseWindow(WindowToday, "", "", sh, now)
	res, err := svc.HistoryWindow(in.Id, w)
	if err != nil {
		t.Fatalf("HistoryWindow: %v", err)
	}
	if res.Total.Up != 100 || res.Total.Down != 200 {
		t.Errorf("Total = %d/%d，期望 100/200（昨天那个桶不该算进来）",
			res.Total.Up, res.Total.Down)
	}
}

// 聚合必须带 granularity：小时桶与日桶各自独立累加，日桶不由小时桶汇总
// 而来，不带条件会把同一段时间算两遍。
func TestHistoryWindowFiltersByGranularity(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30303, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	// 同一个时刻各写一个粒度的桶，值相同。「今日」走小时粒度，只该命中前者。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignDay(now, sh), 100, 200)
	writeBucket(t, model.GranularityDay, in.Id, model.AlignDay(now, sh), 100, 200)

	w := ParseWindow(WindowToday, "", "", sh, now)
	res, err := svc.HistoryWindow(in.Id, w)
	if err != nil {
		t.Fatalf("HistoryWindow: %v", err)
	}
	if res.Total.Up != 100 {
		t.Errorf("Total.Up = %d，期望 100（日桶不该被一起算进来）", res.Total.Up)
	}
}

// 旧的 History(range) 签名必须继续可用且行为不变——Overview 与任何未改的
// 调用方都依赖它。改它的签名会牵动系统状态页，那不在本次范围。
func TestLegacyHistoryStillWorks(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30304, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	res, err := svc.History(in.Id, Range24h, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(res.Points) != 24 {
		t.Errorf("点数 = %d，期望 24", len(res.Points))
	}
	// 旧路径也要填 Total，两条路径的语义必须一致。
	if res.Total.Up != 0 || res.Total.Down != 0 {
		t.Errorf("Total = %+v，期望零值（库里没写任何桶）", res.Total)
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败**

Run: `go test ./web/service/ -run 'TestHistoryWindow|TestLegacyHistory' -v`

Expected: 编译失败，`svc.HistoryWindow undefined`。

- [ ] **Step 3: 给 `TrafficHistoryResult` 加 `Total`**

`web/service/traffic_history.go`：

```go
type TrafficHistoryResult struct {
	Granularity string         `json:"granularity"`
	Labels      []string       `json:"labels"`
	Points      []TrafficPoint `json:"points"`
	// Total 是 Points 各点之和，**不是一次独立的 SQL SUM**。
	//
	// History 用 bucket_start 精确相等去 join 按当前时区重算的刻度，而独立
	// 的 SUM 聚合不受对齐约束。管理员改过面板时区之后（整小时时区切换会让
	// 旧桶与新刻度完全不相交），独立 SUM 会让「图是一条平的 0 线、数字却
	// 有值」——那是最难解释的一种不一致。让它等于图上那些点的和，两者永远
	// 自洽，数据在保留期内随新数据自愈。
	Total  TrafficPoint `json:"total"`
	Reason string       `json:"reason"`
}
```

- [ ] **Step 4: 新增 `buildSlotsInWindow` 与 `HistoryWindow`，并让旧 `History` 复用同一段**

在 `buildSlots` 之后追加：

```go
// buildSlotsInWindow 生成窗口内全部刻度的桶起点，升序。
//
// 与 buildSlots 的区别是端点来自窗口而不是「从 now 往回数 count 个」。
// 小时用算术递增；日必须用 AddDate，因为一天不总是 86400 秒。
func buildSlotsInWindow(g model.TrafficGranularity, w TrafficWindow, loc *time.Location) []int64 {
	var slots []int64
	if g == model.GranularityDay {
		day := time.Unix(model.AlignDay(time.Unix(w.Start, 0).In(loc), loc), 0).In(loc)
		for day.Unix() < w.End {
			slots = append(slots, day.Unix())
			day = day.AddDate(0, 0, 1)
		}
		return slots
	}
	start := model.AlignHour(time.Unix(w.Start, 0).In(loc), loc)
	for t := start; t < w.End; t += 3600 {
		slots = append(slots, t)
	}
	return slots
}

// sumPoints 把各点求和。Total 的唯一来源，见 TrafficHistoryResult.Total 的注释。
func sumPoints(points []TrafficPoint) TrafficPoint {
	var total TrafficPoint
	for _, p := range points {
		total.Up += p.Up
		total.Down += p.Down
	}
	return total
}

// HistoryWindow 返回单个入站在给定窗口内的分时用量，刻度稠密（缺失的桶补零）。
//
// 与 History 的区别只是范围来自 TrafficWindow 而不是固定档位。History 保留
// 不动：Overview 与任何未改的调用方仍依赖它，改它的签名会牵动系统状态页。
func (s *TrafficHistoryService) HistoryWindow(inboundId int, w TrafficWindow) (*TrafficHistoryResult, error) {
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	slots := buildSlotsInWindow(w.Granularity, w, loc)
	result := &TrafficHistoryResult{
		Granularity: granularityName(w.Granularity),
		Labels:      formatLabels(w.Granularity, slots, loc),
		Points:      make([]TrafficPoint, len(slots)),
	}
	for i, start := range slots {
		result.Points[i] = TrafficPoint{T: start}
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}
	if len(slots) == 0 {
		return result, nil
	}

	var rows []model.TrafficBucket
	// granularity 条件不能省：小时桶与日桶各自独立累加，日桶不由小时桶
	// 汇总而来，不带条件会把同一段时间算两遍。
	err = db.Where("granularity = ? and inbound_id = ? and bucket_start >= ? and bucket_start < ?",
		w.Granularity, inboundId, slots[0], w.End).Find(&rows).Error
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
	result.Total = sumPoints(result.Points)
	return result, nil
}
```

同时在旧 `History` 的 `return result, nil` 之前加一行，让两条路径的 `Total` 语义一致：

```go
	result.Total = sumPoints(result.Points)
	return result, nil
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./web/service/ -run 'TestHistory|TestLegacyHistory|TestTraffic' -v`

Expected: 全部 PASS，含既有的 history 用例。

- [ ] **Step 6: 提交**

```bash
git add web/service/traffic_history.go web/service/traffic_history_test.go
git commit -m "feat(traffic): History 支持任意窗口并返回合计

新增 HistoryWindow(inboundId, window)，范围来自 TrafficWindow 而不是固定
档位。旧的 History(range) 保留不动——Overview 与系统状态页仍依赖它。

Total 是 Points 各点之和而不是独立的 SQL SUM：独立 SUM 不受桶对齐约束，
管理员改过时区后会出现「图是平的 0 线、数字却有值」，那是最难解释的一种
不一致。查询带 granularity 条件，两级粒度各自独立累加，不带会算两遍。"
```

---

### Task 5: Controller——新路由与入参绑定

**Files:**
- Modify: `web/controller/inbound.go`（`initRouter` 第 32-52 行、字段声明第 18-24 行附近、`getTrafficHistory` 第 325-355 行附近；新增 `getIPUsage`）
- Test: `web/controller/inbound_ip_usage_test.go`（新建）

**Interfaces:**
- Consumes: `service.IPUsageService.Query`（Task 3）、`service.ParseWindow`（Task 2）、`service.(*TrafficHistoryService).HistoryWindow`（Task 4）
- Produces: 路由 `POST /aui/inbound/ipUsage/:id`；`getTrafficHistory` 接受 `range`/`start`/`end` 三个入参

- [ ] **Step 1: 写失败的测试**

先确认 controller 包既有测试的写法：

Run: `ls web/controller/*_test.go && head -30 web/controller/inbound_traffic_reset_test.go`

新建 `web/controller/inbound_ip_usage_test.go`。**本测试只覆盖窗口入参的翻译与钳制**，不起 HTTP 服务（controller 的钳制逻辑全部委托给 `service.ParseWindow`，那里已有单测；这里钉的是「controller 确实把三个入参原样交给了它」）：

```go
package controller

import (
	"testing"
	"time"

	"a-ui/web/service"
)

// controller 必须把 range/start/end 三个入参原样交给 ParseWindow，
// 而不是自己解释其中任何一个。
//
// 自己解释的后果是两处逻辑慢慢漂移：ParseWindow 的单测全绿，而真实请求
// 走的是 controller 里那份，界面上的「今日」和测试里的「今日」不是同一段
// 时间，没有任何一层会报错。
func TestIPUsageFormBindsAllThreeWindowFields(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	// 与 getIPUsage 内部完全相同的调用形状。这条测试的价值在于：
	// 将来谁把 controller 改成只传 range，这里会立刻变红。
	w := service.ParseWindow("custom", "2026-09-01", "2026-09-05", loc, now)

	wantStart := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).Unix()
	if w.Start != wantStart {
		t.Errorf("Start = %d，期望 %d", w.Start, wantStart)
	}
	if w.End <= w.Start {
		t.Errorf("End = %d 不大于 Start = %d", w.End, w.Start)
	}
}

// 越界入参不得让接口报错，一律钳制后照常返回。
func TestIPUsageWindowClampsInsteadOfFailing(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, loc)

	for _, c := range []struct{ name, start, end string }{
		{"custom", "垃圾", "更多垃圾"},
		{"custom", "2000-01-01", "2026-09-09"},
		{"不存在的档位", "", ""},
		{"custom", "2026-09-09", "2026-09-01"},
	} {
		w := service.ParseWindow(c.name, c.start, c.end, loc, now)
		if w.End <= w.Start {
			t.Errorf("range=%q start=%q end=%q 得到空区间 [%d, %d)",
				c.name, c.start, c.end, w.Start, w.End)
		}
		if w.End > now.Unix() {
			t.Errorf("range=%q 的 End = %d 超过了现在 %d", c.name, w.End, now.Unix())
		}
	}
}
```

- [ ] **Step 2: 运行测试，确认它失败或通过**

Run: `go test ./web/controller/ -run TestIPUsage -v`

Expected: PASS（这两条只依赖 Task 2 的 `ParseWindow`，Task 2 完成后即为绿）。它们是**回归护栏**而非驱动实现的红灯——真正驱动实现的是下面 Step 4 的手工验证。

- [ ] **Step 3: 加 service 字段与路由**

`web/controller/inbound.go`，在结构体字段里加：

```go
	ipUsageService service.IPUsageService
```

在 `initRouter` 的 `g.POST("/traffic/overview", a.getTrafficOverview)` 之后加：

```go
	g.POST("/ipUsage/:id", a.getIPUsage)
```

- [ ] **Step 4: 实现 `getIPUsage`，并让 `getTrafficHistory` 接受窗口入参**

在 `getTrafficOverview` 之后追加：

```go
// getIPUsage 返回某入站在指定窗口内各来源 IP 的用量。
//
// 刻意不并进 /onlines/:id：那个接口每 2 秒轮询一次，且按展开的入站数逐个
// 请求（inbounds.html 的 syncOnlineTimer）。并进去等于每 2 秒对一张 30 天
// 的表做一次聚合查询再乘以展开的入站数。这边的数据每 60 秒才可能变一次，
// 切换档位时拉一次就够。
func (a *InboundController) getIPUsage(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取来源用量", err)
		return
	}
	form := struct {
		Range string `form:"range"`
		Start string `form:"start"`
		End   string `form:"end"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取来源用量", err)
		return
	}
	loc, err := a.settingService.GetTimeLocation()
	if err != nil {
		jsonMsg(c, "获取来源用量", err)
		return
	}
	// 三个入参原样交给 ParseWindow，controller 不自己解释其中任何一个：
	// 全部钳制都在那个纯函数里，两处各写一份迟早会漂移，而漂移之后界面上
	// 的「今日」和测试里的「今日」不是同一段时间，没有任何一层会报错。
	w := service.ParseWindow(form.Range, form.Start, form.End, loc, time.Now())
	result, err := a.ipUsageService.Query(id, w)
	if err != nil {
		jsonMsg(c, "获取来源用量", err)
		return
	}
	jsonObj(c, result, nil)
}
```

把 `getTrafficHistory` 里的 form 与调用改成：

```go
	form := struct {
		Range string `form:"range"`
		Start string `form:"start"`
		End   string `form:"end"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	loc, err := a.settingService.GetTimeLocation()
	if err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	w := service.ParseWindow(form.Range, form.Start, form.End, loc, time.Now())
	result, err := a.trafficHistoryService.HistoryWindow(id, w)
	if err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	jsonObj(c, result, nil)
```

**确认 `a.settingService` 这个字段在 `InboundController` 上已经存在**：

Run: `grep -n "settingService" web/controller/inbound.go | head -3`

若不存在，在结构体里补 `settingService service.SettingService`（无状态空结构体，零值可用，不需要初始化）。

- [ ] **Step 5: 编译并跑测试，重点盯住三条既有的 history 端点测试**

Run: `go build ./... && go test ./web/controller/ ./web/service/`

Expected: PASS。

`web/controller/traffic_test.go` 里有三条针对 `/traffic/history` 的既有测试，本任务改了那个 handler 的语义，**必须确认它们仍然绿**：

| 测试 | 断言 | 新语义下为什么仍成立 |
|---|---|---|
| `TestTrafficHistoryEndpointBindsRangeFromUrlencodedForm` | `range=1y` → `granularity=="day"` 且 365 个点 | `ParseWindow(Window1y)` 起点是 364 天前的 0 点、终点是现在，跨度 > 30 天走日粒度；`buildSlotsInWindow` 从起点每天一个直到 < End，恰好 365 个（含今天） |
| `TestTrafficHistoryEndpointDefaultsWithoutRange` | 空 `range` → `granularity=="hour"` | 空串认不出，`ParseWindow` 回落「今日」，跨度远小于 30 天走小时粒度。**它只断言粒度、不断言点数**——点数确实从 24 变成了「今天已过的小时数+1」 |
| `TestTrafficHistoryEndpointRejectsNonNumericId` | `/history/abc` → 失败 | 新 handler 仍先 `strconv.Atoi(c.Param("id"))` 再绑定表单，顺序不能颠倒 |

**这三条一条都不许改去迁就实现。** 第一条钉的是「绑定标签必须是 form 而不是 json」——前端发的是 urlencoded，标签写错的话 `range` 永远是空串、图永远只显示默认那一档，而接口照常返回 success。这条防线与本次改动无关，但极易在重写 form 结构体时被顺手破坏。

- [ ] **Step 6: 手工验证接口真的通**

```bash
XUI_DEBUG=true go run main.go
```

另开一个终端（端口与 basePath 用 `./a-ui setting -show` 查；下面假设默认 54321 与 `/`）：

```bash
# 先登录拿 cookie
curl -s -c /tmp/aui.cookie -X POST http://127.0.0.1:54321/login \
  -d 'username=admin&password=admin'

# 今日
curl -s -b /tmp/aui.cookie -X POST http://127.0.0.1:54321/aui/inbound/ipUsage/1 \
  -H 'Content-Type: application/json' -d '{"range":"today"}'

# 自定义区间
curl -s -b /tmp/aui.cookie -X POST http://127.0.0.1:54321/aui/inbound/ipUsage/1 \
  -H 'Content-Type: application/json' -d '{"range":"custom","start":"2026-09-01","end":"2026-09-05"}'

# 越界：必须钳制后正常返回，不能 500
curl -s -b /tmp/aui.cookie -X POST http://127.0.0.1:54321/aui/inbound/ipUsage/1 \
  -H 'Content-Type: application/json' -d '{"range":"custom","start":"垃圾","end":"垃圾"}'

# 入站合计：确认返回体里有 total
curl -s -b /tmp/aui.cookie -X POST http://127.0.0.1:54321/aui/inbound/traffic/history/1 \
  -H 'Content-Type: application/json' -d '{"range":"7d"}'
```

Expected: 四个请求都返回 `{"success":true,...}`；最后一个的 `obj` 里含 `total` 字段。

- [ ] **Step 7: 提交**

```bash
git add web/controller/inbound.go web/controller/inbound_ip_usage_test.go
git commit -m "feat(inbound): 来源用量接口与窗口入参

新增 POST /aui/inbound/ipUsage/:id；getTrafficHistory 改走 HistoryWindow，
接受 range/start/end 三个入参。

三个入参原样交给 service.ParseWindow，controller 不自己解释其中任何一个
——全部钳制都在那个纯函数里，两处各写一份迟早会漂移，而漂移之后界面上的
「今日」和测试里的「今日」不是同一段时间，没有任何一层会报错。

ipUsage 刻意不并进 /onlines/:id：那个接口每 2 秒轮询且按展开的入站数逐个
请求，并进去等于每 2 秒对一张 30 天的表做一次聚合查询。"
```

---

### Task 6: 前端——时间控件、入站合计、表格合并

**Files:**
- Modify: `web/html/xui/inbounds.html`（模板第 95-165 行、`onlineColumns` 第 405-445 行、`data` 第 460-480 行、methods 第 700-800 行）

**Interfaces:**
- Consumes: `POST /aui/inbound/ipUsage/:id`（返回 `{entries, split, beyondRetention, otherCount, otherUp, otherDown, reason}`）；`POST /aui/inbound/traffic/history/:id`（返回体新增 `total: {up, down}`）
- Produces: 无（终端交付）

- [ ] **Step 1: 把时间控件从图表上方移到展开行顶部并换档位**

现有第 151-160 行那段 `a-radio-group`（`24 小时 / 7 天 / 30 天 / 1 年`）整块删掉，在展开行最上方（`<template slot="expandedRowRender" ...>` 内、在线明细 `a-table` **之前**）插入：

```html
<div style="margin-bottom: 12px">
    <a-radio-group size="small" :value="trafficRangeOf(dbInbound.id)"
                   @change="e => changeTrafficRange(dbInbound.id, e.target.value)">
        <a-radio-button value="today">今日</a-radio-button>
        <a-radio-button value="3d">近 3 日</a-radio-button>
        <a-radio-button value="7d">近 7 日</a-radio-button>
        <a-radio-button value="30d">近 30 日</a-radio-button>
        <a-radio-button value="1y">近 1 年</a-radio-button>
    </a-radio-group>
    <a-range-picker size="small" style="margin-left: 8px; width: 240px"
                    :value="customRangeOf(dbInbound.id)"
                    @change="d => changeCustomRange(dbInbound.id, d)"></a-range-picker>
    <span style="margin-left: 16px">
        本段入站用量
        <a-tag color="green">↑ [[ sizeFormat(totalOf(dbInbound.id).up) ]]</a-tag>
        <a-tag color="blue">↓ [[ sizeFormat(totalOf(dbInbound.id).down) ]]</a-tag>
        <a-tooltip>
            <template slot="title">
                来自 xray 的流量统计，是代理转发的应用层字节，与下方按来源 IP 的分项口径不同。
            </template>
            <a-icon type="info-circle"></a-icon>
        </a-tooltip>
    </span>
</div>
```

- [ ] **Step 2: 改 `onlineColumns` 的用量列，并给列头加口径说明**

把第 435-440 行那个 `本次上线用量↑|↓` 列改成：

```js
    }, {
        // slots.title 而不是 title：这一列的列头要带 tooltip，纯字符串做不到。
        slots: { title: 'usageTitle' },
        align: 'center',
        width: 150,
        scopedSlots: { customRender: 'onlineTraffic' },
    }, {
```

在 `a-table` 上补一个列头插槽（与 `slot="onlineTraffic"` 并列）：

```html
<template slot="usageTitle">
    本段用量↑|↓
    <a-tooltip>
        <template slot="title">
            来自内核连接表，含 TLS 与 WebSocket 协议开销，<b>与上方入站合计用量不相等</b>（后者是 xray 统计的应用层字节）。各来源 IP 分项之和会略大于或略小于入站合计。
        </template>
        <a-icon type="info-circle"></a-icon>
    </a-tooltip>
</template>
```

**这段 tooltip 不要因为觉得啰嗦而删掉**：它是设计文档 §2.1 那个物理约束在界面上的唯一出口，删掉之后管理员对不上账时没有任何解释来源。

- [ ] **Step 3: 改用量列的渲染，覆盖四种状态**

把 `slot="onlineTraffic"` 那段改成：

```html
<template slot="onlineTraffic" slot-scope="text, online">
    <template v-if="ipUsageOf(dbInbound.id).beyondRetention">
        <a-tooltip>
            <template slot="title">[[ ipUsageOf(dbInbound.id).reason ]]</template>
            <span>—</span>
        </a-tooltip>
    </template>
    <template v-else-if="online.usageMissing">
        <a-tooltip>
            <template slot="title">活跃满 1 分钟后开始计入</template>
            <a-tag>统计中</a-tag>
        </a-tooltip>
    </template>
    <template v-else-if="!ipUsageOf(dbInbound.id).split">
        <a-tooltip>
            <template slot="title">该时段为升级前写入的数据，无法拆分上下行，显示的是合计用量</template>
            <span>[[ sizeFormat(online.usageUp + online.usageDown) ]]</span>
        </a-tooltip>
    </template>
    <template v-else>
        [[ sizeFormat(online.usageUp) ]] / [[ sizeFormat(online.usageDown) ]]
    </template>
</template>
```

- [ ] **Step 4: 离线行的列降级**

`firstSeen` / `speed` / `concurrency`（连接数）三处插槽各加一个 `online.offline` 分支。`firstSeen` 那段改成：

```html
<template slot="firstSeen" slot-scope="text, online">
    <template v-if="online.offline">
        <a-tooltip>
            <template slot="title">该来源在所选时段内有用量，但当前不在线。显示的是最后活跃时间。</template>
            <span style="color: rgba(0,0,0,.45)">[[ DateUtil.formatMillis(online.lastSeen) ]]</span>
        </a-tooltip>
    </template>
    <template v-else-if="online.firstSeen">[[ DateUtil.formatMillis(online.firstSeen) ]]</template>
    <template v-else>-</template>
</template>
```

`speed` 那段开头加：

```html
<template slot="speed" slot-scope="text, online">
    <template v-if="online.offline">—</template>
    <template v-else>
        <a-tag color="green">↑ [[ sizeFormat(online.upSpeed) ]]/s</a-tag>
        <a-tag color="blue">↓ [[ sizeFormat(online.downSpeed) ]]/s</a-tag>
    </template>
</template>
```

连接数那一列的 `dataIndex: "conns"` 改成插槽（`scopedSlots: { customRender: 'conns' }`），并加：

```html
<template slot="conns" slot-scope="text, online">
    <template v-if="online.offline">—</template>
    <template v-else>[[ online.conns ]]</template>
</template>
```

「踢下线」按钮已有 `v-if="online.conns > 0"`，离线行的 `conns` 是 0，天然隐藏，不用改。

- [ ] **Step 5: 在表底加长尾合并行**

在在线明细 `a-table` 之后插入：

```html
<div v-if="ipUsageOf(dbInbound.id).otherCount > 0"
     style="padding: 8px 16px; color: rgba(0,0,0,.45)">
    其余 [[ ipUsageOf(dbInbound.id).otherCount ]] 个来源共
    ↑ [[ sizeFormat(ipUsageOf(dbInbound.id).otherUp) ]]
    ↓ [[ sizeFormat(ipUsageOf(dbInbound.id).otherDown) ]]
    （按用量排序后未列出的部分）
</div>
```

- [ ] **Step 6: 加 data 字段与方法**

`data` 里加：

```js
            // inboundId -> IPUsageResult。与 onlines 分开存：那个每 2 秒
            // 刷新一次，这个只在切换档位时拉。
            ipUsages: {},
            // inboundId -> [moment, moment]，自定义区间。为空表示走档位。
            customRanges: {},
            // inboundId -> {up, down}，入站合计。
            trafficTotals: {},
```

`trafficRangeOf` 的默认值从 `'24h'` 改成 `'today'`：

```js
            trafficRangeOf(id) {
                return this.trafficRanges[id] || 'today';
            },
```

新增方法：

```js
            ipUsageOf(id) {
                return this.ipUsages[id] || {
                    entries: [], split: true, beyondRetention: false,
                    otherCount: 0, otherUp: 0, otherDown: 0, reason: '',
                };
            },
            totalOf(id) {
                return this.trafficTotals[id] || { up: 0, down: 0 };
            },
            customRangeOf(id) {
                return this.customRanges[id] || null;
            },
            changeCustomRange(id, dates) {
                this.$set(this.customRanges, id, dates);
                this.reloadWindowed(id);
            },
            // 把档位或自定义区间翻译成请求体。服务端 ParseWindow 认 range
            // 为 custom 时才看 start/end，所以两者互斥地发出去。
            windowParamsOf(id) {
                const dates = this.customRangeOf(id);
                if (dates && dates.length === 2 && dates[0] && dates[1]) {
                    return {
                        range: 'custom',
                        start: dates[0].format('YYYY-MM-DD'),
                        end: dates[1].format('YYYY-MM-DD'),
                    };
                }
                return { range: this.trafficRangeOf(id) };
            },
            // 档位一变，三个数据区一起重拉——它们必须永远在讲同一段时间。
            async reloadWindowed(id) {
                await Promise.all([this.loadTrafficChart(id), this.loadIPUsage(id)]);
            },
            async loadIPUsage(id) {
                const msg = await HttpUtil.post('/aui/inbound/ipUsage/' + id, this.windowParamsOf(id));
                if (!msg.success) {
                    return;
                }
                this.$set(this.ipUsages, id, msg.obj);
            },
```

`changeTrafficRange` 改成同时清掉自定义区间（两者互斥，不清的话点了档位按钮界面不动，因为 `windowParamsOf` 仍然优先返回自定义区间）：

```js
            changeTrafficRange(id, range) {
                // Vue 2 无法侦测新增的对象属性，必须走 $set。
                this.$set(this.trafficRanges, id, range);
                // 档位与自定义区间互斥：不清的话点了档位按钮界面纹丝不动，
                // 因为 windowParamsOf 仍然优先返回那个区间。
                this.$set(this.customRanges, id, null);
                this.reloadWindowed(id);
            },
```

`loadTrafficChart` 里的请求体改成 `this.windowParamsOf(id)`，并在拿到 `data` 后记下合计：

```js
                const msg = await HttpUtil.post('/aui/inbound/traffic/history/' + id, this.windowParamsOf(id));
                if (!msg.success) {
                    return;
                }
                const data = msg.obj;
                this.$set(this.trafficReasons, id, data.reason || '');
                this.$set(this.trafficTotals, id, data.total || { up: 0, down: 0 });
```

`onExpand` 的展开分支里，把 `this.loadTrafficChart(dbInbound.id)` 换成 `this.reloadWindowed(dbInbound.id)`；折叠分支里追加清理：

```js
                    this.$delete(this.ipUsages, dbInbound.id);
                    this.$delete(this.trafficTotals, dbInbound.id);
                    this.$delete(this.customRanges, dbInbound.id);
```

- [ ] **Step 7: 合并在线与离线两个数据源**

`onlineOf(id)` 现在直接返回 `this.onlines[id]`。改成合并后再返回：

```js
            onlineOf(id) {
                const base = this.onlines[id] || emptyOnline;
                return {
                    supported: base.supported,
                    reason: base.reason,
                    list: this.mergeUsage(id, base.list || []),
                };
            },
            // 把「当前在线」与「窗口内有用量」两份数据按 IP 做 outer merge。
            //
            // 只列在线的会漏掉窗口内用过、现在不在线的来源：看「近 30 日」
            // 时表里可能只剩一个 IP，而这 30 天有五个用过——管理员看到的是
            //「这个用户只有一个来源」，对「是不是被共享了」给出了反向误导的
            // 答案，而表格渲染得完全合理。
            //
            // 归属地不存在冲突：两个接口都走服务端的 locateWithIPDB，对同一
            // 个 IP、同一份库必然给出同一个结果，所以在线侧的直接沿用。
            mergeUsage(id, onlineList) {
                const usage = this.ipUsageOf(id);
                const byIP = {};
                usage.entries.forEach(e => { byIP[e.ip] = e; });

                const merged = onlineList.map(o => {
                    const u = byIP[o.ip];
                    delete byIP[o.ip];
                    return Object.assign({}, o, {
                        usageUp: u ? u.up : 0,
                        usageDown: u ? u.down : 0,
                        // 在线但库里还没有这一窗口的记录：显示「统计中」而
                        // 不是 0——后者会让管理员以为这个正在跑满带宽的连接
                        // 没有流量。窗口整块降级时这个标记没有意义，跳过。
                        usageMissing: !u && !usage.beyondRetention,
                        offline: false,
                    });
                });

                // 剩下的就是「窗口内用过、当前不在线」的。按用量降序排在后面
                // ——服务端已经排好序，这里按同一顺序取即可。
                const offline = usage.entries
                    .filter(e => byIP[e.ip] !== undefined)
                    .map(e => ({
                        ip: e.ip,
                        location: e.location, locationAlt: e.locationAlt,
                        isp: e.isp, ispAlt: e.ispAlt, sources: e.sources,
                        conns: 0, upSpeed: 0, downSpeed: 0,
                        usageUp: e.up, usageDown: e.down,
                        usageMissing: false,
                        lastSeen: e.lastSeen,
                        offline: true,
                    }));

                // 在线的在前，离线的在后，两者不混排：管理员一眼要能分出
                //「现在有几个人在连」。
                return merged.concat(offline);
            },
```

`a-table` 的 `:row-key` 若当前是 `ip`，离线行与在线行的 IP 不会重复（merge 时已 delete），不用改。**确认一下**：

Run: `grep -n 'row-key' web/html/xui/inbounds.html`

若 row-key 用的是别的字段且离线行没有该字段，改成 `:row-key="r => r.ip"`。

- [ ] **Step 8: 跑模板测试**

Run: `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v`

Expected: PASS。**这两条是模板改动的唯一自动防线**——`getHtmlTemplate` 会吞掉 `ParseFS` 错误，一个语法错误的模板只会在渲染时报 "template not found"，`go build` 发现不了。

- [ ] **Step 9: 手工验证界面**

```bash
XUI_DEBUG=true go run main.go
```

浏览器打开面板 → 入站列表 → 展开任意一个入站，逐项确认：

1. 顶部一排是 `今日 / 近 3 日 / 近 7 日 / 近 30 日 / 近 1 年` 加一个日期区间选择器，图表上方**没有**第二排时间控件
2. 「本段入站用量」的 ↑/↓ 数字随档位变化
3. 点「近 1 年」：用量列整列变成 `—`，鼠标悬停显示「按来源 IP 的明细只保留 30 天…」
4. 选一个自定义区间：三个数据区一起变；再点任一档位按钮，日期选择器清空、数据跟着变
5. 用量列列头的 ⓘ 悬停能看到口径说明
6. 折叠再展开，图表不重复、不报错

- [ ] **Step 10: 提交**

```bash
git add web/html/xui/inbounds.html
git commit -m "feat(ui): 展开行按窗口显示用量，表格含窗口内活跃过的来源

一个时间控件同时驱动入站合计、按 IP 分项、下方图表——三个数据区必须永远
在讲同一段时间。原先图表上方那排 24小时/7天/30天/1年 被替换掉：一个折叠区
里放两个时间控件，「调了上面以为下面也变了」是必然会发生的误解。

表格语义从「当前在线」扩大为「窗口内活跃过」。只列在线的会漏掉窗口内用过、
现在不在线的来源——看近 30 日时表里可能只剩一个 IP 而实际有五个用过，那对
「是不是被共享了」给出了反向误导的答案，且表格渲染得完全合理。

用量列覆盖四种状态：正常拆分、升级前数据显示合计、在线未满门槛显示「统计中」
（显示 0 会让人以为正在跑满带宽的连接没有流量）、超保留期显示「—」并说明原因。"
```

---

### Task 7: 文档同步

**Files:**
- Modify: `CLAUDE.md`（「用量历史与图表」一节之后新增一小节；「已知偏差与注意事项」追加一条）

**Interfaces:**
- Consumes: 前六个任务的全部产出
- Produces: 无（终端交付）

- [ ] **Step 1: 在 CLAUDE.md 的「用量历史与图表」一节末尾之后，新增一节**

```markdown
## 按来源 IP 的分时段用量

入站展开行里每个来源 IP 的用量列，窗口由展开行顶部那个控件决定（今日 / 近 3 日 / 近 7 日 / 近 30 日 / 近 1 年 / 自定义区间）。设计文档在 `docs/superpowers/specs/2026-09-10-per-ip-usage-window-design.md`。

**数据复用 `InboundIPHour`（共享检测那张表）的 `ActiveUp`/`ActiveDown` 两列**，不是新表。`ActiveBytes` 保留为独立累加的字段、**绝不能改成两列之和**：它是并存判定的门槛判据（`coexistMinActiveBytes`），而升级前写入的行两个新列恒为 0，改成计算值会让那批历史行的判据当场失效，共享检测的结论在升级瞬间整体改变而界面上没有任何提示。这张表因此有了第二个消费者，两者对采集门槛的要求方向相反（并存判定要过滤噪声，用量统计希望不漏），冲突一律以并存判定为准。

**按来源 IP 的用量永远对不上入站总量，这是物理约束不是缺陷。** xray 的 Stats API 只有 `inbound`/`outbound`/`user` 三类计数器（`xray/process.go` 的 `parseTraffics`），没有来源 IP 维度，所以按 IP 的用量只能来自内核连接表——那是含 TLS 记录层与 WS 帧开销的链路层字节，与 xray 统计的应用层字节天生差 5~15%。叠加三条采集门槛（每小时漏首个采样间隔约 0.83%、单个 IP 某小时活跃不满 60 秒整段丢弃且不跨小时结转、单入站单小时 50 个 IP 上限），分项之和系统性偏小。**界面上那条列头 tooltip 是这个约束的唯一出口，不要因为觉得啰嗦而删掉它**——删掉之后管理员对不上账时没有任何解释来源。

**窗口翻译与全部钳制在 `service.ParseWindow`（`web/service/traffic_window.go`），controller 只负责调用。** 五类钳制：认不出的档位、不可解析的日期、`start > end`、跨度超 366 天、`End` 超过现在，一律钳制不报错。controller 不得自己解释其中任何一个入参——两处各写一份迟早漂移，而漂移之后界面上的「今日」和测试里的「今日」不是同一段时间，没有任何一层会报错。

**`TrafficHistoryResult.Total` 是 `Points` 各点之和，不是一次独立的 SQL `SUM`。** `History` 用 `bucket_start` 精确相等去 join 按当前时区重算的刻度，而独立 `SUM` 不受对齐约束——管理员改过时区之后会出现「图是一条平的 0 线、数字却有值」，那是最难解释的一种不一致。

**展开行的表格语义是「窗口内活跃过的来源 IP」，不是「当前在线」。** 只列在线的会让「近 30 日」这种窗口漏掉大半来源，而表格渲染得完全合理——那对「是不是被共享了」给出的是反向误导的答案。在线的排前、离线的排后，两者不混排。离线行的连接数与实时网速显示 `—` 而不是 0。

**新接口 `/aui/inbound/ipUsage/:id` 刻意不并进 `/onlines/:id`**：后者每 2 秒轮询一次且按展开的入站数逐个请求（`inbounds.html` 的 `syncOnlineTimer`），并进去等于每 2 秒对一张 30 天的表做一次聚合查询再乘以展开的入站数。

**系统状态页的总览图（`Overview`）没有跟着改**，仍用旧的 `TrafficRange` 四档滑动窗口。因此面板里同时存在两套时间语义：入站展开行是日历（今日 / 近 N 日），系统状态页是滑动（24 小时 / 7 天 / 30 天 / 1 年）。两个页面之间不互相引用同一个数字，不会产生对账问题，但确实是不一致；将来若把系统状态页也改掉，`rangeSpec` 与 `Range24h` 等常量可以一并退役。
```

- [ ] **Step 2: 在「已知偏差与注意事项」列表末尾追加一条**

```markdown
- **半小时偏移时区下「今日」的边界切不准。** `InboundIPHour` 按 `AlignHourUTC` 对齐到 UTC 整点，而用量窗口的边界按面板时区算。整小时偏移的时区（含 UTC+8）下本地 0 点必然落在 UTC 整点上，能精确切；半小时或一刻钟偏移的时区（Asia/Kolkata、Asia/Tehran）下本地 0 点是 UTC 的半点，**边界那一个小时的字节会整块算进或算出**。刻意不改对齐方式——`database/model/sharing.go` 写了当初选 UTC 的理由：按本地时区对齐会重蹈 `TrafficBucket` 那个「管理员改一次时区、旧桶与新刻度不相交、历史整段消失」的坑。误差上界是窗口边界上的一个小时（「今日」是 1/24，「近 7 日」是 1/168）。
```

- [ ] **Step 3: 跑完整门禁**

Run: `make verify`

Expected: vet 通过、全部测试 PASS、构建成功。

- [ ] **Step 4: 检查最终 diff 没有越界**

```bash
git status --short
git diff HEAD~6 --stat
```

确认改动只落在：`database/model/sharing.go`、`web/service/{sharing.go,sharing_accumulator.go,sharing_test.go,traffic_window.go,traffic_window_test.go,ip_usage.go,ip_usage_test.go,traffic_history.go,traffic_history_test.go}`、`web/controller/{inbound.go,inbound_ip_usage_test.go}`、`web/html/xui/inbounds.html`、`CLAUDE.md`、`docs/superpowers/`。

**不应出现**：`web/service/sharing_stat.go`（共享检测判定逻辑）、`web/html/xui/index.html`（系统状态页）、`web/service/online.go` 的任何行为改动。

- [ ] **Step 5: 提交**

```bash
git add CLAUDE.md
git commit -m "docs: 记录按来源 IP 的分时段用量子系统

把六条会被下一个改动者踩到的约束写进 CLAUDE.md：ActiveBytes 不得改成两列
之和、按 IP 的用量与入站总量对不上是物理约束、钳制只在 ParseWindow 一处、
Total 必须等于各点之和、表格语义是「窗口内活跃过」而非「当前在线」、
ipUsage 不得并进 2 秒轮询的 onlines。

另加一条已知偏差：半小时偏移时区下窗口边界切不准，且刻意不修。"
```
