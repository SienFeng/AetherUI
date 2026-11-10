package service

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"a-ui/util/accesslog"
	"a-ui/xray"
)

// xray 的访问日志时间是不带时区的钟面时间。面板经 Process.Start 拉起的核心必须按 UTC 写，
// 采集按 UTC 解析：按本地时间写的话，夏令时结束那天重复的那一小时里，两遍钟面时间完全
// 相同，解析出来的时刻整整早一小时，域名统计与异常扫描的 (time, id) 游标会把它们跳过。
//
// 面板进程这里故意设成东八区：子进程默认继承环境变量，不显式给 TZ=UTC 的话核心就按
// 东八区写，按 UTC 解析会落到 8 小时之后——与开发机自己在哪个时区无关，这条测试都红得了。
func TestXrayStartedByPanelWritesAccessLogInUTC(t *testing.T) {
	requireXrayBinary(t)
	t.Setenv("TZ", "Asia/Shanghai")

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
	logPath := filepath.Join(t.TempDir(), "access.log")
	logConfig, err := json.Marshal(map[string]string{"access": logPath, "loglevel": "warning"})
	if err != nil {
		t.Fatal(err)
	}
	listen, _ := json.Marshal("127.0.0.1")
	cfg := &xray.Config{
		LogConfig: logConfig,
		InboundConfigs: []xray.InboundConfig{{
			Listen: listen, Port: socksPort, Protocol: "socks",
			Settings: []byte(`{"auth":"noauth","udp":false}`),
			Tag:      fmt.Sprintf("inbound-%d", socksPort),
		}},
		OutboundConfigs: []byte(`[{"tag":"direct","protocol":"freedom"}]`),
	}
	proc := xray.NewProcess(cfg)
	if err := proc.Start(); err != nil {
		t.Fatalf("启动 xray: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })
	waitForPort(t, socksPort)

	before := time.Now()
	if err := socksConnect(fmt.Sprintf("127.0.0.1:%d", socksPort), "127.0.0.1", targetPort); err != nil {
		t.Fatalf("经 socks 连接目标: %v", err)
	}
	lines := waitForLogLines(t, logPath)
	e, ok := accesslog.ParseLine(lines[0], time.UTC)
	if !ok {
		t.Fatalf("解析失败: %q", lines[0])
	}
	// 秒级的容差足够：日志行是这一次连接写下的，唯一可能的大偏差就是整数个小时的时区差。
	if d := e.Time.Sub(before); d < -time.Minute || d > time.Minute {
		t.Fatalf("按 UTC 解析的日志时间 %v 与实际时刻 %v 相差 %v——核心没有按 UTC 写日志", e.Time, before.UTC(), d)
	}
}

// http 入站的访问记录写的是请求 URL：CONNECT 是 "//host:port"，普通代理请求是
// "http://host:port/path"。解析器把两者都归一成 host:port，这里用真实核心确认样本没抄错。
func TestHTTPProxyAccessLogTargetsAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer site.Close()
	siteAddr := site.Listener.Addr().String()

	httpPort := freePort(t)
	logPath := filepath.Join(t.TempDir(), "access.log")
	logConfig, err := json.Marshal(map[string]string{"access": logPath, "loglevel": "warning"})
	if err != nil {
		t.Fatal(err)
	}
	listen, _ := json.Marshal("127.0.0.1")
	cfg := &xray.Config{
		LogConfig: logConfig,
		InboundConfigs: []xray.InboundConfig{{
			Listen: listen, Port: httpPort, Protocol: "http",
			Settings: []byte(`{}`),
			Tag:      fmt.Sprintf("inbound-%d", httpPort),
		}},
		OutboundConfigs: []byte(`[{"tag":"direct","protocol":"freedom"}]`),
	}
	proc := xray.NewProcess(cfg)
	if err := proc.Start(); err != nil {
		t.Fatalf("启动 xray: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })
	waitForPort(t, httpPort)

	proxy := fmt.Sprintf("127.0.0.1:%d", httpPort)
	for _, req := range []string{
		"CONNECT " + siteAddr + " HTTP/1.1\r\nHost: " + siteAddr + "\r\n\r\n",
		"GET http://" + siteAddr + "/index.html?q=1 HTTP/1.1\r\nHost: " + siteAddr + "\r\nConnection: close\r\n\r\n",
	} {
		if err := rawHTTPProxyRequest(proxy, req); err != nil {
			t.Fatalf("经 http 代理发请求: %v", err)
		}
	}

	var entries []accesslog.Entry
	tl := &accesslog.Tailer{Path: logPath}
	for i := 0; i < 100 && len(entries) < 2; i++ {
		lines, err := tl.Read(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			e, ok := accesslog.ParseLine(line, time.UTC)
			if !ok {
				t.Fatalf("真实 http 入站的日志行解析失败:\n%s", line)
			}
			entries = append(entries, e)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(entries) < 2 {
		t.Fatalf("只等到 %d 条访问记录，期望 CONNECT 与 GET 各一条", len(entries))
	}
	for _, e := range entries {
		if e.Target != siteAddr {
			t.Errorf("Target = %q，期望归一成 %q", e.Target, siteAddr)
		}
	}
}

// rawHTTPProxyRequest 向 http 代理发一个原始请求，读到响应行即返回。
func rawHTTPProxyRequest(proxy, req string) error {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, " 200 ") {
		return fmt.Errorf("代理返回 %q", strings.TrimSpace(line))
	}
	return nil
}
