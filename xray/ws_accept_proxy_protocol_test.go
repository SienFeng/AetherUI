package xray

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/infra/conf"
)

// acceptProxyProtocol 必须写在 wsSettings **里面**，键名一个字母都不能差。
//
// 这条测试守的是本项目最典型的一类静默失效：infra/conf 从不调用
// DisallowUnknownFields，键名写错、或层级放错（比如放到 streamSettings 上、
// 或放进 sockopt 之外的任何地方）都不会有任何报错——`run -test` 照样
// Configuration OK，面板照样 running，只是这个开关变成纯装饰，而它恰恰是
// 「入站藏在 443 后面还能拿到真实客户端 IP」的唯一开关。
//
// 生成端在 web/assets/js/model/xray.js 的 WsStreamSettings，
// 由 web.TestWsStreamSettingsEmitsAcceptProxyProtocol 钉住它确实发出这个键；
// 这一条钉的是「核心确实读得到」。核心升级换掉 infra/conf 时，两条一起红。
func TestWsAcceptProxyProtocolIsReadByCore(t *testing.T) {
	// 与 WsStreamSettings.toJson() 产出的形状一致。
	raw := `{"network":"ws","security":"none",` +
		`"wsSettings":{"acceptProxyProtocol":true,"path":"/p","headers":{}}}`

	var sc conf.StreamConfig
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		t.Fatalf("解析 streamSettings: %v", err)
	}
	if sc.WSSettings == nil {
		t.Fatal("wsSettings 没有被解析出来")
	}
	if !sc.WSSettings.AcceptProxyProtocol {
		t.Error("acceptProxyProtocol 没有被核心读到——键名或层级不对，" +
			"而这种错误不会有任何一层报错")
	}
}
