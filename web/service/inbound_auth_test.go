package service

import (
	"testing"

	"a-ui/database/model"
)

func TestInboundRequiresAuth(t *testing.T) {
	cases := []struct {
		name     string
		protocol model.Protocol
		settings string
		want     bool
	}{
		{"vmess", model.VMess, `{"clients":[{"id":"x"}]}`, true},
		{"vless", model.VLESS, `{"clients":[{"id":"x"}]}`, true},
		{"trojan", model.Trojan, `{"clients":[{"password":"x"}]}`, true},
		{"socks 带账号", model.Protocol("socks"), `{"auth":"password","accounts":[{"user":"a","pass":"b"}]}`, true},
		{"socks noauth", model.Protocol("socks"), `{"auth":"noauth"}`, false},
		{"socks password 却没账号", model.Protocol("socks"), `{"auth":"password","accounts":[]}`, false},
		{"http 带账号", model.Http, `{"accounts":[{"user":"a","pass":"b"}]}`, true},
		{"http 没账号", model.Http, `{"accounts":[]}`, false},
		{"http 没有 accounts 字段", model.Http, `{}`, false},
		{"dokodemo-door", model.Protocol("dokodemo-door"), `{"address":"1.1.1.1","port":53}`, false},
		{"shadowsocks", model.Shadowsocks, `{"method":"aes-128-gcm","password":"x"}`, true},
		{"shadowsocks none", model.Shadowsocks, `{"method":"none","password":""}`, false},
		{"shadowsocks plain", model.Shadowsocks, `{"method":"plain","password":"x"}`, false},
		{"settings 损坏按需要认证处理", model.Protocol("socks"), `{`, true},
	}
	for _, c := range cases {
		in := &model.Inbound{Protocol: c.protocol, Settings: c.settings}
		if got := inboundRequiresAuth(in); got != c.want {
			t.Errorf("%s: inboundRequiresAuth = %v, want %v", c.name, got, c.want)
		}
	}
}
