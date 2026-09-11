package service

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// setupTrafficTest 建一对全新的临时库。两个库句柄都是包级变量，
// 每个测试重新 Init 一次即可互不干扰。
func setupTrafficTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	// 用量库句柄是包级变量，会跨用例残留——SQLite 在文件被 t.TempDir 清掉
	// 之后仍能通过已打开的 fd 读到旧数据。计量池就在这个库里，不清空的话
	// 本用例写进池的行会漏进分流注入器的测试，把断言打成随机失败。
	t.Cleanup(database.ResetTrafficDBForTest)
}

func mkTrafficInbound(t *testing.T, port int, remark string) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS,
		Tag:      fmt.Sprintf("inbound-%v", port),
		Remark:   remark,
		Enable:   true,
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("创建入站: %v", err)
	}
	return in
}

// countBuckets 返回某粒度下的全部桶，按 bucket_start 升序。
func listBuckets(t *testing.T, g model.TrafficGranularity) []model.TrafficBucket {
	t.Helper()
	var rows []model.TrafficBucket
	err := database.GetTrafficDB().
		Where("granularity = ?", g).
		Order("bucket_start asc, inbound_id asc").
		Find(&rows).Error
	if err != nil {
		t.Fatalf("查询桶: %v", err)
	}
	return rows
}

func TestRecordWritesBothGranularities(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30001, "甲")
	svc := TrafficHistoryService{}
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)

	err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 100, Down: 900},
	}, now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	hours := listBuckets(t, model.GranularityHour)
	if len(hours) != 1 {
		t.Fatalf("小时桶行数 = %d，期望 1", len(hours))
	}
	if hours[0].Up != 100 || hours[0].Down != 900 || hours[0].InboundId != in.Id {
		t.Errorf("小时桶 = %+v，期望 up=100 down=900 inboundId=%d", hours[0], in.Id)
	}
	days := listBuckets(t, model.GranularityDay)
	if len(days) != 1 {
		t.Fatalf("日桶行数 = %d，期望 1（日桶独立累加，不依赖后续汇总）", len(days))
	}
	if days[0].Up != 100 || days[0].Down != 900 {
		t.Errorf("日桶 = %+v，期望 up=100 down=900", days[0])
	}
}

func TestRecordAccumulatesWithinSameBucket(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30002, "甲")
	svc := TrafficHistoryService{}
	base := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)

	for i, d := range []int64{10, 20, 30} {
		// 同一小时内的三轮采样，间隔 10 秒，与真实的 XrayTrafficJob 一致。
		at := base.Add(time.Duration(i) * 10 * time.Second)
		if err := svc.Record([]*xray.Traffic{
			{IsInbound: true, Tag: in.Tag, Up: d, Down: d * 2},
		}, at); err != nil {
			t.Fatalf("Record 第 %d 轮: %v", i, err)
		}
	}

	hours := listBuckets(t, model.GranularityHour)
	if len(hours) != 1 {
		t.Fatalf("小时桶行数 = %d，期望 1（同一小时应该 UPSERT 累加，不是插新行）", len(hours))
	}
	if hours[0].Up != 60 || hours[0].Down != 120 {
		t.Errorf("累加结果 = up %d / down %d，期望 60 / 120", hours[0].Up, hours[0].Down)
	}
}

func TestRecordSkipsZeroDelta(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30003, "甲")
	svc := TrafficHistoryService{}

	err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 0, Down: 0},
	}, time.Now())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	// 挂机用户大部分小时没有任何流量。存 0 行只是把磁盘填满，
	// 图上的 0 由前端补零画出来。
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("零增量写了 %d 行，期望一行都不写", len(rows))
	}
}

func TestRecordIgnoresOutboundAndUnknownTags(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30004, "甲")
	svc := TrafficHistoryService{}

	err := svc.Record([]*xray.Traffic{
		{IsInbound: false, Tag: in.Tag, Up: 500, Down: 500},     // 出站，不是本子系统的维度
		{IsInbound: true, Tag: "api", Up: 500, Down: 500},       // 模板里的 api 入站，库里没有
		{IsInbound: true, Tag: "inbound-59999", Up: 7, Down: 8}, // 已删除的入站
	}, time.Now())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	// 落成 inbound_id=0 只会在图上多出一条没人认领的曲线。
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写了 %d 行，期望全部忽略：%+v", len(rows), rows)
	}
}

func TestRecordMergesDuplicateTagsInOneRound(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30005, "甲")
	svc := TrafficHistoryService{}

	err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 1, Down: 2},
		{IsInbound: true, Tag: in.Tag, Up: 3, Down: 4},
	}, time.Now())
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	hours := listBuckets(t, model.GranularityHour)
	if len(hours) != 1 {
		t.Fatalf("小时桶行数 = %d，期望 1", len(hours))
	}
	if hours[0].Up != 4 || hours[0].Down != 6 {
		t.Errorf("合并结果 = up %d / down %d，期望 4 / 6", hours[0].Up, hours[0].Down)
	}
}

func TestRecordIsNoOpWhenDatabaseUnavailable(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30006, "甲")
	// 模拟库没打开：面板启动时 InitTrafficDB 失败就是这个状态。
	if err := database.InitTrafficDB(filepath.Join(t.TempDir(), "x.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
	database.ResetTrafficDBForTest()

	svc := TrafficHistoryService{}
	if err := svc.Record([]*xray.Traffic{
		{IsInbound: true, Tag: in.Tag, Up: 1, Down: 1},
	}, time.Now()); err != nil {
		t.Errorf("库不可用时 Record 应静默返回 nil，实际返回 %v", err)
	}
}

func TestTrafficRetentionDefaults(t *testing.T) {
	setupTrafficTest(t)
	svc := SettingService{}

	// 默认值直接影响磁盘占用与图能拉多远，改动要有意识。
	if got, err := svc.GetTrafficHourRetentionDays(); err != nil || got != 30 {
		t.Errorf("小时桶保留天数默认 = %d (err %v)，期望 30", got, err)
	}
	if got, err := svc.GetTrafficDayRetentionDays(); err != nil || got != 365 {
		t.Errorf("日桶保留天数默认 = %d (err %v)，期望 365", got, err)
	}
}

// writeBucket 直接往库里塞一个桶，用于构造清理与查询测试的初始状态。
func writeBucket(t *testing.T, g model.TrafficGranularity, inboundId int, start, up, down int64) {
	t.Helper()
	row := &model.TrafficBucket{
		Granularity: g, InboundId: inboundId, BucketStart: start, Up: up, Down: down,
	}
	if err := database.GetTrafficDB().Create(row).Error; err != nil {
		t.Fatalf("写入桶: %v", err)
	}
}

func TestCleanupAppliesRetentionPerGranularity(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30101, "甲")
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc := TrafficHistoryService{}

	old := now.Add(-40 * 24 * time.Hour).Unix()   // 40 天前
	fresh := now.Add(-10 * 24 * time.Hour).Unix() // 10 天前
	for _, g := range []model.TrafficGranularity{model.GranularityHour, model.GranularityDay} {
		writeBucket(t, g, in.Id, old, 1, 1)
		writeBucket(t, g, in.Id, fresh, 2, 2)
	}

	// 小时桶保留 30 天：40 天前的该删，10 天前的该留。
	deleted, err := svc.Cleanup(model.GranularityHour, 30, now)
	if err != nil {
		t.Fatalf("Cleanup 小时桶: %v", err)
	}
	if deleted != 1 {
		t.Errorf("删除了 %d 行小时桶，期望 1", deleted)
	}
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 1 || rows[0].BucketStart != fresh {
		t.Errorf("剩余小时桶 = %+v，期望只剩 10 天前那条", rows)
	}
	// 日桶保留期更长，同一时刻的日桶不该被上面那次清理带走。
	if rows := listBuckets(t, model.GranularityDay); len(rows) != 2 {
		t.Errorf("日桶剩 %d 行，期望 2——清理必须按 granularity 隔离", len(rows))
	}
}

func TestPruneOrphansRemovesDeletedInboundBuckets(t *testing.T) {
	setupTrafficTest(t)
	alive := mkTrafficInbound(t, 30102, "在")
	svc := TrafficHistoryService{}

	writeBucket(t, model.GranularityHour, alive.Id, 1000, 5, 5)
	// 一个库里已经不存在的入站 id。SQLite 会复用自增 id，留着它的话，
	// 下一个建出来的入站会看到上一个用户的曲线，而且引用不再悬空，
	// 生成期的任何跳过防线都拦不住。
	writeBucket(t, model.GranularityHour, 9999, 1000, 7, 7)

	pruned, err := svc.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Errorf("清理了 %d 行，期望 1", pruned)
	}
	rows := listBuckets(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].InboundId != alive.Id {
		t.Errorf("剩余 = %+v，期望只剩存活入站那条", rows)
	}
}

func TestDeleteByInboundOnlyTouchesTarget(t *testing.T) {
	setupTrafficTest(t)
	a := mkTrafficInbound(t, 30103, "甲")
	b := mkTrafficInbound(t, 30104, "乙")
	svc := TrafficHistoryService{}

	writeBucket(t, model.GranularityHour, a.Id, 1000, 1, 1)
	writeBucket(t, model.GranularityDay, a.Id, 1000, 1, 1)
	writeBucket(t, model.GranularityHour, b.Id, 1000, 2, 2)

	if err := svc.DeleteByInbound(a.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	if rows := listBuckets(t, model.GranularityHour); len(rows) != 1 || rows[0].InboundId != b.Id {
		t.Errorf("小时桶剩余 = %+v，期望只剩乙的", rows)
	}
	if rows := listBuckets(t, model.GranularityDay); len(rows) != 0 {
		t.Errorf("日桶剩余 = %+v，期望甲的两级都被删掉", rows)
	}
}

func TestHistoryPadsMissingBucketsWithZero(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30201, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	// 只写当前小时这一个桶，其余 23 个小时库里根本没有行（零流量不写行）。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 111, 222)

	res, err := svc.History(in.Id, Range24h, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(res.Points) != 24 {
		t.Fatalf("点数 = %d，期望 24（缺失的桶必须补零，否则图上会缺一段而不是显示 0）", len(res.Points))
	}
	if len(res.Labels) != len(res.Points) {
		t.Fatalf("labels %d 与 points %d 不等长，Chart.js 会错位", len(res.Labels), len(res.Points))
	}
	last := res.Points[len(res.Points)-1]
	if last.Up != 111 || last.Down != 222 {
		t.Errorf("最后一个点 = %+v，期望 up=111 down=222（当前小时应在最右）", last)
	}
	for i, p := range res.Points[:len(res.Points)-1] {
		if p.Up != 0 || p.Down != 0 {
			t.Errorf("第 %d 个点 = %+v，期望补零", i, p)
		}
	}
	if res.Granularity != "hour" {
		t.Errorf("granularity = %q，期望 hour", res.Granularity)
	}
}

func TestHistoryOneYearUsesDayBuckets(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30202, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	writeBucket(t, model.GranularityDay, in.Id, model.AlignDay(now, sh), 9, 9)
	// 同一天的小时桶不该混进 1 年这一档。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 500, 500)

	res, err := svc.History(in.Id, Range1y, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if res.Granularity != "day" || len(res.Points) != 365 {
		t.Fatalf("granularity=%q 点数=%d，期望 day / 365", res.Granularity, len(res.Points))
	}
	if last := res.Points[364]; last.Up != 9 || last.Down != 9 {
		t.Errorf("最后一个点 = %+v，期望取日桶的 9/9 而不是小时桶的 500/500", last)
	}
}

func TestHistoryExcludesOtherInbounds(t *testing.T) {
	setupTrafficTest(t)
	a := mkTrafficInbound(t, 30203, "甲")
	b := mkTrafficInbound(t, 30204, "乙")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	writeBucket(t, model.GranularityHour, b.Id, model.AlignHour(now, sh), 999, 999)

	res, err := svc.History(a.Id, Range24h, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, p := range res.Points {
		if p.Up != 0 || p.Down != 0 {
			t.Fatalf("甲的图里出现了乙的数据: %+v", p)
		}
	}
}

func TestHistoryUnknownRangeFallsBackTo24h(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30205, "甲")
	svc := TrafficHistoryService{}

	res, err := svc.History(in.Id, TrafficRange("不认识的档位"), time.Now())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// 前端传错档位时给一张能看的图，而不是报错或空图。
	if res.Granularity != "hour" || len(res.Points) != 24 {
		t.Errorf("granularity=%q 点数=%d，期望回落到 24 小时档", res.Granularity, len(res.Points))
	}
}

func TestHistoryReportsReasonWhenDatabaseUnavailable(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30206, "甲")
	database.ResetTrafficDBForTest()
	svc := TrafficHistoryService{}

	res, err := svc.History(in.Id, Range24h, time.Now())
	if err != nil {
		t.Fatalf("库不可用时不该返回错误，实际: %v", err)
	}
	// 「看不到」和「没有」必须能被区分开，否则管理员会以为这个人没用流量。
	if res.Reason == "" {
		t.Error("库不可用时 Reason 应说明原因，不能返回一张看起来正常的空图")
	}
}

func TestOverviewRanksByTotalAndTruncates(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)
	slot := model.AlignHour(now, sh)

	// 三个入站，用量 300 / 100 / 200，取 Top 2 应得到 300 和 200。
	big := mkTrafficInbound(t, 30301, "大")
	small := mkTrafficInbound(t, 30302, "小")
	mid := mkTrafficInbound(t, 30303, "中")
	writeBucket(t, model.GranularityHour, big.Id, slot, 150, 150)
	writeBucket(t, model.GranularityHour, small.Id, slot, 50, 50)
	writeBucket(t, model.GranularityHour, mid.Id, slot, 100, 100)

	res, err := svc.Overview(Range24h, 2, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(res.Series) != 2 {
		t.Fatalf("系列数 = %d，期望 2", len(res.Series))
	}
	if res.Series[0].Remark != "大" || res.Series[1].Remark != "中" {
		t.Errorf("排序 = %q, %q，期望 大, 中（按总量降序）", res.Series[0].Remark, res.Series[1].Remark)
	}
	if got := res.Series[0].Points[len(res.Series[0].Points)-1]; got != 300 {
		t.Errorf("最大系列的最后一个点 = %d，期望 300（up+down）", got)
	}
	for _, s := range res.Series {
		if len(s.Points) != len(res.Labels) {
			t.Errorf("系列 %q 的点数 %d 与 labels %d 不等长", s.Remark, len(s.Points), len(res.Labels))
		}
	}
}

func TestOverviewReturnsAllWhenFewerThanTopN(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	in := mkTrafficInbound(t, 30304, "唯一")
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 1, 1)

	res, err := svc.Overview(Range24h, 12, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(res.Series) != 1 {
		t.Errorf("系列数 = %d，期望 1", len(res.Series))
	}
}

func TestOverviewFallsBackToIdWhenRemarkEmpty(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	in := mkTrafficInbound(t, 30305, "")
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now, sh), 1, 1)

	res, err := svc.Overview(Range24h, 12, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	// 图例上留一个空标签，管理员分不出这条线是谁的。
	want := fmt.Sprintf("#%d", in.Id)
	if len(res.Series) != 1 || res.Series[0].Remark != want {
		t.Errorf("备注 = %q，期望回落成 %q", res.Series[0].Remark, want)
	}
}

func TestOverviewIgnoresBucketsOutsideRange(t *testing.T) {
	setupTrafficTest(t)
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 4, 17, 30, 0, 0, sh)

	in := mkTrafficInbound(t, 30306, "甲")
	// 48 小时前的桶落在 24 小时档之外，既不该出现在点里，
	// 也不该让这个入站因为它而挤进 Top N。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(now.Add(-48*time.Hour), sh), 9999, 9999)

	res, err := svc.Overview(Range24h, 12, now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(res.Series) != 0 {
		t.Errorf("系列数 = %d，期望 0——范围外的桶不该把入站带进 Top N", len(res.Series))
	}
}

// mustLoadShanghai 与面板的默认时区一致（defaultValueMap 里的 timeLocation）。
func mustLoadShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

// Total 必须严格等于 Points 各点之和，而不是一次独立的 SQL SUM。
//
// History 用 bucket_start 精确相等去 join buildSlots 按当前时区重算的刻度，
// 而独立的 SUM 聚合不受对齐约束。管理员改过面板时区之后（整小时时区切换会
// 让旧桶与新刻度完全不相交，见 2026-09-04-traffic-history-design.md §3.3），
// 独立 SUM 会让「图是一条平的 0 线、数字却有值」——那是最难解释的一种不
// 一致。让 Total 等于图上那些点的和，两者永远自洽。
func TestHistoryWindowTotalEqualsSumOfPoints(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30301, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	for i, hour := range []int{10, 11, 12} {
		at := time.Date(2026, 9, 10, hour, 0, 0, 0, sh)
		writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(at, sh),
			int64(100*(i+1)), int64(200*(i+1)))
	}

	w := ParseWindow(WindowToday, "", "", sh, now)
	res, err := svc.HistoryWindow(in.Id, w)
	if err != nil {
		t.Fatalf("HistoryWindow: %v", err)
	}

	var sumUp, sumDown int64
	for _, p := range res.Points {
		sumUp += p.Up
		sumDown += p.Down
	}
	if res.Total.Up != sumUp || res.Total.Down != sumDown {
		t.Errorf("Total = %d/%d，期望等于各点之和 %d/%d",
			res.Total.Up, res.Total.Down, sumUp, sumDown)
	}
	if res.Total.Up != 600 || res.Total.Down != 1200 {
		t.Errorf("Total = %d/%d，期望 600/1200", res.Total.Up, res.Total.Down)
	}
}

// 落在窗口外的桶不能被算进去。
//
// 「今日」是本地今天 0 点到现在，昨天那个桶必须落在外面——否则相邻两个
// 窗口会把边界重复计算，「今日」加「昨日」会大于「近 2 日」。
func TestHistoryWindowExcludesBucketsOutsideWindow(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30302, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	inside := time.Date(2026, 9, 10, 10, 0, 0, 0, sh)
	outside := time.Date(2026, 9, 9, 10, 0, 0, 0, sh)
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(inside, sh), 100, 200)
	writeBucket(t, model.GranularityHour, in.Id, model.AlignHour(outside, sh), 999, 999)

	w := ParseWindow(WindowToday, "", "", sh, now)
	res, err := svc.HistoryWindow(in.Id, w)
	if err != nil {
		t.Fatalf("HistoryWindow: %v", err)
	}
	if res.Total.Up != 100 || res.Total.Down != 200 {
		t.Errorf("Total = %d/%d，期望 100/200（昨天那个桶不该算进来）",
			res.Total.Up, res.Total.Down)
	}
}

// 聚合必须带 granularity：小时桶与日桶各自独立累加，日桶不由小时桶汇总
// 而来，不带条件会把同一段时间算两遍。
func TestHistoryWindowFiltersByGranularity(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30303, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	// 同一个时刻各写一个粒度的桶，值相同。「今日」走小时粒度，只该命中前者。
	writeBucket(t, model.GranularityHour, in.Id, model.AlignDay(now, sh), 100, 200)
	writeBucket(t, model.GranularityDay, in.Id, model.AlignDay(now, sh), 100, 200)

	w := ParseWindow(WindowToday, "", "", sh, now)
	res, err := svc.HistoryWindow(in.Id, w)
	if err != nil {
		t.Fatalf("HistoryWindow: %v", err)
	}
	if res.Total.Up != 100 {
		t.Errorf("Total.Up = %d，期望 100（日桶不该被一起算进来）", res.Total.Up)
	}
}

// 旧的 History(range) 签名必须继续可用且行为不变——Overview 与任何未改的
// 调用方都依赖它。改它的签名会牵动系统状态页，那不在本次范围。
func TestLegacyHistoryStillWorks(t *testing.T) {
	setupTrafficTest(t)
	in := mkTrafficInbound(t, 30304, "甲")
	svc := TrafficHistoryService{}
	sh := mustLoadShanghai(t)
	now := time.Date(2026, 9, 10, 14, 30, 0, 0, sh)

	res, err := svc.History(in.Id, Range24h, now)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(res.Points) != 24 {
		t.Errorf("点数 = %d，期望 24", len(res.Points))
	}
	// 旧路径也要填 Total，两条路径的语义必须一致。
	if res.Total.Up != 0 || res.Total.Down != 0 {
		t.Errorf("Total = %+v，期望零值（库里没写任何桶）", res.Total)
	}
}
