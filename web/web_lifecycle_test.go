package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"a-ui/database"
	"a-ui/web/global"
	"a-ui/web/service"
)

// lifecycleServer 准备一个用临时库、且「拉起 xray 与定时任务」那一步换成桩的
// Server：真实的那一步会拉起 xray（并占用它的 gRPC 端口）、几秒后去 GitHub 查版本，
// 测试里一样都不能碰。返回的 started 记录桩有没有被调用。
func lifecycleServer(t *testing.T, configure func(s *service.SettingService)) (*Server, *atomic.Bool) {
	t.Helper()
	keepGinGlobals(t)
	if err := database.InitDB(filepath.Join(t.TempDir(), "lifecycle.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if configure != nil {
		configure(&service.SettingService{})
	}
	oldInitial, oldMax := listenRetryInitial, listenRetryMax
	listenRetryInitial, listenRetryMax = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { listenRetryInitial, listenRetryMax = oldInitial, oldMax })

	s := NewServer()
	var started atomic.Bool
	s.startBackground = func() { started.Store(true) }
	old := global.GetWebServer()
	global.SetWebServer(s)
	t.Cleanup(func() {
		s.Stop()
		global.SetWebServer(old)
	})
	return s, &started
}

func occupyPort(t *testing.T) (port int, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			ln.Close()
		}
	}
	t.Cleanup(release)
	return ln.Addr().(*net.TCPAddr).Port, release
}

func freePort(t *testing.T) int {
	t.Helper()
	port, release := occupyPort(t)
	release()
	return port
}

func listenOn(port int) func(s *service.SettingService) {
	return func(s *service.SettingService) {
		s.SetListen("127.0.0.1")
		s.SetPort(port)
	}
}

// waitForPanel 轮询直到 url 回 200，或超时。
func waitForPanel(t *testing.T, client *http.Client, url string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// 面板端口被别的程序占着（或者监听地址不在本机网卡上）时，改动前 Start 直接返回
// 错误：xray 与定时任务都还没拉起来，main.go 记一行日志就以退出码 0 退出，而
// SIGHUP 重启那条路径上旧 Server 已经把 xray 停了——全部代理用户断网，没有任何
// 东西会把它重新拉起来。
//
// 现在 xray 与定时任务照常拉起，Web 服务在后台按间隔重试，端口一空出来面板就
// 自己恢复。
func TestStartKeepsXrayAndJobsRunningWhenListenFails(t *testing.T) {
	port, release := occupyPort(t)
	s, started := lifecycleServer(t, listenOn(port))

	if err := s.Start(); err != nil {
		t.Fatalf("监听失败不该让 Start 失败（那会连 xray 一起停掉），实际: %v", err)
	}
	if !started.Load() {
		t.Fatal("监听失败时 xray 与定时任务没有被拉起")
	}

	release()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	if !waitForPanel(t, http.DefaultClient, url, 5*time.Second) {
		t.Fatal("端口空出来之后面板没有自动恢复监听")
	}
}

// writeSelfSignedPair 在 dir 下写一对 tls.LoadX509KeyPair 能加载的自签证书。
func writeSelfSignedPair(t *testing.T, certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "panel.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 面板证书文件事后被删（或证书同步脚本出了岔子）时，改动前开机后 LoadX509KeyPair
// 失败、Start 返回错误，xray 同样起不来。现在 xray 照常运行；证书文件恢复之后
// 面板自己重新监听上，不会退回成不带证书的明文。
func TestStartKeepsXrayRunningWhenPanelCertIsMissing(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "panel.crt"), filepath.Join(dir, "panel.key")
	port := freePort(t)
	s, started := lifecycleServer(t, func(ss *service.SettingService) {
		listenOn(port)(ss)
		ss.SetCertFile(certFile)
		ss.SetKeyFile(keyFile)
	})

	if err := s.Start(); err != nil {
		t.Fatalf("证书文件缺失不该让 Start 失败，实际: %v", err)
	}
	if !started.Load() {
		t.Fatal("证书文件缺失时 xray 与定时任务没有被拉起")
	}
	// 没有证书时绝不能退回明文服务。用一次明文 HTTP 请求判断而不是裸 TCP 连接：
	// 后台重试每一轮都会短暂监听一下、加载证书失败后立刻关掉，裸连接恰好落在
	// 那个窗口里也能连上，但拿不到任何 HTTP 响应。
	plain := &http.Client{Timeout: time.Second}
	if resp, err := plain.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/"); err == nil {
		resp.Body.Close()
		t.Fatalf("证书缺失时面板退回成了明文 HTTP 服务（status %d）", resp.StatusCode)
	}

	writeSelfSignedPair(t, certFile, keyFile)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if !waitForPanel(t, client, "https://127.0.0.1:"+strconv.Itoa(port)+"/", 5*time.Second) {
		t.Fatal("证书文件恢复之后面板没有自动以 HTTPS 恢复监听")
	}
}

// 库里躺着一个 gin 注册不了的根路径（改动前设置页会照存含 : 或 * 的值）时，
// initRouter 里的 StaticFS 直接 panic，改动前那会带走整个进程。
func TestStartRecoversFromRouterPanic(t *testing.T) {
	s, started := lifecycleServer(t, func(ss *service.SettingService) {
		listenOn(freePort(t))(ss)
		ss.SetBasePath("/a:b/")
	})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("根路径非法时 Start panic 了: %v", r)
		}
	}()
	if err := s.Start(); err != nil {
		t.Fatalf("路由初始化失败不该让 Start 失败（那会连 xray 一起停掉），实际: %v", err)
	}
	if !started.Load() {
		t.Fatal("路由初始化失败时 xray 与定时任务没有被拉起")
	}
	// xray 的重启消费任务原本挂在 InboundController 上，路由没建起来它也就没了；
	// 这时必须另挂一个，否则 xray 崩了之后不会有人把它拉起来。恰好一个：panic
	// 发生在任何 controller 构造之前（拉起 xray 与定时任务那一步在这里是桩），
	// 多出来的就是与 InboundController 那个并存、互相抢标志的第二个消费者。
	if n := len(s.cron.Entries()); n != 1 {
		t.Fatalf("路由初始化失败后应当恰好补挂 1 个 xray 重启消费任务，实际 cron 里有 %d 个任务", n)
	}
}

// 读请求头必须有时限，否则一个只发半截请求头的连接能一直占着；但不能设
// ReadTimeout/WriteTimeout：导入分流配置要传最大 10 MB，更新 IP 库、切换 xray
// 版本这类请求要跑好几分钟。
func TestStartSetsHeaderAndIdleTimeoutsOnly(t *testing.T) {
	s, _ := lifecycleServer(t, listenOn(freePort(t)))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// 端口是空闲的，监听在 Start 里同步完成，这里读字段不会与后台重试并发。
	hs := s.httpServer
	if hs == nil {
		t.Fatal("Start 之后没有 http.Server")
	}
	if hs.ReadHeaderTimeout != panelReadHeaderTimeout || hs.IdleTimeout != panelIdleTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, IdleTimeout = %v，期望 %v / %v",
			hs.ReadHeaderTimeout, hs.IdleTimeout, panelReadHeaderTimeout, panelIdleTimeout)
	}
	if hs.ReadTimeout != 0 || hs.WriteTimeout != 0 {
		t.Fatalf("ReadTimeout = %v, WriteTimeout = %v，不能设：会掐断大文件导入与耗时几分钟的请求",
			hs.ReadTimeout, hs.WriteTimeout)
	}
}

// 改动前 Stop 先 cancel 掉 s.ctx 再拿它去 Shutdown：Shutdown 立刻返回
// context.Canceled，在途请求没人等；随后又去关一个 Shutdown 已经关掉的 listener。
// 所以 Stop 永远返回错误（main.go 每次 SIGHUP 都记一行 "stop server err"），
// 新 Server 也会在旧请求还没处理完时就起来。
func TestStopDrainsInFlightRequestsAndReturnsNil(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var finished atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		io.WriteString(w, "done")
	})
	s := NewServer()
	s.httpServer = &http.Server{Handler: handler}
	s.listener = ln
	go s.httpServer.Serve(ln)

	got := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- string(b)
	}()
	<-entered
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 返回错误: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Stop 在在途请求处理完之前就返回了")
	}
	if body := <-got; body != "done" {
		t.Fatalf("在途请求没有正常完成: %q", body)
	}
}

// Stop 要等正在跑的定时任务收尾（有上限）：否则 SIGHUP 重建出来的新 Server 会与
// 旧 Server 还没跑完的同一个任务并发执行。
func TestStopWaitsForRunningCronJob(t *testing.T) {
	s := NewServer()
	s.cron = cron.New(cron.WithSeconds())
	running := make(chan struct{})
	var finished atomic.Bool
	var once atomic.Bool
	s.cron.AddFunc("* * * * * *", func() {
		if !once.CompareAndSwap(false, true) {
			return
		}
		close(running)
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
	})
	s.cron.Start()
	select {
	case <-running:
	case <-time.After(3 * time.Second):
		t.Fatal("定时任务没有开始执行")
	}
	s.Stop()
	if !finished.Load() {
		t.Fatal("Stop 没有等正在跑的定时任务收尾")
	}
}

// Stop 之后后台的监听重试必须跟着停：SIGHUP 重建出来的新 Server 会自己去监听，
// 旧 Server 的重试要是还在，端口一空出来就会被它抢先占住。
func TestStopCancelsPendingListenRetry(t *testing.T) {
	port, release := occupyPort(t)
	s, _ := lifecycleServer(t, listenOn(port))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	release()
	time.Sleep(5 * listenRetryMax)
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		conn.Close()
		t.Fatal("Stop 之后旧 Server 的后台重试仍然占住了端口")
	}
}

// 只发半截请求头的连接会在 ReadHeaderTimeout 之后被断开，而不是一直占着。
func TestPanelServerDropsConnectionsThatNeverFinishHeaders(t *testing.T) {
	old := panelReadHeaderTimeout
	panelReadHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { panelReadHeaderTimeout = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(http.NotFoundHandler())
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: panel.example.com\r\n"); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadAll(conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("半截请求头的连接 5 秒后仍未被断开")
	}
}
