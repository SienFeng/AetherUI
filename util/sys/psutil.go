package sys

import (
	"os"
	"path/filepath"
)

// HostProc 返回 procfs 的挂载点，可选地再拼上若干路径片段。
//
// 原实现用 //go:linkname 借用 gopsutil 的 internal/common.HostProc，
// 把本包与 gopsutil 的内部布局绑死：换个大版本（v3 → v4 导入路径带 /v4）
// 符号就找不到，且报的是链接期错误，比编译错误难查。函数本体只有读环境
// 变量、缺省 /proc、Join 三件事，实现掉就把这条依赖断干净了。
//
// 变量名沿用 gopsutil 的 HOST_PROC：容器里把宿主机 /proc 挂到别处时，
// 本包与 gopsutil 采集到的是同一份数据，不会一个看容器、一个看宿主机。
func HostProc(combineWith ...string) string {
	root := os.Getenv("HOST_PROC")
	if root == "" {
		root = "/proc"
	}
	if len(combineWith) == 0 {
		return root
	}
	return filepath.Join(append([]string{root}, combineWith...)...)
}
