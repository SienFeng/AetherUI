//go:build !linux

package tcshape

import "errors"

// Supported 表示本平台能否下发限速规则。
const Supported = false

// ErrUnsupported 表示当前平台没有 tc / iproute2。
var ErrUnsupported = errors.New("端口限速依赖 Linux 的 tc，当前系统不支持")

// Run 在非 Linux 平台上一律返回 ErrUnsupported，绝不假装执行成功。
func Run([]Command) error { return ErrUnsupported }

// DetectInterface 在非 Linux 平台上一律返回 ErrUnsupported。
func DetectInterface() (string, error) { return "", ErrUnsupported }

// IsActive 在非 Linux 平台上恒为 false。
func IsActive() bool { return false }
