package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database/model"
)

// temporalBase 是所有错峰用例的起点：UTC 某天 00:00。用 UTC 跑判定，
// 位图的「第几个小时」就等于这里写的 hour，用例读起来才直观。
var temporalBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// usage 造出「某画像在第 day 天的 hours 这几个小时里各用了 bytes」。
func usage(m NetworkMeta, ip string, day int, hours []int, bytes int64) []model.InboundIPHour {
	out := make([]model.InboundIPHour, 0, len(hours))
	for _, h := range hours {
		ts := temporalBase.AddDate(0, 0, day).Add(time.Duration(h) * time.Hour).Unix()
		out = append(out, profRow(1, ts, ip, m, bytes))
	}
	return out
}

// spread 把某画像在 days 天里每天都按同一组小时活跃。
func spread(m NetworkMeta, ip string, days int, hours []int, bytes int64) []model.InboundIPHour {
	var out []model.InboundIPHour
	for d := 0; d < days; d++ {
		out = append(out, usage(m, ip, d, hours, bytes)...)
	}
	return out
}

func analyzeTemporal(rows []model.InboundIPHour) []TemporalPair {
	profiles := buildProfiles(rows, time.UTC)
	return computeTemporal(rows, profiles, time.UTC)
}

var (
	daytime = []int{8, 9, 10, 11, 12, 13, 14, 15, 16}
	night   = []int{18, 19, 20, 21, 22, 23, 0, 1}
)

// 跨省 + 长期 + 几乎不重叠：这正是第一期明确漏检的那个形态。
func TestTemporalDetectsCrossProvinceOffPeak(t *testing.T) {
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, daytime, 5<<20),
		spread(cn("广东省", "中国移动"), "120.86.1.1", 7, night, 5<<20)...,
	)
	pairs := analyzeTemporal(rows)
	if len(pairs) != 1 {
		t.Fatalf("命中 %v 对, want 1：%+v", len(pairs), pairs)
	}
	p := pairs[0]
	if p.Distance != distanceCrossProvince {
		t.Errorf("Distance = %v, want 跨省", p.Distance)
	}
	if p.OverlapHours != 0 || p.OverlapRatio != 0 {
		t.Errorf("Overlap = %v 小时 / %v, want 0", p.OverlapHours, p.OverlapRatio)
	}
	if !p.Persistent {
		t.Error("7 天全不重叠应命中更强的那一档")
	}
}

// **整个设计的支点。** 家宽 + 手机（同省不同 ISP，距离 1）天然错峰，而且
// 三道稳定性门槛全部轻松满足。若它能进错峰信号，拿到的分数会与真正的跨省
// 共享完全相同——区分度为零，而且不是调权重能解决的。
func TestTemporalIgnoresSameProvinceDifferentISP(t *testing.T) {
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, night, 5<<20),
		spread(cn("江苏省", "中国移动"), "117.136.1.1", 7, daytime, 5<<20)...,
	)
	if pairs := analyzeTemporal(rows); len(pairs) != 0 {
		t.Fatalf("同省不同 ISP 命中了错峰: %+v", pairs)
	}
}

// 旅游 / 出差是「旧省停、新省起」：两个画像各自活跃多天，但**共同活跃**
// 的天数只有交界那一两天。SharedDays 门槛专门挡这个。
func TestTemporalIgnoresTravelMigration(t *testing.T) {
	var rows []model.InboundIPHour
	// 第 0~4 天在江苏，第 4~8 天在广东，只有第 4 天重叠。
	for d := 0; d <= 4; d++ {
		rows = append(rows, usage(cn("江苏省", "中国电信"), "49.86.1.1", d, daytime, 5<<20)...)
	}
	for d := 4; d <= 8; d++ {
		rows = append(rows, usage(cn("广东省", "中国移动"), "120.86.1.1", d, night, 5<<20)...)
	}
	pairs := analyzeTemporal(rows)
	if len(pairs) != 0 {
		t.Fatalf("迁移场景命中了错峰（SharedDays 门槛失效）: %+v", pairs)
	}
}

// 重叠明显时不算错峰——那本来就该由并存信号去报。
func TestTemporalIgnoresHeavyOverlap(t *testing.T) {
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, daytime, 5<<20),
		spread(cn("广东省", "中国移动"), "120.86.1.1", 7, daytime, 5<<20)...,
	)
	if pairs := analyzeTemporal(rows); len(pairs) != 0 {
		t.Fatalf("完全重叠命中了错峰: %+v", pairs)
	}
}

// 第二个画像流量占比太低时不算：一次连错网络、一个挂着没用的设备，不该
// 被当成「另一个使用者」。
func TestTemporalIgnoresTinySecondProfile(t *testing.T) {
	rows := append(
		spread(cn("江苏省", "中国电信"), "49.86.1.1", 7, daytime, 100<<20),
		spread(cn("广东省", "中国移动"), "120.86.1.1", 7, night, 2<<20)...,
	)
	profiles := buildProfiles(rows, time.UTC)
	var second NetworkProfile
	for _, p := range profiles {
		if p.Province == "广东省" {
			second = p
		}
	}
	if second.TrafficShare >= temporalMinShare {
		t.Fatalf("用例没造对：第二画像占比 %v，本该低于门槛 %v", second.TrafficShare, temporalMinShare)
	}
	if pairs := computeTemporal(rows, profiles, time.UTC); len(pairs) != 0 {
		t.Fatalf("占比过低仍命中错峰: %+v", pairs)
	}
}

// 跨国同样算。旧的 computeCoexist 只认省份，境外来源会整体退化成降级口径；
// 画像层有 Country，跨国错峰必须能测出来。
func TestTemporalDetectsCrossCountryOffPeak(t *testing.T) {
	rows := append(
		spread(NetworkMeta{Country: "美国", ISP: "Amazon"}, "3.3.3.3", 7, daytime, 5<<20),
		spread(NetworkMeta{Country: "日本", ISP: "Google"}, "35.1.1.1", 7, night, 5<<20)...,
	)
	pairs := analyzeTemporal(rows)
	if len(pairs) != 1 || pairs[0].Distance != distanceCrossCountry {
		t.Fatalf("跨国错峰未命中: %+v", pairs)
	}
}

// 同一个外国内部（距离 unknown）不算。ipdb 对境外只保留国家，
// 「US Amazon + US Vultr」没有可用的地理维度，猜一个距离出来就是编造证据。
func TestTemporalIgnoresUnknownDistance(t *testing.T) {
	rows := append(
		spread(NetworkMeta{Country: "美国", ISP: "Amazon"}, "3.3.3.3", 7, daytime, 5<<20),
		spread(NetworkMeta{Country: "美国", ISP: "Vultr"}, "45.1.1.1", 7, night, 5<<20)...,
	)
	if pairs := analyzeTemporal(rows); len(pairs) != 0 {
		t.Fatalf("同国内部命中了错峰: %+v", pairs)
	}
}

// 重叠比例的分母取两者中较小的活跃小时数。用大的做分母时，一个「每天挂
// 20 小时」的画像与一个「每天只用 2 小时且全部落在对方空档」的画像会算出
// 极小的比值，看起来像完全不重叠，其实是小的那个被整个包住了。
func TestTemporalOverlapRatioUsesSmallerDenominator(t *testing.T) {
	big := cn("江苏省", "中国电信")
	small := cn("广东省", "中国移动")
	allDay := []int{}
	for h := 0; h < 20; h++ {
		allDay = append(allDay, h)
	}
	rows := append(
		spread(big, "49.86.1.1", 7, allDay, 5<<20),
		spread(small, "120.86.1.1", 7, []int{8, 9, 10, 11, 12, 13}, 20<<20)...,
	)
	pairs := analyzeTemporal(rows)
	if len(pairs) != 0 {
		t.Fatalf("小画像被大画像整个包住，重叠率应为 1，不该命中错峰: %+v", pairs)
	}
}

// 输出顺序必须确定。
func TestTemporalOutputIsDeterministic(t *testing.T) {
	var rows []model.InboundIPHour
	metas := []NetworkMeta{
		cn("江苏省", "中国电信"), cn("广东省", "中国移动"), cn("四川省", "中国联通"),
	}
	hourSets := [][]int{{8, 9, 10, 11, 12, 13}, {16, 17, 18, 19, 20, 21}, {0, 1, 2, 3, 4, 5}}
	for i, m := range metas {
		rows = append(rows, spread(m, fmt.Sprintf("10.%v.1.1", i), 7, hourSets[i], 5<<20)...)
	}
	first := analyzeTemporal(rows)
	if len(first) != 3 {
		t.Fatalf("命中 %v 对, want 3（三个画像两两互不重叠）", len(first))
	}
	for i := 0; i < 20; i++ {
		again := analyzeTemporal(rows)
		for j := range first {
			if first[j].KeyA != again[j].KeyA || first[j].KeyB != again[j].KeyB {
				t.Fatalf("第 %v 次调用顺序不同", i)
			}
		}
	}
}

// 位图按面板时区排小时。同一批数据在两个时区下「几点活跃」不同，
// 而错峰判的正是这个。
func TestTemporalUsesPanelTimezoneForHourOfDay(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	m := cn("江苏省", "中国电信")
	rows := usage(m, "49.86.1.1", 0, []int{0}, 5<<20)
	masks := buildDayMasks(rows, shanghai)
	key := profileKeyOf(profileMeta{Country: "中国", Province: "江苏省", ISP: "中国电信", Family: "49.86.1.0/24"})
	// UTC 9-01 00:00 = 上海 9-01 08:00。
	day, ok := masks[key]["2026-09-01"]
	if !ok {
		t.Fatalf("上海时区下没有 2026-09-01 这一天: %+v", masks)
	}
	if day != 1<<8 {
		t.Errorf("位图 = %b, want 第 8 位（上海时间 08:00）", day)
	}
}
