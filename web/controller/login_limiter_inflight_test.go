package controller

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

const inflightIP = "203.0.113.7"

func mustBegin(t *testing.T, l *loginLimiter, ip string) *loginTicket {
	t.Helper()
	ticket, _, gate := l.Begin(ip)
	if gate != loginGateOpen {
		t.Fatalf("Begin(%s) = %v，期望放行", ip, gate)
	}
	return ticket
}

// 同一来源「已失败次数 + 正在校验的次数」到上限之后不再放行，被挡回的那次不计为失败。
func TestLimiterCapsAttemptsInFlightPerSource(t *testing.T) {
	l, _ := newClockedLimiter()
	tickets := make([]*loginTicket, 0, loginMaxFailures)
	for i := 0; i < loginMaxFailures; i++ {
		tickets = append(tickets, mustBegin(t, l, inflightIP))
	}
	if _, _, gate := l.Begin(inflightIP); gate != loginGateBusy {
		t.Fatalf("第 %d 个同时进行的尝试 = %v，期望 loginGateBusy", loginMaxFailures+1, gate)
	}
	// 别的来源不受影响。
	if _, _, gate := l.Begin("198.51.100.1"); gate != loginGateOpen {
		t.Fatalf("另一个来源被牵连: %v", gate)
	}
	for i, ticket := range tickets {
		locked := ticket.Fail()
		if locked != (i == loginMaxFailures-1) {
			t.Fatalf("第 %d 次失败 locked = %v", i+1, locked)
		}
	}
	if _, blocked := l.Blocked(inflightIP); !blocked {
		t.Fatal("在途的尝试全部失败之后没有锁定")
	}
}

// 真正并发地抢名额：无论多少个 goroutine 同时来，放行的都不超过上限。
func TestLimiterAdmitsAtMostMaxFailuresConcurrently(t *testing.T) {
	l := newLoginLimiter()
	var mu sync.Mutex
	var admitted []*loginTicket
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ticket, _, gate := l.Begin(inflightIP); gate == loginGateOpen {
				mu.Lock()
				admitted = append(admitted, ticket)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(admitted) != loginMaxFailures {
		t.Fatalf("并发放行了 %d 个，期望恰好 %d 个", len(admitted), loginMaxFailures)
	}
	for _, ticket := range admitted {
		ticket.Fail()
	}
	if _, blocked := l.Blocked(inflightIP); !blocked {
		t.Fatal("放行的尝试全部失败之后没有锁定")
	}
}

// 被挡回的尝试不计为失败；名额还回来之后照常放行。
func TestLimiterBusyRefusalIsNotCountedAsFailure(t *testing.T) {
	l, _ := newClockedLimiter()
	for i := 0; i < loginMaxFailures-1; i++ {
		failOnce(l, inflightIP)
	}
	first := mustBegin(t, l, inflightIP)
	if _, _, gate := l.Begin(inflightIP); gate != loginGateBusy {
		t.Fatalf("已有 %d 次失败加 1 次在途时应当挡回，实际 %v", loginMaxFailures-1, gate)
	}
	first.Abort()
	// Abort 可重复调用，不会多还名额。
	first.Abort()
	if l.entries[limiterKey(inflightIP)].inFlight != 0 {
		t.Fatalf("Abort 之后在途数 = %d", l.entries[limiterKey(inflightIP)].inFlight)
	}
	if _, blocked := l.Blocked(inflightIP); blocked {
		t.Fatal("被挡回与中止的尝试被计成了失败")
	}
	if !failOnce(l, inflightIP) {
		t.Fatalf("第 %d 次真正的失败没有触发锁定", loginMaxFailures)
	}
}

// 改动前的 Reset 会删掉整条记录：同一来源一边并发猜密码、一边有一次成功，成功
// 就把同时记下的失败一起清掉了。现在一次成功只清掉它开始之前就有的失败。
func TestLimiterSuccessKeepsFailuresRecordedDuringIt(t *testing.T) {
	l, _ := newClockedLimiter()
	ok := mustBegin(t, l, inflightIP)
	a := mustBegin(t, l, inflightIP)
	b := mustBegin(t, l, inflightIP)
	a.Fail()
	ok.Succeed()
	b.Fail()
	// 与成功同时进行的那两次失败都还在：再错 loginMaxFailures-2 次就锁定。
	for i := 0; i < loginMaxFailures-3; i++ {
		if failOnce(l, inflightIP) {
			t.Fatalf("第 %d 次失败就锁定了", i+3)
		}
	}
	if !failOnce(l, inflightIP) {
		t.Fatal("成功把同时进行的失败清掉了：累计到上限却没有锁定")
	}
}

// 管理员自己先输错几次、再输对：开始之前就有的失败照旧清零。
func TestLimiterSuccessClearsFailuresBeforeIt(t *testing.T) {
	l, _ := newClockedLimiter()
	for i := 0; i < loginMaxFailures-2; i++ {
		failOnce(l, inflightIP)
	}
	ok := mustBegin(t, l, inflightIP)
	concurrent := mustBegin(t, l, inflightIP)
	concurrent.Fail()
	ok.Succeed()
	// 清掉的是开始之前的 loginMaxFailures-2 次，剩下同时进行的那 1 次。
	for i := 0; i < loginMaxFailures-2; i++ {
		if failOnce(l, inflightIP) {
			t.Fatalf("清零之后第 %d 次失败就锁定了", i+2)
		}
	}
	if !failOnce(l, inflightIP) {
		t.Fatal("累计到上限却没有锁定")
	}
}

// 已经生效的锁定，一次成功也解除不了。
func TestLimiterSuccessNeverLiftsALock(t *testing.T) {
	l, now := newClockedLimiter()
	ok := mustBegin(t, l, inflightIP)
	l.mu.Lock()
	l.entries[limiterKey(inflightIP)].lockedUntil = now.Add(loginLockDuration)
	l.mu.Unlock()
	ok.Succeed()
	if _, blocked := l.Blocked(inflightIP); !blocked {
		t.Fatal("一次成功把已经生效的锁定解除了")
	}
}

// 回收表项时不能删掉还有尝试在校验中的记录：它们结束时要把名额还回这条记录。
func TestLimiterEvictionKeepsEntriesWithAttemptsInFlight(t *testing.T) {
	l, now := newClockedLimiter()
	pending := make([]*loginTicket, 0, loginMaxEntries)
	for i := 0; i < loginMaxEntries; i++ {
		pending = append(pending, mustBegin(t, l, fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)))
	}
	*now = now.Add(loginFailWindow + time.Second)
	// 表满且没有可回收的记录：新来源放行但不计数（既有取舍）。
	if ticket, _, gate := l.Begin("192.0.2.1"); gate != loginGateOpen || ticket.tracked {
		t.Fatalf("表满时新来源应当放行且不计数，实际 gate = %v", gate)
	}
	if len(l.entries) != loginMaxEntries {
		t.Fatalf("有在途尝试的记录被回收了: 剩 %d 条", len(l.entries))
	}
	for _, ticket := range pending {
		ticket.Fail()
	}
	if e := l.entries[limiterKey("10.0.0.0")]; e == nil || e.failures != 1 || e.inFlight != 0 {
		t.Fatalf("在途尝试结束后的记录不对: %+v", e)
	}
}
