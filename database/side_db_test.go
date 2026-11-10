package database

import (
	"path/filepath"
	"sync"
	"testing"
)

// 面板「重启」是 SIGHUP 重建 Server，每次 Server.Start 都会再调一遍
// InitAccessLogDB / InitTrafficDB。改动前它们每次都新开一个连接池、直接覆盖包级
// 变量，旧池从不关闭——点一次重启漏两个池。同一个文件已经开着时必须复用原来的
// 句柄：旧 Server 还在收尾的定时任务手里拿的正是它，关掉或换掉都会让那些任务
// 撞上 "database is closed"。
func TestSideDatabasesReuseTheOpenHandleForTheSamePath(t *testing.T) {
	t.Cleanup(ResetTrafficDBForTest)
	t.Cleanup(resetAccessLogDBForTest)
	dir := t.TempDir()
	cases := []struct {
		name string
		init func(string) error
		get  func() interface{}
	}{
		{"访问日志库", InitAccessLogDB, func() interface{} { return GetAccessLogDB() }},
		{"用量库", InitTrafficDB, func() interface{} { return GetTrafficDB() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".db")
			if err := c.init(path); err != nil {
				t.Fatalf("首次初始化: %v", err)
			}
			first := c.get()
			for i := 0; i < 3; i++ {
				if err := c.init(path); err != nil {
					t.Fatalf("第 %d 次重复初始化: %v", i+2, err)
				}
				if c.get() != first {
					t.Fatalf("第 %d 次重复初始化换了一个新句柄，旧连接池泄漏", i+2)
				}
			}
		})
	}
}

// 句柄的读取方（各个定时任务、各个请求）与初始化方不在同一个 goroutine 上。
// 改动前是裸的包级变量，只有 go test -race 看得出来。
func TestSideDatabaseHandlesAreSafeToReadWhileInitializing(t *testing.T) {
	t.Cleanup(ResetTrafficDBForTest)
	t.Cleanup(resetAccessLogDBForTest)
	dir := t.TempDir()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = GetAccessLogDB()
				_ = GetTrafficDB()
			}
		}
	}()
	for i := 0; i < 3; i++ {
		if err := InitAccessLogDB(filepath.Join(dir, "access.db")); err != nil {
			t.Fatal(err)
		}
		if err := InitTrafficDB(filepath.Join(dir, "traffic.db")); err != nil {
			t.Fatal(err)
		}
		// 换个路径再来一次：测试里常见（每个用例一个临时库），同样要安全。
		if err := InitTrafficDB(filepath.Join(dir, "traffic-2.db")); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
