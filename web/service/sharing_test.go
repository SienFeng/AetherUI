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
