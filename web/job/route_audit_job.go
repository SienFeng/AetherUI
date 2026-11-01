package job

import (
	"errors"
	"sync"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// routeAuditInterval 是同一应用代次下重新审计的最长间隔。
const routeAuditInterval = 10 * time.Minute

// routeAuditMaxTruncated 是连续被预算截断的轮数上限：截断的一轮不推进状态、下一次唤醒
// 就重试（VerifyAll 让上一轮没轮到的规则排前面），但规则多到每轮都跑不完、或 RPC 一直超时
// 时，无限重试就是在 1 核机器上一轮接一轮地满负荷跑。到上限后按成功推进，回到
// 「代次变化或满 10 分钟」的节奏；没拿到新结果的规则在摘要里仍是 stale/未验证，不会给绿。
const routeAuditMaxTruncated = 3

// shouldAudit：应用代次变了（核心换过配置），或距上次成功审计已满 10 分钟。
func shouldAudit(curGen, lastGen uint64, lastAt, now time.Time) bool {
	return curGen != lastGen || now.Sub(lastAt) >= routeAuditInterval
}

// RouteAuditJob 在后台对全部启用的路由规则跑一轮核心验证并写缓存。
// 每 30 秒唤醒一次，只有代次变化或满 10 分钟才真正审计；单核 1G 机器上不做更勤的全量验证。
type RouteAuditJob struct {
	running sync.Mutex // cron 不会跳过仍在运行的上一轮，用它防止重入
	lastGen uint64
	lastAt  time.Time
	// truncated 是连续被预算截断的轮数，成功或推进后清零。
	truncated int

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

	// 上一轮（如耗时超过调度间隔的全量验证）未结束时直接跳过，避免并发读写下面的状态字段。
	if !j.running.TryLock() {
		return
	}
	defer j.running.Unlock()

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
		// 预算用完（ErrVerifyTruncated）：已跑完的结果已写入，但这一轮不完整，同样不推进，
		// 下一轮把没轮到的规则补上（VerifyAll 让它们排在前面）。
		switch {
		case errors.Is(err, service.ErrVerifyBusy) || errors.Is(err, service.ErrCoreNotReady):
			logger.Debug("路由审计跳过：", err)
		case errors.Is(err, service.ErrVerifyTruncated):
			j.truncated++
			if j.truncated >= routeAuditMaxTruncated {
				logger.Warning("路由审计连续", j.truncated, "轮被预算截断，部分规则本周期没有新结果，按完成推进：", err)
				j.truncated = 0
				j.lastGen, j.lastAt = usedGen, now
				return
			}
			logger.Warning("路由审计本轮未完成，下一轮继续：", err)
		default:
			logger.Warning("路由审计失败：", err)
		}
		return
	}
	j.truncated = 0
	j.lastGen, j.lastAt = usedGen, now
}
