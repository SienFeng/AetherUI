package controller

import (
	"net/url"
	"strings"
	"testing"
)

// 设置页改账号走的是 updateUser → UserService.UpdateUser。登录页会 trim 输入，
// 一个带首尾空格的新密码一旦落库，管理员就再也登不上了，所以必须在保存时拒绝，
// 并且拒绝之后旧账号密码照常可用。
func TestUpdateUserRejectsSurroundingWhitespace(t *testing.T) {
	cases := []struct{ name, username, password string }{
		{"密码末尾有空格", "admin", "n3w-pass "},
		{"用户名开头有空格", " admin", "n3w-pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newAuthRouter(t)
			c := mustLogin(t, r, "admin", "admin")
			msg, _ := doAuth(t, r, authReq{path: "/aui/setting/updateUser", cookie: c, form: url.Values{
				"oldUsername": {"admin"}, "oldPassword": {"admin"},
				"newUsername": {tc.username}, "newPassword": {tc.password},
			}})
			if msg.Success {
				t.Fatalf("带首尾空格的账号密码（%q / %q）保存成功了", tc.username, tc.password)
			}
			if !strings.Contains(msg.Msg, "首尾") {
				t.Errorf("提示没有说清楚原因: %q", msg.Msg)
			}
			if login, _ := doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin")}); !login.Success {
				t.Fatalf("被拒绝的修改让原账号密码失效了: %s", login.Msg)
			}
		})
	}
}
