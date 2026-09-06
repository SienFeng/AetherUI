package service

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/xray"
)

// 出站节点连通性探测。
//
// 一次探测起一个临时 xray 子进程，配置里只有两段：一个监听回环的 socks
// 入站，和被测节点本身的出站。没有 routing 段——单出站天然就是默认出站，
// 所有流量必走它。面板随后经这个本地 socks 请求一个固定 URL，拿到响应
// 才算通，耗时即延迟。
//
// 这是唯一能证明「密码对、协议参数对、真的能传数据」的做法：只做 TCP
// 拨号的话，密码错、UUID 错、节点被墙全都会显示成「通」。
//
// 失败语义与 routing_validate.go 刻意相反。那里是 fail open——校验器自身
// 故障绝不能挡住管理员保存配置；这里的探测是纯旁路，不参与任何保存或
// 配置生成，所以 xray 二进制缺失时必须明说「无法测试」，绝不能因为
// 「没测出问题」就显示绿灯。
const (
	// ProbeOK 表示经该节点成功取到了探测目标的响应。
	ProbeOK = "ok"
	// ProbeFail 表示链路不通，Error 里带原因。
	ProbeFail = "fail"
	// ProbeUnavailable 表示探测本身没能进行（缺少 xray 二进制等），
	// 既不是通也不是不通。
	ProbeUnavailable = "unavailable"
)

const (
	// probeTTL 之后的结果视为过期并被清出内存。探测结果是「刚才测的那一下」，
	// 不是历史记录；留得越久越容易让人对着一个早已失效的绿灯做判断。
	probeTTL = 30 * time.Minute
	// probeConcurrency 是全局并发上限。每次探测都会起一个 xray 子进程并
	// 真实出网，「全部测试」按钮在节点多时不该把机器压垮。
	probeConcurrency = 3
	// probeReadyTimeout 是等临时 socks 入站开始监听的上限。
	probeReadyTimeout = 2 * time.Second
	// probeHTTPTimeout 是经代理请求探测目标的上限。
	probeHTTPTimeout = 10 * time.Second
	// probeErrorLimit 截断错误信息，避免一条超长的核心报错撑爆列表。
	probeErrorLimit = 200
	// probeOutboundTag 是临时配置里出站的 tag。临时进程独立于面板的核心，
	// 不会与 model.IsReservedTag 那套保留 tag 相互影响。
	probeOutboundTag = "probe-out"
)

// probeTargets 按顺序尝试，任一成功即算通。固定在代码里、不做成设置项：
// 新增设置项要同步改五处，漏掉前端模型那一处会让整个保存配置接口失败，
// 为一个探测目标付这个代价不值。是变量只为让测试指向本地 httptest。
var probeTargets = []string{
	"https://www.gstatic.com/generate_204",
	"https://cp.cloudflare.com/generate_204",
}

// probeBinaryPath 是变量而非直接调用，只为让测试能模拟二进制缺失。
var probeBinaryPath = xray.GetBinaryPath

// probeRunner 是真正执行一次探测的函数，独立成变量只为让测试能注入
// 异常路径（见 Probe 里对 panic 的兜底）。
var probeRunner = func(s *ProbeService, node *model.OutboundNode) *ProbeResult {
	return s.run(node)
}

// ProbeResult 是一次探测的结果。
type ProbeResult struct {
	Status    string `json:"status"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
	CheckedAt int64  `json:"checkedAt"`

	// tag 不导出，因此不会出现在 JSON 里，只用于挡住 SQLite 的 id 复用：
	// 删掉 id=N 的节点后新建一个，它还会拿到 id=N，只按 id 索引的缓存会
	// 让新节点凭空继承旧节点的结果。Tag 一经分配不可变，是可靠的判据。
	tag string
}

// probeCall 是一次进行中的探测，用于同一节点的并发去重。
type probeCall struct {
	done   chan struct{}
	result *ProbeResult
}

var (
	probeMu       sync.Mutex
	probeCache    = map[int]*ProbeResult{}
	probeInflight = map[int]*probeCall{}
	probeSem      = make(chan struct{}, probeConcurrency)
)

type ProbeService struct {
	outboundService OutboundNodeService
}

// ResultsFor 返回这批节点各自的缓存结果，顺带把过期条目清出内存。
//
// 必须传入节点本身而不是只传 id：tag 校验依赖它。缓存里没有、已过期或
// tag 对不上的节点不出现在返回值里，前端据此显示「未测试」。
func (s *ProbeService) ResultsFor(nodes []*model.OutboundNode) map[int]*ProbeResult {
	deadline := time.Now().Add(-probeTTL).Unix()
	out := make(map[int]*ProbeResult, len(nodes))

	probeMu.Lock()
	defer probeMu.Unlock()
	// 先按整份缓存清理过期条目。只在读取时过滤是不够的——被删掉的节点
	// 不会再出现在 nodes 里，它的条目就永远留在内存里了。
	for id, res := range probeCache {
		if res.CheckedAt < deadline {
			delete(probeCache, id)
		}
	}
	for _, node := range nodes {
		res, ok := probeCache[node.Id]
		if !ok || res.tag != node.Tag {
			continue
		}
		copied := *res
		out[node.Id] = &copied
	}
	return out
}

// Forget 丢弃某个节点的探测结果，删除节点时调用。
//
// 不能只靠 ResultsFor 的 tag 校验兜底：那条防线只在「新节点恰好拿到同一个
// id」时才生效，而残留条目在此之前会一直占着内存。
func (s *ProbeService) Forget(id int) {
	probeMu.Lock()
	delete(probeCache, id)
	probeMu.Unlock()
}

// Probe 探测一个出站节点并把结果写入缓存。
//
// 返回的 error 只表示「没能开始探测」（节点不存在等）；链路不通是正常
// 结果，通过 ProbeResult.Status 表达。
func (s *ProbeService) Probe(id int) (*ProbeResult, error) {
	node, err := s.outboundService.Get(id)
	if err != nil {
		return nil, common.NewError("出站节点不存在:", err)
	}

	// 同一节点的并发探测复用同一次执行：每次探测都要起子进程并真实出网，
	// 重复点击不该变成重复消耗。
	probeMu.Lock()
	if call, ok := probeInflight[id]; ok {
		probeMu.Unlock()
		<-call.done
		return call.result, nil
	}
	call := &probeCall{done: make(chan struct{})}
	probeInflight[id] = call
	probeMu.Unlock()

	// 收尾必须走 defer。run 里一旦 panic（gin 的 recovery 会在更外层接住它），
	// 不清理 inflight 表的话 call.done 永不关闭，这个节点此后每一次测试都会
	// 永久阻塞在上面那个 <-call.done 上，只能重启面板才能恢复。
	var result *ProbeResult
	defer func() {
		if result == nil {
			result = &ProbeResult{
				Status:    ProbeUnavailable,
				Error:     "测试过程异常中断",
				CheckedAt: time.Now().Unix(),
				tag:       node.Tag,
			}
		}
		probeMu.Lock()
		delete(probeInflight, id)
		// 「无法测试」不进缓存：它不是对这个节点的判断，缓存下来只会让
		// TTL 内都不再重试。
		if result.Status != ProbeUnavailable {
			probeCache[id] = result
		}
		probeMu.Unlock()
		call.result = result
		close(call.done)
	}()

	result = probeRunner(s, node)
	return result, nil
}

func (s *ProbeService) run(node *model.OutboundNode) *ProbeResult {
	now := time.Now().Unix()
	unavailable := func(reason string) *ProbeResult {
		return &ProbeResult{Status: ProbeUnavailable, Error: reason, CheckedAt: now, tag: node.Tag}
	}
	fail := func(reason string) *ProbeResult {
		// 界面上只看得到截断后的一句话，日志里留一份完整的，便于排查。
		logger.Debug("出站节点连通性测试失败 tag=", node.Tag, " reason=", reason)
		return &ProbeResult{Status: ProbeFail, Error: truncateProbeError(reason), CheckedAt: now, tag: node.Tag}
	}

	binaryPath := probeBinaryPath()
	if _, err := os.Stat(binaryPath); err != nil {
		return unavailable("缺少 xray 二进制，无法测试")
	}

	var ob map[string]any
	if err := json.Unmarshal([]byte(node.Config), &ob); err != nil || ob == nil {
		return unavailable("节点配置不是合法的 JSON，无法测试")
	}
	ob["tag"] = probeOutboundTag

	probeSem <- struct{}{}
	defer func() { <-probeSem }()

	port, err := freeLoopbackPort()
	if err != nil {
		return unavailable("无法分配本地测试端口：" + err.Error())
	}

	cfgPath, err := writeProbeConfig(ob, port)
	if err != nil {
		return unavailable(err.Error())
	}
	defer os.Remove(cfgPath)

	cmd := exec.Command(binaryPath, "run", "-c", cfgPath)
	if err := cmd.Start(); err != nil {
		return unavailable("无法启动测试用 xray 进程：" + err.Error())
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		// Wait 回收子进程，否则每次探测都留下一个僵尸。
		cmd.Wait()
	}()

	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !waitForListener(addr, probeReadyTimeout) {
		return fail("测试用 xray 未能启动，请检查节点配置")
	}

	latency, err := probeThroughProxy(addr)
	if err != nil {
		return fail(describeProbeError(err))
	}
	return &ProbeResult{
		Status:    ProbeOK,
		LatencyMs: latency.Milliseconds(),
		CheckedAt: now,
		tag:       node.Tag,
	}
}

// writeProbeConfig 落一份最小配置：回环 socks 入站 + 被测出站，没有 routing。
// 文件里带着节点密码，权限必须是 0600。
func writeProbeConfig(ob map[string]any, port int) (string, error) {
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "none"},
		"inbounds": []any{map[string]any{
			"tag":      "probe-in",
			"listen":   "127.0.0.1",
			"port":     port,
			"protocol": "socks",
			"settings": map[string]any{"auth": "noauth", "udp": false},
		}},
		"outbounds": []any{ob},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "a-ui-probe-*.json")
	if err != nil {
		return "", common.NewError("无法创建临时配置文件:", err)
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		os.Remove(file.Name())
		return "", common.NewError("无法设置临时配置文件权限:", err)
	}
	if _, err := file.Write(data); err != nil {
		os.Remove(file.Name())
		return "", common.NewError("无法写入临时配置文件:", err)
	}
	return file.Name(), nil
}

// freeLoopbackPort 让内核分配一个空闲的回环端口。
//
// 关闭到 xray 监听之间有一个理论上的抢占窗口，代价只是这一次探测报
// 「未能启动」，重试即可——比自己维护端口池简单得多。
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func waitForListener(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// probeThroughProxy 依次尝试各个探测目标，任一成功即返回其耗时。
//
// 标准库的 http.Transport 原生支持 socks5 代理 URL，不需要额外依赖。
func probeThroughProxy(proxyAddr string) (time.Duration, error) {
	proxyURL, err := url.Parse("socks5://" + proxyAddr)
	if err != nil {
		return 0, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
		Timeout: probeHTTPTimeout,
		// 探测只关心「这一跳通不通」，不跟随跳转，免得把跳转耗时算进延迟。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	var lastErr error
	for _, target := range probeTargets {
		start := time.Now()
		resp, err := client.Get(target)
		if err != nil {
			lastErr = err
			continue
		}
		elapsed := time.Since(start)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("目标返回 HTTP %d", resp.StatusCode)
			continue
		}
		return elapsed, nil
	}
	if lastErr == nil {
		lastErr = common.NewError("没有可用的探测目标")
	}
	return 0, lastErr
}

// describeProbeError 把 Go 的网络错误换成管理员看得懂的一句话。
//
// 刻意只识别少数几种形态：过度分类会在核心或标准库改动措辞时开始说错话，
// 而说错话比直接给出原始错误更难排查。
func describeProbeError(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "context deadline exceeded"),
		strings.Contains(text, "Client.Timeout"),
		strings.Contains(text, "i/o timeout"):
		return "连接超时（节点无响应或被阻断）"
	case strings.Contains(text, "connection refused"):
		return "连接被拒绝（本地测试代理未就绪）"
	case strings.Contains(text, "general SOCKS server failure"),
		strings.Contains(text, "host unreachable"),
		strings.Contains(text, "network unreachable"):
		return "节点无法建立到目标的连接（认证失败或服务器不可达）"
	}
	return text
}

func truncateProbeError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= probeErrorLimit {
		return s
	}
	// 按字节截断可能切碎多字节字符，按 rune 走一遍。
	runes := []rune(s)
	limit := probeErrorLimit
	if len(runes) < limit {
		limit = len(runes)
	}
	return string(runes[:limit]) + "…"
}
