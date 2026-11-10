package service

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// settingBackedSource 造一个启用状态由设置项决定的测试源，判定与正式的四个源相同：
// 源地址为空即停用（enabledByURL）。
func settingBackedSource(key, path, settingKey string) ipdbSource {
	src := testSource(path, 1)
	src.Key, src.Name = key, key
	src.URL = func(s *SettingService) (string, error) { return s.getOptionalString(settingKey) }
	src.Enabled = enabledByURL(src.URL)
	return src
}

// loadedKeys 返回 DB() 此刻参与查询的源。
func loadedKeys(s *IPDBService) string {
	m := s.DB()
	if m == nil {
		return ""
	}
	var keys []string
	for _, n := range m.Sources() {
		keys = append(keys, n.Key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func setSourceURL(t *testing.T, key, value string) {
	t.Helper()
	if err := (&SettingService{}).setString(key, value); err != nil {
		t.Fatal(err)
	}
}

// 界面上显示「未启用」（源地址留空）的源，文件往往还在磁盘上。面板每次启动都把它加载回来的话，
// 它会继续参与归属地判定、多数票、画像键与地区限制的允许集——和界面说的正相反。
func TestLoadSkipsSourceWhoseURLIsCleared(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	built := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seedDatabaseAt(t, filepath.Join(dir, "on.dat"), built)
	seedDatabaseAt(t, filepath.Join(dir, "off.dat"), built)
	useTestSources(t, []ipdbSource{
		settingBackedSource("on", filepath.Join(dir, "on.dat"), "testOnUrl"),
		settingBackedSource("off", filepath.Join(dir, "off.dat"), "testOffUrl"),
	})
	setSourceURL(t, "testOnUrl", "https://example.com/on.txt")

	s := IPDBService{}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := loadedKeys(&s); got != "on" {
		t.Fatalf("加载的源 = %q，期望只有启用了的 on", got)
	}
}

// 保存设置时清空源地址：这个源立刻从内存卸下，并要求重新下发核心配置。
func TestApplySourceTogglesUnloadsClearedSource(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	built := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seedDatabaseAt(t, filepath.Join(dir, "on.dat"), built)
	seedDatabaseAt(t, filepath.Join(dir, "off.dat"), built)
	useTestSources(t, []ipdbSource{
		settingBackedSource("on", filepath.Join(dir, "on.dat"), "testOnUrl"),
		settingBackedSource("off", filepath.Join(dir, "off.dat"), "testOffUrl"),
	})
	setSourceURL(t, "testOnUrl", "https://example.com/on.txt")
	setSourceURL(t, "testOffUrl", "https://example.com/off.txt")
	s := IPDBService{}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}

	before, err := s.EnabledSources()
	if err != nil {
		t.Fatal(err)
	}
	setSourceURL(t, "testOffUrl", "")
	if !s.ApplySourceToggles(before) {
		t.Error("有源被停用，应当报告变化（调用方据此置重启标志）")
	}
	if got := loadedKeys(&s); got != "on" {
		t.Fatalf("停用之后参与查询的源 = %q，期望只剩 on", got)
	}
	status, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range status.Sources {
		if st.Key == "off" && (st.Enabled || st.Loaded) {
			t.Errorf("off 的状态 = %+v，期望未启用、未加载", st)
		}
	}
}

// 反过来：重新填上源地址时，磁盘上那份库直接装回来，不必为此重新下载
// （IP2Location 每 24 小时只放行 5 次）。
func TestApplySourceTogglesReloadsReenabledSourceFromDisk(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	built := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seedDatabaseAt(t, filepath.Join(dir, "on.dat"), built)
	seedDatabaseAt(t, filepath.Join(dir, "off.dat"), built)
	useTestSources(t, []ipdbSource{
		settingBackedSource("on", filepath.Join(dir, "on.dat"), "testOnUrl"),
		settingBackedSource("off", filepath.Join(dir, "off.dat"), "testOffUrl"),
	})
	setSourceURL(t, "testOnUrl", "https://example.com/on.txt")
	s := IPDBService{}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}

	before, err := s.EnabledSources()
	if err != nil {
		t.Fatal(err)
	}
	setSourceURL(t, "testOffUrl", "https://example.com/off.txt")
	if !s.ApplySourceToggles(before) {
		t.Error("有源被重新启用，应当报告变化")
	}
	if got := loadedKeys(&s); got != "off,on" {
		t.Fatalf("重新启用之后参与查询的源 = %q，期望 off,on", got)
	}
}

// 存设置时启用状态没变（改的是别的项）：不装卸、不要求下发。
func TestApplySourceTogglesReportsNoChange(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	seedDatabaseAt(t, filepath.Join(dir, "on.dat"), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	useTestSources(t, []ipdbSource{settingBackedSource("on", filepath.Join(dir, "on.dat"), "testOnUrl")})
	setSourceURL(t, "testOnUrl", "https://example.com/on.txt")
	s := IPDBService{}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	before, err := s.EnabledSources()
	if err != nil {
		t.Fatal(err)
	}
	setSourceURL(t, "testOnUrl", "https://example.com/on-mirror.txt")
	if s.ApplySourceToggles(before) {
		t.Error("启用状态没变，不该报告变化")
	}
	if got := loadedKeys(&s); got != "on" {
		t.Fatalf("参与查询的源 = %q，期望 on", got)
	}
}

// 下载一个源可能要几分钟（ip2region 35 MB、DB-IP 82 MB）。期间管理员清空了源地址的话，
// 下载完成时不能再把它装回去——界面上它已经是「未启用」。
func TestSetDBDoesNotInstallSourceDisabledWhileDownloading(t *testing.T) {
	setupDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "off.dat")
	db := seedDatabaseAt(t, path, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	useTestSources(t, []ipdbSource{settingBackedSource("off", path, "testOffUrl")})

	s := IPDBService{}
	s.setDB("off", db)
	if got := loadedKeys(&s); got != "" {
		t.Fatalf("源地址已清空的源被装进了内存: %q", got)
	}
}

// 四个正式的源都按「源地址是否为空」判定启用，没有一个漏掉判定（漏掉就是永远启用）。
func TestProductionSourcesAreEnabledByTheirURL(t *testing.T) {
	setupDB(t)
	ss := &SettingService{}
	urlKeys := map[string]string{
		ip2regionKey: "ipdbSourceUrl", qqwryKey: "qqwrySourceUrl",
		ip2locationKey: "ip2locationSourceUrl", dbipKey: "dbipSourceUrl",
	}
	for _, src := range ipdbSourceList() {
		if src.Enabled == nil {
			t.Errorf("正式源 %s 没有启用判定", src.Key)
			continue
		}
		key, ok := urlKeys[src.Key]
		if !ok {
			t.Errorf("未知的正式源 %s：给它补上启用判定的用例", src.Key)
			continue
		}
		setSourceURL(t, key, "")
		if on, err := src.Enabled(ss); err != nil || on {
			t.Errorf("%s 源地址留空时 Enabled = %v, %v，期望停用", src.Key, on, err)
		}
		setSourceURL(t, key, "https://example.com/"+src.Key+".dat")
		if on, err := src.Enabled(ss); err != nil || !on {
			t.Errorf("%s 填了源地址时 Enabled = %v, %v，期望启用", src.Key, on, err)
		}
	}
}
