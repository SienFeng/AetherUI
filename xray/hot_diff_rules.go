package xray

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// RuleDelta 是只动了路由表尾一段规则时的增量下发：先按 ruleTag 删掉 Remove 里的规则（RoutingService.RemoveRule），
// 再把 AppendRouting 里的规则追加到表尾（AddRule，shouldAppend=true）。核心执行完之后的规则表与整表下发
// newCfg 逐条相同（TestTailRuleDeltaReproducesTheNewRuleListExactly）。
//
// 为什么要有它：整表下发时 xray 把每条规则重建一遍，而内联的域名与 IP 段不进它的缓存，每建一个匹配器强制
// GC 两次（common/geodata 的 MphValueMatcher.Build 与 IPSetFactory，"peak mem"）。计量规则每条带 IP 守卫，
// 一条就是 4 次；计量池每小时都会换几个域名，整表下发一次在 1 核的生产机上让 xray 满核约 9 秒（2026-11-12
// 香港生产机实测）。本机用同版本 xray 量过：整表 200 次强制 GC，增量 18 次（新进 2 条、IP 规则 5 条）。
type RuleDelta struct {
	// Remove 是要删掉的 ruleTag，顺序无关。核心里没有某个 tag 时 xray 不报错。
	Remove []string
	// AppendCount 是追加的规则条数；为 0 时 AppendRouting 为 nil。
	AppendCount int
	// AppendRouting 是只含追加规则的 routing 段，规则按新配置里的顺序。去掉了 balancers：追加模式下 xray 会
	// 把段里的负载均衡器当成新的再加一遍，撞上已有的 tag 就报错；其余字段原样保留，供 infra/conf 编译。
	AppendRouting []byte
}

// TailRuleDelta 计算把运行中的核心从 oldCfg 的路由规则搬到 newCfg 的增量。前提是两边的差异全部落在
// 表尾那段 outboundTag 被 isTag 认得的规则里（面板的计量规则永远排在最后）。不满足前提、或者无法保证
// 结果与整表下发逐条相同时返回 error 说明原因，调用方改为整表下发。
//
// 只重建必须重建的：内容不变的规则留在核心里，退出的规则按 ruleTag 删掉。新规则只能接在表尾，所以第一处
// 对不上的位置之后、本来要保留的规则也得删了重加——计量的 IP 规则必须排在全部域名规则之后，新进池的域名
// 会插在它们前面。池按进池时间排序（MeterPoolService.Pool），新进的排在各自那一组的末尾，重建的通常只有
// 新进的几条和其后的 IP 规则。
//
// 表尾规则必须带非空、互不相同、也不与前面的规则重名的 ruleTag：RemoveRule 按 ruleTag 删，重名会误删
// 前面的规则；追加时 xray 也会拒绝与已有规则重名的 ruleTag。
func TailRuleDelta(oldCfg, newCfg *Config, isTag func(string) bool) (*RuleDelta, error) {
	if oldCfg == nil || newCfg == nil {
		return nil, errors.New("配置为空")
	}
	oldR, err := splitTailRouting(oldCfg.RouterConfig, isTag)
	if err != nil {
		return nil, fmt.Errorf("旧配置：%w", err)
	}
	newR, err := splitTailRouting(newCfg.RouterConfig, isTag)
	if err != nil {
		return nil, fmt.Errorf("新配置：%w", err)
	}
	if !bytes.Equal(oldR.rest, newR.rest) {
		return nil, errors.New("routing 里规则以外的字段有变化")
	}
	if len(oldR.head) != len(newR.head) {
		return nil, errors.New("表尾以外的规则有变化")
	}
	for i := range oldR.head {
		if !sameJSON(oldR.head[i], newR.head[i]) {
			return nil, errors.New("表尾以外的规则有变化")
		}
	}
	for _, side := range []*tailRouting{oldR, newR} {
		for _, r := range side.tail {
			if oldR.headTags[r.ruleTag] || newR.headTags[r.ruleTag] {
				return nil, fmt.Errorf("表尾规则的 ruleTag %q 与前面的规则重名", r.ruleTag)
			}
		}
	}

	inNew := make(map[string]bool, len(newR.tail))
	for _, r := range newR.tail {
		inNew[string(r.norm)] = true
	}
	d := &RuleDelta{}
	kept := make([]tailRule, 0, len(oldR.tail))
	for _, r := range oldR.tail {
		if inNew[string(r.norm)] {
			kept = append(kept, r)
		} else {
			d.Remove = append(d.Remove, r.ruleTag)
		}
	}
	same := 0
	for same < len(kept) && same < len(newR.tail) && bytes.Equal(kept[same].norm, newR.tail[same].norm) {
		same++
	}
	for _, r := range kept[same:] {
		d.Remove = append(d.Remove, r.ruleTag)
	}

	appendRules := newR.tail[same:]
	if len(appendRules) == 0 {
		return d, nil
	}
	raws := make([]json.RawMessage, len(appendRules))
	for i, r := range appendRules {
		raws[i] = r.raw
	}
	encodedRules, err := json.Marshal(raws)
	if err != nil {
		return nil, fmt.Errorf("编码追加规则：%w", err)
	}
	routing := make(map[string]json.RawMessage, len(newR.fields))
	for k, v := range newR.fields {
		if k != "rules" && k != "balancers" {
			routing[k] = v
		}
	}
	routing["rules"] = encodedRules
	out, err := json.Marshal(routing)
	if err != nil {
		return nil, fmt.Errorf("编码追加段：%w", err)
	}
	d.AppendCount = len(appendRules)
	d.AppendRouting = out
	return d, nil
}

type tailRule struct {
	raw     json.RawMessage
	norm    []byte
	ruleTag string
}

type tailRouting struct {
	fields   map[string]json.RawMessage // routing 段的各字段原样，追加段从这里取 rules / balancers 以外的字段
	rest     []byte                     // 去掉 rules 之后规范化的 routing 段
	head     []json.RawMessage          // 表尾之前的规则
	headTags map[string]bool            // 表尾之前的规则里出现过的非空 ruleTag
	tail     []tailRule                 // 表尾 outboundTag 被 isTag 认得的规则
}

func splitTailRouting(raw []byte, isTag func(string) bool) (*tailRouting, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("没有 routing 段")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("routing 段无法解析")
	}
	var rules []json.RawMessage
	if encoded, ok := fields["rules"]; ok {
		if err := json.Unmarshal(encoded, &rules); err != nil {
			return nil, errors.New("rules 无法解析")
		}
	}
	others := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		if k != "rules" {
			others[k] = v
		}
	}
	encodedOthers, err := json.Marshal(others)
	if err != nil {
		return nil, errors.New("routing 段无法编码")
	}
	rest, ok := normalizeJSON(encodedOthers)
	if !ok {
		return nil, errors.New("routing 段无法规范化")
	}

	t := &tailRouting{fields: fields, rest: rest, headTags: map[string]bool{}}
	seen := make(map[string]bool)
	for i, r := range rules {
		var h struct {
			OutboundTag string `json:"outboundTag"`
			RuleTag     string `json:"ruleTag"`
		}
		if err := json.Unmarshal(r, &h); err != nil {
			return nil, fmt.Errorf("第 %d 条规则无法解析", i)
		}
		if h.OutboundTag == "" || !isTag(h.OutboundTag) {
			if len(t.tail) > 0 {
				return nil, fmt.Errorf("第 %d 条规则（出站 %q）排在表尾规则之后", i, h.OutboundTag)
			}
			t.head = append(t.head, r)
			if h.RuleTag != "" {
				t.headTags[h.RuleTag] = true
			}
			continue
		}
		if h.RuleTag == "" {
			return nil, fmt.Errorf("表尾规则（出站 %s）没有 ruleTag", h.OutboundTag)
		}
		if seen[h.RuleTag] {
			return nil, fmt.Errorf("表尾规则的 ruleTag %q 重复", h.RuleTag)
		}
		seen[h.RuleTag] = true
		norm, ok := normalizeJSON(r)
		if !ok {
			return nil, fmt.Errorf("第 %d 条规则无法规范化", i)
		}
		t.tail = append(t.tail, tailRule{raw: r, norm: norm, ruleTag: h.RuleTag})
	}
	return t, nil
}

// sameJSON 先比字节（生成是逐字节确定的，绝大多数情况在这里就结束，免得把 11 万条的大规则再解析一遍），
// 不同再按语义比。
func sameJSON(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	na, okA := normalizeJSON(a)
	nb, okB := normalizeJSON(b)
	return okA && okB && bytes.Equal(na, nb)
}
