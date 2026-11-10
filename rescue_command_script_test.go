package main

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 打印给管理员照着敲的 `a-ui setting …` 必须写全路径 /usr/local/a-ui/a-ui：/usr/bin/a-ui 是管理菜单脚本，
// 它不认识 setting 子命令，只会打印用法。安装完成与失败时那条「恢复监听所有 IP」的救援命令曾经就是裸的，
// 管理员照着敲、面板照样只监听回环。只看打印语句（echo / LOGI / LOGE / LOGD）：注释里提到命令名不算。
func TestPrintedSettingCommandsUseFullPath(t *testing.T) {
	printed := regexp.MustCompile(`^\s*(echo|LOGI|LOGE|LOGD)\b`)
	bare := regexp.MustCompile(`(^|[^/\w-])a-ui setting\b`)
	for _, name := range []string{"install.sh", "install_en.sh", "a-ui.sh", "a-ui_en.sh"} {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if !printed.MatchString(line) {
				continue
			}
			if bare.MatchString(strings.ReplaceAll(line, "/usr/local/a-ui/a-ui setting", "")) {
				t.Errorf("%s:%d 打印的 a-ui setting 没写全路径 /usr/local/a-ui/a-ui：%s", name, n, strings.TrimSpace(line))
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	}
}
