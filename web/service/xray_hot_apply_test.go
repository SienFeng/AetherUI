package service

import (
	"testing"

	"a-ui/util/json_util"
	"a-ui/xray"
)

func rawf(s string) json_util.RawMessage { return json_util.RawMessage(s) }

func hotApplyBaseConfig() *xray.Config {
	return &xray.Config{
		LogConfig: rawf(`{"loglevel":"warning"}`),
		API:       rawf(`{"services":["HandlerService","RoutingService"],"tag":"api"}`),
		Stats:     rawf(`{}`),
		InboundConfigs: []xray.InboundConfig{
			{Port: 62789, Protocol: "dokodemo-door", Tag: "api", Settings: rawf(`{"address":"127.0.0.1"}`)},
		},
		OutboundConfigs: rawf(`[{"protocol":"freedom","tag":"direct"}]`),
		RouterConfig:    rawf(`{"domainStrategy":"AsIs","rules":[]}`),
	}
}

// 拿不到 api 端口时必须判定为「不能热应用」，让调用方走重启，
// 而不是拿 0 端口去连然后卡在超时上。
func TestTryHotApplyWithoutAPIPort(t *testing.T) {
	s := &XrayService{}
	oldCfg := hotApplyBaseConfig()
	newCfg := hotApplyBaseConfig()
	newCfg.RouterConfig = rawf(`{"domainStrategy":"AsIs","rules":[{"type":"field","domain":["geosite:openai"],"outboundTag":"direct"}]}`)

	process := xray.NewProcess(oldCfg)
	// 进程没启动，apiPort 为 0。
	if s.tryHotApply(process, newCfg) {
		t.Fatal("拿不到 api 端口时不该判定热应用成功")
	}
}

// 差分判定为「必须重启」时，绝不能去连核心。
func TestTryHotApplyRefusesRestartOnlyChange(t *testing.T) {
	s := &XrayService{}
	oldCfg := hotApplyBaseConfig()
	newCfg := hotApplyBaseConfig()
	// policy 没有重载接口。
	newCfg.Policy = rawf(`{"levels":{"0":{"handshake":10}}}`)

	process := xray.NewProcess(oldCfg)
	if s.tryHotApply(process, newCfg) {
		t.Fatal("policy 变化必须走重启")
	}
}
