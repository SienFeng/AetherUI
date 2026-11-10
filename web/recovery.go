package web

import (
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"a-ui/logger"
)

// newEngine 等价于 gin.Default()（Logger + Recovery），只是把 handler 里 panic
// 的去向从 gin.DefaultErrorWriter 换成面板日志。
//
// initRouter 在非调试模式下会先把 gin.DefaultErrorWriter 设成 io.Discard（压掉
// gin 自己的调试输出），而 gin.Default() 的 Recovery 写的正是它——改动前任何
// 一个接口 panic 都只剩一个 500，日志里什么都没有。
func newEngine() *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Logger(), recoverToPanelLog(logger.Error))
	return engine
}

// recoverToPanelLog 截住 handler 里的 panic，回 500，并把方法、路径、panic 的值
// 与堆栈交给 logError。
//
// 不用 gin.RecoveryWithWriter：它在调试模式和连接已断（broken pipe）时会把整份
// 请求头写进日志，其中的 Cookie 就是 30 天有效的会话凭证。这里只记路径不记查询
// 串，理由相同。传 nil 作为 writer 让 gin 自己一个字都不写；连接已断的情况 gin
// 不会调用这里，也就不记——那不是程序的问题。
func recoverToPanelLog(logError func(args ...interface{})) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, err interface{}) {
		logError("处理请求时发生 panic:", c.Request.Method, c.Request.URL.Path, "-", err, "\n"+string(debug.Stack()))
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}
