package web

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func renderLogin(t *testing.T, twoFactor bool) string {
	t.Helper()
	tmpl := parseAllTemplates(t)
	var buf bytes.Buffer
	data := map[string]any{
		"base_path": "/", "cur_ver": "test", "title": "登录", "request_uri": "/",
		"two_factor": twoFactor,
	}
	if err := tmpl.ExecuteTemplate(&buf, "login.html", data); err != nil {
		t.Fatalf("渲染 login.html: %v", err)
	}
	return buf.String()
}

// 验证码输入框只在开启两步验证时出现，并且必须能被粘贴和密码管理器填写。
func TestLoginPageShowsCodeFieldOnlyWhenTwoFactorIsOn(t *testing.T) {
	off := renderLogin(t, false)
	if strings.Contains(off, `id="login-code"`) {
		t.Error("未开启两步验证时渲染了验证码输入框")
	}
	if !strings.Contains(off, "twoFactor: false") {
		t.Error("未开启时脚本里的 twoFactor 不是 false")
	}

	on := renderLogin(t, true)
	if !strings.Contains(on, `id="login-code"`) {
		t.Fatal("开启两步验证时没有渲染验证码输入框")
	}
	if !strings.Contains(on, "twoFactor: true") {
		t.Error("开启时脚本里的 twoFactor 不是 true")
	}
	for _, attr := range []string{`autocomplete="one-time-code"`, `inputmode="numeric"`, `for="login-code"`} {
		if !strings.Contains(on, attr) {
			t.Errorf("验证码输入框缺少 %s", attr)
		}
	}
}

// csrfGuard 的全部前提是：面板自己发出的每个请求都带 X-Requested-With。
// 这个头是 axios-init.js 全局加的——有人把那一行删掉或改掉，面板上所有
// 按钮会一起变成 403，而 Go 侧的测试一条都不会红。
func TestAxiosSendsTheHeaderCSRFGuardRequires(t *testing.T) {
	src, err := os.ReadFile("assets/js/axios-init.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `axios.defaults.headers.common['X-Requested-With'] = 'XMLHttpRequest'`) {
		t.Fatal("axios-init.js 不再全局设置 X-Requested-With: XMLHttpRequest，csrfGuard 会拒绝面板自己的所有请求")
	}
}

// 面板里不能有绕开 axios 的非 GET 请求：它们带不上那个头，会被 csrfGuard 403。
func TestNoStateChangingRequestBypassesAxios(t *testing.T) {
	tmpl := parseAllTemplates(t)
	for _, page := range topLevelPages {
		html := renderPage(t, tmpl, page)
		for _, bad := range []string{"fetch(", "new XMLHttpRequest", "sendBeacon(", `method="post"`, `method="POST"`} {
			if strings.Contains(html, bad) {
				t.Errorf("%s 里出现了 %q：不经 axios 的请求带不上 X-Requested-With，会被 csrfGuard 拒绝", page, bad)
			}
		}
	}
}
