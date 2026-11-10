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
	}
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
		{"共享风险中", func(in *relocateInput) { in.RiskLevel = RiskLevelMedium }},
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
