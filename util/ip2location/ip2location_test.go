package ip2location

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"a-ui/util/ipdb"
)

// zipOf 把一份 CSV 文本打成上游那种发行包：除 CSV 外还带两个说明文件，
// 解析器必须能从中挑出 CSV。
func zipOf(t *testing.T, csv string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"README_LITE.TXT":          "readme",
		"LICENSE_LITE.TXT":         "license",
		"IP2LOCATION-LITE-DB3.CSV": csv,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestParseTranslatesChineseSegments(t *testing.T) {
	// 16777472~16778239 是真实数据里的第一个中国段（1.0.1.0~1.0.3.255）。
	const csv = `"16777216","16777471","AU","Australia","Queensland","Brisbane"
"16777472","16778239","CN","China","Fujian","Fuzhou"
"16778240","16779263","CN","China","Guangdong","Guangzhou"
`
	got, err := Parse(zipOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("解析出 %d 段，想要 3 段", len(got))
	}
	// 境外段只保留国家名，且沿用上游的英文写法——翻成中文会和现有两个源
	// 全部判成分歧。
	if got[0].Country != "Australia" || got[0].Region != "" || got[0].City != "" {
		t.Errorf("境外段 = %+v；只该保留国家名", got[0])
	}
	if got[1].Country != ipdb.ChinaCountry || got[1].Region != "福建省" || got[1].City != "福州市" {
		t.Errorf("中国段 = %+v；想要 中国/福建省/福州市", got[1])
	}
	if got[2].Region != "广东省" || got[2].City != "广州市" {
		t.Errorf("第三段 = %+v；想要 广东省/广州市", got[2])
	}
}

// 上游用 "-" 表示「无此字段」。不显式处理的话会拿 "-" 去查省表和城市表，
// 虽然同样查不到、同样留空，但那是巧合而不是设计。
func TestParseTreatsDashAsMissing(t *testing.T) {
	const csv = `"0","16777215","-","-","-","-"
"16777216","16777471","CN","China","-","-"
`
	got, err := Parse(zipOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	// 第一行国家码是 "-"，CountryFromCode 认不出，整条丢弃。
	if len(got) != 1 {
		t.Fatalf("解析出 %d 段，想要 1 段（保留段应被丢弃）", len(got))
	}
	if got[0].Country != ipdb.ChinaCountry || got[0].Region != "" || got[0].City != "" {
		t.Errorf("= %+v；省市为 \"-\" 时必须留空", got[0])
	}
}

// ip2region 把港澳台放在「中国 | 香港特别行政区」这一层，而本源用独立国家码。
// 不折叠的话这七万多个段会被当成境外段、省份整个丢掉。
func TestParseFoldsGreaterChina(t *testing.T) {
	const csv = `"16777216","16777471","HK","Hong Kong","Hong Kong","Hong Kong"
"16777472","16778239","TW","Taiwan (Province of China)","Taipei","Taipei"
"16778240","16779263","MO","Macao","Macao","Macao"
`
	got, err := Parse(zipOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"香港特别行政区", "台湾省", "澳门特别行政区"}
	for i, w := range want {
		if got[i].Country != ipdb.ChinaCountry {
			t.Errorf("第 %d 段国家 = %q；想要 %q", i, got[i].Country, ipdb.ChinaCountry)
		}
		if got[i].Region != w {
			t.Errorf("第 %d 段省份 = %q；想要 %q", i, got[i].Region, w)
		}
	}
}

// 合并必须在解析阶段做：上游近 300 万行，逐行留一个 Record 会在解析阶段就
// 占掉数百 MB，而面板常跑在小内存 VPS 上。
func TestParseMergesAdjacentIdenticalSegments(t *testing.T) {
	const csv = `"16777216","16777471","CN","China","Fujian","Fuzhou"
"16777472","16778239","CN","China","Fujian","Fuzhou"
"16778240","16779263","CN","China","Fujian","Xiamen"
`
	got, err := Parse(zipOf(t, csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("解析出 %d 段，想要 2 段（前两段归属地相同且相邻，应合并）", len(got))
	}
	if got[0].Start != 16777216 || got[0].End != 16778239 {
		t.Errorf("合并后第一段 = %d~%d；想要 16777216~16778239", got[0].Start, got[0].End)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"起止颠倒":   `"16778239","16777216","CN","China","Fujian","Fuzhou"`,
		"字段不足":   `"16777216","16777471","CN"`,
		"IP 非数字": `"abc","16777471","CN","China","Fujian","Fuzhou"`,
	}
	for name, csv := range cases {
		if _, err := Parse(zipOf(t, csv+"\n")); err == nil {
			t.Errorf("%s：应当报错而不是跳过——宁可更新失败保留旧库，也不能生成一份内容残缺的库", name)
		}
	}
	if _, err := Parse(bytes.NewReader([]byte("not a zip"))); err == nil {
		t.Error("非 ZIP 输入应当报错")
	}
	if _, err := Parse(zipOf(t, "")); err == nil ||
		!strings.Contains(err.Error(), "没有解析出任何数据段") {
		t.Errorf("空 CSV 应当报错，实际 err=%v", err)
	}
}
