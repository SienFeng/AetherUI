// Package domain 把访问目标归并到注册域名（eTLD+1）。
//
// 独立成包而不是塞进 web/service：归并是纯函数，可以完整单测，
// 而且第二期的计量池同样要用它。
package domain

import (
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"

	"a-ui/util/accesslog"
)

// Registrable 把访问日志里的目标（host:port 形式）归并成注册域名。
//
// 归并到 eTLD+1 而不是保留完整域名，有三个理由：管理员的心智是「传到哪个
// 网站」而不是「传到哪台机器」；一屏广告子域名会收敛成两三个词；以及第二期
// 的计量规则用 domain:<注册域名> 一条就覆盖它的全部子域名。
//
// 三类值原样返回，都不丢弃——丢弃会让这部分流量在榜单上凭空消失，而管理员
// 看不出少了东西：
//   - IP 字面量目标（客户端直连 IP）。它们在第二期需要 ip 条件而不是 domain
//     条件，不参与计量，但访问次数照样要统计。
//   - 本身就是公共后缀的（"com"）：EffectiveTLDPlusOne 对它返回错误。
//   - 不含点的主机名（"localhost"）：同样返回错误。
func Registrable(target string) string {
	host, ok := accesslog.HostOf(target)
	if !ok || host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	// 转小写并剥掉末尾的点，两件事都必须做在调用 publicsuffix 之前：
	// 它对 "example.com." 报 empty label 而不是自动忽略，回落之后
	// "example.com." 与 "example.com" 会分裂成两个桶，没有任何一层会报错。
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || etld1 == "" {
		return host
	}
	return etld1
}
