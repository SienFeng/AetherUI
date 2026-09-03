package service

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"a-ui/database/model"
	"a-ui/util/accesslog"
	"a-ui/xray"
)

// 拿真实的 xray 产出日志，走完「注入配置 → xray 写日志 → 解析 → 入库」
// 整条链路。解析器的单测用的是抄录下来的样本，这里验证的是样本没抄错、
// 而且这一版 xray 的输出格式没变。
//
// 不依赖外网：目标是本进程起的一个 TCP 监听。
func TestAccessLogPipelineAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupAccessLogDB(t)

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	targetPort := target.Addr().(*net.TCPAddr).Port

	socksPort := freePort(t)
	inboundTag := fmt.Sprintf("inbound-%d", socksPort)
	in := mkInbound(t, socksPort)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	// 用 injectAccessLog 生成 log 段，而不是手写——这样测的是生产代码。
	cfg := &xray.Config{}
	if err := injectAccessLog(cfg, true, logPath); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{
		"log": json.RawMessage(cfg.LogConfig),
		"inbounds": []any{map[string]any{
			"tag": inboundTag, "listen": "127.0.0.1", "port": socksPort, "protocol": "socks",
			"settings": map[string]any{"auth": "noauth", "udp": false},
		}},
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom"},
			map[string]any{"tag": "a-ui-block", "protocol": "blackhole"},
		},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(xray.GetBinaryPath(), "run", "-c", cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 xray: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	waitForPort(t, socksPort)

	// 通过 socks 连一次目标，制造一条访问记录。
	if err := socksConnect(fmt.Sprintf("127.0.0.1:%d", socksPort), "127.0.0.1", targetPort); err != nil {
		t.Fatalf("经 socks 连接目标: %v", err)
	}

	lines := waitForLogLines(t, logPath)

	var entries []accesslog.Entry
	for _, line := range lines {
		e, ok := accesslog.ParseLine(line, time.Local)
		if !ok {
			t.Fatalf("真实 xray 日志行解析失败，格式可能变了:\n%s", line)
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		t.Fatal("没有解析出任何记录")
	}

	e := entries[0]
	if e.Inbound != inboundTag {
		t.Errorf("Inbound = %q，期望 %q", e.Inbound, inboundTag)
	}
	if e.SourceIP != "127.0.0.1" {
		t.Errorf("SourceIP = %q，期望 127.0.0.1", e.SourceIP)
	}
	if want := fmt.Sprintf("127.0.0.1:%d", targetPort); e.Target != want {
		t.Errorf("Target = %q，期望 %q", e.Target, want)
	}
	if e.Route == "" {
		t.Error("Route 为空，「走的什么路由」这一列会是空的")
	}

	s := AccessLogService{}
	if _, err := s.Store(entries); err != nil {
		t.Fatalf("Store: %v", err)
	}
	result, err := s.GetAccessLogs(AccessLogQuery{InboundId: in.Id, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("GetAccessLogs: %v", err)
	}
	if result.Total == 0 {
		t.Fatal("入库后查不到记录，tag 到 inbound id 的解析可能不对")
	}
	var row model.AccessLog
	for _, r := range result.List {
		if strings.HasPrefix(r.Target, "127.0.0.1:") {
			row = r
		}
	}
	if row.InboundId != in.Id {
		t.Errorf("InboundId = %d，期望 %d", row.InboundId, in.Id)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待端口 %d 超时", port)
}

func waitForLogLines(t *testing.T, path string) []string {
	t.Helper()
	tl := &accesslog.Tailer{Path: path}
	for i := 0; i < 100; i++ {
		lines, err := tl.Read(1 << 20)
		if err != nil {
			t.Fatalf("读取日志: %v", err)
		}
		if len(lines) > 0 {
			return lines
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待访问日志写入超时")
	return nil
}

// socksConnect 走一次最小的 SOCKS5 握手 + CONNECT，只为让 xray 记一条日志。
func socksConnect(proxyAddr, host string, port int) error {
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := c.Read(resp); err != nil {
		return err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("socks 握手失败: %v", resp)
	}

	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	reply := make([]byte, 10)
	if _, err := c.Read(reply); err != nil {
		return err
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("socks CONNECT 被拒: %v", reply[1])
	}
	return nil
}
