package service

import (
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// 池按进池时刻排序，新进的排在最后：增量下发只需要把新进的接到核心规则表的末尾，不用重建排在它后面的规则。
// 同一时刻进池的按 (入站, 域名) 排，保证生成逐字节确定。
func TestPoolOrdersByEntryTimeSoNewcomersComeLast(t *testing.T) {
	setupMeterPoolTest(t)
	putPoolRowAt(t, 7, "zeta.com", 0, 1000)
	putPoolRowAt(t, 3, "beta.com", 0, 1000)
	putPoolRowAt(t, 3, "alpha.com", 0, 3000)
	putPoolRowAt(t, 9, "aaa.com", 0, 2000)

	got, err := (&MeterPoolService{}).Pool(time.Unix(4000, 0))
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	want := []MeterEntry{
		{InboundId: 3, Domain: "beta.com"},
		{InboundId: 7, Domain: "zeta.com"},
		{InboundId: 9, Domain: "aaa.com"},
		{InboundId: 3, Domain: "alpha.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("Pool 返回 %+v，期望 %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Pool 返回 %+v，期望 %+v", got, want)
		}
	}
}

// RemoveRule 按 ruleTag 删规则，增量下发靠它摘掉退池的计量规则：每条计量规则都要带 ruleTag，而且与它的计量出站同名
// （出站 tag 已经按 (入站, 域名) 唯一）。
func TestInjectMeterRulesCarryTheirOutboundTagAsRuleTag(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32011)
	putPoolRow(t, in.Id, "example.com", 0)
	putPoolRow(t, in.Id, "192.0.2.10", 0)

	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	meter := 0
	for _, r := range decodeRules(t, cfg) {
		tag, _ := r["outboundTag"].(string)
		if !model.IsMeterTag(tag) {
			continue
		}
		meter++
		if r["ruleTag"] != tag {
			t.Errorf("计量规则 %v 的 ruleTag = %v，期望与出站同名", tag, r["ruleTag"])
		}
	}
	if meter != 2 {
		t.Fatalf("计量规则 %d 条，期望 2", meter)
	}
}

// injectMeterConfig 用当前的库生成一份配置。
func injectMeterConfig(t *testing.T) *xray.Config {
	t.Helper()
	cfg := newTemplateConfig(t)
	if err := (&RoutingInjector{}).Inject(cfg); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	return cfg
}

func meterTailDelta(t *testing.T, before, after *xray.Config) *xray.RuleDelta {
	t.Helper()
	d, err := xray.TailRuleDelta(before, after, model.IsMeterTag)
	if err != nil {
		t.Fatalf("换池应能增量下发：%v", err)
	}
	return d
}

// 字母序排在最前的新域名进池：只追加它这一条，原有的计量规则不动。按 (入站, 域名) 排的话它会插到最前面，
// 后面每一条都得删了重建——那正是整表下发的代价。
func TestNewPoolEntryIsAppendedWithoutRebuildingTheOthers(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32012)
	putPoolRowAt(t, in.Id, "zeta.com", 0, 1000)
	putPoolRowAt(t, in.Id, "mid.com", 0, 1000)
	before := injectMeterConfig(t)
	putPoolRowAt(t, in.Id, "alpha.com", 0, 5000)
	after := injectMeterConfig(t)

	d := meterTailDelta(t, before, after)
	if len(d.Remove) != 0 || d.AppendCount != 1 {
		t.Fatalf("新进池一条只该追加这一条：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

// IP 计量规则必须排在全部域名规则之后：新域名进池时，排在它后面的 IP 规则要删了重加，别的不动。
func TestNewPoolDomainRebuildsOnlyItselfAndTheIPRules(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32013)
	putPoolRowAt(t, in.Id, "zeta.com", 0, 1000)
	putPoolRowAt(t, in.Id, "192.0.2.10", 0, 1000)
	before := injectMeterConfig(t)
	putPoolRowAt(t, in.Id, "alpha.com", 0, 5000)
	after := injectMeterConfig(t)

	d := meterTailDelta(t, before, after)
	if strings.Join(d.Remove, ",") != model.MeterTag(in.Id, "192.0.2.10") || d.AppendCount != 2 {
		t.Fatalf("只该重建新域名与其后的 IP 规则：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}

func TestRetiredPoolEntryIsRemovedWithoutRebuildingTheOthers(t *testing.T) {
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32014)
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		putPoolRowAt(t, in.Id, d, 0, 1000)
	}
	before := injectMeterConfig(t)
	// 退池：Recompute 写的是未来的冷却时刻，Pool 只认 cooldown_until = 0。
	if err := database.GetTrafficDB().Model(&model.MeterDomain{}).
		Where("inbound_id = ? and domain = ?", in.Id, "b.com").
		Update("cooldown_until", time.Now().Add(24*time.Hour).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	after := injectMeterConfig(t)

	d := meterTailDelta(t, before, after)
	if strings.Join(d.Remove, ",") != model.MeterTag(in.Id, "b.com") || d.AppendCount != 0 {
		t.Fatalf("退池只该删掉退池的那条：Remove=%v AppendCount=%d", d.Remove, d.AppendCount)
	}
}
