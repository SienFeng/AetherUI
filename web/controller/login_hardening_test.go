package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"a-ui/logger"
	"a-ui/web/entity"
)

// postLogin 发一次登录请求并解出响应。可以在任意 goroutine 里调用：出错时返回
// error，不调用 t.Fatal（那只能在测试主 goroutine 里用）。
func postLogin(r *gin.Engine, remoteAddr, username, password string) (entity.Msg, error) {
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var msg entity.Msg
	err := json.Unmarshal(w.Body.Bytes(), &msg)
	return msg, err
}

// 限速的检查（Blocked）与记账（Fail）原来是两段分开的临界区，中间隔着一次约
// 50ms 的 bcrypt：同一来源同时发来的一批错误密码全部通过了检查，再各自记一笔
// 失败——实测一批 40 个错误密码有二三十个被真正比对过，限速形同虚设。
//
// 现在同一来源「已失败次数 + 正在比对的次数」不能超过上限：一批全错的并发请求里
// 恰好有 loginMaxFailures 个被比对，前 loginMaxFailures-1 个回「用户名或密码
// 错误」，最后一个触发锁定，其余的直接被挡回；之后哪怕密码正确也不放行。
func TestConcurrentWrongPasswordsCannotOutrunTheLimiter(t *testing.T) {
	r := newAuthRouter(t)
	const attacker = "203.0.113.7:5000"
	const burst = 40

	var wrong, locked, other int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			msg, err := postLogin(r, attacker, "admin", "wrong-guess")
			switch {
			case err != nil:
				t.Errorf("解析响应: %v", err)
			case msg.Success:
				t.Error("错误密码登录成功了")
			case msg.Msg == "用户名或密码错误":
				atomic.AddInt32(&wrong, 1)
			case strings.Contains(msg.Msg, "分钟后再试"):
				atomic.AddInt32(&locked, 1)
			default:
				atomic.AddInt32(&other, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wrong > loginMaxFailures-1 {
		t.Fatalf("同一来源并发的 %d 个错误密码里有 %d 个被比对后回「用户名或密码错误」，上限应为 %d（锁定 %d 个，其余 %d 个）",
			burst, wrong, loginMaxFailures-1, locked, other)
	}
	if _, blocked := loginGuard.Blocked("203.0.113.7"); !blocked {
		t.Fatal("一批并发的错误密码之后该来源没有被锁定")
	}
	msg, err := postLogin(r, attacker, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Success || !strings.Contains(msg.Msg, "分钟后再试") {
		t.Fatalf("锁定期内正确密码应当被拒绝，实际: %+v", msg)
	}
}

// countingBody 是一个按需生成内容的请求体：{"username":"aaaa…","password":"x"}，
// 记下被读走了多少字节。不预先分配整个请求体，测出来的才是服务端读了多少。
type countingBody struct {
	parts [][]byte
	fill  int64
	read  int64
}

func newCountingBody(fill int64) *countingBody {
	return &countingBody{
		parts: [][]byte{[]byte(`{"username":"`), nil, []byte(`","password":"x"}`)},
		fill:  fill,
	}
}

func (b *countingBody) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		switch {
		case len(b.parts[0]) > 0:
			c := copy(p[n:], b.parts[0])
			b.parts[0] = b.parts[0][c:]
			n += c
		case b.fill > 0:
			c := int64(len(p) - n)
			if c > b.fill {
				c = b.fill
			}
			for i := int64(0); i < c; i++ {
				p[n+int(i)] = 'a'
			}
			b.fill -= c
			n += int(c)
		case len(b.parts[2]) > 0:
			c := copy(p[n:], b.parts[2])
			b.parts[2] = b.parts[2][c:]
			n += c
		default:
			b.read += int64(n)
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
	}
	b.read += int64(n)
	return n, nil
}

// /login 不需要登录就能访问，改动前却会把整个请求体读进内存再解析（gin 的 JSON
// 绑定没有任何上限）：一个 64 MiB 的用户名让 RSS 涨了 520 MiB。请求体在解析之前
// 就要设上限，超出的部分一个字节都不该读。
func TestLoginReadsAtMostTheBodyCap(t *testing.T) {
	r := newAuthRouter(t)
	for _, contentType := range []string{"application/json", "application/x-www-form-urlencoded"} {
		t.Run(contentType, func(t *testing.T) {
			body := newCountingBody(512 << 10)
			req := httptest.NewRequest(http.MethodPost, "/login", body)
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			req.RemoteAddr = "198.51.100.20:5000"
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// MaxBytesReader 最多多读 1 字节；再留出解码器一次读缓冲的余量。
			if limit := int64(loginMaxBodyBytes + 4096); body.read > limit {
				t.Fatalf("服务端读走了 %d 字节，上限应为 %d", body.read, limit)
			}
			var msg entity.Msg
			if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
				t.Fatalf("解析响应 %q: %v", w.Body.String(), err)
			}
			if msg.Success {
				t.Fatal("超大请求体的登录成功了")
			}
		})
	}
}

// 超长的用户名/密码在查库与 bcrypt 之前就拒绝，也不计入限速（它不可能是正确的
// 凭据：写入路径不允许存这么长的用户名，bcrypt 只认 72 字节的密码）。
func TestLoginRejectsOverlongCredentialsBeforeCheckingThem(t *testing.T) {
	r := newAuthRouter(t)
	for _, c := range []struct{ name, username, password string }{
		{"用户名超长", strings.Repeat("u", 300), "admin"},
		{"密码超长", "admin", strings.Repeat("p", 300)},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg, err := postLogin(r, "198.51.100.21:5000", c.username, c.password)
			if err != nil {
				t.Fatal(err)
			}
			if msg.Success || !strings.Contains(msg.Msg, "过长") {
				t.Fatalf("超长输入应当直接回「过长」，实际: %+v", msg)
			}
		})
	}
	loginGuard.mu.Lock()
	n := len(loginGuard.entries)
	loginGuard.mu.Unlock()
	if n != 0 {
		t.Fatalf("超长输入被计入了限速（%d 条记录）", n)
	}
}

// captureLog 把面板日志临时写进文件（logger 在 InitLogger 那一刻捕获 os.Stderr）。
func captureLog(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "log-*.txt")
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

// 登录失败日志里的用户名是攻击者可控的任意内容，只记开头一截。
func TestLoginFailureLogTruncatesUsername(t *testing.T) {
	readLog := captureLog(t)
	r := newAuthRouter(t)
	long := "probe-" + strings.Repeat("x", 200)
	if _, err := postLogin(r, "198.51.100.22:5000", long, "wrong"); err != nil {
		t.Fatal(err)
	}
	logged := readLog()
	if !strings.Contains(logged, "login failed") {
		t.Fatalf("没有记下登录失败，日志:\n%s", logged)
	}
	if strings.Contains(logged, long) {
		t.Fatal("登录失败日志里记下了完整的超长用户名")
	}
	if !strings.Contains(logged, "probe-") {
		t.Fatalf("日志里应当保留用户名开头的一截，实际:\n%s", logged)
	}
}
