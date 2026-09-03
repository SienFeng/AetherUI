package qqwry

import (
	"encoding/binary"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 纯真库的线格式：
//
//	头部 8 字节：索引区起始偏移、索引区结束偏移（均为小端 uint32）
//	索引项 7 字节：起始 IP（小端 uint32）+ 记录偏移（小端 uint24）
//	记录：结束 IP（小端 uint32）+ 国家字符串 + 地区字符串，两者都可能被
//	     0x01 / 0x02 重定向到别处，字符串是 GBK 且以 \0 结尾
//
// 下面这个 builder 按同样的格式拼字节，好在开发机上驱动解析器。

type builder struct {
	body    []byte // 记录区
	entries []entry
}

type entry struct {
	startIP uint32
	offset  uint32
}

func gbk(t *testing.T, s string) []byte {
	t.Helper()
	out, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("GBK 编码 %q: %v", s, err)
	}
	return append(out, 0)
}

// putString 把字符串放进记录区，返回它的绝对偏移。
func (b *builder) putString(t *testing.T, s string) uint32 {
	off := uint32(len(b.body)) + 8
	b.body = append(b.body, gbk(t, s)...)
	return off
}

func u24(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16)} }

// addPlain 加一条国家、地区都内联的记录。
func (b *builder) addPlain(t *testing.T, startIP, endIP uint32, country, area string) {
	off := uint32(len(b.body)) + 8
	var rec []byte
	rec = binary.LittleEndian.AppendUint32(rec, endIP)
	rec = append(rec, gbk(t, country)...)
	rec = append(rec, gbk(t, area)...)
	b.body = append(b.body, rec...)
	b.entries = append(b.entries, entry{startIP: startIP, offset: off})
}

// addCountryRedirect 加一条国家被 0x02 重定向、地区内联的记录。
func (b *builder) addCountryRedirect(t *testing.T, startIP, endIP uint32, countryAt uint32, area string) {
	off := uint32(len(b.body)) + 8
	var rec []byte
	rec = binary.LittleEndian.AppendUint32(rec, endIP)
	rec = append(rec, 0x02)
	rec = append(rec, u24(countryAt)...)
	rec = append(rec, gbk(t, area)...)
	b.body = append(b.body, rec...)
	b.entries = append(b.entries, entry{startIP: startIP, offset: off})
}

// addWholeRedirect 加一条整条记录被 0x01 重定向到另一条记录的项。
func (b *builder) addWholeRedirect(startIP, endIP uint32, target uint32) {
	off := uint32(len(b.body)) + 8
	var rec []byte
	rec = binary.LittleEndian.AppendUint32(rec, endIP)
	rec = append(rec, 0x01)
	rec = append(rec, u24(target)...)
	b.body = append(b.body, rec...)
	b.entries = append(b.entries, entry{startIP: startIP, offset: off})
}

func (b *builder) build() []byte {
	indexStart := uint32(len(b.body)) + 8
	var idx []byte
	for _, e := range b.entries {
		idx = binary.LittleEndian.AppendUint32(idx, e.startIP)
		idx = append(idx, u24(e.offset)...)
	}
	indexEnd := indexStart + uint32(len(idx)) - 7

	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out[0:], indexStart)
	binary.LittleEndian.PutUint32(out[4:], indexEnd)
	out = append(out, b.body...)
	return append(out, idx...)
}

func ip(a, b, c, d byte) uint32 {
	return uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8 | uint32(d)
}

func TestParseReadsPlainRecords(t *testing.T) {
	b := &builder{}
	b.addPlain(t, ip(1, 0, 0, 0), ip(1, 0, 0, 255), "中国–江苏–南京", "电信")
	b.addPlain(t, ip(2, 0, 0, 0), ip(2, 255, 255, 255), "美国", "CZ88.NET")

	recs, err := Parse(b.build())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(recs))
	}
	if recs[0].Start != ip(1, 0, 0, 0) || recs[0].End != ip(1, 0, 0, 255) {
		t.Errorf("第一条区间 = %d~%d", recs[0].Start, recs[0].End)
	}
	if recs[0].Country != "中国" || recs[0].Region != "江苏省" || recs[0].City != "南京" {
		t.Errorf("第一条归属地 = %q/%q/%q，期望 中国/江苏省/南京",
			recs[0].Country, recs[0].Region, recs[0].City)
	}
	// 境外只保留国家，与 ip2region 那一路的口径一致。
	if recs[1].Country != "美国" || recs[1].Region != "" || recs[1].City != "" {
		t.Errorf("第二条归属地 = %q/%q/%q，期望只有国家", recs[1].Country, recs[1].Region, recs[1].City)
	}
}

func TestParseFollowsCountryRedirect(t *testing.T) {
	b := &builder{}
	at := b.putString(t, "中国–浙江–杭州")
	b.addCountryRedirect(t, ip(3, 0, 0, 0), ip(3, 0, 0, 255), at, "阿里云")

	recs, err := Parse(b.build())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("记录数 = %d，期望 1", len(recs))
	}
	if recs[0].Region != "浙江省" || recs[0].City != "杭州" {
		t.Errorf("归属地 = %q/%q，0x02 重定向没跟到", recs[0].Region, recs[0].City)
	}
}

func TestParseFollowsWholeRecordRedirect(t *testing.T) {
	b := &builder{}
	b.addPlain(t, ip(4, 0, 0, 0), ip(4, 0, 0, 255), "中国–四川–成都", "电信")
	// 0x01 重定向的目标是「国家位置」，即记录偏移 + 4（跳过 4 字节结束 IP）
	target := uint32(8 + 4)
	b.addWholeRedirect(ip(5, 0, 0, 0), ip(5, 0, 0, 255), target)

	recs, err := Parse(b.build())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(recs))
	}
	// 0x01 是整条记录的重定向。不处理它的话会解出乱码——真实库里
	// 这种记录很多。
	if recs[1].Region != "四川省" || recs[1].City != "成都" {
		t.Errorf("第二条归属地 = %q/%q，0x01 重定向没跟到", recs[1].Region, recs[1].City)
	}
}

func TestParseNormalizesProvinceNames(t *testing.T) {
	cases := []struct {
		raw    string
		region string
		city   string
	}{
		{"中国–江苏–南京", "江苏省", "南京"},
		{"中国–北京–北京", "北京市", "北京"},
		{"中国–内蒙古–呼和浩特", "内蒙古", "呼和浩特"},
		{"中国–香港", "香港特别行政区", ""},
		{"中国–新疆–乌鲁木齐", "新疆", "乌鲁木齐"},
		// 只有国家、没有省份：真实库里云厂商 anycast 段就是这样。
		{"中国", "", ""},
		// 不是省份的第二段（云厂商）不能被当成省份塞进去。
		{"腾讯云", "", ""},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			b := &builder{}
			b.addPlain(t, ip(1, 0, 0, 0), ip(1, 0, 0, 255), c.raw, "x")
			recs, err := Parse(b.build())
			if err != nil {
				t.Fatal(err)
			}
			if recs[0].Region != c.region || recs[0].City != c.city {
				t.Errorf("解析 %q 得到 %q/%q，期望 %q/%q",
					c.raw, recs[0].Region, recs[0].City, c.region, c.city)
			}
		})
	}
}

func TestParseRejectsCorruptData(t *testing.T) {
	cases := map[string][]byte{
		"太短":      {1, 2, 3},
		"索引区越界":   {0xff, 0xff, 0xff, 0x7f, 0xff, 0xff, 0xff, 0x7f},
		"索引区起止倒置": append(binary.LittleEndian.AppendUint32(nil, 100), binary.LittleEndian.AppendUint32(nil, 50)...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(data); err == nil {
				t.Error("期望报错，实际通过")
			}
		})
	}
}

func TestParseSkipsRedirectLoop(t *testing.T) {
	// 数据损坏时重定向可能成环。不设深度上限会直接栈溢出，
	// 而这段代码跑在定时更新任务里，panic 会杀掉整个面板进程。
	b := &builder{}
	off := uint32(len(b.body)) + 8 + 4 // 自己的国家位置
	b.addWholeRedirect(ip(1, 0, 0, 0), ip(1, 0, 0, 255), off)
	if _, err := Parse(b.build()); err == nil {
		t.Error("自指的重定向应当被判为数据损坏")
	}
}

// resolve 的 0x01 分支必须同时把国家和地区都从重定向目标读出来。
//
// 单靠 Parse 的输出测不出这一点：ipdb.Record 不保留 ISP 字段，area 被丢掉了，
// 而 readString 自己也会跟 0x01，于是删掉 resolve 里的 0x01 分支后国家依然正确。
// 只有直接看 area 才能分辨——将来若要展示运营商，读错的就是这个值。
func TestResolveFollowsWholeRedirectForBothFields(t *testing.T) {
	b := &builder{}
	b.addPlain(t, ip(4, 0, 0, 0), ip(4, 0, 0, 255), "中国–四川–成都", "电信")
	b.addWholeRedirect(ip(5, 0, 0, 0), ip(5, 0, 0, 255), 8+4)
	data := b.build()

	// 第二条索引项指向的记录偏移
	secondRecOff := uint32(8 + 4 + len(gbk(t, "中国–四川–成都")) + len(gbk(t, "电信")))
	country, area, err := resolve(data, secondRecOff+4, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if country != "中国–四川–成都" {
		t.Errorf("country = %q，0x01 重定向没跟到", country)
	}
	if area != "电信" {
		t.Errorf("area = %q，期望 电信——0x01 的地区也必须从重定向目标读", area)
	}
}
