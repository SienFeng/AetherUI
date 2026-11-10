package web

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/robfig/cron/v3"
	"a-ui/database"
	"a-ui/web/global"
	"a-ui/web/service"
)

// 会话 cookie 的 Path 是面板根路径（web/session 的 cookiePath），浏览器只在这个
// 路径之下才带上它。哪个接口挂到了根路径之外，它就永远收不到会话、永远是未登录
// 状态——所以面板的每一条路由都必须在根路径之下。
func TestEveryPanelRouteLivesUnderBasePath(t *testing.T) {
	keepGinGlobals(t)
	if err := database.InitDB(filepath.Join(t.TempDir(), "routes.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	const base = "/Ab3xK9pQ/"
	if err := (&service.SettingService{}).SetBasePath(base); err != nil {
		t.Fatal(err)
	}
	s := NewServer()
	s.cron = cron.New()
	old := global.GetWebServer()
	global.SetWebServer(s)
	t.Cleanup(func() { global.SetWebServer(old) })

	engine, err := s.initRouter()
	if err != nil {
		t.Fatalf("initRouter: %v", err)
	}
	routes := engine.Routes()
	if len(routes) == 0 {
		t.Fatal("一条路由都没有注册")
	}
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, base) {
			t.Errorf("路由 %s %s 不在面板根路径 %s 之下，会话 cookie 不会发给它", r.Method, r.Path, base)
		}
	}
}
