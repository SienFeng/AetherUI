package controller

import (
	"github.com/gin-gonic/gin"
	"a-ui/logger"
	"a-ui/web/service"
)

type XUIController struct {
	BaseController

	inboundController *InboundController
	settingController *SettingController
	routingController *RoutingController
}

func NewXUIController(g *gin.RouterGroup) *XUIController {
	a := &XUIController{}
	a.initRouter(g)
	return a
}

func (a *XUIController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/aui")
	g.Use(a.checkLogin)

	g.GET("/", a.index)
	g.GET("/inbounds", a.inbounds)
	g.GET("/routing", a.routing)
	g.GET("/setting", a.setting)

	a.inboundController = NewInboundController(g)
	a.settingController = NewSettingController(g)
	a.routingController = NewRoutingController(g)
}

func (a *XUIController) index(c *gin.Context) {
	html(c, "index.html", "系统状态", nil)
}

func (a *XUIController) inbounds(c *gin.Context) {
	// 新建入站时自动填充域名与证书路径，省掉逐个手填——手填错的代价是
	// 整份 xray 配置加载失败，机器上全部用户一起断网。
	// 取不到就当没有默认值，不影响页面渲染。
	settingService := service.SettingService{}
	domain, err := settingService.GetDefaultDomain()
	if err != nil {
		logger.Warning("get default domain failed:", err)
	}
	certFile, err := settingService.GetDefaultCertFile()
	if err != nil {
		logger.Warning("get default cert file failed:", err)
	}
	keyFile, err := settingService.GetDefaultKeyFile()
	if err != nil {
		logger.Warning("get default key file failed:", err)
	}
	html(c, "inbounds.html", "入站列表", gin.H{
		"default_domain":    domain,
		"default_cert_file": certFile,
		"default_key_file":  keyFile,
	})
}

func (a *XUIController) setting(c *gin.Context) {
	html(c, "setting.html", "设置", nil)
}

func (a *XUIController) routing(c *gin.Context) {
	html(c, "routing.html", "分流管理", nil)
}
