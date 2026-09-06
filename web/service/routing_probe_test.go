package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// newProbeNode 直接落库造一个出站节点，绕开 AddFromLink 的解析与校验——
// 这些测试关心的是探测链路，不是新增路径。
func newProbeNode(t *testing.T, tag string, ob map[string]any) *model.OutboundNode {
	t.Helper()
	ob["tag"] = tag
	encoded, err := json.Marshal(ob)
	if err != nil {
		t.Fatalf("marshal outbound: %v", err)
	}
	protocol, _ := ob["protocol"].(string)
	node := &model.OutboundNode{
		Tag:      tag,
		Remark:   tag,
		Protocol: protocol,
		Config:   string(encoded),
		Enable:   true,
	}
	if err := database.GetDB().Save(node).Error; err != nil {
		t.Fatalf("save node: %v", err)
	}
	return node
}

func freedomOutbound() map[string]any {
	return map[string]any{"protocol": "freedom", "settings": map[string]any{}}
}

// resetProbeState 清空包级缓存，避免用例之间互相污染。
func resetProbeState(t *testing.T) {
	t.Helper()
	probeMu.Lock()
	probeCache = map[int]*ProbeResult{}
	probeMu.Unlock()
	t.Cleanup(func() {
		probeMu.Lock()
		probeCache = map[int]*ProbeResult{}
		probeMu.Unlock()
	})
}

// useProbeTarget 把探测目标指向本地 httptest server，让端到端测试不依赖外网。
func useProbeTarget(t *testing.T, urls ...string) {
	t.Helper()
	old := probeTargets
	probeTargets = urls
	t.Cleanup(func() { probeTargets = old })
}

// SQLite 会复用被删除的自增 id：删掉 id=N 的节点后新建一个，它还会拿到 id=N。
// 缓存若只按 id 索引，新节点会凭空继承旧节点那条绿色的「412 ms」，而界面上
// 没有任何一层会察觉——这正是 CLAUDE.md 里点名的那类静默错误。
func TestProbeResultsIgnoreStaleEntryAfterIdReuse(t *testing.T) {
	setupDB(t)
	resetProbeState(t)
	s := ProbeService{}

	old := newProbeNode(t, "a-ui-old", freedomOutbound())
	probeMu.Lock()
	probeCache[old.Id] = &ProbeResult{Status: ProbeOK, LatencyMs: 412, CheckedAt: time.Now().Unix(), tag: old.Tag}
	probeMu.Unlock()

	// 同一个 id、不同的 tag，模拟 id 被复用后的新节点。
	reused := &model.OutboundNode{Id: old.Id, Tag: "a-ui-new"}
	got := s.ResultsFor([]*model.OutboundNode{reused})
	if _, ok := got[reused.Id]; ok {
		t.Fatalf("stale result leaked to reused id: %+v", got[reused.Id])
	}

	// 原节点仍应读得到自己的结果。
	got = s.ResultsFor([]*model.OutboundNode{old})
	if got[old.Id] == nil || got[old.Id].LatencyMs != 412 {
		t.Fatalf("original node lost its result: %+v", got[old.Id])
	}
}

func TestProbeResultsDropExpiredEntries(t *testing.T) {
	setupDB(t)
	resetProbeState(t)
	s := ProbeService{}

	node := newProbeNode(t, "a-ui-expire", freedomOutbound())
	probeMu.Lock()
	probeCache[node.Id] = &ProbeResult{
		Status:    ProbeOK,
		LatencyMs: 100,
		CheckedAt: time.Now().Add(-probeTTL - time.Minute).Unix(),
		tag:       node.Tag,
	}
	probeMu.Unlock()

	if got := s.ResultsFor([]*model.OutboundNode{node}); got[node.Id] != nil {
		t.Fatalf("expired result returned: %+v", got[node.Id])
	}
	// 过期条目要被真正清出 map，而不是只在读取时过滤——否则删掉的节点
	// 会把条目永远留在内存里。
	probeMu.Lock()
	_, still := probeCache[node.Id]
	probeMu.Unlock()
	if still {
		t.Error("expired entry still in cache after read")
	}
}

func TestProbeForgetRemovesEntry(t *testing.T) {
	setupDB(t)
	resetProbeState(t)
	s := ProbeService{}

	node := newProbeNode(t, "a-ui-forget", freedomOutbound())
	probeMu.Lock()
	probeCache[node.Id] = &ProbeResult{Status: ProbeOK, CheckedAt: time.Now().Unix(), tag: node.Tag}
	probeMu.Unlock()

	s.Forget(node.Id)
	probeMu.Lock()
	_, ok := probeCache[node.Id]
	probeMu.Unlock()
	if ok {
		t.Error("Forget did not remove the entry")
	}
}

// 二进制缺失时必须报「无法测试」，绝不能因为「没测出问题」而显示成通——
// 这与 routing_validate.go 的 fail open 取向相反，那里放行的是保存动作，
// 这里放行等于对着一个坏节点显示绿灯。
func TestProbeReportsUnavailableWhenBinaryMissing(t *testing.T) {
	setupDB(t)
	resetProbeState(t)
	old := probeBinaryPath
	probeBinaryPath = func() string { return "bin/definitely-not-here" }
	t.Cleanup(func() { probeBinaryPath = old })

	node := newProbeNode(t, "a-ui-nobin", freedomOutbound())
	res, err := (&ProbeService{}).Probe(node.Id)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Status != ProbeUnavailable {
		t.Fatalf("Status = %q, want %q (err=%q)", res.Status, ProbeUnavailable, res.Error)
	}
	// 无法测试的结果不该占着缓存，否则 TTL 内都不会再试。
	probeMu.Lock()
	_, cached := probeCache[node.Id]
	probeMu.Unlock()
	if cached {
		t.Error("unavailable result should not be cached")
	}
}

// 端到端：真实 xray 进程 + 本地 httptest 目标，验证整条链路
// （起进程 → socks 入站 → 出站 → HTTP 204 → 延迟读数）。
func TestProbeEndToEndAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetProbeState(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	useProbeTarget(t, srv.URL)

	node := newProbeNode(t, "a-ui-e2e", freedomOutbound())
	res, err := (&ProbeService{}).Probe(node.Id)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Status != ProbeOK {
		t.Fatalf("Status = %q, want ok (err=%q)", res.Status, res.Error)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", res.LatencyMs)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Error("target was never reached through the proxy chain")
	}
	// 结果要进缓存，列表接口才看得到。
	if got := (&ProbeService{}).ResultsFor([]*model.OutboundNode{node}); got[node.Id] == nil {
		t.Error("successful result not cached")
	}
}

// 出站指向一个连不上的 socks 服务器时必须报失败，不能因为
// 「xray 进程起来了」就算通。
func TestProbeReportsFailureForUnreachableNode(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetProbeState(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	useProbeTarget(t, srv.URL)

	// 127.0.0.1:1 上不会有任何服务在监听。
	node := newProbeNode(t, "a-ui-dead", map[string]any{
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "127.0.0.1", "port": 1}},
		},
	})
	res, err := (&ProbeService{}).Probe(node.Id)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Status != ProbeFail {
		t.Fatalf("Status = %q, want fail", res.Status)
	}
	if res.Error == "" {
		t.Error("failed probe must carry a reason")
	}
}

// 同一节点的并发探测只应真正跑一次，后来者复用同一个结果。
func TestProbeDeduplicatesConcurrentCallsForSameNode(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetProbeState(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	useProbeTarget(t, srv.URL)

	node := newProbeNode(t, "a-ui-dedup", freedomOutbound())
	var wg sync.WaitGroup
	results := make([]*ProbeResult, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := (&ProbeService{}).Probe(node.Id)
			if err != nil {
				t.Errorf("Probe: %v", err)
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("target hit %d times, want 1 (probes were not deduplicated)", got)
	}
	for i, res := range results {
		if res == nil || res.Status != ProbeOK {
			t.Errorf("results[%d] = %+v, want ok", i, res)
		}
	}
}

// 删除节点必须连带丢弃探测结果。只靠 ResultsFor 的 tag 校验兜底是不够的：
// 那条防线只在「新节点恰好拿到同一个 id」时才生效，在此之前残留条目会
// 一直占着内存直到 TTL 到期。
func TestDelOutboundForgetsProbeResult(t *testing.T) {
	setupDB(t)
	resetProbeState(t)

	node := newProbeNode(t, "a-ui-del", freedomOutbound())
	probeMu.Lock()
	probeCache[node.Id] = &ProbeResult{Status: ProbeOK, CheckedAt: time.Now().Unix(), tag: node.Tag}
	probeMu.Unlock()

	if err := (&OutboundNodeService{}).Del(node.Id); err != nil {
		t.Fatalf("Del: %v", err)
	}
	probeMu.Lock()
	_, still := probeCache[node.Id]
	probeMu.Unlock()
	if still {
		t.Error("probe result survived node deletion")
	}
}

// 改了配置，上一次的探测结果就不再是对当前配置的判断。留着它，一个刚被
// 改错的节点会继续顶着上一次那盏绿灯。
func TestUpdateOutboundConfigForgetsProbeResult(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetProbeState(t)

	node := newProbeNode(t, "a-ui-upd", map[string]any{
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "1.2.3.4", "port": 1080}},
		},
	})
	probeMu.Lock()
	probeCache[node.Id] = &ProbeResult{Status: ProbeOK, LatencyMs: 412, CheckedAt: time.Now().Unix(), tag: node.Tag}
	probeMu.Unlock()

	changed := *node
	changed.Config = `{"protocol":"socks","settings":{"servers":[{"address":"5.6.7.8","port":1080}]}}`
	if err := (&OutboundNodeService{}).Update(&changed); err != nil {
		t.Fatalf("Update: %v", err)
	}
	probeMu.Lock()
	_, still := probeCache[node.Id]
	probeMu.Unlock()
	if still {
		t.Error("probe result survived a config change")
	}
}

// 反过来：只改备注不该丢弃结果，否则每次重命名都要重测一遍。
func TestUpdateOutboundRemarkKeepsProbeResult(t *testing.T) {
	setupDB(t)
	resetProbeState(t)

	node := newProbeNode(t, "a-ui-rename", freedomOutbound())
	probeMu.Lock()
	probeCache[node.Id] = &ProbeResult{Status: ProbeOK, LatencyMs: 412, CheckedAt: time.Now().Unix(), tag: node.Tag}
	probeMu.Unlock()

	changed := *node
	changed.Remark = "改个名字"
	if err := (&OutboundNodeService{}).Update(&changed); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := (&ProbeService{}).ResultsFor([]*model.OutboundNode{node}); got[node.Id] == nil {
		t.Error("probe result was dropped by a remark-only edit")
	}
}

// run 里 panic 时，inflight 表必须照样被清理。否则 call.done 永不关闭，
// 这个节点此后每一次测试都会永久卡在等待上，只能重启面板才能恢复。
func TestProbeRecoversInflightSlotAfterPanic(t *testing.T) {
	setupDB(t)
	resetProbeState(t)

	node := newProbeNode(t, "a-ui-panic", freedomOutbound())
	old := probeRunner
	probeRunner = func(*ProbeService, *model.OutboundNode) *ProbeResult {
		panic("boom")
	}
	t.Cleanup(func() { probeRunner = old })

	// 等待者要拿到兜底结果而不是一直挂着，所以先起一个再让本体 panic。
	waiterDone := make(chan *ProbeResult, 1)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to propagate to the caller")
			}
		}()
		go func() {
			// 给上面的调用留出注册 inflight 的时间；晚到也无妨，那时
			// inflight 已清理，这一次会自己重新跑（下面的 restore 保证不再 panic）。
			time.Sleep(20 * time.Millisecond)
			res, err := (&ProbeService{}).Probe(node.Id)
			if err != nil {
				t.Errorf("waiter Probe: %v", err)
			}
			waiterDone <- res
		}()
		(&ProbeService{}).Probe(node.Id)
	}()

	// 换成一个立刻返回的假 runner：这条用例验证的是 inflight 槽位有没有被
	// 释放，不该顺带去真实出网一次（慢，且在没有外网的 CI 上会不稳定）。
	probeRunner = func(*ProbeService, *model.OutboundNode) *ProbeResult {
		return &ProbeResult{Status: ProbeOK, CheckedAt: time.Now().Unix(), tag: node.Tag}
	}
	select {
	case res := <-waiterDone:
		if res == nil {
			t.Error("waiter got a nil result")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("waiter blocked forever: inflight slot was not released")
	}

	probeMu.Lock()
	_, stuck := probeInflight[node.Id]
	probeMu.Unlock()
	if stuck {
		t.Error("inflight entry survived the panic; the node is now permanently locked")
	}
}
