package geodat

import (
	"bytes"
	"testing"
)

// v2ray GeoIP 的 dat 是 protobuf：
//
//	CIDR      { bytes ip = 1; uint32 prefix = 2; }
//	GeoIP     { string country_code = 1; repeated CIDR cidr = 2; }
//	GeoIPList { repeated GeoIP entry = 1; }
//
// 手写编码只用到两种 wire type：2（长度前缀）和 0（varint）。

func TestEncodeSingleIPv4CIDR(t *testing.T) {
	got, err := Encode([]Entry{{Tag: "CN", CIDRs: []string{"1.2.3.0/24"}}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []byte{
		0x0a, 0x0e, // GeoIPList.entry, 长度 14
		0x0a, 0x02, 'C', 'N', // GeoIP.country_code = "CN"
		0x12, 0x08, // GeoIP.cidr, 长度 8
		0x0a, 0x04, 1, 2, 3, 0, // CIDR.ip = 1.2.3.0
		0x10, 0x18, // CIDR.prefix = 24
	}
	if !bytes.Equal(got, want) {
		t.Errorf("编码结果 = % x\n期望         = % x", got, want)
	}
}

func TestEncodeUppercasesTag(t *testing.T) {
	// dat 里的 tag 查找大小写不敏感。统一大写生成，避免同一份数据出现
	// 两种写法。
	got, err := Encode([]Entry{{Tag: "allow01", CIDRs: []string{"10.0.0.0/8"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("ALLOW01")) {
		t.Errorf("编码结果里没有大写的 tag: % x", got)
	}
	if bytes.Contains(got, []byte("allow01")) {
		t.Error("编码结果里仍有小写 tag")
	}
}

func TestEncodeIPv6CIDR(t *testing.T) {
	got, err := Encode([]Entry{{Tag: "V6", CIDRs: []string{"2408::/16"}}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// IPv6 的 ip 字段是 16 字节。
	if !bytes.Contains(got, []byte{0x0a, 0x10, 0x24, 0x08}) {
		t.Errorf("没有找到 16 字节的 IPv6 地址字段: % x", got)
	}
}

func TestEncodeMultipleEntriesAndCIDRs(t *testing.T) {
	got, err := Encode([]Entry{
		{Tag: "A", CIDRs: []string{"1.0.0.0/8", "2.0.0.0/8"}},
		{Tag: "B", CIDRs: []string{"3.0.0.0/8"}},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if bytes.Count(got, []byte{0x0a, 0x01, 'A'}) != 1 || bytes.Count(got, []byte{0x0a, 0x01, 'B'}) != 1 {
		t.Errorf("两个 entry 没有都编进去: % x", got)
	}
	if n := bytes.Count(got, []byte{0x10, 0x08}); n != 3 {
		t.Errorf("prefix=8 出现 %d 次，期望 3 次（共 3 条 CIDR）", n)
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	entries := []Entry{
		{Tag: "A", CIDRs: []string{"1.0.0.0/8", "2.0.0.0/8"}},
		{Tag: "B", CIDRs: []string{"3.0.0.0/8"}},
	}
	first, err := Encode(entries)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Encode(entries)
		if err != nil {
			t.Fatal(err)
		}
		// 生成必须逐字节确定：dat 变了 ruleTag 里的哈希就变，配置字节随之变化，
		// 那个 10 秒 cron 会重启 xray。不确定的话它会永远在重启。
		if !bytes.Equal(first, again) {
			t.Fatalf("第 %d 次结果与首次不同", i)
		}
	}
}

func TestEncodeRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		entries []Entry
	}{
		{"tag 为空", []Entry{{Tag: "", CIDRs: []string{"1.0.0.0/8"}}}},
		{"CIDR 为空列表", []Entry{{Tag: "A"}}},
		{"CIDR 格式非法", []Entry{{Tag: "A", CIDRs: []string{"不是网段"}}}},
		{"缺少掩码", []Entry{{Tag: "A", CIDRs: []string{"1.2.3.4"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Encode(c.entries); err == nil {
				t.Error("期望报错，实际通过")
			}
		})
	}
}

func TestEncodeRejectsDuplicateTags(t *testing.T) {
	// 同一个 tag 出现两次时 xray 的行为未经验证，与其赌不如挡住：
	// 生成端按省份集合去重，本来就不该出现重复。
	_, err := Encode([]Entry{
		{Tag: "A", CIDRs: []string{"1.0.0.0/8"}},
		{Tag: "A", CIDRs: []string{"2.0.0.0/8"}},
	})
	if err == nil {
		t.Error("重复的 tag 应当被拒绝")
	}
}

func TestVarintEncoding(t *testing.T) {
	cases := []struct {
		in   uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{24, []byte{0x18}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
	}
	for _, c := range cases {
		if got := appendVarint(nil, c.in); !bytes.Equal(got, c.want) {
			t.Errorf("appendVarint(%d) = % x，期望 % x", c.in, got, c.want)
		}
	}
}
