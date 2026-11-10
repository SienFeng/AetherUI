package main

import (
	"bytes"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
)

// bareSettingHints 数出文本里没带完整路径的 `a-ui setting`。
func bareSettingHints(s string) int {
	return strings.Count(s, "a-ui setting") - strings.Count(s, "/usr/local/a-ui/a-ui setting")
}

// -show 打印的救援提示必须给出二进制的完整路径：管理员在终端里敲的 a-ui 是
// /usr/bin/a-ui 那个管理菜单脚本（a-ui.sh），它没有 setting 子命令，照着
// `a-ui setting -disable2fa` 敲下去什么都不会发生。
//
// Port: / Listen: 两行的格式同时是契约：a-ui.sh 用 grep '^Port:' 这类写法解析它们。
func TestShowRescueHintsUseFullBinaryPath(t *testing.T) {
	if err := database.InitDB(t.TempDir() + "/show.db"); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	// 让「两步验证已开启」那条提示也出现：twoFactorSecret 非空即表示已开启。
	if err := database.GetDB().Create(&model.Setting{Key: "twoFactorSecret", Value: "JBSWY3DPEHPK3PXP"}).Error; err != nil {
		t.Fatal(err)
	}
	out, err := currentSettings()
	if err != nil {
		t.Fatalf("currentSettings: %v", err)
	}
	for _, prefix := range []string{"Port: ", "Listen: "} {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, prefix) {
				found = true
			}
		}
		if !found {
			t.Errorf("输出里没有以 %q 开头的行：\n%s", prefix, out)
		}
	}
	for _, want := range []string{
		"/usr/local/a-ui/a-ui setting -username",
		"/usr/local/a-ui/a-ui setting -disable2fa",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("救援提示里应当有 %q，实际：\n%s", want, out)
		}
	}
	if n := bareSettingHints(out); n != 0 {
		t.Errorf("有 %d 处救援提示没带完整路径（/usr/bin/a-ui 是菜单脚本，没有 setting 子命令）：\n%s", n, out)
	}
}

// 成功的 `a-ui setting` 输出里不能出现 "failed"：安装脚本除了看退出码，还会在
// 输出里找这个词判断有没有失败。
func TestSettingCommandSuccessOutputNeverSaysFailed(t *testing.T) {
	initSettingTestDB(t)
	var out bytes.Buffer
	code := applySettingFlags(settingFlags{
		Listen: strPtr("127.0.0.1"), Port: freeLoopbackPort(t), BasePath: strPtr("/Ab3xK9pQ/"),
		Username: "root", Password: "n3w-pass", Disable2FA: true,
	}, &out)
	if code != 0 {
		t.Fatalf("合法的参数失败了，输出:\n%s", out.String())
	}
	if strings.Contains(strings.ToLower(out.String()), "failed") {
		t.Fatalf("成功的输出里出现了 failed：\n%s", out.String())
	}
}
