package service

import (
	"net"
	"strings"
	"testing"
)

// occupyLoopbackPort 在 127.0.0.1 上占一个临时端口，模拟「被别的程序占着」。
func occupyLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用临时端口: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func assertPanelAddrStored(t *testing.T, listen string, port int, basePath string) {
	t.Helper()
	s := SettingService{}
	gotListen, _ := s.GetListen()
	gotPort, _ := s.GetPort()
	gotBase, _ := s.GetBasePath()
	if gotListen != listen || gotPort != port || gotBase != basePath {
		t.Fatalf("库里的面板地址 = %q %d %q，期望保持 %q %d %q", gotListen, gotPort, gotBase, listen, port, basePath)
	}
}

// 被别的程序占着的端口存进去，下次面板启动必然监听失败。保存时试着监听一次。
func TestUpdateAllSettingRejectsPortAnotherProgramHolds(t *testing.T) {
	setupDB(t)
	busy := occupyLoopbackPort(t)

	all := validBaseSetting()
	all.WebListen = "127.0.0.1"
	all.WebPort = busy
	err := (&SettingService{}).UpdateAllSetting(all)
	if err == nil || !strings.Contains(err.Error(), "端口") {
		t.Fatalf("端口 %d 被占用时应当拒绝保存，实际 err = %v", busy, err)
	}
	assertPanelAddrStored(t, "", 54321, "/")
}

// 端口没变就不试：面板自己正占着它，一试必然「被占用」，那样管理员连一个
// 无关的设置项都保存不了。
func TestUpdateAllSettingSkipsBindProbeWhenPortIsUnchanged(t *testing.T) {
	setupDB(t)
	held := occupyLoopbackPort(t) // 扮演正在运行的面板自己
	s := &SettingService{}
	if err := s.SetListen("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPort(held); err != nil {
		t.Fatal(err)
	}

	all := validBaseSetting()
	all.WebListen = "127.0.0.1"
	all.WebPort = held
	all.TimeLocation = "UTC"
	if err := s.UpdateAllSetting(all); err != nil {
		t.Fatalf("端口没变时保存其它设置项被拒绝: %v", err)
	}
}

// 安装向导的域名拓扑：面板只监听 127.0.0.1，Caddy 按写死在 Caddyfile 里的端口与
// 根路径反代。在设置页改这两项不会同步到 Caddy，保存并重启后面板就从外面消失了。
func TestUpdateAllSettingRejectsPanelAddrChangeBehindCaddy(t *testing.T) {
	cases := []struct {
		name     string
		listen   string
		port     int
		basePath string
	}{
		{"改端口", "127.0.0.1", 0, "/Ab3xK9pQ/"}, // port 0 表示换成一个空闲端口
		{"改根路径", "127.0.0.1", 54321, "/Zz9yY8xX/"},
		{"换成另一个回环地址", "::1", 54321, "/Ab3xK9pQ/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupDB(t)
			s := &SettingService{}
			if err := s.SetListen("127.0.0.1"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetBasePath("/Ab3xK9pQ/"); err != nil {
				t.Fatal(err)
			}
			port := c.port
			if port == 0 {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port = ln.Addr().(*net.TCPAddr).Port
				ln.Close()
			}

			all := validBaseSetting()
			all.WebListen = c.listen
			all.WebPort = port
			all.WebBasePath = c.basePath
			err := s.UpdateAllSetting(all)
			if err == nil || !strings.Contains(err.Error(), "18. 配置域名与伪装站") {
				t.Fatalf("域名拓扑下%s应当被拒绝并指向 a-ui 菜单，实际 err = %v", c.name, err)
			}
			assertPanelAddrStored(t, "127.0.0.1", 54321, "/Ab3xK9pQ/")
		})
	}
}

// 反面：主动离开这个拓扑（监听地址改成非回环）是管理员自己的选择，照常放行；
// 什么都没改（只改了别的设置项）当然也要放行。
func TestUpdateAllSettingAllowsLeavingOrKeepingCaddyTopology(t *testing.T) {
	setupDB(t)
	s := &SettingService{}
	if err := s.SetListen("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBasePath("/Ab3xK9pQ/"); err != nil {
		t.Fatal(err)
	}

	keep := validBaseSetting()
	keep.WebListen = "127.0.0.1"
	keep.WebBasePath = "/Ab3xK9pQ/"
	keep.TimeLocation = "UTC"
	if err := s.UpdateAllSetting(keep); err != nil {
		t.Fatalf("域名拓扑下保存无关设置项被拒绝: %v", err)
	}

	leave := validBaseSetting()
	leave.WebListen = ""
	leave.WebBasePath = "/Zz9yY8xX/"
	if err := s.UpdateAllSetting(leave); err != nil {
		t.Fatalf("改成监听所有 IP（离开域名拓扑）时被拒绝: %v", err)
	}
	assertPanelAddrStored(t, "", 54321, "/Zz9yY8xX/")
}
