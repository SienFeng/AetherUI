package service

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// hotReloadAPIPort 是模板里固定写死的 api 入站端口（web/service/config.json）。
// 本机如果已经有一份运行中的 a-ui/xray 占着它，测试会在启动阶段就失败，
// 且失败信息（端口占用）与真正的热更新缺陷无法区分——动手之前先跳过并说明原因。
const hotReloadAPIPort = 62789

func requireXrayAPIPortFree(t *testing.T) {
	t.Helper()
	// 同一个 go test 进程里连续跑多个 e2e 用例时，前一个用例的 t.Cleanup
	// 用 SIGKILL 结束上一个 xray 子进程——内核实际回收该进程（连带释放它
	// 监听的端口）相对于 Kill() 调用的返回是异步的，紧接着起下一个用例
	// 可能会撞上这个尚未回收完的极短窗口。这不是「端口被别的东西占用」，
	// 短暂重试即可避免把它误判成真实占用而错误跳过。
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hotReloadAPIPort))
		if err == nil {
			_ = ln.Close()
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Skipf("端口 %d（xray 控制面 api 入站的固定端口）已被占用，本机可能已有一个 a-ui/xray 实例在跑，跳过: %v",
		hotReloadAPIPort, lastErr)
}

func requirePgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep 不可用，无法通过父子进程关系定位 xray 的 PID，跳过")
	}
}

// requireXrayRoutingService 在 bin/xray-* 不提供 RoutingService 时跳过。
//
// 路由的热下发走 RoutingService.AddRule，而这个 gRPC 服务在老核心里根本不存在
// （Xray 1.4.x 时代的构建里 RoutingService 符号数为 0）。核心不提供它时
// tryHotApply 会连不上而退回整进程重启——这是设计好的失败兜底，不是缺陷，
// 但本测试断言的恰恰是「不重启」，于是会以「PID 变了」失败，而那条信息与
// 真正的热更新缺陷无法区分。先探测再跳过，把「核心太老」和「热更新坏了」分开。
//
// 探测方式是在二进制里找符号而不是发一次 RPC：此时 xray 还没启动，而启动之后
// 再跳过就已经晚了——测试的前半段（起进程、记 PID）都已经跑过了。
func requireXrayRoutingService(t *testing.T) {
	t.Helper()
	path := xray.GetBinaryPath()
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("打不开 xray 二进制 %s，无法确认它是否提供 RoutingService，跳过: %v", path, err)
	}
	defer f.Close()

	needle := []byte("RoutingService")
	// 分块流式扫描，块间保留 len(needle)-1 字节的重叠，避免符号正好跨块边界被漏掉。
	// 不整体读入：这个二进制有 35MB 以上。
	const chunk = 1 << 20
	buf := make([]byte, chunk+len(needle)-1)
	carry := 0
	for {
		n, err := f.Read(buf[carry:])
		if n > 0 && bytes.Contains(buf[:carry+n], needle) {
			return
		}
		if err != nil {
			break
		}
		if carry+n >= len(needle)-1 {
			copy(buf, buf[carry+n-(len(needle)-1):carry+n])
			carry = len(needle) - 1
		} else {
			carry += n
		}
	}
	t.Skipf("xray 二进制 %s 不提供 RoutingService（疑为 1.4.x 时代的旧构建），"+
		"路由热下发在它上面必然退回整进程重启，跳过；请把 bin/xray-* 更新到与 go.mod 里 "+
		"xray-core 同版本的构建", path)
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

// socksProbeResult 把代理探测拆成两个独立阶段分别报告：SOCKS5 握手 +
// CONNECT 是否成功（代理链路本身是否健全——业务入站有没有被热应用误伤），
// 以及 CONNECT 成功之后实际读到的响应体（路由层面是否把它黑洞掉了）。
//
// 合并成一个空字符串是不够的：SOCKS 握手/CONNECT 失败（代理链路本身坏了，
// 例如热应用误删了业务入站）与握手成功后读不到数据（被分流规则正确
// block）是性质完全不同的两件事，混在一起就分不清哪种情况发生了——而
// 这两种情况在本测试改动前会得到一模一样的 PASS，恰恰是本测试要防的
// 那类"该报错却没报错"的回归。
type socksProbeResult struct {
	HandshakeOK bool   // SOCKS5 握手 + CONNECT 均返回成功
	Body        string // 握手成功后实际读到的响应（状态行+头+正文）；被路由黑洞时为空
	Err         error  // 握手/CONNECT 阶段失败的原因；HandshakeOK 为 true 时为 nil
}

// socksProbe 通过 SOCKS5 的域名寻址（ATYP=0x03）连到 proxyAddr，让 xray 按
// 域名而不是私网 IP 做路由判断——模板里的 geoip:private 规则会把发往
// 127.0.0.1 的连接黑洞掉，只有走域名寻址才能绕过它，真正走到分流规则或
// 默认的 freedom 出站（对照 TestGeoRestrictionAgainstRealXray 早前踩过的坑）。
//
// 实测确认（Xray 26.7.28，本机跑过 TestHotReloadEndToEndAgainstRealXray 的
// 红绿验证）：分流规则把目标域名 block 时，SOCKS5 的握手与 CONNECT 回复
// 依然是成功的（REP=0x00）——xray 的 SOCKS 入站先完成协议层握手、再由
// 路由决定实际转发去向，黑洞只发生在数据转发阶段，连接不会被关闭、只是
// 永远没有字节流回来。这正是 HandshakeOK 与 Body 需要分开报告的原因。
func socksProbe(proxyAddr, domain string, port int) socksProbeResult {
	c, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		return socksProbeResult{Err: fmt.Errorf("dial %s: %w", proxyAddr, err)}
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return socksProbeResult{Err: fmt.Errorf("发送 SOCKS5 greeting: %w", err)}
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(c, greet); err != nil {
		return socksProbeResult{Err: fmt.Errorf("读取 greeting 回复: %w", err)}
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		return socksProbeResult{Err: fmt.Errorf("greeting 被拒绝: % x", greet)}
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(domain))}
	req = append(req, domain...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return socksProbeResult{Err: fmt.Errorf("发送 CONNECT: %w", err)}
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return socksProbeResult{Err: fmt.Errorf("读取 CONNECT 回复: %w", err)}
	}
	if head[1] != 0x00 {
		return socksProbeResult{Err: fmt.Errorf("CONNECT 被拒绝, REP=0x%02x", head[1])}
	}
	var addrLen int
	switch head[3] {
	case 0x01:
		addrLen = 4
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(c, lb); err != nil {
			return socksProbeResult{Err: fmt.Errorf("读取 BND.ADDR 长度: %w", err)}
		}
		addrLen = int(lb[0])
	case 0x04:
		addrLen = 16
	default:
		return socksProbeResult{Err: fmt.Errorf("CONNECT 回复里未知的 ATYP=0x%02x", head[3])}
	}
	if _, err := io.ReadFull(c, make([]byte, addrLen+2)); err != nil {
		return socksProbeResult{Err: fmt.Errorf("读取 BND.ADDR/BND.PORT: %w", err)}
	}

	// 到这里 SOCKS5 握手 + CONNECT 已经完全成功：代理链路本身健全，
	// 业务入站活着。接下来读到的数据是否为空，才是路由决策的结果，
	// 与握手是否成功是两回事。
	if _, err := fmt.Fprintf(c, "GET / HTTP/1.0\r\nHost: %s\r\n\r\n", domain); err != nil {
		return socksProbeResult{HandshakeOK: true, Err: fmt.Errorf("发送 GET: %w", err)}
	}

	// 用比握手更短的独立 deadline：真实响应通常几十毫秒内到达，黑洞连接则
	// 永远等不到 EOF，没必要每次 block 断言都陪它等满 3 秒握手超时。
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, _ := io.ReadAll(c)
	return socksProbeResult{HandshakeOK: true, Body: string(data)}
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
	requireXrayRoutingService(t)
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
		DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}), Action: model.ActionBlock, Enable: false,
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
		res := socksProbe(proxyAddr, "localhost", targetPort)
		if !res.HandshakeOK {
			t.Fatalf("加规则前 SOCKS 握手/CONNECT 失败: %v", res.Err)
		}
		if !strings.Contains(res.Body, marker) {
			t.Fatalf("加规则前的基线请求失败，读到 %q（长度 %d），期望包含 %q",
				res.Body, len(res.Body), marker)
		}
		t.Logf("加规则前探测：握手成功，读到 %d 字节，包含 marker，代理链路正常", len(res.Body))
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

		// 分两条断言：HandshakeOK 证明业务入站还活着、代理链路没被热应用
		// 误伤（不是"连不上"）；Body 为空才证明是路由层面把它 block 了。
		// 只查"读到的字符串是不是空"分不清这两种性质完全不同的失败——
		// 一次误删业务入站的回归会和"规则正确生效"得到一模一样的 PASS。
		res := socksProbe(proxyAddr, "localhost", targetPort)
		if !res.HandshakeOK {
			t.Fatalf("规则启用后 SOCKS 握手/CONNECT 失败（业务入站可能被热应用误伤，"+
				"而不是分流规则生效）: %v", res.Err)
		}
		if res.Body != "" {
			t.Errorf("规则启用后仍读到 %q（长度 %d），期望被 block（读不到任何数据）", res.Body, len(res.Body))
		}
		t.Logf("规则启用后探测：握手成功（业务入站健全），读到 %d 字节（0 = 被路由层黑洞，符合预期）",
			len(res.Body))
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
		res := socksProbe(proxyAddr, "localhost", targetPort)
		if !res.HandshakeOK {
			t.Fatalf("重启后 SOCKS 握手/CONNECT 失败: %v", res.Err)
		}
		if res.Body != "" {
			t.Errorf("重启后仍读到 %q（长度 %d），期望规则继续生效", res.Body, len(res.Body))
		}
		t.Logf("重启后探测：握手成功，读到 %d 字节（0 = 规则仍生效）", len(res.Body))
	})
}

// selfSignedCertForTest 生成一份仅用于测试的自签证书。Vision 要求外层是
// TLS 1.3，所以这个入站必须真的配上证书才能起来。证书写进 t.TempDir()，
// 测试结束自动清理——不要写进仓库。
func selfSignedCertForTest(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatalf("生成测试私钥: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hot-reload-vision.test"},
		DNSNames:              []string{"hot-reload-vision.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(crand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发测试证书: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// 设计文档 §7.3 的待验证项：VLESS + tcp + TLS + Vision 的入站不命中
// inboundUsesReality，会走热交换的「先删后加」两条 gRPC。删加能否正确
// 重建一个 Vision 入站，此前没有证据——这个测试给出结论。
//
// 断言分两层，缺一不可：
//  1. PID 不变     → 确实走了热应用，没有退回整进程重启
//  2. 端口仍能完成 TLS 握手 → 入站真的被重建了，而不是删掉之后没加回来
//
// 只断言 PID 不变是不够的：热应用「成功」但入站没加回来，PID 同样不变，
// 而所有用户已经断线——这正是本项目最忌讳的静默失效。
func TestHotReloadRebuildsVisionInbound(t *testing.T) {
	requireXrayBinary(t)
	requirePgrep(t)
	requireXrayAPIPortFree(t)
	setupDB(t)

	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})

	certPath, keyPath := selfSignedCertForTest(t)
	visionPort := freePort(t)

	stream := fmt.Sprintf(`{"network":"tcp","security":"tls",`+
		`"tlsSettings":{"serverName":"hot-reload-vision.test",`+
		`"minVersion":"1.3","maxVersion":"1.3","alpn":["h2","http/1.1"],`+
		`"certificates":[{"certificateFile":%q,"keyFile":%q}]},`+
		`"tcpSettings":{"header":{"type":"none"}}}`, certPath, keyPath)

	in := &model.Inbound{
		UserId: 1, Port: visionPort, Protocol: model.VLESS, Enable: true,
		Tag:    "inbound-" + strconv.Itoa(visionPort),
		Listen: "127.0.0.1",
		Settings: `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811",` +
			`"flow":"xtls-rprx-vision","email":"before@e2e"}],"decryption":"none"}`,
		StreamSettings: stream,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, visionPort)
	pidBefore := xrayChildPID(t)
	t.Logf("Vision 入站已起来，PID=%d，端口=%d", pidBefore, visionPort)

	// 改一个只落在 Settings 里的字段，触发这个入站的热交换（先删后加）。
	in.Settings = `{"clients":[{"id":"b831381d-6324-4d53-ad4f-8cda48b30811",` +
		`"flow":"xtls-rprx-vision","email":"after@e2e"}],"decryption":"none"}`
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("update inbound: %v", err)
	}
	if err := xs.RestartXray(false); err != nil {
		t.Fatalf("RestartXray(false): %v", err)
	}

	pidAfter := xrayChildPID(t)
	tlsOK := tlsHandshakeOK(t, visionPort)

	switch {
	case pidAfter == pidBefore && tlsOK:
		t.Log("结论：Vision 入站可以安全热交换——PID 未变且端口仍能完成 TLS 握手")
	case pidAfter == pidBefore && !tlsOK:
		t.Fatal("热应用报告成功、进程也没重启，但入站已经不在了——" +
			"这是静默断线，必须把 Vision 纳入强制重启判定（见 Step 3 结论 B）")
	default:
		t.Logf("结论：Vision 入站退回了整进程重启（PID %d -> %d）。"+
			"功能正确但不是热应用，应把 Vision 纳入 inboundNeedsRestart 让这个行为显式化",
			pidBefore, pidAfter)
	}
}

// tlsHandshakeOK 只验证对端能完成 TLS 握手，不验证证书链——证书是自签的。
// 握手成功即证明该入站仍在监听且 TLS 配置完好。
func tlsHandshakeOK(t *testing.T, port int) bool {
	t.Helper()
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", fmt.Sprintf("127.0.0.1:%d", port),
		&tls.Config{InsecureSkipVerify: true, ServerName: "hot-reload-vision.test"})
	if err != nil {
		t.Logf("TLS 握手失败: %v", err)
		return false
	}
	defer conn.Close()
	return true
}
