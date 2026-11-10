package service

import (
	"fmt"
	"testing"
	"time"

	"a-ui/database/model"
	"a-ui/util/accesslog"
)

// shrinkDomainStatCaps 把每桶的目标上限缩成 3，几个目标就能走到封顶分支。
func shrinkDomainStatCaps(t *testing.T) {
	t.Helper()
	oldHour, oldDay := domainStatMaxHourTargets, domainStatMaxDayTargets
	domainStatMaxHourTargets, domainStatMaxDayTargets = 3, 3
	t.Cleanup(func() { domainStatMaxHourTargets, domainStatMaxDayTargets = oldHour, oldDay })
}

// storeVisits 给每个目标写 n 次访问记录（同一时刻）。
func storeVisits(t *testing.T, tag string, at time.Time, visits map[string]int) {
	t.Helper()
	var entries []accesslog.Entry
	for target, n := range visits {
		for i := 0; i < n; i++ {
			entries = append(entries, accesslog.Entry{Time: at, SourceIP: "192.0.2.10", Network: "tcp",
				Target: target, Inbound: tag, Route: "direct", Accepted: true})
		}
	}
	if _, err := (&AccessLogService{}).Store(entries); err != nil {
		t.Fatalf("Store: %v", err)
	}
}

func domainStatCounts(t *testing.T, g model.TrafficGranularity) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, r := range listDomainStats(t, g) {
		out[r.Domain] += r.Count
	}
	return out
}

// 每个不同的目标（含 IP 字面量）都各占一个小时行和一个日行、分别留 30 天和 365 天：一个跑 BT
// 或被扫描器借道的入站一天就能留下上万行。每桶封顶，同一批里次数多的新目标先占位，超出的
// 次数并进一行——总次数一次不少。
func TestAggregateCapsDistinctTargetsPerBucket(t *testing.T) {
	setupDomainStatTest(t)
	shrinkDomainStatCaps(t)
	in := mkTrafficInbound(t, 31101, "甲")
	at := time.Date(2026, 11, 9, 3, 10, 0, 0, time.UTC)
	storeVisits(t, in.Tag, at, map[string]int{
		"198.51.100.1:6881": 5, "198.51.100.2:6881": 4, "198.51.100.3:6881": 3,
		"198.51.100.4:6881": 2, "198.51.100.5:6881": 1,
	})
	if _, err := (&DomainStatService{}).Aggregate(); err != nil {
		t.Fatal(err)
	}
	for _, g := range []model.TrafficGranularity{model.GranularityHour, model.GranularityDay} {
		got := domainStatCounts(t, g)
		want := map[string]int64{"198.51.100.1": 5, "198.51.100.2": 4, "198.51.100.3": 3, domainStatFolded: 3}
		if len(got) != len(want) {
			t.Fatalf("粒度 %v 的行 = %v，期望 %v", g, got, want)
		}
		for d, c := range want {
			if got[d] != c {
				t.Errorf("粒度 %v 的 %q = %d，期望 %d（全部行 %v）", g, d, got[d], c, got)
			}
		}
	}
}

// 上限只挡新目标：已经有行的目标继续累加，哪怕桶已经满了。
func TestAggregateCapDoesNotEvictExistingTargets(t *testing.T) {
	setupDomainStatTest(t)
	shrinkDomainStatCaps(t)
	in := mkTrafficInbound(t, 31102, "甲")
	at := time.Date(2026, 11, 9, 3, 10, 0, 0, time.UTC)
	svc := DomainStatService{}

	storeVisits(t, in.Tag, at, map[string]int{"198.51.100.1:443": 1, "198.51.100.2:443": 1, "198.51.100.3:443": 1})
	if _, err := svc.Aggregate(); err != nil {
		t.Fatal(err)
	}
	storeVisits(t, in.Tag, at.Add(time.Minute), map[string]int{"198.51.100.1:443": 2, "198.51.100.9:443": 50})
	if _, err := svc.Aggregate(); err != nil {
		t.Fatal(err)
	}
	got := domainStatCounts(t, model.GranularityHour)
	if got["198.51.100.1"] != 3 || got["198.51.100.9"] != 0 || got[domainStatFolded] != 50 {
		t.Fatalf("小时行 = %v，期望已有的 198.51.100.1 累加到 3、桶满之后的新目标并进合并行", got)
	}
}

// 两类各算各的：IP 洪水挤不掉域名。
func TestAggregateIPFloodDoesNotCrowdOutDomains(t *testing.T) {
	setupDomainStatTest(t)
	shrinkDomainStatCaps(t)
	in := mkTrafficInbound(t, 31103, "甲")
	at := time.Date(2026, 11, 9, 3, 10, 0, 0, time.UTC)
	visits := map[string]int{"www.example.com:443": 1, "cdn.example.net:443": 1}
	for i := 1; i <= 10; i++ {
		visits[fmt.Sprintf("203.0.113.%d:6881", i)] = 5
	}
	storeVisits(t, in.Tag, at, visits)
	if _, err := (&DomainStatService{}).Aggregate(); err != nil {
		t.Fatal(err)
	}
	got := domainStatCounts(t, model.GranularityHour)
	if got["example.com"] != 1 || got["example.net"] != 1 {
		t.Fatalf("小时行 = %v，域名被 IP 挤掉了", got)
	}
}

// 榜单不列合并行：它不是一个目标，点进去按子串查访问日志也查不出任何东西。
func TestTopDomainsHidesFoldedRow(t *testing.T) {
	setupDomainStatTest(t)
	shrinkDomainStatCaps(t)
	in := mkTrafficInbound(t, 31104, "甲")
	now := time.Now()
	visits := map[string]int{}
	for i := 1; i <= 6; i++ {
		visits[fmt.Sprintf("203.0.113.%d:6881", i)] = 10 * i
	}
	storeVisits(t, in.Tag, now, visits)
	if _, err := (&DomainStatService{}).Aggregate(); err != nil {
		t.Fatal(err)
	}
	res, err := (&DomainStatService{}).TopDomains(in.Id, TopRange1h, TopOrderCount, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.List) != 3 {
		t.Fatalf("榜单 = %+v，期望只列单独成行的 3 个目标", res.List)
	}
	for _, r := range res.List {
		if r.Domain == domainStatFolded {
			t.Fatalf("榜单列出了合并行: %+v", res.List)
		}
	}
}

// 合并行可能是某个桶里次数最多的一行，计量池绝不能选中它（domain 条件为空等于不限制）。
func TestMeterCandidatesSkipFoldedRow(t *testing.T) {
	aggs := []meterAgg{{Domain: domainStatFolded, Count: 100000}, {Domain: "example.com", Count: 10}}
	for _, c := range buildMeterCandidates(aggs, nil, nil, nil, nil, time.Now()) {
		if c.Domain == domainStatFolded {
			t.Fatalf("合并行进了计量候选: %+v", c)
		}
	}
}
