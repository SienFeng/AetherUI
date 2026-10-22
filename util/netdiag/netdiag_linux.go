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

// closeRcvBufSize 是销毁通知 socket 的接收缓冲区。通知是多播，读得慢了内核
// 直接丢弃、下一次读返回 ENOBUFS；繁忙的代理每秒可能关掉上千条连接（含
// xray 自己连出去的那些），默认的 200 KB 左右撑不住突发。
const closeRcvBufSize = 4 << 20

// WatchClosed 订阅内核的 TCP 连接销毁通知，每收到一条就以「关闭时的最终
// 字节数」调用一次 handle。它会一直阻塞，只在订阅失败或读取出错时返回。
//
// 通知覆盖整机的 TCP socket，过滤由调用方做。接收缓冲区溢出时内核已经丢了
// 若干条通知，调用 overflow 后继续读——丢掉的只是那几条连接的尾段字节，
// 不该让整个订阅停下来。
//
// 需要 root（CAP_NET_ADMIN）以及 Linux 4.9 以上（sock_diag 销毁广播）；缺一
// 都会在 bind 时返回错误，不会静默订阅到空。
func WatchClosed(handle func(Conn), overflow func()) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_INET_DIAG)
	if err != nil {
		return fmt.Errorf("打开 netlink socket 失败: %w", err)
	}
	defer syscall.Close(fd)

	// FORCE 版本不受 net.core.rmem_max 限制（需要 CAP_NET_ADMIN）；失败时退回
	// 普通版本，拿到多少算多少。
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, closeRcvBufSize); err != nil {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, closeRcvBufSize)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: destroyGroups()}); err != nil {
		return fmt.Errorf("订阅连接销毁通知失败: %w", err)
	}

	buf := make([]byte, recvBufSize)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.ENOBUFS {
			overflow()
			continue
		}
		if err != nil {
			return fmt.Errorf("读取连接销毁通知失败: %w", err)
		}
		// 一段报文解析失败只丢这一段，后面的通知照常处理。
		conns, _, err := parseDump(buf[:n])
		if err != nil {
			continue
		}
		for _, c := range conns {
			handle(c)
		}
	}
}
