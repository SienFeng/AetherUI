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
	// inFlight 是已经放行、还没出结果的尝试数（正在跑 bcrypt 的那些）。
	inFlight int
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

// loginGate 是 Begin 的结论。
type loginGate int

const (
	// loginGateOpen：放行，拿着返回的凭据去校验密码，结束时必须调 Fail/Succeed/Abort 之一。
	loginGateOpen loginGate = iota
	// loginGateLocked：该来源在锁定期内。
	loginGateLocked
	// loginGateBusy：该来源「已失败次数 + 正在校验的次数」已经到了上限。
	loginGateBusy
)

// loginTicket 是一次已放行的登录尝试，记着放行那一刻该来源的失败计数。
type loginTicket struct {
	l   *loginLimiter
	key string
	// tracked 为假表示这次尝试没有计数：表满且全是锁定中的来源时，新来源暂时
	// 不计数（宁可放过一个新来源，也不提前解除别人的锁定），沿用改动前的取舍。
	tracked        bool
	failuresBefore int
	windowBefore   time.Time
	finished       bool
}

// Begin 在校验密码之前占一个名额。
//
// 检查与记账必须在同一次持锁里完成。改动前「是否锁定」（Blocked）与「记一次
// 失败」（Fail）是两段分开的临界区，中间隔着一次约 50ms 的 bcrypt：同一来源
// 同时发来的一批错误密码全部通过检查、再各自记账，实测一批 40 个里有二三十个
// 被真正比对过。现在「已失败次数 + 正在校验的次数」到了上限就不再放行（回
// loginGateBusy，不计为失败），所以无论并发多高，一个锁定周期里真正被比对的
// 错误密码都不会超过 loginMaxFailures 个。
//
// 正常使用不受影响：管理员一次只提交一个登录请求，正在校验的只有那一个。
func (l *loginLimiter) Begin(ip string) (*loginTicket, time.Duration, loginGate) {
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
			// 表里全是仍在锁定期或仍有尝试在校验中的来源。宁可这一个新来源暂时
			// 不计数，也不能为了腾位置把别人的锁定提前解除。
			return &loginTicket{l: l, key: key}, 0, loginGateOpen
		}
		e = &loginAttempt{windowStart: now}
		l.entries[key] = e
	}
	if remain := e.lockedUntil.Sub(now); remain > 0 {
		return nil, remain, loginGateLocked
	}
	if now.Sub(e.windowStart) > loginFailWindow {
		e.failures = 0
		e.windowStart = now
	}
	if e.failures+e.inFlight >= loginMaxFailures {
		return nil, 0, loginGateBusy
	}
	e.inFlight++
	return &loginTicket{l: l, key: key, tracked: true, failuresBefore: e.failures, windowBefore: e.windowStart}, 0, loginGateOpen
}

// finish 结束一次尝试、归还名额，返回该来源的记录；已经结束过或没有计数时返回 nil。
// 调用方持锁。
func (t *loginTicket) finish() *loginAttempt {
	if t.finished {
		return nil
	}
	t.finished = true
	if !t.tracked {
		return nil
	}
	e := t.l.entries[t.key]
	if e != nil {
		e.inFlight--
	}
	return e
}

// Fail 记一次失败，返回这次失败是否触发了锁定。
func (t *loginTicket) Fail() bool {
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	e := t.finish()
	if e == nil {
		return false
	}
	now := t.l.now()
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

// Succeed 在登录成功后清掉这个来源**在本次尝试开始之前**积累的失败。
//
// 两条边界，都是为了不让一次成功替同一来源的并发猜测「洗白」：
//   - 校验期间别的请求记下的失败不清：它们和这次成功是同时进行的，不是管理员
//     自己此前输错的那几次。
//   - 已经生效的锁定不解除。有在途名额的约束，锁定本不会在一次成功校验的
//     途中生效；这一条是防御性的，确保任何情况下都不会出现「成功一次就把锁
//     清掉」——改动前 Reset 直接删掉整条记录，锁定也一并没了。
func (t *loginTicket) Succeed() {
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	e := t.finish()
	if e == nil {
		return
	}
	if e.lockedUntil.After(t.l.now()) {
		return
	}
	// 窗口在这期间翻过页（旧失败已经作废）时，现有的失败全都发生在这次尝试开始
	// 之后，一条都不清。
	if e.windowStart.Equal(t.windowBefore) {
		e.failures -= t.failuresBefore
		if e.failures < 0 {
			e.failures = 0
		}
	}
	if e.failures == 0 && e.inFlight == 0 {
		delete(t.l.entries, t.key)
	}
}

// Abort 归还名额，不记成功也不记失败：读不出两步验证状态这类与凭据无关的失败、
// 以及 handler 中途 panic 时用。可重复调用，已经 Fail/Succeed 过的凭据上是空操作，
// 所以 handler 里放一个 defer 兜底即可。
func (t *loginTicket) Abort() {
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	e := t.finish()
	if e != nil && e.failures == 0 && e.inFlight == 0 && !e.lockedUntil.After(t.l.now()) {
		delete(t.l.entries, t.key)
	}
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

// evict 删掉既不在锁定期内、计数窗口也已过期、也没有尝试正在校验的记录。
// 调用方持锁。有在途尝试的记录不能删：它们结束时要把名额还回这条记录。
func (l *loginLimiter) evict(now time.Time) {
	for key, e := range l.entries {
		if e.inFlight == 0 && now.After(e.lockedUntil) && now.Sub(e.windowStart) > loginFailWindow {
			delete(l.entries, key)
		}
	}
}
