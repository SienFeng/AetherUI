package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"a-ui/util/ipdb"
)

const ipdbSampleSource = `0.0.0.0|0.255.255.255|Reserved|Reserved|Reserved|0|0
1.0.1.0|1.0.3.255|中国|福建省|福州市|中国电信|CN
2.0.0.0|2.0.0.255|中国|江苏省|南京市|中国电信|CN
2.0.1.0|2.0.1.255|中国|江苏省|南京市|中国联通|CN
3.0.0.0|3.255.255.255|United States|California|San Jose|0|US
`

// serveSource 起一个本地 HTTP 服务返回指定内容或状态码。
func serveSource(t *testing.T, body string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// seedDatabaseAt 在指定位置放一份带指定生成时间的库，用来模拟「库是昨天生成的」。
func seedDatabaseAt(t *testing.T, path string, builtAt time.Time) *ipdb.DB {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建库文件: %v", err)
	}
	if err := ipdb.Build(strings.NewReader(ipdbSampleSource), f, builtAt); err != nil {
		f.Close()
		t.Fatalf("生成库: %v", err)
	}
	f.Close()
	db, err := ipdb.Load(path)
	if err != nil {
		t.Fatalf("回读库: %v", err)
	}
	return db
}

// seedOldDatabase 在目标位置放一份可用的旧库，用于验证更新失败时它原样保留。
func seedOldDatabase(t *testing.T, dir string) (path string, before []byte) {
	t.Helper()
	path = filepath.Join(dir, "ipdb.dat")
	s := IPDBService{}
	if _, err := s.fetchAndBuild(testSource(path, 1), serveSource(t, ipdbSampleSource, http.StatusOK), path); err != nil {
		t.Fatalf("准备旧库: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取旧库: %v", err)
	}
	return path, before
}

func assertFileUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("旧库应当还在，却读不到: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("旧库被改动了：更新失败时必须原样保留（旧 %d 字节，新 %d 字节）",
			len(before), len(after))
	}
}

// assertNoLeftovers 目录里只应剩下目标文件，不能有中间产物。
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录: %v", err)
	}
	for _, e := range entries {
		// 只有 .ipdb-*.tmp 是中间产物；各数据源自己的 .dat 是正常结果。
		if strings.HasPrefix(e.Name(), ".ipdb-") {
			t.Errorf("残留了中间文件 %q", e.Name())
		}
	}
}

func TestFetchAndBuildInstallsNewDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")

	s := IPDBService{}
	db, err := s.fetchAndBuild(testSource(path, 1), serveSource(t, ipdbSampleSource, http.StatusOK), path)
	if err != nil {
		t.Fatalf("fetchAndBuild: %v", err)
	}
	if db == nil {
		t.Fatal("返回的 DB 为 nil")
	}
	// 5 行输入中有 2 行同为江苏南京且相连，应合并
	if got := db.SegmentCount(); got != 4 {
		t.Errorf("SegmentCount = %d, want 4", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("目标文件未生成: %v", err)
	}
	assertNoLeftovers(t, dir)
}

func TestFetchAndBuildKeepsOldDatabaseWhenDownloadFails(t *testing.T) {
	dir := t.TempDir()
	path, before := seedOldDatabase(t, dir)

	s := IPDBService{}
	if _, err := s.fetchAndBuild(testSource(path, 1), serveSource(t, "", http.StatusInternalServerError), path); err == nil {
		t.Fatal("want error, got nil")
	}
	assertFileUnchanged(t, path, before)
	assertNoLeftovers(t, dir)
}

func TestFetchAndBuildKeepsOldDatabaseWhenSourceIsMalformed(t *testing.T) {
	dir := t.TempDir()
	path, before := seedOldDatabase(t, dir)

	s := IPDBService{}
	if _, err := s.fetchAndBuild(testSource(path, 1), serveSource(t, "这不是 IP 库\n乱七八糟\n", http.StatusOK), path); err == nil {
		t.Fatal("want error, got nil")
	}
	assertFileUnchanged(t, path, before)
	assertNoLeftovers(t, dir)
}

// 下载被中途截断时，生成出来的库是「合法但残缺」的——段数会静默变少，
// 而省份 CIDR 集合变小会让地区限制误拒合法用户。用段数下限挡住这种情况。
func TestFetchAndBuildRejectsSuspiciouslySmallDatabase(t *testing.T) {
	dir := t.TempDir()
	path, before := seedOldDatabase(t, dir)

	s := IPDBService{}
	_, err := s.fetchAndBuild(testSource(path, 100000), serveSource(t, ipdbSampleSource, http.StatusOK), path)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	assertFileUnchanged(t, path, before)
	assertNoLeftovers(t, dir)
}

func TestFetchAndBuildWorksWhenNoOldDatabaseExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")

	s := IPDBService{}
	if _, err := s.fetchAndBuild(testSource(path, 1), serveSource(t, "垃圾内容", http.StatusOK), path); err == nil {
		t.Fatal("want error, got nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("失败时不该留下任何目标文件")
	}
	assertNoLeftovers(t, dir)
}

func TestAllSettingRejectsBadIPDBSettings(t *testing.T) {
	const okURL = "https://example.com/ipv4_source.txt"

	t.Run("非法地址", func(t *testing.T) {
		for _, bad := range []string{"example.com/x.txt", "ftp://example.com/x.txt", "https://"} {
			s := validBaseSetting()
			s.IPDBSourceUrl = bad
			if err := s.CheckValid(); err == nil {
				t.Errorf("ip2region 地址 %q 应当被拒绝", bad)
			}
			s = validBaseSetting()
			s.QQWrySourceUrl = bad
			if err := s.CheckValid(); err == nil {
				t.Errorf("纯真库地址 %q 应当被拒绝", bad)
			}
		}
	})

	t.Run("单个源留空表示不启用", func(t *testing.T) {
		s := validBaseSetting()
		s.QQWrySourceUrl = ""
		if err := s.CheckValid(); err != nil {
			t.Errorf("只留一个源应当合法: %v", err)
		}
		s = validBaseSetting()
		s.IPDBSourceUrl = ""
		if err := s.CheckValid(); err != nil {
			t.Errorf("只留一个源应当合法: %v", err)
		}
	})

	t.Run("两个源都留空要被拒绝", func(t *testing.T) {
		// 都留空的话归属地显示与地区限制会一起失效，而界面上看不出任何异常。
		s := validBaseSetting()
		s.IPDBSourceUrl = ""
		s.QQWrySourceUrl = ""
		if err := s.CheckValid(); err == nil {
			t.Error("两个源都留空应当被拒绝")
		}
	})

	t.Run("更新时刻格式", func(t *testing.T) {
		for _, bad := range []string{"25:00", "4:00pm", "0400", "04:60", "abc"} {
			s := validBaseSetting()
			s.IPDBUpdateTime = bad
			if err := s.CheckValid(); err == nil {
				t.Errorf("更新时刻 %q 应当被拒绝", bad)
			}
		}
		// 留空表示关闭自动更新。
		for _, good := range []string{"", "00:00", "04:30", "23:59"} {
			s := validBaseSetting()
			s.IPDBUpdateTime = good
			s.IPDBSourceUrl = okURL
			if err := s.CheckValid(); err != nil {
				t.Errorf("更新时刻 %q 应当合法: %v", good, err)
			}
		}
	})
}

// countingSource 返回地址和一个请求计数器，用来断言「不该更新时一次网络请求都不发」。
func countingSource(t *testing.T, body string) (string, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func setUpdateTime(t *testing.T, v string) {
	t.Helper()
	ss := SettingService{}
	if err := ss.setString("ipdbUpdateTime", v); err != nil {
		t.Fatalf("写入更新时刻: %v", err)
	}
	// 到点判断是在设置的时区里做的。测试必须把时区固定下来，否则同一个
	// 挂钟时刻在不同机器上会落到不同的「今天」。
	if err := ss.setString("timeLocation", "UTC"); err != nil {
		t.Fatalf("写入时区: %v", err)
	}
}

// testSource 造一个指向临时目录的数据源，避免测试碰到 bin/ 下的真实库。
func testSource(path string, minSegments int) ipdbSource {
	return ipdbSource{
		Key: "test", Name: "测试源", Path: path, MinSegments: minSegments,
		URL:   func(s *SettingService) (string, error) { return "", nil },
		Build: ipdb.Build,
	}
}

// useTestSources 把数据源换成指向临时目录的版本，并在测试结束后还原。
func useTestSources(t *testing.T, sources []ipdbSource) {
	t.Helper()
	old := ipdbSourceList
	ipdbSourceList = func() []ipdbSource { return sources }
	t.Cleanup(func() {
		ipdbSourceList = old
		ipdbLock.Lock()
		ipdbDBs = map[string]*ipdb.DB{}
		ipdbLock.Unlock()
	})
	ipdbLock.Lock()
	ipdbDBs = map[string]*ipdb.DB{}
	ipdbLock.Unlock()
}

func urlSource(key, path, url string) ipdbSource {
	return ipdbSource{
		Key: key, Name: key, Path: path, MinSegments: 1,
		URL:   func(s *SettingService) (string, error) { return url, nil },
		Build: ipdb.Build,
	}
}

// 自动更新默认关闭。关闭状态下定时任务必须彻底不动手——不是「下载了再丢弃」，
// 而是根本不发请求：ip2region 源数据 35 MB，白下载既费流量也费 CPU。
func TestScheduledUpdateDoesNothingWhenDisabled(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "")
	dir := t.TempDir()
	path, before := seedOldDatabase(t, dir)
	url, hits := countingSource(t, ipdbSampleSource)
	useTestSources(t, []ipdbSource{urlSource("a", path, url)})

	updated, err := (&IPDBService{}).RunScheduledUpdate(time.Now())
	if err != nil {
		t.Fatalf("RunScheduledUpdate: %v", err)
	}
	if updated != 0 {
		t.Errorf("更新了 %d 个源，关闭自动更新时应为 0", updated)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("发起了 %d 次网络请求，关闭时应为 0", n)
	}
	assertFileUnchanged(t, path, before)
}

func TestScheduledUpdateDoesNothingBeforeTheScheduledTime(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "23:59")
	dir := t.TempDir()
	path, before := seedOldDatabase(t, dir)
	url, hits := countingSource(t, ipdbSampleSource)
	useTestSources(t, []ipdbSource{urlSource("a", path, url)})

	s := IPDBService{}
	db, err := ipdb.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.setDB("a", db)

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	updated, err := s.RunScheduledUpdate(now)
	if err != nil {
		t.Fatalf("RunScheduledUpdate: %v", err)
	}
	if updated != 0 {
		t.Errorf("更新了 %d 个源，还没到 23:59 不该更新", updated)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("发起了 %d 次网络请求，未到点时应为 0", n)
	}
	assertFileUnchanged(t, path, before)
}

func TestScheduledUpdateRunsOnceAfterTheScheduledTime(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "04:00")
	dir := t.TempDir()
	path := filepath.Join(dir, "a.dat")
	url, hits := countingSource(t, ipdbSampleSource)
	useTestSources(t, []ipdbSource{urlSource("a", path, url)})

	s := IPDBService{}
	// 库是昨天生成的，今天 12:00 已经过了 04:00，应当更新
	s.setDB("a", seedDatabaseAt(t, path, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)))
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	updated, err := s.RunScheduledUpdate(now)
	if err != nil {
		t.Fatalf("RunScheduledUpdate: %v", err)
	}
	if updated != 1 {
		t.Fatalf("更新了 %d 个源，期望 1", updated)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("发起了 %d 次网络请求，应为 1", n)
	}

	// 同一天再跑一次不应重复下载：到点更新是「每天一次」，不是「过点就一直下」。
	updated, err = s.RunScheduledUpdate(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if updated != 0 {
		t.Errorf("同一天重复更新了 %d 次", updated)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("同一天发起了 %d 次网络请求，应仍为 1", n)
	}
	assertNoLeftovers(t, dir)
}

// 一个源坏掉不能把另一个源的更新也挡住——这正是多源要解决的问题之一。
func TestScheduledUpdateOneBadSourceDoesNotBlockTheOther(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "04:00")
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "good.dat")
	badPath, badBefore := seedOldDatabase(t, dir)
	goodURL, _ := countingSource(t, ipdbSampleSource)
	badURL, _ := countingSource(t, "彻底不是 IP 库的内容")
	useTestSources(t, []ipdbSource{
		urlSource("bad", badPath, badURL),
		urlSource("good", goodPath, goodURL),
	})

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	updated, err := (&IPDBService{}).RunScheduledUpdate(now)
	if err != nil {
		t.Fatalf("RunScheduledUpdate: %v", err)
	}
	if updated != 1 {
		t.Errorf("更新了 %d 个源，期望坏的那个失败、好的那个成功", updated)
	}
	if _, statErr := os.Stat(goodPath); statErr != nil {
		t.Errorf("好的源没有生成: %v", statErr)
	}
	// 坏掉的源必须原样保留旧库，绝不能换成半截数据。
	assertFileUnchanged(t, badPath, badBefore)
	assertNoLeftovers(t, dir)
}

func TestScheduledUpdateSkipsSourceWithEmptyUrl(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "04:00")
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dat")
	useTestSources(t, []ipdbSource{urlSource("a", path, "")})

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	updated, err := (&IPDBService{}).RunScheduledUpdate(now)
	if err != nil {
		t.Fatalf("RunScheduledUpdate: %v", err)
	}
	// 地址留空是「不启用这个源」的表达方式，不能当成错误。
	if updated != 0 {
		t.Errorf("更新了 %d 个源，地址留空时应为 0", updated)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("地址留空却生成了库文件")
	}
}

func TestBuildQQWryRejectsGarbageAndOversizedInput(t *testing.T) {
	var buf bytes.Buffer
	if err := buildQQWry(strings.NewReader("这不是纯真库"), &buf, time.Unix(1000, 0)); err == nil {
		t.Error("非法内容应当报错，绝不能生成一份空库——空的省份集合会让地区限制拒绝所有人")
	}
	buf.Reset()
	huge := strings.NewReader(strings.Repeat("x", qqwryMaxSourceBytes+10))
	if err := buildQQWry(huge, &buf, time.Unix(1000, 0)); err == nil {
		t.Error("超过体积上限应当报错")
	}
}

func TestUpdateNowCanTargetASingleSource(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.dat")
	pathB := filepath.Join(dir, "b.dat")
	urlA, hitsA := countingSource(t, ipdbSampleSource)
	urlB, hitsB := countingSource(t, ipdbSampleSource)
	useTestSources(t, []ipdbSource{urlSource("a", pathA, urlA), urlSource("b", pathB, urlB)})

	s := IPDBService{}
	updated, _, err := s.UpdateNow("b")
	if err != nil {
		t.Fatalf("UpdateNow: %v", err)
	}
	if updated != 1 {
		t.Fatalf("更新了 %d 个源，期望只更新 b", updated)
	}
	// 只更新一个源的意义就在这里：不必为了刷新 2.5 MB 的纯真库
	// 把 35 MB 的 ip2region 也重下一遍。
	if n := atomic.LoadInt32(hitsA); n != 0 {
		t.Errorf("源 a 被请求了 %d 次，指定只更新 b 时应为 0", n)
	}
	if n := atomic.LoadInt32(hitsB); n != 1 {
		t.Errorf("源 b 被请求了 %d 次，期望 1", n)
	}
}

func TestUpdateNowRejectsUnknownSourceKey(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	url, hits := countingSource(t, ipdbSampleSource)
	useTestSources(t, []ipdbSource{urlSource("a", filepath.Join(dir, "a.dat"), url)})

	if _, _, err := (&IPDBService{}).UpdateNow("不存在的源"); err == nil {
		t.Error("未知的数据源标识应当报错，而不是静默什么都不做")
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("发起了 %d 次网络请求，应为 0", n)
	}
}

func TestUpdateNowWithEmptyKeyUpdatesEverySource(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	urlA, hitsA := countingSource(t, ipdbSampleSource)
	urlB, hitsB := countingSource(t, ipdbSampleSource)
	useTestSources(t, []ipdbSource{
		urlSource("a", filepath.Join(dir, "a.dat"), urlA),
		urlSource("b", filepath.Join(dir, "b.dat"), urlB),
	})

	updated, _, err := (&IPDBService{}).UpdateNow("")
	if err != nil {
		t.Fatalf("UpdateNow: %v", err)
	}
	if updated != 2 {
		t.Fatalf("更新了 %d 个源，期望 2", updated)
	}
	if atomic.LoadInt32(hitsA) != 1 || atomic.LoadInt32(hitsB) != 1 {
		t.Errorf("两个源各应被请求 1 次，实际 %d / %d",
			atomic.LoadInt32(hitsA), atomic.LoadInt32(hitsB))
	}
}

// ---- 落盘位置迁移与条件请求 ----

// etagRecorder 记录服务端每次收到的 If-None-Match。用锁而不是裸切片：
// 处理器跑在另一个 goroutine 上，-race 下裸切片会报数据竞争。
type etagRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *etagRecorder) add(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, v)
}

func (r *etagRecorder) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// etagSource 起一个带 ETag 的源：If-None-Match 匹配时返回 304，否则返回内容。
func etagSource(t *testing.T, body, etag string) (string, *etagRecorder) {
	t.Helper()
	rec := &etagRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm := r.Header.Get("If-None-Match")
		rec.add(inm)
		w.Header().Set("ETag", etag)
		if inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %v: %v", path, err)
	}
	return b
}

// 新位置没有、旧位置有时，把旧位置那份搬过来。
//
// 旧位置的文件必须保留：发版包里带着它，全新安装靠它开箱可用。
func TestMigrateSeedsNewPathFromLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.dat")
	newPath := filepath.Join(dir, "new.dat")
	seedDatabaseAt(t, legacy, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	src := testSource(newPath, 1)
	src.LegacyPath = legacy
	useTestSources(t, []ipdbSource{src})

	(&IPDBService{}).MigrateLegacyFiles()

	if !bytes.Equal(readFileOrFail(t, legacy), readFileOrFail(t, newPath)) {
		t.Error("新位置的内容应当与旧位置一致")
	}
}

// 新位置已有时绝不能被旧位置覆盖。
//
// 这正是本次事故的形态：管理员刚更新出来的库被发版包里那份旧构建换掉，
// 界面还显示「已加载」，看不出数据已经退回去了。
func TestMigrateDoesNotOverwriteExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.dat")
	newPath := filepath.Join(dir, "new.dat")
	seedDatabaseAt(t, legacy, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seedDatabaseAt(t, newPath, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	want := readFileOrFail(t, newPath)

	src := testSource(newPath, 1)
	src.LegacyPath = legacy
	useTestSources(t, []ipdbSource{src})

	(&IPDBService{}).MigrateLegacyFiles()

	if !bytes.Equal(want, readFileOrFail(t, newPath)) {
		t.Error("新位置已有库时不得被旧位置覆盖")
	}
}

// 本地已有库时带上 If-None-Match，上游返回 304 就跳过，一个字节都不重写。
func TestUpdateSendsConditionalRequestAndSkipsOn304(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")
	url, rec := etagSource(t, ipdbSampleSource, `"v1"`)

	src := urlSource("test", path, url)
	src.EtagKey, src.CheckedAtKey = "testEtag", "testCheckedAt"
	useTestSources(t, []ipdbSource{src})

	svc := &IPDBService{}

	// 首次：本地什么都没有，必须无条件下载。
	updated, upToDate, err := svc.UpdateNow("")
	if err != nil || updated != 1 || upToDate != 0 {
		t.Fatalf("首次更新 = (%d, %d, %v)，期望 (1, 0, nil)", updated, upToDate, err)
	}
	before := readFileOrFail(t, path)

	// 再次：ETag 已记下，应当带上条件头并被 304 挡回。
	updated, upToDate, err = svc.UpdateNow("")
	if err != nil || updated != 0 || upToDate != 1 {
		t.Fatalf("再次更新 = (%d, %d, %v)，期望 (0, 1, nil)", updated, upToDate, err)
	}
	if got := rec.values(); len(got) != 2 || got[0] != "" || got[1] != `"v1"` {
		t.Errorf("If-None-Match 序列 = %q，期望首次为空、二次带上 ETag", got)
	}
	if !bytes.Equal(before, readFileOrFail(t, path)) {
		t.Error("上游未变更时不应重写库文件")
	}
}

// 库文件不在时绝不能发条件请求：一旦被 304 挡回就什么都拿不到，
// 库永远补不回来。更新面板清空安装目录之后，处境正是这个。
func TestUpdateSkipsConditionalRequestWhenDatabaseMissing(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")
	url, rec := etagSource(t, ipdbSampleSource, `"v1"`)

	src := urlSource("test", path, url)
	src.EtagKey, src.CheckedAtKey = "testEtag", "testCheckedAt"
	useTestSources(t, []ipdbSource{src})

	svc := &IPDBService{}
	if _, _, err := svc.UpdateNow(""); err != nil {
		t.Fatalf("首次更新: %v", err)
	}

	// 模拟库文件随安装目录一起被删掉
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除库文件: %v", err)
	}
	ipdbLock.Lock()
	delete(ipdbDBs, "test")
	ipdbLock.Unlock()

	updated, upToDate, err := svc.UpdateNow("")
	if err != nil || updated != 1 || upToDate != 0 {
		t.Fatalf("库缺失时 = (%d, %d, %v)，期望无条件重下 (1, 0, nil)", updated, upToDate, err)
	}
	got := rec.values()
	if last := got[len(got)-1]; last != "" {
		t.Errorf("库缺失时仍带了 If-None-Match %q；被 304 挡回的话库就永远补不回来", last)
	}
}

// 304 之后当天不得反复重问。
//
// ShouldUpdateNow 的判据是库的生成时间，而 304 不会更新它。不单独记下
// 「已向上游确认过」的话，这个每 10 分钟跑一次的任务会整天不停地问。
func TestScheduledUpdateStopsRecheckingAfterNotModified(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "04:00")
	dir := t.TempDir()
	path := filepath.Join(dir, "ipdb.dat")
	db := seedDatabaseAt(t, path, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	url, rec := etagSource(t, ipdbSampleSource, `"v1"`)

	src := urlSource("test", path, url)
	src.EtagKey, src.CheckedAtKey = "testEtag", "testCheckedAt"
	useTestSources(t, []ipdbSource{src})

	svc := &IPDBService{}
	svc.setDB("test", db)
	ss := SettingService{}
	if err := ss.setString("testEtag", `"v1"`); err != nil {
		t.Fatalf("写入 ETag: %v", err)
	}

	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{now, now.Add(10 * time.Minute), now.Add(20 * time.Minute)} {
		updated, err := svc.RunScheduledUpdate(at)
		if err != nil || updated != 0 {
			t.Fatalf("第 %d 轮 = (%d, %v)，上游未变更时不应计入更新", i+1, updated, err)
		}
	}
	if n := len(rec.values()); n != 1 {
		t.Errorf("向上游发起了 %d 次请求，期望 1 次：304 之后当天不该反复重问", n)
	}
}
