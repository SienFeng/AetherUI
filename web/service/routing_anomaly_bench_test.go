package service

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// 实测大规则扫描的内存与耗时：10 万条 domain: 的组 + 1 万条访问日志跑一次大规则扫描，
// 峰值堆增量必须小于 100 MB（1 核 1 GB 的目标机器的预算）。数字写进 CLAUDE.md。
func TestAnomalyScanLargeGroupFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("占用较多内存与时间，-short 下跳过")
	}
	const (
		domainCount = 100000
		logCount    = 10000
		budgetBytes = 100 << 20
	)
	domains := make([]string, domainCount)
	for i := range domains {
		domains[i] = fmt.Sprintf("domain:d%d.example.com", i)
	}
	domains[0] = "domain:openai.com"
	f := newAnomalyFixture(t, domains, nil)

	rows := make([]model.AccessLog, logCount)
	for i := range rows {
		r := wrongRow(f.a.Id, anomalyNow.Add(-time.Duration(i%3600)*time.Second))
		if i%2 == 1 {
			r.Target = fmt.Sprintf("d%d.example.com:443", i)
		}
		rows[i] = r
	}
	if err := database.GetAccessLogDB().CreateInBatches(rows, 500).Error; err != nil {
		t.Fatalf("insert logs: %v", err)
	}
	rows, domains = nil, nil // 测量前放掉测试自己的大切片，只量扫描的增量

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// 后台采样峰值 HeapAlloc：扫描结束后匹配器已释放，只看结束时的值会低估。
	var peak uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak {
				peak = ms.HeapAlloc
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	start := time.Now()
	err := f.svc.ScanAccessLog(anomalyNow, true)
	elapsed := time.Since(start)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("ScanAccessLog(large): %v", err)
	}

	growth := int64(peak) - int64(before.HeapAlloc)
	t.Logf("大规则扫描：%d 条域名 / %d 条日志，耗时 %.2fs，扫描前 HeapAlloc %.1f MB，峰值 %.1f MB，增量 %.1f MB",
		domainCount, logCount, elapsed.Seconds(),
		float64(before.HeapAlloc)/(1<<20), float64(peak)/(1<<20), float64(growth)/(1<<20))
	if growth >= budgetBytes {
		t.Fatalf("峰值堆增量 %.1f MB 超过 %d MB 预算", float64(growth)/(1<<20), budgetBytes>>20)
	}
	if a := f.svc.Anomaly(f.rule.Id, anomalyNow); a.Count == 0 {
		t.Fatalf("扫描应记到异常（openai.com 走了默认出站）: %+v", a)
	}
}
