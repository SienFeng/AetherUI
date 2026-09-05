package service

import (
	"encoding/json"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
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
	ipdbService        IPDBService
}

func (s *RoutingInjector) Inject(cfg *xray.Config) error {
	outbounds, usableOutboundTags, err := s.buildOutbounds(cfg.OutboundConfigs)
	if err != nil {
		return err
	}
	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)

	blockRules, proxyRules, err := s.buildRules(usableOutboundTags)
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
	geoRules, err := s.buildGeoRules()
	if err != nil {
		return err
	}
	// 地区规则排在本项目生成的其余规则之前。这是对「一律 append 到末尾」
	// 的一处受控例外：模板原有的安全规则仍保持更高优先级，但地区限制属于
	// 准入判定，逻辑上必须先于任何分流决策。排在分流之后的话，非允许地区的
	// 用户访问被分流的域名时会先命中分流规则走代理出站，限制被静默绕过。
	rules = append(rules, geoRules...)
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

// buildOutbounds 返回注入后的出站数组，以及「实际写入了配置的」节点 id -> tag 映射。
//
// 第二个返回值是关键：buildRules 必须只认这些 tag。一个 Config 损坏而被跳过的节点，
// 如果其 tag 仍被规则引用，就会形成悬空 outboundTag —— 而 xray 对此不报错，运行时
// 会静默回落到默认出站（直连），造成「以为分流/封禁了，其实直连出去」。
// tagDefaultOutbound 给模板里首个出站补上 tag。
//
// 首个出站就是 xray 的默认出站，未命中任何路由规则的流量都走它。xray 只在
// 出站带 tag 时才往访问日志写 "[入站 -> 出站]"，裸出站产生的记录没有方括号，
// 会被 accesslog.ParseLine 丢弃——直连流量在访问日志里整片消失，而分流出去的
// 流量记录完好，管理员看到的是一份沉默地偏斜的记录。
//
// 只补首个：其余没有 tag 的出站无法被路由规则引用，本来就永远不会被选中。
// 已经有 tag 的不覆盖——用户的路由规则可能正引用着它，改掉会让规则指向不存在
// 的出站，而 xray 对悬空 outboundTag 不报错，只会静默回落直连。
//
// 只作用于模板里已有的出站，必须在 append 生成的出站之前调用：那些出站自带
// tag，补上去会造成重复。
func tagDefaultOutbound(outbounds []any) {
	if len(outbounds) == 0 {
		return
	}
	ob, ok := outbounds[0].(map[string]any)
	if !ok || ob == nil {
		return
	}
	if tag, ok := ob["tag"].(string); ok && tag != "" {
		return
	}
	ob["tag"] = model.DefaultOutboundTag
}

func (s *RoutingInjector) buildOutbounds(existing json_util.RawMessage) ([]any, map[int]string, error) {
	outbounds := make([]any, 0)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &outbounds); err != nil {
			return nil, nil, err
		}
	}
	tagDefaultOutbound(outbounds)

	nodes, err := s.outboundService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	usable := make(map[int]string, len(nodes))
	for _, node := range nodes {
		// C1 修复之前分配出去的保留 tag 节点可能还躺在库里。放行它就会输出
		// 两个 a-ui-block，xray 报 "existing tag found" 并拒绝启动——全员断网，
		// 而面板首页照样显示 running。宁可这一个节点不生效。
		if model.IsReservedTag(node.Tag) {
			logger.Warning("skip outbound node whose tag collides with a reserved tag, id:",
				node.Id, "tag:", node.Tag)
			continue
		}
		var ob map[string]any
		if err := json.Unmarshal([]byte(node.Config), &ob); err != nil {
			// 单个节点配置损坏时跳过，不让整份配置生成失败；但必须记录，
			// 否则管理员无从察觉这个节点已经不生效了。
			logger.Warning("skip outbound node with corrupt config, id:", node.Id,
				"tag:", node.Tag, "err:", err)
			continue
		}
		// Config 为 "null" 时 Unmarshal 不报错却留下一个 nil map，下面那行赋值
		// 会 panic。本函数由每 10 秒的重启 cron 走到，而 cron 没有 panic 恢复，
		// 一个 panic 就会杀掉整个面板进程。
		if ob == nil {
			logger.Warning("skip outbound node whose config decodes to null, id:",
				node.Id, "tag:", node.Tag)
			continue
		}
		ob["tag"] = node.Tag
		outbounds = append(outbounds, ob)
		usable[node.Id] = node.Tag
	}

	// 黑洞出站始终注入，不复用模板里的 blocked——用户可能把它删掉，
	// 而 xray 对悬空 outboundTag 不报错，block 规则会静默变成直连。
	outbounds = append(outbounds, map[string]any{
		"tag":      model.BlockOutboundTag,
		"protocol": "blackhole",
		"settings": map[string]any{},
	})
	return outbounds, usable, nil
}

func (s *RoutingInjector) buildRules(outboundTagById map[int]string) ([]any, []any, error) {
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

	blockRules := make([]any, 0)
	proxyRules := make([]any, 0)
	for _, rule := range rules {
		generated, isBlock, skip := s.buildRule(rule, inboundTagById, outboundTagById)
		if skip != nil {
			// 设计 §5.3 接受这道防线的理由是「宁可规则不生效，用户能察觉」。
			// 跳过若不记日志，用户其实察觉不到：规则表照常渲染，生成的配置里
			// 却没有这条规则，流量默默走了默认出站。
			logger.Warning("skip routing rule, id:", rule.Id, "remark:", rule.Remark,
				"reason:", skip)
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

// buildRule 生成一条规则，或说明为什么必须整条丢弃。
//
// 第三个返回值非 nil 即表示跳过，且必须给出原因——调用方要把它记进日志，
// 否则这道防线对用户是隐形的。
//
// 绝不能退而求其次生成一条缺少 domain 的规则：xray 把缺失的条件视为
// 「不限制」，那样的规则会劫持该入站的全部流量，且不会有任何报错。
//
// 域名来自一条规则引用的多个域名组的合并（DomainGroupIds，升序）。每个组内
// 再合并手工录入（Domains 字段）与订阅更新（SubscribedDomains 字段）。两级
// 合并顺序都是确定的（组间按 id 升序、组内手工在前订阅在后，均保留首次出现），
// 是「生成逐字节确定」不变量的一部分。
func (s *RoutingInjector) buildRule(
	rule *model.RoutingRule,
	inboundTagById map[int]string,
	outboundTagById map[int]string,
) (map[string]any, bool, error) {
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return nil, false, common.NewError("规则的域名组数据损坏, id:", rule.Id, "err:", err)
	}
	if len(groupIds) == 0 {
		return nil, false, common.NewError("规则没有指定任何域名组, id:", rule.Id,
			"（域名条件为空会让规则退化成劫持该入站全部流量）")
	}

	// 按 DomainGroupIds 的升序逐组取域名。失效的组剔除而不是整条丢弃：
	// 一个订阅从未拉取成功的空组，不该把同一条规则里本来好好的分流一起
	// 废掉；对 block 规则尤其如此——整条丢弃等于本该封禁的域名全部裸奔，
	// 部分生成至少封住了还在的那部分。
	//
	// 「数据损坏」与「组为空」的后果完全相同（该组贡献 0 条域名），统一
	// 走剔除；剔除的方向是缩小匹配范围，安全侧一致。
	lists := make([][]string, 0, len(groupIds))
	for _, gid := range groupIds {
		group, groupErr := s.domainGroupService.Get(gid)
		if groupErr != nil {
			logger.Warning("routing rule drops a domain group that no longer exists, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		manual, decodeErr := DecodeDomains(group.Domains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a domain group with corrupt manual domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		subscribed, decodeErr := DecodeSubscribedDomains(group.SubscribedDomains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a domain group with corrupt subscribed domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		// 组内合并顺序确定（手工在前、订阅在后、保留首次出现）。
		one := MergeDomains(manual, subscribed)
		if len(one) == 0 {
			logger.Warning("routing rule drops an empty domain group, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		lists = append(lists, one)
	}

	// 跨组按上面的遍历顺序合并去重。禁止改用遍历 map 产生顺序——
	// 那样生成不再逐字节确定，Config.Equals 恒为 false，10 秒的重启 cron
	// 会不停重启 xray。
	domains := MergeDomains(lists...)
	if len(domains) == 0 {
		return nil, false, common.NewError("规则的域名组全部不存在或为空, rule id:", rule.Id,
			"group ids:", groupIds,
			"（域名条件为空会让规则退化成劫持该入站全部流量）")
	}

	generated := map[string]any{
		"type":   "field",
		"domain": domains,
	}

	inboundIds, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return nil, false, common.NewError("规则的入站数据损坏, id:", rule.Id, "err:", err)
	}
	if len(inboundIds) > 0 {
		tags := make([]string, 0, len(inboundIds))
		missing := make([]int, 0)
		for _, id := range inboundIds {
			tag, ok := inboundTagById[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			tags = append(tags, tag)
		}
		if len(tags) == 0 {
			// 剩下空数组绝不能输出。实测（Xray 26.7.28）确认 xray 把
			// inboundTag: [] 当作「不限制」而非「不匹配任何入站」——一条本该
			// 只覆盖甲的规则会劫持所有人的这批域名，且 Configuration OK、
			// 面板首页照样显示 running。与 domain 为空数组是同一类事故。
			return nil, false, common.NewError("规则指定的入站全部不存在或已禁用, ids:", inboundIds)
		}
		if len(missing) > 0 {
			// 部分失效不整条丢弃：剩下的入站仍该按规则走。但必须记录，
			// 否则被剔除的那些用户会静默回落直连而无人察觉。
			logger.Warning("routing rule drops inbounds that no longer exist or are disabled, rule id:",
				rule.Id, "inbound ids:", missing)
		}
		// tags 的顺序由 InboundIds 的升序保证，不得改用遍历 inboundTagById
		// 这个 map 来产生顺序——那样生成不再逐字节确定。
		generated["inboundTag"] = tags
	}

	switch rule.Action {
	case model.ActionBlock:
		generated["outboundTag"] = model.BlockOutboundTag
		return generated, true, nil
	case model.ActionProxy:
		tag, ok := outboundTagById[rule.OutboundId]
		if !ok {
			return nil, false, common.NewError("出站节点不存在、已禁用或未写入配置, id:",
				rule.OutboundId)
		}
		generated["outboundTag"] = tag
		return generated, false, nil
	default:
		return nil, false, common.NewError("未知的动作:", rule.Action)
	}
}

// buildGeoRules 生成地区限制规则，并把允许集写进 bin/a-ui-geo.dat。
//
// 任何一步失败都返回错误让整份配置生成失败，绝不退而求其次地省掉规则：
// 省掉等于地区限制失效，而面板上一切显示正常。配置生成失败时 xray 保持
// 原状继续跑，是安全的一侧。
func (s *RoutingInjector) buildGeoRules() ([]any, error) {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	if !AnyInboundUsesRegions(inbounds) {
		return nil, nil
	}

	db := s.ipdbService.DB()
	if db == nil {
		return nil, common.NewError("已配置地区限制，但 IP 归属地库未加载；" +
			"请到「面板设置 → IP 归属地库」更新，或清空入站的地区限制")
	}
	plan, err := buildGeoPlan(inbounds, db)
	if err != nil {
		return nil, err
	}
	if len(plan.Entries) == 0 {
		return nil, nil
	}
	hash, err := writeGeoDat(geoDatPath, plan.Entries)
	if err != nil {
		return nil, common.NewError("生成地区限制数据文件失败:", err)
	}
	return rulesFromGeoPlan(inbounds, plan, hash), nil
}
