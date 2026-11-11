package service

import (
	"strings"
	"testing"

	"a-ui/database/model"
	"a-ui/util/ipdb"
)

func TestCIDRSetContains(t *testing.T) {
	s := newCIDRSet([]string{"192.0.2.0/24", "203.0.113.0/25", "2001:db8::/32"})
	cases := map[string]bool{
		"192.0.2.0":     true,
		"192.0.2.255":   true,
		"192.0.3.0":     false,
		"198.51.100.1":  false,
		"203.0.113.127": true,
		"203.0.113.128": false,
		"2001:db8::1":   true,
		"2001:db9::1":   false,
		"not-an-ip":     false,
	}
	for ip, want := range cases {
		if got := s.Contains(ip); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestPreviewFromRows(t *testing.T) {
	const mb = 1 << 20
	rows := []model.InboundIPHour{
		{IP: "192.0.2.10", HourStart: 3600, ActiveBytes: 5 * mb, Province: "江苏省"},
		{IP: "192.0.2.10", HourStart: 7200, ActiveBytes: 5 * mb, Province: "江苏省"},
		{IP: "198.51.100.7", HourStart: 7200, ActiveBytes: 3 * mb, Province: "广东省", City: "深圳市", ISP: "电信"},
		{IP: "2001:db8::5", HourStart: 10800, ActiveBytes: 1 * mb},
	}
	p := previewFromRows(rows, newCIDRSet([]string{"192.0.2.0/24"}))

	if p.TotalHours != 3 {
		t.Errorf("TotalHours = %d, 期望 3（不同小时数）", p.TotalHours)
	}
	if p.TotalBytes != 14*mb {
		t.Errorf("TotalBytes = %d", p.TotalBytes)
	}
	if len(p.Blocked) != 1 || p.Blocked[0].IP != "198.51.100.7" {
		t.Fatalf("Blocked = %+v", p.Blocked)
	}
	b := p.Blocked[0]
	if b.Hours != 1 || b.Bytes != 3*mb || b.Province != "广东省" || b.ISP != "电信" || b.LastHour != 7200 {
		t.Errorf("被拦来源 = %+v", b)
	}
	if p.BlockedBytes != 3*mb || p.BlockedHours != 1 {
		t.Errorf("BlockedBytes=%d BlockedHours=%d", p.BlockedBytes, p.BlockedHours)
	}
	// IPv6 不进允许集比较：启用地区限制后它直接用不了，单列出来。
	if len(p.IPv6) != 1 || p.IPv6[0].IP != "2001:db8::5" {
		t.Errorf("IPv6 = %+v", p.IPv6)
	}
}

func TestMajorityProvince(t *testing.T) {
	loc := func(region string) ipdb.SourceLocation {
		return ipdb.SourceLocation{Location: ipdb.Location{Country: "中国", Region: region}}
	}
	if got := majorityProvince([]ipdb.SourceLocation{loc("江苏省"), loc("浙江省"), loc("浙江省")}); got != "浙江省" {
		t.Errorf("多数 = %q", got)
	}
	if got := majorityProvince([]ipdb.SourceLocation{loc("江苏省"), loc("浙江省")}); got != "江苏省" {
		t.Errorf("平票取源顺序第一个, got %q", got)
	}
	foreign := ipdb.SourceLocation{Location: ipdb.Location{Country: "美国"}}
	if got := majorityProvince([]ipdb.SourceLocation{foreign}); got != "" {
		t.Errorf("境外应为空, got %q", got)
	}
}

// 省份的 IP 段按多数票算一次要扫遍全部源的全部网段（四个源约 125 万段），1 核机器上约 0.4 秒。
// 自动出差放行每分钟对每个入站都要算、并发判定每 30 秒、生成配置时每个地区入站各算一次，
// 不缓存的话生产机每分钟有 5~6 秒满核（2026-11-11 栈采样定位）。IP 库没换就不能重算。
func TestAllowedSetForIsCachedUntilIPDBChanges(t *testing.T) {
	setupDB(t)
	resetProvinceCIDRCache(t)
	t.Cleanup(func() { resetProvinceCIDRCache(t) })
	s := ipdbServiceWithSources(t, buildLocTestDB(t, "江苏省", "南京市", "电信"), buildLocTestDB(t, "江苏省", "南京市", "电信"))

	set1, err := allowedSetFor([]string{"江苏省"})
	if err != nil || set1 == nil || !set1.Contains("1.0.0.1") {
		t.Fatalf("两个源都说是江苏，应当放行 1.0.0.1，实际 set=%v err=%v", set1, err)
	}
	if set2, _ := allowedSetFor([]string{"江苏省"}); set2 != set1 {
		t.Error("IP 库没换，第二次取同一个省的允许集却重算了一遍")
	}

	// 顺序不同的同一组省份是同一个允许集；调用方传进来的切片不能被改动。
	order := []string{"浙江省", "江苏省"}
	a, _ := allowedSetFor(order)
	b, _ := allowedSetFor([]string{"江苏省", "浙江省"})
	if a != b {
		t.Error("省份顺序不同就重算了一遍")
	}
	if order[0] != "浙江省" || order[1] != "江苏省" {
		t.Errorf("缓存键排序改动了调用方的切片：%v", order)
	}

	s.setDB("alt", buildLocTestDB(t, "浙江省", "杭州市", "电信"))
	set3, _ := allowedSetFor([]string{"江苏省"})
	if set3 == set1 {
		t.Fatal("换了一个源的库之后还在用旧的允许集")
	}
	// 并集口径：一个源说江苏就放行，换库后照样包含；按新库重算出来的应与不经缓存的结果一致。
	if !set3.Contains("1.0.0.1") {
		t.Error("并集口径下仍有一个源说是江苏，应当放行")
	}
}

// 缓存键必须带合并口径：同一个库，切换口径后要按新口径算，切回来也不能拿到过期的那一份。
func TestRegionCIDRSourceCacheFollowsMatchMode(t *testing.T) {
	setupDB(t)
	resetProvinceCIDRCache(t)
	t.Cleanup(func() { resetProvinceCIDRCache(t) })
	// 一个源说江苏、一个源说浙江：并集放行，多数判据 1:1 不放行。
	ipdbServiceWithSources(t, buildLocTestDB(t, "江苏省", "南京市", "电信"), buildLocTestDB(t, "浙江省", "杭州市", "电信"))
	settings := &SettingService{}

	union, _ := allowedSetFor([]string{"江苏省"})
	if !union.Contains("1.0.0.1") {
		t.Fatal("并集口径下应当放行")
	}
	if err := settings.setInt("regionMatchMode", RegionMatchMajority); err != nil {
		t.Fatal(err)
	}
	if majority, _ := allowedSetFor([]string{"江苏省"}); majority.Contains("1.0.0.1") {
		t.Error("切到多数判据后还在用并集口径的缓存")
	}
	if cidrs := regionCIDRSource((&IPDBService{}).DB(), settings).CIDRsOfProvinces([]string{"江苏省"}); len(cidrs) != 0 {
		t.Errorf("多数判据 1:1 不该有任何段，实际 %v", cidrs)
	}
	if err := settings.setInt("regionMatchMode", RegionMatchUnion); err != nil {
		t.Fatal(err)
	}
	if back, _ := allowedSetFor([]string{"江苏省"}); !back.Contains("1.0.0.1") {
		t.Error("切回并集后拿到了多数判据的结果")
	}
}

// 经缓存的结果必须与直接计算逐字节相同（它会写进 geo dat，内容哈希不能变），
// 且仍然实现 provinceLister（buildGeoPlanWith 靠它察觉多省允许集缺了省）。
func TestCachedRegionCIDRSourceMatchesUncached(t *testing.T) {
	setupDB(t)
	resetProvinceCIDRCache(t)
	t.Cleanup(func() { resetProvinceCIDRCache(t) })
	ipdbServiceWithSources(t, buildLocTestDB(t, "江苏省", "南京市", "电信"), buildLocTestDB(t, "江苏省", "无锡市", "联通"))
	db := (&IPDBService{}).DB()
	settings := &SettingService{}

	for _, mode := range []int{RegionMatchUnion, RegionMatchMajority} {
		if err := settings.setInt("regionMatchMode", mode); err != nil {
			t.Fatal(err)
		}
		src := regionCIDRSource(db, settings)
		var want []string
		if mode == RegionMatchMajority {
			want = db.MajorityCIDRsOfProvinces([]string{"江苏省"})
		} else {
			want = db.CIDRsOfProvinces([]string{"江苏省"})
		}
		for round := 0; round < 2; round++ { // 第二轮走缓存
			got := src.CIDRsOfProvinces([]string{"江苏省"})
			if strings.Join(got, ",") != strings.Join(want, ",") || len(want) == 0 {
				t.Errorf("口径 %d 第 %d 轮：缓存结果 %v，直接计算 %v", mode, round, got, want)
			}
		}
		if _, ok := src.(provinceLister); !ok {
			t.Errorf("口径 %d：缓存包装丢了 provinceLister，多省允许集缺省份将不再被察觉", mode)
		}
	}
}
