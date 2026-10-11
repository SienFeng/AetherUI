package web

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"a-ui/web/entity"
)

// csrfGuard 拦下跨站伪造的状态变更请求。
//
// 面板的所有接口都由 axios 发出，而 web/assets/js/axios-init.js 给每个
// 请求都带了 X-Requested-With: XMLHttpRequest。这个头有一个关键性质：
// 跨站的 <form> 提交根本加不上自定义头；跨站的 fetch/XHR 想加它就必须
// 先过 CORS 预检，而面板不响应任何预检。所以「非只读请求必须带这个头」
// 就足以把跨站请求挡在外面，不需要再往每个模板里塞 token。
//
// 现代浏览器还会带 Sec-Fetch-Site，明确标着 cross-site 的直接拒绝，
// 作为第二层。会话 cookie 的 SameSite=Lax（web/session）是第三层。
//
// 新增任何不经 axios 发出的非 GET 请求（原生表单、fetch、sendBeacon）
// 都必须自己带上这个头，否则会在这里被 403。
func csrfGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if c.GetHeader("Sec-Fetch-Site") == "cross-site" ||
			c.GetHeader("X-Requested-With") != "XMLHttpRequest" {
			c.AbortWithStatusJSON(http.StatusForbidden, entity.Msg{
				Success: false,
				Msg:     "请求被拒绝：来源校验未通过，请刷新页面后重试",
			})
			return
		}
		c.Next()
	}
}
