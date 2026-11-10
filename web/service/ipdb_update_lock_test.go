package service

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingSource 起一个本地源：第一个请求挂住，直到测试调用 release；之后的请求
// 立刻返回样例数据。entered 在第一个请求到达时关闭，hits 记总请求数。
func blockingSource(t *testing.T) (url string, entered <-chan struct{}, release func(), hits *int32) {
	t.Helper()
	var n int32
	in := make(chan struct{})
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			close(in)
			<-gate
		}
		_, _ = w.Write([]byte(ipdbSampleSource))
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// Cleanup 后注册先执行：先放行挂住的请求，srv.Close 才不会一直等它。
	t.Cleanup(release)
	return srv.URL, in, release, &n
}

// startBlockedUpdate 发起一次「手动更新」并等它卡在下载上，返回它的结果通道。
func startBlockedUpdate(t *testing.T, s *IPDBService, entered <-chan struct{}) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := s.UpdateNow("")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("第一次更新迟迟没有发出下载请求")
	}
	return done
}

// 两次更新同时跑会同时下载几十 MB、同时写同一个目标文件与它的 ETag/确认时间，
// 谁后写完谁说了算。手动更新撞上正在进行的更新时直接告诉管理员「更新进行中」，
// 不再发第二份下载。
func TestUpdateNowRefusesWhileAnotherUpdateRuns(t *testing.T) {
	setupDB(t)
	url, entered, release, hits := blockingSource(t)
	useTestSources(t, []ipdbSource{urlSource("a", filepath.Join(t.TempDir(), "a.dat"), url)})
	s := &IPDBService{}

	done := startBlockedUpdate(t, s, entered)
	_, _, err := s.UpdateNow("")
	if err == nil || !strings.Contains(err.Error(), "更新进行中") {
		t.Fatalf("另一次更新正在进行时，手动更新应当返回「更新进行中」，实际 err = %v", err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("上游被请求了 %d 次，被拒绝的那次更新不该再发下载请求", n)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("先开始的那次更新失败: %v", err)
	}
	// 锁随更新结束释放，之后的手动更新照常进行。
	if _, _, err := s.UpdateNow(""); err != nil {
		t.Fatalf("上一次更新结束后再更新失败: %v", err)
	}
}

// 定时任务撞上手动更新时跳过这一轮（下一个 10 分钟再判断），不报错。
func TestScheduledUpdateSkipsWhileAnotherUpdateRuns(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "00:00")
	url, entered, release, hits := blockingSource(t)
	useTestSources(t, []ipdbSource{urlSource("a", filepath.Join(t.TempDir(), "a.dat"), url)})
	s := &IPDBService{}

	done := startBlockedUpdate(t, s, entered)
	updated, err := s.RunScheduledUpdate(time.Now())
	if err != nil || updated != 0 {
		t.Fatalf("另一次更新正在进行时定时更新应当跳过，实际 updated = %d, err = %v", updated, err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("上游被请求了 %d 次，定时更新不该在另一次更新进行时下载", n)
	}
	release()
	<-done
}

// 启动后的首次补拉同理。
func TestInitialUpdateSkipsWhileAnotherUpdateRuns(t *testing.T) {
	setupDB(t)
	setUpdateTime(t, "05:00")
	url, entered, release, hits := blockingSource(t)
	src := urlSource("a", filepath.Join(t.TempDir(), "a.dat"), url)
	src.CheckedAtKey = "ipdbLockTestCheckedAt"
	useTestSources(t, []ipdbSource{src})
	s := &IPDBService{}

	done := startBlockedUpdate(t, s, entered)
	updated, err := s.RunInitialUpdate()
	if err != nil || updated != 0 {
		t.Fatalf("另一次更新正在进行时首次补拉应当跳过，实际 updated = %d, err = %v", updated, err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("上游被请求了 %d 次，首次补拉不该在另一次更新进行时下载", n)
	}
	release()
	<-done
}
