package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/web/controller"
)

func TestCSRFGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(csrfGuard())
	hits := 0
	handler := func(c *gin.Context) { hits++; c.String(http.StatusOK, "ok") }
	r.GET("/x", handler)
	r.POST("/x", handler)

	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"GET 不受限", http.MethodGet, nil, http.StatusOK},
		{"跨站表单 POST（带不上自定义头）", http.MethodPost, nil, http.StatusForbidden},
		{"面板自己的 axios 请求", http.MethodPost,
			map[string]string{"X-Requested-With": "XMLHttpRequest", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"不带 Sec-Fetch-Site 的旧浏览器", http.MethodPost,
			map[string]string{"X-Requested-With": "XMLHttpRequest"}, http.StatusOK},
		{"浏览器标明跨站", http.MethodPost,
			map[string]string{"X-Requested-With": "XMLHttpRequest", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"头的值不对", http.MethodPost,
			map[string]string{"X-Requested-With": "fetch"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := hits
			req := httptest.NewRequest(c.method, "/x", nil)
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d", w.Code, c.want)
			}
			if reached := hits > before; reached != (c.want == http.StatusOK) {
				t.Errorf("handler 是否被执行 = %v，与状态码不符", reached)
			}
		})
	}
}

// 上面那条只测了中间件本身。这条走面板真正使用的那条中间件链加上真实的
// 登录接口：凭据完全正确的跨站请求也必须在到达登录逻辑之前被拒。
func TestCrossSiteLoginIsRejectedBeforeReachingLoginLogic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "router.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	engine := gin.New()
	useSessionAndCSRF(engine, []byte("test-secret"))
	controller.NewIndexController(engine.Group("/"))

	post := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader("username=admin&password=admin"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	w := post(nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("不带 X-Requested-With 的登录 status = %d, want 403", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("被拒绝的请求仍然下发了 cookie")
	}

	w = post(map[string]string{"X-Requested-With": "XMLHttpRequest"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("面板自己的登录请求被拒: %d %s", w.Code, w.Body.String())
	}
}
