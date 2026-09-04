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
