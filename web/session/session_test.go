package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/crypto"
)

// newSessionRouter 搭一个与面板相同形状的最小路由：会话中间件、base_path 中间件，
// 全部接口挂在根路径之下。
func newSessionRouter(t *testing.T, basePath string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "session.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	user := &model.User{}
	if err := database.GetDB().First(user).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	r.Use(func(c *gin.Context) { c.Set("base_path", basePath) })
	g := r.Group(basePath)
	g.POST("/login", func(c *gin.Context) {
		if err := SetLoginUser(c, user); err != nil {
			t.Errorf("SetLoginUser: %v", err)
		}
		c.String(http.StatusOK, "ok")
	})
	g.GET("/check", func(c *gin.Context) {
		if IsLogin(c) {
			c.String(http.StatusOK, "in")
		} else {
			c.String(http.StatusOK, "out")
		}
	})
	g.GET("/logout", func(c *gin.Context) {
		ClearSession(c)
		c.String(http.StatusOK, "bye")
	})
	// 升级前的代码签发的会话：Path=/，会话里只有 id、用户名与指纹三项。
	g.POST("/legacy-login", func(c *gin.Context) {
		s := sessions.Default(c)
		s.Set(loginUserId, user.Id)
		s.Set(loginUserName, user.Username)
		s.Set(loginUserFp, crypto.SessionFingerprint(user.Username, user.Password))
		s.Options(sessions.Options{Path: "/", MaxAge: maxAgeSeconds, HttpOnly: true})
		if err := s.Save(); err != nil {
			t.Errorf("写入旧格式会话: %v", err)
		}
		c.String(http.StatusOK, "ok")
	})
	return r
}

func serve(r *gin.Engine, method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// sessionCookies 按 Path 取出响应里所有名为 session 的 Set-Cookie。
func sessionCookies(w *httptest.ResponseRecorder) cookiesByPath {
	out := cookiesByPath{}
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			out[c.Path] = c
		}
	}
	return out
}

// cookiesByPath 打印时只列 Path 与 MaxAge，不把 cookie 的值写进测试输出。
type cookiesByPath map[string]*http.Cookie

func (m cookiesByPath) String() string {
	s := "["
	for path, c := range m {
		s += fmt.Sprintf(" Path=%s MaxAge=%d", path, c.MaxAge)
	}
	return s + " ]"
}

const panelBase = "/Ab3xK9pQ/"

// 域名拓扑下同一个域名上，面板只占 /<根路径>/，其余路径都由 Caddy 反代到第三方
// 伪装站。会话 cookie 的 Path 是 / 的话，浏览器访问伪装站的每个请求都会带上它——
// 等于把一张 30 天有效的面板登录凭证交给了第三方。
func TestSessionCookieIsScopedToPanelBasePath(t *testing.T) {
	r := newSessionRouter(t, panelBase)
	w := serve(r, http.MethodPost, panelBase+"login")
	got := sessionCookies(w)
	c := got["/Ab3xK9pQ/"]
	if c == nil || c.MaxAge < 0 {
		t.Fatalf("会话 cookie 的 Path 应为 /Ab3xK9pQ/（带末尾斜杠：Caddy 只把 /<根路径>/* 转给面板，不带斜杠的 /<根路径> 走伪装站，Path=/Ab3xK9pQ 的 cookie 会被带过去），实际下发了 %v", got)
	}
	// 升级前签发的 Path=/ 那一份要顺手删掉，否则它还会被继续发给伪装站。
	if legacy := got["/"]; legacy == nil || legacy.MaxAge >= 0 {
		t.Errorf("登录时应当同时删掉 Path=/ 的旧会话 cookie，实际 %v", got)
	}
	if body := serve(r, http.MethodGet, panelBase+"check", c).Body.String(); body != "in" {
		t.Fatalf("带着新 cookie 访问面板接口被当成未登录: %q", body)
	}
}

// 退出登录要把两份都删掉：只删新路径那份的话，升级前留下的 Path=/ 会话还在，
// 退出等于没退。
func TestLogoutExpiresPanelAndLegacyRootCookie(t *testing.T) {
	r := newSessionRouter(t, panelBase)
	c := sessionCookies(serve(r, http.MethodPost, panelBase+"login"))["/Ab3xK9pQ/"]
	if c == nil {
		t.Fatal("登录没有下发会话 cookie")
	}
	got := sessionCookies(serve(r, http.MethodGet, panelBase+"logout", c))
	for _, path := range []string{"/Ab3xK9pQ/", "/"} {
		if e := got[path]; e == nil || e.MaxAge >= 0 {
			t.Errorf("退出登录没有删掉 Path=%s 的会话 cookie，实际 %v", path, got)
		}
	}
}

// 升级前签发的 Path=/ 会话一律作废（升级后要重新登录一次）：它们早已被浏览器
// 带去过伪装站，不能再被当成有效凭证。
func TestPreUpgradeRootPathSessionIsRejected(t *testing.T) {
	r := newSessionRouter(t, panelBase)
	legacy := sessionCookies(serve(r, http.MethodPost, panelBase+"legacy-login"))["/"]
	if legacy == nil {
		t.Fatal("没有拿到旧格式的会话 cookie")
	}
	if body := serve(r, http.MethodGet, panelBase+"check", legacy).Body.String(); body != "out" {
		t.Fatalf("升级前签发的 Path=/ 会话仍被当作已登录: %q", body)
	}
}

// 根路径就是 / 的部署（未经安装向导、或手动改回 / 的）照旧用 Path=/，也不能
// 顺手把刚下发的那一份删掉。
func TestRootBasePathKeepsRootCookie(t *testing.T) {
	r := newSessionRouter(t, "/")
	w := serve(r, http.MethodPost, "/login")
	var all []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			all = append(all, c)
		}
	}
	if len(all) != 1 || all[0].Path != "/" || all[0].MaxAge < 0 {
		t.Fatalf("根路径为 / 时应当只下发一份 Path=/ 的有效 cookie，实际 %d 份", len(all))
	}
	if body := serve(r, http.MethodGet, "/check", all[0]).Body.String(); body != "in" {
		t.Fatalf("根路径为 / 时登录后被当成未登录: %q", body)
	}
}
