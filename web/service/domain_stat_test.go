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

// access_log 的自增 id 会被复用：GORM 的 sqlite 驱动对 primaryKey;autoIncrement
// 生成的是裸 rowid 别名，没有 AUTOINCREMENT。AccessLogCleanupJob 不看访问日志
// 开关、无条件按保留期删行，管理员关闭访问日志超过保留期再打开、或手工清库，
// 都会让整张表清空、新行的 id 从 1 重新开始，而位点还停在被清空前的高位。
// 不处理的话 Where("id > 位点") 从此恒为空，聚合永久停摆且没有任何一行日志
// （DomainStatJob 只在 n>0 时才打日志）。
//
// 这条测试守住 Aggregate 里自愈逻辑的场景①（三种场景之一，与
// TestAggregateSelfHealDoesNotDoubleCountAfterPartialDelete、
// TestAggregateSelfHealHandlesMixedPartialDeleteAndNewData 合起来才是完整的
// 回归覆盖）：清空后确实有新数据落地，自愈必须靠 LastLogTime（而不是比较
// id）识别出"位点已失效"并正确接上这条新数据——早期实现把位点直接归零或
// 对齐到清空后的 max(id)，都无法同时满足这条与另外两条测试，具体反例见
// 上面两条测试的注释。
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
		t.Fatalf("自愈这一轮 Aggregate: %v", err)
	}
	if n != 1 {
		t.Errorf("自愈后消费了 %d 条，期望 1（清空后新写的那一条）：位点没有被正确回退重新聚合", n)
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
	// 已经聚合完的历史桶——自愈逻辑只回退位点，不会动 domain_stat。
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

// 场景②：删除入站导致的部分删除，删除后没有任何新数据落地。自愈判据若只看
// max(id) 与位点的大小关系，会把这种情形和场景①（整表清空 + 有新数据）误判
// 成一回事，进而把位点回退、让删除前就已经聚合过的另一个入站的历史行被重新
// 读一遍，产生静默的虚高计数——这条测试复现该场景：入站 A 先写（占低位
// id、时间早），入站 B 后写（占高位 id、时间晚）；聚合一次；只删 B 的访问
// 日志（真实路径：InboundService.DelInbound → AccessLogService.
// DeleteByInbound），A 的行原封不动、表没有清空；不写任何新数据，直接再聚合
// 一次——A 的计数必须原样不变。用 LastLogTime 判断"库里还有没有比上次聚合
// 更晚的记录"能正确处理这个场景：A 剩下的那一行时间不晚于上次聚合时记录的
// LastLogTime（它在删除前就已经被聚合过），自愈逻辑据此判断位点仍然有效，
// 不会触发回退。
func TestAggregateSelfHealDoesNotDoubleCountAfterPartialDelete(t *testing.T) {
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

	// 不写任何新数据，直接再聚合一次：Where(id > 位点) 读到空，触发自愈判断。
	if _, err := svc.Aggregate(); err != nil {
		t.Fatalf("第二次 Aggregate: %v", err)
	}
	after := sumByDomain(listDomainStats(t, model.GranularityHour))
	if after["a.example"] != 1 {
		t.Errorf("删除 B 后再次聚合，a.example 计数 = %d，期望仍为 1（自愈逻辑不应把 A 的历史行重新聚合一遍）", after["a.example"])
	}
}

// 场景③（混合场景，旧判据完全抓不到的盲区）：删除入站腾出的高位 id 之后，
// 又有新数据落回同一段 id 区间。此时 max(id) 可能被新数据重新顶回原位点
// 附近甚至以上，仅比较 max(id) 与位点大小连"位点已失效"都判断不出来，新
// 数据会被永久跳过且没有任何报错。这条测试复现它：入站 A 先写（占低位
// id），入站 B 后写（占高位 id）；聚合一次；删除 B 的访问日志（不清空表）；
// 新入站 C 紧接着写入，其记录复用了刚被删除、原本属于 B 的那几个 id；再聚合
// 一次——C 的新数据必须被聚合到，且 A 的历史计数不能被重复累加。
func TestAggregateSelfHealHandlesMixedPartialDeleteAndNewData(t *testing.T) {
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
		t.Errorf("a.example 计数 = %d，期望仍为 1（不能因为自愈把删除前的历史行重新聚合一遍）", after["a.example"])
	}
}

// 边界①：首次运行、access_log 完全为空。cursor 与 LastLogTime 都是零值，
// Where(id > 0) 直接读到空，落进自愈分支；此时 time > 0 在空表上同样查不到
// 任何行，minId=0，正常结束。这条测试钉住这个边界不 panic、不误报警告、
// 也不写出任何脏位点——不需要任何特判，但值得有一条测试守着。
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

// 边界②：AutoMigrate 给旧位点行加 LastLogTime 列后的状态——LastLogId 已经是
// 一个真实的历史位点，LastLogTime 却是零值（列刚加上，这一行从未跑过带新
// 判据的聚合）。若不特判，Where(time > 0) 会命中全表，把这个本来有效的位点
// 误判成失效并回退到 0，导致早已聚合过的历史数据被重新聚合一遍、计数翻倍。
func TestAggregateTreatsMigratedZeroLastLogTimeAsValid(t *testing.T) {
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

	n, err := (&DomainStatService{}).Aggregate()
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if n != 0 {
		t.Errorf("消费了 %d 条，期望 0（位点应被当作仍然有效，不触发自愈）", n)
	}
	hours := listDomainStats(t, model.GranularityHour)
	if len(hours) != 1 || hours[0].Count != 2 {
		t.Errorf("聚合后 = %+v，期望恰好一行 old.example/2（不应被自愈逻辑重新聚合一遍）", hours)
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
