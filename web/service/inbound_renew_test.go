package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// 独立写死，不复用生产代码里的常量——那样常量本身写错时测试会跟着一起错。
const testDayMillis = int64(86400000)

func nowMillis() int64 {
	return time.Now().Unix() * 1000
}

// makeInbound 直接落库，绕开 AddInbound 的端口校验——本组测试关心的是续期
// 本身，不是新增流程。
func makeInbound(t *testing.T, expiryTime int64, enable bool, up, down int64) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId:         1,
		Port:           10011,
		Protocol:       model.VLESS,
		Tag:            "inbound-10011",
		Remark:         "甲",
		Enable:         enable,
		ExpiryTime:     expiryTime,
		Up:             up,
		Down:           down,
		Total:          0,
		Settings:       `{"clients":[{"id":"x"}]}`,
		StreamSettings: `{"network":"tcp"}`,
		Sniffing:       `{"enabled":true}`,
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	return in
}

func reloadInbound(t *testing.T, id int) *model.Inbound {
	t.Helper()
	s := InboundService{}
	got, err := s.GetInbound(id)
	if err != nil {
		t.Fatalf("GetInbound: %v", err)
	}
	return got
}

func TestRenewInboundAddsDaysOnTopOfUnexpiredTime(t *testing.T) {
	setupDB(t)
	old := nowMillis() + 10*testDayMillis
	in := makeInbound(t, old, true, 0, 0)

	s := InboundService{}
	if err := s.RenewInbound(in.Id, 30, 0); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}

	got := reloadInbound(t, in.Id).ExpiryTime
	want := old + 30*testDayMillis
	if got != want {
		t.Errorf("expiryTime = %d, want %d（相差 %d 天）", got, want, (got-want)/testDayMillis)
	}
}

func TestRenewInboundStartsFromNowWhenAlreadyExpired(t *testing.T) {
	setupDB(t)
	in := makeInbound(t, nowMillis()-5*testDayMillis, false, 0, 0)

	before := nowMillis()
	s := InboundService{}
	if err := s.RenewInbound(in.Id, 30, 0); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}
	after := nowMillis()

	got := reloadInbound(t, in.Id).ExpiryTime
	if got < before+30*testDayMillis || got > after+30*testDayMillis {
		t.Errorf("expiryTime = %d, want 落在 [%d, %d] 区间内", got, before+30*testDayMillis, after+30*testDayMillis)
	}
}

// 无限期（expiryTime = 0）续期后会变成有期限的。这是降级，但只要管理员在
// 弹窗里看到了「无限期 → 具体日期」的预览就是他自己的决定，服务端照做。
func TestRenewInboundStartsFromNowWhenNeverExpires(t *testing.T) {
	setupDB(t)
	in := makeInbound(t, 0, true, 0, 0)

	before := nowMillis()
	s := InboundService{}
	if err := s.RenewInbound(in.Id, 7, 0); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}
	after := nowMillis()

	got := reloadInbound(t, in.Id).ExpiryTime
	if got < before+7*testDayMillis || got > after+7*testDayMillis {
		t.Errorf("expiryTime = %d, want 落在 [%d, %d] 区间内", got, before+7*testDayMillis, after+7*testDayMillis)
	}
}

func TestRenewInboundResetsTrafficAndReenables(t *testing.T) {
	setupDB(t)
	// 因超流量被 CheckInboundJob 停用的典型状态
	in := makeInbound(t, nowMillis()-dayMillis, false, 900, 1100)

	s := InboundService{}
	if err := s.RenewInbound(in.Id, 30, 0); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}

	got := reloadInbound(t, in.Id)
	if got.Up != 0 || got.Down != 0 {
		t.Errorf("up/down = %d/%d, want 0/0", got.Up, got.Down)
	}
	if !got.Enable {
		t.Error("enable = false, want true（续期后应自动重新启用）")
	}
}

func TestRenewInboundSetsExplicitExpiryTime(t *testing.T) {
	setupDB(t)
	in := makeInbound(t, nowMillis()+3*testDayMillis, true, 0, 0)
	want := nowMillis() + 100*testDayMillis

	s := InboundService{}
	if err := s.RenewInbound(in.Id, 0, want); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}

	if got := reloadInbound(t, in.Id).ExpiryTime; got != want {
		t.Errorf("expiryTime = %d, want %d", got, want)
	}
}

// 管理员手动把到期时间设到过去，等价于「立即停用」。此时重新启用毫无意义：
// 30 秒后 CheckInboundJob 会再把它停用，中间还白白重启一次 xray。
func TestRenewInboundKeepsDisabledWhenNewExpiryIsInThePast(t *testing.T) {
	setupDB(t)
	in := makeInbound(t, nowMillis()+30*testDayMillis, false, 500, 500)
	past := nowMillis() - 10*testDayMillis

	s := InboundService{}
	if err := s.RenewInbound(in.Id, 0, past); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}

	got := reloadInbound(t, in.Id)
	if got.ExpiryTime != past {
		t.Errorf("expiryTime = %d, want %d", got.ExpiryTime, past)
	}
	if got.Enable {
		t.Error("enable = true, want false（到期时间在过去时不该重新启用）")
	}
	if got.Up != 500 || got.Down != 500 {
		t.Errorf("up/down = %d/%d, want 500/500（未真正续期时不该清零流量）", got.Up, got.Down)
	}
}

func TestRenewInboundRejectsInvalidArguments(t *testing.T) {
	cases := []struct {
		name       string
		days       int
		expiryTime int64
	}{
		{"两者都没给", 0, 0},
		{"两者都给了", 30, nowMillis() + testDayMillis},
		{"天数为负", -1, 0},
		{"天数超出上限", 4000, 0},
		{"到期时间为负", 0, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupDB(t)
			in := makeInbound(t, 0, true, 7, 8)

			s := InboundService{}
			if err := s.RenewInbound(in.Id, c.days, c.expiryTime); err == nil {
				t.Fatal("want error, got nil")
			}

			got := reloadInbound(t, in.Id)
			if got.ExpiryTime != 0 || got.Up != 7 || got.Down != 8 {
				t.Errorf("参数非法时不该改动任何字段，got expiry=%d up=%d down=%d",
					got.ExpiryTime, got.Up, got.Down)
			}
		})
	}
}

func TestRenewInboundRejectsUnknownId(t *testing.T) {
	setupDB(t)
	s := InboundService{}
	if err := s.RenewInbound(99999, 30, 0); err == nil {
		t.Fatal("want error for unknown id, got nil")
	}
}

func TestRenewInboundLeavesOtherColumnsUntouched(t *testing.T) {
	setupDB(t)
	in := makeInbound(t, nowMillis()+testDayMillis, true, 0, 0)

	s := InboundService{}
	if err := s.RenewInbound(in.Id, 30, 0); err != nil {
		t.Fatalf("RenewInbound: %v", err)
	}

	got := reloadInbound(t, in.Id)
	for _, c := range []struct {
		field     string
		got, want any
	}{
		{"userId", got.UserId, in.UserId},
		{"port", got.Port, in.Port},
		{"tag", got.Tag, in.Tag},
		{"remark", got.Remark, in.Remark},
		{"protocol", got.Protocol, in.Protocol},
		{"total", got.Total, in.Total},
		{"settings", got.Settings, in.Settings},
		{"streamSettings", got.StreamSettings, in.StreamSettings},
		{"sniffing", got.Sniffing, in.Sniffing},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}
