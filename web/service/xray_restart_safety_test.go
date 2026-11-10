package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// useTemplateWithAPIPort 把模板里 api 入站的端口换成 apiPort 存进设置，并返回存进去的模板。
//
// 本包其余真实 xray 的用例都用模板写死的 62789；同机并行跑多个面板测试时它是最常见的撞车点。
// 新用例一律走临时端口。mutate 非 nil 时可以在存之前再改模板（例如默认出站的 settings）。
func useTemplateWithAPIPort(t *testing.T, apiPort int, mutate func(tpl map[string]any)) map[string]any {
	t.Helper()
	var tpl map[string]any
	if err := json.Unmarshal([]byte(xrayTemplateConfig), &tpl); err != nil {
		t.Fatalf("解析默认模板: %v", err)
	}
	inbounds, _ := tpl["inbounds"].([]any)
	found := false
	for _, raw := range inbounds {
		ib, _ := raw.(map[string]any)
		if ib != nil && ib["tag"] == xray.APIInboundTag {
			ib["port"] = apiPort
			found = true
		}
	}
	if !found {
		t.Fatal("默认模板里没有 api 入站")
	}
	if mutate != nil {
		mutate(tpl)
	}
	data, err := json.Marshal(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&SettingService{}).saveSetting("xrayTemplateConfig", string(data)); err != nil {
		t.Fatalf("保存模板: %v", err)
	}
	return tpl
}

// useFakeXray 把工作目录切到一个临时目录，里面的 bin/xray-* 是个 shell 脚本：收到 -version
// 打印版本，否则执行 body。与 xray 包的 fakeXray 同一个做法，这里不碰仓库里的 bin/。
func useFakeXray(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// run -test 一律答 Configuration OK：保存入站时的真实 xray 校验也会调到它，不答就等满超时。
	script := "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo 'Xray 26.7.28 (fake)'; exit 0; fi\n" +
		"if [ \"$1\" = \"run\" ] && [ \"$2\" = \"-test\" ]; then echo 'Configuration OK.'; exit 0; fi\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, xray.GetBinaryPath()), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

// stopXrayAtCleanup 在用例结束时停掉本用例拉起的核心并清掉包级状态。
func stopXrayAtCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})
}

// 新配置写不下去（磁盘满、只读、路径被占成目录……）时，正在跑的核心必须留着。
// 修复前 RestartXray 先 p.Stop() 再让 Start 去写 bin/config.json，写失败时旧核心已经被杀，
// 新核心又起不来——一次写失败就是整机断网。
func TestRestartXrayKeepsRunningCoreWhenConfigCannotBeWritten(t *testing.T) {
	setupDB(t)
	useTemplateWithAPIPort(t, freePort(t), nil)
	dir := useFakeXray(t, "exec sleep 30")
	stopXrayAtCleanup(t)

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("首次 RestartXray(true): %v", err)
	}
	waitRunning(t, xs)
	before := currentProcess()

	// 把 bin/config.json 换成一个目录：改名覆盖与原地写都必然失败。
	cfgPath := filepath.Join(dir, xray.GetConfigPath())
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfgPath, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := xs.RestartXray(true); err == nil {
		t.Fatal("bin/config.json 写不下去时 RestartXray 应当报错")
	}
	if currentProcess() != before {
		t.Fatal("写配置失败后进程对象被换掉了，旧核心已经不归面板管")
	}
	if !xs.IsXrayRunning() {
		t.Fatal("写配置失败后旧核心被停掉了：整机断网，而新配置根本没写下去")
	}
}

// resetPendingTrafficForTest 清掉包级的入站用量暂存：它跨用例存活，上一个用例故意制造的
// 写库失败会把增量留给下一个用例。
func resetPendingTrafficForTest(t *testing.T) {
	t.Helper()
	pendingTrafficMu.Lock()
	pendingTraffic = nil
	pendingTrafficMu.Unlock()
	t.Cleanup(func() {
		pendingTrafficMu.Lock()
		pendingTraffic = nil
		pendingTrafficMu.Unlock()
	})
}

func waitRunning(t *testing.T, xs *XrayService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !xs.IsXrayRunning() {
		if time.Now().After(deadline) {
			t.Fatal("等待核心进入运行状态超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 主库写入失败时，这一轮拉到的入站增量不能丢：GetTraffic(reset=true) 已经把核心侧清零了，
// 丢了就是用户这 10 秒的用量凭空消失（超额不被停用）。下一轮写成功时要把两轮一起补上。
func TestAddTrafficKeepsDeltasWhenMainDBWriteFails(t *testing.T) {
	setupDB(t)
	resetPendingTrafficForTest(t)
	in := &model.Inbound{UserId: 1, Port: 20811, Protocol: model.VLESS, Enable: true, Tag: "inbound-20811",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}"}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	if err := db.Exec(`CREATE TRIGGER fail_traffic BEFORE UPDATE OF up ON inbounds
		BEGIN SELECT RAISE(ABORT, 'disk I/O error (simulated)'); END`).Error; err != nil {
		t.Fatalf("建触发器: %v", err)
	}

	svc := &InboundService{}
	err := svc.AddTraffic([]*xray.Traffic{{IsInbound: true, Tag: in.Tag, Up: 100, Down: 1000}})
	if err == nil {
		t.Fatal("主库写失败时 AddTraffic 应当报错")
	}

	if err := db.Exec(`DROP TRIGGER fail_traffic`).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTraffic([]*xray.Traffic{{IsInbound: true, Tag: in.Tag, Up: 7, Down: 70}}); err != nil {
		t.Fatalf("第二轮 AddTraffic: %v", err)
	}
	got, err := svc.GetInbound(in.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Up != 107 || got.Down != 1070 {
		t.Fatalf("up/down = %d/%d，期望 107/1070（失败那一轮的增量要在下一轮补上）", got.Up, got.Down)
	}

	// 补上之后暂存必须清空，否则下一轮会再加一遍。
	if err := svc.AddTraffic([]*xray.Traffic{{IsInbound: true, Tag: in.Tag, Up: 1, Down: 1}}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetInbound(in.Id)
	if got.Up != 108 || got.Down != 1071 {
		t.Fatalf("up/down = %d/%d，期望 108/1071（补过的增量不能重复累加）", got.Up, got.Down)
	}
}

// 整进程重启会清零核心里的计数器。重启前不最后拉一次的话，距上次采集（最多 10 秒）内
// 所有用户的用量全部丢失——每次改配置都是一次。
func TestRestartXrayDrainsTrafficBeforeStopping(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	resetPendingTrafficForTest(t)
	apiPort := freePort(t)
	useTemplateWithAPIPort(t, apiPort, nil)
	stopXrayAtCleanup(t)

	socksPort := freePort(t)
	in := &model.Inbound{
		UserId: 1, Port: socksPort, Protocol: model.Protocol("socks"), Enable: true,
		Tag:            "inbound-" + strconv.Itoa(socksPort),
		Listen:         "127.0.0.1",
		Settings:       `{"auth":"noauth","udp":false}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatal(err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, socksPort)
	waitForPort(t, apiPort)

	// 经这个入站送一段数据，要的只是入站计数器上有数：目标是一个只读不回的本地服务，
	// 被私网守卫拦下时 freedom 同样把上行读完丢弃，两种情况都计入入站上行。
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	go func() {
		for {
			c, err := sink.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	payload := make([]byte, 64<<10)
	sendThroughSocks(t, fmt.Sprintf("127.0.0.1:%d", socksPort), "localhost",
		sink.Addr().(*net.TCPAddr).Port, payload)
	time.Sleep(300 * time.Millisecond)

	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("第二次 RestartXray(true): %v", err)
	}
	got, err := (&InboundService{}).GetInbound(in.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Up < int64(len(payload)) {
		t.Fatalf("重启后入站 up = %d，期望至少 %d：重启前没有最后拉一次计数器，这段用量丢了", got.Up, len(payload))
	}
}

// sendThroughSocks 经 socks5（域名寻址）连到 host:port 并写出 payload，不关心对端是否回包。
func sendThroughSocks(t *testing.T, proxyAddr, host string, port int, payload []byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连 socks 入站: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		t.Fatalf("socks greeting: %v", err)
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 10)
	if _, err := io.ReadFull(c, head); err != nil || head[1] != 0x00 {
		t.Fatalf("socks CONNECT: %v % x", err, head)
	}
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("写 payload: %v", err)
	}
}
