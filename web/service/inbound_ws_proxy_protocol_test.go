package service

import (
	"testing"

	"a-ui/database/model"
)

// 带 acceptProxyProtocol 的 ws 入站必须能通过面板自己那道真实 xray 校验
// （ValidateInboundReplacing）。校验是 fail open 的，只有核心明确判定非法
// 才会拒绝——所以这条测试真正排除的是「核心根本不认这个字段、AddInbound
// 直接把用户挡在门外」。
//
// 与 xray.TestWsAcceptProxyProtocolIsReadByCore 分工不同：那条钉「键名和层级
// 对不对」（infra/conf 解析层），这条钉「整份配置送进真实二进制不会被拒」。
func TestAddInboundAcceptsWsAcceptProxyProtocol(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	in := &model.Inbound{
		UserId: 1, Port: 10011, Protocol: model.VLESS, Tag: "inbound-10011",
		Remark: "藏在 fallback 后面", Enable: true,
		Settings:       `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811"}],"decryption":"none"}`,
		StreamSettings: `{"network":"ws","security":"none","wsSettings":{"acceptProxyProtocol":true,"path":"/p","headers":{}}}`,
		Sniffing:       `{"enabled":true,"destOverride":["http","tls"]}`,
	}
	if err := (&InboundService{}).AddInbound(in); err != nil {
		t.Fatalf("AddInbound 拒绝了带 acceptProxyProtocol 的 ws 入站: %v", err)
	}
}
