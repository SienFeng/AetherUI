package service

import (
	"fmt"
	"strings"
	"time"

	"a-ui/config"
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
