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
