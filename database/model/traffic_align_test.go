package model

import (
	"testing"
	"time"
)

// 夏令时回拨那一小时钟面重复（柏林 2026-10-25 02:00~03:00 走两遍）：两个小时必须落进两个桶。
// 改动前 AlignHour 用 time.Date 重新解析钟面时间，两小时被解析成同一个时刻，流量并进一个桶，
// 图上一格是空的、相邻一格是两倍。
func TestAlignHourSeparatesTheRepeatedDSTHour(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("没有时区数据:", err)
	}
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)  // 当地 02:30 CEST
	second := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC) // 当地 02:30 CET
	a, b := AlignHour(first, berlin), AlignHour(second, berlin)
	if a == b {
		t.Fatalf("回拨那两个小时落进了同一个桶: %d", a)
	}
	if a != first.Truncate(time.Hour).Unix() || b != second.Truncate(time.Hour).Unix() {
		t.Errorf("桶起点 = %d / %d，期望 %d / %d", a, b, first.Truncate(time.Hour).Unix(), second.Truncate(time.Hour).Unix())
	}
	// 半小时偏移的时区：桶起点是当地整点（UTC 的半点）。
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("没有时区数据:", err)
	}
	at := time.Date(2026, 11, 9, 10, 10, 0, 0, time.UTC) // 当地 15:40
	if got, want := AlignHour(at, kolkata), time.Date(2026, 11, 9, 15, 0, 0, 0, kolkata).Unix(); got != want {
		t.Errorf("加尔各答 15:40 的桶起点 = %d，期望当地 15:00 = %d", got, want)
	}
}
