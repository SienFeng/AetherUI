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
