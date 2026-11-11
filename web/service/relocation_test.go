package service

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// readyLowRisk 给入站写一份已就绪、低风险的共享风险快照（长住改地区与预支额度都要求评分已就绪）。
func readyLowRisk(t *testing.T, inboundId int) {
	t.Helper()
	if err := database.GetTrafficDB().Create(&model.InboundRiskSnapshot{InboundId: inboundId,
		EngineVersion: sharingRiskEngineVersion, Level: RiskLevelLow, State: RiskStateReady, Confidence: 80}).Error; err != nil {
		t.Fatal(err)
	}
}

// masterSwitchOn 打开「自动管理地区限制」总开关（长住改地区只在它开着时运行）。
func masterSwitchOn(t *testing.T) {
	t.Helper()
	if err := database.GetDB().Create(&model.Setting{Key: "autoRegionEnable", Value: "1"}).Error; err != nil {
		t.Fatal(err)
	}
}

// baseRelocate：家里（江苏 192.0.2.0/24）最后一次实质使用在 6 天前，上海市这 5 天里天天在用（在自动通行证里被放行）。
func baseRelocate() relocateInput {
	var away []servedAwayRow
	for d := 0; d < 5; d++ {
		away = append(away, servedAwayRow{Province: "上海市", HourStart: atHour(-time.Duration(d)*24*time.Hour - time.Hour), ActiveBytes: 20 * atMB})
	}
	g := atNow.Add(-5 * 24 * time.Hour)
	p := autoPass(g, atNow.Add(48*time.Hour), time.Time{}, "")
	return relocateInput{
		Now: atNow, Loc: atLoc, Enabled: true, Days: 5, Active: &p,
		HomeRows:   []model.InboundIPHour{{IP: "192.0.2.9", HourStart: atHour(-6 * 24 * time.Hour), ActiveBytes: 20 * atMB}},
		ServedAway: away,
		History:    []model.InboundRegionPass{p},
		RiskLevel:  RiskLevelLow,
	}
}

// recentOf 把一组动静包成 relocateInput.Recent。
func recentOf(sources ...recentSource) func() ([]recentSource, error) {
	return func() ([]recentSource, error) { return sources, nil }
}

func TestDecideRelocation(t *testing.T) {
	if d := decideRelocation(baseRelocate()); d.Province != "上海市" || !strings.Contains(d.Reason, "上海市") {
		t.Fatalf("家里 6 天没用、上海天天在用，应改成上海市: %+v", d)
	}
	cases := []struct {
		name string
		set  func(*relocateInput)
	}{
		{"总开关关", func(in *relocateInput) { in.Enabled = false }},
		{"天数为 0（不自动改）", func(in *relocateInput) { in.Days = 0 }},
		{"家里 3 天前还在用", func(in *relocateInput) {
			in.HomeRows = append(in.HomeRows, model.InboundIPHour{IP: "192.0.2.9", HourStart: atHour(-3 * 24 * time.Hour), ActiveBytes: 20 * atMB})
		}},
		{"家里此刻在用", func(in *relocateInput) { in.HomeLiveActive = true }},
		{"外地只有 2 天在用", func(in *relocateInput) { in.ServedAway = in.ServedAway[:2] }},
		{"观测中断", func(in *relocateInput) { in.ObservationStale = true }},
		{"共享风险读不出", func(in *relocateInput) { in.RiskUnknown = true }},
		// 等级为空串：评分还没就绪（学习中、降级、引擎版本刚变、还没有快照）——改地区会把这个省永久合法化，
		// 「测不了」不能当成「低风险」。
		{"共享风险评分还没就绪", func(in *relocateInput) { in.RiskLevel = "" }},
		{"共享风险中", func(in *relocateInput) { in.RiskLevel = RiskLevelMedium }},
		{"取近期动静失败", func(in *relocateInput) {
			in.Recent = func() ([]recentSource, error) { return nil, errors.New("访问日志库坏了") }
		}},
		{"无认证的入站", func(in *relocateInput) { in.AuthLess = true }},
		{"手动通行证", func(in *relocateInput) { p := *in.Active; p.Auto = false; in.Active = &p }},
		{"暂停地区规则的通行证", func(in *relocateInput) { p := *in.Active; p.Regions = `["*"]`; in.Active = &p }},
		{"多省通行证", func(in *relocateInput) { p := *in.Active; p.Regions = `["上海市","浙江省"]`; in.Active = &p }},
		{"外地有两个省都在实质使用", func(in *relocateInput) {
			for d := 0; d < 3; d++ {
				in.ServedAway = append(in.ServedAway, servedAwayRow{Province: "浙江省", HourStart: atHour(-time.Duration(d)*24*time.Hour - 3*time.Hour), ActiveBytes: 20 * atMB})
			}
		}},
		{"管理员收回过这个省最近的一张通行证", func(in *relocateInput) {
			in.Active = nil
			revoked := autoPass(atNow.Add(-2*24*time.Hour), atNow.Add(24*time.Hour), atNow.Add(-24*time.Hour), model.RegionPassRevoked)
			in.History = append(in.History, revoked)
		}},
		{"30 天内已经自动改去过别的省", func(in *relocateInput) {
			in.State = &model.InboundAutoRegion{State: model.AutoRegionApplied, Regions: `["江苏省"]`, PrevRegions: `["浙江省"]`,
				RelocatedAt: atNow.Add(-10 * 24 * time.Hour).UnixMilli()}
		}},
		{"外地的行量不够实质使用", func(in *relocateInput) {
			for i := range in.ServedAway {
				in.ServedAway[i].ActiveBytes = 100 << 10
			}
		}},
	}
	for _, c := range cases {
		in := baseRelocate()
		c.set(&in)
		if d := decideRelocation(in); d.Province != "" {
			t.Errorf("%s: 不该改地区: %+v", c.name, d)
		}
	}
	// 外地的天数要求随设置变小：设成 2 天时，有 2 天在用就够。
	in := baseRelocate()
	in.Days = 2
	in.HomeRows = []model.InboundIPHour{{IP: "192.0.2.9", HourStart: atHour(-3 * 24 * time.Hour), ActiveBytes: 20 * atMB}}
	in.ServedAway = in.ServedAway[:2]
	if d := decideRelocation(in); d.Province != "上海市" {
		t.Errorf("长住天数设为 2 时，家里 3 天没用、上海 2 天在用就该改: %+v", d)
	}
}

// 本月自动放行额度先用完、通行证没撑到第 N 天：凭通行证期间留下的实质使用照样改地区。改动前要求此刻还有一张
// 生效中的通行证，这种人从此一直被拦——第 6 天起「近 7 天在家里用过」也不满足了，再也发不出通行证。
func TestDecideRelocationWithoutLivePass(t *testing.T) {
	in := baseRelocate()
	expired := autoPass(atNow.Add(-5*24*time.Hour), atNow.Add(-24*time.Hour), atNow.Add(-24*time.Hour), model.RegionPassExpired)
	in.Active = nil
	in.History = []model.InboundRegionPass{expired}
	if d := decideRelocation(in); d.Province != "上海市" {
		t.Fatalf("通行证到期后家里 6 天没用、上海 5 天在用，应改成上海市: %+v", d)
	}
}

// 改地区之前要确认人此刻还在那：近 24 小时这个省没有任何动静（没在用、也没来连），或者最近的动静在别处
// （去了下一站、出了境），都先不改。改动前只看「近 N 天里有 3 天在用」：A→B（3 天）→C 的人在 C 时地区就被改成
// 了 B，回家以后反被锁在门外，不会自愈。被拦的来连在这里只用来确认 / 否决人在哪，不算住在那里的天数。
func TestDecideRelocationRequiresStillThere(t *testing.T) {
	// 上海最后一次被放行的实质使用在 30 小时前（之后通行证到期，人在不在上海还不知道）。
	lapsed := func() relocateInput {
		in := baseRelocate()
		in.Active = nil
		in.ServedAway = nil
		for d := 0; d < 4; d++ {
			in.ServedAway = append(in.ServedAway, servedAwayRow{Province: "上海市",
				HourStart: atHour(-30*time.Hour - time.Duration(d)*24*time.Hour), ActiveBytes: 20 * atMB})
		}
		return in
	}
	if d := decideRelocation(lapsed()); d.Province != "" {
		t.Fatalf("近 24 小时上海没有任何动静（可能已经离开、或一直没上线）：先不改: %+v", d)
	}
	in := lapsed()
	in.Recent = recentOf(recentSource{Province: "上海市", Hour: atHour(-time.Hour)})
	if d := decideRelocation(in); d.Province != "上海市" {
		t.Fatalf("上海的来源 1 小时前还在来连：人还在上海，应改成上海市: %+v", d)
	}
	in.Recent = recentOf(recentSource{Province: "上海市", Hour: atHour(-2 * time.Hour)},
		recentSource{Province: "浙江省", Hour: atHour(-time.Hour)})
	if d := decideRelocation(in); d.Province != "" {
		t.Fatalf("最近的动静在浙江（去了下一站）：不该改成上海: %+v", d)
	}
	in.Recent = recentOf(recentSource{Province: "上海市", Hour: atHour(-2 * time.Hour)},
		recentSource{Hour: atHour(-time.Hour)})
	if d := decideRelocation(in); d.Province != "" {
		t.Fatalf("最近的动静在境外或查不到省份的地方：不该改成上海: %+v", d)
	}
	// 同一小时两边都有（手机与笔记本分属两个省）：不算离开。
	in.Recent = recentOf(recentSource{Province: "上海市", Hour: atHour(-time.Hour)},
		recentSource{Province: "浙江省", Hour: atHour(-time.Hour)})
	if d := decideRelocation(in); d.Province != "上海市" {
		t.Fatalf("同一小时两边都有动静，不算离开: %+v", d)
	}
	// 通行证还在、上海 1 小时前还在用：近期放行的使用本身就说明人在（baseRelocate）；浙江更晚的来连照样否决。
	in = baseRelocate()
	in.Recent = recentOf(recentSource{Province: "浙江省", Hour: atHour(0)})
	if d := decideRelocation(in); d.Province != "" {
		t.Fatalf("浙江的来连比上海的使用更晚：不该改成上海: %+v", d)
	}
}

// 出差超过 N 天会先改到出差地，回家后再改回来：回到原来的地区不受「30 天内只自动改一次」限制。
func TestDecideRelocationBackToPreviousHomeIsNotCapped(t *testing.T) {
	in := baseRelocate()
	in.State = &model.InboundAutoRegion{State: model.AutoRegionApplied, Regions: `["江苏省"]`, PrevRegions: `["上海市"]`,
		RelocatedAt: atNow.Add(-10 * 24 * time.Hour).UnixMilli()}
	if d := decideRelocation(in); d.Province != "上海市" {
		t.Fatalf("回到原来的上海市不该被 30 天限制挡住: %+v", d)
	}
}

// 改地区之后，那几张通行证算「住在那里」而不是出差，不再占本月的自动放行额度。
func TestAutoDaysThisMonthIgnoresRelocatedPasses(t *testing.T) {
	relocated := autoPass(atNow.Add(-8*24*time.Hour), atNow.Add(-5*24*time.Hour), atNow.Add(-3*24*time.Hour), model.RegionPassRelocated)
	trip := autoPass(atNow.Add(-2*24*time.Hour), atNow.Add(24*time.Hour), atNow.Add(-24*time.Hour), model.RegionPassHome)
	got := autoDaysThisMonth([]model.InboundRegionPass{relocated, trip}, atNow, atLoc)
	if got < 0.99 || got > 1.01 {
		t.Errorf("本月自动放行天数 = %.2f，期望只算出差那 1 天", got)
	}
}

// relocateFixture 建一个地区为江苏省、开着自动出差、持有一张上海市自动通行证的入站。
func relocateFixture(t *testing.T, port int) (*model.Inbound, *model.InboundRegionPass) {
	t.Helper()
	in := setupPassInbound(t, port, []string{"江苏省"})
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in.Id).Update("auto_travel", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pass, err := (&RegionPassService{}).GrantAuto(in.Id, "上海市", now.Add(48*time.Hour).UnixMilli(), now.Add(-5*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return in, pass
}

func TestRelocateWritesRegionsEndsPassAndRecordsPrevious(t *testing.T) {
	in, pass := relocateFixture(t, 60501)
	now := time.Now()
	ok, err := relocate(in, pass, "上海市", "长住上海", now)
	if err != nil || !ok {
		t.Fatalf("relocate = %v, %v", ok, err)
	}
	got, _ := (&InboundService{}).GetInbound(in.Id)
	if got.Regions != `["上海市"]` || !got.AutoTravel {
		t.Errorf("地区 = %s，自动出差 = %v；期望改成上海市并保持自动出差", got.Regions, got.AutoTravel)
	}
	var p model.InboundRegionPass
	database.GetDB().First(&p, pass.Id)
	if p.EndedAt == 0 || p.EndReason != model.RegionPassRelocated {
		t.Errorf("通行证应以 relocated 结束: %+v", p)
	}
	st, _ := (&AutoRegionService{}).stateOf(in.Id)
	if st == nil || st.State != model.AutoRegionApplied || st.PrevRegions != `["江苏省"]` || st.Regions != `["上海市"]` {
		t.Errorf("状态行 = %+v，期望 applied、原地区江苏省", st)
	}
}

func TestRelocateRespectsConcurrentAdminEdit(t *testing.T) {
	in, pass := relocateFixture(t, 60502)
	// 判定之后、写入之前，管理员把地区改成了浙江省：自动改地区必须让路。
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in.Id).Update("regions", `["浙江省"]`).Error; err != nil {
		t.Fatal(err)
	}
	ok, err := relocate(in, pass, "上海市", "长住上海", time.Now())
	if ok || err != nil {
		t.Fatalf("管理员刚改过地区，应静默跳过: ok=%v err=%v", ok, err)
	}
	got, _ := (&InboundService{}).GetInbound(in.Id)
	var p model.InboundRegionPass
	database.GetDB().First(&p, pass.Id)
	st, _ := (&AutoRegionService{}).stateOf(in.Id)
	if got.Regions != `["浙江省"]` || p.EndedAt != 0 || st != nil {
		t.Errorf("什么都不该改: regions=%s pass=%+v state=%+v", got.Regions, p, st)
	}
	// 管理员刚收回了通行证：同样让路（他不想放行这个省）。
	in2, pass2 := relocateFixture(t, 60503)
	if _, err := (&RegionPassService{}).RevokePass(in2.Id, pass2.Id, model.RegionPassRevoked, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := relocate(in2, pass2, "上海市", "长住上海", time.Now()); ok || err != nil {
		t.Fatalf("通行证已被收回，应静默跳过: ok=%v err=%v", ok, err)
	}
	if got, _ := (&InboundService{}).GetInbound(in2.Id); got.Regions != `["江苏省"]` {
		t.Errorf("地区不该变: %s", got.Regions)
	}
}

func TestUndoRelocationRestoresPreviousRegions(t *testing.T) {
	in, pass := relocateFixture(t, 60504)
	now := time.Now()
	if ok, err := relocate(in, pass, "上海市", "长住上海", now); !ok || err != nil {
		t.Fatalf("relocate = %v, %v", ok, err)
	}
	if err := (&AutoRegionService{}).Undo(in.Id, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := (&InboundService{}).GetInbound(in.Id)
	if got.Regions != `["江苏省"]` || got.AutoTravel {
		t.Errorf("撤销长住改地区应恢复江苏省并关闭自动出差: regions=%s autoTravel=%v", got.Regions, got.AutoTravel)
	}
	st, _ := (&AutoRegionService{}).stateOf(in.Id)
	if st == nil || st.State != model.AutoRegionManual {
		t.Errorf("撤销后由管理员接手: %+v", st)
	}
}

func TestUpdateInboundRejectsStaleWriteBackOfPreviousRegions(t *testing.T) {
	in, pass := relocateFixture(t, 60505)
	now := time.Now()
	if ok, err := relocate(in, pass, "上海市", "长住上海", now); !ok || err != nil {
		t.Fatalf("relocate = %v, %v", ok, err)
	}
	// 页面停在改地区之前：整行写回带着江苏省。
	stale := *in
	stale.Regions = `["江苏省"]`
	if err := (&InboundService{}).UpdateInbound(&stale); err == nil || !strings.Contains(err.Error(), "刷新") {
		t.Fatalf("过期页面把原来的地区写回来，应当被拒: %v", err)
	}
	// 管理员明确改成第三个省：放行。
	fresh := *in
	fresh.Regions = `["浙江省"]`
	if err := (&InboundService{}).UpdateInbound(&fresh); err != nil {
		t.Fatalf("改成别的省应当放行: %v", err)
	}
}

// TestRunRelocatesWhenMasterSwitchOn 走整轮任务：家里（江苏）6 天没用、上海 5 天在用、持有上海的自动通行证。
func TestRunRelocatesWhenMasterSwitchOn(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		in, pass := relocateFixture(t, 60506)
		if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
			t.Fatal(err)
		}
		// 不用 setupAccessLogDB：它会重建主库，刚建的入站就没了。
		if err := database.InitAccessLogDB(filepath.Join(t.TempDir(), "access.db")); err != nil {
			t.Fatal(err)
		}
		db, tdb := database.GetDB(), database.GetTrafficDB()
		if enabled {
			if err := db.Create(&model.Setting{Key: "autoRegionEnable", Value: "1"}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := (&SettingService{}).SetAccessLogEnable(true); err != nil {
			t.Fatal(err)
		}
		ipOf := func(province string) string {
			cidrs := regionCIDRSource((&IPDBService{}).DB(), &SettingService{}).CIDRsOfProvinces([]string{province})
			if len(cidrs) == 0 {
				t.Skip("IP 库里没有" + province + "的段")
			}
			_, block, err := net.ParseCIDR(cidrs[0])
			if err != nil {
				t.Fatal(err)
			}
			return block.IP.String()
		}
		now := time.Now()
		hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
		rows := []model.InboundIPHour{{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(6 * 24 * time.Hour), ActiveSeconds: 1800, ActiveBytes: 50 << 20}}
		for d := 0; d < 5; d++ {
			rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("上海市"), HourStart: hour(time.Duration(d)*24*time.Hour + time.Hour),
				ActiveSeconds: 1800, ActiveBytes: 50 << 20})
		}
		if err := tdb.Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
		readyLowRisk(t, in.Id)
		markObserved(now)
		changed, err := (&AutoTravelService{}).Run(now)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := (&InboundService{}).GetInbound(in.Id)
		var p model.InboundRegionPass
		db.First(&p, pass.Id)
		if enabled && (!changed || got.Regions != `["上海市"]` || p.EndReason != model.RegionPassRelocated) {
			t.Errorf("总开关开着：应改成上海市并结束通行证: changed=%v regions=%s pass=%+v", changed, got.Regions, p)
		}
		if !enabled && (got.Regions != `["江苏省"]` || p.EndedAt != 0) {
			t.Errorf("总开关关着：升级后行为不变，不该改地区: regions=%s pass=%+v", got.Regions, p)
		}
	}
	resetObserved()
}

// 整轮任务的接线：近期动静来自访问日志里被地区规则拦下的来连。
//   - 上海天天在用（最后一次 2 小时前）、通行证还在，但浙江的来源 1 分钟前被拦（人去了下一站）：不改地区；
//   - 上海的通行证 30 小时前到期（之后被拦着），上海的来源 5 分钟前还在来连：人还在上海，照常改。
//
// 「最后一次放行」与「来连」故意隔开两个小时以上：比较按整点小时，挨得近的话测试在整点后几分钟跑会落进同一小时。
func TestRunRelocationChecksWhereTheUserIs(t *testing.T) {
	in, pass := relocateFixture(t, 60531)
	ipOf := travelE2E(t)
	masterSwitchOn(t)
	readyLowRisk(t, in.Id)
	now := time.Now()
	hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
	rows := []model.InboundIPHour{{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(6 * 24 * time.Hour), ActiveSeconds: 1800, ActiveBytes: 50 << 20}}
	for d := 0; d < 5; d++ {
		rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("上海市"), HourStart: hour(time.Duration(d)*24*time.Hour + 2*time.Hour),
			ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	}
	if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-time.Minute).UnixMilli(), InboundId: in.Id,
		SourceIP: ipOf("浙江省"), Route: model.BlockOutboundTag, Accepted: true}).Error; err != nil {
		t.Fatal(err)
	}
	markObserved(now)
	if _, err := (&AutoTravelService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	got, _ := (&InboundService{}).GetInbound(in.Id)
	var p model.InboundRegionPass
	database.GetDB().First(&p, pass.Id)
	if got.Regions != `["江苏省"]` || p.EndedAt != 0 {
		t.Fatalf("浙江的来连比上海的使用更晚，不该改成上海，上海还在用也不该结束通行证: regions=%s pass=%+v status=%+v",
			got.Regions, p, AutoTravelStatuses()[in.Id])
	}

	in2 := setupPassInbound(t, 60532, []string{"江苏省"})
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in2.Id).Update("auto_travel", true).Error; err != nil {
		t.Fatal(err)
	}
	ipOf = travelE2E(t)
	writeRegionGeoDat(t, []string{"江苏省"}) // 没有通行证：靠 dat 认出上海的来连是被地区规则拦下的
	masterSwitchOn(t)
	readyLowRisk(t, in2.Id)
	expired := &model.InboundRegionPass{InboundId: in2.Id, Auto: true, Regions: `["上海市"]`,
		GrantedAt: now.Add(-5 * 24 * time.Hour).UnixMilli(), Until: now.Add(-30 * time.Hour).UnixMilli(),
		EndedAt: now.Add(-30 * time.Hour).UnixMilli(), EndReason: model.RegionPassExpired}
	if err := database.GetDB().Create(expired).Error; err != nil {
		t.Fatal(err)
	}
	rows = []model.InboundIPHour{{InboundId: in2.Id, IP: ipOf("江苏省"), HourStart: hour(6 * 24 * time.Hour), ActiveSeconds: 1800, ActiveBytes: 50 << 20}}
	for d := 0; d < 4; d++ {
		rows = append(rows, model.InboundIPHour{InboundId: in2.Id, IP: ipOf("上海市"), HourStart: hour(31*time.Hour + time.Duration(d)*24*time.Hour),
			ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	}
	if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-5 * time.Minute).UnixMilli(), InboundId: in2.Id,
		SourceIP: ipOf("上海市"), Route: model.BlockOutboundTag, Accepted: true}).Error; err != nil {
		t.Fatal(err)
	}
	markObserved(now)
	if _, err := (&AutoTravelService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	if got, _ := (&InboundService{}).GetInbound(in2.Id); got.Regions != `["上海市"]` {
		t.Fatalf("上海的来源还在来连，人还在上海：应改成上海市: regions=%s status=%+v", got.Regions, AutoTravelStatuses()[in2.Id])
	}
}

// 双栈设备的 IPv6 来连在地区限制下一律被拦，查不到省份：它不说明人去了别处，不能否决长住改地区（代码审查指出：
// 不过滤的话，人在上海只要某一小时上海的放行不满 1 MB、IPv6 又在来连，就一直改不过去）。与 recentGeoBlocked 一样只看 IPv4。
func TestRunRelocationIgnoresIPv6Noise(t *testing.T) {
	in, _ := relocateFixture(t, 60533)
	ipOf := travelE2E(t)
	masterSwitchOn(t)
	readyLowRisk(t, in.Id)
	now := time.Now()
	hour := func(ago time.Duration) int64 { u := now.Add(-ago).Unix(); return u - u%3600 }
	rows := []model.InboundIPHour{{InboundId: in.Id, IP: ipOf("江苏省"), HourStart: hour(6 * 24 * time.Hour), ActiveSeconds: 1800, ActiveBytes: 50 << 20}}
	for d := 0; d < 5; d++ {
		// 上海最后一次放行在 2 小时前，IPv6 来连在 1 分钟前：一定落在更晚的小时。
		rows = append(rows, model.InboundIPHour{InboundId: in.Id, IP: ipOf("上海市"), HourStart: hour(time.Duration(d)*24*time.Hour + 2*time.Hour),
			ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	}
	if err := database.GetTrafficDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetAccessLogDB().Create(&model.AccessLog{Time: now.Add(-time.Minute).UnixMilli(), InboundId: in.Id,
		SourceIP: "2001:db8::77", Route: model.BlockOutboundTag, Accepted: true}).Error; err != nil {
		t.Fatal(err)
	}
	markObserved(now)
	if _, err := (&AutoTravelService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	if got, _ := (&InboundService{}).GetInbound(in.Id); got.Regions != `["上海市"]` {
		t.Fatalf("IPv6 来连不该否决长住改地区: regions=%s status=%+v", got.Regions, AutoTravelStatuses()[in.Id])
	}
}

// 通行证已经到期（额度用完）时改地区：没有生效中的通行证可结束；判定窗口里这个省到期的自动通行证改记成
// relocated（不再占本月额度）；状态行记下改去新省的时刻。
func TestRelocateWithoutLivePassRelabelsExpiredPasses(t *testing.T) {
	in := setupPassInbound(t, 60511, []string{"江苏省"})
	now := time.Now()
	expired := &model.InboundRegionPass{InboundId: in.Id, Auto: true, Regions: `["上海市"]`,
		GrantedAt: now.Add(-4 * 24 * time.Hour).UnixMilli(), Until: now.Add(-2 * 24 * time.Hour).UnixMilli(),
		EndedAt: now.Add(-2 * 24 * time.Hour).UnixMilli(), EndReason: model.RegionPassExpired}
	other := &model.InboundRegionPass{InboundId: in.Id, Auto: true, Regions: `["浙江省"]`,
		GrantedAt: now.Add(-6 * 24 * time.Hour).UnixMilli(), Until: now.Add(-5 * 24 * time.Hour).UnixMilli(),
		EndedAt: now.Add(-5 * 24 * time.Hour).UnixMilli(), EndReason: model.RegionPassExpired}
	for _, p := range []*model.InboundRegionPass{expired, other} {
		if err := database.GetDB().Create(p).Error; err != nil {
			t.Fatal(err)
		}
	}
	ok, err := relocate(in, nil, "上海市", "长住上海", now)
	if err != nil || !ok {
		t.Fatalf("relocate(nil pass) = %v, %v", ok, err)
	}
	got, _ := (&InboundService{}).GetInbound(in.Id)
	if got.Regions != `["上海市"]` {
		t.Errorf("地区 = %s，期望上海市", got.Regions)
	}
	var a, b model.InboundRegionPass
	database.GetDB().First(&a, expired.Id)
	database.GetDB().First(&b, other.Id)
	if a.EndReason != model.RegionPassRelocated {
		t.Errorf("上海市那张到期的自动通行证应改记成 relocated，实际 %q", a.EndReason)
	}
	if b.EndReason != model.RegionPassExpired {
		t.Errorf("别的省的通行证不该被改记，实际 %q", b.EndReason)
	}
	st, _ := (&AutoRegionService{}).stateOf(in.Id)
	if st == nil || st.RelocatedAt != now.UnixMilli() || st.PrevRegions != `["江苏省"]` {
		t.Errorf("状态行 = %+v，期望记下改去新省的时刻与原地区", st)
	}
}

// 因「换了地方」结束的这个省的自动通行证（出差先到这里、去过别处又回来），改地区后同样算「住在那里」，
// 不再占本月的自动放行额度。
func TestRelocateRelabelsMovedPassesOfThatProvince(t *testing.T) {
	in := setupPassInbound(t, 60513, []string{"江苏省"})
	now := time.Now()
	moved := &model.InboundRegionPass{InboundId: in.Id, Auto: true, Regions: `["上海市"]`,
		GrantedAt: now.Add(-4 * 24 * time.Hour).UnixMilli(), Until: now.Add(-24 * time.Hour).UnixMilli(),
		EndedAt: now.Add(-2 * 24 * time.Hour).UnixMilli(), EndReason: model.RegionPassMoved}
	if err := database.GetDB().Create(moved).Error; err != nil {
		t.Fatal(err)
	}
	if ok, err := relocate(in, nil, "上海市", "长住上海", now); err != nil || !ok {
		t.Fatalf("relocate(nil pass) = %v, %v", ok, err)
	}
	var got model.InboundRegionPass
	database.GetDB().First(&got, moved.Id)
	if got.EndReason != model.RegionPassRelocated {
		t.Errorf("上海市那张换了地方结束的自动通行证应改记成 relocated，实际 %q", got.EndReason)
	}
}

// 搬回原来的地区（出差超过 N 天先改过去、回家后改回来）不刷新「30 天内只改去一个新省」的时刻，
// 也保留系统替它打开过自动出差放行的记录。
func TestRelocateBackHomeKeepsRelocatedAtAndAdoptedAt(t *testing.T) {
	in, pass := relocateFixture(t, 60512)
	now := time.Now()
	firstAt := now.Add(-6 * 24 * time.Hour).UnixMilli()
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		Regions: `["江苏省"]`, PrevRegions: `["上海市"]`, RelocatedAt: firstAt, AdoptedAt: firstAt - 1000,
		UpdatedAt: firstAt}).Error; err != nil {
		t.Fatal(err)
	}
	if ok, err := relocate(in, pass, "上海市", "回到上海", now); !ok || err != nil {
		t.Fatalf("relocate = %v, %v", ok, err)
	}
	st, _ := (&AutoRegionService{}).stateOf(in.Id)
	if st == nil || st.RelocatedAt != firstAt || st.AdoptedAt != firstAt-1000 {
		t.Errorf("状态行 = %+v，搬回原地区不该刷新 RelocatedAt，也不该丢 AdoptedAt", st)
	}
}

// 打开了「不自动设置地区限制」的入站：长住外地也不自动改地区。
func TestRelocateRespectsOptOut(t *testing.T) {
	in, pass := relocateFixture(t, 60505)
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in.Id).Update("no_auto_region", true).Error; err != nil {
		t.Fatal(err)
	}
	if ok, err := relocate(in, pass, "上海市", "长住上海", time.Now()); ok || err != nil {
		t.Fatalf("打开了开关，应静默跳过: ok=%v err=%v", ok, err)
	}
	if got, _ := (&InboundService{}).GetInbound(in.Id); got.Regions != `["江苏省"]` {
		t.Errorf("地区不该变: %s", got.Regions)
	}
}

// 老库加列后存量行的 no_auto_region 是 NULL：照常参与（UPDATE 的条件按 coalesce 写，不能写成 = false）。
func TestRelocateTreatsNullOptOutAsParticipating(t *testing.T) {
	in, pass := relocateFixture(t, 60506)
	if err := database.GetDB().Exec("UPDATE inbounds SET no_auto_region = NULL WHERE id = ?", in.Id).Error; err != nil {
		t.Fatal(err)
	}
	if ok, err := relocate(in, pass, "上海市", "长住上海", time.Now()); !ok || err != nil {
		t.Fatalf("NULL 应当按「参与」处理: ok=%v err=%v", ok, err)
	}
}
