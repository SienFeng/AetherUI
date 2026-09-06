package job

import (
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// TrafficResetJob 把到了周期的入站流量清零，并把「因超流量被自动停用」的那
// 一批拉回来。
//
// 每小时跑一次而不是每天一次：判定用的是「上次重置时刻 vs 本周期重置时刻」，
// 一小时的粒度足以让重置在当天 01:00 之前发生，也让面板停机后的补跑最多迟
// 一小时。跑得再密只是徒增查库次数——常态下这个任务只做一次带 where 的查询
// 就返回。
type TrafficResetJob struct {
	inboundService service.InboundService
	settingService service.SettingService
	xrayService    service.XrayService
}

func NewTrafficResetJob() *TrafficResetJob {
	return new(TrafficResetJob)
}

func (j *TrafficResetJob) Run() {
	// cron 已配了 Recover，这里仍照现有 job 的惯例再挡一层：日志里能带上
	// 具体任务名，而不是只知道「某个 job 挂了」。
	defer common.Recover("流量重置任务")

	loc, err := j.settingService.GetTimeLocation()
	if err != nil {
		logger.Warning("读取面板时区失败，跳过本轮流量重置:", err)
		return
	}

	reset, reEnabled, err := j.inboundService.ResetDueTraffic(time.Now(), loc)
	if err != nil {
		logger.Warning("流量重置失败:", err)
		// 不 return：ResetDueTraffic 是逐条独立提交的，出错之前已经改掉的
		// 那些照样要让 xray 跟上。
	}
	if reset > 0 {
		logger.Debugf("重置了 %v 个入站的流量，其中 %v 个被重新启用", reset, reEnabled)
	}
	// 只有重新启用才改变 xray 配置——只有 enable=true 的入站才会被合成进去。
	// 单纯清零 up/down 不进配置，白置标志只会让那个 10 秒的消费任务空跑一次。
	if reEnabled > 0 {
		j.xrayService.SetToNeedRestart()
	}
}
