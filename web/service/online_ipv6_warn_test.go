package service

import (
	"errors"
	"sync"
	"testing"
)

// Kick 读连接表时不持 onlineSampleLock，而 sample() 持着它：两条路径会同时走到
// dumpAllConns 里「IPv6 读取失败只告警一次」的那个标记。这个竞争只有 go test -race
// 看得出来（make verify 不开 -race），所以单独钉一条。
//
// IPv6 那一支在非 Linux 上走不到（netdiag.Dump 直接失败），这里直接并发调告警函数本身。
func TestIPv6DumpWarningIsSafeForConcurrentUse(t *testing.T) {
	old := onlineIPv6Warned.Load()
	onlineIPv6Warned.Store(false)
	t.Cleanup(func() { onlineIPv6Warned.Store(old) })

	err := errors.New("ipv6 disabled")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			warnIPv6DumpOnce(err)
		}()
	}
	wg.Wait()
	if !onlineIPv6Warned.Load() {
		t.Error("告警过之后标记应当已置位，否则下一次采样还会再刷一条")
	}
}
