package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/netdiag"
	"a-ui/web/service"
)

// SharingSampleJob 每轮采一次各入站的活跃来源 IP，喂给共享检测。
//
// 独立于 ConcurrencyJob：后者在「无人设并发额度且无封禁」时一次系统调用
// 都不做，把采集挂在它后面的话，检测在最常见的默认配置下会无声失效。
type SharingSampleJob struct {
	sharingService service.SharingService

	// 平台不支持时只提示一次，否则每 30 秒刷一条日志。与 ConcurrencyJob
	// 同一个处理方式。
	unsupportedWarned bool
}

func NewSharingSampleJob() *SharingSampleJob {
	return new(SharingSampleJob)
}

func (j *SharingSampleJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层：日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("共享检测采样任务")

	err := j.sharingService.Sample(time.Now())
	if err == nil {
		return
	}
	if err == netdiag.ErrUnsupported {
		if !j.unsupportedWarned {
			j.unsupportedWarned = true
			logger.Warning("当前系统不支持内核连接表，共享检测不会生效:", err)
		}
		return
	}
	// 采集失败只告警，绝不阻断任何既有流程：检测是辅助手段。
	logger.Warning("共享检测采样失败:", err)
}
