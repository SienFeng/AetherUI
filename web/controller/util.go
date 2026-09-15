package controller

import (
	"github.com/gin-gonic/gin"
	"net"
	"net/http"
	"strings"
	"a-ui/config"
	"a-ui/logger"
	"a-ui/web/entity"
	"a-ui/web/service"
)

func getUriId(c *gin.Context) int64 {
	s := struct {
		Id int64 `uri:"id"`
	}{}

	_ = c.BindUri(&s)
	return s.Id
}

func getRemoteIp(c *gin.Context) string {
	value := c.GetHeader("X-Forwarded-For")
	if value != "" {
		ips := strings.Split(value, ",")
		return ips[0]
	} else {
		addr := c.Request.RemoteAddr
		ip, _, _ := net.SplitHostPort(addr)
		return ip
	}
}

func jsonMsg(c *gin.Context, msg string, err error) {
	jsonMsgObj(c, msg, nil, err)
}

func jsonObj(c *gin.Context, obj interface{}, err error) {
	jsonMsgObj(c, "", obj, err)
}

func jsonMsgObj(c *gin.Context, msg string, obj interface{}, err error) {
	m := entity.Msg{
		Obj: obj,
	}
	if err == nil {
		m.Success = true
		if msg != "" {
			m.Msg = msg + "成功"
		}
	} else {
		m.Success = false
		m.Msg = msg + "失败: " + err.Error()
		logger.Warning(msg+"失败: ", err)
	}
	c.JSON(http.StatusOK, m)
}

func pureJsonMsg(c *gin.Context, success bool, msg string) {
	if success {
		c.JSON(http.StatusOK, entity.Msg{
			Success: true,
			Msg:     msg,
		})
	} else {
		c.JSON(http.StatusOK, entity.Msg{
			Success: false,
			Msg:     msg,
		})
	}
}

func html(c *gin.Context, name string, title string, data gin.H) {
	if data == nil {
		data = gin.H{}
	}
	data["title"] = title
	data["request_uri"] = c.Request.RequestURI
	data["base_path"] = c.GetString("base_path")
	data["time_zone"] = panelTimeZone()
	c.HTML(http.StatusOK, name, getContext(data))
}

// panelTimeZone 返回面板时区的 IANA 名，前端据此按面板时区显示时间
// （web/assets/js/util/date-util.js）。
//
// 走 GetTimeLocation 而不是直接读设置项：它对非法值回落到默认时区，前端与
// 服务端（曲线刻度、用量窗口、定时任务）因此始终是同一个口径。读库失败时
// 返回空串，前端据此回落到浏览器本地时区——那是改动前的行为，页面照常可用。
func panelTimeZone() string {
	settingService := service.SettingService{}
	loc, err := settingService.GetTimeLocation()
	if err != nil {
		logger.Warning("get time location failed:", err)
		return ""
	}
	return loc.String()
}

func getContext(h gin.H) gin.H {
	a := gin.H{
		"cur_ver": config.GetVersion(),
	}
	if h != nil {
		for key, value := range h {
			a[key] = value
		}
	}
	return a
}

func isAjax(c *gin.Context) bool {
	return c.GetHeader("X-Requested-With") == "XMLHttpRequest"
}
