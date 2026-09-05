package job

import (
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// PanelVersionJob 每 6 小时刷新一次面板版本缓存。
//
// GitHub 未认证 API 限速 60 次/小时/IP，6 小时一次每天只用 4 次，
// 加上管理员手动刷新（controller 侧另有 1 分钟节流）绰绰有余。
//
// 拉取失败只记日志、不打扰：LastError 已经写进缓存，界面上会显示。
type PanelVersionJob struct {
	panelVersionService service.PanelVersionService
}

func NewPanelVersionJob() *PanelVersionJob {
	return new(PanelVersionJob)
}

func (j *PanelVersionJob) Run() {
	defer common.Recover("检查面板版本")
	if err := j.panelVersionService.Refresh(); err != nil {
		logger.Warning("检查面板版本失败:", err)
	}
}
