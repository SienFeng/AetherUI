package service

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestZip 造一个内容可控的 zip，用来脱网验证解包逻辑。
func writeTestZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "xray.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建测试 zip 失败: %v", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	for name, content := range entries {
		e, err := w.Create(name)
		if err != nil {
			t.Fatalf("写 zip 条目 %s 失败: %v", name, err)
		}
		if _, err := e.Write([]byte(content)); err != nil {
			t.Fatalf("写 zip 条目 %s 内容失败: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭测试 zip 失败: %v", err)
	}
	return path
}

// 目标路径必须显式传入：本包的 TestMain 会 chdir 到仓库根，用相对路径
// bin/ 会直接覆盖仓库里真实的 xray 二进制。
func TestExtractXrayFilesWritesAllThreeTargets(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"xray":        "FAKE-XRAY-BINARY",
		"geosite.dat": "FAKE-GEOSITE",
		"geoip.dat":   "FAKE-GEOIP",
	})

	r, closeZip, err := openXrayZip(zipPath)
	if err != nil {
		t.Fatalf("openXrayZip: %v", err)
	}
	defer closeZip()

	dir := t.TempDir()
	binPath := filepath.Join(dir, "xray-linux-amd64")
	geositePath := filepath.Join(dir, "geosite.dat")
	geoipPath := filepath.Join(dir, "geoip.dat")

	if err := extractXrayFiles(r, binPath, geositePath, geoipPath); err != nil {
		t.Fatalf("extractXrayFiles: %v", err)
	}

	for path, want := range map[string]string{
		binPath:     "FAKE-XRAY-BINARY",
		geositePath: "FAKE-GEOSITE",
		geoipPath:   "FAKE-GEOIP",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("%s 内容期望 %q，实际 %q", path, want, string(got))
		}
	}

	// 核心必须带可执行位：改成「先写临时文件再 rename」之后，权限由临时
	// 文件带过去，用 os.CreateTemp（0600）之类的写法会静默丢掉这一位，
	// 而面板「切换版本」装出来的核心从此起不来——起不来又不回传，只有
	// 用户断流才看得见。
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("stat 核心: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("核心缺少可执行位，实际权限 %v", info.Mode().Perm())
	}

	assertNoTempResidue(t, dir)
}

// assertNoTempResidue 断言目录里没有留下 .tmp 中间文件。
func assertNoTempResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("解包结束后仍残留临时文件 %s", e.Name())
		}
	}
}

// zip 里缺 xray 条目时必须报错，且不能破坏已经存在的目标文件——
// 这是「宁可不更新，也不留下半个核心」。
func TestExtractXrayFilesMissingBinaryLeavesTargetsIntact(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"geosite.dat": "FAKE-GEOSITE",
		"geoip.dat":   "FAKE-GEOIP",
	})

	r, closeZip, err := openXrayZip(zipPath)
	if err != nil {
		t.Fatalf("openXrayZip: %v", err)
	}
	defer closeZip()

	dir := t.TempDir()
	binPath := filepath.Join(dir, "xray-linux-amd64")
	if err := os.WriteFile(binPath, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatalf("预置旧二进制: %v", err)
	}

	err = extractXrayFiles(r, binPath, filepath.Join(dir, "geosite.dat"), filepath.Join(dir, "geoip.dat"))
	if err == nil {
		t.Fatal("zip 缺 xray 条目，期望报错，实际成功")
	}

	got, readErr := os.ReadFile(binPath)
	if readErr != nil {
		t.Fatalf("读取旧二进制: %v", readErr)
	}
	if string(got) != "OLD-BINARY" {
		t.Fatalf("解包失败时旧二进制被破坏，实际内容 %q", string(got))
	}

	assertNoTempResidue(t, dir)
}

// zip 里**有 xray 但缺 geosite.dat**——这正是修复前会毁掉核心的那条路径：
// 三个条目各写各的，等到第二个条目报错时核心已经被原地换掉了，「缺条目不会
// 破坏已有的核心」只对 xray 这一个条目成立。改成先全部写临时文件、全部成功
// 才 rename 之后，这里三个目标必须一个都没变。
func TestExtractXrayFilesMissingGeositeLeavesAllTargetsIntact(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"xray":      "NEW-BINARY",
		"geoip.dat": "NEW-GEOIP",
	})

	r, closeZip, err := openXrayZip(zipPath)
	if err != nil {
		t.Fatalf("openXrayZip: %v", err)
	}
	defer closeZip()

	dir := t.TempDir()
	binPath := filepath.Join(dir, "xray-linux-amd64")
	geositePath := filepath.Join(dir, "geosite.dat")
	geoipPath := filepath.Join(dir, "geoip.dat")
	old := map[string]string{
		binPath:     "OLD-BINARY",
		geositePath: "OLD-GEOSITE",
		geoipPath:   "OLD-GEOIP",
	}
	for path, content := range old {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("预置 %s: %v", path, err)
		}
	}

	if err := extractXrayFiles(r, binPath, geositePath, geoipPath); err == nil {
		t.Fatal("zip 缺 geosite.dat 条目，期望报错，实际成功")
	}

	for path, want := range old {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("读取 %s: %v", path, readErr)
		}
		if string(got) != want {
			t.Fatalf("解包失败时 %s 被改动，期望 %q，实际 %q", path, want, string(got))
		}
	}

	assertNoTempResidue(t, dir)
}

// 损坏的 zip 必须在 openXrayZip 这一步就被拒绝——UpdateXray 靠这一步
// 排在 StopXray 之前，才能做到「包坏了就不停核心」。
func TestOpenXrayZipRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.zip")
	if err := os.WriteFile(path, []byte("this is not a zip"), 0o644); err != nil {
		t.Fatalf("写损坏文件: %v", err)
	}

	if _, _, err := openXrayZip(path); err == nil {
		t.Fatal("损坏的 zip 期望报错，实际成功")
	}
}

// GetXrayVersions 拉的是 /releases，首项即最新发布（含 prerelease）。
func TestFirstReleaseTag(t *testing.T) {
	t.Run("正常列表返回首项", func(t *testing.T) {
		got, err := firstReleaseTag([]string{"v26.9.9", "v26.7.28", "v26.3.27"})
		if err != nil {
			t.Fatalf("firstReleaseTag: %v", err)
		}
		if got != "v26.9.9" {
			t.Fatalf("期望 v26.9.9，实际 %q", got)
		}
	})

	t.Run("空列表要报错", func(t *testing.T) {
		// GitHub 限流时返回的是带 message 字段的 JSON 对象，GetXrayVersions
		// 对它反序列化会失败，但空数组同样合法解析出一个长度为 0 的 slice，
		// 必须靠这一条挡住——否则会拿空字符串去拼下载 URL。
		if _, err := firstReleaseTag([]string{}); err == nil {
			t.Fatal("列表为空，期望报错，实际成功")
		}
	})

	t.Run("首项为空串要报错", func(t *testing.T) {
		if _, err := firstReleaseTag([]string{""}); err == nil {
			t.Fatal("首项为空串，期望报错，实际成功")
		}
	})
}
