package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runShell 在 PATH 前面加上 stubDir 跑一段 bash，返回合并输出。
func runShell(t *testing.T, stubDir, home, script string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 证书菜单直接回车（域名为空）时，修复前签发失败后的清理是 rm -rf ~/.acme.sh/——acme.sh 本体、账户密钥与其它所有
// 域名的证书一起删光。现在域名先校验，清理只删这个域名自己的两个目录（含 ECC 的 _ecc）。
func TestCertMenuDomainHelpers(t *testing.T) {
	for _, script := range []string{"a-ui.sh", "a-ui_en.sh"} {
		t.Run(script, func(t *testing.T) {
			fns := extractShellFunc(t, script, "valid_cert_domain") + extractShellFunc(t, script, "acme_cert_exists") +
				extractShellFunc(t, script, "remove_acme_domain")
			home := t.TempDir()
			stubs := t.TempDir()
			acmeDir := filepath.Join(home, ".acme.sh")
			log := filepath.Join(home, "acme-calls")
			writeExec(t, filepath.Join(acmeDir, "acme.sh"), `echo "$@" >> `+log+`
if [ "$1" = "--list" ]; then
  echo "Main_Domain  KeyLength  SAN_Domains  CA  Created  Renew"
  echo "sub.example.com  ec-256  no  LetsEncrypt.org  2026  2027"
fi`)
			for _, d := range []string{"other.example.net", "example.com", "example.com_ecc"} {
				if err := os.MkdirAll(filepath.Join(acmeDir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			for _, bad := range []string{"", "a b.com", "*", "x;rm -rf /", "-a.com", "example", "../etc"} {
				if out := runShell(t, stubs, home, fns+`valid_cert_domain "`+strings.ReplaceAll(bad, `"`, `\"`)+`" && echo VALID`); strings.Contains(out, "VALID") {
					t.Errorf("域名 %q 应当被拒绝", bad)
				}
			}
			for _, good := range []string{"example.com", "sub.example-1.com", "xn--fiqs8s.cn"} {
				if out := runShell(t, stubs, home, fns+`valid_cert_domain "`+good+`" && echo VALID`); !strings.Contains(out, "VALID") {
					t.Errorf("域名 %q 应当通过", good)
				}
			}

			runShell(t, stubs, home, fns+`remove_acme_domain ""`)
			if _, err := os.Stat(filepath.Join(acmeDir, "acme.sh")); err != nil {
				t.Fatal("空域名的清理把 ~/.acme.sh 删掉了")
			}

			out := runShell(t, stubs, home, fns+`acme_cert_exists example.com && echo EXISTS; acme_cert_exists sub.example.com && echo SUB`)
			if strings.Contains(out, "EXISTS") || !strings.Contains(out, "SUB") {
				t.Errorf("已有证书的判断应当按主域名精确比对，实际输出 %q", out)
			}

			runShell(t, stubs, home, fns+`remove_acme_domain example.com`)
			for d, want := range map[string]bool{"example.com": false, "example.com_ecc": false, "other.example.net": true, "acme.sh": true} {
				_, err := os.Stat(filepath.Join(acmeDir, d))
				if (err == nil) != want {
					t.Errorf("清理 example.com 之后 %s 存在=%v，应当=%v", d, err == nil, want)
				}
			}
		})
	}
}

// curl https://get.acme.sh | sh 下载失败时 sh 读到空输入照样退出 0，修复前菜单报「acme安装成功」。
func TestInstallAcmeFailsWhenDownloadFails(t *testing.T) {
	for _, script := range []string{"a-ui.sh", "a-ui_en.sh"} {
		home, stubs := t.TempDir(), t.TempDir()
		writeExec(t, filepath.Join(stubs, "curl"), "exit 7")
		out := runShell(t, stubs, home, "LOGI() { echo \"$@\"; }\nLOGE() { echo \"$@\"; }\n"+
			extractShellFunc(t, script, "install_acme")+"install_acme; echo rc=$?")
		if !strings.Contains(out, "rc=1") {
			t.Errorf("%s: 下载失败时 install_acme 应当返回失败，输出 %q", script, out)
		}
	}
}

// 证书文件按域名命名：修复前固定写 /root/cert/fullchain.cer，与 Caddy 的证书同步（a-ui-cert-sync）以及别的域名
// 互相覆盖，证书与私钥配不上对，下次整进程重启时整份 xray 配置加载失败。
func TestCertMenuDoesNotWriteSharedFullchain(t *testing.T) {
	for _, script := range []string{"a-ui.sh", "a-ui_en.sh"} {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "--fullchain-file /root/cert/fullchain.cer") || strings.Contains(s, "rm -rf ~/.acme.sh/${") {
			t.Errorf("%s 仍在写共用的 fullchain.cer 或按未校验的变量 rm -rf", script)
		}
		if strings.Count(s, "\"$(caddy_managed_domain | tr '[:upper:]' '[:lower:]')\" ==") != 2 {
			t.Errorf("%s: 两种签发方式都要拒绝 Caddy 正在管理的域名", script)
		}
	}
}

// 内置静态伪装页用专用目录：修复前用 /var/www/html，刚停用的 apache/nginx 的站点（含 PHP 源码、.git）原样对外。
func TestStaticMaskSiteUsesDedicatedDocroot(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "/var/www/html") && !strings.Contains(string(b), "It used to be /var/www/html") &&
			!strings.Contains(string(b), "修复前用 /var/www/html") {
			t.Errorf("%s 仍在用 /var/www/html", script)
		}
		dir := t.TempDir()
		out := runShell(t, t.TempDir(), dir, "mask_docroot="+dir+"/mask\n"+extractShellFunc(t, script, "ensure_static_site")+
			"mkdir -p "+dir+"/html && echo apache > "+dir+"/html/index.html\nensure_static_site && cat "+dir+"/mask/index.html | head -1")
		if !strings.Contains(out, "<!doctype html>") {
			t.Errorf("%s: 伪装页应当写进专用目录，输出 %q", script, out)
		}
	}
}

// 这次才装上的 Caddy 在写好 Caddyfile 之前失败时要停掉并取消自启；Caddy 已经在跑时回滚提示不能说
// 「80/443 上没有任何服务在监听」。
func TestFreshCaddyStoppedAndRollbackHintMatchesReality(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		t.Run(script, func(t *testing.T) {
			fns := extractShellFunc(t, script, "stop_fresh_caddy") + extractShellFunc(t, script, "print_stopped_svc_rollback_hint")
			stubs := t.TempDir()
			log := filepath.Join(stubs, "calls")
			writeExec(t, filepath.Join(stubs, "systemctl"), `echo "$@" >> `+log+`
[ "$1" = "is-active" ] && exit 0
exit 0`)
			out := runShell(t, stubs, t.TempDir(), fns+"caddy_freshly_installed=1\nstop_fresh_caddy\nstop_fresh_caddy\nstopped_web_svc=nginx\nprint_stopped_svc_rollback_hint")
			calls, _ := os.ReadFile(log)
			if strings.Count(string(calls), "disable --now caddy") != 1 {
				t.Errorf("新装的 Caddy 应当被停掉并取消自启恰好一次，calls=%q", calls)
			}
			if !strings.Contains(out, "systemctl disable --now caddy && systemctl enable --now nginx") {
				t.Errorf("Caddy 在跑时回滚提示应当先停 Caddy，输出 %q", out)
			}

			os.Remove(log)
			runShell(t, stubs, t.TempDir(), fns+"caddy_freshly_installed=0\nstop_fresh_caddy")
			if calls, _ := os.ReadFile(log); len(calls) != 0 {
				t.Errorf("不是这次装的 Caddy 不该动，calls=%q", calls)
			}
		})
	}
}

// apt 装 Caddy 的任何一步失败都要删掉这次写下的源列表：修复前没有 gpg 时钥匙没写成、源列表却留下，此后整机
// apt update 报错。
func TestInstallCaddyAptCleansUpOnFailure(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		dir := t.TempDir()
		list := filepath.Join(dir, "caddy-stable.list")
		keyring := filepath.Join(dir, "caddy.gpg")
		fn := strings.NewReplacer("/etc/apt/sources.list.d/caddy-stable.list", list,
			"/usr/share/keyrings/caddy-stable-archive-keyring.gpg", keyring).Replace(extractShellFunc(t, script, "install_caddy_apt"))
		stubs := t.TempDir()
		// 源列表与钥匙都写成了，最后 apt-get install caddy 失败。
		writeExec(t, filepath.Join(stubs, "apt-get"), `case "$*" in *"-qq caddy"*) exit 100;; esac; exit 0`)
		// curl 把内容写到 -o 指定的文件。
		writeExec(t, filepath.Join(stubs, "curl"), `while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then echo data > "$2"; fi; shift; done`)
		writeExec(t, filepath.Join(stubs, "gpg"), `while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then echo key > "$2"; fi; shift; done`)
		out := runShell(t, stubs, dir, "red=; plain=\n"+fn+"install_caddy_apt; echo rc=$?")
		if !strings.Contains(out, "rc=1") {
			t.Errorf("%s: apt 安装失败时应当返回失败，输出 %q", script, out)
		}
		if _, err := os.Stat(list); !os.IsNotExist(err) {
			t.Errorf("%s: 失败后源列表没有删掉", script)
		}
		if _, err := os.Stat(keyring); !os.IsNotExist(err) {
			t.Errorf("%s: 失败后钥匙没有删掉", script)
		}

		// 管理员早先自己加过的源列表不能删（只删这次新写下的）。
		if err := os.WriteFile(list, []byte("deb existing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runShell(t, stubs, dir, "red=; plain=\n"+fn+"install_caddy_apt")
		if b, err := os.ReadFile(list); err != nil || string(b) != "data\n" && string(b) != "deb existing\n" {
			t.Errorf("%s: 早先就有的源列表被删掉了 (%v)", script, err)
		}
	}
}
