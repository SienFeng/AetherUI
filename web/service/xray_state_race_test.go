package service

import (
	"sync"
	"testing"

	"a-ui/xray"
)

// 面板首页每 2 秒查一次 xray 状态（IsXrayRunning / GetXrayResult / GetXrayVersion），流量任务每
// 10 秒取一次流量，都不拿 lock；RestartXray 却在 lock 里换进程。包级变量 p 与 result 的读写
// 必须是并发安全的——result 是字符串，读到撕裂的值可能直接让面板崩掉。用 -race 跑才看得出来。
func TestXrayServiceStateIsSafeForConcurrentUse(t *testing.T) {
	old := currentProcess()
	t.Cleanup(func() {
		lock.Lock()
		swapProcess(old)
		lock.Unlock()
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &XrayService{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.IsXrayRunning()
				s.GetXrayResult()
				s.GetXrayVersion()
				_ = s.GetXrayErr()
				_, _ = s.GetXrayTraffic()
			}
		}()
	}
	for i := 0; i < 200; i++ {
		lock.Lock()
		swapProcess(xray.NewProcess(&xray.Config{}))
		lock.Unlock()
	}
	close(stop)
	wg.Wait()
}
