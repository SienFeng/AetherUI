package service

import (
	"reflect"
	"testing"

	"a-ui/database/model"
)

// hourRow 造一条小时桶记录。Task 3 的测试也用它。
func hourRow(inboundId int, hour int64, ip, province string, seconds int) model.InboundIPHour {
	return model.InboundIPHour{
		InboundId: inboundId, HourStart: hour, IP: ip,
		Province: province, ActiveSeconds: seconds,
	}
}

// 整份设计的支点：并存判的是「同一小时内两个省份同时活跃」，不是
// 「出现过新省份」。旅游是位置迁移（旧的停了新的才开始），转卖是位置
// 并存（两地长期各自活跃）——只有按并存判，旅游才不会被误报。
func TestCoexistCountsOnlySimultaneousProvinces(t *testing.T) {
	const h = 3600
	cases := []struct {
		name  string
		rows  []model.InboundIPHour
		hours int
	}{
		{
			name: "转卖：两省长期并存",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 0*h, "2.2.2.2", "上海", 3600),
				hourRow(1, 1*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 1*h, "2.2.2.2", "上海", 3600),
			},
			hours: 2,
		},
		{
			name: "旅游：位置迁移，旧省停了新省才开始",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 1*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 2*h, "2.2.2.2", "上海", 3600),
				hourRow(1, 3*h, "2.2.2.2", "上海", 3600),
			},
			hours: 0,
		},
		{
			name: "错峰使用：不同小时，已知漏检（设计文档 §9）",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 1*h, "2.2.2.2", "上海", 3600),
			},
			hours: 0,
		},
		{
			name: "同一个人的手机加宽带：同省多 IP",
			rows: []model.InboundIPHour{
				hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
				hourRow(1, 0*h, "2.2.2.2", "江苏", 3600),
			},
			hours: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeCoexist(c.rows).Hours; got != c.hours {
				t.Errorf("Hours = %v, want %v", got, c.hours)
			}
		})
	}
}

func TestCoexistReportsProvincesSorted(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "2.2.2.2", "江苏", 3600),
		hourRow(1, 0*h, "1.1.1.1", "上海", 3600),
	}
	got := computeCoexist(rows).Provinces
	want := []string{"上海", "江苏"} // 升序，与输入顺序无关
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Provinces = %v, want %v", got, want)
	}
}

// 全部归属地未知（IPv6 来源，或归属地库未加载）时降级为 IP 口径。
func TestCoexistFallsBackToIPWhenNoProvinceKnown(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "", 3600),
		hourRow(1, 0*h, "2.2.2.2", "", 3600),
	}
	stat := computeCoexist(rows)
	if !stat.ByIP {
		t.Fatal("全部省份为空时应降级为 IP 口径")
	}
	if stat.Hours != 1 || stat.IPs != 2 {
		t.Errorf("Hours=%v IPs=%v, want 1/2", stat.Hours, stat.IPs)
	}
}

// 两种口径绝不混用：只要有一条记录带上了省份，就以省份口径为准。混用会让
// 同一个数字时而是省、时而是 IP，管理员无从判断自己在看什么。
func TestCoexistPrefersProvinceWheneverAnyIsKnown(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
		hourRow(1, 0*h, "2.2.2.2", "", 3600),
	}
	stat := computeCoexist(rows)
	if stat.ByIP {
		t.Fatal("有已知省份时不应降级为 IP 口径")
	}
	if stat.Hours != 0 {
		t.Errorf("Hours = %v, want 0（只有一个已知省份，不构成并存）", stat.Hours)
	}
}

func TestCoexistFlaggedNeedsDisplayMinimum(t *testing.T) {
	if (CoexistStat{Hours: coexistDisplayMinHours - 1}).Flagged() {
		t.Error("低于显示下限不该打标")
	}
	if !(CoexistStat{Hours: coexistDisplayMinHours}).Flagged() {
		t.Error("达到显示下限就该打标")
	}
}

func TestSuggestRegionsCoversNinetyFivePercent(t *testing.T) {
	const h = 3600
	// 江苏 900 秒（90%）、上海 80（8%）、浙江 20（2%）。
	// 90 不够 95，加上海到 98 够了，浙江这条长尾被切掉——一次出差、
	// 一次连错网络不该让建议退化成「几乎不限制」。
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 900),
		hourRow(1, 1*h, "2.2.2.2", "上海", 80),
		hourRow(1, 2*h, "3.3.3.3", "浙江", 20),
	}
	got := suggestRegions(rows).Suggested
	want := []string{"上海", "江苏"} // 升序；UTF-8 下 "上" < "江"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Suggested = %v, want %v", got, want)
	}
}

// 若该入站正在被共享，建议值会把买家的省份一并算进去。**不自动剔除**，
// 只标出来：面板分不清「买家的省」和「用户老家常挂的设备」，猜错两个
// 方向都是错的。直接采纳等于把转卖合法化，而这一步完全静默——界面正常、
// 配置正常生成，只是流量走对了买家。
func TestSuggestRegionsFlagsCoexistingWithoutRemoving(t *testing.T) {
	const h = 3600
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 1800),
		hourRow(1, 0*h, "2.2.2.2", "上海", 1800),
	}
	got := suggestRegions(rows)
	want := []string{"上海", "江苏"}
	if !reflect.DeepEqual(got.Suggested, want) {
		t.Errorf("Suggested = %v, want %v（不得自动剔除并存省份）", got.Suggested, want)
	}
	if !reflect.DeepEqual(got.Coexisting, want) {
		t.Errorf("Coexisting = %v, want %v", got.Coexisting, want)
	}
}

func TestSuggestRegionsIgnoresUnknownProvince(t *testing.T) {
	const h = 3600
	// 归属地未知的行不参与建议：拿空串当省份填进地区限制会让整份配置
	// 生成失败（EncodeRegionsStrict 会拒），或者更糟——静默变成不限制。
	rows := []model.InboundIPHour{
		hourRow(1, 0*h, "1.1.1.1", "江苏", 3600),
		hourRow(1, 0*h, "2.2.2.2", "", 3600),
	}
	got := suggestRegions(rows).Suggested
	want := []string{"江苏"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Suggested = %v, want %v", got, want)
	}
}

func TestSuggestRegionsEmptyWhenNoProvinceAtAll(t *testing.T) {
	rows := []model.InboundIPHour{hourRow(1, 0, "1.1.1.1", "", 3600)}
	got := suggestRegions(rows)
	if len(got.Suggested) != 0 || len(got.Coexisting) != 0 {
		t.Errorf("全未知时应给空建议, got %+v", got)
	}
}
