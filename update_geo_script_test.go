package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"a-ui/xray"
)

// update_geo 由 cron 每两天无人值守地跑一次，坏数据一旦落进 bin/ 就会在随后的
// 重启里让 xray 起不来、整机断网。这组测试把两个管理脚本里的 update_geo 原样
// 抽出来，用桩替换 curl / systemctl，校验交给真实 xray，守住三条：下载失败与
// 下载到的不是 dat 文件时都不动现有文件、不重启；数据正常时替换并重启。
func TestUpdateGeoOnlyReplacesValidatedData(t *testing.T) {
	xrayBin, err := filepath.Abs(filepath.Join("bin", xray.GetBinaryName()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(xrayBin); err != nil {
		t.Skipf("缺少 %s，无法用真实 xray 校验: %v", xrayBin, err)
	}
	srcDir, err := filepath.Abs("bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(srcDir, name)); err != nil {
			t.Skipf("缺少 bin/%s: %v", name, err)
		}
	}

	for _, script := range []string{"a-ui.sh", "a-ui_en.sh"} {
		fn := extractShellFunc(t, script, "update_geo")
		for _, tc := range []struct {
			name        string
			mode        string
			wantOK      bool
			wantRestart bool
		}{
			{"下载失败", "fail", false, false},
			{"下载到错误页", "garbage", false, false},
			{"数据正常", "good", true, true},
		} {
			t.Run(script+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				binDir := filepath.Join(root, "a-ui", "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(xrayBin, filepath.Join(binDir, "xray-linux-test")); err != nil {
					t.Fatal(err)
				}
				old := map[string][]byte{"geoip.dat": []byte("old-geoip"), "geosite.dat": []byte("old-geosite")}
				for name, b := range old {
					if err := os.WriteFile(filepath.Join(binDir, name), b, 0o644); err != nil {
						t.Fatal(err)
					}
				}

				harness := `
LOGE() { echo "ERR $*"; }
LOGI() { echo "INF $*"; }
PATH_FOR_GEO_IP="$ROOT/a-ui/bin/geoip.dat"
PATH_FOR_GEO_SITE="$ROOT/a-ui/bin/geosite.dat"
PATH_FOR_CONFIG="$ROOT/a-ui/bin/config.json"
URL_FOR_GEO_IP="https://example.invalid/geoip.dat"
URL_FOR_GEO_SITE="https://example.invalid/geosite.dat"
curl() {
    local out=""
    while [[ $# -gt 0 ]]; do
        [[ "$1" == "-o" ]] && { out="$2"; shift; }
        shift
    done
    case "$MODE" in
    fail) return 22 ;;
    garbage) echo "<html>429 Too Many Requests</html>" >"$out" ;;
    good) cp "$SRC/$(basename "$out")" "$out" ;;
    esac
}
systemctl() { echo "$*" >>"$ROOT/systemctl.log"; }
` + fn + `
update_geo
`
				cmd := exec.Command("bash", "-c", harness)
				cmd.Env = append(os.Environ(), "ROOT="+root, "SRC="+srcDir, "MODE="+tc.mode)
				out, runErr := cmd.CombinedOutput()
				if gotOK := runErr == nil; gotOK != tc.wantOK {
					t.Fatalf("返回成功=%v，期望 %v；输出:\n%s", gotOK, tc.wantOK, out)
				}

				for name, oldContent := range old {
					got, err := os.ReadFile(filepath.Join(binDir, name))
					if err != nil {
						t.Fatal(err)
					}
					if tc.wantOK {
						want, err := os.ReadFile(filepath.Join(srcDir, name))
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(got, want) {
							t.Fatalf("%s 未替换为新数据", name)
						}
					} else if !bytes.Equal(got, oldContent) {
						t.Fatalf("%s 被改动了，失败路径必须保留现有文件", name)
					}
				}

				restartLog, _ := os.ReadFile(filepath.Join(root, "systemctl.log"))
				if gotRestart := strings.Contains(string(restartLog), "restart a-ui"); gotRestart != tc.wantRestart {
					t.Fatalf("重启面板=%v，期望 %v；systemctl 调用: %q", gotRestart, tc.wantRestart, restartLog)
				}

				leftovers, _ := filepath.Glob(filepath.Join(root, "a-ui", ".geo-update-*"))
				if len(leftovers) > 0 {
					t.Fatalf("临时目录未清理: %v", leftovers)
				}
			})
		}
	}
}

// extractShellFunc 取出脚本里 name() { ... } 的完整定义，结尾以单独一行 "}" 为界。
func extractShellFunc(t *testing.T, script, name string) string {
	t.Helper()
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "\n"+name+"() {\n")
	if start < 0 {
		t.Fatalf("%s 里找不到函数 %s", script, name)
	}
	end := strings.Index(s[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s 里函数 %s 没有结尾", script, name)
	}
	return s[start+1 : start+end+3]
}
