package xray

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeXray 在临时目录里放一个假的 xray（shell 脚本），并把工作目录切过去：GetBinaryPath 与
// GetConfigPath 都是相对 bin/ 的路径。脚本收到 -version 时打印版本，否则执行 body。
func fakeXray(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("需要 /bin/sh")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo 'Xray 26.7.28 (fake)'; exit 0; fi\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, GetBinaryPath()), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

// 面板首页每 2 秒查一次状态、流量任务每 10 秒取一次端口、运行检查任务每 30 秒查一次是否在跑，
// 它们都不拿 XrayService 的锁；进程的启动与退出却发生在 Start 起的 goroutine 里。这些读写
// 必须是并发安全的——用 -race 跑这条测试才能看出来（go test -race ./xray -run Concurrent）。
func TestProcessStateIsSafeForConcurrentUse(t *testing.T) {
	fakeXray(t, "sleep 0.3")
	p := NewProcess(&Config{InboundConfigs: []InboundConfig{{Tag: "api", Port: 62789}}})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sawRunning := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.IsRunning() {
			sawRunning = true
		} else if sawRunning {
			break // 跑过、又退出了
		}
		_ = p.GetErr()
		_ = p.GetVersion()
		_ = p.GetAPIPort()
		_ = p.GetConfig()
		time.Sleep(time.Millisecond)
	}
	if !sawRunning {
		t.Fatal("假 xray 应当跑起来过")
	}
	if p.IsRunning() {
		t.Fatal("假 xray 退出后不应再显示为运行中")
	}
	if p.GetAPIPort() != 62789 || p.GetVersion() != "26.7.28" {
		t.Errorf("端口与版本: %d %q", p.GetAPIPort(), p.GetVersion())
	}

	// Stop 与等待退出的 goroutine 同时动这个进程。
	q := NewProcess(&Config{})
	fakeXray(t, "sleep 30")
	if err := q.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 5000 && !q.IsRunning(); i++ {
		time.Sleep(time.Millisecond)
	}
	if err := q.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i := 0; i < 5000 && q.IsRunning(); i++ {
		time.Sleep(time.Millisecond)
	}
	if q.IsRunning() {
		t.Fatal("Stop 之后进程应当退出")
	}
}

// 二进制起不来时 Start 照旧返回 nil（启动失败只记在 GetErr 里，见 CLAUDE.md），
// 后台记下的错误与前台的读取之间同样不能有竞争。
func TestProcessStartFailureIsRecordedSafely(t *testing.T) {
	fakeXray(t, "exit 0")
	if err := os.Remove(GetBinaryPath()); err != nil {
		t.Fatal(err)
	}
	p := NewProcess(&Config{})
	if err := p.Start(); err != nil {
		t.Fatalf("Start 应当照旧返回 nil: %v", err)
	}
	for i := 0; i < 5000 && p.GetErr() == nil; i++ {
		_ = p.IsRunning()
		time.Sleep(time.Millisecond)
	}
	if p.GetErr() == nil || p.IsRunning() {
		t.Fatalf("起不来的进程应当记下错误且不在运行: err=%v running=%v", p.GetErr(), p.IsRunning())
	}
}

// 同一个进程对象被同时 Start 两次：只能真的起一个。修复前「是否在跑」要等后台 goroutine 把
// 子进程起来之后才为真，同时到达的两次调用都会通过检查、起两个 xray。
func TestProcessConcurrentStartLaunchesOnce(t *testing.T) {
	fakeXray(t, "sleep 30")
	for round := 0; round < 20; round++ {
		p := NewProcess(&Config{})
		results := make(chan error, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				results <- p.Start()
			}()
		}
		close(start)
		ok := 0
		for i := 0; i < 2; i++ {
			if err := <-results; err == nil {
				ok++
			}
		}
		for i := 0; i < 5000 && !p.IsRunning(); i++ {
			time.Sleep(time.Millisecond)
		}
		_ = p.Stop()
		if ok != 1 {
			t.Fatalf("第 %d 轮：同时 Start 两次，成功了 %d 次", round, ok)
		}
	}
}
