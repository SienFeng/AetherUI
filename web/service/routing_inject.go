package service

import (
	"encoding/json"

	"a-ui/database/model"
	"a-ui/util/json_util"
	"a-ui/xray"
)

// RoutingInjector 把数据库里的出站节点与分流规则增量注入到 xray 配置中。
// 用户手写的 xrayTemplateConfig 原样保留，生成内容一律追加在末尾：
//   - 出站追加到末尾，模板里的 freedom 才能继续当 xray 的默认出站
//   - 规则追加到末尾，模板里屏蔽私网/BT 的安全规则才能保持更高优先级
type RoutingInjector struct {
	domainGroupService DomainGroupService
	outboundService    OutboundNodeService
	ruleService        RoutingRuleService
	inboundService     InboundService
}

func (s *RoutingInjector) Inject(cfg *xray.Config) error {
	outbounds, err := s.buildOutbounds(cfg.OutboundConfigs)
	if err != nil {
		return err
	}
	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)

	blockRules, proxyRules, err := s.buildRules()
	if err != nil {
		return err
	}

	routing := map[string]any{}
	if len(cfg.RouterConfig) > 0 {
		if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
			return err
		}
	}
	rules, _ := routing["rules"].([]any)
	if rules == nil {
		rules = make([]any, 0)
	}
	rules = append(rules, blockRules...)
	rules = append(rules, proxyRules...)
	routing["rules"] = rules

	encodedRouting, err := json.Marshal(routing)
	if err != nil {
		return err
	}
	cfg.RouterConfig = json_util.RawMessage(encodedRouting)
	return nil
}

func (s *RoutingInjector) buildOutbounds(existing json_util.RawMessage) ([]any, error) {
	outbounds := make([]any, 0)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &outbounds); err != nil {
			return nil, err
		}
	}

	nodes, err := s.outboundService.GetEnabled()
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		var ob map[string]any
		if err := json.Unmarshal([]byte(node.Config), &ob); err != nil {
			// 单个节点配置损坏时跳过，不能让整份配置生成失败
			continue
		}
		ob["tag"] = node.Tag
		outbounds = append(outbounds, ob)
	}

	// 黑洞出站始终注入，不复用模板里的 blocked——用户可能把它删掉，
	// 而 xray 对悬空 outboundTag 不报错，block 规则会静默变成直连。
	outbounds = append(outbounds, map[string]any{
		"tag":      model.BlockOutboundTag,
		"protocol": "blackhole",
		"settings": map[string]any{},
	})
	return outbounds, nil
}

func (s *RoutingInjector) buildRules() ([]any, []any, error) {
	rules, err := s.ruleService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	if len(rules) == 0 {
		return nil, nil, nil
	}

	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, nil, err
	}
	inboundTagById := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		if in.Enable {
			inboundTagById[in.Id] = in.Tag
		}
	}

	nodes, err := s.outboundService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	outboundTagById := make(map[int]string, len(nodes))
	for _, node := range nodes {
		outboundTagById[node.Id] = node.Tag
	}

	blockRules := make([]any, 0)
	proxyRules := make([]any, 0)
	for _, rule := range rules {
		generated, isBlock := s.buildRule(rule, inboundTagById, outboundTagById)
		if generated == nil {
			continue
		}
		if isBlock {
			blockRules = append(blockRules, generated)
		} else {
			proxyRules = append(proxyRules, generated)
		}
	}
	return blockRules, proxyRules, nil
}

// buildRule 返回 nil 表示这条规则条件残缺，必须整条丢弃。
// 绝不能退而求其次生成一条缺少 domain 的规则：xray 把缺失的条件视为
// 「不限制」，那样的规则会劫持该入站的全部流量，且不会有任何报错。
func (s *RoutingInjector) buildRule(
	rule *model.RoutingRule,
	inboundTagById map[int]string,
	outboundTagById map[int]string,
) (map[string]any, bool) {
	group, err := s.domainGroupService.Get(rule.DomainGroupId)
	if err != nil {
		return nil, false
	}
	domains, err := DecodeDomains(group.Domains)
	if err != nil || len(domains) == 0 {
		return nil, false
	}

	generated := map[string]any{
		"type":   "field",
		"domain": domains,
	}

	if rule.InboundId > 0 {
		tag, ok := inboundTagById[rule.InboundId]
		if !ok {
			return nil, false
		}
		generated["inboundTag"] = []string{tag}
	}

	switch rule.Action {
	case model.ActionBlock:
		generated["outboundTag"] = model.BlockOutboundTag
		return generated, true
	case model.ActionProxy:
		tag, ok := outboundTagById[rule.OutboundId]
		if !ok {
			return nil, false
		}
		generated["outboundTag"] = tag
		return generated, false
	default:
		return nil, false
	}
}
