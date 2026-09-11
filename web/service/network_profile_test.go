package service

import (
	"testing"
	"time"

	"a-ui/database/model"
)

// profRow 造一行观测。bytes 要自己给——1 MB 门槛是这整套判定的支点，
// 让它在每个用例里显式出现，比藏在默认值里好。
func profRow(inboundId int, hourStart int64, ip string, m NetworkMeta, bytes int64) model.InboundIPHour {
	return model.InboundIPHour{
		InboundId: inboundId, HourStart: hourStart, IP: ip,
		Country: m.Country, Province: m.Province, City: m.City, ISP: m.ISP,
		IdentityVersion: sharingIdentityVersion,
		ActiveSeconds:   1800, ActiveBytes: bytes,
		ActiveUp: bytes / 4, ActiveDown: bytes - bytes/4,
	}
}

func rowsOf(list ...model.InboundIPHour) []model.InboundIPHour { return list }

func cn(province, isp string) NetworkMeta {
	return NetworkMeta{Country: "中国", Province: province, ISP: isp}
}

// 同省同 ISP 换 IP、换 /24 都不该拆成两个画像——动态家宽换段是常态，
// 拆开就是把一个人数成两个。
func TestBuildProfilesKeepsSameProvinceAndISPTogether(t *testing.T) {
	const h = 3600
	rows := rowsOf(
		profRow(1, 0*h, "49.86.1.4", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 1*h, "49.86.22.9", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 2*h, "49.87.14.5", cn("江苏省", "中国电信"), 5<<20),
	)
	got := buildProfiles(rows, time.UTC)
	if len(got) != 1 {
		t.Fatalf("画像数 = %v, want 1；得到 %+v", len(got), keysOf(got))
	}
	if got[0].ActiveHours != 3 {
		t.Errorf("ActiveHours = %v, want 3", got[0].ActiveHours)
	}
	if len(got[0].Prefixes) != 3 {
		t.Errorf("网络族数 = %v, want 3（换段仍记录，只是不拆画像）", len(got[0].Prefixes))
	}
}

// 同一小时里同一个画像下有多个 IP 时只算一个活跃小时。按行数算会让换 IP
// 频繁的用户凭空多出几倍活跃小时，而门槛判定正是按小时数设的。
func TestBuildProfilesCountsDistinctHours(t *testing.T) {
	const h = 3600
	rows := rowsOf(
		profRow(1, 0*h, "49.86.1.4", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 0*h, "49.86.1.9", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 0*h, "49.86.2.1", cn("江苏省", "中国电信"), 5<<20),
	)
	got := buildProfiles(rows, time.UTC)
	if len(got) != 1 || got[0].ActiveHours != 1 {
		t.Fatalf("ActiveHours = %+v, want 单个画像 1 小时", got)
	}
}

func TestBuildProfilesSplitsByProvinceAndISP(t *testing.T) {
	const h = 3600
	cases := []struct {
		name string
		a, b NetworkMeta
		want int
	}{
		{"同省不同 ISP → 两个画像", cn("江苏省", "中国电信"), cn("江苏省", "中国移动"), 2},
		{"不同省同 ISP → 两个画像", cn("江苏省", "中国电信"), cn("广东省", "中国电信"), 2},
		{"完全相同 → 一个画像", cn("江苏省", "中国电信"), cn("江苏省", "中国电信"), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := rowsOf(
				profRow(1, 0*h, "1.1.1.1", c.a, 5<<20),
				profRow(1, 1*h, "2.2.2.2", c.b, 5<<20),
			)
			if got := buildProfiles(rows, time.UTC); len(got) != c.want {
				t.Errorf("画像数 = %v, want %v；得到 %v", len(got), c.want, keysOf(got))
			}
		})
	}
}

// 城市不进画像键。运营商 IP 定位到市的稳定性远不如到省，把南通/南京/苏州
// 判成三个用户是纯粹的误报来源。
func TestBuildProfilesIgnoresCityInKey(t *testing.T) {
	const h = 3600
	a := NetworkMeta{Country: "中国", Province: "江苏省", City: "南通市", ISP: "中国电信"}
	b := NetworkMeta{Country: "中国", Province: "江苏省", City: "南京市", ISP: "中国电信"}
	rows := rowsOf(
		profRow(1, 0*h, "1.1.1.1", a, 5<<20),
		profRow(1, 1*h, "2.2.2.2", b, 5<<20),
	)
	got := buildProfiles(rows, time.UTC)
	if len(got) != 1 {
		t.Fatalf("画像数 = %v, want 1；城市不该进键", len(got))
	}
	if len(got[0].Cities) != 2 {
		t.Errorf("Cities = %v, want 两个（作为展示子特征保留）", got[0].Cities)
	}
}

func TestBuildProfilesMarksHostingAndForeignForms(t *testing.T) {
	const h = 3600
	t.Run("境外 IDC", func(t *testing.T) {
		m := NetworkMeta{Country: "美国", ISP: "Amazon"}
		got := buildProfiles(rowsOf(profRow(1, 0*h, "3.3.3.3", m, 5<<20)), time.UTC)
		if len(got) != 1 || !got[0].Hosting {
			t.Fatalf("= %+v, want 单个 Hosting 画像", got)
		}
		if got[0].Key != "美国|IDC|Amazon" {
			t.Errorf("Key = %q", got[0].Key)
		}
	})

	t.Run("境外普通网络退到网络族", func(t *testing.T) {
		m := NetworkMeta{Country: "日本"}
		got := buildProfiles(rowsOf(profRow(1, 0*h, "3.3.3.3", m, 5<<20)), time.UTC)
		if len(got) != 1 || got[0].Hosting {
			t.Fatalf("= %+v, want 单个非 Hosting 画像", got)
		}
		if got[0].Key != "日本|prefix:3.3.3.0/24" {
			t.Errorf("Key = %q", got[0].Key)
		}
	})

	t.Run("中国云厂商同样标 Hosting，但键仍是省+ISP 形态", func(t *testing.T) {
		m := cn("浙江省", "阿里云")
		got := buildProfiles(rowsOf(profRow(1, 0*h, "47.1.2.3", m, 5<<20)), time.UTC)
		if len(got) != 1 || !got[0].Hosting {
			t.Fatalf("= %+v, want Hosting", got)
		}
		if got[0].Key != "CN|浙江省|阿里云" {
			t.Errorf("Key = %q", got[0].Key)
		}
	})

	t.Run("完全没有地理信息时退到纯网络族", func(t *testing.T) {
		got := buildProfiles(rowsOf(
			profRow(1, 0*h, "240e:3b1:2:abcd::1", NetworkMeta{}, 5<<20),
			profRow(1, 1*h, "240e:3b1:2:abcd:dead::9", NetworkMeta{}, 5<<20),
		), time.UTC)
		if len(got) != 1 {
			t.Fatalf("画像数 = %v, want 1（同一个 /64 的临时地址）；得到 %v", len(got), keysOf(got))
		}
		if got[0].Key != "prefix:240e:3b1:2:abcd::/64" {
			t.Errorf("Key = %q", got[0].Key)
		}
	})
}

// 不过 1 MB 门槛的行完全不参与画像。判据必须与并存判定一致，不另搞一套——
// 两套门槛会让管理员看到「共享检测：无异常 / 风险评分：高风险」而无从解释。
func TestBuildProfilesAppliesTheSameByteGateAsCoexist(t *testing.T) {
	const h = 3600
	rows := rowsOf(
		profRow(1, 0*h, "1.1.1.1", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, 1*h, "2.2.2.2", cn("广东省", "中国移动"), coexistMinActiveBytes-1),
	)
	got := buildProfiles(rows, time.UTC)
	if len(got) != 1 || got[0].Province != "江苏省" {
		t.Fatalf("= %v, want 只剩江苏那个画像", keysOf(got))
	}
	if got[0].TrafficShare != 1 {
		t.Errorf("TrafficShare = %v, want 1（被门槛挡掉的行也不进分母）", got[0].TrafficShare)
	}
}

// 输出顺序必须确定，否则同一份数据每次渲染的次序都不一样。
func TestBuildProfilesOutputIsDeterministic(t *testing.T) {
	const h = 3600
	rows := rowsOf(
		profRow(1, 0*h, "1.1.1.1", cn("江苏省", "中国电信"), 3<<20),
		profRow(1, 1*h, "2.2.2.2", cn("广东省", "中国移动"), 9<<20),
		profRow(1, 2*h, "3.3.3.3", cn("上海市", "中国联通"), 6<<20),
	)
	first := buildProfiles(rows, time.UTC)
	for i := 0; i < 20; i++ {
		again := buildProfiles(rows, time.UTC)
		for j := range first {
			if first[j].Key != again[j].Key {
				t.Fatalf("第 %v 次调用顺序不同: %v vs %v", i, keysOf(first), keysOf(again))
			}
		}
	}
	if first[0].Province != "广东省" {
		t.Errorf("首位 = %q, want 流量最大的广东省", first[0].Province)
	}
}

// 活跃天数按面板时区切，不按 UTC。UTC+8 下按 UTC 切天会让每天前 8 小时
// 算进前一天，而这个数直接进稳定画像的门槛判定。
func TestBuildProfilesCountsDaysInPanelTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2026-09-05 20:00 与 23:00 UTC = 上海时间 9-06 凌晨 4 点与 7 点，同一天。
	base := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC).Unix()
	rows := rowsOf(
		profRow(1, base, "1.1.1.1", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, base+3*3600, "1.1.1.2", cn("江苏省", "中国电信"), 5<<20),
	)
	if got := buildProfiles(rows, shanghai); got[0].ActiveDays != 1 {
		t.Errorf("上海时区下 ActiveDays = %v, want 1", got[0].ActiveDays)
	}
	if got := buildProfiles(rows, time.UTC); got[0].ActiveDays != 1 {
		t.Errorf("UTC 下 ActiveDays = %v, want 1（同属 9-05）", got[0].ActiveDays)
	}
	// 换一对刻意跨上海日界的时刻：15:00 与 17:00 UTC = 23:00 与次日 01:00。
	base2 := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC).Unix()
	rows2 := rowsOf(
		profRow(1, base2, "1.1.1.1", cn("江苏省", "中国电信"), 5<<20),
		profRow(1, base2+2*3600, "1.1.1.2", cn("江苏省", "中国电信"), 5<<20),
	)
	if got := buildProfiles(rows2, shanghai); got[0].ActiveDays != 2 {
		t.Errorf("上海时区下跨日界 ActiveDays = %v, want 2", got[0].ActiveDays)
	}
	if got := buildProfiles(rows2, time.UTC); got[0].ActiveDays != 1 {
		t.Errorf("UTC 下同两个时刻 ActiveDays = %v, want 1——这正是必须按面板时区切的原因", got[0].ActiveDays)
	}
}

// 距离是本子系统唯一的新支点，五个取值各钉一条。
func TestProfileDistance(t *testing.T) {
	p := func(country, province, isp string) NetworkProfile {
		return NetworkProfile{Country: country, Province: province, ISP: isp}
	}
	cases := []struct {
		name string
		a, b NetworkProfile
		want int
	}{
		{"同省同 ISP", p("中国", "江苏省", "中国电信"), p("中国", "江苏省", "中国电信"), distanceSameISP},
		{"同省不同 ISP（家宽 + 手机）", p("中国", "江苏省", "中国电信"), p("中国", "江苏省", "中国移动"), distanceSameProvince},
		{"跨省", p("中国", "江苏省", "中国电信"), p("中国", "广东省", "中国移动"), distanceCrossProvince},
		{"跨国", p("中国", "江苏省", "中国电信"), p("美国", "", "Amazon"), distanceCrossCountry},
		{"同一个外国内部 → unknown", p("美国", "", "Amazon"), p("美国", "", "Vultr"), distanceUnknown},
		{"任一侧没有国家 → unknown", p("", "", ""), p("中国", "江苏省", "中国电信"), distanceUnknown},
		{"中国但任一侧没有省份 → unknown", p("中国", "", "中国电信"), p("中国", "江苏省", "中国电信"), distanceUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := profileDistance(c.a, c.b); got != c.want {
				t.Errorf("profileDistance = %v, want %v", got, c.want)
			}
			if got := profileDistance(c.b, c.a); got != c.want {
				t.Errorf("反向 profileDistance = %v, want %v（必须对称）", got, c.want)
			}
		})
	}
}

// 只有跨省、跨国才进计分信号。这一条守的是整个设计的支点：家宽 + 手机
// （距离 1）与真正的跨省共享（距离 2）必须分得开。
func TestOnlyCrossProvinceAndAboveAreScorable(t *testing.T) {
	if distanceSameProvince >= distanceScorable {
		t.Error("同省不同 ISP 进了计分线——家宽 + 手机会与真实共享拿到相同分数")
	}
	if distanceCrossProvince < distanceScorable || distanceCrossCountry < distanceScorable {
		t.Error("跨省 / 跨国必须在计分线之上")
	}
	if distanceUnknown >= distanceScorable {
		t.Error("unknown 绝不能进计分线——那是拿一个猜出来的距离去加分")
	}
}

func keysOf(ps []NetworkProfile) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Key)
	}
	return out
}
