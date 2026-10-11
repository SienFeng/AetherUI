package controller

import (
	"errors"
	"github.com/gin-gonic/gin"
	"time"
	"a-ui/web/entity"
	"a-ui/web/service"
	"a-ui/web/session"
)

type updateUserForm struct {
	OldUsername string `json:"oldUsername" form:"oldUsername"`
	OldPassword string `json:"oldPassword" form:"oldPassword"`
	NewUsername string `json:"newUsername" form:"newUsername"`
	NewPassword string `json:"newPassword" form:"newPassword"`
}

type SettingController struct {
	settingService   service.SettingService
	userService      service.UserService
	twoFactorService service.TwoFactorService
	panelService   service.PanelService
	ipdbService    service.IPDBService
	shapingService service.ShapingService
}

func NewSettingController(g *gin.RouterGroup) *SettingController {
	a := &SettingController{}
	a.initRouter(g)
	return a
}

func (a *SettingController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/setting")

	g.POST("/all", a.getAllSetting)
	g.POST("/update", a.updateSetting)
	g.POST("/updateUser", a.updateUser)
	g.POST("/twoFactor/status", a.twoFactorStatus)
	g.POST("/twoFactor/setup", a.twoFactorSetup)
	g.POST("/twoFactor/enable", a.twoFactorEnable)
	g.POST("/twoFactor/disable", a.twoFactorDisable)
	g.POST("/restartPanel", a.restartPanel)
	g.POST("/ipdbStatus", a.ipdbStatus)
	g.POST("/updateIPDB", a.updateIPDB)
	g.POST("/shapingStatus", a.shapingStatus)
	g.POST("/clearShaping", a.clearShaping)
}

func (a *SettingController) getAllSetting(c *gin.Context) {
	allSetting, err := a.settingService.GetAllSetting()
	if err != nil {
		jsonMsg(c, "获取设置", err)
		return
	}
	jsonObj(c, allSetting, nil)
}

func (a *SettingController) updateSetting(c *gin.Context) {
	allSetting := &entity.AllSetting{}
	err := c.ShouldBind(allSetting)
	if err != nil {
		jsonMsg(c, "修改设置", err)
		return
	}
	err = a.settingService.UpdateAllSetting(allSetting)
	jsonMsg(c, "修改设置", err)
}

func (a *SettingController) updateUser(c *gin.Context) {
	form := &updateUserForm{}
	err := c.ShouldBind(form)
	if err != nil {
		jsonMsg(c, "修改用户", err)
		return
	}
	user := session.GetLoginUser(c)
	if user.Username != form.OldUsername || !a.userService.VerifyPassword(user.Id, form.OldPassword) {
		jsonMsg(c, "修改用户", errors.New("原用户名或原密码错误"))
		return
	}
	if form.NewUsername == "" || form.NewPassword == "" {
		jsonMsg(c, "修改用户", errors.New("新用户名和新密码不能为空"))
		return
	}
	updated, err := a.userService.UpdateUser(user.Id, form.NewUsername, form.NewPassword)
	if err == nil {
		// 改完密码，此前签发的所有会话（含这一个）都因指纹不符而失效。
		// 给当前这个会话重新签发，管理员不必在改完之后马上重新登录。
		err = session.SetLoginUser(c, updated)
	}
	jsonMsg(c, "修改用户", err)
}

type twoFactorForm struct {
	Password string `json:"password" form:"password"`
	Code     string `json:"code" form:"code"`
}

// twoFactorStatus 只回答「开没开」，绝不返回密钥。
func (a *SettingController) twoFactorStatus(c *gin.Context) {
	enabled, err := a.twoFactorService.Enabled()
	if err != nil {
		jsonMsg(c, "获取两步验证状态", err)
		return
	}
	jsonObj(c, gin.H{"enabled": enabled}, nil)
}

// twoFactorSetup 生成待确认的密钥。要求再输一次密码：密钥只在这一个响应里
// 出现一次，不能让一个被借用的已登录会话就把它拿走。
func (a *SettingController) twoFactorSetup(c *gin.Context) {
	form := &twoFactorForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "生成密钥", err)
		return
	}
	user := session.GetLoginUser(c)
	if !a.userService.VerifyPassword(user.Id, form.Password) {
		jsonMsg(c, "生成密钥", errors.New("当前密码错误"))
		return
	}
	secret, uri, err := a.twoFactorService.Begin(user.Username)
	if err != nil {
		jsonMsg(c, "生成密钥", err)
		return
	}
	jsonObj(c, gin.H{"secret": secret, "uri": uri}, nil)
}

func (a *SettingController) twoFactorEnable(c *gin.Context) {
	form := &twoFactorForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "开启两步验证", err)
		return
	}
	jsonMsg(c, "开启两步验证", a.twoFactorService.Enable(form.Code))
}

// twoFactorDisable 关闭两步验证，要求密码与一个当前有效的验证码。
// 验证器丢了的话走不通这里，救援入口是 `a-ui setting -disable2fa`。
func (a *SettingController) twoFactorDisable(c *gin.Context) {
	form := &twoFactorForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, "关闭两步验证", err)
		return
	}
	user := session.GetLoginUser(c)
	if !a.userService.VerifyPassword(user.Id, form.Password) || !a.twoFactorService.Verify(form.Code) {
		jsonMsg(c, "关闭两步验证", errors.New("当前密码或验证码错误"))
		return
	}
	jsonMsg(c, "关闭两步验证", a.twoFactorService.Disable())
}

func (a *SettingController) restartPanel(c *gin.Context) {
	err := a.panelService.RestartPanel(time.Second * 3)
	jsonMsg(c, "重启面板", err)
}

// ipdbStatus 返回各个 IP 归属地数据源的当前状态。某个源未加载时它的字段为零值，
// 由前端提示管理员去更新，而不是把接口做成失败——库缺失不影响面板其它功能。
func (a *SettingController) ipdbStatus(c *gin.Context) {
	status, err := a.ipdbService.Status()
	if err != nil {
		jsonMsg(c, "获取 IP 库状态", err)
		return
	}
	jsonObj(c, status, nil)
}

func (a *SettingController) updateIPDB(c *gin.Context) {
	// 前端发的是 urlencoded，绑定标签必须是 form。key 留空表示更新全部。
	form := struct {
		Key string `form:"key"`
	}{}
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "更新 IP 库", err)
		return
	}
	// upToDate 单独回传：管理员点了更新，界面得说清楚是真的换了新库，
	// 还是上游本来就没变。
	updated, upToDate, err := a.ipdbService.UpdateNow(form.Key)
	if err != nil {
		jsonMsg(c, "更新 IP 库", err)
		return
	}
	jsonObj(c, gin.H{"updated": updated, "upToDate": upToDate}, nil)
}

func (a *SettingController) shapingStatus(c *gin.Context) {
	jsonObj(c, a.shapingService.Status(), nil)
}

// clearShaping 是 §4.5 要求的手动撤销入口：tc 出问题时，管理员必须有一个
// 不依赖任何前置条件的清除手段。
func (a *SettingController) clearShaping(c *gin.Context) {
	jsonMsg(c, "清除限速规则", a.shapingService.Teardown())
}
