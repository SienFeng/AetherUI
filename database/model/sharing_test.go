package model

import (
	"testing"
	"time"
)

// 同一时刻的两种时区表示必须落进同一个桶。这是「按 UTC 对齐」相对
// AlignHour 的全部价值：管理员改面板时区不会让历史桶与新刻度错开。
// AlignHour 那边的教训是改时区后「1 年」档历史整体消失。
//
// 必须用非整小时偏移的时区（如 Asia/Kolkata +5:30）验证。整小时偏移
// （如 Asia/Shanghai +8:00）下，按本地民用时间截断与按 UTC 截断在数学上
// 重合，错误实现也能通过本测试——这会让回归测试保持通过同时掩盖 bug。
func TestAlignHourUTCIsTimezoneIndependent(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	utc := time.Date(2026, 9, 5, 7, 30, 0, 0, time.UTC)
	local := utc.In(kolkata) // 同一时刻，本地表示为 13:00 +05:30

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
