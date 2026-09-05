package service

import (
	"encoding/json"
	"strings"

	"a-ui/logger"
	"a-ui/util/json_util"
	"a-ui/xray"
)

// dnsFallbackServer 是保证出现在 servers 列表里的兜底解析器。
// xray 的 "localhost" 表示走系统解析器（/etc/resolv.conf）。
//
// 存在的理由：管理员配置的解析器全部不可用时，退化成「和不配这个功能一样」，
// 而不是断网。没有它的话，一次上游 DoH 故障就是全员解析失败。
const dnsFallbackServer = "localhost"

// freedomDomainStrategy 只能是 UseIP 系列，绝不能改成 ForceIP 系列。
//
// transport/internet/config.go:13 的矩阵里 UseIP* 是 strategy code 1、
// ForceIP* 是 code 2；proxy/freedom/freedom.go:298 只在 ForceIP() 为真时
// 把解析失败变成连接失败，UseIP 下只记日志并回落按域名直连。
// 这一条把「DNS 配错等于全员断网」整个消掉，是本功能敢做的前提。
const freedomDomainStrategy = "UseIP"

// DNSInjector 在管理员配置了 DNS 服务器时，接管 xray 的 dns 段与默认出站
// settings 里的 domainStrategy。
//
// 必须排在 RoutingInjector.Inject 之后调用：那一步会把整个 outbounds 数组
// 反序列化再重新序列化，并由 tagDefaultOutbound 保证首个出站存在且带 tag；
// 本注入器只往那个对象的 settings 里加一个键，不必自己处理数组不存在的
// 情况。若调用顺序反了，routingInjector 重建 outbounds 数组时会把这里
// 写的键悄悄冲掉，且不会有任何报错——这条不变量靠
// TestDNSInjectorSetsFreedomDomainStrategyThroughGetXrayConfig 兜底。
type DNSInjector struct {
	settingService SettingService
}

// Inject 列表为空时一个字节都不改——升级后行为零变化靠的就是这一条。
func (s *DNSInjector) Inject(cfg *xray.Config) error {
	raw, err := s.settingService.GetDNSServers()
	if err != nil {
		return err
	}
	servers := ParseDNSServers(raw)
	if len(servers) == 0 {
		return nil
	}

	// 保证列表里有系统解析器兜底。管理员自己写了就不再补——他把它放在
	// 第一位是有意为之（优先系统解析），补第二个只会让界面与配置对不上。
	hasFallback := false
	for _, item := range servers {
		if item == dnsFallbackServer {
			hasFallback = true
			break
		}
	}
	if !hasFallback {
		servers = append(servers, dnsFallbackServer)
	}

	// 模板里手写的 dns 段（hosts/queryStrategy/clientIp/disableCache/tag
	// 等）会被整体替换掉，不做合并——合并意味着字段级别的取舍规则，而这
	// 些字段之间存在耦合（比如 hosts 覆盖单个域名的解析结果、
	// disableCache 影响所有 servers），谁的优先级更高没有唯一答案。接管
	// 是本功能故意做出的选择，但必须让管理员看见，而不是配了一项设置、
	// 模板里原有的 dns 定制就无声无息地消失。
	if len(cfg.DNSConfig) > 0 {
		logger.Warning("dns servers configured; replacing the template's existing dns section " +
			"(any hosts/queryStrategy/clientIp/disableCache/tag there is discarded)")
	}

	// 顺序原样保留：DNS 有优先级语义，排序会改变行为。
	encoded, err := json.Marshal(map[string]any{"servers": servers})
	if err != nil {
		return err
	}
	cfg.DNSConfig = json_util.RawMessage(encoded)

	return s.applyFreedomStrategy(cfg)
}

// applyFreedomStrategy 给数组首位的默认出站的 settings 加上 domainStrategy。
//
// 不做这一步，dns 段对直连流量完全是空转：freedom 只在自己的
// domainStrategy.HasStrategy() 为真时才调 internet.LookupForIP
// （proxy/freedom/freedom.go:290），而只有那个函数打的是 xray 的内置 DNS
// 客户端（transport/internet/dialer.go:87）；默认的 AsIs 走系统解析器。
//
// 手写模板的人几乎不会知道这一点，这正是本功能相对「自己往模板里塞一段
// dns」的主要价值。
//
// 必须写在 outbound 的 settings 里，绝不能写在 outbound 对象本身上：
// freedom 的 domainStrategy 是 infra/conf.FreedomConfig 的字段
// （infra/conf/freedom.go:21），是 settings 里的东西；outbound 对象对应
// infra/conf.OutboundDetourConfig，其完整字段集是 protocol/sendThrough/
// tag/settings/streamSettings/proxySettings/mux/targetStrategy
// （infra/conf/xray.go:213-222）——里面根本没有 domainStrategy 这一项。
// infra/conf 从不调用 DisallowUnknownFields，写错层级不会有任何报错：
// run -test 照样 Configuration OK，面板首页照样 running，freedom 停在
// AsIs 不变，dns 段对直连流量完全不起作用——这正是本功能存在的理由要防
// 的那种故障，写错一层就原样复现了一遍。
func (s *DNSInjector) applyFreedomStrategy(cfg *xray.Config) error {
	outbounds := make([]any, 0)
	if len(cfg.OutboundConfigs) > 0 {
		if err := json.Unmarshal(cfg.OutboundConfigs, &outbounds); err != nil {
			return err
		}
	}
	if len(outbounds) == 0 {
		logger.Warning("dns servers configured but the generated config has no outbound; " +
			"direct traffic keeps using the system resolver")
		return nil
	}
	first, ok := outbounds[0].(map[string]any)
	if !ok || first == nil {
		logger.Warning("dns servers configured but the first outbound is not an object; " +
			"direct traffic keeps using the system resolver")
		return nil
	}
	// 管理员可能改过模板，把首位换成别的协议。只有 freedom 认 domainStrategy，
	// 给别的协议加这个键要么被忽略、要么让整份配置非法——后者会全员断网。
	if first["protocol"] != "freedom" {
		logger.Warning("dns servers configured but the default outbound is not freedom (protocol:",
			first["protocol"], "); direct traffic keeps using the system resolver")
		return nil
	}
	settings, _ := first["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		first["settings"] = settings
	}
	// infra/conf/freedom.go:62-65 里 Build 只在 targetStrategy 为空时才
	// 回落到 domainStrategy——若模板已经显式写了非空 targetStrategy，我们
	// 写的 domainStrategy 会被静默忽略。这不是能覆盖的：那是管理员在模板
	// 里做出的显式选择，覆盖它是另一种「改动用户可见行为」。仍然写
	// domainStrategy（管理员日后清空 targetStrategy 时立刻生效），但把
	// 「这项设置其实不起作用」变成一条看得见的日志，而不是又一次无声空转。
	if ts, ok := settings["targetStrategy"].(string); ok && ts != "" {
		logger.Warning("dns servers configured but the default outbound's settings.targetStrategy " +
			"is already set to a non-empty value; targetStrategy takes precedence over " +
			"domainStrategy in freedom, so dns servers will not govern direct traffic until " +
			"targetStrategy is cleared")
	}
	settings["domainStrategy"] = freedomDomainStrategy

	encoded, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encoded)
	return nil
}

// ParseDNSServers 把 textarea 原文切成有序、去重的服务器列表。
//
// 只做切分与去重，语法校验在 entity.CheckValid（保存那一刻）：生成期再拒绝
// 已经落库的值，只会让整份配置生成失败、xray 保持旧配置，而管理员在界面上
// 看不到任何线索。
func ParseDNSServers(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
