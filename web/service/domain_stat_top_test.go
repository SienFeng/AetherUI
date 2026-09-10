package service

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// putTrafficBucket 直接写一行用量桶，供覆盖度的分母使用。
func putTrafficBucket(t *testing.T, inboundId int, bucketStart, up, down int64) {
	t.Helper()
	row := &model.TrafficBucket{
		Granularity: model.GranularityHour, InboundId: inboundId,
		BucketStart: bucketStart, Up: up, Down: down,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入用量桶: %v", err)
	}
}

func TestTopDomainsOrdersByBytes(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31801, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	// few.com 次数少但上传巨大——正是管理员要找的那种「谁在偷偷上传」。
	putDomainStat(t, in.Id, "few.com", bucket, 2, 900, 10)
	putDomainStat(t, in.Id, "many.com", bucket, 500, 5, 900)

	svc := &DomainStatService{}
	byCount, err := svc.TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if byCount.List[0].Domain != "many.com" {
		t.Errorf("按次数排首位 = %q，期望 many.com", byCount.List[0].Domain)
	}
	byUp, err := svc.TopDomains(in.Id, TopRange1h, TopOrderUp, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if byUp.List[0].Domain != "few.com" {
		t.Errorf("按上传排首位 = %q，期望 few.com——这正是「哪些域名上传最多」"+
			"这个诉求要回答的问题", byUp.List[0].Domain)
	}
	byDown, err := svc.TopDomains(in.Id, TopRange1h, TopOrderDown, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if byDown.List[0].Domain != "many.com" {
		t.Errorf("按下载排首位 = %q，期望 many.com", byDown.List[0].Domain)
	}
	if byUp.OrderBy != string(TopOrderUp) {
		t.Errorf("OrderBy = %q，期望 up——前端要靠它回显", byUp.OrderBy)
	}
}

func TestTopDomainsFallsBackToCountOnUnknownOrder(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31802, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)

	// 这是个展示接口，一个拼错的参数不该变成报错弹窗。
	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopDomainOrder("乱写"), 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.OrderBy != string(TopOrderCount) {
		t.Errorf("OrderBy = %q，期望回落 count", got.OrderBy)
	}
}

func TestTopDomainsMeteredFollowsPool(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31803, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	svc := &DomainStatService{}

	got, err := svc.TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Metered {
		t.Error("池为空时 Metered 应为 false——前端据此隐藏字节列，" +
			"显示一列恒为 0 的「上传」会被理解成「他没上传过」")
	}
	if got.Coverage != nil {
		t.Errorf("Coverage = %+v，期望 nil", got.Coverage)
	}

	putPoolRow(t, in.Id, "doubleclick.net", 0)
	got, err = svc.TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if !got.Metered {
		t.Error("池非空时 Metered 应为 true")
	}
}

// Metered 的判据必须与 MeterPoolService.Pool 完全一致：一个只剩「已退场、
// 冷却已过期、等着被下一次 Recompute 删掉」的行的入站，池里其实空无一物。
// 两处判据一旦漂移，界面会为它显示上传/下载两列，而那两列恒为 0——那正是
// 这个字段本身要避免的误读。
func TestTopDomainsMeteredIgnoresExpiredCooldownRows(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31807, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	putPoolRow(t, in.Id, "retired.com", now.Add(-time.Hour).Unix())

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Metered {
		t.Error("只剩过期冷却行时 Metered 应为 false——那些域名已经退场，" +
			"不生成任何计量出站，字节列恒为 0")
	}
}

func TestTopDomainsCoverageRatio(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31804, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	putDomainStat(t, in.Id, "doubleclick.net", bucket, 1, 300, 200) // 已计量 500
	putTrafficBucket(t, in.Id, bucket, 600, 400)                    // 总计 1000

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Coverage == nil {
		t.Fatal("Coverage 为 nil，期望有值")
	}
	if got.Coverage.MeteredBytes != 500 || got.Coverage.TotalBytes != 1000 {
		t.Errorf("Coverage = %+v，期望已计量 500 / 总计 1000", got.Coverage)
	}
	if got.Coverage.Ratio == nil || *got.Coverage.Ratio != 0.5 {
		t.Errorf("Ratio = %v，期望 0.5", got.Coverage.Ratio)
	}
}

func TestTopDomainsCoverageNilWhenNoTotal(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31805, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Coverage == nil || got.Coverage.Ratio != nil {
		t.Errorf("Coverage = %+v，期望 Ratio 为 nil——分母为 0 时不显示，"+
			"显示 0%% 会被理解成「一点都没归因到」", got.Coverage)
	}
}

func TestTopDomainsCoverageClampsAboveOne(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31806, "甲")
	putPoolRow(t, in.Id, "doubleclick.net", 0)
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	// 入站计的是加密流、出站计的是明文流，协议开销通常让覆盖率小于 1；
	// 但极端情况下可能越界，显示 103% 会让整块数据失去可信度。
	putDomainStat(t, in.Id, "doubleclick.net", bucket, 1, 900, 900)
	putTrafficBucket(t, in.Id, bucket, 500, 500)

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if got.Coverage.Ratio == nil || *got.Coverage.Ratio != 1 {
		t.Errorf("Ratio = %v，期望钳到 1", got.Coverage.Ratio)
	}
}

// 榜单要能区分域名与 IP 两类目标。
//
// 类型是推导值不是存储值：DomainStat.Domain 从第一期起就「IP 字面量原样」存，
// 形态本身已经是完备的判据。加一列 kind 只会多出一处需要与推导保持一致的
// 真相源，两者一旦漂移，界面上的类型标签会和实际生成的规则形态对不上。
func TestTopDomainsMarksIPLiterals(t *testing.T) {
	setupMeterPoolTest(t)
	in := mkTrafficInbound(t, 31811, "甲")
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	loc, _ := (&SettingService{}).GetTimeLocation()
	bucket := model.AlignHour(now, loc)

	putDomainStat(t, in.Id, "72.235.209.83", bucket, 2495, 0, 0)
	putDomainStat(t, in.Id, "2001:db8::1", bucket, 300, 0, 0)
	putDomainStat(t, in.Id, "acspubs.org", bucket, 131, 0, 0)

	got, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	kinds := make(map[string]string, len(got.List))
	for _, row := range got.List {
		kinds[row.Domain] = row.Kind
	}
	for _, c := range []struct{ target, want string }{
		{"72.235.209.83", "ip"},
		{"2001:db8::1", "ip"},
		{"acspubs.org", "domain"},
	} {
		if kinds[c.target] != c.want {
			t.Errorf("%q 的 Kind = %q，期望 %q", c.target, kinds[c.target], c.want)
		}
	}
}
