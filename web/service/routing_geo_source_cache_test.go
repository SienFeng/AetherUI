package service

import (
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"a-ui/database/model"
)

// reset 清空缓存与计数并停掉计时器（只在测试里用）。早先触发、还在等锁的计时器回调
// 看到空表就直接返回。
func (c *geoSourceCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep.stop()
	c.entries = map[string]*geoSourceEntry{}
	c.v6, c.v6Key, c.builds = nil, "", 0
}

// resetGeoSourceCache 清空全进程共用的地区匹配器缓存：一个测试留下的常驻匹配器会串到
// 下一个测试的构建计数里。
func resetGeoSourceCache(t *testing.T) {
	t.Helper()
	geoSources.reset()
	t.Cleanup(geoSources.reset)
}

// setGeoSourceIdleTTL 在缓存的锁里改闲置上限（计时器回调在锁里读它），测试结束改回。
func setGeoSourceIdleTTL(t *testing.T, d time.Duration) {
	t.Helper()
	geoSources.mu.Lock()
	old := geoSourceIdleTTL
	geoSourceIdleTTL = d
	geoSources.mu.Unlock()
	t.Cleanup(func() {
		geoSources.mu.Lock()
		geoSourceIdleTTL = old
		geoSources.mu.Unlock()
	})
}

// geoSourceStats 读出缓存里的条目数与累计构建次数。
func geoSourceStats() (entries, builds int) {
	geoSources.mu.Lock()
	defer geoSources.mu.Unlock()
	return len(geoSources.entries), geoSources.builds
}

// forcedGCs 在关掉自动 GC 的情况下运行 fn，返回期间发生的 GC 次数。GOGC 关掉之后
// 只有显式的 runtime.GC() 会触发回收（xray 建匹配器时那些 "peak mem" 的强制 GC），
// 计数与机器快慢、堆大小无关。
func forcedGCs(t *testing.T, fn func()) uint32 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.NumGC - before.NumGC
}

// 地区匹配器按 dat 代码在全进程共用一份：自动出差放行（每分钟）、并发判定（每 30 秒）、
// 审计与异常扫描、「需要留意」各自的构造入口拿到的是同一个，同一代码的两个入站也是同一个。
// 改动前每次构造都重建，xray 建带 IP 段的匹配器每次强制 GC 两次，堆里常驻着大匹配器时
// 生产机上每次重建约 0.8 核·秒，每分钟三次。
func TestGeoSourceRulesSharedAcrossBuilds(t *testing.T) {
	resetGeoSourceCache(t)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	inbounds := []*model.Inbound{
		{Id: 4, Enable: true, Regions: regions},
		{Id: 7, Enable: true, Regions: regions},
	}
	r := newGeoBlockMatcher(inbounds).byInbound[4]
	if r == nil {
		t.Fatal("前提：入站 4 应有地区匹配器")
	}
	builds := map[string]*geoBlockMatcher{
		"newGeoBlockMatcher（审计、异常扫描、需要留意）": newGeoBlockMatcher(inbounds),
		"newLiveGeoBlockMatcher（并发判定）":     newLiveGeoBlockMatcher(inbounds, nil),
		"newGeoBlockMatcherWith（自动出差放行）":   newGeoBlockMatcherWith(inbounds, nil, allowedSetFor),
	}
	for name, g := range builds {
		if g.byInbound[4] != r || g.byInbound[7] != r {
			t.Errorf("%s 重建了同一个代码的地区匹配器", name)
		}
		if g.Blocks(4, "198.51.100.77") || !g.Blocks(4, "203.0.113.81") || !g.Blocks(7, "2408:8000::1") {
			t.Errorf("%s 的判定不对", name)
		}
	}
	if entries, n := geoSourceStats(); entries != 1 || n != 1 {
		t.Errorf("缓存 %d 条、累计构建 %d 次，期望各 1", entries, n)
	}
}

// 预热之后反复构造地区匹配器（三个入口都算）不再触发任何强制 GC。
func TestRegionMatcherWarmBuildsForceNoGC(t *testing.T) {
	resetGeoSourceCache(t)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	inbounds := []*model.Inbound{{Id: 4, Enable: true, Regions: regions}}
	newGeoBlockMatcher(inbounds) // 第一次照常构建
	n := forcedGCs(t, func() {
		for i := 0; i < 10; i++ {
			newGeoBlockMatcher(inbounds)
			newLiveGeoBlockMatcher(inbounds, nil)
			newGeoBlockMatcherWith(inbounds, nil, allowedSetFor)
		}
	})
	if n != 0 {
		t.Errorf("预热之后 30 次构造又强制 GC 了 %d 次", n)
	}
}

// 升级前的 dat 只有 tag、不按内容寻址：同一个代码的内容可能变，不能缓存，每次照旧构建；
// 拒绝 IPv6 的那一半与代码无关，照样共用。
func TestGeoSourceRulesLegacyCodeNotCached(t *testing.T) {
	resetGeoSourceCache(t)
	writeTestGeoDat(t, []string{"192.0.2.0/24"}) // 建好 bin 目录并登记清理
	provinces := []string{"北京市"}
	allow := []string{"198.51.100.0/24"}
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: regionTag(provinces), CIDRs: allow}}); err != nil {
		t.Fatalf("writeGeoDat: %v", err)
	}
	regions, err := EncodeRegions(provinces)
	if err != nil {
		t.Fatal(err)
	}
	inbounds := []*model.Inbound{{Id: 4, Enable: true, Regions: regions}}
	g1, g2 := newGeoBlockMatcher(inbounds), newGeoBlockMatcher(inbounds)
	r1, r2 := g1.byInbound[4], g2.byInbound[4]
	if r1 == nil || r2 == nil {
		t.Fatal("前提：旧格式的代码也应建得出匹配器")
	}
	if r1 == r2 {
		t.Error("不按内容寻址的代码不能缓存")
	}
	if r1.v6 != r2.v6 {
		t.Error("拒绝 IPv6 来源的匹配器与代码无关，应共用")
	}
	if g2.Blocks(4, "198.51.100.77") || !g2.Blocks(4, "203.0.113.81") || !g2.Blocks(4, "2408:8000::1") {
		t.Error("旧格式代码的判定不对")
	}
	if entries, n := geoSourceStats(); entries != 0 || n != 2 {
		t.Errorf("缓存 %d 条、累计构建 %d 次，期望 0 条、2 次", entries, n)
	}
}

// 允许集一变（IP 库更新、改地区）dat 代码就变：新代码另建，按新内容判定。缓存按完整的代码认，
// 不能按省份集合的 tag 认——那样 IP 库更新后会一直拿着旧允许集。
func TestGeoSourceRulesFollowNewCode(t *testing.T) {
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	resetGeoSourceCache(t)
	inbounds := []*model.Inbound{{Id: 4, Enable: true, Regions: regions}}
	g1 := newGeoBlockMatcher(inbounds)
	// 直接改写 dat（writeTestGeoDat 会清缓存）：旧代码的匹配器还留在缓存里。
	allow := []string{"192.0.2.0/24"}
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: geoDatCode(regionTag([]string{"北京市"}), allow), CIDRs: allow}}); err != nil {
		t.Fatalf("writeGeoDat: %v", err)
	}
	g2 := newGeoBlockMatcher(inbounds)
	if g1.byInbound[4] == nil || g2.byInbound[4] == nil || g1.byInbound[4] == g2.byInbound[4] {
		t.Fatal("内容变了应换成新代码的匹配器")
	}
	if !g2.Blocks(4, "198.51.100.77") || g2.Blocks(4, "192.0.2.7") {
		t.Error("应按新的允许集判定")
	}
	if g1.Blocks(4, "198.51.100.77") {
		t.Error("旧代码的匹配器仍按旧内容判定")
	}
	if entries, n := geoSourceStats(); entries != 2 || n != 2 {
		t.Errorf("缓存 %d 条、累计构建 %d 次，期望新旧两个代码各一条、各建一次", entries, n)
	}
}

// 闲置超过 geoSourceIdleTTL 的条目由计时器放掉（地区限制关掉之后不再有人来取），
// 之后再取就重建。
func TestGeoSourceRulesExpireWhenIdle(t *testing.T) {
	resetGeoSourceCache(t)
	setGeoSourceIdleTTL(t, 200*time.Millisecond)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	inbounds := []*model.Inbound{{Id: 4, Enable: true, Regions: regions}}
	r1 := newGeoBlockMatcher(inbounds).byInbound[4]
	if entries, _ := geoSourceStats(); entries != 1 {
		t.Fatalf("前提：建好之后应留在缓存里，现有 %d 条", entries)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if entries, _ := geoSourceStats(); entries == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("闲置超时之后缓存仍未放掉")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r2 := newGeoBlockMatcher(inbounds).byInbound[4]
	if r2 == nil || r2 == r1 {
		t.Error("放掉之后再取应重建")
	}
	if _, n := geoSourceStats(); n != 2 {
		t.Errorf("累计构建 %d 次，期望 2", n)
	}
}

// 正在用的条目不会被放掉：计时器醒来时发现它还在用，就改到它闲置满上限时再醒；之后不用了，
// 不需要再有新的构建，它也会按时放掉。
func TestGeoSourceRulesKeepEntriesInUse(t *testing.T) {
	resetGeoSourceCache(t)
	setGeoSourceIdleTTL(t, 500*time.Millisecond)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	inbounds := []*model.Inbound{{Id: 4, Enable: true, Regions: regions}}
	r := newGeoBlockMatcher(inbounds).byInbound[4]
	// 每 25 毫秒用一次，总共跨过 3 个闲置上限。
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if got := newGeoBlockMatcher(inbounds).byInbound[4]; got != r {
			t.Fatal("一直在用的条目被放掉了")
		}
	}
	if _, n := geoSourceStats(); n != 1 {
		t.Errorf("累计构建 %d 次，期望 1", n)
	}
	// 不再用了：等计时器自己醒来放掉（期间没有任何构建替它重新排期）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entries, _ := geoSourceStats(); entries == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("停用之后计时器没有再醒来放掉它")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// 建不出来（dat 里没有这个代码）不缓存，下次照样再试，dat 补上之后就建得出来。
func TestGeoSourceRulesErrorNotCached(t *testing.T) {
	resetGeoSourceCache(t)
	writeTestGeoDat(t, []string{"192.0.2.0/24"})
	const code = "ALLOWDEADBEEFC0BADC0DE" // 只在本测试里用：别的测试按真实内容算代码
	if _, err := geoSources.rulesFor(code); err == nil {
		t.Fatal("dat 里没有的代码应建不出来")
	}
	if entries, n := geoSourceStats(); entries != 0 || n != 0 {
		t.Fatalf("失败的构建不该进缓存：%d 条、构建 %d 次", entries, n)
	}
	if _, err := writeGeoDat(geoDatPath, []geodatEntry{{Tag: code, CIDRs: []string{"198.51.100.0/24"}}}); err != nil {
		t.Fatalf("writeGeoDat: %v", err)
	}
	r, err := geoSources.rulesFor(code)
	if err != nil || r == nil {
		t.Fatalf("dat 补上之后应建得出来: %v", err)
	}
	if again, _ := geoSources.rulesFor(code); again != r {
		t.Error("建出来之后应缓存")
	}
}

// 几个任务同时构造（自动出差、并发判定、审计各在自己的协程里）：只建一次，判定照常。
func TestGeoSourceRulesConcurrentUse(t *testing.T) {
	resetGeoSourceCache(t)
	regions := writeTestGeoDat(t, []string{"198.51.100.0/24"})
	inbounds := []*model.Inbound{{Id: 4, Enable: true, Regions: regions}}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				g := newGeoBlockMatcher(inbounds)
				if g.Blocks(4, "198.51.100.77") || !g.Blocks(4, "203.0.113.81") {
					errs <- "并发构造时判定不对"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if _, n := geoSourceStats(); n != 1 {
		t.Errorf("累计构建 %d 次，期望 1", n)
	}
}
