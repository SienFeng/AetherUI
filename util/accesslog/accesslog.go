// Package accesslog 解析 xray 的访问日志，并按偏移增量读取日志文件。
//
// 两部分都不依赖数据库和操作系统特性，可以完整单测；存储、保留期与配置
// 注入放在 web/service 里。
package accesslog

import (
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

	target, rest, ok := cutField(rest)
	if !ok {
		return Entry{}, false
	}

	// 方括号里是路由信息。没有它就不知道这条记录属于谁，直接丢弃。
	open := strings.Index(rest, "[")
	closeIdx := strings.Index(rest, "]")
	if open < 0 || closeIdx < open {
		return Entry{}, false
	}
	inbound, route := splitDetour(rest[open+1 : closeIdx])
	if inbound == "" {
		return Entry{}, false
	}

	network, targetAddr := splitNetworkPrefix(target)
	_, sourceAddr := splitNetworkPrefix(source)
	sourceIP, ok := HostOf(sourceAddr)
	if !ok {
		return Entry{}, false
	}

	return Entry{
		Time:     ts,
		SourceIP: sourceIP,
		Network:  network,
		Target:   targetAddr,
		Inbound:  inbound,
		Route:    route,
		Accepted: accepted,
		Email:    emailOf(rest[closeIdx+1:]),
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
// 分隔符实测有 "->" 与 ">>" 两种：命中路由规则用前者，走默认出站用后者。
func splitDetour(detour string) (inbound, route string) {
	detour = strings.TrimSpace(detour)
	best := -1
	bestLen := 0
	for _, sep := range []string{" -> ", " >> "} {
		if idx := strings.Index(detour, sep); idx >= 0 && (best < 0 || idx < best) {
			best, bestLen = idx, len(sep)
		}
	}
	if best < 0 {
		// rejected 的行只有入站 tag，没有出站。
		return detour, ""
	}
	return strings.TrimSpace(detour[:best]), strings.TrimSpace(detour[best+bestLen:])
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
