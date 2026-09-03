package service

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// hotReloadAPIPort 是模板里固定写死的 api 入站端口（web/service/config.json）。
// 本机如果已经有一份运行中的 a-ui/xray 占着它，测试会在启动阶段就失败，
// 且失败信息（端口占用）与真正的热更新缺陷无法区分——动手之前先跳过并说明原因。
const hotReloadAPIPort = 62789

func requireXrayAPIPortFree(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hotReloadAPIPort))
	if err != nil {
		t.Skipf("端口 %d（xray 控制面 api 入站的固定端口）已被占用，本机可能已有一个 a-ui/xray 实例在跑，跳过: %v",
			hotReloadAPIPort, err)
	}
	_ = ln.Close()
}

func requirePgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep 不可用，无法通过父子进程关系定位 xray 的 PID，跳过")
	}
}

// xrayChildPID 返回当前测试进程刚拉起的 xray 子进程 PID。
//
// xray.Process 不对外暴露底层 *exec.Cmd（web/service 只是消费方，没必要拿到
// 强类型的进程句柄），只能从操作系统层面观察：RestartXray 内部直接用
// os/exec fork+exec bin/xray-*，子进程的父进程就是本测试二进制自身的 PID。
// 用 pgrep -P 精确定位，不需要遍历全部进程再按命令行模糊匹配，也不会被
// 开发机上其它无关的 xray 进程干扰。
//
// RestartXray 的重启路径先 Kill 旧进程再拉起新的，旧进程的回收发生在它那个
// cmd.Run() 所在的 goroutine 里、不受本函数控制，中间有一瞬间可能新旧两个
// 子进程都在，所以允许短暂重试直到只剩一个。
func xrayChildPID(t *testing.T) int {
	t.Helper()
	mypid := strconv.Itoa(os.Getpid())
	deadline := time.Now().Add(5 * time.Second)
	var lastOut string
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-P", mypid).Output()
		if err == nil {
			fields := strings.Fields(string(out))
			if len(fields) == 1 {
				if pid, convErr := strconv.Atoi(fields[0]); convErr == nil {
					return pid
				}
			}
			lastOut = string(out)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待恰好一个 xray 子进程超时（pgrep -P %s），最近一次输出=%q err=%v", mypid, lastOut, lastErr)
	return 0
}

// httpThroughSocksDomain 通过 SOCKS5 的域名寻址（ATYP=0x03）连到 proxyAddr，
// 让 xray 按域名而不是私网 IP 做路由判断——模板里的 geoip:private 规则会把
// 发往 127.0.0.1 的连接黑洞掉，只有走域名寻址才能绕过它，真正走到分流规则
// 或默认的 freedom 出站（对照 TestGeoRestrictionAgainstRealXray 早前踩过的坑）。
//
// 返回读到的完整响应（状态行 + 头 + 正文）。读不到任何字节返回空串——黑洞
// 出站会让 SOCKS CONNECT 握手看起来成功、连接建立了，只是之后再没有任何
// 数据、连接也不会被关闭，这与「连接失败」是两种不同的失败模式，只能靠
// 「等不到数据」而不是 err 是否非 nil 来区分。
func httpThroughSocksDomain(proxyAddr, domain string, port int) string {
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return ""
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(c, greet); err != nil || greet[0] != 0x05 || greet[1] != 0x00 {
		return ""
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(domain))}
	req = append(req, domain...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return ""
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil || head[1] != 0x00 {
		return ""
	}
	var addrLen int
	switch head[3] {
	case 0x01:
		addrLen = 4
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(c, lb); err != nil {
			return ""
		}
		addrLen = int(lb[0])
	case 0x04:
		addrLen = 16
	default:
		return ""
	}
	if _, err := io.ReadFull(c, make([]byte, addrLen+2)); err != nil {
		return ""
	}

	if _, err := fmt.Fprintf(c, "GET / HTTP/1.0\r\nHost: %s\r\n\r\n", domain); err != nil {
		return ""
	}

	// 用比握手更短的独立 deadline：真实响应通常几十毫秒内到达，黑洞连接则
	// 永远等不到 EOF，没必要每次 block 断言都陪它等满 3 秒握手超时。
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, _ := io.ReadAll(c)
	return string(data)
}

// TestHotReloadEndToEndAgainstRealXray 是本计划唯一一处用真实 xray 进程证明
// 「配置热应用」真的生效的测试。此前 9 个任务全是单元测试：ComputeHotDiff、
// XrayAPI 的封装、RestartXray 的分支选择都各自测过，但没有任何测试真正启动
// 过 xray 并验证「进程没重启」与「核心行为真的变了」同时成立——只验证前者，
// 后者却没变，是热更新最危险的失效模式（核心与面板的认知从此不一致）。
//
// 四个阶段对应设计里要证明的四件事，见各 t.Run 内的注释。
func TestHotReloadEndToEndAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	requirePgrep(t)
	requireXrayAPIPortFree(t)
	setupDB(t)

	// p / lock 是包级变量，本测试是本包第一个真正驱动 XrayService.RestartXray
	// 起真实进程的测试；无论测试通过与否都必须把它停干净，否则会残留一个
	// xray 进程污染开发机，也会让同包后续测试观察到一个「本不该存在」的 p。
	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})

	const marker = "aetherui-hot-reload-e2e-ok"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(target.Close)
	targetPort := target.Listener.Addr().(*net.TCPAddr).Port

	// 业务入站：仅监听 127.0.0.1 的免密 SOCKS，让测试自己就能当"已配置的客户端"
	// 发起代理请求，不需要真的起一个客户端程序。
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
		t.Fatalf("save inbound: %v", err)
	}

	encodedDomains, err := EncodeDomains([]string{"full:localhost"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	group := &model.DomainGroup{Remark: "热更新验证", Domains: encodedDomains}
	if err := (&DomainGroupService{}).Add(group); err != nil {
		t.Fatalf("add domain group: %v", err)
	}

	// 规则先建成禁用状态：既满足"起步时库里就有一条分流规则"，又不在第一次
	// 启动时就挡住 localhost，好留出一个"加规则前请求成功"的基线。
	rule := &model.RoutingRule{
		Remark: "block localhost for hot-reload e2e", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupId: group.Id, Action: model.ActionBlock, Enable: false,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	xs := &XrayService{}
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)

	var pidAfterStart, pidAfterHotApply int

	// 1. 真 xray 起得来：用临时库建的入站 + 分流规则合成配置，走
	// RestartXray(true) 真正拉起一个 xray 子进程。
	t.Run("真xray起得来", func(t *testing.T) {
		if err := xs.RestartXray(true); err != nil {
			t.Fatalf("RestartXray(true): %v", err)
		}
		waitForPort(t, socksPort)
		if !xs.IsXrayRunning() {
			t.Fatal("RestartXray(true) 之后 IsXrayRunning() = false")
		}
		pidAfterStart = xrayChildPID(t)
		t.Logf("xray 已启动，PID=%d", pidAfterStart)

		// 规则此时 Enable=false，buildRules 只取启用的规则，生成的配置里
		// 压根没有这条 block——顺带证明下面的探测请求手段本身可靠
		// （不是因为路由/target 本来就不通才读不到数据）。
		got := httpThroughSocksDomain(proxyAddr, "localhost", targetPort)
		if !strings.Contains(got, marker) {
			t.Fatalf("加规则前的基线请求失败，读到 %q（长度 %d），期望包含 %q",
				got, len(got), marker)
		}
		t.Logf("加规则前探测：读到 %d 字节，包含 marker，代理链路正常", len(got))
	})

	// 2 + 3. 改分流规则不重启，且规则真的进了核心：启用那条 block 规则，
	// 走 RestartXray(false)。断言 PID 不变（没有整进程重启），并且同一个
	// 代理请求从"成功"变成"读不到任何数据"——不能只验证 PID，"没重启"
	// 不等于"新规则生效了"，这条才是热更新是否真的有效的直接证据。
	t.Run("改分流规则不重启且规则真的进了核心", func(t *testing.T) {
		rule.Enable = true
		if err := (&RoutingRuleService{}).Update(rule); err != nil {
			t.Fatalf("update rule: %v", err)
		}
		if err := xs.RestartXray(false); err != nil {
			t.Fatalf("RestartXray(false): %v", err)
		}
		pidAfterHotApply = xrayChildPID(t)
		if pidAfterHotApply != pidAfterStart {
			t.Fatalf("PID 变了：%d -> %d，期望热应用不重启进程", pidAfterStart, pidAfterHotApply)
		}
		t.Logf("热应用规则后 PID=%d（不变），进程确实没有重启", pidAfterHotApply)

		got := httpThroughSocksDomain(proxyAddr, "localhost", targetPort)
		if got != "" {
			t.Errorf("规则启用后仍读到 %q（长度 %d），期望被 block（读不到任何数据）", got, len(got))
		}
		t.Logf("规则启用后探测：读到 %d 字节（0 = 被黑洞出站吞掉，符合预期）", len(got))
	})

	// 4. 该重启的仍然会重启：改访问日志开关（改的是 log 段，没有运行时重载
	// 接口），走 RestartXray(false)。断言 PID 变了。这条比第 2 条更重要——
	// 热更新最危险的失效模式不是"该热更新却重启了"，而是"该重启却判定成
	// 热应用了"，核心与面板的认知从此不一致且无从察觉。
	t.Run("该重启的仍然会重启", func(t *testing.T) {
		if err := (&SettingService{}).saveSetting("accessLogEnable", "1"); err != nil {
			t.Fatalf("save accessLogEnable: %v", err)
		}
		if err := xs.RestartXray(false); err != nil {
			t.Fatalf("RestartXray(false): %v", err)
		}
		waitForPort(t, socksPort)
		pidAfterForcedRestart := xrayChildPID(t)
		if pidAfterForcedRestart == pidAfterHotApply {
			t.Fatalf("PID 未变：%d，log 段变化没有重载接口，期望触发完整重启", pidAfterForcedRestart)
		}
		t.Logf("切换访问日志开关后 PID：%d -> %d（变了），触发了完整重启", pidAfterHotApply, pidAfterForcedRestart)

		// 完整重启会重新生成配置，那条已经启用的 block 规则理应还在——
		// 顺带确认重启路径没有丢状态，而不只是"进程换了个 PID"。
		got := httpThroughSocksDomain(proxyAddr, "localhost", targetPort)
		if got != "" {
			t.Errorf("重启后仍读到 %q（长度 %d），期望规则继续生效", got, len(got))
		}
		t.Logf("重启后探测：读到 %d 字节（0 = 规则仍生效）", len(got))
	})
}
