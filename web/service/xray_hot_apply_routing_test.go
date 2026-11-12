package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"a-ui/util/json_util"
	"a-ui/xray"
)

// fakeRoutingCore 按 xray 的语义维护一张规则表：整表下发替换全部，RemoveRules 按 ruleTag 删，
// AppendRoutingRules 接在表尾。fail* 非空时对应调用报错且不改表。
type fakeRoutingCore struct {
	rules                            []string // 规范化的规则 JSON
	calls                            []string
	failRemove, failAppend, failFull error
	removeFirstThenFail              bool // RemoveRules 删掉第一个 tag 之后才报错（删到一半失败）
}

func normalizedRules(t *testing.T, routing []byte) []string {
	t.Helper()
	var r struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(routing, &r); err != nil {
		t.Fatalf("解析 routing: %v", err)
	}
	out := make([]string, len(r.Rules))
	for i, raw := range r.Rules {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		out[i] = string(b)
	}
	return out
}

func newFakeRoutingCore(t *testing.T, cfg *xray.Config) *fakeRoutingCore {
	return &fakeRoutingCore{rules: normalizedRules(t, cfg.RouterConfig)}
}

type fakeRoutingAPI struct {
	t    *testing.T
	core *fakeRoutingCore
}

func (f fakeRoutingAPI) ApplyRoutingConfig(routing []byte) error {
	f.core.calls = append(f.core.calls, "full")
	if f.core.failFull != nil {
		return f.core.failFull
	}
	f.core.rules = normalizedRules(f.t, routing)
	return nil
}

func (f fakeRoutingAPI) RemoveRules(tags []string) error {
	f.core.calls = append(f.core.calls, "remove:"+strings.Join(tags, ","))
	if f.core.failRemove != nil {
		return f.core.failRemove
	}
	if f.core.removeFirstThenFail && len(tags) > 1 {
		if err := f.RemoveRules(tags[:1]); err != nil {
			return err
		}
		return errors.New("removed the first tag, then failed")
	}
	drop := map[string]bool{}
	for _, tag := range tags {
		drop[tag] = true
	}
	kept := f.core.rules[:0]
	for _, r := range f.core.rules {
		var head struct {
			RuleTag string `json:"ruleTag"`
		}
		_ = json.Unmarshal([]byte(r), &head)
		if head.RuleTag != "" && drop[head.RuleTag] {
			continue
		}
		kept = append(kept, r)
	}
	f.core.rules = kept
	return nil
}

func (f fakeRoutingAPI) AppendRoutingRules(routing []byte) error {
	f.core.calls = append(f.core.calls, "append")
	if f.core.failAppend != nil {
		return f.core.failAppend
	}
	f.core.rules = append(f.core.rules, normalizedRules(f.t, routing)...)
	return nil
}

func meterRule(name string) string {
	tag := "a-ui-meter-1-" + name
	return `{"type":"field","ruleTag":"` + tag + `","inboundTag":["in-1"],"domain":["domain:` + name + `"],"outboundTag":"` + tag + `"}`
}

func meterRoutingConfig(rules ...string) *xray.Config {
	cfg := hotApplyBaseConfig()
	all := append([]string{`{"type":"field","inboundTag":["api"],"outboundTag":"api"}`,
		`{"type":"field","domain":["domain:big.example"],"outboundTag":"direct"}`}, rules...)
	cfg.RouterConfig = json_util.RawMessage(`{"domainStrategy":"AsIs","rules":[` + strings.Join(all, ",") + `]}`)
	return cfg
}

func assertCoreMatches(t *testing.T, core *fakeRoutingCore, cfg *xray.Config) {
	t.Helper()
	if got, want := strings.Join(core.rules, "\n"), strings.Join(normalizedRules(t, cfg.RouterConfig), "\n"); got != want {
		t.Fatalf("核心规则表与新配置不同：\n得到 %s\n期望 %s", got, want)
	}
}

func TestApplyRoutingDiffAppendsOnlyTheChangedMeterRules(t *testing.T) {
	oldCfg := meterRoutingConfig(meterRule("a.com"), meterRule("b.com"))
	newCfg := meterRoutingConfig(meterRule("a.com"), meterRule("c.com"))
	core := newFakeRoutingCore(t, oldCfg)

	incremental, err := applyRoutingDiff(fakeRoutingAPI{t, core}, oldCfg, newCfg, &xray.HotDiff{RoutingConfig: newCfg.RouterConfig}, true)
	if err != nil || !incremental {
		t.Fatalf("只换了计量规则应增量下发：incremental=%v err=%v", incremental, err)
	}
	if got := strings.Join(core.calls, " "); got != "remove:a-ui-meter-1-b.com append" {
		t.Fatalf("控制面调用 = %q，期望只删退池的、只追加新进的", got)
	}
	assertCoreMatches(t, core, newCfg)
}

func TestApplyRoutingDiffReplacesTheWholeTableForOtherChanges(t *testing.T) {
	oldCfg := meterRoutingConfig(meterRule("a.com"))
	newCfg := meterRoutingConfig(meterRule("a.com"), meterRule("c.com"))
	core := newFakeRoutingCore(t, oldCfg)

	incremental, err := applyRoutingDiff(fakeRoutingAPI{t, core}, oldCfg, newCfg, &xray.HotDiff{RoutingConfig: newCfg.RouterConfig}, false)
	if err != nil || incremental {
		t.Fatalf("不是只换计量池时应整表下发：incremental=%v err=%v", incremental, err)
	}
	if got := strings.Join(core.calls, " "); got != "full" {
		t.Fatalf("控制面调用 = %q，期望整表下发", got)
	}
	assertCoreMatches(t, core, newCfg)
}

// TailRuleDelta 的前提不满足时直接整表下发（这里用旧配置的计量规则缺 ruleTag 来触发）。
func TestApplyRoutingDiffFallsBackToTheWholeTableWhenTheDeltaCannotBeComputed(t *testing.T) {
	oldCfg := meterRoutingConfig(strings.Replace(meterRule("a.com"), `"ruleTag":"a-ui-meter-1-a.com",`, "", 1))
	newCfg := meterRoutingConfig(meterRule("a.com"), meterRule("c.com"))
	core := newFakeRoutingCore(t, oldCfg)

	incremental, err := applyRoutingDiff(fakeRoutingAPI{t, core}, oldCfg, newCfg, &xray.HotDiff{RoutingConfig: newCfg.RouterConfig}, true)
	if err != nil || incremental {
		t.Fatalf("算不出增量时应整表下发：incremental=%v err=%v", incremental, err)
	}
	if got := strings.Join(core.calls, " "); got != "full" {
		t.Fatalf("控制面调用 = %q，期望直接整表下发", got)
	}
	assertCoreMatches(t, core, newCfg)
}

// 增量的任何一步失败都退回整表下发：整表替换不依赖核心当前的状态，半途的删除不会留下痕迹。
// 不能直接判热应用失败——那会整进程重启 xray，掐断所有人的连接，而换池每小时都有。
func TestApplyRoutingDiffFallsBackToTheWholeTableWhenAnIncrementalCallFails(t *testing.T) {
	for step, wantCalls := range map[string]string{
		"remove": "remove:a-ui-meter-1-b.com full",
		"append": "remove:a-ui-meter-1-b.com append full",
	} {
		oldCfg := meterRoutingConfig(meterRule("a.com"), meterRule("b.com"))
		newCfg := meterRoutingConfig(meterRule("a.com"), meterRule("c.com"))
		core := newFakeRoutingCore(t, oldCfg)
		if step == "remove" {
			core.failRemove = errors.New("boom")
		} else {
			core.failAppend = errors.New("boom")
		}

		incremental, err := applyRoutingDiff(fakeRoutingAPI{t, core}, oldCfg, newCfg, &xray.HotDiff{RoutingConfig: newCfg.RouterConfig}, true)
		if err != nil || incremental {
			t.Fatalf("%s 失败后应退回整表下发并成功：incremental=%v err=%v", step, incremental, err)
		}
		if got := strings.Join(core.calls, " "); got != wantCalls {
			t.Fatalf("%s 失败时的控制面调用 = %q，期望 %q（先试增量，失败后整表下发）", step, got, wantCalls)
		}
		assertCoreMatches(t, core, newCfg)
	}
}

// 删到一半失败：核心里少了一部分规则，整表下发把它补齐。
func TestApplyRoutingDiffRecoversFromAPartialRemoval(t *testing.T) {
	oldCfg := meterRoutingConfig(meterRule("a.com"), meterRule("b.com"), meterRule("c.com"))
	newCfg := meterRoutingConfig(meterRule("a.com"), meterRule("d.com"))
	core := newFakeRoutingCore(t, oldCfg)
	core.removeFirstThenFail = true

	incremental, err := applyRoutingDiff(fakeRoutingAPI{t, core}, oldCfg, newCfg, &xray.HotDiff{RoutingConfig: newCfg.RouterConfig}, true)
	if err != nil || incremental {
		t.Fatalf("删到一半失败后应退回整表下发并成功：incremental=%v err=%v", incremental, err)
	}
	if got := strings.Join(core.calls, " "); got != "remove:a-ui-meter-1-b.com,a-ui-meter-1-c.com remove:a-ui-meter-1-b.com full" {
		t.Fatalf("控制面调用 = %q", got)
	}
	assertCoreMatches(t, core, newCfg)
}

func TestApplyRoutingDiffReturnsTheWholeTableError(t *testing.T) {
	oldCfg := meterRoutingConfig(meterRule("a.com"))
	newCfg := meterRoutingConfig(meterRule("a.com"), meterRule("c.com"))
	core := newFakeRoutingCore(t, oldCfg)
	core.failAppend = errors.New("append boom")
	core.failFull = errors.New("full boom")

	if _, err := applyRoutingDiff(fakeRoutingAPI{t, core}, oldCfg, newCfg, &xray.HotDiff{RoutingConfig: newCfg.RouterConfig}, true); err == nil {
		t.Fatal("整表下发也失败时应返回错误，让调用方退回重启")
	}
	if got := strings.Join(core.calls, " "); got != "append full" {
		t.Fatalf("控制面调用 = %q，期望先试追加、再整表下发", got)
	}
}

func TestApplyRoutingDiffWithoutRoutingChangeSendsNothing(t *testing.T) {
	cfg := meterRoutingConfig(meterRule("a.com"))
	core := newFakeRoutingCore(t, cfg)
	if incremental, err := applyRoutingDiff(fakeRoutingAPI{t, core}, cfg, cfg, &xray.HotDiff{}, true); err != nil || incremental {
		t.Fatalf("路由没变不该下发：incremental=%v err=%v", incremental, err)
	}
	if len(core.calls) != 0 {
		t.Fatalf("路由没变却调用了控制面：%v", core.calls)
	}
}
