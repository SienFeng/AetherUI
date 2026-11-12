package service

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/geodat"
	"a-ui/xray"
)

// reset 清空缓存与计数并停掉计时器（只在测试里用）。
func (c *smallMatcherCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep.stop()
	c.entries = map[[sha256.Size]byte]*smallMatcherEntry{}
	c.builds = 0
}

// resetSmallMatchers 清空全进程共用的小规则匹配器缓存。
func resetSmallMatchers(t *testing.T) {
	t.Helper()
	smallMatchers.reset()
	t.Cleanup(smallMatchers.reset)
}

// setSmallMatcherIdleTTL 在缓存的锁里改闲置上限（计时器回调在锁里读它），测试结束改回。
func setSmallMatcherIdleTTL(t *testing.T, d time.Duration) {
	t.Helper()
	smallMatchers.mu.Lock()
	old := smallMatcherIdleTTL
	smallMatcherIdleTTL = d
	smallMatchers.mu.Unlock()
	t.Cleanup(func() {
		smallMatchers.mu.Lock()
		smallMatcherIdleTTL = old
		smallMatchers.mu.Unlock()
	})
}

// smallMatcherStats 读出缓存里的条目数与累计构建次数。
func smallMatcherStats() (entries, builds int) {
	smallMatchers.mu.Lock()
	defer smallMatchers.mu.Unlock()
	return len(smallMatchers.entries), smallMatchers.builds
}

// smallCached 读出缓存里这组条目的匹配器（不算一次使用）；没有返回 nil。
func smallCached(domains, cidrs []string) *groupMatcher {
	smallMatchers.mu.Lock()
	defer smallMatchers.mu.Unlock()
	if e := smallMatchers.entries[groupMatcherKey(domains, cidrs)]; e != nil {
		return e.m
	}
	return nil
}

// setRuleGroupCidrs 把规则引用的第一个组的手工 IP 段改成 cidrs。
func setRuleGroupCidrs(t *testing.T, rule *model.RoutingRule, cidrs string) {
	t.Helper()
	gids, err := DecodeDomainGroupIds(rule.DomainGroupIds)
	if err != nil || len(gids) == 0 {
		t.Fatalf("规则 %d 没有域名组: %v", rule.Id, err)
	}
	if err := database.GetDB().Model(&model.DomainGroup{}).Where("id = ?", gids[0]).Update("cidrs", cidrs).Error; err != nil {
		t.Fatal(err)
	}
}

// testDatWriter 返回往 bin/ 下写测试 dat 的函数：每次写完显式推进修改时间（不依赖文件系统的
// 时间精度），测试结束删掉写过的文件。
func testDatWriter(t *testing.T) func(name string, data []byte) {
	t.Helper()
	xray.EnsureAssetLocation()
	stamp := time.Now().Add(-time.Hour)
	written := map[string]bool{}
	t.Cleanup(func() {
		for path := range written {
			_ = os.Remove(path)
		}
	})
	return func(name string, data []byte) {
		t.Helper()
		path := filepath.Join("bin", name)
		written[path] = true
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		stamp = stamp.Add(time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

// testGeoIPDat 编码一个只含一个类别的 geoip dat。
func testGeoIPDat(t *testing.T, code string, cidrs ...string) []byte {
	t.Helper()
	data, err := geodat.Encode([]geodat.Entry{{Tag: code, CIDRs: cidrs}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 小规则的匹配器跨轮复用：后台审计每 10 分钟一轮，改动前每轮都把每条小规则的匹配器重建一遍，
// xray 建域名匹配器强制 GC 3~4 次、建 IP 段集合再 2 次；堆里常驻着大匹配器时，生产机上一轮
// 审计因此满核 3~4 秒。两轮之间强制 GC 过也不重建，用的是同一个。
func TestSmallMatcherReusedAcrossVerifyRounds(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "keyword:gpt"}, nil)
	setRuleGroupCidrs(t, f.rule, `["192.0.2.0/24"]`)
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	vs := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	domains, cidrs := []string{"domain:openai.com", "keyword:gpt"}, []string{"192.0.2.0/24"}

	for round := 1; round <= 2; round++ {
		all, _, err := vs.VerifyAll()
		if err != nil {
			t.Fatalf("第 %d 轮 VerifyAll: %v", round, err)
		}
		if res := all[f.rule.Id]; res == nil || len(res.Users) == 0 || res.Users[0].Log == nil || res.Users[0].Log.Error != "" {
			t.Fatalf("第 %d 轮应带上访问日志证据: %+v", round, res)
		}
		runtime.GC()
	}
	if smallCached(domains, cidrs) == nil {
		t.Fatal("小规则的匹配器应留在缓存里")
	}
	if _, n := smallMatcherStats(); n != 1 {
		t.Errorf("两轮审计累计构建 %d 次，期望 1", n)
	}
}

// 异常扫描的计划与审计用的是同一个实例：常驻内存只算一份（计划本来就按代次常驻着它）。
// 先审计后扫描：审计建的那一个要进缓存，计划重建时拿到的就是它。
func TestSmallMatcherSharedWithAnomalyPlans(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "keyword:gpt"}, nil)
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	vs := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	if _, _, err := vs.VerifyAll(); err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
	cached := smallCached([]string{"domain:openai.com", "keyword:gpt"}, nil)
	if entries, n := smallMatcherStats(); cached == nil || entries != 1 || n != 1 {
		t.Fatalf("审计的匹配器应取自缓存：缓存 %d 条、构建 %d 次", entries, n)
	}
	mustScan(t, f.svc, anomalyNow, false)
	anomalyPlanMu.Lock()
	var planned *groupMatcher
	for _, p := range anomalyPlans.rules {
		if p.ruleId == f.rule.Id {
			planned = p.matcher
		}
	}
	anomalyPlanMu.Unlock()
	if planned != cached {
		t.Error("异常扫描的计划应拿到审计建的同一个匹配器")
	}
	if _, n := smallMatcherStats(); n != 1 {
		t.Errorf("累计构建 %d 次，期望 1", n)
	}
}

// 条目变了（改组、订阅刷新）按新条目另建；旧条目的匹配器仍按旧条目判定，再用旧条目时直接复用。
func TestSmallMatcherRebuiltWhenEntriesChange(t *testing.T) {
	resetSmallMatchers(t)
	a := smallMatchers.get([]string{"domain:a.example"}, nil)
	b := smallMatchers.get([]string{"domain:b.example"}, nil)
	if a.err != nil || b.err != nil {
		t.Fatalf("get: %v / %v", a.err, b.err)
	}
	if a.m == b.m || !b.m.Match("x.b.example:443") || b.m.Match("x.a.example:443") || !a.m.Match("x.a.example:443") {
		t.Fatal("不同条目应是各自的匹配器，按各自的条目判定")
	}
	if again := smallMatchers.get([]string{"domain:a.example"}, nil); again.m != a.m {
		t.Error("同样的条目应复用")
	}
	if _, n := smallMatcherStats(); n != 2 {
		t.Errorf("累计构建 %d 次，期望 2", n)
	}
}

// 条目引用的 dat 文件换了内容（geo 数据更新）：重建并按新内容判定。引用同一个文件的其它条目
// 一起作废——留着的话它们攥着旧的 IP 集合，xray 按「文件:代码」的缓存会把旧集合交给重建。
func TestSmallMatcherRebuiltWhenDatChanges(t *testing.T) {
	resetSmallMatchers(t)
	write := testDatWriter(t)
	siteName, ipName := "a-ui-test-smallcache-site.dat", "a-ui-test-smallcache-ip.dat"
	writeIP := func(cidr string) { write(ipName, testGeoIPDat(t, "SMALLCACHE", cidr)) }
	domains := []string{"ext:" + siteName + ":smallcache", "domain:pad.example"}
	cidrs := []string{"ext:" + ipName + ":smallcache"}
	siblingCidrs := []string{"ext:" + ipName + ":smallcache", "203.0.113.0/24"}

	write(siteName, encodeTestGeoSite("smallcache", "old.example"))
	writeIP("192.0.2.0/24")
	func() {
		e := smallMatchers.get(domains, cidrs)
		s := smallMatchers.get(nil, siblingCidrs)
		if e.err != nil || s.err != nil {
			t.Fatalf("get: %v / %v", e.err, s.err)
		}
		if !e.m.Match("old.example:443") || !e.m.Match("192.0.2.7:443") || !s.m.Match("192.0.2.7:443") {
			t.Fatal("前提：应命中旧内容")
		}
	}()

	write(siteName, encodeTestGeoSite("smallcache", "new-content.example"))
	writeIP("198.51.100.0/24")
	e2 := smallMatchers.get(domains, cidrs)
	if e2.err != nil {
		t.Fatalf("get: %v", e2.err)
	}
	if e2.m.Match("old.example:443") || !e2.m.Match("new-content.example:443") {
		t.Error("geosite dat 换了内容，匹配器仍是旧的")
	}
	if e2.m.Match("192.0.2.7:443") || !e2.m.Match("198.51.100.7:443") {
		t.Error("geoip dat 换了内容，匹配器仍是旧的")
	}
	if smallCached(nil, siblingCidrs) != nil {
		t.Error("引用同一个 dat 的其它条目应一起作废")
	}
	s2 := smallMatchers.get(nil, siblingCidrs)
	if s2.err != nil || s2.m.Match("192.0.2.7:443") || !s2.m.Match("198.51.100.7:443") {
		t.Errorf("另一个条目重建后也应按新内容判定: %v", s2.err)
	}
	if _, n := smallMatcherStats(); n != 4 {
		t.Errorf("累计构建 %d 次，期望 4", n)
	}
}

// dat 换了之后第一次构建的恰好是一组新条目（改了组、订阅刷新）、引用着同一个类别：旧条目还攥着
// 旧集合，xray 按「文件:类别」的弱缓存会把它交给新条目，而新条目按新的文件状态记下、再也不重建。
// 所以每次构建之前都要先作废引用的 dat 已经变了的条目（再强制 GC 一次）。
func TestSmallMatcherNewEntriesSeeChangedDat(t *testing.T) {
	resetSmallMatchers(t)
	write := testDatWriter(t)
	name := "a-ui-test-smallcache-new.dat"
	old := []string{"ext:" + name + ":smallnew"}
	fresh := []string{"ext:" + name + ":smallnew", "203.0.113.0/24"}

	write(name, testGeoIPDat(t, "SMALLNEW", "192.0.2.0/24"))
	func() {
		e := smallMatchers.get(nil, old)
		if e.err != nil || !e.m.Match("192.0.2.7:443") {
			t.Fatalf("前提：应命中旧内容: %v", e.err)
		}
	}()

	write(name, testGeoIPDat(t, "SMALLNEW", "198.51.100.0/24"))
	n := smallMatchers.get(nil, fresh)
	if n.err != nil || n.m.Match("192.0.2.7:443") || !n.m.Match("198.51.100.7:443") {
		t.Errorf("引用同一个类别的新条目拿到了旧内容: %v", n.err)
	}
	if smallCached(nil, old) != nil {
		t.Error("引用的 dat 变了的旧条目应在这次构建之前作废")
	}
	if o := smallMatchers.get(nil, old); o.err != nil || o.m.Match("192.0.2.7:443") || !o.m.Match("198.51.100.7:443") {
		t.Errorf("旧条目重建后也应按新内容判定: %v", o.err)
	}
}

// 闲置超过 smallMatcherIdleTTL 的条目由计时器放掉（规则删了、改了条目），之后再用就重建；
// 一直在用的不受影响，等它不用了，计时器也会按时再醒来放掉它。
func TestSmallMatcherExpiresWhenIdle(t *testing.T) {
	resetSmallMatchers(t)
	setSmallMatcherIdleTTL(t, 500*time.Millisecond)
	idle, busy := []string{"domain:idle.example"}, []string{"domain:busy.example"}
	first := smallMatchers.get(idle, nil).m
	kept := smallMatchers.get(busy, nil).m
	if smallCached(idle, nil) != first || smallCached(busy, nil) != kept {
		t.Fatal("前提：建好之后应留在缓存里")
	}
	deadline := time.Now().Add(5 * time.Second)
	for smallCached(idle, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("闲置超时之后缓存仍未放掉")
		}
		if got := smallMatchers.get(busy, nil).m; got != kept {
			t.Fatal("一直在用的条目被放掉了")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if smallCached(busy, nil) != kept {
		t.Fatal("一直在用的条目跟着闲置的一起被放掉了")
	}
	// busy 不再用了：计时器上一次醒来时它还在用、改了下次醒来的时刻，现在要按时放掉它
	// （期间没有任何构建替它重新排期）。
	for smallCached(busy, nil) != nil {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("停用之后计时器没有再醒来放掉它")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if again := smallMatchers.get(idle, nil).m; again == nil || again == first {
		t.Error("放掉之后再用应重建")
	}
}

// 建不出来不缓存，下次照样再试：dat 补上之后就建得出来。
func TestSmallMatcherBuildErrorNotCached(t *testing.T) {
	resetSmallMatchers(t)
	xray.EnsureAssetLocation()
	name := "a-ui-test-smallcache-missing.dat"
	path := filepath.Join("bin", name)
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	domains := []string{"ext:" + name + ":missing"}
	if e := smallMatchers.get(domains, nil); e.err == nil {
		t.Fatal("dat 不存在时应建不出来")
	}
	if entries, n := smallMatcherStats(); entries != 0 || n != 0 {
		t.Fatalf("失败的构建不该进缓存：%d 条、构建 %d 次", entries, n)
	}
	if err := os.WriteFile(path, encodeTestGeoSite("missing", "found.example"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := smallMatchers.get(domains, nil)
	if e.err != nil || !e.m.Match("found.example:443") {
		t.Fatalf("dat 补上之后应建得出来: %v", e.err)
	}
	if again := smallMatchers.get(domains, nil); again.m != e.m {
		t.Error("建出来之后应缓存")
	}
}

// 审计与异常扫描在各自的协程里同时取同一组条目：只建一次。
func TestSmallMatcherConcurrentGetBuildsOnce(t *testing.T) {
	resetSmallMatchers(t)
	domains := []string{"domain:concurrent.example", "keyword:conc"}
	var wg sync.WaitGroup
	got := make([]*groupMatcher, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = smallMatchers.get(domains, nil).m
		}(i)
	}
	wg.Wait()
	for i, m := range got {
		if m == nil || m != got[0] {
			t.Fatalf("第 %d 个协程拿到的匹配器不同", i)
		}
	}
	if _, n := smallMatcherStats(); n != 1 {
		t.Errorf("累计构建 %d 次，期望 1", n)
	}
}

// 生产机的形状：一条常驻的大规则、几条带 IP 段与关键词的小规则、配了地区限制的入站、
// 被排在前面的规则抢先命中的连接。预热之后，一整轮审计、异常扫描（含代次推进后的计划重建）
// 都不该再触发任何强制 GC——每一次强制 GC 都要把常驻的大匹配器扫一遍，生产机上约 50 毫秒。
func TestWarmAuditRoundForcesNoGC(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "keyword:gpt"}, nil)
	auditLargeThreshold = 5
	setRuleGroupCidrs(t, f.rule, `["192.0.2.0/24"]`)

	// 地区限制：入站 A 只放行 198.51.100.0/24。
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	if err := database.GetDB().Model(f.a).Update("regions", regions).Error; err != nil {
		t.Fatal(err)
	}
	// 排在 f.rule 之后、期望另一个出站的规则：A 的连接被 f.rule 抢先送去 a-ui-us，要做归因。
	hk := &model.OutboundNode{
		Tag: "a-ui-hk", Remark: "香港节点", Protocol: "socks", Enable: true,
		Config: `{"protocol":"socks","settings":{"servers":[{"address":"5.6.7.8","port":1080}]}}`,
	}
	if err := database.GetDB().Save(hk).Error; err != nil {
		t.Fatal(err)
	}
	general := newTestGroupWithDomains(t, "General", []string{"domain:openai.com"})
	later := &model.RoutingRule{
		Remark: "general", InboundIds: mustEncodeIds(t, []int{f.a.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{general.Id}),
		Action:         model.ActionProxy, OutboundId: hk.Id, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(later); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	// 大规则：条目数超过门槛，匹配器取自常驻槽。
	bigDomains := []string{}
	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		bigDomains = append(bigDomains, "domain:big-"+d+".example")
	}
	big := newTestGroupWithDomains(t, "Big", bigDomains)
	bigRule := &model.RoutingRule{
		Remark: "big", InboundIds: mustEncodeIds(t, []int{f.b.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{big.Id}),
		Action:         model.ActionProxy, OutboundId: f.node.Id, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(bigRule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	at := anomalyNow.Add(-time.Hour)
	insertLogs(t, 2, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443",
		Route: f.node.Tag, SourceIP: "198.51.100.77", Accepted: true})
	insertLogs(t, 3, model.AccessLog{Time: at.UnixMilli(), InboundId: f.a.Id, Target: "api.openai.com:443",
		Route: model.BlockOutboundTag, SourceIP: "203.0.113.81", Accepted: true})
	insertLogs(t, 2, model.AccessLog{Time: at.UnixMilli(), InboundId: f.b.Id, Target: "x.big-c.example:443",
		Route: model.DefaultOutboundTag, SourceIP: "198.51.100.9", Accepted: true})

	vs := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	round := func(now time.Time) map[int]*RuleVerifyResult {
		t.Helper()
		all, _, err := vs.VerifyAll()
		if err != nil {
			t.Fatalf("VerifyAll: %v", err)
		}
		mustScan(t, f.svc, now, false)
		mustScan(t, f.svc, now, true)
		return all
	}
	round(anomalyNow) // 预热：每个匹配器第一次照常构建

	xrayApplyGen.Inc() // 代次推进：异常扫描的计划要重建
	var all map[int]*RuleVerifyResult
	if n := forcedGCs(t, func() { all = round(anomalyNow.Add(time.Minute)) }); n != 0 {
		t.Errorf("预热之后一整轮审计与扫描又强制 GC 了 %d 次", n)
	}

	// 确认这一轮确实走过了地区判定、抢先命中归因与常驻槽，没有因为别的原因提前返回。
	if u := findUser(t, all[f.rule.Id], f.a.Id); u.Log == nil || u.Log.GeoBlocked != 3 {
		t.Errorf("应把被地区规则拦下的连接单独计数: %+v", u.Log)
	}
	u := findUser(t, all[later.Id], f.a.Id)
	if u.Log == nil || len(u.Log.Others) != 1 || u.Log.Others[0].ShadowedBy == nil || u.Log.Others[0].ShadowedBy.RuleId != f.rule.Id {
		t.Errorf("被 f.rule 抢先命中的连接应归因到它: %+v", u.Log)
	}
	if _, builds, users := largeSlot(); builds != 1 || users != 0 {
		t.Errorf("大匹配器累计构建 %d 次、占用 %d，期望 1 次、无人占用", builds, users)
	}
	if _, n := smallMatcherStats(); n != 2 {
		t.Errorf("小规则匹配器累计构建 %d 次，期望 2（两种条目各一次）", n)
	}
	if _, n := geoSourceStats(); n != 1 {
		t.Errorf("地区匹配器累计构建 %d 次，期望 1", n)
	}
}
