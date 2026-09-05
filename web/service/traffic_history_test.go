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
