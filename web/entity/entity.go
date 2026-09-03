package entity

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"time"
	"a-ui/util/common"
	"a-ui/xray"
)

type Msg struct {
	Success bool        `json:"success"`
	Msg     string      `json:"msg"`
	Obj     interface{} `json:"obj"`
}

type Pager struct {
	Current  int         `json:"current"`
	PageSize int         `json:"page_size"`
	Total    int         `json:"total"`
	OrderBy  string      `json:"order_by"`
	Desc     bool        `json:"desc"`
	Key      string      `json:"key"`
	List     interface{} `json:"list"`
}

type AllSetting struct {
	WebListen   string `json:"webListen" form:"webListen"`
	WebPort     int    `json:"webPort" form:"webPort"`
	WebCertFile string `json:"webCertFile" form:"webCertFile"`
	WebKeyFile  string `json:"webKeyFile" form:"webKeyFile"`
	WebBasePath string `json:"webBasePath" form:"webBasePath"`

	XrayTemplateConfig string `json:"xrayTemplateConfig" form:"xrayTemplateConfig"`

	TimeLocation string `json:"timeLocation" form:"timeLocation"`

	SubscriptionUpdateTime string `json:"subscriptionUpdateTime" form:"subscriptionUpdateTime"`

	IPDBSourceUrl  string `json:"ipdbSourceUrl" form:"ipdbSourceUrl"`
	QQWrySourceUrl string `json:"qqwrySourceUrl" form:"qqwrySourceUrl"`
	IPDBUpdateTime string `json:"ipdbUpdateTime" form:"ipdbUpdateTime"`

	AccessLogEnable        int `json:"accessLogEnable" form:"accessLogEnable"`
	AccessLogRetentionDays int `json:"accessLogRetentionDays" form:"accessLogRetentionDays"`
	ConcurrencyIdleTimeout int `json:"concurrencyIdleTimeout" form:"concurrencyIdleTimeout"`

	TCInterface string `json:"tcInterface" form:"tcInterface"`
}

// checkIPDBSourceUrl 允许留空（表示不启用该源），非空时必须是完整的 http(s) 地址。
func checkIPDBSourceUrl(label, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return common.NewError(label+"必须是 http 或 https 开头的完整地址:", raw)
	}
	return nil
}

func validInterfaceName(name string) bool {
	if len(name) > 15 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == ':' || r == '@':
		default:
			return false
		}
	}
	return true
}

func (s *AllSetting) CheckValid() error {
	if s.WebListen != "" {
		ip := net.ParseIP(s.WebListen)
		if ip == nil {
			return common.NewError("web listen is not valid ip:", s.WebListen)
		}
	}

	if s.WebPort <= 0 || s.WebPort > 65535 {
		return common.NewError("web port is not a valid port:", s.WebPort)
	}

	if s.WebCertFile != "" || s.WebKeyFile != "" {
		_, err := tls.LoadX509KeyPair(s.WebCertFile, s.WebKeyFile)
		if err != nil {
			return common.NewErrorf("cert file <%v> or key file <%v> invalid: %v", s.WebCertFile, s.WebKeyFile, err)
		}
	}

	if !strings.HasPrefix(s.WebBasePath, "/") {
		s.WebBasePath = "/" + s.WebBasePath
	}
	if !strings.HasSuffix(s.WebBasePath, "/") {
		s.WebBasePath += "/"
	}

	xrayConfig := &xray.Config{}
	err := json.Unmarshal([]byte(s.XrayTemplateConfig), xrayConfig)
	if err != nil {
		return common.NewError("xray template config invalid:", err)
	}

	_, err = time.LoadLocation(s.TimeLocation)
	if err != nil {
		return common.NewError("time location not exist:", s.TimeLocation)
	}

	// 用 time.Parse 而不是手写正则：标准库负责格式与范围，
	// 25:00 / 04:60 这类越界值它会直接拒绝。
	if _, err := time.Parse("15:04", s.SubscriptionUpdateTime); err != nil {
		return common.NewError("订阅更新时间格式不正确，应为 HH:MM:", s.SubscriptionUpdateTime)
	}

	// 两个归属地数据源的地址：留空表示不启用该源，但不能两个都空——
	// 那样归属地显示与地区限制都会失效，而地址写错只有真正更新时才暴露。
	if err := checkIPDBSourceUrl("IP 库源地址", s.IPDBSourceUrl); err != nil {
		return err
	}
	if err := checkIPDBSourceUrl("纯真库源地址", s.QQWrySourceUrl); err != nil {
		return err
	}
	if s.IPDBSourceUrl == "" && s.QQWrySourceUrl == "" {
		return common.NewError("至少要保留一个 IP 归属地库的源地址，否则归属地与地区限制都会失效")
	}

	// 留空表示关闭自动更新。
	if s.IPDBUpdateTime != "" {
		if _, err := time.Parse("15:04", s.IPDBUpdateTime); err != nil {
			return common.NewError("IP 库更新时间格式不正确，应为 HH:MM:", s.IPDBUpdateTime)
		}
	}

	if s.AccessLogEnable != 0 && s.AccessLogEnable != 1 {
		return common.NewError("访问日志开关只能是 0 或 1:", s.AccessLogEnable)
	}

	// 不允许 0：0 在这里最容易被理解成「永不清除」，而实现上会变成
	// 「立刻全删」，语义正好相反。要关闭记录请用上面的开关。
	// 0 表示关闭闲置判定。下限 30 秒：常见应用层心跳在 30~60 秒，再短会把
	// 只是暂时没有流量的正常用户判成离线。
	if s.ConcurrencyIdleTimeout != 0 && (s.ConcurrencyIdleTimeout < 30 || s.ConcurrencyIdleTimeout > 86400) {
		return common.NewError("并发闲置超时应为 0（关闭）或 30 ~ 86400 秒:", s.ConcurrencyIdleTimeout)
	}
	if s.AccessLogRetentionDays < 1 || s.AccessLogRetentionDays > 365 {
		return common.NewError("访问日志保留天数应在 1 ~ 365 天之间:", s.AccessLogRetentionDays)
	}

	// 网卡名会被拼进 tc/ip 的命令参数。留空表示自动探测。
	if s.TCInterface != "" && !validInterfaceName(s.TCInterface) {
		return common.NewError("限速网卡名不合法（只允许字母、数字和 . _ - : @，最长 15 字符）:", s.TCInterface)
	}

	return nil
}
