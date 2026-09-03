package netdiag

import (
	"encoding/binary"
	"net"
	"testing"
)

// 下面这组 helper 按内核的线格式手工拼字节。开发机是 macOS，没有 netlink，
// 只有把「拼请求」和「解响应」做成纯函数才能在这里驱动它们。

func encodeNlMsg(msgType uint16, payload []byte) []byte {
	buf := make([]byte, nlMsgHdrLen+align4(len(payload)))
	binary.NativeEndian.PutUint32(buf[0:], uint32(nlMsgHdrLen+len(payload)))
	binary.NativeEndian.PutUint16(buf[4:], msgType)
	copy(buf[nlMsgHdrLen:], payload)
	return buf
}

func encodeAttr(attrType uint16, payload []byte) []byte {
	buf := make([]byte, rtAttrHdrLen+align4(len(payload)))
	binary.NativeEndian.PutUint16(buf[0:], uint16(rtAttrHdrLen+len(payload)))
	binary.NativeEndian.PutUint16(buf[2:], attrType)
	copy(buf[rtAttrHdrLen:], payload)
	return buf
}

// encodeTCPInfo 只填我们真正读的那两个字段，其余留零。
func encodeTCPInfo(bytesAcked, bytesReceived uint64) []byte {
	buf := make([]byte, tcpInfoBytesReceivedOff+8)
	binary.NativeEndian.PutUint64(buf[tcpInfoBytesAckedOff:], bytesAcked)
	binary.NativeEndian.PutUint64(buf[tcpInfoBytesReceivedOff:], bytesReceived)
	return buf
}

func encodeDiagMsg(family uint8, sport, dport uint16, src, dst net.IP, cookie uint64) []byte {
	buf := make([]byte, diagMsgLen)
	buf[0] = family
	buf[1] = tcpStateEstablished
	binary.BigEndian.PutUint16(buf[4:], sport)
	binary.BigEndian.PutUint16(buf[6:], dport)
	if family == afInet {
		copy(buf[8:], src.To4())
		copy(buf[24:], dst.To4())
	} else {
		copy(buf[8:], src.To16())
		copy(buf[24:], dst.To16())
	}
	binary.NativeEndian.PutUint64(buf[44:], cookie)
	return buf
}

func diagResponse(family uint8, sport, dport uint16, src, dst net.IP, cookie uint64, info []byte) []byte {
	payload := encodeDiagMsg(family, sport, dport, src, dst, cookie)
	if info != nil {
		payload = append(payload, encodeAttr(inetDiagInfo, info)...)
	}
	return encodeNlMsg(sockDiagByFamily, payload)
}

func TestParseDumpExtractsIPv4Connection(t *testing.T) {
	buf := diagResponse(afInet, 39001, 51234,
		net.ParseIP("10.0.0.5"), net.ParseIP("114.114.114.114"), 0x1122334455667788,
		encodeTCPInfo(9000, 700))

	conns, done, err := parseDump(buf)
	if err != nil {
		t.Fatalf("parseDump 报错: %v", err)
	}
	if done {
		t.Error("没有 NLMSG_DONE，done 不应为 true")
	}
	if len(conns) != 1 {
		t.Fatalf("连接数 = %d，期望 1", len(conns))
	}
	c := conns[0]
	if c.LocalPort != 39001 {
		t.Errorf("LocalPort = %d，期望 39001", c.LocalPort)
	}
	if c.RemotePort != 51234 {
		t.Errorf("RemotePort = %d，期望 51234", c.RemotePort)
	}
	if !c.RemoteIP.Equal(net.ParseIP("114.114.114.114")) {
		t.Errorf("RemoteIP = %v，期望 114.114.114.114", c.RemoteIP)
	}
	if c.Cookie != 0x1122334455667788 {
		t.Errorf("Cookie = %#x，期望 0x1122334455667788", c.Cookie)
	}
	// tcpi_bytes_acked 是服务端发出去并被确认的字节 = 下行；
	// tcpi_bytes_received 是收到客户端的字节 = 上行。
	if !c.HasBytes {
		t.Fatal("HasBytes = false，期望 true")
	}
	if c.BytesDown != 9000 {
		t.Errorf("BytesDown = %d，期望 9000", c.BytesDown)
	}
	if c.BytesUp != 700 {
		t.Errorf("BytesUp = %d，期望 700", c.BytesUp)
	}
}

func TestParseDumpExtractsIPv6Connection(t *testing.T) {
	buf := diagResponse(afInet6, 39002, 40000,
		net.ParseIP("::1"), net.ParseIP("2408:8207::1"), 7, encodeTCPInfo(1, 2))

	conns, _, err := parseDump(buf)
	if err != nil {
		t.Fatalf("parseDump 报错: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("连接数 = %d，期望 1", len(conns))
	}
	if !conns[0].RemoteIP.Equal(net.ParseIP("2408:8207::1")) {
		t.Errorf("RemoteIP = %v，期望 2408:8207::1", conns[0].RemoteIP)
	}
}

func TestParseDumpReadsAllMessagesAndReportsDone(t *testing.T) {
	var buf []byte
	buf = append(buf, diagResponse(afInet, 39001, 1, net.ParseIP("10.0.0.5"), net.ParseIP("1.2.3.4"), 1, encodeTCPInfo(1, 1))...)
	buf = append(buf, diagResponse(afInet, 39001, 2, net.ParseIP("10.0.0.5"), net.ParseIP("5.6.7.8"), 2, encodeTCPInfo(2, 2))...)
	buf = append(buf, encodeNlMsg(nlMsgDone, nil)...)

	conns, done, err := parseDump(buf)
	if err != nil {
		t.Fatalf("parseDump 报错: %v", err)
	}
	if !done {
		t.Error("收到 NLMSG_DONE，done 应为 true")
	}
	if len(conns) != 2 {
		t.Fatalf("连接数 = %d，期望 2", len(conns))
	}
}

func TestParseDumpReportsNetlinkError(t *testing.T) {
	// NLMSG_ERROR 的载荷前 4 字节是负的 errno。
	payload := make([]byte, 4)
	errno := int32(-1) // EPERM
	binary.NativeEndian.PutUint32(payload, uint32(errno))
	_, _, err := parseDump(encodeNlMsg(nlMsgError, payload))
	if err == nil {
		t.Fatal("NLMSG_ERROR 必须报错，不能当成空结果")
	}
}

func TestParseDumpRejectsTruncatedDiagMsg(t *testing.T) {
	short := make([]byte, diagMsgLen-1)
	_, _, err := parseDump(encodeNlMsg(sockDiagByFamily, short))
	if err == nil {
		t.Fatal("载荷短于 inet_diag_msg 必须报错，不能越界读")
	}
}

func TestParseDumpMarksBytesUnknownWithoutTCPInfo(t *testing.T) {
	buf := diagResponse(afInet, 39001, 1, net.ParseIP("10.0.0.5"), net.ParseIP("1.2.3.4"), 1, nil)
	conns, _, err := parseDump(buf)
	if err != nil {
		t.Fatalf("parseDump 报错: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("连接数 = %d，期望 1", len(conns))
	}
	if conns[0].HasBytes {
		t.Error("内核没返回 tcp_info 时 HasBytes 必须为 false")
	}
}

func TestParseDumpMarksBytesUnknownWhenTCPInfoTooShort(t *testing.T) {
	// 老内核的 tcp_info 里没有 tcpi_bytes_acked/received，按偏移硬读会读到别的字段。
	short := make([]byte, tcpInfoBytesAckedOff)
	buf := diagResponse(afInet, 39001, 1, net.ParseIP("10.0.0.5"), net.ParseIP("1.2.3.4"), 1, short)
	conns, _, err := parseDump(buf)
	if err != nil {
		t.Fatalf("parseDump 报错: %v", err)
	}
	if conns[0].HasBytes {
		t.Error("tcp_info 短于所需长度时 HasBytes 必须为 false")
	}
}

func TestBuildDumpRequestMatchesKernelLayout(t *testing.T) {
	req := buildDumpRequest(afInet, 42)

	if len(req) != nlMsgHdrLen+diagReqLen {
		t.Fatalf("请求长度 = %d，期望 %d", len(req), nlMsgHdrLen+diagReqLen)
	}
	if got := binary.NativeEndian.Uint32(req[0:]); got != uint32(len(req)) {
		t.Errorf("nlmsg_len = %d，期望 %d", got, len(req))
	}
	if got := binary.NativeEndian.Uint16(req[4:]); got != sockDiagByFamily {
		t.Errorf("nlmsg_type = %d，期望 %d", got, sockDiagByFamily)
	}
	if got := binary.NativeEndian.Uint16(req[6:]); got != nlmFRequest|nlmFDump {
		t.Errorf("nlmsg_flags = %#x，期望 %#x", got, nlmFRequest|nlmFDump)
	}
	if got := binary.NativeEndian.Uint32(req[8:]); got != 42 {
		t.Errorf("nlmsg_seq = %d，期望 42", got)
	}

	body := req[nlMsgHdrLen:]
	if body[0] != afInet {
		t.Errorf("sdiag_family = %d，期望 %d", body[0], afInet)
	}
	if body[1] != ipprotoTCP {
		t.Errorf("sdiag_protocol = %d，期望 %d", body[1], ipprotoTCP)
	}
	if body[2] != 1<<(inetDiagInfo-1) {
		t.Errorf("idiag_ext = %#x，没有申请 INET_DIAG_INFO，就拿不到字节计数", body[2])
	}
	if got := binary.NativeEndian.Uint32(body[4:]); got != 1<<tcpStateEstablished {
		t.Errorf("idiag_states = %#x，期望只要 ESTABLISHED", got)
	}
}

func TestBuildDestroyRequestCarriesSocketIdentity(t *testing.T) {
	c := Conn{
		Cookie:     0x99887766554433,
		LocalIP:    net.ParseIP("10.0.0.5"),
		LocalPort:  39001,
		RemoteIP:   net.ParseIP("114.114.114.114"),
		RemotePort: 51234,
	}
	req := buildDestroyRequest(c, 7)

	if got := binary.NativeEndian.Uint16(req[4:]); got != sockDestroy {
		t.Errorf("nlmsg_type = %d，期望 SOCK_DESTROY(%d)", got, sockDestroy)
	}
	flags := binary.NativeEndian.Uint16(req[6:])
	// 不带 NLM_F_DUMP：这是针对单个 socket 的操作，不是遍历。
	if flags&nlmFDump != 0 {
		t.Errorf("nlmsg_flags = %#x，SOCK_DESTROY 不能带 NLM_F_DUMP", flags)
	}
	// 必须带 NLM_F_ACK：否则内核只在失败时回消息，成功与"内核不支持"无法区分，
	// 踢人接口就会在什么都没做的情况下报成功。
	if flags&nlmFAck == 0 {
		t.Errorf("nlmsg_flags = %#x，缺少 NLM_F_ACK，拿不到执行结果", flags)
	}

	body := req[nlMsgHdrLen:]
	if got := binary.BigEndian.Uint16(body[reqSockIDOff:]); got != 39001 {
		t.Errorf("idiag_sport = %d，期望 39001（网络字节序）", got)
	}
	if got := binary.BigEndian.Uint16(body[reqSockIDOff+2:]); got != 51234 {
		t.Errorf("idiag_dport = %d，期望 51234（网络字节序）", got)
	}
	if got := net.IP(body[reqSockIDOff+20 : reqSockIDOff+24]); !got.Equal(net.ParseIP("114.114.114.114")) {
		t.Errorf("idiag_dst = %v，期望 114.114.114.114", got)
	}
	if got := binary.NativeEndian.Uint64(body[reqSockIDOff+40:]); got != c.Cookie {
		t.Errorf("idiag_cookie = %#x，期望 %#x；cookie 不对内核会拒绝或误杀别的连接", got, c.Cookie)
	}
}
