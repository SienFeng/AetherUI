// Package netdiag 通过 Linux 的 netlink INET_DIAG 读取内核 TCP 连接表。
//
// 只有真正收发 netlink 报文的那一小段是平台相关的（netdiag_linux.go）；
// 报文的拼装与解析都在本文件里，是不依赖任何系统调用的纯函数，
// 因此可以在非 Linux 的开发机上完整测试。
package netdiag

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ErrUnsupported 表示当前平台没有 netlink，连接表能力不可用。
// 调用方必须把它如实透出到界面，不能当成"没有在线连接"。
var ErrUnsupported = errors.New("内核连接表仅在 Linux 上可用")

// 以下常量全部是 Linux 内核的线格式取值，与本地平台的 syscall 常量无关，
// 因此直接写死——在 macOS 上 AF_INET6 是 30，而我们要发给 Linux 内核的是 10。
const (
	afInet  uint8 = 2
	afInet6 uint8 = 10

	ipprotoTCP uint8 = 6

	tcpStateEstablished = 1

	// netlink 消息类型。SOCK_DIAG_BY_FAMILY / SOCK_DESTROY 不在 Go 的 syscall 包里。
	nlMsgError       uint16 = 0x2
	nlMsgDone        uint16 = 0x3
	sockDiagByFamily uint16 = 20
	sockDestroy      uint16 = 21

	nlmFRequest uint16 = 0x001
	nlmFAck     uint16 = 0x004
	nlmFDump    uint16 = 0x300 // NLM_F_ROOT | NLM_F_MATCH

	nlMsgHdrLen  = 16 // struct nlmsghdr
	rtAttrHdrLen = 4  // struct rtattr
	diagReqLen   = 56 // struct inet_diag_req_v2
	diagMsgLen   = 72 // struct inet_diag_msg
	sockIDLen    = 48 // struct inet_diag_sockid

	// inet_diag_req_v2 里 sockid 的起始偏移（family/protocol/ext/pad + states）。
	reqSockIDOff = 8
	// inet_diag_msg 里 sockid 的起始偏移（family/state/timer/retrans）。
	msgSockIDOff = 4

	inetDiagInfo uint16 = 2 // 扩展号，同时也是响应里 tcp_info 的属性类型

	// struct tcp_info 里两个字节计数器的偏移。前面是 8 字节的 u8 字段
	// （含 wscale 与 delivery_rate_app_limited）加 24 个 u32，再加两个 u64
	// （pacing_rate / max_pacing_rate），正好落在 120 / 128。
	// 这两个字段自 Linux 4.1 起存在；更老的内核 tcp_info 更短，靠长度检查兜住。
	tcpInfoBytesAckedOff    = 120
	tcpInfoBytesReceivedOff = 128
)

// Family 是要查询的地址族。
type Family uint8

const (
	FamilyIPv4 Family = Family(afInet)
	FamilyIPv6 Family = Family(afInet6)
)

// Conn 是内核连接表里的一条 established TCP 连接。
type Conn struct {
	// Cookie 是内核为该 socket 分配的唯一标识，用于跨采样识别同一条连接，
	// 也是 SOCK_DESTROY 的必要参数——只靠四元组会误杀重建的同名连接。
	Cookie uint64

	LocalIP    net.IP
	LocalPort  uint16
	RemoteIP   net.IP
	RemotePort uint16

	// BytesUp 是客户端 → 服务端的累计字节（tcpi_bytes_received），
	// BytesDown 是服务端 → 客户端的累计字节（tcpi_bytes_acked），
	// 与面板既有的 up/down 口径一致。
	BytesUp   uint64
	BytesDown uint64

	// HasBytes 为 false 表示内核没给 tcp_info 或版本太老，字节数不可信。
	HasBytes bool
}

func align4(n int) int {
	return (n + 3) &^ 3
}

// buildDumpRequest 拼一条「列出某地址族的全部 established TCP 连接」的请求。
func buildDumpRequest(family uint8, seq uint32) []byte {
	buf := make([]byte, nlMsgHdrLen+diagReqLen)

	binary.NativeEndian.PutUint32(buf[0:], uint32(len(buf)))
	binary.NativeEndian.PutUint16(buf[4:], sockDiagByFamily)
	binary.NativeEndian.PutUint16(buf[6:], nlmFRequest|nlmFDump)
	binary.NativeEndian.PutUint32(buf[8:], seq)
	// nlmsg_pid 留 0，由内核填。

	body := buf[nlMsgHdrLen:]
	body[0] = family
	body[1] = ipprotoTCP
	// 申请 INET_DIAG_INFO 扩展，否则响应里没有 tcp_info，也就没有字节计数。
	// 扩展位从 1 开始编号，所以要减一。
	body[2] = 1 << (inetDiagInfo - 1)
	binary.NativeEndian.PutUint32(body[4:], 1<<tcpStateEstablished)

	return buf
}

// buildDestroyRequest 拼一条针对单个 socket 的强制关闭请求。
func buildDestroyRequest(c Conn, seq uint32) []byte {
	buf := make([]byte, nlMsgHdrLen+diagReqLen)

	binary.NativeEndian.PutUint32(buf[0:], uint32(len(buf)))
	binary.NativeEndian.PutUint16(buf[4:], sockDestroy)
	// 带 ACK 才能拿到执行结果：内核在成功时也回一条 errno=0 的 NLMSG_ERROR，
	// 否则"关掉了"和"内核没编译 CONFIG_INET_DIAG_DESTROY"无法区分。
	binary.NativeEndian.PutUint16(buf[6:], nlmFRequest|nlmFAck)
	binary.NativeEndian.PutUint32(buf[8:], seq)

	body := buf[nlMsgHdrLen:]
	family := afInet6
	if c.RemoteIP.To4() != nil {
		family = afInet
	}
	body[0] = family
	body[1] = ipprotoTCP
	binary.NativeEndian.PutUint32(body[4:], 1<<tcpStateEstablished)
	putSockID(body[reqSockIDOff:], family, c)

	return buf
}

func putSockID(dst []byte, family uint8, c Conn) {
	binary.BigEndian.PutUint16(dst[0:], c.LocalPort)
	binary.BigEndian.PutUint16(dst[2:], c.RemotePort)
	if family == afInet {
		copy(dst[4:], c.LocalIP.To4())
		copy(dst[20:], c.RemoteIP.To4())
	} else {
		copy(dst[4:], c.LocalIP.To16())
		copy(dst[20:], c.RemoteIP.To16())
	}
	// dst[36:40] 是 idiag_if，留 0。
	binary.NativeEndian.PutUint64(dst[40:], c.Cookie)
}

// parseDump 解析一段 netlink 响应缓冲区。第二个返回值为 true 表示遇到了
// NLMSG_DONE，调用方可以停止继续读。
func parseDump(buf []byte) ([]Conn, bool, error) {
	var conns []Conn

	for len(buf) >= nlMsgHdrLen {
		msgLen := int(binary.NativeEndian.Uint32(buf[0:]))
		msgType := binary.NativeEndian.Uint16(buf[4:])
		if msgLen < nlMsgHdrLen || msgLen > len(buf) {
			return nil, false, fmt.Errorf("netlink 消息长度非法: %d（剩余 %d 字节）", msgLen, len(buf))
		}
		payload := buf[nlMsgHdrLen:msgLen]

		switch msgType {
		case nlMsgDone:
			return conns, true, nil
		case nlMsgError:
			if err := netlinkError(payload); err != nil {
				return nil, false, err
			}
		case sockDiagByFamily:
			c, err := parseDiagMsg(payload)
			if err != nil {
				return nil, false, err
			}
			conns = append(conns, c)
		}

		buf = buf[align4(msgLen):]
	}

	return conns, false, nil
}

// netlinkError 解析 NLMSG_ERROR 的载荷。errno 为 0 时这是一条 ACK，不是错误。
func netlinkError(payload []byte) error {
	if len(payload) < 4 {
		return errors.New("netlink 错误消息被截断")
	}
	code := int32(binary.NativeEndian.Uint32(payload))
	if code == 0 {
		return nil
	}
	return fmt.Errorf("netlink 返回错误: errno %d", -code)
}

func parseDiagMsg(payload []byte) (Conn, error) {
	if len(payload) < diagMsgLen {
		return Conn{}, fmt.Errorf("inet_diag_msg 被截断: %d 字节，至少需要 %d", len(payload), diagMsgLen)
	}

	family := payload[0]
	id := payload[msgSockIDOff : msgSockIDOff+sockIDLen]

	c := Conn{
		LocalPort:  binary.BigEndian.Uint16(id[0:]),
		RemotePort: binary.BigEndian.Uint16(id[2:]),
		Cookie:     binary.NativeEndian.Uint64(id[40:]),
	}
	if family == afInet {
		c.LocalIP = net.IP(append([]byte(nil), id[4:8]...))
		c.RemoteIP = net.IP(append([]byte(nil), id[20:24]...))
	} else {
		c.LocalIP = net.IP(append([]byte(nil), id[4:20]...))
		c.RemoteIP = net.IP(append([]byte(nil), id[20:36]...))
	}

	if info, ok := findAttr(payload[diagMsgLen:], inetDiagInfo); ok && len(info) >= tcpInfoBytesReceivedOff+8 {
		c.BytesDown = binary.NativeEndian.Uint64(info[tcpInfoBytesAckedOff:])
		c.BytesUp = binary.NativeEndian.Uint64(info[tcpInfoBytesReceivedOff:])
		c.HasBytes = true
	}

	return c, nil
}

func findAttr(buf []byte, want uint16) ([]byte, bool) {
	for len(buf) >= rtAttrHdrLen {
		attrLen := int(binary.NativeEndian.Uint16(buf[0:]))
		attrType := binary.NativeEndian.Uint16(buf[2:])
		if attrLen < rtAttrHdrLen || attrLen > len(buf) {
			return nil, false
		}
		if attrType == want {
			return buf[rtAttrHdrLen:attrLen], true
		}
		buf = buf[align4(attrLen):]
	}
	return nil, false
}
