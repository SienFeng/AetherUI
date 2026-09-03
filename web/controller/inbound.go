package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"strconv"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/web/global"
	"a-ui/web/service"
	"a-ui/web/session"
)

type InboundController struct {
	inboundService   service.InboundService
	xrayService      service.XrayService
	onlineService    service.OnlineService
	accessLogService service.AccessLogService
	geoService       service.GeoService
}

func NewInboundController(g *gin.RouterGroup) *InboundController {
	a := &InboundController{}
	a.initRouter(g)
	a.startTask()
	return a
}

func (a *InboundController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/inbound")

	g.POST("/list", a.getInbounds)
	g.POST("/add", a.addInbound)
	g.POST("/del/:id", a.delInbound)
	g.POST("/update/:id", a.updateInbound)
	g.POST("/renew/:id", a.renewInbound)
	g.POST("/onlines/:id", a.getOnlines)
	g.POST("/kick/:id", a.kick)
	g.POST("/accessLogs/:id", a.getAccessLogs)
	g.POST("/provinces", a.getProvinces)
}

func (a *InboundController) startTask() {
	webServer := global.GetWebServer()
	c := webServer.GetCron()
	c.AddFunc("@every 10s", func() {
		if a.xrayService.IsNeedRestartAndSetFalse() {
			err := a.xrayService.RestartXray(false)
			if err != nil {
				logger.Error("restart xray failed:", err)
			}
		}
	})
}

func (a *InboundController) getInbounds(c *gin.Context) {
	user := session.GetLoginUser(c)
	inbounds, err := a.inboundService.GetInbounds(user.Id)
	if err != nil {
		jsonMsg(c, "获取", err)
		return
	}
	jsonObj(c, inbounds, nil)
}

func (a *InboundController) addInbound(c *gin.Context) {
	inbound := &model.Inbound{}
	err := c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, "添加", err)
		return
	}
	user := session.GetLoginUser(c)
	inbound.UserId = user.Id
	inbound.Enable = true
	inbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)
	err = a.inboundService.AddInbound(inbound)
	jsonMsg(c, "添加", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *InboundController) delInbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除", err)
		return
	}
	err = a.inboundService.DelInbound(id)
	jsonMsg(c, "删除", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *InboundController) updateInbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改", err)
		return
	}
	inbound := &model.Inbound{
		Id: id,
	}
	err = c.ShouldBind(inbound)
	if err != nil {
		jsonMsg(c, "修改", err)
		return
	}
	err = a.inboundService.UpdateInbound(inbound)
	jsonMsg(c, "修改", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *InboundController) renewInbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "续期", err)
		return
	}
	// 前端的 axios 拦截器用 Qs.stringify 把请求体转成了 urlencoded，
	// 所以这里必须用 form 标签而不是 json 标签。
	form := struct {
		Days       int   `form:"days"`
		ExpiryTime int64 `form:"expiryTime"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "续期", err)
		return
	}
	err = a.inboundService.RenewInbound(id, form.Days, form.ExpiryTime)
	jsonMsg(c, "续期", err)
	if err == nil {
		// 被停用的入站不在 xray 配置里，重新启用后必须让它重新出现。
		// enable 没变化时 RestartXray(false) 会用 Config.Equals 判定无需重启。
		a.xrayService.SetToNeedRestart()
	}
}

func (a *InboundController) getOnlines(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取在线明细", err)
		return
	}
	result, err := a.onlineService.GetOnlines(id)
	if err != nil {
		jsonMsg(c, "获取在线明细", err)
		return
	}
	jsonObj(c, result, nil)
}

func (a *InboundController) kick(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "踢下线", err)
		return
	}
	// 同续期接口：前端 axios 拦截器发的是 urlencoded，必须用 form 标签。
	form := struct {
		IP string `form:"ip"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "踢下线", err)
		return
	}
	killed, err := a.onlineService.Kick(id, form.IP)
	if err != nil {
		jsonMsg(c, "踢下线", err)
		return
	}
	jsonObj(c, killed, nil)
}

func (a *InboundController) getAccessLogs(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取访问日志", err)
		return
	}
	// 同其它接口：前端发的是 urlencoded，绑定标签必须是 form。
	form := struct {
		IP       string `form:"ip"`
		Key      string `form:"key"`
		Page     int    `form:"page"`
		PageSize int    `form:"pageSize"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取访问日志", err)
		return
	}
	result, err := a.accessLogService.GetAccessLogs(service.AccessLogQuery{
		InboundId: id,
		SourceIP:  form.IP,
		Keyword:   form.Key,
		Page:      form.Page,
		PageSize:  form.PageSize,
	})
	if err != nil {
		jsonMsg(c, "获取访问日志", err)
		return
	}
	jsonObj(c, result, nil)
}

func (a *InboundController) getProvinces(c *gin.Context) {
	jsonObj(c, a.geoService.Regions(), nil)
}
