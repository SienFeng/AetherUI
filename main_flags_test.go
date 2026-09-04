package main

import (
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/web/service"
)

func TestParseSettingFlagsDistinguishesUnsetFromEmpty(t *testing.T) {
	t.Run("未传 -listen 时为 nil", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-port", "8080"})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen != nil {
			t.Fatalf("未传 -listen，期望 nil，实际 %q", *f.Listen)
		}
		if f.Port != 8080 {
			t.Fatalf("port 期望 8080，实际 %d", f.Port)
		}
	})

	t.Run("传 -listen 空串时非 nil", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-listen", ""})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen == nil {
			t.Fatal("传了 -listen \"\"，期望非 nil（救援用：清空监听地址）")
		}
		if *f.Listen != "" {
			t.Fatalf("期望空串，实际 %q", *f.Listen)
		}
	})

	t.Run("传 -listen 有值", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-listen", "127.0.0.1", "-basepath", "/Ab3xK9pQ/"})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen == nil || *f.Listen != "127.0.0.1" {
			t.Fatalf("listen 期望 127.0.0.1，实际 %v", f.Listen)
		}
		if f.BasePath == nil || *f.BasePath != "/Ab3xK9pQ/" {
			t.Fatalf("basepath 期望 /Ab3xK9pQ/，实际 %v", f.BasePath)
		}
	})
}

// -show 是 `a-ui setting -show` 的只读数据源，供 a-ui.sh 菜单第 7 项与
// restore_direct_panel 查询端口/根路径用。同 bootstrap 包的
// TestCheckIsReadOnly 一个思路：跑一遍只读入口前后对比设置有没有变，
// 而不是走 os.Exit 的 showSetting，直接测不碰 os.Exit 的 currentSettings。
func TestCurrentSettingsIsReadOnly(t *testing.T) {
	if err := database.InitDB(t.TempDir() + "/test.db"); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	settingService := service.SettingService{}
	beforePort, err := settingService.GetPort()
	if err != nil {
		t.Fatalf("GetPort: %v", err)
	}
	beforeListen, err := settingService.GetListen()
	if err != nil {
		t.Fatalf("GetListen: %v", err)
	}
	beforeBasePath, err := settingService.GetBasePath()
	if err != nil {
		t.Fatalf("GetBasePath: %v", err)
	}

	userService := service.UserService{}
	beforeUser, err := userService.GetFirstUser()
	if err != nil {
		t.Fatalf("GetFirstUser: %v", err)
	}

	out, err := currentSettings()
	if err != nil {
		t.Fatalf("currentSettings: %v", err)
	}
	if out == "" {
		t.Fatal("期望非空输出")
	}
	if !strings.Contains(out, beforeUser.Username) {
		t.Fatalf("输出里应包含用户名 %q，实际输出：%s", beforeUser.Username, out)
	}
	// 全新数据库的默认监听地址是空串（监听所有 IP），展示文本不能是空白，
	// 否则看起来像是取值失败——见 currentSettings 里的换算逻辑。
	if beforeListen == "" && !strings.Contains(out, "all interfaces") {
		t.Fatalf("空监听地址应展示为 all interfaces，实际输出：%s", out)
	}

	afterPort, err := settingService.GetPort()
	if err != nil {
		t.Fatalf("GetPort after: %v", err)
	}
	afterListen, err := settingService.GetListen()
	if err != nil {
		t.Fatalf("GetListen after: %v", err)
	}
	afterBasePath, err := settingService.GetBasePath()
	if err != nil {
		t.Fatalf("GetBasePath after: %v", err)
	}
	afterUser, err := userService.GetFirstUser()
	if err != nil {
		t.Fatalf("GetFirstUser after: %v", err)
	}

	if beforePort != afterPort {
		t.Fatalf("currentSettings 不应写入，port 被改成 %v（原为 %v）", afterPort, beforePort)
	}
	if beforeListen != afterListen {
		t.Fatalf("currentSettings 不应写入，listen 被改成 %q（原为 %q）", afterListen, beforeListen)
	}
	if beforeBasePath != afterBasePath {
		t.Fatalf("currentSettings 不应写入，basePath 被改成 %q（原为 %q）", afterBasePath, beforeBasePath)
	}
	if beforeUser.Username != afterUser.Username || beforeUser.Password != afterUser.Password {
		t.Fatalf("currentSettings 不应写入，用户信息被改了：%+v -> %+v", beforeUser, afterUser)
	}
}
