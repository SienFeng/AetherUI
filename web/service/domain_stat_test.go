package service

import (
	"fmt"
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

// access_log 的自增 id 会被复用：GORM 的 sqlite 驱动对 primaryKey;autoIncrement
// 生成的是裸 rowid 别名，没有 AUTOINCREMENT。AccessLogCleanupJob 不看访问日志
// 开关、无条件按保留期删行，管理员关闭访问日志超过保留期再打开、或手工清库，
// 都会让整张表清空、新行的 id 从 1 重新开始。
//
// 位点现在以 (LastLogTime, LastLogId) 为复合序、Time 为主序，这个场景不再
// 需要任何自愈或回退逻辑：清空后新写入的 d.com，不管它复用到的 id 是几，
// 只要它的 Time 晚于位点记录的 Time，就会被 Where(time >= 位点时间 AND
// (time > 位点时间 OR id > 位点id)) 正常读到——这条测试与
// TestAggregateDoesNotDoubleCountAfterPartialDelete、
// TestAggregateHandlesMixedPartialDeleteAndNewData、
// TestAggregateHandlesNewDataExceedingReleasedIdRange 合起来覆盖了 id 复用
// 的几种典型场景，均无需特殊处理即可正确工作（函数名仍保留"Recovers"是
// 因为它描述的运维场景没变，不是因为代码里还有一段"恢复"逻辑）。
func TestAggregateRecoversAfterAccessLogIdsReset(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31010, "甲")
	before := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	// 清空前写三条，把位点推到 3，模拟一段真实的历史积累。
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: before, SourceIP: "1.2.3.4", Network: "tcp", Target: "a.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: before, SourceIP: "1.2.3.4", Network: "tcp", Target: "b.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: before, SourceIP: "1.2.3.4", Network: "tcp", Target: "c.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store（清空前）: %v", err)
	}
	svc := &DomainStatService{}
	n, err := svc.Aggregate()
	if err != nil {
		t.Fatalf("清空前 Aggregate: %v", err)
	}
	if n != 3 {
		t.Fatalf("清空前消费了 %d 条，期望 3", n)
	}

	// 模拟 AccessLogCleanupJob：无条件按保留期清空整表（cutoff 设在全部
	// 记录之后即可，不必关心访问日志开关——清理任务本来就不看它）。
	if _, err := (&AccessLogService{}).Cleanup(1, before.Add(48*time.Hour)); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	var remaining int64
	if err := database.GetAccessLogDB().Model(&model.AccessLog{}).Count(&remaining).Error; err != nil {
		t.Fatalf("统计访问日志: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("清空后仍剩 %d 行访问日志，测试前提不成立", remaining)
	}

	// 清空后新写一条：SQLite 复用被删除的 rowid，这一条的 id 会是 1，
	// 远小于清空前的位点 3。
	after := before.Add(72 * time.Hour)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: after, SourceIP: "1.2.3.4", Network: "tcp", Target: "d.com:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store（清空后）: %v", err)
	}
	var newId int64
	if err := database.GetAccessLogDB().Model(&model.AccessLog{}).Select("id").Scan(&newId).Error; err != nil {
		t.Fatalf("读新行 id: %v", err)
	}
	if newId != 1 {
		t.Fatalf("清空后新行 id = %d，期望 1（前提：GORM 的 sqlite 驱动不生成 AUTOINCREMENT，此前提不成立则本测试的模拟场景失效）", newId)
	}

	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("清空后 Aggregate: %v", err)
	}
	if n != 1 {
		t.Errorf("清空后消费了 %d 条，期望 1（清空后新写的那一条）：Time 主序应该正常读到它", n)
	}
	hours := listDomainStats(t, model.GranularityHour)
	counts := make(map[string]int64, len(hours))
	for _, h := range hours {
		counts[h.Domain] += h.Count
	}
	if counts["d.com"] != 1 {
		t.Errorf("d.com 计数 = %d，期望 1", counts["d.com"])
	}
	// domain_stat 与 access_log 是两张独立的表，清空 access_log 不影响
	// 已经聚合完的历史桶——位点只往前走，不会动 domain_stat。
	for _, dom := range []string{"a.com", "b.com", "c.com"} {
		if counts[dom] != 1 {
			t.Errorf("清空前的历史桶 %s = %d，期望保留为 1", dom, counts[dom])
		}
	}

	// 位点应已推进到新写入那条记录的 id，再跑一次不应重复计入。
	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("第三次 Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("第三次消费了 %d 条，期望 0（位点应已推进）", n)
	}
}

// 场景②：删除入站导致的部分删除，删除后没有任何新数据落地——这条测试复现
// 该场景：入站 A 先写（占低位 id、时间早），入站 B 后写（占高位 id、时间
// 晚）；聚合一次；只删 B 的访问日志（真实路径：InboundService.DelInbound →
// AccessLogService.DeleteByInbound），A 的行原封不动、表没有清空；不写任何
// 新数据，直接再聚合一次——A 的计数必须原样不变。位点以 (LastLogTime,
// LastLogId) 为复合序、Time 为主序，天然处理好这个场景：A 剩下的那一行
// 时间不晚于位点记录的 Time（它在删除前就已经被聚合过），Where 子句
// 自然把它排除在外，不需要任何专门的判断逻辑。
func TestAggregateDoesNotDoubleCountAfterPartialDelete(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31012, "甲")
	other := mkTrafficInbound(t, 31013, "乙")
	tA := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tB := tA.Add(time.Minute)

	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tA, SourceIP: "1.2.3.4", Network: "tcp", Target: "a.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store A: %v", err)
	}
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store B: %v", err)
	}

	svc := &DomainStatService{}
	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第一次 Aggregate: %v", err)
	}
	sumByDomain := func(rows []model.DomainStat) map[string]int64 {
		m := make(map[string]int64, len(rows))
		for _, r := range rows {
			m[r.Domain] += r.Count
		}
		return m
	}
	before := sumByDomain(listDomainStats(t, model.GranularityHour))
	if before["a.example"] != 1 || before["b.example"] != 3 {
		t.Fatalf("第一次聚合后 = %+v，期望 a.example=1 b.example=3", before)
	}

	// 只删 B 的访问日志，A 的行原封不动，表没有清空。
	if err := (&AccessLogService{}).DeleteByInbound(other.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}

	// 不写任何新数据，直接再聚合一次：Where 子句应该读到空（A 剩下的那一行
	// Time 不晚于位点），无需任何特殊处理。
	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	after := sumByDomain(listDomainStats(t, model.GranularityHour))
	if after["a.example"] != 1 {
		t.Errorf("删除 B 后再次聚合，a.example 计数 = %d，期望仍为 1（不应把 A 的历史行重新聚合一遍）", after["a.example"])
	}
}

// 场景③（混合场景）：删除入站腾出的高位 id 之后，又有新数据落回同一段 id
// 区间，且新数据量与被释放的 id 数相等——这条测试复现它：入站 A 先写（占
// 低位 id），入站 B 后写（占高位 id）；聚合一次；删除 B 的访问日志（不清空
// 表）；新入站 C 紧接着写入 3 条，其记录复用了刚被删除、原本属于 B 的那几个
// id；再聚合一次——C 的新数据必须被聚合到，且 A 的历史计数不能被重复累加。
// 位点以 Time 为主序之后，C 的新数据能不能被读到只取决于它的 Time 是否晚于
// 位点，与它拿到的 id 是多少完全无关，因此天然正确。新数据量**超过**被
// 释放的 id 数、导致新行的 id 反超旧位点这个更极端的子场景，见
// TestAggregateHandlesNewDataExceedingReleasedIdRange——那条测试对应的是旧
// 方案（按 id 判断是否需要自愈）完全检测不到、新数据永久丢失且无任何报错
// 的真实缺陷；这里的场景③（新旧数量相等）用旧方案则表现为"自愈能检测到、
// 但归零会导致 A 被重复计数"。两条测试合起来才是完整的覆盖。
func TestAggregateHandlesMixedPartialDeleteAndNewData(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31014, "甲")
	other := mkTrafficInbound(t, 31015, "乙")
	third := mkTrafficInbound(t, 31016, "丙")
	tA := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tB := tA.Add(time.Minute)
	tC := tB.Add(time.Hour) // 明显晚于 tB，确保落在不同的小时桶，断言更干净

	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tA, SourceIP: "1.2.3.4", Network: "tcp", Target: "a.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store A: %v", err)
	}
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store B: %v", err)
	}

	svc := &DomainStatService{}
	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第一次 Aggregate: %v", err)
	}
	sumByDomain := func(rows []model.DomainStat) map[string]int64 {
		m := make(map[string]int64, len(rows))
		for _, r := range rows {
			m[r.Domain] += r.Count
		}
		return m
	}
	before := sumByDomain(listDomainStats(t, model.GranularityHour))
	if before["a.example"] != 1 || before["b.example"] != 3 {
		t.Fatalf("第一次聚合后 = %+v，期望 a.example=1 b.example=3", before)
	}

	// 删除 B 的访问日志，腾出它占用的那几个 id（表不清空，A 的行原封不动）。
	if err := (&AccessLogService{}).DeleteByInbound(other.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}

	// 新入站 C 紧接着写入 3 条：SQLite 复用刚被删除的 rowid，这三条会拿到
	// B 之前占用的那几个 id——把 max(id) 顶回删除前的位点附近，制造"新数据
	// 落回旧 id 区间"的混合场景。
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tC, SourceIP: "9.9.9.9", Network: "tcp", Target: "c.example:443", Inbound: third.Tag, Route: "direct", Accepted: true},
		{Time: tC, SourceIP: "9.9.9.9", Network: "tcp", Target: "c.example:443", Inbound: third.Tag, Route: "direct", Accepted: true},
		{Time: tC, SourceIP: "9.9.9.9", Network: "tcp", Target: "c.example:443", Inbound: third.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store C: %v", err)
	}

	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	after := sumByDomain(listDomainStats(t, model.GranularityHour))
	if after["c.example"] != 3 {
		t.Errorf("c.example 计数 = %d，期望 3（复用旧 id 区间的新数据必须被聚合，不能因为 id 没超过旧位点而被永久跳过）", after["c.example"])
	}
	if after["a.example"] != 1 {
		t.Errorf("a.example 计数 = %d，期望仍为 1（不能把删除前的历史行重新聚合一遍）", after["a.example"])
	}
}

// 缺陷场景（前三轮实现全部漏掉的格子）：删掉高位 id 之后，新数据量**超过**
// 被释放的 id 数，导致新行的 id 反超旧位点。旧方案（不论是"空批次时归零"
// 还是"空批次时对齐到 max(id)"）都是靠 Where(id > 位点) 读到空批次才触发
// 自愈判断的；这里新行的 id 直接越过了旧位点，Where(id > 位点) 天然读到
// 非空结果，自愈分支根本不会被触发——那些 id 落在 (被释放前的 max(id),
// 旧位点] 区间内的新数据会被永久跳过，且没有任何日志、任何报错。
//
// 位点以 Time 为主序之后，这个格子天然不成立：读取条件不再包含任何形式的
// "id > 位点"比较，新数据能不能被读到只取决于它的 Time 是否晚于位点。
func TestAggregateHandlesNewDataExceedingReleasedIdRange(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31018, "甲")
	other := mkTrafficInbound(t, 31019, "乙")
	third := mkTrafficInbound(t, 31020, "丙")
	tA := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tB := tA.Add(time.Minute)
	tD := tB.Add(time.Hour)

	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tA, SourceIP: "1.2.3.4", Network: "tcp", Target: "a.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store A: %v", err)
	}
	// B 写 3 条，占用紧接着 A 之后的 3 个 id。
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
		{Time: tB, SourceIP: "5.6.7.8", Network: "tcp", Target: "b.example:443", Inbound: other.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store B: %v", err)
	}

	svc := &DomainStatService{}
	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第一次 Aggregate: %v", err)
	}
	sumByDomain := func(rows []model.DomainStat) map[string]int64 {
		m := make(map[string]int64, len(rows))
		for _, r := range rows {
			m[r.Domain] += r.Count
		}
		return m
	}
	before := sumByDomain(listDomainStats(t, model.GranularityHour))
	if before["a.example"] != 1 || before["b.example"] != 3 {
		t.Fatalf("第一次聚合后 = %+v，期望 a.example=1 b.example=3", before)
	}

	// 删除 B 的 3 条，腾出 3 个 id（表不清空，A 的行原封不动）。
	if err := (&AccessLogService{}).DeleteByInbound(other.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}

	// 新入站 D 紧接着写入 5 条——比被释放的 3 个 id 多，前 3 条复用 B 空出
	// 的 id，后 2 条的 id 会反超旧位点（旧位点 = A 的 1 条 + B 的 3 条 = 4；
	// D 的 5 条会拿到 id 2~6，其中 5、6 超过了旧位点 4）。这正是旧方案
	// （仅靠 id > 位点 判断要不要触发自愈）检测不到的那个格子。
	entries := make([]accesslog.Entry, 0, 5)
	for i := 0; i < 5; i++ {
		entries = append(entries, accesslog.Entry{
			Time: tD, SourceIP: "9.9.9.9", Network: "tcp", Target: "d.example:443",
			Inbound: third.Tag, Route: "direct", Accepted: true,
		})
	}
	if _, err := (&AccessLogService{}).Store(entries); err != nil {
		t.Fatalf("Store D: %v", err)
	}
	var maxId, releasedTop int64
	if err := database.GetAccessLogDB().Model(&model.AccessLog{}).Select("COALESCE(MAX(id), 0)").Scan(&maxId).Error; err != nil {
		t.Fatal(err)
	}
	releasedTop = 4 // 旧位点：A(1) + B(2,3,4) 里最大的那个
	if maxId <= releasedTop {
		t.Fatalf("测试前提不成立：D 写入后 max(id)=%d，应严格大于旧位点 %d 才能触发缺陷场景", maxId, releasedTop)
	}

	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	after := sumByDomain(listDomainStats(t, model.GranularityHour))
	if after["d.example"] != 5 {
		t.Errorf("d.example 计数 = %d，期望 5（新行的 id 反超旧位点，也必须一条不少地被聚合到）", after["d.example"])
	}
	if after["a.example"] != 1 {
		t.Errorf("a.example 计数 = %d，期望仍为 1（不能把删除前的历史行重新聚合一遍）", after["a.example"])
	}
}

// 边界①：首次运行、access_log 完全为空。cursor 与 LastLogTime 都是零值，
// Where(time >= 0 AND (time > 0 OR id > 0)) 在空表上查不到任何行，正常
// 结束，不需要任何特判。这条测试钉住这个边界不 panic、不写出任何脏位点。
func TestAggregateNoOpOnEmptyAccessLog(t *testing.T) {
	setupDomainStatTest(t)
	n, err := (&DomainStatService{}).Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("消费了 %d 条，期望 0（access_log 为空）", n)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写入了 %d 行，期望 0", len(rows))
	}
}

// 边界②（迁移）：AutoMigrate 给旧位点行加 LastLogTime 列后的状态——
// LastLogId 已经是一个真实的历史位点，LastLogTime 却是零值（列刚加上，这
// 一行从未跑过带新判据的聚合）。若不处理，Where(time > 0) 会命中全表，把
// 升级前已经聚合过的历史数据重新聚合一遍、计数翻倍——这是"重复计入"，比
// "漏一段历史"严重得多。
//
// 处理方式是"从现在开始，不补算历史"：检测到这个组合时，把位点对齐到当前
// access_log 里 (time, id) 最大的那一行，而不是简单地"当作有效、什么都不
// 做"（那样虽然这次调用不会出错，但位点仍然停留在缺 Time 的状态，st.
// LastLogTime 依旧是 0，下一次调用还要再检测一遍，且一旦后续代码路径改变
// 判断顺序就可能重新暴露"time > 0 命中全表"这个洞——不如直接把它修好）。
func TestAggregateMigrationAlignsToLatestExistingRowWithoutReaggregating(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31017, "甲")
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "old.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "old.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	// 模拟"这两条在旧版本上早就聚合过"：直接把 domain_stat 的结果和位点摆
	// 到位，不经过 Aggregate()——这样 LastLogTime 才会是真正的零值，而不是
	// 被 Aggregate() 正常填上的真实时间戳。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tdb := database.GetTrafficDB()
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "old.example", model.AlignHour(at, loc), 2); err != nil {
		t.Fatalf("写历史桶: %v", err)
	}
	if err := tdb.Create(&model.DomainStatCursor{Id: 1, LastLogId: 2, LastLogTime: 0}).Error; err != nil {
		t.Fatalf("写迁移后的旧位点: %v", err)
	}

	svc := &DomainStatService{}
	n, err := svc.Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("消费了 %d 条，期望 0（升级前的历史不应被重新聚合）", n)
	}
	hours := listDomainStats(t, model.GranularityHour)
	if len(hours) != 1 || hours[0].Count != 2 {
		t.Errorf("聚合后 = %+v，期望恰好一行 old.example/2（不应被重新聚合一遍）", hours)
	}

	// 位点必须被真正对齐、往前推进，不能只是"这次调用没出错"：写一条更晚
	// 的新数据，验证下一次 Aggregate 能正常接上它，证明修复后的位点是可用
	// 的，不是每次调用都要重新检测这个迁移分支。
	later := at.Add(time.Hour)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: later, SourceIP: "1.2.3.4", Network: "tcp", Target: "new.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store（新数据）: %v", err)
	}
	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	if n != 1 {
		t.Errorf("第二次消费了 %d 条，期望 1（迁移修复后的位点应能正常接上新数据）", n)
	}
	counts := make(map[string]int64)
	for _, h := range listDomainStats(t, model.GranularityHour) {
		counts[h.Domain] += h.Count
	}
	if counts["old.example"] != 2 {
		t.Errorf("old.example 计数 = %d，期望仍为 2", counts["old.example"])
	}
	if counts["new.example"] != 1 {
		t.Errorf("new.example 计数 = %d，期望 1", counts["new.example"])
	}
}

// 边界③（表当前为空时的迁移）：LastLogTime==0 且 LastLogId>0，但此刻
// access_log 恰好是空的（比如迁移前的历史后来又被保留期清理掉了）。这时
// "对齐到当前最新一行"取不到任何行，退化成对齐到 (0, 0)——等价于一次全新
// 安装，之后任何新数据的 Time 都大于 0，会被正常读到，不需要特殊处理。
func TestAggregateMigrationWithEmptyAccessLogResetsCleanly(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31021, "甲")
	tdb := database.GetTrafficDB()
	// 只写迁移后的脏位点，access_log 里什么都不写——模拟历史数据后来被
	// 保留期清理掉、只剩一个悬空位点的情形。
	if err := tdb.Create(&model.DomainStatCursor{Id: 1, LastLogId: 999, LastLogTime: 0}).Error; err != nil {
		t.Fatalf("写迁移后的旧位点: %v", err)
	}

	svc := &DomainStatService{}
	n, err := svc.Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("消费了 %d 条，期望 0（access_log 为空，无可聚合）", n)
	}

	// 之后正常写入的新数据必须能被读到，证明位点被正确重置为 (0, 0) 而不是
	// 停留在无法再被任何后续数据超越的脏值上。
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: at, SourceIP: "1.2.3.4", Network: "tcp", Target: "fresh.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	if n != 1 {
		t.Errorf("第二次消费了 %d 条，期望 1", n)
	}
}

// 时钟回拨：AccessLog.Time 是 xray 写日志行时的系统墙钟，NTP 步进、DST
// 秋季回拨（每年一次）都可能让它在同一批甚至前后两批之间倒退。这条测试
// 验证残留取舍成立的两个方向：①同一批内 Time 非单调也不会导致任何行被
// 重复计数；②真的发生跳过时（后写入的一行 Time 反而更早），后果是这一行
// 永久不被计入（榜单少一段数据），而不是任何形式的重复计数——方向落在
// 安全侧，这是本项目一贯的取舍（"宁可漏读也不能重复计入"）。
//
// **重要：这条测试的 n == 0 断言守的是一个刻意接受的取舍，不是正确性
// 不变量。** 如果将来给这类跳过加缓解措施（比如给回拨预留一个回看窗口、
// 允许 Time 在一定范围内倒退仍被读到），这条测试会先变红——那不代表改坏
// 了东西，而是这条测试在提醒"你正在动一个已知取舍，确认这是你想要的
// 变化"。不要因为这条测试失败就假设自己引入了 bug，也不要为了让它通过
// 而回避处理真正的缓解需求。
func TestAggregateClockRollbackSkipsRatherThanDoubleCounts(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31022, "甲")
	tLater := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tEarlier := tLater.Add(-time.Hour) // 模拟回拨前后的两个读数

	// 先写一条"较晚"时间的记录（id 更小），再写一条"较早"时间的记录
	// （id 更大）——id 恒定递增反映的是插入顺序，与 Time 的先后无关，这正
	// 是时钟回拨会制造出的现象：插入顺序在后的一行，Time 反而更早。
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tLater, SourceIP: "1.2.3.4", Network: "tcp", Target: "late.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store（较晚时间，较小 id）: %v", err)
	}
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tEarlier, SourceIP: "1.2.3.4", Network: "tcp", Target: "early.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store（较早时间，较大 id，模拟回拨）: %v", err)
	}

	svc := &DomainStatService{}
	// 两条记录在同一批里被一起读到（远小于 domainStatBatchSize），
	// ORDER BY time asc, id asc 会把 early.example 排在 late.example 前面，
	// 但两条都在这一批内，聚合时不区分顺序，两个域名都应该被正确计入恰好
	// 一次。
	n, err := svc.Aggregate()
	if err != nil {
		t.Fatalf("第一次 Aggregate: %v", err)
	}
	if n != 2 {
		t.Fatalf("第一次消费了 %d 条，期望 2", n)
	}
	counts := func() map[string]int64 {
		m := make(map[string]int64)
		for _, h := range listDomainStats(t, model.GranularityHour) {
			m[h.Domain] += h.Count
		}
		return m
	}
	got := counts()
	if got["late.example"] != 1 || got["early.example"] != 1 {
		t.Fatalf("第一次聚合后 = %+v，期望 late.example=1 early.example=1（批内乱序不应导致任何一条被漏记或重复）", got)
	}

	// 幂等性：位点应该已经推进到这一批里 (time, id) 复合序最大的那一行
	// （late.example，因为它的 Time 更晚），再跑一次不应该重复计入任何一条。
	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("第二次消费了 %d 条，期望 0（位点应已推进，不应重复读取）", n)
	}

	// 现在模拟"位点已经推进之后，又有一条 Time 更早的记录姗姗来迟"——
	// 这是残留取舍要覆盖的情形：这条记录会被永久跳过，而不是被重复计入
	// 任何域名。
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: tEarlier, SourceIP: "1.2.3.4", Network: "tcp", Target: "missed.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store（位点推进后姗姗来迟的旧时间记录）: %v", err)
	}
	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("第三次 Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("第三次消费了 %d 条，期望 0（这条记录的 Time 早于位点，应被跳过而不是读到）", n)
	}
	got = counts()
	if got["missed.example"] != 0 {
		t.Errorf("missed.example 计数 = %d，期望 0（应被永久跳过，不应出现在榜单里）", got["missed.example"])
	}
	// 最关键的断言：跳过绝不能表现成任何已有域名被重复计数。
	if got["late.example"] != 1 || got["early.example"] != 1 {
		t.Errorf("既有域名计数变成了 %+v，期望仍是 late.example=1 early.example=1（跳过不能以任何形式表现成重复计数）", got)
	}
}

// 批次截断点恰好落在同一毫秒的多条记录中间：domainStatBatchSize 一轮最多读
// 20000 行，这里故意写 20500 条完全同一毫秒的记录，逼第一轮读到的 20000 条
// 里最后一条与第 20001 条共享同一个 Time 值，位点的 (time, id) 尾数只能靠
// id 区分。这条测试验证这种情况下不会漏读也不会重读——原理与标准的 keyset
// 分页（seek method）一致：位点是 (Time, Id) 复合序，Where 子句严格排除
// 「<= 位点」的行，只要 id 在同一毫秒内保持唯一，续轮查询就能精确从截断点
// 后一条继续，不依赖 batch 是否恰好落在一个新毫秒的开头。
//
// 这条测试比较慢（写 20500 行、跑多轮聚合），但仍在几百毫秒量级，作为正式
// 回归测试保留是划算的——这个格子在设计讨论里被专门点名"要单独看"。
func TestAggregateHandlesBatchBoundaryMidMillisecond(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31023, "甲")
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	const n = domainStatBatchSize + 500
	entries := make([]accesslog.Entry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, accesslog.Entry{
			Time: at, SourceIP: "1.2.3.4", Network: "tcp",
			Target:  fmt.Sprintf("d%d.example:443", i),
			Inbound: in.Tag, Route: "direct", Accepted: true,
		})
	}
	if _, err := (&AccessLogService{}).Store(entries); err != nil {
		t.Fatalf("Store: %v", err)
	}

	svc := &DomainStatService{}
	total := 0
	// domainStatMaxRounds=20 轮足够消化 20500 条（两轮：20000 + 500），这里
	// 多留几轮余量，读到 0 就提前退出。
	for i := 0; i < 5; i++ {
		got, err := svc.Aggregate()
		if err != nil {
			t.Fatalf("Aggregate 第 %d 次: %v", i, err)
		}
		total += got
		if got == 0 {
			break
		}
	}
	if total != n {
		t.Fatalf("总共消费了 %d 条，期望 %d（跨批次边界不能漏读）", total, n)
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != n {
		t.Fatalf("domain_stat 行数 = %d，期望 %d 个不同域名各一行", len(rows), n)
	}
	for _, r := range rows {
		if r.Count != 1 {
			t.Fatalf("域名 %s 计数 = %d，期望 1（不能因跨批次边界被重复计入）", r.Domain, r.Count)
		}
	}
}

// 位点跑到未来：系统时钟一度超前（NTP 故障、date -s 打错、虚拟机快照回滚后
// 再前进）时写下的日志会把位点顶到未来某个时刻；时钟校正回来之后，
// time >= 位点 从此永远不可能被真实数据满足，聚合永久停摆——且这次停摆
// 无界（幅度等于时钟当时前跳的量）、没有任何一行日志、也不会自愈
// （AccessLogCleanupJob 的清理条件是 time < cutoff，永远删不到这些
// "来自未来"的行）。
//
// 这条测试只断言可观察的行为（停摆、不 panic、不自愈），不断言
// logger.Warning 的具体内容——本项目的 logger 直接写 os.Stderr，没有可
// 注入的 writer，其它测试里对 WARNING 的验证也都是靠人工看 `go test -v`
// 的输出，不是程序化断言，这里保持一致。**明确不做自愈**：断言的是位点
// 保持不动、新真实数据也读不到，这是刻意接受的取舍（见 DomainStatCursor
// 注释与设计文档 §4.3 的"残留代价"），不是待修的 bug——将来任何缓解
// 措施都会先撞红这条测试，改的时候要清楚自己在动一个已知取舍，不是在
// 修一个遗留缺陷。
func TestAggregateWarnsAndStopsWhenPositionIsInTheFuture(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31024, "甲")

	// 直接把位点摆到未来 48 小时——模拟"时钟一度超前，写下的日志把位点
	// 顶到未来，随后时钟校正回来"这个故障后的状态，不需要真的操纵系统
	// 时钟。48 小时明显超过代码里 24 小时的告警容差。
	future := time.Now().Add(48 * time.Hour)
	tdb := database.GetTrafficDB()
	if err := tdb.Create(&model.DomainStatCursor{Id: 1, LastLogId: 5, LastLogTime: future.UnixMilli()}).Error; err != nil {
		t.Fatalf("写入未来位点: %v", err)
	}

	// 时钟校正回来之后写的真实数据，Time 是当前时刻——远早于位点里那个
	// "未来"时刻，应该被永久跳过。
	now := time.Now()
	if _, err := (&AccessLogService{}).Store([]accesslog.Entry{
		{Time: now, SourceIP: "1.2.3.4", Network: "tcp", Target: "stranded.example:443", Inbound: in.Tag, Route: "direct", Accepted: true},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	svc := &DomainStatService{}
	n, err := svc.Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("消费了 %d 条，期望 0（位点在未来，真实数据的 Time 追不上它）", n)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("写入了 %d 行，期望 0（不应该有任何自愈动作）", len(rows))
	}

	// 再跑一次，确认这是稳定的停摆而不是偶发：不 panic、不报错、不会
	// 自己好起来。
	n, err = svc.Aggregate()
	if err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("第二次消费了 %d 条，期望 0（停摆应该是持续的，不会自愈）", n)
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

// TopDomains 的查询用 sum(count) 把同一个域名跨多个桶的次数加起来，这是这条
// 查询存在的唯一理由——但此前三条 TopDomains 测试里每个域名都只落在一个桶，
// sum(count) 与裸列 count 取值恰好相同，把 SQL 里的 "sum(count) as count"
// 改成 "count" 也能照样通过。这条测试专门构造「单桶名次」与「跨桶求和后的
// 名次」相反的数据：A 域名分散在三个桶里各 5 次（合计 15），B 域名集中在
// 单个桶里 9 次。只有真的执行了 sum() 才会让 A 排在 B 前面；只看任意单独
// 一个桶都会是 B 更靠前（9 > 5）。同时这也顺带钉住了 GROUP BY 与 ORDER BY
// 里 "count" 别名的解析：用错别名（比如引用了原始列而不是聚合结果）会让
// 排序退回到单桶视角，同样会被这条断言抓到。
func TestTopDomainsSumsAcrossBuckets(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31011, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tdb := database.GetTrafficDB()
	write := func(dom string, hoursAgo int, count int64) {
		t.Helper()
		start := model.AlignHour(now.Add(-time.Duration(hoursAgo)*time.Hour), loc)
		if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, dom, start, count); err != nil {
			t.Fatalf("写桶: %v", err)
		}
	}
	// a.com：三个桶各 5 次，合计 15。
	write("a.com", 0, 5)
	write("a.com", 1, 5)
	write("a.com", 2, 5)
	// b.com：单个桶 9 次，任何一个 a.com 的桶单看都比它小。
	write("b.com", 0, 9)

	res, err := (&DomainStatService{}).TopDomains(in.Id, TopRange6h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	if len(res.List) != 2 {
		t.Fatalf("返回 %+v，期望恰好两个域名", res.List)
	}
	if res.List[0].Domain != "a.com" || res.List[0].Count != 15 {
		t.Errorf("首位 = %+v，期望 a.com/15（跨桶求和后应领先）", res.List[0])
	}
	if res.List[1].Domain != "b.com" || res.List[1].Count != 9 {
		t.Errorf("次位 = %+v，期望 b.com/9", res.List[1])
	}
}

// since 与桶起点一样落在对齐边界上，TopDomains 用 ">="，所以「最近 1 小时」
// 实际覆盖 since 自身那一桶到当前，一共两桶（见 TopDomains 里的注释）。
// 这个用例守两件事：1h 与 6h 命中的桶数不同（不是"只要不报错就行"），且
// 确实存在一个超出档位范围、被排除在外的更老的桶——只比条数的话，
// 边界算错但恰好数量相同的回归会溜过去，所以要连域名一起比对。
func TestTopDomainsRespectsRangeBoundary(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31005, "甲")
	now := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	cur := model.AlignHour(now, loc)
	write := func(dom string, hoursAgo int64) {
		t.Helper()
		if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, dom, cur-hoursAgo*3600, 1); err != nil {
			t.Fatal(err)
		}
	}
	write("a.com", 0)
	write("b.com", 1)
	write("c.com", 3)
	write("d.com", 7)

	domains := func(res *TopDomainResult) []string {
		got := make([]string, 0, len(res.List))
		for _, r := range res.List {
			got = append(got, r.Domain)
		}
		return got
	}
	assertDomains := func(t *testing.T, res *TopDomainResult, want []string) {
		t.Helper()
		got := domains(res)
		if len(got) != len(want) {
			t.Fatalf("返回 %v，期望 %v", got, want)
		}
		for i := range want {
			// 四个桶的 count 都是 1，排序退化成 "domain asc"（GORM 的
			// Order("count desc, domain asc")），所以命中的域名天然按
			// 字母序返回，这里可以逐位比对，不需要先排序。
			if got[i] != want[i] {
				t.Fatalf("返回 %v，期望 %v", got, want)
			}
		}
	}

	svc := &DomainStatService{}
	// since = AlignHour(now-1h) = cur-3600，">=" 命中 cur-3600 与 cur，
	// 即 b.com 与 a.com；cur-3*3600（c.com）与 cur-7*3600（d.com）都更早，
	// 被排除。
	res, err := svc.TopDomains(in.Id, TopRange1h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	assertDomains(t, res, []string{"a.com", "b.com"})

	// since = cur-6*3600，命中前三个桶（a/b/c.com），cur-7*3600（d.com）
	// 超出 6h 范围、必须被排除。
	res, err = svc.TopDomains(in.Id, TopRange6h, 10, now)
	if err != nil {
		t.Fatalf("TopDomains: %v", err)
	}
	assertDomains(t, res, []string{"a.com", "b.com", "c.com"})
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

// TopDomains 开头校验入站存在性，这是一次对外可见的契约变更（从「返回空
// 榜单」改成「返回错误」）：不校验的话，一个库里不存在的入站 id 会返回一张
// 空榜单，管理员会把它读成「这个人没访问过任何网站」，而不是「你查的这个
// 入站根本不存在」——这条错误路径此前没有任何测试断言。
func TestTopDomainsRejectsUnknownInbound(t *testing.T) {
	setupDomainStatTest(t)
	// 库里一个入站都没建，任意正数 id 都不存在。
	_, err := (&DomainStatService{}).TopDomains(99999, TopRange24h, 10, time.Now())
	if err == nil {
		t.Fatal("不存在的入站应返回 error，实际未报错")
	}
}

// 清理条件必须带 granularity：不带的话一次「清理小时桶」会把同样早于该
// 时刻的日桶一起删掉，15 天榜单静默变空。
func TestCleanupOnlyTouchesGivenGranularity(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31008, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	old := now.Add(-60 * 24 * time.Hour)
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "a.com", model.AlignHour(old, loc), 1); err != nil {
		t.Fatal(err)
	}
	if err := upsertDomainStat(tdb, model.GranularityDay, in.Id, "a.com", model.AlignDay(old, loc), 1); err != nil {
		t.Fatal(err)
	}

	svc := &DomainStatService{}
	deleted, err := svc.Cleanup(model.GranularityHour, 30, now)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 1 {
		t.Errorf("删了 %d 行，期望 1", deleted)
	}
	if rows := listDomainStats(t, model.GranularityDay); len(rows) != 1 {
		t.Errorf("日桶被误删：剩 %d 行，期望 1", len(rows))
	}
}

// SQLite 会复用被删除的自增 id：不清掉的话，下一个建出来的入站会看到
// 上一个用户访问过哪些网站，而且引用不再悬空，任何跳过式的防线都拦不住。
func TestDeleteByInboundAndPruneOrphans(t *testing.T) {
	setupDomainStatTest(t)
	in := mkTrafficInbound(t, 31009, "甲")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tdb := database.GetTrafficDB()
	if err := upsertDomainStat(tdb, model.GranularityHour, in.Id, "a.com", model.AlignHour(now, loc), 1); err != nil {
		t.Fatal(err)
	}
	// 一个库里根本不存在的入站 id：模拟 DelInbound 那次清理失败留下的残留。
	if err := upsertDomainStat(tdb, model.GranularityHour, 99999, "b.com", model.AlignHour(now, loc), 1); err != nil {
		t.Fatal(err)
	}

	svc := &DomainStatService{}
	if err := svc.DeleteByInbound(in.Id); err != nil {
		t.Fatalf("DeleteByInbound: %v", err)
	}
	rows := listDomainStats(t, model.GranularityHour)
	if len(rows) != 1 || rows[0].Domain != "b.com" {
		t.Fatalf("DeleteByInbound 后剩 %+v，期望只剩孤儿行 b.com", rows)
	}
	pruned, err := svc.PruneOrphans()
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if pruned != 1 {
		t.Errorf("清了 %d 行孤儿，期望 1", pruned)
	}
	if rows := listDomainStats(t, model.GranularityHour); len(rows) != 0 {
		t.Errorf("仍剩 %d 行", len(rows))
	}
}
