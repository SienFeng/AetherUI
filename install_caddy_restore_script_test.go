package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 重跑向导时 write_caddyfile 已经把 Caddyfile 换成新的随机根路径，之后证书没签下来或 bootstrap 失败、面板还在
// 旧根路径上：旧版不换回 Caddyfile，旧地址落到伪装站、新地址 404，面板从外网进不去，提示却说「Caddy 现在用的
// 还是上一次的配置」。面板已经在新根路径上时新配置才是对的，不能换回。
func TestRestoreCaddyfileOnlyWhenPanelDidNotMove(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		fns := extractShellFunc(t, script, "drop_caddyfile_backup") + extractShellFunc(t, script, "restore_caddyfile_if_panel_unchanged") +
			extractShellFunc(t, script, "restore_caddyfile_on_interrupt")
		for _, tc := range []struct {
			name        string
			panelBase   string
			wantRestore bool
		}{
			{"面板还在旧根路径", "/old/", true},
			{"面板已在新根路径", "/new/", false},
		} {
			t.Run(script+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				caddyfile := filepath.Join(dir, "Caddyfile")
				backup := filepath.Join(dir, "Caddyfile.prev")
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				must(os.WriteFile(caddyfile, []byte("new"), 0o644))
				must(os.WriteFile(backup, []byte("old"), 0o644))
				aui := filepath.Join(dir, "a-ui")
				must(os.WriteFile(aui, []byte("#!/bin/sh\necho 'Port: 54321'\necho 'BasePath: "+tc.panelBase+"'\n"), 0o755))
				log := filepath.Join(dir, "calls")
				must(os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755))
				body := strings.NewReplacer("/etc/caddy/Caddyfile", caddyfile, "/usr/local/a-ui/a-ui", aui).Replace(fns)
				// 走中断处理那条路（等证书时按 Ctrl-C）：与失败分支同一个判定。
				cmd := exec.Command("bash", "-c", "caddyfile_backup="+backup+"\npending_caddy_basepath=/new/\n"+body+
					"\nrestore_caddyfile_on_interrupt; restore_caddyfile_if_panel_unchanged /new/ >/dev/null; echo \"rc=$( [[ $(cat "+caddyfile+") == old ]] && echo 0 || echo 1) pending=${pending_caddy_basepath}\"")
				cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
				out, _ := cmd.CombinedOutput()
				got, _ := os.ReadFile(caddyfile)
				calls, _ := os.ReadFile(log)
				if tc.wantRestore {
					if string(got) != "old" || !strings.Contains(string(calls), "restart caddy") || !strings.Contains(string(out), "rc=0") {
						t.Fatalf("应当换回并重启 Caddy：Caddyfile=%q calls=%q out=%s", got, calls, out)
					}
				} else {
					if string(got) != "new" || len(calls) != 0 || !strings.Contains(string(out), "rc=1") {
						t.Fatalf("面板已在新根路径，不该换回：Caddyfile=%q calls=%q out=%s", got, calls, out)
					}
					if _, err := os.Stat(backup); !os.IsNotExist(err) {
						t.Fatal("用不上的备份应当删掉")
					}
				}
			})
		}
	}
}
