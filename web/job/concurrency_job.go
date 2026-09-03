package job

import (
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/netdiag"
	"a-ui/web/service"
)

// ConcurrencyJob 每轮重新判定各入站的并发额度并断开超额来源。
//
// 判定是幂等收敛的（见 ConcurrencyService.Enforce），漏跑一轮不会让状态跑偏。
type ConcurrencyJob struct {
	concurrencyService service.ConcurrencyService

	// 平台不支持时只提示一次，否则每秒刷一条日志。
	unsupportedWarned bool
}

func NewConcurrencyJob() *ConcurrencyJob {
	return new(ConcurrencyJob)
}

func (j *ConcurrencyJob) Run() {
	// cron 自身不做 panic 恢复，这里的 panic 会杀掉整个面板进程。
	defer common.Recover("并发判定任务")

	err := j.concurrencyService.Enforce()
	if err == nil {
		return
	}
	if err == netdiag.ErrUnsupported {
		if !j.unsupportedWarned {
			j.unsupportedWarned = true
			logger.Warning("已配置并发限制，但当前系统不支持内核连接表，限制不会生效:", err)
		}
		return
	}
	logger.Warning("并发判定失败:", err)
}
