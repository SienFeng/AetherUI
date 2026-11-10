package xray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// bin/config.json 里有全部用户的 UUID、密码与 REALITY 私钥，只能是属主可读写。
// 修复前按 fs.ModePerm 新建，umask 022 下落成 0755，同机任何用户都读得到。
func TestStartWritesConfigFileOwnerOnly(t *testing.T) {
	fakeXray(t, "sleep 2")
	p := NewProcess(&Config{InboundConfigs: []InboundConfig{{Tag: "api", Port: 62789}}})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	info, err := os.Stat(GetConfigPath())
	if err != nil {
		t.Fatalf("stat %s: %v", GetConfigPath(), err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("bin/config.json 权限 = %v，期望 0600", perm)
	}
}

// 升级前留下的 0755 文件也要收紧：os.WriteFile 对已存在的文件不改权限位，只靠「新建时
// 给 0600」修不到存量部署。
func TestWriteConfigFileTightensExistingPermissions(t *testing.T) {
	fakeXray(t, "exit 0")
	if err := os.WriteFile(GetConfigPath(), []byte("{}"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(GetConfigPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigFile(&Config{}); err != nil {
		t.Fatalf("WriteConfigFile: %v", err)
	}
	info, err := os.Stat(GetConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("存量 0755 的 bin/config.json 重写后权限 = %v，期望 0600", perm)
	}
}

// 写失败（磁盘满、只读、目标是目录……）时原文件必须一个字节都不动，也不留临时文件。
// os.WriteFile 是先截断再写，失败时留下的是半截配置。
func TestWriteConfigFileLeavesOldFileOnFailure(t *testing.T) {
	fakeXray(t, "exit 0")
	old := []byte(`{"old":true}`)
	if err := os.WriteFile(GetConfigPath(), old, 0o600); err != nil {
		t.Fatal(err)
	}
	// 目录只读：临时文件建不出来，写入必然失败。
	binDir := filepath.Dir(GetConfigPath())
	if err := os.Chmod(binDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(binDir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限约束，构造不出写失败")
	}

	if err := WriteConfigFile(&Config{InboundConfigs: []InboundConfig{{Tag: "api", Port: 1}}}); err == nil {
		t.Fatal("bin/ 只读时 WriteConfigFile 应当报错")
	}
	got, err := os.ReadFile(GetConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("写失败后原文件被改动: %q", got)
	}
	entries, _ := os.ReadDir(binDir)
	for _, e := range entries {
		if e.Name() != "config.json" && e.Name() != filepath.Base(GetBinaryPath()) {
			t.Errorf("写失败后残留了文件: %s", e.Name())
		}
	}
}

// WriteConfigFile 写出的就是 Start 会交给 xray 的那份：同一份配置两次写出的字节相同。
func TestWriteConfigFileContentMatchesConfig(t *testing.T) {
	fakeXray(t, "exit 0")
	cfg := &Config{InboundConfigs: []InboundConfig{{Tag: "api", Port: 62789, Protocol: "dokodemo-door"}}}
	if err := WriteConfigFile(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(GetConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("写出的配置与原配置不一致:\n%s\n期望:\n%s", got, want)
	}
}

// Stop 返回时旧进程必须已经退出：调用方紧接着就要起新进程，旧进程还占着端口的话，
// 新 xray 会因 bind: address already in use 起不来，整机所有入站一起断。
// 修复前 Stop 只发 SIGKILL 就返回，回收发生在另一个 goroutine 里。
func TestStopWaitsForProcessExit(t *testing.T) {
	fakeXray(t, "exec sleep 30")
	for i := 0; i < 10; i++ {
		p := NewProcess(&Config{InboundConfigs: []InboundConfig{{Tag: "api", Port: 62789}}})
		if err := p.Start(); err != nil {
			t.Fatalf("第 %d 轮 Start: %v", i, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !p.IsRunning() {
			if time.Now().After(deadline) {
				t.Fatalf("第 %d 轮：假 xray 没跑起来", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("第 %d 轮 Stop: %v", i, err)
		}
		if p.IsRunning() {
			t.Fatalf("第 %d 轮：Stop 返回时旧进程仍显示在运行，调用方此时起新进程会撞上旧进程占着的端口", i)
		}
	}
}
