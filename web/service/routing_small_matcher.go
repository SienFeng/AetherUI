package service

import (
	"crypto/sha256"
	"fmt"
	"runtime"
	"sync"
	"time"

	"a-ui/logger"
)

// 小规则（合并后条目数不超过 auditLargeThreshold）的 xray 匹配器按条目内容在全进程共用：
// 后台审计（规则本身、抢先命中归因的候选）与异常扫描的计划都从这里取，拿到的是同一个实例。
//
// 为什么缓存：xray 每建一个域名匹配器都强制 GC 3~4 次（common/geodata/strmatcher 的
// MphValueMatcher.Build，"peak mem"），建 IP 段集合再 2 次；只由手写条目组成的域名匹配器还不进
// 它自己的缓存。审计每 10 分钟一轮，改动前每轮把每条小规则的匹配器重建一遍；v1.56.2 起堆里常驻着
// 大匹配器（约 53 MB），1 核的生产机上每次强制 GC 约 50 毫秒，一轮审计因此满核 3~4 秒
// （2026-11-12 香港生产机实测）。异常扫描的计划本来就按代次常驻着同样的匹配器，两边共用一个
// 实例，常驻内存基本不变；多出来的只有改动前用完即丢的那些（改过的条目的旧版本、访问日志关着时
// 抢先命中归因的候选），闲置满上限后放掉（一个 2 万条的组约 6 MB）。
//
// 认法与大规则的常驻槽相同：合并后条目的内容哈希（groupMatcherKey），加上条目引用的 dat 文件的
// 身份、大小与修改时间，每次取用都核对。每次真正构建之前，先作废所有引用的 dat 已经变了的条目，
// 有作废的就强制 GC 一次：xray 的缓存只按「文件:类别」认、不看内容，旧对象还活着时会直接交给
// 这次构建。这只顾得到本缓存里的引用：访问日志开着时异常扫描的计划一直攥着同样的匹配器（计划要到
// 代次推进才重建，重建时旧计划还在），进行中的一轮审计、大规则的常驻槽也可能攥着，所以不重启面板
// 直接换 dat 时，这些匹配器多半仍按旧内容判定，而且按新的文件状态记下、不再重试——要用新内容就
// 重启面板（改动前同样如此）。生产上 geo 数据由 a-ui geo 更新，换完会重启面板。日志里的
// 「dat 文件变了」只说明重建过，不说明拿到了新内容。
//
// 只放掉闲置超过 smallMatcherIdleTTL 的条目（规则删了、改了条目），不按条目数淘汰：在用的条目
// 每轮审计都会取到，按数量踢掉只会让下一轮原样重建、尖峰回来。访问日志关掉之后条目照样到期
// 放掉，但异常扫描的计划仍攥着它们（计划只在代次或规则变了时重建，改动前同样如此）。

// smallMatcherIdleTTL 是条目的闲置上限：审计每 10 分钟一轮，留出 3 个周期。用 var 只为测试能调小。
var smallMatcherIdleTTL = 30 * time.Minute

type smallMatcherCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*smallMatcherEntry
	builds  int // 累计成功构建的次数（测试用）
	sweep   idleTimer
}

type smallMatcherEntry struct {
	m        *groupMatcher
	dats     []datStamp
	lastUsed time.Time
}

var smallMatchers = &smallMatcherCache{entries: map[[sha256.Size]byte]*smallMatcherEntry{}}

// smallMatcherBuild 是 get 实际调用的构建函数；与大规则的 largeMatcherBuild 分开，测试往一边
// 注入故障时不会波及另一边。
var smallMatcherBuild = buildStampedGroupMatcher

// get 返回 domains/cidrs（已合并去重的条目）的匹配器，调用方只读。建不出来时 err 非 nil，
// 不缓存，下次再试。在锁里构建：审计与异常扫描同时要同一组条目时只建一次。锁里只会取到
// xray 内部与日志的锁（它们不会回头等这里），也不等大规则的常驻槽：buildAnomalyPlans 持着
// anomalyPlanMu、验证握着常驻槽的租约来取，都不会互等。
func (c *smallMatcherCache) get(domains, cidrs []string) *matcherEntry {
	key := groupMatcherKey(domains, cidrs)
	c.mu.Lock()
	defer c.mu.Unlock()
	stale := false
	if e := c.entries[key]; e != nil {
		if datsUnchanged(e.dats) {
			e.lastUsed = time.Now()
			return &matcherEntry{m: e.m}
		}
		stale = true
	}
	// 要构建了（没有缓存，或引用的 dat 换了）：先作废所有引用的 dat 已经变了的条目，有作废的
	// 就强制 GC 一次，再建。xray 的缓存只按「文件:类别」认，别的条目还攥着的旧集合会被交给
	// 这次构建——新条目引用着同一个类别时也一样。没有作废任何条目就不强制 GC。
	dropped := 0
	for k, e := range c.entries {
		if !datsUnchanged(e.dats) {
			delete(c.entries, k)
			dropped++
		}
	}
	reason := "此前没有缓存"
	if stale {
		reason = "dat 文件变了"
	}
	if dropped > 0 {
		runtime.GC()
		reason += fmt.Sprintf("；作废了 %d 个引用旧 dat 的条目，别处还引用着的旧内容要等面板重启才会换掉", dropped)
	}
	start := time.Now()
	m, dats, err := smallMatcherBuild(domains, cidrs)
	if err != nil {
		return &matcherEntry{err: err}
	}
	c.entries[key] = &smallMatcherEntry{m: m, dats: dats, lastUsed: time.Now()}
	c.builds++
	c.sweep.ensure(smallMatcherIdleTTL, c.expireIdle)
	logger.Infof("路由审计：小规则匹配器已构建（%s），%d 条域名、%d 条 IP，耗时 %.2f 秒",
		reason, len(domains), len(cidrs), time.Since(start).Seconds())
	return &matcherEntry{m: m}
}

// expireIdle 由计时器叫醒：放掉闲置超过上限的条目，还有条目就在最早的那条到期时再醒。
func (c *smallMatcherCache) expireIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep.fired()
	ttl, now := smallMatcherIdleTTL, time.Now()
	var next time.Duration
	for k, e := range c.entries {
		left := ttl - now.Sub(e.lastUsed)
		if left <= 0 {
			delete(c.entries, k)
			continue
		}
		if next == 0 || left < next {
			next = left
		}
	}
	if next > 0 {
		c.sweep.schedule(next, c.expireIdle)
	}
}

// idleTimer 叫醒缓存去放掉闲置的条目，同一时刻至多排着一次。不带锁：调用方在自己的锁里调用。
type idleTimer struct {
	t  *time.Timer
	at time.Time // 排着的叫醒时刻；零值表示没排
}

// ensure 在没排着的时候排一次：d 之后调用 fn。
func (it *idleTimer) ensure(d time.Duration, fn func()) {
	if it.at.IsZero() {
		it.schedule(d, fn)
	}
}

// schedule 把叫醒时刻改到 d 之后。
func (it *idleTimer) schedule(d time.Duration, fn func()) {
	it.at = time.Now().Add(d)
	if it.t == nil {
		it.t = time.AfterFunc(d, fn)
	} else {
		it.t.Reset(d)
	}
}

// fired 在回调开头调用：这一次已经醒了，之后要再排就得重新 ensure / schedule。
func (it *idleTimer) fired() { it.at = time.Time{} }

// stop 停掉排着的叫醒（测试清空缓存时用）。早先已经触发、还在等锁的回调照常跑一遍，无害。
func (it *idleTimer) stop() {
	if it.t != nil {
		it.t.Stop()
	}
	it.at = time.Time{}
}
