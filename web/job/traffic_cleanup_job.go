package job

import (
	"time"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// TrafficCleanupJob 按各自的保留期清理两级用量数据，并顺带清掉已删除入站
// 遗留的孤儿桶。
//
// 不并进 AccessLogCleanupJob：那会让它名不副实，也会让两种数据的清理失败
// 互相牵连——访问日志库出问题不该连带停掉用量数据的清理。
type TrafficCleanupJob struct {
	trafficService    service.TrafficHistoryService
	settingService    service.SettingService
	sharingService    service.SharingService
	domainStatService service.DomainStatService
}

func NewTrafficCleanupJob() *TrafficCleanupJob {
	return new(TrafficCleanupJob)
}

func (j *TrafficCleanupJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层。
	defer common.Recover("用量历史清理任务")

	now := time.Now()

	if days, err := j.settingService.GetTrafficHourRetentionDays(); err != nil {
		logger.Warning("读取用量小时数据保留天数失败:", err)
	} else if deleted, err := j.trafficService.Cleanup(model.GranularityHour, days, now); err != nil {
		logger.Warning("清理过期用量小时数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期用量小时数据", deleted)
	}

	if days, err := j.settingService.GetTrafficDayRetentionDays(); err != nil {
		logger.Warning("读取用量每日数据保留天数失败:", err)
	} else if deleted, err := j.trafficService.Cleanup(model.GranularityDay, days, now); err != nil {
		logger.Warning("清理过期用量每日数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期用量每日数据", deleted)
	}

	// 共享检测的行与用量桶同库，清理挂在同一个任务里：再开一个每小时任务
	// 只是多一份注册与一份 panic 面。保留期是常量，不读设置。
	if deleted, err := j.sharingService.Cleanup(now); err != nil {
		logger.Warning("清理过期共享检测记录失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期共享检测记录", deleted)
	}

	if pruned, err := j.sharingService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿共享检测记录失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的共享检测记录", pruned)
	}

	if pruned, err := j.trafficService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿用量数据失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的用量数据", pruned)
	}

	// 域名统计与用量桶同库、同粒度常量，因此共用同一份保留期设置，
	// 清理也挂在同一个任务里。
	if days, err := j.settingService.GetTrafficHourRetentionDays(); err != nil {
		logger.Warning("读取用量小时数据保留天数失败:", err)
	} else if deleted, err := j.domainStatService.Cleanup(model.GranularityHour, days, now); err != nil {
		logger.Warning("清理过期域名统计小时数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期域名统计小时数据", deleted)
	}

	if days, err := j.settingService.GetTrafficDayRetentionDays(); err != nil {
		logger.Warning("读取用量每日数据保留天数失败:", err)
	} else if deleted, err := j.domainStatService.Cleanup(model.GranularityDay, days, now); err != nil {
		logger.Warning("清理过期域名统计每日数据失败:", err)
	} else if deleted > 0 {
		logger.Debugf("清理了 %v 条过期域名统计每日数据", deleted)
	}

	if pruned, err := j.domainStatService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿域名统计失败:", err)
	} else if pruned > 0 {
		logger.Warningf("清理了 %v 条已删除入站遗留的域名统计", pruned)
	}
}
