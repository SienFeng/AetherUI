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

// page 渲染登录后的页面：带上「还在用默认密码」的标记（common_sider.html 的 defaultPasswordBanner）。
// 只在这里加、不加进 html()：登录页也走 html()，不能对未登录的人透露默认密码还能用。
func page(c *gin.Context, name string, title string, data gin.H) {
	if data == nil {
		data = gin.H{}
	}
	data["default_password"] = (&service.UserService{}).UsingDefaultPassword()
	html(c, name, title, data)
}

func (a *XUIController) index(c *gin.Context) {
	page(c, "index.html", "系统状态", nil)
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
	page(c, "inbounds.html", "入站列表", gin.H{
		"default_domain":    domain,
		"default_cert_file": certFile,
		"default_key_file":  keyFile,
	})
}

func (a *XUIController) setting(c *gin.Context) {
	page(c, "setting.html", "设置", nil)
}

func (a *XUIController) routing(c *gin.Context) {
	// 「补全流量识别」开着时，生成配置会替关了嗅探的入站打开嗅探，规则列表的「永远不会命中」
	// 警告要按同一口径判断（routing.html 的 sniffingOff，镜像 service.sniffingBlind）。
	settingService := service.SettingService{}
	page(c, "routing.html", "分流管理", gin.H{"force_sniffing": settingService.GetForceSniffing()})
}
