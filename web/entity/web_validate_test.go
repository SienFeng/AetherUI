package entity

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// 根路径会被拼进 gin 的路由（StaticFS(basePath+"assets")、engine.Group(basePath)）
// 和 Caddy 的 `handle <basePath>*`。改动前 CheckValid 只补首尾斜杠：粘贴一个完整
// 网址、或者含 : * 的值都能存进去，下次面板启动时 gin 注册路由直接 panic，面板
// 连同 xray 一起起不来。
func TestCheckValidRejectsUnroutableBasePath(t *testing.T) {
	for _, p := range []string{
		"/https://panel.example.com/abc/",
		"https://panel.example.com/abc",
		"/a:b/",
		"/:id/",
		"/a*b/",
		"/*any",
		"/a b/",
		"/a//b/",
		"/../",
		"/./x/",
		"/x/../",
		"/a%2Fb/",
		"/面板/",
		"/a?b/",
		"/a#b/",
	} {
		t.Run(p, func(t *testing.T) {
			s := validBaseSettingForEntityTest()
			s.WebBasePath = p
			err := s.CheckValid()
			if err == nil {
				t.Fatalf("根路径 %q 应当被拒绝，实际通过了（存为 %q）", p, s.WebBasePath)
			}
			if !strings.Contains(err.Error(), "根路径") {
				t.Fatalf("错误信息应当指向根路径，实际: %v", err)
			}
		})
	}
}

func TestCheckValidAcceptsRoutableBasePath(t *testing.T) {
	cases := map[string]string{
		"":               "/",
		"/":              "/",
		"abc":            "/abc/",
		"/abc":           "/abc/",
		"abc/":           "/abc/",
		"/Ab3xK9pQr2Zt/": "/Ab3xK9pQr2Zt/",
		"/a-b_c.d~e/":    "/a-b_c.d~e/",
		"/x/y/z/":        "/x/y/z/",
		"/..a/":          "/..a/",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			s := validBaseSettingForEntityTest()
			s.WebBasePath = in
			if err := s.CheckValid(); err != nil {
				t.Fatalf("根路径 %q 应当通过，实际: %v", in, err)
			}
			if s.WebBasePath != want {
				t.Fatalf("根路径 %q 补齐后 = %q，期望 %q", in, s.WebBasePath, want)
			}
		})
	}
}

// isLocalAddress 用真实网卡判断，测试里拿它跳过「恰好就在本机上」的文档地址。
func isLocalAddress(t *testing.T, ip string) bool {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("读不出本机网卡地址: %v", err)
	}
	want := net.ParseIP(ip)
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(want) {
			return true
		}
	}
	return false
}

// 面板监听一个不在本机网卡上的地址（典型是云服务器的公网 IP——它常常是 NAT
// 映射，根本不在网卡上）会在启动时报 can't assign requested address。改动前
// CheckValid 只查它是不是合法 IP。
func TestCheckValidRejectsListenAddressNotOnThisMachine(t *testing.T) {
	for _, ip := range []string{"192.0.2.11", "198.51.100.7", "203.0.113.9", "2001:db8::11"} {
		t.Run(ip, func(t *testing.T) {
			if isLocalAddress(t, ip) {
				t.Skipf("%v 恰好配在本机网卡上", ip)
			}
			s := validBaseSettingForEntityTest()
			s.WebListen = ip
			err := s.CheckValid()
			if err == nil {
				t.Fatalf("监听地址 %v 不在本机网卡上，应当被拒绝", ip)
			}
			if !strings.Contains(err.Error(), "网卡") {
				t.Fatalf("错误信息应当说明地址不在本机网卡上，实际: %v", err)
			}
		})
	}
}

// 空串（所有 IP）、通配地址、回环地址永远可用，本机网卡上的地址同样可用。
func TestCheckValidAcceptsListenAddressesThisMachineCanBind(t *testing.T) {
	listens := []string{"", "0.0.0.0", "::", "127.0.0.1", "::1"}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
				listens = append(listens, n.IP.String())
				break
			}
		}
	}
	for _, l := range listens {
		t.Run(l, func(t *testing.T) {
			s := validBaseSettingForEntityTest()
			s.WebListen = l
			if err := s.CheckValid(); err != nil {
				t.Fatalf("监听地址 %q 应当通过，实际: %v", l, err)
			}
		})
	}
}

// CheckPanelAddrChange 的两条规则在这里用桩替掉真实监听，逐条核对边界。
func TestCheckPanelAddrChangeRules(t *testing.T) {
	var probed []string
	old := probePanelListen
	t.Cleanup(func() { probePanelListen = old })
	probePanelListen = func(addr string) error {
		probed = append(probed, addr)
		if addr == "127.0.0.1:2053" {
			return errors.New("bind: address already in use")
		}
		return nil
	}
	caddy := PanelAddr{Listen: "127.0.0.1", Port: 54321, BasePath: "/Ab3xK9pQ/"}
	direct := PanelAddr{Listen: "", Port: 54321, BasePath: "/"}
	cases := []struct {
		name      string
		cur, next PanelAddr
		wantErr   string
		wantProbe []string
	}{
		{"域名拓扑下什么都没改", caddy, caddy, "", nil},
		{"域名拓扑下改端口", caddy, PanelAddr{"127.0.0.1", 8443, "/Ab3xK9pQ/"}, "18. 配置域名与伪装站", nil},
		{"域名拓扑下改根路径", caddy, PanelAddr{"127.0.0.1", 54321, "/x/"}, "18. 配置域名与伪装站", nil},
		{"域名拓扑下换回环地址", caddy, PanelAddr{"127.0.0.2", 54321, "/Ab3xK9pQ/"}, "18. 配置域名与伪装站", nil},
		{"离开域名拓扑并改端口", caddy, PanelAddr{"", 8443, "/Ab3xK9pQ/"}, "", []string{":8443"}},
		{"直连改端口", direct, PanelAddr{"", 8443, "/"}, "", []string{":8443"}},
		{"直连改成被占用的端口", direct, PanelAddr{"127.0.0.1", 2053, "/"}, "无法监听端口 2053", []string{"127.0.0.1:2053"}},
		{"直连只改监听地址", direct, PanelAddr{"127.0.0.1", 54321, "/"}, "", nil},
		{"直连改成回环监听并改端口（不是安装向导的拓扑）", direct, PanelAddr{"127.0.0.1", 8443, "/"}, "", []string{"127.0.0.1:8443"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probed = nil
			err := CheckPanelAddrChange(c.cur, c.next)
			if c.wantErr == "" && err != nil {
				t.Fatalf("应当放行，实际: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("应当报含 %q 的错误，实际: %v", c.wantErr, err)
			}
			if strings.Join(probed, ",") != strings.Join(c.wantProbe, ",") {
				t.Fatalf("试监听的地址 = %v，期望 %v", probed, c.wantProbe)
			}
		})
	}
}

// 用桩替掉真实网卡，确定性地核对「在不在本机网卡上」的判断，以及读不出网卡
// 地址时放行。
func TestCheckWebListenIsLocalAgainstStubbedInterfaces(t *testing.T) {
	old := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = old })
	_, v4, _ := net.ParseCIDR("192.0.2.10/24")
	v4.IP = net.ParseIP("192.0.2.10")
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{v4, &net.IPAddr{IP: net.ParseIP("2001:db8::10")}}, nil
	}
	for listen, ok := range map[string]bool{
		"":                  true,
		"0.0.0.0":           true,
		"::":                true,
		"127.0.0.53":        true,
		"192.0.2.10":        true,
		"::ffff:192.0.2.10": true,
		"2001:db8::10":      true,
		"192.0.2.11":        false,
		"2001:db8::11":      false,
		"not-an-ip":         false,
	} {
		if err := CheckWebListenIsLocal(listen); (err == nil) != ok {
			t.Errorf("CheckWebListenIsLocal(%q) = %v，期望通过 = %v", listen, err, ok)
		}
	}

	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("读不出网卡") }
	if err := CheckWebListenIsLocal("192.0.2.11"); err != nil {
		t.Errorf("读不出网卡地址时应当放行，实际: %v", err)
	}
}
