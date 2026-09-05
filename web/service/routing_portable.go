package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"a-ui/config"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
)

// 导出文件的格式标识与版本。两者都是跨版本契约，改动前必须想清楚
// 旧文件怎么办——导入端认不出 Kind 或 Version 会整体拒绝。
const (
	ExportKind    = "a-ui-routing-export"
	ExportVersion = 1
)

// 导出范围。分项导出不隐式扩大：scope=rules 就只导规则，不带上它引用的
// 域名组和出站节点——隐式扩大会让 all 和 rules 的区别消失。
const (
	ExportScopeAll          = "all"
	ExportScopeDomainGroups = "domainGroups"
	ExportScopeOutbounds    = "outbounds"
	ExportScopeRules        = "rules"
)

// PortableDomainGroup 不含 SubscribedDomains 与 LastUpdatedAt/LastError/
// LastSkipped：前者单个组可达十几万条，后三者是本机这一次拉取的状态，
// 搬到另一台机器毫无意义，还会让新机器显示一个假的「刚刚更新」。
type PortableDomainGroup struct {
	Remark       string   `json:"remark"`
	Domains      []string `json:"domains"`
	SubscribeUrl string   `json:"subscribeUrl"`
}

// PortableOutbound 保留原 tag：规则靠 tag 对上引用，导入端重新分配会让
// 所有引用它的规则失效。
type PortableOutbound struct {
	Tag      string `json:"tag"`
	Remark   string `json:"remark"`
	Protocol string `json:"protocol"`
	Config   string `json:"config"`
	Enable   bool   `json:"enable"`
}

// PortableInboundRef 是入站的跨机器线索。
//
// 入站的三个候选键都不可靠：Id 跨机器无意义且 SQLite 会复用；Tag 由端口
// 算出（UpdateInbound 里 Tag = "inbound-<port>"），存 tag 等于存端口；
// Remark 可重复。所以两个都给，让导入端自己两级匹配并在判断不了时诚实
// 地说出来。
type PortableInboundRef struct {
	Remark string `json:"remark"`
	Port   int    `json:"port"`
}

type PortableRule struct {
	Remark         string `json:"remark"`
	DomainGroupRef string `json:"domainGroupRef"`
	// OutboundRef 在 action=block 时为空。
	OutboundRef string `json:"outboundRef"`
	// InboundRefs 为空切片表示「对所有入站生效」，是用户显式表达的语义。
	// 必须序列化成 [] 而不是 null——导入端要能把它与「字段缺失」区分开。
	InboundRefs []PortableInboundRef `json:"inboundRefs"`
	Action      string               `json:"action"`
	Priority    int                  `json:"priority"`
	Enable      bool                 `json:"enable"`
}

type ExportFile struct {
	Kind         string                `json:"kind"`
	Version      int                   `json:"version"`
	ExportedAt   int64                 `json:"exportedAt"`
	ExportedBy   string                `json:"exportedBy"`
	Scope        []string              `json:"scope"`
	DomainGroups []PortableDomainGroup `json:"domainGroups"`
	Outbounds    []PortableOutbound    `json:"outbounds"`
	Rules        []PortableRule        `json:"rules"`
}

type RoutingPortableService struct {
	domainGroupService DomainGroupService
	outboundService    OutboundNodeService
	ruleService        RoutingRuleService
	inboundService     InboundService
}

func scopeIncludes(scope, want string) bool {
	return scope == ExportScopeAll || scope == want
}

func validExportScope(scope string) bool {
	switch scope {
	case ExportScopeAll, ExportScopeDomainGroups, ExportScopeOutbounds, ExportScopeRules:
		return true
	}
	return false
}

func (s *RoutingPortableService) Export(scope string) (*ExportFile, error) {
	if !validExportScope(scope) {
		return nil, common.NewErrorf("未知的导出范围：%q", scope)
	}

	groups, err := s.domainGroupService.GetAll()
	if err != nil {
		return nil, err
	}
	// 重名检查不看 scope：规则文件与域名组文件将来会被配套使用，
	// 只在导出规则时才检查会留下一个可被绕开的洞。
	if err := checkDuplicateGroupRemarks(groups); err != nil {
		return nil, err
	}

	f := &ExportFile{
		Kind:         ExportKind,
		Version:      ExportVersion,
		ExportedAt:   time.Now().UnixMilli(),
		ExportedBy:   "a-ui " + config.GetVersion(),
		Scope:        []string{},
		DomainGroups: []PortableDomainGroup{},
		Outbounds:    []PortableOutbound{},
		Rules:        []PortableRule{},
	}
	if scope == ExportScopeAll {
		f.Scope = []string{ExportScopeDomainGroups, ExportScopeOutbounds, ExportScopeRules}
	} else {
		f.Scope = []string{scope}
	}

	if scopeIncludes(scope, ExportScopeDomainGroups) {
		for _, g := range groups {
			// 解码失败当作空列表：这个组本身已经损坏（buildRule 会丢弃引用
			// 它的规则），但组的备注和订阅地址仍是有用的，导出它比整个
			// 导出失败对管理员更有帮助。
			manual, err := DecodeDomains(g.Domains)
			if err != nil {
				manual = nil
			}
			if manual == nil {
				manual = []string{}
			}
			f.DomainGroups = append(f.DomainGroups, PortableDomainGroup{
				Remark:       g.Remark,
				Domains:      manual,
				SubscribeUrl: g.SubscribeUrl,
			})
		}
	}

	if scopeIncludes(scope, ExportScopeOutbounds) {
		nodes, err := s.outboundService.GetAll()
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			f.Outbounds = append(f.Outbounds, PortableOutbound{
				Tag: n.Tag, Remark: n.Remark, Protocol: n.Protocol,
				Config: n.Config, Enable: n.Enable,
			})
		}
	}

	if scopeIncludes(scope, ExportScopeRules) {
		rules, err := s.ruleService.GetAll()
		if err != nil {
			return nil, err
		}
		groupById := make(map[int]*model.DomainGroup, len(groups))
		for _, g := range groups {
			groupById[g.Id] = g
		}
		nodes, err := s.outboundService.GetAll()
		if err != nil {
			return nil, err
		}
		nodeById := make(map[int]*model.OutboundNode, len(nodes))
		for _, n := range nodes {
			nodeById[n.Id] = n
		}
		inbounds, err := s.inboundService.GetAllInbounds()
		if err != nil {
			return nil, err
		}
		inboundById := make(map[int]*model.Inbound, len(inbounds))
		for _, in := range inbounds {
			inboundById[in.Id] = in
		}

		for _, r := range rules {
			pr, skip := s.toPortableRule(r, groupById, nodeById, inboundById)
			if skip != nil {
				// 引用已经悬空的规则本来就不会写进配置，导出它只会在对面
				// 产生同样一条不生效的规则，还占着 checkConflict 的位置。
				logger.Warning("导出跳过规则「", ruleLabel(r), "」：", skip)
				continue
			}
			f.Rules = append(f.Rules, pr)
		}
	}

	return f, nil
}

// checkDuplicateGroupRemarks 是导出侧唯一的硬拒绝。
func checkDuplicateGroupRemarks(groups []*model.DomainGroup) error {
	seen := make(map[string]bool, len(groups))
	dups := make([]string, 0)
	for _, g := range groups {
		if seen[g.Remark] {
			dups = append(dups, g.Remark)
			continue
		}
		seen[g.Remark] = true
	}
	if len(dups) == 0 {
		return nil
	}
	return common.NewErrorf(
		"域名组备注重复：%s。导出文件用备注引用域名组，重名会让导入端无法确定规则指向哪一个，请先改名再导出。",
		strings.Join(dups, "、"))
}

func (s *RoutingPortableService) toPortableRule(
	r *model.RoutingRule,
	groupById map[int]*model.DomainGroup,
	nodeById map[int]*model.OutboundNode,
	inboundById map[int]*model.Inbound,
) (PortableRule, error) {
	g, ok := groupById[r.DomainGroupId]
	if !ok {
		return PortableRule{}, common.NewErrorf("域名组 #%d 不存在", r.DomainGroupId)
	}
	outboundRef := ""
	if r.Action == model.ActionProxy {
		n, ok := nodeById[r.OutboundId]
		if !ok {
			return PortableRule{}, common.NewErrorf("出站节点 #%d 不存在", r.OutboundId)
		}
		outboundRef = n.Tag
	}
	ids, err := DecodeInboundIds(r.InboundIds)
	if err != nil {
		return PortableRule{}, common.NewError("入站数据损坏:", err)
	}
	// 空切片而不是 nil：nil 会被 encoding/json 序列化成 null，导入端就无法
	// 把「对所有入站生效」和「字段缺失」区分开——而这两者的后果天差地别。
	refs := make([]PortableInboundRef, 0, len(ids))
	for _, id := range ids {
		in, ok := inboundById[id]
		if !ok {
			// 悬空引用，本机上这条规则的这一部分已经不生效了。整条跳过，
			// 不能只剔掉这一个——剔到最后变成空数组就是「对所有人生效」。
			return PortableRule{}, common.NewErrorf("入站 #%d 不存在", id)
		}
		refs = append(refs, PortableInboundRef{Remark: in.Remark, Port: in.Port})
	}
	return PortableRule{
		Remark:         r.Remark,
		DomainGroupRef: g.Remark,
		OutboundRef:    outboundRef,
		InboundRefs:    refs,
		Action:         r.Action,
		Priority:       r.Priority,
		Enable:         r.Enable,
	}, nil
}

// resolveInboundRefs 把导出文件里的入站线索映射到本机的入站 id。
//
// 两级匹配：先按 remark 精确匹配（**恰好命中 1 条才算**，重名视为无法
// 区分），失败再按 port 匹配（port 有 unique 约束，命中即唯一）。
// 备注优先是因为换机器后端口很可能改了，而备注是管理员认得的东西。
//
// 返回的 missing 非空时，调用方**必须**把整条规则导入成禁用状态，
// 绝不能拿部分命中的 ids 当作完整覆盖集：剔掉认不出的那几个之后，
// 一条本该只覆盖某个人的规则会被缩小或（剔到空时）放大成覆盖全体，
// 而 xray 对空 inboundTag 返回 Configuration OK，不会有任何报错。
func resolveInboundRefs(refs []PortableInboundRef, inbounds []*model.Inbound) ([]int, []string) {
	byRemark := make(map[string][]*model.Inbound, len(inbounds))
	byPort := make(map[int]*model.Inbound, len(inbounds))
	for _, in := range inbounds {
		byRemark[in.Remark] = append(byRemark[in.Remark], in)
		byPort[in.Port] = in
	}

	ids := make([]int, 0, len(refs))
	missing := make([]string, 0)
	for _, ref := range refs {
		if matched := byRemark[ref.Remark]; len(matched) == 1 {
			ids = append(ids, matched[0].Id)
			continue
		}
		if in, ok := byPort[ref.Port]; ok {
			ids = append(ids, in.Id)
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (端口 %d)", ref.Remark, ref.Port))
	}
	return ids, missing
}

type ImportCounts struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"` // 本机已存在
	Failed  int `json:"failed"`
}

type ImportReport struct {
	DomainGroups ImportCounts `json:"domainGroups"`
	Outbounds    ImportCounts `json:"outbounds"`
	Rules        ImportCounts `json:"rules"`
	// Messages 是人话，逐条说明每一个非 Created 的结果。前端用 modal 展示，
	// 不能用 $message——可能有几十行。
	Messages []string `json:"messages"`
}

func (r *ImportReport) say(format string, a ...any) {
	r.Messages = append(r.Messages, fmt.Sprintf(format, a...))
}

// Import 逐条处理、逐条报告，**不用事务**。
//
// 出站节点落库前要 exec 真实 xray 校验，一次几百毫秒且策略是 fail open。
// 包进事务会在校验期间长时间持有 SQLite 那把写锁，把整个面板（含每 10 秒
// 的流量统计、每秒的并发判定）一起卡住。这与 routing_validate.go 里
// 「落库之前校验，因此不需要事务回滚」的取向一致。
//
// 代价是导入可能「成功一半」。可接受：每条的成败都在报告里，而且导入是
// 幂等的（冲突一律跳过），重跑一次就补齐了。
func (s *RoutingPortableService) Import(raw string) (*ImportReport, error) {
	var f ExportFile
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, common.NewError("导入文件不是合法的 JSON:", err)
	}
	if f.Kind != ExportKind {
		return nil, common.NewErrorf("不是 AetherUI 的分流配置文件（kind=%q）", f.Kind)
	}
	if f.Version != ExportVersion {
		return nil, common.NewErrorf(
			"导入文件版本 %d 与当前面板支持的版本 %d 不一致，请用同版本的面板导出",
			f.Version, ExportVersion)
	}

	report := &ImportReport{Messages: []string{}}
	changed := false

	if s.importDomainGroups(f.DomainGroups, report) {
		changed = true
	}
	if s.importOutbounds(f.Outbounds, report) {
		changed = true
	}
	if s.importRules(f.Rules, report) {
		changed = true
	}

	if changed {
		// 复用既有链路：置原子标志 → InboundController 的 10 秒 cron 消费
		// → RestartXray(false) → Config.Equals 察觉 RouterConfig/OutboundConfigs
		// 变了 → 先试热应用，不行才整进程重启。管理员不需要额外操作。
		(&XrayService{}).SetToNeedRestart()
	}
	return report, nil
}

func (s *RoutingPortableService) importDomainGroups(items []PortableDomainGroup, report *ImportReport) bool {
	if len(items) == 0 {
		return false
	}
	existing, err := s.domainGroupService.GetAll()
	if err != nil {
		report.say("读取本机域名组失败：%v", err)
		report.DomainGroups.Failed += len(items)
		return false
	}
	byRemark := make(map[string]bool, len(existing))
	for _, g := range existing {
		byRemark[g.Remark] = true
	}

	changed := false
	subscribedCount := 0
	for _, item := range items {
		if item.Remark == "" {
			report.DomainGroups.Failed++
			report.say("有一个域名组的备注为空，已跳过")
			continue
		}
		if byRemark[item.Remark] {
			report.DomainGroups.Skipped++
			report.say("域名组「%s」已存在，跳过", item.Remark)
			continue
		}
		// 走与表单同一条校验路径。导入文件是不可信输入，与管理员在表单里
		// 输入的东西同级。
		encoded := "[]"
		if len(item.Domains) > 0 {
			list, err := ParseDomains(strings.Join(item.Domains, "\n"))
			if err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」的域名格式有误：%v", item.Remark, err)
				continue
			}
			if err := ValidateDomains(list); err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」的域名未通过校验：%v", item.Remark, err)
				continue
			}
			encoded, err = EncodeDomains(list)
			if err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」编码失败：%v", item.Remark, err)
				continue
			}
		}
		if item.SubscribeUrl != "" {
			if err := ValidateSubscribeURL(item.SubscribeUrl); err != nil {
				report.DomainGroups.Failed++
				report.say("域名组「%s」的订阅地址非法：%v", item.Remark, err)
				continue
			}
		}
		// LastUpdatedAt 留 0：ShouldUpdateNow 对 0 直接返回 true，
		// SubscriptionJob（每 10 分钟）会自动补上首次拉取。这里不同步拉，
		// 一个慢地址能把这个 HTTP 请求挂满 30 秒。
		g := &model.DomainGroup{
			Remark: item.Remark, Domains: encoded, SubscribeUrl: item.SubscribeUrl,
		}
		if err := s.domainGroupService.Add(g); err != nil {
			report.DomainGroups.Failed++
			report.say("域名组「%s」写库失败：%v", item.Remark, err)
			continue
		}
		byRemark[item.Remark] = true
		report.DomainGroups.Created++
		changed = true
		if item.SubscribeUrl != "" {
			subscribedCount++
		}
	}
	if subscribedCount > 0 {
		report.say("%d 个域名组已加入订阅，最迟 10 分钟内完成首次拉取；在此之前，仅依赖订阅内容的规则不会写进配置",
			subscribedCount)
	}
	return changed
}

func (s *RoutingPortableService) importOutbounds(items []PortableOutbound, report *ImportReport) bool {
	if len(items) == 0 {
		return false
	}
	existing, err := s.outboundService.GetAll()
	if err != nil {
		report.say("读取本机出站节点失败：%v", err)
		report.Outbounds.Failed += len(items)
		return false
	}
	byTag := make(map[string]bool, len(existing))
	for _, n := range existing {
		byTag[n.Tag] = true
	}

	changed := false
	for _, item := range items {
		if item.Tag == "" || len(item.Tag) > 128 {
			report.Outbounds.Failed++
			report.say("出站节点「%s」的 tag 为空或过长，已跳过", item.Remark)
			continue
		}
		// 保留 tag 不在 outbound_nodes 表里，数据库唯一约束看不见它们。
		// 撞名会让 xray 报 existing tag found 并拒绝启动整份配置——全员断网，
		// 而面板首页仍显示 running。判定统一走 model.IsReservedTag。
		if model.IsReservedTag(item.Tag) {
			report.Outbounds.Failed++
			report.say("出站节点「%s」的 tag %s 是系统保留 tag，拒绝导入", item.Remark, item.Tag)
			continue
		}
		if byTag[item.Tag] {
			report.Outbounds.Skipped++
			report.say("出站节点 %s 已存在，跳过", item.Tag)
			continue
		}

		var ob map[string]any
		if err := json.Unmarshal([]byte(item.Config), &ob); err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 的配置不是合法 JSON：%v", item.Tag, err)
			continue
		}
		// "null" 能通过 Unmarshal 却留下一个 nil map，下一行赋值直接 panic。
		if ob == nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 的配置为 null", item.Tag)
			continue
		}
		ob["tag"] = item.Tag
		// 与新增/编辑路径同样过真实 xray 校验：一个坏配置会让整份
		// bin/config.json 加载失败、全员断网。fail open 策略照旧——
		// xray 二进制缺失或超时时 ValidateOutbound 会放行并记日志。
		if err := ValidateOutbound(ob); err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 未通过 xray 校验：%v", item.Tag, err)
			continue
		}
		encoded, err := json.Marshal(ob)
		if err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 编码失败：%v", item.Tag, err)
			continue
		}
		protocol := item.Protocol
		if p, ok := ob["protocol"].(string); ok && p != "" {
			protocol = p
		}
		node := &model.OutboundNode{
			Tag: item.Tag, Remark: item.Remark, Protocol: protocol,
			Config: string(encoded), Enable: item.Enable,
		}
		if err := database.GetDB().Save(node).Error; err != nil {
			report.Outbounds.Failed++
			report.say("出站节点 %s 写库失败：%v", item.Tag, err)
			continue
		}
		byTag[item.Tag] = true
		report.Outbounds.Created++
		changed = true
	}
	return changed
}

func (s *RoutingPortableService) importRules(items []PortableRule, report *ImportReport) bool {
	if len(items) == 0 {
		return false
	}
	groups, err := s.domainGroupService.GetAll()
	if err != nil {
		report.say("读取本机域名组失败：%v", err)
		report.Rules.Failed += len(items)
		return false
	}
	groupByRemark := make(map[string]*model.DomainGroup, len(groups))
	for _, g := range groups {
		groupByRemark[g.Remark] = g
	}
	nodes, err := s.outboundService.GetAll()
	if err != nil {
		report.say("读取本机出站节点失败：%v", err)
		report.Rules.Failed += len(items)
		return false
	}
	nodeByTag := make(map[string]*model.OutboundNode, len(nodes))
	for _, n := range nodes {
		nodeByTag[n.Tag] = n
	}
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		report.say("读取本机入站失败：%v", err)
		report.Rules.Failed += len(items)
		return false
	}

	changed := false
	for _, item := range items {
		label := item.Remark
		if label == "" {
			label = "(无备注)"
		}
		g, ok := groupByRemark[item.DomainGroupRef]
		if !ok {
			report.Rules.Failed++
			report.say("规则「%s」引用的域名组「%s」不存在，整条跳过", label, item.DomainGroupRef)
			continue
		}
		outboundId := 0
		if item.Action == model.ActionProxy {
			n, ok := nodeByTag[item.OutboundRef]
			if !ok {
				report.Rules.Failed++
				report.say("规则「%s」引用的出站节点 %s 不存在，整条跳过", label, item.OutboundRef)
				continue
			}
			outboundId = n.Id
		}

		ids, missing := resolveInboundRefs(item.InboundRefs, inbounds)
		enable := item.Enable
		if len(missing) > 0 {
			// 绝不把认不出的入站剔掉后当作完整覆盖集——剔到空就是
			// 「对所有入站生效」。导入成禁用状态，把缺失的点名报告，
			// 管理员打开编辑弹窗勾一下就好。整条丢弃也不对：规则的其余
			// 部分（域名组、出站、优先级、动作）都是好的。
			enable = false
			report.say("规则「%s」的入站 %s 在本机未找到，已导入但保持禁用，请手工指定入站后启用",
				label, strings.Join(missing, "、"))
		}
		encoded, err := EncodeInboundIds(ids)
		if err != nil {
			report.Rules.Failed++
			report.say("规则「%s」的入站编码失败：%v", label, err)
			continue
		}
		// 部分命中却编码成了 []，等于把规则放大到全体。这种情况只可能在
		// 已命中集合为空时出现，此时必须整条丢弃而不是导入一条覆盖全员的
		// 规则——哪怕它是禁用的，管理员一旦启用就会全员中招。
		if encoded == "[]" && len(item.InboundRefs) > 0 {
			report.Rules.Failed++
			report.say("规则「%s」的入站在本机一个都没找到，整条跳过（若照常导入，它会变成对所有用户生效）", label)
			continue
		}

		rule := &model.RoutingRule{
			Remark: item.Remark, InboundIds: encoded, DomainGroupId: g.Id,
			Action: item.Action, OutboundId: outboundId,
			Priority: item.Priority, Enable: enable,
		}
		// 走 Add 而不是直接写库：它自带 validate（域名组/出站存在、动作合法）
		// 与 checkConflict（同一域名组下入站不得重叠）。
		if err := s.ruleService.Add(rule); err != nil {
			report.Rules.Failed++
			report.say("规则「%s」导入失败：%v", label, err)
			continue
		}
		report.Rules.Created++
		changed = true
	}
	return changed
}
