package service

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"a-ui/util/common"
)

// 逐行解析，不做全局格式识别。Surge/Clash classical、Clash YAML、纯域名列表、
// 纯 IP 列表四种格式的行特征互不冲突，逐行判断比先猜格式更健壮：真实订阅
// 文件里混着注释、YAML 头和规则行，全局识别一旦猜错就整份文件解析失败。
//
// 域名与 IP 分别收集：一个分流组同时承载两类条件，生成期各自成为一条独立的
// xray 规则。不认识的规则类型一律跳过并计数，绝不猜测。
//
// 明确不翻译的两类：IP-ASN（xray 没有 ASN 匹配能力）、SRC-IP-CIDR（那是
// source，按客户端 IP 匹配，塞进 ip 条件是语义错误）。
// 其余仍会遇到的：SRC-PORT、DST-PORT、PROCESS-NAME、PROCESS-PATH、
// USER-AGENT、URL-REGEX、RULE-SET、SUB-DOMAIN、DOMAIN-WILDCARD、AND、OR、
// NOT、PROTOCOL、NETWORK、IN-PORT。

// ParseSubscription 把订阅文件文本解析成 xray 的域名与 IP 语法。
// 返回（域名列表, IP 段列表, 跳过的条数, 错误）。
//
// 两侧都解析不出内容时返回错误而非空列表：调用方据此保留上一次成功的数据。
// 若在这里返回空列表，上游改格式或 URL 失效返回 404 页面时，分流组会被清空，
// 引用它的规则被 buildRule 跳过，流量静默退回直连。
func ParseSubscription(raw string) ([]string, []string, int, error) {
	// 部分订阅源（尤其是 Windows 工具导出的）在文件开头带 UTF-8 BOM，
	// 不去掉的话它会粘在第一行的规则类型前面，导致该行的 switch/前缀匹配
	// 全部失配而被当成一条跳过的规则——只丢一行，不易察觉，但仍是数据损失。
	raw = strings.TrimPrefix(raw, "\uFEFF")

	domains := make([]string, 0, 256)
	cidrs := make([]string, 0, 64)
	seenDomain := make(map[string]bool, 256)
	seenCidr := make(map[string]bool, 64)
	skipped := 0

	for _, line := range strings.Split(raw, "\n") {
		item := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if item == "" || strings.HasPrefix(item, "#") || strings.HasPrefix(item, ";") {
			continue
		}
		// Clash YAML 的头，不是规则
		if item == "payload:" {
			continue
		}
		// Clash YAML 的条目：- '+.example.com'
		if strings.HasPrefix(item, "- ") {
			item = strings.TrimSpace(strings.TrimPrefix(item, "- "))
			item = strings.Trim(item, `'"`)
		}

		converted, isIP, ok := convertSubscriptionLine(item)
		if !ok {
			skipped++
			continue
		}
		if isIP {
			if seenCidr[converted] {
				continue
			}
			seenCidr[converted] = true
			cidrs = append(cidrs, converted)
			continue
		}
		if seenDomain[converted] {
			continue
		}
		seenDomain[converted] = true
		domains = append(domains, converted)
	}

	if len(domains) == 0 && len(cidrs) == 0 {
		return nil, nil, skipped, common.NewError(
			"订阅内容里没有解析出任何域名或 IP 段（跳过了", skipped, "条无法翻译的规则）")
	}
	return domains, cidrs, skipped, nil
}

// convertSubscriptionLine 把一行转成 xray 语法。
// 第二个返回值为 true 表示它是 IP 段而非域名；第三个为 false 表示这行应当
// 被跳过并计数。
func convertSubscriptionLine(item string) (string, bool, bool) {
	if idx := strings.Index(item, ","); idx > 0 {
		ruleType := strings.ToUpper(strings.TrimSpace(item[:idx]))
		rest := item[idx+1:]
		// 丢掉策略段与 no-resolve 之类的尾巴，只取第一个值
		if next := strings.Index(rest, ","); next >= 0 {
			rest = rest[:next]
		}
		value := strings.TrimSpace(rest)

		switch ruleType {
		case "DOMAIN-SUFFIX":
			v, ok := domainRule("domain:", value)
			return v, false, ok
		case "DOMAIN":
			v, ok := domainRule("full:", value)
			return v, false, ok
		case "DOMAIN-KEYWORD":
			// xray 的 keyword: 就是子串匹配，与 DOMAIN-KEYWORD 语义一致。
			// 会误伤（ads 命中 downloads.example.com），但那是这个规则类型在
			// Shadowrocket/Clash 里的固有行为，不是本实现引入的偏差。
			//
			// 必须归一大小写：xray 只把目标域名转小写、不归一化配置里的模式
			// （app/router/condition.go:59），大写关键词永不命中。
			//
			// 必须带显式前缀：手工录入路径（ParseDomains）存的是 keyword:xxx，
			// 两条路径形态不一致的话 MergeDomains 按字符串去重，去不掉重复。
			//
			// 非 ASCII 的关键词跳过：嗅探出的域名一律是 ASCII（中文域名是 xn--
			// 形态），这样的关键词永远匹配不到，与手工录入路径的拒绝同一口径。
			if !isValidKeyword(value) || !isASCII(value) {
				return "", false, false
			}
			return "keyword:" + strings.ToLower(value), false, true
		case "IP-CIDR", "IP-CIDR6":
			if !isValidCIDR(value) {
				return "", false, false
			}
			return value, true, true
		case "GEOIP":
			// GEOIP,CN → geoip:cn。类别是否真的存在于机器上那份 geoip.dat，
			// 交给 ValidateCidrs 的真实 xray 判定。
			code := strings.ToLower(value)
			if !isValidGeoCode(code) {
				return "", false, false
			}
			return "geoip:" + code, true, true
		default:
			// IP-ASN / SRC-IP-CIDR 等已知但无法忠实翻译的类型，以及一切
			// 不认识的类型，一律跳过，绝不猜测
			return "", false, false
		}
	}
	// 无逗号：纯域名列表或纯 IP 列表。先试 IP——isValidDomain 明确拒绝 IP
	// 字面量，所以顺序反了会让整份中国 IP 段列表被当成垃圾全部跳过。
	if isValidCIDR(item) {
		return item, true, true
	}
	// 纯域名列表：.example.com / +.example.com / *.example.com / example.com
	// 这类列表的惯例是后缀匹配
	v, ok := domainRule("domain:", item)
	return v, false, ok
}

// isValidGeoCode 只做防呆：geoip 的类别是字母数字短串（cn、private、
// telegram），拦住带空格、斜杠、点的说明文字。
func isValidGeoCode(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func domainRule(prefix, value string) (string, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "+.")
	value = strings.TrimPrefix(value, "*.")
	value = strings.TrimPrefix(value, ".")
	value = strings.TrimSuffix(value, ".")
	// 国际化域名转成 punycode，理由见 domainToASCII；转不了就当垃圾跳过。
	value, err := domainToASCII(value)
	if err != nil || !isValidDomain(value) {
		return "", false
	}
	return prefix + strings.ToLower(value), true
}

// isValidDomain 只做防呆，不追求 RFC 完备：拦住 URL、带空格的说明文字、
// HTML 片段这些明显不是域名的东西，避免它们原样进入 xray 配置。
func isValidDomain(s string) bool {
	if s == "" || len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	if strings.ContainsAny(s, " \t/\\:?#@<>\"'()[]{}|,") {
		return false
	}
	// 纯 IP 字面量（如 "1.1.1.1"）满足上面所有检查——含点、无非法字符、
	// 每个 label 都是合法字符——但作为 xray 的 domain 条件永远匹配不到任何
	// 流量的 SNI/Host。放过它比拦下它更糟：订阅拉取会「成功」，界面显示一个
	// 体面的域名条数，规则却是个静默的哑规则。
	if net.ParseIP(s) != nil {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" {
			return false
		}
		for _, r := range label {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			// 不收非 ASCII：xray 虽然接受「例子.cn」这样的配置，但嗅探出的 SNI/Host
			// 是 punycode，那是一条永不命中的哑规则。调用方（domainRule）已先用
			// domainToASCII 转成 xn-- 形态，到这里还剩非 ASCII 就是转不了的垃圾。
			if !isAlnum && r != '-' && r != '_' {
				return false
			}
		}
	}
	return true
}

// isValidKeyword 比域名宽松（关键词不含点也合法），但仍要拦住空白与分隔符。
func isValidKeyword(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	return !strings.ContainsAny(s, " \t/\\:?#@<>\"'()[]{}|,")
}

const (
	subscriptionTimeout  = 30 * time.Second
	subscriptionMaxBytes = 10 << 20 // 10 MB，约 30 万条域名
)

// ValidateSubscribeURL 在保存表单时就拦住非法地址，不必等到拉取时才报错。
//
// 只限制 scheme。不拦内网地址：本面板是单管理员系统，管理员本就有 shell 级
// 权限，拦截 SSRF 换不来实际的安全收益，却会挡住「订阅局域网里自建的列表」
// 这种合理用法。
// maxSubscribeURLs 是一个域名组能配的订阅地址上限。
//
// 每个地址各有一次最长 subscriptionTimeout 的 HTTP 请求，而整个订阅子系统由
// subscriptionMu 串行化：地址数量直接乘进「一轮刷新最坏要占用多久那把锁」。
// 10 个已经远超实际需要（真实场景是 1~3 个），再多多半是贴错了内容。
const maxSubscribeURLs = 10

// ParseSubscribeURLs 把 DomainGroup.SubscribeUrl 里换行分隔的多个地址切出来，
// 去掉空白行、按首次出现去重。
//
// 存成换行分隔的字符串而不是 JSON 数组，是为了让单地址组的值与多地址功能
// 上线前**逐字节相同**：老数据一行都不用迁移，回退到旧版本二进制时单地址组
// 照常工作（旧代码把整个字段当一个 URL 用，而它确实就是一个 URL）。多地址组
// 在旧版本上会拉取失败并走 recordFailure，保留上一次的内容——陈旧但不为空，
// 落在安全侧。
//
// 顺序即合并顺序，是「生成逐字节确定」的一部分：调用方按这个顺序逐个拉取，
// 再按同样的顺序交给 MergeDomains。
func ParseSubscribeURLs(raw string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 2)
	for _, line := range strings.Split(raw, "\n") {
		item := strings.TrimSpace(line)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// ValidateSubscribeURLs 逐个校验换行分隔的订阅地址。
//
// 一个地址都没有是合法的，表示这个组不订阅——空值必须放行，否则所有不订阅的
// 域名组都保存不了。
func ValidateSubscribeURLs(raw string) error {
	urls := ParseSubscribeURLs(raw)
	if len(urls) > maxSubscribeURLs {
		return common.NewError("订阅地址最多", maxSubscribeURLs, "个，当前", len(urls), "个")
	}
	for i, u := range urls {
		if err := ValidateSubscribeURL(u); err != nil {
			if len(urls) > 1 {
				return common.NewError(fmt.Sprintf("第 %d 个", i+1), err)
			}
			return err
		}
	}
	return nil
}

// ValidateSubscribeURL 检查一个订阅地址。报错里的地址先经 displaySubscribeURL 去掉查询串、片段与账号密码：
// 订阅地址常带个人令牌（?token=…），报错会进保存失败的提示与面板日志，而日志出口只隐去形如 scheme:// 的
// 完整地址——缺 scheme 的、带空格解析失败的原样带出去。
func ValidateSubscribeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return common.NewError("订阅地址无法解析（检查是否夹着空格等非法字符）:", displaySubscribeURL(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return common.NewError("订阅地址必须以 http:// 或 https:// 开头:", displaySubscribeURL(raw))
	}
	if u.Host == "" {
		return common.NewError("订阅地址缺少主机名:", displaySubscribeURL(raw))
	}
	return nil
}

// displaySubscribeURL 是能放进报错与日志的订阅地址：主机与路径留着（排查靠它们），查询串、片段、账号密码
// 与路径里看起来像令牌的段（common.MaskSecretPathSegments）去掉。
// 只做字符串切分、不走 url.Parse：要处理的恰恰是写坏了、解析不了的地址。
func displaySubscribeURL(raw string) string {
	s, suffix := raw, ""
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s, suffix = s[:i], "?***"
	}
	start := 0
	if i := strings.Index(s, "://"); i >= 0 {
		start = i + 3
	}
	rest := s[start:]
	authority := rest
	if j := strings.Index(rest, "/"); j >= 0 {
		authority = rest[:j]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		s = s[:start] + "***@" + rest[at+1:]
	}
	return common.MaskSecretPathSegments(s) + suffix
}

// fetchSubscription 拉取订阅内容。超时与体积上限都是硬限制。
func fetchSubscription(rawURL string) (string, error) {
	if err := ValidateSubscribeURL(rawURL); err != nil {
		return "", err
	}

	resp, err := subscriptionClient().Get(rawURL)
	if err != nil {
		return "", common.NewError("拉取订阅失败:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", common.NewError("订阅地址返回状态码:", resp.StatusCode)
	}

	// 多读 1 字节用于判断是否超限：LimitReader 本身只会静默截断，
	// 截断后的内容解析出来是残缺的域名列表，比直接失败更危险。
	body, err := io.ReadAll(io.LimitReader(resp.Body, subscriptionMaxBytes+1))
	if err != nil {
		return "", common.NewError("读取订阅内容失败:", err)
	}
	if len(body) > subscriptionMaxBytes {
		return "", common.NewError("订阅内容超过 10MB 上限")
	}
	return string(body), nil
}

// errSubscriptionLocalAddr 是订阅地址（或它重定向到的地址）落在本机或内网时的错误。
var errSubscriptionLocalAddr = errors.New("订阅地址指向本机或内网地址，出于安全考虑不允许拉取（订阅须放在公网可访问的地址上）")

// subscriptionAllowLocal 只给测试用：测试里的订阅服务都在 127.0.0.1 上。
var subscriptionAllowLocal atomic.Bool

// AllowLocalSubscriptionsForTest 让拉取订阅可以访问本机地址。只供测试调用（各包的 TestMain）。
func AllowLocalSubscriptionsForTest() { subscriptionAllowLocal.Store(true) }

// subscriptionAddrBlocked 判断拉订阅时能不能连这个 IP：本机、内网（RFC 1918 / fc00::/7）、链路本地
// （169.254.0.0/16，云厂商的元数据服务就在这里）、运营商级 NAT（100.64.0.0/10）、未指定与组播地址一律不连。
func subscriptionAddrBlocked(ip net.IP) bool {
	if subscriptionAllowLocal.Load() {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnatRange.Contains(ip)
}

var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// subscriptionClient 是拉订阅用的 HTTP 客户端。
//
// 修复前用的是默认客户端：订阅地址（以及订阅服务器返回的重定向）可以指向本机的面板、云厂商的元数据
// 服务或内网机器，面板替人去访问它们。检查放在拨号那一刻（Dialer.Control 看的是真正要连的 IP），
// 而不是事先解析一次主机名：事先解析挡不住重定向，也挡不住两次解析结果不同（DNS 重绑定）。
// 不走环境变量里的代理：走代理时真正连的是代理，这道检查就看不到目标地址了。
func subscriptionClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: subscriptionTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || subscriptionAddrBlocked(ip) {
				return errSubscriptionLocalAddr
			}
			return nil
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = dialer.DialContext
	// 每次拉取都新建一个 Transport，空闲连接留着没人复用，只会在 IdleConnTimeout 里白占着 socket。
	transport.DisableKeepAlives = true
	return &http.Client{Timeout: subscriptionTimeout, Transport: transport}
}

// ShouldUpdateNow 判断某个域名组现在是否该更新。
//
// 这里刻意不按配置时间去建 cron entry，而是固定间隔跑、每次自己判断。
// 换来两个收益：改更新时间立即生效，不必走 SIGHUP 重启面板（cron entry 是在
// Server.Start 里注册的）；面板重启若恰好跨过时间点，也会在下一个检查窗口补上。
//
// lastUpdatedAt == 0 表示从未成功过，此时立即更新、不等时间点——新建的订阅组
// 否则会一直空到第二天凌晨，而空域名组会让引用它的规则被 buildRule 跳过。
func ShouldUpdateNow(now time.Time, lastUpdatedAt int64, hour, minute int) bool {
	if lastUpdatedAt == 0 {
		return true
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if now.Before(target) {
		return false
	}
	// 上次成功早于今天的时间点，说明今天这一轮还没跑过
	return time.UnixMilli(lastUpdatedAt).In(now.Location()).Before(target)
}

// urlInTextPattern 抓出一段文本里的 URL，交给 redactURL 脱敏。
//
// 终止字符除了空白、引号与尖括号，还包括本包错误文案里紧跟在地址后面的全角
// 标点（refreshLockedURLs 用「（」包住原因、用「；」分隔多个地址）：不在这里
// 停下的话，整段原因会被当成 URL 的一部分。不按「只收 ASCII」来停，是因为
// 主机名可能是中文域名，那样会在主机名中间停住，后面的 query 反而漏网。
var urlInTextPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>（）；，、]+`)

// redactURL 去掉一个地址里的 userinfo、query 与 fragment，只留 scheme://host/path，
// 用于写日志。
//
// 订阅地址的 query 里常带访问令牌（规则托管、机场订阅的 ?token=、?key=），userinfo
// 里则是账号密码；而面板日志会被拷去排查、贴进工单。只在日志里脱敏：界面上显示的
// 失败原因是管理员自己填的地址，原样给他看才知道是哪一行出了问题。
//
// 不用 url.Parse：要脱敏的恰恰包括解析不了的地址（ValidateSubscribeURL 的报错就会
// 带着它们），解析失败时原样返回等于把令牌写进日志。
func redactURL(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	scheme, rest := raw[:i+3], raw[i+3:]
	suffix := ""
	// fragment 与 query 谁先出现谁之后的内容都不要；只有 query 留一个占位，
	// 让排查的人知道原地址是带参数的。
	if j := strings.IndexAny(rest, "?#"); j >= 0 {
		if rest[j] == '?' {
			suffix = "?***"
		}
		rest = rest[:j]
	}
	// authority 到第一个 / 为止，其中最后一个 @ 之前是 userinfo；path 里的 @ 不算。
	authEnd := strings.Index(rest, "/")
	if authEnd < 0 {
		authEnd = len(rest)
	}
	if at := strings.LastIndex(rest[:authEnd], "@"); at >= 0 {
		rest = "***" + rest[at:]
	}
	return scheme + common.MaskSecretPathSegments(rest) + suffix
}

// redactURLsInText 对一段文本（多是嵌着订阅地址的错误信息）里的每个 URL 做 redactURL。
// net/http 的错误会以 Get "<地址>": ... 的形式带出整串地址，它只把密码换成 ***，
// query 原样保留。
func redactURLsInText(s string) string {
	return urlInTextPattern.ReplaceAllStringFunc(s, redactURL)
}
