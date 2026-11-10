package service

import (
	"errors"
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
	if got := planNetworkRejections(list, 2, nil, nil); len(got) != 0 {
		t.Errorf("一个 /64 加一个 IPv4 是两个网络，额度 2 不该断: %v", got)
	}
	got := planNetworkRejections(list, 1, nil, nil)
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
	if got := planNetworkRejections(list, 1, blocked, nil); len(got) != 0 {
		t.Errorf("被拦的来源占了名额，本人被断开: %v", got)
	}
	if got := planNetworkRejections(list, 1, nil, nil); !reflect.DeepEqual(got, []string{"192.0.2.9"}) {
		t.Errorf("对照：不剔除被拦来源时本人会被断开，实际 %v", got)
	}
}

// 名额冲突时本人常住省、真的在用的网络优先，不论谁先连上：外省通行证生效期间本人回到家里，通行证省份
// 那边一直连着的话，按先来后到本人每秒被断一次，要等通行证被收回才能连续使用。
func TestPlanNetworkRejectionsPrefersHomeNetworks(t *testing.T) {
	inUse := func(ip string, firstSeen int64) OnlineIP {
		e := liveIP(ip, firstSeen)
		e.Down = coexistMinActiveBytes
		return e
	}
	list := []OnlineIP{
		inUse("198.51.100.7", 100), // 通行证省份的来源，先连上、一直连着
		inUse("192.0.2.9", 200),    // 本人回到家里，已经在用
	}
	home := func(ip string) bool { return ip == "192.0.2.9" }
	if got := planNetworkRejections(list, 1, nil, home); !reflect.DeepEqual(got, []string{"198.51.100.7"}) {
		t.Errorf("家里省的网络应当留下、外省的断开，实际 %v", got)
	}
	if got := planNetworkRejections(list, 1, nil, nil); !reflect.DeepEqual(got, []string{"192.0.2.9"}) {
		t.Errorf("对照：没有常住省信息时按先来后到，本人被断开，实际 %v", got)
	}
	// 同一档里仍按先来后到：两个都在家里省（手机流量 + 家里 Wi-Fi），后到的断开。
	both := func(ip string) bool { return true }
	if got := planNetworkRejections(list, 1, nil, both); !reflect.DeepEqual(got, []string{"192.0.2.9"}) {
		t.Errorf("都在家里省时应按先来后到，实际 %v", got)
	}
	// 被拦的来源仍然不占名额，也不因为在家里省就被留下来占位。
	blocked := func(ip string) bool { return ip == "192.0.2.9" }
	if got := planNetworkRejections(list, 1, blocked, home); len(got) != 0 {
		t.Errorf("被拦来源不计入，剩一个网络不该断，实际 %v", got)
	}
}

// 家里省一条刚连上、只传了几 KB 的连接（设备客户端测速、主动探测）不能把出差中正在用的本人挤掉：
// 门槛与 liveSignals 判「家里在用」相同。
func TestPlanNetworkRejectionsIgnoresHomeProbes(t *testing.T) {
	traveler := liveIP("198.51.100.7", 100)
	traveler.Down = 50 * coexistMinActiveBytes
	probe := liveIP("192.0.2.9", 200)
	probe.Down = 4096
	home := func(ip string) bool { return ip == "192.0.2.9" }
	if got := planNetworkRejections([]OnlineIP{traveler, probe}, 1, nil, home); !reflect.DeepEqual(got, []string{"192.0.2.9"}) {
		t.Errorf("家里省的小连接不该挤掉出差中的本人，实际 %v", got)
	}
	probe.Down = coexistMinActiveBytes
	if got := planNetworkRejections([]OnlineIP{traveler, probe}, 1, nil, home); !reflect.DeepEqual(got, []string{"198.51.100.7"}) {
		t.Errorf("家里真的在用（≥1 MB）时应当优先，实际 %v", got)
	}
}

// homeNetworks 只给持有通行证、存储了地区的入站建允许集；同一组省份只建一次；建不出来的入站不放进去
// （退回先来后到），而不是放一个空集合把所有来源都判成「不在家里」。
func TestHomeNetworksOnlyForInboundsWithPass(t *testing.T) {
	calls := 0
	orig := concurrencyHomeSetOf
	concurrencyHomeSetOf = func(p []string) (*cidrSet, error) {
		calls++
		if p[0] == "坏省份" {
			return nil, errTestHomeSet
		}
		return newCIDRSet([]string{"192.0.2.0/24"}), nil
	}
	t.Cleanup(func() { concurrencyHomeSetOf = orig })

	inbounds := []*model.Inbound{
		{Id: 1, Regions: `["江苏省"]`}, // 有通行证
		{Id: 2, Regions: `["江苏省"]`}, // 有通行证，同一组省份
		{Id: 3, Regions: `["江苏省"]`}, // 没有通行证
		{Id: 4, Regions: ``},        // 有通行证但没有存储地区
		{Id: 5, Regions: `["坏省份"]`}, // 有通行证，允许集建不出来
	}
	pass := &model.InboundRegionPass{}
	passes := map[int]*model.InboundRegionPass{1: pass, 2: pass, 4: pass, 5: pass}
	got := homeNetworks(inbounds, passes)
	if got[1] == nil || got[2] == nil {
		t.Fatalf("持有通行证的入站应有常住省允许集: %v", got)
	}
	if got[3] != nil || got[4] != nil || got[5] != nil {
		t.Errorf("没有通行证、没有存储地区、允许集建不出的入站都不该有: %v", got)
	}
	if calls != 2 {
		t.Errorf("同一组省份只建一次（江苏 + 坏省份 = 2 次），实际 %d 次", calls)
	}
	if !got[1].Contains("192.0.2.9") || got[1].Contains("198.51.100.7") {
		t.Error("允许集内容不对")
	}
}

var errTestHomeSet = errors.New("IP 归属地库未加载")

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
