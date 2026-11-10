package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 安装 / 更新过了提交点之后，config_after_install 与 setup_wizard 还要等交互输入，面板这时停着。旧版在提交点
// 撤掉了中断处理，这时 Ctrl-C 或 SSH 断线就把面板留在停止状态，全部节点一直断着。
func TestInstallRestartsPanelWhenInterruptedAfterCommit(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		// 中断处理必须覆盖 config_after_install 到最后的 systemctl start a-ui。
		set := strings.Index(s, "    trap interrupted_after_commit INT TERM HUP\n")
		cfg := strings.Index(s, "\n    config_after_install\n")
		start := strings.Index(s[cfg:], "\n    systemctl start a-ui\n")
		clear := strings.Index(s[cfg:], "\n    trap - INT TERM HUP\n")
		if set < 0 || cfg < 0 || set > cfg || start < 0 || clear < start {
			t.Fatalf("%s：提交点之后的中断处理没有覆盖 config_after_install 到 systemctl start a-ui", script)
		}

		fn := extractShellFunc(t, script, "interrupted_after_commit")
		for _, tc := range []struct {
			name      string
			stopped   string
			wantStart bool
		}{
			{"更新：面板本来在跑", "1", true},
			{"全新安装：不拉起默认账户", "0", false},
		} {
			t.Run(script+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				log := filepath.Join(dir, "calls")
				stub := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
				if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("bash", "-c", "a_ui_stopped="+tc.stopped+"\n"+fn+"\ninterrupted_after_commit")
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
				if err := cmd.Run(); err == nil {
					t.Fatal("中断后应当以非零退出")
				}
				calls, _ := os.ReadFile(log)
				if got := strings.Contains(string(calls), "start a-ui"); got != tc.wantStart {
					t.Fatalf("拉起面板 = %v，期望 %v；systemctl 调用：%q", got, tc.wantStart, calls)
				}
			})
		}
	}
}
