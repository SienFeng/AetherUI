package job

import (
	"a-ui/logger"
	"a-ui/web/service"
)

// SubscriptionJob 每 10 分钟检查一次有没有到点该更新的订阅域名组。
//
// cron 配了 cron.Recover（web/web.go）作为通用兜底，panic 不会杀掉面板进程，但这一轮之后的逻辑
// 就不跑了。可能失败的操作仍然走 error 返回，不依赖 recover。
type SubscriptionJob struct {
	domainGroupService service.DomainGroupService
	xrayService        service.XrayService
}

func NewSubscriptionJob() *SubscriptionJob {
	return new(SubscriptionJob)
}

func (j *SubscriptionJob) Run() {
	// 孤儿兜底：DomainGroupService.Del 已在正常路径上连带删了订阅结果行，
	// 这里挡的是手工改库和 Del 中途失败。SQLite 会复用被删除的自增 id，
	// 残留行会绑到下一个新建的域名组上，让它莫名其妙带着别人的订阅内容参与
	// 分流——而引用不再悬空，生成期的跳过防线拦不住。
	if pruned, err := j.domainGroupService.PruneSubscriptionOrphans(); err != nil {
		logger.Warning("prune orphan domain group subscriptions err:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除域名组遗留的订阅结果", pruned)
	}

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
