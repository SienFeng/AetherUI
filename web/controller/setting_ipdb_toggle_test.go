package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/entity"
	"a-ui/web/service"
)

// validSettingForm 是一份能通过 CheckValid 的完整设置（与 service 包的 validBaseSetting 同值）。
// dnsServers / ipRuleResolveDomain 与库里的默认值相同，保存时不会去跑真实 xray。
func validSettingForm() *entity.AllSetting {
	return &entity.AllSetting{
		WebPort:                     54321,
		WebBasePath:                 "/",
		TimeLocation:                "Asia/Shanghai",
		XrayTemplateConfig:          "{}",
		IPDBSourceUrl:               "https://example.com/ipv4_source.txt",
		QQWrySourceUrl:              "https://example.com/qqwry.dat",
		AccessLogRetentionDays:      7,
		TrafficHourRetentionDays:    30,
		TrafficDayRetentionDays:     365,
		AutoTravelHomeSilenceHours:  2,
		AutoTravelStableDays:        3,
		AutoTravelPassHours:         72,
		AutoTravelMonthlyDays:       10,
		AutoTravelWeeklyGrants:      3,
		AutoTravelCooldownDays:      7,
		AutoRegionObserveDays:       1,
		AutoRegionMaxBlockedPercent: 5,
		SpikeThrottleMultiplier:     10,
		SpikeThrottleFloorGB:        20,
		SpikeThrottleMbps:           5,
	}
}

func postSettingJSON(t *testing.T, r *gin.Engine, path string, body any) entity.Msg {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var msg entity.Msg
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return msg
}

// 设置页把某个 IP 库源的地址清空（界面随即显示「未启用」）：这个源要立刻停止参与判定，
// 而地区限制的允许集是按已加载的库现算的，所以还要求重新下发核心配置——置重启标志只在
// controller 里做，与「立即更新 IP 库」同一个出口。
func TestUpdateSettingRegeneratesCoreWhenAnIPDBSourceIsCleared(t *testing.T) {
	r := newSettingRouter(t)
	// 存量部署里 IP2Location 是启用着的。
	if err := database.GetDB().Create(&model.Setting{Key: "ip2locationSourceUrl", Value: "https://example.com/ip2location.zip"}).Error; err != nil {
		t.Fatal(err)
	}
	xs := service.XrayService{}
	xs.IsNeedRestartAndSetFalse()
	t.Cleanup(func() { xs.IsNeedRestartAndSetFalse() })

	form := validSettingForm() // IP2LocationSourceUrl 留空
	if msg := postSettingJSON(t, r, "/aui/setting/update", form); !msg.Success {
		t.Fatalf("保存设置失败: %s", msg.Msg)
	}
	if !xs.IsNeedRestartAndSetFalse() {
		t.Error("清空了一个 IP 库源的地址，没有要求重新下发核心配置")
	}

	// 原样再存一次：没有源的启用状态变化，不该再要求下发。
	if msg := postSettingJSON(t, r, "/aui/setting/update", form); !msg.Success {
		t.Fatalf("保存设置失败: %s", msg.Msg)
	}
	if xs.IsNeedRestartAndSetFalse() {
		t.Error("启用状态没变，却要求重新下发核心配置")
	}
}

// 设置页里会进核心配置的项（拦 25 端口、补全流量识别、拦截机房来源、访问日志、DNS……）保存后要求
// 重新下发：改动前只有装卸了 IP 库源才置重启标志，管理员关掉「拦截 25 端口」后页面提示保存成功，
// 核心却一直按旧规则拦，要等某个无关的改动或手动重启面板才生效。
func TestUpdateSettingRegeneratesCoreWhenASettingChanges(t *testing.T) {
	r := newSettingRouter(t)
	xs := service.XrayService{}
	form := validSettingForm()
	if msg := postSettingJSON(t, r, "/aui/setting/update", form); !msg.Success {
		t.Fatalf("保存设置失败: %s", msg.Msg)
	}
	xs.IsNeedRestartAndSetFalse()
	t.Cleanup(func() { xs.IsNeedRestartAndSetFalse() })

	form.BlockSmtp = 1 - form.BlockSmtp
	if msg := postSettingJSON(t, r, "/aui/setting/update", form); !msg.Success {
		t.Fatalf("保存设置失败: %s", msg.Msg)
	}
	if !xs.IsNeedRestartAndSetFalse() {
		t.Error("改了「拦截 25 端口」开关，没有要求重新下发核心配置")
	}
}
