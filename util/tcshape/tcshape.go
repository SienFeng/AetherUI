// Package tcshape 生成 Linux tc 的限速命令序列。
//
// 只负责「算出要执行哪些命令」，不执行——命令生成是纯函数，可以在任何
// 平台上完整单测；真正的执行在 tcshape_linux.go，非 Linux 一律不可用。
//
// 模型：
//
//	下行（服务端 → 客户端）= 主网卡的 egress，按**源端口**分类；
//	上行（客户端 → 服务端）= 主网卡的 ingress。tc 不能直接整形 ingress，
//	标准做法是把它重定向到 ifb 虚拟设备，在 ifb 上做 egress，按**目的端口**分类。
//
// 安全底线：两棵树的 root qdisc 都带 `default 9999`，未被任何 filter 命中的
// 流量落到 1:9999 这个不限速的类。**SSH 走的就是这一类。** 改动本包时这条
// 不能削弱——配错会让整机失联。
package tcshape

import (
	"fmt"
	"sort"
	"strconv"
)

const (
	// DefaultClassId 是兜底类。未被 filter 命中的流量全落在这里，不限速。
	DefaultClassId = 9999

	// IfbDevice 是本项目专用的 ifb 设备名。用独有的名字而不是通用的 ifb0，
	// 一来不会踩到管理员自己建的设备，二来它的存在就是「限速由本面板接管中」
	// 的所有权标记。
	IfbDevice = "a-ui-ifb0"

	// unlimitedMbit 是默认类的速率。HTB 的类必须给速率，给一个远高于任何
	// 真实链路的值，效果上等同不限速。
	unlimitedMbit = 10000

	// maxMbit 是单个入站限速的上限，纯粹是防手滑输入天文数字。
	maxMbit = 100000
)

// Limit 是一个入站的限速配置。UpMbit / DownMbit 为 0 表示该方向不限。
type Limit struct {
	InboundId int
	Port      int
	UpMbit    int
	DownMbit  int
}

// Command 是一条要执行的命令。
type Command struct {
	Args []string
	// IgnoreError 为 true 时执行失败不算失败。拆除类命令都是这样：
	// 规则本来就不存在时 tc 会报错，当成失败会让整次应用中断。
	IgnoreError bool
}

// validIface 只允许网卡名里出现字母、数字和少数几个符号。
// 这些名字会被拼进 exec 的参数列表（不经过 shell），但仍然挡一道：
// 一个奇怪的名字更可能是配置写错了，而不是真有这块网卡。
func validIface(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == ':' || r == '@':
		default:
			return false
		}
	}
	return true
}

func mbit(v int) string { return strconv.Itoa(v) + "mbit" }

func classId(inboundId int) string { return "1:" + strconv.Itoa(inboundId) }

// BuildApplyPlan 生成「推倒重建」的完整命令序列。
//
// 不做增量 diff：整树重建才是真正幂等的，漏跑一轮、状态被外部改动都能自愈。
// 代价是重建瞬间流量不受整形（不是丢弃），可以接受。
func BuildApplyPlan(iface string, limits []Limit) ([]Command, error) {
	if !validIface(iface) {
		return nil, fmt.Errorf("tcshape: 网卡名不合法: %q", iface)
	}

	sorted := make([]Limit, 0, len(limits))
	seen := map[int]bool{}
	for _, l := range limits {
		if l.InboundId <= 0 || l.InboundId >= DefaultClassId {
			// 撞上默认类的话，这个用户的限速会变成整机未分类流量的限速，
			// SSH 会跟着一起被限死。
			return nil, fmt.Errorf("tcshape: 入站 id %d 超出可用范围 1~%d",
				l.InboundId, DefaultClassId-1)
		}
		if seen[l.InboundId] {
			return nil, fmt.Errorf("tcshape: 入站 id %d 重复", l.InboundId)
		}
		seen[l.InboundId] = true
		if l.Port <= 0 || l.Port > 65535 {
			return nil, fmt.Errorf("tcshape: 入站 %d 的端口 %d 不合法", l.InboundId, l.Port)
		}
		if l.UpMbit < 0 || l.DownMbit < 0 || l.UpMbit > maxMbit || l.DownMbit > maxMbit {
			return nil, fmt.Errorf("tcshape: 入站 %d 的限速值超出 0~%d Mbps", l.InboundId, maxMbit)
		}
		if l.UpMbit == 0 && l.DownMbit == 0 {
			return nil, fmt.Errorf("tcshape: 入站 %d 上下行都没有限速，不应出现在计划里", l.InboundId)
		}
		sorted = append(sorted, l)
	}
	if len(sorted) == 0 {
		return nil, fmt.Errorf("tcshape: 没有任何限速配置")
	}
	// 按入站 id 升序：命令序列必须逐条确定，否则没法用哈希判断
	// 「需不需要重新下发」，每轮都会推倒重建。
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].InboundId < sorted[j].InboundId })

	cmds := BuildTeardownPlan(iface)

	// ifb 设备。可能已存在（上一轮没拆干净），容错。
	cmds = append(cmds,
		Command{Args: []string{"ip", "link", "add", IfbDevice, "type", "ifb"}, IgnoreError: true},
		Command{Args: []string{"ip", "link", "set", IfbDevice, "up"}},
	)

	// 下行：主网卡 egress。
	cmds = append(cmds, rootTree(iface)...)
	for _, l := range sorted {
		if l.DownMbit == 0 {
			continue
		}
		cmds = append(cmds, leafClass(iface, l.InboundId, l.DownMbit))
		cmds = append(cmds, portFilters(iface, "sport", l.Port, l.InboundId)...)
	}

	// 上行：主网卡 ingress 全量重定向到 ifb，在 ifb 上做 egress。
	//
	// 顺序很重要：先在 ifb 上把队列树建好，最后才装重定向。反过来的话，
	// 中间那一小段时间里全部入站流量会被送进一个还没配好的设备。
	cmds = append(cmds, rootTree(IfbDevice)...)
	for _, l := range sorted {
		if l.UpMbit == 0 {
			continue
		}
		cmds = append(cmds, leafClass(IfbDevice, l.InboundId, l.UpMbit))
		cmds = append(cmds, portFilters(IfbDevice, "dport", l.Port, l.InboundId)...)
	}
	cmds = append(cmds,
		Command{Args: []string{"tc", "qdisc", "add", "dev", iface, "handle", "ffff:", "ingress"}},
		Command{Args: []string{
			"tc", "filter", "add", "dev", iface, "parent", "ffff:",
			"protocol", "all", "prio", "1", "u32", "match", "u32", "0", "0",
			"action", "mirred", "egress", "redirect", "dev", IfbDevice,
		}},
	)

	return cmds, nil
}

// rootTree 建 HTB 根与那个**不限速的默认类**。这两条命令是安全底线，
// 缺了默认类的话未分类流量（含 SSH）会没有归属。
func rootTree(dev string) []Command {
	def := "1:" + strconv.Itoa(DefaultClassId)
	return []Command{
		{Args: []string{"tc", "qdisc", "add", "dev", dev, "root", "handle", "1:",
			"htb", "default", strconv.Itoa(DefaultClassId)}},
		{Args: []string{"tc", "class", "add", "dev", dev, "parent", "1:", "classid", def,
			"htb", "rate", mbit(unlimitedMbit), "ceil", mbit(unlimitedMbit)}},
	}
}

func leafClass(dev string, inboundId, rate int) Command {
	return Command{Args: []string{
		"tc", "class", "add", "dev", dev, "parent", "1:", "classid", classId(inboundId),
		"htb", "rate", mbit(rate), "ceil", mbit(rate),
	}}
}

// portFilters 为一个端口生成 IPv4 与 IPv6 两条 filter。
// u32 的 protocol ip 只匹配 IPv4，只下 v4 的话 IPv6 客户端完全不受限速。
func portFilters(dev, which string, port, inboundId int) []Command {
	p := strconv.Itoa(port)
	flow := classId(inboundId)
	return []Command{
		{Args: []string{
			"tc", "filter", "add", "dev", dev, "parent", "1:",
			"protocol", "ip", "prio", "1", "u32",
			"match", "ip", which, p, "0xffff", "flowid", flow,
		}},
		{Args: []string{
			"tc", "filter", "add", "dev", dev, "parent", "1:",
			"protocol", "ipv6", "prio", "2", "u32",
			"match", "ip6", which, p, "0xffff", "flowid", flow,
		}},
	}
}

// BuildTeardownPlan 生成把限速完全撤掉的命令序列。
//
// 每条都忽略错误：规则不存在时 tc 会报错，那是正常情形。
// 网卡名不合法时返回空计划，绝不拿一个可疑的名字去执行删除。
func BuildTeardownPlan(iface string) []Command {
	if !validIface(iface) {
		return nil
	}
	cmds := []Command{
		{Args: []string{"tc", "qdisc", "del", "dev", iface, "root"}, IgnoreError: true},
		{Args: []string{"tc", "qdisc", "del", "dev", iface, "ingress"}, IgnoreError: true},
	}
	return append(cmds, BuildIfbTeardownPlan()...)
}

// BuildIfbTeardownPlan 只拆本项目专用的 ifb 设备，不碰任何真实网卡。
//
// 探测不出默认网卡时用它兜底：至少要把 ifb 拆掉，否则主网卡上的 ingress
// 重定向会一直指着一个配不全的设备。
func BuildIfbTeardownPlan() []Command {
	return []Command{
		{Args: []string{"tc", "qdisc", "del", "dev", IfbDevice, "root"}, IgnoreError: true},
		{Args: []string{"ip", "link", "set", IfbDevice, "down"}, IgnoreError: true},
		{Args: []string{"ip", "link", "del", IfbDevice}, IgnoreError: true},
	}
}
