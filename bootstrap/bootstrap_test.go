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
