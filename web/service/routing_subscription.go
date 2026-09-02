package service

import (
	"strings"

	"a-ui/util/common"
)

// 逐行解析，不做全局格式识别。Surge/Clash classical、Clash YAML、纯域名列表
// 三种格式的行特征互不冲突，逐行判断比先猜格式更健壮：真实订阅文件里混着
// 注释、YAML 头和规则行，全局识别一旦猜错就整份文件解析失败。
//
// 已知的非域名规则类型一律跳过并计数。不尝试翻译成 xray 的其他条件——
// 域名组这个概念只承载域名，把 IP 规则塞进来需要动整个数据模型。
// 实际遇到的规则类型：IP-CIDR、IP-CIDR6、IP-ASN、GEOIP、SRC-IP-CIDR、SRC-PORT、
// DST-PORT、PROCESS-NAME、PROCESS-PATH、USER-AGENT、URL-REGEX、RULE-SET、SUB-DOMAIN、
// DOMAIN-WILDCARD、AND、OR、NOT、PROTOCOL、NETWORK、IN-PORT。

// ParseSubscription 把订阅文件文本解析成 xray 域名语法。
// 返回（域名列表, 跳过的非域名条数, 错误）。
//
// 解析不出任何域名时返回错误而非空列表：调用方据此保留上一次成功的数据。
// 若在这里返回空列表，上游改格式或 URL 失效返回 404 页面时，域名组会被清空，
// 引用它的规则被 buildRule 跳过，流量静默退回直连。
func ParseSubscription(raw string) ([]string, int, error) {
	domains := make([]string, 0, 256)
	seen := make(map[string]bool, 256)
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

		converted, ok := convertSubscriptionLine(item)
		if !ok {
			skipped++
			continue
		}
		if seen[converted] {
			continue
		}
		seen[converted] = true
		domains = append(domains, converted)
	}

	if len(domains) == 0 {
		return nil, skipped, common.NewError(
			"订阅内容里没有解析出任何域名（跳过了", skipped, "条非域名规则）")
	}
	return domains, skipped, nil
}

// convertSubscriptionLine 把一行转成 xray 域名语法。第二个返回值为 false
// 表示这行应当被跳过并计数。
func convertSubscriptionLine(item string) (string, bool) {
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
			return domainRule("domain:", value)
		case "DOMAIN":
			return domainRule("full:", value)
		case "DOMAIN-KEYWORD":
			// xray 的裸域名就是子串匹配，与 DOMAIN-KEYWORD 语义一致。
			// 会误伤（ads 命中 downloads.example.com），但那是这个规则类型
			// 在 Shadowrocket/Clash 里的固有行为，不是本实现引入的偏差。
			// 必须归一大小写：域名匹配大小写不敏感，未归一可能在 xray 里永不命中。
			if !isValidKeyword(value) {
				return "", false
			}
			return strings.ToLower(value), true
		default:
			// 已知的非域名规则类型一律跳过，不认识的类型也一律跳过，绝不猜测
			return "", false
		}
	}
	// 纯域名列表：.example.com / +.example.com / *.example.com / example.com
	// 这类列表的惯例是后缀匹配
	return domainRule("domain:", item)
}

func domainRule(prefix, value string) (string, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "+.")
	value = strings.TrimPrefix(value, "*.")
	value = strings.TrimPrefix(value, ".")
	value = strings.TrimSuffix(value, ".")
	if !isValidDomain(value) {
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
	for _, label := range strings.Split(s, ".") {
		if label == "" {
			return false
		}
		for _, r := range label {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			// 允许非 ASCII：xray 接受中文等国际化域名
			if !isAlnum && r != '-' && r != '_' && r < 128 {
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
