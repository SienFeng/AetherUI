package job

import (
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// DomainStatJob 把访问日志增量聚合成域名分时桶。
//
// 10 分钟一轮而不是跟着 AccessLogCollectJob（5 秒）跑：榜单的最小档位是
// 1 小时，没有必要更频繁；而每一轮都要开事务写两级桶，跟着 5 秒跑等于
// 把写入放大几十倍去换一个没人看得出来的新鲜度。
type DomainStatJob struct {
	domainStatService service.DomainStatService
}

func NewDomainStatJob() *DomainStatJob {
	return new(DomainStatJob)
}

func (j *DomainStatJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层——日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("域名统计聚合任务")

	n, err := j.domainStatService.Aggregate()
	if err != nil {
		logger.Warning("聚合域名统计失败:", err)
		return
	}
	if n > 0 {
		logger.Debugf("聚合了 %v 条访问日志到域名统计", n)
	}
}
