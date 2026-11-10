package job

import (
	"time"

	"a-ui/logger"
	"a-ui/web/service"
)

type CheckInboundJob struct {
	xrayService       service.XrayService
	inboundService    service.InboundService
	regionPassService service.RegionPassService
}

func NewCheckInboundJob() *CheckInboundJob {
	return new(CheckInboundJob)
}

func (j *CheckInboundJob) Run() {
	count, err := j.inboundService.DisableInvalidInbounds()
	if err != nil {
		logger.Warning("disable invalid inbounds err:", err)
	}
	// 部分成功也要下发：DisableInvalidInbounds 是两条 UPDATE，第二条失败时第一条停掉的入站已经
	// 落库，count 就是它停掉的条数。只在 err == nil 时置标志，这些入站在库里已停用、核心里却继续跑。
	if count > 0 {
		logger.Debugf("disabled %v inbounds", count)
		j.xrayService.SetToNeedRestart()
	}

	// 端口被别的程序占着而没下发的入站：端口空出来了就置重启标志把它加回去。
	j.xrayService.RecheckPortConflicts()

	// 出差通行证到期由这里改库完成：生成期不读时间。失败时通行证会一直生效，
	// 方向是放宽，必须告警。
	n, err := j.regionPassService.ExpireDue(time.Now())
	if err != nil {
		logger.Warning("清理到期的出差通行证失败，这些通行证会继续生效:", err)
	} else if n > 0 {
		j.xrayService.SetToNeedRestart()
	}
}
