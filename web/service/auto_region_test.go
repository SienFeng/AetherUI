package service

import (
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

var arNow = time.Date(2026, 11, 20, 10, 30, 0, 0, atLoc)

func arHour(daysAgo int) int64 {
	t := arNow.Add(-time.Duration(daysAgo) * 24 * time.Hour).Unix()
	return t - t%3600
}

// homeRowsFor 是从今天往前 days 个不同日子在江苏的实质流量（含此刻所在的小时：最近也在用）。
func homeRowsFor(days int) []model.InboundIPHour {
	var rows []model.InboundIPHour
	for d := 0; d < days; d++ {
		rows = append(rows, model.InboundIPHour{IP: "192.0.2.9", HourStart: arHour(d), Province: "江苏省",
			ActiveSeconds: 1800, ActiveBytes: 50 << 20})
	}
	return rows
}

func arInput() autoRegionInput {
	return autoRegionInput{
		Now: arNow, Loc: atLoc, Rows: homeRowsFor(5),
		Params:             defaultAutoRegionParams(),
		UnplaceablePercent: func([]string) (float64, error) { return 0, nil },
		AllowedSet:         arSetFor,
	}
}

// arSetFor 是测试里的允许集：江苏省 = 192.0.2.0/24，河南省 = 198.51.100.0/24，其余省份没有网段。
func arSetFor(regions []string) (*cidrSet, error) {
	var cidrs []string
	for _, r := range regions {
		switch r {
		case "江苏省":
			cidrs = append(cidrs, "192.0.2.0/24")
		case "河南省":
			cidrs = append(cidrs, "198.51.100.0/24")
		}
	}
	return newCIDRSet(cidrs), nil
}

func TestAutoRegionWaitsDuringObservation(t *testing.T) {
	in := arInput() // 第一次实质使用在 4 天前，观察期设为 7 天
	in.Params.ObserveDays = 7
	if d := decideAutoRegion(in); d.Action != autoRegionWait {
		t.Fatalf("观察期未满应当继续观察: %+v", d)
	}
}

func TestAutoRegionWaitsWithoutStableDays(t *testing.T) {
	in := arInput()
	in.Params.ObserveDays = 7
	// 观察期满（7 天前用过、今天也在用），但只有这 2 天有实质流量
	in.Rows = []model.InboundIPHour{
		{IP: "192.0.2.9", HourStart: arHour(7), Province: "江苏省", ActiveSeconds: 1800, ActiveBytes: 50 << 20},
		{IP: "192.0.2.9", HourStart: arHour(0), Province: "江苏省", ActiveSeconds: 1800, ActiveBytes: 50 << 20},
	}
	if d := decideAutoRegion(in); d.Action != autoRegionWait || !strings.Contains(d.Reason, "天数不足") {
		t.Fatalf("观察 7 天、稳定天数 3，近 7 天只有 2 天数据应当继续观察: %+v", d)
	}
}

func TestAutoRegionAppliesWhenClean(t *testing.T) {
	d := decideAutoRegion(arInput())
	if d.Action != autoRegionApply || !reflect.DeepEqual(d.Regions, []string{"江苏省"}) {
		t.Fatalf("条件全部满足应当自动设置江苏省: %+v", d)
	}
}

// 设置的那个省自己要用满「稳定使用天数」才设置：自动出差放行要求家里近 7 天有这么多天才发通行证，
// 锁得比它早的话，新用户头几天出门就被拦着、没有任何自动途径放行；开通第一天恰好在外地的人还会被
// 锁在外地省，回家以后一直进不来（那个省永远凑不够天数，又没发过通行证，长住改地区也没有依据）。
func TestAutoRegionWaitsUntilTargetProvinceIsStable(t *testing.T) {
	away := func(daysAgo int) model.InboundIPHour {
		return model.InboundIPHour{IP: "198.51.100.7", HourStart: arHour(daysAgo), Province: "河南省",
			ActiveSeconds: 1800, ActiveBytes: 50 << 20}
	}
	// 开通后头两天都在外地河南：观察期（1 天）已满、最近也在用，但河南只有 2 天。
	firstDaysAway := arInput()
	firstDaysAway.Rows = []model.InboundIPHour{away(1), away(0)}
	if d := decideAutoRegion(firstDaysAway); d.Action != autoRegionWait ||
		!strings.Contains(d.Reason, "河南省") || !strings.Contains(d.Reason, "天数不足") {
		t.Fatalf("河南只用了 2 天，不到稳定使用天数 3：应继续观察: %+v", d)
	}
	// 新用户在家江苏用了 2 天：先不锁；用满 3 天再设置。
	newUser := arInput()
	newUser.Rows = homeRowsFor(2)
	if d := decideAutoRegion(newUser); d.Action != autoRegionWait {
		t.Fatalf("江苏只用了 2 天：应继续观察: %+v", d)
	}
	newUser.Rows = homeRowsFor(3)
	if d := decideAutoRegion(newUser); d.Action != autoRegionApply || !reflect.DeepEqual(d.Regions, []string{"江苏省"}) {
		t.Fatalf("江苏用满 3 天：应设置为江苏省: %+v", d)
	}
	// 稳定使用天数调成 1：2 天就够（出差放行同样只要 1 天）。
	newUser.Rows = homeRowsFor(2)
	newUser.Params.StableDays = 1
	if d := decideAutoRegion(newUser); d.Action != autoRegionApply {
		t.Fatalf("稳定使用天数设为 1 时 2 天就够: %+v", d)
	}
}

// 用到两个省、主省份自己不满稳定使用天数：同样先不设——选错了家又没发过通行证，就再也改不回来。
func TestAutoRegionDominantProvinceMustAlsoBeStable(t *testing.T) {
	in := arInput()
	in.Rows = nil
	for _, d := range []int{3, 4} { // 江苏 2 天，时长短
		in.Rows = append(in.Rows, model.InboundIPHour{IP: "192.0.2.9", HourStart: arHour(d), Province: "江苏省",
			ActiveSeconds: 600, ActiveBytes: 20 << 20})
	}
	for _, d := range []int{0, 1} { // 河南 2 天，时长长：天数打平按时长选中河南
		in.Rows = append(in.Rows, model.InboundIPHour{IP: "198.51.100.7", HourStart: arHour(d), Province: "河南省",
			ActiveSeconds: 3000, ActiveBytes: 80 << 20})
	}
	if d := decideAutoRegion(in); d.Action != autoRegionWait || !strings.Contains(d.Reason, "河南省") {
		t.Fatalf("主省份河南只有 2 天：应继续观察: %+v", d)
	}
	// 天数按执行同口径的允许集数：快照省份写的是河南、IP 却不在河南网段里的行不算河南的天数。
	in = arInput()
	in.Rows = []model.InboundIPHour{
		{IP: "203.0.113.5", HourStart: arHour(2), Province: "河南省", ActiveSeconds: 1800, ActiveBytes: 50 << 20},
		{IP: "198.51.100.7", HourStart: arHour(1), Province: "河南省", ActiveSeconds: 1800, ActiveBytes: 50 << 20},
		{IP: "198.51.100.7", HourStart: arHour(0), Province: "河南省", ActiveSeconds: 1800, ActiveBytes: 50 << 20},
	}
	if d := decideAutoRegion(in); d.Action != autoRegionWait {
		t.Fatalf("按河南网段只有 2 天：应继续观察: %+v", d)
	}
	// 允许集建不出来：这一轮判定不了。
	in = arInput()
	in.AllowedSet = func([]string) (*cidrSet, error) { return nil, errors.New("库坏了") }
	if d := decideAutoRegion(in); d.Action != autoRegionUnsure {
		t.Fatalf("允许集建不出来时这一轮不判: %+v", d)
	}
}




// 建议里有省份出现在并存记录中：不再交管理员确认，只放最常用的那一个省——一起写进去会把共享者的省
// 「合法化」，其余省份交给有防共享门槛的自动出差放行。
func TestAutoRegionCoexistSetsDominantProvinceOnly(t *testing.T) {
	in := arInput()
	for d := 1; d <= 5; d++ {
		in.Rows = append(in.Rows, model.InboundIPHour{IP: "198.51.100.7", HourStart: arHour(d), Province: "河南省",
			ActiveSeconds: 1700, ActiveBytes: 40 << 20})
	}
	d := decideAutoRegion(in)
	if d.Action != autoRegionApply || !reflect.DeepEqual(d.Regions, []string{"江苏省"}) || !strings.Contains(d.Reason, "河南省") {
		t.Fatalf("有异地并存：只设置最常用的江苏省，原因里点名河南省: %+v", d)
	}
}

func TestAutoRegionRiskStillSetsOneProvince(t *testing.T) {
	for _, level := range []string{RiskLevelMedium, RiskLevelHigh, RiskLevelSevere} {
		in := arInput()
		in.RiskLevel = level
		if d := decideAutoRegion(in); d.Action != autoRegionApply || !reflect.DeepEqual(d.Regions, []string{"江苏省"}) {
			t.Errorf("风险 %s 也照样只设一个省: %+v", level, d)
		}
	}
}

// 设置之后会被拦、而且自动出差放行也救不了的流量（来源没有可放行的省份）太多：没有共享迹象就继续观察，
// 有共享迹象照样设置。
func TestAutoRegionUnplaceableTrafficGate(t *testing.T) {
	in := arInput()
	in.UnplaceablePercent = func([]string) (float64, error) { return 12, nil }
	d := decideAutoRegion(in)
	if d.Action != autoRegionWait || !strings.Contains(d.Reason, "12%") {
		t.Fatalf("会有 12%% 救不了的流量被拦（上限 5%%）、没有共享迹象：应继续观察: %+v", d)
	}
	in.RiskLevel = RiskLevelMedium
	if d := decideAutoRegion(in); d.Action != autoRegionApply {
		t.Fatalf("有共享迹象时照样设置: %+v", d)
	}
	in.RiskLevel = ""
	in.UnplaceablePercent = func([]string) (float64, error) { return 0, errors.New("库坏了") }
	if d := decideAutoRegion(in); d.Action != autoRegionUnsure {
		t.Fatalf("算不出来时这一轮不判: %+v", d)
	}
}

// 用到了多个省：数据不足 3 天时多看两天，主省份才认得准；之后只设主省份，评分就绪与否都一样。
func TestAutoRegionMultiProvinceWaitsThreeDaysThenSetsDominant(t *testing.T) {
	short := arInput()
	short.Rows = homeRowsFor(2)
	short.Rows = append(short.Rows, model.InboundIPHour{IP: "198.51.100.7", HourStart: arHour(1) + 2*3600, Province: "河南省",
		ActiveSeconds: 1700, ActiveBytes: 40 << 20})
	if d := decideAutoRegion(short); d.Action != autoRegionWait || !strings.Contains(d.Reason, "3 天") {
		t.Fatalf("多省、数据只有 2 天：应继续观察: %+v", d)
	}
	in := arInput()
	for d := 1; d <= 5; d++ {
		in.Rows = append(in.Rows, model.InboundIPHour{IP: "198.51.100.7", HourStart: arHour(d) + 2*3600, Province: "河南省",
			ActiveSeconds: 1700, ActiveBytes: 40 << 20})
	}
	if d := decideAutoRegion(in); d.Action != autoRegionApply || !reflect.DeepEqual(d.Regions, []string{"江苏省"}) {
		t.Errorf("多省只设主省份江苏省: %+v", d)
	}
}

func TestAutoRegionWaitsWhenAccessLogOff(t *testing.T) {
	in := arInput()
	in.AccessLogOff = true
	if d := decideAutoRegion(in); d.Action != autoRegionWait || !strings.Contains(d.Reason, "访问日志") {
		t.Fatalf("访问日志没开：设置之后被拦的人发现不了，先不设置: %+v", d)
	}
}

func TestDominantProvince(t *testing.T) {
	row := func(daysAgo int, province string, secs int, mb int64) model.InboundIPHour {
		return model.InboundIPHour{IP: "198.51.100.1", HourStart: arHour(daysAgo), Province: province, ActiveSeconds: secs, ActiveBytes: mb << 20}
	}
	rows := []model.InboundIPHour{
		row(0, "江苏省", 600, 5), row(1, "江苏省", 600, 5), row(2, "江苏省", 600, 5),
		row(0, "河南省", 9000, 90), row(1, "河南省", 9000, 90),
		row(3, "河南省", 9000, 0), // 只有零星字节的小时不算一天
	}
	if got := dominantProvince(rows, []string{"河南省", "江苏省"}, atLoc); got != "江苏省" {
		t.Errorf("天数优先：江苏 3 天胜过河南 2 天，得到 %s", got)
	}
	rows = append(rows, row(4, "河南省", 9000, 90))
	if got := dominantProvince(rows, []string{"河南省", "江苏省"}, atLoc); got != "河南省" {
		t.Errorf("天数打平按活跃时长：得到 %s", got)
	}
	if got := dominantProvince(rows, []string{"江苏省"}, atLoc); got != "江苏省" {
		t.Errorf("只在建议的省份里选：得到 %s", got)
	}
}

func TestEligibleForAutoRegion(t *testing.T) {
	plain := &model.Inbound{Id: 1, Enable: true, Regions: "[]"}
	cases := []struct {
		name string
		in   *model.Inbound
		st   *model.InboundAutoRegion
		want bool
	}{
		{"新入站", plain, nil, true},
		{"pending 每轮重算", plain, &model.InboundAutoRegion{State: model.AutoRegionPending}, true},
		{"已停用", &model.Inbound{Id: 1, Enable: false, Regions: "[]"}, nil, false},
		{"已有地区", &model.Inbound{Id: 1, Enable: true, Regions: `["江苏省"]`}, nil, false},
		{"管理员接手", plain, &model.InboundAutoRegion{State: model.AutoRegionManual}, false},
		{"已自动设置后被清空", plain, &model.InboundAutoRegion{State: model.AutoRegionApplied}, false},
	}
	for _, c := range cases {
		if got := eligibleForAutoRegion(c.in, c.st); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func autoRegionStateOf(t *testing.T, id int) *model.InboundAutoRegion {
	t.Helper()
	var rows []model.InboundAutoRegion
	if err := database.GetDB().Where("inbound_id = ?", id).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// 管理员在编辑表单里改过地区，以后就不能再被自动设回去。（自动设置后 7 天内清空地区要走
// 「撤销」，见 TestUpdateInboundRejectsStaleWriteOverAutoRegion；7 天后在表单里清空，见
// TestUpdateInboundAllowsClearingOldAutoRegion。）
func TestUpdateInboundRegionsMarksManual(t *testing.T) {
	in := setupPassInbound(t, 60401, []string{"江苏省"})
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		Regions: `["江苏省"]`, UpdatedAt: time.Now().UnixMilli()}).Error)
	got := mustGet(t, in.Id)
	got.Regions = `["上海市"]`
	must((&InboundService{}).UpdateInbound(got))
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionManual {
		t.Fatalf("改过地区后应标为 manual: %+v", st)
	}
	// 只改备注不算接手
	other := setupPassInbound(t, 60402, nil)
	must(database.GetDB().Create(&model.InboundAutoRegion{InboundId: other.Id, State: model.AutoRegionPending}).Error)
	o := mustGet(t, other.Id)
	o.Remark = "改备注"
	must((&InboundService{}).UpdateInbound(o))
	if st := autoRegionStateOf(t, other.Id); st == nil || st.State != model.AutoRegionPending {
		t.Fatalf("只改备注不该改变自动设置状态: %+v", st)
	}
}

func TestUndoClearsRegionsAutoTravelAndPass(t *testing.T) {
	in := setupPassInbound(t, 60403, []string{"江苏省"})
	db := database.GetDB()
	if err := db.Model(&model.Inbound{}).Where("id = ?", in.Id).UpdateColumn("auto_travel", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied, Regions: `["江苏省"]`}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), true, now); err != nil {
		t.Fatal(err)
	}
	if err := (&AutoRegionService{}).Undo(in.Id, now); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	got := mustGet(t, in.Id)
	if model.HasRegions(got.Regions) || got.AutoTravel {
		t.Errorf("撤销后地区与自动出差都应清掉: regions=%s autoTravel=%v", got.Regions, got.AutoTravel)
	}
	if p, _ := (&RegionPassService{}).ActiveOf(in.Id); p != nil {
		t.Error("撤销后活动通行证应结束")
	}
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionManual {
		t.Errorf("撤销后应标为 manual: %+v", st)
	}
}

func TestDelInboundDeletesAutoRegionState(t *testing.T) {
	in := setupPassInbound(t, 60404, nil)
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionPending}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	if st := autoRegionStateOf(t, in.Id); st != nil {
		t.Errorf("删除入站后状态仍在（SQLite 会复用 id）: %+v", st)
	}
}

// 默认观察 1 天：只有 1 天的数据也能判定，稳定天数要求不超过观察天数。
func TestAutoRegionDefaultObservesOneDay(t *testing.T) {
	if defaultAutoRegionParams().ObserveDays != 1 {
		t.Fatalf("默认观察天数 = %d，期望 1", defaultAutoRegionParams().ObserveDays)
	}
	in := arInput()
	in.Rows = homeRowsFor(2) // 昨天第一次用，今天也在用：观察期已满
	// 2026-11-10 管理员选定：观察期满还要等这个省用满稳定使用天数（默认 3），否则锁上之后出差放行用不了。
	if d := decideAutoRegion(in); d.Action != autoRegionWait || !strings.Contains(d.Reason, "天数不足") {
		t.Fatalf("观察 1 天已满、江苏只用了 2 天：应等用满 3 天: %+v", d)
	}
	in.Rows = homeRowsFor(3)
	if d := decideAutoRegion(in); d.Action != autoRegionApply {
		t.Fatalf("观察期满、江苏用满 3 天的干净数据应当自动设置: %+v", d)
	}
}

// 本地端到端运行时发现：最早时刻的查询曾恒返回 0，所有用户永远停在「观察中」。
// 现在一轮一次查出所有候选入站的最早时刻，没有记录的入站不在结果里。
func TestEarliestHoursOf(t *testing.T) {
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
	if got, err := earliestHoursOf([]int{9}); err != nil || len(got) != 0 {
		t.Fatalf("没有记录时结果应为空: got=%v err=%v", got, err)
	}
	for _, r := range []struct {
		id int
		h  int64
	}{{9, 7200}, {9, 3600}, {9, 10800}, {10, 18000}, {11, 3600}} {
		if err := database.GetTrafficDB().Create(&model.InboundIPHour{InboundId: r.id, IP: "192.0.2.1", HourStart: r.h}).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := earliestHoursOf([]int{9, 10, 12})
	if err != nil {
		t.Fatal(err)
	}
	if got[9] != 3600 || got[10] != 18000 || len(got) != 2 {
		t.Fatalf("应只含请求的、有记录的入站，且取各自最早一条: %v", got)
	}
}

// 任务本轮开头读到的快照还是 pending，管理员在这之后点了「忽略」（库里已是 manual）：
// 任务不能再写地区，也不能把状态改回 pending / applied——那正是「覆盖管理员的决定」。
func TestApplyDecisionRespectsAdminTakeover(t *testing.T) {
	in := setupPassInbound(t, 60405, nil)
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionManual}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := &model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionPending, Regions: `["江苏省"]`, Reason: "旧原因"}
	s := &AutoRegionService{}
	for _, d := range []autoRegionDecision{
		{Action: autoRegionApply, Regions: []string{"江苏省"}, Reason: "使用记录稳定"},
		{Action: autoRegionWait, Reason: "数据不足"},
	} {
		applied, err := s.applyDecision(mustGet(t, in.Id), snapshot, d, time.Now())
		if applied || err != nil {
			t.Errorf("%s: 管理员已接手，任务应当静默跳过: applied=%v err=%v", d.Action, applied, err)
		}
	}
	if got := mustGet(t, in.Id); model.HasRegions(got.Regions) || got.AutoTravel {
		t.Errorf("管理员忽略之后任务仍写了地区: regions=%s autoTravel=%v", got.Regions, got.AutoTravel)
	}
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionManual {
		t.Errorf("管理员的决定被任务改掉了: %+v", st)
	}
}



// 列表页停在自动设置之前：切换启用 / 重置流量 / 编辑保存都会整行写回，带着设置前的空地区。
// 放过去的话刚设置的地区被清掉，入站还被当成「管理员改过地区」而永远不再自动设置。
func TestUpdateInboundRejectsStaleWriteOverAutoRegion(t *testing.T) {
	in := setupPassInbound(t, 60408, nil)
	stale := mustGet(t, in.Id) // 列表页加载时的样子：还没有地区
	db := database.GetDB()
	if err := db.Model(&model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumns(map[string]any{"regions": `["江苏省"]`, "auto_travel": true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		Regions: `["江苏省"]`, UpdatedAt: time.Now().UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}

	stale.Enable = false // 在没刷新的页面上切了一下启用开关
	if err := (&InboundService{}).UpdateInbound(stale); err == nil || !strings.Contains(err.Error(), "刷新") {
		t.Fatalf("过期页面的整行写回应当被拒并提示刷新: %v", err)
	}
	got := mustGet(t, in.Id)
	if !sameRegions(got.Regions, `["江苏省"]`) || !got.AutoTravel || !got.Enable {
		t.Errorf("被拒的写回仍改了库: regions=%s autoTravel=%v enable=%v", got.Regions, got.AutoTravel, got.Enable)
	}
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionApplied {
		t.Errorf("被拒的写回改了自动设置状态: %+v", st)
	}

	// 刷新之后，表单带着自动设置的地区，照常保存，状态不变。
	fresh := mustGet(t, in.Id)
	fresh.Remark = "刷新后改备注"
	if err := (&InboundService{}).UpdateInbound(fresh); err != nil {
		t.Fatalf("刷新后的保存不该被拦: %v", err)
	}
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionApplied {
		t.Errorf("只改备注不该改变自动设置状态: %+v", st)
	}
}

// 自动设置超过 autoRegionAppliedVisible 之后，列表上已经没有撤销入口，表单是唯一的修改途径，不能再拦。
func TestUpdateInboundAllowsClearingOldAutoRegion(t *testing.T) {
	in := setupPassInbound(t, 60409, []string{"江苏省"})
	old := time.Now().Add(-autoRegionAppliedVisible - time.Hour)
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		Regions: `["江苏省"]`, UpdatedAt: old.UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, in.Id)
	got.Regions = "[]"
	if err := (&InboundService{}).UpdateInbound(got); err != nil {
		t.Fatalf("超过撤销入口的时限后，表单清空地区不该被拦: %v", err)
	}
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionManual {
		t.Errorf("管理员在表单里清空地区后应标为 manual: %+v", st)
	}
}




// 打开了开关却没加载 IP 库：Run 要把原因报出来（任务据此记日志），不能静默地什么都不做。
func TestAutoRegionRunReportsMissingIPDB(t *testing.T) {
	setupGeoDB(t)
	useTestSources(t, nil) // 一个源都没有 = IP 库未加载
	if err := database.GetDB().Create(&model.Setting{Key: "autoRegionEnable", Value: "1"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AutoRegionService{}).Run(time.Now()); !errors.Is(err, ErrAutoRegionNoIPDB) {
		t.Fatalf("应当报告 IP 库未加载: %v", err)
	}
}


// 整轮端到端：真实的主库、用量库、IP 库与设置，跑一轮 Run 之后各类入站各得其所。
// 排在最前面的入站用量数据读不出来，不能拖累其余入站（曾经一个出错整轮中断）。
func TestAutoRegionRunEndToEnd(t *testing.T) {
	setupGeoDB(t)
	requireIPDB(t)
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
	db, tdb := database.GetDB(), database.GetTrafficDB()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Create(&model.Setting{Key: "autoRegionEnable", Value: "1"}).Error)
	must((&SettingService{}).SetAccessLogEnable(true))

	// 江苏省允许集里的一个地址，运行时从 IP 库取：仓库里不写任何真实 IP。
	cidrs := regionCIDRSource((&IPDBService{}).DB(), &SettingService{}).CIDRsOfProvinces([]string{"江苏省"})
	if len(cidrs) == 0 {
		t.Skip("IP 库里没有江苏省的段")
	}
	_, block, err := net.ParseCIDR(cidrs[0])
	must(err)
	home := block.IP.String()

	now := time.Now()
	hourOf := func(daysAgo int) int64 {
		u := now.Add(-time.Duration(daysAgo) * 24 * time.Hour).Unix()
		return u - u%3600
	}
	useDays := func(in *model.Inbound, ip, province string, days int) {
		for d := 0; d < days; d++ {
			must(tdb.Create(&model.InboundIPHour{InboundId: in.Id, IP: ip, HourStart: hourOf(d), Province: province,
				ActiveSeconds: 1800, ActiveBytes: 50 << 20}).Error)
		}
	}

	broken := mkRegionInbound(t, 60420, true, nil) // 最先处理：用量数据读不出来
	clean := mkRegionInbound(t, 60421, true, nil)  // 只在江苏用 → 自动设置
	both := mkRegionInbound(t, 60422, true, nil)   // 江苏、河南同一小时都在用 → 只设江苏（主省份）
	off := mkRegionInbound(t, 60423, false, nil)   // 停用 → 不动
	taken := mkRegionInbound(t, 60424, true, nil)  // 管理员接手过 → 不动
	idle := mkRegionInbound(t, 60425, true, nil)   // 从没用过 → 不写状态
	stale := mkRegionInbound(t, 60426, true, nil)  // 升级前留下的待确认行 → 撤下

	useDays(broken, home, "江苏省", 3)
	must(tdb.Exec("INSERT INTO inbound_ip_hours (inbound_id, ip, hour_start, province, active_seconds, active_bytes) VALUES (?, ?, 'x', '江苏省', 1, 1)",
		broken.Id, "192.0.2.99").Error)
	useDays(clean, home, "江苏省", 5)
	useDays(both, home, "江苏省", 5)
	useDays(both, "198.51.100.7", "河南省", 5)
	useDays(off, home, "江苏省", 5)
	useDays(taken, home, "江苏省", 5)
	must(tdb.Create(&model.InboundIPHour{InboundId: stale.Id, IP: home, HourStart: hourOf(20), Province: "江苏省",
		ActiveSeconds: 1800, ActiveBytes: 50 << 20}).Error)
	must(db.Create(&model.InboundAutoRegion{InboundId: taken.Id, State: model.AutoRegionManual}).Error)
	must(db.Create(&model.InboundAutoRegion{InboundId: stale.Id, State: model.AutoRegionPending, Regions: `["江苏省"]`, Reason: "旧原因"}).Error)

	changed, err := (&AutoRegionService{}).Run(now)
	if err != nil || !changed {
		t.Fatalf("一个入站出错不该拖累整轮: changed=%v err=%v", changed, err)
	}
	if got := mustGet(t, clean.Id); !sameRegions(got.Regions, `["江苏省"]`) || !got.AutoTravel {
		t.Errorf("只在江苏用的应当自动设置: regions=%s autoTravel=%v", got.Regions, got.AutoTravel)
	}
	if st := autoRegionStateOf(t, clean.Id); st == nil || st.State != model.AutoRegionApplied {
		t.Errorf("自动设置后状态应为 applied: %+v", st)
	}
	if st := autoRegionStateOf(t, both.Id); st == nil || st.State != model.AutoRegionApplied || !strings.Contains(st.Reason, "河南省") {
		t.Errorf("两省同时使用：只设最常用的省，原因里点名河南省: %+v", st)
	}
	if got := mustGet(t, both.Id); !sameRegions(got.Regions, `["江苏省"]`) {
		t.Errorf("两省同时使用应只设江苏省: %s", got.Regions)
	}
	for name, in := range map[string]*model.Inbound{"停用": off, "管理员接手": taken, "数据读不出来": broken} {
		if got := mustGet(t, in.Id); model.HasRegions(got.Regions) {
			t.Errorf("%s的入站不该被设置: %s", name, got.Regions)
		}
	}
	if st := autoRegionStateOf(t, off.Id); st != nil {
		t.Errorf("停用的入站不该有状态: %+v", st)
	}
	if st := autoRegionStateOf(t, taken.Id); st == nil || st.State != model.AutoRegionManual {
		t.Errorf("管理员接手的状态不该变: %+v", st)
	}
	if st := autoRegionStateOf(t, idle.Id); st != nil {
		t.Errorf("没用过的入站不该写状态: %+v", st)
	}
	if st := autoRegionStateOf(t, stale.Id); st != nil {
		t.Errorf("数据滑出窗口后旧建议应撤下: %+v", st)
	}
}


// 在表单里把地区清空：生效中的通行证一并结束。留着它，列表上会继续挂着一张说「到期后恢复入站
// 本身的地区限制」的通行证；以后再设地区时，它还会接着放行一个管理员没打算放的省。
func TestUpdateInboundClearingRegionsEndsActivePass(t *testing.T) {
	in := setupPassInbound(t, 60414, []string{"江苏省"})
	now := time.Now()
	if _, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, in.Id)
	got.Regions = "[]"
	if err := (&InboundService{}).UpdateInbound(got); err != nil {
		t.Fatal(err)
	}
	if p, _ := (&RegionPassService{}).ActiveOf(in.Id); p != nil {
		t.Errorf("清空地区后通行证仍在生效: %+v", p)
	}
}

// 预演是表单里的功能：IP 库没加载时，提示要说预演做不了，不能说成通行证的事。
func TestPreviewWithoutIPDBSaysWhatIsWrong(t *testing.T) {
	setupGeoDB(t)
	useTestSources(t, nil)
	in := mkRegionInbound(t, 60415, true, nil)
	_, err := (&RegionPreviewService{}).Preview(in.Id, []string{"江苏省"}, time.Now())
	if err == nil || strings.Contains(err.Error(), "通行证") || !strings.Contains(err.Error(), "IP 归属地库未加载") {
		t.Fatalf("提示应当说 IP 库未加载、与通行证无关: %v", err)
	}
}

// 一次性的会话不是使用习惯：管理员新建入站后自己测了一会儿，24 小时后不能按管理员所在的省
// 自动设置（真正的用户可能在别的省，一连上就被拦）。观察要求「第一次实质使用在观察期之前」
// 且「最近也在用」，一次会话不可能同时满足两头。
func TestAutoRegionIgnoresOneOffSession(t *testing.T) {
	in := arInput()
	in.Rows = []model.InboundIPHour{
		{IP: "192.0.2.9", HourStart: arHour(1) - 6*3600, Province: "江苏省", ActiveSeconds: 900, ActiveBytes: 20 << 20},
		{IP: "192.0.2.9", HourStart: arHour(1) - 5*3600, Province: "江苏省", ActiveSeconds: 900, ActiveBytes: 20 << 20},
	}
	if d := decideAutoRegion(in); d.Action != autoRegionWait {
		t.Fatalf("一天多以前的一次会话、之后没再用，不该自动设置: %+v", d)
	}
}

// 观察期按 7 天窗口里的实质使用算，不按库里保留的最早一条记录：一个闲置了一阵的入站，用户
// 这次第一次用（比如正在出差），不能因为几周前有过记录就当场按出差地设置。
func TestAutoRegionMeasuresObservationInsideWindow(t *testing.T) {
	in := arInput() // 库里可能保留着几周前的记录，但近 7 天只有今天在用
	in.Rows = []model.InboundIPHour{
		{IP: "192.0.2.9", HourStart: arHour(0), Province: "浙江省", ActiveSeconds: 1800, ActiveBytes: 50 << 20},
	}
	if d := decideAutoRegion(in); d.Action != autoRegionWait {
		t.Fatalf("近 7 天的使用还不满观察期，不该自动设置: %+v", d)
	}
}


// 本轮开头之后入站被删了：任务不能再给这个 id 写状态（SQLite 复用 id，下一个新入站会继承它），
// 也不该把它当成错误刷日志——静默跳过。
func TestApplyDecisionSkipsDeletedInbound(t *testing.T) {
	in := setupPassInbound(t, 60416, nil)
	snapshot := mustGet(t, in.Id)
	if err := (&InboundService{}).DelInbound(in.Id); err != nil {
		t.Fatal(err)
	}
	if applied, err := (&AutoRegionService{}).applyDecision(snapshot, nil,
		autoRegionDecision{Action: autoRegionApply, Regions: []string{"江苏省"}, Reason: "使用记录稳定"}, time.Now()); applied || err != nil {
		t.Fatalf("入站已删除应静默跳过: applied=%v err=%v", applied, err)
	}
	if st := autoRegionStateOf(t, in.Id); st != nil {
		t.Fatalf("已删除的入站留下了建议: %+v", st)
	}
}

// 关掉总开关：待确认的建议撤下（不再显示、不再计入提醒、也不能再被确认写入）；入站被删后
// 残留的行一并清掉。已自动设置与管理员接手的记录保留。
func TestAutoRegionRunCleansUpWhenSwitchedOffAndOrphans(t *testing.T) {
	in := setupPassInbound(t, 60417, nil)
	kept := mkRegionInbound(t, 60418, true, []string{"江苏省"})
	db := database.GetDB()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionPending, Regions: `["江苏省"]`}).Error)
	must(db.Create(&model.InboundAutoRegion{InboundId: kept.Id, State: model.AutoRegionApplied, Regions: `["江苏省"]`}).Error)
	must(db.Create(&model.InboundAutoRegion{InboundId: 999998, State: model.AutoRegionManual}).Error) // 孤儿
	if _, err := (&AutoRegionService{}).Run(time.Now()); err != nil {                                   // 开关默认关
		t.Fatal(err)
	}
	if st := autoRegionStateOf(t, in.Id); st != nil {
		t.Errorf("关掉开关后待确认的建议应撤下: %+v", st)
	}
	if st := autoRegionStateOf(t, kept.Id); st == nil || st.State != model.AutoRegionApplied {
		t.Errorf("已自动设置的记录应保留: %+v", st)
	}
	if st := autoRegionStateOf(t, 999998); st != nil {
		t.Errorf("入站已不存在的残留行应清掉: %+v", st)
	}
}

// 停用的入站：7 天内的「已自动设置」照样给出，管理员要能从标签撤销——撤销是自动设置后 7 天内
// 清空地区的唯一途径，提示也让他去点那个标签。
func TestInboundAttentionShowsAppliedOnDisabledInbound(t *testing.T) {
	setupAccessLogDB(t)
	in := mkRegionInbound(t, 50046, false, []string{"江苏省"})
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		Regions: `["江苏省"]`, UpdatedAt: time.Now().UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := (&InboundAttentionService{}).Summary(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a := got[in.Id]; a == nil || a.AutoRegion == nil || a.AutoRegion.State != model.AutoRegionApplied {
		t.Fatalf("停用入站的自动设置也要给出: %+v", a)
	}
}

// 撤销自动设置时，生效中的通行证在同一个事务里结束：留着的话，以后再设地区它会接着放行。
func TestUndoEndsPassWithinTheSameChange(t *testing.T) {
	in := setupPassInbound(t, 60419, []string{"江苏省"})
	db := database.GetDB()
	if err := db.Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied, Regions: `["江苏省"]`,
		UpdatedAt: time.Now().UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p, err := (&RegionPassService{}).Grant(in.Id, []string{"上海市"}, now.Add(time.Hour).UnixMilli(), false, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&AutoRegionService{}).Undo(in.Id, now); err != nil {
		t.Fatal(err)
	}
	var ended model.InboundRegionPass
	if err := db.Where("id = ?", p.Id).Take(&ended).Error; err != nil {
		t.Fatal(err)
	}
	if ended.EndedAt == 0 || ended.EndReason != model.RegionPassCleared {
		t.Errorf("撤销时通行证应以「清空了地区」结束: %+v", ended)
	}
}

// autoRegionRunEnv 打开总开关与访问日志、建好用量库，供整轮测试用。
func autoRegionRunEnv(t *testing.T) {
	t.Helper()
	setupGeoDB(t)
	requireIPDB(t)
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Setting{Key: "autoRegionEnable", Value: "1"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&SettingService{}).SetAccessLogEnable(true); err != nil {
		t.Fatal(err)
	}
}

func mkNoAuthSocks(t *testing.T, port int, regions string) *model.Inbound {
	t.Helper()
	in := &model.Inbound{UserId: 1, Port: port, Protocol: model.Protocol("socks"), Tag: "inbound-" + itoaS(port), Enable: true,
		Settings: `{"auth":"noauth","udp":false}`, StreamSettings: plainTCPStream, Sniffing: "{}", Regions: regions}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatal(err)
	}
	return in
}

// 总开关打开时，已有地区限制、从没被系统接管过的入站（v1.47.0 时手工设置的）一次性打开自动出差放行；
// 管理员之后关掉，系统不再改回来；无认证的入站不打开。
func TestRunAdoptsExistingRegionInbounds(t *testing.T) {
	autoRegionRunEnv(t)
	plain := mkRegionInbound(t, 60440, true, []string{"江苏省"})
	turnedOff := mkRegionInbound(t, 60441, true, []string{"江苏省"})
	noAuth := mkNoAuthSocks(t, 60442, `["江苏省"]`)
	disabled := mkRegionInbound(t, 60443, false, []string{"江苏省"})
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: turnedOff.Id, State: model.AutoRegionAdopted}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AutoRegionService{}).Run(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, plain.Id); !got.AutoTravel {
		t.Error("存量的地区入站应被接管：打开自动出差放行")
	}
	if st := autoRegionStateOf(t, plain.Id); st == nil || st.State != model.AutoRegionAdopted {
		t.Errorf("接管后应记一行 adopted: %+v", st)
	}
	if got := mustGet(t, turnedOff.Id); got.AutoTravel {
		t.Error("接管过、管理员又关掉的不该再被打开")
	}
	if got := mustGet(t, noAuth.Id); got.AutoTravel {
		t.Error("无认证的入站不该打开自动出差放行")
	}
	if got := mustGet(t, disabled.Id); got.AutoTravel {
		t.Error("停用的入站不动")
	}
	// 总开关关着：一个都不接管（升级后行为零变化）。
	autoRegionRunEnv(t)
	if err := database.GetDB().Model(&model.Setting{}).Where("key = ?", "autoRegionEnable").Update("value", "0").Error; err != nil {
		t.Fatal(err)
	}
	other := mkRegionInbound(t, 60444, true, []string{"江苏省"})
	if _, err := (&AutoRegionService{}).Run(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, other.Id); got.AutoTravel {
		t.Error("总开关关着不该接管")
	}
}

// 升级前留下的待确认行：每轮开头撤下，总开关开着也一样（现在不再有人去确认它）。
func TestRunDropsLegacyPendingRows(t *testing.T) {
	autoRegionRunEnv(t)
	in := mkRegionInbound(t, 60445, true, nil)
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionPending, Regions: `["江苏省"]`}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AutoRegionService{}).Run(time.Now()); err != nil {
		t.Fatal(err)
	}
	if st := autoRegionStateOf(t, in.Id); st != nil {
		t.Errorf("待确认行应被撤下: %+v", st)
	}
}

func TestWriteRegionsLeavesAutoTravelOffForAuthLess(t *testing.T) {
	setupGeoDB(t)
	requireIPDB(t)
	in := mkNoAuthSocks(t, 60446, "")
	err := writeRegions(in.Id, []string{"江苏省"}, &model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		UpdatedAt: time.Now().UnixMilli()}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, in.Id)
	if !sameRegions(got.Regions, `["江苏省"]`) || got.AutoTravel {
		t.Errorf("无认证的入站：地区照写，自动出差放行不开: regions=%s autoTravel=%v", got.Regions, got.AutoTravel)
	}
}

// 升级后、打开总开关之前，管理员在编辑里改过地区：那时写下的是 manual 行。改动前接管只看「有没有行」，
// 这些入站永远不会被打开自动出差放行，与总开关说明的「已有地区限制的用户自动打开出差放行」不符。
// 系统替它打开过一次（AdoptedAt 非 0）之后，管理员关掉就不再打开；之后再改地区（又写 manual）也不会。
func TestRunAdoptsManualRowsNotYetAdopted(t *testing.T) {
	autoRegionRunEnv(t)
	editedBefore := mkRegionInbound(t, 60450, true, []string{"江苏省"})
	turnedOffAfter := mkRegionInbound(t, 60451, true, []string{"江苏省"})
	db := database.GetDB()
	if err := db.Create(&model.InboundAutoRegion{InboundId: editedBefore.Id, State: model.AutoRegionManual}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.InboundAutoRegion{InboundId: turnedOffAfter.Id, State: model.AutoRegionManual,
		AdoptedAt: time.Now().Add(-time.Hour).UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AutoRegionService{}).Run(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, editedBefore.Id); !got.AutoTravel {
		t.Error("打开总开关之前改过地区的入站也应被接管")
	}
	st := autoRegionStateOf(t, editedBefore.Id)
	if st == nil || st.AdoptedAt == 0 || st.State != model.AutoRegionManual {
		t.Errorf("接管后应记下 AdoptedAt，状态仍是 manual（自动设置不再碰它的地区）: %+v", st)
	}
	if got := mustGet(t, turnedOffAfter.Id); got.AutoTravel {
		t.Error("系统替它打开过、管理员又关掉的不该再被打开")
	}
}

// 管理员改地区（MarkManual）不能抹掉「系统替它打开过自动出差放行」与「最近一次改去新省」的记录：
// 抹掉了，接管会再打开一次管理员关掉的开关，30 天只改一个新省的限制也会被绕开。
func TestMarkManualKeepsAdoptedAtAndRelocatedAt(t *testing.T) {
	setupDB(t)
	db := database.GetDB()
	at := time.Now().Add(-2 * time.Hour).UnixMilli()
	if err := db.Create(&model.InboundAutoRegion{InboundId: 7, State: model.AutoRegionApplied, Regions: `["上海市"]`,
		PrevRegions: `["江苏省"]`, AdoptedAt: at, RelocatedAt: at}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&AutoRegionService{}).MarkManual(db, 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, _ := (&AutoRegionService{}).stateOf(7)
	if st == nil || st.State != model.AutoRegionManual || st.AdoptedAt != at || st.RelocatedAt != at {
		t.Errorf("MarkManual 之后 = %+v，期望 manual 且保留 AdoptedAt/RelocatedAt", st)
	}
}

// 自动设置地区时一并打开了自动出差放行：记下 AdoptedAt，管理员之后关掉、再改地区，也不会被接管重新打开。
func TestAutoSetRecordsAdoptedAt(t *testing.T) {
	autoRegionRunEnv(t)
	in := mkRegionInbound(t, 60452, true, nil)
	st := &model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied, Reason: "测试", UpdatedAt: time.Now().UnixMilli()}
	if err := writeRegions(in.Id, []string{"江苏省"}, st, true); err != nil {
		t.Fatalf("writeRegions: %v", err)
	}
	if got := autoRegionStateOf(t, in.Id); got == nil || got.AdoptedAt == 0 {
		t.Errorf("自动设置并打开自动出差放行后应记下 AdoptedAt: %+v", got)
	}
}

// 撤销会关掉这个入站的自动出差放行，接管不能再把它打开：长住改地区可能抢在第一次接管之前发生，那时
// 状态行的 AdoptedAt 还是 0，撤销把它变成 manual 之后正好符合「manual 且没接管过」——10 分钟内又被打开。
func TestUndoIsNotReadoptedAfterRelocationBeforeAdoption(t *testing.T) {
	autoRegionRunEnv(t)
	in := mkRegionInbound(t, 60460, true, []string{"浙江省"})
	db := database.GetDB()
	if err := db.Model(&model.Inbound{}).Where("id = ?", in.Id).UpdateColumn("auto_travel", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied, Regions: `["浙江省"]`,
		PrevRegions: `["江苏省"]`, RelocatedAt: time.Now().UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&AutoRegionService{}).Undo(in.Id, time.Now()); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if _, err := (&AutoRegionService{}).Run(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, in.Id); got.AutoTravel {
		t.Error("撤销关掉的自动出差放行被接管又打开了")
	}
}

// 接管途中入站被删：不能留下一行 adopted——SQLite 复用 id，下一个新建的入站继承它之后永远不会被自动设置地区。
func TestAdoptLeavesNoRowForDeletedInbound(t *testing.T) {
	autoRegionRunEnv(t)
	in := mkRegionInbound(t, 60461, true, []string{"江苏省"})
	ghost := *in
	if err := database.GetDB().Delete(&model.Inbound{}, in.Id).Error; err != nil {
		t.Fatal(err)
	}
	(&AutoRegionService{}).adopt([]*model.Inbound{&ghost}, map[int]*model.InboundAutoRegion{}, time.Now())
	if st := autoRegionStateOf(t, in.Id); st != nil {
		t.Errorf("入站已被删除，接管不该留下状态行: %+v", st)
	}
}

// 观察天数大于 7：观察期必须按更长的窗口量。曾经只读近 7 天的行，「第一次实质使用距今满 N 天」在
// N > 7 时永远达不到，自动设置看着开着、一直显示「观察中」。
func TestAutoRegionObserveLongerThanWindow(t *testing.T) {
	in := arInput()
	in.Params.ObserveDays = 10
	in.Rows = homeRowsFor(7)
	in.SpanRows = homeRowsFor(11)
	if d := decideAutoRegion(in); d.Action != autoRegionApply {
		t.Fatalf("用了 11 天、观察期 10 天，应当自动设置: %+v", d)
	}
	in.SpanRows = homeRowsFor(9)
	if d := decideAutoRegion(in); d.Action != autoRegionWait {
		t.Fatalf("只用了 9 天、观察期 10 天，应当继续观察: %+v", d)
	}
	// 观察期设到保留期（30 天）：行只保留 30 天，按原样永远满不了，按保留期内能量到的最长时长判。
	in.Params.ObserveDays = sharingRetentionDays
	in.SpanRows = homeRowsFor(sharingRetentionDays)
	if d := decideAutoRegion(in); d.Action != autoRegionApply {
		t.Fatalf("观察期设成保留期、整段都在用，应当自动设置: %+v", d)
	}
}

// Run 读的两段行：近 7 天给建议与稳定天数用，观察期窗口给观察期用。
func TestAutoRegionRowsReadsObservationWindow(t *testing.T) {
	setupGeoDB(t)
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for d := 0; d < 12; d++ {
		u := now.Add(-time.Duration(d) * 24 * time.Hour).Unix()
		if err := database.GetTrafficDB().Create(&model.InboundIPHour{InboundId: 7, IP: "192.0.2.9", HourStart: u - u%3600,
			Province: "江苏省", ActiveSeconds: 1800, ActiveBytes: 50 << 20}).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := defaultAutoRegionParams()
	p.ObserveDays = 10
	rows, span, err := autoRegionRows(7, p, now)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-time.Duration(sharingWindowDays) * 24 * time.Hour).Unix()
	for _, r := range rows {
		if r.HourStart < cutoff {
			t.Fatalf("近 7 天的行里混进了更早的: %+v", r)
		}
	}
	if len(span) <= len(rows) {
		t.Fatalf("观察期窗口应当比近 7 天读得多: span=%d rows=%d", len(span), len(rows))
	}
	if d := decideAutoRegion(autoRegionInput{Now: now, Loc: time.Local, Rows: rows, SpanRows: span, Params: p,
		UnplaceablePercent: func([]string) (float64, error) { return 0, nil }, AllowedSet: arSetFor}); d.Action != autoRegionApply {
		t.Fatalf("用了 12 天、观察期 10 天，应当自动设置: %+v", d)
	}
}

// 入站打开「不自动设置地区限制」（Inbound.NoAutoRegion，2026-11-10 管理员要求，给少数特例用）：不首次自动设置。
func TestEligibleForAutoRegionSkipsOptedOut(t *testing.T) {
	in := &model.Inbound{Id: 1, Enable: true, Regions: "[]", NoAutoRegion: true}
	if eligibleForAutoRegion(in, nil) {
		t.Error("打开了「不自动设置地区限制」的入站仍被列为自动设置的候选")
	}
}

// 本轮开头读到的入站还没打开开关，写入之前管理员打开了：任务必须让路。
func TestWriteRegionsRespectsOptOutTurnedOnMidRound(t *testing.T) {
	setupGeoDB(t)
	requireIPDB(t)
	in := mkRegionInbound(t, 60447, true, nil)
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", in.Id).Update("no_auto_region", true).Error; err != nil {
		t.Fatal(err)
	}
	err := writeRegions(in.Id, []string{"江苏省"}, &model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		UpdatedAt: time.Now().UnixMilli()}, true)
	if !errors.Is(err, errAutoRegionStale) {
		t.Fatalf("写入前打开了开关，应当让路: %v", err)
	}
	if got := mustGet(t, in.Id); model.HasRegions(got.Regions) {
		t.Errorf("打开了开关仍被写了地区: %s", got.Regions)
	}
}

// 截图里的场景：地区刚被自动设置（7 天内），管理员在编辑里打开「不自动设置地区限制」同时清空地区——这是
// 明确的意图，不能再当成过期页面拒掉。没打开开关时照旧拒（TestUpdateInboundRejectsStaleWriteOverAutoRegion）。
func TestUpdateInboundOptOutAllowsClearingFreshAutoRegion(t *testing.T) {
	in := setupPassInbound(t, 60410, []string{"江苏省"})
	if err := database.GetDB().Create(&model.InboundAutoRegion{InboundId: in.Id, State: model.AutoRegionApplied,
		Regions: `["江苏省"]`, UpdatedAt: time.Now().UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	edit := mustGet(t, in.Id)
	edit.Regions = "[]"
	edit.NoAutoRegion = true
	if err := (&InboundService{}).UpdateInbound(edit); err != nil {
		t.Fatalf("打开开关同时清空地区不该被拦: %v", err)
	}
	got := mustGet(t, in.Id)
	if model.HasRegions(got.Regions) || !got.NoAutoRegion {
		t.Errorf("regions=%s noAutoRegion=%v，期望已清空且开关已保存", got.Regions, got.NoAutoRegion)
	}
	// 再关掉开关：照常保存（之后自动设置也不会再碰它——管理员改过地区，状态是 manual）。
	got.NoAutoRegion = false
	if err := (&InboundService{}).UpdateInbound(got); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, in.Id).NoAutoRegion {
		t.Error("开关关不回去")
	}
	if st := autoRegionStateOf(t, in.Id); st == nil || st.State != model.AutoRegionManual {
		t.Errorf("清空地区后应标为 manual: %+v", st)
	}
}

// 管理员要的那个场景走一遍真的 Run：两个入站都只在江苏用、用满观察期，打开「不自动设置地区限制」的那个保持没有
// 地区、也不留状态行，另一个照常被设成江苏。
func TestAutoRegionRunSkipsOptedOutInboundEndToEnd(t *testing.T) {
	autoRegionRunEnv(t)
	cidrs := regionCIDRSource((&IPDBService{}).DB(), &SettingService{}).CIDRsOfProvinces([]string{"江苏省"})
	if len(cidrs) == 0 {
		t.Skip("IP 库里没有江苏省的段")
	}
	_, block, err := net.ParseCIDR(cidrs[0])
	if err != nil {
		t.Fatal(err)
	}
	home := block.IP.String()
	optedOut := mkRegionInbound(t, 60448, true, nil)
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", optedOut.Id).Update("no_auto_region", true).Error; err != nil {
		t.Fatal(err)
	}
	normal := mkRegionInbound(t, 60449, true, nil)
	now := time.Now()
	for _, in := range []*model.Inbound{optedOut, normal} {
		for d := 0; d < 5; d++ {
			u := now.Add(-time.Duration(d) * 24 * time.Hour).Unix()
			if err := database.GetTrafficDB().Create(&model.InboundIPHour{InboundId: in.Id, IP: home, HourStart: u - u%3600,
				Province: "江苏省", ActiveSeconds: 1800, ActiveBytes: 50 << 20}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := (&AutoRegionService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, optedOut.Id); model.HasRegions(got.Regions) {
		t.Errorf("打开了「不自动设置地区限制」仍被自动设置: %s", got.Regions)
	}
	if st := autoRegionStateOf(t, optedOut.Id); st != nil {
		t.Errorf("打开了开关的入站不该留状态行: %+v", st)
	}
	if got := mustGet(t, normal.Id); !sameRegions(got.Regions, `["江苏省"]`) {
		t.Errorf("对照入站应当照常设成江苏省: %s", got.Regions)
	}
}
