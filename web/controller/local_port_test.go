package controller

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// localPort 给出这次连接实际到达的面板端口（设置页重启后据此判断是不是直连）。用真实监听测：
// httptest.NewRecorder 没有连接，拿不到本端地址。
func TestLocalPortIsTheConnectionsLocalPort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/p", func(c *gin.Context) { c.String(http.StatusOK, localPort(c)) })
	srv := httptest.NewServer(r)
	defer srv.Close()
	_, want, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/p")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != want {
		t.Fatalf("localPort = %q，期望 %q", got, want)
	}
	// 没有连接信息（测试里的 recorder）：空串，按「不是直连」处理。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p", nil))
	if w.Body.String() != "" {
		t.Fatalf("没有连接信息时应当是空串，实际 %q", w.Body.String())
	}
}
