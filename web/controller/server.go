package controller

import (
	"github.com/gin-gonic/gin"
	"time"
	"a-ui/logger"
	"a-ui/web/global"
	"a-ui/web/service"
)

type ServerController struct {
	BaseController

	serverService service.ServerService

	lastStatus        *service.Status
	lastGetStatusTime time.Time

	lastVersions        []string
	lastGetVersionsTime time.Time

	panelVersionService service.PanelVersionService

	lastRefreshPanelVersionTime time.Time
}

func NewServerController(g *gin.RouterGroup) *ServerController {
	a := &ServerController{
		lastGetStatusTime: time.Now(),
	}
	a.initRouter(g)
	a.startTask()
	return a
}

func (a *ServerController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/server")

	g.Use(a.checkLogin)
	g.POST("/status", a.status)
	g.POST("/getXrayVersion", a.getXrayVersion)
	g.POST("/installXray/:version", a.installXray)
	g.POST("/getNewX25519Cert", a.getNewX25519Cert)
	g.POST("/getNewMldsa65", a.getNewMldsa65)
	g.POST("/getNewEchCert", a.getNewEchCert)
	g.POST("/panelVersion", a.panelVersion)
	g.POST("/refreshPanelVersion", a.refreshPanelVersion)
	g.POST("/upgradePanel", a.upgradePanel)
	g.POST("/upgradeLog", a.upgradeLog)
}

func (a *ServerController) refreshStatus() {
	a.lastStatus = a.serverService.GetStatus(a.lastStatus)
}

func (a *ServerController) startTask() {
	webServer := global.GetWebServer()
	c := webServer.GetCron()
	c.AddFunc("@every 2s", func() {
		now := time.Now()
		if now.Sub(a.lastGetStatusTime) > time.Minute*3 {
			return
		}
		a.refreshStatus()
	})
}

func (a *ServerController) status(c *gin.Context) {
	a.lastGetStatusTime = time.Now()

	jsonObj(c, a.lastStatus, nil)
}

func (a *ServerController) getXrayVersion(c *gin.Context) {
	now := time.Now()
	if now.Sub(a.lastGetVersionsTime) <= time.Minute {
		jsonObj(c, a.lastVersions, nil)
		return
	}

	versions, err := a.serverService.GetXrayVersions()
	if err != nil {
		jsonMsg(c, "获取版本", err)
		return
	}

	a.lastVersions = versions
	a.lastGetVersionsTime = time.Now()

	jsonObj(c, versions, nil)
}

func (a *ServerController) installXray(c *gin.Context) {
	version := c.Param("version")
	err := a.serverService.UpdateXray(version)
	jsonMsg(c, "安装 xray", err)
}

func (a *ServerController) getNewX25519Cert(c *gin.Context) {
	cert, err := a.serverService.GetNewX25519Cert()
	if err != nil {
		jsonMsg(c, "生成 REALITY 密钥", err)
		return
	}
	jsonObj(c, cert, nil)
}

func (a *ServerController) getNewMldsa65(c *gin.Context) {
	keys, err := a.serverService.GetNewMldsa65()
	if err != nil {
		jsonMsg(c, "生成 ML-DSA-65 密钥", err)
		return
	}
	jsonObj(c, keys, nil)
}

func (a *ServerController) getNewEchCert(c *gin.Context) {
	keys, err := a.serverService.GetNewEchCert(c.PostForm("serverName"))
	if err != nil {
		jsonMsg(c, "生成 ECH 密钥", err)
		return
	}
	jsonObj(c, keys, nil)
}

func (a *ServerController) panelVersion(c *gin.Context) {
	jsonObj(c, a.panelVersionService.Get(), nil)
}

// refreshPanelVersion 强制重查。节流 1 分钟，防的是管理员连点刷新撞上
// GitHub 60 次/小时/IP 的未认证限速——照抄 getXrayVersion 的既有做法。
//
// 撞到节流时返回缓存而不是报错：管理员点刷新看到的是「版本信息」，
// 一个红色的「太频繁」提示既没用又像出了故障。
func (a *ServerController) refreshPanelVersion(c *gin.Context) {
	if time.Since(a.lastRefreshPanelVersionTime) <= time.Minute {
		jsonObj(c, a.panelVersionService.Get(), nil)
		return
	}
	a.lastRefreshPanelVersionTime = time.Now()
	if err := a.panelVersionService.Refresh(); err != nil {
		// 不返回错误：Refresh 失败时上一次成功的数据仍在缓存里，且失败
		// 原因已经写进 LastError，前端会显示。这里再报一次错会让界面同时
		// 弹红条又显示旧数据，反而看不懂。
		logger.Warning("手动检查面板版本失败:", err)
	}
	jsonObj(c, a.panelVersionService.Get(), nil)
}

func (a *ServerController) upgradePanel(c *gin.Context) {
	version := c.PostForm("version")
	if err := a.panelVersionService.Upgrade(version); err != nil {
		jsonMsg(c, "启动更新", err)
		return
	}
	// 更新已经交给独立的 systemd unit，面板马上会被 stop 掉。
	// 这条响应必须在那之前发出去。
	jsonMsgObj(c, "", gin.H{
		"started": true,
		"version": version,
	}, nil)
}

func (a *ServerController) upgradeLog(c *gin.Context) {
	lines, err := a.panelVersionService.UpgradeLog()
	if err != nil {
		jsonMsg(c, "读取更新日志", err)
		return
	}
	jsonObj(c, gin.H{"lines": lines}, nil)
}
