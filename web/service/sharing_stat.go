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
