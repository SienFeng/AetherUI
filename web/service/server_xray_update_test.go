package service

import (
	"archive/zip"
	"os"
	"path/filepath"
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
