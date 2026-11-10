package service

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

const gb = int64(1) << 30

func spikeParams() SpikeThrottleParams {
	return SpikeThrottleParams{Enabled: true, Multiplier: 10, FloorBytes: 20 * gb, Mbps: 5}
}

// 当天流量超过「近 14 天日常用量中位数 × 倍数」并且不少于下限，才算暴涨；历史不足 7 天不判（新账号没有「平时」）。
func TestDecideSpike(t *testing.T) {
	week := func(perDay int64, n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = perDay
		}
		return out
	}
	cases := []struct {
		name  string
		today int64
		past  []int64
		p     SpikeThrottleParams
		want  bool
	}{
		{"历史不足 7 天", 100 * gb, week(gb, 6), spikeParams(), false},
		{"平时 1 GB、今天 15 GB：没到 20 GB 下限", 15 * gb, week(gb, 14), spikeParams(), false},
		{"平时 1 GB、今天 25 GB", 25 * gb, week(gb, 14), spikeParams(), true},
		{"平时 3 GB、今天 25 GB：没到 30 GB（10 倍）", 25 * gb, week(3*gb, 14), spikeParams(), false},
		{"平时 3 GB、今天 31 GB", 31 * gb, week(3*gb, 14), spikeParams(), true},
		{"开关关着", 100 * gb, week(gb, 14), SpikeThrottleParams{Multiplier: 10, FloorBytes: 20 * gb, Mbps: 5}, false},
		// 中位数不被一两天的大下载带偏：14 天里有两天 50 GB，平时仍是 1 GB。
		{"两天大下载不抬高平时", 25 * gb, append(week(gb, 12), 50*gb, 50*gb), spikeParams(), true},
	}
	for _, c := range cases {
		if got, _ := decideSpike(c.today, c.past, c.p); got != c.want {
			t.Errorf("%s: %v，期望 %v", c.name, got, c.want)
		}
	}
}

func addHourBucket(t *testing.T, inboundId int, at time.Time, loc *time.Location, bytes int64) {
	t.Helper()
	b := &model.TrafficBucket{Granularity: model.GranularityHour, InboundId: inboundId,
		BucketStart: model.AlignHour(at, loc), Up: bytes / 4, Down: bytes - bytes/4}
	if err := database.GetTrafficDB().Create(b).Error; err != nil {
		t.Fatal(err)
	}
}

// 按面板时区切天：今天从当地 0 点算起，过去 14 天各算一整天。
func TestDailyUsageSumsLocalDays(t *testing.T) {
	setupTrafficDBForTest(t)
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 20, 10, 30, 0, 0, loc)
	addHourBucket(t, 1, now.Add(-time.Hour), loc, 3*gb)                    // 今天 09:00
	addHourBucket(t, 1, time.Date(2026, 11, 20, 0, 0, 0, 0, loc), loc, gb) // 今天 00:00
	addHourBucket(t, 1, time.Date(2026, 11, 19, 23, 0, 0, 0, loc), loc, 2*gb)
	addHourBucket(t, 1, time.Date(2026, 11, 6, 12, 0, 0, 0, loc), loc, 7*gb) // 14 天前那天
	addHourBucket(t, 1, time.Date(2026, 11, 5, 12, 0, 0, 0, loc), loc, 9*gb) // 15 天前：不算
	addHourBucket(t, 2, now.Add(-time.Hour), loc, 50*gb)                     // 别的入站
	today, past, err := dailyUsage(1, now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if today != 4*gb {
		t.Errorf("今天 = %d，期望 4 GB", today)
	}
	if len(past) != 14 || past[0] != 2*gb || past[13] != 7*gb {
		t.Errorf("过去 14 天 = %v（第 1 天应是昨天 2 GB，第 14 天应是 7 GB）", past)
	}
}

// 暴涨时发一张限速到当地当天结束的单；到期删掉；开关关掉时生效中的一并撤掉。
func TestSpikeThrottleRunCreatesAndExpires(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	loc := panelLoc(t)
	now := time.Now()
	in := makeInboundFor(t, 10081, func(*model.Inbound) {})
	for d := 1; d <= 14; d++ {
		addHourBucket(t, in.Id, time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day()-d, 12, 0, 0, 0, loc), loc, gb)
	}
	addHourBucket(t, in.Id, now, loc, 25*gb)
	s := SpikeThrottleService{}
	if _, err := s.Run(now); err != nil {
		t.Fatal(err)
	}
	var th model.InboundThrottle
	if err := database.GetDB().Where("inbound_id = ?", in.Id).First(&th).Error; err != nil {
		t.Fatalf("没有发限速单: %v", err)
	}
	n := now.In(loc)
	midnight := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, loc).UnixMilli()
	if th.Until != midnight || th.Mbps != 5 {
		t.Errorf("限速单 = %+v，期望限 5 Mbps 到当地明天 0 点 %d", th, midnight)
	}
	if _, err := s.Run(time.UnixMilli(midnight).Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var left int64
	database.GetDB().Model(&model.InboundThrottle{}).Where("inbound_id = ?", in.Id).Count(&left)
	if left != 0 {
		t.Error("到期的限速单没有删掉")
	}
	if _, err := s.Run(now); err != nil {
		t.Fatal(err)
	}
	if err := (&SettingService{}).saveSetting("spikeThrottleEnable", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(now); err != nil {
		t.Fatal(err)
	}
	database.GetDB().Model(&model.InboundThrottle{}).Where("inbound_id = ?", in.Id).Count(&left)
	if left != 0 {
		t.Error("开关关掉后生效中的限速单没有撤掉")
	}
}

// 限速集合把生效中的暴涨限速算进去：没手动限速的按暴涨限速；手动限得更低的保持手动值。
func TestDesiredLimitsIncludesSpikeThrottle(t *testing.T) {
	setupShapingDB(t)
	plain := mkShapedInbound(t, 40021, true, 0, 0)
	lower := mkShapedInbound(t, 40022, true, 0, 3)
	now := time.Now()
	for _, id := range []int{plain.Id, lower.Id} {
		if err := database.GetDB().Create(&model.InboundThrottle{InboundId: id, Mbps: 5,
			Until: now.Add(time.Hour).UnixMilli(), CreatedAt: now.UnixMilli()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	limits, err := (&ShapingService{}).desiredLimits()
	if err != nil {
		t.Fatal(err)
	}
	got := map[int][2]int{}
	for _, l := range limits {
		got[l.InboundId] = [2]int{l.UpMbit, l.DownMbit}
	}
	if got[plain.Id] != [2]int{5, 5} || got[lower.Id] != [2]int{5, 3} {
		t.Errorf("限速集合 = %v，期望 %d 上下行 5、%d 上行 5 下行保持 3", got, plain.Id, lower.Id)
	}
}

func setupTrafficDBForTest(t *testing.T) {
	t.Helper()
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "traffic.db")); err != nil {
		t.Fatal(err)
	}
}

// 「需要留意」带上生效中的暴涨限速：入站列表靠它显示「限速中」，管理员不看日志也知道为什么变慢了。
func TestAttentionIncludesActiveThrottle(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	in := makeInboundFor(t, 10091, func(*model.Inbound) {})
	now := time.Now()
	if err := database.GetDB().Create(&model.InboundThrottle{InboundId: in.Id, Mbps: 5,
		Until: now.Add(time.Hour).UnixMilli(), TodayBytes: 25 * gb, Baseline: gb}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := (&InboundAttentionService{}).Summary(now)
	if err != nil {
		t.Fatal(err)
	}
	if got[in.Id] == nil || got[in.Id].Throttle == nil || got[in.Id].Throttle.Mbps != 5 {
		t.Errorf("需要留意里没有生效中的暴涨限速: %+v", got[in.Id])
	}
}

// 端口被别的程序占着、被临时摘出配置的入站：管理员不看日志也要在列表上看到它没有生效。
func TestAttentionIncludesPortConflict(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	resetPortConflictsForTest(t)
	in := makeInboundFor(t, 10092, func(*model.Inbound) {})
	registerPortConflict(in.Tag, inboundListenSpec{Port: in.Port, TCP: true}, errors.New("address already in use"))
	got, err := (&InboundAttentionService{}).Summary(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got[in.Id] == nil || got[in.Id].PortConflict == nil || got[in.Id].PortConflict.Port != in.Port {
		t.Errorf("需要留意里没有端口冲突: %+v", got[in.Id])
	}
}

// 开关读不出（库锁、值损坏）时这一轮什么都不做：按「关闭」处理会把所有生效中的限速单一次删光，
// 被限速的人在下一轮（10 分钟后）重新被限之前全速跑，tc 计划也跟着来回重建。
func TestSpikeThrottleRunKeepsThrottlesWhenSwitchUnreadable(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	now := time.Now()
	in := makeInboundFor(t, 10082, func(*model.Inbound) {})
	th := model.InboundThrottle{InboundId: in.Id, Until: now.Add(time.Hour).UnixMilli(), Mbps: 5}
	if err := database.GetDB().Create(&th).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&SettingService{}).saveSetting("spikeThrottleEnable", "坏值"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&SpikeThrottleService{}).Run(now); err == nil {
		t.Error("开关读不出时应当报错跳过这一轮")
	}
	var left int64
	database.GetDB().Model(&model.InboundThrottle{}).Where("inbound_id = ?", in.Id).Count(&left)
	if left != 1 {
		t.Error("开关读不出，生效中的限速单被当成「开关关闭」删掉了")
	}
}

// 入站打开「不参与流量暴涨限速」（中转节点：后面挂着很多人，限一次全体变慢）：暴涨也不发限速单，
// 开关打开之前发的限速单在下一轮撤掉。别的入站照常判。
func TestSpikeThrottleRunSkipsExemptInbounds(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	loc := panelLoc(t)
	now := time.Now()
	relay := makeInboundFor(t, 10082, func(in *model.Inbound) { in.SkipSpikeThrottle = true })
	plain := makeInboundFor(t, 10083, func(*model.Inbound) {})
	for _, id := range []int{relay.Id, plain.Id} {
		for d := 1; d <= 14; d++ {
			addHourBucket(t, id, time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day()-d, 12, 0, 0, 0, loc), loc, gb)
		}
		addHourBucket(t, id, now, loc, 25*gb)
	}
	// 开关打开之前留下的限速单。
	if err := database.GetDB().Create(&model.InboundThrottle{InboundId: relay.Id, Mbps: 5,
		Until: now.Add(time.Hour).UnixMilli(), CreatedAt: now.UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	changed, err := (&SpikeThrottleService{}).Run(now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("撤掉了限速单、也新发了一张，应当报告限速集合变了")
	}
	var n int64
	database.GetDB().Model(&model.InboundThrottle{}).Where("inbound_id = ?", relay.Id).Count(&n)
	if n != 0 {
		t.Errorf("不参与暴涨限速的入站仍有 %d 张限速单", n)
	}
	database.GetDB().Model(&model.InboundThrottle{}).Where("inbound_id = ?", plain.Id).Count(&n)
	if n != 1 {
		t.Errorf("普通入站暴涨后限速单 %d 张，期望 1", n)
	}
}

// 老库 AutoMigrate 加上 skip_spike_throttle 列时没有默认值，已有的行是 NULL。NULL 必须按「参与」处理：
// 用 SQL 的 `= false` 过滤会把所有存量入站静默排除在暴涨检测之外。
func TestSpikeThrottleRunTreatsNullExemptColumnAsParticipating(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	loc := panelLoc(t)
	now := time.Now()
	in := makeInboundFor(t, 10084, func(*model.Inbound) {})
	if err := database.GetDB().Exec("UPDATE inbounds SET skip_spike_throttle = NULL WHERE id = ?", in.Id).Error; err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= 14; d++ {
		addHourBucket(t, in.Id, time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day()-d, 12, 0, 0, 0, loc), loc, gb)
	}
	addHourBucket(t, in.Id, now, loc, 25*gb)
	if _, err := (&SpikeThrottleService{}).Run(now); err != nil {
		t.Fatal(err)
	}
	var n int64
	database.GetDB().Model(&model.InboundThrottle{}).Where("inbound_id = ?", in.Id).Count(&n)
	if n != 1 {
		t.Errorf("列为 NULL 的存量入站暴涨后限速单 %d 张，期望 1", n)
	}
}

// 过去的日子小时桶被清掉了（「用量小时数据保留天数」可以设到 1 天），日桶还在：照样有「平时」可比。旧版只读
// 小时桶，小时数据保留不到 7 天时暴涨限速永远不触发，界面上什么提示都没有。
func TestDailyUsageFallsBackToDayBuckets(t *testing.T) {
	setupTrafficDBForTest(t)
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 20, 10, 30, 0, 0, loc)
	for d := 1; d <= 14; d++ {
		day := time.Date(2026, 11, 20-d, 0, 0, 0, 0, loc)
		if err := database.GetTrafficDB().Create(&model.TrafficBucket{Granularity: model.GranularityDay, InboundId: 1,
			BucketStart: model.AlignDay(day, loc), Up: gb / 2, Down: gb / 2}).Error; err != nil {
			t.Fatal(err)
		}
	}
	addHourBucket(t, 1, time.Date(2026, 11, 19, 23, 0, 0, 0, loc), loc, gb/4) // 昨天还剩的小时桶
	addHourBucket(t, 1, now.Add(-time.Hour), loc, 3*gb)                       // 今天
	today, past, err := dailyUsage(1, now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if today != 3*gb {
		t.Errorf("今天 = %d，期望 3 GB（今天只看小时桶）", today)
	}
	for i, v := range past {
		if v != gb {
			t.Fatalf("past[%d] = %d，期望 1 GB（取日桶与小时桶里大的那个）：%v", i, v, past)
		}
	}
}

// 用量库没打开（启动时 InitTrafficDB 失败只告警）：报错跳过，不能空指针 panic、每 10 分钟一条堆栈。
func TestDailyUsageWithoutTrafficDB(t *testing.T) {
	database.ResetTrafficDBForTest()
	if _, _, err := dailyUsage(1, time.Now(), time.UTC); err == nil {
		t.Fatal("用量库没打开应当报错")
	}
}

// 读开关只读一次：守卫读到「开」、之后再读一次却碰上瞬时错误（按「关」处理）的话，会把所有生效中的限速单删光。
// 改了限速档位，已发出的单跟着改，不再按旧档位限到当天结束。
func TestSpikeThrottleRunAppliesCurrentMbpsToActiveThrottles(t *testing.T) {
	setupDB(t)
	setupTrafficDBForTest(t)
	now := time.Now()
	in := makeInboundFor(t, 10083, func(*model.Inbound) {})
	if err := database.GetDB().Create(&model.InboundThrottle{InboundId: in.Id, Until: now.Add(time.Hour).UnixMilli(), Mbps: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&SettingService{}).saveSetting("spikeThrottleMbps", "20"); err != nil {
		t.Fatal(err)
	}
	changed, err := (&SpikeThrottleService{}).Run(now)
	if err != nil {
		t.Fatal(err)
	}
	var th model.InboundThrottle
	if err := database.GetDB().Where("inbound_id = ?", in.Id).First(&th).Error; err != nil {
		t.Fatal(err)
	}
	if th.Mbps != 20 || !changed {
		t.Errorf("限速单 Mbps = %d changed=%v，期望改成 20 并报告有变化", th.Mbps, changed)
	}
}

// 设置页关掉「流量暴涨自动限速」或改了档位：保存时当场作用到生效中的限速单，不等暴涨检测下一轮（10 分钟）。
func TestSpikeThrottleApplySettingsNow(t *testing.T) {
	setupDB(t)
	now := time.Now()
	in := makeInboundFor(t, 10084, func(*model.Inbound) {})
	if err := database.GetDB().Create(&model.InboundThrottle{InboundId: in.Id, Until: now.Add(time.Hour).UnixMilli(), Mbps: 5}).Error; err != nil {
		t.Fatal(err)
	}
	s := SpikeThrottleService{}
	if err := s.settingService.saveSetting("spikeThrottleMbps", "8"); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ApplySettingsNow(); err != nil || !changed {
		t.Fatalf("改档位：changed=%v err=%v", changed, err)
	}
	var th model.InboundThrottle
	if err := database.GetDB().Where("inbound_id = ?", in.Id).First(&th).Error; err != nil || th.Mbps != 8 {
		t.Fatalf("限速单 = %+v err=%v，期望改成 8 Mbps", th, err)
	}
	if err := s.settingService.saveSetting("spikeThrottleEnable", "0"); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ApplySettingsNow(); err != nil || !changed {
		t.Fatalf("关开关：changed=%v err=%v", changed, err)
	}
	var left int64
	database.GetDB().Model(&model.InboundThrottle{}).Count(&left)
	if left != 0 {
		t.Error("开关关掉后生效中的限速单没有当场撤掉")
	}
	// 开关读不出：什么都不动。
	if err := database.GetDB().Create(&model.InboundThrottle{InboundId: in.Id, Until: now.Add(time.Hour).UnixMilli(), Mbps: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.settingService.saveSetting("spikeThrottleEnable", "坏值"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplySettingsNow(); err == nil {
		t.Error("开关读不出时应当报错")
	}
	database.GetDB().Model(&model.InboundThrottle{}).Count(&left)
	if left != 1 {
		t.Error("开关读不出，限速单被删了")
	}
}
