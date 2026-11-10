package service

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	statsservice "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"a-ui/database"
	"a-ui/database/model"
)

const e2eVlessUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// startEchoServer 起一个本地回显服务，返回端口。
func startEchoServer(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// echoRoundTrip 往 c 写 "ping" 并在 2 秒内读回，返回读到的内容（被拦时为空）。
func echoRoundTrip(c net.Conn) string {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return ""
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	n, _ := io.ReadFull(c, buf)
	return string(buf[:n])
}

// socks5Dial 经 socks5（域名寻址，即 socks5h）连到 host:port，返回 CONNECT 成功之后的连接。
func socks5Dial(proxyAddr, host string, port int) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	fail := func(err error) (net.Conn, error) { c.Close(); return nil, err }
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fail(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil {
		return fail(err)
	}
	req := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return fail(err)
	}
	head := make([]byte, 10)
	if _, err := io.ReadFull(c, head); err != nil {
		return fail(err)
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("CONNECT 被拒绝, REP=0x%02x", head[1]))
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// vlessDial 是一个最小的 VLESS 客户端（无 TLS、无 flow）：写请求头后返回连接，响应头由
// echoRoundTrip 读回显时一并跳过——这里只比较「读没读到数据」，所以回显前多出的两个字节
// （版本与附加信息长度）不影响判定。
func vlessDial(addr, host string, port int) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	id, _ := hex.DecodeString(strings.ReplaceAll(e2eVlessUUID, "-", ""))
	req := []byte{0x00}
	req = append(req, id...)
	req = append(req, 0x00, 0x01, byte(port>>8), byte(port), 0x02, byte(len(host)))
	req = append(req, host...)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// getSysStats 经 dial 建立的连接调一次 xray 控制面的 StatsService.GetSysStats。
func getSysStats(dial func() (net.Conn, error)) error {
	conn, err := grpc.NewClient("passthrough:///xray-api",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return dial() }),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = statsservice.NewStatsServiceClient(conn).GetSysStats(ctx, &statsservice.SysStatsRequest{})
	return err
}

// allowPrivateInDefaultOutbound 给模板的默认出站写一条开头的 {"action":"allow"}——管理员关掉
// 私网守卫的写法（守卫追加在管理员规则之后，xray 取第一条命中的）。只给探测目标只能是本机回环
// 服务、验证的又不是守卫本身的用例用。
func allowPrivateInDefaultOutbound(tpl map[string]any) {
	obs, _ := tpl["outbounds"].([]any)
	if len(obs) == 0 {
		return
	}
	if first, ok := obs[0].(map[string]any); ok {
		first["settings"] = map[string]any{"finalRules": []any{map[string]any{"action": "allow"}}}
	}
}

func saveE2EInbound(t *testing.T, protocol string, port int, settings string) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.Protocol(protocol), Enable: true,
		Tag:            "inbound-" + strconv.Itoa(port),
		Listen:         "127.0.0.1",
		Settings:       settings,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save %s inbound: %v", protocol, err)
	}
	return in
}

// 用真实 xray 证明私网守卫的四个结论（改动前审计实测：一个 socks 用户经 localhost 调通了
// 控制面的 GetSysStats）：
//   - socks 用户经主机名再也碰不到控制面与本机服务；
//   - dokodemo 转发到 localhost 主机名照常能用（管理员亲手配的目标）；
//   - dokodemo 转发到 127.0.0.1 字面量与改动前一样被模板的 geoip:private 规则拦着——
//     「dokodemo → 127.0.0.1 能通」从来就不成立，这里把现状钉住；
//   - vless 的结论不变（freedom 内置规则本来就拦）。
func TestFreedomPrivateGuardAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	apiPort := freePort(t)
	useTemplateWithAPIPort(t, apiPort, nil)
	stopXrayAtCleanup(t)

	echoPort := startEchoServer(t)
	socks := saveE2EInbound(t, "socks", freePort(t), `{"auth":"noauth","udp":false}`)
	dokoHost := saveE2EInbound(t, "dokodemo-door", freePort(t),
		fmt.Sprintf(`{"address":"localhost","port":%d,"network":"tcp"}`, echoPort))
	dokoIP := saveE2EInbound(t, "dokodemo-door", freePort(t),
		fmt.Sprintf(`{"address":"127.0.0.1","port":%d,"network":"tcp"}`, echoPort))
	vless := saveE2EInbound(t, "vless", freePort(t),
		`{"clients":[{"id":"`+e2eVlessUUID+`","flow":""}],"decryption":"none"}`)

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	for _, port := range []int{apiPort, socks.Port, dokoHost.Port, dokoIP.Port, vless.Port} {
		waitForPort(t, port)
	}
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socks.Port)

	t.Run("对照：直连控制面能调通", func(t *testing.T) {
		err := getSysStats(func() (net.Conn, error) {
			return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", apiPort), 3*time.Second)
		})
		if err != nil {
			t.Fatalf("直连控制面 GetSysStats 失败，探测手段本身不可靠: %v", err)
		}
	})

	t.Run("socks用户经localhost碰不到控制面", func(t *testing.T) {
		err := getSysStats(func() (net.Conn, error) { return socks5Dial(socksAddr, "localhost", apiPort) })
		if err == nil {
			t.Fatal("socks 用户经 localhost 调通了 xray 控制面的 GetSysStats：私网守卫没有生效")
		}
	})

	t.Run("socks用户经localhost碰不到本机服务", func(t *testing.T) {
		c, err := socks5Dial(socksAddr, "localhost", echoPort)
		if err != nil {
			t.Fatalf("socks 握手失败（入站本身坏了，不是守卫生效）: %v", err)
		}
		defer c.Close()
		if got := echoRoundTrip(c); got != "" {
			t.Fatalf("socks 用户经 localhost 读到了本机服务的回显 %q", got)
		}
	})

	t.Run("dokodemo转发到localhost照常能用", func(t *testing.T) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", dokoHost.Port), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if got := echoRoundTrip(c); got != "ping" {
			t.Fatalf("dokodemo → localhost 读到 %q，期望 ping：守卫误伤了管理员配的端口转发", got)
		}
	})

	t.Run("dokodemo转发到127.0.0.1与改动前一样被模板规则拦着", func(t *testing.T) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", dokoIP.Port), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if got := echoRoundTrip(c); got != "" {
			t.Fatalf("dokodemo → 127.0.0.1 读到 %q：模板的 geoip:private 规则一直拦着它，结论不该变", got)
		}
	})

	t.Run("vless经localhost与改动前一样被拦", func(t *testing.T) {
		c, err := vlessDial(fmt.Sprintf("127.0.0.1:%d", vless.Port), "localhost", echoPort)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if got := echoRoundTrip(c); got != "" {
			t.Fatalf("vless 用户经 localhost 读到了 %q：freedom 内置的私网拦截本来就覆盖 vless", got)
		}
	})
}
