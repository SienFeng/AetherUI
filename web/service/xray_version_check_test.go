package service

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"a-ui/xray"
)

// zipWithXray 把 binary 作为 zip 里的 xray 条目打成一个发布包，返回 zip 文件路径。
func zipWithXray(t *testing.T, binary []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "xray", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Xray-test.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openZip(t *testing.T, path string) *zip.Reader {
	t.Helper()
	r, closeZip, err := openXrayZip(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeZip)
	return r
}

func fakeCandidate(body string) []byte {
	return []byte("#!/bin/sh\n" + body + "\n")
}

func staticConfig(cfg map[string]any) func() (map[string]any, error) {
	return func() (map[string]any, error) { return cfg, nil }
}

func assertNoCandidateLeft(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("校验之后 %s 里留下了 %s", dir, e.Name())
	}
}

// 「切换版本」换上的核心不接受当前配置（比如 26.x 删掉了 mKCP 的 header/seed）时，切换必须当场拒绝，
// 而不是先停核心、换文件，再让新核心加载失败——那是整机断网，面板首页还显示 running。
func TestCheckCandidateCoreRefusesConfigTheNewCoreRejects(t *testing.T) {
	dir := t.TempDir()
	r := openZip(t, zipWithXray(t, fakeCandidate(
		`echo 'Failed to start: main: failed to load config files: > The feature mkcp header & seed has been removed'; exit 23`)))
	err := checkCandidateCoreIn(r, dir, staticConfig(map[string]any{"inbounds": []any{}}))
	if err == nil {
		t.Fatal("新核心拒绝了当前配置，切换却被放行")
	}
	if !strings.Contains(err.Error(), "mkcp header & seed has been removed") {
		t.Errorf("报错应当带上新核心的原话: %v", err)
	}
	assertNoCandidateLeft(t, dir)
}

func TestCheckCandidateCoreAcceptsWhenConfigurationOK(t *testing.T) {
	dir := t.TempDir()
	r := openZip(t, zipWithXray(t, fakeCandidate(`echo 'Configuration OK.'; exit 0`)))
	if err := checkCandidateCoreIn(r, dir, staticConfig(map[string]any{})); err != nil {
		t.Fatalf("新核心接受了当前配置，却被拒绝: %v", err)
	}
	assertNoCandidateLeft(t, dir)
}

// 校验与 run -test 的保存时校验方向相反：那边放行的是「证明不了非法」的配置，最坏是核心拒绝启动；
// 这边放行的是一次换核心，放错了就是整机断网。所以起不来（架构不对、包损坏）、不认 run -test、
// 超时都拒绝。
func TestCheckCandidateCoreFailsClosedWhenItCannotVerify(t *testing.T) {
	cases := map[string][]byte{
		"不是可执行文件":             []byte("\x7fELF garbage"),
		"不认 run -test":        fakeCandidate(`echo 'xray: unknown command "run"'; exit 1`),
		"没有 Configuration OK": fakeCandidate(`exit 0`),
	}
	for name, bin := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := openZip(t, zipWithXray(t, bin))
			if err := checkCandidateCoreIn(r, dir, staticConfig(map[string]any{})); err == nil {
				t.Fatal("无法确认新核心能加载当前配置，切换却被放行")
			}
			assertNoCandidateLeft(t, dir)
		})
	}
}

// 用真实的 26.7.28 当「新核心」：它拒绝带 mKCP header 的入站，这正是本次修复要拦下的那种切换。
// 候选必须和 geo 数据在同一个目录：xray 从可执行文件所在目录找 geoip.dat（模板规则与私网守卫都引用它）。
func TestCheckCandidateCoreWithRealXrayRejectsKCPHeader(t *testing.T) {
	requireXrayBinary(t)
	real, err := os.ReadFile(xray.GetBinaryPath())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(xray.GetBinaryPath()), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(abs, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	r := openZip(t, zipWithXray(t, real))

	setupDB(t)
	cfg, err := (&XrayService{}).GetXrayConfig()
	if err != nil {
		t.Fatal(err)
	}
	ok := func() (map[string]any, error) { return configAsMap(cfg) }
	if err := checkCandidateCoreIn(r, dir, ok); err != nil {
		t.Fatalf("真实核心应当接受当前生成的配置: %v", err)
	}

	withKCP := func() (map[string]any, error) {
		m, err := configAsMap(cfg)
		if err != nil {
			return nil, err
		}
		m["inbounds"] = append(m["inbounds"].([]any), map[string]any{
			"tag": "inbound-20999", "port": 20999, "listen": "127.0.0.1", "protocol": "vless",
			"settings": map[string]any{"clients": []any{map[string]any{"id": e2eVlessUUID}}, "decryption": "none"},
			"streamSettings": map[string]any{"network": "kcp",
				"kcpSettings": map[string]any{"header": map[string]any{"type": "wechat-video"}}},
		})
		return m, nil
	}
	err = checkCandidateCoreIn(r, dir, withKCP)
	if err == nil || !strings.Contains(err.Error(), "mkcp header") {
		t.Fatalf("真实 26.7.28 拒绝 mKCP header，切换应当被拦下并带上原因，实际: %v", err)
	}
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	assertNoCandidateLeft(t, dir)
}

// UpdateXray 的顺序：校验必须在停核心之前。拒绝时正在跑的核心不能被停、核心文件不能被换。
func TestUpdateXrayRefusesBeforeStoppingTheCore(t *testing.T) {
	setupDB(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	dir := useFakeXray(t, "exec sleep 30")
	stopXrayAtCleanup(t)
	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, xs)
	before := currentProcess()
	binPath := filepath.Join(dir, xray.GetBinaryPath())
	original, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}

	zipPath := zipWithXray(t, fakeCandidate(`echo 'Failed to start: unknown config field'; exit 23`))
	oldDownload := downloadXrayFn
	downloadXrayFn = func(*ServerService, string) (string, error) { return zipPath, nil }
	t.Cleanup(func() { downloadXrayFn = oldDownload })

	if err := (&ServerService{}).UpdateXray("v99.0.0"); err == nil {
		t.Fatal("新核心拒绝当前配置，UpdateXray 却返回成功")
	}
	if currentProcess() != before || !xs.IsXrayRunning() {
		t.Fatal("校验没通过，正在跑的核心却被停掉/换掉了")
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("校验没通过，核心文件却被换掉了")
	}
	entries, _ := os.ReadDir(filepath.Dir(binPath))
	for _, e := range entries {
		if strings.Contains(e.Name(), "candidate") {
			t.Errorf("bin/ 里留下了候选核心 %s", e.Name())
		}
	}
}
