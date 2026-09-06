package service

import (
	"reflect"
	"testing"
	"time"

	"a-ui/database/model"
)

func TestMeterPoolCapacity(t *testing.T) {
	// K = min(meterPoolCapMax, meterOutboundBudget / N)。上限来自实测：
	// 每个计量出站给真实 xray 校验多加约 1.03 ms，200 个约 0.21 秒。
	cases := []struct{ enabled, want int }{
		{0, 60}, // 没有启用入站：按 1 算，不做特殊分支
		{1, 60},
		{2, 60}, // 200/2 = 100 > 60，被 capMax 压住
		{4, 50}, // 200/4 = 50
		{10, 20},
		{200, 1},
		{201, 0}, // 预算被摊薄到 0：整个计量功能自动停用
	}
	for _, c := range cases {
		if got := meterPoolCapacity(c.enabled); got != c.want {
			t.Errorf("meterPoolCapacity(%d) = %d，期望 %d", c.enabled, got, c.want)
		}
	}
}

func TestMeterAvgBytesPerConn(t *testing.T) {
	if got := meterAvgBytesPerConn(nil); got != 0 {
		t.Errorf("空输入 = %v，期望 0", got)
	}
	// 只有连接数没有字节（刚上线第一小时）时也必须是 0，
	// 这样权重退化成 0，排序键自然落到第二项 count desc。
	if got := meterAvgBytesPerConn([]meterAgg{{Domain: "a.com", Count: 10}}); got != 0 {
		t.Errorf("零字节 = %v，期望 0", got)
	}
	aggs := []meterAgg{
		{Domain: "a.com", Bytes: 900, Count: 3},
		{Domain: "b.com", Bytes: 100, Count: 2},
	}
	if got := meterAvgBytesPerConn(aggs); got != 200 {
		t.Errorf("平均 = %v，期望 200（1000 字节 / 5 次连接）", got)
	}
	// 零字节的行不进分母。这一条是有区分力的那条：上面两条 fixture 的每一行
	// 都是非零字节，「按全部求和」与「只按非零行求和」在它们身上恰好重合。
	mixed := []meterAgg{
		{Domain: "metered.com", Bytes: 1000, Count: 5},
		{Domain: "unmetered.com", Bytes: 0, Count: 4},
	}
	if got := meterAvgBytesPerConn(mixed); got != 200 {
		t.Errorf("混合输入 = %v，期望 200（只按 metered.com 的 1000/5 算，"+
			"unmetered.com 那 4 次连接不进分母）", got)
	}
}

func TestBuildMeterCandidatesFiltersNonRegistrable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	aggs := []meterAgg{
		{Domain: "doubleclick.net", Bytes: 100, Count: 1},
		{Domain: "com", Bytes: 999, Count: 9},             // 公共后缀本身：domain:com 会吸走全部 .com
		{Domain: "1.2.3.4", Bytes: 999, Count: 9},         // IP 字面量：domain 条件对它永不命中
		{Domain: "www.example.com", Bytes: 999, Count: 9}, // 子域名：池里只放归并后的注册域名
	}
	cands := buildMeterCandidates(aggs, nil, nil, nil, now)
	if len(cands) != 1 || cands[0].Domain != "doubleclick.net" {
		t.Fatalf("候选 = %+v，期望只剩 doubleclick.net", cands)
	}
}

func TestBuildMeterCandidatesEstimatesUnmeteredDomains(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// 已计量域名 a：1000 字节 / 5 次连接 → 平均 200 字节/连接。
	// 未计量域名 b：只有 4 次连接，折算权重 = 4 × 200 = 800。
	aggs := []meterAgg{
		{Domain: "a.com", Bytes: 1000, Count: 5},
		{Domain: "b.com", Bytes: 0, Count: 4},
	}
	cands := buildMeterCandidates(aggs, nil, nil, nil, now)
	byDom := map[string]meterCandidate{}
	for _, c := range cands {
		byDom[c.Domain] = c
	}
	if byDom["a.com"].Weight != 1000 {
		t.Errorf("a.com 权重 = %v，期望 1000（实测字节直接用）", byDom["a.com"].Weight)
	}
	if byDom["b.com"].Weight != 800 {
		t.Errorf("b.com 权重 = %v，期望 800（4 次 × 平均 200 字节）——"+
			"未计量域名不折算的话字节恒为 0，永远进不了池，池会锁死在上线首日那批",
			byDom["b.com"].Weight)
	}
	// 排序：权重高的在前
	if cands[0].Domain != "a.com" {
		t.Errorf("首位 = %q，期望 a.com", cands[0].Domain)
	}
}

func TestBuildMeterCandidatesGivesIncumbentsTheSwapMargin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-10 * time.Hour).Unix() // 早已过最小驻留期
	inPool := map[string]*model.MeterDomain{
		"a.com": {Domain: "a.com", EnteredAt: old},
	}
	aggs := []meterAgg{
		{Domain: "a.com", Bytes: 100, Count: 1},
		{Domain: "b.com", Bytes: 100, Count: 1},
	}
	cands := buildMeterCandidates(aggs, inPool, nil, nil, now)
	byDom := map[string]meterCandidate{}
	for _, c := range cands {
		byDom[c.Domain] = c
	}
	// 在位加成就是「替换余量」：权重相同的挑战者挤不掉在位者，
	// 否则两个权重相近的域名会每轮互换，出站被反复热增删。
	if byDom["a.com"].Weight != 125 {
		t.Errorf("在位者权重 = %v，期望 125（100 × 1.25）", byDom["a.com"].Weight)
	}
	if byDom["b.com"].Weight != 100 {
		t.Errorf("挑战者权重 = %v，期望 100（无加成）", byDom["b.com"].Weight)
	}
	if cands[0].Domain != "a.com" {
		t.Errorf("首位 = %q，期望在位者 a.com 排前面", cands[0].Domain)
	}
}

func TestBuildMeterCandidatesKeepsPoolDomainsWithNoRows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	inPool := map[string]*model.MeterDomain{
		// 刚进池，窗口内一条 DomainStat 行都没有（零增量不写行）
		"fresh.com": {Domain: "fresh.com", EnteredAt: now.Unix()},
	}
	cands := buildMeterCandidates(nil, inPool, nil, nil, now)
	if len(cands) != 1 || cands[0].Domain != "fresh.com" {
		t.Fatalf("候选 = %+v，期望包含池里那个没有任何行的域名——"+
			"漏掉它会让它绕过最小驻留闸门被静默挤出去", cands)
	}
	if !cands[0].MustKeep {
		t.Error("MustKeep = false，期望 true：刚进池不足两轮的域名必须强制保留")
	}
}

func TestBuildMeterCandidatesExcludesCoolingAndRetired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	aggs := []meterAgg{
		{Domain: "cool.com", Bytes: 999, Count: 9},
		{Domain: "gone.com", Bytes: 999, Count: 9},
		{Domain: "ok.com", Bytes: 1, Count: 1},
	}
	inPool := map[string]*model.MeterDomain{
		"gone.com": {Domain: "gone.com", EnteredAt: now.Add(-10 * time.Hour).Unix()},
	}
	cands := buildMeterCandidates(aggs, inPool,
		map[string]bool{"cool.com": true}, map[string]bool{"gone.com": true}, now)
	if len(cands) != 1 || cands[0].Domain != "ok.com" {
		t.Fatalf("候选 = %+v，期望只剩 ok.com", cands)
	}
}

func TestPickPoolHonoursMinHoldThenWeight(t *testing.T) {
	// 容量 1，权重最高的是 hot.com，但 fresh.com 处于最小驻留期。
	cands := []meterCandidate{
		{Domain: "hot.com", Weight: 10000, Count: 1},
		{Domain: "fresh.com", Weight: 0, Count: 0, MustKeep: true},
	}
	got := pickPool(cands, 1)
	if !reflect.DeepEqual(got, []string{"fresh.com"}) {
		t.Errorf("pickPool = %v，期望 [fresh.com]——刚进池的域名权重必然是 0，"+
			"不保护就会被自己的 0 权重挤出去，永远拿不到实测数据", got)
	}
}

func TestPickPoolFillsByWeightAndReturnsSorted(t *testing.T) {
	cands := []meterCandidate{
		{Domain: "z.com", Weight: 300},
		{Domain: "a.com", Weight: 200},
		{Domain: "m.com", Weight: 100},
	}
	got := pickPool(cands, 2)
	// 选中的是权重前二（z/a），但返回必须按域名字典序——生成期靠它保证
	// 配置逐字节确定。
	if !reflect.DeepEqual(got, []string{"a.com", "z.com"}) {
		t.Errorf("pickPool = %v，期望 [a.com z.com]", got)
	}
}

func TestPickPoolCapacityZeroSelectsNothing(t *testing.T) {
	cands := []meterCandidate{{Domain: "a.com", Weight: 1, MustKeep: true}}
	if got := pickPool(cands, 0); len(got) != 0 {
		t.Errorf("pickPool(k=0) = %v，期望空——预算摊薄到 0 时整个功能停用", got)
	}
}
