package controller

import (
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/service"
)

// 设置页关掉「流量暴涨自动限速」：生效中的限速单在保存时当场撤掉。旧版要等暴涨检测下一轮（最多 10 分钟），
// 这期间 tc 照样限着、列表上还挂着「限速中」。
func TestUpdateSettingLiftsSpikeThrottlesWhenSwitchedOff(t *testing.T) {
	r := newSettingRouter(t)
	if err := database.GetDB().Create(&model.Setting{Key: "spikeThrottleEnable", Value: "1"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.InboundThrottle{InboundId: 1, Mbps: 5,
		Until: time.Now().Add(time.Hour).UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	xs := service.XrayService{}
	t.Cleanup(func() { xs.IsNeedRestartAndSetFalse() })

	form := validSettingForm() // SpikeThrottleEnable 为 0
	if msg := postSettingJSON(t, r, "/aui/setting/update", form); !msg.Success {
		t.Fatalf("保存设置失败: %s", msg.Msg)
	}
	var left int64
	database.GetDB().Model(&model.InboundThrottle{}).Count(&left)
	if left != 0 {
		t.Error("关掉开关保存之后，生效中的限速单还在")
	}
}
