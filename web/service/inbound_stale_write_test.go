package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// 入站列表只在打开页面和管理员自己操作后才刷新，而 up/down 每 10 秒被流量任务累加、enable 会被
// 到期/超流量任务改写、autoTravel 会被总开关接管改写。整行写回会把这些字段倒回页面打开时的快照：
// 超额被停的人被放回来、用量被抹掉、自动接管被悄悄撤销。

func TestUpdateInboundNeverWritesUpDownFromTheForm(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10031, func(in *model.Inbound) {
		in.Total, in.Up, in.Down = 1<<40, 5000, 7000
	})
	s := InboundService{}
	edit := *in
	edit.Up, edit.Down = 100, 200 // 页面打开时的快照
	edit.Remark = "改个备注"
	if err := s.UpdateInbound(&edit); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}
	if got := reloadInbound(t, in.Id); got.Up != 5000 || got.Down != 7000 {
		t.Errorf("up/down = %d/%d，期望保持库里的 5000/7000（编辑表单不该能改用量）", got.Up, got.Down)
	}
}

func TestUpdateInboundKeepsJobDisabledWhenThePageDidNotTouchEnable(t *testing.T) {
	setupDB(t)
	// 页面打开时它还是启用的；之后任务因超流量停用了它；管理员在旧页面上只改了备注。
	in := makeInboundFor(t, 10032, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, true
		in.Total, in.Up, in.Down = 1000, 600, 400
	})
	s := InboundService{}
	edit := *in
	edit.Enable = true
	edit.Remark = "改个备注"
	base := true
	if err := s.UpdateInboundFrom(&edit, InboundEditBase{Enable: &base}); err != nil {
		t.Fatalf("UpdateInboundFrom: %v", err)
	}
	if got := reloadInbound(t, in.Id); got.Enable || !got.DisabledByTraffic {
		t.Errorf("Enable=%v DisabledByTraffic=%v，期望仍是任务停用的状态", got.Enable, got.DisabledByTraffic)
	}
}

func TestUpdateInboundAppliesEnableTheAdminChanged(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10033, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, true
	})
	s := InboundService{}
	edit := *in
	edit.Enable = true
	base := false
	if err := s.UpdateInboundFrom(&edit, InboundEditBase{Enable: &base}); err != nil {
		t.Fatalf("UpdateInboundFrom: %v", err)
	}
	if got := reloadInbound(t, in.Id); !got.Enable || got.DisabledByTraffic {
		t.Errorf("Enable=%v DisabledByTraffic=%v，管理员启用了它", got.Enable, got.DisabledByTraffic)
	}
}

// 「允许服务器来源」只能从编辑表单改：UpdateInboundFrom 逐字段拷贝，漏拷的话开关保存「成功」却从不落库。
func TestUpdateInboundPersistsAllowServerSources(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10034, func(*model.Inbound) {})
	s := InboundService{}
	edit := *in
	edit.AllowServerSources = true
	if err := s.UpdateInboundFrom(&edit, InboundEditBase{}); err != nil {
		t.Fatalf("UpdateInboundFrom: %v", err)
	}
	if got := reloadInbound(t, in.Id); !got.AllowServerSources {
		t.Fatal("打开允许服务器来源后没有落库")
	}
	edit.AllowServerSources = false
	if err := s.UpdateInboundFrom(&edit, InboundEditBase{}); err != nil {
		t.Fatalf("UpdateInboundFrom: %v", err)
	}
	if got := reloadInbound(t, in.Id); got.AllowServerSources {
		t.Error("关掉允许服务器来源后仍是打开的")
	}
}

func activeAutoPassFor(t *testing.T, inboundId int, now time.Time) *model.InboundRegionPass {
	t.Helper()
	p := &model.InboundRegionPass{InboundId: inboundId, Auto: true, Regions: `["浙江省"]`,
		GrantedAt: now.Add(-time.Hour).UnixMilli(), Until: now.Add(48 * time.Hour).UnixMilli()}
	if err := database.GetDB().Create(p).Error; err != nil {
		t.Fatalf("create pass: %v", err)
	}
	return p
}

// 总开关接管把存量入站的 autoTravel 打开了；页面是接管之前打开的（autoTravel=false），
// 管理员在上面拨了一下启用开关或改了备注——不能把接管悄悄撤销，也不能收回生效中的自动通行证。
func TestUpdateInboundKeepsAdoptedAutoTravelFromStalePage(t *testing.T) {
	setupDB(t)
	now := time.Now()
	in := makeInboundFor(t, 10034, func(in *model.Inbound) { in.AutoTravel = true })
	pass := activeAutoPassFor(t, in.Id, now)
	s := InboundService{}
	edit := *in
	edit.AutoTravel = false
	edit.Remark = "改个备注"
	base := false
	if err := s.UpdateInboundFrom(&edit, InboundEditBase{AutoTravel: &base}); err != nil {
		t.Fatalf("UpdateInboundFrom: %v", err)
	}
	if got := reloadInbound(t, in.Id); !got.AutoTravel {
		t.Error("旧页面原样提交的 autoTravel=false 把接管撤销了")
	}
	var after model.InboundRegionPass
	database.GetDB().First(&after, pass.Id)
	if after.EndedAt != 0 {
		t.Errorf("生效中的自动通行证被结束了（%s）", after.EndReason)
	}
}

func TestUpdateInboundTurnsAutoTravelOffWhenTheAdminDid(t *testing.T) {
	setupDB(t)
	now := time.Now()
	in := makeInboundFor(t, 10035, func(in *model.Inbound) { in.AutoTravel = true })
	pass := activeAutoPassFor(t, in.Id, now)
	s := InboundService{}
	edit := *in
	edit.AutoTravel = false
	base := true
	if err := s.UpdateInboundFrom(&edit, InboundEditBase{AutoTravel: &base}); err != nil {
		t.Fatalf("UpdateInboundFrom: %v", err)
	}
	if got := reloadInbound(t, in.Id); got.AutoTravel {
		t.Error("管理员关掉的自动出差放行没有生效")
	}
	var after model.InboundRegionPass
	database.GetDB().First(&after, pass.Id)
	if after.EndedAt == 0 || after.EndReason != model.RegionPassAutoOff {
		t.Errorf("自动通行证应以 auto_off 结束，实际 ended_at=%d reason=%q", after.EndedAt, after.EndReason)
	}
}

// 列表上的启用开关只改启用状态：改动前它把页面上整行 dbInbound 原样提交，用量、地区、自动出差一起被写回旧值。
func TestSetInboundEnableTouchesOnlyEnable(t *testing.T) {
	setupDB(t)
	in := makeInboundFor(t, 10036, func(in *model.Inbound) {
		in.Up, in.Down, in.AutoTravel, in.Remark = 5000, 7000, true, "原备注"
	})
	s := InboundService{}
	if err := s.SetInboundEnable(in.Id, false); err != nil {
		t.Fatalf("SetInboundEnable(false): %v", err)
	}
	got := reloadInbound(t, in.Id)
	if got.Enable || got.Up != 5000 || got.Down != 7000 || !got.AutoTravel || got.Remark != "原备注" {
		t.Errorf("停用后 enable=%v up/down=%d/%d autoTravel=%v remark=%q", got.Enable, got.Up, got.Down, got.AutoTravel, got.Remark)
	}
	if got.DisabledByTraffic {
		t.Error("手动停用不该被标成「因超流量停用」")
	}

	byTraffic := makeInboundFor(t, 10037, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, true
	})
	if err := s.SetInboundEnable(byTraffic.Id, true); err != nil {
		t.Fatalf("SetInboundEnable(true): %v", err)
	}
	if got := reloadInbound(t, byTraffic.Id); !got.Enable || got.DisabledByTraffic {
		t.Errorf("重新启用后 enable=%v DisabledByTraffic=%v", got.Enable, got.DisabledByTraffic)
	}
}

// 「重置流量」只清用量；因超流量被停、还没到期的，原因随之消失，与周期重置一样拉回来。
func TestResetInboundTrafficZeroesUsageAndRevivesTrafficDisabled(t *testing.T) {
	setupDB(t)
	now := time.Now()
	in := makeInboundFor(t, 10038, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, true
		in.Total, in.Up, in.Down = 1000, 600, 400
	})
	s := InboundService{}
	revived, err := s.ResetInboundTraffic(in.Id, now)
	if err != nil {
		t.Fatalf("ResetInboundTraffic: %v", err)
	}
	got := reloadInbound(t, in.Id)
	if got.Up != 0 || got.Down != 0 {
		t.Errorf("up/down = %d/%d，期望清零", got.Up, got.Down)
	}
	if !revived || !got.Enable || got.DisabledByTraffic {
		t.Errorf("revived=%v enable=%v DisabledByTraffic=%v，期望拉回来", revived, got.Enable, got.DisabledByTraffic)
	}
}

func TestResetInboundTrafficDoesNotReviveExpiredOrManuallyDisabled(t *testing.T) {
	setupDB(t)
	now := time.Now()
	expired := makeInboundFor(t, 10039, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, true
		in.Up, in.Down = 600, 400
		in.ExpiryTime = now.Add(-24 * time.Hour).UnixMilli()
	})
	manual := makeInboundFor(t, 10040, func(in *model.Inbound) {
		in.Enable, in.DisabledByTraffic = false, false
		in.Up, in.Down = 600, 400
	})
	s := InboundService{}
	for _, in := range []*model.Inbound{expired, manual} {
		revived, err := s.ResetInboundTraffic(in.Id, now)
		if err != nil {
			t.Fatalf("ResetInboundTraffic(%d): %v", in.Id, err)
		}
		got := reloadInbound(t, in.Id)
		if revived || got.Enable {
			t.Errorf("入站 %d 被拉回来了（revived=%v enable=%v）", in.Id, revived, got.Enable)
		}
		if got.Up != 0 || got.Down != 0 {
			t.Errorf("入站 %d up/down = %d/%d，期望清零", in.Id, got.Up, got.Down)
		}
	}
}
