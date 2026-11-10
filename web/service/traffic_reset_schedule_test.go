package service

import (
	"testing"
	"time"

	"a-ui/database/model"
)

// 改重置日程（订阅周期的日号、续期挪了日号、面板时区）不能当场触发一次计划外的清零：新日程下「本周期」的
// 时刻多半已经过去、又晚于上次清零的时刻，任务下一轮就会马上清一次。改动前只有「换模式」挡了这一下。

// scheduleDays 选两个日号：旧的那个最近一次时刻早于新的那个，而新的那个本月已经过去。
func scheduleDays(now time.Time, loc *time.Location) (dOld, dNew int) {
	dNew = min(now.In(loc).Day(), 28)
	if dNew > 1 {
		return dNew - 1, dNew
	}
	return 28, 1
}

func futureExpiryOnDay(now time.Time, loc *time.Location, day int) int64 {
	n := now.In(loc)
	return time.Date(n.Year()+1, n.Month(), day, 12, 0, 0, 0, loc).UnixMilli()
}

func panelLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := (&SettingService{}).GetTimeLocation()
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestBillCycleExpiryEditDoesNotResetImmediately(t *testing.T) {
	setupDB(t)
	loc, now := panelLoc(t), time.Now()
	dOld, dNew := scheduleDays(now, loc)
	in := makeInboundFor(t, 10051, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetBillCycle
		in.ExpiryTime = futureExpiryOnDay(now, loc, dOld)
		in.LastResetAt = resetInstantFor(now, dOld, loc)
		in.Total, in.Up, in.Down = 1<<40, 5000, 7000
	})
	edit := *in
	edit.ExpiryTime = futureExpiryOnDay(now, loc, dNew)
	if err := (&InboundService{}).UpdateInbound(&edit); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&InboundService{}).ResetDueTraffic(now.Add(time.Minute), loc); err != nil {
		t.Fatal(err)
	}
	if got := reloadInbound(t, in.Id); got.Up != 5000 || got.Down != 7000 {
		t.Errorf("订阅周期日号 %d → %d 之后当场清零了: up/down=%d/%d", dOld, dNew, got.Up, got.Down)
	}
}

func TestRenewShiftingBillDayRaisesResetFloor(t *testing.T) {
	setupDB(t)
	loc, now := panelLoc(t), time.Now()
	dOld, dNew := scheduleDays(now, loc)
	expiry := futureExpiryOnDay(now, loc, dOld)
	days := 1
	for time.UnixMilli(expiry).In(loc).AddDate(0, 0, days).Day() != dNew {
		days++
	}
	in := makeInboundFor(t, 10052, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetBillCycle
		in.ExpiryTime = expiry
		in.LastResetAt = resetInstantFor(now, dOld, loc)
	})
	if err := (&InboundService{}).RenewInbound(in.Id, days, 0); err != nil {
		t.Fatal(err)
	}
	got := reloadInbound(t, in.Id)
	if got.LastResetAt < resetInstantFor(now, dNew, loc) {
		t.Errorf("续期把订阅周期日号从 %d 挪到 %d，LastResetAt 没跟着顶上去：下一轮会计划外清零", dOld, dNew)
	}
}

func TestTimeZoneChangeDoesNotResetImmediately(t *testing.T) {
	setupDB(t)
	sh, _ := time.LoadLocation("Asia/Shanghai")
	be, _ := time.LoadLocation("Europe/Berlin")
	now := time.Now()
	if n := now.In(sh); n.Day() == 1 && n.Hour() < 8 {
		t.Skip("月初头 8 小时里柏林的本月 1 号还没到，这个场景不成立")
	}
	in := makeInboundFor(t, 10053, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetMonthly
		in.LastResetAt = resetInstantFor(now, 1, sh)
		in.Total, in.Up, in.Down = 1<<40, 5000, 7000
	})
	s := validBaseSetting()
	s.TimeLocation = "Europe/Berlin"
	if err := (&SettingService{}).UpdateAllSetting(s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&InboundService{}).ResetDueTraffic(now.Add(time.Minute), be); err != nil {
		t.Fatal(err)
	}
	if got := reloadInbound(t, in.Id); got.Up != 5000 || got.Down != 7000 {
		t.Errorf("面板时区往西改之后当场清零了: up/down=%d/%d", got.Up, got.Down)
	}
}

// 日程没变的编辑（改个备注）不能顶掉一次已经到期、任务还没来得及跑的重置：每月 1 号 00:00 之后、
// 每小时一轮的任务跑之前改一下入站，这个月就不清了。
func TestEditWithoutScheduleChangeKeepsDueReset(t *testing.T) {
	setupDB(t)
	loc, now := panelLoc(t), time.Now()
	thisMonth := resetInstantFor(now, 1, loc)
	edited := makeInboundFor(t, 10054, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetMonthly
		in.LastResetAt = thisMonth - 1 // 上个周期清过，本周期还没清
		in.Total, in.Up, in.Down = 1<<40, 5000, 7000
	})
	renewed := makeInboundFor(t, 10056, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetMonthly
		in.LastResetAt = thisMonth - 1
	})
	edit := *edited
	edit.Remark = "改个备注"
	if err := (&InboundService{}).UpdateInbound(&edit); err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).RenewInbound(renewed.Id, 30, 0); err != nil {
		t.Fatal(err)
	}
	n, _, err := (&InboundService{}).ResetDueTraffic(now.Add(time.Minute), loc)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("本周期到期的重置只跑了 %d 个，期望改备注的与续期的两个都照常清零（不能被顶掉）", n)
	}
	if got := reloadInbound(t, edited.Id); got.Up != 0 || got.Down != 0 {
		t.Errorf("改了备注的入站没有按期清零: up/down=%d/%d", got.Up, got.Down)
	}
}

// 换时区时，旧时区下本周期的重置已经到期、还没跑：留给任务去清，不能顺手顶掉。
func TestTimeZoneChangeKeepsPendingReset(t *testing.T) {
	setupDB(t)
	sh, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Now()
	in := makeInboundFor(t, 10055, func(in *model.Inbound) {
		in.TrafficResetMode = model.TrafficResetMonthly
		in.LastResetAt = resetInstantFor(now, 1, sh) - 1
	})
	if err := (&InboundService{}).RaiseResetFloorsForTimeZone(now, sh, mustLoc(t, "Europe/Berlin")); err != nil {
		t.Fatal(err)
	}
	if got := reloadInbound(t, in.Id); got.LastResetAt != in.LastResetAt {
		t.Errorf("旧时区下还没跑的重置被顶掉了: %d → %d", in.LastResetAt, got.LastResetAt)
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
