package xray

import (
	"bytes"
	"encoding/json"
	"strings"

	"a-ui/logger"
	"a-ui/util/json_util"
)

// HotDiff 是把运行中的核心从一份配置搬到另一份所需的控制面操作。
// 只覆盖核心支持运行时重载的部分：入站、出站、路由规则。
type HotDiff struct {
	RemovedInboundTags  []string
	AddedInbounds       [][]byte
	RemovedOutboundTags []string
	AddedOutbounds      [][]byte
	// RoutingConfig 是整段新的 routing 配置，nil 表示路由没变。
	RoutingConfig []byte
}

// Empty 表示不需要做任何操作。
func (d *HotDiff) Empty() bool {
	return len(d.RemovedInboundTags) == 0 &&
		len(d.AddedInbounds) == 0 &&
		len(d.RemovedOutboundTags) == 0 &&
		len(d.AddedOutbounds) == 0 &&
		d.RoutingConfig == nil
}

// ComputeHotDiff 比较新旧配置，返回把核心从 old 搬到 new 的操作集。
//
// ok 为 false 表示这次改动碰到了没有运行时重载接口的东西（log / dns /
// policy / api / stats / …），必须整进程重启。
//
// 判定一律保守：拿不准就返回 false。热更新是省掉一次断线的优化，不是新的
// 失败点——误判成"能热应用"的代价是核心与面板认知不一致，比多重启一次严重
// 得多。
func ComputeHotDiff(oldCfg, newCfg *Config) (*HotDiff, bool) {
	if oldCfg == nil || newCfg == nil {
		logger.Debug("hot diff: oldCfg 或 newCfg 为 nil，需要重启")
		return nil, false
	}

	// 没有重载接口的段必须语义等价。比较对 JSON 空白不敏感：管理员在设置页
	// 重新格式化一下模板，不该被当成真改动而触发重启。
	static := []struct {
		name     string
		old, new json_util.RawMessage
	}{
		{"log", oldCfg.LogConfig, newCfg.LogConfig},
		{"dns", oldCfg.DNSConfig, newCfg.DNSConfig},
		{"transport", oldCfg.Transport, newCfg.Transport},
		{"policy", oldCfg.Policy, newCfg.Policy},
		{"api", oldCfg.API, newCfg.API},
		{"stats", oldCfg.Stats, newCfg.Stats},
		{"reverse", oldCfg.Reverse, newCfg.Reverse},
		{"fakeDns", oldCfg.FakeDNS, newCfg.FakeDNS},
	}
	for _, section := range static {
		if !rawEqualNormalized(section.old, section.new) {
			logger.Debug("hot diff: [", section.name, "] 段有变化且没有重载接口，需要重启")
			return nil, false
		}
	}

	diff := &HotDiff{}
	if !diffInbounds(oldCfg, newCfg, diff) {
		return nil, false
	}
	if !diffOutbounds(oldCfg, newCfg, diff) {
		return nil, false
	}
	if !diffRouting(oldCfg, newCfg, diff) {
		return nil, false
	}
	return diff, true
}

// diffInbounds 计算入站的增删。改动过的入站按"先删后加"处理。
func diffInbounds(oldCfg, newCfg *Config, diff *HotDiff) bool {
	oldByTag, ok := inboundsByTag(oldCfg.InboundConfigs, "旧")
	if !ok {
		return false
	}
	newByTag, ok := inboundsByTag(newCfg.InboundConfigs, "新")
	if !ok {
		return false
	}

	for i := range oldCfg.InboundConfigs {
		oldIb := &oldCfg.InboundConfigs[i]
		newIb, exists := newByTag[oldIb.Tag]
		if exists && oldIb.Equals(newIb) {
			continue
		}
		// api 入站承载着面板正在用的那条 gRPC 连接，动它等于自断手脚。
		if oldIb.Tag == "api" {
			logger.Debug("hot diff: api 入站有变化，需要重启")
			return false
		}
		// Reality 的鉴权器无法靠 gRPC 的删+加可靠重建（3x-ui 实测结论）。
		if exists && (inboundUsesReality(oldIb) || inboundUsesReality(newIb)) {
			logger.Debug("hot diff: 入站 [", oldIb.Tag, "] 涉及 Reality，需要重启")
			return false
		}
		diff.RemovedInboundTags = append(diff.RemovedInboundTags, oldIb.Tag)
		if exists {
			raw, err := json.Marshal(newIb)
			if err != nil {
				logger.Debug("hot diff: 入站 [", oldIb.Tag, "] 序列化失败，需要重启:", err)
				return false
			}
			diff.AddedInbounds = append(diff.AddedInbounds, raw)
		}
	}

	for i := range newCfg.InboundConfigs {
		newIb := &newCfg.InboundConfigs[i]
		if _, exists := oldByTag[newIb.Tag]; exists {
			continue
		}
		if newIb.Tag == "api" {
			logger.Debug("hot diff: 新增了 api 入站，需要重启")
			return false
		}
		if inboundUsesReality(newIb) {
			logger.Debug("hot diff: 新增入站 [", newIb.Tag, "] 使用 Reality，需要重启")
			return false
		}
		raw, err := json.Marshal(newIb)
		if err != nil {
			logger.Debug("hot diff: 新增入站 [", newIb.Tag, "] 序列化失败，需要重启:", err)
			return false
		}
		diff.AddedInbounds = append(diff.AddedInbounds, raw)
	}
	return true
}

// inboundsByTag 按 tag 建索引。tag 为空或重复时返回 false——那种配置本身
// 就有问题，交给重启路径让核心去报错，别在这里替它拼凑。
//
// label 只用于日志（"旧"/"新"），标出是哪一侧的配置有问题。
func inboundsByTag(inbounds []InboundConfig, label string) (map[string]*InboundConfig, bool) {
	byTag := make(map[string]*InboundConfig, len(inbounds))
	for i := range inbounds {
		ib := &inbounds[i]
		if ib.Tag == "" {
			logger.Debug("hot diff: ", label, " 配置第 ", i, " 个入站 tag 为空，需要重启")
			return nil, false
		}
		if _, dup := byTag[ib.Tag]; dup {
			logger.Debug("hot diff: ", label, " 配置的入站 tag [", ib.Tag, "] 重复，需要重启")
			return nil, false
		}
		byTag[ib.Tag] = ib
	}
	return byTag, true
}

// inboundUsesReality 判断入站的 streamSettings.security 是否为 reality。
func inboundUsesReality(ib *InboundConfig) bool {
	if len(ib.StreamSettings) == 0 {
		return false
	}
	var ss struct {
		Security string `json:"security"`
	}
	if err := json.Unmarshal(ib.StreamSettings, &ss); err != nil {
		// 解不动就当它是，走重启：宁可多重启一次，也不要把一个自己都读不懂
		// 的入站热塞进核心。
		return true
	}
	// xray 把 security 小写化后再构建传输层（不区分大小写），所以
	// "REALITY"/"Reality" 都是能工作的 Reality 入站；直接做字符串相等会
	// 认不出它们，把一个实际用了 Reality 的入站错当成普通入站热交换掉。
	return strings.EqualFold(ss.Security, "reality")
}

// diffOutbounds 计算出站的增删。
//
// AetherUI 的 Config.OutboundConfigs 是整块 RawMessage（不是结构体切片），
// 所以先解成 []json.RawMessage 再按各自的 tag 索引。
//
// 数组首位是 xray 的默认出站，改动它会改变所有未命中规则的流量去向，而
// 控制面没有"换默认出站"的接口——必须重启；index ≥ 1 的出站增删则可以
// 热应用，见 decodeOutbounds 与下面两个循环的注释。
func diffOutbounds(oldCfg, newCfg *Config, diff *HotDiff) bool {
	if bytes.Equal(oldCfg.OutboundConfigs, newCfg.OutboundConfigs) {
		return true
	}
	oldList, oldTags, ok := decodeOutbounds(oldCfg.OutboundConfigs, "旧")
	if !ok {
		return false
	}
	newList, newTags, ok := decodeOutbounds(newCfg.OutboundConfigs, "新")
	if !ok {
		return false
	}
	if len(oldList) == 0 || len(newList) == 0 {
		logger.Debug("hot diff: 出站列表一侧为空，需要重启")
		return false
	}
	// 默认出站（首位）必须逐字节不变。
	if !rawEqualNormalized(oldList[0], newList[0]) {
		logger.Debug("hot diff: 默认出站有变化，需要重启")
		return false
	}

	oldByTag := map[string]json.RawMessage{}
	for i, tag := range oldTags {
		oldByTag[tag] = oldList[i]
	}
	newByTag := map[string]json.RawMessage{}
	for i, tag := range newTags {
		newByTag[tag] = newList[i]
	}

	// 按原数组顺序遍历，保证操作序列本身也是确定的。index 0（默认出站）
	// 跳过：它已经在上面被单独逐字节比对过，且真实生成的配置里它多半没有
	// tag（见 decodeOutbounds 的注释）——把它放进这两个循环，要么会把空
	// 字符串当 tag 传给 DelOutbound/走进 AddedOutbounds，要么（它恰好有
	// tag 时）会把它错当成一个可以被增删的普通出站，而它其实从不参与增删。
	for i, tag := range oldTags {
		if i == 0 {
			continue
		}
		newOb, exists := newByTag[tag]
		if exists && rawEqualNormalized(oldList[i], newOb) {
			continue
		}
		diff.RemovedOutboundTags = append(diff.RemovedOutboundTags, tag)
		if exists {
			diff.AddedOutbounds = append(diff.AddedOutbounds, newOb)
		}
	}
	for i, tag := range newTags {
		if i == 0 {
			continue
		}
		if _, exists := oldByTag[tag]; exists {
			continue
		}
		diff.AddedOutbounds = append(diff.AddedOutbounds, newList[i])
	}
	return true
}

// decodeOutbounds 把出站数组解成逐条的原始 JSON 与对应的 tag。
//
// index 0 是 xray 的默认出站。RoutingInjector.buildOutbounds 原样保留模板
// （web/service/config.json）里的出站数组再往后追加，而模板首位的 freedom
// 出站没有 tag——所以每一份真实生成的配置，数组首位都是无 tag 的。它豁免
// 「tag 非空」这条要求：反正它已经在 diffOutbounds 里被单独逐字节比对，
// 且天然不参与增删（改默认出站必须重启，见上）。index 0 若确实带了 tag，
// 仍要参与下面的去重检查，不能和后面的出站撞名。
//
// index ≥ 1 的元素依旧要求 tag 非空且唯一——这些才是真正可以被 gRPC
// 控制面增删的出站，tag 是核心用来定位它们的唯一标识。
//
// label 只用于日志（"旧"/"新"），标出是哪一侧的配置有问题。
func decodeOutbounds(raw json_util.RawMessage, label string) ([]json.RawMessage, []string, bool) {
	if len(raw) == 0 {
		logger.Debug("hot diff: ", label, " 配置的出站段为空，需要重启")
		return nil, nil, false
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		logger.Debug("hot diff: ", label, " 配置的出站段不是合法的 JSON 数组，需要重启:", err)
		return nil, nil, false
	}
	tags := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for i, item := range list {
		var head struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			logger.Debug("hot diff: ", label, " 配置第 ", i, " 个出站不是合法的 JSON 对象，需要重启:", err)
			return nil, nil, false
		}
		if head.Tag == "" {
			if i != 0 {
				logger.Debug("hot diff: ", label, " 配置第 ", i, " 个出站 tag 为空，需要重启")
				return nil, nil, false
			}
			tags = append(tags, "")
			continue
		}
		if seen[head.Tag] {
			logger.Debug("hot diff: ", label, " 配置的出站 tag [", head.Tag, "] 重复，需要重启")
			return nil, nil, false
		}
		seen[head.Tag] = true
		tags = append(tags, head.Tag)
	}
	return list, tags, true
}

// diffRouting 判断路由改动能否热应用。
//
// rules 与 balancers 有重载接口，其余键（主要是 domainStrategy /
// domainMatcher）在进程启动时固定，变了就必须重启。
func diffRouting(oldCfg, newCfg *Config, diff *HotDiff) bool {
	if rawEqualNormalized(oldCfg.RouterConfig, newCfg.RouterConfig) {
		return true
	}
	// 一侧完全没有 routing 段，说明运行中的核心可能压根没起路由模块，
	// 那样 RoutingService 也不在，只能重启。
	if len(oldCfg.RouterConfig) == 0 || len(newCfg.RouterConfig) == 0 {
		logger.Debug("hot diff: routing 段一侧缺失，需要重启")
		return false
	}
	oldRest, ok := routingWithoutReloadable(oldCfg.RouterConfig)
	if !ok {
		logger.Debug("hot diff: 旧配置的 routing 段无法解析（去掉 rules/balancers 后），需要重启")
		return false
	}
	newRest, ok := routingWithoutReloadable(newCfg.RouterConfig)
	if !ok {
		logger.Debug("hot diff: 新配置的 routing 段无法解析（去掉 rules/balancers 后），需要重启")
		return false
	}
	if !bytes.Equal(oldRest, newRest) {
		logger.Debug("hot diff: routing 的不可重载部分有变化，需要重启")
		return false
	}
	diff.RoutingConfig = append([]byte(nil), newCfg.RouterConfig...)
	return true
}

// routingWithoutReloadable 把 routing 段里可运行时重载的键摘掉，
// 返回归一化后的剩余部分，用于比较"必须重启的那一半"。
func routingWithoutReloadable(raw []byte) ([]byte, bool) {
	parsed := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, false
	}
	delete(parsed, "rules")
	delete(parsed, "balancers")
	// encoding/json 对 map key 排序，输出因此是确定的。
	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rawEqualNormalized 比较两段 JSON 的语义是否相同，忽略空白与 key 顺序。
//
// 不能直接 bytes.Equal：设置页把模板重新格式化一遍是很常见的操作，那不该
// 被当成配置变化而触发一次全员断线的重启。
//
// 参数类型用 []byte 而非 json_util.RawMessage：调用方既有 Config 字段那样
// 的 json_util.RawMessage，也有 decodeOutbounds 解出来的标准库
// json.RawMessage——两者底层都是 []byte 但互相不能直接赋值，唯有都赋值给
// 未命名类型 []byte 的形参才不需要在每个调用点手动转换。
func rawEqualNormalized(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	na, okA := normalizeJSON(a)
	nb, okB := normalizeJSON(b)
	if !okA || !okB {
		// 有一侧解不动就退回字节比较的结论（已知不等），走重启。
		return false
	}
	return bytes.Equal(na, nb)
}

// normalizeJSON 把一段 JSON 解出来再序列化回去，消除空白差异并让 map key
// 排序。空输入视作合法，归一化成空。
func normalizeJSON(raw []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, true
	}
	var v any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}
