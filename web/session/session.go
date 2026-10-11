package session

import (
	"net"
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/crypto"
)

// 会话里只放这三样基本类型，绝不放 model.User：会话 cookie 只签名不加密
// （web.go 的 cookie.NewStore 只给了一把签名密钥），放进去的东西浏览器端
// 和链路上都读得到。改动前整个 User 连同密码都在里面。
const (
	loginUserId   = "LOGIN_USER_ID"
	loginUserName = "LOGIN_USER_NAME"
	loginUserFp   = "LOGIN_USER_FP"

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

func SetLoginUser(c *gin.Context, user *model.User) error {
	s := sessions.Default(c)
	s.Set(loginUserId, user.Id)
	s.Set(loginUserName, user.Username)
	s.Set(loginUserFp, crypto.SessionFingerprint(user.Username, user.Password))
	s.Options(sessions.Options{
		Path:     "/",
		MaxAge:   maxAgeSeconds,
		HttpOnly: true,
		Secure:   isSecureRequest(c),
		// Lax 而不是 Strict：Strict 下从书签、聊天软件里的链接点进面板时
		// 不带 cookie，每次都要重新登录。
		SameSite: http.SameSiteLaxMode,
	})
	c.Set(ctxUserKey, user)
	return s.Save()
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
	s := sessions.Default(c)
	s.Clear()
	s.Options(sessions.Options{
		Path:   "/",
		MaxAge: -1,
	})
	s.Save()
	c.Set(ctxUserKey, (*model.User)(nil))
}
