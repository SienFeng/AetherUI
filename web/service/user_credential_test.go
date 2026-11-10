package service

import (
	"strings"
	"testing"

	"a-ui/util/crypto"
)

// badCredentials 是登录页无法原样提交的用户名/密码：登录页对输入做 trim
// （String.prototype.trim，连全角空格 U+3000 和 BOM U+FEFF 一起去掉），而设置页
// 与 `a-ui setting` 原样落库——存进去的值在登录时永远对不上，管理员被锁在门外。
var badCredentials = []struct {
	name, username, password string
}{
	{"用户名前有空格", " admin", "n3w-pass"},
	{"用户名后有空格", "admin ", "n3w-pass"},
	{"用户名后有全角空格", "admin\u3000", "n3w-pass"},
	{"密码后有空格", "admin", "n3w-pass "},
	{"密码前有制表符", "admin", "\tn3w-pass"},
	{"密码后有换行", "admin", "n3w-pass\n"},
	{"密码前有 BOM", "admin", "\ufeffn3w-pass"},
	{"用户名超长", strings.Repeat("a", 257), "n3w-pass"},
}

func assertStillAdminAdmin(t *testing.T) {
	t.Helper()
	user, err := (&UserService{}).GetFirstUser()
	if err != nil {
		t.Fatalf("GetFirstUser: %v", err)
	}
	if user.Username != "admin" || !crypto.CheckPassword(user.Password, "admin") {
		t.Fatalf("被拒绝的修改仍然落库了: username=%q", user.Username)
	}
}

func TestUpdateFirstUserRejectsCredentialsTheLoginPageCannotSubmit(t *testing.T) {
	for _, c := range badCredentials {
		t.Run(c.name, func(t *testing.T) {
			setupDB(t)
			err := (&UserService{}).UpdateFirstUser(c.username, c.password)
			if err == nil {
				t.Fatalf("UpdateFirstUser(%q, %q) 应当被拒绝", c.username, c.password)
			}
			assertStillAdminAdmin(t)
		})
	}
}

func TestUpdateUserRejectsCredentialsTheLoginPageCannotSubmit(t *testing.T) {
	for _, c := range badCredentials {
		t.Run(c.name, func(t *testing.T) {
			setupDB(t)
			user, err := (&UserService{}).GetFirstUser()
			if err != nil {
				t.Fatalf("GetFirstUser: %v", err)
			}
			if _, err := (&UserService{}).UpdateUser(user.Id, c.username, c.password); err == nil {
				t.Fatalf("UpdateUser(%q, %q) 应当被拒绝", c.username, c.password)
			}
			assertStillAdminAdmin(t)
		})
	}
}

// 中间有空格、含中文都是合法的：只拒绝首尾。
func TestCredentialCheckAllowsInnerSpacesAndUnicode(t *testing.T) {
	setupDB(t)
	if err := (&UserService{}).UpdateFirstUser("管理 员", "pass word 密码"); err != nil {
		t.Fatalf("中间带空格的账号密码被拒绝: %v", err)
	}
	if (&UserService{}).CheckUser("管理 员", "pass word 密码") == nil {
		t.Fatal("改完之后用新账号密码登录失败")
	}
}
