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
