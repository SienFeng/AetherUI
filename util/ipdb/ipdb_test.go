package ipdb

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

// 取自上游 ipv4_source.txt 的真实片段，格式为
// startIP|endIP|国家|省份|城市|ISP|国家码
const sampleSource = `0.0.0.0|0.255.255.255|Reserved|Reserved|Reserved|0|0
1.0.0.0|1.0.0.255|Australia|Queensland|0|0|AU
1.0.1.0|1.0.3.255|中国|福建省|福州市|中国电信|CN
1.0.4.0|1.0.7.255|Australia|Victoria|Melbourne|0|AU
2.0.0.0|2.0.0.255|中国|江苏省|南京市|中国电信|CN
2.0.1.0|2.0.1.255|中国|江苏省|南京市|中国联通|CN
2.0.2.0|2.0.2.255|中国|江苏省|苏州市|中国电信|CN
3.0.0.0|3.255.255.255|United States|California|San Jose|0|US
`

var testBuiltAt = time.Unix(1788000000, 0)

func buildSample(t *testing.T, src string) *DB {
	t.Helper()
	var buf bytes.Buffer
	if err := Build(strings.NewReader(src), &buf, testBuiltAt); err != nil {
		t.Fatalf("Build: %v", err)
	}
	db, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return db
}

func TestLookupReturnsProvinceAndCityForChineseIP(t *testing.T) {
	db := buildSample(t, sampleSource)

	loc, ok := db.Lookup(net.ParseIP("1.0.2.5"))
	if !ok {
		t.Fatal("Lookup 返回 not found")
	}
	if loc.Country != "中国" || loc.Region != "福建省" || loc.City != "福州市" {
		t.Errorf("got %+v, want 中国/福建省/福州市", loc)
	}
}

// 用户选定的精度是「境外只到国家，中国到省市」，境外段不该带省/市。
func TestLookupReturnsCountryOnlyForForeignIP(t *testing.T) {
	db := buildSample(t, sampleSource)

	loc, ok := db.Lookup(net.ParseIP("3.1.2.3"))
	if !ok {
		t.Fatal("Lookup 返回 not found")
	}
	if loc.Country != "United States" {
		t.Errorf("Country = %q, want United States", loc.Country)
	}
	if loc.Region != "" || loc.City != "" {
		t.Errorf("境外段不该带省市，got Region=%q City=%q", loc.Region, loc.City)
	}
}

func TestLookupAtSegmentBoundaries(t *testing.T) {
	db := buildSample(t, sampleSource)

	for _, c := range []struct{ ip, wantRegion string }{
		{"1.0.1.0", "福建省"},   // 段首
		{"1.0.3.255", "福建省"}, // 段尾
		{"1.0.4.0", ""},      // 下一段段首（澳大利亚）
		{"0.0.0.0", ""},      // 整个表的第一个 IP
		{"255.255.255.255", ""},
	} {
		loc, _ := db.Lookup(net.ParseIP(c.ip))
		if loc.Region != c.wantRegion {
			t.Errorf("Lookup(%s).Region = %q, want %q", c.ip, loc.Region, c.wantRegion)
		}
	}
}

// 相邻且归属地相同的段必须合并——上游按 ISP 拆得很碎，而我们不存 ISP。
func TestBuildMergesAdjacentSegmentsWithSameLocation(t *testing.T) {
	db := buildSample(t, sampleSource)

	// 2.0.0.0-2.0.0.255 与 2.0.1.0-2.0.1.255 同为江苏省南京市（仅 ISP 不同），应合并为一段；
	// 2.0.2.0 是苏州市，不合并。
	if got := db.SegmentCount(); got != 7 {
		t.Errorf("SegmentCount = %d, want 7（8 行输入中有 2 行应合并）", got)
	}
	loc, _ := db.Lookup(net.ParseIP("2.0.1.128"))
	if loc.City != "南京市" {
		t.Errorf("合并后 2.0.1.128 的城市 = %q, want 南京市", loc.City)
	}
}

func TestLookupRejectsIPv6(t *testing.T) {
	db := buildSample(t, sampleSource)
	if _, ok := db.Lookup(net.ParseIP("2001:db8::1")); ok {
		t.Error("IPv6 应返回 not found（本版本只收录 IPv4）")
	}
}

// 生成必须逐字节确定：否则每次更新都会在 git 里留下无意义的 diff，也无法校验。
func TestBuildIsDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := Build(strings.NewReader(sampleSource), &a, testBuiltAt); err != nil {
		t.Fatalf("Build a: %v", err)
	}
	if err := Build(strings.NewReader(sampleSource), &b, testBuiltAt); err != nil {
		t.Fatalf("Build b: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("两次生成的字节不一致")
	}
}

func TestProvincesListsChineseProvincesOnly(t *testing.T) {
	db := buildSample(t, sampleSource)

	got := db.Provinces()
	want := []string{"江苏省", "福建省"} // 按 Unicode 升序
	if len(got) != len(want) {
		t.Fatalf("Provinces() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Provinces() = %v, want %v", got, want)
		}
	}
}

// 一个省的多个城市在 IP 上相连时必须先合并成区间再转 CIDR，
// 否则会产出一堆本可合并的 /24，让 geo 数据文件白白变大。
func TestCIDRsOfProvincesMergesAcrossCitiesAndConverts(t *testing.T) {
	db := buildSample(t, sampleSource)

	// 江苏省覆盖 2.0.0.0-2.0.2.255（南京两段 + 苏州一段，三段相连）
	got := db.CIDRsOfProvinces([]string{"江苏省"})
	want := []string{"2.0.0.0/23", "2.0.2.0/24"}
	assertStrings(t, got, want)
}

func TestCIDRsOfProvincesCombinesMultipleProvinces(t *testing.T) {
	db := buildSample(t, sampleSource)

	got := db.CIDRsOfProvinces([]string{"江苏省", "福建省"})
	// 福建 1.0.1.0-1.0.3.255 在前，江苏 2.0.0.0-2.0.2.255 在后，按 IP 升序输出
	want := []string{"1.0.1.0/24", "1.0.2.0/23", "2.0.0.0/23", "2.0.2.0/24"}
	assertStrings(t, got, want)
}

// 调用方（生成 geo 数据文件那一侧）必须把空结果当成「不要生成这条规则」：
// 空的允许集配上 ! 取反等于「拒绝所有人」。
func TestCIDRsOfUnknownProvinceIsEmpty(t *testing.T) {
	db := buildSample(t, sampleSource)

	if got := db.CIDRsOfProvinces([]string{"火星省"}); len(got) != 0 {
		t.Errorf("未知省份应返回空，got %v", got)
	}
	if got := db.CIDRsOfProvinces(nil); len(got) != 0 {
		t.Errorf("空入参应返回空，got %v", got)
	}
}

func TestBuildRejectsMalformedInput(t *testing.T) {
	cases := []struct{ name, src string }{
		{"字段数不足", "1.0.0.0|1.0.0.255|中国|江苏省|南京市|电信\n"},
		{"起始 IP 非法", "not-an-ip|1.0.0.255|中国|江苏省|南京市|电信|CN\n"},
		{"起止倒置", "1.0.1.0|1.0.0.255|中国|江苏省|南京市|电信|CN\n"},
		{"段乱序", "2.0.0.0|2.0.0.255|中国|江苏省|南京市|电信|CN\n1.0.0.0|1.0.0.255|中国|福建省|福州市|电信|CN\n"},
		{"段重叠", "1.0.0.0|1.0.1.255|中国|江苏省|南京市|电信|CN\n1.0.1.0|1.0.2.255|中国|福建省|福州市|电信|CN\n"},
		{"空输入", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Build(strings.NewReader(c.src), &buf, testBuiltAt); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestParseRejectsCorruptData(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(strings.NewReader(sampleSource), &buf, testBuiltAt); err != nil {
		t.Fatalf("Build: %v", err)
	}
	good := buf.Bytes()

	t.Run("文件头标识错误", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		copy(bad, "XXXXXXXX")
		if _, err := Parse(bad); err == nil {
			t.Fatal("want error, got nil")
		}
	})
	t.Run("格式版本不支持", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[8] = 99
		if _, err := Parse(bad); err == nil {
			t.Fatal("want error, got nil")
		}
	})
	t.Run("文件被截断", func(t *testing.T) {
		for _, n := range []int{0, 10, headerSize, len(good) / 2, len(good) - 1} {
			if _, err := Parse(good[:n]); err == nil {
				t.Errorf("截断到 %d 字节仍解析成功，应报错", n)
			}
		}
	})
}

func TestBuiltAtRoundTrips(t *testing.T) {
	db := buildSample(t, sampleSource)
	if !db.BuiltAt().Equal(testBuiltAt) {
		t.Errorf("BuiltAt() = %v, want %v", db.BuiltAt(), testBuiltAt)
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
