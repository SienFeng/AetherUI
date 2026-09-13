package ipdb

import (
	"bytes"
	"net"
	"sort"
)

// Named 是一个带标识的数据源。
type Named struct {
	Key string
	DB  *DB
}

// SourceLocation 是某个数据源对一个 IP 的判定。
type SourceLocation struct {
	Source   string
	Location Location
}

// Multi 把多个离线归属地库并起来用。
//
// 合并策略是**并集**而不是交集：实测过 ip2region、纯真与几家在线接口对同一
// 批 IP 的判定，四者互有出入，没有哪一个是权威。对用户而言「本该能连却连不上」
// 比「本不该连却连上了」严重得多，所以任一数据源认为某段属于所选省份就放行。
type Multi struct {
	sources []Named
}

// NewMulti 按给定顺序组合数据源。顺序决定 Lookup 的返回顺序，
// 也决定并集结果的稳定性，调用方必须传入固定顺序。
func NewMulti(sources []Named) *Multi {
	kept := make([]Named, 0, len(sources))
	for _, s := range sources {
		if s.DB != nil {
			kept = append(kept, s)
		}
	}
	return &Multi{sources: kept}
}

// Len 返回实际加载成功的数据源个数。
func (m *Multi) Len() int { return len(m.sources) }

// Sources 返回各数据源的标识与库，顺序与构造时一致。
func (m *Multi) Sources() []Named {
	out := make([]Named, len(m.sources))
	copy(out, m.sources)
	return out
}

// CIDRsOfProvinces 返回所有数据源认为属于这些省份的 IP 段的并集。
//
// 结果去重并按地址升序排列。顺序必须确定：它最终会写进 geo dat，
// 内容哈希不稳定的话，那个 10 秒的重启 cron 会不停重启 xray。
func (m *Multi) CIDRsOfProvinces(provinces []string) []string {
	seen := map[string]bool{}
	var all []string
	for _, s := range m.sources {
		for _, c := range s.DB.CIDRsOfProvinces(provinces) {
			if seen[c] {
				continue
			}
			seen[c] = true
			all = append(all, c)
		}
	}
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return lessCIDR(all[i], all[j]) })
	return all
}

// MajorityCIDRsOfProvinces 返回**没有被多数源反对**的 IP 段，是 CIDRsOfProvinces
// 那个并集口径的收紧版本。
//
// 判据是逐地址比较两个计数，**沉默的源不计入任何一边**：
//
//	支持 = 认为该地址属于所选省份的源数
//	反对 = 认为它属于**其他省**的源数
//	沉默 = 没有该地址省级数据的源数（没收录，或收录了但是境外段）
//
//	支持 > 反对  → 放行
//	否则         → 不放行
//
// **判「反对」而不是判「支持够不够多」，是这个实现与朴素共识制的唯一区别，
// 也是它全部的价值所在。** 实测（三个源，采样单源命中的区间）：其余源根本没有
// 该 IP 省级数据的占 25%~49%，认为属于别的省的占 51%~75%。前者是数据缺口而不是
// 证据——把「没看过」当成「反对」，会让「1 个源认定江苏 + 3 个源没收录」与
// 「1 个源认定江苏 + 3 个源认定浙江」得到同样的结论，而这两件事完全不同。
// 按支持数设阈值（哪怕只要求 2 票）必然抹掉这个区别，因为信息在数票那一步就丢了。
//
// 与并集的实际差距没有想象中大：实测三源并集只比最大的单源宽 1%~18%（按覆盖的
// IP 地址数，不是 CIDR 条数——两个源对同一片地址的切分粒度不同会让条数翻倍而
// 地址几乎完全重合，拿条数衡量会把 1.03 倍看成 1.45 倍）。
//
// 结果与 CIDRsOfProvinces 同样是升序且逐字节确定：它同样会进 geo dat，
// 内容哈希不稳定会让那个 10 秒的 cron 不停重启 xray。
func (m *Multi) MajorityCIDRsOfProvinces(provinces []string) []string {
	if len(provinces) == 0 || len(m.sources) == 0 {
		return nil
	}
	want := make(map[string]bool, len(provinces))
	for _, p := range provinces {
		want[p] = true
	}

	// 事件点用 uint64：段的末地址是 0xFFFFFFFF 时，结束点 end+1 会溢出 uint32。
	type event struct {
		at       uint64
		support  int
		opposing int
	}
	events := make([]event, 0, 1024)
	for _, s := range m.sources {
		for _, seg := range s.DB.segments {
			loc := s.DB.locations[seg.loc]
			// 沉默：境外段与没有省份的段都不表态，既不支持也不反对。
			if loc.Country != chinaCountry || loc.Region == "" {
				continue
			}
			lo, hi := uint64(seg.start), uint64(seg.end)+1
			if want[loc.Region] {
				events = append(events, event{lo, 1, 0}, event{hi, -1, 0})
			} else {
				events = append(events, event{lo, 0, 1}, event{hi, 0, -1})
			}
		}
	}
	if len(events) == 0 {
		return nil
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at < events[j].at })

	out := make([]string, 0, 64)
	var (
		support, opposing int
		rangeStart        uint64
		inRange           bool
	)
	for i := 0; i < len(events); {
		at := events[i].at
		// 同一个地址上的事件必须一次全部应用完再判定，否则一段的结束与另一段
		// 的开始之间会凭空出现一个宽度为零、计数却已经变化的区间。
		for i < len(events) && events[i].at == at {
			support += events[i].support
			opposing += events[i].opposing
			i++
		}
		switch admit := support > opposing; {
		case admit && !inRange:
			rangeStart, inRange = at, true
		case !admit && inRange:
			out = append(out, rangeToCIDRs(uint32(rangeStart), uint32(at-1))...)
			inRange = false
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lessCIDR 按网络地址、再按掩码长度排序，纯字符串比较会把 10.x 排到 9.x 前面。
func lessCIDR(a, b string) bool {
	ipA, netA, errA := net.ParseCIDR(a)
	ipB, netB, errB := net.ParseCIDR(b)
	if errA != nil || errB != nil {
		return a < b
	}
	if c := bytes.Compare(ipA.To16(), ipB.To16()); c != 0 {
		return c < 0
	}
	onesA, _ := netA.Mask.Size()
	onesB, _ := netB.Mask.Size()
	return onesA < onesB
}

// Provinces 返回所有数据源收录的省级地区名的并集，升序。
func (m *Multi) Provinces() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 34)
	for _, s := range m.sources {
		for _, p := range s.DB.Provinces() {
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Lookup 返回每个收录了该 IP 的数据源各自的判定。
//
// 不做仲裁，原样返回：各源判定不一致时界面要把分歧显示出来，
// 那正是多源存在的意义。
func (m *Multi) Lookup(ip net.IP) []SourceLocation {
	out := make([]SourceLocation, 0, len(m.sources))
	for _, s := range m.sources {
		loc, ok := s.DB.Lookup(ip)
		if !ok {
			continue
		}
		out = append(out, SourceLocation{Source: s.Key, Location: loc})
	}
	return out
}
