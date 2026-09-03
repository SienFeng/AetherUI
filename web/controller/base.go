package controller

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"a-ui/web/service"
	"a-ui/web/session"
)

type BaseController struct {
	shapingService service.ShapingService
}

func (a *BaseController) checkLogin(c *gin.Context) {
	if !session.IsLogin(c) {
		if isAjax(c) {
			pureJsonMsg(c, false, "登录时效已过，请重新登录")
		} else {
			c.Redirect(http.StatusTemporaryRedirect, c.GetString("base_path"))
		}
		c.Abort()
	} else {
		// 一次已登录的面板请求 = 管理员仍然能访问面板 = 刚下发的限速规则
		// 没有把网络掐断。这是端口限速自动撤销机制的确认信号，见
		// service.ShapingService.CheckRollback。放在这里是因为它覆盖了
		// 全部需要登录的接口，包括前端每 2 秒轮询的 /server/status。
		a.shapingService.Heartbeat()
		c.Next()
	}
}
