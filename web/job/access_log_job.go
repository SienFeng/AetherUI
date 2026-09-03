package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// AccessLogCollectJob 把 xray 写下的访问日志读进独立的 SQLite 库。
//
// 关闭访问日志时它直接返回，不读文件也不写库。
type AccessLogCollectJob struct {
	accessLogService service.AccessLogService
}

func NewAccessLogCollectJob() *AccessLogCollectJob {
	return new(AccessLogCollectJob)
}

func (j *AccessLogCollectJob) Run() {
	// cron 自身不做 panic 恢复，这里的 panic 会杀掉整个面板进程。
	defer common.Recover("访问日志采集任务")

	if err := j.accessLogService.Collect(); err != nil {
		logger.Warning("采集访问日志失败:", err)
	}
}

// AccessLogCleanupJob 按保留期清理访问日志，并顺带清掉已删除入站遗留的孤儿记录。
type AccessLogCleanupJob struct {
	accessLogService service.AccessLogService
	settingService   service.SettingService
	banService       service.IPBanService
}

func NewAccessLogCleanupJob() *AccessLogCleanupJob {
	return new(AccessLogCleanupJob)
}

func (j *AccessLogCleanupJob) Run() {
	defer common.Recover("访问日志清理任务")

	days, err := j.settingService.GetAccessLogRetentionDays()
	if err != nil {
		logger.Warning("读取访问日志保留天数失败:", err)
		return
	}
	deleted, err := j.accessLogService.Cleanup(days, time.Now())
	if err != nil {
		logger.Warning("清理过期访问日志失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期访问日志", deleted)
	}

	pruned, err := j.accessLogService.PruneOrphans()
	if err != nil {
		logger.Warning("清理孤儿访问日志失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的访问日志", pruned)
	}

	// 过期封禁只是不再生效，行还留在库里。判定路径每秒都会走到，不适合顺手
	// 做 DELETE 去抢主库写锁，因此挪到这个每小时一次的任务里清。
	expired, err := j.banService.PruneExpired(time.Now())
	if err != nil {
		logger.Warning("清理过期封禁失败:", err)
	} else if expired > 0 {
		logger.Debugf("清理了 %v 条过期封禁", expired)
	}
}
