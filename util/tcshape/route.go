package tcshape

import (
	"errors"
	"strconv"
	"strings"
)

// procNetRoute 是内核导出的路由表。用它而不是执行 `ip route`：
// 少一次进程创建，也不依赖 iproute2 的输出格式。
const procNetRoute = "/proc/net/route"

// parseDefaultRoute 从 /proc/net/route 的内容里挑出默认路由所在的网卡。
//
// 目的地址为 00000000 的行就是默认路由；有多条时取 metric 最小的那条。
// 找不到时报错而不是猜一个——拿错网卡去下限速规则，轻则不生效，
// 重则动了不该动的那块网卡。
func parseDefaultRoute(content string) (string, error) {
	best := ""
	bestMetric := 0
	for i, line := range strings.Split(content, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}
		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = fields[0], metric
		}
	}
	if best == "" {
		return "", errors.New("tcshape: 找不到默认路由所在的网卡")
	}
	if !validIface(best) {
		return "", errors.New("tcshape: 默认路由的网卡名不合法: " + best)
	}
	return best, nil
}
