package service

import (
	"sort"
	"time"

	"a-ui/database/model"
)

// 错峰检测：两个稳定画像各自规律活跃，却几乎从不在同一小时出现。
//
// 这是第一期明确留下的漏检（白天甲用、晚上乙用，落在不同小时，不产生并存
// 记录）。但它**在证据强度上结构性地弱于并存**，必须一开始就说清楚：
//
// 跨省错峰与「每周在两个城市之间通勤的同一个人」在网络层完全同形——搬过家
// 的、周末回老家的、异地双城工作的，全是这个形状。没有任何网络层数据能把
// 它们分开。所以错峰的分值上限低于并存，而且单独存在时**不允许**把风险推
// 到「高」（评分层有一道显式钳制，见 sharing_risk.go）。设计文档 §7.3。

// 错峰的触发门槛，全部同时满足才算。
const (
	// temporalMinDistance 是最要紧的一条：没有它整个信号毫无意义。
	// 家宽 + 手机（同省不同 ISP，距离 1）天然错峰，会与真正的跨省共享
	// 拿到完全相同的分数。
	temporalMinDistance = distanceScorable

	temporalMinActiveDays  = 3
	temporalMinActiveHours = 6
	temporalMinShare       = 0.10

	// temporalMinSharedDays 专门挡旅游。一次出差是「旧省停、新省起」，
	// 两个画像各自活跃 5 天但**共同活跃**只有交界的 1~2 天，够不到这条。
	// 这与第一期用「并存」挡旅游是同一个机制，只是换了一个维度。
	temporalMinSharedDays = 3

	// temporalMaxOverlapRatio 是「几乎不重叠」的判据。
	temporalMaxOverlapRatio = 0.10

	// 更强的一档：连续更久、重叠更少。
	temporalPersistentMinSharedDays = 5
	temporalPersistentMaxOverlap    = 0.05
)

// TemporalPair 是一对画像的错峰分析结果。
type TemporalPair struct {
	KeyA string `json:"keyA"`
	KeyB string `json:"keyB"`

	Distance int `json:"distance"`

	// SharedDays 是两者都有活跃的日历天数（按面板时区切天）。
	SharedDays int `json:"sharedDays"`

	AHours       int     `json:"aHours"`
	BHours       int     `json:"bHours"`
	OverlapHours int     `json:"overlapHours"`
	OverlapRatio float64 `json:"overlapRatio"`

	// Persistent 表示命中了更强的那一档。
	Persistent bool `json:"persistent"`
}

// buildDayMasks 把观测折成「画像 → 每天的 24 小时活跃位图」。
//
// 位图用 uint32 的低 24 位，位序是「面板时区下的第几个小时」——桶本身按
// UTC 对齐，但「白天用还是晚上用」是给人看的判断，必须按面板时区排。
func buildDayMasks(rows []model.InboundIPHour, loc *time.Location) map[string]map[string]uint32 {
	if loc == nil {
		loc = time.UTC
	}
	out := map[string]map[string]uint32{}
	for _, r := range rows {
		if !effectiveRow(r) {
			continue
		}
		key := profileKeyOf(metaOfRow(r))
		local := time.Unix(r.HourStart, 0).In(loc)
		day := local.Format("2006-01-02")
		if out[key] == nil {
			out[key] = map[string]uint32{}
		}
		out[key][day] |= 1 << uint(local.Hour())
	}
	return out
}

// computeTemporal 找出窗口内全部命中错峰的画像对。
//
// **纯函数，不查库。** 输出按 (KeyA, KeyB) 升序，保证同一份输入永远产生
// 同一个次序——禁止遍历 map 产生数组顺序。
func computeTemporal(rows []model.InboundIPHour, profiles []NetworkProfile, loc *time.Location) []TemporalPair {
	// 距离 0（同省同 ISP）、1（同省不同 ISP，即家宽 + 手机）与 unknown 一律
	// 不进。这一条不是可调参数，是整个信号成立的前提。
	return temporalPairsWhere(rows, profiles, loc, func(d int) bool { return d >= temporalMinDistance })
}

// computeTemporalSameProvince 找出**同省不同 ISP**（距离 1）的错峰对。
//
// 它专供上下文证据使用，**不计分**：这个形态会对几乎每个双设备用户命中
// （家宽 + 手机就是它），给分等于给所有正常用户一个固定底分，纯噪声。先把
// 它采集出来显示在证据列表里，跑两周看真实命中率，再决定要不要赋分。
func computeTemporalSameProvince(rows []model.InboundIPHour, profiles []NetworkProfile, loc *time.Location) []TemporalPair {
	return temporalPairsWhere(rows, profiles, loc, func(d int) bool { return d == distanceSameProvince })
}

func temporalPairsWhere(rows []model.InboundIPHour, profiles []NetworkProfile, loc *time.Location, accept func(int) bool) []TemporalPair {
	stable := stableProfiles(profiles)
	if len(stable) < 2 {
		return nil
	}
	masks := buildDayMasks(rows, loc)

	var out []TemporalPair
	for i := 0; i < len(stable); i++ {
		for j := i + 1; j < len(stable); j++ {
			a, b := stable[i], stable[j]
			dist := profileDistance(a, b)
			if !accept(dist) {
				continue
			}
			if !temporalEligible(a) || !temporalEligible(b) {
				continue
			}
			pair, ok := pairOverlap(a, b, dist, masks)
			if !ok {
				continue
			}
			out = append(out, pair)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].KeyA != out[j].KeyA {
			return out[i].KeyA < out[j].KeyA
		}
		return out[i].KeyB < out[j].KeyB
	})
	return out
}

// temporalEligible 是单个画像进入错峰判定的门槛。比 Stable 更严：一个只占
// 6% 流量的画像即便「稳定」，拿它去论证错峰共享也太弱。
func temporalEligible(p NetworkProfile) bool {
	return p.ActiveDays >= temporalMinActiveDays &&
		p.ActiveHours >= temporalMinActiveHours &&
		p.TrafficShare >= temporalMinShare
}

// pairOverlap 算一对画像的重叠情况，不满足门槛时返回 ok=false。
func pairOverlap(a, b NetworkProfile, dist int, masks map[string]map[string]uint32) (TemporalPair, bool) {
	ma, mb := masks[a.Key], masks[b.Key]
	if ma == nil || mb == nil {
		return TemporalPair{}, false
	}

	shared, overlap := 0, 0
	for day, maskA := range ma {
		maskB, ok := mb[day]
		if !ok {
			continue
		}
		shared++
		overlap += popcount24(maskA & maskB)
	}
	if shared < temporalMinSharedDays {
		return TemporalPair{}, false
	}

	// 分母取两者中较小的活跃小时数：一个每天挂 20 小时的画像与一个每天只用
	// 2 小时的画像，若那 2 小时全部落在对方的空档里，用大的做分母会算出一个
	// 极小的比值，看起来像「完全不重叠」，其实是「小的那个完全被包住」。
	minHours := a.ActiveHours
	if b.ActiveHours < minHours {
		minHours = b.ActiveHours
	}
	if minHours == 0 {
		return TemporalPair{}, false
	}
	ratio := float64(overlap) / float64(minHours)
	if ratio > temporalMaxOverlapRatio {
		return TemporalPair{}, false
	}

	return TemporalPair{
		KeyA: a.Key, KeyB: b.Key, Distance: dist,
		SharedDays: shared,
		AHours:     a.ActiveHours, BHours: b.ActiveHours,
		OverlapHours: overlap, OverlapRatio: ratio,
		Persistent: shared >= temporalPersistentMinSharedDays && ratio <= temporalPersistentMaxOverlap,
	}, true
}

func popcount24(v uint32) int {
	n := 0
	for v != 0 {
		v &= v - 1
		n++
	}
	return n
}
