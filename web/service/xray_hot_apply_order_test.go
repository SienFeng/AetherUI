package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"a-ui/database/model"
	"a-ui/xray"
)

// orderOutbound 是数组首位默认出站之后的一个出站；ver 不同表示同 tag 换了内容（编辑出站）。
type orderOutbound struct {
	tag string
	ver int
}

// orderRule 是一条「ruleTag → 出站」的规则。
type orderRule struct{ ruleTag, outbound string }

// hotOrderConfig 造一份只有控制面入站、出站与规则的配置，热应用的各种顺序问题都只跟这三样有关。
func hotOrderConfig(t *testing.T, outbounds []orderOutbound, rules []orderRule) *xray.Config {
	t.Helper()
	outs := []any{map[string]any{"protocol": "freedom", "settings": map[string]any{}}}
	for _, o := range outbounds {
		outs = append(outs, map[string]any{"protocol": "freedom", "tag": o.tag, "settings": map[string]any{"userLevel": o.ver}})
	}
	rs := make([]any, 0, len(rules))
	for _, r := range rules {
		rs = append(rs, map[string]any{"type": "field", "ruleTag": r.ruleTag, "domain": []string{"domain:" + r.ruleTag + ".test"}, "outboundTag": r.outbound})
	}
	raw, err := json.Marshal(map[string]any{
		"inbounds":  []any{map[string]any{"listen": "127.0.0.1", "port": 62789, "protocol": "dokodemo-door", "settings": map[string]any{"address": "127.0.0.1"}, "tag": "api"}},
		"outbounds": outs,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rs},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &xray.Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeHotCore 按 xray 的语义维护出站集合与规则表：同 tag 的出站加第二次报 existing tag found（AddHandler 的行为），
// 整表下发替换全部规则，RemoveRules 按 ruleTag 删，追加接在表尾。每次调用之后检查有没有规则指向不存在的出站——
// 那样的规则在 xray 里会让命中它的连接被直接断开（TestCoreDropsConnectionsRoutedToAMissingOutbound）。
type fakeHotCore struct {
	t           *testing.T
	outbounds   map[string]bool
	rules       []orderRule
	calls       []string
	dangling    []string // 「哪次调用之后：哪条规则指向了不存在的出站」
	failRouting error    // 非空时整表下发报错且不改表
}

func newFakeHotCore(t *testing.T, cfg *xray.Config) *fakeHotCore {
	c := &fakeHotCore{t: t, outbounds: map[string]bool{}}
	var obs []struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(cfg.OutboundConfigs, &obs); err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.Tag != "" {
			c.outbounds[o.Tag] = true
		}
	}
	c.rules = orderRulesOf(t, cfg.RouterConfig)
	return c
}

func orderRulesOf(t *testing.T, routing []byte) []orderRule {
	t.Helper()
	var r struct {
		Rules []struct {
			RuleTag     string `json:"ruleTag"`
			OutboundTag string `json:"outboundTag"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(routing, &r); err != nil {
		t.Fatalf("解析 routing: %v", err)
	}
	out := make([]orderRule, 0, len(r.Rules))
	for _, x := range r.Rules {
		out = append(out, orderRule{x.RuleTag, x.OutboundTag})
	}
	return out
}

func (c *fakeHotCore) did(call string) {
	c.calls = append(c.calls, call)
	for _, r := range c.rules {
		if !c.outbounds[r.outbound] {
			c.dangling = append(c.dangling, fmt.Sprintf("%s 之后：规则 %s → 不存在的出站 %s", call, r.ruleTag, r.outbound))
		}
	}
}

func (c *fakeHotCore) DelInbound(tag string) error { c.did("delInbound " + tag); return nil }
func (c *fakeHotCore) AddInbound([]byte) error     { c.did("addInbound"); return nil }

func (c *fakeHotCore) DelOutbound(tag string) error {
	delete(c.outbounds, tag)
	c.did("delOutbound " + tag)
	return nil
}

func (c *fakeHotCore) AddOutbound(raw []byte) error {
	var ob struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &ob); err != nil {
		return err
	}
	if c.outbounds[ob.Tag] {
		c.calls = append(c.calls, "addOutbound "+ob.Tag+"（失败）")
		return fmt.Errorf("existing tag found: %s", ob.Tag)
	}
	c.outbounds[ob.Tag] = true
	c.did("addOutbound " + ob.Tag)
	return nil
}

func (c *fakeHotCore) ApplyRoutingConfig(routing []byte) error {
	if c.failRouting != nil {
		c.calls = append(c.calls, "full（失败）")
		return c.failRouting
	}
	c.rules = orderRulesOf(c.t, routing)
	c.did("full")
	return nil
}

func (c *fakeHotCore) RemoveRules(tags []string) error {
	drop := map[string]bool{}
	for _, tag := range tags {
		drop[tag] = true
	}
	kept := c.rules[:0:0]
	for _, r := range c.rules {
		if !drop[r.ruleTag] {
			kept = append(kept, r)
		}
	}
	c.rules = kept
	c.did("removeRules " + strings.Join(tags, ","))
	return nil
}

func (c *fakeHotCore) AppendRoutingRules(routing []byte) error {
	added := orderRulesOf(c.t, routing)
	c.rules = append(c.rules, added...)
	tags := make([]string, len(added))
	for i, r := range added {
		tags[i] = r.ruleTag
	}
	c.did("appendRules " + strings.Join(tags, ","))
	return nil
}

func (c *fakeHotCore) outboundTags() []string {
	out := make([]string, 0, len(c.outbounds))
	for tag := range c.outbounds {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func sortedTags(outbounds []orderOutbound) []string {
	out := make([]string, len(outbounds))
	for i, o := range outbounds {
		out[i] = o.tag
	}
	sort.Strings(out)
	return out
}

// runSendHotDiff 按 tryHotApply 的方式算出 diff 与 meterOnly，交给 sendHotDiff 下发到假核心。
func runSendHotDiff(t *testing.T, core hotApplyAPI, oldCfg, newCfg *xray.Config) error {
	t.Helper()
	diff, ok := xray.ComputeHotDiff(oldCfg, newCfg)
	if !ok {
		t.Fatal("这组改动应当能热应用")
	}
	meterOnly := xray.OnlyTouchesTags(oldCfg, newCfg, diff, model.IsMeterTag)
	_, err := sendHotDiff(core, oldCfg, newCfg, diff, meterOnly)
	return err
}

// 热应用的任何一步之后，核心里都不能有指向不存在出站的规则：xray 会把命中它的连接直接断开、不回落默认出站。
// 所以退出配置的出站要等路由不再引用它之后才删，新出站要在引用它的路由下发之前加好。
//
// 改动前先删出站、后下发路由：计量池每次换池（生产上几乎每小时），从刚退池的域名进来的新连接在这段窗口里全被断开；
// 管理员连节点带规则一起删时同理。
func TestSendHotDiffNeverLeavesARuleOnAMissingOutbound(t *testing.T) {
	const (
		meterOld  = "a-ui-meter-1-old.com"
		meterKeep = "a-ui-meter-1-keep.com"
		meterNew  = "a-ui-meter-1-new.com"
	)
	cases := []struct {
		name               string
		oldObs, newObs     []orderOutbound
		oldRules, newRules []orderRule
		wantMeterOnly      bool
	}{
		{
			name:          "换池：退池的计量出站在它的规则删掉之后才删（增量下发）",
			oldObs:        []orderOutbound{{"a-ui-node-a", 0}, {meterOld, 0}, {meterKeep, 0}},
			newObs:        []orderOutbound{{"a-ui-node-a", 0}, {meterKeep, 0}, {meterNew, 0}},
			oldRules:      []orderRule{{"r-a", "a-ui-node-a"}, {meterOld, meterOld}, {meterKeep, meterKeep}},
			newRules:      []orderRule{{"r-a", "a-ui-node-a"}, {meterKeep, meterKeep}, {meterNew, meterNew}},
			wantMeterOnly: true,
		},
		{
			name:     "删节点连同它的规则、加节点连同它的规则（整表下发）",
			oldObs:   []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-b", 0}},
			newObs:   []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-c", 0}},
			oldRules: []orderRule{{"r-a", "a-ui-node-a"}, {"r-b", "a-ui-node-b"}},
			newRules: []orderRule{{"r-a", "a-ui-node-a"}, {"r-c", "a-ui-node-c"}},
		},
		{
			name:     "路由没变也照样删掉退出配置的出站",
			oldObs:   []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-b", 0}},
			newObs:   []orderOutbound{{"a-ui-node-a", 0}},
			oldRules: []orderRule{{"r-a", "a-ui-node-a"}},
			newRules: []orderRule{{"r-a", "a-ui-node-a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldCfg := hotOrderConfig(t, tc.oldObs, tc.oldRules)
			newCfg := hotOrderConfig(t, tc.newObs, tc.newRules)
			if diff, _ := xray.ComputeHotDiff(oldCfg, newCfg); xray.OnlyTouchesTags(oldCfg, newCfg, diff, model.IsMeterTag) != tc.wantMeterOnly {
				t.Fatalf("用例前提不对：meterOnly 应为 %v", tc.wantMeterOnly)
			}
			core := newFakeHotCore(t, oldCfg)
			if err := runSendHotDiff(t, core, oldCfg, newCfg); err != nil {
				t.Fatalf("sendHotDiff: %v（调用：%v）", err, core.calls)
			}
			if len(core.dangling) > 0 {
				t.Fatalf("下发途中出现了指向不存在出站的规则：\n%s\n调用顺序：%v", strings.Join(core.dangling, "\n"), core.calls)
			}
			if got, want := core.outboundTags(), sortedTags(tc.newObs); !reflect.DeepEqual(got, want) {
				t.Errorf("下发完的出站 = %v，应为 %v", got, want)
			}
			if !reflect.DeepEqual(core.rules, tc.newRules) {
				t.Errorf("下发完的规则表 = %v，应为 %v", core.rules, tc.newRules)
			}
		})
	}
}

// 编辑出站（同 tag 换内容）只能先删后加——xray 的 AddHandler 对已存在的 tag 报 existing tag found。
// 删与加必须紧挨着：引用它的规则悬空的时间只有一次回环 RPC。改动前先把所有要换的出站都删掉再逐个加回，
// 换几个出站，前面那几个就多悬空几次 RPC。
func TestSendHotDiffReplacesEachEditedOutboundRightAway(t *testing.T) {
	obs := []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-b", 0}}
	edited := []orderOutbound{{"a-ui-node-a", 1}, {"a-ui-node-b", 1}}
	rules := []orderRule{{"r-a", "a-ui-node-a"}, {"r-b", "a-ui-node-b"}}
	oldCfg := hotOrderConfig(t, obs, rules)
	newCfg := hotOrderConfig(t, edited, rules)
	core := newFakeHotCore(t, oldCfg)
	if err := runSendHotDiff(t, core, oldCfg, newCfg); err != nil {
		t.Fatalf("sendHotDiff: %v", err)
	}
	want := []string{"delOutbound a-ui-node-a", "addOutbound a-ui-node-a", "delOutbound a-ui-node-b", "addOutbound a-ui-node-b"}
	if !reflect.DeepEqual(core.calls, want) {
		t.Fatalf("调用顺序 = %v，应为 %v", core.calls, want)
	}
	// 悬空只发生在删掉的那一次调用之后，下一次调用就是把它加回来。
	wantDangling := []string{
		"delOutbound a-ui-node-a 之后：规则 r-a → 不存在的出站 a-ui-node-a",
		"delOutbound a-ui-node-b 之后：规则 r-b → 不存在的出站 a-ui-node-b",
	}
	if !reflect.DeepEqual(core.dangling, wantDangling) {
		t.Fatalf("悬空窗口 = %v，应为 %v", core.dangling, wantDangling)
	}
}

// 路由下发失败时 sendHotDiff 报错、调用方退回整进程重启；退出配置的出站此时还没删，旧规则在重启之前照常能用。
func TestSendHotDiffKeepsRetiredOutboundsWhenRoutingFails(t *testing.T) {
	oldCfg := hotOrderConfig(t,
		[]orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-b", 0}},
		[]orderRule{{"r-a", "a-ui-node-a"}, {"r-b", "a-ui-node-b"}})
	newCfg := hotOrderConfig(t,
		[]orderOutbound{{"a-ui-node-a", 0}},
		[]orderRule{{"r-a", "a-ui-node-a"}})
	core := newFakeHotCore(t, oldCfg)
	core.failRouting = errors.New("boom")
	err := runSendHotDiff(t, core, oldCfg, newCfg)
	if err == nil {
		t.Fatal("路由下发失败应返回 error，由调用方退回重启")
	}
	if !core.outbounds["a-ui-node-b"] {
		t.Fatalf("路由下发失败时不该已经删掉旧规则还在用的出站（调用：%v）", core.calls)
	}
	if len(core.dangling) > 0 {
		t.Fatalf("出现了指向不存在出站的规则：%v", core.dangling)
	}
}
