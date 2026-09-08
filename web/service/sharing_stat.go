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

// coexistMinActiveBytes 是「实质使用」门槛：一条记录要参与并存判定，它在
// 这一小时里的上下行字节之和必须达到这个值。
//
// 为什么判据是字节而不是活跃时长：生产实测过一例——某入站显示「并存 15
// 小时 / 2 省」，第二个省那个 IP 每小时只累计 60~180 秒，而**访问日志里
// 一条记录都没有**。被路由规则拦下的请求会留下 route=a-ui-block 的记录，
// 它连那个都没有，说明它从未发出过一个可路由的请求，只是在反复建连、做
// TLS 握手。sharingObservable 已经挡掉了「完全没有字节往来」的连接，但
// 握手本身就产生字节，所以时长维度分不出这种连接与真实使用。
//
// 1 MB 是量级判断而非实测标定：一次 TLS + ws 握手是几 KB，每小时重连几次
// 是几十 KB 量级；真在用代理的一小时轻松几十 MB。留了一个数量级的余量。
// 明细页会显示每行的字节数，跑一段时间后可据真实分布调整。
const coexistMinActiveBytes = 1 << 20

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

// hasActiveBytes 判断这批行里有没有字节量，即并存判定该不该走字节门槛。
//
// 单独抽出来是因为它必须**对整批数据算一次**，不能逐小时各算各的：明细表
// 回溯 30 天，逐小时算会让升级前的小时按旧口径判、升级后的按新口径判，同一
// 张表里两种判据混用，管理员无从判断自己在看什么。这与 CoexistStat.ByIP
// 那条「一旦有任何一条记录带上省份就整体以省份口径为准」是同一条原则。
func hasActiveBytes(rows []model.InboundIPHour) bool {
	for _, r := range rows {
		if r.ActiveBytes > 0 {
			return true
		}
	}
	return false
}

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
	return computeCoexistGated(rows, hasActiveBytes(rows))
}

// computeCoexistGated 是 computeCoexist 的显式判据版本，供需要对多批数据
// 沿用同一判据的调用方使用（SharingService.Detail 的逐小时筛选）。
func computeCoexistGated(rows []model.InboundIPHour, byteGate bool) CoexistStat {
	// 没有数据不等于「归属地库未加载」。不挡住的话空输入会因为 hasProvince
	// 为 false 而被报成降级口径，界面上显示一条与事实无关的告警。
	if len(rows) == 0 {
		return CoexistStat{}
	}

	// byteGate 为真时按字节门槛判，为假时回落旧的「只看有没有活跃时长」口径。
	//
	// 判据是「整批数据里有没有字节量」而不是逐行判断 ActiveBytes 是否为 0：
	// 逐行判会有歧义——一条恰好落在采样边界、本小时字节增量为 0 的新行，与
	// 升级前写入的老行长得一模一样。
	//
	// 代价是升级后老行立刻不再参与判定，橙标会清零，7 天窗口滚过去才重新
	// 长出来；而 Summary 只返回 Flagged() 的入站，所以橙标消失后弹窗入口也
	// 跟着消失，那段历史在界面上暂时看不到（库里的行不删，保留期照旧）。
	// 这是刻意的：老行无法证明有实质流量，报不出来好过报一个不可信的数字
	// ——这个功能是告警，误报冤枉用户比漏报严重。
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
		// 省份口径与 IP 降级口径都过这道门槛。降级口径的误报率本来就更高
		// （同一个人的手机与宽带就是两个 IP），放宽只会让它更不可信。
		if byteGate && r.ActiveBytes < coexistMinActiveBytes {
			continue
		}
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
		// 返回非 nil 的空切片而不是零值 RegionSuggestion{}：零值的字段是
		// nil 切片，序列化成 JSON 的 null，而前端对 suggestion.suggested
		// 取 .length 会抛 TypeError。这条路径的触发条件（窗口内所有行的
		// Province 都为空）恰好就是 CoexistStat.ByIP 为真的定义，也就是
		// 说降级形态下点开明细必然踩中。
		return RegionSuggestion{Suggested: []string{}, Coexisting: []string{}}
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
