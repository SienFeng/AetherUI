package link

import "testing"

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
