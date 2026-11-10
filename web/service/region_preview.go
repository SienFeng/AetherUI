package service

import (
	"encoding/binary"
	"net"
	"sort"
	"time"

	"a-ui/database/model"
	"a-ui/util/common"
	"a-ui/util/ipdb"
)

// cidrSet 是一个只读的内存 IP 集合，供预演与自动出差放行判断「这个来源在不在
// 允许集里」。
//
// 不用 geoBlockMatcher：它读的是核心正在用的 dat，只认 dat 里已有的 tag。预演的
// 候选集合、发了通行证之后的「存储的地区」都不在 dat 里。CIDR 取自与执行同口径的
// regionCIDRSource，所以两边对同一个 IP 的判断一致。
type cidrSet struct {
	v4 []v4Range // 按 start 升序且互不重叠
	v6 []*net.IPNet
}

type v4Range struct{ start, end uint32 }

func newCIDRSet(cidrs []string) *cidrSet {
	s := &cidrSet{}
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if v4 := n.IP.To4(); v4 != nil {
			ones, _ := n.Mask.Size()
			start := binary.BigEndian.Uint32(v4)
			end := start | (uint32(0xffffffff) >> uint(ones))
			if ones == 0 {
				end = 0xffffffff
			}
			s.v4 = append(s.v4, v4Range{start, end})
			continue
		}
		s.v6 = append(s.v6, n)
	}
	sort.Slice(s.v4, func(i, j int) bool { return s.v4[i].start < s.v4[j].start })
	merged := s.v4[:0]
	for _, r := range s.v4 {
		if n := len(merged); n > 0 && uint64(r.start) <= uint64(merged[n-1].end)+1 {
			if r.end > merged[n-1].end {
				merged[n-1].end = r.end
			}
			continue
		}
		merged = append(merged, r)
	}
	s.v4 = merged
	return s
}

// Contains 报告 ip 是否落在集合里；非法 IP 一律为否。nil 接收者安全。
func (s *cidrSet) Contains(ip string) bool {
	if s == nil {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	if v4 := parsed.To4(); v4 != nil {
		x := binary.BigEndian.Uint32(v4)
		i := sort.Search(len(s.v4), func(i int) bool { return s.v4[i].end >= x })
		return i < len(s.v4) && s.v4[i].start <= x
	}
	for _, n := range s.v6 {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// allowedSetFor 按当前 IP 库与合并口径构造 provinces 的允许集。
func allowedSetFor(provinces []string) (*cidrSet, error) {
	db := (&IPDBService{}).DB()
	if db == nil {
		return nil, common.NewError("IP 归属地库未加载")
	}
	return newCIDRSet(regionCIDRSource(db, &SettingService{}).CIDRsOfProvinces(provinces)), nil
}

// majorityProvince 取各源里票数最多的中国省份，平票按源顺序取第一个；没有任何
// 源给出中国省份时为空。与界面「按多数票显示归属地」同一思路，供一键发通行证用。
func majorityProvince(found []ipdb.SourceLocation) string {
	votes := map[string]int{}
	order := []string{}
	for _, sl := range found {
		if sl.Location.Country != ipdb.ChinaCountry || sl.Location.Region == "" {
			continue
		}
		if votes[sl.Location.Region] == 0 {
			order = append(order, sl.Location.Region)
		}
		votes[sl.Location.Region]++
	}
	best := ""
	for _, p := range order {
		if votes[p] > votes[best] {
			best = p
		}
	}
	return best
}

// provinceOfIP 是 majorityProvince 的便捷入口；IP 库未加载或 IP 非法时为空。
func provinceOfIP(ip string) string {
	db := (&IPDBService{}).DB()
	parsed := net.ParseIP(ip)
	if db == nil || parsed == nil {
		return ""
	}
	return majorityProvince(db.Lookup(parsed))
}

// RegionPreview 回答「按这几个省限制，过去 7 天会拦掉谁」。
type RegionPreview struct {
	TotalHours   int             `json:"totalHours"`
	BlockedHours int             `json:"blockedHours"`
	TotalBytes   int64           `json:"totalBytes"`
	BlockedBytes int64           `json:"blockedBytes"`
	Blocked      []PreviewSource `json:"blocked"` // 按字节降序，最多 previewMaxSources 个
	MoreBlocked  int             `json:"moreBlocked"`
	// IPv6 来源单列：启用地区限制后它们直接用不了，不是「按省份被拦」。
	IPv6       []PreviewSource  `json:"ipv6"`
	Suggestion RegionSuggestion `json:"suggestion"`
	WindowDays int              `json:"windowDays"`
}

// PreviewSource 是一个来源 IP 在窗口内的汇总，归属地取它最新一行的快照。
type PreviewSource struct {
	IP       string `json:"ip"`
	Province string `json:"province"`
	City     string `json:"city"`
	ISP      string `json:"isp"`
	Hours    int    `json:"hours"`
	Bytes    int64  `json:"bytes"`
	LastHour int64  `json:"lastHour"` // Unix 秒
}

const previewMaxSources = 20

// previewFromRows 是预演的纯计算部分。
func previewFromRows(rows []model.InboundIPHour, allowed *cidrSet) RegionPreview {
	p := RegionPreview{Blocked: []PreviewSource{}, IPv6: []PreviewSource{}}
	allHours := map[int64]bool{}
	blockedHours := map[int64]bool{}
	byIP := map[string]*PreviewSource{}
	hoursByIP := map[string]map[int64]bool{}
	var order []string
	for _, r := range rows {
		allHours[r.HourStart] = true
		p.TotalBytes += r.ActiveBytes
		src := byIP[r.IP]
		if src == nil {
			src = &PreviewSource{IP: r.IP}
			byIP[r.IP] = src
			hoursByIP[r.IP] = map[int64]bool{}
			order = append(order, r.IP)
		}
		src.Bytes += r.ActiveBytes
		hoursByIP[r.IP][r.HourStart] = true
		if r.HourStart >= src.LastHour {
			src.LastHour = r.HourStart
			src.Province, src.City, src.ISP = r.Province, r.City, r.ISP
		}
	}
	p.TotalHours = len(allHours)
	for _, ip := range order {
		src := byIP[ip]
		src.Hours = len(hoursByIP[ip])
		parsed := net.ParseIP(ip)
		switch {
		case parsed != nil && parsed.To4() == nil:
			p.IPv6 = append(p.IPv6, *src)
		case !allowed.Contains(ip):
			p.Blocked = append(p.Blocked, *src)
			p.BlockedBytes += src.Bytes
			for h := range hoursByIP[ip] {
				blockedHours[h] = true
			}
		}
	}
	p.BlockedHours = len(blockedHours)
	byBytes := func(list []PreviewSource) {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Bytes != list[j].Bytes {
				return list[i].Bytes > list[j].Bytes
			}
			return list[i].IP < list[j].IP
		})
	}
	byBytes(p.Blocked)
	byBytes(p.IPv6)
	if len(p.Blocked) > previewMaxSources {
		p.MoreBlocked = len(p.Blocked) - previewMaxSources
		p.Blocked = p.Blocked[:previewMaxSources]
	}
	if len(p.IPv6) > previewMaxSources {
		p.IPv6 = p.IPv6[:previewMaxSources]
	}
	return p
}

// RegionPreviewService 计算地区限制的预演。
type RegionPreviewService struct{}

// Preview 用该入站近 sharingWindowDays 天按来源 IP 的小时用量，算出按 regions
// 限制时会被拦掉的来源。只覆盖活跃满 60 秒、落过库的来源（见 sharingFlushThreshold）。
func (s *RegionPreviewService) Preview(inboundId int, regions []string, now time.Time) (*RegionPreview, error) {
	// windowRows 把 0 当成「所有入站」：不挡的话，一个手写的请求能拿到混着所有客户来源的预演。
	if inboundId <= 0 {
		return nil, common.NewError("入站不存在")
	}
	if _, err := (&InboundService{}).GetInbound(inboundId); err != nil {
		return nil, common.NewError("入站不存在")
	}
	regions = normalizeRegions(regions)
	if len(regions) == 0 {
		return nil, common.NewError("请先选择地区")
	}
	// 先查 IP 库：checkPassProvinces 在库没加载时说的是通行证的事，放在表单里文不对题。
	if (&IPDBService{}).DB() == nil {
		return nil, common.NewError("IP 归属地库未加载，无法预演；请先到「面板设置 → IP 归属地库」更新")
	}
	if err := checkPassProvinces(regions); err != nil {
		return nil, err
	}
	allowed, err := allowedSetFor(regions)
	if err != nil {
		return nil, err
	}
	rows, err := (&SharingService{}).windowRows(inboundId, sharingWindowDays, now)
	if err != nil {
		return nil, err
	}
	p := previewFromRows(rows, allowed)
	p.Suggestion = suggestRegions(rows)
	p.WindowDays = sharingWindowDays
	return &p, nil
}
