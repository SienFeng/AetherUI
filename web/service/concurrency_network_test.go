package service

import (
	"reflect"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

func liveIP(ip string, firstSeen int64) OnlineIP {
	return OnlineIP{IP: ip, Conns: 1, FirstSeen: firstSeen}
}

// 「同时在用的网络数」按网络数：IPv4 一个地址是一个网络（同一个 Wi-Fi 共用一个出口），IPv6 按 /64
// （同一户人家的设备、手机轮换的隐私地址都在同一个 /64 里）。先到的网络留下，后到的整个网络被断开。
func TestPlanNetworkRejectionsCountsIPv6PerSlash64(t *testing.T) {
	list := []OnlineIP{
		liveIP("2001:db8:1:1::10", 100),
		liveIP("2001:db8:1:1::20", 300),
		liveIP("192.0.2.7", 200),
	}
	if got := planNetworkRejections(list, 2, nil); len(got) != 0 {
		t.Errorf("一个 /64 加一个 IPv4 是两个网络，额度 2 不该断: %v", got)
	}
	got := planNetworkRejections(list, 1, nil)
	if !reflect.DeepEqual(got, []string{"192.0.2.7"}) {
		t.Errorf("额度 1：先到的 /64 留下，后到的 192.0.2.7 断开，实际 %v", got)
	}
}

// 被地区规则或机房拦截挡在门外的来源本来就用不了这个入站，不能占名额：额度是 1 的时候，一个先连上、
// 一直被拦着重试的外省共享者会把本人挤掉。
func TestPlanNetworkRejectionsIgnoresBlockedSources(t *testing.T) {
	list := []OnlineIP{
		liveIP("198.51.100.7", 100), // 被地区规则拦着，却先连上
		liveIP("192.0.2.9", 200),    // 本人
	}
	blocked := func(ip string) bool { return ip == "198.51.100.7" }
	if got := planNetworkRejections(list, 1, blocked); len(got) != 0 {
		t.Errorf("被拦的来源占了名额，本人被断开: %v", got)
	}
	if got := planNetworkRejections(list, 1, nil); !reflect.DeepEqual(got, []string{"192.0.2.9"}) {
		t.Errorf("对照：不剔除被拦来源时本人会被断开，实际 %v", got)
	}
}

// 默认同时在用网络数：入站自己没设（0）就按默认；自己设了按自己的；默认为 0 时只管设了额度或有封禁的。
func TestLimitedInboundsAppliesDefaultLimit(t *testing.T) {
	setupDB(t)
	plain := makeInboundFor(t, 10071, func(in *model.Inbound) {})
	own := makeInboundFor(t, 10072, func(in *model.Inbound) { in.ConcurrencyLimit = 3 })
	disabled := makeInboundFor(t, 10073, func(in *model.Inbound) { in.Enable = false })
	_ = disabled
	s := SettingService{}
	if err := s.saveSetting("defaultConcurrencyLimit", "1"); err != nil {
		t.Fatal(err)
	}
	limits := effectiveLimitsOf(t)
	if limits[plain.Id] != 1 || limits[own.Id] != 3 {
		t.Errorf("额度 = %v，期望没设的按默认 1、设了 3 的按 3", limits)
	}
	if _, ok := limits[disabled.Id]; ok {
		t.Error("停用的入站不在 xray 配置里，不该进判定")
	}
	if err := s.saveSetting("defaultConcurrencyLimit", "0"); err != nil {
		t.Fatal(err)
	}
	limits = effectiveLimitsOf(t)
	if _, ok := limits[plain.Id]; ok || limits[own.Id] != 3 {
		t.Errorf("默认为 0（不限）时只管自己设了额度的: %v", limits)
	}
	_ = database.GetDB()
}

func effectiveLimitsOf(t *testing.T) map[int]int {
	t.Helper()
	cs := ConcurrencyService{}
	inbounds, def, err := cs.limitedInboundsWithDefault(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]int{}
	for _, in := range inbounds {
		out[in.Id] = effectiveConcurrencyLimit(in, def)
	}
	return out
}
