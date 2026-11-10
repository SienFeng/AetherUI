package session

import (
	"net"
	"net/http"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/crypto"
)

// 会话里只放这几样基本类型，绝不放 model.User：会话 cookie 只签名不加密
// （web.go 的 cookie.NewStore 只给了一把签名密钥），放进去的东西浏览器端
// 和链路上都读得到。改动前整个 User 连同密码都在里面。
const (
	loginUserId   = "LOGIN_USER_ID"
	loginUserName = "LOGIN_USER_NAME"
	loginUserFp   = "LOGIN_USER_FP"
	// loginCookiePath 记着这份会话签发时 cookie 的 Path，见 cookiePath。
	loginCookiePath = "LOGIN_COOKIE_PATH"

	// CookieName 是会话 cookie 的名字，web.go 挂会话中间件时用的也是它。
	CookieName = "session"

	// 同一次请求内缓存解析结果的 gin.Context key：checkLogin 与随后的
	// handler 各取一次，不该查两次库。
	ctxUserKey = "session_login_user"

	// 与改动前相同的有效期（gorilla/sessions 的默认值）。显式写出是因为
	// 下面设置 Options 会整体覆盖默认值。
	maxAgeSeconds = 86400 * 30
)

// isSecureRequest 判断这次请求在浏览器一侧是不是 HTTPS。
//
// 经 Caddy 转发时面板自己收到的是明文，要看 X-Forwarded-Proto；但只在
// 对端是本机回环时才信它——面板直接暴露时这个头是访客随便写的。
func isSecureRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && c.GetHeader("X-Forwarded-Proto") == "https"
}

// cookiePath 返回会话 cookie 的 Path：面板 url 根路径，**保留末尾斜杠**（/abc/）；
// 根路径是 / 或上下文里没有 base_path（测试）时为 /。
//
// 不能用 Path=/：安装向导的域名拓扑下，同一个域名上面板只占 /<根路径>/，其余
// 路径都由 Caddy 反代给第三方伪装站，Path=/ 的 cookie 会随访问伪装站的每个请求
// 一起发过去——等于把一张 30 天有效的面板登录凭证交给了第三方。
//
// 也不能去掉末尾斜杠（/abc）：Caddy 的匹配器是 /abc/*，不带斜杠的 /abc 落到
// 伪装站，而按 RFC 6265 Path=/abc 的 cookie 恰好会随 /abc 一起发出去——管理员
// 少敲一个斜杠就把凭证交出去了。带斜杠也不丢任何东西：面板的全部路由（登录页、
// /login、/logout、/server/*、/aui/*、静态资源）都在 /abc/ 之下，直连面板时
// /abc 由 gin 重定向到 /abc/，本来就不需要 cookie。
func cookiePath(c *gin.Context) string {
	p := c.GetString("base_path")
	if p == "" || p == "/" {
		return "/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// expireRootCookie 让浏览器删掉 Path=/ 的同名会话 cookie，即升级前签发的那一份。
// 只在会话 cookie 本身不是 Path=/ 时调用，否则会把刚下发的那一份也删掉。
func expireRootCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isSecureRequest(c), SameSite: http.SameSiteLaxMode,
	})
}

func SetLoginUser(c *gin.Context, user *model.User) error {
	path := cookiePath(c)
	s := sessions.Default(c)
	s.Set(loginUserId, user.Id)
	s.Set(loginUserName, user.Username)
	s.Set(loginUserFp, crypto.SessionFingerprint(user.Username, user.Password))
	s.Set(loginCookiePath, path)
	s.Options(sessions.Options{
		Path:     path,
		MaxAge:   maxAgeSeconds,
		HttpOnly: true,
		Secure:   isSecureRequest(c),
		// Lax 而不是 Strict：Strict 下从书签、聊天软件里的链接点进面板时
		// 不带 cookie，每次都要重新登录。
		SameSite: http.SameSiteLaxMode,
	})
	c.Set(ctxUserKey, user)
	if err := s.Save(); err != nil {
		return err
	}
	if path != "/" {
		expireRootCookie(c)
	}
	return nil
}

// GetLoginUser 返回当前登录的用户，未登录或会话已失效时返回 nil。
//
// 每次都回库核对指纹：用户名或密码改过之后，旧会话必须立刻失效，而会话
// 存在客户端 cookie 里，服务端没有别的办法吊销它。
//
// 升级前签发的旧格式 cookie 同样落进「未登录」，但路径不是「key 不存在」：
// 旧 cookie 里是 gob 编码的 model.User，而这个类型已不再 gob.Register，
// securecookie 解码直接失败，gin-contrib/sessions 记一行
// "[sessions] ERROR!" 后给出一个空会话。那行日志在升级后出现是预期的，
// 管理员重新登录、cookie 被覆盖后即消失。
func GetLoginUser(c *gin.Context) *model.User {
	if cached, ok := c.Get(ctxUserKey); ok {
		user, _ := cached.(*model.User)
		return user
	}
	user := loadLoginUser(c)
	c.Set(ctxUserKey, user)
	return user
}

func loadLoginUser(c *gin.Context) *model.User {
	s := sessions.Default(c)
	id, ok := s.Get(loginUserId).(int)
	if !ok {
		return nil
	}
	fp, ok := s.Get(loginUserFp).(string)
	if !ok {
		return nil
	}
	// 只认为当前根路径签发的会话。升级前签发的 Path=/ 会话没有这一项，一律作废
	// （升级后要重新登录一次）：它们早已随访问伪装站的请求发给过第三方。改过
	// 根路径之后旧会话同样作废——浏览器本来也不会再把它发到新路径上。
	if p, _ := s.Get(loginCookiePath).(string); p != cookiePath(c) {
		return nil
	}
	user := &model.User{}
	err := database.GetDB().Model(model.User{}).Where("id = ?", id).First(user).Error
	if err != nil {
		return nil
	}
	if crypto.SessionFingerprint(user.Username, user.Password) != fp {
		return nil
	}
	return user
}

func IsLogin(c *gin.Context) bool {
	return GetLoginUser(c) != nil
}

func ClearSession(c *gin.Context) {
	path := cookiePath(c)
	s := sessions.Default(c)
	s.Clear()
	s.Options(sessions.Options{
		Path:   path,
		MaxAge: -1,
	})
	s.Save()
	// 升级前签发的 Path=/ 那一份也要删：它与新路径那份是两个 cookie，只删一份的话
	// 浏览器还留着另一份。
	if path != "/" {
		expireRootCookie(c)
	}
	c.Set(ctxUserKey, (*model.User)(nil))
}
