package link

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// 面板前端生成的分享链接，必须能被面板自己的解析器读回来。
// 生成端在 web/assets/js/model/xray.js 的 genVLESSLink，解析端在
// outbound.go——两边的参数名靠这个测试对齐，改错一个名字就会红。
func TestGeneratedRealityLinkParsesBack(t *testing.T) {
	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@1.2.3.4:443" +
		"?type=tcp&security=reality&sni=www.example.com&pbk=THEPBK&sid=0123456789abcdef" +
		"&fp=chrome&spx=%2F&flow=xtls-rprx-vision#remark"

	res, err := ParseLink(raw)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	re := streamSub(t, res, "realitySettings")
	for k, want := range map[string]string{
		"publicKey": "THEPBK", "shortId": "0123456789abcdef",
		"serverName": "www.example.com", "fingerprint": "chrome", "spiderX": "/",
	} {
		if got := re[k]; got != want {
			t.Errorf("realitySettings[%q] = %v, want %q", k, got, want)
		}
	}
}

// 下面几条的链接原样取自 xray.js 的 genVLESSLink / genTrojanLink / genSSLink 的实际输出
// （各用例的入站配置写在注释里）。生成端改了参数名或格式，这里的样本要跟着更新，
// 并且必须仍能被解析成同样的出站。

// VLESS + tcp + http 伪装。type 已被传输方式占用，解析端只认 headerType=http；
// 改动前生成端不带它，客户端会按 header none 连接，与服务端的 http 伪装对不上。
func TestGeneratedVlessTcpHTTPLinkParsesBack(t *testing.T) {
	// tcpSettings.header: http，path ["/a", "/b"]，Host ["cdn.example.com", "www.example.com"]
	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@192.0.2.10:443" +
		"?type=tcp&security=none&headerType=http&path=%2Fa%2C%2Fb&host=cdn.example.com%2Cwww.example.com#case"
	res, err := ParseLink(raw)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	header, _ := streamSub(t, res, "tcpSettings")["header"].(map[string]any)
	if header["type"] != "http" {
		t.Fatalf("tcp header type = %v, want http", header["type"])
	}
	request, _ := header["request"].(map[string]any)
	if got := fmt.Sprint(request["path"]); got != "[/a /b]" {
		t.Errorf("request.path = %s, want [/a /b]", got)
	}
	headers, _ := request["headers"].(map[string]any)
	if got := fmt.Sprint(headers["Host"]); got != "[cdn.example.com www.example.com]" {
		t.Errorf("request.headers.Host = %s, want [cdn.example.com www.example.com]", got)
	}
}

// Trojan 链接与 VLESS 同一套参数。改动前生成端只拼 password@address:port，而解析端对
// 缺省的 security 一律按 tls 处理：REALITY 链接丢了 pbk/sid/sni，tls 链接丢了证书域名，
// ws 丢了 path/host，security=none 的入站被当成 tls。
func TestGeneratedTrojanLinksParseBack(t *testing.T) {
	cases := []struct {
		name, link, address, password, network, security string
		sub                                              string            // streamSettings 下要核对的子对象
		want                                             map[string]string // 子对象里的字段
	}{
		{
			// security tls，tlsSettings.serverName example.com（面板按 IP 打开）
			name:    "tls 用证书域名作地址",
			link:    "trojan://pw123@example.com:443?type=tcp&security=tls&sni=example.com&alpn=h2%2Chttp%2F1.1&fp=chrome#case",
			address: "example.com", password: "pw123", network: "tcp", security: "tls",
			sub: "tlsSettings", want: map[string]string{"serverName": "example.com", "fingerprint": "chrome"},
		},
		{
			name:    "reality",
			link:    "trojan://pw123@192.0.2.10:443?type=tcp&security=reality&sni=www.example.com&pbk=PUBKEY&sid=0123abcd&fp=chrome&spx=%2F#case",
			address: "192.0.2.10", password: "pw123", network: "tcp", security: "reality",
			sub: "realitySettings", want: map[string]string{"publicKey": "PUBKEY", "shortId": "0123abcd",
				"serverName": "www.example.com", "fingerprint": "chrome", "spiderX": "/"},
		},
		{
			// wsSettings path /ws、Host cdn.example.com，tls 域名 cdn.example.com
			name:    "ws + tls",
			link:    "trojan://pw123@cdn.example.com:443?type=ws&security=tls&path=%2Fws&host=cdn.example.com&sni=cdn.example.com&alpn=h2%2Chttp%2F1.1&fp=chrome#case",
			address: "cdn.example.com", password: "pw123", network: "ws", security: "tls",
			sub: "wsSettings", want: map[string]string{"path": "/ws", "host": "cdn.example.com"},
		},
		{
			name:    "security none 必须显式写出",
			link:    "trojan://pw123@192.0.2.10:443?type=tcp&security=none#case",
			address: "192.0.2.10", password: "pw123", network: "tcp", security: "none",
		},
		{
			// 密码 p@ss:w/rd#1?% —— 不转义的话链接会在 @ 与 # 处被截断
			name:    "密码里的保留字符",
			link:    "trojan://p%40ss%3Aw%2Frd%231%3F%25@192.0.2.10:443?type=tcp&security=tls&alpn=h2%2Chttp%2F1.1&fp=chrome#case",
			address: "192.0.2.10", password: "p@ss:w/rd#1?%", network: "tcp", security: "tls",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseLink(tc.link)
			if err != nil {
				t.Fatalf("ParseLink: %v", err)
			}
			srv := firstServer(t, res.Outbound)
			if srv["address"] != tc.address || srv["port"] != 443 || srv["password"] != tc.password {
				t.Errorf("server = %#v, want address %q port 443 password %q", srv, tc.address, tc.password)
			}
			ss, _ := res.Outbound["streamSettings"].(map[string]any)
			if ss["network"] != tc.network || ss["security"] != tc.security {
				t.Errorf("network/security = %v/%v, want %s/%s", ss["network"], ss["security"], tc.network, tc.security)
			}
			if tc.sub == "" {
				return
			}
			sub := streamSub(t, res, tc.sub)
			for k, want := range tc.want {
				if got := sub[k]; got != want {
					t.Errorf("%s[%q] = %v, want %q", tc.sub, k, got, want)
				}
			}
		})
	}
}

// 监听地址是 IPv6 时，生成端按 RFC 3986 给主机加方括号（改动前 VLESS 直接抛
// Invalid URL，Trojan / SS 拼出主机与端口分不开的串）。解析回来的地址必须是裸地址。
func TestGeneratedIPv6LinksParseBack(t *testing.T) {
	ssBody := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:sspw@[2001:db8::1]:443"))
	cases := map[string]string{
		"vless":  "vless://b831381d-6324-4d53-ad4f-8cda48b30811@[2001:db8::1]:443?type=tcp&security=none#case",
		"trojan": "trojan://pw123@[2001:db8::1]:443?type=tcp&security=reality&sni=www.example.com&pbk=PUBKEY&sid=0123abcd&fp=chrome&spx=%2F#case",
		// genSSLink 的整段 base64 写法：method:password@[host]:port
		"ss legacy": "ss://" + ssBody + "#case",
		// SIP002 写法，别的客户端导出的 IPv6 节点就是这样
		"ss sip002": "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:sspw")) + "@[2001:db8::1]:443#case",
	}
	for name, link := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := ParseLink(link)
			if err != nil {
				t.Fatalf("ParseLink: %v", err)
			}
			var address, port any
			if settings := res.Outbound["settings"].(map[string]any); settings["servers"] != nil {
				srv := firstServer(t, res.Outbound)
				address, port = srv["address"], srv["port"]
			} else {
				address, port = settings["address"], settings["port"]
			}
			if address != "2001:db8::1" || port != 443 {
				t.Errorf("address/port = %v/%v, want 2001:db8::1/443", address, port)
			}
		})
	}
}
