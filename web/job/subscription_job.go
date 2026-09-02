package job

import (
	"a-ui/logger"
	"a-ui/web/service"
)

// SubscriptionJob 每 10 分钟检查一次有没有到点该更新的订阅域名组。
//
// 注意：cron 没有 panic 恢复（Server.Start 里的 cron.New 未配 cron.Recover），
// 这个 Run 里的任何 panic 都会杀掉整个面板进程。所有可能失败的操作都必须
// 走 error 返回，不能依赖 recover 兜底。
type SubscriptionJob struct {
	domainGroupService service.DomainGroupService
	xrayService        service.XrayService
}

func NewSubscriptionJob() *SubscriptionJob {
	return new(SubscriptionJob)
}

func (j *SubscriptionJob) Run() {
	count, err := j.domainGroupService.RefreshDue()
	if err != nil {
		logger.Warning("refresh domain group subscriptions err:", err)
		return
	}
	if count > 0 {
		logger.Debugf("refreshed %v domain group subscriptions", count)
		// 复用既有链路：置标志 → InboundController 的 10 秒 cron 消费 →
		// RestartXray(false) → Config.Equals 发现 RouterConfig 变了 → 重启 xray。
		// 管理员不需要重启面板，也不需要重启 xray。
		j.xrayService.SetToNeedRestart()
	}
}
