package service

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/accesslog"
)

// setupDomainStatTest 建三个全新的临时库：主库（入站）、访问日志库（聚合的
// 输入）、用量库（桶的去处）。三个句柄都是包级变量，每个测试重新 Init 一次
// 即可互不干扰。
func setupDomainStatTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "main.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := database.InitAccessLogDB(filepath.Join(dir, "access.db")); err != nil {
		t.Fatalf("InitAccessLogDB: %v", err)
	}
	if err := database.InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
		t.Fatalf("InitTrafficDB: %v", err)
	}
}

// listDomainStats 返回某粒度下的全部行，顺序确定，便于逐行断言。
func listDomainStats(t *testing.T, g model.TrafficGranularity) []model.DomainStat {
	t.Helper()
	var rows []model.DomainStat
	err := database.GetTrafficDB().
		Where("granularity = ?", g).
		Order("bucket_start asc, inbound_id asc, domain asc").
		Find(&rows).Error
	if err != nil {
		t.Fatalf("查询域名统计: %v", err)
	}
	return rows
}

func TestAggregateWritesBothGranularities(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31001, "甲")

	// 面板时区默认 Asia/Shanghai：UTC 17:30 是当地次日 01:30，
	// 小时桶落在当地 01:00，日桶落在当地 00:00。
	at := time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "www.speedtest.net:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "cdn.speedtest.net:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "s22.cnzz.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	n, err := (&DomainStatService{}).Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 3 {
		t.Fatalf("消费了 %d 条，期望 3", n)
	}

	hours := listDomainStats(t, model.GranularityHour)
	if len(hours) != 2 {
		t.Fatalf("小时桶 %d 行，期望 2（两个注册域名）: %+v", len(hours), hours)
	}
	// 两个 speedtest.net 的子域名必须合并成一行，计数为 2。
	if hours[0].Domain != "cnzz.com" || hours[0].Count != 1 {
		t.Errorf("第一行 = %q/%d，期望 cnzz.com/1", hours[0].Domain, hours[0].Count)
	}
	if hours[1].Domain != "speedtest.net" || hours[1].Count != 2 {
		t.Errorf("第二行 = %q/%d，期望 speedtest.net/2", hours[1].Domain, hours[1].Count)
	}
	if hours[0].InboundId != in.Id {
		t.Errorf("inboundId = %d，期望 %d", hours[0].InboundId, in.Id)
	}

	days := listDomainStats(t, model.GranularityDay)
	if len(days) != 2 {
		t.Fatalf("日桶 %d 行，期望 2", len(days))
	}
	// 日桶独立累加，计数应与小时桶一致（同一天内只有这三条）。
	if days[1].Domain != "speedtest.net" || days[1].Count != 2 {
		t.Errorf("日桶 = %q/%d，期望 speedtest.net/2", days[1].Domain, days[1].Count)
	}
	// 桶按面板时区（Asia/Shanghai）对齐，不是 UTC。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if want := model.AlignHour(at, loc); hours[0].BucketStart != want {
		t.Errorf("小时桶起点 = %d，期望 %d（按 Asia/Shanghai 对齐）", hours[0].BucketStart, want)
	}
	if want := model.AlignDay(at, loc); days[0].BucketStart != want {
		t.Errorf("日桶起点 = %d，期望 %d", days[0].BucketStart, want)
	}
}

// 位点必须推进：再跑一次不能把同一批日志重复计入，否则每 10 分钟一轮，
// 一天下来榜单会虚高 144 倍，而且没有任何一层会报错。
func TestAggregateIsIdempotent(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31002, "甲")
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "example.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	svc := &DomainStatService{}
	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第一次 Aggregate: %v", err)
	}
	n, err := svc.Aggregate()
	if err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("第二次消费了 %d 条，期望 0（位点应已推进）", n)
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Errorf("重复聚合后 = %+v，期望恰好一行且 count=1", rows)
	}
}

// inbound_id = 0 的行（模板里 api 入站留下的、已删除入站留下的）不进榜单：
// 界面按入站查，落成 0 只会变成永远看不见的垃圾行。
func TestAggregateSkipsUnknownInbound(t *testing.T) {
	setupDomainStatTest(t)
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "example.com:443", Inbound: "api", Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := (&DomainStatService{}).Aggregate(); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写入了 %d 行，期望 0: %+v", len(rows), rows)
	}
}
