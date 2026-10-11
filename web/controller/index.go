package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"math"
	"net/http"
	"time"
	"a-ui/logger"
	"a-ui/web/service"
	"a-ui/web/session"
)

type LoginForm struct {
	Username string `json:"username" form:"username"`
	Password string `json:"password" form:"password"`
	// 两步验证码。未开启两步验证时前端不提交它。
	Code string `json:"code" form:"code"`
}

type IndexController struct {
	BaseController

	userService      service.UserService
	twoFactorService service.TwoFactorService
}

func NewIndexController(g *gin.RouterGroup) *IndexController {
	a := &IndexController{}
	a.initRouter(g)
	return a
}

func (a *IndexController) initRouter(g *gin.RouterGroup) {
	g.GET("/", a.index)
	g.POST("/login", a.login)
	g.GET("/logout", a.logout)
}

func (a *IndexController) index(c *gin.Context) {
	if session.IsLogin(c) {
		c.Redirect(http.StatusTemporaryRedirect, "aui/")
		return
	}
	// 是否开启两步验证随页面下发，登录页据此决定要不要显示验证码输入框。
	// 不做成「密码对了再问验证码」的两步流程：那会在第一步就告诉攻击者
	// 密码猜对了。读库失败时按已开启处理，多显示一个输入框没有害处。
	twoFactor, err := a.twoFactorService.Enabled()
	html(c, "login.html", "登录", gin.H{"two_factor": twoFactor || err != nil})
}

func (a *IndexController) login(c *gin.Context) {
	var form LoginForm
	err := c.ShouldBind(&form)
	if err != nil {
		pureJsonMsg(c, false, "数据格式错误")
		return
	}
	if form.Username == "" {
		pureJsonMsg(c, false, "请输入用户名")
		return
	}
	if form.Password == "" {
		pureJsonMsg(c, false, "请输入密码")
		return
	}

	ip := getRemoteIp(c)
	if remain, blocked := loginGuard.Blocked(ip); blocked {
		pureJsonMsg(c, false, lockedMsg(remain))
		return
	}

	twoFactor, err := a.twoFactorService.Enabled()
	if err != nil {
		// 读不出两步验证的状态时拒绝登录，而不是当成没开。
		logger.Warning("read two-factor state failed:", err)
		pureJsonMsg(c, false, "暂时无法登录，请稍后重试")
		return
	}

	// 密码与验证码都校验完再统一给结论，两种失败对外是同一句话：分开
	// 报错等于告诉攻击者密码已经猜对、只差验证码。
	user := a.userService.CheckUser(form.Username, form.Password)
	ok := user != nil
	if ok && twoFactor {
		ok = a.twoFactorService.Verify(form.Code)
	}
	if !ok {
		// 只记用户名与来源，绝不记提交上来的密码：登录失败最常见的原因
		// 是输错了一两个字符，日志里的「错误密码」离真密码往往只差一点。
		logger.Infof("login failed: username %q from %s", form.Username, ip)
		if loginGuard.Fail(ip) {
			logger.Warningf("login locked for %s after %d failures", ip, loginMaxFailures)
			pureJsonMsg(c, false, lockedMsg(loginLockDuration))
			return
		}
		if twoFactor {
			pureJsonMsg(c, false, "用户名、密码或验证码错误")
		} else {
			pureJsonMsg(c, false, "用户名或密码错误")
		}
		return
	}
	loginGuard.Reset(ip)

	err = session.SetLoginUser(c, user)
	logger.Info("user", user.Id, "login success from", ip)
	jsonMsg(c, "登录", err)
}

func lockedMsg(remain time.Duration) string {
	minutes := int(math.Ceil(remain.Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("登录失败次数过多，请 %d 分钟后再试", minutes)
}

func (a *IndexController) logout(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user != nil {
		logger.Info("user", user.Id, "logout")
	}
	session.ClearSession(c)
	c.Redirect(http.StatusTemporaryRedirect, c.GetString("base_path"))
}
