package job

import (
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/service"
)

func setupJobDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	// 重启标志是包级变量，会跨用例串味。每个用例先消费掉上一轮的残留。
	(&service.XrayService{}).IsNeedRestartAndSetFalse()
}

func makeJobInbound(t *testing.T, mutate func(*model.Inbound)) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: 10011, Protocol: model.VLESS, Tag: "inbound-10011",
		Remark:         "测试",
		Settings:       `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811"}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp"}`,
		Sniffing:       `{"enabled":true}`,
	}
	mutate(in)
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	return in
}

// 重新启用一个入站会改变 xray 的配置（只有 enable=true 的入站才进配置）。
// 不置重启标志的话，用户在库里已经是「启用」状态、面板也显示启用，核心里
// 却没有这个入站——他连不上，而面板不会有任何提示。
func TestTrafficResetJobFlagsRestartWhenInboundsAreReEnabled(t *testing.T) {
	setupJobDB(t)
	makeJobInbound(t, func(in *model.Inbound) {
		in.TrafficResetMode, in.LastResetAt = model.TrafficResetMonthly, 0
		in.Enable, in.DisabledByTraffic = false, true
		in.ExpiryTime = time.Now().Add(30 * 24 * time.Hour).UnixMilli()
		in.Total, in.Up, in.Down = 1000, 600, 400
	})

	NewTrafficResetJob().Run()

	if !(&service.XrayService{}).IsNeedRestartAndSetFalse() {
		t.Error("重新启用了入站却没有置重启标志")
	}
}

// 只清零、没有任何入站被重新启用时，配置一个字节都没变。白置标志会让
// 那个 10 秒的消费任务空跑一次 RestartXray，多一次无谓的配置比对。
func TestTrafficResetJobDoesNotFlagRestartWhenOnlyZeroing(t *testing.T) {
	setupJobDB(t)
	makeJobInbound(t, func(in *model.Inbound) {
		in.TrafficResetMode, in.LastResetAt = model.TrafficResetMonthly, 0
		in.Enable, in.DisabledByTraffic = true, false
		in.Up, in.Down = 111, 222
	})

	NewTrafficResetJob().Run()

	if (&service.XrayService{}).IsNeedRestartAndSetFalse() {
		t.Error("只清零也置了重启标志")
	}
}
