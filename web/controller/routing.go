package controller

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"a-ui/database/model"
	"a-ui/util/link"
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
	// SubscribeUrl 用指针，为的是区分「表单没提供这个字段」（nil）与
	// 「用户主动清空了订阅地址」（指向空串）。两者后果天差地别：前者是前端
	// 疏漏，此时必须沿用库里的原值；后者是取消订阅，此时清空订阅数据才对。
	// 用 string 绑定的话两种情况都是空串，一次「只改备注」的提交就会被
	// DomainGroupService.Update 判成「订阅地址变了」而清掉已拉取的域名——
	// 域名组变空、规则被 buildRule 跳过、流量静默走直连。
	SubscribeUrl *string `json:"subscribeUrl" form:"subscribeUrl"`
}

// 列表页只需要摘要。域名组挂上订阅后可能有几万条域名，
// 每次开页面把全量传一遍既没意义，前端渲染几万个 tag 还会卡死浏览器。
const domainGroupPreviewLimit = 5

// 编辑弹窗里订阅域名是只读展示，给个上限避免渲染卡顿。
const subscribedPreviewLimit = 200

type domainGroupSummary struct {
	Id              int      `json:"id"`
	Remark          string   `json:"remark"`
	Preview         []string `json:"preview"`
	EffectiveCount  int      `json:"effectiveCount"`
	ManualCount     int      `json:"manualCount"`
	SubscribedCount int      `json:"subscribedCount"`
	SubscribeUrl    string   `json:"subscribeUrl"`
	LastUpdatedAt   int64    `json:"lastUpdatedAt"`
	LastError       string   `json:"lastError"`
	LastSkipped     int      `json:"lastSkipped"`
}

type domainGroupDetail struct {
	Id                int      `json:"id"`
	Remark            string   `json:"remark"`
	Domains           string   `json:"domains"`
	SubscribeUrl      string   `json:"subscribeUrl"`
	SubscribedPreview []string `json:"subscribedPreview"`
	SubscribedCount   int      `json:"subscribedCount"`
	LastUpdatedAt     int64    `json:"lastUpdatedAt"`
	LastError         string   `json:"lastError"`
	LastSkipped       int      `json:"lastSkipped"`
}

// decodeGroupDomains 解出一个组的手工域名与订阅域名。数据损坏时当作空列表，
// 界面还能显示这个组的其余信息，管理员才有机会去修它。
func decodeGroupDomains(group *model.DomainGroup) (manual, subscribed []string) {
	manual, err := service.DecodeDomains(group.Domains)
	if err != nil {
		manual = nil
	}
	subscribed, err = service.DecodeSubscribedDomains(group.SubscribedDomains)
	if err != nil {
		subscribed = nil
	}
	return manual, subscribed
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
	dg.POST("/detail/:id", a.detailDomainGroup)
	dg.POST("/add", a.addDomainGroup)
	dg.POST("/update/:id", a.updateDomainGroup)
	dg.POST("/del/:id", a.delDomainGroup)
	dg.POST("/refresh/:id", a.refreshDomainGroup)

	ob := g.Group("/outbound")
	ob.POST("/list", a.listOutbounds)
	ob.POST("/add", a.addOutbound)
	ob.POST("/update/:id", a.updateOutbound)
	ob.POST("/del/:id", a.delOutbound)
	ob.POST("/parse", a.parseOutboundLink)

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
	summaries := make([]*domainGroupSummary, 0, len(groups))
	for _, group := range groups {
		manual, subscribed := decodeGroupDomains(group)
		merged := service.MergeDomains(manual, subscribed)
		preview := merged
		if len(preview) > domainGroupPreviewLimit {
			preview = preview[:domainGroupPreviewLimit]
		}
		summaries = append(summaries, &domainGroupSummary{
			Id:              group.Id,
			Remark:          group.Remark,
			Preview:         preview,
			EffectiveCount:  len(merged),
			ManualCount:     len(manual),
			SubscribedCount: len(subscribed),
			SubscribeUrl:    group.SubscribeUrl,
			LastUpdatedAt:   group.LastUpdatedAt,
			LastError:       group.LastError,
			LastSkipped:     group.LastSkipped,
		})
	}
	jsonObj(c, summaries, nil)
}

// detailDomainGroup 供编辑弹窗使用。list 只返回摘要，弹窗要展示的手工域名原文
// 与订阅域名预览没有别的来源。订阅域名全量任何时候都不出现在响应里。
func (a *RoutingController) detailDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	group, err := a.domainGroupService.Get(id)
	if err != nil {
		jsonMsg(c, "获取域名组", err)
		return
	}
	manual, subscribed := decodeGroupDomains(group)
	preview := subscribed
	if len(preview) > subscribedPreviewLimit {
		preview = preview[:subscribedPreviewLimit]
	}
	jsonObj(c, &domainGroupDetail{
		Id:                group.Id,
		Remark:            group.Remark,
		Domains:           strings.Join(manual, "\n"),
		SubscribeUrl:      group.SubscribeUrl,
		SubscribedPreview: preview,
		SubscribedCount:   len(subscribed),
		LastUpdatedAt:     group.LastUpdatedAt,
		LastError:         group.LastError,
		LastSkipped:       group.LastSkipped,
	}, nil)
}

func (a *RoutingController) refreshDomainGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "更新订阅", err)
		return
	}
	err = a.domainGroupService.Refresh(id)
	jsonMsg(c, "更新订阅", err)
	if err == nil {
		a.xrayService.SetToNeedRestart()
	}
}

// encodeDomainsFromForm 把 textarea 原文校验并转成入库格式。
// 允许为空：域名组可以只有订阅内容，手工域名一条不填。
// 合并后仍为空的组，其规则会被 buildRule 跳过并记 warning，防线不受影响。
func encodeDomainsFromForm(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "[]", nil
	}
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
	// 保存时只校验格式，不去拉取——一个慢地址会把这个 HTTP 请求挂满 30 秒。
	// 内容由管理员点「立即更新」或定时任务拉取。
	subscribeUrl := ""
	if form.SubscribeUrl != nil {
		subscribeUrl = *form.SubscribeUrl
	}
	if subscribeUrl != "" {
		if err := service.ValidateSubscribeURL(subscribeUrl); err != nil {
			jsonMsg(c, "添加域名组", err)
			return
		}
	}
	group := &model.DomainGroup{
		Remark: form.Remark, Domains: encoded, SubscribeUrl: subscribeUrl,
	}
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
	// form.SubscribeUrl 为 nil 表示这次提交根本没带订阅地址字段，此时必须
	// 沿用库里的原值。否则 Update 会看到「原值 -> 空串」，判定为订阅地址被
	// 改动而清空 SubscribedDomains——一次「只改备注」的提交就让这个域名组
	// 变空，引用它的分流规则被 buildRule 跳过，流量静默走直连。
	old, err := a.domainGroupService.Get(id)
	if err != nil {
		jsonMsg(c, "修改域名组", err)
		return
	}
	subscribeUrl := old.SubscribeUrl
	if form.SubscribeUrl != nil {
		subscribeUrl = *form.SubscribeUrl
	}
	if subscribeUrl != "" {
		if err := service.ValidateSubscribeURL(subscribeUrl); err != nil {
			jsonMsg(c, "修改域名组", err)
			return
		}
	}
	err = a.domainGroupService.Update(&model.DomainGroup{
		Id: id, Remark: form.Remark, Domains: encoded, SubscribeUrl: subscribeUrl,
	})
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

// parseOutboundLink 只解析不落库，供前端预览。
func (a *RoutingController) parseOutboundLink(c *gin.Context) {
	form := &addOutboundForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "解析链接", err)
		return
	}
	result, err := link.ParseLink(form.Link)
	if err != nil {
		jsonMsg(c, "解析链接", err)
		return
	}
	encoded, err := json.MarshalIndent(result.Outbound, "", "  ")
	if err != nil {
		jsonMsg(c, "解析链接", err)
		return
	}
	jsonObj(c, string(encoded), nil)
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
