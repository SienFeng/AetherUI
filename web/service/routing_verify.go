package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/geodata"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/xray"
)

// 路由验证的纯函数部分：样例选取、期望比对、结论合并。不碰数据库与核心，
// 便于用表驱动测试把「没有证据就不给绿色」这条约束钉住。

// 核心级状态（CoreState.State）。
const (
	coreStateOK         = "ok"
	coreStatePending    = "pending"
	coreStateNotRunning = "not_running"
	coreStateBusy       = "busy"
	coreStateError      = "error"
)

// 用户行的最终结论（UserVerify.Verdict）。
const (
	verdictVerified     = "verified"
	verdictMismatch     = "mismatch"
	verdictPending      = "pending"
	verdictUnverifiable = "unverifiable"
)

// RuleVerifyResult 是一条规则的验证结果，json 字段名与设计文档 §7 一致。
type RuleVerifyResult struct {
	RuleId    int              `json:"ruleId"`
	CheckedAt int64            `json:"checkedAt"`
	Expected  ExpectedOutbound `json:"expected"`
	Core      CoreState        `json:"core"`
	AccessLog AccessLogState   `json:"accessLog"`
	Truncated bool             `json:"truncated"`
	Users     []UserVerify     `json:"users"`
}

// ExpectedOutbound 是规则期望的出口。Action 为 proxy|block|direct。
type ExpectedOutbound struct {
	Action string `json:"action"`
	Tag    string `json:"tag"`
	Label  string `json:"label"`
}

// CoreState 是核心级前置状态：ok|pending|not_running|busy|error。
type CoreState struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

// AccessLogState 描述访问日志证据是否可用。
type AccessLogState struct {
	Enabled     bool `json:"enabled"`
	WindowHours int  `json:"windowHours"`
}

// UserVerify 是图上一个用户（入站）的验证结果。
type UserVerify struct {
	InboundId int            `json:"inboundId"`
	Name      string         `json:"name"`
	Verdict   string         `json:"verdict"`
	Reason    string         `json:"reason"`
	Samples   []SampleResult `json:"samples"`
	Log       *LogEvidence   `json:"log"`
}

// SampleResult 是对一个样例目标做 TestRoute 的结果。
type SampleResult struct {
	Target      string `json:"target"`
	ActualTag   string `json:"actualTag"`
	ActualLabel string `json:"actualLabel"`
	Match       bool   `json:"match"`
	HitByRule   bool   `json:"hitByRule"`
	Error       string `json:"error"`
}

// LogEvidence 是访问日志里属于本规则的真实连接统计。
type LogEvidence struct {
	Matched   int64      `json:"matched"`
	Rejected  int64      `json:"rejected"`
	Truncated bool       `json:"truncated"`
	Others    []LogOther `json:"others"`
}

// LogOther 是走了非期望出站的连接分组。
type LogOther struct {
	Route    string   `json:"route"`
	Label    string   `json:"label"`
	Count    int64    `json:"count"`
	LastAt   int64    `json:"lastAt"`
	Examples []string `json:"examples"`
}

// sampleTarget 是一个待 TestRoute 的目标：域名与 IP 二选一。
type sampleTarget struct {
	Domain  string
	IP      net.IP
	Display string
}

// sampleTargets 对一个域名组取样：先域名后 IP，总数不超过 perGroup。
// 只有 full:/domain: 能还原出确定的域名；keyword/regexp/dotless/geosite/ext 推不出
// 可靠样例，geoip:/ext:/带 ! 取反的 IP 条目同理，一律跳过而不是猜一个。
func sampleTargets(domains, cidrs []string, perGroup int) []sampleTarget {
	out := []sampleTarget{}
	for _, d := range domains {
		if len(out) >= perGroup {
			return out
		}
		var host string
		switch {
		case strings.HasPrefix(d, "full:"):
			host = strings.TrimPrefix(d, "full:")
		case strings.HasPrefix(d, "domain:"):
			host = strings.TrimPrefix(d, "domain:")
		}
		if host = strings.TrimSpace(host); host != "" {
			out = append(out, sampleTarget{Domain: host, Display: host})
		}
	}
	for _, c := range cidrs {
		if len(out) >= perGroup {
			break
		}
		if ip := firstUsableIP(strings.TrimSpace(c)); ip != nil {
			out = append(out, sampleTarget{IP: ip, Display: ip.String()})
		}
	}
	return out
}

// firstUsableIP 返回 IP 段内第一个可用地址：网络地址 +1，/32、/128 与单个 IP 取自身。
// 解析不了（取反、geoip:、ext: 等）返回 nil。
func firstUsableIP(entry string) net.IP {
	if entry == "" || strings.HasPrefix(entry, "!") {
		return nil
	}
	if ip := net.ParseIP(entry); ip != nil {
		return ip
	}
	_, ipNet, err := net.ParseCIDR(entry)
	if err != nil {
		return nil
	}
	ones, bits := ipNet.Mask.Size()
	first := make(net.IP, len(ipNet.IP))
	copy(first, ipNet.IP)
	if ones == bits {
		return first
	}
	for i := len(first) - 1; i >= 0; i-- {
		first[i]++
		if first[i] != 0 {
			break
		}
	}
	return first
}

// judgeSample 比对一次 TestRoute 的结果与期望。noMatch 表示核心返回 ErrNoClue
// （没有任何规则命中，走默认出站）。
//   - proxy/block：实际 tag 必须严格等于期望 tag。
//   - direct：实际出口是默认出站即算一致（默认 tag、计量 tag、未命中三种），
//     但后两种不是由本规则命中，hitByRule=false，调用方据此在明细里注明。
func judgeSample(exp ExpectedOutbound, actualTag string, noMatch bool, defaultTag string) (match, hitByRule bool) {
	hitByRule = !noMatch && !model.IsMeterTag(actualTag)
	if exp.Action == model.ActionDirect {
		return noMatch || model.IsMeterTag(actualTag) || actualTag == defaultTag, hitByRule
	}
	return !noMatch && actualTag == exp.Tag, hitByRule
}

// routeHead 取访问日志 Route 里 " -> " / " >> " 之前的第一段（经中间出站时 Route 是整串）。
func routeHead(route string) string {
	for _, sep := range []string{" -> ", " >> "} {
		if i := strings.Index(route, sep); i >= 0 {
			route = route[:i]
		}
	}
	return strings.TrimSpace(route)
}

// combineVerdict 把核心级状态、样例结果与访问日志合并成用户行的最终结论。
// 判定顺序：
//  1. 核心处于 pending：一律 pending。此时核心仍持有旧配置，样例与配置不符是必然的，
//     报「不符」是误报；明细仍保留在结果里。
//  2. 有任一样例不符或日志有其他出站：mismatch。
//  3. 核心不可用 / 没有样例 / 有样例出错：unverifiable。
//  4. 其余（样例全部符合）：verified。
//
// 访问日志只能把结论推向 mismatch 或给 verified 补充说明，永远不能单独产生 verified。
func combineVerdict(coreState string, samples []SampleResult, log *LogEvidence) (verdict, reason string) {
	if coreState == coreStatePending {
		return verdictPending, "配置已保存，正在等待下发到核心（最多约 10 秒）"
	}

	for _, s := range samples {
		if s.Error == "" && !s.Match {
			return verdictMismatch, fmt.Sprintf("核心实际出站与配置不符（样例 %s）", s.Target)
		}
	}
	if log != nil && len(log.Others) > 0 {
		return verdictMismatch, "访问日志里有连接走了其他出站"
	}

	switch coreState {
	case coreStateNotRunning:
		return verdictUnverifiable, "xray 未运行"
	case coreStateBusy:
		return verdictUnverifiable, "核心正在更新配置，请稍后重新验证"
	case coreStateOK:
	default:
		return verdictUnverifiable, "核心验证出错，无法验证"
	}

	if len(samples) == 0 {
		return verdictUnverifiable, "本规则的域名都是关键词/正则/geosite 写法，推不出可测试的域名"
	}
	for _, s := range samples {
		if s.Error != "" {
			// 有样例没验成就不能说「全部符合」，宁可灰也不给绿。
			return verdictUnverifiable, "部分样例验证失败：" + s.Error
		}
	}

	if log != nil && log.Matched > 0 {
		return verdictVerified, fmt.Sprintf("近 24h %d 次真实连接", log.Matched)
	}
	return verdictVerified, ""
}

// logOtherMaxExamples 是每个「其他出站」分组保留的示例连接数。
const logOtherMaxExamples = 5

// groupMatcher 用 xray 自己的匹配器判断一个访问日志目标是否属于某个域名组。
// 归属判定不手写：写法（domain:/full:/keyword:/regexp:/geosite:…）与 geoip 的语义
// 只有交给 xray 才会和核心实际路由一致。
type groupMatcher struct {
	domain *router.DomainMatcher
	ip     *router.IPMatcher
}

// newGroupMatcher 构造匹配器；某一侧条目为空时不构造那一侧（其匹配恒为否）。
func newGroupMatcher(domains, cidrs []string) (*groupMatcher, error) {
	xray.EnsureAssetLocation()
	m := &groupMatcher{}
	if len(domains) > 0 {
		// 默认类型 Domain_Substr 与 infra/conf 解析 routing 规则时一致（裸串按子串）。
		rules, err := geodata.ParseDomainRules(domains, geodata.Domain_Substr)
		if err != nil {
			return nil, fmt.Errorf("解析域名条件: %w", err)
		}
		if m.domain, err = router.NewDomainMatcher(rules); err != nil {
			return nil, fmt.Errorf("构造域名匹配器: %w", err)
		}
	}
	if len(cidrs) > 0 {
		rules, err := geodata.ParseIPRules(cidrs)
		if err != nil {
			return nil, fmt.Errorf("解析 IP 条件: %w", err)
		}
		if m.ip, err = router.NewIPMatcher(rules, router.MatcherAsType_Target); err != nil {
			return nil, fmt.Errorf("构造 IP 匹配器: %w", err)
		}
	}
	return m, nil
}

// Match 判断访问日志里的目标（host:port 或 [v6]:port）是否属于该组。
func (m *groupMatcher) Match(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		if m.ip == nil {
			return false
		}
		// IPMatcher 只有 Apply(routing.Context)，借 command 包的适配器塞入目标 IP。
		ctx := routercmd.AsRoutingContext(&routercmd.RoutingContext{TargetIPs: [][]byte{ip}})
		return m.ip.Apply(ctx)
	}
	return m.domain != nil && m.domain.ApplyDomain(host)
}

// buildLogEvidence 把日志分组折算成证据：只统计属于本组的行；被拒绝的计入 Rejected，
// 其余按 routeHead 与期望出站比对，符合计入 Matched，不符的按出站分组进 Others。
func buildLogEvidence(rows []TargetRouteCount, m *groupMatcher, exp ExpectedOutbound, defaultTag string, labelOf func(tag string) string) LogEvidence {
	ev := LogEvidence{Others: []LogOther{}}
	byRoute := map[string]*LogOther{}
	var order []string
	for _, r := range rows {
		if !m.Match(r.Target) {
			continue
		}
		if !r.Accepted {
			ev.Rejected += r.Count
			continue
		}
		head := routeHead(r.Route)
		if match, _ := judgeSample(exp, head, false, defaultTag); match {
			ev.Matched += r.Count
			continue
		}
		o := byRoute[head]
		if o == nil {
			o = &LogOther{Route: head, Label: labelOf(head), Examples: []string{}}
			byRoute[head] = o
			order = append(order, head)
		}
		o.Count += r.Count
		if r.LastAt > o.LastAt {
			o.LastAt = r.LastAt
		}
		if len(o.Examples) < logOtherMaxExamples && !hasExample(o.Examples, r.Target) {
			o.Examples = append(o.Examples, r.Target)
		}
	}
	for _, k := range order {
		ev.Others = append(ev.Others, *byRoute[k])
	}
	sort.SliceStable(ev.Others, func(i, j int) bool { return ev.Others[i].Count > ev.Others[j].Count })
	return ev
}

func hasExample(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// 以下是编排部分：读库、取样、调核心、查访问日志，最后交给上面的纯函数下结论。
// 全程只读：TestRoute 不发布结果（PublishResult=false），不重启、不写配置、不写库。

// 规模与超时（设计 §3.6、§4.2）。
const (
	verifyRPCTimeout         = 3 * time.Second
	verifyBudget             = 20 * time.Second
	verifyPerGroupSamples    = 2
	verifyPerUserSamples     = 6
	verifyGlobalInboundLimit = 50
	verifyLogWindowHours     = 24
	verifyLogGroupLimit      = 5000
)

// 结果里的固定文案。
const (
	labelBlock           = "封禁"
	labelDirect          = "直连"
	labelDefaultOutbound = "直连（默认出站）"
	labelNoMatch         = "默认出站（未命中任何规则）"
	sampleErrTimeout     = "超时"
)

// routeFunc 是对核心做一次 TestRoute 的函数，签名同 XrayAPI.TestRoute。
type routeFunc func(ctx context.Context, inboundTag, domain string, ip net.IP) (tag string, noMatch bool, err error)

// verifyMu 保证同一时刻只有一个验证在跑：管理员连点时不把核心打满。
// 与包级 lock 无关——lock 只由 RuntimeSnapshot 瞬时持有。
var verifyMu sync.Mutex

// RoutingVerifyService 对一条分流规则做「核心实时路由 + 访问日志」两路核对。
// 零值可用：字段为 nil/0 时用真实实现与默认值，测试可注入伪造的 route/runtime。
type RoutingVerifyService struct {
	route              routeFunc
	runtime            func() XrayRuntime
	now                func() time.Time
	rpcTimeout, budget time.Duration
}

// verifyUser 是待验证的一个用户（入站）。
type verifyUser struct {
	inbound  *model.Inbound
	disabled bool
}

// verifyPlan 是从库里推出的、与核心无关的验证计划。
type verifyPlan struct {
	expected   ExpectedOutbound
	defaultTag string
	targets    []sampleTarget
	domains    []string // 所有组合并后的域名条目（访问日志归属判定用）
	cidrs      []string // 所有组合并后的 IP 条目
	users      []verifyUser
	truncated  bool
	labelOf    func(tag string) string
}

// Verify 验证一条规则。规则不存在返回 error；其余情况（含规则停用、不会写进配置、
// 核心不可用）都返回结果，由 Core.State 说明原因。
func (s *RoutingVerifyService) Verify(ruleId int) (*RuleVerifyResult, error) {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	res := &RuleVerifyResult{
		RuleId:    ruleId,
		CheckedAt: now.UnixMilli(),
		AccessLog: AccessLogState{WindowHours: verifyLogWindowHours},
		Users:     []UserVerify{},
	}
	if !verifyMu.TryLock() {
		res.Core = CoreState{State: coreStateBusy, Message: "已有验证在进行，请稍后"}
		return res, nil
	}
	defer verifyMu.Unlock()

	rule, err := (&RoutingRuleService{}).Get(ruleId)
	if err != nil {
		return nil, err
	}
	if !rule.Enable {
		res.Core = CoreState{State: coreStateError, Message: "规则已停用，不会写进核心配置"}
		return res, nil
	}

	runtime := s.runtime
	if runtime == nil {
		runtime = (&XrayService{}).RuntimeSnapshot
	}
	rt := runtime()

	plan, reason := s.planRule(rule, rt)
	if reason != "" {
		res.Core = CoreState{State: coreStateError, Message: reason}
		return res, nil
	}
	res.Expected = plan.expected
	res.Truncated = plan.truncated

	res.Core = coreStateOf(rt)
	route := s.route
	if route == nil && (res.Core.State == coreStateOK || res.Core.State == coreStatePending) {
		api := &xray.XrayAPI{}
		if err := api.Init(rt.APIPort); err != nil {
			res.Core = CoreState{State: coreStateError, Message: "连接核心控制面失败：" + err.Error()}
		} else {
			defer api.Close()
			route = api.TestRoute
		}
	}
	canRoute := res.Core.State == coreStateOK || res.Core.State == coreStatePending

	logEnabled, err := (&SettingService{}).GetAccessLogEnable()
	if err != nil {
		logger.Warning("路由验证：读取访问日志开关失败，按未开启处理：", err)
		logEnabled = false
	}
	res.AccessLog.Enabled = logEnabled
	var matcher *groupMatcher
	if logEnabled {
		if matcher, err = newGroupMatcher(plan.domains, plan.cidrs); err != nil {
			// 匹配器建不出来就没有日志证据，但不影响核心那一路。
			logger.Warning("路由验证：构造访问日志匹配器失败，rule id:", rule.Id, "err:", err)
			matcher = nil
		}
	}

	rpcTimeout := s.rpcTimeout
	if rpcTimeout <= 0 {
		rpcTimeout = verifyRPCTimeout
	}
	budget := s.budget
	if budget <= 0 {
		budget = verifyBudget
	}
	budgetCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	sinceMs := now.Add(-verifyLogWindowHours * time.Hour).UnixMilli()
	for _, vu := range plan.users {
		u := UserVerify{InboundId: vu.inbound.Id, Name: inboundName(vu.inbound), Samples: []SampleResult{}}
		if vu.disabled {
			// 停用的入站不在核心配置里，buildRule 也会把它剔掉，对它调 TestRoute 没有意义。
			u.Verdict, u.Reason = verdictUnverifiable, "该用户已停用，规则不会对其生效"
			res.Users = append(res.Users, u)
			continue
		}
		if canRoute {
			u.Samples = runSamples(budgetCtx, route, rpcTimeout, vu.inbound.Tag, plan)
		}
		if matcher != nil {
			rows, truncated, err := (&AccessLogService{}).GroupByTargetRoute(vu.inbound.Id, sinceMs, verifyLogGroupLimit)
			if err != nil {
				logger.Warning("路由验证：读取访问日志失败，inbound id:", vu.inbound.Id, "err:", err)
			} else {
				ev := buildLogEvidence(rows, matcher, plan.expected, plan.defaultTag, plan.labelOf)
				ev.Truncated = truncated
				u.Log = &ev
			}
		}
		u.Verdict, u.Reason = combineVerdict(res.Core.State, u.Samples, u.Log)
		if res.Core.State == coreStateError && u.Verdict == verdictUnverifiable {
			// combineVerdict 只给笼统原因，这里补上核心那一侧的具体错误。
			u.Reason = "核心验证出错：" + res.Core.Message
		}
		res.Users = append(res.Users, u)
	}
	return res, nil
}

// coreStateOf 把运行时快照映射成核心级状态。pending 仍执行 TestRoute（设计 §3.5）。
func coreStateOf(rt XrayRuntime) CoreState {
	switch {
	case rt.Busy:
		return CoreState{State: coreStateBusy, Message: "核心正在更新配置，请稍后重新验证"}
	case !rt.Running:
		return CoreState{State: coreStateNotRunning, Message: "xray 未运行"}
	case rt.Pending:
		return CoreState{State: coreStatePending, Message: "配置已保存，正在等待下发到核心（最多约 10 秒）"}
	default:
		return CoreState{State: coreStateOK}
	}
}

// runSamples 对一个用户逐个样例调 route。单次 rpcTimeout，整批共用 budgetCtx；
// 预算用完后剩下的样例不再调用，直接记「超时」。
func runSamples(budgetCtx context.Context, route routeFunc, rpcTimeout time.Duration, inboundTag string, plan *verifyPlan) []SampleResult {
	out := make([]SampleResult, 0, len(plan.targets))
	for _, t := range plan.targets {
		r := SampleResult{Target: t.Display}
		if budgetCtx.Err() != nil {
			r.Error = sampleErrTimeout
			out = append(out, r)
			continue
		}
		ctx, cancel := context.WithTimeout(budgetCtx, rpcTimeout)
		tag, noMatch, err := route(ctx, inboundTag, t.Domain, t.IP)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			if timedOut || errors.Is(err, context.DeadlineExceeded) {
				r.Error = sampleErrTimeout
			} else {
				r.Error = err.Error()
			}
			out = append(out, r)
			continue
		}
		r.ActualTag = tag
		if noMatch {
			r.ActualLabel = labelNoMatch
		} else {
			r.ActualLabel = plan.labelOf(tag)
		}
		r.Match, r.HitByRule = judgeSample(plan.expected, tag, noMatch, plan.defaultTag)
		out = append(out, r)
	}
	return out
}

// planRule 从库里推出验证计划；返回非空 reason 表示规则不会写进核心配置，不发 RPC。
//
// 「会不会写进配置」直接交给 buildRule 判断，不另写一份：两份判定迟早漂移，
// 漂移后这里说「可以验证」的规则核心里其实没有。
func (s *RoutingVerifyService) planRule(rule *model.RoutingRule, rt XrayRuntime) (*verifyPlan, string) {
	inbounds, err := (&InboundService{}).GetAllInbounds()
	if err != nil {
		return nil, "读取入站失败：" + err.Error()
	}
	sort.Slice(inbounds, func(i, j int) bool { return inbounds[i].Id < inbounds[j].Id })

	injector := &RoutingInjector{}
	// 传空模板只为拿到「实际会写进配置的节点」映射，与生成期同一套过滤。
	_, usable, _, err := injector.buildOutbounds(nil)
	if err != nil {
		return nil, "读取出站节点失败：" + err.Error()
	}

	// 默认出站 tag 取核心实际持有的配置；核心没在跑时退回当前生成配置，
	// 两者都是 tagDefaultOutbound 写出的同一个值，只是前者才是核心眼里的。
	defaultTag := rt.DefaultTag
	if defaultTag == "" && rule.Action == model.ActionDirect {
		if cfg, cfgErr := (&XrayService{}).GetXrayConfig(); cfgErr == nil {
			defaultTag = defaultOutboundTag(cfg)
		}
	}
	if _, _, skip := injector.buildRule(rule, enabledInboundTags(inbounds), usable, defaultTag); skip != nil {
		return nil, "本规则不会写进核心配置：" + skip.Error()
	}

	plan := &verifyPlan{defaultTag: defaultTag}
	groupIds, _ := DecodeDomainGroupIds(rule.DomainGroupIds) // buildRule 已校验过
	domainLists, cidrLists := ruleGroupConditions(groupIds)
	for i := range domainLists {
		plan.targets = append(plan.targets, sampleTargets(domainLists[i], cidrLists[i], verifyPerGroupSamples)...)
	}
	plan.targets = dedupeTargets(plan.targets, verifyPerUserSamples)
	plan.domains = MergeDomains(domainLists...)
	plan.cidrs = MergeDomains(cidrLists...)

	nodes, err := (&OutboundNodeService{}).GetAll()
	if err != nil {
		return nil, "读取出站节点失败：" + err.Error()
	}
	// 含停用节点：访问日志里可能还留着它们的 tag。
	remarkByTag := make(map[string]string, len(nodes))
	for _, n := range nodes {
		remarkByTag[n.Tag] = nodeLabel(n)
	}
	plan.labelOf = func(tag string) string {
		switch {
		case tag == model.BlockOutboundTag:
			return labelBlock
		case (defaultTag != "" && tag == defaultTag) || model.IsMeterTag(tag):
			return labelDefaultOutbound
		}
		if label, ok := remarkByTag[tag]; ok {
			return label
		}
		return tag
	}

	switch rule.Action {
	case model.ActionProxy:
		node, err := (&OutboundNodeService{}).Get(rule.OutboundId)
		if err != nil {
			return nil, "读取出站节点失败：" + err.Error()
		}
		plan.expected = ExpectedOutbound{Action: rule.Action, Tag: node.Tag, Label: nodeLabel(node)}
	case model.ActionBlock:
		plan.expected = ExpectedOutbound{Action: rule.Action, Tag: model.BlockOutboundTag, Label: labelBlock}
	case model.ActionDirect:
		plan.expected = ExpectedOutbound{Action: rule.Action, Tag: defaultTag, Label: labelDirect}
	}

	inboundIds, _ := DecodeInboundIds(rule.InboundIds) // buildRule 已校验过
	if len(inboundIds) == 0 {
		// 全局规则：只验证启用的入站，按 id 升序截到上限。
		for _, in := range inbounds {
			if !in.Enable {
				continue
			}
			if len(plan.users) >= verifyGlobalInboundLimit {
				plan.truncated = true
				break
			}
			plan.users = append(plan.users, verifyUser{inbound: in})
		}
		return plan, ""
	}
	byId := make(map[int]*model.Inbound, len(inbounds))
	for _, in := range inbounds {
		byId[in.Id] = in
	}
	for _, id := range inboundIds {
		in, ok := byId[id]
		if !ok {
			// 已删除的入站 buildRule 同样剔掉；没有名字可展示，不列出。
			continue
		}
		plan.users = append(plan.users, verifyUser{inbound: in, disabled: !in.Enable})
	}
	return plan, ""
}

// ruleGroupConditions 按 groupIds 的顺序逐组取合并后的域名与 IP 条目，
// 与 buildRule 的取法一致：组不存在、数据损坏、两侧都空的组剔除（buildRule 已记过日志）。
// 返回的两个切片按下标一一对应同一个组。
func ruleGroupConditions(groupIds []int) (domainLists, cidrLists [][]string) {
	gs := &DomainGroupService{}
	for _, gid := range groupIds {
		group, err := gs.Get(gid)
		if err != nil {
			continue
		}
		manualDomains, err := DecodeDomains(group.Domains)
		if err != nil {
			continue
		}
		subscribedDomains, err := DecodeSubscribedDomains(group.SubscribedDomains)
		if err != nil {
			continue
		}
		manualCidrs, err := DecodeCidrs(group.Cidrs)
		if err != nil {
			continue
		}
		subscribedCidrs, err := DecodeSubscribedCidrs(group.SubscribedCidrs)
		if err != nil {
			continue
		}
		domains := MergeDomains(manualDomains, subscribedDomains)
		cidrs := MergeDomains(manualCidrs, subscribedCidrs)
		if len(domains) == 0 && len(cidrs) == 0 {
			continue
		}
		domainLists = append(domainLists, domains)
		cidrLists = append(cidrLists, cidrs)
	}
	return domainLists, cidrLists
}

// dedupeTargets 按展示值去重（多个组可能含同一个域名），并截到 limit 个。
func dedupeTargets(targets []sampleTarget, limit int) []sampleTarget {
	out := make([]sampleTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if len(out) >= limit {
			break
		}
		if seen[t.Display] {
			continue
		}
		seen[t.Display] = true
		out = append(out, t)
	}
	return out
}

func nodeLabel(n *model.OutboundNode) string {
	if n.Remark != "" {
		return n.Remark
	}
	return n.Tag
}

func inboundName(in *model.Inbound) string {
	if in.Remark != "" {
		return in.Remark
	}
	return in.Tag
}
