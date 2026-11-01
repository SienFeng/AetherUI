package job

import (
	"errors"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// routeAuditInterval 是同一应用代次下重新审计的最长间隔。
const routeAuditInterval = 10 * time.Minute

// shouldAudit：应用代次变了（核心换过配置），或距上次成功审计已满 10 分钟。
func shouldAudit(curGen, lastGen uint64, lastAt, now time.Time) bool {
	return curGen != lastGen || now.Sub(lastAt) >= routeAuditInterval
}

// RouteAuditJob 在后台对全部启用的路由规则跑一轮核心验证并写缓存。
// 每 30 秒唤醒一次，只有代次变化或满 10 分钟才真正审计；单核 1G 机器上不做更勤的全量验证。
type RouteAuditJob struct {
	lastGen uint64
	lastAt  time.Time

	// 以下三个字段供测试注入；nil 时用真实实现。
	now        func() time.Time
	currentGen func() uint64
	audit      func() (uint64, error)

	auditService service.RoutingAuditService
}

func NewRouteAuditJob() *RouteAuditJob {
	return new(RouteAuditJob)
}

func (j *RouteAuditJob) Run() {
	defer common.Recover("路由审计任务")

	now := time.Now()
	if j.now != nil {
		now = j.now()
	}
	gen := service.XrayApplyGeneration()
	if j.currentGen != nil {
		gen = j.currentGen()
	}
	if !shouldAudit(gen, j.lastGen, j.lastAt, now) {
		return
	}
	run := j.auditService.RunAudit
	if j.audit != nil {
		run = j.audit
	}
	usedGen, err := run()
	if err != nil {
		// 核心未就绪或验证正忙属于正常现象：不推进 lastGen/lastAt，下一轮重试。
		if errors.Is(err, service.ErrVerifyBusy) || errors.Is(err, service.ErrCoreNotReady) {
			logger.Debug("路由审计跳过：", err)
		} else {
			logger.Warning("路由审计失败：", err)
		}
		return
	}
	j.lastGen, j.lastAt = usedGen, now
}
