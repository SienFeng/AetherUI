package web

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"a-ui/logger"
	"a-ui/web/network"
)

// 面板 Web 服务生命周期用到的几个时长。做成变量只为测试能把它们调短。
var (
	// panelReadHeaderTimeout 是读完请求头的时限：挡住只发半截请求头、一直占着
	// 连接的慢速攻击。
	panelReadHeaderTimeout = 10 * time.Second
	// panelIdleTimeout 是 keep-alive 空闲连接的保留时长。
	panelIdleTimeout = 2 * time.Minute

	// listenRetryInitial / listenRetryMax：监听失败后重试的初始间隔与上限。
	listenRetryInitial = 5 * time.Second
	listenRetryMax     = time.Minute

	// shutdownTimeout 是 Stop 等在途请求处理完的时限，cronDrainTimeout 是等正在
	// 跑的定时任务收尾的时限。
	shutdownTimeout  = 5 * time.Second
	cronDrainTimeout = 5 * time.Second
)

// listenRetryRemindEvery：同一个监听错误每重试这么多次再记一次日志（间隔到上限
// 之后约半小时一次），免得一夜刷出几百行。
const listenRetryRemindEvery = 30

// newHTTPServer 给面板的 Web 服务设上时限。
//
// 只设读请求头与空闲连接两项：没有 ReadHeaderTimeout 时，一个只发半截请求头的
// 连接能一直占着。刻意不设 ReadTimeout/WriteTimeout——它们管的是整个请求体与
// 整个响应：导入分流配置要传最大 10 MB，慢速链路上读一分钟很正常；更新 IP 库、
// 切换 xray 版本这类请求要跑好几分钟才响应。未登录的 /login 请求体另有 64 KB 的
// 上限（controller.loginMaxBodyBytes）。
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: panelReadHeaderTimeout,
		IdleTimeout:       panelIdleTimeout,
	}
}

// initRouterSafely 调 initRouter，并把其中的 panic 转成错误。
//
// gin 注册路由时遇到非法路径直接 panic（根路径含 : 或 * 时 StaticFS 就会），
// 改动前这会带走整个进程，连同 xray 一起。
func (s *Server) initRouterSafely() (engine *gin.Engine, err error) {
	defer func() {
		if r := recover(); r != nil {
			engine, err = nil, fmt.Errorf("注册路由时 panic: %v", r)
		}
	}()
	return s.initRouter()
}

// consumeXrayRestartFlag 在路由没建起来时补挂 xray 的重启消费任务。
//
// 消费 xrayService 重启标志的那个 10 秒任务挂在 InboundController 上，路由没建
// 起来它也就不存在；而 CheckXrayRunningJob 发现 xray 没在跑时只会置标志——没有
// 人消费的话，xray 一旦崩溃就再也不会被拉起来。
func (s *Server) consumeXrayRestartFlag() {
	s.cron.AddFunc("@every 10s", func() {
		if s.xrayService.IsNeedRestartAndSetFalse() {
			if err := s.xrayService.RestartXray(false); err != nil {
				logger.Error("restart xray failed:", err)
			}
		}
	})
}

// openPanelListener 按库里当前的设置打开面板的监听（配了面板证书时套上 TLS）。
// 每次都重新读设置、重新加载证书文件：后台重试时，管理员用命令行改掉的端口、
// 补回来的证书文件都能直接生效。返回的地址只用于日志。
func (s *Server) openPanelListener() (net.Listener, string, error) {
	certFile, err := s.settingService.GetCertFile()
	if err != nil {
		return nil, "", err
	}
	keyFile, err := s.settingService.GetKeyFile()
	if err != nil {
		return nil, "", err
	}
	listen, err := s.settingService.GetListen()
	if err != nil {
		return nil, "", err
	}
	port, err := s.settingService.GetPort()
	if err != nil {
		return nil, "", err
	}
	addr := net.JoinHostPort(listen, strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, addr, err
	}
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			listener.Close()
			return nil, addr, err
		}
		c := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		listener = network.NewAutoHttpsListener(listener)
		listener = tls.NewListener(listener, c)
		logger.Info("web server run https on", listener.Addr())
	} else {
		logger.Info("web server run http on", listener.Addr())
	}
	return listener, addr, nil
}

// serveOrRetry 打开监听并开始服务；打不开时记错误日志，转入后台重试。
func (s *Server) serveOrRetry(handler http.Handler) {
	listener, addr, err := s.openPanelListener()
	if err == nil {
		s.serve(handler, listener)
		return
	}
	logger.Error(listenFailedMessage(addr, err))
	s.retrying.Add(1)
	go func() {
		defer s.retrying.Done()
		s.retryListen(handler, err)
	}()
}

// serve 在 listener 上开始服务。Server 已经 Stop 时关掉 listener 并返回 false：
// 后台重试可能恰好在 Stop 的同时打开了监听。
func (s *Server) serve(handler http.Handler, listener net.Listener) bool {
	srv := newHTTPServer(handler)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		listener.Close()
		return false
	}
	s.httpServer, s.listener = srv, listener
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("面板 Web 服务异常退出:", err)
		}
	}()
	return true
}

// retryListen 在后台按间隔（指数增长到 listenRetryMax）重试监听，直到成功或
// Server 被 Stop。
func (s *Server) retryListen(handler http.Handler, firstErr error) {
	delay := listenRetryInitial
	last := firstErr.Error()
	for attempt := 1; ; attempt++ {
		if !s.waitOrStop(delay) {
			return
		}
		listener, addr, err := s.openPanelListener()
		if err == nil {
			if s.serve(handler, listener) {
				logger.Info("面板 Web 服务在第", attempt, "次重试后恢复监听", addr)
			}
			return
		}
		if msg := err.Error(); msg != last || attempt%listenRetryRemindEvery == 0 {
			logger.Error(listenFailedMessage(addr, err))
			last = msg
		}
		if delay *= 2; delay > listenRetryMax {
			delay = listenRetryMax
		}
	}
}

// routerFailedHint 是路由初始化失败时日志里的救援提示。
//
// 救援命令写二进制的完整路径：管理员 SSH 上去敲的 a-ui 是 /usr/bin/a-ui 那个管理
// 菜单脚本（a-ui.sh），它没有 setting 子命令；a-ui restart 则是菜单脚本自己的
// 子命令，照常写短名。listenFailedMessage 同理。
const routerFailedHint = "——若是面板 url 根路径不合法（含 : * 等字符），请在服务器上执行 /usr/local/a-ui/a-ui setting -basepath /新路径/ 后再执行 a-ui restart"

// listenFailedMessage 拼监听失败的日志：说清楚代理用户不受影响、面板会自己重试，
// 以及几种常见原因各自的救援命令。
func listenFailedMessage(addr string, err error) string {
	return fmt.Sprintf("面板 Web 服务无法监听 %v: %v。xray 与定时任务照常运行，代理用户不受影响；面板会在后台自动重试。"+
		"端口被占用：/usr/local/a-ui/a-ui setting -port <新端口>；监听地址不在本机网卡上：/usr/local/a-ui/a-ui setting -listen \"\"；"+
		"面板证书文件丢失：把证书文件放回原处即可自动恢复。改完设置后执行 a-ui restart", addr, err)
}

// waitOrStop 等 d；Server 在此期间被 Stop 时立即返回 false。
func (s *Server) waitOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}
