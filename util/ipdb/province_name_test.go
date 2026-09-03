package ipdb

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestCanonicalProvinceAcceptsShortAndFullNames(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"江苏", "江苏省"},
		{"江苏省", "江苏省"},
		{"北京", "北京市"},
		{"北京市", "北京市"},
		{"内蒙古", "内蒙古"},
		{"内蒙古自治区", "内蒙古"},
		{"广西", "广西"},
		{"广西壮族自治区", "广西"},
		{"香港", "香港特别行政区"},
		{"香港特别行政区", "香港特别行政区"},
		{" 浙江 ", "浙江省"},
	}
	for _, c := range cases {
		got, ok := CanonicalProvince(c.in)
		if !ok || got != c.want {
			t.Errorf("CanonicalProvince(%q) = %q,%v，期望 %q,true", c.in, got, ok, c.want)
		}
	}
}

func TestCanonicalProvinceRejectsNonProvinces(t *testing.T) {
	// 纯真库里云厂商 anycast 段的第二段就不是省份，误当成省份会污染整个
	// 允许集——生成的 CIDR 会把一批本不该放行的段带进来。
	for _, s := range []string{"", "腾讯云", "阿里云", "中国", "美国", "CZ88.NET", "省"} {
		if got, ok := CanonicalProvince(s); ok {
			t.Errorf("CanonicalProvince(%q) = %q,true，它不是省级地区", s, got)
		}
	}
}

// 标准名单必须覆盖真实 ip2region 库里出现的全部省份。漏一个的话，
// 纯真库那一路的同名省份会归一失败，多源合并静默少一块数据。
func TestCanonicalListCoversRealDatabase(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("拿不到源码路径")
	}
	datPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "bin", "ipdb.dat")
	db, err := Load(datPath)
	if err != nil {
		t.Skipf("真实库不可用: %v", err)
	}
	for _, p := range db.Provinces() {
		if got, ok := CanonicalProvince(p); !ok || got != p {
			t.Errorf("真实库里的省份 %q 不在标准名单里（归一结果 %q,%v）", p, got, ok)
		}
	}
}
