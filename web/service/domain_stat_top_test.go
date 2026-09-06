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
