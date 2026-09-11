package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// SharingRiskJob 定期重算各入站的共享风险快照。
//
// 为什么要预计算而不是每次请求现场算：第一期的并存统计现场算尚可（毫秒级），
// 加上画像 + 两两错峰之后不行了——入站数 × 7 天 × 每小时数十行，而入站列表
// 每次加载都要这个数。风险是长期行为分析，不是实时防火墙，10 分钟绰绰有余。
type SharingRiskJob struct {
	riskService service.SharingRiskService
}

func NewSharingRiskJob() *SharingRiskJob {
	return new(SharingRiskJob)
}

func (j *SharingRiskJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层：日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("共享风险评分任务")

	if err := j.riskService.Evaluate(time.Now()); err != nil {
		// 评估失败只告警，绝不阻断任何既有流程：这是辅助手段。
		logger.Warning("共享风险评分失败:", err)
	}
}

// RunInitial 是面板启动后的首轮评估，由 Server.startTask 起的 goroutine 调用。
//
// cron.AddJob 的首次执行在一个完整周期之后，不做首轮触发的话面板刚启动的
// 10 分钟内 Snapshot 表是空的，而**空快照绝不能显示成「低风险」**。
//
// 这个方法由 goroutine 直接调用、**不经过 cron**，所以 cron 那层 Recover
// 覆盖不到它——Run 里那个 defer 不是「更早的一层」而是唯一的一层，去掉就是
// 一个 panic 杀掉整个面板进程。不要照抄 startTask 里 PanelVersionJob 那条
// 没有 Recover 的写法。
func (j *SharingRiskJob) RunInitial() {
	defer common.Recover("共享风险评分首轮")

	if err := j.riskService.Evaluate(time.Now()); err != nil {
		logger.Warning("共享风险首轮评分失败:", err)
	}
}
