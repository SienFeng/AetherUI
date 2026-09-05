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

	IPRuleResolveDomain int `json:"ipRuleResolveDomain" form:"ipRuleResolveDomain"`

	DNSServers string `json:"dnsServers" form:"dnsServers"`

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

// dnsServerSchemes 是 xray 的 dns.servers 真正认识的地址前缀，一个不多一个不少。
//
// 名单直接抄自核心的分派表 app/dns/nameserver.go:53-76（NewServer）：只有
// localhost（整串精确匹配）、下面这七个 scheme、fakedns 与裸 IP 目标会各自
// 走到专门的解析器实现，**其余一律落进函数末尾那个 UDP 分支**，把整个字符串
// 当成主机名去连。
//
// 所以这份名单两侧的错误都必须挡住，而且两种失败在面板上都完全看不见：
//   - udp:// tls:// quic://（注意 quic 只有 quic+local:// 这一种形式）看着
//     天经地义，实测（bin/xray-darwin-arm64，26.7.28）全部 Configuration OK，
//     运行时却变成一个指向 "udp://223.5.5.5" 这种不可解析主机名的 UDP 客户端。
//     DNS 设置从此完全空转，不报任何错，管理员以为解析已经换掉了。
//   - IP:端口（1.1.1.1:53）更糟。它是域名族地址，进 url.Parse 直接失败，
//     实测 xray **拒绝启动**（exit 23，"first path segment in URL cannot
//     contain colon"）。而 dns 在 xray/hot_diff.go 的 static 名单里，保存必然
//     触发整进程重启；Process.Start() 把 cmd.Run() 丢进 goroutine，从不回传
//     启动失败，于是 /server/status 继续返回 running、errorMsg 为空，而机器上
//     所有用户已经断网。改动前的校验器不但放行它，报错文案还在推荐这种写法。
//
// 前缀匹配不会互相遮蔽：https+local:// 的第 6 个字符是 '+'，https:// 是 ':'，
// HasPrefix 不成立；h2c / tcp 同理。每个 +local 形式都有单独的用例守着。
var dnsServerSchemes = []string{
	"https://", "h2c://", "https+local://", "h2c+local://",
	"quic+local://", "tcp://", "tcp+local://",
}

// checkDNSServer 只查语法，不测可达性。
//
// 可达性交给运行时：配错的最坏后果已经被「只用 UseIP 系列」兜住——解析
// 失败时 freedom 回落按域名直连（proxy/freedom/freedom.go:298 只在
// ForceIP() 时才把失败变成断连），而路由侧的 IPIfNonMatch 解析失败也只是
// IP 规则不命中（features/routing/dns/context.go:21）。在保存这一刻做网络
// 探测，换来的是一次网络抖动就把管理员挡在门外。
//
// 裸域名（dns.google）拒绝：xray 要先解析这个域名本身才能用它，而此时还
// 没有可用的解析器，是个鸡生蛋问题。IP 型端点（https://8.8.8.8/dns-query）
// 零 bootstrap 依赖，是唯一稳妥的写法。
func checkDNSServer(item string) error {
	if item == "localhost" {
		return nil
	}
	for _, scheme := range dnsServerSchemes {
		if !strings.HasPrefix(item, scheme) {
			continue
		}
		if len(item) == len(scheme) {
			return common.NewError("DNS 服务器地址缺少主机名:", item)
		}
		return nil
	}
	// 刻意不做 net.SplitHostPort：IP:端口 是 xray 唯一会拒绝启动的写法
	// （见 dnsServerSchemes 的注释），必须在这里挡下。
	if net.ParseIP(item) == nil {
		return common.NewError("DNS 服务器地址不支持:", item,
			"——应为裸 IP（8.8.8.8 / 2001:4860:4860::8888）、localhost，或 "+
				strings.Join(dnsServerSchemes, " ")+"开头的地址。"+
				"IP:端口 会让 xray 拒绝启动；udp:// tls:// quic:// 这三种写法 xray 并不认识，"+
				"会静默退化成一个连不上的 UDP 解析器。"+
				"不带协议头的裸域名（dns.google）也不支持：xray 要先解析它本身才能用它")
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

	// 只接受 0/1：反射只支持 int，前端的 switch 也只会送这两个值。
	// 放行其他值会让生成期写出一个 xray 不认识的 domainStrategy，
	// 而那会让整份配置加载失败——全员断网。
	if s.IPRuleResolveDomain != 0 && s.IPRuleResolveDomain != 1 {
		return common.NewError("「IP 规则匹配域名目标」只能是 0 或 1:", s.IPRuleResolveDomain)
	}

	// 空表示不启用，是正常状态。
	for _, line := range strings.Split(s.DNSServers, "\n") {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		if err := checkDNSServer(item); err != nil {
			return err
		}
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
