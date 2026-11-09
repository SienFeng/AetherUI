package service

import (
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
