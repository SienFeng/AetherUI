package service

import (
	"sort"
	"strings"
	"time"

	"a-ui/database/model"
	"a-ui/util/ipdb"
)

// 网络画像把「同一个网络出来的若干次观测」聚成一个分析单位。
//
// 整个子系统的支点是：**画像数量本身不是信号**。一个完全正常的单人用户，
// 家里电信宽带、外出用移动蜂窝，在下面的画像键下就是两个独立稳定画像，而且
// 天然错峰。若把「两个稳定画像 + 错峰」直接计分，他拿到的分数会与真正的
// 跨省共享完全相同——区分度为零，而且不是调权重能解决的。
//
// 真正的支点是**画像距离**（profileDistance）：只有跨省、跨国才携带信息。
// 设计文档 §6。

// 稳定画像的门槛。下面所有信号只认稳定画像——低于这些量的画像是一次出差、
// 一次连错网络留下的痕迹，拿它做判断就是把偶发当常态。
const (
	stableProfileMinDays    = 3
	stableProfileMinHours   = 6
	stableProfileMinBytes   = 10 << 20
	stableProfileMinShare   = 0.05
	profileKeySeparator     = "|"
	profileKeyPrefixMarker  = "prefix:"
	profileKeyHostingMarker = "IDC"
)

// NetworkProfile 是一个网络画像在分析窗口内的全部度量。
//
// 刻意**不提供 per-prefix 的字节 / 天数 / 小时数**：画像键把同省同 ISP 的
// 不同网络族合并进了同一个画像，那个维度在这里算不出来。真需要时另立
// PrefixStat 单独立项，不在这里硬塞一个算不准的近似。
type NetworkProfile struct {
	Key      string `json:"key"`
	Country  string `json:"country"`
	Province string `json:"province"`
	ISP      string `json:"isp"`

	// Cities / Prefixes / IPs 升序去重，仅供展示。
	Cities   []string `json:"cities"`
	Prefixes []string `json:"prefixes"`
	IPs      []string `json:"ips"`

	FirstSeen int64 `json:"firstSeen"`
	LastSeen  int64 `json:"lastSeen"`

	// ActiveDays 是有过有效小时的**日历天数**，按面板时区切天。
	ActiveDays int `json:"activeDays"`
	// ActiveHours 是**去重后的**有效小时数：同一小时里该画像下有多个 IP 时
	// 只算一个小时。按行数算会让一个换 IP 频繁的用户凭空多出几倍活跃小时，
	// 而门槛判定正是按小时数设的。
	ActiveHours int `json:"activeHours"`

	Bytes        int64   `json:"bytes"`
	Up           int64   `json:"up"`
	Down         int64   `json:"down"`
	TrafficShare float64 `json:"trafficShare"`

	// Hosting 表示这个画像落在机房 / 云厂商，而不是普通接入网络。
	Hosting bool `json:"hosting"`
}

// Stable 判断这个画像够不够格参与信号判定。
func (p NetworkProfile) Stable() bool {
	return p.ActiveDays >= stableProfileMinDays &&
		p.ActiveHours >= stableProfileMinHours &&
		p.Bytes >= stableProfileMinBytes &&
		p.TrafficShare >= stableProfileMinShare
}

// profileMeta 是构造画像键需要的那几个字段，从一行观测里取。
type profileMeta struct {
	Country  string
	Province string
	ISP      string
	Family   string
}

func metaOfRow(r model.InboundIPHour) profileMeta {
	return profileMeta{
		Country:  r.Country,
		Province: r.Province,
		ISP:      r.ISP,
		Family:   networkFamilyOrIP(r.IP),
	}
}

// profileKeyOf 按可用信息分层构造画像键，第一个能构造出来的即为结果。
//
//	中国 IPv4              CN|<省>|<ISP>
//	中国 IPv4，ISP 未知     CN|<省>|prefix:<网络族>
//	境外，已识别 IDC        <国>|IDC|<IDC名>
//	境外普通网络            <国>|prefix:<网络族>
//	无国家信息（v6 等）      prefix:<网络族>
//
// **城市不进键。** 运营商 IP 定位到市的稳定性远不如到省——把南通 / 南京 /
// 苏州判成三个用户是纯粹的误报来源。城市作为展示用的子特征保留。
func profileKeyOf(m profileMeta) string {
	if m.Country == ipdb.ChinaCountry && m.Province != "" {
		if m.ISP != "" {
			return join("CN", m.Province, m.ISP)
		}
		return join("CN", m.Province, profileKeyPrefixMarker+m.Family)
	}
	if m.Country != "" {
		// 境外段的 ISP 只可能是知名 IDC——CanonicalISP(raw, foreign=true)
		// 对其余一律返回空串（util/ipdb/isp_name.go:97）。
		if m.ISP != "" {
			return join(m.Country, profileKeyHostingMarker, m.ISP)
		}
		return join(m.Country, profileKeyPrefixMarker+m.Family)
	}
	return profileKeyPrefixMarker + m.Family
}

func join(parts ...string) string { return strings.Join(parts, profileKeySeparator) }

// profileDistance 的取值。距离是本子系统唯一的新支点，见 §6。
const (
	// distanceUnknown 表示信息不足以判断两个画像的地理关系。**不参与任何
	// 计分信号**——宁可不计分让管理员察觉，也绝不拿一个猜出来的距离去加分。
	distanceUnknown = -1
	// distanceSameISP 同省同 ISP：信息量为零，动态家宽换段是常态。
	distanceSameISP = 0
	// distanceSameProvince 同省不同 ISP：信息量近乎为零，**家宽 + 手机就是
	// 这个形态**，是最常见的良性双画像。
	distanceSameProvince = 1
	// distanceCrossProvince 跨省（中国境内）。
	distanceCrossProvince = 2
	// distanceCrossCountry 跨国。
	distanceCrossCountry = 3
)

// distanceScorable 是计分信号的准入线：只有跨省、跨国才携带信息。
const distanceScorable = distanceCrossProvince

// profileDistance 算两个画像的地理距离。
//
// 最后一条分支（同一个外国内部）返回 unknown 是有意的保守取值：
// ipdb.normalize 对境外只保留国家，同一个外国内部没有任何可用的地理维度，
// 「US Amazon + US Vultr」这种组合因此不计分。记在能力边界里，要修需要
// ASN 库。
func profileDistance(a, b NetworkProfile) int {
	if a.Country == "" || b.Country == "" {
		return distanceUnknown
	}
	if a.Country != b.Country {
		return distanceCrossCountry
	}
	if a.Country != ipdb.ChinaCountry {
		return distanceUnknown
	}
	if a.Province == "" || b.Province == "" {
		return distanceUnknown
	}
	if a.Province != b.Province {
		return distanceCrossProvince
	}
	if a.ISP != b.ISP {
		return distanceSameProvince
	}
	return distanceSameISP
}

// effectiveRow 判断一行观测算不算「有效小时」。
//
// 判据与并存判定完全一致（coexistMinActiveBytes）。**不另搞一套门槛**：
// 两套门槛会让管理员看到「共享检测：无异常 / 风险评分：高风险」而不知道
// 为什么，而这个 1 MB 是有生产实测支撑的唯一一个数。
func effectiveRow(r model.InboundIPHour) bool {
	return r.ActiveBytes >= coexistMinActiveBytes
}

// buildProfiles 把一批观测聚成画像。
//
// **纯函数，不查库。** 本期几乎所有阈值都要按生产分布回标，纯函数才谈得上
// 用真实数据反复重算。
//
// 输出按 Bytes 降序、Key 升序——禁止遍历 map 产生数组顺序，否则同一份输入
// 每次渲染出来的次序都不一样。
func buildProfiles(rows []model.InboundIPHour, loc *time.Location) []NetworkProfile {
	if loc == nil {
		loc = time.UTC
	}

	type acc struct {
		p        NetworkProfile
		days     map[string]bool
		hours    map[int64]bool
		cities   map[string]bool
		prefixes map[string]bool
		ips      map[string]bool
	}
	byKey := map[string]*acc{}
	order := make([]string, 0)
	var totalBytes int64

	for _, r := range rows {
		if !effectiveRow(r) {
			continue
		}
		m := metaOfRow(r)
		key := profileKeyOf(m)
		a := byKey[key]
		if a == nil {
			a = &acc{
				p: NetworkProfile{
					Key: key, Country: m.Country, Province: m.Province, ISP: m.ISP,
					Hosting:   ipdb.IsKnownIDC(m.ISP),
					FirstSeen: r.HourStart, LastSeen: r.HourStart,
				},
				days: map[string]bool{}, hours: map[int64]bool{},
				cities: map[string]bool{}, prefixes: map[string]bool{}, ips: map[string]bool{},
			}
			byKey[key] = a
			order = append(order, key)
		}
		a.p.Bytes += r.ActiveBytes
		a.p.Up += r.ActiveUp
		a.p.Down += r.ActiveDown
		totalBytes += r.ActiveBytes

		if r.HourStart < a.p.FirstSeen {
			a.p.FirstSeen = r.HourStart
		}
		if r.HourStart > a.p.LastSeen {
			a.p.LastSeen = r.HourStart
		}
		a.days[dayKey(r.HourStart, loc)] = true
		a.hours[r.HourStart] = true
		if r.City != "" {
			a.cities[r.City] = true
		}
		a.prefixes[m.Family] = true
		a.ips[r.IP] = true
	}

	out := make([]NetworkProfile, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		a.p.ActiveDays = len(a.days)
		a.p.ActiveHours = len(a.hours)
		a.p.Cities = sortedKeys(a.cities)
		a.p.Prefixes = sortedKeys(a.prefixes)
		a.p.IPs = sortedKeys(a.ips)
		if totalBytes > 0 {
			a.p.TrafficShare = float64(a.p.Bytes) / float64(totalBytes)
		}
		out = append(out, a.p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// dayKey 把一个 UTC 小时戳折成「面板时区下的哪一天」。
//
// 桶本身按 UTC 对齐（model.AlignHourUTC，刻意与 TrafficBucket 相反），但
// 「活跃了几天」是给人看的，必须按面板时区切——UTC+8 下按 UTC 切天会让
// 每天的前 8 小时算进前一天，而这个数直接进稳定画像的门槛判定。
func dayKey(hourStart int64, loc *time.Location) string {
	return time.Unix(hourStart, 0).In(loc).Format("2006-01-02")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stableProfiles 过滤出够格参与信号判定的画像，保持 buildProfiles 的顺序。
func stableProfiles(profiles []NetworkProfile) []NetworkProfile {
	out := make([]NetworkProfile, 0, len(profiles))
	for _, p := range profiles {
		if p.Stable() {
			out = append(out, p)
		}
	}
	return out
}
