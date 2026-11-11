package service

import (
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/xray"
)

// 入站端口的可用性。
//
// 端口被本机别的程序占着（装域名时的 Caddy :443、面板自己的端口、模板里控制面 api 入站的端口、
// sshd）时，修复前保存照样成功：checkPortExist 只查库里有没有同端口的入站，保存时的 run -test
// 从不真的 bind。随后热应用的 AddInbound 失败 → 退回整进程重启（Reality 入站本来就必须整进程
// 重启）→ 新核心因 bind: address already in use 起不来 → 机器上所有入站一起断，而 RestartXray
// 返回 nil，CheckXrayRunningJob 每 30 秒重试一次、永远失败。
//
// 两道防线：保存时拦住（本文件），运行时撞上的入站摘出配置、不拖垮其余入站（xray_port_conflict.go）。

// inboundListenSpec 是一个入站要在本机占用的监听：地址、端口，以及要不要 TCP / UDP。
type inboundListenSpec struct {
	Listen string // 去掉 JSON 引号后的监听地址，"" 表示所有地址
	Port   int
	TCP    bool
	UDP    bool
	// Derived 表示这是从「所有地址」拆出来的单个本机地址（见 unheldPart）：探它只认 EADDRINUSE，
	// 别的失败（IPv6 地址还在 DAD、刚被摘掉）不能让整个入站被判为端口冲突。
	Derived bool
}

// listenSpecOf 从生成的入站配置推出它要占的监听。第二个返回值为 false 表示它不占端口
// （unix socket、没有端口、tun），不参与端口判定。
func listenSpecOf(ib *xray.InboundConfig) (inboundListenSpec, bool) {
	listen := ""
	if len(ib.Listen) > 0 {
		if err := json.Unmarshal(ib.Listen, &listen); err != nil {
			return inboundListenSpec{}, false
		}
	}
	if strings.HasPrefix(listen, "/") || strings.HasPrefix(listen, "@") || ib.Port <= 0 {
		return inboundListenSpec{}, false
	}
	tcp, udp := inboundSocketNeeds(ib.Protocol, ib.Settings, ib.StreamSettings)
	if !tcp && !udp {
		return inboundListenSpec{}, false
	}
	return inboundListenSpec{Listen: listen, Port: ib.Port, TCP: tcp, UDP: udp}, true
}

// inboundSocketNeeds 推断入站要在端口上监听 TCP 还是 UDP（或两者）。
//
// 两层叠加：协议层面的网络（socks 的 udp、shadowsocks / dokodemo 的 network、wireguard 与
// hysteria 只走 UDP）决定要不要额外的 UDP 监听；走 TCP 的那部分再由传输层决定实际是哪种
// socket——kcp / quic / hysteria 与 h3 的 xhttp 底下是 UDP。判错的后果不对称：漏探 UDP 只是
// 回到修复前的状态，所以拿不准的一律按 TCP。
func inboundSocketNeeds(protocol string, settings, stream []byte) (tcp, udp bool) {
	var s struct {
		UDP     bool            `json:"udp"`
		Network json.RawMessage `json:"network"`
	}
	_ = json.Unmarshal(settings, &s)
	protoTCP, protoUDP := true, false
	switch strings.ToLower(protocol) {
	case "socks", "mixed":
		protoUDP = s.UDP
	case "shadowsocks", "dokodemo-door", "tunnel":
		// 缺省只有 tcp（infra/conf 的 NetworkList.Build 对空值返回 TCP）。
		if nets := parseNetworkList(s.Network); len(nets) > 0 {
			protoTCP, protoUDP = nets["tcp"], nets["udp"]
		}
	case "wireguard", "hysteria":
		protoTCP, protoUDP = false, true
	case "tun":
		return false, false
	}
	if protoTCP {
		if streamUsesUDP(stream) {
			udp = true
		} else {
			tcp = true
		}
	}
	if protoUDP {
		udp = true
	}
	return tcp, udp
}

// parseNetworkList 解析 xray 的 NetworkList：逗号分隔的字符串或字符串数组。
func parseNetworkList(raw json.RawMessage) map[string]bool {
	var items []string
	var joined string
	if err := json.Unmarshal(raw, &joined); err == nil {
		items = strings.Split(joined, ",")
	} else if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, it := range items {
		if it = strings.ToLower(strings.TrimSpace(it)); it != "" {
			out[it] = true
		}
	}
	return out
}

// streamUsesUDP 判断传输层是否跑在 UDP 上。
func streamUsesUDP(stream []byte) bool {
	var ss struct {
		Network string `json:"network"`
		TLS     struct {
			ALPN []string `json:"alpn"`
		} `json:"tlsSettings"`
	}
	if err := json.Unmarshal(stream, &ss); err != nil {
		return false
	}
	switch strings.ToLower(ss.Network) {
	case "kcp", "mkcp", "quic", "hysteria":
		return true
	case "xhttp", "splithttp":
		for _, a := range ss.TLS.ALPN {
			if strings.EqualFold(a, "h3") {
				return true
			}
		}
	}
	return false
}

// listenOverlaps 判断两个监听地址会不会抢同一个端口：任一侧是「所有地址」（""、0.0.0.0、::）就重叠，
// 否则地址相同才重叠（127.0.0.1 与 ::ffff:127.0.0.1 算相同）。
func listenOverlaps(a, b string) bool {
	if isAnyListen(a) || isAnyListen(b) {
		return true
	}
	ipa, ipb := net.ParseIP(a), net.ParseIP(b)
	if ipa != nil && ipb != nil {
		return ipa.Equal(ipb)
	}
	return a == b
}

func isAnyListen(s string) bool {
	if s == "" {
		return true
	}
	ip := net.ParseIP(s)
	return ip != nil && ip.IsUnspecified()
}

// probeListenFn 是实际 bind 探测的实现，测试可以换掉它来模拟「探测时还空着、核心 bind 时被抢」。
var probeListenFn = probeListen

// probeListen 按 xray 的方式（监听地址为空即 0.0.0.0）实际 bind 一下 spec 要的 TCP / UDP，
// 马上关掉。调用方必须持有 lock：探测本身占着端口的那一瞬间若恰好撞上核心 bind，失败的是核心。
func probeListen(spec inboundListenSpec) error {
	host := spec.Listen
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(spec.Port))
	if spec.TCP {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		_ = l.Close()
	}
	if spec.UDP {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return err
		}
		_ = c.Close()
	}
	return nil
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// heldListeners 返回运行中的核心配置（running，nil 表示核心没在跑）里入站占着的监听（地址 + 端口 + 协议）。
// 这些监听正被我们自己的 xray 占着，探测必然撞上自己；热应用会先删后加，端口由核心自己释放、再占。
func heldListeners(running *xray.Config) []inboundListenSpec {
	if running == nil {
		return nil
	}
	var held []inboundListenSpec
	for i := range running.InboundConfigs {
		if spec, ok := listenSpecOf(&running.InboundConfigs[i]); ok {
			held = append(held, spec)
		}
	}
	return held
}

// localAddrsFn 列出本机网卡上的地址，测试可以换掉。
var localAddrsFn = func() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range addrs {
		// IPv6 链路本地地址不带 zone 绑不上，跳过；IPv4 的 169.254/16 照常探。
		if n, ok := a.(*net.IPNet); ok && !(n.IP.To4() == nil && n.IP.IsLinkLocalUnicast()) && !n.IP.IsMulticast() {
			ips = append(ips, n.IP)
		}
	}
	return ips, nil
}

// unheldPart 返回 spec 里运行中的核心没占着、需要实际 bind 探测的部分，什么都不剩时返回空。
//
// 按协议、按地址分开算：
//   - 核心只占着某端口的 TCP 时，入站改成也要 UDP（socks 打开 udp），UDP 那部分照样要探。
//   - 核心占着「所有地址」或同一个地址：探测必然撞上自己，跳过。
//   - 核心占着别的具体地址（127.0.0.1 → 10.0.0.5）：新地址没人替我们占着，照常探。
//   - 核心占着具体地址、新监听是所有地址（127.0.0.1 → 0.0.0.0）：直接探 0.0.0.0 必然撞上自己的核心，
//     于是改为逐个探本机网卡上核心没占着的地址——别的程序占着公网地址的这个端口时，新核心整个起不来。
//     本机网卡之外的回环地址（127.0.0.2 这类）列举不到，这一处仍可能漏。
func unheldPart(spec inboundListenSpec, held []inboundListenSpec) []inboundListenSpec {
	var probes []inboundListenSpec
	for _, udp := range []bool{false, true} {
		if (!udp && !spec.TCP) || (udp && !spec.UDP) {
			continue
		}
		var holders []string
		covered := false
		for _, h := range held {
			if h.Port != spec.Port || (udp && !h.UDP) || (!udp && !h.TCP) {
				continue
			}
			if isAnyListen(h.Listen) || (!isAnyListen(spec.Listen) && listenOverlaps(h.Listen, spec.Listen)) {
				covered = true
				break
			}
			holders = append(holders, h.Listen)
		}
		if covered {
			continue
		}
		one := func(listen string) inboundListenSpec {
			return inboundListenSpec{Listen: listen, Port: spec.Port, TCP: !udp, UDP: udp}
		}
		if len(holders) == 0 || !isAnyListen(spec.Listen) {
			probes = append(probes, one(spec.Listen))
			continue
		}
		ips, err := localAddrsFn()
		if err != nil {
			logger.Warning("列举本机地址失败，跳过端口", spec.Port, "的探测:", err)
			continue
		}
	next:
		for _, ip := range ips {
			for _, h := range holders {
				if listenOverlaps(h, ip.String()) {
					continue next
				}
			}
			pr := one(ip.String())
			pr.Derived = true
			probes = append(probes, pr)
		}
	}
	return probes
}

// probeAll 依次探测 probes，返回第一个失败。
func probeAll(probes []inboundListenSpec) error {
	for _, pr := range probes {
		err := probeListenFn(pr)
		if err == nil {
			continue
		}
		if pr.Derived && !isAddrInUse(err) {
			logger.Debug("探测本机地址", pr.Listen, "端口", pr.Port, "失败（不是端口占用，忽略）:", err)
			continue
		}
		return err
	}
	return nil
}

// checkInboundPortUsable 在保存入站时拦住「下发时才会撞上」的端口。old 为 nil 表示新建。
//
// 只在入站会占端口时检查（启用着），而且只在监听真的变了（地址、端口、要不要 UDP）或从停用变成
// 启用时才检查：入站在跑时它的端口正被我们自己的核心占着，改个备注不能被探测拦下。
//
// 三步：模板自己的入站（控制面 api，核心在跑时它被自己的核心占着、探测会被跳过，必须单独拦）、
// 面板自己的端口、实际 bind 探测。探测只把「端口被占用」（EADDRINUSE）当成拒绝理由；别的失败
// （权限不足的低端口、地址不在本机）记日志放行——这道校验是辅助手段，不能因为它自己拿不准就
// 把管理员挡在门外，与 run -test 校验的 fail open 同一个取向。
func (s *InboundService) checkInboundPortUsable(inbound *model.Inbound, old *model.Inbound) error {
	if !inbound.Enable {
		return nil
	}
	spec, ok := listenSpecOf(inbound.GenXrayInboundConfig())
	if !ok {
		return nil
	}
	if old != nil && old.Enable {
		if prev, ok := listenSpecOf(old.GenXrayInboundConfig()); ok && prev == spec {
			return nil
		}
	}
	settingService := SettingService{}
	if err := checkTemplateInboundPorts(&settingService, spec); err != nil {
		return err
	}
	if err := checkPanelPort(&settingService, spec); err != nil {
		return err
	}
	probes := unheldPart(spec, heldListeners(runningConfigOf(currentProcess())))
	if len(probes) == 0 {
		return nil
	}
	lock.Lock()
	err := probeAll(probes)
	lock.Unlock()
	if err == nil {
		return nil
	}
	if isAddrInUse(err) {
		return common.NewErrorf("端口 %d 已被本机其它程序占用，xray 无法监听这个入站（%v）；请换一个端口，或先停掉占用它的程序",
			spec.Port, err)
	}
	logger.Warning("探测入站端口", spec.Port, "失败（不是端口占用，按可用处理）:", err)
	return nil
}

// checkTemplateInboundPorts 拒绝与模板里的入站抢同一个端口（地址重叠且 TCP/UDP 有交集）。
// 模板读不出、解析不了时放行：那是别处的问题，GetXrayConfig 会如实报错。
func checkTemplateInboundPorts(settingService *SettingService, spec inboundListenSpec) error {
	tpl, err := settingService.GetXrayConfigTemplate()
	if err != nil {
		return nil
	}
	cfg := &xray.Config{}
	if err := json.Unmarshal([]byte(tpl), cfg); err != nil {
		return nil
	}
	for i := range cfg.InboundConfigs {
		ts, ok := listenSpecOf(&cfg.InboundConfigs[i])
		if !ok || ts.Port != spec.Port || !listenOverlaps(ts.Listen, spec.Listen) {
			continue
		}
		if (ts.TCP && spec.TCP) || (ts.UDP && spec.UDP) {
			return common.NewErrorf("端口 %d 已被 xray 模板里的入站 [%s] 占用，请换一个端口",
				spec.Port, cfg.InboundConfigs[i].Tag)
		}
	}
	return nil
}

// checkPanelPort 拒绝与面板自己的监听抢端口。面板只听 TCP。
func checkPanelPort(settingService *SettingService, spec inboundListenSpec) error {
	if !spec.TCP {
		return nil
	}
	port, err := settingService.GetPort()
	if err != nil || port != spec.Port {
		return nil
	}
	listen, err := settingService.GetListen()
	if err != nil {
		return nil
	}
	if listenOverlaps(listen, spec.Listen) {
		return common.NewErrorf("端口 %d 是面板自己的端口，请换一个端口", spec.Port)
	}
	return nil
}
