package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

func TestDisabledReasonOf(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, loc)
	at := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, loc).UnixMilli() }
	cases := []struct {
		name        string
		in          model.Inbound
		wantReason  string
		wantRestore int64
	}{
		{"已到期优先", model.Inbound{ExpiryTime: at(2026, 11, 1, 0), DisabledByTraffic: true, TrafficResetMode: model.TrafficResetMonthly},
			disabledReasonExpired, 0},
		{"手动停用", model.Inbound{ExpiryTime: at(2026, 12, 1, 0)}, disabledReasonManual, 0},
		{"超额·每月 1 号恢复", model.Inbound{DisabledByTraffic: true, TrafficResetMode: model.TrafficResetMonthly},
			disabledReasonTraffic, at(2026, 12, 1, 0)},
		// 线上的典型情况：按订阅周期、到期 11-24 20:00，重置日 24 号 00:00 早于到期，会恢复。
		{"超额·订阅周期恢复", model.Inbound{DisabledByTraffic: true, TrafficResetMode: model.TrafficResetBillCycle, ExpiryTime: at(2026, 11, 24, 20)},
			disabledReasonTraffic, at(2026, 11, 24, 0)},
		{"超额·没开重置", model.Inbound{DisabledByTraffic: true}, disabledReasonTraffic, 0},
		// 每月 1 号重置，但 11-20 就到期：重置任务不会拉回已到期的。
		{"超额·重置前已到期", model.Inbound{DisabledByTraffic: true, TrafficResetMode: model.TrafficResetMonthly, ExpiryTime: at(2026, 11, 20, 0)},
			disabledReasonTraffic, 0},
	}
	for _, c := range cases {
		reason, restore := disabledReasonOf(&c.in, now, loc)
		if reason != c.wantReason || restore != c.wantRestore {
			t.Errorf("%s：得到 (%s, %d)，期望 (%s, %d)", c.name, reason, restore, c.wantReason, c.wantRestore)
		}
	}
}

func TestNextResetInstantForRollsOverYearAndClamps(t *testing.T) {
	loc := time.UTC
	if got := nextResetInstantFor(time.Date(2026, 12, 20, 0, 0, 0, 0, loc), 1, loc); got != time.Date(2027, 1, 1, 0, 0, 0, 0, loc).UnixMilli() {
		t.Errorf("跨年：%v", time.UnixMilli(got).UTC())
	}
	// 31 号的周期在二月钳到月末。
	if got := nextResetInstantFor(time.Date(2027, 2, 1, 0, 0, 0, 0, loc), 31, loc); got != time.Date(2027, 2, 28, 0, 0, 0, 0, loc).UnixMilli() {
		t.Errorf("钳制：%v", time.UnixMilli(got).UTC())
	}
	// 恰好是重置时刻那一刻：已经发生过，下一次在下个月。
	if got := nextResetInstantFor(time.Date(2026, 11, 1, 0, 0, 0, 0, loc), 1, loc); got != time.Date(2026, 12, 1, 0, 0, 0, 0, loc).UnixMilli() {
		t.Errorf("当刻：%v", time.UnixMilli(got).UTC())
	}
}

// 列表上的地区拦截提醒：只统计配了地区限制、来源在允许集外、确实进了黑洞的连接。
func TestInboundAttentionGeoBlocked(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	limited := mkInbound(t, 50031)
	if err := database.GetDB().Model(limited).Update("regions", regions).Error; err != nil {
		t.Fatal(err)
	}
	open := mkInbound(t, 50032)
	now := time.Now()
	add := func(inboundId int, src, route string, ago time.Duration, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-ago).UnixMilli(), InboundId: inboundId,
				SourceIP: src, Target: "api.openai.com:443", Route: route, Accepted: true}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	add(limited.Id, "203.0.113.81", model.BlockOutboundTag, time.Hour, 3)
	add(limited.Id, "203.0.113.93", model.BlockOutboundTag, 2*time.Hour, 5)
	add(limited.Id, "198.51.100.77", model.BlockOutboundTag, time.Hour, 4)   // 允许集内：分流规则封的
	add(limited.Id, "203.0.113.81", model.BlockOutboundTag, 30*time.Hour, 9) // 窗口外
	add(open.Id, "203.0.113.81", model.BlockOutboundTag, time.Hour, 2)       // 没配地区限制

	got, err := (&InboundAttentionService{}).Summary(now)
	if err != nil {
		t.Fatal(err)
	}
	g := got[limited.Id]
	if g == nil || g.GeoBlocked == nil {
		t.Fatalf("应有地区拦截统计：%+v", got)
	}
	if g.GeoBlocked.Count != 8 || len(g.GeoBlocked.IPs) != 2 || g.GeoBlocked.IPs[0].IP != "203.0.113.93" {
		t.Errorf("GeoBlocked = %+v，期望共 8 次、2 个来源、按次数降序", g.GeoBlocked)
	}
	if a := got[open.Id]; a != nil && a.GeoBlocked != nil {
		t.Errorf("没配地区限制的入站不应有地区拦截：%+v", a.GeoBlocked)
	}
	if g.DisabledReason != "" {
		t.Errorf("启用中的入站不应有停用原因：%q", g.DisabledReason)
	}
}

func TestInboundAttentionExpiresSoonAndDisabled(t *testing.T) {
	setupAccessLogDB(t)
	now := time.Now()
	soon := mkInbound(t, 50041)
	later := mkInbound(t, 50042)
	off := mkInbound(t, 50043)
	db := database.GetDB()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Model(soon).Update("expiry_time", now.Add(3*24*time.Hour).UnixMilli()).Error)
	must(db.Model(later).Update("expiry_time", now.Add(15*24*time.Hour).UnixMilli()).Error)
	must(db.Model(off).Updates(map[string]any{"enable": false, "expiry_time": now.Add(3 * 24 * time.Hour).UnixMilli()}).Error)

	got, err := (&InboundAttentionService{}).Summary(now)
	if err != nil {
		t.Fatal(err)
	}
	if a := got[soon.Id]; a == nil || !a.ExpiresSoon {
		t.Errorf("3 天后到期应提醒：%+v", a)
	}
	if a := got[later.Id]; a != nil {
		t.Errorf("15 天后到期不提醒：%+v", a)
	}
	if a := got[off.Id]; a == nil || a.ExpiresSoon || a.DisabledReason != disabledReasonManual {
		t.Errorf("停用入站只给停用原因、不提醒到期：%+v", a)
	}
}
