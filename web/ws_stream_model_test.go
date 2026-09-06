package web

import (
	"regexp"
	"strings"
	"testing"
)

// 前端的 WsStreamSettings 必须把 acceptProxyProtocol 发进 wsSettings。
//
// 与 xray.TestWsAcceptProxyProtocolIsReadByCore 成对：那一条钉「核心读得到
// 这个键」，这一条钉「前端确实发出这个键」。少了任何一条，一个拼错的键名
// 都会让开关变成纯装饰，而 infra/conf 不做未知字段检查，没有任何一层会报错。
func TestWsStreamSettingsEmitsAcceptProxyProtocol(t *testing.T) {
	data, err := assetsFS.ReadFile("assets/js/model/xray.js")
	if err != nil {
		t.Fatalf("读取 xray.js: %v", err)
	}
	body := wsStreamSettingsClass(t, string(data))

	// toJson 里必须原样出现这个键名，前面的构造函数属性同名。
	if !regexp.MustCompile(`\bacceptProxyProtocol\b`).MatchString(body) {
		t.Error("WsStreamSettings 里没有 acceptProxyProtocol：" +
			"这个开关不会进入 wsSettings，界面上却看得见、点得动")
	}
}

func wsStreamSettingsClass(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "class WsStreamSettings")
	if start < 0 {
		t.Fatal("xray.js 里找不到 class WsStreamSettings，要么文件结构变了，要么本测试该更新了")
	}
	rest := src[start:]
	end := strings.Index(rest, "\nclass ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
