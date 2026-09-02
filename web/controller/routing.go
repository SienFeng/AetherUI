package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"a-ui/database/model"
	"a-ui/web/service"
)

type addOutboundForm struct {
	Remark string `json:"remark" form:"remark"`
	Link   string `json:"link" form:"link"`
	Config string `json:"config" form:"config"`
}

type domainGroupForm struct {
	Id      int    `json:"id" form:"id"`
	Remark  string `json:"remark" form:"remark"`
	Domains string `json:"domains" form:"domains"`
}

type RoutingController struct {
	domainGroupService service.DomainGroupService
	outboundService    service.OutboundNodeService
	ruleService        service.RoutingRuleService
	xrayService        service.XrayService
}

func NewRoutingController(g *gin.RouterGroup) *RoutingController {
	a := &RoutingController{}
	a.initRouter(g)
	return a
}

func (a *RoutingController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/routing")

	dg := g.Group("/domain-group")
	dg.POST("/list", a.listDomainGroups)
	dg.POST("/add", a.addDomainGroup)
	dg.POST("/update/:id", a.updateDomainGroup)
	dg.POST("/del/:id", a.delDomainGroup)

	ob := g.Group("/outbound")
	ob.POST("/list", a.listOutbounds)
	ob.POST("/add", a.addOutbound)
	ob.POST("/update/:id", a.updateOutbound)
	ob.POST("/del/:id", a.delOutbound)

	rl := g.Group("/rule")
	rl.POST("/list", a.listRules)
	rl.POST("/add", a.addRule)
	rl.POST("/update/:id", a.updateRule)
	rl.POST("/del/:id", a.delRule)
}

// 域名组

func (a *RoutingController) listDomainGroups(c *gin.Context) {
	groups, err := a.domainGroupService.GetAll()
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	jsonObj(c, groups, nil)
}

// encodeDomainsFromForm 把 textarea 原文校验并转成入库格式。
func encodeDomainsFromForm(raw string) (string, error) {
	list, err := service.ParseDomains(raw)
	if err != nil {
		return "", err
	}
	if err := service.ValidateDomains(list); err != nil {
		return "", err
	}
	return service.EncodeDomains(list)
}

func (a *RoutingController) addDomainGroup(c *gin.Context) {
	form := &domainGroupForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "添加域名组", err)
		return
	}
	encoded, err := encodeDomainsFromForm(form.Domains)
	if err != nil {
		jsonMsg(c, "添加域名组", err)
		return
	}
	group := &model.DomainGroup{Remark: form.Remark, Domains: encoded}
	err = a.domainGroupService.Add(group)
	jsonMsg(c, "添加域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	form := &domainGroupForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	encoded, err := encodeDomainsFromForm(form.Domains)
	if err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	err = a.domainGroupService.Update(&model.DomainGroup{Id: id, Remark: form.Remark, Domains: encoded})
	jsonMsg(c, "修改域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) delDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除域名组", err)
		return
	}
	err = a.domainGroupService.Del(id)
	jsonMsg(c, "删除域名组", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

// 出站节点

func (a *RoutingController) listOutbounds(c *gin.Context) {
	nodes, err := a.outboundService.GetAll()
	if err != nil {
		jsonMsg(c, "获取出站节点", err)
		return
	}
	jsonObj(c, nodes, nil)
}

func (a *RoutingController) addOutbound(c *gin.Context) {
	form := &addOutboundForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "添加出站节点", err)
		return
	}
	var err error
	if form.Link != "" {
		_, err = a.outboundService.AddFromLink(form.Link, form.Remark)
	} else {
		_, err = a.outboundService.AddFromJSON(form.Config, form.Remark)
	}
	jsonMsg(c, "添加出站节点", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateOutbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改出站节点", err)
		return
	}
	node := &model.OutboundNode{Id: id}
	if err := c.ShouldBind(node); err != nil {
		jsonMsg(c, "修改出站节点", err)
		return
	}
	err = a.outboundService.Update(node)
	jsonMsg(c, "修改出站节点", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) delOutbound(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除出站节点", err)
		return
	}
	err = a.outboundService.Del(id)
	jsonMsg(c, "删除出站节点", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

// 分流规则

func (a *RoutingController) listRules(c *gin.Context) {
	rules, err := a.ruleService.GetAll()
	if err != nil {
		jsonMsg(c, "获取分流规则", err)
		return
	}
	jsonObj(c, rules, nil)
}

func (a *RoutingController) addRule(c *gin.Context) {
	rule := &model.RoutingRule{}
	if err := c.ShouldBind(rule); err != nil {
		jsonMsg(c, "添加分流规则", err)
		return
	}
	err := a.ruleService.Add(rule)
	jsonMsg(c, "添加分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) updateRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	rule := &model.RoutingRule{Id: id}
	if err := c.ShouldBind(rule); err != nil {
		jsonMsg(c, "修改分流规则", err)
		return
	}
	err = a.ruleService.Update(rule)
	jsonMsg(c, "修改分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

func (a *RoutingController) delRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "删除分流规则", err)
		return
	}
	err = a.ruleService.Del(id)
	jsonMsg(c, "删除分流规则", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}
