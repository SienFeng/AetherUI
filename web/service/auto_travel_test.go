package service

import (
	"testing"
	"time"

	"a-ui/database/model"
)

const atMB = 1 << 20

var atLoc = time.FixedZone("CST", 8*3600)

// atNow 是 2026-11-20 10:30（面板时区），月中，便于构造当月记录。
var atNow = time.Date(2026, 11, 20, 10, 30, 0, 0, atLoc)

func atHour(offset time.Duration) int64 {
	t := atNow.Add(offset).Unix()
	return t - t%3600
}

// stableHomeRows 是近 7 天里 4 个不同日子的家宽实质流量，最晚一条在 5 天前。
func stableHomeRows() []model.InboundIPHour {
	var rows []model.InboundIPHour
	for _, d := range []int{5, 6, 7, 8} {
		rows = append(rows, model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(-time.Duration(d) * 24 * time.Hour), ActiveBytes: 5 * atMB})
	}
	return rows
}

func baseInput() autoTravelInput {
	return autoTravelInput{
		Now: atNow, Loc: atLoc, Params: defaultAutoTravelParams(),
		HomeRows: stableHomeRows(),
		Blocked:  []autoTravelCandidate{{IP: "198.51.100.7", Province: "上海市"}},
	}
}

func autoPass(granted, until, ended time.Time, reason string) model.InboundRegionPass {
	p := model.InboundRegionPass{Auto: true, Regions: `["上海市"]`, GrantedAt: granted.UnixMilli(), Until: until.UnixMilli(), EndReason: reason}
	if !ended.IsZero() {
		p.EndedAt = ended.UnixMilli()
	}
	return p
}

func TestAutoTravelGrantsWhenHomeSilent(t *testing.T) {
	d := decideAutoTravel(baseInput())
	if d.Action != autoTravelGranted || d.Province != "上海市" {
		t.Fatalf("decision = %+v", d)
	}
	if d.Until != atNow.Add(defaultAutoTravelParams().PassDuration).UnixMilli() {
		t.Errorf("Until = %d", d.Until)
	}
}

func TestAutoTravelDeniesWhenHomeRecentlyActive(t *testing.T) {
	in := baseInput()
	in.HomeRows = append(in.HomeRows, model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(-time.Hour), ActiveBytes: 2 * atMB})
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("家里 1 小时前还在用，不该放行: %+v", d)
	}
}

func TestAutoTravelDeniesWhenHomeLiveActive(t *testing.T) {
	in := baseInput()
	in.HomeLiveActive = true
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("家里此刻在线活跃，不该放行: %+v", d)
	}
}

func TestAutoTravelDeniesWithoutStableHome(t *testing.T) {
	in := baseInput()
	in.HomeRows = stableHomeRows()[:2]
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("只有 2 天的家庭数据，不该放行: %+v", d)
	}
}

func TestAutoTravelDeniesHighRisk(t *testing.T) {
	for _, level := range []string{RiskLevelHigh, RiskLevelSevere} {
		in := baseInput()
		in.RiskLevel = level
		if d := decideAutoTravel(in); d.Action != autoTravelDenied {
			t.Errorf("风险 %s 时不该放行: %+v", level, d)
		}
	}
}

func TestAutoTravelDeniesInCooldown(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-3 * 24 * time.Hour)
	in.History = []model.InboundRegionPass{autoPass(g, g.Add(72*time.Hour), g.Add(time.Hour), model.RegionPassCoexist)}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("并存结束后 7 天内不该再放行: %+v", d)
	}
}

func TestAutoTravelDeniesWeeklyCap(t *testing.T) {
	in := baseInput()
	for _, days := range []int{1, 3, 5} {
		g := atNow.Add(-time.Duration(days) * 24 * time.Hour)
		in.History = append(in.History, autoPass(g, g.Add(72*time.Hour), g.Add(2*time.Hour), model.RegionPassHome))
	}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("一周内已自动放行 3 次，不该再放行: %+v", d)
	}
}

func TestAutoTravelDeniesMonthlyCap(t *testing.T) {
	in := baseInput()
	g := time.Date(2026, 11, 2, 0, 0, 0, 0, atLoc)
	// 11-02 到 11-12 生效 10 天，早于一周窗口，只触发月度上限。
	in.History = []model.InboundRegionPass{autoPass(g, g.Add(240*time.Hour), g.Add(240*time.Hour), model.RegionPassExpired)}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("当月已自动放行 10 天，不该再放行: %+v", d)
	}
}

func TestAutoTravelEndsCoexist(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-5 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	in.Active = &p
	in.HomeLiveActive, in.PassLiveActive = true, true
	d := decideAutoTravel(in)
	if d.Action != autoTravelEnded || d.EndReason != model.RegionPassCoexist {
		t.Fatalf("家里与外地同时在用应以 coexist 结束: %+v", d)
	}
}

func TestAutoTravelEndsHome(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-5 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	in.Active = &p
	in.HomeRows = append(in.HomeRows, model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(0), ActiveBytes: 2 * atMB})
	d := decideAutoTravel(in)
	if d.Action != autoTravelEnded || d.EndReason != model.RegionPassHome {
		t.Fatalf("家里恢复使用应以 home 结束: %+v", d)
	}
}

func TestAutoTravelRenews(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-60 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "") // 剩 12 小时
	in.Active = &p
	in.History = []model.InboundRegionPass{p}
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-2 * time.Hour), ActiveBytes: 3 * atMB}}
	d := decideAutoTravel(in)
	if d.Action != autoTravelRenewed || d.Until != atNow.Add(defaultAutoTravelParams().PassDuration).UnixMilli() {
		t.Fatalf("decision = %+v", d)
	}
}

func TestAutoTravelIgnoresManualPass(t *testing.T) {
	in := baseInput()
	p := model.InboundRegionPass{Regions: `["*"]`, GrantedAt: atNow.Add(-time.Hour).UnixMilli(), Until: atNow.Add(time.Hour).UnixMilli()}
	in.Active = &p
	in.HomeLiveActive, in.PassLiveActive = true, true
	if d := decideAutoTravel(in); d.Action != autoTravelIdle {
		t.Fatalf("手动通行证不归自动放行管: %+v", d)
	}
}

// 管理员手动收回过自动通行证，说明他不认可这次放行；一分钟后又自动发回去是在跟管理员对着干。
func TestAutoTravelDeniesAfterAdminRevokedAutoPass(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-30 * time.Minute)
	in.History = []model.InboundRegionPass{autoPass(g, g.Add(72*time.Hour), g.Add(10*time.Minute), model.RegionPassRevoked)}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("管理员收回自动通行证后 7 天内不该再自动放行: %+v", d)
	}
}

// 续期要求通行证省份里有实质流量（≥1MB），与其他判断同一门槛，握手级的流量不算。
func TestAutoTravelRenewRequiresSubstantialTraffic(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-60 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	in.Active = &p
	in.History = []model.InboundRegionPass{p}
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-2 * time.Hour), ActiveBytes: 200 << 10}}
	if d := decideAutoTravel(in); d.Action == autoTravelRenewed {
		t.Fatalf("外地只有 200KB 不该续期: %+v", d)
	}
}

// 阈值来自面板设置：调短静默时长、调短通行证、放宽月上限都要立刻体现在判定里。
func TestAutoTravelUsesConfiguredParams(t *testing.T) {
	in := baseInput()
	// 家里 2 小时前那一小时还在用：默认（静默 2 小时）不放行，静默改成 1 小时就放行。
	in.HomeRows = append(in.HomeRows, model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(-2 * time.Hour), ActiveBytes: 2 * atMB})
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("默认静默 2 小时，不该放行: %+v", d)
	}
	in.Params.HomeSilence = time.Hour
	in.Params.PassDuration = 24 * time.Hour
	d := decideAutoTravel(in)
	if d.Action != autoTravelGranted || d.Until != atNow.Add(24*time.Hour).UnixMilli() {
		t.Fatalf("静默 1 小时、通行证 24 小时: %+v", d)
	}

	in = baseInput()
	g := time.Date(2026, 11, 2, 0, 0, 0, 0, atLoc)
	in.History = []model.InboundRegionPass{autoPass(g, g.Add(240*time.Hour), g.Add(240*time.Hour), model.RegionPassExpired)}
	in.Params.MonthlyDays = 15
	if d := decideAutoTravel(in); d.Action != autoTravelGranted {
		t.Fatalf("月上限调到 15 天，已用 10 天应当放行: %+v", d)
	}
}
