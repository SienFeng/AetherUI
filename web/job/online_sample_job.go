package job

import (
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/netdiag"
	"a-ui/web/service"
)

// OnlineSampleJob 按 service.OnlineSampleInterval 采一次内核连接表。
//
// 在此之前常驻的采样只有 30 秒一次的共享采样：并发判定在无人设额度时一次
// 都不采，页面轮询只在管理员开着展开行时才有。30 秒超过 onlineMaxSampleGap，
// 每一轮都只重建基准，来源 30 秒后就被判闲置，共享检测收不到活跃时长。
// 采样本身有 500ms 的去重闸门，与并发判定、页面轮询共存不会重复读表。
type OnlineSampleJob struct {
	onlineService service.OnlineService

	// 平台不支持时只提示一次，否则每 5 秒刷一条日志。
	unsupportedWarned bool
}

func NewOnlineSampleJob() *OnlineSampleJob {
	return new(OnlineSampleJob)
}

func (j *OnlineSampleJob) Run() {
	defer common.Recover("连接表采样任务")

	err := j.onlineService.Sample()
	if err == nil {
		return
	}
	if err == netdiag.ErrUnsupported {
		if !j.unsupportedWarned {
			j.unsupportedWarned = true
			logger.Warning("当前系统不支持内核连接表，在线明细与按来源 IP 的用量不会生效:", err)
		}
		return
	}
	// 失败只告警：这是辅助数据，下一轮自然重试。
	logger.Warning("连接表采样失败:", err)
}
