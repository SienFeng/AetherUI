package controller

import (
	"net"
	"sync"
	"time"
)

// 登录失败限速：同一来源在 loginFailWindow 内失败 loginMaxFailures 次，
// 锁定 loginLockDuration。
//
// 只按来源 IP 计，刻意不按用户名锁：面板只有一个管理员，按用户名锁等于
// 让任何知道用户名的人都能把管理员锁在门外。状态只在内存里，重启面板
// 即清空——那也是管理员把自己锁住时的出路（SSH 上去 a-ui restart）。
const (
	loginMaxFailures  = 5
	loginFailWindow   = 10 * time.Minute
	loginLockDuration = 15 * time.Minute
	// 记录数上限。不设上限的话，攻击者轮换来源地址就能把这张表撑到
	// 吃光内存。
	loginMaxEntries = 10000
)

type loginAttempt struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginAttempt
	now     func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: map[string]*loginAttempt{}, now: time.Now}
}

// 包级单例：controller 随面板「重启」（SIGHUP 重建 Server）反复构造，
// 计数挂在实例上的话，点一下重启面板就把锁定清掉了。
var loginGuard = newLoginLimiter()

// limiterKey 把来源地址归一成限速用的 key。IPv6 取 /64：一个普通的 IPv6
// 接入就有 2^64 个地址可换，按完整地址计数等于没有限速。
func limiterKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Blocked 报告该来源当前是否被锁定，以及还要等多久。
func (l *loginLimiter) Blocked(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[limiterKey(ip)]
	if e == nil {
		return 0, false
	}
	if remain := e.lockedUntil.Sub(l.now()); remain > 0 {
		return remain, true
	}
	return 0, false
}

// Fail 记一次失败，返回这次失败是否触发了锁定。
func (l *loginLimiter) Fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := limiterKey(ip)
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= loginMaxEntries {
			l.evict(now)
		}
		if len(l.entries) >= loginMaxEntries {
			// 表里全是仍在锁定期内的来源。宁可这一个新来源暂时不计数，
			// 也不能为了腾位置把别人的锁定提前解除。
			return false
		}
		e = &loginAttempt{windowStart: now}
		l.entries[key] = e
	}
	if now.Sub(e.windowStart) > loginFailWindow {
		e.failures = 0
		e.windowStart = now
	}
	e.failures++
	if e.failures >= loginMaxFailures {
		e.failures = 0
		e.windowStart = now
		e.lockedUntil = now.Add(loginLockDuration)
		return true
	}
	return false
}

// Reset 在登录成功后清掉该来源的失败记录。
func (l *loginLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, limiterKey(ip))
}

// evict 删掉既不在锁定期内、计数窗口也已过期的记录。调用方持锁。
func (l *loginLimiter) evict(now time.Time) {
	for key, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.windowStart) > loginFailWindow {
			delete(l.entries, key)
		}
	}
}
