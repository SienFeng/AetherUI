package bootstrap

import (
	"testing"

	"a-ui/database"
	"a-ui/web/service"
)

func setupDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(t.TempDir() + "/test.db"); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func TestRunCaddyModeWritesSettings(t *testing.T) {
	setupDB(t)

	res, err := Run(Options{
		Mode:     "caddy",
		Domain:   "example.com",
		BasePath: "/Ab3xK9pQ/",
		Listen:   "127.0.0.1",
		Port:     54321,
		CertFile: "/root/cert/fullchain.cer",
		KeyFile:  "/root/cert/example.com.key",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("全新数据库不应被跳过")
	}
	if want := "https://example.com/Ab3xK9pQ/"; res.PanelURL != want {
		t.Fatalf("PanelURL 期望 %q，实际 %q", want, res.PanelURL)
	}

	s := service.SettingService{}
	if v, _ := s.GetListen(); v != "127.0.0.1" {
		t.Fatalf("webListen 实际 %q", v)
	}
	if v, _ := s.GetBasePath(); v != "/Ab3xK9pQ/" {
		t.Fatalf("webBasePath 实际 %q", v)
	}
	if v, _ := s.GetDefaultDomain(); v != "example.com" {
		t.Fatalf("defaultDomain 实际 %q", v)
	}
	if v, _ := s.GetDefaultCertFile(); v != "/root/cert/fullchain.cer" {
		t.Fatalf("defaultCertFile 实际 %q", v)
	}
}

// a-ui update 会 rm -rf /usr/local/a-ui 后重跑 install.sh。没有这道幂等
// 判断的话，每次面板更新都会把管理员改过的 basePath、监听地址覆盖回去。
func TestRunSkipsWhenAlreadyInitialized(t *testing.T) {
	setupDB(t)

	first := Options{Mode: "caddy", Domain: "example.com", BasePath: "/first/",
		Listen: "127.0.0.1", Port: 54321}
	if _, err := Run(first); err != nil {
		t.Fatalf("首次 Run: %v", err)
	}

	second := first
	second.BasePath = "/second/"
	res, err := Run(second)
	if err != nil {
		t.Fatalf("二次 Run: %v", err)
	}
	if !res.Skipped {
		t.Fatal("已初始化时应被跳过")
	}

	s := service.SettingService{}
	if v, _ := s.GetBasePath(); v != "/first/" {
		t.Fatalf("basePath 被覆盖了，实际 %q", v)
	}
}

func TestRunForceOverwrites(t *testing.T) {
	setupDB(t)

	first := Options{Mode: "caddy", Domain: "example.com", BasePath: "/first/",
		Listen: "127.0.0.1", Port: 54321}
	if _, err := Run(first); err != nil {
		t.Fatalf("首次 Run: %v", err)
	}

	second := first
	second.BasePath = "/second/"
	second.Force = true
	res, err := Run(second)
	if err != nil {
		t.Fatalf("force Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("-force 时不应跳过")
	}

	s := service.SettingService{}
	if v, _ := s.GetBasePath(); v != "/second/" {
		t.Fatalf("basePath 期望 /second/，实际 %q", v)
	}
}

// 安装脚本靠 -check 做幂等探测。它必须是只读的：若探测本身产生写入，
// 全新机器上第一次探测就会把探测参数变成实际配置。
func TestCheckIsReadOnly(t *testing.T) {
	setupDB(t)

	res, err := Run(Options{Check: true})
	if err != nil {
		t.Fatalf("Run(-check): %v", err)
	}
	if res.Skipped {
		t.Fatal("全新数据库应报告未初始化")
	}

	s := service.SettingService{}
	if v, _ := s.GetBasePath(); v != "/" {
		t.Fatalf("-check 不应写入任何设置，basePath 实际 %q", v)
	}

	if _, err := Run(Options{Mode: "caddy", Domain: "example.com",
		BasePath: "/Ab3xK9pQ/", Listen: "127.0.0.1", Port: 54321}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	res, err = Run(Options{Check: true})
	if err != nil {
		t.Fatalf("Run(-check) 二次: %v", err)
	}
	if !res.Skipped {
		t.Fatal("已初始化后应报告已初始化")
	}
}

func TestRunRejectsCaddyModeWithoutDomain(t *testing.T) {
	setupDB(t)
	if _, err := Run(Options{Mode: "caddy", BasePath: "/x/", Port: 54321}); err == nil {
		t.Fatal("mode=caddy 缺 -domain 应报错")
	}
}

// mode=reality 是无域名分支：安装脚本不写证书/域名相关设置，改为直接建一个
// VLESS+Vision+REALITY 入站。这个入站要走 InboundService.AddInbound（而不是
// 直接写库），本机若没有 bin/xray-<GOOS>-<GOARCH>，其内部的 xray 校验按项目
// 既定的 fail open 策略放行，测试仍应通过。
func TestRunRealityModeCreatesInbound(t *testing.T) {
	setupDB(t)

	res, err := Run(Options{
		Mode:        "reality",
		BasePath:    "/Zz9Yy8/",
		Port:        45678,
		RealityDest: "www.tesla.com:443",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("全新数据库不应被跳过")
	}

	inboundService := service.InboundService{}
	inbounds, err := inboundService.GetAllInbounds()
	if err != nil {
		t.Fatalf("GetAllInbounds: %v", err)
	}
	if len(inbounds) != 1 {
		t.Fatalf("期望 1 个入站，实际 %d", len(inbounds))
	}
	if inbounds[0].Port != 443 {
		t.Fatalf("入站端口期望 443，实际 %d", inbounds[0].Port)
	}

	// UserId 必须落到管理员账号上：InboundController.getInbounds 按
	// user_id 过滤，落空会让这个入站在管理员登录后的列表里"隐形"。
	userService := service.UserService{}
	admin, err := userService.GetFirstUser()
	if err != nil {
		t.Fatalf("GetFirstUser: %v", err)
	}
	if inbounds[0].UserId != admin.Id {
		t.Fatalf("入站 UserId 期望 %d（管理员），实际 %d", admin.Id, inbounds[0].UserId)
	}
}

// mode=caddy 必须清空面板自己的直连 TLS 证书（webCertFile/webKeyFile）：
// Caddy 已经终结 TLS、以明文转发到面板，遗留值会被 network.AutoHttpsConn
// 把明文连接误判成"非 TLS 连接"（对每个请求回 307 到原 URL，从外面看是
// 死循环），若证书文件已不存在则 Server.Start() 直接失败、面板进程退出，
// 而 a-ui.service 没有 Restart= 策略，面板从此彻底不再监听——这两种情况
// 唯一被打印过的恢复命令 `a-ui setting -listen ""` 都救不回来，因为它只
// 改 webListen，不碰这两项。
func TestRunCaddyModeClearsWebTLS(t *testing.T) {
	setupDB(t)

	s := service.SettingService{}
	if err := s.SetCertFile("/root/stale-cert.crt"); err != nil {
		t.Fatalf("SetCertFile: %v", err)
	}
	if err := s.SetKeyFile("/root/stale-key.key"); err != nil {
		t.Fatalf("SetKeyFile: %v", err)
	}

	if _, err := Run(Options{
		Mode:     "caddy",
		Domain:   "example.com",
		BasePath: "/Ab3xK9pQ/",
		Listen:   "127.0.0.1",
		Port:     54321,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if v, _ := s.GetCertFile(); v != "" {
		t.Fatalf("webCertFile 期望被清空，实际 %q", v)
	}
	if v, _ := s.GetKeyFile(); v != "" {
		t.Fatalf("webKeyFile 期望被清空，实际 %q", v)
	}
}

// mode=reality 下面板仍是直连暴露给外部，管理员可能确实需要给面板自己配
// 一份 HTTPS，不能替他清掉。
func TestRunRealityModeKeepsWebTLS(t *testing.T) {
	setupDB(t)

	s := service.SettingService{}
	if err := s.SetCertFile("/root/panel-cert.crt"); err != nil {
		t.Fatalf("SetCertFile: %v", err)
	}
	if err := s.SetKeyFile("/root/panel-key.key"); err != nil {
		t.Fatalf("SetKeyFile: %v", err)
	}

	if _, err := Run(Options{
		Mode:        "reality",
		BasePath:    "/Zz9Yy8/",
		Port:        45678,
		RealityDest: "www.tesla.com:443",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if v, _ := s.GetCertFile(); v != "/root/panel-cert.crt" {
		t.Fatalf("webCertFile 不应被 mode=reality 触碰，实际 %q", v)
	}
	if v, _ := s.GetKeyFile(); v != "/root/panel-key.key" {
		t.Fatalf("webKeyFile 不应被 mode=reality 触碰，实际 %q", v)
	}
}

// 新装面板的开箱默认：IP 分流规则也匹配域名目标 + 加密 DNS。
//
// 前半段的断言不是凑数：defaultValueMap 里这两项必须保持 0 / 空串，
// 存量部署「升级后行为零变化」全靠它。一旦有人为了省事把默认值改掉，
// 老面板里从未点过「保存配置」的那些会跟着静默启用 DoH 与 IPIfNonMatch，
// 这条测试的前半段会先红。
func TestRunWritesRoutingDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"caddy", Options{Mode: "caddy", Domain: "example.com", BasePath: "/Ab3xK9pQ/",
			Listen: "127.0.0.1", Port: 54321}},
		{"reality", Options{Mode: "reality", RealityDest: "www.microsoft.com:443",
			BasePath: "/Ab3xK9pQ/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDB(t)
			s := service.SettingService{}

			if v, _ := s.GetIPRuleResolveDomain(); v {
				t.Fatal("defaultValueMap 里 ipRuleResolveDomain 必须仍是 0")
			}
			if v, _ := s.GetDNSServers(); v != "" {
				t.Fatalf("defaultValueMap 里 dnsServers 必须仍是空串，实际 %q", v)
			}

			if _, err := Run(tc.opts); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if v, _ := s.GetIPRuleResolveDomain(); !v {
				t.Error("新装应打开「IP 规则匹配域名目标」")
			}
			if v, _ := s.GetDNSServers(); v != DefaultDNSServers {
				t.Errorf("dnsServers 期望 %q，实际 %q", DefaultDNSServers, v)
			}
		})
	}
}

// 写进去的默认值必须能原样通过设置页的校验。
//
// checkDNSServer 拒绝的写法（udp:// tls:// quic:// IP:端口、裸域名）在 xray
// 那边要么静默空转要么拒绝启动，把其中之一写成新装默认，管理员第一次打开
// 设置页点「保存配置」就会失败——而且报错只指向 DNS，端口、证书路径等一切
// 无关字段会一起保存不了。
func TestRoutingDefaultsPassSettingValidation(t *testing.T) {
	setupDB(t)

	if _, err := Run(Options{Mode: "caddy", Domain: "example.com",
		BasePath: "/Ab3xK9pQ/", Listen: "127.0.0.1", Port: 54321}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	all, err := (&service.SettingService{}).GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if err := all.CheckValid(); err != nil {
		t.Fatalf("新装默认设置未通过校验: %v", err)
	}
}

// -force 是 install.sh --wizard-only（a-ui 菜单「配置域名与伪装站」）重跑
// 向导的路径，面板此前已经被配置过。向导问的是域名和伪装站，不能顺手把
// 管理员自己调过的解析路径改回默认——那是静默的夹带修改。
func TestRunForceKeepsExistingRoutingDefaults(t *testing.T) {
	setupDB(t)

	first := Options{Mode: "caddy", Domain: "example.com", BasePath: "/first/",
		Listen: "127.0.0.1", Port: 54321}
	if _, err := Run(first); err != nil {
		t.Fatalf("首次 Run: %v", err)
	}

	s := service.SettingService{}
	if err := s.SetDNSServers("https://9.9.9.9/dns-query"); err != nil {
		t.Fatalf("SetDNSServers: %v", err)
	}
	if err := s.SetIPRuleResolveDomain(false); err != nil {
		t.Fatalf("SetIPRuleResolveDomain: %v", err)
	}

	second := first
	second.BasePath = "/second/"
	second.Force = true
	if _, err := Run(second); err != nil {
		t.Fatalf("force Run: %v", err)
	}

	if v, _ := s.GetDNSServers(); v != "https://9.9.9.9/dns-query" {
		t.Errorf("dnsServers 被向导覆盖了，实际 %q", v)
	}
	if v, _ := s.GetIPRuleResolveDomain(); v {
		t.Error("ipRuleResolveDomain 被向导覆盖了")
	}
}
