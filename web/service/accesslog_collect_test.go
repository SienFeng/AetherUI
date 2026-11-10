package service

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// shrinkAccessLogCaps 把三个大小上限缩到 KB 级：单轮读 1 KB、常规上限 4 KB、兜底上限 16 KB。
// 比例与生产（4 MB / 16 MB / 64 MB）相同。
func shrinkAccessLogCaps(t *testing.T) {
	t.Helper()
	oldRead, oldFile, oldEmergency := accessLogMaxReadBytes, accessLogMaxFileSize, accessLogEmergencyFileSize
	accessLogMaxReadBytes, accessLogMaxFileSize, accessLogEmergencyFileSize = 1<<10, 4<<10, 16<<10
	t.Cleanup(func() {
		accessLogMaxReadBytes, accessLogMaxFileSize, accessLogEmergencyFileSize = oldRead, oldFile, oldEmergency
	})
}

// appendAccessLines 追加 n 行可解析、目标各不相同的访问记录（每行约 110 字节）。
func appendAccessLines(t *testing.T, path, tag string, from, n int) {
	t.Helper()
	var b strings.Builder
	for i := from; i < from+n; i++ {
		b.WriteString(accessLine(tag, fmt.Sprintf("site%d.example.com:443", i), i))
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
}

func accessLogFileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// 库一直写不进去（表坏了、库被锁、盘满）时进度不前进、文件也不截断——修复前这条路径上
// 没有任何上限，access.log 会一直长到把磁盘写满（实测写库连续失败 5 轮，17.8 MB 一个
// 字节都没截）。兜底上限保证增长有界。
func TestCollectCapsAccessLogWhileStoreKeepsFailing(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	shrinkAccessLogCaps(t)
	in := mkInbound(t, 50031)
	path := useTempAccessLogFile(t)
	s := AccessLogService{}

	if err := database.GetAccessLogDB().Migrator().DropTable(&model.AccessLog{}); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 5; round++ {
		appendAccessLines(t, path, in.Tag, round*100, 40)
		if err := s.Collect(); err == nil {
			t.Fatalf("第 %d 轮：表不存在时 Collect 应报错", round)
		}
		if size := accessLogFileSize(t, path); size > accessLogEmergencyFileSize {
			t.Fatalf("写库连续失败 %d 轮后 access.log 有 %d 字节，超过兜底上限 %d", round, size, accessLogEmergencyFileSize)
		}
	}
}

// 恢复读取进度失败（访问日志库被锁、进度表坏了）同样要有上限。
func TestCollectCapsAccessLogWhenResumeFails(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	shrinkAccessLogCaps(t)
	in := mkInbound(t, 50032)
	path := useTempAccessLogFile(t)

	if err := database.GetAccessLogDB().Migrator().DropTable(&model.AccessLogCursor{}); err != nil {
		t.Fatal(err)
	}
	appendAccessLines(t, path, in.Tag, 0, 180)
	if err := (&AccessLogService{}).Collect(); err == nil {
		t.Fatal("进度表不存在时恢复应报错")
	}
	if size := accessLogFileSize(t, path); size > accessLogEmergencyFileSize {
		t.Fatalf("恢复进度失败后 access.log 有 %d 字节，超过兜底上限 %d", size, accessLogEmergencyFileSize)
	}
}

// 兜底上限只兜底：写库失败、积压还没到兜底上限时不截断，库恢复后积压要一行不少地补进来。
// 修复前恢复后的第一轮只写进第一批（单轮读取上限），随即按常规上限把文件整个清空，
// 剩下的积压全部丢掉（实测丢了 76%）——而 Tailer 的约定是「消费完全部内容之后再截断」。
func TestCollectImportsWholeBacklogAfterStoreRecovers(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	shrinkAccessLogCaps(t)
	in := mkInbound(t, 50033)
	path := useTempAccessLogFile(t)
	s := AccessLogService{}
	adb := database.GetAccessLogDB()

	if err := adb.Migrator().DropTable(&model.AccessLog{}); err != nil {
		t.Fatal(err)
	}
	appendAccessLines(t, path, in.Tag, 0, 70) // 约 8 KB：超过常规上限、低于兜底上限
	before := accessLogFileSize(t, path)
	if err := s.Collect(); err == nil {
		t.Fatal("表不存在时 Collect 应报错")
	}
	if size := accessLogFileSize(t, path); size != before {
		t.Fatalf("积压 %d 字节还没到兜底上限，写库失败后却被截成了 %d", before, size)
	}

	if err := adb.AutoMigrate(&model.AccessLog{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(); err != nil {
		t.Fatalf("恢复后 Collect: %v", err)
	}
	if n := countAccessLogs(t); n != 70 {
		t.Fatalf("恢复后入库 %d 行，期望积压的 70 行全部补上", n)
	}
	if size := accessLogFileSize(t, path); size != 0 {
		t.Errorf("追平之后文件仍有 %d 字节，超过常规上限 %d 的文件应当清空", size, accessLogMaxFileSize)
	}
}

// 落后时（面板停过一阵、刚重启）一次调用连读几轮追平，不能读一批就截断。
func TestCollectDrainsBacklogBeforeTruncating(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	shrinkAccessLogCaps(t)
	in := mkInbound(t, 50034)
	path := useTempAccessLogFile(t)

	appendAccessLines(t, path, in.Tag, 0, 100) // 约 11 KB
	if err := (&AccessLogService{}).Collect(); err != nil {
		t.Fatal(err)
	}
	if n := countAccessLogs(t); n != 100 {
		t.Fatalf("入库 %d 行，期望 100 行一行不少", n)
	}
	if size := accessLogFileSize(t, path); size != 0 {
		t.Errorf("追平之后文件仍有 %d 字节", size)
	}
}

// 磁盘将满时停止写库，但 access.log 自己也在吃这块盘：照旧读过即弃并保住大小上限，
// 与访问日志库没打开时的处理一致。修复前这条分支直接返回，xray 继续往文件里追加。
func TestCollectStillCapsAccessLogWhenDiskIsLow(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	shrinkAccessLogCaps(t)
	in := mkInbound(t, 50035)
	path := useTempAccessLogFile(t)
	oldCheck, oldWarned := hasEnoughDisk, accessLogDiskWarned
	hasEnoughDisk = func() bool { return false }
	t.Cleanup(func() { hasEnoughDisk, accessLogDiskWarned = oldCheck, oldWarned })

	appendAccessLines(t, path, in.Tag, 0, 60) // 约 7 KB，超过常规上限
	if err := (&AccessLogService{}).Collect(); err != nil {
		t.Fatal(err)
	}
	if size := accessLogFileSize(t, path); size > accessLogMaxFileSize {
		t.Fatalf("磁盘将满时 access.log 仍有 %d 字节，超过常规上限 %d", size, accessLogMaxFileSize)
	}
	if n := countAccessLogs(t); n != 0 {
		t.Errorf("磁盘将满时不该写库，却写了 %d 行", n)
	}
}

// 兜底截断之后库里的进度（偏移 + 首行）已经过时。面板重启时首行对不上，必须从头读，
// 截断后新写的行一行都不能跳过。
func TestCollectRereadsFromStartAfterEmergencyTruncationAndRestart(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	shrinkAccessLogCaps(t)
	in := mkInbound(t, 50036)
	path := useTempAccessLogFile(t)
	s := AccessLogService{}
	adb := database.GetAccessLogDB()

	appendAccessLines(t, path, in.Tag, 0, 10)
	if err := s.Collect(); err != nil {
		t.Fatal(err)
	}
	if err := adb.Migrator().DropTable(&model.AccessLog{}); err != nil {
		t.Fatal(err)
	}
	appendAccessLines(t, path, in.Tag, 10, 180)
	if err := s.Collect(); err == nil {
		t.Fatal("表不存在时 Collect 应报错")
	}
	if size := accessLogFileSize(t, path); size > accessLogEmergencyFileSize {
		t.Fatalf("没有触发兜底截断，文件 %d 字节", size)
	}

	// 截断之后 xray 接着写，写得比旧偏移还长：只能靠首行认出文件换过了。
	appendAccessLines(t, path, in.Tag, 1000, 20)
	if err := adb.AutoMigrate(&model.AccessLog{}); err != nil {
		t.Fatal(err)
	}
	simulateRestart(path)
	if err := s.Collect(); err != nil {
		t.Fatal(err)
	}
	if n := countAccessLogs(t); n != 20 {
		t.Fatalf("重启后入库 %d 行，期望截断后新写的 20 行全部读到", n)
	}
}

// 核心按 UTC 写时间（见 TestXrayStartedByPanelWritesAccessLogInUTC），采集按 UTC 解析。
// 2026-11-01 是纽约夏令时结束的那天：05:30Z 与 06:30Z 在纽约的钟面时间都是 01:30。
// 修复前核心按本地时间写、面板按本地时间解析，这两条会被解析成同一个时刻，后一条整整
// 早一小时，域名统计与异常扫描的 (time, id) 游标从此把它跳过。
func TestCollectParsesTimestampsAsUTC(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	in := mkInbound(t, 50037)
	path := useTempAccessLogFile(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("没有时区数据: %v", err)
	}
	// 面板进程的本地时区换成纽约，红灯就不取决于开发机自己在哪个时区。
	oldLocal := time.Local
	time.Local = ny
	t.Cleanup(func() { time.Local = oldLocal })

	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)
	line := func(at time.Time, target string) string {
		return at.Format("2006/01/02 15:04:05.000000") + " from tcp:192.0.2.235:9677 accepted tcp:" + target + " [" + in.Tag + " -> direct]\n"
	}
	writeFile(t, path, line(first, "a.example.com:443")+line(second, "b.example.com:443"))
	if err := (&AccessLogService{}).Collect(); err != nil {
		t.Fatal(err)
	}
	var rows []model.AccessLog
	if err := database.GetAccessLogDB().Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Time != first.UnixMilli() || rows[1].Time != second.UnixMilli() {
		t.Fatalf("入库时间 = %+v，期望 %d 与 %d（按 UTC 解析）", rows, first.UnixMilli(), second.UnixMilli())
	}
}

// 读到的日志行不可能来自未来：同一台机器、同一个时钟，先写后读。按 UTC 解析出未来时刻，
// 只可能是升级前那一版核心按本地时间写下、还没读走的残行（东八区会落到 8 小时之后），
// 或者两次读之间系统时钟往回拨过。这样的行若原样入库，会把域名统计与异常扫描的
// (time, id) 游标顶到几小时之后，期间写进来的新行全部被跳过——所以按读到的时刻记。
func TestCollectClampsTimestampsFromTheFuture(t *testing.T) {
	setupAccessLogDB(t)
	setAccessLog(t, true)
	in := mkInbound(t, 50038)
	path := useTempAccessLogFile(t)

	before := time.Now()
	ahead := before.Add(8 * time.Hour).UTC()
	writeFile(t, path, ahead.Format("2006/01/02 15:04:05.000000")+" from tcp:192.0.2.235:9677 accepted tcp:www.example.com:443 ["+in.Tag+" -> direct]\n")
	if err := (&AccessLogService{}).Collect(); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	var rows []model.AccessLog
	if err := database.GetAccessLogDB().Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Time < before.UnixMilli() || rows[0].Time > after.UnixMilli() {
		t.Fatalf("入库时间 = %+v，期望钳到读取时刻 [%d, %d]", rows, before.UnixMilli(), after.UnixMilli())
	}
}
