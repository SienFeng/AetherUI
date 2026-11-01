package service

import (
	"fmt"
	"net"
	"strings"

	"a-ui/database/model"
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
type AccessLogState struct {
	Enabled     bool `json:"enabled"`
	WindowHours int  `json:"windowHours"`
}

// UserVerify 是图上一个用户（入站）的验证结果。
type UserVerify struct {
	InboundId int            `json:"inboundId"`
	Name      string         `json:"name"`
	Verdict   string         `json:"verdict"`
	Reason    string         `json:"reason"`
	Samples   []SampleResult `json:"samples"`
	Log       *LogEvidence   `json:"log"`
}

// SampleResult 是对一个样例目标做 TestRoute 的结果。
type SampleResult struct {
	Target      string `json:"target"`
	ActualTag   string `json:"actualTag"`
	ActualLabel string `json:"actualLabel"`
	Match       bool   `json:"match"`
	HitByRule   bool   `json:"hitByRule"`
	Error       string `json:"error"`
}

// LogEvidence 是访问日志里属于本规则的真实连接统计。
type LogEvidence struct {
	Matched   int64      `json:"matched"`
	Rejected  int64      `json:"rejected"`
	Truncated bool       `json:"truncated"`
	Others    []LogOther `json:"others"`
}

// LogOther 是走了非期望出站的连接分组。
type LogOther struct {
	Route    string   `json:"route"`
	Label    string   `json:"label"`
	Count    int64    `json:"count"`
	LastAt   int64    `json:"lastAt"`
	Examples []string `json:"examples"`
}

// sampleTarget 是一个待 TestRoute 的目标：域名与 IP 二选一。
type sampleTarget struct {
	Domain  string
	IP      net.IP
	Display string
}

// sampleTargets 对一个域名组取样：先域名后 IP，总数不超过 perGroup。
// 只有 full:/domain: 能还原出确定的域名；keyword/regexp/dotless/geosite/ext 推不出
// 可靠样例，geoip:/ext:/带 ! 取反的 IP 条目同理，一律跳过而不是猜一个。
func sampleTargets(domains, cidrs []string, perGroup int) []sampleTarget {
	out := []sampleTarget{}
	for _, d := range domains {
		if len(out) >= perGroup {
			return out
		}
		var host string
		switch {
		case strings.HasPrefix(d, "full:"):
			host = strings.TrimPrefix(d, "full:")
		case strings.HasPrefix(d, "domain:"):
			host = strings.TrimPrefix(d, "domain:")
		}
		if host = strings.TrimSpace(host); host != "" {
			out = append(out, sampleTarget{Domain: host, Display: host})
		}
	}
	for _, c := range cidrs {
		if len(out) >= perGroup {
			break
		}
		if ip := firstUsableIP(strings.TrimSpace(c)); ip != nil {
			out = append(out, sampleTarget{IP: ip, Display: ip.String()})
		}
	}
	return out
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
