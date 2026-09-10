package service

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"path/filepath"
	"testing"
	"time"

	"a-ui/xray"
)

// TestMeterRuleShapeAgainstRealXray 把 spec §6.1 的交叉实验固化成回归测试。
//
// 守的是一件事：**计量规则不能屏蔽 IP 规则的第二遍**。xray 在 IPIfNonMatch
// 下走两遍规则（app/router/router.go:245-273），第二遍只在第一遍一条都没命中
// 时才发生。计量规则带 domain 条件，会参与并可能命中第一遍——一旦命中，第二遍
// 永远不会发生，模板自带的 geoip:private 与管理员所有 CIDR 规则对池内域名
// 静默失效。加上 ip: ["0.0.0.0/0","::/0"] 守卫可以修好它：守卫匹配任意 IP，
// 但要求目标已经有 IP，域名目标在第一遍没有。
//
// 判据：默认出站是黑洞、计量出站是 freedom，所以「读到数据」就等价于
// 「计量规则命中了」。目标 meter.test 由 dns.hosts 指到 127.0.0.1，正好落在
// geoip:private 覆盖的范围里。
func TestMeterRuleShapeAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)

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
			_, _ = c.Write([]byte("HELLO"))
			c.Close()
		}
	}()
	targetPort := target.Addr().(*net.TCPAddr).Port

	run := func(t *testing.T, strategy string, withPrivateRule, guarded bool) string {
		t.Helper()
		socksPort := freePort(t)

		meterRule := map[string]any{
			"type":        "field",
			"inboundTag":  []string{"in"},
			"domain":      []string{"domain:meter.test"},
			"outboundTag": "a-ui-meter-1-meter.test",
		}
		if guarded {
			meterRule["ip"] = []string{"0.0.0.0/0", "::/0"}
		}
		rules := []any{}
		if withPrivateRule {
			// 与 web/service/config.json 模板里那条一模一样。
			rules = append(rules, map[string]any{
				"type": "field", "ip": []string{"geoip:private"}, "outboundTag": "a-ui-block",
			})
		}
		rules = append(rules, meterRule)

		routing := map[string]any{"rules": rules}
		if strategy != "" {
			routing["domainStrategy"] = strategy
		}
		cfg := map[string]any{
			"log": map[string]any{"loglevel": "warning"},
			"dns": map[string]any{
				"hosts":   map[string]any{"meter.test": "127.0.0.1"},
				"servers": []any{"localhost"},
			},
			"inbounds": []any{map[string]any{
				"tag": "in", "listen": "127.0.0.1", "port": socksPort, "protocol": "socks",
				"settings": map[string]any{"auth": "noauth", "udp": false},
			}},
			"outbounds": []any{
				// 默认出站故意做成黑洞：这样「读到数据」就只可能来自计量出站。
				map[string]any{"tag": "a-ui-default", "protocol": "blackhole", "settings": map[string]any{}},
				map[string]any{"tag": "a-ui-block", "protocol": "blackhole", "settings": map[string]any{}},
				map[string]any{"tag": "a-ui-meter-1-meter.test", "protocol": "freedom",
					"settings": map[string]any{"domainStrategy": "UseIP"}},
			},
			"routing": routing,
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, encoded, 0o644); err != nil {
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

		return socksReadDomain(fmt.Sprintf("127.0.0.1:%d", socksPort), "meter.test", targetPort)
	}

	t.Run("两遍匹配下不带守卫会绕过私网封禁", func(t *testing.T) {
		// 危险本身。计量规则在第一遍命中，第二遍永远不发生，
		// geoip:private 对这个域名完全失效——流量照常送达。
		if got := run(t, "IPIfNonMatch", true, false); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO——这条用例复现的是危险本身，"+
				"它不成立说明整个前提变了，先回去核对 xray 的两遍匹配逻辑", got)
		}
	})

	t.Run("两遍匹配下带守卫则封禁生效", func(t *testing.T) {
		// 守卫让计量规则在第一遍必然不命中，第二遍 geoip:private 先命中，
		// 行为与「没有计量功能」时逐字节一致。
		if got := run(t, "IPIfNonMatch", true, true); got != "" {
			t.Errorf("读到 %q，期望空——带守卫时私网封禁必须照常生效，"+
				"否则计量会静默关掉管理员所有的 IP 段规则", got)
		}
	})

	t.Run("两遍匹配下带守卫不妨碍计量", func(t *testing.T) {
		// 没有别的规则挡路时，守卫形态在第二遍照常命中计量出站。
		if got := run(t, "IPIfNonMatch", false, true); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO——守卫不该让计量本身失效", got)
		}
	})

	t.Run("单遍匹配下守卫恒假", func(t *testing.T) {
		// 这条用例解释了形态为什么必须按 domainStrategy 二选一：AsIs 下
		// 没有第二遍，守卫永远拿不到 IP，计量规则永不命中，流量回落默认出站
		//（这里是黑洞，所以读不到数据）。
		if got := run(t, "", false, true); got != "" {
			t.Errorf("读到 %q，期望空——单遍匹配下守卫恒假，必须改用纯 domain 形态", got)
		}
	})

	t.Run("单遍匹配下纯形态照常计量", func(t *testing.T) {
		if got := run(t, "", false, false); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO", got)
		}
	})
}

// socksReadDomain 通过 socks5 以**域名**形式连到目标并读回内容，读不到就返回空串。
//
// 与 geo_e2e_test.go 里的 socksRead 的唯一区别是地址类型用 0x03（域名）而不是
// 0x01（IPv4）——计量规则匹配的正是域名，用 IP 字面量发起的话 domain 条件永不命中。
func socksReadDomain(proxyAddr, host string, port int) string {
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return ""
	}
	if _, err := c.Read(make([]byte, 2)); err != nil {
		return ""
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return ""
	}
	if _, err := c.Read(make([]byte, 32)); err != nil {
		return ""
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		return ""
	}
	return string(buf[:n])
}

// IP 计量规则与 IPv6 的计量 tag 必须能被真实 xray 接受。
//
// IPv6 那条是设计 §4.4 标记的未验证假设：tag 会含冒号
//（a-ui-meter-7-2001:db8::1）。xray 对 tag 字符集很宽松（含中文都
// Configuration OK），但冒号此前没有实测过。这条测试就是那个假设的验收。
//
// 若它失败，**不要自行改 tag 形态**：退路是对 IPv6 做一次确定性转写，而
// 绝不能把冒号换成短横线——ParseMeterTag 按第一个短横线切分，那会让反查
// 静默错位（把 a-ui-meter-7-2001-db8--1 反查成入站 7、目标 "2001-db8--1"，
// 与池表里的键对不上，字节永远归不进去且没有任何一层会报错）。
func TestIPMeterRulesAreAcceptedByRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupMeterPoolTest(t)
	in := newTestInbound(t, 32021)
	putPoolRow(t, in.Id, "72.235.209.83", 0)
	putPoolRow(t, in.Id, "2001:db8::1", 0)
	putPoolRow(t, in.Id, "acspubs.org", 0)
	// 打开两遍匹配：域名规则会带守卫、IP 规则不带，两种形态同时送检。
	if err := (&SettingService{}).setString("ipRuleResolveDomain", "1"); err != nil {
		t.Fatalf("setString: %v", err)
	}

	data, err := generatedConfigJSON()
	if err != nil {
		t.Fatalf("生成配置: %v", err)
	}
	path := filepath.Join(t.TempDir(), "meter-ip.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写临时配置: %v", err)
	}

	out, err := exec.Command(xray.GetBinaryPath(), "run", "-test", "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("真实 xray 拒绝了含 IP 计量规则的配置: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Configuration OK") {
		t.Fatalf("xray 没有给出 Configuration OK，输出：\n%s", out)
	}
	t.Logf("xray 接受了含 IPv4/IPv6 计量 tag 的配置：%s", strings.TrimSpace(string(out)))
}
