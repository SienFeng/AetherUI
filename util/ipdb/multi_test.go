package ipdb

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func buildDB(t *testing.T, records []Record) *DB {
	t.Helper()
	var buf bytes.Buffer
	if err := BuildRecords(records, &buf, time.Unix(1000, 0)); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return db
}

func ipv4(a, b, c, d byte) uint32 {
	return uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8 | uint32(d)
}

func TestMultiUnionsCIDRsAcrossSources(t *testing.T) {
	a := buildDB(t, []Record{
		{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "江苏省", City: "南京"},
	})
	b := buildDB(t, []Record{
		{Start: ipv4(2, 0, 0, 0), End: ipv4(2, 0, 0, 255), Country: "中国", Region: "江苏省", City: "苏州"},
	})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	got := m.CIDRsOfProvinces([]string{"江苏省"})
	// 并集而不是交集：对用户来说「本该能连却连不上」比「本不该连却连上了」
	// 严重得多，任一数据源认为是江苏就放行。
	if len(got) != 2 {
		t.Fatalf("CIDR 数 = %d，期望 2（两个源各一段）：%v", len(got), got)
	}
	if got[0] != "1.0.0.0/24" || got[1] != "2.0.0.0/24" {
		t.Errorf("结果 = %v，期望升序的 [1.0.0.0/24 2.0.0.0/24]", got)
	}
}

func TestMultiDeduplicatesIdenticalCIDRs(t *testing.T) {
	rec := []Record{{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "江苏省"}}
	m := NewMulti([]Named{
		{Key: "a", DB: buildDB(t, rec)},
		{Key: "b", DB: buildDB(t, rec)},
	})
	got := m.CIDRsOfProvinces([]string{"江苏省"})
	// 两个源给出同一段时不能重复输出：dat 会白白变大，且重复内容会让
	// 内容哈希对「源顺序」敏感。
	if len(got) != 1 {
		t.Errorf("CIDR 数 = %d，期望去重后 1 条：%v", len(got), got)
	}
}

func TestMultiIsDeterministic(t *testing.T) {
	a := buildDB(t, []Record{
		{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "江苏省"},
		{Start: ipv4(9, 0, 0, 0), End: ipv4(9, 0, 0, 255), Country: "中国", Region: "江苏省"},
	})
	b := buildDB(t, []Record{
		{Start: ipv4(5, 0, 0, 0), End: ipv4(5, 0, 0, 255), Country: "中国", Region: "江苏省"},
	})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	first := m.CIDRsOfProvinces([]string{"江苏省"})
	if len(first) != 3 {
		t.Fatalf("CIDR 数 = %d，期望 3：%v", len(first), first)
	}
	// 结果必须按地址升序，而不是按数据源先后拼接：后者会让 b 的 5.0.0.0
	// 排到 a 的 9.0.0.0 后面。
	if first[0] != "1.0.0.0/24" || first[1] != "5.0.0.0/24" || first[2] != "9.0.0.0/24" {
		t.Fatalf("结果 = %v，期望按地址升序", first)
	}
	for i := 0; i < 5; i++ {
		again := m.CIDRsOfProvinces([]string{"江苏省"})
		for j := range first {
			if again[j] != first[j] {
				// 生成必须逐字节确定，否则 dat 内容哈希每轮都变，
				// 那个 10 秒的重启 cron 会不停重启 xray。
				t.Fatalf("第 %d 次第 %d 条不同: %q vs %q", i, j, again[j], first[j])
			}
		}
	}
}

func TestMultiSortsNumericallyNotLexically(t *testing.T) {
	a := buildDB(t, []Record{
		{Start: ipv4(9, 0, 0, 0), End: ipv4(9, 0, 0, 255), Country: "中国", Region: "江苏省"},
		{Start: ipv4(10, 0, 0, 0), End: ipv4(10, 0, 0, 255), Country: "中国", Region: "江苏省"},
	})
	m := NewMulti([]Named{{Key: "a", DB: a}})
	got := m.CIDRsOfProvinces([]string{"江苏省"})
	// 纯字符串比较会把 "10.0.0.0/24" 排到 "9.0.0.0/24" 前面。
	if len(got) != 2 || got[0] != "9.0.0.0/24" || got[1] != "10.0.0.0/24" {
		t.Errorf("结果 = %v，期望 [9.0.0.0/24 10.0.0.0/24]", got)
	}
}

func TestMultiUnionsProvinces(t *testing.T) {
	a := buildDB(t, []Record{{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "江苏省"}})
	b := buildDB(t, []Record{{Start: ipv4(2, 0, 0, 0), End: ipv4(2, 0, 0, 255), Country: "中国", Region: "河南省"}})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	got := m.Provinces()
	if len(got) != 2 || got[0] != "江苏省" || got[1] != "河南省" {
		t.Errorf("省份 = %v，期望两个源的并集且有序", got)
	}
}

func TestMultiLookupReportsEverySource(t *testing.T) {
	a := buildDB(t, []Record{{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "江苏省", City: "南京"}})
	b := buildDB(t, []Record{{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "山东省", City: "济南"}})
	m := NewMulti([]Named{{Key: "ip2region", DB: a}, {Key: "qqwry", DB: b}})

	got := m.Lookup(net.ParseIP("1.0.0.1"))
	// 两个源判定不一致时必须都返回，界面才能把分歧显示出来——
	// 这正是多源的意义所在。
	if len(got) != 2 {
		t.Fatalf("结果数 = %d，期望 2：%+v", len(got), got)
	}
	if got[0].Source != "ip2region" || got[0].Location.Region != "江苏省" {
		t.Errorf("第一个源 = %+v", got[0])
	}
	if got[1].Source != "qqwry" || got[1].Location.Region != "山东省" {
		t.Errorf("第二个源 = %+v", got[1])
	}
}

func TestMultiLookupSkipsSourcesWithoutTheIP(t *testing.T) {
	a := buildDB(t, []Record{{Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "中国", Region: "江苏省"}})
	b := buildDB(t, []Record{{Start: ipv4(2, 0, 0, 0), End: ipv4(2, 0, 0, 255), Country: "中国", Region: "河南省"}})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	got := m.Lookup(net.ParseIP("1.0.0.1"))
	if len(got) != 1 || got[0].Source != "a" {
		t.Errorf("结果 = %+v，期望只有收录了该 IP 的源", got)
	}
}

func TestEmptyMultiIsSafeToUse(t *testing.T) {
	m := NewMulti(nil)
	if m.Len() != 0 {
		t.Errorf("Len = %d，期望 0", m.Len())
	}
	if got := m.CIDRsOfProvinces([]string{"江苏省"}); got != nil {
		t.Errorf("CIDRsOfProvinces = %v，期望 nil", got)
	}
	if got := m.Provinces(); len(got) != 0 {
		t.Errorf("Provinces = %v，期望空", got)
	}
	if got := m.Lookup(net.ParseIP("1.2.3.4")); len(got) != 0 {
		t.Errorf("Lookup = %v，期望空", got)
	}
}
