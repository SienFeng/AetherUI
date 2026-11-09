package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// AutoTravelJob 每分钟跑一轮自动出差放行，改了通行证就置重启标志，交给 10 秒的
// 重启消费任务热下发路由。
type AutoTravelJob struct {
	autoTravelService service.AutoTravelService
	xrayService       service.XrayService
}

func NewAutoTravelJob() *AutoTravelJob {
	return new(AutoTravelJob)
}

func (j *AutoTravelJob) Run() {
	defer common.Recover("自动出差放行任务")
	changed, err := j.autoTravelService.Run(time.Now())
	if err != nil {
		logger.Warning("自动出差放行失败:", err)
		return
	}
	if changed {
		j.xrayService.SetToNeedRestart()
	}
}
