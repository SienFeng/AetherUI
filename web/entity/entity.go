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

	AccessLogEnable          int `json:"accessLogEnable" form:"accessLogEnable"`
	AccessLogRetentionDays   int `json:"accessLogRetentionDays" form:"accessLogRetentionDays"`
	TrafficHourRetentionDays int `json:"trafficHourRetentionDays" form:"trafficHourRetentionDays"`
	TrafficDayRetentionDays  int `json:"trafficDayRetentionDays" form:"trafficDayRetentionDays"`
	ConcurrencyIdleTimeout   int `json:"concurrencyIdleTimeout" form:"concurrencyIdleTimeout"`

	TCInterface string `json:"tcInterface" form:"tcInterface"`

	DefaultDomain   string `json:"defaultDomain" form:"defaultDomain"`
	DefaultCertFile string `json:"defaultCertFile" form:"defaultCertFile"`
	DefaultKeyFile  string `json:"defaultKeyFile" form:"defaultKeyFile"`
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

// isLoopbackListen 判断面板监听地址是否是回环地址，即面板只能经由本机的
// 前置反向代理访问。
//
// 空串**不算**：它是 webListen 的默认值，含义是「监听所有 IP」，那种部署下
// 面板直接对外，自行配置 TLS 是完全正当的。
func isLoopbackListen(listen string) bool {
	if listen == "" {
		return false
	}
	ip := net.ParseIP(listen)
	return ip != nil && ip.IsLoopback()
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

	// 这一条必须排在下面的 LoadX509KeyPair 之前：只填了公钥或只填了密钥时，
	// 加载必定失败，报出来的是 "open : no such file or directory"——指向一个
	// 空路径，完全看不出真正的问题是「这个拓扑下根本不该填」。
	//
	// 面板监听在回环地址，意味着它藏在前置反向代理后面（安装向导的 Caddy
	// 拓扑就是这样）：TLS 由反代终结，再以明文转发进来。此时面板若还自己
	// 监听 TLS，network.AutoHttpsConn 会把反代转发来的明文首包判成「非 TLS
	// 连接」，对每个请求回一个 307 跳到同一个 URL，从外面看就是无限重定向，
	// 面板彻底打不开。
	//
	// 之所以要在保存这一刻硬拒绝，而不是只在界面上提示：这个状态没有退路。
	// 面板已经打不开，改不回来；重装也救不回来——bootstrap 靠 webBasePath
	// != "/" 判定「已经配置过」而整体跳过，不会重新清空这两项；而唯一被打印
	// 过的救援命令 `a-ui setting -listen ""` 只改监听地址，不碰证书字段。
	//
	// 反过来，面板直接对外暴露时（无域名安装的 REALITY 分支不装任何反代，
	// webListen 保持空串即「监听所有 IP」）这两项是管理员给面板加 HTTPS 的
	// 唯一手段，所以只对回环地址生效，绝不能扩大到全部。
	if isLoopbackListen(s.WebListen) && (s.WebCertFile != "" || s.WebKeyFile != "") {
		return common.NewErrorf("面板监听在回环地址 %v 时不能配置面板证书："+
			"此时 TLS 由前置反向代理终结并以明文转发给面板，面板再自行监听 TLS 会导致每个请求被 307 重定向到自身、形成死循环而彻底打不开。"+
			"请把「面板证书公钥文件路径」和「面板证书密钥文件路径」都留空，证书交给反向代理配置", s.WebListen)
	}

	if s.WebCertFile != "" || s.WebKeyFile != "" {
		_, err := tls.LoadX509KeyPair(s.WebCertFile, s.WebKeyFile)
		if err != nil {
			return common.NewErrorf("cert file <%v> or key file <%v> invalid: %v", s.WebCertFile, s.WebKeyFile, err)
		}
	}

	// 只校验路径格式，不做 tls.LoadX509KeyPair。这三个是「新建入站时的默认
	// 填充值」，面板自己不加载它们；证书尚未签发就填了路径是正常状态，
	// 在这里做加载校验会让整个设置页保存失败，连带端口、时区一起遭殃。
	for _, p := range []struct {
		name  string
		value string
	}{
		{"default cert file", s.DefaultCertFile},
		{"default key file", s.DefaultKeyFile},
	} {
		if p.value != "" && !strings.HasPrefix(p.value, "/") {
			return common.NewErrorf("%v must be an absolute path: %v", p.name, p.value)
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

	// 小时桶是图上「近期看细节」的那一级，行数随天数线性增长；日桶一年
	// 才 365 行，上界给得宽。两者都不允许为 0：0 会让清理任务把全部历史
	// 一次删光，而这不是任何人想通过「填 0」表达的意思。
	if s.TrafficHourRetentionDays < 1 || s.TrafficHourRetentionDays > 365 {
		return common.NewError("用量小时数据保留天数应在 1 ~ 365 天之间:", s.TrafficHourRetentionDays)
	}
	if s.TrafficDayRetentionDays < 1 || s.TrafficDayRetentionDays > 3650 {
		return common.NewError("用量每日数据保留天数应在 1 ~ 3650 天之间:", s.TrafficDayRetentionDays)
	}

	// 网卡名会被拼进 tc/ip 的命令参数。留空表示自动探测。
	if s.TCInterface != "" && !validInterfaceName(s.TCInterface) {
		return common.NewError("限速网卡名不合法（只允许字母、数字和 . _ - : @，最长 15 字符）:", s.TCInterface)
	}

	return nil
}
