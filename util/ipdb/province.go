package ipdb

import (
	"fmt"
	"math/bits"
	"net"
	"sort"
)

// Provinces 返回库中收录的全部中国省级名称，按 Unicode 升序，供前端下拉框使用。
// 上游用 "0" 表示省份未知，normalize 已把它归一成空串，这里一并排除。
func (d *DB) Provinces() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 40)
	for _, loc := range d.locations {
		if loc.Country != chinaCountry || loc.Region == "" || seen[loc.Region] {
			continue
		}
		seen[loc.Region] = true
		out = append(out, loc.Region)
	}
	sort.Strings(out)
	return out
}

// CIDRsOfProvinces 返回给定省份覆盖的全部 IPv4 段，转换成最小的 CIDR 集合。
//
// 先把相连的段合并成区间再转 CIDR：同一个省的多个城市在 IP 上往往是相连的，
// 不合并会产出一堆本可以合成一条的 /24。
//
// 结果按 IP 升序，且对同样的输入逐字节确定——调用方要用它生成 xray 的 geo
// 数据文件，而那份文件的内容哈希会进 ruleTag，不确定的顺序会导致 xray 反复重启。
//
// 返回空切片表示这些省份在库中没有任何 IP 段。调用方**必须**把空结果当成
// 「不要生成这条规则」：空的允许集配上 ! 取反，等于拒绝所有人。
func (d *DB) CIDRsOfProvinces(provinces []string) []string {
	if len(provinces) == 0 {
		return nil
	}
	want := make(map[string]bool, len(provinces))
	for _, p := range provinces {
		want[p] = true
	}

	out := make([]string, 0, 64)
	var (
		hasRange   bool
		start, end uint32
	)
	flush := func() {
		if hasRange {
			out = append(out, rangeToCIDRs(start, end)...)
			hasRange = false
		}
	}
	for _, s := range d.segments {
		loc := d.locations[s.loc]
		if loc.Country != chinaCountry || !want[loc.Region] {
			continue
		}
		if hasRange && end+1 == s.start {
			end = s.end
			continue
		}
		flush()
		hasRange, start, end = true, s.start, s.end
	}
	flush()
	if len(out) == 0 {
		return nil
	}
	return out
}

// rangeToCIDRs 把一个闭区间拆成最少的 CIDR。每一步取「起点对齐允许的最大块」
// 与「剩余长度允许的最大块」中较小的那个。
//
// 全程用 uint64 运算：区间是 0.0.0.0-255.255.255.255 时长度为 2^32，用 uint32 会溢出成 0。
func rangeToCIDRs(startIP, endIP uint32) []string {
	var out []string
	s, e := uint64(startIP), uint64(endIP)
	for s <= e {
		alignment := uint(bits.TrailingZeros32(uint32(s))) // s 为 0 时返回 32
		size := uint(bits.Len64(e-s+1)) - 1
		if size > alignment {
			size = alignment
		}
		out = append(out, fmt.Sprintf("%s/%d", uint32ToIP(uint32(s)), 32-size))
		s += 1 << size
	}
	return out
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
