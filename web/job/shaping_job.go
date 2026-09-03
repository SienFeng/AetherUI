package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/tcshape"
	"a-ui/web/service"
)

// ShapingJob 让内核里的 tc 限速规则与库里的配置保持一致，并在下发新配置后
// 迟迟收不到面板请求时自动撤销。
type ShapingJob struct {
	shapingService service.ShapingService

	unsupportedWarned bool
}

func NewShapingJob() *ShapingJob {
	return new(ShapingJob)
}

func (j *ShapingJob) Run() {
	// cron 自身不做 panic 恢复，这里的 panic 会杀掉整个面板进程。
	defer common.Recover("限速任务")

	// 撤销检查放在最前面：即使这一轮的下发出问题，超时保护也要照常生效。
	j.shapingService.CheckRollback(time.Now())

	err := j.shapingService.Reconcile()
	if err == nil {
		return
	}
	if err == tcshape.ErrUnsupported {
		if !j.unsupportedWarned {
			j.unsupportedWarned = true
			logger.Warning("已配置端口限速，但当前系统不支持 tc，限速不会生效:", err)
		}
		return
	}
	logger.Warning("下发端口限速失败:", err)
}
