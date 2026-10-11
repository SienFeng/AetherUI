package controller

import (
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/totp"
	"a-ui/web/entity"
	"a-ui/web/service"
)

// newAuthRouter 搭一个带会话的最小路由：登录接口 + 需要登录的设置接口。
// 登录页的 GET / 需要模板，这里不注册。每次调用都换一个全新的限速器，
// 测试之间互不影响。
func newAuthRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "auth.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	old := loginGuard
	loginGuard = newLoginLimiter()
	t.Cleanup(func() { loginGuard = old })

	r := gin.New()
	r.Use(sessions.Sessions("session", cookie.NewStore([]byte("test-secret"))))
	index := &IndexController{}
	r.POST("/login", index.login)
	g := r.Group("/aui")
	g.Use((&BaseController{}).checkLogin)
	(&SettingController{}).initRouter(g)
	return r
}

type authReq struct {
	path       string
	form       url.Values
	cookie     *http.Cookie
	remoteAddr string
	headers    map[string]string
}

func doAuth(t *testing.T, r *gin.Engine, q authReq) (entity.Msg, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, q.path, strings.NewReader(q.form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	for k, v := range q.headers {
		req.Header.Set(k, v)
	}
	if q.remoteAddr != "" {
		req.RemoteAddr = q.remoteAddr
	}
	if q.cookie != nil {
		req.AddCookie(q.cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var msg entity.Msg
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("解析响应 %q: %v", w.Body.String(), err)
	}
	return msg, w
}

func sessionCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			return c
		}
	}
	return nil
}

func loginForm(username, password string) url.Values {
	return url.Values{"username": {username}, "password": {password}}
}

func mustLogin(t *testing.T, r *gin.Engine, username, password string) *http.Cookie {
	t.Helper()
	msg, w := doAuth(t, r, authReq{path: "/login", form: loginForm(username, password)})
	if !msg.Success {
		t.Fatalf("登录失败: %s", msg.Msg)
	}
	c := sessionCookie(w)
	if c == nil {
		t.Fatal("登录成功但没有下发会话 cookie")
	}
	return c
}

func isLoggedIn(t *testing.T, r *gin.Engine, c *http.Cookie) bool {
	t.Helper()
	msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/status", cookie: c})
	return msg.Success
}

func TestLoginSessionCookieIsHardenedAndCarriesNoPassword(t *testing.T) {
	r := newAuthRouter(t)
	// 用一个不会与用户名或别的字段撞车的密码，下面才能断言它没出现在 cookie 里。
	const password = "Dist1nct-Passw0rd"
	if err := (&service.UserService{}).UpdateFirstUser("admin", password); err != nil {
		t.Fatal(err)
	}
	c := mustLogin(t, r, "admin", password)

	if !c.HttpOnly {
		t.Error("会话 cookie 缺少 HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Secure {
		t.Error("明文 HTTP 请求下发了 Secure cookie，浏览器会直接丢弃它")
	}
	if !isLoggedIn(t, r, c) {
		t.Fatal("带着刚下发的 cookie 访问需登录接口被拒绝")
	}

	// 会话 cookie 只签名不加密：把它一层层解开，里面不能有密码，也不能有
	// 库里的哈希。改动前整个 User（含明文密码）都在这里面。
	user := &model.User{}
	if err := database.GetDB().First(user).Error; err != nil {
		t.Fatal(err)
	}
	decoded := decodeCookieLayers(t, c.Value)
	if !strings.Contains(decoded, "admin") {
		t.Fatal("解出来的内容里连用户名都没有，说明解码方式不对，下面两条断言无效")
	}
	if strings.Contains(decoded, password) {
		t.Error("会话 cookie 里出现了明文密码")
	}
	if strings.Contains(decoded, user.Password) {
		t.Error("会话 cookie 里出现了库里的密码哈希")
	}
}

// decodeCookieLayers 解开 securecookie 的两层 base64，返回最里层的 gob 字节。
func decodeCookieLayers(t *testing.T, value string) string {
	t.Helper()
	outer, err := base64.URLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("解 cookie 外层: %v", err)
	}
	// 第三段是原始的 MAC 字节，里面可能恰好含有 '|'，所以只切前两刀。
	parts := strings.SplitN(string(outer), "|", 3)
	if len(parts) != 3 {
		t.Fatalf("cookie 结构不是 date|value|mac: %d 段", len(parts))
	}
	inner, err := base64.URLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解 cookie 内层: %v", err)
	}
	return string(inner)
}

func TestSecureCookieBehindLocalReverseProxy(t *testing.T) {
	r := newAuthRouter(t)
	_, w := doAuth(t, r, authReq{
		path: "/login", form: loginForm("admin", "admin"),
		remoteAddr: "127.0.0.1:40000",
		headers:    map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"},
	})
	if c := sessionCookie(w); c == nil || !c.Secure {
		t.Error("经本机反代的 HTTPS 请求没有下发 Secure cookie")
	}
	// 面板直接暴露时这个头是访客写的，不能据此下发 Secure。
	_, w = doAuth(t, r, authReq{
		path: "/login", form: loginForm("admin", "admin"),
		remoteAddr: "203.0.113.9:40000",
		headers:    map[string]string{"X-Forwarded-Proto": "https"},
	})
	if c := sessionCookie(w); c == nil || c.Secure {
		t.Error("信任了公网对端伪造的 X-Forwarded-Proto")
	}
}

func TestChangingPasswordInvalidatesOtherSessions(t *testing.T) {
	r := newAuthRouter(t)
	other := mustLogin(t, r, "admin", "admin")
	mine := mustLogin(t, r, "admin", "admin")

	msg, w := doAuth(t, r, authReq{path: "/aui/setting/updateUser", cookie: mine, form: url.Values{
		"oldUsername": {"admin"}, "oldPassword": {"admin"},
		"newUsername": {"admin"}, "newPassword": {"n3w-pass"},
	}})
	if !msg.Success {
		t.Fatalf("改密码失败: %s", msg.Msg)
	}
	if isLoggedIn(t, r, other) {
		t.Error("改密码之后，另一个旧会话仍然有效")
	}
	if isLoggedIn(t, r, mine) {
		t.Error("改密码之前签发的 cookie 仍然有效")
	}
	// 发起修改的这个会话拿到了重新签发的 cookie，不用立刻重新登录。
	renewed := sessionCookie(w)
	if renewed == nil || !isLoggedIn(t, r, renewed) {
		t.Error("改密码的会话没有被重新签发")
	}
	if msg, _ := doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin")}); msg.Success {
		t.Error("旧密码仍能登录")
	}
}

func TestUpdateUserRequiresCurrentPassword(t *testing.T) {
	r := newAuthRouter(t)
	c := mustLogin(t, r, "admin", "admin")
	msg, _ := doAuth(t, r, authReq{path: "/aui/setting/updateUser", cookie: c, form: url.Values{
		"oldUsername": {"admin"}, "oldPassword": {"wrong"},
		"newUsername": {"admin"}, "newPassword": {"n3w-pass"},
	}})
	if msg.Success {
		t.Fatal("原密码错误时改密码成功了")
	}
	if !isLoggedIn(t, r, c) {
		t.Error("被拒绝的修改让当前会话失效了")
	}
}

// 来自 `a-ui setting -password` 的改动（另一个进程直接改库）同样要让
// 已有会话失效——服务端没有别的办法吊销一个存在客户端的 cookie。
func TestOutOfBandPasswordChangeInvalidatesSession(t *testing.T) {
	r := newAuthRouter(t)
	c := mustLogin(t, r, "admin", "admin")
	if err := (&service.UserService{}).UpdateFirstUser("admin", "changed-by-cli"); err != nil {
		t.Fatal(err)
	}
	if isLoggedIn(t, r, c) {
		t.Error("命令行改密码之后旧会话仍然有效")
	}
}

func TestLoginLockoutCannotBeBypassed(t *testing.T) {
	r := newAuthRouter(t)
	const attacker = "203.0.113.9:5000"
	for i := 1; i <= loginMaxFailures; i++ {
		msg, _ := doAuth(t, r, authReq{
			path: "/login", form: loginForm("admin", "guess"), remoteAddr: attacker,
			// 每次换一个伪造的来源头：面板直接暴露时它不可信，不能让它
			// 把计数分散到别的地址上。
			headers: map[string]string{"X-Forwarded-For": "198.51.100." + string(rune('0'+i))},
		})
		if msg.Success {
			t.Fatal("错误密码登录成功了")
		}
	}
	// 锁定后，哪怕密码是对的也不放行——否则锁定期内仍然可以继续猜。
	msg, _ := doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin"), remoteAddr: attacker})
	if msg.Success {
		t.Fatal("锁定期内正确密码登录成功，限速形同虚设")
	}
	if !strings.Contains(msg.Msg, "分钟后再试") {
		t.Errorf("锁定提示 = %q", msg.Msg)
	}
	// 别的来源（管理员自己）不受影响。
	msg, _ = doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin"), remoteAddr: "192.0.2.50:5000"})
	if !msg.Success {
		t.Errorf("另一个来源被牵连锁定: %s", msg.Msg)
	}
}

// 经本机 Caddy 转发时所有连接的对端都是 127.0.0.1，必须按 Caddy 追加的
// 最后一段计数；访客自己写在前面的那些段不能生效。
func TestLoginLockoutBehindReverseProxyUsesLastForwardedHop(t *testing.T) {
	r := newAuthRouter(t)
	for i := 0; i < loginMaxFailures; i++ {
		doAuth(t, r, authReq{
			path: "/login", form: loginForm("admin", "guess"), remoteAddr: "127.0.0.1:5000",
			headers: map[string]string{"X-Forwarded-For": "10.9.9." + string(rune('1'+i)) + ", 203.0.113.9"},
		})
	}
	msg, _ := doAuth(t, r, authReq{
		path: "/login", form: loginForm("admin", "admin"), remoteAddr: "127.0.0.1:5001",
		headers: map[string]string{"X-Forwarded-For": "203.0.113.9"},
	})
	if msg.Success {
		t.Fatal("攻击者在 X-Forwarded-For 前面伪造地址绕过了限速")
	}
	// 同样经反代进来的另一个真实来源照常能登录：不能把整个 127.0.0.1 锁死。
	msg, _ = doAuth(t, r, authReq{
		path: "/login", form: loginForm("admin", "admin"), remoteAddr: "127.0.0.1:5002",
		headers: map[string]string{"X-Forwarded-For": "192.0.2.50"},
	})
	if !msg.Success {
		t.Errorf("反代后的另一个来源被牵连锁定: %s", msg.Msg)
	}
}

func TestSuccessfulLoginResetsFailureCount(t *testing.T) {
	r := newAuthRouter(t)
	for round := 0; round < 3; round++ {
		for i := 0; i < loginMaxFailures-1; i++ {
			doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "typo")})
		}
		if msg, _ := doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin")}); !msg.Success {
			t.Fatalf("第 %d 轮：输错几次后的正确登录被拒: %s", round+1, msg.Msg)
		}
	}
}

// waitForStableTotpStep 保证接下来几秒不会跨过一个 30 秒的时间步。
//
// 下面的用例要连续用掉相邻三步的验证码（开启、登录、关闭各一个，重放防护
// 要求一个比一个新），它们同时有效的前提是「当前步」在用例运行期间不变。
// 正好卡在步的末尾时等到下一步开头再开始，最多等 4 秒。
func waitForStableTotpStep(t *testing.T) {
	t.Helper()
	if remain := totp.Period - time.Now().Unix()%totp.Period; remain <= 4 {
		time.Sleep(time.Duration(remain) * time.Second)
	}
}

func TestTwoFactorLoginFlow(t *testing.T) {
	waitForStableTotpStep(t)
	r := newAuthRouter(t)
	c := mustLogin(t, r, "admin", "admin")

	// 生成密钥要再输一次密码。
	if msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/setup", cookie: c,
		form: url.Values{"password": {"wrong"}}}); msg.Success {
		t.Fatal("密码错误时生成了密钥")
	}
	msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/setup", cookie: c,
		form: url.Values{"password": {"admin"}}})
	if !msg.Success {
		t.Fatalf("生成密钥失败: %s", msg.Msg)
	}
	obj, _ := msg.Obj.(map[string]any)
	secret, _ := obj["secret"].(string)
	if secret == "" {
		t.Fatalf("响应里没有密钥: %v", msg.Obj)
	}

	// 时间步往后挪，保证登录用的码比开启时用掉的那个新（重放防护）。
	codeAt := func(steps int) string {
		code, err := totp.Code(secret, time.Now().Add(time.Duration(steps*totp.Period)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/enable", cookie: c,
		form: url.Values{"code": {codeAt(-1)}}}); !msg.Success {
		t.Fatalf("开启两步验证失败: %s", msg.Msg)
	}
	// 状态接口只说开没开，不回传密钥。
	msg, w := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/status", cookie: c})
	if !msg.Success || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("状态接口异常或泄露了密钥: %s", w.Body.String())
	}

	// 开启后只有密码登不上，且提示不透露「密码其实是对的」。
	msg, _ = doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin")})
	if msg.Success {
		t.Fatal("开启两步验证后，只凭密码登录成功了")
	}
	wrongPw, _ := doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "nope")})
	if msg.Msg != wrongPw.Msg {
		t.Errorf("缺验证码与密码错误的提示不同，泄露了密码是否正确: %q vs %q", msg.Msg, wrongPw.Msg)
	}

	form := loginForm("admin", "admin")
	form.Set("code", codeAt(0))
	msg, w = doAuth(t, r, authReq{path: "/login", form: form})
	if !msg.Success {
		t.Fatalf("密码 + 验证码登录失败: %s", msg.Msg)
	}
	c2 := sessionCookie(w)

	// 关闭需要密码 + 一个新的有效验证码。
	if msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/disable", cookie: c2,
		form: url.Values{"password": {"admin"}, "code": {codeAt(0)}}}); msg.Success {
		t.Fatal("用已经用过的验证码关闭成功了")
	}
	if msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/disable", cookie: c2,
		form: url.Values{"password": {"wrong"}, "code": {codeAt(1)}}}); msg.Success {
		t.Fatal("密码错误时关闭成功了")
	}
	// 上一条请求里密码错在先、验证码没被校验，因此 codeAt(1) 仍然可用。
	if msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/disable", cookie: c2,
		form: url.Values{"password": {"admin"}, "code": {codeAt(1)}}}); !msg.Success {
		t.Fatalf("关闭两步验证失败: %s", msg.Msg)
	}
	if msg, _ := doAuth(t, r, authReq{path: "/login", form: loginForm("admin", "admin")}); !msg.Success {
		t.Fatalf("关闭后只凭密码登录失败: %s", msg.Msg)
	}
}

// 验证码错误同样计入限速，否则 6 位数字可以被直接穷举。
func TestWrongTwoFactorCodeCountsTowardLockout(t *testing.T) {
	waitForStableTotpStep(t)
	r := newAuthRouter(t)
	c := mustLogin(t, r, "admin", "admin")
	msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/setup", cookie: c,
		form: url.Values{"password": {"admin"}}})
	secret := msg.Obj.(map[string]any)["secret"].(string)
	code, _ := totp.Code(secret, time.Now().Add(-totp.Period*time.Second))
	if msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/enable", cookie: c,
		form: url.Values{"code": {code}}}); !msg.Success {
		t.Fatalf("开启失败: %s", msg.Msg)
	}

	for i := 0; i < loginMaxFailures; i++ {
		form := loginForm("admin", "admin")
		form.Set("code", "000000")
		doAuth(t, r, authReq{path: "/login", form: form, remoteAddr: "203.0.113.9:5000"})
	}
	form := loginForm("admin", "admin")
	good, _ := totp.Code(secret, time.Now())
	form.Set("code", good)
	if msg, _ := doAuth(t, r, authReq{path: "/login", form: form, remoteAddr: "203.0.113.9:5000"}); msg.Success {
		t.Fatal("连续输错验证码之后没有被锁定")
	}
}

func TestUnauthenticatedRequestIsRejected(t *testing.T) {
	r := newAuthRouter(t)
	msg, _ := doAuth(t, r, authReq{path: "/aui/setting/twoFactor/status"})
	if msg.Success {
		t.Fatal("未登录访问了需登录接口")
	}
	// 伪造的 cookie（签名对不上）同样当作未登录。
	msg, _ = doAuth(t, r, authReq{path: "/aui/setting/twoFactor/status",
		cookie: &http.Cookie{Name: "session", Value: "forged"}})
	if msg.Success {
		t.Fatal("伪造的 cookie 通过了登录校验")
	}
}

// 升级前的会话里是整个 model.User。带着那样的 cookie 回来的浏览器必须被当成
// 未登录，而不是 panic（旧代码里是 obj.(model.User) 的类型断言）或被放行。
//
// 真实的新二进制里 model.User 没有 gob.Register，旧 cookie 在解码阶段就失败；
// 测试进程里为了造出旧 cookie 不得不注册它，于是这里走的是「解码成功但没有
// 新格式的 key」那条路径。两条路径的结论必须相同，这里守的是后一条。
func TestLegacySessionCookieIsTreatedAsLoggedOut(t *testing.T) {
	gob.Register(model.User{})
	r := newAuthRouter(t)
	r.POST("/legacy-login", func(c *gin.Context) {
		s := sessions.Default(c)
		s.Set("LOGIN_USER", model.User{Id: 1, Username: "admin", Password: "admin"})
		if err := s.Save(); err != nil {
			t.Errorf("写入旧格式会话: %v", err)
		}
		c.JSON(http.StatusOK, entity.Msg{Success: true})
	})
	_, w := doAuth(t, r, authReq{path: "/legacy-login"})
	legacy := sessionCookie(w)
	if legacy == nil {
		t.Fatal("没有拿到旧格式的 cookie")
	}
	if isLoggedIn(t, r, legacy) {
		t.Fatal("旧格式的会话 cookie 仍被当作已登录")
	}
}
