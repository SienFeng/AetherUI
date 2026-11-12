package xray

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"a-ui/util/json_util"
)

func isTestMeterTag(tag string) bool { return strings.HasPrefix(tag, "m-") }

// 表头：模板与管理员规则（含一条带 ruleTag 的地区规则），不属于计量。
var tailDeltaPrefix = []string{
	`{"type":"field","inboundTag":["api"],"outboundTag":"api"}`,
	`{"type":"field","ruleTag":"a-ui-geo-1","inboundTag":["in-1"],"source":["::/0"],"outboundTag":"blocked"}`,
	`{"type":"field","domain":["domain:big.example"],"outboundTag":"proxy"}`,
}

func meterRuleJSON(name string) string {
	tag := "m-" + name
	if strings.HasPrefix(name, "ip") {
		return fmt.Sprintf(`{"type":"field","ruleTag":%q,"inboundTag":["in-1"],"ip":["192.0.2.%d/32"],"outboundTag":%q}`, tag, len(name), tag)
	}
	return fmt.Sprintf(`{"type":"field","ruleTag":%q,"inboundTag":["in-1"],"domain":["domain:%s.example"],"ip":["0.0.0.0/0","::/0"],"outboundTag":%q}`, tag, name, tag)
}

func routingCfg(extra string, rules ...string) *Config {
	all := append(append([]string{}, tailDeltaPrefix...), rules...)
	return &Config{RouterConfig: json_util.RawMessage(`{"domainStrategy":"IPIfNonMatch"` + extra + `,"rules":[` + strings.Join(all, ",") + `]}`)}
}

func meterRules(names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = meterRuleJSON(n)
	}
	return out
}

// applyDeltaLikeXray 按 xray 的语义把增量作用到规则表上：RemoveRule 删掉 ruleTag 相同的全部规则，
// AddRule(shouldAppend=true) 把新规则接在表尾。返回每条规则的规范化 JSON。
func applyDeltaLikeXray(t *testing.T, oldCfg *Config, d *RuleDelta) []string {
	t.Helper()
	cur := rulesOf(t, oldCfg.RouterConfig)
	remove := map[string]bool{}
	for _, tag := range d.Remove {
		remove[tag] = true
	}
	kept := cur[:0]
	for _, r := range cur {
		var head struct {
			RuleTag string `json:"ruleTag"`
		}
		_ = json.Unmarshal([]byte(r), &head)
		if head.RuleTag != "" && remove[head.RuleTag] {
			continue
		}
		kept = append(kept, r)
	}
	if d.AppendCount == 0 {
		if d.AppendRouting != nil {
			t.Fatalf("没有要追加的规则时 AppendRouting 应为 nil，得到 %s", d.AppendRouting)
		}
		return kept
	}
	appended := rulesOf(t, d.AppendRouting)
	if len(appended) != d.AppendCount {
		t.Fatalf("AppendCount=%d，AppendRouting 里有 %d 条", d.AppendCount, len(appended))
	}
	return append(kept, appended...)
}

func rulesOf(t *testing.T, routing []byte) []string {
	t.Helper()
	var r struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(routing, &r); err != nil {
		t.Fatalf("解析 routing: %v", err)
	}
	out := make([]string, len(r.Rules))
	for i, raw := range r.Rules {
		n, ok := normalizeJSON(raw)
		if !ok {
			t.Fatalf("规则 %d 无法规范化", i)
		}
		out[i] = string(n)
	}
	return out
}

func mustDelta(t *testing.T, oldCfg, newCfg *Config) *RuleDelta {
	t.Helper()
	d, err := TailRuleDelta(oldCfg, newCfg, isTestMeterTag)
	if err != nil {
		t.Fatalf("TailRuleDelta: %v", err)
	}
	if got, want := applyDeltaLikeXray(t, oldCfg, d), rulesOf(t, newCfg.RouterConfig); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("增量作用后的规则表与新配置不同：\n得到 %v\n期望 %v", got, want)
	}
	return d
}

func TestTailRuleDeltaAppendsOnlyTheNewRule(t *testing.T) {
	d := mustDelta(t, routingCfg("", meterRules("a", "b", "c")...), routingCfg("", meterRules("a", "b", "c", "d")...))
	if len(d.Remove) != 0 || d.AppendCount != 1 {
		t.Fatalf("新进池一条只该追加这一条：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

func TestTailRuleDeltaRemovesRetiredRulesWithoutRebuildingOthers(t *testing.T) {
	d := mustDelta(t, routingCfg("", meterRules("a", "b", "c")...), routingCfg("", meterRules("a", "c")...))
	if strings.Join(d.Remove, ",") != "m-b" || d.AppendCount != 0 {
		t.Fatalf("只退池时只该删退池的那条：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

// 计量的 IP 规则必须排在全部域名规则之后，新进池的域名因此插在中间：只重建它之后的那一段。
func TestTailRuleDeltaRebuildsOnlyTheRulesAfterAnInsertion(t *testing.T) {
	d := mustDelta(t, routingCfg("", meterRules("a", "b", "ip1", "ip22")...), routingCfg("", meterRules("a", "b", "c", "ip1", "ip22")...))
	if strings.Join(d.Remove, ",") != "m-ip1,m-ip22" || d.AppendCount != 3 {
		t.Fatalf("插入点之后的才需要重建：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

func TestTailRuleDeltaReplacesARuleWhoseContentChanged(t *testing.T) {
	changed := strings.Replace(meterRuleJSON("b"), `"in-1"`, `"in-2"`, 1)
	d := mustDelta(t, routingCfg("", meterRules("a", "b", "c")...), routingCfg("", meterRuleJSON("a"), changed, meterRuleJSON("c")))
	if d.AppendCount != 2 {
		t.Fatalf("内容变了的规则要删了重加（连同它后面的）：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

func TestTailRuleDeltaFromAndToAnEmptyPool(t *testing.T) {
	if d := mustDelta(t, routingCfg(""), routingCfg("", meterRules("a", "ip1")...)); d.AppendCount != 2 || len(d.Remove) != 0 {
		t.Fatalf("空池到有池：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
	if d := mustDelta(t, routingCfg("", meterRules("a", "ip1")...), routingCfg("")); d.AppendCount != 0 || len(d.Remove) != 2 {
		t.Fatalf("有池到空池：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

// 追加用的 routing 段必须去掉 balancers：追加模式下 xray 会把它们当成新的负载均衡器再加一遍，撞上已有的 tag 报错。
func TestTailRuleDeltaAppendRoutingCarriesNoBalancers(t *testing.T) {
	bal := `,"balancers":[{"tag":"b1","selector":["proxy"]}]`
	d := mustDelta(t, routingCfg(bal, meterRules("a")...), routingCfg(bal, meterRules("a", "b")...))
	var r map[string]json.RawMessage
	if err := json.Unmarshal(d.AppendRouting, &r); err != nil {
		t.Fatal(err)
	}
	if _, has := r["balancers"]; has {
		t.Fatalf("追加段不该带 balancers：%s", d.AppendRouting)
	}
	if string(r["domainStrategy"]) != `"IPIfNonMatch"` {
		t.Fatalf("追加段应保留 routing 的其余字段：%s", d.AppendRouting)
	}
}

// 随机的池变化（退池、进池、改内容、乱序）下，增量作用后的规则表都必须与整表下发逐条相同。
func TestTailRuleDeltaReproducesTheNewRuleListExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(20261112))
	universe := []string{"a", "b", "c", "d", "e", "f", "g", "h", "ip1", "ip22", "ip333", "ip4444"}
	pick := func() []string {
		var doms, ips []string
		for _, n := range rng.Perm(len(universe)) {
			if rng.Intn(2) == 0 {
				continue
			}
			if strings.HasPrefix(universe[n], "ip") {
				ips = append(ips, universe[n])
			} else {
				doms = append(doms, universe[n])
			}
		}
		return append(doms, ips...)
	}
	for i := 0; i < 500; i++ {
		mustDelta(t, routingCfg("", meterRules(pick()...)...), routingCfg("", meterRules(pick()...)...))
	}
}

func TestTailRuleDeltaRefusesWhatItCannotDoExactly(t *testing.T) {
	base := routingCfg("", meterRules("a", "b")...)
	cases := []struct {
		name     string
		old, new *Config
	}{
		{"表头规则变了", base, &Config{RouterConfig: json_util.RawMessage(strings.Replace(string(routingCfg("", meterRules("a", "c")...).RouterConfig), "big.example", "other.example", 1))}},
		{"domainStrategy 变了", base, &Config{RouterConfig: json_util.RawMessage(strings.Replace(string(routingCfg("", meterRules("a", "c")...).RouterConfig), "IPIfNonMatch", "AsIs", 1))}},
		{"balancers 变了", routingCfg(`,"balancers":[{"tag":"b1","selector":["x"]}]`, meterRules("a")...), routingCfg(`,"balancers":[{"tag":"b1","selector":["y"]}]`, meterRules("a", "b")...)},
		{"计量规则后面还有别的规则", base, routingCfg("", append(meterRules("a", "c"), tailDeltaPrefix[2])...)},
		{"旧配置里计量规则后面还有别的规则", routingCfg("", append(meterRules("a"), tailDeltaPrefix[2])...), routingCfg("", meterRules("a", "c")...)},
		// 两边后面跟的是同一条规则：其余部分比对得上，可追加的新规则会排到它后面，顺序就错了。
		{"两边的计量规则后面都跟着同一条别的规则", routingCfg("", append(meterRules("a"), tailDeltaPrefix[2])...), routingCfg("", append(meterRules("a", "c"), tailDeltaPrefix[2])...)},
		{"计量规则没有 ruleTag", base, routingCfg("", meterRuleJSON("a"), strings.Replace(meterRuleJSON("c"), `"ruleTag":"m-c",`, "", 1))},
		{"旧配置的计量规则没有 ruleTag", routingCfg("", strings.Replace(meterRuleJSON("a"), `"ruleTag":"m-a",`, "", 1)), routingCfg("", meterRules("a", "c")...)},
		{"计量规则的 ruleTag 重复", base, routingCfg("", meterRuleJSON("a"), strings.Replace(meterRuleJSON("c"), `"ruleTag":"m-c"`, `"ruleTag":"m-a"`, 1))},
		{"表头规则占用了计量规则的 ruleTag", base, routingCfg("", meterRuleJSON("a"), strings.Replace(meterRuleJSON("c"), `"ruleTag":"m-c"`, `"ruleTag":"a-ui-geo-1"`, 1))},
		{"routing 解析不了", base, &Config{RouterConfig: json_util.RawMessage(`{"rules":[`)}},
		{"一侧没有 routing 段", base, &Config{}},
	}
	for _, c := range cases {
		if d, err := TailRuleDelta(c.old, c.new, isTestMeterTag); err == nil {
			t.Errorf("%s：应拒绝增量下发，却得到 Remove=%v AppendCount=%d", c.name, d.Remove, d.AppendCount)
		}
	}
	if _, err := TailRuleDelta(nil, base, isTestMeterTag); err == nil {
		t.Error("旧配置为 nil 时应拒绝")
	}
}
