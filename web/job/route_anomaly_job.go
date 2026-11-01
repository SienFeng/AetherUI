package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// routeLargeScanInterval 是大规则扫描的常规间隔。
const routeLargeScanInterval = 10 * time.Minute

// shouldScanLarge：首次（零值）或距上次大规则扫描已满 10 分钟。
func shouldScanLarge(lastLarge, now time.Time) bool {
	return now.Sub(lastLarge) >= routeLargeScanInterval
}

// RouteAnomalyJob 每分钟扫一次访问日志里的路由异常：小规则每轮扫，大规则默认 10 分钟一次。
// 上一次大规则扫描读满整批（可能落后于日志写入）时，下一轮不等间隔直接再扫；
// 但每次运行最多扫一次大规则，不会进入紧循环。
type RouteAnomalyJob struct {
	lastLarge time.Time

	// 以下三个字段供测试注入；nil 时用真实实现。
	now        func() time.Time
	scan       func(now time.Time, large bool) error
	backlogged func() bool

	auditService service.RoutingAuditService
}

func NewRouteAnomalyJob() *RouteAnomalyJob {
	return new(RouteAnomalyJob)
}

func (j *RouteAnomalyJob) Run() {
	defer common.Recover("路由异常扫描任务")

	now := time.Now()
	if j.now != nil {
		now = j.now()
	}
	scan := j.auditService.ScanAccessLog
	if j.scan != nil {
		scan = j.scan
	}
	backlogged := j.auditService.LargeScanBacklogged
	if j.backlogged != nil {
		backlogged = j.backlogged
	}

	if err := scan(now, false); err != nil {
		logger.Warning("路由异常扫描（小规则）失败：", err)
	}
	if shouldScanLarge(j.lastLarge, now) || backlogged() {
		if err := scan(now, true); err != nil {
			logger.Warning("路由异常扫描（大规则）失败：", err)
		}
		j.lastLarge = now
	}
}
