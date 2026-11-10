package controller

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ServerController 的几个缓存字段同时被两类 goroutine 读写：每 2 秒一次的
// cron 任务（refreshStatus），和前端每 2 秒轮询一次的 /server/status（以及
// 管理员点出来的版本查询）——每个请求都在自己的 goroutine 里。不加锁就是
// 数据竞争，只有 go test -race 看得出来。
//
// 只走不联网的分支：getXrayVersion / refreshPanelVersion 都先把时间戳设成
// 「刚查过」，走缓存那一支，不会去碰 GitHub。
func TestServerControllerCachesAreSafeForConcurrentUse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	a := &ServerController{
		lastGetStatusTime:           now,
		lastGetVersionsTime:         now,
		lastRefreshPanelVersionTime: now,
	}
	call := func(h gin.HandlerFunc) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/server/x", nil)
		h(c)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				// cron 任务的本体：先读 lastGetStatusTime，再刷新 lastStatus。
				a.refreshIfWatched()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				call(a.status)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				call(a.getXrayVersion)
				call(a.refreshPanelVersion)
			}
		}()
	}
	wg.Wait()
}
