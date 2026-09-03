package service

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"a-ui/util/geodat"
	"a-ui/xray"
)

// 拿真实 xray 验证地区限制的核心机制：dat 由本项目的编码器生成，
// 路由条件写成 ext:<文件>:!<TAG>，来源在允许集内放行、不在则黑洞。
//
// 这条测试守的是三件事，任何一件不成立地区限制都会静默失效：
//  1. 手写的 protobuf 编码 xray 真的能读懂；
//  2. ! 取反语法真的生效（而不是被当成普通 tag 而永不命中）；
//  3. dat 放在 bin/ 下、配置里不带目录前缀引用，xray 能找到文件。
//
// 不依赖外网：目标是本进程起的一个 TCP 监听。
func TestGeoRestrictionAgainstRealXray(t *testing.T) {
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

	// dat 必须落在 xray 二进制所在目录（bin/），配置里按文件名引用。
	datPath := filepath.Join("bin", "a-ui-geo-e2e.dat")
	t.Cleanup(func() { _ = os.Remove(datPath) })

	run := func(t *testing.T, allowCIDR string) string {
		t.Helper()
		data, err := geodat.Encode([]geodat.Entry{{Tag: "ALLOWE2E", CIDRs: []string{allowCIDR}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(datPath, data, 0o644); err != nil {
			t.Fatal(err)
		}

		socksPort := freePort(t)
		cfg := map[string]any{
			"log": map[string]any{"loglevel": "warning"},
			"inbounds": []any{map[string]any{
				"tag": "in", "listen": "127.0.0.1", "port": socksPort, "protocol": "socks",
				"settings": map[string]any{"auth": "noauth", "udp": false},
			}},
			"outbounds": []any{
				map[string]any{"tag": "direct", "protocol": "freedom"},
				map[string]any{"tag": "a-ui-block", "protocol": "blackhole", "settings": map[string]any{}},
			},
			"routing": map[string]any{"rules": []any{map[string]any{
				"type": "field", "ruleTag": "a-ui-geo-e2e", "inboundTag": []string{"in"},
				"source": []string{"ext:a-ui-geo-e2e.dat:!ALLOWE2E"}, "outboundTag": "a-ui-block",
			}}},
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

		return socksRead(fmt.Sprintf("127.0.0.1:%d", socksPort), "127.0.0.1", targetPort)
	}

	t.Run("来源在允许集内则放行", func(t *testing.T) {
		if got := run(t, "127.0.0.0/8"); got != "HELLO" {
			t.Errorf("读到 %q，期望 HELLO——允许集内的来源被误挡了", got)
		}
	})

	t.Run("来源不在允许集内则拒绝", func(t *testing.T) {
		// 注意判据是「读不到数据」而不是「连接失败」：黑洞出站会让连接
		// 建立成功但没有任何数据，用返回码判断会得出相反的结论。
		if got := run(t, "8.8.8.0/24"); got != "" {
			t.Errorf("读到 %q，期望空——允许集外的来源应当被黑洞", got)
		}
	})
}

// socksRead 通过 socks5 连到目标并读回内容，读不到就返回空串。
func socksRead(proxyAddr, host string, port int) string {
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
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return ""
	}
	if _, err := c.Read(make([]byte, 10)); err != nil {
		return ""
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		return ""
	}
	return string(buf[:n])
}
