package web

import (
	"context"
	"embed"
	"errors"
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
	"strings"
	"sync"
	"time"
	"a-ui/config"
	"a-ui/database"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/controller"
	"a-ui/web/job"
	"a-ui/web/service"
	"a-ui/web/session"
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
	// mu 保护 httpServer / listener / stopped：监听失败时它们由后台重试的
	// goroutine 写入（retryListen），Stop 在另一个 goroutine 里读。
	mu         sync.Mutex
	stopped    bool
	httpServer *http.Server
	listener   net.Listener
	// retrying 等后台的监听重试退出，见 Stop。
	retrying sync.WaitGroup

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

	// startBackground 拉起不依赖 Web 服务的那一半：IP 库、两个独立库、xray 与
	// 全部定时任务（默认是 startDataPlane）。做成字段只为测试：真实的那一份会
	// 拉起 xray、连它的 gRPC 端口、过几秒去 GitHub 查版本，测试里换成桩。
	startBackground func()
}

func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ctx:    ctx,
		cancel: cancel,
	}
	s.startBackground = s.startDataPlane
	return s
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

// useSessionAndCSRF 挂上会话与跨站请求防护。拆成独立函数是为了让测试能在
// 不启动整个面板（initRouter 还会注册定时任务、依赖全局 Server）的前提下，
// 验证这两层确实挂在了引擎上——中间件写得再对，没挂上也等于没有。
func useSessionAndCSRF(engine *gin.Engine, secret []byte) {
	store := cookie.NewStore(secret)
	engine.Use(sessions.Sessions(session.CookieName, store))
	engine.Use(csrfGuard())
}

func (s *Server) initRouter() (*gin.Engine, error) {
	if config.IsDebug() {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.DefaultWriter = io.Discard
		gin.DefaultErrorWriter = io.Discard
		gin.SetMode(gin.ReleaseMode)
	}

	engine := newEngine()

	secret, err := s.settingService.GetSecret()
	if err != nil {
		return nil, err
	}

	basePath, err := s.settingService.GetBasePath()
	if err != nil {
		return nil, err
	}
	assetsBasePath := basePath + "assets/"

	useSessionAndCSRF(engine, secret)
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

	// 下面几条延迟任务都等在 s.ctx 上：SIGHUP 重建 Server 时旧 Server 的 Stop 会
	// 取消它，延迟还没到的就不再执行，不会与新 Server 的同一个任务各跑一遍。
	go func() {
		if !s.waitOrStop(time.Second * 5) {
			return
		}
		// 每 10 秒统计一次流量，首次启动延迟 5 秒，与重启 xray 的时间错开
		s.cron.AddJob("@every 10s", job.NewXrayTrafficJob())
	}()

	// 每 30 秒检查一次 inbound 流量超出和到期的情况
	s.cron.AddJob("@every 30s", job.NewCheckInboundJob())

	// 每 10 分钟检查一次域名组订阅是否到了更新时间
	s.cron.AddJob("@every 10m", job.NewSubscriptionJob())

	// 每 10 分钟自检一次 IP 归属地库是否到了配置的更新时刻（默认关闭，关闭时不发请求）
	s.cron.AddJob("@every 10m", job.NewIPDBUpdateJob())

	// 启动后补一次「这台机器从未拉取过」的 IP 库。新装的面板只带 ip2region
	// 的种子库、不带纯真库，光靠上面那个到点判断，凌晨装机的管理员要等到
	// 当天的更新时刻才拿到完整的库。延迟 30 秒是为了避开面板启动、xray 启动
	// 与这两个源合计约 60 MB 的下载互相抢网络。关闭自动更新时它一个字节都
	// 不下，存量部署因此完全无感。
	go func() {
		if !s.waitOrStop(time.Second * 30) {
			return
		}
		job.NewIPDBUpdateJob().RunInitial()
	}()

	// 并发判定每秒跑一次。没有任何入站设置额度时它会直接返回，
	// 不做任何系统调用，所以常态下的开销只是一次极小的查库。
	s.cron.AddJob("@every 1s", job.NewConcurrencyJob())

	// 每 30 秒采一次活跃来源 IP 供共享检测使用。刻意不复用并发判定那次
	// 采样：Enforce 在无人设并发额度时一次系统调用都不做，挂在它后面
	// 会让检测在默认配置下静默失效。间隔必须与 service.sharingSampleStep
	// 一致，累加器按它折算活跃时长。
	s.cron.AddJob("@every 30s", job.NewSharingSampleJob())

	// 独立的连接表采样，周期必须不大于 onlineMaxSampleGap（见
	// service.OnlineSampleInterval）。用 cron.Every 直接引用那个常量，不写
	// 字符串，两边就不会各改各的。
	s.cron.Schedule(cron.Every(service.OnlineSampleInterval), job.NewOnlineSampleJob())

	// 订阅内核的连接销毁通知，补上连接表看不到的短连接与尾段字节。进程内
	// 只起一个，SIGHUP 重建 Server 时不会重复订阅。
	new(service.OnlineService).StartCloseWatcher()

	// 每 5 秒把 xray 写下的访问日志读进独立的库；关闭时直接返回
	// 风险评分：长期行为分析，不是实时防火墙，10 分钟绰绰有余。
	s.cron.AddJob("@every 10m", job.NewSharingRiskJob())

	s.cron.AddJob("@every 5s", job.NewAccessLogCollectJob())

	// 自动出差放行：只对开了「自动出差」的地区受限入站生效，见 AutoTravelService。
	s.cron.AddJob("@every 1m", job.NewAutoTravelJob())
	// 自动设置地区限制：总开关在面板设置里，默认关闭。
	s.cron.AddJob("@every 10m", job.NewAutoRegionJob())

	// 路由规则后台审计：代次变化或满 10 分钟才真正验证；异常扫描每分钟一次。
	s.cron.AddJob("@every 30s", job.NewRouteAuditJob())
	s.cron.AddJob("@every 1m", job.NewRouteAnomalyJob())

	// 每小时按保留期清理访问日志
	s.cron.AddJob("@every 1h", job.NewAccessLogCleanupJob())

	// 域名统计聚合。见 DomainStatJob 里关于周期的说明。
	s.cron.AddJob("@every 10m", job.NewDomainStatJob())

	// 计量池重算。见 MeterPoolJob 里关于周期与「为什么不做延迟首次触发」的说明。
	s.cron.AddJob("@every 1h", job.NewMeterPoolJob())

	// 每小时按各自的保留期清理用量历史
	s.cron.AddJob("@every 1h", job.NewTrafficCleanupJob())

	// 每小时检查一次有没有入站到了流量重置日；没人设重置日时只做一次带
	// where 的查询就返回
	s.cron.AddJob("@every 1h", job.NewTrafficResetJob())

	// 每 10 秒对齐一次端口限速规则；没人配限速时不碰 tc
	s.cron.AddJob("@every 10s", job.NewShapingJob())
	// 流量暴涨自动限速：10 分钟一轮发 / 撤限速单，ShapingJob 负责把限速下发到内核。
	s.cron.AddJob("@every 10m", job.NewSpikeThrottleJob())

	// 每 6 小时检查一次面板是否有新版本。
	s.cron.AddJob("@every 6h", job.NewPanelVersionJob())

	// cron.AddJob 的首次执行在一个完整周期之后，不做延迟触发的话新装的
	// 面板要等 6 小时才显示版本状态。延迟 10 秒是为了避开面板刚启动时
	// 和 xray 启动抢网络。
	go func() {
		if !s.waitOrStop(time.Second * 10) {
			return
		}
		job.NewPanelVersionJob().Run()
	}()

	// 同理，风险评分的首轮也要提前触发：Snapshot 表为空时界面显示的是
	// 「学习中」，等满 10 分钟纯属没必要。15 秒是为了避开面板刚启动时和
	// xray 抢资源。
	//
	// RunInitial 内部自带 common.Recover，**不要去掉**：这条 goroutine
	// 不经过 cron，上面那层 cron.Recover 覆盖不到它。
	go func() {
		if !s.waitOrStop(time.Second * 15) {
			return
		}
		job.NewSharingRiskJob().RunInitial()
	}()
}

// Start 拉起面板：定时任务的 cron、Web 路由、xray 与全部定时任务，最后是 Web 服务。
//
// xray 与定时任务不依赖 Web 服务，不论路由建没建起来、端口能不能监听都要拉起来。
// 改动前它们排在 net.Listen 与加载面板证书之后，任何一步失败 Start 都返回错误：
// 首次启动时进程记一行日志就以退出码 0 退出；SIGHUP 重启时旧 Server 已经把 xray
// 停了——两条路径都是全部代理用户断网，而 a-ui.service 没配 Restart=，没有任何
// 东西会把它重新拉起来。端口被占、监听地址不在本机网卡上、面板证书文件被删、
// 库里躺着一个 gin 注册不了的根路径，都能触发。
//
// 现在 Web 服务的问题只记错误日志（带救援命令）：监听失败在后台按间隔重试，条件
// 恢复（端口空出来、证书文件补回来）之后面板自己恢复；绝不退回成「不带根路径/
// 证书地监听 0.0.0.0」这种更危险的状态。路由初始化失败（多半是根路径非法）重试
// 无用，要等管理员用命令行改掉再重启。所以 Start 现在总是返回 nil。
func (s *Server) Start() error {
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		// 读不出时区说明连设置表都读不了。照样往下走：能拉起多少拉起多少，
		// 也比整个进程退出、全员断网强。
		logger.Error("读取面板时区失败，定时任务改按本机时区运行:", err)
		loc = time.Local
	}
	// robfig/cron 自身不做 panic 恢复，任何一个定时任务 panic 都会直接带走整个
	// 面板进程——实测过一次：XrayTrafficJob 解析流量时越界，面板启动 25 秒后静默
	// 退出，systemd 只报 status=2。Recover 把 panic 限制在单次任务执行内。
	s.cron = cron.New(cron.WithLocation(loc), cron.WithSeconds(), cron.WithChain(cron.Recover(cronLogger{})))
	s.cron.Start()

	engine, routerErr := s.initRouterSafely()

	s.startBackground()

	if routerErr != nil {
		logger.Error("面板 Web 服务初始化失败，面板暂时无法访问；xray 与定时任务照常运行，代理用户不受影响:", routerErr, routerFailedHint)
		s.consumeXrayRestartFlag()
		return nil
	}
	s.serveOrRetry(engine)
	return nil
}

// startDataPlane 拉起 IP 库、访问日志库、用量库、xray 与全部定时任务。
func (s *Server) startDataPlane() {
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
}

// Stop 依次停掉 Web 服务、定时任务与 xray。SIGHUP 重启时紧接着就是新 Server 的
// Start，所以每一步都要真的收尾再往下走，并且各自有上限，不让一个卡住的请求或
// 任务把重启拖住。
func (s *Server) Stop() error {
	s.mu.Lock()
	s.stopped = true
	srv := s.httpServer
	s.mu.Unlock()

	// 先取消 ctx：后台的监听重试与 startTask 里那几条延迟任务都等在它上面。
	// Shutdown 不能再用它——改动前正是先 cancel 再 Shutdown(s.ctx)，Shutdown
	// 立刻返回 context.Canceled，在途请求没人等，随后又去关一个 Shutdown 已经
	// 关掉的 listener，Stop 因此永远返回错误。
	s.cancel()
	// 等后台的监听重试真正退出再往下走：它可能正打开着一个刚监听上的端口，
	// 新 Server 紧接着就要监听同一个端口。重试的每一步（读设置、listen）都很快，
	// 收到取消后立即返回。
	s.retrying.Wait()

	var err error
	if srv != nil {
		// Shutdown 会关掉它正在 Serve 的 listener，不必再单独 Close。
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		err = srv.Shutdown(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			logger.Warning("仍有请求在", shutdownTimeout, "内没有处理完，强制关闭剩余连接")
			err = srv.Close()
		}
	}

	// 等正在跑的定时任务收尾，再停 xray：流量统计这类任务正在和 xray 通信，
	// 先停 xray 会让它们这一轮白跑；也免得新 Server 的同一个任务与它并发执行。
	if s.cron != nil {
		select {
		case <-s.cron.Stop().Done():
		case <-time.After(cronDrainTimeout):
			logger.Warning("仍有定时任务在", cronDrainTimeout, "内没有跑完，不再等待")
		}
	}
	s.xrayService.StopXray()
	return err
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
