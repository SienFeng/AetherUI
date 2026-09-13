package dbip

import (
	"bytes"
	"compress/gzip"
	"testing"

	"a-ui/util/ipdb"
)

func gzOf(t *testing.T, csv string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(csv)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestParseTranslatesChineseSegments(t *testing.T) {
	// 取自真实数据的前几行，字段顺序：起止 IP、洲、国家码、省、市、经纬度。
	const csv = `1.0.0.0,1.0.0.255,OC,AU,Queensland,"South Brisbane",-27.4767,153.017
1.0.1.0,1.0.3.255,AS,CN,Fujian,Wenquan,26.0998,119.297
1.0.8.0,1.0.15.255,AS,CN,Guangdong,Guangzhou,23.1317,113.266
`
	got, err := Parse(gzOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("解析出 %d 段，想要 3 段", len(got))
	}
	// DB-IP 没有 country_name，国家名只能靠国家码查表。
	if got[0].Country != "Australia" {
		t.Errorf("境外段国家 = %q；想要 Australia（由国家码查出）", got[0].Country)
	}
	// Wenquan（温泉）是街道级地名，翻不出地级市，城市必须留空而不是原样存入。
	if got[1].Region != "福建省" || got[1].City != "" {
		t.Errorf("= %+v；想要 福建省 + 空城市（街道级地名留空）", got[1])
	}
	if got[2].Region != "广东省" || got[2].City != "广州市" {
		t.Errorf("= %+v；想要 广东省/广州市", got[2])
	}
}

// DB-IP 把 IPv4 与 IPv6 混在同一个文件里。本项目的 Record 用 uint32 存 IP，
// 只收 IPv4；IPv6 行是上游的正常内容，跳过而不是报错。
func TestParseSkipsIPv6Rows(t *testing.T) {
	const csv = `1.0.1.0,1.0.3.255,AS,CN,Fujian,Fuzhou,26.0998,119.297
2001:250::,2001:250:ffff:ffff:ffff:ffff:ffff:ffff,AS,CN,Beijing,Beijing,39.9,116.4
1.0.8.0,1.0.15.255,AS,CN,Guangdong,Guangzhou,23.1317,113.266
`
	got, err := Parse(gzOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("解析出 %d 段，想要 2 段（IPv6 行应被跳过）", len(got))
	}
	// 跳过 IPv6 之后，剩下的 IPv4 段之间相对顺序不变，仍满足 BuildRecords 的升序要求。
	if got[0].Start >= got[1].Start {
		t.Error("跳过 IPv6 后剩余段必须仍然升序")
	}
}

func TestParseFoldsGreaterChina(t *testing.T) {
	const csv = `1.0.0.0,1.0.0.255,AS,HK,Central and Western,Hong Kong,22.28,114.15
1.0.1.0,1.0.1.255,AS,TW,Taiwan,Taipei,25.03,121.56
1.0.2.0,1.0.2.255,AS,MO,Macao,Macao,22.2,113.54
`
	got, err := Parse(gzOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"香港特别行政区", "台湾省", "澳门特别行政区"}
	for i, w := range want {
		if got[i].Country != ipdb.ChinaCountry || got[i].Region != w {
			t.Errorf("第 %d 段 = %q/%q；想要 %q/%q",
				i, got[i].Country, got[i].Region, ipdb.ChinaCountry, w)
		}
	}
}

func TestParseMergesAdjacentIdenticalSegments(t *testing.T) {
	const csv = `1.0.0.0,1.0.0.255,AS,CN,Fujian,Fuzhou,26.0,119.0
1.0.1.0,1.0.1.255,AS,CN,Fujian,Fuzhou,26.0,119.0
1.0.2.0,1.0.2.255,AS,CN,Fujian,Xiamen,24.4,118.0
`
	got, err := Parse(gzOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("解析出 %d 段，想要 2 段（前两段应合并）", len(got))
	}
	if got[0].End != 1<<24|1<<8|255 {
		t.Errorf("合并后第一段结束 = %d；想要 1.0.1.255", got[0].End)
	}
}

// 国家码不认识时整条丢弃，不留一条空国家的记录：那种记录既不算中国也没有国家，
// 界面上是一片空白，也无从判断是查不到还是没收录。
func TestParseDropsUnknownCountry(t *testing.T) {
	const csv = `0.0.0.0,0.255.255.255,ZZ,ZZ,,,0,0
1.0.1.0,1.0.3.255,AS,CN,Fujian,Fuzhou,26.0998,119.297
`
	got, err := Parse(gzOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("解析出 %d 段，想要 1 段（ZZ 保留段应被丢弃）", len(got))
	}
	if got[0].Country != ipdb.ChinaCountry {
		t.Errorf("剩下的那段 = %+v；想要中国段", got[0])
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"起止颠倒":  "1.0.3.255,1.0.1.0,AS,CN,Fujian,Fuzhou,26.0,119.0\n",
		"字段不足":  "1.0.1.0,1.0.3.255,AS\n",
		"IP 越界": "1.0.1.300,1.0.3.255,AS,CN,Fujian,Fuzhou,26.0,119.0\n",
		"IP 残缺": "1.0.1,1.0.3.255,AS,CN,Fujian,Fuzhou,26.0,119.0\n",
	}
	for name, csv := range cases {
		if _, err := Parse(gzOf(t, csv)); err == nil {
			t.Errorf("%s：应当报错而不是跳过", name)
		}
	}
	if _, err := Parse(bytes.NewReader([]byte("not gzip"))); err == nil {
		t.Error("非 gzip 输入应当报错")
	}
}

func TestParseIPv4(t *testing.T) {
	cases := map[string]uint32{
		"0.0.0.0":         0,
		"1.0.1.0":         1<<24 | 1<<8,
		"255.255.255.255": ^uint32(0),
	}
	for in, want := range cases {
		if got, ok := parseIPv4(in); !ok || got != want {
			t.Errorf("parseIPv4(%q) = %d, %v；想要 %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "1.2.3", "1.2.3.4.5", "1.2.3.256", "1.2.3.a", "1..2.3", "1.2.3."} {
		if _, ok := parseIPv4(in); ok {
			t.Errorf("parseIPv4(%q) 应当失败", in)
		}
	}
}
