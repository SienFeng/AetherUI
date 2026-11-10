package service

import (
	"sync"
	"time"
)

// LogThrottle 让同一条持续性故障不至于每轮任务都刷一遍日志：同一段文字在 every 内只放行一次，
// 文字一变立刻放行。用 NewLogThrottle 创建。
//
// 用在「失败后每 10 秒重试一次」这类路径上：重试是对的，但一个坏掉的模板每天刷八千多条
// 同样的 Error，会把真正新出现的告警淹没。
type LogThrottle struct {
	mu    sync.Mutex
	every time.Duration
	last  string
	at    time.Time
}

func NewLogThrottle(every time.Duration) *LogThrottle {
	return &LogThrottle{every: every}
}

// Allow 报告 msg 这一次要不要记。
func (l *LogThrottle) Allow(msg string) bool {
	return l.allowAt(msg, time.Now())
}

func (l *LogThrottle) allowAt(msg string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if msg == l.last && now.Sub(l.at) < l.every && !now.Before(l.at) {
		return false
	}
	l.last, l.at = msg, now
	return true
}

// Reset 忘掉上一条：故障恢复之后再出现，第一条照常记。
func (l *LogThrottle) Reset() {
	l.mu.Lock()
	l.last, l.at = "", time.Time{}
	l.mu.Unlock()
}
