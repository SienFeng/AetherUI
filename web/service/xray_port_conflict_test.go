package service

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"a-ui/database/model"
	"a-ui/xray"
)

// resetPortConflictsForTest 清掉包级的冲突登记：它跨用例存活。
func resetPortConflictsForTest(t *testing.T) {
	t.Helper()
	clear := func() {
		portConflictMu.Lock()
		portConflicts = map[string]portConflictEntry{}
		portConflictMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// socksAlive 判断 socks 入站在不在：能完成 greeting 就算在。
func socksAlive(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return false
	}
	reply := make([]byte, 2)
	_, err = io.ReadFull(c, reply)
	return err == nil && reply[0] == 0x05
}

// coreStaysUp 在 d 内反复确认核心没有退出（bind 失败的核心起来后很快就退出）。
func coreStaysUp(xs *XrayService, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !xs.IsXrayRunning() {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func conflictTags(xs *XrayService) map[string]int {
	out := map[string]int{}
	for _, c := range xs.PortConflicts() {
		out[c.Tag] = c.Port
	}
	return out
}

// 核心启动时某个入站的端口已被别的程序占着（比如后来装的 Caddy 占了 :443）：修复前整份配置
// 加载失败，所有入站一起断，RestartXray 却返回 nil。现在这个入站被摘出去并登记，其余入站照常；
// 端口腾出来之后，下一轮检查把它加回去。
func TestFullStartSkipsInboundWhosePortIsTaken(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetPortConflictsForTest(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	stopXrayAtCleanup(t)

	a := socksInbound(freePort(t), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	b := socksInbound(freePort(t), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	for _, in := range []*model.Inbound{a, b} {
		if err := saveInboundRow(in); err != nil {
			t.Fatal(err)
		}
	}
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", b.Port))
	if err != nil {
		t.Fatal(err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	waitForPort(t, a.Port)
	if !coreStaysUp(xs, 1500*time.Millisecond) || !socksAlive(a.Port) {
		t.Fatal("一个入站的端口被占，其余入站跟着断了：整份配置没能加载")
	}
	if got := conflictTags(xs); got[b.Tag] != b.Port {
		t.Fatalf("被占端口的入站没有登记为冲突: %v", got)
	}
	// 冲突状态下配置稳定，不会每 10 秒重启一次。
	pid := currentProcess()
	if err := xs.RestartXray(false); err != nil {
		t.Fatal(err)
	}
	if currentProcess() != pid {
		t.Fatal("冲突状态下再次下发换了进程：会形成 10 秒一次的重启循环")
	}

	// 端口腾出来：下一轮检查置重启标志，入站加回来。
	squatter.Close()
	xs.IsNeedRestartAndSetFalse()
	xs.RecheckPortConflicts()
	if !xs.IsNeedRestartAndSetFalse() {
		t.Fatal("冲突端口已腾出，RecheckPortConflicts 却没有置重启标志")
	}
	if err := xs.RestartXray(false); err != nil {
		t.Fatal(err)
	}
	waitForPort(t, b.Port)
	if !socksAlive(b.Port) {
		t.Fatal("端口腾出后入站没有加回来")
	}
	if got := conflictTags(xs); len(got) != 0 {
		t.Fatalf("冲突已解除，登记却还在: %v", got)
	}
}

// 核心在跑时新加的入站撞上被占的端口：修复前热应用的 AddInbound 失败 → 退回整进程重启 → 新核心
// bind 失败 → 所有入站一起断。现在保留正在跑的核心，只把这个入站摘出去并登记。
func TestHotApplyKeepsCoreWhenAddedInboundPortIsTaken(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetPortConflictsForTest(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	stopXrayAtCleanup(t)

	a := socksInbound(freePort(t), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(a); err != nil {
		t.Fatal(err)
	}
	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatal(err)
	}
	waitForPort(t, a.Port)
	before := currentProcess()

	b := socksInbound(holdTCP(t, "127.0.0.1"), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(b); err != nil {
		t.Fatal(err)
	}
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	if currentProcess() != before || !coreStaysUp(xs, time.Second) || !socksAlive(a.Port) {
		t.Fatal("新入站的端口被占，正在跑的核心被换掉/断了")
	}
	if got := conflictTags(xs); got[b.Tag] != b.Port {
		t.Fatalf("没有登记冲突: %v", got)
	}
}

// 探测时还空着、核心 bind 时被抢（竞态）：AddInbound 报 address already in use。不能退回整进程
// 重启（新核心同样 bind 失败），而是登记冲突、其余改动照常下发，并把核心持有的配置记成「没有这个
// 入站」——否则下一轮比对恒不相等，形成重启循环。
func TestHotApplyBindFailureIsRecordedInsteadOfRestarting(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetPortConflictsForTest(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	stopXrayAtCleanup(t)

	a := socksInbound(freePort(t), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(a); err != nil {
		t.Fatal(err)
	}
	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatal(err)
	}
	waitForPort(t, a.Port)
	before := currentProcess()

	// 让探测永远说「空着」，模拟竞态。
	oldProbe := probeListenFn
	probeListenFn = func(inboundListenSpec) error { return nil }
	t.Cleanup(func() { probeListenFn = oldProbe })

	b := socksInbound(holdTCP(t, "127.0.0.1"), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(b); err != nil {
		t.Fatal(err)
	}
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	if currentProcess() != before || !coreStaysUp(xs, time.Second) || !socksAlive(a.Port) {
		t.Fatal("AddInbound 撞上端口占用后退回了整进程重启，或核心断了")
	}
	if got := conflictTags(xs); got[b.Tag] != b.Port {
		t.Fatalf("AddInbound 的端口占用没有登记: %v", got)
	}
	for _, ib := range currentProcess().GetConfig().InboundConfigs {
		if ib.Tag == b.Tag {
			t.Fatal("核心持有的配置里记着一个其实没加进去的入站")
		}
	}
	fresh, err := xs.GetXrayConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !currentProcess().GetConfig().Equals(fresh) {
		t.Fatal("登记冲突之后新生成的配置与核心持有的不相等：10 秒的消费任务会不停重试")
	}
}

// 生成侧：只摘 tag 与监听都与登记时一致的入站；管理员改了端口，旧登记不再适用。
func TestExcludePortConflictsOnlyMatchesSameListen(t *testing.T) {
	resetPortConflictsForTest(t)
	spec := inboundListenSpec{Listen: "127.0.0.1", Port: 20443, TCP: true}
	registerPortConflict("inbound-20443", spec, fmt.Errorf("bind: address already in use"))

	cfg := &xray.Config{InboundConfigs: []xray.InboundConfig{
		{Tag: "api", Port: 62789, Protocol: "dokodemo-door", Listen: rawf(`"127.0.0.1"`)},
		{Tag: "inbound-20443", Port: 20443, Protocol: "socks", Listen: rawf(`"127.0.0.1"`), Settings: rawf(`{}`)},
		{Tag: "inbound-20444", Port: 20444, Protocol: "socks", Listen: rawf(`"127.0.0.1"`), Settings: rawf(`{}`)},
	}}
	excludePortConflicts(cfg)
	if len(cfg.InboundConfigs) != 2 || cfg.InboundConfigs[1].Tag != "inbound-20444" {
		t.Fatalf("摘除结果不对: %+v", cfg.InboundConfigs)
	}

	moved := &xray.Config{InboundConfigs: []xray.InboundConfig{
		{Tag: "inbound-20443", Port: 20443, Protocol: "socks", Listen: rawf(`"127.0.0.2"`), Settings: rawf(`{}`)},
	}}
	excludePortConflicts(moved)
	if len(moved.InboundConfigs) != 1 {
		t.Fatal("监听地址变了，旧登记不该再摘掉它")
	}
}

// 运行中的核心只占着这个端口的 TCP 时，把入站改成也要 UDP（socks 打开 udp）不能整个跳过探测：
// UDP 被别的程序占着，新核心照样起不来。
func TestHeldPortStillProbesProtocolsTheCoreDoesNotHold(t *testing.T) {
	running := &xray.Config{InboundConfigs: []xray.InboundConfig{
		{Tag: "inbound-20500", Port: 20500, Protocol: "socks", Listen: rawf(`"127.0.0.1"`), Settings: rawf(`{"udp":false}`)},
	}}
	tcp, udp := heldListeners(running)
	spec := inboundListenSpec{Listen: "127.0.0.1", Port: 20500, TCP: true, UDP: true}
	rest, ok := unheldPart(spec, tcp, udp)
	if !ok || rest.TCP || !rest.UDP {
		t.Fatalf("核心只占着 TCP，还要探 UDP；实际 %+v ok=%v", rest, ok)
	}
	if _, ok := unheldPart(inboundListenSpec{Listen: "127.0.0.1", Port: 20500, TCP: true}, tcp, udp); ok {
		t.Fatal("要的正是核心占着的那部分，不该再探测")
	}
}

// 入站被删除、停用或改了监听之后，旧的冲突登记要清掉：否则 PortConflicts 一直报一个已经不存在的冲突。
func TestPortConflictIsForgottenWhenInboundGoesAway(t *testing.T) {
	setupDB(t)
	resetPortConflictsForTest(t)
	in := socksInbound(freePort(t), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(in); err != nil {
		t.Fatal(err)
	}
	spec := inboundListenSpec{Listen: "127.0.0.1", Port: in.Port, TCP: true}
	registerPortConflict(in.Tag, spec, fmt.Errorf("bind: address already in use"))
	xs := &XrayService{}
	if _, err := xs.GetXrayConfig(); err != nil {
		t.Fatal(err)
	}
	if got := conflictTags(xs); got[in.Tag] != in.Port {
		t.Fatalf("入站还在、监听没变，登记应当保留: %v", got)
	}

	in.Enable = false
	if err := saveInboundRow(in); err != nil {
		t.Fatal(err)
	}
	if _, err := xs.GetXrayConfig(); err != nil {
		t.Fatal(err)
	}
	if got := conflictTags(xs); len(got) != 0 {
		t.Fatalf("入站已停用，冲突登记却还在: %v", got)
	}
}
