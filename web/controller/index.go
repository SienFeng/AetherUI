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

// loginMaxBodyBytes 是 /login 请求体的上限。登录表单只有用户名、密码、验证码
// 三个短字段，64 KB 绰绰有余；/login 不需要登录就能访问，不设上限的话一个
// 64 MiB 的用户名就能让面板多吃掉几百 MB 内存。
const loginMaxBodyBytes = 64 << 10

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
	// 先给请求体设上限再解析：gin 的 JSON 绑定没有任何上限，urlencoded 也要读满
	// 10 MB 才报错。超出时绑定报错，落进下面的「数据格式错误」。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, loginMaxBodyBytes)
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
	// 超长输入不可能是正确的凭据（写入路径不允许存这么长的用户名，bcrypt 只认
	// 72 字节的密码），在查库、跑 bcrypt、记日志之前就拒绝，也不计入限速。
	if len(form.Username) > service.MaxCredentialBytes || len(form.Password) > service.MaxCredentialBytes ||
		len(form.Code) > service.MaxCredentialBytes {
		pureJsonMsg(c, false, "用户名、密码或验证码过长")
		return
	}

	ip := getRemoteIp(c)
	attempt, remain, gate := loginGuard.Begin(ip)
	switch gate {
	case loginGateLocked:
		pureJsonMsg(c, false, lockedMsg(remain))
		return
	case loginGateBusy:
		// 同一来源同时在校验的请求已经够凑满一次锁定了，再放行就等于让并发绕过
		// 限速。不计为失败：真正的结论要等在途的那几个出结果。
		pureJsonMsg(c, false, "登录尝试过于频繁，请稍后再试")
		return
	}
	// 没走到 Fail/Succeed 的出口（读不出两步验证状态、中途 panic）都要把名额还回去，
	// 否则这个来源会永久少一个名额。
	defer attempt.Abort()

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
		logger.Infof("login failed: username %q from %s", truncateForLog(form.Username), ip)
		if attempt.Fail() {
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
	attempt.Succeed()

	err = session.SetLoginUser(c, user)
	logger.Info("user", user.Id, "login success from", ip)
	jsonMsg(c, "登录", err)
}

// logUsernameMaxRunes 是登录失败日志里用户名最多保留的字符数。
const logUsernameMaxRunes = 32

// truncateForLog 截短写进日志的用户名。登录失败时它是攻击者可控的任意内容，
// 只留开头一截足够辨认，再注明原长度。
func truncateForLog(s string) string {
	r := []rune(s)
	if len(r) <= logUsernameMaxRunes {
		return s
	}
	return fmt.Sprintf("%s…(共 %d 字节)", string(r[:logUsernameMaxRunes]), len(s))
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
