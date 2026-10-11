package controller

import (
	"fmt"
	"testing"
	"time"
)

func newClockedLimiter() (*loginLimiter, *time.Time) {
	now := time.Unix(1700000000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterLocksAfterMaxFailures(t *testing.T) {
	l, now := newClockedLimiter()
	for i := 1; i < loginMaxFailures; i++ {
		if l.Fail("1.2.3.4") {
			t.Fatalf("第 %d 次失败就锁定了", i)
		}
		if _, blocked := l.Blocked("1.2.3.4"); blocked {
			t.Fatalf("第 %d 次失败后已被锁定", i)
		}
	}
	if !l.Fail("1.2.3.4") {
		t.Fatalf("第 %d 次失败未触发锁定", loginMaxFailures)
	}
	remain, blocked := l.Blocked("1.2.3.4")
	if !blocked || remain != loginLockDuration {
		t.Fatalf("blocked = %v, remain = %v, want true, %v", blocked, remain, loginLockDuration)
	}
	// 别的来源不受牵连。
	if _, blocked := l.Blocked("1.2.3.5"); blocked {
		t.Error("未失败过的来源被锁定")
	}

	*now = now.Add(loginLockDuration - time.Second)
	if _, blocked := l.Blocked("1.2.3.4"); !blocked {
		t.Error("锁定期未满就解锁了")
	}
	*now = now.Add(2 * time.Second)
	if _, blocked := l.Blocked("1.2.3.4"); blocked {
		t.Error("锁定期已满仍未解锁")
	}
	// 解锁后重新从零计数，而不是再错一次就又锁上。
	if l.Fail("1.2.3.4") {
		t.Error("解锁后第一次失败就再次锁定")
	}
}

func TestLimiterWindowExpires(t *testing.T) {
	l, now := newClockedLimiter()
	for i := 0; i < loginMaxFailures-1; i++ {
		l.Fail("1.2.3.4")
	}
	*now = now.Add(loginFailWindow + time.Second)
	if l.Fail("1.2.3.4") {
		t.Error("窗口已过期，旧的失败不该继续累计")
	}
}

func TestLimiterResetClearsFailures(t *testing.T) {
	l, _ := newClockedLimiter()
	for i := 0; i < loginMaxFailures-1; i++ {
		l.Fail("1.2.3.4")
	}
	l.Reset("1.2.3.4")
	if l.Fail("1.2.3.4") {
		t.Error("登录成功后失败计数未清零")
	}
}

// 一个普通的 IPv6 接入就有 2^64 个地址可换，必须按 /64 计数。
func TestLimiterGroupsIPv6By64(t *testing.T) {
	l, _ := newClockedLimiter()
	for i := 0; i < loginMaxFailures; i++ {
		l.Fail(fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if _, blocked := l.Blocked("2001:db8:1:2:ffff::9"); !blocked {
		t.Error("同一个 /64 内换地址绕过了限速")
	}
	if _, blocked := l.Blocked("2001:db8:1:3::1"); blocked {
		t.Error("另一个 /64 被牵连")
	}
}

func TestLimiterIsBounded(t *testing.T) {
	l, now := newClockedLimiter()
	for i := 0; i < loginMaxEntries+500; i++ {
		l.Fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if len(l.entries) > loginMaxEntries {
		t.Fatalf("记录数 = %d，超过上限 %d", len(l.entries), loginMaxEntries)
	}
	// 窗口过期后旧记录可被回收，新来源重新能被计数。
	*now = now.Add(loginFailWindow + time.Second)
	for i := 0; i < loginMaxFailures; i++ {
		l.Fail("192.0.2.1")
	}
	if _, blocked := l.Blocked("192.0.2.1"); !blocked {
		t.Error("表满且旧记录已过期时，新来源没有被计数")
	}
	if len(l.entries) > loginMaxEntries {
		t.Fatalf("回收后记录数 = %d，超过上限", len(l.entries))
	}
}

// 表里全是锁定中的来源时，不能为了腾位置把别人的锁定提前解除。
func TestLimiterNeverEvictsActiveLocks(t *testing.T) {
	l, now := newClockedLimiter()
	for i := 0; i < loginMaxFailures; i++ {
		l.Fail("198.51.100.7")
	}
	for i := 0; i < loginMaxEntries+10; i++ {
		l.Fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	*now = now.Add(loginFailWindow + time.Second)
	l.Fail("192.0.2.1")
	if _, blocked := l.Blocked("198.51.100.7"); !blocked {
		t.Error("回收记录时把仍在锁定期内的来源解锁了")
	}
}
