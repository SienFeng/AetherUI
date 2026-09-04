package service

import (
	"strings"
	"testing"
)

// 这四个选项今天仍然摆在面板的下拉里（web/html/xui/form/stream/stream_settings.html:9-10
// 与 web/assets/js/model/xray.js:43-46），但当前核心在配置构建阶段就会拒绝。
// xray 加载配置是全有或全无：任何一个入站用了它们，整机所有用户一起断网。
//
// 这份表驱动测试是「必须把它们从界面上删掉」的可执行证据。Task 4 与 Task 8
// 删掉它们之后，这个测试仍然应当通过——它锁的是核心的行为，不是面板的行为。
func TestRemovedTransportsAreRejectedByCore(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	cases := []struct {
		name   string
		stream map[string]any
		hint   string
	}{
		{
			name:   "network=http",
			hint:   "HTTP transport",
			stream: map[string]any{"network": "http", "security": "none", "httpSettings": map[string]any{"path": "/"}},
		},
		{
			name:   "network=quic",
			hint:   "QUIC transport",
			stream: map[string]any{"network": "quic", "security": "none", "quicSettings": map[string]any{"security": "none", "key": ""}},
		},
		{
			name: "security=xtls",
			hint: "XTLS",
			stream: map[string]any{"network": "tcp", "security": "xtls",
				"xtlsSettings": map[string]any{"serverName": "example.org"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ib := map[string]any{
				"tag": "inbound-dead-" + tc.name, "port": 44401, "protocol": "vless",
				"settings": map[string]any{
					"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": ""}},
					"decryption": "none",
				},
				"streamSettings": tc.stream,
			}
			err := ValidateInboundReplacing(ib, "")
			if err == nil {
				t.Fatalf("%s 应当被核心拒绝——它还留在面板下拉里，用户选中就会导致全机断网", tc.name)
			}
			t.Logf("核心的拒绝理由: %v", err)
		})
	}
}

// 旧的 flow 值走的是另一条代码路径（infra/conf/vless.go:51 的白名单），
// 单独一个用例。
func TestRemovedFlowValuesAreRejectedByCore(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	for _, flow := range []string{"xtls-rprx-origin", "xtls-rprx-direct"} {
		t.Run(flow, func(t *testing.T) {
			ib := map[string]any{
				"tag": "inbound-flow-probe", "port": 44402, "protocol": "vless",
				"settings": map[string]any{
					"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": flow}},
					"decryption": "none",
				},
				"streamSettings": map[string]any{"network": "tcp", "security": "none"},
			}
			if err := ValidateInboundReplacing(ib, ""); err == nil {
				t.Fatalf("flow=%s 应当被核心拒绝", flow)
			}
		})
	}
}

// 以下三个是 Task 4-7 改完 xray.js 之后，前端必须能生成出来的形状。
// 任何一个在这里过不了，说明目标形状本身就是错的，不必等到手工验证。

func TestRealityVisionContractIsAccepted(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	keys, err := (&ServerService{}).GetNewX25519Cert()
	if err != nil {
		t.Fatal(err)
	}
	ib := realityInboundWith(keys["privateKey"].(string), "0123456789abcdef")
	// 前端会把客户端半边参数放在 realitySettings.settings 里（3x-ui 的约定）。
	// 核心的 REALITYConfig 没有这个字段，实测确认它被忽略而不是被拒绝。
	rs := ib["streamSettings"].(map[string]any)["realitySettings"].(map[string]any)
	rs["settings"] = map[string]any{
		"publicKey": keys["publicKey"], "fingerprint": "chrome",
		"serverName": "", "spiderX": "/",
	}
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Fatalf("REALITY+Vision 的目标形状被拒绝: %v", err)
	}
}

// Vision 配普通 TLS 时，minVersion 必须是 1.3：核心在运行期才检查
// （proxy/vless/inbound/inbound.go:573），run -test 查不出来。所以这个测试
// 只能守住「配置合法」，TLS 1.3 的强制要靠表单（Task 11）。
// 这条限制写在这里，是为了让改表单的人知道为什么不能只依赖后端校验。
func TestTlsVisionContractIsAccepted(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	// 简报原假设 `xray run -test` 只做配置构建校验、不读证书内容，已在
	// 上一个任务（web/service/ech_test.go 的 writeSelfSignedCert 注释）里
	// 被实测证伪：核心会尝试解析证书文件，路径不存在直接拒绝。这里改用
	// 真实生成的自签证书，而不是简报里的占位路径 "cert.crt"/"private.key"。
	certFile, keyFile := writeSelfSignedCert(t)

	ib := map[string]any{
		"tag": "inbound-tls-vision", "port": 44403, "protocol": "vless",
		"settings": map[string]any{
			"clients":    []any{map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "flow": "xtls-rprx-vision"}},
			"decryption": "none",
		},
		"streamSettings": map[string]any{
			"network": "tcp", "security": "tls",
			"tlsSettings": map[string]any{
				"serverName": "example.org",
				"minVersion": "1.3", "maxVersion": "1.3",
				"alpn":             []any{"h2", "http/1.1"},
				"rejectUnknownSni": false,
				"certificates": []any{map[string]any{
					"certificateFile": certFile, "keyFile": keyFile,
					"ocspStapling": 3600,
				}},
				"settings": map[string]any{"fingerprint": "chrome", "allowInsecure": false},
			},
			"tcpSettings": map[string]any{"header": map[string]any{"type": "none"}},
		},
	}
	if err := ValidateInboundReplacing(ib, ""); err != nil {
		t.Fatalf("TLS+Vision 的目标形状被拒绝: %v", err)
	}
}

// tlsSettings.settings 是面板自己加的客户端半边（fingerprint / allowInsecure /
// echConfigList），核心的 TLSConfig 里没有这个键。和 realitySettings.settings
// 一样，必须确认它被忽略而不是被拒绝，否则整个模型设计要改。
func TestPanelOnlySettingsKeyIsIgnoredByCore(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)

	// 同上：简报里的占位证书路径已被证伪，改用真实生成的自签证书。
	certFile, keyFile := writeSelfSignedCert(t)

	stream := `{"network":"ws","security":"tls",` +
		`"tlsSettings":{"serverName":"example.org","alpn":["h2","http/1.1"],` +
		`"minVersion":"1.2","maxVersion":"1.3",` +
		`"certificates":[{"certificateFile":"` + certFile + `","keyFile":"` + keyFile + `","ocspStapling":3600}],` +
		`"settings":{"fingerprint":"chrome","allowInsecure":false,"echConfigList":""}},` +
		`"wsSettings":{"path":"/","headers":{}}}`

	in := newInboundFor(44404, stream, true)
	if err := (&InboundService{}).AddInbound(in); err != nil {
		if strings.Contains(err.Error(), "xray") {
			t.Fatalf("面板自用的 settings 键被核心拒绝了，模型设计需要改: %v", err)
		}
		t.Fatalf("AddInbound: %v", err)
	}
}
