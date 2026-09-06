package service

import (
	"encoding/json"
	"strings"
	"time"

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
	settingService     SettingService
}

func (s *RoutingInjector) Inject(cfg *xray.Config) error {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return err
	}
	inboundTagById := enabledInboundTags(inbounds)

	// 计量池：出站与规则必须用同一份，理由见 filterMeterPool。
	// 用量库不可用时 Pool 返回空，整个计量功能自动停用，配置照常生成。
	meterPool, err := (&MeterPoolService{}).Pool(time.Now())
	if err != nil {
		return err
	}
	meterPool = filterMeterPool(meterPool, inboundTagById)

	outbounds, usableOutboundTags, defaultOutboundTag, err := s.buildOutbounds(cfg.OutboundConfigs)
	if err != nil {
		return err
	}
	outbounds, meterPool, err = appendMeterOutbounds(outbounds, meterPool)
	if err != nil {
		return err
	}
	encodedOutbounds, err := json.Marshal(outbounds)
	if err != nil {
		return err
	}
	cfg.OutboundConfigs = json_util.RawMessage(encodedOutbounds)

	blockRules, routeRules, err := s.buildRules(inboundTagById, usableOutboundTags, defaultOutboundTag)
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

	// 开关为 0 时【不碰】domainStrategy：模板里管理员可能手写过它，
	// 覆盖成默认值是在他不知情时改变分流行为。升级后行为零变化也靠这一条。
	//
	// 这一段必须排在计量规则之前：计量规则的形态取决于核心会不会在路由匹配期
	// 解析域名，判据正是这里写完之后的最终 domainStrategy。
	resolveDomain, err := s.settingService.GetIPRuleResolveDomain()
	if err != nil {
		return err
	}
	if resolveDomain {
		routing["domainStrategy"] = "IPIfNonMatch"
	}
	meterRules := buildMeterRules(meterPool, inboundTagById,
		meterRuleNeedsIPGuard(routing["domainStrategy"]))

	// 地区规则排在本项目生成的其余规则之前。这是对「一律 append 到末尾」
	// 的一处受控例外：模板原有的安全规则仍保持更高优先级，但地区限制属于
	// 准入判定，逻辑上必须先于任何分流决策。排在分流之后的话，非允许地区的
	// 用户访问被分流的域名时会先命中分流规则走代理出站，限制被静默绕过。
	//
	// 计量规则永远在最末：只有「本来会走默认出站」的流量才进计量，已被管理员
	// 规则命中的流量完全不受影响。
	rules = append(rules, geoRules...)
	rules = append(rules, blockRules...)
	rules = append(rules, routeRules...)
	rules = append(rules, meterRules...)
	routing["rules"] = rules

	encodedRouting, err := json.Marshal(routing)
	if err != nil {
		return err
	}
	cfg.RouterConfig = json_util.RawMessage(encodedRouting)
	return nil
}

// buildOutbounds 返回注入后的出站数组、「实际写入了配置的」节点 id -> tag 映射，
// 以及默认出站实际生效的 tag（供 ActionDirect 的规则引用，理由见 tagDefaultOutbound）。
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
//
// 返回实际生效的那个 tag（补上去的，或者模板里原本就有的），空串表示模板里
// 压根没有可引用的默认出站。ActionDirect 的规则要引用它，**必须用这个返回值
// 而不是硬编码 model.DefaultOutboundTag**：管理员手写模板给首个出站起过名字
// 时上面那条早退分支会原样保留，硬编码就会发出一个悬空 outboundTag。那种
// 情况下 xray 不报错、运行时静默回落默认出站——结果碰巧还是直连，于是这个
// 错误既不会被发现也不会被修，直到某天默认出站不再是 freedom。
func tagDefaultOutbound(outbounds []any) string {
	if len(outbounds) == 0 {
		return ""
	}
	ob, ok := outbounds[0].(map[string]any)
	if !ok || ob == nil {
		return ""
	}
	if tag, ok := ob["tag"].(string); ok && tag != "" {
		return tag
	}
	ob["tag"] = model.DefaultOutboundTag
	return model.DefaultOutboundTag
}

// enabledInboundTags 返回启用入站的 id -> tag。
//
// 提到 Inject 里算一次再往下传，是因为计量规则与分流规则都要用它。
// 各自再查一次 GetAllInbounds 的话，每次配置生成就要多读一遍带着
// settings/streamSettings/sniffing 这些 JSON 大字段的整张表。
func enabledInboundTags(inbounds []*model.Inbound) map[int]string {
	byId := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		if in.Enable {
			byId[in.Id] = in.Tag
		}
	}
	return byId
}

// filterMeterPool 剔除指向已停用或已删除入站的池行。
//
// 出站与规则必须消费**同一份**过滤后的列表：只过滤一侧会留下没有规则引用
// 的孤儿出站（无害但无意义），或者引用不存在出站的悬空规则——后者危险得多，
// xray 对悬空 outboundTag 不报错，运行时静默回落默认出站。
func filterMeterPool(pool []MeterEntry, inboundTagById map[int]string) []MeterEntry {
	out := make([]MeterEntry, 0, len(pool))
	for _, e := range pool {
		if _, ok := inboundTagById[e.InboundId]; ok {
			out = append(out, e)
		}
	}
	return out
}

// appendMeterOutbounds 给池里每个 (入站, 域名) 追加一个默认出站的深拷贝，
// 返回新的出站数组与**实际写进配置的**池条目。
//
// 第二个返回值是关键，与 buildOutbounds 返回 usableOutboundTags 是同一个
// 道理：调用方必须只为实际写进配置的那些条目生成规则，否则会形成悬空
// outboundTag，而 xray 对此不报错、静默回落默认出站。
//
// 深拷贝走 JSON 往返：浅拷贝会共享 settings 那个 map，DNSInjector 给计量
// 出站写 domainStrategy 时会同时写进默认出站（或者反过来），两者再也分不开。
//
// 拿不到可复制的默认出站时整个放弃计量并记 Warning，而不是退而求其次自己
// 造一个 freedom：默认出站是管理员可以改的（换协议、加 sendThrough），
// 造一个假的等于让被计量的流量走上一条与其它直连流量不同的路径。
func appendMeterOutbounds(outbounds []any, pool []MeterEntry) ([]any, []MeterEntry, error) {
	if len(pool) == 0 {
		return outbounds, nil, nil
	}
	if len(outbounds) == 0 {
		logger.Warning("生成配置里没有出站，本次不生成计量出站；域名榜单的字节列会保持为空")
		return outbounds, nil, nil
	}
	base, ok := outbounds[0].(map[string]any)
	if !ok || base == nil {
		logger.Warning("模板首位出站不是一个对象，本次不生成计量出站；域名榜单的字节列会保持为空")
		return outbounds, nil, nil
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range pool {
		var clone map[string]any
		if err := json.Unmarshal(encoded, &clone); err != nil {
			return nil, nil, err
		}
		clone["tag"] = model.MeterTag(e.InboundId, e.Domain)
		outbounds = append(outbounds, clone)
	}
	return outbounds, pool, nil
}

// meterRuleNeedsIPGuard 判断计量规则要不要带 ip 守卫。
//
// 守卫是 ip: ["0.0.0.0/0", "::/0"]：匹配任意 IP，但**要求目标已经有 IP**。
// 域名目标在第一遍匹配时没有 IP，规则整条不命中；第二遍挂上 DNS 客户端、
// 解析出 IP 之后守卫恒真。于是所有既有的 ip 条件规则（它们都排在计量规则
// 之前）在第二遍照常拿到它们本来的机会。
//
// 不加守卫的后果在真实 xray 上实测复现过（spec §6.1 用例 A）：计量规则在
// 第一遍命中，第二遍永远不发生，模板自带的 geoip:private 与管理员所有 CIDR
// 规则对池内域名——恰恰是流量最大的那批——静默失效。
//
// 判据必须与 xray 自己的解析完全一致：infra/conf/router.go 的
// getDomainStrategy 就是 strings.ToLower 之后 switch ipifnonmatch /
// ipondemand，其余一律 AsIs。判错的两种后果不对称：该带没带是上面那个安全
// 问题，不该带却带了只是计量空转（用例 D），所以无法识别的值一律按
// 「不解析」处理，落在后者。
func meterRuleNeedsIPGuard(strategy any) bool {
	s, ok := strategy.(string)
	if !ok {
		return false
	}
	switch strings.ToLower(s) {
	case "ipifnonmatch", "ipondemand":
		return true
	default:
		return false
	}
}

// buildMeterRules 生成计量规则。调用方必须把它们追加在所有其它规则之后。
//
// 这里刻意把 domain 与 ip 并进同一条规则，看上去违反了「绝不把两类条件并进
// 同一条（那是 AND）」那条不变量——必须解释清楚，否则将来一定会有人来「修」它。
// 那条不变量约束的是**管理员表达的**规则：管理员说「这批域名**或**这批 IP 走
// B」，写成一条就变成 AND、几乎永不命中。这里的 AND 是刻意要的——「域名是 X
// **且** 目标已经解析出 IP」，第二个合取项不是匹配条件，是一个遍次闸门。
// buildRule 生成管理员规则时仍然严格拆成两条，一个字节都不改。
func buildMeterRules(pool []MeterEntry, inboundTagById map[int]string, guard bool) []any {
	rules := make([]any, 0, len(pool))
	for _, e := range pool {
		// pool 已由 filterMeterPool 过滤过，这里必然取得到。
		tag := inboundTagById[e.InboundId]
		rule := map[string]any{
			"type":       "field",
			"inboundTag": []string{tag},
			// 一律带显式 domain: 前缀。含点的裸串在 xray 里是子串匹配
			//（infra/conf/router.go:175 的 defaultType 是 Domain_Substr），
			// doubleclick.net 会命中 notdoubleclick.net.evil。
			"domain":      []string{"domain:" + e.Domain},
			"outboundTag": model.MeterTag(e.InboundId, e.Domain),
		}
		if guard {
			rule["ip"] = []string{"0.0.0.0/0", "::/0"}
		}
		rules = append(rules, rule)
	}
	return rules
}

func (s *RoutingInjector) buildOutbounds(existing json_util.RawMessage) ([]any, map[int]string, string, error) {
	outbounds := make([]any, 0)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &outbounds); err != nil {
			return nil, nil, "", err
		}
	}
	defaultTag := tagDefaultOutbound(outbounds)

	nodes, err := s.outboundService.GetEnabled()
	if err != nil {
		return nil, nil, "", err
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
	return outbounds, usable, defaultTag, nil
}

// buildRules 返回两组规则：block 的一组必须整体排在另一组之前（见 Inject）。
//
// 第二组把 proxy 与 direct 混在一起，按 priority asc, id asc 排序——它们是
// 对等的分流动作，都只是「把命中的流量送到某个出站」，谁在前由管理员设的
// 优先级决定。只有 block 需要单独提前，那是硬约束：违规域名的封禁不能被
// 任何一条分流规则绕过。
func (s *RoutingInjector) buildRules(
	inboundTagById map[int]string,
	outboundTagById map[int]string,
	defaultOutboundTag string,
) ([]any, []any, error) {
	rules, err := s.ruleService.GetEnabled()
	if err != nil {
		return nil, nil, err
	}
	if len(rules) == 0 {
		return nil, nil, nil
	}
	// inboundTagById 由 Inject 算好传进来：计量规则也要用它，各自查一次
	// GetAllInbounds 会让每次配置生成多读一遍带 JSON 大字段的整张表。
	blockRules := make([]any, 0)
	routeRules := make([]any, 0)
	for _, rule := range rules {
		generated, isBlock, skip := s.buildRule(rule, inboundTagById, outboundTagById, defaultOutboundTag)
		if skip != nil {
			// 设计 §5.3 接受这道防线的理由是「宁可规则不生效，用户能察觉」。
			// 跳过若不记日志，用户其实察觉不到：规则表照常渲染，生成的配置里
			// 却没有这条规则，流量默默走了默认出站。
			logger.Warning("skip routing rule, id:", rule.Id, "remark:", rule.Remark,
				"reason:", skip)
			continue
		}
		// 一条数据库规则最多产出两条 xray 规则（域名一条、IP 一条）。
		// 顺序由 buildRule 固定（domain 在前），这里原样追加，不重排。
		for _, g := range generated {
			if isBlock {
				blockRules = append(blockRules, g)
			} else {
				routeRules = append(routeRules, g)
			}
		}
	}
	return blockRules, routeRules, nil
}

// buildRule 生成 0~2 条 xray 规则，或说明为什么必须整条丢弃。
//
// 第三个返回值非 nil 即表示跳过，且必须给出原因——调用方要把它记进日志，
// 否则这道防线对用户是隐形的。
//
// 一条数据库规则最多产出两条 xray 规则：域名条件一条、IP 条件一条。
// **绝不把两者并列进同一条**——同一条规则内的条件是 AND
// （app/router/config.go:33 的 BuildCondition + condition.go:35 的
// ConditionChan.Apply），并列会让「这批域名或这批 IP 走 B」变成「域名命中
// 且解析出的 IP 也命中」，几乎永不命中，而 xray 返回 Configuration OK、
// 面板首页照样显示 running。
//
// 也绝不能退而求其次生成一条条件为空数组的规则：xray 把长度为 0 的条件
// 视为「不限制」，那样的规则会劫持该入站的全部流量，且不会有任何报错。
//
// 条件来自一条规则引用的多个分流组的合并（DomainGroupIds，升序）。每个组内
// 再合并手工录入与订阅更新。两级合并顺序都是确定的，是「生成逐字节确定」
// 不变量的一部分。
func (s *RoutingInjector) buildRule(
	rule *model.RoutingRule,
	inboundTagById map[int]string,
	outboundTagById map[int]string,
	defaultOutboundTag string,
) ([]map[string]any, bool, error) {
	groupIds, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil {
		return nil, false, common.NewError("规则的分流组数据损坏, id:", rule.Id, "err:", err)
	}
	if len(groupIds) == 0 {
		return nil, false, common.NewError("规则没有指定任何分流组, id:", rule.Id,
			"（条件为空会让规则退化成劫持该入站全部流量）")
	}

	// 按 DomainGroupIds 的升序逐组取条件。失效的组剔除而不是整条丢弃：
	// 一个订阅从未拉取成功的空组，不该把同一条规则里本来好好的分流一起
	// 废掉；对 block 规则尤其如此——整条丢弃等于本该封禁的目标全部裸奔，
	// 部分生成至少封住了还在的那部分。
	//
	// 「数据损坏」与「组为空」的后果完全相同（该组贡献 0 条条件），统一
	// 走剔除；剔除的方向是缩小匹配范围，安全侧一致。
	domainLists := make([][]string, 0, len(groupIds))
	cidrLists := make([][]string, 0, len(groupIds))
	for _, gid := range groupIds {
		group, groupErr := s.domainGroupService.Get(gid)
		if groupErr != nil {
			logger.Warning("routing rule drops a group that no longer exists, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		manualDomains, decodeErr := DecodeDomains(group.Domains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt manual domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		subscribedDomains, decodeErr := DecodeSubscribedDomains(group.SubscribedDomains)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt subscribed domains, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		manualCidrs, decodeErr := DecodeCidrs(group.Cidrs)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt manual cidrs, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		subscribedCidrs, decodeErr := DecodeSubscribedCidrs(group.SubscribedCidrs)
		if decodeErr != nil {
			logger.Warning("routing rule drops a group with corrupt subscribed cidrs, rule id:",
				rule.Id, "group id:", gid, "err:", decodeErr)
			continue
		}
		// 组内合并顺序确定（手工在前、订阅在后、保留首次出现）。
		// MergeDomains 只是「有序去重」，两类值都用它。
		oneDomains := MergeDomains(manualDomains, subscribedDomains)
		oneCidrs := MergeDomains(manualCidrs, subscribedCidrs)
		if len(oneDomains) == 0 && len(oneCidrs) == 0 {
			logger.Warning("routing rule drops an empty group, rule id:",
				rule.Id, "group id:", gid)
			continue
		}
		if len(oneDomains) > 0 {
			domainLists = append(domainLists, oneDomains)
		}
		if len(oneCidrs) > 0 {
			cidrLists = append(cidrLists, oneCidrs)
		}
	}

	// 跨组按上面的遍历顺序合并去重。禁止改用遍历 map 产生顺序——
	// 那样生成不再逐字节确定，Config.Equals 恒为 false，10 秒的重启 cron
	// 会不停重启 xray。
	domains := MergeDomains(domainLists...)
	cidrs := MergeDomains(cidrLists...)
	if len(domains) == 0 && len(cidrs) == 0 {
		return nil, false, common.NewError("规则的分流组全部不存在或为空, rule id:", rule.Id,
			"group ids:", groupIds,
			"（条件为空会让规则退化成劫持该入站全部流量）")
	}

	inboundIds, err := DecodeInboundIds(rule.InboundIds)
	if err != nil {
		return nil, false, common.NewError("规则的入站数据损坏, id:", rule.Id, "err:", err)
	}
	var inboundTags []string
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
			// 只覆盖甲的规则会劫持所有人的流量，且 Configuration OK、
			// 面板首页照样显示 running。
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
		inboundTags = tags
	}

	var outboundTag string
	isBlock := false
	switch rule.Action {
	case model.ActionBlock:
		outboundTag = model.BlockOutboundTag
		isBlock = true
	case model.ActionProxy:
		tag, ok := outboundTagById[rule.OutboundId]
		if !ok {
			return nil, false, common.NewError("出站节点不存在、已禁用或未写入配置, id:",
				rule.OutboundId)
		}
		outboundTag = tag
	case model.ActionDirect:
		// 引用默认出站实际生效的 tag，绝不硬编码 model.DefaultOutboundTag。
		//
		// 取不到就整条丢弃，哪怕悬空引用在这里「碰巧」也会回落到默认出站、
		// 结果与预期一致：那是同一份不变量（绝不输出悬空 outboundTag）在
		// 唯一一处后果不严重的地方，破一次例就等于把它降级成建议。而且
		// 一旦模板的默认出站不再是 freedom，这个巧合会当场变成静默错误。
		if defaultOutboundTag == "" {
			return nil, false, common.NewError("模板里没有可引用的默认出站，直连规则无法生成, rule id:",
				rule.Id)
		}
		outboundTag = defaultOutboundTag
	default:
		return nil, false, common.NewError("未知的动作:", rule.Action)
	}

	// 顺序固定：domain 在前、ip 在后。这是「生成逐字节确定」的一部分。
	generated := make([]map[string]any, 0, 2)
	emit := func(conditionKey string, values []string) {
		g := map[string]any{
			"type":        "field",
			conditionKey:  values,
			"outboundTag": outboundTag,
		}
		// 空数组绝不写进配置（见函数注释）。inboundTags 为空表示这是一条
		// 全局规则，此时正确的做法是压根不输出 inboundTag 这个键。
		if len(inboundTags) > 0 {
			g["inboundTag"] = inboundTags
		}
		generated = append(generated, g)
	}
	if len(domains) > 0 {
		emit("domain", domains)
	}
	if len(cidrs) > 0 {
		emit("ip", cidrs)
	}
	return generated, isBlock, nil
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
