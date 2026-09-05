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
	// InboundRefs 是指针而不是值类型切片：显式的空数组 [] 表示「对所有入站
	// 生效」，是用户明确表达的语义；而 JSON 的 null 与**键缺失**在导入端
	// 必须整条拒绝——手工改过、别的工具生成的、传输被截断的文件都可能命中。
	// 值类型切片做不到这一点：encoding/json 把 null、缺失键、显式 []
	// 全部 unmarshal 成 len()==0 的 nil 切片，三者在 Go 侧完全无法区分，
	// 会让「拒绝」误判成「显式全局规则」，把规则静默放大到全体。
	// 导出侧 toPortableRule 永远返回非 nil 指针；导入侧见到 nil 指针
	// （对应 null 或字段缺失）必须整条拒绝，见 importRules。
	InboundRefs *[]PortableInboundRef `json:"inboundRefs"`
	Action      string                `json:"action"`
	Priority    int                   `json:"priority"`
	Enable      bool                  `json:"enable"`
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
	// 域名组重名检查覆盖 domainGroups 与 rules 两个 scope：前者导出的就是域名组
	// 本身，后者虽然不导出域名组、但规则里带着 domainGroupRef，重名同样会让导入端
	// 无法确定指向哪一个。scope=outbounds 与域名组完全无关，不该被库里两个不相干的
	// 同名组挡住——那会把管理员指向一个与他正在做的事毫无关系的地方。
	if scopeIncludes(scope, ExportScopeDomainGroups) || scopeIncludes(scope, ExportScopeRules) {
		if err := checkDuplicateGroupRemarks(groups); err != nil {
			return nil, err
		}
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
		InboundRefs:    &refs,
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
		// 空备注不参与 remark 匹配。DBInbound.remark 的默认值就是空串、表单也
		// 不强制填，所以「备注为空的入站」是本项目的合法状态且相当常见；拿空串
		// 去 byRemark 里查，会命中本机某个同样没起名的入站，remark 优先级高于
		// port，端口线索会被完全绕过——结果是一条 enable=true 的规则绑在一个与
		// 源机器毫无关系的用户身上，且因为「命中了」而连禁用兜底都不会触发。
		if ref.Remark != "" {
			if matched := byRemark[ref.Remark]; len(matched) == 1 {
				ids = append(ids, matched[0].Id)
				continue
			}
		}
		if in, ok := byPort[ref.Port]; ok {
			ids = append(ids, in.Id)
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (端口 %d)", ref.Remark, ref.Port))
	}
	return ids, missing
}

// hasDuplicateIds 判断两个不同的 ref 是否被两级匹配撞到了同一个本机入站
// （remark 换机器后对不上、退到 port 又刚好命中了另一条 ref 已经命中过的
// 那个）。EncodeInboundIds 会静默把重复 id 去重，覆盖范围因此缩小——
// 这是消费端能零成本察觉的塌缩，调用方必须报告并禁用，不能让一条本该
// 覆盖多人的规则悄悄少覆盖一个人。
func hasDuplicateIds(ids []int) bool {
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return true
		}
		seen[id] = true
	}
	return false
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

// fail 记一条导致某个分类 Failed++ 的消息：既写进报告（前端展示导入结果
// 唯一的反馈通道），也记 logger.Warning。管理员关掉 modal 之后不该完全没
// 有留痕——这与 buildRules 把跳过原因记进 logger.Warning 的既有做法一致，
// 「不让防线对用户隐形」不能只对生成期成立。
func (r *ImportReport) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.Messages = append(r.Messages, msg)
	logger.Warning("导入分流配置：", msg)
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
		report.DomainGroups.Failed += len(items)
		report.fail("读取本机域名组失败：%v", err)
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
			report.fail("有一个域名组的备注为空，已跳过")
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
				report.fail("域名组「%s」的域名格式有误：%v", item.Remark, err)
				continue
			}
			if err := ValidateDomains(list); err != nil {
				report.DomainGroups.Failed++
				report.fail("域名组「%s」的域名未通过校验：%v", item.Remark, err)
				continue
			}
			encoded, err = EncodeDomains(list)
			if err != nil {
				report.DomainGroups.Failed++
				report.fail("域名组「%s」编码失败：%v", item.Remark, err)
				continue
			}
		}
		if item.SubscribeUrl != "" {
			if err := ValidateSubscribeURL(item.SubscribeUrl); err != nil {
				report.DomainGroups.Failed++
				report.fail("域名组「%s」的订阅地址非法：%v", item.Remark, err)
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
			report.fail("域名组「%s」写库失败：%v", item.Remark, err)
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
		report.Outbounds.Failed += len(items)
		report.fail("读取本机出站节点失败：%v", err)
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
			report.fail("出站节点「%s」的 tag 为空或过长，已跳过", item.Remark)
			continue
		}
		// 保留 tag 不在 outbound_nodes 表里，数据库唯一约束看不见它们。
		// 撞名会让 xray 报 existing tag found 并拒绝启动整份配置——全员断网，
		// 而面板首页仍显示 running。判定统一走 model.IsReservedTag。
		if model.IsReservedTag(item.Tag) {
			report.Outbounds.Failed++
			report.fail("出站节点「%s」的 tag %s 是系统保留 tag，拒绝导入", item.Remark, item.Tag)
			continue
		}
		// 手工新增路径的 allocTag 恒定产出 a-ui-<...>，结构上不可能与模板
		// 撞名——「所有生成的 tag 统一带 a-ui- 前缀，与手工模板隔离」这条
		// 不变量就是这么成立的。导入保留原 tag（这是对的，规则靠 tag 对上
		// 引用），因此这里必须自己把这条隔离补回来：web/service/config.json
		// 模板里有一个 tag 为 blocked 的出站，一个同 tag 的导入节点会让
		// 生成配置出现重复 tag，与保留 tag 撞名同一形状同一后果——xray 拒绝
		// 启动整份配置、全员断网、面板首页仍显示 running，只是这里没有
		// IsReservedTag 那种 fail-close 的专门防线，只能靠 ValidateOutbound
		// 的 fail-open 兜底，所以必须在这里补一道 fail-close 的前缀检查。
		if !strings.HasPrefix(item.Tag, model.OutboundTagPrefix+"-") {
			report.Outbounds.Failed++
			report.fail("出站节点「%s」的 tag %s 不是 a-ui- 前缀，可能与模板出站撞名，拒绝导入", item.Remark, item.Tag)
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
			report.fail("出站节点 %s 的配置不是合法 JSON：%v", item.Tag, err)
			continue
		}
		// "null" 能通过 Unmarshal 却留下一个 nil map，下一行赋值直接 panic。
		if ob == nil {
			report.Outbounds.Failed++
			report.fail("出站节点 %s 的配置为 null", item.Tag)
			continue
		}
		ob["tag"] = item.Tag
		// 与新增/编辑路径同样过真实 xray 校验：一个坏配置会让整份
		// bin/config.json 加载失败、全员断网。fail open 策略照旧——
		// xray 二进制缺失或超时时 ValidateOutbound 会放行并记日志。
		if err := ValidateOutbound(ob); err != nil {
			report.Outbounds.Failed++
			report.fail("出站节点 %s 未通过 xray 校验：%v", item.Tag, err)
			continue
		}
		encoded, err := json.Marshal(ob)
		if err != nil {
			report.Outbounds.Failed++
			report.fail("出站节点 %s 编码失败：%v", item.Tag, err)
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
			report.fail("出站节点 %s 写库失败：%v", item.Tag, err)
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
		report.Rules.Failed += len(items)
		report.fail("读取本机域名组失败：%v", err)
		return false
	}
	// DomainGroup.Remark 没有唯一约束（controller 新增域名组也不查重），
	// 本机存在两个同名组是完全可达的状态。导出侧对此是硬拒绝
	// （checkDuplicateGroupRemarks），导入侧面对同一个歧义绝不能猜一个
	// （比如取 id 最小/最大的那个）——猜错会产生一条指向错误域名组的规则，
	// 而规则表会渲染得完全正常、配置也会正常生成，只是流量走错了节点，
	// 没有任何一层防线会发现。
	groupByRemark := make(map[string]*model.DomainGroup, len(groups))
	ambiguousGroupRemark := make(map[string]bool, len(groups))
	for _, g := range groups {
		if _, exists := groupByRemark[g.Remark]; exists {
			ambiguousGroupRemark[g.Remark] = true
			continue
		}
		groupByRemark[g.Remark] = g
	}
	nodes, err := s.outboundService.GetAll()
	if err != nil {
		report.Rules.Failed += len(items)
		report.fail("读取本机出站节点失败：%v", err)
		return false
	}
	nodeByTag := make(map[string]*model.OutboundNode, len(nodes))
	for _, n := range nodes {
		nodeByTag[n.Tag] = n
	}
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		report.Rules.Failed += len(items)
		report.fail("读取本机入站失败：%v", err)
		return false
	}

	changed := false
	for _, item := range items {
		label := item.Remark
		if label == "" {
			label = "(无备注)"
		}

		// nil 指针对应 JSON 的 null 或字段缺失，与显式的空数组 []（对所有
		// 入站生效，用户明确表达的语义）在 Go 侧本来无法区分——这正是把
		// InboundRefs 从值类型切片改成指针的原因。手工改过、别的工具生成
		// 的、传输被截断的文件都可能命中这条，必须整条拒绝，绝不能当成
		// 显式全局规则放行，否则会静默放大到全体入站。
		if item.InboundRefs == nil {
			report.Rules.Failed++
			report.fail("规则「%s」缺少 inboundRefs 字段，无法区分「对所有入站生效」与「字段缺失」，整条跳过", label)
			continue
		}
		refs := *item.InboundRefs

		// 空字符串不参与域名组匹配，理由与 resolveInboundRefs 里空 remark
		// 不参与入站匹配完全同构：DomainGroupRef 为空时本该是「引用的域名组
		// 缺失/损坏」，但 groupByRemark[""] 会在本机恰好存在一个备注为空的
		// 域名组时静默命中它，产生一条指向错误域名组的规则——与 §3.2 判定
		// 的域名组重名歧义同一形状，必须在查表之前拒绝，不能让空串冒充业务键。
		if item.DomainGroupRef == "" {
			report.Rules.Failed++
			report.fail("规则「%s」没有指定域名组（domainGroupRef 为空），整条跳过", label)
			continue
		}

		if ambiguousGroupRemark[item.DomainGroupRef] {
			report.Rules.Failed++
			report.fail("规则「%s」引用的域名组「%s」在本机有多个同名组，无法确定指向哪一个，整条跳过（请先在域名组页面改名）",
				label, item.DomainGroupRef)
			continue
		}
		g, ok := groupByRemark[item.DomainGroupRef]
		if !ok {
			report.Rules.Failed++
			report.fail("规则「%s」引用的域名组「%s」不存在，整条跳过", label, item.DomainGroupRef)
			continue
		}

		outboundId := 0
		if item.Action == model.ActionProxy {
			n, ok := nodeByTag[item.OutboundRef]
			if !ok {
				report.Rules.Failed++
				report.fail("规则「%s」引用的出站节点 %s 不存在，整条跳过", label, item.OutboundRef)
				continue
			}
			outboundId = n.Id
			// 出站存在但被禁用：规则本身照常导入（其余部分都是好的），
			// 但 buildRule 在生成期会因出站被禁用整条跳过它——规则列表
			// 上却看起来是一条正常启用的规则，管理员察觉不到这个落差。
			if !n.Enable {
				report.say("规则「%s」引用的出站节点 %s 当前处于禁用状态，该规则不会写进配置，请先启用该节点",
					label, item.OutboundRef)
			}
		}

		ids, missing := resolveInboundRefs(refs, inbounds)
		// 一个都没找到是唯一必须整条丢弃的情形：refs 非空却没有任何一项
		// 解析出 id，若照常导入会在编码时被去重成 []，等于把这条规则放大
		// 成「对所有入站生效」——哪怕导入成禁用状态也不行，管理员一旦手滑
		// 启用就会全员中招。这个判断必须放在下面「部分命中」分支之前：
		// 否则报告会先说「已导入但保持禁用」、紧接着又说「整条跳过」，
		// 两句自相矛盾，管理员会去规则列表里找一条根本不存在的禁用规则。
		if len(refs) > 0 && len(ids) == 0 {
			report.Rules.Failed++
			report.fail("规则「%s」的入站在本机一个都没找到，整条跳过（若照常导入，它会变成对所有用户生效）", label)
			continue
		}

		enable := item.Enable
		if len(missing) > 0 {
			// 绝不把认不出的入站剔掉后当作完整覆盖集——剔到空就是上面
			// 已经处理的「对所有入站生效」。部分命中时导入成禁用状态，
			// 把缺失的点名报告，管理员打开编辑弹窗勾一下就好。整条丢弃
			// 也不对：规则的其余部分（域名组、出站、优先级、动作）都是好的。
			enable = false
			report.say("规则「%s」的入站 %s 在本机未找到，已导入但保持禁用，请手工指定入站后启用",
				label, strings.Join(missing, "、"))
		}
		// 两级匹配可能把两个不同的 ref 撞到同一个本机入站（一个 ref 按
		// remark 命中了它，另一个 ref 的 remark 对不上、退到 port 又刚好
		// 命中了同一个入站）。EncodeInboundIdsStrict 会静默把重复 id
		// 去重，覆盖范围因此缩小——这是消费端能零成本察觉的塌缩，必须
		// 报告并禁用，不能让一条本该覆盖多人的规则悄悄少覆盖一个人。
		if hasDuplicateIds(ids) {
			enable = false
			report.say("规则「%s」有多个入站引用指向了本机同一个入站，覆盖范围已缩小，已导入但保持禁用，请手工确认", label)
		}

		// 用 Strict 版本：写入路径的既有约定（EncodeInboundIds 的函数注释
		// 明写「写入路径一律用 EncodeInboundIdsStrict」）。但 Strict 看的
		// 是解析后的 ids，不是原始的 item.InboundRefs——全部认不出时 ids
		// 本身就是空切片，Strict 在这条路径上和非 Strict 一样安静地返回
		// "[]"，不会报错。上面那道「一个都没找到」检查必须自己写，不能
		// 指望 Strict 替我们拦住。
		encoded, err := EncodeInboundIdsStrict(ids)
		if err != nil {
			report.Rules.Failed++
			report.fail("规则「%s」的入站编码失败：%v", label, err)
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
			// checkConflict 拒绝的规则语义上就是「Skipped 定义」里的
			// 「本机已存在」——已经有一条规则覆盖了同样的范围。用错误文本
			// 匹配而不是哨兵错误类型，因为 checkConflict 没有导出一个；
			// 落进 Failed 会让一次完全正常的重跑显示成「规则：失败 N」，
			// 与「导入是幂等的」这条设计前提自相矛盾。
			if strings.Contains(err.Error(), "冲突") {
				report.Rules.Skipped++
				// checkConflict 的错误里带着具体冲突规则名与相交入站
				// （routing_rule.go 的「与分流规则「%s」冲突：%s在域名组
				// 「%s」下已被它覆盖」），原样附上，管理员才知道是跟哪条撞的。
				report.say("规则「%s」已存在同覆盖范围的规则，跳过：%v", label, err)
			} else {
				report.Rules.Failed++
				report.fail("规则「%s」导入失败：%v", label, err)
			}
			continue
		}
		report.Rules.Created++
		changed = true
	}
	return changed
}
