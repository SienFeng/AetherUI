package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/geodata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/xray"
)

// 路由验证的纯函数部分：样例选取、期望比对、结论合并。不碰数据库与核心，
// 便于用表驱动测试把「没有证据就不给绿色」这条约束钉住。

// 核心级状态（CoreState.State）。
const (
	coreStateOK         = "ok"
	coreStatePending    = "pending"
	coreStateNotRunning = "not_running"
	coreStateBusy       = "busy"
	coreStateError      = "error"
)

// 用户行的最终结论（UserVerify.Verdict）。
const (
	verdictVerified     = "verified"
	verdictMismatch     = "mismatch"
	verdictPending      = "pending"
	verdictUnverifiable = "unverifiable"
)

// RuleVerifyResult 是一条规则的验证结果，json 字段名与设计文档 §7 一致。
type RuleVerifyResult struct {
	RuleId    int              `json:"ruleId"`
	CheckedAt int64            `json:"checkedAt"`
	Expected  ExpectedOutbound `json:"expected"`
	Core      CoreState        `json:"core"`
	AccessLog AccessLogState   `json:"accessLog"`
	Truncated bool             `json:"truncated"`
	Users     []UserVerify     `json:"users"`
	// ApplyGen 是取快照那一刻的应用代次（XrayApplyGeneration），审计缓存据此判新旧。
	ApplyGen uint64 `json:"applyGen"`
}

// ExpectedOutbound 是规则期望的出口。Action 为 proxy|block|direct。
type ExpectedOutbound struct {
	Action string `json:"action"`
	Tag    string `json:"tag"`
	Label  string `json:"label"`
}

// CoreState 是核心级前置状态：ok|pending|not_running|busy|error。
type CoreState struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

// AccessLogState 描述访问日志证据是否可用。
// Error 非空表示读取失败（开关读不出、匹配器建不出），与「没有数据」区分开。
type AccessLogState struct {
	Enabled     bool   `json:"enabled"`
	WindowHours int    `json:"windowHours"`
	Error       string `json:"error,omitempty"`
}

// UserVerify 是图上一个用户（入站）的验证结果。
type UserVerify struct {
	InboundId int    `json:"inboundId"`
	Name      string `json:"name"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
	// Disabled 表示该入站已停用：规则不会对它生效，规则级结论不把它算进去。
	Disabled bool           `json:"disabled,omitempty"`
	Samples  []SampleResult `json:"samples"`
	Log      *LogEvidence   `json:"log"`
}

// SampleResult 是对一个样例目标做 TestRoute 的结果。
type SampleResult struct {
	Target      string `json:"target"`
	ActualTag   string `json:"actualTag"`
	ActualLabel string `json:"actualLabel"`
	Match       bool   `json:"match"`
	HitByRule   bool   `json:"hitByRule"`
	Error       string `json:"error"`
	// ShadowedBy 非 nil：这个不符的样例被排在本规则之前的另一条规则抢先命中（见 attributeShadows）。
	ShadowedBy *ShadowRule `json:"shadowedBy,omitempty"`
}

// ShadowRule 是抢先命中某类连接的、在核心里排在本规则之前的另一条分流规则。
type ShadowRule struct {
	RuleId int    `json:"ruleId"`
	Remark string `json:"remark"`
}

// LogEvidence 是访问日志里属于本规则的真实连接统计。
// Error 非空表示该用户的日志查询失败，其余字段此时无意义（不是「没有连接」）。
type LogEvidence struct {
	Matched  int64 `json:"matched"`
	Rejected int64 `json:"rejected"`
	// GeoBlocked 是来源 IP 在入站地区限制之外、被地区规则送进黑洞的连接数。地区规则排在
	// 所有分流规则之前，这些连接从未轮到本规则，不算走错，单独列出（见 geoBlockMatcher）。
	GeoBlocked int64 `json:"geoBlocked"`
	// NoRuleHit 是核心没有命中任何规则（访问日志里的 " >> "）的连接数。日志记的是客户端
	// 请求的目标，核心分流看的是嗅探出的域名；目标属于本规则却没命中任何规则，说明核心
	// 看到的是另一个域名，不算本规则走错（见 accesslog.Entry.NoRuleHit）。
	NoRuleHit int64      `json:"noRuleHit"`
	Truncated bool       `json:"truncated"`
	Others    []LogOther `json:"others"`
	Error     string     `json:"error,omitempty"`
}

// LogOther 是走了非期望出站的连接分组。
type LogOther struct {
	Route    string   `json:"route"`
	Label    string   `json:"label"`
	Count    int64    `json:"count"`
	LastAt   int64    `json:"lastAt"`
	Examples []string `json:"examples"`
	// ShadowedBy 非 nil：这组连接（按示例判定）被排在本规则之前的另一条规则抢先命中。
	ShadowedBy *ShadowRule `json:"shadowedBy,omitempty"`
}

// sampleTarget 是一个待 TestRoute 的目标：域名与 IP 二选一。
type sampleTarget struct {
	Domain  string
	IP      net.IP
	Display string
}

// sampleTargets 对一个域名组取样：先域名后 IP，总数不超过 perGroup。
// 组里有可用的 IP 条目且 perGroup ≥ 2 时给 IP 预留一个名额：否则一个域名够多的组
// 永远只测到域名那一半，buildRule 为它生成的那条 IP 规则从来没被验证过。
// 只有 full:/domain: 能还原出确定的域名；keyword/regexp/dotless/geosite/ext 推不出
// 可靠样例，geoip:/ext:/带 ! 取反的 IP 条目同理，一律跳过而不是猜一个。
//
// IP 样例绕开 geoip:private（见 publicSampleIP）。第二个返回值报告是否有 IP 条目
// 因为整段都在私网里而一个样例都没取：上层据此把「无法验证」的原因说清楚。
func sampleTargets(domains, cidrs []string, perGroup int) ([]sampleTarget, bool) {
	ips := []sampleTarget{}
	privateOnly := false
	isPrivate := geodata.GetPrivateIPMatcher().Match
	for _, c := range cidrs {
		if len(ips) >= perGroup {
			break
		}
		ip, allPrivate := publicSampleIP(strings.TrimSpace(c), isPrivate)
		if ip != nil {
			ips = append(ips, sampleTarget{IP: ip, Display: ip.String()})
		} else if allPrivate {
			privateOnly = true
		}
	}
	domainLimit := perGroup
	if len(ips) > 0 && perGroup >= 2 {
		domainLimit = perGroup - 1
	}
	out := []sampleTarget{}
	for _, d := range domains {
		if len(out) >= domainLimit {
			break
		}
		var host string
		switch {
		case strings.HasPrefix(d, "full:"):
			host = strings.TrimPrefix(d, "full:")
		case strings.HasPrefix(d, "domain:"):
			host = strings.TrimPrefix(d, "domain:")
		}
		// 样例要长得像真实流量：嗅探出的域名是 punycode。新录入的条目已经存成 xn--
		// 形态；升级前存进库的中文域名在这里转一下，核心里那条未转换的规则命中不了，
		// 结论如实是「不符」，而不是拿同一个中文字符串自问自答亮绿灯。
		if ascii, err := domainToASCII(strings.TrimSpace(host)); err == nil && ascii != "" {
			out = append(out, sampleTarget{Domain: ascii, Display: ascii})
		}
	}
	for _, t := range ips {
		if len(out) >= perGroup {
			break
		}
		out = append(out, t)
	}
	return out, privateOnly
}

// publicSampleIP 在一个 IP 条目里挑一个不落在 geoip:private 的地址作样例。返回
// （样例, 整段都在私网里）：解析不了的条目（取反、geoip:、ext:）返回 (nil, false)。
//
// 模板里的 geoip:private → blocked 排在所有注入规则之前（注入不变量 1：模板的安全
// 规则保持更高优先级），落在私网里的样例必然先被它截走，好好的规则于是被判「不符」。
// 0.0.0.0/0、::/0 的「网络地址 +1」恰好是 0.0.0.1、::1，正中这一条。整段都是私网时
// 本规则对这些地址本来就轮不到，拿任何样例去测都不说明它配得对不对。
//
// 先试网络地址 +1（原来的取法，大多数段它就不是私网）；是私网时再依次试「它所在的
// /bits-1、/bits-2……/(段前缀+1) 块的下一块起点」。私网表由按 CIDR 对齐的块组成，
// first 所在那块私网的下一块起点必在这串候选里，由细到粗第一个不是私网的，就是段内
// 紧跟在那块私网之后的地址——不必另抄一份私网段列表来算边界。
//
// 私网判定用 xray 自己的 geodata.GetPrivateIPMatcher（common/geodata/consts.go）：
// 与仓库种子 geoip.dat 的 PRIVATE 类别逐段一致（含 RFC 5737 的三个文档段），而它不必
// 读文件。不手写判定，理由同 groupMatcher。
func publicSampleIP(entry string, isPrivate func(net.IP) bool) (net.IP, bool) {
	first := firstUsableIP(entry)
	if first == nil {
		return nil, false
	}
	if !isPrivate(first) {
		return first, false
	}
	_, ipNet, err := net.ParseCIDR(entry)
	if err != nil {
		return nil, true // 单个地址，且是私网
	}
	ones, bits := ipNet.Mask.Size()
	for prefix := bits; prefix > ones; prefix-- {
		next := nextBlockStart(first, bits-prefix)
		if next == nil || !ipNet.Contains(next) {
			continue
		}
		if !isPrivate(next) {
			return next, false
		}
	}
	return nil, true
}

// nextBlockStart 返回 ip 所在的、低 hostBits 位为主机位的那一块的下一块起点；
// 已到地址空间末尾时返回 nil。
func nextBlockStart(ip net.IP, hostBits int) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	n := len(out)
	full, rem := hostBits/8, hostBits%8
	for i := 0; i < full && i < n; i++ {
		out[n-1-i] = 0
	}
	if full < n && rem > 0 {
		out[n-1-full] &^= byte(1<<rem - 1)
	}
	inc := uint16(1) << rem
	for idx := n - 1 - full; idx >= 0; idx-- {
		sum := uint16(out[idx]) + inc
		out[idx] = byte(sum)
		if sum < 256 {
			return out
		}
		inc = 1
	}
	return nil
}

// firstUsableIP 返回 IP 段内第一个可用地址：网络地址 +1，/32、/128 与单个 IP 取自身。
// 解析不了（取反、geoip:、ext: 等）返回 nil。
func firstUsableIP(entry string) net.IP {
	if entry == "" || strings.HasPrefix(entry, "!") {
		return nil
	}
	if ip := net.ParseIP(entry); ip != nil {
		return ip
	}
	_, ipNet, err := net.ParseCIDR(entry)
	if err != nil {
		return nil
	}
	ones, bits := ipNet.Mask.Size()
	first := make(net.IP, len(ipNet.IP))
	copy(first, ipNet.IP)
	if ones == bits {
		return first
	}
	for i := len(first) - 1; i >= 0; i-- {
		first[i]++
		if first[i] != 0 {
			break
		}
	}
	return first
}

// judgeSample 比对一次 TestRoute 的结果与期望。noMatch 表示核心返回 ErrNoClue
// （没有任何规则命中，走默认出站）。
//   - proxy/block：实际 tag 必须严格等于期望 tag。
//   - direct：实际出口是默认出站即算一致（默认 tag、计量 tag、未命中三种），
//     但后两种不是由本规则命中，hitByRule=false，调用方据此在明细里注明。
func judgeSample(exp ExpectedOutbound, actualTag string, noMatch bool, defaultTag string) (match, hitByRule bool) {
	hitByRule = !noMatch && !model.IsMeterTag(actualTag)
	if exp.Action == model.ActionDirect {
		return noMatch || model.IsMeterTag(actualTag) || actualTag == defaultTag, hitByRule
	}
	return !noMatch && actualTag == exp.Tag, hitByRule
}

// routeHead 取访问日志 Route 里 " -> " / " >> " 之前的第一段（经中间出站时 Route 是整串）。
func routeHead(route string) string {
	for _, sep := range []string{" -> ", " >> "} {
		if i := strings.Index(route, sep); i >= 0 {
			route = route[:i]
		}
	}
	return strings.TrimSpace(route)
}

// sniffingBlind 判断核心在路由阶段是否拿不到这个入站的目标域名：sniffing 关闭
// （或 destOverride 不含 http/tls、JSON 解析不了），且不是 socks/http 代理入站。
// 逐条镜像 routing.html 的 sniffingOff 与 isProxyInbound（ruleIssue 用它们给显式
// 指定的入站报「永远不会命中」）；两边必须一起改，否则页面说「会命中」而这里不给绿，
// 或反过来。TestRoute 把 TargetDomain 直接交给 PickRoute、不经过嗅探，所以这一层
// 只能由面板自己补上——否则全局规则会对这类入站亮出没有依据的绿灯。
//
// forceSniffing 是「补全流量识别」开关：开着时按生成配置同一个函数（forcedSniffing）先补全再判断，
// 否则关了嗅探的入站在核心里明明拿得到域名，这里却剔掉域名样例、页面还标「永远不会命中」。
func sniffingBlind(in *model.Inbound, forceSniffing bool) bool {
	if in.Protocol == "socks" || in.Protocol == model.Http {
		return false
	}
	raw := in.Sniffing
	if forceSniffing {
		raw = forcedSniffing(string(in.Protocol), raw)
	}
	if raw == "" {
		raw = "{}"
	}
	var sniffing struct {
		Enabled      bool     `json:"enabled"`
		DestOverride []string `json:"destOverride"`
	}
	if err := json.Unmarshal([]byte(raw), &sniffing); err != nil {
		return true
	}
	if !sniffing.Enabled {
		return true
	}
	for _, d := range sniffing.DestOverride {
		if d == "http" || d == "tls" {
			return false
		}
	}
	return true
}

// ipOnlyTargets 只保留 IP 样例：IP 条件匹配的是连接目标 IP，与嗅探无关。
func ipOnlyTargets(targets []sampleTarget) []sampleTarget {
	out := []sampleTarget{}
	for _, t := range targets {
		if t.IP != nil {
			out = append(out, t)
		}
	}
	return out
}

// combineVerdict 把核心级状态、样例结果与访问日志合并成用户行的最终结论。
// 判定顺序：
//  1. 核心处于 pending：一律 pending。此时核心仍持有旧配置，样例与配置不符是必然的，
//     报「不符」是误报；明细仍保留在结果里。
//  2. 有任一样例不符或日志有其他出站：mismatch。
//  3. 核心不可用 / 没有样例 / 有样例出错：unverifiable。
//  4. 其余（样例全部符合）：verified。
//
// 访问日志只能把结论推向 mismatch 或给 verified 补充说明，永远不能单独产生 verified。
func combineVerdict(coreState string, samples []SampleResult, log *LogEvidence) (verdict, reason string) {
	if coreState == coreStatePending {
		return verdictPending, "配置已保存，正在等待下发到核心（最多约 10 秒）"
	}

	for _, s := range samples {
		if s.Error == "" && !s.Match {
			return verdictMismatch, fmt.Sprintf("核心实际出站与配置不符（样例 %s）", s.Target)
		}
	}
	if log != nil && len(log.Others) > 0 {
		return verdictMismatch, "访问日志里有连接走了其他出站"
	}

	switch coreState {
	case coreStateNotRunning:
		return verdictUnverifiable, "xray 未运行"
	case coreStateBusy:
		return verdictUnverifiable, "核心正在更新配置，请稍后重新验证"
	case coreStateOK:
	default:
		return verdictUnverifiable, "核心验证出错，无法验证"
	}

	if len(samples) == 0 {
		return verdictUnverifiable, "本规则的域名都是关键词/正则/geosite 写法，推不出可测试的域名"
	}
	for _, s := range samples {
		if s.Error != "" {
			// 有样例没验成就不能说「全部符合」，宁可灰也不给绿。
			return verdictUnverifiable, "部分样例验证失败：" + s.Error
		}
	}

	if log != nil && log.Matched > 0 {
		return verdictVerified, fmt.Sprintf("近 24h %d 次真实连接", log.Matched)
	}
	return verdictVerified, ""
}

// logOtherMaxExamples 是每个「其他出站」分组保留的示例连接数。
const logOtherMaxExamples = 5

// groupMatcher 用 xray 自己的匹配器判断一个访问日志目标是否属于某个域名组。
// 归属判定不手写：写法（domain:/full:/keyword:/regexp:/geosite:…）与 geoip 的语义
// 只有交给 xray 才会和核心实际路由一致。
type groupMatcher struct {
	domain *router.DomainMatcher
	ip     *router.IPMatcher
}

// groupRules 是一组条目交给 xray 解析后的规则，构建匹配器之前的中间结果。
// 匹配器不在调用处现建：小规则取自 smallMatchers、大规则取自常驻槽（largeMatchers），
// 两者都经 buildStampedGroupMatcher 解析、构建。
// xray 的解析逐条一一对应（失败即报错），某一侧有条目就有同样多条规则。
type groupRules struct {
	domain []*geodata.DomainRule
	ip     []*geodata.IPRule
}

func parseGroupRules(domains, cidrs []string) (*groupRules, error) {
	xray.EnsureAssetLocation()
	r := &groupRules{}
	var err error
	if len(domains) > 0 {
		// 默认类型 Domain_Substr 与 infra/conf 解析 routing 规则时一致（裸串按子串）。
		if r.domain, err = geodata.ParseDomainRules(domains, geodata.Domain_Substr); err != nil {
			return nil, fmt.Errorf("解析域名条件: %w", err)
		}
	}
	if len(cidrs) > 0 {
		if r.ip, err = geodata.ParseIPRules(cidrs); err != nil {
			return nil, fmt.Errorf("解析 IP 条件: %w", err)
		}
	}
	return r, nil
}

// datFiles 返回规则引用的 dat 文件（geosite:/geoip:/ext: 写法），去重、按首次出现的顺序。
func (r *groupRules) datFiles() []string {
	var files []string
	seen := map[string]bool{}
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	for _, d := range r.domain {
		if g, ok := d.Value.(*geodata.DomainRule_Geosite); ok {
			add(g.Geosite.File)
		}
	}
	for _, ip := range r.ip {
		if g := ip.GetGeoip(); g != nil {
			add(g.File)
		}
	}
	return files
}

func (r *groupRules) build() (*groupMatcher, error) {
	m := &groupMatcher{}
	var err error
	if len(r.domain) > 0 {
		if m.domain, err = router.NewDomainMatcher(r.domain); err != nil {
			return nil, fmt.Errorf("构造域名匹配器: %w", err)
		}
	}
	if len(r.ip) > 0 {
		if m.ip, err = router.NewIPMatcher(r.ip, router.MatcherAsType_Target); err != nil {
			return nil, fmt.Errorf("构造 IP 匹配器: %w", err)
		}
	}
	return m, nil
}

// Match 判断访问日志里的目标（host:port 或 [v6]:port）是否属于该组。
func (m *groupMatcher) Match(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		if m.ip == nil {
			return false
		}
		// IPMatcher 只有 Apply(routing.Context)，借 command 包的适配器塞入目标 IP。
		ctx := routercmd.AsRoutingContext(&routercmd.RoutingContext{TargetIPs: [][]byte{ip}})
		return m.ip.Apply(ctx)
	}
	return m.domain != nil && m.domain.ApplyDomain(host)
}

// buildLogEvidence 把日志分组折算成证据：只统计属于本组的行；被拒绝的计入 Rejected，
// 其余按 routeHead 与期望出站比对，符合计入 Matched。不符的行先排除两类不是本规则
// 造成的：被入站地区限制拦下的（geoBlocked 判来源，nil 表示不做此判断）计入 GeoBlocked，
// 核心没命中任何规则的计入 NoRuleHit；剩下的按出站分组进 Others。
func buildLogEvidence(rows []TargetRouteCount, m *groupMatcher, exp ExpectedOutbound, defaultTag string, labelOf func(tag string) string, geoBlocked func(sourceIP string) bool) LogEvidence {
	ev := LogEvidence{Others: []LogOther{}}
	byRoute := map[string]*LogOther{}
	var order []string
	for _, r := range rows {
		if !m.Match(r.Target) {
			continue
		}
		if !r.Accepted {
			ev.Rejected += r.Count
			continue
		}
		head := routeHead(r.Route)
		if match, _ := judgeSample(exp, head, false, defaultTag); match {
			ev.Matched += r.Count
			continue
		}
		if head == model.BlockOutboundTag && geoBlocked != nil && geoBlocked(r.SourceIP) {
			ev.GeoBlocked += r.Count
			continue
		}
		if r.NoRuleHit {
			ev.NoRuleHit += r.Count
			continue
		}
		o := byRoute[head]
		if o == nil {
			o = &LogOther{Route: head, Label: labelOf(head), Examples: []string{}}
			byRoute[head] = o
			order = append(order, head)
		}
		o.Count += r.Count
		if r.LastAt > o.LastAt {
			o.LastAt = r.LastAt
		}
		if len(o.Examples) < logOtherMaxExamples && !hasExample(o.Examples, r.Target) {
			o.Examples = append(o.Examples, r.Target)
		}
	}
	for _, k := range order {
		ev.Others = append(ev.Others, *byRoute[k])
	}
	sort.SliceStable(ev.Others, func(i, j int) bool { return ev.Others[i].Count > ev.Others[j].Count })
	return ev
}

func hasExample(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// 以下是编排部分：读库、取样、调核心、查访问日志，最后交给上面的纯函数下结论。
// 全程只读：TestRoute 不发布结果（PublishResult=false），不重启、不写配置、不写库。

// 规模与超时（设计 §3.6、§4.2）。
const (
	verifyRPCTimeout         = 3 * time.Second
	verifyBudget             = 20 * time.Second
	verifyAllBudget          = 60 * time.Second // 后台全量验证一整轮（审计设计 §3.3）
	verifyPerGroupSamples    = 2
	verifyPerUserSamples     = 6
	verifyGlobalInboundLimit = 50
	verifyLogWindowHours     = 24
	verifyLogGroupLimit      = 5000
)

// 结果里的固定文案。
const (
	labelBlock           = "封禁"
	labelDirect          = "直连"
	labelDefaultOutbound = "直连（默认出站）"
	labelNoMatch         = "默认出站（未命中任何规则）"
	labelUnknownOutbound = "未知出站"
	reasonSniffingOff    = "该入站未开启 sniffing（http/tls），核心在路由阶段拿不到域名，域名规则对其不会命中"
	reasonSniffingIPOnly = "仅验证了 IP 条件：该入站未开启 sniffing（http/tls），域名条件对其不会命中"
	reasonPrivateOnly    = "IP 段都落在 geoip:private：默认模板里屏蔽私网的规则排在所有分流规则之前，这些地址的样例会先被它截走，无法据此验证本规则"
	sampleErrTimeout     = "超时"
)

// routeFunc 是对核心做一次 TestRoute 的函数，签名同 XrayAPI.TestRoute。
type routeFunc func(ctx context.Context, inboundTag, domain string, ip net.IP) (tag string, noMatch bool, err error)

// VerifyAll 的两种「本轮不出结果」：调用方据此跳过本轮、不动缓存，下一轮重试。
// ErrVerifyTruncated 不同：整轮预算用完，已跑完的规则结果照常返回（有效），但这一轮
// 不算完整，调用方不能把它当作一次成功的审计（下一轮要重试剩下的规则）。
var (
	ErrVerifyBusy      = errors.New("已有验证在进行")
	ErrCoreNotReady    = errors.New("xray 核心未就绪")
	ErrVerifyTruncated = errors.New("整轮预算用完，部分规则本轮没有新结果")
)

// verifyMu 保证同一时刻只有一个验证在跑：管理员连点时不把核心打满。
// 与包级 lock 无关——lock 只由 RuntimeSnapshot 瞬时持有。
var verifyMu sync.Mutex

// RoutingVerifyService 对一条分流规则做「核心实时路由 + 访问日志」两路核对。
// 零值可用：字段为 nil/0 时用真实实现与默认值，测试可注入伪造的 route/runtime。
type RoutingVerifyService struct {
	route              routeFunc
	runtime            func() XrayRuntime
	now                func() time.Time
	rpcTimeout, budget time.Duration

	// 访问日志两处读取的注入点，nil 时用真实实现；测试用它制造读取失败。
	accessLogEnabled func() (bool, error)
	groupLogs        func(inboundId int, sinceMs int64, limit int) ([]TargetRouteCount, bool, error)

	// 合成当前配置的注入点（只在需要默认出站 tag 时调用），nil 时用 GetXrayConfig。
	xrayConfig func() (*xray.Config, error)
}

// verifyUser 是待验证的一个用户（入站）。
type verifyUser struct {
	inbound  *model.Inbound
	disabled bool
}

// verifyPlan 是从库里推出的、与核心无关的验证计划。
type verifyPlan struct {
	expected   ExpectedOutbound
	defaultTag string
	targets    []sampleTarget
	domains    []string // 所有组合并后的域名条目（访问日志归属判定用）
	cidrs      []string // 所有组合并后的 IP 条目
	users      []verifyUser
	truncated  bool
	labelOf    func(tag string) string
	groupKey   string // 实际参与的组 id 序列，访问日志匹配器按它在一轮内复用
	groupIds   []int  // 实际参与的组 id，与 groupKey 同序
	// large：合并后条目数超过 auditLargeThreshold（与异常扫描的大小规则同一口径）。
	// 大规则的组条目与匹配器不在一轮内常驻，用完即释放（见 verifyOne）。
	large bool
	// privateOnly：有组的 IP 条目整段落在 geoip:private 里没取到样例，用来解释
	// 「一个样例都没有」的无法验证（见 reasonPrivateOnly）。
	privateOnly bool
}

// Verify 验证一条规则。规则不存在返回 error；其余情况（含规则停用、不会写进配置、
// 核心不可用）都返回结果，由 Core.State 说明原因。
//
// 与 VerifyAll 走同一段 prepareShared + verifyOne，按需与后台的口径不会漂移。
// 结果带核心证据（核心在跑、不忙、控制面连得上、没有超出预算）时写回审计缓存，
// 页面与列表看到同一份。
func (s *RoutingVerifyService) Verify(ruleId int) (*RuleVerifyResult, error) {
	now := s.nowTime()
	if !verifyMu.TryLock() {
		res := newRuleVerifyResult(ruleId, now)
		res.Core = CoreState{State: coreStateBusy, Message: "已有验证在进行，请稍后"}
		return res, nil
	}
	defer verifyMu.Unlock()

	rule, err := (&RoutingRuleService{}).Get(ruleId)
	if err != nil {
		return nil, err
	}
	if !rule.Enable {
		res := newRuleVerifyResult(ruleId, now)
		res.Core = CoreState{State: coreStateError, Message: "规则已停用，不会写进核心配置"}
		return res, nil
	}

	sh, err := s.prepareShared(s.snapshot())
	if err != nil {
		res := newRuleVerifyResult(ruleId, now)
		res.Core = CoreState{State: coreStateError, Message: err.Error()}
		return res, nil
	}
	defer sh.close()

	budgetCtx, cancel := context.WithTimeout(context.Background(), s.budgetOr(verifyBudget))
	defer cancel()
	res := s.verifyOne(budgetCtx, sh, rule, now)
	// 与 VerifyAll 同口径：预算用完时剩下的样例只是「超时」，不拿它顶掉缓存里的旧结论。
	if sh.ready && budgetCtx.Err() == nil {
		storeVerifyResult(res)
	}
	return res, nil
}

// VerifyAll 对所有启用规则跑一轮验证，返回各规则结果与本轮使用的应用代次。
// 只读、与 Verify 互斥：拿不到 verifyMu（用户正在手动验证）或核心忙返回 ErrVerifyBusy；
// 核心未运行、控制面连不上、或本轮有 RPC 报 Unavailable（核心刚拉起还没监听，或中途
// 退出）返回 ErrCoreNotReady。这些错误下不返回任何结果，调用方据此不动缓存。
//
// 整轮共用一个预算与一个 API 连接；入站、出站映射、节点、访问日志开关、小规则的域名组
// 条目与样例在一轮内只算一次。预算用完时，没跑完的规则不进结果（保留旧结果，
// 不用一份全是「超时」的结论顶掉它），已跑完的照常返回，同时返回 ErrVerifyTruncated。
//
// 规则的处理顺序：缓存里还没有当前代次结果的排前面（上一轮被预算截掉的规则下一轮先跑，
// 不会每轮都卡在同一处）；其余按引用的域名组排，同组序列的规则相邻，本轮那一个小规则
// 匹配器的索引（matcherKey）因此能复用。小规则的匹配器取自全进程共用的 smallMatchers；
// 大规则的取自常驻槽，一轮内同一时刻至多一个存活。
func (s *RoutingVerifyService) VerifyAll() (map[int]*RuleVerifyResult, uint64, error) {
	if !verifyMu.TryLock() {
		return nil, 0, ErrVerifyBusy
	}
	defer verifyMu.Unlock()

	rt := s.snapshot()
	switch {
	case rt.Busy:
		return nil, 0, ErrVerifyBusy
	case !rt.Running:
		return nil, 0, ErrCoreNotReady
	}

	rules, err := (&RoutingRuleService{}).GetEnabled()
	if err != nil {
		return nil, 0, fmt.Errorf("读取分流规则失败：%w", err)
	}
	sh, err := s.prepareShared(rt)
	if err != nil {
		return nil, 0, err
	}
	defer sh.close()
	if !sh.ready {
		return nil, 0, fmt.Errorf("%w：%s", ErrCoreNotReady, sh.core.Message)
	}

	// grpc 连接即使就绪，核心也可能在本轮中途退出：任何一次 RPC 报 Unavailable，
	// 本轮的灰色结论就不是「核心对规则的判断」，而是「核心不在」，整轮不出结果。
	unavailable := false
	if inner := sh.route; inner != nil {
		sh.route = func(ctx context.Context, inboundTag, domain string, ip net.IP) (string, bool, error) {
			tag, noMatch, err := inner(ctx, inboundTag, domain, ip)
			if err != nil && status.Code(err) == codes.Unavailable {
				unavailable = true
			}
			return tag, noMatch, err
		}
	}

	orderRulesForRound(rules, rt.ApplyGen)
	now := s.nowTime()
	budgetCtx, cancel := context.WithTimeout(context.Background(), s.budgetOr(verifyAllBudget))
	defer cancel()
	out := make(map[int]*RuleVerifyResult, len(rules))
	truncated := false
	for i, rule := range rules {
		res := s.verifyOne(budgetCtx, sh, rule, now)
		if unavailable {
			return nil, 0, fmt.Errorf("%w：核心控制面不可用", ErrCoreNotReady)
		}
		if budgetCtx.Err() != nil {
			// 这条规则跑到一半预算就用完了，剩下的样例被记成「超时」，不是有效结论。
			logger.Warning("路由全量验证：整轮预算用完，", len(rules)-i, "条规则本轮没有新结果")
			truncated = true
			break
		}
		out[rule.Id] = res
	}
	if truncated {
		return out, rt.ApplyGen, ErrVerifyTruncated
	}
	return out, rt.ApplyGen, nil
}

// orderRulesForRound 排定一轮全量验证的处理顺序（原地、稳定），理由见 VerifyAll。
// 组序列直接用库里的 DomainGroupIds 原文排：同一组集合编码后逐字节相同（升序去重存储）。
func orderRulesForRound(rules []*model.RoutingRule, applyGen uint64) {
	current := make(map[int]bool, len(rules))
	for _, r := range rules {
		if res, ok := cachedVerifyResult(r.Id); ok && res.ApplyGen == applyGen {
			current[r.Id] = true
		}
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if ci, cj := current[rules[i].Id], current[rules[j].Id]; ci != cj {
			return !ci
		}
		return rules[i].DomainGroupIds < rules[j].DomainGroupIds
	})
}

func (s *RoutingVerifyService) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *RoutingVerifyService) snapshot() XrayRuntime {
	if s.runtime != nil {
		return s.runtime()
	}
	return (&XrayService{}).RuntimeSnapshot()
}

func (s *RoutingVerifyService) budgetOr(def time.Duration) time.Duration {
	if s.budget > 0 {
		return s.budget
	}
	return def
}

func newRuleVerifyResult(ruleId int, now time.Time) *RuleVerifyResult {
	return &RuleVerifyResult{
		RuleId:    ruleId,
		CheckedAt: now.UnixMilli(),
		AccessLog: AccessLogState{WindowHours: verifyLogWindowHours},
		Users:     []UserVerify{},
	}
}

// verifyShared 是一次验证（单条或一整轮）里与具体规则无关、只需算一次的上下文。
// 只在持有 verifyMu 的单个 goroutine 里使用，惰性缓存不加锁。
type verifyShared struct {
	rt       XrayRuntime
	core     CoreState // 核心级状态；控制面连不上时为 error 并带原因
	route    routeFunc // core 为 ok/pending 时非 nil
	closeAPI func()
	// ready 表示核心在跑、不忙、控制面就绪：本次结果有核心证据，可以写进审计缓存。
	ready      bool
	rpcTimeout time.Duration

	inboundTags map[int]string // 启用入站 id→tag，与生成期同一套过滤
	inbounds    []*model.Inbound
	usable      map[int]string // buildOutbounds 实际写进配置的节点 id→tag
	nodes       map[int]*model.OutboundNode
	remarkByTag map[string]string // 含停用节点：访问日志里可能还留着它们的 tag

	xrayConfig    func() (*xray.Config, error)
	cfgDefaultTag *string // 惰性：只有 direct 规则且快照里没有默认 tag 时才去合成配置

	logEnabled bool
	logErr     string
	// forceSniffing：「补全流量识别」开关，见 sniffingBlind。
	forceSniffing bool
	groupLogs     func(inboundId int, sinceMs int64, limit int) ([]TargetRouteCount, bool, error)
	// geoBlock 判断封禁连接是否来自入站地区限制之外；惰性构建，一轮内只建一次。
	geoBlock      *geoBlockMatcher
	geoBlockReady bool

	groups map[int]*groupConditions
	// 本轮最近用到的小规则匹配器（取自全进程共用的 smallMatchers，与异常扫描的计划是同一个
	// 实例）。只是省掉同组序列的规则重复给上万条条目算内容哈希：VerifyAll 把同组序列的规则
	// 排在一起。大规则的匹配器不在这里，取自全进程共用的常驻槽（largeMatchers）。
	matcherKey string
	matcherEnt *matcherEntry
	// largeRelease 是本轮手里那一个大匹配器的放手函数（至多一个，见 acquireLarge）。
	largeRelease func()
	logRows      map[logRowsKey]*logRowsEntry

	// 抢先命中归因（attributeShadows）用：核心里的规则顺序与各候选规则的匹配器。
	// 小规则的取自 smallMatchers（这里只是本轮的索引）；大规则取自常驻槽，同一时刻只占用
	// 一个，归因结束即放手。
	coreOrder      []*model.RoutingRule
	coreOrderErr   error
	coreOrderReady bool
	shadowSmall    map[string]*matcherEntry
	shadowLargeKey string
	shadowLargeEnt *matcherEntry
	shadowLargeIds []int
}

// logRowsKey 是入站日志分组的缓存键：窗口起点随规则的忽略时刻而不同，必须进键。
type logRowsKey struct {
	inboundId int
	sinceMs   int64
}

// groupConditions 是一个域名组合并后的条目及其样例；skip 表示 buildRule 会剔掉这个组。
type groupConditions struct {
	domains, cidrs []string
	targets        []sampleTarget
	// privateOnly：有 IP 条目整段都在 geoip:private 里、没取到样例（见 sampleTargets）。
	privateOnly bool
	skip        bool
}

type matcherEntry struct {
	m   *groupMatcher
	err error
}

type logRowsEntry struct {
	rows      []TargetRouteCount
	truncated bool
	err       error
}

func (sh *verifyShared) close() {
	sh.releaseLargeLease()
	if sh.closeAPI != nil {
		sh.closeAPI()
	}
}

// acquireLarge 从常驻槽取大匹配器，先放掉本轮手里的那一个：同一个调用方握着一个再去等
// 另一个会把自己卡死（槽在换之前要等所有人放手）。
func (sh *verifyShared) acquireLarge(domains, cidrs []string) *matcherEntry {
	sh.releaseLargeLease()
	e, release := largeMatchers.acquire(domains, cidrs)
	sh.largeRelease = release
	return e
}

func (sh *verifyShared) releaseLargeLease() {
	if sh.largeRelease != nil {
		sh.largeRelease()
		sh.largeRelease = nil
	}
}

// prepareShared 读库与连核心控制面。读库失败返回 error（原因已带前缀）；控制面连不上
// 不算 error，而是写进 core（State=error），由调用方决定是给出结果还是报未就绪。
func (s *RoutingVerifyService) prepareShared(rt XrayRuntime) (*verifyShared, error) {
	inbounds, err := (&InboundService{}).GetAllInbounds()
	if err != nil {
		return nil, fmt.Errorf("读取入站失败：%w", err)
	}
	sort.Slice(inbounds, func(i, j int) bool { return inbounds[i].Id < inbounds[j].Id })

	// 传空模板只为拿到「实际会写进配置的节点」映射，与生成期同一套过滤。
	_, usable, _, err := (&RoutingInjector{}).buildOutbounds(nil)
	if err != nil {
		return nil, fmt.Errorf("读取出站节点失败：%w", err)
	}
	nodeList, err := (&OutboundNodeService{}).GetAll()
	if err != nil {
		return nil, fmt.Errorf("读取出站节点失败：%w", err)
	}

	sh := &verifyShared{
		rt:          rt,
		core:        coreStateOf(rt),
		inbounds:    inbounds,
		inboundTags: enabledInboundTags(inbounds),
		usable:      usable,
		nodes:       make(map[int]*model.OutboundNode, len(nodeList)),
		remarkByTag: make(map[string]string, len(nodeList)),
		xrayConfig:  s.xrayConfig,
		groupLogs:   s.groupLogs,
		groups:      map[int]*groupConditions{},
		logRows:     map[logRowsKey]*logRowsEntry{},
		shadowSmall: map[string]*matcherEntry{},
		rpcTimeout:  s.rpcTimeout,
	}
	sh.forceSniffing = (&SettingService{}).GetForceSniffing()
	for _, n := range nodeList {
		sh.nodes[n.Id] = n
		sh.remarkByTag[n.Tag] = nodeLabel(n)
	}
	if sh.xrayConfig == nil {
		sh.xrayConfig = (&XrayService{}).GetXrayConfig
	}
	if sh.groupLogs == nil {
		sh.groupLogs = (&AccessLogService{}).GroupByTargetRoute
	}
	if sh.rpcTimeout <= 0 {
		sh.rpcTimeout = verifyRPCTimeout
	}

	accessLogEnabled := s.accessLogEnabled
	if accessLogEnabled == nil {
		accessLogEnabled = (&SettingService{}).GetAccessLogEnable
	}
	// 读取失败一律写进 Error，不伪装成「未开启」或「没有连接」。
	if sh.logEnabled, err = accessLogEnabled(); err != nil {
		logger.Warning("路由验证：读取访问日志开关失败：", err)
		sh.logErr = "读取访问日志开关失败：" + err.Error()
		sh.logEnabled = false
	}

	// 控制面放在最后连：前面读库失败时不留下一个没人关的连接。
	sh.route = s.route
	if sh.core.State == coreStateOK || sh.core.State == coreStatePending {
		if sh.route == nil {
			api := &xray.XrayAPI{}
			if err := api.Init(rt.APIPort); err != nil {
				sh.core = CoreState{State: coreStateError, Message: "连接核心控制面失败：" + err.Error()}
			} else {
				sh.closeAPI = api.Close
				// NewClient 是惰性的，Init 成功不代表核心在听：主动连一次，连不上就是未就绪，
				// 不让后面每个样例都报一遍 Unavailable、拼出一份全灰的「结论」。
				waitCtx, cancel := context.WithTimeout(context.Background(), sh.rpcTimeout)
				err := api.WaitReady(waitCtx)
				cancel()
				if err != nil {
					sh.core = CoreState{State: coreStateError, Message: "连接核心控制面失败：" + err.Error()}
				} else {
					sh.route = api.TestRoute
				}
			}
		}
		sh.ready = sh.core.State != coreStateError
	}
	return sh, nil
}

// configDefaultTag 取当前生成配置的默认出站 tag，一轮内只合成一次配置。
func (sh *verifyShared) configDefaultTag() string {
	if sh.cfgDefaultTag == nil {
		tag := ""
		if cfg, err := sh.xrayConfig(); err == nil {
			tag = defaultOutboundTag(cfg)
		}
		sh.cfgDefaultTag = &tag
	}
	return *sh.cfgDefaultTag
}

// geoBlocker 返回本轮的地区限制判定，一轮内只构建一次。
func (sh *verifyShared) geoBlocker() *geoBlockMatcher {
	if !sh.geoBlockReady {
		sh.geoBlockReady = true
		sh.geoBlock = newGeoBlockMatcher(sh.inbounds)
	}
	return sh.geoBlock
}

// group 返回一个域名组的条目与样例，一轮内每个组只读库、取样一次。
func (sh *verifyShared) group(gid int) *groupConditions {
	if g, ok := sh.groups[gid]; ok {
		return g
	}
	g := &groupConditions{skip: true}
	if domains, cidrs, ok := groupConditionsOf(gid); ok {
		targets, privateOnly := sampleTargets(domains, cidrs, verifyPerGroupSamples)
		g = &groupConditions{
			domains:     domains,
			cidrs:       cidrs,
			targets:     targets,
			privateOnly: privateOnly,
		}
	}
	sh.groups[gid] = g
	return g
}

// matcher 返回一条规则的访问日志匹配器：大规则取自常驻槽（releaseLarge 放手），小规则
// 取自全进程共用的 smallMatchers（按条目内容复用，审计每轮不再重建）。
// 刻意不按组各建一个再取「任一命中」：取反条目（!geoip:cn）在合并列表里是对并集取反，
// 拆成逐组再 OR 就成了对各组分别取反，语义与核心里那条合并后的规则不一致。
func (sh *verifyShared) matcher(plan *verifyPlan) *matcherEntry {
	if plan.large {
		return sh.acquireLarge(plan.domains, plan.cidrs)
	}
	if sh.matcherEnt != nil && sh.matcherKey == plan.groupKey {
		return sh.matcherEnt
	}
	// 先丢掉本轮对上一个匹配器的引用再取：少一处攥着旧对象（dat 换了的情形见 smallMatchers）。
	sh.matcherKey, sh.matcherEnt = "", nil
	e := smallMatchers.get(plan.domains, plan.cidrs)
	sh.matcherKey, sh.matcherEnt = plan.groupKey, e
	return e
}

// releaseLarge 在一条大规则判定完之后放手：放掉常驻槽的租约（匹配器留在槽里给下一次用）
// 与它引用的各组条目。之后若还有规则用到这些组，按需重读（读库的开销远小于常驻十几万条）。
func (sh *verifyShared) releaseLarge(plan *verifyPlan) {
	sh.releaseLargeLease()
	if sh.matcherKey == plan.groupKey {
		sh.matcherKey, sh.matcherEnt = "", nil
	}
	for _, gid := range plan.groupIds {
		delete(sh.groups, gid)
	}
}

// rows 返回一个入站自 sinceMs 起的分组统计；与规则无关，一轮内每个（入站, 起点）只查一次。
func (sh *verifyShared) rows(inboundId int, sinceMs int64) *logRowsEntry {
	key := logRowsKey{inboundId: inboundId, sinceMs: sinceMs}
	if e, ok := sh.logRows[key]; ok {
		return e
	}
	rows, truncated, err := sh.groupLogs(inboundId, sinceMs, verifyLogGroupLimit)
	e := &logRowsEntry{rows: rows, truncated: truncated, err: err}
	sh.logRows[key] = e
	return e
}

// verifyOne 对一条规则下结论。Verify 与 VerifyAll 共用这一份判定。
// budgetCtx 是调用方的整体预算（单条 20s，整轮 60s）。
func (s *RoutingVerifyService) verifyOne(budgetCtx context.Context, sh *verifyShared, rule *model.RoutingRule, now time.Time) *RuleVerifyResult {
	res := newRuleVerifyResult(rule.Id, now)
	res.ApplyGen = sh.rt.ApplyGen

	plan, reason := s.planRule(sh, rule)
	if reason != "" {
		res.Core = CoreState{State: coreStateError, Message: reason}
		return res
	}
	if plan.large {
		defer sh.releaseLarge(plan)
	}
	res.Expected = plan.expected
	res.Truncated = plan.truncated
	res.Core = sh.core
	canRoute := res.Core.State == coreStateOK || res.Core.State == coreStatePending

	res.AccessLog.Enabled = sh.logEnabled
	res.AccessLog.Error = sh.logErr
	var matcher *groupMatcher
	if sh.logEnabled {
		if e := sh.matcher(plan); e.err != nil {
			// 匹配器建不出来就没有日志证据，但不影响核心那一路。
			logger.Warning("路由验证：构造访问日志匹配器失败，rule id:", rule.Id, "err:", e.err)
			res.AccessLog.Error = "无法判断访问日志里哪些连接属于本规则：" + e.err.Error()
		} else {
			matcher = e.m
		}
	}

	// 窗口起点尊重「忽略此前的异常」：与列表上的异常摘要同口径，两处不会一边清零一边仍报不符。
	sinceMs := max(now.Add(-verifyLogWindowHours*time.Hour).UnixMilli(), anomalyIgnoreBefore(rule.Id))
	for _, vu := range plan.users {
		u := UserVerify{InboundId: vu.inbound.Id, Name: inboundName(vu.inbound), Samples: []SampleResult{}}
		if vu.disabled {
			// 停用的入站不在核心配置里，buildRule 也会把它剔掉，对它调 TestRoute 没有意义。
			u.Verdict, u.Reason = verdictUnverifiable, "该用户已停用，规则不会对其生效"
			u.Disabled = true
			res.Users = append(res.Users, u)
			continue
		}
		// 核心拿不到这个入站的域名时，域名样例在 TestRoute 里命中也不代表真实流量会命中，
		// 不能算作证据；IP 样例照测。
		targets := plan.targets
		blind := len(plan.domains) > 0 && sniffingBlind(vu.inbound, sh.forceSniffing)
		if blind {
			targets = ipOnlyTargets(plan.targets)
		}
		if canRoute {
			u.Samples = runSamples(budgetCtx, sh.route, sh.rpcTimeout, vu.inbound.Tag, targets, plan)
		}
		if matcher != nil {
			if e := sh.rows(vu.inbound.Id, sinceMs); e.err != nil {
				// 只带 Error 的证据：Others 为空、Matched 为 0，combineVerdict 既不会因它
				// 判 mismatch，也不会把它当作真实连接数。
				logger.Warning("路由验证：读取访问日志失败，inbound id:", vu.inbound.Id, "err:", e.err)
				u.Log = &LogEvidence{Others: []LogOther{}, Error: "读取访问日志失败：" + e.err.Error()}
			} else {
				geo, inboundId := sh.geoBlocker(), vu.inbound.Id
				ev := buildLogEvidence(e.rows, matcher, plan.expected, plan.defaultTag, plan.labelOf,
					func(src string) bool { return geo.Blocks(inboundId, src) })
				ev.Truncated = e.truncated
				u.Log = &ev
			}
		}
		u.Verdict, u.Reason = combineVerdict(res.Core.State, u.Samples, u.Log)
		if res.Core.State == coreStateError && u.Verdict == verdictUnverifiable {
			// combineVerdict 只给笼统原因，这里补上核心那一侧的具体错误。
			u.Reason = "核心验证出错：" + res.Core.Message
		}
		if blind && res.Core.State == coreStateOK {
			switch {
			case u.Verdict == verdictUnverifiable && len(u.Samples) == 0:
				u.Reason = reasonSniffingOff
			case u.Verdict == verdictVerified:
				u.Reason = reasonSniffingIPOnly
			}
		} else if plan.privateOnly && res.Core.State == coreStateOK &&
			u.Verdict == verdictUnverifiable && len(u.Samples) == 0 {
			// combineVerdict 的笼统原因只说域名推不出样例；这里是 IP 段整段落在私网里。
			u.Reason = reasonPrivateOnly
		}
		res.Users = append(res.Users, u)
	}
	if plan.large {
		// 先放掉本规则的大匹配器再做归因：归因可能要建另一条大规则的匹配器，同一时刻只留一个。
		sh.releaseLarge(plan)
	}
	s.attributeShadows(sh, rule, res.Users)
	return res
}

// attributeShadows 给走错的样例与访问日志分组标注「被哪条更靠前的规则抢先命中」，
// 并在一个用户的全部不符证据都能归因时改写它的 Reason。只改说明，不改 Verdict：
// 被遮蔽仍算本规则走错（设计 docs/superpowers/specs/2026-11-01-routing-flow-audit-design.md）。
//
// 核心的 TestRoute 只回出站 tag、访问日志也不记规则，所以归因由面板推断：在核心规则顺序里
// 排在本规则之前、覆盖该用户、出站恰是实际出站、且域名组命中目标（xray 自己的匹配器，
// 与 buildLogEvidence 同一套）的第一条规则。推不出来（模板里手写的规则、IP 两遍匹配等）
// 就不标，界面退回原来的「与配置不符」，宁可不说也不乱指一条规则。
func (s *RoutingVerifyService) attributeShadows(sh *verifyShared, rule *model.RoutingRule, users []UserVerify) {
	defer sh.releaseShadowLarge()
	for i := range users {
		u := &users[i]
		if u.Verdict != verdictMismatch {
			continue
		}
		total, shadowed := 0, 0
		var refs []*ShadowRule
		note := func(ref *ShadowRule) {
			total++
			if ref == nil {
				return
			}
			shadowed++
			for _, r := range refs {
				if r.RuleId == ref.RuleId {
					return
				}
			}
			refs = append(refs, ref)
		}
		for j := range u.Samples {
			smp := &u.Samples[j]
			if smp.Error != "" || smp.Match {
				continue
			}
			if smp.ActualTag != "" {
				smp.ShadowedBy = sh.findShadow(rule, u.InboundId, smp.ActualTag, []string{smp.Target})
			}
			note(smp.ShadowedBy)
		}
		if u.Log != nil {
			for j := range u.Log.Others {
				o := &u.Log.Others[j]
				o.ShadowedBy = sh.findShadow(rule, u.InboundId, o.Route, o.Examples)
				note(o.ShadowedBy)
			}
		}
		if total == 0 || shadowed != total {
			continue
		}
		if len(refs) == 1 {
			u.Reason = fmt.Sprintf("这些连接被排在前面的规则「%s」抢先命中，没有轮到本规则", shadowRemark(refs[0]))
		} else {
			u.Reason = fmt.Sprintf("这些连接被排在前面的 %d 条规则抢先命中，没有轮到本规则", len(refs))
		}
	}
}

func shadowRemark(r *ShadowRule) string {
	if r.Remark != "" {
		return r.Remark
	}
	return "未命名规则"
}

// findShadow 在核心顺序里本规则之前的规则中，找出把 examples 送到 route 出站的那一条。
// examples 必须全部归到同一条规则才算数：日志分组只按出站聚合，几条示例分属不同规则时
// 说不清这一组「被谁抢先」，不标。
func (sh *verifyShared) findShadow(rule *model.RoutingRule, inboundId int, route string, examples []string) *ShadowRule {
	if len(examples) == 0 {
		return nil
	}
	order := sh.rulesInCoreOrder()
	var found *model.RoutingRule
	for _, ex := range examples {
		var hit *model.RoutingRule
		for _, c := range order {
			if c.Id == rule.Id {
				break // 只看排在本规则之前的
			}
			if !sh.shadowCandidate(c, inboundId, route) {
				continue
			}
			if m := sh.shadowMatcher(c); m != nil && m.Match(ex) {
				hit = c
				break
			}
		}
		if hit == nil || (found != nil && found.Id != hit.Id) {
			return nil
		}
		found = hit
	}
	return &ShadowRule{RuleId: found.Id, Remark: found.Remark}
}

// rulesInCoreOrder 是启用规则在核心里的先后：block 整组在前，其余按 priority、id 升序
// （GetEnabled 的顺序），与 RoutingInjector.buildRules 一致。一轮内只读一次库。
func (sh *verifyShared) rulesInCoreOrder() []*model.RoutingRule {
	if sh.coreOrderReady {
		return sh.coreOrder
	}
	sh.coreOrderReady = true
	rules, err := (&RoutingRuleService{}).GetEnabled()
	if err != nil {
		logger.Warning("路由验证：读取分流规则失败，不做抢先命中归因：", err)
		sh.coreOrderErr = err
		return nil
	}
	ordered := make([]*model.RoutingRule, 0, len(rules))
	for _, r := range rules {
		if r.Action == model.ActionBlock {
			ordered = append(ordered, r)
		}
	}
	for _, r := range rules {
		if r.Action != model.ActionBlock {
			ordered = append(ordered, r)
		}
	}
	sh.coreOrder = ordered
	return ordered
}

// shadowCandidate：规则会写进配置（同 planRule 交给 buildRule 判）、覆盖该入站、出站就是 route。
func (sh *verifyShared) shadowCandidate(c *model.RoutingRule, inboundId int, route string) bool {
	defaultTag := sh.rt.DefaultTag
	if defaultTag == "" && c.Action == model.ActionDirect {
		defaultTag = sh.configDefaultTag()
	}
	if _, _, skip := (&RoutingInjector{}).buildRule(c, sh.inboundTags, sh.usable, defaultTag); skip != nil {
		return false
	}
	ids, err := DecodeInboundIds(c.InboundIds)
	if err != nil {
		return false
	}
	if len(ids) > 0 && !slices.Contains(ids, inboundId) {
		return false
	}
	switch c.Action {
	case model.ActionProxy:
		return sh.usable[c.OutboundId] == route
	case model.ActionBlock:
		return route == model.BlockOutboundTag
	case model.ActionDirect:
		return route == defaultTag || model.IsMeterTag(route)
	}
	return false
}

// shadowMatcher 返回候选规则合并后条目的匹配器；建不出来返回 nil（该候选不参与归因）。
func (sh *verifyShared) shadowMatcher(c *model.RoutingRule) *groupMatcher {
	gids, err := DecodeDomainGroupIds(c.DomainGroupIds)
	if err != nil {
		return nil
	}
	var domainLists, cidrLists [][]string
	var used []string
	var usedIds []int
	for _, gid := range gids {
		g := sh.group(gid)
		if g.skip {
			continue
		}
		domainLists = append(domainLists, g.domains)
		cidrLists = append(cidrLists, g.cidrs)
		used = append(used, strconv.Itoa(gid))
		usedIds = append(usedIds, gid)
	}
	key := strings.Join(used, ",")
	if e, ok := sh.shadowSmall[key]; ok {
		return e.m
	}
	if sh.shadowLargeEnt != nil && sh.shadowLargeKey == key {
		return sh.shadowLargeEnt.m
	}
	domains, cidrs := MergeDomains(domainLists...), MergeDomains(cidrLists...)
	var e *matcherEntry
	if len(domains)+len(cidrs) > auditLargeThreshold {
		sh.releaseShadowLarge() // 先放手旧的再取，同一时刻只占用一个大匹配器
		e = sh.acquireLarge(domains, cidrs)
		sh.shadowLargeKey, sh.shadowLargeEnt, sh.shadowLargeIds = key, e, usedIds
	} else {
		e = smallMatchers.get(domains, cidrs)
		sh.shadowSmall[key] = e
	}
	if e.err != nil {
		logger.Warning("路由验证：构造规则匹配器失败，rule id:", c.Id, "err:", e.err)
		return nil
	}
	return e.m
}

// releaseShadowLarge 放手归因用的大匹配器，并放掉它的组条目（可能是为它才读进 sh.groups 的）。
// 先丢掉自己对匹配器的引用再放手：放手后等着换槽的一方会立刻 GC，引用还在的话旧匹配器
// 躲过这次回收，xray 的弱缓存就可能把它当新的交出去。
func (sh *verifyShared) releaseShadowLarge() {
	if sh.shadowLargeEnt == nil {
		return
	}
	for _, gid := range sh.shadowLargeIds {
		delete(sh.groups, gid)
	}
	sh.shadowLargeKey, sh.shadowLargeEnt, sh.shadowLargeIds = "", nil, nil
	sh.releaseLargeLease()
}

// coreStateOf 把运行时快照映射成核心级状态。pending 仍执行 TestRoute（设计 §3.5）。
func coreStateOf(rt XrayRuntime) CoreState {
	switch {
	case rt.Busy:
		return CoreState{State: coreStateBusy, Message: "核心正在更新配置，请稍后重新验证"}
	case !rt.Running:
		return CoreState{State: coreStateNotRunning, Message: "xray 未运行"}
	case rt.Pending:
		return CoreState{State: coreStatePending, Message: "配置已保存，正在等待下发到核心（最多约 10 秒）"}
	default:
		return CoreState{State: coreStateOK}
	}
}

// runSamples 对一个用户逐个样例调 route。单次 rpcTimeout，整批共用 budgetCtx；
// 预算用完后剩下的样例不再调用，直接记「超时」。
func runSamples(budgetCtx context.Context, route routeFunc, rpcTimeout time.Duration, inboundTag string, targets []sampleTarget, plan *verifyPlan) []SampleResult {
	out := make([]SampleResult, 0, len(targets))
	for _, t := range targets {
		r := SampleResult{Target: t.Display}
		if budgetCtx.Err() != nil {
			r.Error = sampleErrTimeout
			out = append(out, r)
			continue
		}
		ctx, cancel := context.WithTimeout(budgetCtx, rpcTimeout)
		tag, noMatch, err := route(ctx, inboundTag, t.Domain, t.IP)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			if timedOut || errors.Is(err, context.DeadlineExceeded) {
				r.Error = sampleErrTimeout
			} else {
				r.Error = err.Error()
			}
			out = append(out, r)
			continue
		}
		r.ActualTag = tag
		if noMatch {
			r.ActualLabel = labelNoMatch
		} else {
			r.ActualLabel = plan.labelOf(tag)
		}
		r.Match, r.HitByRule = judgeSample(plan.expected, tag, noMatch, plan.defaultTag)
		out = append(out, r)
	}
	return out
}

// planRule 从共享上下文推出一条规则的验证计划；返回非空 reason 表示规则不会写进
// 核心配置，不发 RPC。
//
// 「会不会写进配置」直接交给 buildRule 判断，不另写一份：两份判定迟早漂移，
// 漂移后这里说「可以验证」的规则核心里其实没有。
func (s *RoutingVerifyService) planRule(sh *verifyShared, rule *model.RoutingRule) (*verifyPlan, string) {
	// 默认出站 tag 取核心实际持有的配置；核心没在跑时退回当前生成配置，
	// 两者都是 tagDefaultOutbound 写出的同一个值，只是前者才是核心眼里的。
	defaultTag := sh.rt.DefaultTag
	if defaultTag == "" && rule.Action == model.ActionDirect {
		defaultTag = sh.configDefaultTag()
	}
	if _, _, skip := (&RoutingInjector{}).buildRule(rule, sh.inboundTags, sh.usable, defaultTag); skip != nil {
		return nil, "本规则不会写进核心配置：" + skip.Error()
	}

	plan := &verifyPlan{defaultTag: defaultTag}
	groupIds, _ := DecodeDomainGroupIds(rule.DomainGroupIds) // buildRule 已校验过
	var domainLists, cidrLists [][]string
	var used []string
	for _, gid := range groupIds {
		g := sh.group(gid)
		if g.skip {
			continue
		}
		domainLists = append(domainLists, g.domains)
		cidrLists = append(cidrLists, g.cidrs)
		plan.targets = append(plan.targets, g.targets...)
		plan.privateOnly = plan.privateOnly || g.privateOnly
		used = append(used, strconv.Itoa(gid))
		plan.groupIds = append(plan.groupIds, gid)
	}
	plan.targets = dedupeTargets(plan.targets, verifyPerUserSamples)
	plan.domains = MergeDomains(domainLists...)
	plan.cidrs = MergeDomains(cidrLists...)
	plan.groupKey = strings.Join(used, ",")
	plan.large = len(plan.domains)+len(plan.cidrs) > auditLargeThreshold

	plan.labelOf = func(tag string) string {
		switch {
		case tag == "":
			// 访问日志里被接受却没记出站的行（Route 为空）：没有出站可展示，
			// 给一个明确的文案，不让界面渲染出「走了 （最近一次…）」这种空白。
			return labelUnknownOutbound
		case tag == model.BlockOutboundTag:
			return labelBlock
		case (defaultTag != "" && tag == defaultTag) || model.IsMeterTag(tag):
			return labelDefaultOutbound
		}
		if label, ok := sh.remarkByTag[tag]; ok {
			return label
		}
		return tag
	}

	switch rule.Action {
	case model.ActionProxy:
		node, ok := sh.nodes[rule.OutboundId]
		if !ok {
			if plan.large {
				sh.releaseLarge(plan) // 大规则的组条目已读进 sh.groups，不能留到整轮结束
			}
			return nil, "读取出站节点失败：节点不存在"
		}
		plan.expected = ExpectedOutbound{Action: rule.Action, Tag: node.Tag, Label: nodeLabel(node)}
	case model.ActionBlock:
		plan.expected = ExpectedOutbound{Action: rule.Action, Tag: model.BlockOutboundTag, Label: labelBlock}
	case model.ActionDirect:
		plan.expected = ExpectedOutbound{Action: rule.Action, Tag: defaultTag, Label: labelDirect}
	}

	inboundIds, _ := DecodeInboundIds(rule.InboundIds) // buildRule 已校验过
	if len(inboundIds) == 0 {
		// 全局规则：只验证启用的入站，按 id 升序截到上限。
		for _, in := range sh.inbounds {
			if !in.Enable {
				continue
			}
			if len(plan.users) >= verifyGlobalInboundLimit {
				plan.truncated = true
				break
			}
			plan.users = append(plan.users, verifyUser{inbound: in})
		}
		return plan, ""
	}
	byId := make(map[int]*model.Inbound, len(sh.inbounds))
	for _, in := range sh.inbounds {
		byId[in.Id] = in
	}
	for _, id := range inboundIds {
		in, ok := byId[id]
		if !ok {
			// 已删除的入站 buildRule 同样剔掉；没有名字可展示，不列出。
			continue
		}
		plan.users = append(plan.users, verifyUser{inbound: in, disabled: !in.Enable})
	}
	return plan, ""
}

// groupConditionsOf 取一个域名组合并后的域名与 IP 条目，与 buildRule 的逐组取法一致：
// 组不存在、数据损坏、两侧都空的组剔除（ok=false，buildRule 已记过日志）。
func groupConditionsOf(gid int) (domains, cidrs []string, ok bool) {
	group, err := (&DomainGroupService{}).Get(gid)
	if err != nil {
		return nil, nil, false
	}
	manualDomains, err := DecodeDomains(group.Domains)
	if err != nil {
		return nil, nil, false
	}
	subscribedDomains, err := DecodeSubscribedDomains(group.SubscribedDomains)
	if err != nil {
		return nil, nil, false
	}
	manualCidrs, err := DecodeCidrs(group.Cidrs)
	if err != nil {
		return nil, nil, false
	}
	subscribedCidrs, err := DecodeSubscribedCidrs(group.SubscribedCidrs)
	if err != nil {
		return nil, nil, false
	}
	domains = MergeDomains(manualDomains, subscribedDomains)
	cidrs = MergeDomains(manualCidrs, subscribedCidrs)
	if len(domains) == 0 && len(cidrs) == 0 {
		return nil, nil, false
	}
	return domains, cidrs, true
}

// dedupeTargets 按展示值去重（多个组可能含同一个域名），并截到 limit 个。
func dedupeTargets(targets []sampleTarget, limit int) []sampleTarget {
	out := make([]sampleTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if len(out) >= limit {
			break
		}
		if seen[t.Display] {
			continue
		}
		seen[t.Display] = true
		out = append(out, t)
	}
	return out
}

func nodeLabel(n *model.OutboundNode) string {
	if n.Remark != "" {
		return n.Remark
	}
	return n.Tag
}

func inboundName(in *model.Inbound) string {
	if in.Remark != "" {
		return in.Remark
	}
	return in.Tag
}
