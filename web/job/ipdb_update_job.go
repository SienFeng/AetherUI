package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
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
// 注意：Run 挂在 cron 上，那个实例配了 cron.Recover，panic 不会杀掉面板进程；
// RunInitial 跑在 startTask 自己起的 goroutine 里，覆盖不到那一层，见下。
type IPDBUpdateJob struct {
	ipdbService service.IPDBService
	xrayService service.XrayService
}

func NewIPDBUpdateJob() *IPDBUpdateJob {
	return new(IPDBUpdateJob)
}

func (j *IPDBUpdateJob) Run() {
	updated, err := j.ipdbService.RunScheduledUpdate(time.Now())
	// 地区限制的允许集按库现算：不置重启标志的话，核心手里一直是旧的允许集，
	// 要等某次无关改动才一次性推下去。
	afterIPDBUpdate(&j.xrayService, "IP 库定时更新", updated, err)
}

// RunInitial 供面板启动后触发一次：把这台机器从未拉取过的库立刻补上。
//
// 与 Run 分成两个方法而不是给 Run 加参数，是因为两者的调用方式不同——它跑在
// startTask 自己起的 goroutine 里，不经过 cron，那层 cron.Recover 覆盖不到，
// 一个 panic 会直接杀掉整个面板进程，所以这里必须自己挡一层。
func (j *IPDBUpdateJob) RunInitial() {
	defer common.Recover("IP 库首次更新任务")

	updated, err := j.ipdbService.RunInitialUpdate()
	afterIPDBUpdate(&j.xrayService, "IP 库首次更新", updated, err) // 理由同 Run
}

// afterIPDBUpdate 记日志，并在有源换了新库时置重启标志——哪怕别的源失败了：前面的源已经换上
// 新库，允许集按库现算，不置标志的话核心一直用旧允许集。配置其实没变时 Config.Equals 会过滤掉。
func afterIPDBUpdate(xs *service.XrayService, what string, updated int, err error) {
	if err != nil {
		// 失败的源旧库仍在生效，功能不受影响，所以只告警。
		logger.Warning(what, "失败:", err)
	}
	if updated > 0 {
		logger.Info(what, "完成，换了新库的数据源个数:", updated)
		xs.SetToNeedRestart()
	}
}
