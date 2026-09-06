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

func TestTopDomainsOrdersByCountWithinRange(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31003, "甲")
	other := mkTrafficInbound(t, 31004, "乙")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tdb := database.GetTrafficDB()
	// 三个域名落在最近 3 小时；一个落在 30 小时前（1h/6h 档不该看到它）。
	write := func(inboundId int, dom string, hoursAgo int, count int64) {
		t.Helper()
		start := model.AlignHour(now.Add(-time.Duration(hoursAgo)*time.Hour), loc)
		if err := upsertDomainStat(tdb, model.GranularityHour, inboundId, dom, start, count); err != nil {
			t.Fatalf("写桶: %v", err)
		}
	}
	write(in.Id, "speedtest.net", 0, 5)
	write(in.Id, "doubleclick.net", 1, 9)
	write(in.Id, "cnzz.com", 2, 7)
	write(in.Id, "old.example", 30, 100)
	write(other.Id, "notmine.com", 0, 999)

	svc := &DomainStatService{}
	res, err := svc.TopDomains(in.Id, TopRange6h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if res.Metered {
		t.Error("第一期不该声称有计量数据")
	}
	got := make([]string, 0, len(res.List))
	for _, r := range res.List {
		got = append(got, r.Domain)
	}
	want := []string{"doubleclick.net", "cnzz.com", "speedtest.net"}
	if len(got) != len(want) {
		t.Fatalf("返回 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("返回 %v，期望 %v（按次数降序）", got, want)
		}
	}
	if res.List[0].Count != 9 {
		t.Errorf("首位次数 = %d，期望 9", res.List[0].Count)
	}
}

// 1h 档只看当前这一个小时桶；6h 档要把更早的桶算进来。
// 边界算错的表征只是"数字偏小"，没有任何一层会报错。
func TestTopDomainsRespectsRangeBoundary(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31005, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	cur := model.AlignHour(now, loc)
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "a.com", cur, 1); err != nil {
		t.Fatal(err)
	}
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "b.com", cur-3600, 1); err != nil {
		t.Fatal(err)
	}

	svc := &DomainStatService{}
	res, err := svc.TopDomains(in.Id, TopRange1h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 1 || res.List[0].Domain != "a.com" {
		t.Errorf("1h 档返回 %+v，期望只有 a.com", res.List)
	}
	res, err = svc.TopDomains(in.Id, TopRange6h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 2 {
		t.Errorf("6h 档返回 %d 条，期望 2", len(res.List))
	}
}

// 7d / 15d 走日桶。走错粒度会把两级数据混在一起加倍计数。
func TestTopDomainsUsesDayBucketsForLongRanges(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31006, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	day := model.AlignDay(now, loc)
	if err := upsertDomainStat(tdb, model.GranularityDay, in.Id, "d.com", day, 42); err != nil {
		t.Fatal(err)
	}
	// 同一天的小时桶：如果查询粒度写错，这 7 会被一起加进来变成 49。
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "d.com", model.AlignHour(now, loc), 7); err != nil {
		t.Fatal(err)
	}

	res, err := (&DomainStatService{}).TopDomains(in.Id, TopRange7d, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 1 || res.List[0].Count != 42 {
		t.Errorf("返回 %+v，期望恰好一条 count=42", res.List)
	}
}

func TestTopDomainsRejectsUnknownRange(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31007, "甲")
	res, err := (&DomainStatService{}).TopDomains(in.Id, TopDomainRange("99h"), 10, time.Now())
	if err != nil {
		t.Fatalf("非法档位不该报错，应回落默认: %v", err)
	}
	if res.Range != string(TopRange24h) {
		t.Errorf("回落到 %q，期望 %q", res.Range, TopRange24h)
	}
}
