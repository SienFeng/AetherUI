// Package accesslog 解析 xray 的访问日志，并按偏移增量读取日志文件。
//
// 两部分都不依赖数据库和操作系统特性，可以完整单测；存储、保留期与配置
// 注入放在 web/service 里。
package accesslog

import (
	"net"
	"net/url"
	"strings"
	"time"
)

// timeLayout 是 xray 访问日志的时间格式，微秒精度。
const timeLayout = "2006/01/02 15:04:05.000000"

// Entry 是访问日志里的一条记录。
type Entry struct {
	Time     time.Time
	SourceIP string
	Network  string // tcp / udp
	Target   string // 目标 host:port
	Inbound  string // 入站 tag
	// Route 是方括号里第一个分隔符之后的全部内容，例如 "direct"、
	// "a-ui-block"，经过中间出站时是 "proxy-hk -> direct"。
	// 保留整串而不是只取最后一段，是为了不丢掉"经过了谁"。
	Route    string
	Accepted bool
	Email    string
	// NoRuleHit 表示入站后的分隔符是 " >> "：核心没有任何路由规则命中，走了默认出站
	// （xray app/dispatcher 的 routedDispatch；命中规则时是 " -> "）。
	//
	// 必须单独留下这个区别：日志里的 Target 是客户端请求的目标，而核心分流用的是嗅探出来的
	// 域名（sniffing destOverride）。两者不同时，日志会显示「某个本该命中规则的域名走了默认
	// 出站」，只有这个标记能说明核心当时实际没命中任何规则、看的是另一个域名。
	NoRuleHit bool
}

// ParseLine 解析一行访问日志。无法识别的行返回 false，调用方应当丢弃，
// 不要写入数据库——错误日志、启动横幅都会混在同一个文件里。
func ParseLine(line string, loc *time.Location) (Entry, bool) {
	line = strings.TrimSpace(line)
	if len(line) < len(timeLayout) {
		return Entry{}, false
	}

	ts, err := time.ParseInLocation(timeLayout, line[:len(timeLayout)], loc)
	if err != nil {
		return Entry{}, false
	}
	rest := strings.TrimSpace(line[len(timeLayout):])

	if !strings.HasPrefix(rest, "from ") {
		return Entry{}, false
	}
	rest = rest[len("from "):]

	source, rest, ok := cutField(rest)
	if !ok {
		return Entry{}, false
	}

	status, rest, ok := cutField(rest)
	if !ok {
		return Entry{}, false
	}
	accepted := status == "accepted"
	if !accepted && status != "rejected" {
		return Entry{}, false
	}

	// 目标为空时 "rejected" 后面紧跟的是第二个空格（核心记 rejected 时 To 恒为空串）。
	// 不在这里拦住的话，cutField 会跳过空字段、把原因的第一个词当成目标。
	if strings.HasPrefix(rest, " ") {
		return Entry{}, false
	}
	target, rest, ok := cutField(rest)
	if !ok {
		return Entry{}, false
	}

	// 方括号里是路由信息，必须紧跟在目标之后（common/log/access.go 的 AccessMessage.String）。
	// 没有它就不知道这条记录属于谁，直接丢弃。不能在整行里找第一个方括号：rejected 的行没有
	// [detour]，原因里却常带着 IPv6 地址，那对方括号曾被当成入站 tag。
	if !strings.HasPrefix(rest, "[") {
		return Entry{}, false
	}
	closeIdx := strings.Index(rest, "]")
	if closeIdx < 0 {
		return Entry{}, false
	}
	inbound, route, noRuleHit := splitDetour(rest[1:closeIdx])
	if inbound == "" {
		return Entry{}, false
	}

	network, targetAddr := splitNetworkPrefix(target)
	targetAddr = normalizeURLTarget(targetAddr)
	_, sourceAddr := splitNetworkPrefix(source)
	sourceIP, ok := HostOf(sourceAddr)
	if !ok {
		return Entry{}, false
	}

	return Entry{
		Time:      ts,
		SourceIP:  sourceIP,
		Network:   network,
		Target:    targetAddr,
		Inbound:   inbound,
		Route:     route,
		Accepted:  accepted,
		Email:     emailOf(rest[closeIdx+1:]),
		NoRuleHit: noRuleHit,
	}, true
}

// cutField 切下一个以空格分隔的字段。
func cutField(s string) (field, rest string, ok bool) {
	s = strings.TrimLeft(s, " ")
	idx := strings.Index(s, " ")
	if idx <= 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// splitNetworkPrefix 把 "tcp:1.2.3.4:443" 拆成 ("tcp", "1.2.3.4:443")。
// 实测来源地址的这个前缀有时有、有时没有，两种都要认。
func splitNetworkPrefix(s string) (network, addr string) {
	for _, n := range []string{"tcp:", "udp:"} {
		if strings.HasPrefix(s, n) {
			return strings.TrimSuffix(n, ":"), s[len(n):]
		}
	}
	return "", s
}

// normalizeURLTarget 把 http 入站记下的请求 URL 归一成与其它入站同形的 host:port。
//
// http 入站的访问记录写的是 request.URL（proxy/http/server.go），不是目标地址：CONNECT
// 是 "//host:port"，普通代理请求是 "http://host[:port]/path"。不归一的话
// domain.Registrable 会把 "http" 当成主机名，域名统计里冒出一个叫 http 的「网站」，
// 按目标查日志、分流验证的样例也都对不上。默认端口与核心建连时一致：https 为 443，
// 其余（含 CONNECT）为 80。顺带丢掉 URL 里可能带着的账号密码与路径，它们不该落库。
// 认不出的串原样返回：宁可显示原文，也不编一个不存在的地址。
func normalizeURLTarget(target string) string {
	if !strings.HasPrefix(target, "//") && !strings.Contains(target, "://") {
		return target
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return target
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// HostOf 去掉地址末尾的端口，并剥掉 IPv6 的方括号——留着方括号会和
// 在线明细里的 IP 对不上。
func HostOf(addr string) (string, bool) {
	if addr == "" {
		return "", false
	}
	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end > 0 {
			return addr[1:end], true
		}
		return "", false
	}
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		return addr[:idx], true
	}
	return addr, true
}

// splitDetour 把方括号里的内容拆成入站 tag 与其后的路由串。
// 分隔符实测有 "->" 与 ">>" 两种：命中路由规则用前者，走默认出站用后者；
// noRuleHit 报告入站后面紧跟的是不是 ">>"。
func splitDetour(detour string) (inbound, route string, noRuleHit bool) {
	detour = strings.TrimSpace(detour)
	best := -1
	bestSep := ""
	for _, sep := range []string{" -> ", " >> "} {
		if idx := strings.Index(detour, sep); idx >= 0 && (best < 0 || idx < best) {
			best, bestSep = idx, sep
		}
	}
	if best < 0 {
		// rejected 的行只有入站 tag，没有出站。
		return detour, "", false
	}
	return strings.TrimSpace(detour[:best]), strings.TrimSpace(detour[best+len(bestSep):]), bestSep == " >> "
}

// emailOf 取出行尾可选的 " email: xxx"。本项目不给客户端配 email，
// 但别人的配置可能有，解析器要能容纳。
func emailOf(tail string) string {
	idx := strings.Index(tail, "email: ")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(tail[idx+len("email: "):])
}
