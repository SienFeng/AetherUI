package web

import (
	"errors"
	"strings"
	"testing"
)

// 日志里的救援命令必须给出二进制的完整路径：管理员 SSH 上去敲的 a-ui 是
// /usr/bin/a-ui 那个管理菜单脚本（a-ui.sh），它没有 setting 子命令。
// a-ui restart 是菜单脚本自己的子命令，照常写短名。
func TestPanelRescueHintsUseFullBinaryPath(t *testing.T) {
	for name, msg := range map[string]string{
		"监听失败":    listenFailedMessage("127.0.0.1:54321", errors.New("bind: address already in use")),
		"路由初始化失败": routerFailedHint,
	} {
		if !strings.Contains(msg, "/usr/local/a-ui/a-ui setting") {
			t.Errorf("%s的提示里没有带完整路径的救援命令：%s", name, msg)
		}
		if bare := strings.Count(msg, "a-ui setting") - strings.Count(msg, "/usr/local/a-ui/a-ui setting"); bare != 0 {
			t.Errorf("%s的提示里有 %d 处 a-ui setting 没带完整路径：%s", name, bare, msg)
		}
	}
}
