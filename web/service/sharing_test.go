package service

import (
	"fmt"
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
	// 用量库句柄是包级变量，会跨用例残留——SQLite 在文件被 t.TempDir 清掉
	// 之后仍能通过已打开的 fd 读到旧数据。计量池就在这个库里，不清空的话
	// 本用例写进池的行会漏进分流注入器的测试，把断言打成随机失败。
	t.Cleanup(database.ResetTrafficDBForTest)
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

// 被并发限制拒绝、连接已断干净的来源不算实质活跃：snapshotAt 会为它补造
// 一条只设 Blocked（Idle 是零值 false）的条目，让管理员在在线明细里看得
// 见是谁被挡；这类来源一个字节都没传过，计入会凭空抬高活跃时长、污染
// 地区建议。
func TestSharingObservableExcludesIdleAndBlocked(t *testing.T) {
	cases := []struct {
		name string
		e    OnlineIP
		want bool
	}{
		{"正常活跃", OnlineIP{Idle: false, Blocked: false}, true},
		{"闲置", OnlineIP{Idle: true, Blocked: false}, false},
		{"被并发限制拒绝、连接已断（Idle 为零值 false）", OnlineIP{Idle: false, Blocked: true}, false},
		{"闲置且被拒绝", OnlineIP{Idle: true, Blocked: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sharingObservable(c.e); got != c.want {
				t.Errorf("sharingObservable(%+v) = %v, want %v", c.e, got, c.want)
			}
		})
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

	// 并存小时数必须造足显示下限。只造一个的话，即使 Summary 错用了保留期
	// 天数、把窗口外的行读了进来，Hours=1 仍低于下限、Flagged() 仍是 false，
	// 测试照样通过——那就探测不到「两个窗口常量混用」这个最容易犯的错误。
	base := now.Add(-(sharingWindowDays + 1) * 24 * time.Hour)
	for i := 0; i < coexistDisplayMinHours; i++ {
		at := base.Add(-time.Duration(i) * time.Hour)
		for ip, province := range map[string]string{"1.1.1.1": "江苏", "2.2.2.2": "上海"} {
			f := sharingFlush{
				InboundId: in.Id, IP: ip, Province: province,
				HourStart: model.AlignHourUTC(at), ActiveSeconds: 3600,
			}
			if err := upsertIPHour(db, f); err != nil {
				t.Fatalf("写入: %v", err)
			}
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

// windowRows 把 0 当作「读全部入站」的哨兵值，那是给 Summary 用的内部约定。
// Detail 必须在入口挡住非正 id，否则 /sharing/detail/0 会静默返回跨所有
// 入站聚合出来的统计与建议。
func TestDetailRejectsNonPositiveInboundId(t *testing.T) {
	setupSharingTest(t)
	svc := SharingService{}
	if _, err := svc.Detail(0, time.Now()); err == nil {
		t.Error("Detail(0) 应当报错：0 是 windowRows「读全部入站」的哨兵值，放行会静默返回跨入站的聚合数据")
	}
}

// Detail 的 Stat/Suggestion 用判定窗口（sharingWindowDays），Hours 明细回溯
// 用保留期（sharingRetentionDays）——这是本任务最容易混用、且混用后不会有
// 任何报错的一处。这条测试一次钉住三处：两个窗口分别生效、Hours 倒序、
// 只列发生过并存的小时。
func TestDetailUsesWindowForStatAndRetentionForHours(t *testing.T) {
	setupSharingTest(t)
	in := mkSharingInbound(t, 31013, "明细")
	db := database.GetTrafficDB()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	writeCoexistHour := func(at time.Time) {
		t.Helper()
		for ip, province := range map[string]string{"1.1.1.1": "江苏", "2.2.2.2": "上海"} {
			f := sharingFlush{
				InboundId: in.Id, IP: ip, Province: province,
				HourStart: model.AlignHourUTC(at), ActiveSeconds: 3600,
			}
			if err := upsertIPHour(db, f); err != nil {
				t.Fatalf("写入: %v", err)
			}
		}
	}

	inWindowA := now.Add(-2 * time.Hour)
	inWindowB := now.Add(-1 * time.Hour)
	writeCoexistHour(inWindowA)
	writeCoexistHour(inWindowB)

	// 窗口外、保留期内：不该进 Stat，但该进 Hours 明细。
	// 这一条是「Stat 用判定窗口、Hours 用保留期」的分界证据。
	outOfWindow := now.Add(-(sharingWindowDays + 1) * 24 * time.Hour)
	writeCoexistHour(outOfWindow)

	// 保留期外：两边都不该有。
	outOfRetention := now.Add(-(sharingRetentionDays + 1) * 24 * time.Hour)
	writeCoexistHour(outOfRetention)

	// 只有单省活跃的一个小时：不构成并存，不该出现在 Hours 里。
	solo := sharingFlush{
		InboundId: in.Id, IP: "3.3.3.3", Province: "江苏",
		HourStart: model.AlignHourUTC(now.Add(-3 * time.Hour)), ActiveSeconds: 3600,
	}
	if err := upsertIPHour(db, solo); err != nil {
		t.Fatalf("写入: %v", err)
	}

	svc := SharingService{}
	got, err := svc.Detail(in.Id, now)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	// Stat 只数判定窗口内的并存小时：两个。若误用保留期会变成三个。
	if got.Stat.Hours != 2 {
		t.Errorf("Stat.Hours = %v, want 2（只数判定窗口内的）", got.Stat.Hours)
	}
	// Hours 回溯到保留期：三个（窗口内 2 + 窗口外保留期内 1），
	// 不含保留期外那个，也不含只有单省的那个小时。
	if len(got.Hours) != 3 {
		t.Fatalf("len(Hours) = %v, want 3（回溯到保留期、剔除保留期外与非并存小时）", len(got.Hours))
	}
	// 最近的排最前。
	if got.Hours[0].HourStart != model.AlignHourUTC(inWindowB) {
		t.Errorf("Hours[0].HourStart = %v, want 最近的 %v",
			got.Hours[0].HourStart, model.AlignHourUTC(inWindowB))
	}
	if got.Hours[len(got.Hours)-1].HourStart != model.AlignHourUTC(outOfWindow) {
		t.Errorf("Hours 末位 = %v, want 最旧的 %v",
			got.Hours[len(got.Hours)-1].HourStart, model.AlignHourUTC(outOfWindow))
	}
}

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

	// 轮次与落库时机的关系必须算准，否则断言会落在一个空的返回值上：
	// step=30、sharingFlushThreshold=60，flush 发生在 seconds-flushedAt>=60
	// 的那一轮。
	//   轮1 t=0   新建 cell 只设基线不计字节，seconds=30，30-0 <60  不落库
	//   轮2 t=30  +1000/+2000，seconds=60，60-0>=60  落库，flushedAt=60
	//   轮3 t=60  重连回退按全量 +100/+200，seconds=90，90-60=30<60  不落库
	//   轮4 t=90  无增量，seconds=120，120-60=60>=60  落库 ← 断言这一轮
	obs := func(up, down int64) []sharingObservation {
		return []sharingObservation{{InboundId: 1, IP: "1.1.1.1", Up: up, Down: down}}
	}
	a.observe(base, obs(500, 900), 30)
	a.observe(base.Add(30*time.Second), obs(1500, 2900), 30)
	a.observe(base.Add(60*time.Second), obs(100, 200), 30)
	flushes := a.observe(base.Add(90*time.Second), obs(100, 200), 30)

	if len(flushes) != 1 {
		t.Fatalf("落库条数 = %d，期望 1（第 4 轮累计 120 秒，距上次落库又满 60 秒）", len(flushes))
	}
	f := flushes[0]
	// 轮2 的 +1000/+2000，加上轮3 重连按全量计入的 +100/+200。
	// 若这里出现负数，说明 up/down 没走 deltaBytes 而是直接相减了。
	if f.ActiveUp != 1100 || f.ActiveDown != 2200 {
		t.Errorf("ActiveUp/ActiveDown = %d/%d，期望 1100/2200", f.ActiveUp, f.ActiveDown)
	}
	if f.ActiveUp+f.ActiveDown != f.ActiveBytes {
		t.Errorf("上下行之和 %d 与 ActiveBytes %d 不等",
			f.ActiveUp+f.ActiveDown, f.ActiveBytes)
	}
}
