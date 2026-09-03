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
