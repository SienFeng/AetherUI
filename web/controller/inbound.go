package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"strconv"
	"time"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/web/global"
	"a-ui/web/service"
	"a-ui/web/session"
)

type InboundController struct {
	inboundService        service.InboundService
	xrayService           service.XrayService
	onlineService         service.OnlineService
	accessLogService      service.AccessLogService
	geoService            service.GeoService
	trafficHistoryService service.TrafficHistoryService
	sharingService        service.SharingService
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
	g.POST("/onlineCounts", a.getOnlineCounts)
	g.POST("/kick/:id", a.kick)
	g.POST("/unban/:id", a.unban)
	g.POST("/accessLogs/:id", a.getAccessLogs)
	g.POST("/recentSources/:id", a.getRecentSources)
	g.POST("/traffic/history/:id", a.getTrafficHistory)
	g.POST("/traffic/overview", a.getTrafficOverview)
	g.POST("/sharing/summary", a.getSharingSummary)
	g.POST("/sharing/detail/:id", a.getSharingDetail)
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

// getOnlineCounts 一次性给出列表页每一行的在线设备数。
//
// 不让前端对每个入站各调一次 getOnlines：那边一次采样就更新了所有端口，
// 逐个请求只是把同一份快照切开来取。
func (a *InboundController) getOnlineCounts(c *gin.Context) {
	user := session.GetLoginUser(c)
	counts, err := a.onlineService.CountAll(user.Id)
	if err != nil {
		jsonMsg(c, "获取在线设备数", err)
		return
	}
	jsonObj(c, counts, nil)
}

func (a *InboundController) kick(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "踢下线", err)
		return
	}
	// 同续期接口：前端 axios 拦截器发的是 urlencoded，必须用 form 标签。
	// banSeconds：0 只断开当前连接，> 0 封禁该秒数，< 0 永久封禁。
	form := struct {
		IP         string `form:"ip"`
		BanSeconds int    `form:"banSeconds"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "踢下线", err)
		return
	}
	killed, err := a.onlineService.KickAndBan(id, form.IP, form.BanSeconds)
	if err != nil {
		jsonMsg(c, "踢下线", err)
		return
	}
	jsonObj(c, killed, nil)
}

func (a *InboundController) unban(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "解除封禁", err)
		return
	}
	form := struct {
		IP string `form:"ip"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "解除封禁", err)
		return
	}
	if err := a.onlineService.Unban(id, form.IP); err != nil {
		jsonMsg(c, "解除封禁", err)
		return
	}
	jsonMsg(c, "解除封禁", nil)
}

// getRecentSources 回答「谁来过」。在线明细是瞬时视图，人一断开那行就消失，
// 挂在行上的访问日志入口也跟着没了；日志一直在库里，缺的只是入口。
func (a *InboundController) getRecentSources(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取近期来源", err)
		return
	}
	list, err := a.accessLogService.RecentSources(id, 0)
	if err != nil {
		jsonMsg(c, "获取近期来源", err)
		return
	}
	jsonObj(c, list, nil)
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
		// 自动刷新走这条路径，跳过昂贵的总数统计，见 AccessLogQuery.SkipTotal。
		SkipTotal bool `form:"skipTotal"`
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
		SkipTotal: form.SkipTotal,
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

func (a *InboundController) getTrafficHistory(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	// 这里实际收到的是 JSON，不是字面意义上的 urlencoded：追踪本项目自带的
	// axios v0.18.0 可知 dispatchRequest 在扁平化 headers 之前先跑
	// transformRequest，那时 headers['Content-Type'] 还是 undefined，于是
	// setContentTypeIfUnset 打上 application/json;charset=utf-8，随后的
	// merge 里胜过 axios-init.js 给 post 设的 urlencoded。Gin 因此走的是
	// JSON 绑定，range 能绑上是因为 struct 字段没写 json tag 时
	// encoding/json 按大小写不敏感匹配字段名——这不是缺陷，与 getAccessLogs
	// 等既有接口完全一致。form tag 留着是为了与既有接口保持一致，且在真的
	// urlencoded 场景下同样能绑定；不改成 json tag（会改变绑定行为，不在
	// 发版前做）。
	form := struct {
		Range string `form:"range"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	result, err := a.trafficHistoryService.History(id, service.TrafficRange(form.Range), time.Now())
	if err != nil {
		jsonMsg(c, "获取用量历史", err)
		return
	}
	jsonObj(c, result, nil)
}

func (a *InboundController) getTrafficOverview(c *gin.Context) {
	form := struct {
		Range string `form:"range"`
		Top   int    `form:"top"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "获取用量总览", err)
		return
	}
	// 图例上超过几十条线已经没法看了，而这个数字来自请求体：前端的 bug 或
	// 一次手工请求传个大数，会让服务端白白拉出远超所需的系列。上界在这里
	// 钳住——controller 是不可信输入的边界，service 不该为此操心。
	if form.Top <= 0 || form.Top > 50 {
		form.Top = 12
	}
	result, err := a.trafficHistoryService.Overview(service.TrafficRange(form.Range), form.Top, time.Now())
	if err != nil {
		jsonMsg(c, "获取用量总览", err)
		return
	}
	jsonObj(c, result, nil)
}

// getSharingSummary 返回各入站的并存统计。
//
// 刻意不塞进入站列表主接口：不改已有接口契约，而且这样天然 fail open——
// 这个查询失败时列表照常渲染，只是没有并存标记。反过来把聚合塞进列表主
// 接口，一次慢查询就能让整个入站列表打不开。
func (a *InboundController) getSharingSummary(c *gin.Context) {
	result, err := a.sharingService.Summary(time.Now())
	if err != nil {
		jsonMsg(c, "获取共享检测统计", err)
		return
	}
	jsonObj(c, result, nil)
}

// getSharingDetail 返回某入站的共享检测明细。
func (a *InboundController) getSharingDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取共享检测明细", err)
		return
	}
	result, err := a.sharingService.Detail(id, time.Now())
	if err != nil {
		jsonMsg(c, "获取共享检测明细", err)
		return
	}
	jsonObj(c, result, nil)
}
