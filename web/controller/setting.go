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
	settingService service.SettingService
	userService    service.UserService
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
	if user.Username != form.OldUsername || user.Password != form.OldPassword {
		jsonMsg(c, "修改用户", errors.New("原用户名或原密码错误"))
		return
	}
	if form.NewUsername == "" || form.NewPassword == "" {
		jsonMsg(c, "修改用户", errors.New("新用户名和新密码不能为空"))
		return
	}
	err = a.userService.UpdateUser(user.Id, form.NewUsername, form.NewPassword)
	if err == nil {
		user.Username = form.NewUsername
		user.Password = form.NewPassword
		session.SetLoginUser(c, user)
	}
	jsonMsg(c, "修改用户", err)
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
