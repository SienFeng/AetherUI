package service

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"runtime"
	"sync"
	"time"

	"a-ui/logger"

	"github.com/xtls/xray-core/common/platform/filesystem"
)

// 大规则（合并后条目数超过 auditLargeThreshold）的 xray 匹配器在全进程共用一个常驻槽：
// 路由审计（verifyShared 里规则本身与抢先命中归因的候选）和访问日志异常扫描都从这里取。
//
// 为什么常驻：一个 11 万条的域名组建一次匹配器，在 1 核机器上要满核 3~6 秒，建好后约占
// 50 多 MB 堆。此前两条路径各自每 10 分钟建一次、用完即丢，而 xray 的 MphDomainMatcherFactory
// 只用弱引用缓存，GC（Go 至少每 2 分钟一次）之后再用就整套重建。生产机上每天重建两百多次，
// 每次都是一根满核尖峰（2026-11-12 香港时间实测）。现在只在条目或它引用的 dat 文件变了之后重建
// （订阅刷新、改组、更新 geo 数据），平时直接复用。代价是这一个匹配器在两次使用之间也占着
// 内存，用户确认接受。
//
// 认法：合并后条目的内容哈希，加上条目引用的 dat 文件（geosite:/geoip:/ext:）的身份、大小与
// 修改时间。条目顺序不同只会多建一次，不会认错。
//
// 内存口径不变：同一时刻至多一个大匹配器存活。换槽（条目或 dat 变了、要用另一个组序列）
// 要等所有正在用旧匹配器的调用方放手，放掉旧的、GC 之后再建新的。闲置超过
// largeMatcherIdleTTL（大规则删了、访问日志关了）自动放掉。
//
// 调用方约定：acquire 之后必须调用返回的 release（可重复调用）；放手之前不能再 acquire。
// 换槽要等所有人放手，自己握着一个再去等另一个会把自己卡死（verifyShared 用单租约保证）。

// largeMatcherIdleTTL 是常驻匹配器的闲置上限。两个任务每 10 分钟各用一次，留出 3 个周期。
// 用 var 只为测试能调小。
var largeMatcherIdleTTL = 30 * time.Minute

type largeMatcherCache struct {
	mu   sync.Mutex
	cond sync.Cond // L 为 &mu；有人放手、建完时广播

	m        *groupMatcher // nil 表示槽空
	key      [sha256.Size]byte
	dats     []datStamp
	users    int  // 正在用 m 的调用方
	building bool // 有调用方正在建，其余的等它建完
	builds   int  // 累计成功构建的次数（日志与测试用）
	lastUsed time.Time
	idle     *time.Timer
}

func newLargeMatcherCache() *largeMatcherCache {
	c := &largeMatcherCache{}
	c.cond.L = &c.mu
	return c
}

var largeMatchers = newLargeMatcherCache()

// acquire 返回 domains/cidrs（已合并去重的条目）的匹配器与放手函数。建不出来时 err 非 nil，
// 槽保持为空，下次再试（失败一般在解析阶段，不到构建那一步）。
func (c *largeMatcherCache) acquire(domains, cidrs []string) (*matcherEntry, func()) {
	key := largeMatcherKey(domains, cidrs)
	c.mu.Lock()
	for {
		if c.building {
			// 另一个调用方正在建：等它建完再看能不能直接用，两个任务不会同时各建一份。
			c.cond.Wait()
			continue
		}
		if c.m != nil && c.key == key && datsUnchanged(c.dats) {
			c.users++
			m := c.m
			c.mu.Unlock()
			return &matcherEntry{m: m}, c.releaser()
		}
		if c.users == 0 {
			break
		}
		// 槽里的旧匹配器还有人在用：等他们放手再换。
		c.cond.Wait()
	}
	reason := "此前没有常驻"
	switch {
	case c.m != nil && c.key != key:
		reason = "条目变了"
	case c.m != nil:
		reason = "dat 文件变了"
	}
	c.dropLocked()
	c.building = true
	c.mu.Unlock()
	built := false
	defer func() {
		if !built {
			// 构建中途 panic（照常往上抛，由任务的 Recover 记录）：不能让 building 一直挂着，
			// 否则之后所有调用方都等在这里，审计与扫描两个任务从此停住。
			c.mu.Lock()
			c.building = false
			c.cond.Broadcast()
			c.mu.Unlock()
		}
	}()

	// 放掉旧的之后、建新的之前强制回收一次。xray 的匹配器缓存是弱引用，只按「文件:类别」
	// 与条目认、不看 dat 内容：旧匹配器还没被回收时，同样的条目会直接拿回它，dat 换了内容
	// 也照样是旧的。换槽前已等所有人放手，这次 GC 能把它真正放掉，新旧两个也不同时占着内存。
	runtime.GC()
	start := time.Now()
	m, dats, err := largeMatcherBuild(domains, cidrs)
	elapsed := time.Since(start)
	built = true

	c.mu.Lock()
	c.building = false
	c.cond.Broadcast()
	if err != nil {
		c.mu.Unlock()
		return &matcherEntry{err: err}, func() {}
	}
	c.m, c.key, c.dats = m, key, dats
	c.builds++
	c.users++
	c.mu.Unlock()
	logger.Infof("路由审计：大规则匹配器已重建（%s），%d 条域名、%d 条 IP，耗时 %.1f 秒",
		reason, len(domains), len(cidrs), elapsed.Seconds())
	return &matcherEntry{m: m}, c.releaser()
}

func (c *largeMatcherCache) releaser() func() {
	var once sync.Once
	return func() { once.Do(c.release) }
}

func (c *largeMatcherCache) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users--
	c.lastUsed = time.Now()
	if c.users > 0 {
		return
	}
	c.cond.Broadcast()
	if c.idle == nil {
		c.idle = time.AfterFunc(largeMatcherIdleTTL, c.expireIdle)
	} else {
		c.idle.Reset(largeMatcherIdleTTL)
	}
}

// expireIdle 在闲置超时后放掉常驻的匹配器。计时器可能是早先一次放手留下的：按最近一次
// 放手的时刻判断，还没到就按剩余时间再等。
func (c *largeMatcherCache) expireIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || c.users > 0 || c.building {
		return
	}
	if left := largeMatcherIdleTTL - time.Since(c.lastUsed); left > 0 {
		c.idle.Reset(left)
		return
	}
	c.dropLocked()
	logger.Info("路由审计：大规则匹配器闲置超过 ", largeMatcherIdleTTL, "，已释放")
}

func (c *largeMatcherCache) dropLocked() {
	c.m, c.dats, c.key = nil, nil, [sha256.Size]byte{}
}

// largeMatcherKey 是合并后条目的内容哈希；每个条目带长度前缀，拼接不会有歧义。
func largeMatcherKey(domains, cidrs []string) [sha256.Size]byte {
	h := sha256.New()
	buf := make([]byte, 0, 32<<10)
	add := func(list []string) {
		buf = binary.AppendUvarint(buf, uint64(len(list)))
		for _, s := range list {
			buf = binary.AppendUvarint(buf, uint64(len(s)))
			buf = append(buf, s...)
			if len(buf) >= 16<<10 {
				h.Write(buf)
				buf = buf[:0]
			}
		}
	}
	add(domains)
	add(cidrs)
	h.Write(buf)
	var key [sha256.Size]byte
	h.Sum(key[:0])
	return key
}

// datStamp 是一个 dat 文件在构建时的状态；info 为 nil 表示当时就读不到。
type datStamp struct {
	file string
	info os.FileInfo
}

// stampDats 记下 files 当前的状态。在解析之后、读取内容建匹配器之前调用：构建期间文件
// 又被换掉的话，下次核对对不上，会再建一次，而不是一直用旧内容。
func stampDats(files []string) []datStamp {
	out := make([]datStamp, 0, len(files))
	for _, f := range files {
		info, err := filesystem.StatAsset(f) // 与 xray 打开文件同一套路径解析
		if err != nil {
			info = nil
		}
		out = append(out, datStamp{file: f, info: info})
	}
	return out
}

// datsUnchanged 报告记下的 dat 文件是否都还是原来那份。读不到算变了：交给重建去报错，
// 不拿可能已经过时的匹配器继续用。
func datsUnchanged(stamps []datStamp) bool {
	for _, s := range stamps {
		cur, err := filesystem.StatAsset(s.file)
		if err != nil || s.info == nil || !os.SameFile(s.info, cur) ||
			cur.Size() != s.info.Size() || !cur.ModTime().Equal(s.info.ModTime()) {
			return false
		}
	}
	return true
}

// largeMatcherBuild 是 acquire 实际调用的构建函数；用 var 只为测试能注入构建中途崩溃。
var largeMatcherBuild = buildLargeGroupMatcher

// buildLargeGroupMatcher 解析条目、记下引用的 dat 文件，再建匹配器。
func buildLargeGroupMatcher(domains, cidrs []string) (*groupMatcher, []datStamp, error) {
	rules, err := parseGroupRules(domains, cidrs)
	if err != nil {
		return nil, nil, err
	}
	dats := stampDats(rules.datFiles())
	m, err := rules.build()
	if err != nil {
		return nil, nil, err
	}
	return m, dats, nil
}
