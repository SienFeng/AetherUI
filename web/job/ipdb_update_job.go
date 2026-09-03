package job

import (
	"time"

	"a-ui/logger"
	"a-ui/web/service"
)

// IPDBUpdateJob 每 10 分钟自检一次：到了配置的更新时刻就重建各个 IP 归属地库。
//
// 更新时刻默认留空，即关闭自动更新——源数据几十 MB，是否定期拉取由管理员决定。
// 关闭时本任务不会发起任何网络请求。
//
// 10 分钟的自检间隔与域名组订阅一致：管理员改了更新时刻，或时刻自然到达，
// 都最多 10 分钟内生效，不需要重启面板。
//
// 注意：cron 没有 panic 恢复（Server.Start 里的 cron.New 未配 cron.Recover），
// 这个 Run 里的任何 panic 都会杀掉整个面板进程，因此所有失败都走 error 返回。
type IPDBUpdateJob struct {
	ipdbService service.IPDBService
}

func NewIPDBUpdateJob() *IPDBUpdateJob {
	return new(IPDBUpdateJob)
}

func (j *IPDBUpdateJob) Run() {
	updated, err := j.ipdbService.RunScheduledUpdate(time.Now())
	if err != nil {
		// 更新失败时旧库仍在生效，功能不受影响，所以只告警不中断。
		logger.Warning("update ip database err:", err)
		return
	}
	if updated > 0 {
		logger.Info("ip database updated, 数据源个数:", updated)
	}
}
