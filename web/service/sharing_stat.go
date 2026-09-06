package service

import (
	"sort"

	"a-ui/database/model"
)

// coexistDisplayMinHours 是并存标记的显示下限。
//
// 它只用来挡掉旅游迁移交界处那 1~2 小时的噪声（旧地区的设备还挂着、新
// 地区已经开始用），**不承担判断职责**：面板显示的是「并存 37 小时 / 3 省」
// 这样的事实，不做疑似分级。分级等于面板替管理员下判断，而阈值一旦调不好，
// 告警就退化成噪声或漏报。
const coexistDisplayMinHours = 3

// CoexistStat 是某入站在判定窗口内的并存统计。
type CoexistStat struct {
	// Hours 是并存小时数：某个 HourStart 下存在至少两条 Province 不同且
	// 都非空的记录，这一小时就计一次。
	Hours int `json:"hours"`
	// Provinces 是并存中出现过的省份，升序去重。
	Provinces []string `json:"provinces"`
	// ByIP 为 true 表示窗口内所有记录的 Province 都为空（IPv6 来源或归属地
	// 库未加载），判定降级为 IP 口径：Hours 变成「同一小时有 ≥2 个不同 IP」
	// 的小时数，IPs 是涉及的 IP 数。
	//
	// 降级口径误报率高得多——同一个人的手机和宽带就是两个 IP。界面必须
	// 明说当前是降级状态，别让管理员以为「3 IP 并存」是抓到了转卖。
	ByIP bool `json:"byIp"`
	IPs  int  `json:"ips"`
}

// Flagged 判断这份统计是否达到显示下限。
func (s CoexistStat) Flagged() bool { return s.Hours >= coexistDisplayMinHours }

// computeCoexist 从窗口内的行算出并存统计。
//
// 判定单位是「小时」而不是「是否出现过新省份」，这是整个功能的支点：
//   - 旅游是位置迁移——旧省停止活跃后新省才开始，两者落在不同小时，不并存
//   - 转卖是位置并存——两地长期各自活跃，持续落在同一批小时里
//
// 已知漏检：错峰共享（白天甲、晚上乙）落在不同小时，检测不到。抓它需要
// 「窗口内去重活跃 IP 数」这类指标，而正常用户 7 天十几个 IP、横跨 2~3 个省
// 是常态，误报率高到没法用。见设计文档 §9。
func computeCoexist(rows []model.InboundIPHour) CoexistStat {
	hasProvince := false
	for _, r := range rows {
		if r.Province != "" {
			hasProvince = true
			break
		}
	}

	byHourProvince := map[int64]map[string]bool{}
	byHourIP := map[int64]map[string]bool{}
	for _, r := range rows {
		if byHourIP[r.HourStart] == nil {
			byHourIP[r.HourStart] = map[string]bool{}
		}
		byHourIP[r.HourStart][r.IP] = true
		if r.Province == "" {
			continue
		}
		if byHourProvince[r.HourStart] == nil {
			byHourProvince[r.HourStart] = map[string]bool{}
		}
		byHourProvince[r.HourStart][r.Province] = true
	}

	stat := CoexistStat{ByIP: !hasProvince}
	group := byHourProvince
	if stat.ByIP {
		group = byHourIP
	}

	seen := map[string]bool{}
	for _, set := range group {
		if len(set) < 2 {
			continue
		}
		stat.Hours++
		for v := range set {
			seen[v] = true
		}
	}

	// 显式排序：上面遍历的是 map，顺序不定。不排的话同一份数据每次
	// 渲染出来的省份次序都不一样。
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	if stat.ByIP {
		stat.IPs = len(values)
	} else {
		stat.Provinces = values
	}
	return stat
}

// regionSuggestCoverage 是建议集合要覆盖的活跃时长占比。
//
// 不取 100%：一次出差、一次连错网络都会在列表里留下一个只占千分之几的
// 省份，全收进来等于建议「不限制」。95% 能盖住常住地与常去地，又能把
// 长尾切掉。
const regionSuggestCoverage = 0.95

// RegionSuggestion 是地区限制的建议值。
type RegionSuggestion struct {
	// Suggested 是按活跃时长降序累计、覆盖到 95% 的省份，升序输出。
	Suggested []string `json:"suggested"`

	// Coexisting 是 Suggested 里同时出现在并存记录中的省份。
	//
	// **刻意不从 Suggested 里剔除。** 面板分不清「买家的省」和「用户老家
	// 常挂的设备」，猜错两个方向都是错的：剔错了管理员采纳后把自己的用户
	// 挡在门外，不剔则可能把买家放行。标出来交给管理员判断是唯一诚实的
	// 做法——而且这一步若猜错完全静默：界面正常、xray 返回 Configuration
	// OK、面板显示 running，只是流量走对了不该走的人。
	Coexisting []string `json:"coexisting"`
}

// suggestRegions 从窗口内的行算出地区限制的建议值。
func suggestRegions(rows []model.InboundIPHour) RegionSuggestion {
	total := 0
	byProvince := map[string]int{}
	for _, r := range rows {
		// 归属地未知的行不参与建议：空串既不是合法省份，填进地区限制
		// 也没有任何意义。
		if r.Province == "" {
			continue
		}
		byProvince[r.Province] += r.ActiveSeconds
		total += r.ActiveSeconds
	}
	if total == 0 {
		return RegionSuggestion{}
	}

	type entry struct {
		province string
		seconds  int
	}
	list := make([]entry, 0, len(byProvince))
	for p, s := range byProvince {
		list = append(list, entry{p, s})
	}
	// 时长降序；时长相同按省份名升序。第二级排序不是可省的——遍历 map
	// 的顺序不定，没有它时两个时长相同的省份谁进 95% 会随机变化。
	sort.Slice(list, func(i, j int) bool {
		if list[i].seconds != list[j].seconds {
			return list[i].seconds > list[j].seconds
		}
		return list[i].province < list[j].province
	})

	acc := 0
	picked := make([]string, 0, len(list))
	for _, e := range list {
		picked = append(picked, e.province)
		acc += e.seconds
		if float64(acc) >= regionSuggestCoverage*float64(total) {
			break
		}
	}
	sort.Strings(picked)

	inCoexist := map[string]bool{}
	for _, p := range computeCoexist(rows).Provinces {
		inCoexist[p] = true
	}
	flagged := make([]string, 0, len(picked))
	for _, p := range picked {
		if inCoexist[p] {
			flagged = append(flagged, p)
		}
	}
	return RegionSuggestion{Suggested: picked, Coexisting: flagged}
}
