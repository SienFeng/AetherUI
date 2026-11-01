package job

import (
	"errors"
	"testing"
	"time"

	"a-ui/web/service"
)

func TestShouldAudit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name      string
		cur, last uint64
		lastAt    time.Time
		want      bool
	}{
		{"同代次 9 分钟", 3, 3, now.Add(-9 * time.Minute), false},
		{"同代次 10 分钟", 3, 3, now.Add(-10 * time.Minute), true},
		{"代次变化", 4, 3, now.Add(-time.Second), true},
		{"首次 lastAt 零值", 3, 3, time.Time{}, true},
	}
	for _, c := range cases {
		if got := shouldAudit(c.cur, c.last, c.lastAt, now); got != c.want {
			t.Errorf("%s: shouldAudit = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestShouldScanLarge(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if !shouldScanLarge(time.Time{}, now) {
		t.Error("零值应立即扫")
	}
	if shouldScanLarge(now.Add(-9*time.Minute), now) {
		t.Error("9 分钟不应扫")
	}
	if !shouldScanLarge(now.Add(-10*time.Minute), now) {
		t.Error("10 分钟应扫")
	}
}

func TestRouteAuditJobSkipsWhenNotReady(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, e := range []error{service.ErrCoreNotReady, service.ErrVerifyBusy} {
		j := NewRouteAuditJob()
		j.now = func() time.Time { return now }
		j.lastGen, j.lastAt = 1, now.Add(-time.Hour)
		j.currentGen = func() uint64 { return 2 }
		j.audit = func() (uint64, error) { return 0, e }
		j.Run()
		if j.lastGen != 1 || !j.lastAt.Equal(now.Add(-time.Hour)) {
			t.Errorf("%v: lastGen/lastAt 被改动: %d %v", e, j.lastGen, j.lastAt)
		}
	}
	j := NewRouteAuditJob()
	j.now = func() time.Time { return now }
	j.currentGen = func() uint64 { return 2 }
	j.audit = func() (uint64, error) { return 0, errors.New("boom") }
	j.Run()
	if j.lastGen != 0 || !j.lastAt.IsZero() {
		t.Error("其他错误也不应推进 lastGen/lastAt")
	}
}

func TestRouteAuditJobAdvancesOnSuccess(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	j := NewRouteAuditJob()
	j.now = func() time.Time { return now }
	j.currentGen = func() uint64 { return 5 }
	calls := 0
	j.audit = func() (uint64, error) { calls++; return 5, nil }
	j.Run()
	if j.lastGen != 5 || !j.lastAt.Equal(now) || calls != 1 {
		t.Fatalf("成功后 lastGen=%d lastAt=%v calls=%d", j.lastGen, j.lastAt, calls)
	}
	j.Run() // 同代次、刚审计过：不再跑
	if calls != 1 {
		t.Errorf("无需审计时不应再调用，calls=%d", calls)
	}
}

func TestRouteAnomalyJobBackloggedForcesLargeScan(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var larges []bool
	backlogged := false
	j := NewRouteAnomalyJob()
	j.now = func() time.Time { return now }
	j.scan = func(_ time.Time, large bool) error { larges = append(larges, large); return nil }
	j.backlogged = func() bool { return backlogged }

	j.Run() // 首次：小 + 大
	if len(larges) != 2 || larges[0] || !larges[1] {
		t.Fatalf("首次应 小+大，得到 %v", larges)
	}
	now = now.Add(time.Minute)
	larges = nil
	j.Run() // <10 分钟、无积压：只小
	if len(larges) != 1 || larges[0] {
		t.Fatalf("无积压应只扫小规则，得到 %v", larges)
	}
	backlogged = true
	now = now.Add(time.Minute)
	larges = nil
	j.Run() // 积压：即使 <10 分钟也扫大规则，且一次运行只扫一次
	if len(larges) != 2 || larges[0] || !larges[1] {
		t.Fatalf("积压时应 小+大（各一次），得到 %v", larges)
	}
}

func TestRouteAuditJobSkipsConcurrentRun(t *testing.T) {
	j := NewRouteAuditJob()
	j.currentGen = func() uint64 { return 1 }
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	j.audit = func() (uint64, error) {
		calls++
		close(entered)
		<-release
		return 1, nil
	}
	done := make(chan struct{})
	go func() { j.Run(); close(done) }()
	<-entered
	j.Run() // 第一轮仍在运行：应立即返回且不再调用 audit
	close(release)
	<-done
	if calls != 1 {
		t.Fatalf("并发的第二次 Run 不应调用 audit，calls=%d", calls)
	}
}

func TestRouteAnomalyJobSkipsConcurrentRun(t *testing.T) {
	j := NewRouteAnomalyJob()
	j.backlogged = func() bool { return false }
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	j.scan = func(time.Time, bool) error {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan struct{})
	go func() { j.Run(); close(done) }()
	<-entered
	j.Run() // 第一轮仍在运行：应立即返回且不再调用 scan
	close(release)
	<-done
	if calls != 2 { // 第一轮自身：小 + 大（首次）
		t.Fatalf("并发的第二次 Run 不应调用 scan，calls=%d", calls)
	}
}

// 被 60 秒预算截断的一轮不算成功：不推进 lastGen/lastAt，下一次唤醒就重试。
func TestRouteAuditJobTruncatedDoesNotAdvance(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	j := NewRouteAuditJob()
	j.now = func() time.Time { return now }
	j.lastGen, j.lastAt = 1, now.Add(-time.Hour)
	j.currentGen = func() uint64 { return 2 }
	calls := 0
	j.audit = func() (uint64, error) { calls++; return 2, service.ErrVerifyTruncated }
	j.Run()
	if j.lastGen != 1 || !j.lastAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("截断后 lastGen/lastAt 被推进: %d %v", j.lastGen, j.lastAt)
	}
	now = now.Add(30 * time.Second)
	j.Run()
	if calls != 2 {
		t.Errorf("截断后下一次唤醒应重试，calls=%d", calls)
	}
}
