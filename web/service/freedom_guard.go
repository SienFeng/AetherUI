package service

import (
	"encoding/json"
	"strings"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/json_util"
	"a-ui/xray"
)

// 默认 freedom 出站的私网守卫。
//
// socks / http 入站的用户只要把目标写成主机名（socks5h 的 localhost、一个解析到 10.x 的域名），
// 默认出站就会替他连到本机回环、内网与云元数据——包括 xray 自己那个不鉴权的 gRPC 控制面（模板
// api 入站的端口，审计时实测 StatsService.GetSysStats 经一个 socks 用户调用成功）。两道既有防线
// 都拦不住：模板的 geoip:private → blocked 只对 IP 目标生效（不开 ipRuleResolveDomain 时域名
// 目标根本不解析；开了也挡不住 DNS rebinding——路由解析一次、freedom 拨号时再解析一次）；freedom
// 内置的私网拦截只覆盖 vless/vmess/trojan/shadowsocks/hysteria/wireguard 入站
// （proxy/freedom/freedom.go 的 getDefaultFinalRule）。
//
// 守卫写在 freedom 自己的 settings.finalRules 里：它判的是解析之后真正要拨的那个地址。对 vless
// 这类入站它与内置规则重复，行为不变。xray 26.7.28 的 run -test 接受这个写法（e2e 用例实测）。

// privateGuardRuleJSON 是追加在 finalRules 末尾的那一条。用 geoip:private 而不是抄一份网段：
// 与模板那条路由规则同一个口径，geo 数据由 a-ui geo 先校验再替换。
const privateGuardRuleJSON = `{"action":"block","ip":["geoip:private"]}`

// dokodemoDirectTag 是给 dokodemo-door 入站用的、不带守卫的默认出站副本的 tag。
//
// 带一个点：allocTag 生成的 tag 是 a-ui- 加上 SlugRemark（非字母数字一律换成连字符），不可能撞上；
// 导入路径只校验前缀，buildOutbounds 对撞名的节点直接跳过（isInjectorOnlyTag）。
const dokodemoDirectTag = model.DokodemoDirectOutboundTag

// isInjectorOnlyTag 判断一个出站节点的 tag 是否与本文件注入的 tag 撞名。
// model.IsReservedTag 管不到它（那个函数在 database/model 里），生成期在这里补一道。
func isInjectorOnlyTag(tag string) bool {
	return tag == dokodemoDirectTag
}

func privateGuardRule() any {
	var rule any
	_ = json.Unmarshal([]byte(privateGuardRuleJSON), &rule)
	return rule
}

// isPrivateGuardRule 判断 finalRules 里的一条是不是本守卫（规范化 JSON 逐字节比较）。
func isPrivateGuardRule(rule any) bool {
	encoded, err := json.Marshal(rule)
	return err == nil && string(encoded) == privateGuardRuleJSON
}

// isFreedomOutbound 判断出站对象是不是 freedom。xray 对协议名不区分大小写，direct 是它的别名
// （infra/conf/xray.go 的 outboundConfigLoader）。
func isFreedomOutbound(ob map[string]any) bool {
	protocol, _ := ob["protocol"].(string)
	switch strings.ToLower(protocol) {
	case "freedom", "direct":
		return true
	}
	return false
}

// guardFreedomPrivate 给数组首位的默认 freedom 出站的 settings.finalRules 末尾追加私网守卫。
//
// 必须在 buildOutbounds 里、appendMeterOutbounds 复制默认出站之前调用：计量出站是它的逐字节副本，
// 被计量的直连流量走副本，副本不带守卫的话一个被计量的主机名就能绕过去。
//
// 追加而不是覆盖：管理员手写的 finalRules 原样排在前面，xray 取第一条命中的，他显式放行的网段照常
// 放行；开头写一条 {"action":"allow"} 就是关掉守卫的写法。已经有同一条的不重复加。settings 或
// finalRules 不是预期的形状（模板写坏了）时不碰，交给 xray 自己去报错。
func guardFreedomPrivate(outbounds []any) {
	if len(outbounds) == 0 {
		return
	}
	ob, ok := outbounds[0].(map[string]any)
	if !ok || ob == nil || !isFreedomOutbound(ob) {
		return
	}
	settings, ok := ob["settings"].(map[string]any)
	if !ok {
		if ob["settings"] != nil {
			return
		}
		settings = map[string]any{}
		ob["settings"] = settings
	}
	rules, ok := settings["finalRules"].([]any)
	if !ok && settings["finalRules"] != nil {
		return
	}
	for _, r := range rules {
		if isPrivateGuardRule(r) {
			return
		}
	}
	settings["finalRules"] = append(rules, privateGuardRule())
}

// dokodemoInboundTags 返回配置里 dokodemo-door 入站的 tag（配置顺序，不含控制面 api 入站）。
func dokodemoInboundTags(inbounds []xray.InboundConfig) []string {
	tags := make([]string, 0)
	for _, ib := range inbounds {
		switch strings.ToLower(ib.Protocol) {
		case "dokodemo-door", "tunnel":
		default:
			continue
		}
		if ib.Tag == "" || ib.Tag == xray.APIInboundTag {
			continue
		}
		tags = append(tags, ib.Tag)
	}
	return tags
}

// applyDokodemoDirect 让 dokodemo-door 入站不受私网守卫影响：追加一份去掉守卫的默认出站副本
// （dokodemoDirectTag），再加一条「这些入站 → 副本」的兜底规则。
//
// dokodemo-door 的目标是管理员在入站上写死的（端口转发），转发到内网主机名是正当用法，不是用户
// 能挑的目标。实测（Xray 26.7.28）改动前：dokodemo → localhost 主机名能通、→ 127.0.0.1 字面量
// 本来就被模板的 geoip:private 规则拦着；这一步之后两者都与改动前完全一致（单遍、两遍匹配都是）。
//
// 几条约束：
//   - 必须在 DNS 注入之后调用（GetXrayConfig 里排在 dnsInjector.Inject 后面）：副本要带上默认出站
//     的 domainStrategy，否则 dokodemo 的主机名目标会悄悄绕过管理员配的内置 DNS。dns_inject.go
//     只给计量副本补这个键。
//   - 兜底规则排在管理员与模板的全部规则之后（封禁、分流照样优先），计量规则之前：排在计量规则
//     之后的话，一个被计量的内网主机名会掉进带守卫的计量副本、在进池一小时后突然断掉。代价是
//     dokodemo 入站的流量不再进计量（它只有一个固定目标，按域名拆分本来就没有信息量）。
//   - 两遍匹配（domainStrategy 为 IPIfNonMatch/IPOnDemand）时带 ip 守卫，判据与计量规则同一个
//     函数：不带的话它在第一遍就命中，geoip:private 对 dokodemo 的域名目标再没有第二遍的机会。
//   - 没有 dokodemo 入站时一个字节都不改；默认出站上没有守卫（不是 freedom、管理员写坏了）时也不改。
func applyDokodemoDirect(cfg *xray.Config) error {
	tags := dokodemoInboundTags(cfg.InboundConfigs)
	if len(tags) == 0 || len(cfg.OutboundConfigs) == 0 {
		return nil
	}
	var outbounds []any
	if err := json.Unmarshal(cfg.OutboundConfigs, &outbounds); err != nil {
		return err
	}
	if len(outbounds) == 0 {
		return nil
	}
	base, ok := outbounds[0].(map[string]any)
	if !ok || base == nil || !isFreedomOutbound(base) {
		return nil
	}
	for _, raw := range outbounds {
		if ob, ok := raw.(map[string]any); ok && ob["tag"] == dokodemoDirectTag {
			logger.Warning("配置里已经有 tag 为", dokodemoDirectTag, "的出站，dokodemo-door 入站这次不走不带私网守卫的直连")
			return nil
		}
	}

	encoded, err := json.Marshal(base)
	if err != nil {
		return err
	}
	var direct map[string]any
	if err := json.Unmarshal(encoded, &direct); err != nil {
		return err
	}
	settings, _ := direct["settings"].(map[string]any)
	rules, _ := settings["finalRules"].([]any)
	kept := make([]any, 0, len(rules))
	for _, r := range rules {
		if !isPrivateGuardRule(r) {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(rules) {
		// 默认出站上本来就没有守卫，dokodemo 不受影响，不必多一个出站。
		return nil
	}
	if len(kept) == 0 {
		delete(settings, "finalRules")
	} else {
		settings["finalRules"] = kept
	}
	direct["tag"] = dokodemoDirectTag
	outbounds = append(outbounds, direct)

	routing := map[string]any{}
	if len(cfg.RouterConfig) > 0 {
		if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
			return err
		}
	}
	existing, _ := routing["rules"].([]any)
	catchAll := map[string]any{
		"type":        "field",
		"inboundTag":  tags,
		"outboundTag": dokodemoDirectTag,
	}
	if meterRuleNeedsIPGuard(routing["domainStrategy"]) {
		catchAll["ip"] = []string{"0.0.0.0/0", "::/0"}
	}
	insertAt := len(existing)
	for i, r := range existing {
		if m, ok := r.(map[string]any); ok {
			if tag, _ := m["outboundTag"].(string); model.IsMeterTag(tag) {
				insertAt = i
				break
			}
		}
	}
	merged := make([]any, 0, len(existing)+1)
	merged = append(merged, existing[:insertAt]...)
	merged = append(merged, catchAll)
	merged = append(merged, existing[insertAt:]...)
	routing["rules"] = merged

	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	encodedRouting, err := json.Marshal(routing)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)
	cfg.RouterConfig = json_util.RawMessage(encodedRouting)
	return nil
}
