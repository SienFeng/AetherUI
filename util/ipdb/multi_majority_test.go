package ipdb

import (
	"strings"
	"testing"
)

func cn(start, end uint32, region string) Record {
	return Record{Start: start, End: end, Country: chinaCountry, Region: region}
}

// 这是 MajorityCIDRsOfProvinces 与朴素共识制的唯一区别，也是它全部的价值：
// 其余源**没有**这个 IP 的省级数据，是数据缺口而不是反对票。实测单源命中的
// 区间里有 25%~49% 属于这一类，把它们当反对会变成纯粹的误拦。
func TestMajoritySilentSourcesDoNotOppose(t *testing.T) {
	// a 认定江苏；b 根本没收录这一段；c 只收录了境外段（有国家没省份）。
	a := buildDB(t, []Record{cn(ipv4(1, 0, 0, 0), ipv4(1, 0, 0, 255), "江苏省")})
	b := buildDB(t, []Record{cn(ipv4(9, 0, 0, 0), ipv4(9, 0, 0, 255), "广东省")})
	c := buildDB(t, []Record{{
		Start: ipv4(1, 0, 0, 0), End: ipv4(1, 0, 0, 255), Country: "Australia",
	}})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}, {Key: "c", DB: c}})

	got := m.MajorityCIDRsOfProvinces([]string{"江苏省"})
	if len(got) != 1 || got[0] != "1.0.0.0/24" {
		t.Errorf("= %v；1 源支持 + 2 源沉默必须放行（沉默不是反对）", got)
	}
}

// 支持与反对相等时不放行：这正是并集口径下会被放宽的那部分。
func TestMajorityTiedVoteIsRejected(t *testing.T) {
	a := buildDB(t, []Record{cn(ipv4(1, 0, 0, 0), ipv4(1, 0, 0, 255), "江苏省")})
	b := buildDB(t, []Record{cn(ipv4(1, 0, 0, 0), ipv4(1, 0, 0, 255), "浙江省")})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	if got := m.MajorityCIDRsOfProvinces([]string{"江苏省"}); got != nil {
		t.Errorf("= %v；1:1 无多数时必须不放行", got)
	}
	// 同一份输入在并集口径下是放行的——两个口径的差异就体现在这里。
	if got := m.CIDRsOfProvinces([]string{"江苏省"}); len(got) != 1 {
		t.Errorf("并集口径 = %v；应当放行，否则这条对照失去意义", got)
	}
}

func TestMajorityAcceptsStrictMajority(t *testing.T) {
	seg := func(region string) *DB {
		return buildDB(t, []Record{cn(ipv4(1, 0, 0, 0), ipv4(1, 0, 0, 255), region)})
	}
	m := NewMulti([]Named{
		{Key: "a", DB: seg("江苏省")},
		{Key: "b", DB: seg("江苏省")},
		{Key: "c", DB: seg("浙江省")},
	})
	got := m.MajorityCIDRsOfProvinces([]string{"江苏省"})
	if len(got) != 1 || got[0] != "1.0.0.0/24" {
		t.Errorf("= %v；2:1 有严格多数，应当放行", got)
	}
}

// 逐地址判定，不是逐段：两个源的段边界不对齐时，同一个段内不同地址的票数
// 可能不同。按段判会让边界上的地址整段跟着一起放行或拒绝。
func TestMajorityIsEvaluatedPerAddressNotPerSegment(t *testing.T) {
	// a 认定 1.0.0.0-1.0.1.255 全是江苏；b 只在后半段反对。
	a := buildDB(t, []Record{cn(ipv4(1, 0, 0, 0), ipv4(1, 0, 1, 255), "江苏省")})
	b := buildDB(t, []Record{cn(ipv4(1, 0, 1, 0), ipv4(1, 0, 1, 255), "浙江省")})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	got := m.MajorityCIDRsOfProvinces([]string{"江苏省"})
	if len(got) != 1 || got[0] != "1.0.0.0/24" {
		t.Errorf("= %v；前半段 1:0 应放行，后半段 1:1 应拒绝", got)
	}
}

// 末地址是 255.255.255.255 时，区间的结束点是 2^32，用 uint32 记事件会溢出成 0，
// 把整个区间折叠掉。
func TestMajorityHandlesLastAddress(t *testing.T) {
	a := buildDB(t, []Record{cn(ipv4(255, 255, 255, 0), ipv4(255, 255, 255, 255), "江苏省")})
	m := NewMulti([]Named{{Key: "a", DB: a}})

	got := m.MajorityCIDRsOfProvinces([]string{"江苏省"})
	if len(got) != 1 || got[0] != "255.255.255.0/24" {
		t.Errorf("= %v；想要 [255.255.255.0/24]", got)
	}
}

// 结果要能直接进 geo dat：升序、逐字节确定。顺序不稳会让内容哈希每轮都变，
// 那个 10 秒的 cron 会不停重启 xray。
func TestMajorityOutputIsSortedAndDeterministic(t *testing.T) {
	a := buildDB(t, []Record{
		cn(ipv4(3, 0, 0, 0), ipv4(3, 0, 0, 255), "江苏省"),
		cn(ipv4(10, 0, 0, 0), ipv4(10, 0, 0, 255), "江苏省"),
	})
	b := buildDB(t, []Record{cn(ipv4(9, 0, 0, 0), ipv4(9, 0, 0, 255), "江苏省")})
	m := NewMulti([]Named{{Key: "a", DB: a}, {Key: "b", DB: b}})

	first := m.MajorityCIDRsOfProvinces([]string{"江苏省"})
	if len(first) != 3 {
		t.Fatalf("= %v；想要 3 段", first)
	}
	want := []string{"3.0.0.0/24", "9.0.0.0/24", "10.0.0.0/24"}
	for i, w := range want {
		if first[i] != w {
			t.Errorf("第 %d 段 = %q；想要 %q（必须按地址升序，不能按字符串序）", i, first[i], w)
		}
	}
	if second := m.MajorityCIDRsOfProvinces([]string{"江苏省"}); strings.Join(first, ",") != strings.Join(second, ",") {
		t.Error("两次调用结果不同，生成必须逐字节确定")
	}
}

func TestMajorityEmptyInputs(t *testing.T) {
	a := buildDB(t, []Record{cn(ipv4(1, 0, 0, 0), ipv4(1, 0, 0, 255), "江苏省")})
	m := NewMulti([]Named{{Key: "a", DB: a}})

	if got := m.MajorityCIDRsOfProvinces(nil); got != nil {
		t.Errorf("空省份列表 = %v；想要 nil", got)
	}
	if got := m.MajorityCIDRsOfProvinces([]string{"西藏"}); got != nil {
		t.Errorf("库里没有的省份 = %v；想要 nil", got)
	}
	empty := NewMulti(nil)
	if got := empty.MajorityCIDRsOfProvinces([]string{"江苏省"}); got != nil {
		t.Errorf("没有数据源 = %v；想要 nil", got)
	}
}
