package service

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

func TestInboundSocketNeeds(t *testing.T) {
	cases := []struct {
		name               string
		protocol, settings string
		stream             string
		tcp, udp           bool
	}{
		{"vless tcp", "vless", `{}`, `{"network":"tcp"}`, true, false},
		{"vmess ws", "vmess", `{}`, `{"network":"ws"}`, true, false},
		{"vless kcp", "vless", `{}`, `{"network":"kcp"}`, false, true},
		{"vless mkcp", "vless", `{}`, `{"network":"mkcp"}`, false, true},
		{"vmess quic", "vmess", `{}`, `{"network":"quic"}`, false, true},
		{"xhttp h3", "vless", `{}`, `{"network":"xhttp","security":"tls","tlsSettings":{"alpn":["h3"]}}`, false, true},
		{"xhttp h2", "vless", `{}`, `{"network":"xhttp","security":"tls","tlsSettings":{"alpn":["h2"]}}`, true, false},
		{"socks no udp", "socks", `{"auth":"noauth","udp":false}`, `{"network":"tcp"}`, true, false},
		{"socks udp", "socks", `{"auth":"noauth","udp":true}`, `{"network":"tcp"}`, true, true},
		{"mixed udp", "mixed", `{"udp":true}`, ``, true, true},
		{"ss default", "shadowsocks", `{"method":"aes-128-gcm"}`, `{"network":"tcp"}`, true, false},
		{"ss tcp,udp", "shadowsocks", `{"network":"tcp,udp"}`, `{"network":"tcp"}`, true, true},
		{"ss udp only", "shadowsocks", `{"network":"udp"}`, `{"network":"tcp"}`, false, true},
		{"ss list", "shadowsocks", `{"network":["tcp","udp"]}`, ``, true, true},
		{"dokodemo udp", "dokodemo-door", `{"network":"udp"}`, ``, false, true},
		{"tunnel tcp,udp", "tunnel", `{"network":"tcp,udp"}`, ``, true, true},
		{"wireguard", "wireguard", `{}`, ``, false, true},
		{"hysteria", "hysteria", `{}`, `{"network":"hysteria"}`, false, true},
		{"http", "http", `{}`, `{"network":"tcp"}`, true, false},
		{"upper case", "SOCKS", `{"udp":true}`, `{"network":"TCP"}`, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tcp, udp := inboundSocketNeeds(c.protocol, []byte(c.settings), []byte(c.stream))
			if tcp != c.tcp || udp != c.udp {
				t.Fatalf("tcp=%v udp=%v，期望 tcp=%v udp=%v", tcp, udp, c.tcp, c.udp)
			}
		})
	}
}

func TestListenOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "127.0.0.1", true},
		{"0.0.0.0", "192.0.2.1", true},
		{"::", "127.0.0.1", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.1", "::ffff:127.0.0.1", true},
		{"192.0.2.1", "198.51.100.1", false},
		{"127.0.0.1", "::1", false},
	}
	for _, c := range cases {
		if got := listenOverlaps(c.a, c.b); got != c.want {
			t.Errorf("listenOverlaps(%q, %q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
		if got := listenOverlaps(c.b, c.a); got != c.want {
			t.Errorf("listenOverlaps(%q, %q) = %v，期望 %v（应当对称）", c.b, c.a, got, c.want)
		}
	}
}

// holdTCP 在 addr 上占住一个 TCP 端口（模拟 Caddy、sshd 这类别的程序），返回端口。
func holdTCP(t *testing.T, host string) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

func socksInbound(port int, listen, settings string) *model.Inbound {
	return &model.Inbound{
		UserId: 1, Port: port, Protocol: model.Protocol("socks"), Enable: true,
		Tag:            "inbound-" + strconv.Itoa(port),
		Listen:         listen,
		Settings:       settings,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       "{}",
	}
}

// 端口已被本机别的程序（装域名时的 Caddy :443、sshd、面板自己）占着时，保存必须当场拒绝：
// 修复前只查库里有没有同端口的入站，保存成功之后热应用的 AddInbound 失败、退回整进程重启，
// 新核心因 bind: address already in use 起不来——机器上所有入站一起断。
func TestAddInboundRejectsPortHeldByAnotherProgram(t *testing.T) {
	setupDB(t)
	port := holdTCP(t, "127.0.0.1")
	err := (&InboundService{}).AddInbound(socksInbound(port, "127.0.0.1", `{"auth":"noauth","udp":false}`))
	if err == nil {
		t.Fatalf("端口 %d 已被别的程序占用，AddInbound 却接受了", port)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("报错应当点名端口 %d: %v", port, err)
	}
}

// 需要 UDP 的入站（socks 开了 udp）也要探 UDP：TCP 空着、UDP 被占时核心照样起不来这个入站。
func TestAddInboundRejectsUDPPortHeldByAnotherProgram(t *testing.T) {
	setupDB(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	port := pc.LocalAddr().(*net.UDPAddr).Port
	err = (&InboundService{}).AddInbound(socksInbound(port, "127.0.0.1", `{"auth":"noauth","udp":true}`))
	if err == nil {
		t.Fatalf("UDP 端口 %d 已被占用，开了 udp 的 socks 入站却保存成功", port)
	}
}

// 模板自己的入站（控制面 api）占着的端口：核心在跑时它被我们自己的 xray 占着，探测会被跳过，
// 所以要单独按模板拒绝。
func TestAddInboundRejectsTemplateInboundPort(t *testing.T) {
	setupDB(t)
	apiPort := freePort(t)
	useTemplateWithAPIPort(t, apiPort, nil)
	err := (&InboundService{}).AddInbound(socksInbound(apiPort, "", `{"auth":"noauth","udp":false}`))
	if err == nil {
		t.Fatalf("端口 %d 是模板里 api 入站的端口，AddInbound 却接受了", apiPort)
	}
}

func TestAddInboundRejectsPanelPort(t *testing.T) {
	setupDB(t)
	panelPort := freePort(t)
	if err := (&SettingService{}).SetPort(panelPort); err != nil {
		t.Fatal(err)
	}
	err := (&InboundService{}).AddInbound(socksInbound(panelPort, "", `{"auth":"noauth","udp":false}`))
	if err == nil {
		t.Fatalf("端口 %d 是面板自己的端口，AddInbound 却接受了", panelPort)
	}
	// 面板只听 127.0.0.1（装了 Caddy）时，入站听另一个具体地址不冲突。
	if err := (&SettingService{}).SetListen("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).AddInbound(socksInbound(panelPort, "::1", `{"auth":"noauth","udp":false}`)); err != nil {
		t.Fatalf("面板听 127.0.0.1、入站听 ::1，不该冲突: %v", err)
	}
}

// 只改备注、限速这类不碰监听的字段时不重新探测：入站在跑时它的端口正被我们自己的核心占着。
func TestUpdateInboundSkipsProbeWhenListenUnchanged(t *testing.T) {
	setupDB(t)
	port := holdTCP(t, "127.0.0.1")
	in := socksInbound(port, "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(in); err != nil {
		t.Fatal(err)
	}
	edit := *in
	edit.Remark = "改个备注"
	if err := (&InboundService{}).UpdateInbound(&edit); err != nil {
		t.Fatalf("只改备注却被端口探测拦下: %v", err)
	}
}

// 改了端口就要探测新端口。
func TestUpdateInboundProbesNewPort(t *testing.T) {
	setupDB(t)
	in := socksInbound(freePort(t), "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(in); err != nil {
		t.Fatal(err)
	}
	held := holdTCP(t, "127.0.0.1")
	edit := *in
	edit.Port = held
	if err := (&InboundService{}).UpdateInbound(&edit); err == nil {
		t.Fatalf("改到一个被占用的端口 %d，UpdateInbound 却接受了", held)
	}
}

// 重新启用一个停用的入站等于让它重新占端口，同样要探测。
func TestUpdateInboundProbesWhenReEnabling(t *testing.T) {
	setupDB(t)
	port := holdTCP(t, "127.0.0.1")
	in := socksInbound(port, "127.0.0.1", `{"auth":"noauth","udp":false}`)
	in.Enable = false
	if err := saveInboundRow(in); err != nil {
		t.Fatal(err)
	}
	edit := *in
	edit.Enable = true
	if err := (&InboundService{}).UpdateInbound(&edit); err == nil {
		t.Fatalf("端口 %d 已被占用，重新启用却被接受了", port)
	}
}

// 端口正被我们自己运行中的核心占着（运行配置里有入站在这个端口上）时不探测：热应用会先删后加，
// 探测必然撞上自己。假核心不真的监听，所以在它带着这个入站起来之后，由本用例占住端口来模拟
// 「被我们的核心占着」（先占的话，启动前的探测会如实把这个入站当成冲突摘掉）。
func TestProbeIsSkippedForPortsOfTheRunningCore(t *testing.T) {
	setupDB(t)
	resetPortConflictsForTest(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	port := freePort(t)
	in := socksInbound(port, "127.0.0.1", `{"auth":"noauth","udp":false}`)
	if err := saveInboundRow(in); err != nil {
		t.Fatal(err)
	}
	useFakeXray(t, "exec sleep 30")
	stopXrayAtCleanup(t)
	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitRunning(t, xs)
	if probes := unheldPart(inboundListenSpec{Listen: "127.0.0.1", Port: port, TCP: true}, heldListeners(runningConfigOf(currentProcess()))); len(probes) != 0 {
		t.Fatalf("前提不成立：运行配置里应当有端口 %d", port)
	}
	held, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })

	edit := *in
	edit.Settings = `{"auth":"noauth","udp":true}` // 监听变了（多了 UDP），会触发探测
	if err := (&InboundService{}).UpdateInbound(&edit); err != nil {
		t.Fatalf("端口 %d 正被自己的核心占着，探测应当跳过: %v", port, err)
	}
}

// saveInboundRow 直接落库，绕开 AddInbound 的校验：用来摆出「库里已经有这个入站」的前提。
func saveInboundRow(in *model.Inbound) error {
	return database.GetDB().Save(in).Error
}

// 生成侧：模板入站的监听规格取自它们自己的 listen/port。
func TestListenSpecOfTemplateAPIInbound(t *testing.T) {
	ib := &xray.InboundConfig{Listen: rawf(`"127.0.0.1"`), Port: 62789, Protocol: "dokodemo-door",
		Settings: rawf(`{"address":"127.0.0.1"}`), Tag: "api"}
	spec, ok := listenSpecOf(ib)
	if !ok || spec.Listen != "127.0.0.1" || spec.Port != 62789 || !spec.TCP || spec.UDP {
		t.Fatalf("api 入站的监听规格 = %+v ok=%v", spec, ok)
	}
	if _, ok := listenSpecOf(&xray.InboundConfig{Listen: rawf(`"/run/xray.sock"`), Port: 0, Protocol: "vless"}); ok {
		t.Fatal("unix socket 监听没有端口，不该参与端口判定")
	}
}

// v2-ui 迁移（AddInbounds）修复前只查端口：核心不认的旧配置直接写进库，下次整进程重启时整份配置
// 加载失败；同一批里两个同端口的入站也照样写进去。现在与 AddInbound 同一套校验，不过就整批不写。
func TestAddInboundsValidatesLikeAddInbound(t *testing.T) {
	setupDB(t)
	a, b := freePort(t), freePort(t)
	// 假核心只拒绝带着入站 a 的配置：不带它的基线照常通过，否则会按「改动前就已经不合法」放行。
	fakeXrayBinary(t, fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo 'Xray 26.7.28 (fake)'; exit 0; fi\n"+
		"if grep -q '\"port\":%d' \"$4\"; then echo 'Failed to start: infra/conf: rejected by fake core'; exit 23; fi\n"+
		"echo 'Configuration OK.'\n", a), 0o755)
	countRows := func() int64 {
		var n int64
		database.GetDB().Model(&model.Inbound{}).Count(&n)
		return n
	}

	err := (&InboundService{}).AddInbounds([]*model.Inbound{
		socksInbound(a, "127.0.0.1", `{"auth":"noauth","udp":false}`),
		socksInbound(b, "127.0.0.1", `{"auth":"noauth","udp":false}`),
	})
	if err == nil {
		t.Fatal("核心拒绝这份入站时 AddInbounds 应当报错")
	}
	if !strings.Contains(err.Error(), "第 1 个入站") {
		t.Errorf("报错应当点名是第几个入站: %v", err)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("校验没过却写进了 %d 个入站", n)
	}

	dup := freePort(t)
	err = (&InboundService{}).AddInbounds([]*model.Inbound{
		socksInbound(dup, "127.0.0.1", `{"auth":"noauth","udp":false}`),
		socksInbound(dup, "127.0.0.1", `{"auth":"noauth","udp":false}`),
	})
	if err == nil || !strings.Contains(err.Error(), "端口相同") {
		t.Fatalf("同一批里两个同端口的入站应当拒绝，实际 %v", err)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("同批端口重复却写进了 %d 个入站", n)
	}
}
