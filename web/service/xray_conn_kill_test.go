package service

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/xray"
)

// 热应用删除入站（停用、到期、超额、删除）或修改入站（先删后加）之后，核心只关了监听器，已经建立的
// 连接继续转发（app/proxyman/inbound/worker.go 的 tcpWorker.Close 只关 hub）。要断的就是这些入站
// 在**旧配置**里的端口；新增的、没变的不碰。
func TestHotDiffKillPorts(t *testing.T) {
	ib := func(tag string, port int, settings string) xray.InboundConfig {
		return xray.InboundConfig{Tag: tag, Port: port, Protocol: "socks", Settings: rawf(settings)}
	}
	oldCfg := hotApplyBaseConfig()
	oldCfg.InboundConfigs = append(oldCfg.InboundConfigs,
		ib("inbound-1001", 1001, `{"auth":"noauth"}`), // 删除
		ib("inbound-1002", 1002, `{"auth":"noauth"}`), // 修改
		ib("inbound-1003", 1003, `{"auth":"noauth"}`), // 不变
	)
	newCfg := hotApplyBaseConfig()
	newCfg.InboundConfigs = append(newCfg.InboundConfigs,
		ib("inbound-1002", 1002, `{"auth":"password","accounts":[{"user":"u","pass":"p"}]}`),
		ib("inbound-1003", 1003, `{"auth":"noauth"}`),
		ib("inbound-1004", 1004, `{"auth":"noauth"}`), // 新增
	)
	diff, ok := xray.ComputeHotDiff(oldCfg, newCfg)
	if !ok {
		t.Fatal("前提不成立：这组改动应当能热应用")
	}
	if got := hotDiffKillPorts(oldCfg, diff); !reflect.DeepEqual(got, []int{1001, 1002}) {
		t.Fatalf("要断开旧连接的端口 = %v，期望 [1001 1002]", got)
	}
	if got := hotDiffKillPorts(oldCfg, &xray.HotDiff{}); len(got) != 0 {
		t.Fatalf("空 diff 不该断任何连接: %v", got)
	}
}

// setConnKiller 换掉断连接的平台能力，用例结束时换回。
func setConnKiller(t *testing.T, supported bool, kill func(port int) (int, error)) {
	t.Helper()
	oldSupported, oldKill := connKillSupported, killPortConns
	connKillSupported = func() bool { return supported }
	killPortConns = kill
	t.Cleanup(func() { connKillSupported, killPortConns = oldSupported, oldKill })
}

// startDokodemoSession 起核心（一个转发到本机回显服务的 dokodemo 入站）并建立一条会话。
func startDokodemoSession(t *testing.T) (*XrayService, int, net.Conn, int) {
	t.Helper()
	requireXrayBinary(t)
	requirePgrep(t)
	setupDB(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	stopXrayAtCleanup(t)

	echoPort := startEchoServer(t)
	doko := saveE2EInbound(t, "dokodemo-door", freePort(t),
		fmt.Sprintf(`{"address":"localhost","port":%d,"network":"tcp"}`, echoPort))
	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, doko.Port)
	session, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", doko.Port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	if got := echoRoundTrip(session); got != "ping" {
		t.Fatalf("前提不成立：会话建立后读到 %q", got)
	}
	return xs, doko.Id, session, doko.Port
}

func disableInbound(t *testing.T, id int) {
	t.Helper()
	if err := database.GetDB().Table("inbounds").Where("id = ?", id).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
}

// 本平台断不开已建立的连接（非 Linux，没有 netdiag）时，删除/修改入站必须退回整进程重启：
// 只有它能保证旧连接断开。修复前热应用之后会话照常转发——停用的用户继续用，超额的继续跑流量。
func TestDisabledInboundSessionEndsWhenConnsCannotBeKilled(t *testing.T) {
	xs, id, session, _ := startDokodemoSession(t)
	setConnKiller(t, false, func(int) (int, error) { return 0, errors.New("unsupported") })
	pidBefore := xrayChildPID(t)

	disableInbound(t, id)
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	if got := echoRoundTrip(session); got != "" {
		t.Fatalf("入站已停用，旧会话仍在转发（读到 %q）", got)
	}
	if pidAfter := xrayChildPID(t); pidAfter == pidBefore {
		t.Fatalf("断不开连接时应当退回整进程重启，PID 却没变（%d）", pidAfter)
	}
}

// 本平台能断连接时走热应用（不重启、不打断其他人），并且要断的正是那个入站的旧端口。
func TestDisabledInboundIsKilledByPortAfterHotApply(t *testing.T) {
	xs, id, _, port := startDokodemoSession(t)
	var mu sync.Mutex
	var killed []int
	setConnKiller(t, true, func(p int) (int, error) {
		mu.Lock()
		killed = append(killed, p)
		mu.Unlock()
		return 1, nil
	})
	pidBefore := xrayChildPID(t)

	disableInbound(t, id)
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	if pidAfter := xrayChildPID(t); pidAfter != pidBefore {
		t.Fatalf("能断连接时应当热应用，PID 却变了：%d -> %d", pidBefore, pidAfter)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(killed, []int{port}) {
		t.Fatalf("断开的端口 = %v，期望 [%d]", killed, port)
	}
}

// 断连接失败（没有 root、内核没编 CONFIG_INET_DIAG_DESTROY）时同样退回整进程重启，
// 不能把「监听器关了」当成「用户断了」。
func TestKillFailureFallsBackToRestart(t *testing.T) {
	xs, id, session, _ := startDokodemoSession(t)
	setConnKiller(t, true, func(int) (int, error) { return 0, errors.New("operation not permitted") })
	pidBefore := xrayChildPID(t)

	disableInbound(t, id)
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}
	if got := echoRoundTrip(session); got != "" {
		t.Fatalf("断连接失败后旧会话仍在转发（读到 %q）", got)
	}
	if pidAfter := xrayChildPID(t); pidAfter == pidBefore {
		t.Fatal("断连接失败时应当退回整进程重启")
	}
}
