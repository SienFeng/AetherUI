package main

import (
	"os/exec"
	"testing"

	"a-ui/database"
	"a-ui/web/service"
)

// 菜单「卸载 Caddy、恢复直连面板」在停 Caddy 之前用 show_listens_all 确认监听地址真的写成了「所有 IP」。
// 曾经只取 `Listen:` 行的第二列判空，而现在的二进制对空地址打印 `Listen: all interfaces`，第二列是 `all`：
// 每次都判失败，监听地址却已经改成所有 IP、面板也已重启，脚本还说「本次不会停用 Caddy」。
// 这里用真实的 currentSettings 输出喂两个脚本里的这个函数。
func TestShowListensAllAcceptsRealShowOutput(t *testing.T) {
	if err := database.InitDB(t.TempDir() + "/show.db"); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := service.SettingService{}
	if err := s.SetListen(""); err != nil {
		t.Fatal(err)
	}
	allOut, err := currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetListen("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	loopOut, err := currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"现在的二进制：所有 IP", allOut, true},
		{"旧二进制：空地址", "Port: 54321\nListen: \nBasePath: /\n", true},
		{"仍是回环", loopOut, false},
		{"没有 Listen 行（无法确认）", "Port: 54321\n", false},
	}
	for _, script := range []string{"a-ui.sh", "a-ui_en.sh"} {
		fn := extractShellFunc(t, script, "show_listens_all")
		for _, tc := range cases {
			t.Run(script+"/"+tc.name, func(t *testing.T) {
				err := exec.Command("bash", "-c", fn+"\nshow_listens_all \"$1\"", "x", tc.out).Run()
				if got := err == nil; got != tc.want {
					t.Fatalf("show_listens_all = %v, want %v；输出：\n%s", got, tc.want, tc.out)
				}
			})
		}
	}
}
