package service

import (
	"sort"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/netdiag"
	"a-ui/xray"
)

// 热应用删除或修改入站之后，断开它在旧端口上已经建立的连接。
//
// xray 的 RemoveHandler 只关监听器（app/proxyman/inbound/worker.go 的 tcpWorker.Close 只关 hub），
// 已建立的连接继续转发：到期、超额、手动停用、删除的用户都还能接着用，审计实测一条 socks 会话在
// 停用后照常回显，1 MiB 额度之外又跑了 4 MiB。改动前整进程重启会顺带掐断所有连接，热应用上线后
// 这条副作用没了，却没有别的东西补上。

// connKillSupported 报告本平台能否断开已建立的连接；killPortConns 断开本地端口为 port 的全部已建立
// TCP 连接。做成变量是为了让测试在没有 netdiag 的平台上验证两条分支。
var (
	connKillSupported = func() bool { return netdiag.Supported }
	killPortConns     = func(port int) (int, error) { return (&OnlineService{}).KillPort(port) }
)

// hotDiffKillPorts 返回 diff 应用之后要断开旧连接的端口（升序去重）：被删除的入站与被修改的入站
// （ComputeHotDiff 把修改记成「先删后加」，二者都在 RemovedInboundTags 里），取它们在**旧配置**里的
// 端口——改了端口的，旧端口上的连接才是要断的。新增的、没变的不碰。
func hotDiffKillPorts(oldCfg *xray.Config, diff *xray.HotDiff) []int {
	if oldCfg == nil || diff == nil || len(diff.RemovedInboundTags) == 0 {
		return nil
	}
	removed := make(map[string]bool, len(diff.RemovedInboundTags))
	for _, tag := range diff.RemovedInboundTags {
		removed[tag] = true
	}
	seen := map[int]bool{}
	ports := make([]int, 0, len(diff.RemovedInboundTags))
	for _, ib := range oldCfg.InboundConfigs {
		if !removed[ib.Tag] || ib.Port <= 0 || seen[ib.Port] {
			continue
		}
		seen[ib.Port] = true
		ports = append(ports, ib.Port)
	}
	sort.Ints(ports)
	return ports
}

// killInboundConns 断开 ports 上已建立的连接。任何一个端口没断干净都返回错误：调用方据此退回
// 整进程重启——只有它能保证旧连接一条不剩，把「监听器关了」当成「用户断了」就是这个缺陷本身。
//
// 按端口断而不是按新旧连接区分：修改过的入站在同一端口上重新监听，热应用到这一步之间新连进来的
// 也会被断一次，客户端重连即可；反过来漏掉一条旧连接就是停用不生效。
func killInboundConns(ports []int) error {
	for _, port := range ports {
		n, err := killPortConns(port)
		if err != nil {
			return common.NewErrorf("断开端口 %d 上的已有连接失败: %v", port, err)
		}
		if n > 0 {
			logger.Info("热应用：已断开端口", port, "上的", n, "条旧连接")
		}
	}
	return nil
}
