package ipdb

import (
	"bytes"
	"encoding/binary"
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

// 相邻且归属地完全相同（含运营商）的段必须合并——上游把同一个城市按接入点
// 拆得很碎。
//
// 这个用例原先取的是 sampleSource 里「同城不同 ISP」的那两行：那时本包不存
// ISP，两段确实等价。运营商进了归属地之后它们不再等价（那是
// TestBuildRecordsDoesNotMergeAdjacentSegmentsWithDifferentISP 断言的行为），
// 所以这里换成一份真正完全相同的输入，验证的不变量没有变。
func TestBuildMergesAdjacentSegmentsWithSameLocation(t *testing.T) {
	const src = `1.0.0.0|1.0.0.255|中国|江苏省|南京市|中国电信|CN
1.0.1.0|1.0.1.255|中国|江苏省|南京市|中国电信|CN
1.0.2.0|1.0.2.255|中国|江苏省|苏州市|中国电信|CN
`
	db := buildSample(t, src)

	if got := db.SegmentCount(); got != 2 {
		t.Errorf("SegmentCount = %d, want 2（3 行输入中前 2 行应合并）", got)
	}
	loc, _ := db.Lookup(net.ParseIP("1.0.1.128"))
	if loc.City != "南京市" {
		t.Errorf("合并后 1.0.1.128 的城市 = %q, want 南京市", loc.City)
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

// sampleSource 的第 6 字段就是 ISP，此前被丢掉了。
func TestBuildReadsISPFromIP2RegionSource(t *testing.T) {
	db := buildSample(t, sampleSource)
	for _, c := range []struct{ ip, isp string }{
		{"1.0.2.5", "中国电信"},
		{"2.0.1.5", "中国联通"},
		{"3.1.2.3", ""}, // 源里是 "0"，境外且非云厂商
	} {
		loc, ok := db.Lookup(net.ParseIP(c.ip))
		if !ok {
			t.Errorf("Lookup(%s) 未命中", c.ip)
			continue
		}
		if loc.ISP != c.isp {
			t.Errorf("Lookup(%s).ISP = %q, want %q", c.ip, loc.ISP, c.isp)
		}
	}
}

// 加 ISP 之后段变细了，但 CIDRsOfProvinces 的输出必须逐字节不变：
// 它的结果进 geo dat 的内容哈希，一变就会让那个 10 秒的 cron 反复重启 xray。
//
// 之所以能不变：CIDRsOfProvinces 按 Region 过滤后自己重新合并连续区间
// （hasRange && end+1 == s.start），不依赖 segments 的合并粒度。
func TestCIDRsOfProvincesUnaffectedByISPSplit(t *testing.T) {
	// 同一批数据的两种口径：一份不带 ISP（相邻同城段会合并），
	// 一份带 ISP（同城不同 ISP 拆成两段）。
	base := []Record{
		{Start: 0x02000000, End: 0x020000FF, Country: "中国", Region: "江苏省", City: "南京市"},
		{Start: 0x02000100, End: 0x020001FF, Country: "中国", Region: "江苏省", City: "南京市"},
		{Start: 0x02000200, End: 0x020002FF, Country: "中国", Region: "江苏省", City: "苏州市"},
	}
	withISP := make([]Record, len(base))
	copy(withISP, base)
	withISP[0].ISP = "中国电信"
	withISP[1].ISP = "中国联通"
	withISP[2].ISP = "中国电信"

	build := func(recs []Record) *DB {
		t.Helper()
		var buf bytes.Buffer
		if err := BuildRecords(recs, &buf, testBuiltAt); err != nil {
			t.Fatalf("BuildRecords: %v", err)
		}
		db, err := Parse(buf.Bytes())
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return db
	}

	plain, split := build(base), build(withISP)
	if plain.SegmentCount() == split.SegmentCount() {
		t.Fatalf("两种口径的段数相同（都是 %d），用例没有覆盖到分裂场景",
			plain.SegmentCount())
	}
	want := plain.CIDRsOfProvinces([]string{"江苏省"})
	got := split.CIDRsOfProvinces([]string{"江苏省"})
	if len(want) != len(got) {
		t.Fatalf("CIDR 条数变了：不带 ISP %v，带 ISP %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("第 %d 条 CIDR 变了: %q -> %q（geo dat 的内容哈希会跟着变，"+
				"那个 10 秒的 cron 会反复重启 xray）", i, want[i], got[i])
		}
	}
}

// v2 写入的 ISP 必须能原样读回来。
func TestBuildRecordsRoundTripsISP(t *testing.T) {
	var buf bytes.Buffer
	recs := []Record{
		{Start: 0x01000000, End: 0x0100FFFF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国电信"},
		{Start: 0x01010000, End: 0x0101FFFF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国联通"},
		{Start: 0x03000000, End: 0x03FFFFFF, Country: "United States", ISP: "Google"},
	}
	if err := BuildRecords(recs, &buf, testBuiltAt); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !db.HasISP() {
		t.Error("HasISP() = false, 新生成的库应当是 v2")
	}
	for _, c := range []struct{ ip, isp string }{
		{"1.0.0.1", "中国电信"},
		{"1.1.0.1", "中国联通"},
		{"3.1.1.1", "Google"},
	} {
		loc, ok := db.Lookup(net.ParseIP(c.ip))
		if !ok {
			t.Errorf("Lookup(%s) 未命中", c.ip)
			continue
		}
		if loc.ISP != c.isp {
			t.Errorf("Lookup(%s).ISP = %q, want %q", c.ip, loc.ISP, c.isp)
		}
	}
}

// 同城不同 ISP 的相邻段不能再被合并掉，否则 ISP 信息会丢。
func TestBuildRecordsDoesNotMergeAdjacentSegmentsWithDifferentISP(t *testing.T) {
	var buf bytes.Buffer
	recs := []Record{
		{Start: 0x02000000, End: 0x020000FF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国电信"},
		{Start: 0x02000100, End: 0x020001FF, Country: "中国", Region: "江苏省", City: "南京市", ISP: "中国联通"},
	}
	if err := BuildRecords(recs, &buf, testBuiltAt); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if db.SegmentCount() != 2 {
		t.Errorf("SegmentCount = %d, want 2（同城不同 ISP 不能合并）", db.SegmentCount())
	}
}

// 升级路径的关键用例：机器上已有的 v1 库必须还能读，ISP 留空。
// 拒绝加载会让整个归属地列当场消失——不只是新的运营商列，
// 连现在能用的省市判定和共享检测的省份判定也一起失效。
func TestParseAcceptsLegacyV1File(t *testing.T) {
	data := buildLegacyV1(t)
	db, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse v1: %v", err)
	}
	if db.HasISP() {
		t.Error("HasISP() = true, v1 库不含 ISP")
	}
	loc, ok := db.Lookup(net.ParseIP("1.0.0.1"))
	if !ok {
		t.Fatal("Lookup 未命中")
	}
	if loc.Country != "中国" || loc.Region != "江苏省" || loc.City != "南京市" {
		t.Errorf("got %+v, want 中国/江苏省/南京市", loc)
	}
	if loc.ISP != "" {
		t.Errorf("ISP = %q, v1 库读出来必须是空串", loc.ISP)
	}
}

// buildLegacyV1 手工拼一份 v1 格式的库：1 个段、1 个归属地。
// 直接写字节而不是留一个 v1 的构建函数在生产代码里——v1 只需要能读，
// 不需要能写。
//
// v1 线格式（全部小端）：
//
//	头部 32 字节：magic(8) + version(4) + builtAt(8) + segCount(4) + locCount(2) + 填充
//	段 10 字节：start(4) + end(4) + locIndex(2)
//	归属地 6 字节：country(2) + region(2) + city(2)，都是字符串池下标
//	字符串池：count(4) + 每项 len(2) + 字节
func buildLegacyV1(t *testing.T) []byte {
	t.Helper()
	pool := []string{"", "中国", "江苏省", "南京市"}

	buf := make([]byte, headerSize)
	copy(buf, magic)
	binary.LittleEndian.PutUint32(buf[8:], 1)
	binary.LittleEndian.PutUint64(buf[12:], uint64(testBuiltAt.Unix()))
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[24:], 1)

	seg := make([]byte, 10)
	binary.LittleEndian.PutUint32(seg[0:], 0x01000000)
	binary.LittleEndian.PutUint32(seg[4:], 0x0100FFFF)
	binary.LittleEndian.PutUint16(seg[8:], 0)
	buf = append(buf, seg...)

	loc := make([]byte, 6)
	binary.LittleEndian.PutUint16(loc[0:], 1)
	binary.LittleEndian.PutUint16(loc[2:], 2)
	binary.LittleEndian.PutUint16(loc[4:], 3)
	buf = append(buf, loc...)

	var n4 [4]byte
	binary.LittleEndian.PutUint32(n4[:], uint32(len(pool)))
	buf = append(buf, n4[:]...)
	for _, s := range pool {
		var n2 [2]byte
		binary.LittleEndian.PutUint16(n2[:], uint16(len(s)))
		buf = append(buf, n2[:]...)
		buf = append(buf, s...)
	}
	return buf
}
