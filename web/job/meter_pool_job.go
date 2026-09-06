package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// MeterPoolJob 每小时重算一次计量池。
//
// 一小时而不是更频繁：榜单的最小档位就是 1 小时，而每换一次池都是一批出站
// 与规则的热增删，且每个退池的 tag 会在核心里留下一对永不回收的计数器
//（见 MeterPoolService 里 meterStaleCounterLimit 的注释）。稳一点比新一点重要。
//
// cron.AddJob 的首次执行在一个完整周期之后，这里刻意不做延迟触发：池已经在
// 库里，面板重启后生成期照常读得到，第一个小时不重算不会丢任何东西；而全新
// 安装的机器第一个小时本来也没有数据可排。
type MeterPoolJob struct {
	meterPoolService service.MeterPoolService
	xrayService      service.XrayService
}

func NewMeterPoolJob() *MeterPoolJob {
	return new(MeterPoolJob)
}

func (j *MeterPoolJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层——日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("计量池重算任务")

	// changed 与 err 必须分开判：Recompute 逐入站累加 changed，任一入站出错就
	// 带着已经发生的变动 return。此时库里的池已经变了，标志却没置的话，下发的
	// 配置会一直停在旧池上，直到下一次无关改动或下一个整点——中间那段时间，
	// 已退池域名的计量出站还在，新进池的域名一个字节都收不到。所以先按 changed
	// 置标志，再报错。
	changed, err := j.meterPoolService.Recompute(time.Now())
	if changed > 0 {
		logger.Debugf("计量池变动 %v 项", changed)
		// 置了标志之后由 InboundController 那个 10 秒的消费任务调
		// RestartXray(false)，走 tryHotApply：出站增删与整段路由替换都有控制面
		// 接口，不会重启进程。池没变就没有配置改动，白置标志只会让消费任务空跑。
		j.xrayService.SetToNeedRestart()
	}
	if err != nil {
		logger.Warning("重算计量池失败:", err)
	}
}
