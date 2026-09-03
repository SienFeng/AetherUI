//go:build linux

package tcshape

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Supported 表示本平台能否下发限速规则。
const Supported = true

// ErrUnsupported 在 Linux 上用不到，定义出来是为了让调用方两边都能引用。
var ErrUnsupported = errors.New("端口限速依赖 Linux 的 tc，当前系统不支持")

// 单条命令的执行超时。tc/ip 都是瞬时命令，卡住只可能是异常。
const commandTimeout = 10 * time.Second

// Run 顺序执行命令。标记了 IgnoreError 的失败会被跳过，其余任何一条失败
// 都立刻返回——半套规则比没有规则更危险。
func Run(cmds []Command) error {
	for _, c := range cmds {
		if len(c.Args) == 0 {
			continue
		}
		out, err := runOne(c.Args)
		if err == nil {
			continue
		}
		if c.IgnoreError {
			continue
		}
		return fmt.Errorf("执行 %q 失败: %w (%s)", strings.Join(c.Args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func runOne(args []string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-time.After(commandTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return "", fmt.Errorf("执行超时")
	}
}

// DetectInterface 返回默认路由所在的网卡名。
func DetectInterface() (string, error) {
	data, err := os.ReadFile(procNetRoute)
	if err != nil {
		return "", fmt.Errorf("tcshape: 读取 %s 失败: %w", procNetRoute, err)
	}
	return parseDefaultRoute(string(data))
}

// IsActive 判断限速是否正由本面板接管。
//
// 用专属 ifb 设备的存在与否作为所有权标记：面板重启后内存状态没了，
// 但内核里的 tc 规则还在。没有这个判断的话，「配置里已经没有限速了」
// 与「本来就没下过限速」两种情况分不开，前者需要拆除、后者绝不能碰
// root qdisc——管理员可能自己配了 fq_codel/cake。
func IsActive() bool {
	_, err := os.Stat("/sys/class/net/" + IfbDevice)
	return err == nil
}
