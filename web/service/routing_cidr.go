package service

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"

	"a-ui/util/common"
)

// xray 支持的 IP 匹配前缀，见 common/geodata/rule_parser.go:16 ParseIPRules。
// geoip:xx 会被 xray 改写成 ext:geoip.dat:xx。
var cidrPrefixes = []string{"geoip:", "ext:", "ext-ip:"}

// ParseCidrs 把用户在 textarea 中一行一条录入的 IP 段解析成入库列表。
//
// 按输入行序输出，不排序：顺序是「生成逐字节确定」不变量的一部分。
func ParseCidrs(raw string) ([]string, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		normalized, err := normalizeCidrRule(item)
		if err != nil {
			return nil, err
		}
		list = append(list, normalized)
	}
	if len(list) == 0 {
		return nil, common.NewError("IP 段列表不能为空")
	}
	return list, nil
}

// normalizeCidrRule 校验一行录入，或说明它为什么不合法。
//
// 不改写内容：geoip 的 code 由 xray 自己 ToUpper（rule_parser.go:74），
// CIDR 也没有需要归一的大小写。这里只做「拦住明显不是 IP 段的东西」。
func normalizeCidrRule(item string) (string, error) {
	// 前导 ! 是取反，可叠加，作用于整条规则（rule_parser.go:49 cutReversePrefix）。
	body := strings.TrimLeft(item, "!")
	if body == "" {
		return "", common.NewError("IP 段不能只有取反符号:", item)
	}
	for _, p := range cidrPrefixes {
		if !strings.HasPrefix(body, p) {
			continue
		}
		if body[len(p):] == "" {
			return "", common.NewError("IP 段格式不支持，前缀后面没有内容:", item)
		}
		// 类别/文件是否真的存在，交给 ValidateCidrs 的真实 xray 判定：
		// geoip.dat 的内容会随面板「安装 xray」变化，在这里硬编码类别清单
		// 迟早与机器上那份 dat 对不上。
		return item, nil
	}
	if !isValidCIDR(body) {
		return "", common.NewError("IP 段格式不支持:", item,
			"——应为 CIDR（1.2.3.0/24）、单个 IP（8.8.8.8）、"+
				"geoip:cn / geoip:!cn，或 ext:文件:标签。域名请填在「手工域名」框里")
	}
	return item, nil
}

// isValidCIDR 镜像 xray 的 parseCIDR（common/geodata/rule_parser.go:102）：
// 允许裸 IP（等价 /32、/128），前缀长度不得超过地址族上限。
//
// 刻意不用 net.ParseCIDR：它拒绝裸 IP，也拒绝 1.2.3.4/24 这种主机位非零
// 的写法，而 xray 两者都接受。校验比 xray 更严会拦下合法配置。
func isValidCIDR(s string) bool {
	ipStr, prefixStr, hasPrefix := strings.Cut(s, "/")
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	maxPrefix := 128
	if ip.To4() != nil {
		maxPrefix = 32
	}
	if !hasPrefix {
		return true
	}
	n, err := strconv.Atoi(prefixStr)
	return err == nil && n >= 0 && n <= maxPrefix
}

// EncodeCidrs 把 IP 段列表序列化为入库格式。
//
// nil 归一成 []：json.Marshal 对 nil 切片产出 "null"，列表页与导出侧就要
// 多一处分支，而 [] 与「没有 IP 段」语义完全一致。
func EncodeCidrs(list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeCidrs 是 EncodeCidrs 的逆操作。
//
// 空字符串当作「没有 IP 段」：升级前建的组这一列就是空的，在这里报错会让
// buildRule 把引用它的规则整条丢弃——分流静默失效。真正的语法错误仍返回
// error，那是数据损坏，宁可让规则被丢弃也不能当成「没有条件」。
func DecodeCidrs(encoded string) ([]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(encoded), &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DecodeSubscribedCidrs 与 DecodeCidrs 行为一致，单独命名是为了让调用点
// 自解释——与 DecodeDomains / DecodeSubscribedDomains 的分工对称。
func DecodeSubscribedCidrs(encoded string) ([]string, error) {
	return DecodeCidrs(encoded)
}
