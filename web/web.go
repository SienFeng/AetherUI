package web

import (
	"context"
	"crypto/tls"
	"embed"
	"github.com/BurntSushi/toml"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/robfig/cron/v3"
	"golang.org/x/text/language"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"a-ui/config"
	"a-ui/database"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/controller"
	"a-ui/web/job"
	"a-ui/web/network"
	"a-ui/web/service"
)

//go:embed assets/*
var assetsFS embed.FS

//go:embed html/*
var htmlFS embed.FS

//go:embed translation/*
var i18nFS embed.FS

var startTime = time.Now()

type wrapAssetsFS struct {
	embed.FS
}

func (f *wrapAssetsFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open("assets/" + name)
	if err != nil {
		return nil, err
	}
	return &wrapAssetsFile{
		File: file,
	}, nil
}

type wrapAssetsFile struct {
	fs.File
}

func (f *wrapAssetsFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return &wrapAssetsFileInfo{
		FileInfo: info,
	}, nil
}

type wrapAssetsFileInfo struct {
	fs.FileInfo
}

func (f *wrapAssetsFileInfo) ModTime() time.Time {
	return startTime
}

type Server struct {
	httpServer *http.Server
	listener   net.Listener

	index  *controller.IndexController
	server *controller.ServerController
	xui    *controller.XUIController

	xrayService    service.XrayService
	settingService service.SettingService
	inboundService service.InboundService
	ipdbService    service.IPDBService

	accessLogService service.AccessLogService

	trafficHistoryService service.TrafficHistoryService

	cron *cron.Cron

	ctx    context.Context
	cancel context.CancelFunc
}

func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *Server) getHtmlFiles() ([]string, error) {
	files := make([]string, 0)
	dir, _ := os.Getwd()
	err := fs.WalkDir(os.DirFS(dir), "web/html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func (s *Server) getHtmlTemplate(funcMap template.FuncMap) (*template.Template, error) {
	t := template.New("").Funcs(funcMap)
	err := fs.WalkDir(htmlFS, "html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			newT, err := t.ParseFS(htmlFS, path+"/*.html")
			if err != nil {
				// ignore
				return nil
			}
			t = newT
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Server) initRouter() (*gin.Engine, error) {
	if config.IsDebug() {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.DefaultWriter = io.Discard
		gin.DefaultErrorWriter = io.Discard
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.Default()

	secret, err := s.settingService.GetSecret()
	if err != nil {
		return nil, err
	}

	basePath, err := s.settingService.GetBasePath()
	if err != nil {
		return nil, err
	}
	assetsBasePath := basePath + "assets/"

	store := cookie.NewStore(secret)
	engine.Use(sessions.Sessions("session", store))
	engine.Use(func(c *gin.Context) {
		c.Set("base_path", basePath)
	})
	engine.Use(func(c *gin.Context) {
		uri := c.Request.RequestURI
		if !strings.HasPrefix(uri, assetsBasePath) {
			return
		}
		if config.IsDebug() {
			// 调试模式下静态资源直接从磁盘读，缓存必须关掉。
			//
			// 静态资源的 URL 带的是 ?<版本号>，而版本号只在发版时变。开发时改了
			// assets/js 但版本号不变，浏览器会命中这条一年期的强缓存继续用旧文件。
			// 后果不只是「改了没生效」：models.js 落后一版时，ObjectUtil.cloneProps
			// 会把服务端返回的新设置项直接丢掉，管理员一点保存就把那些项写成空值。
			c.Header("Cache-Control", "no-store")
			return
		}
		c.Header("Cache-Control", "max-age=31536000")
	})
	err = s.initI18n(engine)
	if err != nil {
		return nil, err
	}

	if config.IsDebug() {
		// for develop
		files, err := s.getHtmlFiles()
		if err != nil {
			return nil, err
		}
		engine.LoadHTMLFiles(files...)
		engine.StaticFS(basePath+"assets", http.FS(os.DirFS("web/assets")))
	} else {
		// for prod
		t, err := s.getHtmlTemplate(engine.FuncMap)
		if err != nil {
			return nil, err
		}
		engine.SetHTMLTemplate(t)
		engine.StaticFS(basePath+"assets", http.FS(&wrapAssetsFS{FS: assetsFS}))
	}

	g := engine.Group(basePath)

	s.index = controller.NewIndexController(g)
	s.server = controller.NewServerController(g)
	s.xui = controller.NewXUIController(g)

	return engine, nil
}

func (s *Server) initI18n(engine *gin.Engine) error {
	bundle := i18n.NewBundle(language.SimplifiedChinese)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	err := fs.WalkDir(i18nFS, "translation", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := i18nFS.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = bundle.ParseMessageFileBytes(data, path)
		return err
	})
	if err != nil {
		return err
	}

	findI18nParamNames := func(key string) []string {
		names := make([]string, 0)
		keyLen := len(key)
		for i := 0; i < keyLen-1; i++ {
			if key[i:i+2] == "{{" { // 判断开头 "{{"
				j := i + 2
				isFind := false
				for ; j < keyLen-1; j++ {
					if key[j:j+2] == "}}" { // 结尾 "}}"
						isFind = true
						break
					}
				}
				if isFind {
					names = append(names, key[i+3:j])
				}
			}
		}
		return names
	}

	var localizer *i18n.Localizer

	engine.FuncMap["i18n"] = func(key string, params ...string) (string, error) {
		names := findI18nParamNames(key)
		if len(names) != len(params) {
			return "", common.NewError("find names:", names, "---------- params:", params, "---------- num not equal")
		}
		templateData := map[string]interface{}{}
		for i := range names {
			templateData[names[i]] = params[i]
		}
		return localizer.Localize(&i18n.LocalizeConfig{
			MessageID:    key,
			TemplateData: templateData,
		})
	}

	engine.Use(func(c *gin.Context) {
		accept := c.GetHeader("Accept-Language")
		localizer = i18n.NewLocalizer(bundle, accept)
		c.Set("localizer", localizer)
		c.Next()
	})

	return nil
}

func (s *Server) startTask() {
	err := s.xrayService.RestartXray(true)
	if err != nil {
		logger.Warning("start xray failed:", err)
	}
	// 每 30 秒检查一次 xray 是否在运行
	s.cron.AddJob("@every 30s", job.NewCheckXrayRunningJob())

	go func() {
		time.Sleep(time.Second * 5)
		// 每 10 秒统计一次流量，首次启动延迟 5 秒，与重启 xray 的时间错开
		s.cron.AddJob("@every 10s", job.NewXrayTrafficJob())
	}()

	// 每 30 秒检查一次 inbound 流量超出和到期的情况
	s.cron.AddJob("@every 30s", job.NewCheckInboundJob())

	// 每 10 分钟检查一次域名组订阅是否到了更新时间
	s.cron.AddJob("@every 10m", job.NewSubscriptionJob())

	// 每 10 分钟自检一次 IP 归属地库是否到了配置的更新时刻（默认关闭，关闭时不发请求）
	s.cron.AddJob("@every 10m", job.NewIPDBUpdateJob())

	// 并发判定每秒跑一次。没有任何入站设置额度时它会直接返回，
	// 不做任何系统调用，所以常态下的开销只是一次极小的查库。
	s.cron.AddJob("@every 1s", job.NewConcurrencyJob())

	// 每 5 秒把 xray 写下的访问日志读进独立的库；关闭时直接返回
	s.cron.AddJob("@every 5s", job.NewAccessLogCollectJob())

	// 每小时按保留期清理访问日志
	s.cron.AddJob("@every 1h", job.NewAccessLogCleanupJob())

	// 每小时按各自的保留期清理用量历史
	s.cron.AddJob("@every 1h", job.NewTrafficCleanupJob())

	// 每 10 秒对齐一次端口限速规则；没人配限速时不碰 tc
	s.cron.AddJob("@every 10s", job.NewShapingJob())

	// 每 6 小时检查一次面板是否有新版本。
	s.cron.AddJob("@every 6h", job.NewPanelVersionJob())

	// cron.AddJob 的首次执行在一个完整周期之后，不做延迟触发的话新装的
	// 面板要等 6 小时才显示版本状态。延迟 10 秒是为了避开面板刚启动时
	// 和 xray 启动抢网络。
	go func() {
		time.Sleep(time.Second * 10)
		job.NewPanelVersionJob().Run()
	}()
}

func (s *Server) Start() (err error) {
	defer func() {
		if err != nil {
			s.Stop()
		}
	}()

	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	// robfig/cron 自身不做 panic 恢复，任何一个定时任务 panic 都会直接带走整个
	// 面板进程——实测过一次：XrayTrafficJob 解析流量时越界，面板启动 25 秒后静默
	// 退出，systemd 只报 status=2。Recover 把 panic 限制在单次任务执行内。
	s.cron = cron.New(cron.WithLocation(loc), cron.WithSeconds(), cron.WithChain(cron.Recover(cronLogger{})))
	s.cron.Start()

	engine, err := s.initRouter()
	if err != nil {
		return err
	}

	certFile, err := s.settingService.GetCertFile()
	if err != nil {
		return err
	}
	keyFile, err := s.settingService.GetKeyFile()
	if err != nil {
		return err
	}
	listen, err := s.settingService.GetListen()
	if err != nil {
		return err
	}
	port, err := s.settingService.GetPort()
	if err != nil {
		return err
	}
	listenAddr := net.JoinHostPort(listen, strconv.Itoa(port))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			listener.Close()
			return err
		}
		c := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		listener = network.NewAutoHttpsListener(listener)
		listener = tls.NewListener(listener, c)
	}
	if certFile != "" || keyFile != "" {
		logger.Info("web server run https on", listener.Addr())
	} else {
		logger.Info("web server run http on", listener.Addr())
	}
	s.listener = listener

	// IP 归属地库缺失或损坏不阻断面板启动：只是归属地显示与地区限制不可用，
	// 管理员可以在设置页点「更新 IP 库」补上。
	// 先把旧位置（安装目录 bin/ 下）的库搬到 /etc/<name>/：更新面板会 rm -rf
	// 整个安装目录，运行期数据留在那里每更新一次就丢一次。
	s.ipdbService.MigrateLegacyFiles()
	if err := s.ipdbService.Load(); err != nil {
		logger.Warning("load ip database failed, 归属地与地区限制将不可用:", err)
	}

	// 访问日志用独立的库。打不开只影响访问日志本身，不该让人登不上面板。
	if err := database.InitAccessLogDB(config.GetAccessLogDBPath()); err != nil {
		logger.Warning("open access log database failed, 访问日志将不可用:", err)
	} else if pruned, err := s.accessLogService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿访问日志失败:", err)
	} else if pruned > 0 {
		// 删除入站时若日志库恰好不可写，记录会留下来。启动时先扫一遍，
		// 把窗口从「最多一小时」缩到「最多到下次重启」。
		logger.Warning("清理了", pruned, "条已删除入站遗留的访问日志")
	}

	// 用量历史同样用独立的库。打不开只影响图表，不影响累计流量与限额判定。
	if err := database.InitTrafficDB(config.GetTrafficDBPath()); err != nil {
		logger.Warning("open traffic history database failed, 用量图表将不可用:", err)
	} else if pruned, err := s.trafficHistoryService.PruneOrphans(); err != nil {
		logger.Warning("清理孤儿用量数据失败:", err)
	} else if pruned > 0 {
		// 与访问日志同理：删除入站时若用量库恰好不可写，桶会留下来。启动时
		// 先扫一遍，把窗口从「最多一小时」缩到「最多到下次重启」。
		logger.Warning("清理了", pruned, "条已删除入站遗留的用量数据")
	}

	s.startTask()

	s.httpServer = &http.Server{
		Handler: engine,
	}

	go func() {
		s.httpServer.Serve(listener)
	}()

	return nil
}

func (s *Server) Stop() error {
	s.cancel()
	s.xrayService.StopXray()
	if s.cron != nil {
		s.cron.Stop()
	}
	var err1 error
	var err2 error
	if s.httpServer != nil {
		err1 = s.httpServer.Shutdown(s.ctx)
	}
	if s.listener != nil {
		err2 = s.listener.Close()
	}
	return common.Combine(err1, err2)
}

func (s *Server) GetCtx() context.Context {
	return s.ctx
}

// cronLogger 把 robfig/cron 的日志接进面板日志。cron.Recover 捕获到 panic 时
// 只会调用 Error，带上完整堆栈。
type cronLogger struct{}

func (cronLogger) Info(msg string, keysAndValues ...interface{}) {
	logger.Debug(append([]interface{}{"cron:", msg}, keysAndValues...)...)
}

func (cronLogger) Error(err error, msg string, keysAndValues ...interface{}) {
	logger.Error(append([]interface{}{"cron:", msg, err}, keysAndValues...)...)
}

func (s *Server) GetCron() *cron.Cron {
	return s.cron
}
