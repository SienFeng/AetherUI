package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// SpikeThrottleJob 每 10 分钟检查一次流量暴涨（见 service.SpikeThrottleService）。限速本身由 ShapingJob
// 每 10 秒按 desiredLimits 收敛下去，这里不碰 xray，也不置重启标志。
type SpikeThrottleJob struct {
	service service.SpikeThrottleService
}

func NewSpikeThrottleJob() *SpikeThrottleJob {
	return new(SpikeThrottleJob)
}

func (j *SpikeThrottleJob) Run() {
	defer common.Recover("流量暴涨检测任务")
	if _, err := j.service.Run(time.Now()); err != nil {
		logger.Warning("流量暴涨检测失败:", err)
	}
}
