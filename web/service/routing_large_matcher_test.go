package service

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/geodat"
	"a-ui/xray"

	"github.com/xtls/xray-core/common/geodata"
)

// reset 清空槽与计数并停掉闲置计时器（只在测试里用，调用时不能有人还占着）。计时器对象
// 保留：早先触发、还在等锁的回调看到槽空就直接返回。
func (c *largeMatcherCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
	}
	c.dropLocked()
	c.users, c.builds, c.building = 0, 0, false
}

// resetLargeMatchers 清空全进程共用的大匹配器槽：一个测试留下的常驻匹配器会串到下一个
// 测试的构建计数与堆基线里。
func resetLargeMatchers(t *testing.T) {
	t.Helper()
	largeMatchers.reset()
	t.Cleanup(largeMatchers.reset)
}

// largeSlot 读出槽里的匹配器、累计构建次数与当前占用数。
func largeSlot() (m *groupMatcher, builds, users int) {
	largeMatchers.mu.Lock()
	defer largeMatchers.mu.Unlock()
	return largeMatchers.m, largeMatchers.builds, largeMatchers.users
}

// encodeTestGeoSite 手写一个只含一个类别的 geosite dat。字段号同 xray 的
// GeoSiteList{entry=1} / GeoSite{code=1, domain=2} / Domain{type=1, value=2}；
// 与 util/geodat 一样不引入 protobuf 运行时。
func encodeTestGeoSite(code string, domains ...string) []byte {
	field := func(b []byte, tag byte, v []byte) []byte {
		b = append(b, tag)
		b = binary.AppendUvarint(b, uint64(len(v)))
		return append(b, v...)
	}
	site := field(nil, 0x0a, []byte(strings.ToUpper(code)))
	for _, d := range domains {
		dm := []byte{0x08, byte(geodata.Domain_Domain)}
		dm = field(dm, 0x12, []byte(d))
		site = field(site, 0x12, dm)
	}
	return field(nil, 0x0a, site)
}

// 路由审计与异常扫描共用一个常驻的大匹配器：大规则扫描 → 全量验证 → 再扫描 → 单条验证，
// 每一步都确实从常驻槽取用过（槽的最近放手时刻前进了；绕过槽自己建的话不会前进），
// 每一步之后都强制 GC，整个过程槽只构建一次，且每一步结束后都没人还占着它。
// 改动前两条路径各建各的、用完即丢，xray 只用弱引用缓存，GC 之后再用就整套重建：
// 生产机上一个 11 万条的组每天重建两百多次，每次满核 3~6 秒。
func TestLargeMatcherSharedAcrossAuditAndScan(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	auditLargeThreshold = 2
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	vs := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	lastUsed := func() time.Time {
		largeMatchers.mu.Lock()
		defer largeMatchers.mu.Unlock()
		return largeMatchers.lastUsed
	}

	var first *groupMatcher
	step := func(name string, run func()) {
		t.Helper()
		before := lastUsed()
		run()
		if !lastUsed().After(before) {
			t.Fatalf("%s没有从常驻槽取用匹配器", name)
		}
		runtime.GC()
		m, builds, users := largeSlot()
		if m == nil || builds != 1 || users != 0 {
			t.Fatalf("%s之后：槽里 %p、累计构建 %d 次、占用 %d；期望常驻、只建 1 次、无人占用", name, m, builds, users)
		}
		if first == nil {
			first = m
		} else if m != first {
			t.Fatalf("%s换掉了常驻的匹配器", name)
		}
	}
	step("大规则扫描", func() { mustScan(t, f.svc, anomalyNow, true) })
	step("全量验证", func() {
		all, _, err := vs.VerifyAll()
		if err != nil {
			t.Fatalf("VerifyAll: %v", err)
		}
		res := all[f.rule.Id]
		if res == nil || len(res.Users) == 0 || res.Users[0].Log == nil || res.Users[0].Log.Error != "" {
			t.Fatalf("全量验证应带上访问日志证据: %+v", res)
		}
	})
	step("再次扫描", func() {
		insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-30*time.Minute)))
		mustScan(t, f.svc, anomalyNow.Add(time.Minute), true)
	})
	step("单条验证", func() {
		if _, err := vs.Verify(f.rule.Id); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(time.Minute)); got.Count != 2 {
		t.Errorf("两次扫描应各计 1 次异常: %+v", got)
	}
}

// 验证里的两处大匹配器（规则本身、抢先命中归因的候选）与异常扫描取的是槽里同一个，
// 各自放手后不留占用；verifyShared 收尾时也会放掉忘了放的。
func TestLargeMatcherVerifyPathsUseSharedSlot(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	auditLargeThreshold = 2
	s := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	sh, err := s.prepareShared(okRuntime())
	if err != nil {
		t.Fatalf("prepareShared: %v", err)
	}
	defer sh.close()

	plan, reason := s.planRule(sh, f.rule)
	if reason != "" || !plan.large {
		t.Fatalf("前提：应是可验证的大规则，reason=%q", reason)
	}
	e := sh.matcher(plan)
	slot, _, users := largeSlot()
	if e.err != nil || e.m == nil || e.m != slot || users != 1 {
		t.Fatalf("规则本身的大匹配器应取自常驻槽并占用它：err=%v 同一个=%v 占用=%d", e.err, e.m == slot, users)
	}
	sh.releaseLarge(plan)
	if _, _, users := largeSlot(); users != 0 {
		t.Fatalf("releaseLarge 之后应放手，占用=%d", users)
	}

	if m := sh.shadowMatcher(f.rule); m == nil || m != slot {
		t.Fatal("归因候选的大匹配器应取自同一个槽")
	}
	if _, _, users := largeSlot(); users != 1 {
		t.Fatalf("归因期间应占用槽，占用=%d", users)
	}
	sh.releaseShadowLarge()
	if _, _, users := largeSlot(); users != 0 {
		t.Fatalf("releaseShadowLarge 之后应放手，占用=%d", users)
	}

	plans, _, err := f.svc.anomalyPlanSet()
	if err != nil {
		t.Fatalf("anomalyPlanSet: %v", err)
	}
	var p *anomalyRulePlan
	for _, rp := range plans.rules {
		if rp.ruleId == f.rule.Id {
			p = rp
		}
	}
	if p == nil || !p.large {
		t.Fatalf("前提：异常扫描里应是大规则: %+v", p)
	}
	m, release := largeMatcherOf(p)
	if m != slot {
		t.Error("异常扫描的大匹配器应取自同一个槽")
	}
	release()

	sh2, err := s.prepareShared(okRuntime())
	if err != nil {
		t.Fatalf("prepareShared: %v", err)
	}
	plan2, _ := s.planRule(sh2, f.rule)
	_ = sh2.matcher(plan2)
	sh2.close()
	if _, builds, users := largeSlot(); builds != 1 || users != 0 {
		t.Fatalf("累计构建 %d 次、占用 %d；期望 1 次、收尾后无人占用", builds, users)
	}
}

// 组条目变了（订阅刷新、手工改组）就重建，并按新条目判定。
func TestLargeMatcherRebuiltWhenEntriesChange(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	auditLargeThreshold = 2
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	mustScan(t, f.svc, anomalyNow, true)
	first, _, _ := largeSlot()
	if first == nil {
		t.Fatal("前提：扫描后应有常驻匹配器")
	}

	gids, err := DecodeDomainGroupIds(f.rule.DomainGroupIds)
	if err != nil || len(gids) != 1 {
		t.Fatalf("decode group ids: %v %v", gids, err)
	}
	encoded, err := EncodeDomains([]string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai", "domain:anthropic.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.DomainGroup{}).Where("id = ?", gids[0]).Update("domains", encoded).Error; err != nil {
		t.Fatalf("update group: %v", err)
	}
	r := wrongRow(f.a.Id, anomalyNow.Add(-30*time.Minute))
	r.Target = "api.anthropic.com:443"
	insertLogs(t, 1, r)
	mustScan(t, f.svc, anomalyNow.Add(time.Minute), true)

	second, builds, users := largeSlot()
	if second == nil || second == first || builds != 2 || users != 0 {
		t.Fatalf("条目变了应重建：换了=%v 累计构建 %d 次 占用 %d", second != first, builds, users)
	}
	if got := f.svc.Anomaly(f.rule.Id, anomalyNow.Add(time.Minute)); got.Count != 2 {
		t.Errorf("新加的 anthropic.com 应按新条目计入: %+v", got)
	}
}

// 条目没变、dat 文件换了内容：xray 的匹配器缓存只按「文件:类别」认，旧匹配器还没被
// 回收时新建的会直接拿到旧内容（同 TestXrayIPSetCacheCannotServeStaleContent）。
// 常驻槽看到 dat 变了要重建，而且必须真的拿到新内容。
func TestLargeMatcherRebuiltWhenDatChanges(t *testing.T) {
	resetLargeMatchers(t)
	xray.EnsureAssetLocation()
	siteName, ipName := "a-ui-test-largecache-site.dat", "a-ui-test-largecache-ip.dat"
	sitePath, ipPath := filepath.Join("bin", siteName), filepath.Join("bin", ipName)
	t.Cleanup(func() {
		_ = os.Remove(sitePath)
		_ = os.Remove(ipPath)
	})
	stamp := time.Now().Add(-time.Hour)
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// 显式推进修改时间：不依赖文件系统的时间精度。
		stamp = stamp.Add(time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	writeIP := func(cidr string) {
		t.Helper()
		data, err := geodat.Encode([]geodat.Entry{{Tag: "LARGECACHE", CIDRs: []string{cidr}}})
		if err != nil {
			t.Fatal(err)
		}
		write(ipPath, data)
	}
	domains := []string{"ext:" + siteName + ":largecache", "domain:pad1.example", "domain:pad2.example"}
	cidrs := []string{"ext:" + ipName + ":largecache"}

	write(sitePath, encodeTestGeoSite("largecache", "old.example"))
	writeIP("192.0.2.0/24")
	e, release := largeMatchers.acquire(domains, cidrs)
	if e.err != nil {
		t.Fatalf("acquire: %v", e.err)
	}
	if !e.m.Match("old.example:443") || !e.m.Match("192.0.2.7:443") {
		t.Fatal("前提：应命中旧内容")
	}
	release()
	e = nil

	write(sitePath, encodeTestGeoSite("largecache", "new-content.example"))
	writeIP("198.51.100.0/24")
	e2, release2 := largeMatchers.acquire(domains, cidrs)
	defer release2()
	if e2.err != nil {
		t.Fatalf("acquire: %v", e2.err)
	}
	if e2.m.Match("old.example:443") || !e2.m.Match("new-content.example:443") {
		t.Error("geosite dat 换了内容，匹配器仍是旧的")
	}
	if e2.m.Match("192.0.2.7:443") || !e2.m.Match("198.51.100.7:443") {
		t.Error("geoip dat 换了内容，匹配器仍是旧的")
	}
	if _, builds, _ := largeSlot(); builds != 2 {
		t.Errorf("累计构建 %d 次，期望 2", builds)
	}
}

// 换槽要等正在用旧匹配器的调用方放手：同一时刻至多一个大匹配器存活，换槽前的 GC
// 也才放得掉旧的。同一组条目可以同时被多方使用。
func TestLargeMatcherSwapWaitsForUsers(t *testing.T) {
	resetLargeMatchers(t)
	a := []string{"domain:a1.example", "domain:a2.example", "domain:a3.example"}
	b := []string{"domain:b1.example", "domain:b2.example", "domain:b3.example"}
	e1, rel1 := largeMatchers.acquire(a, nil)
	e2, rel2 := largeMatchers.acquire(a, nil)
	if e1.err != nil || e2.err != nil || e1.m != e2.m {
		t.Fatal("同一组条目应共用同一个匹配器，且不必互相等待")
	}
	rel2()

	got := make(chan *matcherEntry, 1)
	go func() {
		e, rel := largeMatchers.acquire(b, nil)
		rel()
		got <- e
	}()
	select {
	case <-got:
		t.Fatal("旧匹配器还在用，不应换槽")
	case <-time.After(200 * time.Millisecond):
	}
	rel1()
	select {
	case e := <-got:
		if e.err != nil || !e.m.Match("b1.example:443") || e.m.Match("a1.example:443") {
			t.Fatalf("放手后应换成 b 的匹配器: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("放手后迟迟没有换槽")
	}
	rel1() // 重复放手无副作用
	if _, builds, users := largeSlot(); builds != 2 || users != 0 {
		t.Fatalf("累计构建 %d 次、占用 %d；期望 2 次、无人占用", builds, users)
	}
}

// 审计、扫描与换槽并发（生产上两个任务各在自己的 goroutine 里，-race 下跑）：不死锁、
// 结束后不留占用；扫描与换槽两方拿到的匹配器都对应自己要的条目，全量验证只查不出错。
// 审计确实走常驻槽由 TestLargeMatcherVerifyPathsUseSharedSlot 保证。
func TestLargeMatcherConcurrentAuditScanAndSwap(t *testing.T) {
	f := newAnomalyFixture(t, []string{"domain:openai.com", "domain:chatgpt.com", "full:x.ai"}, nil)
	auditLargeThreshold = 2
	insertLogs(t, 1, wrongRow(f.a.Id, anomalyNow.Add(-time.Hour)))
	vs := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	plans, _, err := f.svc.anomalyPlanSet()
	if err != nil {
		t.Fatalf("anomalyPlanSet: %v", err)
	}
	var p *anomalyRulePlan
	for _, rp := range plans.rules {
		if rp.ruleId == f.rule.Id && rp.large {
			p = rp
		}
	}
	if p == nil {
		t.Fatal("前提：应有大规则计划")
	}
	other := []string{"domain:o1.example", "domain:o2.example", "domain:o3.example"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range 6 {
			wg.Add(3)
			go func() {
				defer wg.Done()
				if _, _, err := vs.VerifyAll(); err != nil && !errors.Is(err, ErrVerifyBusy) {
					t.Errorf("VerifyAll: %v", err)
				}
			}()
			go func() {
				defer wg.Done()
				m, release := largeMatcherOf(p)
				defer release()
				if m == nil || !m.Match("api.openai.com:443") {
					t.Error("扫描拿到的匹配器不对")
				}
			}()
			go func() {
				defer wg.Done()
				e, release := largeMatchers.acquire(other, nil)
				defer release()
				if e.err != nil || !e.m.Match("o1.example:443") || e.m.Match("api.openai.com:443") {
					t.Error("换槽后拿到的匹配器不对")
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("并发取用卡住了")
	}
	if _, _, users := largeSlot(); users != 0 {
		t.Fatalf("结束后仍有 %d 个占用", users)
	}
}

// 两个任务同时要同一个大匹配器：只建一次，另一个等着用现成的。
func TestLargeMatcherConcurrentAcquireBuildsOnce(t *testing.T) {
	resetLargeMatchers(t)
	entries := []string{"domain:c1.example", "domain:c2.example", "domain:c3.example"}
	got := make([]*groupMatcher, 8)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, rel := largeMatchers.acquire(entries, nil)
			defer rel()
			got[i] = e.m
		}()
	}
	wg.Wait()
	for i, m := range got {
		if m == nil || m != got[0] {
			t.Fatalf("第 %d 个调用方拿到的不是同一个匹配器", i)
		}
	}
	if _, builds, users := largeSlot(); builds != 1 || users != 0 {
		t.Fatalf("累计构建 %d 次、占用 %d；期望 1 次、无人占用", builds, users)
	}
}

// 没人再用（大规则删了、访问日志关了）时常驻的匹配器闲置一段时间后自动放掉，
// 不一直占着几十 MB；还有人在用时不放——包括计时器是上一次放手留下、到点时恰好
// 又有人取用并一直占着的情形（审计在快到点时命中，跑样例跨过了到期时刻）。
func TestLargeMatcherReleasedWhenIdle(t *testing.T) {
	resetLargeMatchers(t)
	old := largeMatcherIdleTTL
	largeMatcherIdleTTL = 200 * time.Millisecond
	t.Cleanup(func() { largeMatcherIdleTTL = old })
	entries := []string{"domain:i1.example", "domain:i2.example", "domain:i3.example"}

	_, rel := largeMatchers.acquire(entries, nil)
	rel() // 放手即开始计时
	_, rel = largeMatchers.acquire(entries, nil)
	time.Sleep(600 * time.Millisecond) // 计时器在占用期间到点
	if m, _, _ := largeSlot(); m == nil {
		t.Fatal("还在用的时候不应释放")
	}
	rel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, _, _ := largeSlot(); m == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("闲置超时后应释放")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 只比较前后差值：慢机器上计时器可能在第一次放手与第二次取用之间就到点，多建过一次。
	_, before, _ := largeSlot()
	_, rel = largeMatchers.acquire(entries, nil)
	rel()
	if _, builds, _ := largeSlot(); builds != before+1 {
		t.Fatalf("释放后再用应重建一次：之前累计 %d 次，之后 %d 次", before, builds)
	}
}

// 构建中途崩溃（xray 内部 panic）时不能让槽一直处在「正在建」：panic 照常往上抛（由任务的
// Recover 记录），之后的调用方不被卡死。卡死的话审计与扫描两个任务从此都停在这里。
func TestLargeMatcherBuildPanicDoesNotWedge(t *testing.T) {
	resetLargeMatchers(t)
	orig := largeMatcherBuild
	t.Cleanup(func() { largeMatcherBuild = orig })
	entries := []string{"domain:p1.example", "domain:p2.example", "domain:p3.example"}

	largeMatcherBuild = func([]string, []string) (*groupMatcher, []datStamp, error) { panic("构建崩溃") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("构建崩溃应照常往上抛")
			}
		}()
		largeMatchers.acquire(entries, nil)
	}()
	largeMatcherBuild = orig

	got := make(chan *matcherEntry, 1)
	go func() {
		e, rel := largeMatchers.acquire(entries, nil)
		rel()
		got <- e
	}()
	select {
	case e := <-got:
		if e.err != nil || e.m == nil || !e.m.Match("p1.example:443") {
			t.Fatalf("崩溃之后应能正常构建: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("构建崩溃之后的调用方被卡住了")
	}
}

// 建不出来（引用了不存在的 dat）时返回错误、不占槽，也不卡住后面的调用方。
func TestLargeMatcherBuildErrorNotCached(t *testing.T) {
	resetLargeMatchers(t)
	bad := []string{"ext:a-ui-test-missing.dat:nope", "domain:e1.example", "domain:e2.example"}
	e, rel := largeMatchers.acquire(bad, nil)
	rel()
	if e.err == nil || e.m != nil {
		t.Fatalf("引用不存在的 dat 应返回错误: %+v", e)
	}
	if m, builds, users := largeSlot(); m != nil || builds != 0 || users != 0 {
		t.Fatalf("失败不应占槽：槽 %p 构建 %d 占用 %d", m, builds, users)
	}
	good := []string{"domain:e1.example", "domain:e2.example", "domain:e3.example"}
	e, rel = largeMatchers.acquire(good, nil)
	defer rel()
	if e.err != nil || !e.m.Match("e1.example:443") {
		t.Fatalf("之后的调用应正常: %+v", e)
	}
}
