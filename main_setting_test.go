package main

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/util/crypto"
	"a-ui/web/service"
)

func initSettingTestDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(t.TempDir() + "/setting.db"); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func strPtr(s string) *string { return &s }

// freeLoopbackPort 返回一个此刻空闲的 127.0.0.1 端口。
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type storedPanel struct {
	listen   string
	port     int
	basePath string
	username string
}

func readStoredPanel(t *testing.T) storedPanel {
	t.Helper()
	s := service.SettingService{}
	listen, _ := s.GetListen()
	port, _ := s.GetPort()
	basePath, _ := s.GetBasePath()
	user, err := (&service.UserService{}).GetFirstUser()
	if err != nil {
		t.Fatal(err)
	}
	return storedPanel{listen, port, basePath, user.Username}
}

// `a-ui setting` 任何一步失败都必须以非零退出码结束：安装脚本与 a-ui.sh 靠退出码
// 判断改没改成功。改动前 updateSetting 对每一步失败都只打印一行，进程照样退出 0；
// -port 也完全不校验，70000 这样的值照存不误，面板下次启动必然监听失败。
//
// 并且是全有或全无：一条命令里任何一项被拒绝，其余各项一律不写。
func TestSettingCommandRejectsBadValuesWithNonZeroExit(t *testing.T) {
	cases := []struct {
		name string
		f    settingFlags
		want string
	}{
		{"端口超出范围", settingFlags{Port: 70000}, "port"},
		{"根路径是完整网址", settingFlags{BasePath: strPtr("/https://panel.example.com/abc/")}, "根路径"},
		{"根路径含冒号", settingFlags{BasePath: strPtr("/a:b/")}, "根路径"},
		{"监听地址不是 IP", settingFlags{Listen: strPtr("panel.example.com")}, "listen"},
		{"密码末尾有空格", settingFlags{Username: "admin", Password: "n3w-pass "}, "首尾"},
		{"只给了用户名", settingFlags{Username: "root"}, "password"},
		{"端口非法时同一条命令里的账号也不能改", settingFlags{Port: 70000, Username: "root", Password: "n3w-pass"}, "port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			initSettingTestDB(t)
			before := readStoredPanel(t)
			var out bytes.Buffer
			if code := applySettingFlags(c.f, &out); code == 0 {
				t.Fatalf("应当以非零退出码结束，实际 0；输出:\n%s", out.String())
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("输出里应当说明原因（含 %q），实际:\n%s", c.want, out.String())
			}
			if after := readStoredPanel(t); after != before {
				t.Fatalf("被拒绝的命令仍然写了库: %+v -> %+v", before, after)
			}
		})
	}
}

// 不在本机网卡上的监听地址同样拒绝（云服务器的公网 IP 常常是 NAT 映射）。
func TestSettingCommandRejectsListenAddressNotOnThisMachine(t *testing.T) {
	initSettingTestDB(t)
	var out bytes.Buffer
	if code := applySettingFlags(settingFlags{Listen: strPtr("198.51.100.7")}, &out); code == 0 {
		t.Fatalf("监听一个不在本机网卡上的地址应当失败，输出:\n%s", out.String())
	}
	if got := readStoredPanel(t).listen; got != "" {
		t.Fatalf("监听地址被改成了 %q", got)
	}
}

// 安装向导的域名拓扑（webListen 是 127.0.0.1，Caddy 反代）下，a-ui 菜单的「设置
// 面板端口」走的也是这条命令：照改不误的话 Caddy 仍反代旧端口，面板从外面消失。
// 而救援命令 `a-ui setting -listen ""` 必须照常可用。
func TestSettingCommandBehindCaddy(t *testing.T) {
	initSettingTestDB(t)
	s := service.SettingService{}
	if err := s.SetListen("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := applySettingFlags(settingFlags{Port: freeLoopbackPort(t)}, &out); code == 0 {
		t.Fatalf("域名拓扑下改端口应当失败，输出:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "18. 配置域名与伪装站") {
		t.Errorf("应当指向 a-ui 菜单的「配置域名与伪装站」，实际:\n%s", out.String())
	}

	out.Reset()
	if code := applySettingFlags(settingFlags{Listen: strPtr("")}, &out); code != 0 {
		t.Fatalf("救援命令 -listen \"\" 失败，输出:\n%s", out.String())
	}
	if got := readStoredPanel(t).listen; got != "" {
		t.Fatalf("救援之后监听地址 = %q，期望空串", got)
	}
}

func TestSettingCommandAppliesValidValues(t *testing.T) {
	initSettingTestDB(t)
	port := freeLoopbackPort(t)
	var out bytes.Buffer
	code := applySettingFlags(settingFlags{
		Listen: strPtr("127.0.0.1"), Port: port, BasePath: strPtr("Ab3xK9pQ"),
		Username: "root", Password: "n3w-pass",
	}, &out)
	if code != 0 {
		t.Fatalf("合法的参数失败了，输出:\n%s", out.String())
	}
	got := readStoredPanel(t)
	if got != (storedPanel{"127.0.0.1", port, "/Ab3xK9pQ/", "root"}) {
		t.Fatalf("落库结果 = %+v", got)
	}
	user, _ := (&service.UserService{}).GetFirstUser()
	if !crypto.CheckPassword(user.Password, "n3w-pass") {
		t.Fatal("新密码没有生效")
	}
}

// flag 包分不清「没传 -port」和「传了 -port 0」。0 是非法值（install.sh 用
// $RANDOM 生成随机端口，有 1/32768 的概率得到 0），不能当成没传而静默退出 0——
// 那样脚本会把一个根本没生效的端口打印给管理员。
func TestSettingCommandRejectsExplicitPortZero(t *testing.T) {
	f, err := parseSettingFlags([]string{"-port", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.PortSet {
		t.Fatal("传了 -port 0，PortSet 应为真")
	}
	initSettingTestDB(t)
	var out bytes.Buffer
	if code := applySettingFlags(f, &out); code == 0 {
		t.Fatalf("-port 0 应当失败，输出:\n%s", out.String())
	}
	if got := readStoredPanel(t).port; got != 54321 {
		t.Fatalf("端口被改成了 %d", got)
	}

	f, err = parseSettingFlags([]string{"-disable2fa"})
	if err != nil {
		t.Fatal(err)
	}
	if f.PortSet {
		t.Fatal("没传 -port，PortSet 应为假")
	}
	out.Reset()
	if code := applySettingFlags(f, &out); code != 0 {
		t.Fatalf("只关两步验证失败，输出:\n%s", out.String())
	}
}
