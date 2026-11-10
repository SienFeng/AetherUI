package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"github.com/robfig/cron/v3"
	"a-ui/database"
	"a-ui/logger"
	"a-ui/web/global"
)

// capturePanelLog 把面板日志（logger 包，写 os.Stderr）临时改写进一个文件，
// 返回读取函数。logger.InitLogger 在调用那一刻捕获 os.Stderr，所以要先换掉
// os.Stderr 再重新初始化；结束时两者一起还原。
func capturePanelLog(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "panel-log-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	logger.InitLogger(logging.DEBUG)
	t.Cleanup(func() {
		os.Stderr = old
		logger.InitLogger(logging.INFO)
		f.Close()
	})
	return func() string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

// keepGinGlobals 还原 initRouter 顺手改掉的 gin 全局状态，免得影响本包其它测试。
func keepGinGlobals(t *testing.T) {
	t.Helper()
	mode, out, errOut := gin.Mode(), gin.DefaultWriter, gin.DefaultErrorWriter
	t.Cleanup(func() {
		gin.SetMode(mode)
		gin.DefaultWriter, gin.DefaultErrorWriter = out, errOut
	})
}

// routerForTest 搭出面板真实的路由（initRouter），不启动监听、不拉起 xray。
func routerForTest(t *testing.T) *gin.Engine {
	t.Helper()
	keepGinGlobals(t)
	if err := database.InitDB(filepath.Join(t.TempDir(), "router.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := NewServer()
	// 各个 controller 构造时会往 global.GetWebServer().GetCron() 上挂任务；
	// 这个 cron 不 Start，任务一个都不会跑。
	s.cron = cron.New()
	old := global.GetWebServer()
	global.SetWebServer(s)
	t.Cleanup(func() { global.SetWebServer(old) })
	engine, err := s.initRouter()
	if err != nil {
		t.Fatalf("initRouter: %v", err)
	}
	return engine
}

// handler 里的 panic 会被 gin 的 Recovery 截住、回 500，但改动前它的输出写进了
// gin.DefaultErrorWriter——而 initRouter 在调用 gin.Default() 之前就把它设成了
// io.Discard，于是任何一个接口 panic 都只剩一个 500，日志里什么都没有。
func TestHandlerPanicIsWrittenToPanelLog(t *testing.T) {
	readLog := capturePanelLog(t)
	engine := routerForTest(t)
	engine.GET("/panic-for-test", func(c *gin.Context) {
		panic("panic-marker-for-test")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic-for-test?token=EXAMPLE-TOKEN", nil)
	req.Header.Set("Cookie", "session=EXAMPLE-SESSION-COOKIE")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，期望 500", w.Code)
	}
	logged := readLog()
	if !strings.Contains(logged, "panic-marker-for-test") {
		t.Fatalf("handler 的 panic 没有写进面板日志，日志内容:\n%s", logged)
	}
	if !strings.Contains(logged, "/panic-for-test") {
		t.Errorf("日志里应当带上出事的接口路径，实际:\n%s", logged)
	}
	// 请求头里的会话 cookie 是 30 天有效的登录凭证，查询串里也可能有 token：
	// 都不能跟着 panic 一起进日志。
	for _, secret := range []string{"EXAMPLE-SESSION-COOKIE", "EXAMPLE-TOKEN"} {
		if strings.Contains(logged, secret) {
			t.Errorf("panic 日志里出现了请求里的凭证 %q", secret)
		}
	}
}
