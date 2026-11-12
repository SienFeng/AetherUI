package service

import (
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/geodata"

	"a-ui/logger"
	"a-ui/xray"
)

// 地区限制的匹配器（geoSourceRules）按 dat 代码在全进程共用一份：自动出差放行（每分钟）、
// 并发判定（每 30 秒）、后台审计与异常扫描、「需要留意」都经 newGeoBlockMatcherWith 从这里取。
//
// 为什么缓存：xray 每建一个带 IP 段的匹配器都强制 GC 两次（common/geodata/ip_matcher.go 的
// IPSetFactory.createFrom，"peak mem"），拒绝 IPv6 的 "::/0" 也一样、而且不进它自己的缓存；
// 读 dat 再各强制一次。强制 GC 的耗时与存活的堆成正比：v1.56.2 起大规则匹配器常驻（约 53 MB），
// 1 核的生产机上每次约 50 毫秒，8 个地区入站重建一次约 0.8 核·秒，改动前每分钟三次
// （2026-11-12 香港生产机实测）。
//
// 为什么能一直复用：dat 代码按内容寻址（geoDatCode = tag + 内容哈希），同一个代码在 dat 里永远
// 是同一份允许集，核心用的也是它。升级前的 dat 只有 tag（geoTagOfCode(code) == code），同一个
// 代码的内容会变，不缓存、每次照旧构建。拒绝 IPv6 的那一半与代码无关，全部共用一个。
//
// 只放掉闲置超过 geoSourceIdleTTL 的条目（允许集换了代码、地区限制关了），不按条目数淘汰：
// 在用的条目每分钟都会被取到，按数量踢掉只会让下一次原样重建。条目很小（一份允许集几千段）。

// geoSourceIdleTTL 是条目的闲置上限。用 var 只为测试能调小。
var geoSourceIdleTTL = 30 * time.Minute

type geoSourceCache struct {
	mu sync.Mutex
	// v6 是拒绝 IPv6 来源的匹配器，所有代码共用；v6Key 是建它用的条件（geoRuleSources 的前一半）。
	v6Key   string
	v6      *router.IPMatcher
	entries map[string]*geoSourceEntry // 按内容寻址的 dat 代码 → 匹配器
	builds  int                        // 累计构建次数（测试用）
	sweep   idleTimer
}

type geoSourceEntry struct {
	rules    *geoSourceRules
	lastUsed time.Time
}

var geoSources = &geoSourceCache{entries: map[string]*geoSourceEntry{}}

// rulesFor 返回 dat 代码 code 的两条拒绝规则的匹配器，调用方只读。建不出来（dat 里没有这个
// 代码）时返回 error，不缓存，下次再试。在锁里构建：几个任务同时要同一个代码时只建一次。
func (c *geoSourceCache) rulesFor(code string) (*geoSourceRules, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cacheable := geoTagOfCode(code) != code
	if cacheable {
		if e := c.entries[code]; e != nil {
			e.lastUsed = time.Now()
			return e.rules, nil
		}
	}
	start := time.Now()
	xray.EnsureAssetLocation()
	v6src, outsideSrc := geoRuleSources(code)
	if key := strings.Join(v6src, "\n"); c.v6 == nil || c.v6Key != key {
		v6, err := buildSourceIPMatcher(v6src)
		if err != nil {
			return nil, err
		}
		c.v6, c.v6Key = v6, key
	}
	outside, err := buildSourceIPMatcher(outsideSrc)
	if err != nil {
		return nil, err
	}
	c.builds++
	r := &geoSourceRules{v6: c.v6, outside: outside}
	if cacheable {
		c.entries[code] = &geoSourceEntry{rules: r, lastUsed: time.Now()}
		c.sweep.ensure(geoSourceIdleTTL, c.expireIdle)
		logger.Infof("地区限制：地区匹配器已构建（%s），耗时 %.2f 秒", code, time.Since(start).Seconds())
	}
	return r, nil
}

// expireIdle 由计时器叫醒：放掉闲置超过上限的条目，还有条目就在最早的那条到期时再醒。
func (c *geoSourceCache) expireIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep.fired()
	ttl, now := geoSourceIdleTTL, time.Now()
	var next time.Duration
	for code, e := range c.entries {
		left := ttl - now.Sub(e.lastUsed)
		if left <= 0 {
			delete(c.entries, code)
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

// buildSourceIPMatcher 按核心解析 routing.rules[].source 的同一条路径建来源 IP 匹配器。
func buildSourceIPMatcher(src []string) (*router.IPMatcher, error) {
	rules, err := geodata.ParseIPRules(src)
	if err != nil {
		return nil, err
	}
	return router.NewIPMatcher(rules, router.MatcherAsType_Source)
}
