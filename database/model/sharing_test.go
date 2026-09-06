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
