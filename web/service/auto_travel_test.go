package service

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

const atMB = 1 << 20

// travelE2E 给整轮任务（AutoTravelService.Run）的测试准备用量库、访问日志库，打开访问日志；返回的 ipOf 从真实
// IP 库里取某个省的一个地址：多数票就判在这个省、又不在机房网段里（机房来源会被自动出差剔掉）。
// 调用前先用 relocateFixture / setupPassInbound 建好主库与入站。
func travelE2E(t *testing.T) (ipOf func(province string) string) {
	t.Helper()
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
	// 不用 setupAccessLogDB：它会重建主库，刚建的入站就没了。
	if err := database.InitAccessLogDB(filepath.Join(t.TempDir(), "access.db")); err != nil {
		t.Fatal(err)
	}
	if err := (&SettingService{}).SetAccessLogEnable(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resetObserved)
	return func(province string) string {
		t.Helper()
		idc := idcBlockedSet(&SettingService{}, &IPDBService{})
		for _, c := range regionCIDRSource((&IPDBService{}).DB(), &SettingService{}).CIDRsOfProvinces([]string{province}) {
			_, block, err := net.ParseCIDR(c)
			if err != nil || block.IP.To4() == nil {
				continue
			}
			ip := block.IP.String()
			if provinceOfIP(ip) == province && (idc == nil || !idc.Contains(ip)) {
				return ip
			}
		}
		t.Skip("IP 库里找不到" + province + "的普通（非机房）地址")
		return ""
	}
}

// writeRegionGeoDat 按真实 IP 库给这组省份写一份地区 dat（生产路径 bin/a-ui-geo.dat，测试结束删掉）：匹配器在核心
// 还没用上过地区规则时，按省份集合的 tag 在 dat 里找。没有通行证的入站要靠它认出「被地区规则拦下」的来源；不写的
// 话这类测试就只能靠别的测试留下的 dat 碰运气（曾经单独跑就失败）。
func writeRegionGeoDat(t *testing.T, provinces []string) {
	t.Helper()
	resetGeoGenerations(t)
	cidrs := regionCIDRSource((&IPDBService{}).DB(), &SettingService{}).CIDRsOfProvinces(provinces)
	if len(cidrs) == 0 {
		t.Skip("IP 库里没有这些省的段")
	}
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: geoDatCode(regionTag(provinces), cidrs), CIDRs: cidrs}}); err != nil {
		t.Fatal(err)
	}
	cleanupGeoDat(t)
}

// 整轮任务的接线：持有上海自动通行证、上海 3 小时没用、浙江的来源 2 分钟前被拦（浙江此刻发得出通行证）——Run 要以
// moved 结束上海的通行证。改动前被拦来源只在没有通行证时才读，判定函数在通行证生效期间根本收不到浙江的来源。
func TestRunEndsPassWhenUserMovedOn(t *testing.T) {
	in, pass := relocateFixture(t, 60521)
	ipOf := travelE2E(t)
	now := time.Now()
	hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
	rows := []model.InboundIPHour{{InboundId: in.Id, IP: ipOf("上海市"), HourStart: hour(3 * time.Hour), ActiveSeconds: 1800, ActiveBytes: 50 << 20}}
	for _, ago := range []time.Duration{4*24*time.Hour + 3*time.Hour, 5*24*time.Hour + 3*time.Hour, 6*24*time.Hour + 3*time.Hour} {
		// 出发前家里用过 3 天：换过去的浙江发得出通行证。三条正好相隔 24 小时：不管测试在一天里什么时候跑都落在
		// 3 个不同的日历天（隔得不整，测试在凌晨跑时两条会落进同一天）。
		rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(ago), ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	}
	if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-2 * time.Minute).UnixMilli(), InboundId: in.Id,
		SourceIP: ipOf("浙江省"), Route: model.BlockOutboundTag, Accepted: true}).Error; err != nil {
		t.Fatal(err)
	}
	markObserved(now)
	changed, err := (&AutoTravelService{}).Run(now)
	if err != nil {
		t.Fatal(err)
	}
	var p model.InboundRegionPass
	database.GetDB().First(&p, pass.Id)
	if !changed || p.EndReason != model.RegionPassMoved {
		t.Fatalf("上海 3 小时没用、浙江在被拦：应以 moved 结束上海的通行证: changed=%v pass=%+v status=%+v",
			changed, p, AutoTravelStatuses()[in.Id])
	}
	if active, err := (&RegionPassService{}).ActiveOf(in.Id); err != nil || active == nil || active.Regions != `["浙江省"]` || !active.Auto {
		t.Fatalf("同一轮里应给浙江发新的自动通行证: active=%+v err=%v", active, err)
	}
	// 被拦的只是上海自己的来源（比如通行证期间撞上了别的拦截规则）：不算换了地方。
	in2, pass2 := relocateFixture(t, 60522)
	ipOf2 := travelE2E(t)
	if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-2 * time.Minute).UnixMilli(), InboundId: in2.Id,
		SourceIP: ipOf2("上海市"), Route: model.BlockOutboundTag, Accepted: true}).Error; err != nil {
		t.Fatal(err)
	}
	markObserved(now)
	if _, err := (&AutoTravelService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	database.GetDB().First(&p, pass2.Id)
	if p.EndedAt != 0 {
		t.Fatalf("被拦的只是通行证省份自己的来源，不该结束通行证: %+v", p)
	}
}

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

// 暂停天数设成 0 时，管理员收回通行证也不能在下一分钟被原样发回：收回之后至少暂停一张通行证的时长。
// 改动前收回后的暂停挂在「暂停天数 > 0」上，0 时只有每周次数上限挡得住，收回等于没收。
func TestAutoTravelRevokePausesEvenWithZeroCooldown(t *testing.T) {
	in := baseInput()
	in.Params.Cooldown = 0
	g := atNow.Add(-5 * time.Hour)
	in.History = []model.InboundRegionPass{autoPass(g, g.Add(72*time.Hour), atNow.Add(-time.Minute), model.RegionPassRevoked)}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied || !strings.Contains(d.Reason, "收回") {
		t.Fatalf("暂停天数为 0，管理员 1 分钟前收回的通行证也不该被立刻发回: %+v", d)
	}
	// 收回已过一张通行证的时长（72 小时）：照常放行。
	g = atNow.Add(-80 * time.Hour)
	in.History = []model.InboundRegionPass{autoPass(g, g.Add(72*time.Hour), atNow.Add(-73*time.Hour), model.RegionPassRevoked)}
	if d := decideAutoTravel(in); d.Action != autoTravelGranted {
		t.Fatalf("收回已超过一张通行证的时长，应照常放行: %+v", d)
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

// movedOnInput：持有一张 30 小时前发的上海自动通行证，上海最后一次实质使用在 3 小时前（超过家里静默的 2 小时），
// 近 10 分钟浙江的来源在被地区规则拦。
func movedOnInput() autoTravelInput {
	in := baseInput()
	g := atNow.Add(-30 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
	in.Active, in.History = &p, []model.InboundRegionPass{p}
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-3 * time.Hour), ActiveBytes: 5 * atMB}}
	in.Blocked = []autoTravelCandidate{{IP: "203.0.113.5", Province: "浙江省"}}
	return in
}

// 人已经离开通行证的省、去了别的省：同一轮里以 moved 结束旧通行证、给新的省发一张（发放的全部门槛照旧）。
// 改动前生效中的通行证会把下一站一直挡到它到期，人走了它还可能再续一整张（实测下一站被挡约 2 天）。换和发要在
// 同一轮：留到下一轮再发的话，候选按来源 IP 的顺序挑，旧省还有设备在连时会被原样发回旧省（推演复现过）。
func TestAutoTravelSwitchesPassWhenUserMovedOn(t *testing.T) {
	d := decideAutoTravel(movedOnInput())
	if d.Action != autoTravelMoved || d.EndReason != model.RegionPassMoved || d.Province != "浙江省" ||
		d.Until != atNow.Add(72*time.Hour).UnixMilli() || !strings.Contains(d.Reason, "浙江省") {
		t.Fatalf("上海 3 小时没用、浙江在被拦：应以 moved 结束上海的通行证，同时给浙江发 72 小时: %+v", d)
	}
	cases := []struct {
		name string
		set  func(*autoTravelInput)
	}{
		{"上海 1 小时前还在用", func(in *autoTravelInput) {
			in.PassRows = append(in.PassRows, model.InboundIPHour{IP: "198.51.100.7", HourStart: atHour(-time.Hour), ActiveBytes: 2 * atMB})
		}},
		{"上海此刻在线在用", func(in *autoTravelInput) { in.PassLiveActive = true }},
		{"被拦的只是上海自己的来源", func(in *autoTravelInput) {
			in.Blocked = []autoTravelCandidate{{IP: "198.51.100.8", Province: "上海市"}}
		}},
		{"被拦来源查不到可放行的省份（境外、IPv6）", func(in *autoTravelInput) {
			in.Blocked = []autoTravelCandidate{{IP: "203.0.113.9"}}
		}},
		{"观测中断，看不清上海有没有在用", func(in *autoTravelInput) { in.ObservationStale = true }},
		{"通行证发出还不到家里静默时长", func(in *autoTravelInput) {
			g := atNow.Add(-time.Hour)
			p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "")
			in.Active, in.History = &p, []model.InboundRegionPass{p}
		}},
	}
	for _, c := range cases {
		in := movedOnInput()
		c.set(&in)
		if d := decideAutoTravel(in); d.Action == autoTravelMoved || d.Action == autoTravelEnded {
			t.Errorf("%s: 不该结束上海的通行证: %+v", c.name, d)
		}
	}
}

// 换回一周内刚换走的那个省之前，手上这个省要静默满 12 小时：两个省的设备交替闲置（笔记本白天在 C 用；落在 B 的
// 摄像头整夜在连）时，按家里静默的 2 小时来回换，会把通行证在两省之间翻来覆去，撞上每周次数时停在哪边就锁死哪边
// （推演复现：笔记本在 C 连着 5 天被拦）。真换地方是去新的省，不受影响。
func TestAutoTravelMovingBackNeedsLongerIdle(t *testing.T) {
	back := func(passIdle time.Duration) autoTravelInput {
		in := movedOnInput() // 持有上海的自动通行证，浙江在被拦
		in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-passIdle), ActiveBytes: 5 * atMB}}
		g := atNow.Add(-40 * time.Hour) // 浙江的通行证 30 小时前因「换了地方」结束（换到了上海）
		in.History = append(in.History, autoPass(g, g.Add(72*time.Hour), atNow.Add(-30*time.Hour), model.RegionPassMoved))
		in.History[len(in.History)-1].Regions = `["浙江省"]`
		return in
	}
	if d := decideAutoTravel(back(3 * time.Hour)); d.Action == autoTravelMoved {
		t.Fatalf("上海才静默 3 小时，不该换回刚换走的浙江: %+v", d)
	}
	if d := decideAutoTravel(back(13 * time.Hour)); d.Action != autoTravelMoved || d.Province != "浙江省" {
		t.Fatalf("上海已静默 13 小时：可以换回浙江: %+v", d)
	}
}

// 续期容忍一天左右不上线：通行证的省 24 小时内有过实质使用就续。曾改成「家里静默时长内仍在用」，推演里出差中途
// 歇一天的人通行证就断了，而那时「近 7 天在家用过」已不满足、发不出新的，总开关关着时一直被拦到回家；人走了还续
// 的害处由换了地方（moveOnCandidates）、家里恢复使用就收回、长住的「人还在那」兜着。
func TestAutoTravelRenewToleratesAnIdleDay(t *testing.T) {
	in := baseInput()
	g := atNow.Add(-60 * time.Hour)
	p := autoPass(g, g.Add(72*time.Hour), time.Time{}, "") // 剩 12 小时，在续期窗口里
	in.Active, in.History = &p, []model.InboundRegionPass{p}
	in.Blocked = nil
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(-20 * time.Hour), ActiveBytes: 3 * atMB}}
	if d := decideAutoTravel(in); d.Action != autoTravelRenewed {
		t.Fatalf("上海 20 小时前还用过：应当续期: %+v", d)
	}
	in.PassRows = nil
	if d := decideAutoTravel(in); d.Action == autoTravelRenewed {
		t.Fatalf("上海 24 小时内没有实质使用：不该续期: %+v", d)
	}
}

// 换过去之前要确认新的省此刻发得出通行证，发不出就保留手上这张：只换不看的话，一个人两台设备分属两个省（笔记本
// 在 B、手机在 C）交替闲置，来回换两次就撞上每周次数，旧的已结束、新的发不出，两台都被拦，近 7 天在家用过的天数
// 过期后再也发不出来（推演复现过）。
func TestAutoTravelMovesOnOnlyWhenNewProvinceCanBeGranted(t *testing.T) {
	cases := []struct {
		name string
		set  func(*autoTravelInput)
	}{
		{"一周内已发 3 次（含手上这张）", func(in *autoTravelInput) {
			for _, ago := range []time.Duration{2 * 24 * time.Hour, 4 * 24 * time.Hour} {
				g := atNow.Add(-ago)
				in.History = append(in.History, autoPass(g, g.Add(72*time.Hour), g.Add(3*time.Hour), model.RegionPassMoved))
			}
		}},
		{"近 7 天在家用过的天数已不够", func(in *autoTravelInput) { in.HomeRows = stableHomeRows()[:2] }},
		{"浙江在家里正在用的时候也来连过", func(in *autoTravelInput) { in.SuspectProvinces = map[string]bool{"浙江省": true} }},
		{"本月额度已满、不能预支", func(in *autoTravelInput) {
			ms := time.Date(2026, 11, 1, 0, 0, 0, 0, atLoc)
			in.History = append(in.History, autoPass(ms, ms.Add(240*time.Hour), ms.Add(240*time.Hour), model.RegionPassExpired))
		}},
	}
	for _, c := range cases {
		in := movedOnInput()
		c.set(&in)
		if d := decideAutoTravel(in); d.Action == autoTravelMoved || d.Action == autoTravelEnded {
			t.Errorf("%s: 换过去也发不出通行证，不该结束上海的: %+v", c.name, d)
		}
	}
}

// 因「换了地方」很快结束的通行证也算短命：两个人在两个省错开时间用，每次都会在几个小时后这样结束。
func TestAutoTravelShortPassGateCountsMovedPasses(t *testing.T) {
	moved := func(endedAgo time.Duration) model.InboundRegionPass {
		p := shortHomePass("上海市", endedAgo)
		p.EndReason = model.RegionPassMoved
		return p
	}
	in := baseInput()
	in.History = []model.InboundRegionPass{moved(2 * 24 * time.Hour), moved(24 * time.Hour)}
	if d := decideAutoTravel(in); d.Action != autoTravelDenied {
		t.Fatalf("上海两张刚发就因换了地方结束的通行证：应暂停对上海自动放行: %+v", d)
	}
}

// 本月额度不够撑到长住改地区判定点时预支（2026-11-10 管理员选定）：搬家的人额度只剩 1~2 天时，通行证撑不到
// 判定那天、在新地方攒不够 3 天实质使用，之后先因额度满、再因「近 7 天在家用过」不满足被一直拦着，不会自愈。
// 改了地区之后这些天算「住在那里」、额度照常退回；只有人提前回家没改成时才多占，当月最多超出长住天数。
func TestAutoTravelBorrowsQuotaToRelocationPoint(t *testing.T) {
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, atLoc)
	usedDays := func(hours int) model.InboundRegionPass {
		return autoPass(monthStart, monthStart.Add(time.Duration(hours)*time.Hour), monthStart.Add(time.Duration(hours)*time.Hour), model.RegionPassExpired)
	}
	// baseInput：家里最后一次实质使用在 5 天前（再过半小时就满 5 天），上海还没有任何被放行的使用——
	// 判定点按「上海攒够 3 天」算，约 3 天后。
	borrowing := func(history ...model.InboundRegionPass) autoTravelInput {
		in := baseInput()
		in.History = history
		in.RiskLevel = RiskLevelLow
		in.Relocation = &autoTravelRelocation{Days: 5}
		return in
	}
	// 额度够用（本月一天没用）：照常一整张，不是预支，原因里也不能说预支。
	if d := decideAutoTravel(borrowing()); d.Action != autoTravelGranted || strings.Contains(d.Reason, "预支") ||
		d.Until != atNow.Add(72*time.Hour).UnixMilli() {
		t.Fatalf("额度够用时是普通的一整张，不是预支: %+v", d)
	}
	d := decideAutoTravel(borrowing(usedDays(228))) // 已用 9.5 天，只剩 12 小时
	if d.Action != autoTravelGranted || d.Until != atNow.Add(72*time.Hour).UnixMilli() || !strings.Contains(d.Reason, "预支") {
		t.Fatalf("额度只剩 12 小时、长住判定点在 3 天后：应预支，给满一整张 72 小时: %+v", d)
	}
	// 额度已经用满也照样发：搬家时额度刚好用完的人，不然一张都拿不到，长住改地区也就没有依据。
	if d := decideAutoTravel(borrowing(usedDays(240))); d.Action != autoTravelGranted || !strings.Contains(d.Reason, "预支") {
		t.Fatalf("额度用满、长住判定点在 3 天后：应预支发放: %+v", d)
	}
	// 本月总占用不超过 每月天数 + 长住天数（10 + 5）：已用 14.5 天，只能再给 12 小时。
	if d := decideAutoTravel(borrowing(usedDays(348))); d.Action != autoTravelGranted || d.Until != atNow.Add(12*time.Hour).UnixMilli() {
		t.Fatalf("已用 14.5 天，预支最多到 15 天: %+v", d)
	}
	if d := decideAutoTravel(borrowing(usedDays(360))); d.Action != autoTravelDenied || !strings.Contains(d.Reason, "已满") {
		t.Fatalf("已用 15 天（每月天数 + 长住天数），不能再预支: %+v", d)
	}
	// 长住改地区对这个省不可能成立时不预支，照旧只给剩下的 12 小时。
	// 上海最近的一张是 11-05 发、11-06 被管理员收回的手动通行证（手动的不占自动放行额度，收回已过暂停期）。
	revokedManual := model.InboundRegionPass{Regions: `["上海市"]`, GrantedAt: monthStart.Add(4 * 24 * time.Hour).UnixMilli(),
		Until: monthStart.Add(6 * 24 * time.Hour).UnixMilli(), EndedAt: monthStart.Add(5 * 24 * time.Hour).UnixMilli(),
		EndReason: model.RegionPassRevoked}
	cases := []struct {
		name string
		set  func(*autoTravelInput)
	}{
		{"长住改地区没开（总开关关、长住天数 0 或入站排除）", func(in *autoTravelInput) { in.Relocation = nil }},
		{"共享风险中", func(in *autoTravelInput) { in.RiskLevel = RiskLevelMedium }},
		{"共享风险评分还没就绪", func(in *autoTravelInput) { in.RiskLevel = "" }},
		{"30 天内已经自动改去过别的省", func(in *autoTravelInput) {
			in.Relocation.State = &model.InboundAutoRegion{State: model.AutoRegionApplied, Regions: `["江苏省"]`,
				PrevRegions: `["浙江省"]`, RelocatedAt: atNow.Add(-10 * 24 * time.Hour).UnixMilli()}
		}},
		{"管理员收回过上海最近的一张通行证", func(in *autoTravelInput) { in.History = append(in.History, revokedManual) }},
	}
	for _, c := range cases {
		in := borrowing(usedDays(228))
		c.set(&in)
		if d := decideAutoTravel(in); d.Action != autoTravelGranted || d.Until != atNow.Add(12*time.Hour).UnixMilli() {
			t.Errorf("%s: 不该预支，只给剩下的 12 小时: %+v", c.name, d)
		}
	}
}

// 续期同样预支到长住判定点；到期时刻已经到了判定点（再续也不后移）就不报续期。
func TestAutoTravelRenewalBorrowsQuotaToRelocationPoint(t *testing.T) {
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, atLoc)
	used := autoPass(monthStart, monthStart.Add(228*time.Hour), monthStart.Add(228*time.Hour), model.RegionPassExpired) // 9.5 天
	renewing := func(until time.Time) autoTravelInput {
		in := baseInput()
		g := atNow.Add(-48 * time.Hour) // 生效中的这张已用 2 天：本月共 11.5 天，额度已满
		p := autoPass(g, until, time.Time{}, "")
		in.Active, in.History = &p, []model.InboundRegionPass{used, p}
		in.Blocked = nil
		in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(0), ActiveBytes: 3 * atMB}}
		// 家里最后一次实质使用在 1 天前：再过 4 天多才满「家里连续 5 天没用」。
		in.HomeRows = append(stableHomeRows(), model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(-24 * time.Hour), ActiveBytes: 5 * atMB})
		in.RiskLevel = RiskLevelLow
		in.Relocation = &autoTravelRelocation{Days: 5}
		return in
	}
	// 判定点在 4 天多以后，但本月最多到 15 天（还能预支 3.5 天）：续一整张 72 小时。
	if d := decideAutoTravel(renewing(atNow.Add(10 * time.Hour))); d.Action != autoTravelRenewed || d.Until != atNow.Add(72*time.Hour).UnixMilli() {
		t.Fatalf("额度已满、判定点还早：续期应预支一整张: %+v", d)
	}
	// 额度够用（没有之前那 9.5 天）：照常续一整张，原因里不能说预支。
	plenty := renewing(atNow.Add(10 * time.Hour))
	plenty.History = []model.InboundRegionPass{*plenty.Active}
	if d := decideAutoTravel(plenty); d.Action != autoTravelRenewed || strings.Contains(d.Reason, "预支") {
		t.Fatalf("额度够用时是普通续期，不是预支: %+v", d)
	}
	// 不预支时（长住改地区没开）额度已满，不再续期。
	in := renewing(atNow.Add(10 * time.Hour))
	in.Relocation = nil
	if d := decideAutoTravel(in); d.Action == autoTravelRenewed {
		t.Fatalf("不预支、额度已满：不该续期: %+v", d)
	}
	// 判定点就在今晚：家里最后一次实质使用在 4.5 天前（11-15 22 点那一小时，满 5 天是 11-20 23:00），上海已攒够
	// 3 天。判定点 = 23:00 + 1 小时余量 = 11-21 00:00。
	near := func(until time.Time) autoTravelInput {
		in := renewing(until)
		in.HomeRows = append(stableHomeRows(), model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(-108 * time.Hour), ActiveBytes: 5 * atMB})
		for _, back := range []time.Duration{0, 24 * time.Hour, 48 * time.Hour} {
			in.ServedAway = append(in.ServedAway, servedAwayRow{Province: "上海市", HourStart: atHour(-back), ActiveBytes: 5 * atMB})
		}
		return in
	}
	point := time.Date(2026, 11, 21, 0, 0, 0, 0, atLoc)
	if d := decideAutoTravel(near(atNow.Add(10 * time.Hour))); d.Action != autoTravelRenewed || d.Until != point.UnixMilli() {
		t.Fatalf("应只预支到判定点 11-21 00:00: %+v", d)
	}
	// 到期时刻已经在判定点上：再续也不后移，不报续期，说明预支到哪一刻。
	if d := decideAutoTravel(near(point)); d.Action != autoTravelIdle || !strings.Contains(d.Reason, "预支") {
		t.Fatalf("已预支到判定点，不该再报续期: %+v", d)
	}
}

// 换省前的试探也要看「本人在用时这个省也来连过」：持有自动通行证时，同样要把 SuspectProvinces 算出来交给试探
// （接线；为了省查询，只在换省的前提都成立时才算）。浙江的来源在家里正在用的时候被拦过（家里之后还接着用）、
// 像另一个人：不换，保留上海的通行证。
func TestRunKeepsPassWhenNewProvinceLooksLikeAnotherPerson(t *testing.T) {
	in, pass := relocateFixture(t, 60523)
	ipOf := travelE2E(t)
	now := time.Now()
	hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
	homeAgo := []time.Duration{4*24*time.Hour + 3*time.Hour, 5*24*time.Hour + 3*time.Hour, 6*24*time.Hour + 3*time.Hour}
	rows := []model.InboundIPHour{{InboundId: in.Id, IP: ipOf("上海市"), HourStart: hour(3 * time.Hour), ActiveSeconds: 1800, ActiveBytes: 50 << 20}}
	for _, ago := range homeAgo {
		rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(ago), ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	}
	// 4 天 3 小时前那次，家里接着又用了一小时：浙江是在家里用到一半时来连的。
	rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(homeAgo[0]) + 3600, ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	zj := ipOf("浙江省")
	logs := []model.AccessLog{
		// 家里正在用的时候（4 天 3 小时前那一小时，之后家里还接着用），浙江的来源也被拦过。
		{Time: hour(homeAgo[0])*1000 + 10*60*1000, InboundId: in.Id, SourceIP: zj, Route: model.BlockOutboundTag, Accepted: true},
		{Time: now.Add(-2 * time.Minute).UnixMilli(), InboundId: in.Id, SourceIP: zj, Route: model.BlockOutboundTag, Accepted: true},
	}
	if err := database.GetAccessLogDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	markObserved(now)
	if _, err := (&AutoTravelService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	var p model.InboundRegionPass
	database.GetDB().First(&p, pass.Id)
	if p.EndedAt != 0 {
		t.Fatalf("浙江像另一个人，不该换省、上海的通行证应当保留: pass=%+v status=%+v", p, AutoTravelStatuses()[in.Id])
	}
}

// 整轮任务的接线：总开关开着时，Run 把长住改地区的设置交给判定，额度不够时预支；总开关关着时不预支。
// 每月天数设成 1 天（剩 24 小时、不够一张 72 小时的通行证），不依赖今天是几号。
// 发放路径的接线（「本人在用时这个省也来连过」改成要求被拦之后家里还在接着用，2026-11-11）：
//   - 坐快车：家里最后在用的下一小时，上海的来源被拦；之后家里没再用、已静默满 2 小时——要发上海的通行证。改动前
//     「前一小时家里在用」就判成另一个人，上海整个冷却期发不出来。
//   - 同时在用：上海的来源被拦之后，家里还接着用了一小时——像另一个人，不发（家里此刻已静默满 2 小时，拦下它的是
//     这一条，不是家里静默）。
func TestRunGrantsAfterFastTrainButNotWhileHomeKeptUsing(t *testing.T) {
	for _, c := range []struct {
		name      string
		port      int
		homeAgo   []time.Duration // 家里有实质流量的小时（距今）
		blockedIn time.Duration   // 上海的来源在这个小时（距今）的第 20 分钟被拦
		wantGrant bool
	}{
		{"快车", 60561, []time.Duration{4 * time.Hour, 28 * time.Hour, 52 * time.Hour}, 3 * time.Hour, true},
		{"同时在用", 60562, []time.Duration{4 * time.Hour, 3 * time.Hour, 28 * time.Hour, 52 * time.Hour}, 4 * time.Hour, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := setupPassInbound(t, c.port, []string{"江苏省"})
			if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in.Id).Update("auto_travel", true).Error; err != nil {
				t.Fatal(err)
			}
			ipOf := travelE2E(t)
			writeRegionGeoDat(t, []string{"江苏省"})
			readyLowRisk(t, in.Id)
			now := time.Now()
			hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
			var rows []model.InboundIPHour
			for _, ago := range c.homeAgo {
				rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(ago), ActiveSeconds: 1800, ActiveBytes: 50 << 20})
			}
			if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			sh := ipOf("上海市")
			logs := []model.AccessLog{
				{Time: (hour(c.blockedIn) + 20*60) * 1000, InboundId: in.Id, SourceIP: sh, Route: model.BlockOutboundTag, Accepted: true},
				{Time: now.Add(-2 * time.Minute).UnixMilli(), InboundId: in.Id, SourceIP: sh, Route: model.BlockOutboundTag, Accepted: true},
			}
			if err := database.GetAccessLogDB().Create(&logs).Error; err != nil {
				t.Fatal(err)
			}
			markObserved(now)
			if _, err := (&AutoTravelService{}).Run(now); err != nil {
				t.Fatal(err)
			}
			pass, err := (&RegionPassService{}).ActiveOf(in.Id)
			if err != nil {
				t.Fatal(err)
			}
			status := AutoTravelStatuses()[in.Id]
			if c.wantGrant && (pass == nil || pass.Regions != `["上海市"]` || !pass.Auto) {
				t.Fatalf("家里停下之后上海才来连、之后家里没再用：应当发上海的通行证: pass=%+v status=%+v", pass, status)
			}
			if !c.wantGrant && (pass != nil || !strings.Contains(status.Reason, "像另一个人")) {
				t.Fatalf("上海被拦之后家里还接着用：应以「像另一个人」不发: pass=%+v status=%+v", pass, status)
			}
		})
	}
}

func TestRunBorrowsQuotaWhenRelocationCanFollow(t *testing.T) {
	for _, master := range []bool{true, false} {
		in := setupPassInbound(t, 60541, []string{"江苏省"})
		if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in.Id).Update("auto_travel", true).Error; err != nil {
			t.Fatal(err)
		}
		ipOf := travelE2E(t)
		writeRegionGeoDat(t, []string{"江苏省"})
		if master {
			masterSwitchOn(t)
		}
		if err := database.GetDB().Create(&model.Setting{Key: "autoTravelMonthlyDays", Value: "1"}).Error; err != nil {
			t.Fatal(err)
		}
		readyLowRisk(t, in.Id)
		now := time.Now()
		hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
		var rows []model.InboundIPHour
		for _, ago := range []time.Duration{3 * time.Hour, 27 * time.Hour, 51 * time.Hour} { // 家里 3 天在用，最后一次 3 小时前
			rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(ago), ActiveSeconds: 1800, ActiveBytes: 50 << 20})
		}
		if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-2 * time.Minute).UnixMilli(), InboundId: in.Id,
			SourceIP: ipOf("上海市"), Route: model.BlockOutboundTag, Accepted: true}).Error; err != nil {
			t.Fatal(err)
		}
		markObserved(now)
		if _, err := (&AutoTravelService{}).Run(now); err != nil {
			t.Fatal(err)
		}
		pass, err := (&RegionPassService{}).ActiveOf(in.Id)
		if err != nil || pass == nil {
			t.Fatalf("总开关=%v：应当发放上海的通行证: pass=%+v err=%v status=%+v", master, pass, err, AutoTravelStatuses()[in.Id])
		}
		hours := float64(pass.Until-now.UnixMilli()) / float64(time.Hour.Milliseconds())
		if master && (hours < 71.9 || hours > 72.1) {
			t.Errorf("总开关开着：额度只剩 24 小时、长住判定点在几天后，应预支给满 72 小时，实际 %.1f 小时", hours)
		}
		if !master && (hours < 23.9 || hours > 24.1) {
			t.Errorf("总开关关着：长住改地区不会发生，不预支，只给剩下的 24 小时，实际 %.1f 小时", hours)
		}
	}
}

// 预支的终点还要等别的省滑出长住窗口：长住改地区要求近 N 天里只有一个省被放行过，家里→B（换省结束）→C 的人，
// B 的使用在窗口里时改不了。只按家里静默与 C 攒够天数算终点的话，预支在 B 滑出窗口之前就停了，人在 C 被拦到
// 那一刻（代码审查指出）。两个省交替在用（通勤）不是搬家，长住改不了，不预支。
func TestAutoTravelBorrowWaitsForOtherProvinceToAgeOut(t *testing.T) {
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, atLoc)
	used := autoPass(monthStart, monthStart.Add(228*time.Hour), monthStart.Add(228*time.Hour), model.RegionPassExpired) // 9.5 天
	moved := func() autoTravelInput {
		in := baseInput()
		g := atNow.Add(-48 * time.Hour) // 生效中的上海这张已用 2 天：本月共 11.5 天，额度已满
		p := autoPass(g, atNow.Add(10*time.Hour), time.Time{}, "")
		in.Active, in.History = &p, []model.InboundRegionPass{used, p}
		in.Blocked = nil
		in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(0), ActiveBytes: 3 * atMB}}
		// 家里 5 天多没用（已满 5 天静默），上海这 3 天天天在用（已攒够 3 天）……
		in.HomeRows = []model.InboundIPHour{{IP: "192.0.2.9", HourStart: atHour(-5*24*time.Hour - 2*time.Hour), ActiveBytes: 5 * atMB}}
		for _, back := range []time.Duration{0, 24 * time.Hour, 48 * time.Hour} {
			in.ServedAway = append(in.ServedAway, servedAwayRow{Province: "上海市", HourStart: atHour(-back), ActiveBytes: 5 * atMB})
		}
		// ……但到上海之前在浙江待过（浙江的通行证换省结束），浙江最后一次放行在 49 小时前（上海第一次放行之前）：
		// 还要 3 天多才滑出 5 天窗口，终点比一整张通行证还远。
		in.ServedAway = append(in.ServedAway, servedAwayRow{Province: "浙江省", HourStart: atHour(-49 * time.Hour), ActiveBytes: 5 * atMB})
		in.RiskLevel = RiskLevelLow
		in.Relocation = &autoTravelRelocation{Days: 5}
		return in
	}
	if d := decideAutoTravel(moved()); d.Action != autoTravelRenewed || d.Until != atNow.Add(72*time.Hour).UnixMilli() {
		t.Fatalf("浙江还在长住窗口里、改不了地区：应继续预支到它滑出去为止（续一整张）: %+v", d)
	}
	// 两个省交替在用：浙江在上海开始用之后还被放行过，不是搬家。
	commute := moved()
	commute.ServedAway = append(commute.ServedAway, servedAwayRow{Province: "浙江省", HourStart: atHour(-20 * time.Hour), ActiveBytes: 5 * atMB})
	if d := decideAutoTravel(commute); d.Action == autoTravelRenewed {
		t.Fatalf("两个省交替在用不是搬家，不该预支: %+v", d)
	}
}

// 本月剩余额度比续期窗口还短时，续期算出的到期时刻被额度钉死、不再后移：不能每一轮都报「续期」。
// 改动前额度用完前的最后 24 小时里每分钟写一次库，弹窗一直显示「续期」，其实它马上就要因额度结束。
func TestAutoTravelDoesNotRenewWhenUntilCannotMove(t *testing.T) {
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, atLoc)
	used := autoPass(monthStart, monthStart.Add(7*24*time.Hour), monthStart.Add(7*24*time.Hour), model.RegionPassExpired) // 已用 7 天
	g := atNow.Add(-50 * time.Hour)
	// 生效中的这张已用 50 小时：本月共 9 天 2 小时，剩 22 小时；它的到期时刻正好是额度用完的那一刻。
	active := autoPass(g, atNow.Add(22*time.Hour), time.Time{}, "")
	in := baseInput()
	in.Active = &active
	in.History = []model.InboundRegionPass{used, active}
	in.PassRows = []model.InboundIPHour{{IP: "198.51.100.7", HourStart: atHour(0), ActiveBytes: 3 * atMB}}
	d := decideAutoTravel(in)
	if d.Action != autoTravelIdle || !strings.Contains(d.Reason, "额度") {
		t.Fatalf("到期时刻已卡在额度用完那一刻：不该报续期，应说明额度何时用完: %+v", d)
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

// 「本人在用时这个省也来连过」：被拦落在家里一段实质使用的中间——那一小时或前一小时家里在用，并且后一小时家里还在
// 接着用（2026-11-11 管理员选定）。只看前面的话，离家后一两个小时内在外地连上的人都会被当成另一个人：30 分钟快车去
// 邻省、路上手机走流量（地址判在家里的省）到站连酒店 Wi-Fi，这个省整个冷却期发不出通行证。
func TestSuspectProvincesFromBlockedWhileHome(t *testing.T) {
	home := newCIDRSet([]string{"192.0.2.0/24"})
	h := atHour(-60 * time.Hour)
	const mb2 = 2 * atMB
	rows := []model.InboundIPHour{
		// 家里连着用了三小时。
		{IP: "192.0.2.9", HourStart: h, ActiveBytes: mb2},
		{IP: "192.0.2.9", HourStart: h + 3600, ActiveBytes: mb2},
		{IP: "192.0.2.9", HourStart: h + 2*3600, ActiveBytes: mb2},
		// 中间隔一小时没用，前后各用了一小时。
		{IP: "192.0.2.9", HourStart: h + 20*3600, ActiveBytes: mb2},
		{IP: "192.0.2.9", HourStart: h + 22*3600, ActiveBytes: mb2},
		// 家里之外的来源在用（比如通行证那个省）：不是家里。
		{IP: "203.0.113.5", HourStart: h + 28*3600, ActiveBytes: mb2},
		{IP: "192.0.2.9", HourStart: h + 30*3600, ActiveBytes: mb2},
		// 家里只有几百 KB：不算实质使用。
		{IP: "192.0.2.9", HourStart: h + 40*3600, ActiveBytes: 300 << 10},
		{IP: "192.0.2.9", HourStart: h + 41*3600, ActiveBytes: 300 << 10},
	}
	blocked := []blockedHour{
		{IP: "198.51.100.1", HourMs: (h + 3600) * 1000},    // 家里在用的中间 → 嫌疑
		{IP: "198.51.100.2", HourMs: (h + 2*3600) * 1000},  // 家里最后一小时，之后没再用：到站换了网络 → 像出差
		{IP: "198.51.100.3", HourMs: (h + 3*3600) * 1000},  // 家里停下后的下一小时：坐快车到了 → 像出差
		{IP: "198.51.100.4", HourMs: (h + 21*3600) * 1000}, // 前后两小时家里都在用 → 嫌疑
		{IP: "198.51.100.5", HourMs: (h + 29*3600) * 1000}, // 被拦之后家里才开始用：人回来了 → 不算
		{IP: "198.51.100.6", HourMs: (h + 40*3600) * 1000}, // 家里只有零星流量 → 不算
		{IP: "198.51.100.7", HourMs: (h + 10*3600) * 1000}, // 家里那时没用 → 像出差
	}
	prov := map[string]string{"198.51.100.1": "上海市", "198.51.100.2": "浙江省", "198.51.100.3": "江苏省",
		"198.51.100.4": "安徽省", "198.51.100.5": "福建省", "198.51.100.6": "山东省", "198.51.100.7": "江西省"}
	got := suspectProvinces(blocked, rows, home, func(ip string) string { return prov[ip] })
	if len(got) != 2 || !got["上海市"] || !got["安徽省"] {
		t.Fatalf("suspectProvinces = %v，期望 上海市、安徽省", got)
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
