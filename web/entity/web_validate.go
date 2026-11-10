package entity

import (
	"net"
	"regexp"
	"strconv"
	"strings"

	"a-ui/util/common"
)

// 本文件是「面板从哪里能访问到」这三项设置（监听地址、端口、url 根路径）的校验。
//
// 这三项写错的后果和别的设置项不在一个量级：面板下次启动时监听或注册路由失败，
// 而改动前 Server.Start 失败会连 xray 一起停掉，全部代理用户断网，而且没有任何
// 东西会把它重新拉起来。所以凡是能在保存这一刻判断出「必然起不来」的值，一律在
// 这里拒绝；设置页（CheckValid + SettingService.ValidatePanelAddrChange）与
// `a-ui setting` 命令行共用同一套规则。

// webBasePathPattern 是补齐首尾斜杠之后根路径的白名单：每一段只能由 URL 的
// 非保留字符（RFC 3986 unreserved：字母、数字、. _ ~ -）组成。
//
// 根路径会被原样拼进 gin 的路由（engine.Group(basePath)、StaticFS(basePath+
// "assets")）和 Caddy 的 `handle <basePath>*`：含 : 或 * 时 gin 注册路由直接
// panic；粘贴进来的完整网址（/https://panel.example.com/abc/）带着 : 和 //；
// 空格、%、?、# 这些字符在浏览器、gin 与 Caddy 三处的编解码各不相同，存进去
// 的路径未必是请求到达时的那一个。安装脚本生成的随机根路径只用字母和数字，
// 白名单不会误伤它。
var webBasePathPattern = regexp.MustCompile(`^/([A-Za-z0-9._~-]+/)*$`)

// NormalizeWebBasePath 补齐根路径的首尾斜杠，规则与 CheckValid、
// SettingService.SetBasePath 相同。
func NormalizeWebBasePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// CheckWebBasePath 校验已补齐首尾斜杠的根路径。
//
// 纯由点组成的段（. 和 ..）单独拒绝：它们符合白名单，但浏览器在发出请求之前
// 就会把它们归一化掉，请求永远到不了这个路径。
func CheckWebBasePath(p string) error {
	if !webBasePathPattern.MatchString(p) {
		return common.NewErrorf("面板 url 根路径 %q 不合法：只能由字母、数字和 . _ ~ - 组成，各段之间用 / 分隔（例如 /a1B2c3/）；"+
			"不能是完整网址，也不能含有 : * 空格 %% ? # 或中文等字符，否则面板下次启动时无法注册路由", p)
	}
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "" && strings.Trim(seg, ".") == "" {
			return common.NewErrorf("面板 url 根路径 %q 不合法：不能包含 . 或 .. 这样的路径段，浏览器会把它们直接归一化掉", p)
		}
	}
	return nil
}

// interfaceAddrs 是本机网卡地址的来源，做成变量只为测试。
var interfaceAddrs = net.InterfaceAddrs

// CheckWebListenIsLocal 要求面板监听地址是这台机器能监听的地址：空串（所有 IP）、
// 通配地址（0.0.0.0 / ::）、回环地址，或者确实配在本机某块网卡上的地址。
//
// 典型的错法是把云服务器的公网 IP 填进来：那个 IP 常常是 NAT 映射，根本不在网卡
// 上，面板启动时 listen 报 can't assign requested address。回环地址整段放行
// （Linux 上 127.0.0.0/8 都能监听），不逐个核对。读不出网卡地址时放行：那不是
// 输入的问题，拦下来只会把管理员挡在保存按钮外面。
func CheckWebListenIsLocal(listen string) error {
	if listen == "" {
		return nil
	}
	ip := net.ParseIP(listen)
	if ip == nil {
		return common.NewError("web listen is not valid ip:", listen)
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return nil
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		var local net.IP
		switch v := a.(type) {
		case *net.IPNet:
			local = v.IP
		case *net.IPAddr:
			local = v.IP
		}
		if local != nil && local.Equal(ip) {
			return nil
		}
	}
	return common.NewErrorf("面板监听地址 %v 不在本机任何网卡上，面板将无法启动"+
		"（云服务器的公网 IP 常常是 NAT 映射，并不在网卡上）。留空表示监听所有 IP", listen)
}

// CheckWebPort 校验面板端口的取值范围。
func CheckWebPort(port int) error {
	if port <= 0 || port > 65535 {
		return common.NewError("web port is not a valid port:", port)
	}
	return nil
}

// PanelAddr 是决定面板从哪里能访问到的三项设置。BasePath 是补齐过首尾斜杠的值。
type PanelAddr struct {
	Listen   string
	Port     int
	BasePath string
}

// PanelAddr 取出这份设置里的面板地址三项。
func (s *AllSetting) PanelAddr() PanelAddr {
	return PanelAddr{Listen: s.WebListen, Port: s.WebPort, BasePath: s.WebBasePath}
}

// probePanelListen 试着监听一下再立刻关掉，做成变量只为测试。
var probePanelListen = func(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// CheckPanelAddrChange 判断把面板地址从 cur 改成 next 之后，面板还能不能从原来的
// 入口访问、能不能监听得起来。两条规则都要知道当前值，所以不放进 CheckValid。
//
// 一、安装向导的域名拓扑（面板监听回环地址，由本机 Caddy 反代）下不许改端口、
// 根路径，也不许换成另一个回环地址。install.sh 只在向导里写一次
// `handle /<根路径>* { reverse_proxy 127.0.0.1:<端口> }`，此后没有任何东西会同步
// 它：在设置页改了，保存并重启面板后 Caddy 仍把请求送往旧的端口与路径，面板
// 从外面再也打不开，而回环地址又意味着没有直连的退路。判据与面板证书那条规则
// 相同（isLoopbackListen，空串不算）；改成非回环地址等于主动离开这个拓扑，放行。
//
// 二、端口变了才试着监听一次新地址：被别的程序占着的端口存进去，下次启动必然
// 失败。端口没变时不试——面板自己正占着它，一试必然「被占用」；只改监听地址
// 也不试，原因相同（Linux 上同一端口的通配地址与具体地址互相冲突，冲突的正是
// 面板自己），监听地址能否使用已由 CheckWebListenIsLocal 判断过。
func CheckPanelAddrChange(cur, next PanelAddr) error {
	if isLoopbackListen(cur.Listen) && isLoopbackListen(next.Listen) &&
		(!net.ParseIP(cur.Listen).Equal(net.ParseIP(next.Listen)) ||
			cur.Port != next.Port || cur.BasePath != next.BasePath) {
		return common.NewErrorf("面板当前只监听回环地址 %v，由本机的 Caddy 按端口 %v 与根路径 %v 反向代理（安装向导的域名访问方式）。"+
			"在这里改端口、根路径或回环地址不会同步到 Caddy，保存并重启面板后域名访问会失效，面板从外面再也打不开。"+
			"如需更换，请 SSH 登录服务器运行 a-ui，选择「18. 配置域名与伪装站」重新运行配置向导（会同时改好 Caddy 与面板，根路径会重新随机生成）；"+
			"若要放弃域名访问，先选择「19. 卸载 Caddy 并恢复面板直连」再修改", cur.Listen, cur.Port, cur.BasePath)
	}
	if next.Port != cur.Port {
		if err := probePanelListen(net.JoinHostPort(next.Listen, strconv.Itoa(next.Port))); err != nil {
			return common.NewErrorf("面板无法监听端口 %v：%v。多半是已被其他程序占用，请换一个端口", next.Port, err)
		}
	}
	return nil
}
