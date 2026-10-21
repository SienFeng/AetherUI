package service

import (
	"fmt"
	"testing"

	"a-ui/database/model"
)

// 入站表单从这一版起给 socks / http 也显示 sniffing 开关（xray.js 的
// canSniffing）。这条测试排除的是「核心不接受代理类入站上的 sniffing、
// AddInbound 用真实 xray 校验时把管理员挡在门外」——开关露出来了却保存不了，
// 比不露更糟。
func TestAddInboundAcceptsSniffingOnProxyInbounds(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	for i, c := range []struct {
		protocol model.Protocol
		settings string
	}{
		{model.Protocol("socks"), `{"auth":"password","accounts":[{"user":"u","pass":"p"}],"udp":true,"ip":"127.0.0.1"}`},
		{model.Http, `{"accounts":[{"user":"u","pass":"p"}]}`},
	} {
		port := 10021 + i
		in := &model.Inbound{
			UserId: 1, Port: port, Protocol: c.protocol, Tag: fmt.Sprintf("inbound-%d", port),
			Remark: string(c.protocol), Enable: true,
			Settings: c.settings,
			Sniffing: `{"enabled":true,"destOverride":["http","tls"]}`,
		}
		if err := (&InboundService{}).AddInbound(in); err != nil {
			t.Fatalf("AddInbound 拒绝了开启 sniffing 的 %s 入站: %v", c.protocol, err)
		}
	}
}
