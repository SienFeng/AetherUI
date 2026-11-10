package job

import (
	"errors"
	"sync/atomic"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// AutoRegionJob 每 10 分钟给没有地区限制的入站判定一次能否自动设置。用量数据是
// 小时粒度的，跑得更勤没有意义。写入了地区就置重启标志，交给 10 秒的消费任务下发。
type AutoRegionJob struct {
	autoRegionService service.AutoRegionService
	xrayService       service.XrayService
	// noIPDBLogged 记着「IP 库未加载」是否已经记过日志：这个原因每轮都会碰到，只在开始时
	// 记一次，恢复后（某一轮不再是这个原因）重置。
	noIPDBLogged atomic.Bool
}

func NewAutoRegionJob() *AutoRegionJob {
	return new(AutoRegionJob)
}

func (j *AutoRegionJob) Run() {
	defer common.Recover("自动设置地区限制任务")
	changed, err := j.autoRegionService.Run(time.Now())
	switch {
	case j.shouldLogNoIPDB(err):
		logger.Warning("自动设置地区限制:", err)
	case err != nil && !errors.Is(err, service.ErrAutoRegionNoIPDB):
		logger.Warning("自动设置地区限制失败:", err)
	}
	if changed {
		j.xrayService.SetToNeedRestart()
	}
}

// shouldLogNoIPDB 判断这一轮要不要为「IP 库未加载」记日志：一段持续的未加载只记一次。
func (j *AutoRegionJob) shouldLogNoIPDB(err error) bool {
	if !errors.Is(err, service.ErrAutoRegionNoIPDB) {
		j.noIPDBLogged.Store(false)
		return false
	}
	return j.noIPDBLogged.CompareAndSwap(false, true)
}
