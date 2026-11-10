package bootstrap

import (
	"testing"

	"a-ui/database"
	"a-ui/database/model"
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

// 新装面板的另一组开箱默认：记录访问日志 + 每日自动更新 IP 归属地库。
//
// 与上面那组分流默认值同理，前半段的断言不是凑数：defaultValueMap 里这两项
// 必须保持关闭。访问日志一打开就是 xray 持续写盘加面板每 5 秒采集入库，
// IP 库一打开就是每天几十 MB 的出网流量，都不该因为管理员更新了一次面板
// 就静默发生在一台跑了很久的机器上。
func TestRunWritesObservabilityDefaults(t *testing.T) {
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

			if v, _ := s.GetAccessLogEnable(); v {
				t.Fatal("defaultValueMap 里 accessLogEnable 必须仍是 0")
			}
			if v, _ := s.GetIPDBUpdateTime(); v != "" {
				t.Fatalf("defaultValueMap 里 ipdbUpdateTime 必须仍是空串，实际 %q", v)
			}
			// 自动管理地区限制同理：存量部署升级后不能突然开始替所有用户设地区限制。
			if on, _ := s.GetAutoRegionParams(); on {
				t.Fatal("defaultValueMap 里 autoRegionEnable 必须仍是 0")
			}

			if _, err := Run(tc.opts); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if v, _ := s.GetAccessLogEnable(); !v {
				t.Error("新装应打开访问日志")
			}
			if v, _ := s.GetIPDBUpdateTime(); v != DefaultIPDBUpdateTime {
				t.Errorf("ipdbUpdateTime 期望 %q，实际 %q", DefaultIPDBUpdateTime, v)
			}
			if on, _ := s.GetAutoRegionParams(); !on {
				t.Error("新装应打开自动管理地区限制")
			}
			// 新装后的整份设置必须能通过保存校验：自动管理地区开着而访问日志关着是 CheckValid
			// 拒绝的组合，写出那种状态的话管理员在设置页改什么都保存不了。
			all, err := s.GetAllSetting()
			if err != nil {
				t.Fatalf("GetAllSetting: %v", err)
			}
			if err := all.CheckValid(); err != nil {
				t.Errorf("新装后的设置通不过保存校验: %v", err)
			}
			// 常量本身也要钉一下：它一旦被改成空串，上面那条相等断言照样通过，
			// 而空串的含义是「关闭自动更新」——新装即开启这个意图会静默消失，
			// 连带面板启动后那次首次拉取（RunInitialUpdate）也一并不发生。
			if DefaultIPDBUpdateTime == "" {
				t.Error("DefaultIPDBUpdateTime 不能是空串：空串表示关闭自动更新")
			}
			// 保留天数在 defaultValueMap 里就是 7，bootstrap 不重复写一遍。
			// 这条断言钉的是那个默认值：它一旦被改小，新装的机器会连
			// 「上周那次异常访问」都查不到，而向导这边什么都不会报。
			if v, _ := s.GetAccessLogRetentionDays(); v != 7 {
				t.Errorf("accessLogRetentionDays 期望 7，实际 %d", v)
			}
		})
	}
}

// -force 重跑向导时同样不能覆盖这两项，理由与分流默认值完全相同：管理员
// 可能正是因为磁盘紧张才关掉访问日志、因为流量计费才关掉 IP 库自动更新，
// 而他这次只是想重配域名和伪装站。
func TestRunForceKeepsExistingObservabilityDefaults(t *testing.T) {
	setupDB(t)

	first := Options{Mode: "caddy", Domain: "example.com", BasePath: "/first/",
		Listen: "127.0.0.1", Port: 54321}
	if _, err := Run(first); err != nil {
		t.Fatalf("首次 Run: %v", err)
	}

	s := service.SettingService{}
	if err := s.SetAccessLogEnable(false); err != nil {
		t.Fatalf("SetAccessLogEnable: %v", err)
	}
	if err := s.SetIPDBUpdateTime(""); err != nil {
		t.Fatalf("SetIPDBUpdateTime: %v", err)
	}
	if err := s.SetAutoRegionEnable(false); err != nil {
		t.Fatalf("SetAutoRegionEnable: %v", err)
	}

	second := first
	second.BasePath = "/second/"
	second.Force = true
	if _, err := Run(second); err != nil {
		t.Fatalf("force Run: %v", err)
	}

	if v, _ := s.GetAccessLogEnable(); v {
		t.Error("accessLogEnable 被向导覆盖了")
	}
	if v, _ := s.GetIPDBUpdateTime(); v != "" {
		t.Errorf("ipdbUpdateTime 被向导覆盖了，实际 %q", v)
	}
	if on, _ := s.GetAutoRegionParams(); on {
		t.Error("autoRegionEnable 被向导覆盖了")
	}
}

// mode=defaults 是安装脚本给每台全新安装先写一遍的开箱默认（与向导成败无关）：向导中途失败（443 被占、
// 证书没签下来）时 caddy / reality 两条路径都不会走到写默认值那一步，新装的机器就拿不到自动管理地区
// 限制、访问日志这些默认。它只写这五项，不碰根路径——根路径仍是 "/"，之后的向导照常判定为未配置。
func TestRunDefaultsModeWritesOnlyOpenBoxDefaults(t *testing.T) {
	setupDB(t)
	s := service.SettingService{}
	res, err := Run(Options{Mode: "defaults"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("全新的库不该跳过")
	}
	if on, _ := s.GetAutoRegionParams(); !on {
		t.Error("应打开自动管理地区限制")
	}
	if v, _ := s.GetAccessLogEnable(); !v {
		t.Error("应打开访问日志")
	}
	if v, _ := s.GetIPDBUpdateTime(); v != DefaultIPDBUpdateTime {
		t.Errorf("ipdbUpdateTime = %q", v)
	}
	if v, _ := s.GetDNSServers(); v != DefaultDNSServers {
		t.Errorf("dnsServers = %q", v)
	}
	if v, _ := s.GetIPRuleResolveDomain(); !v {
		t.Error("应打开 IP 规则匹配域名目标")
	}
	if done, _ := alreadyInitialized(); done {
		t.Error("defaults 不能改根路径，否则之后的向导会被当成已配置而整个跳过")
	}
	all, err := s.GetAllSetting()
	if err != nil {
		t.Fatal(err)
	}
	if err := all.CheckValid(); err != nil {
		t.Errorf("写完默认值后的设置通不过保存校验: %v", err)
	}
}

// 已经配置过的面板（根路径不是 "/"）上 defaults 一个字节都不写：管理员可能已经关掉了其中某几项。
func TestRunDefaultsModeSkipsConfiguredPanel(t *testing.T) {
	setupDB(t)
	s := service.SettingService{}
	if err := s.SetBasePath("/Ab3xK9pQ/"); err != nil {
		t.Fatal(err)
	}
	res, err := Run(Options{Mode: "defaults", Force: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Skipped {
		t.Error("已配置的面板应跳过（-force 也不例外）")
	}
	if on, _ := s.GetAutoRegionParams(); on {
		t.Error("已配置的面板不该被改动")
	}
}

// 每台全新安装实际走的顺序：config_after_install 先跑 defaults，随后向导再跑 caddy / reality。defaults
// 不能让向导被当成「已配置」而跳过，也不能让向导之后的整份设置通不过保存校验；reality 建入站时生成配置
// 已经带着 DoH、IPIfNonMatch 与访问日志，入站照样要建得起来。
func TestRunDefaultsThenWizard(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"caddy", Options{Mode: "caddy", Domain: "example.com", BasePath: "/Ab3xK9pQ/",
			Listen: "127.0.0.1", Port: 54321}},
		{"reality", Options{Mode: "reality", RealityDest: "www.tesla.com:443", BasePath: "/Zz9Yy8/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDB(t)
			if _, err := Run(Options{Mode: "defaults"}); err != nil {
				t.Fatalf("defaults: %v", err)
			}
			res, err := Run(tc.opts)
			if err != nil {
				t.Fatalf("向导: %v", err)
			}
			if res.Skipped {
				t.Fatal("defaults 之后的向导被跳过了")
			}
			s := service.SettingService{}
			if bp, _ := s.GetBasePath(); bp != tc.opts.BasePath {
				t.Errorf("根路径 = %q，期望 %q", bp, tc.opts.BasePath)
			}
			if on, _ := s.GetAutoRegionParams(); !on {
				t.Error("自动管理地区限制应保持打开")
			}
			if tc.opts.Mode == "reality" {
				inbounds, err := (&service.InboundService{}).GetAllInbounds()
				if err != nil || len(inbounds) != 1 || inbounds[0].Port != 443 {
					t.Errorf("REALITY 入站没建起来: %v %v", inbounds, err)
				}
			}
			all, err := s.GetAllSetting()
			if err != nil {
				t.Fatal(err)
			}
			if err := all.CheckValid(); err != nil {
				t.Errorf("整份设置通不过保存校验: %v", err)
			}
		})
	}
}

// 根路径仍是 "/" 的在用老机器（向导出现之前装的、或向导当年失败后一直这么用着）重跑向导：它被当成
// 「未配置」照常配置域名，但开箱默认不能跟着写进去——那几项是管理员的既有选择，自动管理地区限制尤其
// 会在一天之内把所有没设地区的用户锁到一个省，界面上什么提示都没有。判据是库里已经有入站。
func TestRunWizardOnUsedPanelKeepsExistingDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"caddy", Options{Mode: "caddy", Domain: "example.com", BasePath: "/Ab3xK9pQ/",
			Listen: "127.0.0.1", Port: 54321}},
		{"reality", Options{Mode: "reality", RealityDest: "www.microsoft.com:443", BasePath: "/Ab3xK9pQ/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDB(t)
			if err := database.GetDB().Create(&model.Inbound{Port: 23456, Protocol: model.VMess, Enable: true,
				Tag: "inbound-23456", Settings: "{}", StreamSettings: "{}", Sniffing: "{}"}).Error; err != nil {
				t.Fatal(err)
			}
			res, err := Run(tc.opts)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Skipped {
				t.Fatal("根路径仍是 / 的面板，向导应当照常配置")
			}
			s := service.SettingService{}
			if on, _ := s.GetAutoRegionParams(); on {
				t.Error("在用的面板重跑向导，自动管理地区限制被打开了")
			}
			if v, _ := s.GetAccessLogEnable(); v {
				t.Error("在用的面板重跑向导，访问日志被打开了")
			}
			if v, _ := s.GetDNSServers(); v != "" {
				t.Errorf("在用的面板重跑向导，DNS 被改成了 %q", v)
			}
		})
	}
}

// -mode defaults 对库里已有入站的面板同样跳过（脚本只在库文件原本不存在时调它，这里再守一道）。
func TestRunDefaultsModeSkipsPanelWithInbounds(t *testing.T) {
	setupDB(t)
	if err := database.GetDB().Create(&model.Inbound{Port: 23456, Protocol: model.VMess, Enable: true,
		Tag: "inbound-23456", Settings: "{}", StreamSettings: "{}", Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	res, err := Run(Options{Mode: "defaults"})
	if err != nil || !res.Skipped {
		t.Fatalf("有入站的面板应当跳过: res=%+v err=%v", res, err)
	}
	if on, _ := (&service.SettingService{}).GetAutoRegionParams(); on {
		t.Error("有入站的面板被打开了自动管理地区限制")
	}
}
