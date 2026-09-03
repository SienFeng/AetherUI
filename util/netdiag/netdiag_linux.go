//go:build linux

package netdiag

import (
	"fmt"
	"sync/atomic"
	"syscall"
)

// Supported 表示本平台能否读取内核连接表。
const Supported = true

// 内核对一次 dump 的响应会分成多条报文，每次 recvfrom 返回若干条完整报文。
// 32 KB 大约能装下 400 条连接的响应，够一次少读几轮。
const recvBufSize = 32 * 1024

// 读超时。这段代码跑在采样 goroutine 里，卡死会让在线明细永远停在旧数据上。
const recvTimeoutSec = 5

var seqCounter atomic.Uint32

func nextSeq() uint32 {
	return seqCounter.Add(1)
}

func openNetlink() (int, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_INET_DIAG)
	if err != nil {
		return -1, fmt.Errorf("打开 netlink socket 失败: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("绑定 netlink socket 失败: %w", err)
	}
	tv := syscall.Timeval{Sec: recvTimeoutSec}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("设置 netlink 读超时失败: %w", err)
	}
	return fd, nil
}

func sendNetlink(fd int, req []byte) error {
	err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK})
	if err != nil {
		return fmt.Errorf("发送 netlink 请求失败: %w", err)
	}
	return nil
}

func recvNetlink(fd int, buf []byte) (int, error) {
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("读取 netlink 响应失败: %w", err)
		}
		return n, nil
	}
}

// Dump 列出指定地址族下全部 established 状态的 TCP 连接。
func Dump(family Family) ([]Conn, error) {
	fd, err := openNetlink()
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)

	if err := sendNetlink(fd, buildDumpRequest(uint8(family), nextSeq())); err != nil {
		return nil, err
	}

	var all []Conn
	buf := make([]byte, recvBufSize)
	for {
		n, err := recvNetlink(fd, buf)
		if err != nil {
			return nil, err
		}
		conns, done, err := parseDump(buf[:n])
		if err != nil {
			return nil, err
		}
		all = append(all, conns...)
		if done {
			return all, nil
		}
	}
}

// Destroy 强制关闭一条 TCP 连接。需要 CAP_NET_ADMIN，且内核要开
// CONFIG_INET_DIAG_DESTROY；两者缺一都会返回错误，不会假装成功。
func Destroy(c Conn) error {
	fd, err := openNetlink()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	if err := sendNetlink(fd, buildDestroyRequest(c, nextSeq())); err != nil {
		return err
	}

	buf := make([]byte, recvBufSize)
	n, err := recvNetlink(fd, buf)
	if err != nil {
		return err
	}
	// 成功时内核回一条 errno = 0 的 NLMSG_ERROR，parseDump 会把它当 ACK 放过。
	if _, _, err := parseDump(buf[:n]); err != nil {
		return err
	}
	return nil
}
