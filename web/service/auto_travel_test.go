package service

import (
	"strings"
	"testing"
	"time"

	"a-ui/database"
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

// 管理员收回的不管是不是自动发的通行证，都说明他此刻不想放行这个省：自动放行不能在下一分钟
// 原样发回去（改动前只对自动通行证生效，手动通行证收回后一分钟就被自动放行顶回来）。
func TestAutoTravelPausesAfterAdminRevokedManualPass(t *testing.T) {
	in := baseInput()
	revoked := model.InboundRegionPass{Regions: `["上海市"]`, GrantedAt: atNow.Add(-2 * time.Hour).UnixMilli(),
		Until: atNow.Add(time.Hour).UnixMilli(), EndedAt: atNow.Add(-time.Minute).UnixMilli(), EndReason: model.RegionPassRevoked}
	in.History = []model.InboundRegionPass{revoked}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("管理员刚收回过通行证应当暂停自动放行: %+v", d)
	}
}

// 每月天数是上限，不是「低于上限就再给一整张」：发放与续期都只给到本月剩下的额度。
func TestAutoTravelPassFitsMonthlyBudget(t *testing.T) {
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, atLoc)
	used := autoPass(monthStart, monthStart.Add(228*time.Hour), monthStart.Add(228*time.Hour), model.RegionPassExpired) // 已用 9.5 天
	in := baseInput()
	in.History = []model.InboundRegionPass{used}
	d := decideAutoTravel(in)
	if d.Action != autoTravelGranted || d.Until != atNow.Add(12*time.Hour).UnixMilli() {
		t.Fatalf("本月只剩半天，应当只放行 12 小时: %+v", d)
	}
	// 单张时长比整月上限还长：一张也不能超过上限。
	long := baseInput()
	long.Params.PassDuration = 720 * time.Hour
	if d := decideAutoTravel(long); d.Action != autoTravelGranted || d.Until != atNow.Add(10*24*time.Hour).UnixMilli() {
		t.Fatalf("一张通行证不能超过每月 10 天: %+v", d)
	}
	// 续期同理。
	renew := baseInput()
	g := atNow.Add(-60 * time.Hour)
	active := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	renew.Active = &active
	renew.History = []model.InboundRegionPass{used, active} // 9.5 天 + 生效中的 2.5 天 = 已满 12 天
	renew.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-2 * time.Hour), ActiveBytes: 3 * atMB}}
	if d := decideAutoTravel(renew); d.Action == autoTravelRenewed {
		t.Fatalf("本月已超上限不该再续期: %+v", d)
	}
}

// 「家里在用 / 外地在用」只认真的在用：刚连上、只传了几百字节的连接（端口扫描、主动探测）
// 不算，否则一次探测就能把出差中的人的通行证收回、还让自动放行暂停 7 天。
func TestLiveSignalsIgnoreProbes(t *testing.T) {
	home := newCIDRSet([]string{"192.0.2.0/24"})
	pass := newCIDRSet([]string{"198.51.100.0/24"})
	probe := OnlineIP{IP: "192.0.2.5", Conns: 1, Up: 200, Down: 100}
	idle := OnlineIP{IP: "192.0.2.6", Conns: 1, Up: 50 << 20, Idle: true}
	if h, p := liveSignals([]OnlineIP{probe, idle}, home, pass); h || p {
		t.Errorf("探测连接与闲置连接不算在用: home=%v pass=%v", h, p)
	}
	user := OnlineIP{IP: "192.0.2.7", Conns: 2, Up: 3 << 20, Down: 1 << 20}
	traveler := OnlineIP{IP: "198.51.100.9", Conns: 1, Down: 2 << 20}
	if h, p := liveSignals([]OnlineIP{user, traveler}, home, pass); !h || !p {
		t.Errorf("有实质流量的连接算在用: home=%v pass=%v", h, p)
	}
}

// 原因文字里的时长跟着设置走，不能写死成默认值。
func TestAutoTravelReasonsUseConfiguredDurations(t *testing.T) {
	in := baseInput()
	in.Params.HomeSilence = 3 * time.Hour
	if d := decideAutoTravel(in); !strings.Contains(d.Reason, "3 小时") {
		t.Errorf("放行原因应写设置的静默时长: %q", d.Reason)
	}
	end := baseInput()
	g := atNow.Add(-2 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	end.Active, end.History = &p, []model.InboundRegionPass{p}
	end.HomeLiveActive, end.PassLiveActive = true, true
	end.Params.Cooldown = 48 * time.Hour
	if d := decideAutoTravel(end); !strings.Contains(d.Reason, "2 天") {
		t.Errorf("暂停时长应写设置值: %q", d.Reason)
	}
	end.Params.Cooldown = 0
	if d := decideAutoTravel(end); strings.Contains(d.Reason, "暂停") {
		t.Errorf("不暂停时不该说暂停: %q", d.Reason)
	}
}

// 续期窗口不能比通行证本身还长：2 小时的通行证不该每分钟续一次。
func TestAutoTravelShortPassRenewsOnlyNearExpiry(t *testing.T) {
	in := baseInput()
	in.Params.PassDuration = 2 * time.Hour
	g := atNow.Add(-10 * time.Minute)
	p := autoPass(g, g.Add(2*time.Hour), time.Time{}, "") // 还剩 1 小时 50 分
	in.Active, in.History = &p, []model.InboundRegionPass{p}
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(0), ActiveBytes: 3 * atMB}}
	if d := decideAutoTravel(in); d.Action == autoTravelRenewed {
		t.Fatalf("刚发不久的短通行证不该续期: %+v", d)
	}
	g = atNow.Add(-80 * time.Minute)
	p = autoPass(g, g.Add(2*time.Hour), time.Time{}, "") // 还剩 40 分钟
	in.Active, in.History = &p, []model.InboundRegionPass{p}
	if d := decideAutoTravel(in); d.Action != autoTravelRenewed {
		t.Fatalf("快到期的短通行证应当续期: %+v", d)
	}
}

// 读不出共享风险时不能当成「没有风险」放行；风险升到高以上之后不再续期。
func TestAutoTravelRiskGateCoversUnknownAndRenewal(t *testing.T) {
	in := baseInput()
	in.RiskUnknown = true
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("风险读不出来时不该自动放行: %+v", d)
	}
	renew := baseInput()
	g := atNow.Add(-60 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	renew.Active, renew.History = &p, []model.InboundRegionPass{p}
	renew.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-2 * time.Hour), ActiveBytes: 3 * atMB}}
	renew.RiskLevel = RiskLevelHigh
	if d := decideAutoTravel(renew); d.Action == autoTravelRenewed {
		t.Fatalf("风险偏高时不该续期: %+v", d)
	}
}

func shortHomePass(province string, endedAgo time.Duration) model.InboundRegionPass {
	ended := atNow.Add(-endedAgo)
	granted := ended.Add(-7 * time.Hour)
	return model.InboundRegionPass{Auto: true, Regions: `["` + province + `"]`, GrantedAt: granted.UnixMilli(),
		Until: granted.Add(72 * time.Hour).UnixMilli(), EndedAt: ended.UnixMilli(), EndReason: model.RegionPassHome}
}

func TestAutoTravelDeniesProvinceBlockedWhileHomeActive(t *testing.T) {
	in := baseInput()
	in.SuspectProvinces = map[string]bool{"上海市": true}
	d := decideAutoTravel(in)
	if d.Action != autoTravelDenied || !strings.Contains(d.Reason, "家里正在用") {
		t.Fatalf("本人在用时这个省也来连过，不该自动放行: %+v", d)
	}
	// 另一个被拦来源的省份没有这种记录：照常放行那一个。
	in.Blocked = append(in.Blocked, autoTravelCandidate{IP: "203.0.113.5", Province: "浙江省"})
	if d := decideAutoTravel(in); d.Action != autoTravelGranted || d.Province != "浙江省" {
		t.Fatalf("应当放行没有嫌疑的那个省: %+v", d)
	}
}

func TestAutoTravelDeniesProvinceAfterShortHomeEndedPasses(t *testing.T) {
	in := baseInput()
	in.History = []model.InboundRegionPass{shortHomePass("上海市", 2*24*time.Hour), shortHomePass("上海市", 24*time.Hour)}
	d := decideAutoTravel(in)
	if d.Action != autoTravelDenied || !strings.Contains(d.Reason, "错开时间") {
		t.Fatalf("两张刚发就因家里恢复使用而收回的通行证，像错峰共享: %+v", d)
	}
	// 别的省的短命通行证不牵连上海市。
	in.History = []model.InboundRegionPass{shortHomePass("浙江省", 2*24*time.Hour), shortHomePass("浙江省", 24*time.Hour)}
	if d := decideAutoTravel(in); d.Action != autoTravelGranted {
		t.Fatalf("别的省的记录不该拦上海市: %+v", d)
	}
	// 只有一张：不算。
	in.History = []model.InboundRegionPass{shortHomePass("上海市", 24*time.Hour)}
	if d := decideAutoTravel(in); d.Action != autoTravelGranted {
		t.Fatalf("一次当天往返不该拦: %+v", d)
	}
}

func TestAutoTravelPausesThirtyDaysAfterRepeatedShortPasses(t *testing.T) {
	in := baseInput()
	// 都在 7 天冷却之外、30 天之内。
	in.History = []model.InboundRegionPass{shortHomePass("上海市", 20*24*time.Hour),
		shortHomePass("上海市", 15*24*time.Hour), shortHomePass("上海市", 10*24*time.Hour)}
	d := decideAutoTravel(in)
	if d.Action != autoTravelDenied || !strings.Contains(d.Reason, "30 天") {
		t.Fatalf("30 天内三张短命通行证应停 30 天: %+v", d)
	}
}

func TestAutoTravelPausesThirtyDaysAfterSecondCoexist(t *testing.T) {
	in := baseInput()
	g1, g2 := atNow.Add(-25*24*time.Hour), atNow.Add(-12*24*time.Hour)
	in.History = []model.InboundRegionPass{autoPass(g2, g2.Add(72*time.Hour), g2.Add(time.Hour), model.RegionPassCoexist)}
	if d := decideAutoTravel(in); d.Action != autoTravelGranted {
		t.Fatalf("一次并存已过 7 天冷却，应照常放行: %+v", d)
	}
	in.History = append(in.History, autoPass(g1, g1.Add(72*time.Hour), g1.Add(time.Hour), model.RegionPassCoexist))
	d := decideAutoTravel(in)
	if d.Action != autoTravelDenied || !strings.Contains(d.Reason, "30 天") {
		t.Fatalf("30 天内第二次并存应暂停 30 天: %+v", d)
	}
}

func TestAutoTravelSkipsAuthLessInbound(t *testing.T) {
	in := baseInput()
	in.AuthLess = true
	if d := decideAutoTravel(in); d.Action != autoTravelDenied || !strings.Contains(d.Reason, "认证") {
		t.Fatalf("无认证的入站不该自动放行: %+v", d)
	}
	g := atNow.Add(-5 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	in.Active = &p
	if d := decideAutoTravel(in); d.Action != autoTravelEnded || d.EndReason != model.RegionPassAutoOff {
		t.Fatalf("无认证的入站上的自动通行证应收回: %+v", d)
	}
}

func TestAutoTravelHoldsWhenObservationStale(t *testing.T) {
	in := baseInput()
	in.ObservationStale = true
	if d := decideAutoTravel(in); d.Action != autoTravelDenied || !strings.Contains(d.Reason, "观测") {
		t.Fatalf("观测中断时不该发放: %+v", d)
	}
	// 快到期、外地仍在用：观测中断时也不续期（家里回来了也看不见）。
	g := atNow.Add(-60 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	in.Active = &p
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-time.Hour), ActiveBytes: 5 * atMB}}
	if d := decideAutoTravel(in); d.Action != autoTravelIdle || !strings.Contains(d.Reason, "观测") {
		t.Fatalf("观测中断时不该续期: %+v", d)
	}
}

func TestCooldownZeroDisablesSharingGates(t *testing.T) {
	in := baseInput()
	in.Params.Cooldown = 0
	in.SuspectProvinces = map[string]bool{"上海市": true}
	g1, g2 := atNow.Add(-25*24*time.Hour), atNow.Add(-12*24*time.Hour)
	in.History = []model.InboundRegionPass{shortHomePass("上海市", 3*24*time.Hour), shortHomePass("上海市", 2*24*time.Hour),
		autoPass(g1, g1.Add(72*time.Hour), g1.Add(time.Hour), model.RegionPassCoexist),
		autoPass(g2, g2.Add(72*time.Hour), g2.Add(time.Hour), model.RegionPassCoexist)}
	if d := decideAutoTravel(in); d.Action != autoTravelGranted {
		t.Fatalf("冷却设为 0（不暂停）时这几道门槛一起关闭: %+v", d)
	}
}

func TestObservationFreshness(t *testing.T) {
	resetObserved()
	t.Cleanup(resetObserved)
	if observationFresh(atNow) {
		t.Fatal("从没采样成功过，不能当成观测正常")
	}
	markObserved(atNow)
	if !observationFresh(atNow.Add(2 * time.Minute)) {
		t.Error("2 分钟前刚采样成功，应当算新鲜")
	}
	if observationFresh(atNow.Add(4 * time.Minute)) {
		t.Error("4 分钟没采样成功，应当算观测中断")
	}
}

func TestSuspectProvincesFromBlockedWhileHome(t *testing.T) {
	home := newCIDRSet([]string{"192.0.2.0/24"})
	h := atHour(-30 * time.Hour)
	rows := []model.InboundIPHour{{IP: "192.0.2.9", HourStart: h, ActiveBytes: 2 * atMB},
		// 家里这一小时只有几百 KB：不算实质使用。
		{IP: "192.0.2.9", HourStart: h + 10*3600, ActiveBytes: 300 << 10}}
	blocked := []blockedHour{
		{IP: "198.51.100.1", HourMs: h * 1000},           // 同一小时家里在用 → 嫌疑
		{IP: "198.51.100.2", HourMs: (h + 5*3600) * 1000}, // 家里那时没用 → 像出差
		{IP: "198.51.100.3", HourMs: (h + 3600) * 1000},   // 前一小时家里在用 → 嫌疑
		{IP: "198.51.100.4", HourMs: (h + 10*3600) * 1000}, // 家里只有零星流量 → 不算
	}
	prov := map[string]string{"198.51.100.1": "上海市", "198.51.100.2": "浙江省", "198.51.100.3": "江苏省", "198.51.100.4": "安徽省"}
	got := suspectProvinces(blocked, rows, home, func(ip string) string { return prov[ip] })
	if len(got) != 2 || !got["上海市"] || !got["江苏省"] {
		t.Fatalf("suspectProvinces = %v，期望 上海市、江苏省", got)
	}
}

func TestBlockedHoursSince(t *testing.T) {
	setupAccessLogDB(t)
	since := atNow.Add(-7 * 24 * time.Hour)
	logs := []model.AccessLog{
		{InboundId: 1, Time: atNow.Add(-2 * time.Hour).UnixMilli(), SourceIP: "198.51.100.1", Route: model.BlockOutboundTag, Accepted: true},
		{InboundId: 1, Time: atNow.Add(-2*time.Hour + time.Minute).UnixMilli(), SourceIP: "198.51.100.1", Route: model.BlockOutboundTag, Accepted: true},
		{InboundId: 1, Time: atNow.Add(-3 * time.Hour).UnixMilli(), SourceIP: "198.51.100.2", Route: "direct", Accepted: true},
		{InboundId: 2, Time: atNow.Add(-2 * time.Hour).UnixMilli(), SourceIP: "198.51.100.3", Route: model.BlockOutboundTag, Accepted: true},
		{InboundId: 1, Time: atNow.Add(-8 * 24 * time.Hour).UnixMilli(), SourceIP: "198.51.100.4", Route: model.BlockOutboundTag, Accepted: true},
		{InboundId: 1, Time: atNow.Add(-time.Hour).UnixMilli(), SourceIP: "198.51.100.5", Route: model.BlockOutboundTag, Accepted: true},
	}
	if err := database.GetAccessLogDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	// 198.51.100.5 进了黑洞但不是地区规则拦的（比如命中了封禁域名）：不算。
	geo := func(ip string) bool { return ip != "198.51.100.5" }
	got, err := blockedHoursSince(1, since, geo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IP != "198.51.100.1" {
		t.Fatalf("blockedHoursSince = %+v，期望只有 198.51.100.1 的一个小时", got)
	}
	want := atNow.Add(-2*time.Hour).UnixMilli() / 3600000 * 3600000
	if got[0].HourMs != want {
		t.Errorf("HourMs = %d，期望整点 %d", got[0].HourMs, want)
	}
}

func resetObserved() { sharingObservedAt.Store(0) }

// TestRecentGeoBlockedSkipsStoredProvince 钉住审查里的 U2：核心的允许集过时（降级沿用旧集合、IP 库刚更新
// 还没重启）时，访问日志里会出现「其实在存储的地区里」却进了黑洞的来源。候选只取面板按当前 IP 库判定
// 确实在存储地区之外的来源，所以不会给已经存进去的省再发一张白占额度的通行证。
func TestRecentGeoBlockedSkipsStoredProvince(t *testing.T) {
	setupAccessLogDB(t)
	logs := []model.AccessLog{
		{InboundId: 1, Time: atNow.Add(-2 * time.Minute).UnixMilli(), SourceIP: "192.0.2.5", Route: model.BlockOutboundTag, Accepted: true},
		{InboundId: 1, Time: atNow.Add(-time.Minute).UnixMilli(), SourceIP: "198.51.100.7", Route: model.BlockOutboundTag, Accepted: true},
	}
	if err := database.GetAccessLogDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	m := &geoBlockMatcher{byInbound: map[int]*geoSourceRules{},
		storedAllow: map[int]*cidrSet{1: newCIDRSet([]string{"192.0.2.0/24"})}}
	got := recentGeoBlocked(1, m, atNow)
	if len(got) != 1 || got[0].IP != "198.51.100.7" {
		t.Fatalf("候选 = %+v，存储地区里的 192.0.2.5 不该成为候选", got)
	}
}

// 核心对这个入站的地区规则是锁死的（IP 归属地库不可用）：通行证放不进任何人，发了只会白占每周次数与每月天数。
func TestAutoTravelSkipsGeoClosedInbound(t *testing.T) {
	in := baseInput()
	in.GeoClosed = true
	if d := decideAutoTravel(in); d.Action != autoTravelDenied || !strings.Contains(d.Reason, "锁死") {
		t.Fatalf("地区规则锁死时不该自动放行: %+v", d)
	}
}

// 锁死的入站对所有来源都判「被拦」，家里的来源也会出现在被拦列表里；它们不是出差。
func TestDropStoredCandidates(t *testing.T) {
	home := newCIDRSet([]string{"192.0.2.0/24"})
	got := dropStoredCandidates([]autoTravelCandidate{
		{IP: "192.0.2.9", Province: "江苏省"}, {IP: "198.51.100.7", Province: "上海市"},
	}, home)
	if len(got) != 1 || got[0].IP != "198.51.100.7" {
		t.Errorf("家里的来源应被去掉: %+v", got)
	}
}

// 机房来源被「拦截机房 / 云服务器来源」挡着：给它所在的省发通行证也放不进来，只白占每周次数与每月天数。
func TestDropIDCCandidates(t *testing.T) {
	idc := newCIDRSet([]string{"203.0.113.0/24"})
	got := dropIDCCandidates([]autoTravelCandidate{
		{IP: "203.0.113.5", Province: "浙江省"}, {IP: "198.51.100.7", Province: "上海市"},
	}, idc)
	if len(got) != 1 || got[0].IP != "198.51.100.7" {
		t.Errorf("机房来源应被去掉: %+v", got)
	}
	if got := dropIDCCandidates([]autoTravelCandidate{{IP: "203.0.113.5"}}, nil); len(got) != 1 {
		t.Error("机房拦截关着时不该去掉任何来源")
	}
}
