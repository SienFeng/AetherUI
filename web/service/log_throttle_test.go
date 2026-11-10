package service

import (
	"testing"
	"time"
)

func TestLogThrottleSuppressesRepeatsWithinWindow(t *testing.T) {
	l := NewLogThrottle(5 * time.Minute)
	t0 := time.Unix(1_800_000_000, 0)
	if !l.allowAt("boom", t0) {
		t.Fatal("第一条应当放行")
	}
	if l.allowAt("boom", t0.Add(10*time.Second)) {
		t.Fatal("窗口内的同一条应当被压掉")
	}
	if !l.allowAt("other", t0.Add(20*time.Second)) {
		t.Fatal("文字变了应当立刻放行")
	}
	if !l.allowAt("other", t0.Add(20*time.Second+5*time.Minute)) {
		t.Fatal("窗口过后同一条应当再次放行")
	}
	l.Reset()
	if !l.allowAt("other", t0.Add(21*time.Second+5*time.Minute)) {
		t.Fatal("Reset 之后应当立刻放行")
	}
	// 时钟回跳不能让日志永远被压住。
	if !l.allowAt("other", t0) {
		t.Fatal("时钟回跳后应当放行")
	}
}
