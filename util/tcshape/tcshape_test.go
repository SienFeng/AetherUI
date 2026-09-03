package tcshape

import (
	"strings"
	"testing"
)

func joined(cmds []Command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.Join(c.Args, " ")
	}
	return out
}

func findCmd(t *testing.T, cmds []Command, substrs ...string) string {
	t.Helper()
	for _, line := range joined(cmds) {
		ok := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				ok = false
				break
			}
		}
		if ok {
			return line
		}
	}
	t.Fatalf("没有找到同时含 %v 的命令，实际命令：\n%s", substrs, strings.Join(joined(cmds), "\n"))
	return ""
}

func countCmd(cmds []Command, substrs ...string) int {
	n := 0
	for _, line := range joined(cmds) {
		ok := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

func onePlan(t *testing.T, limits ...Limit) []Command {
	t.Helper()
	cmds, err := BuildApplyPlan("eth0", limits)
	if err != nil {
		t.Fatalf("BuildApplyPlan: %v", err)
	}
	return cmds
}

// 这条是整个限速功能的安全底线：未被任何 filter 命中的流量必须落到一个
// 不限速的默认类。SSH 走的就是这一类——它要是被限住甚至丢掉，机器就失联了。
func TestPlanSendsUnmatchedTrafficToAnUnlimitedDefaultClass(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, DownMbit: 5, UpMbit: 2})

	for _, dev := range []string{"dev eth0", "dev " + IfbDevice} {
		root := findCmd(t, cmds, "qdisc add", dev, "root", "htb")
		if !strings.Contains(root, "default 9999") {
			t.Errorf("%s 的 root qdisc 没有指定默认类: %s", dev, root)
		}
		def := findCmd(t, cmds, "class add", dev, "classid 1:9999")
		if !strings.Contains(def, "rate 10000mbit") {
			t.Errorf("%s 的默认类不是不限速: %s", dev, def)
		}
	}
}

func TestPlanTearsDownBeforeBuilding(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, DownMbit: 5})

	lines := joined(cmds)
	firstAdd := -1
	lastDel := -1
	for i, line := range lines {
		if strings.Contains(line, "qdisc del") || strings.Contains(line, "link del") {
			lastDel = i
			if !cmds[i].IgnoreError {
				t.Errorf("拆除命令 %q 没有标记为忽略错误——规则不存在时 tc 会报错，"+
					"当成失败会让整次应用中断", line)
			}
		}
		if firstAdd < 0 && (strings.Contains(line, "qdisc add") || strings.Contains(line, "class add")) {
			firstAdd = i
		}
	}
	if lastDel < 0 {
		t.Fatal("计划里没有拆除命令：整树重建才是幂等的")
	}
	if firstAdd < 0 || lastDel > firstAdd {
		t.Errorf("拆除必须全部排在新建之前，lastDel=%d firstAdd=%d", lastDel, firstAdd)
	}
}

func TestPlanDownlinkMatchesSourcePortOnMainInterface(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, DownMbit: 5})

	// 下行 = 服务端发给客户端 = 主网卡的 egress，按**源端口**分类。
	// 匹配成目的端口就限错方向了。
	f := findCmd(t, cmds, "filter add", "dev eth0", "parent 1:", "protocol ip ")
	if !strings.Contains(f, "match ip sport 10011 0xffff") {
		t.Errorf("下行 filter 没有按源端口匹配: %s", f)
	}
	if !strings.Contains(f, "flowid 1:3") {
		t.Errorf("下行 filter 没有指向该入站的类: %s", f)
	}
	c := findCmd(t, cmds, "class add", "dev eth0", "classid 1:3")
	if !strings.Contains(c, "rate 5mbit") || !strings.Contains(c, "ceil 5mbit") {
		t.Errorf("下行限速值不对: %s", c)
	}
}

func TestPlanUplinkMatchesDestPortOnIfb(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, UpMbit: 2})

	// 上行 = 客户端发给服务端 = 主网卡的 ingress。tc 不能直接整形 ingress，
	// 标准做法是把 ingress 重定向到 ifb 虚拟设备，在它上面做 egress。
	redirect := findCmd(t, cmds, "filter add", "dev eth0", "parent ffff:", "mirred")
	if !strings.Contains(redirect, "redirect dev "+IfbDevice) {
		t.Errorf("ingress 没有重定向到 ifb: %s", redirect)
	}
	f := findCmd(t, cmds, "filter add", "dev "+IfbDevice, "protocol ip ")
	if !strings.Contains(f, "match ip dport 10011 0xffff") {
		t.Errorf("上行 filter 没有按目的端口匹配: %s", f)
	}
	c := findCmd(t, cmds, "class add", "dev "+IfbDevice, "classid 1:3")
	if !strings.Contains(c, "rate 2mbit") {
		t.Errorf("上行限速值不对: %s", c)
	}
}

func TestPlanCoversIPv6AsWell(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, DownMbit: 5, UpMbit: 2})

	// u32 的 protocol ip 只匹配 IPv4。只下 v4 的 filter 的话，
	// IPv6 客户端会完全不受限速——限制被静默绕过。
	if n := countCmd(cmds, "filter add", "dev eth0", "protocol ipv6", "match ip6 sport 10011"); n != 1 {
		t.Errorf("主网卡的 IPv6 下行 filter 有 %d 条，期望 1 条", n)
	}
	if n := countCmd(cmds, "filter add", "dev "+IfbDevice, "protocol ipv6", "match ip6 dport 10011"); n != 1 {
		t.Errorf("ifb 的 IPv6 上行 filter 有 %d 条，期望 1 条", n)
	}
}

func TestPlanOmitsDirectionThatIsNotLimited(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, DownMbit: 5}) // 只限下行

	if n := countCmd(cmds, "class add", "dev "+IfbDevice, "classid 1:3"); n != 0 {
		t.Errorf("没配上行限速却在 ifb 上建了类")
	}
	if n := countCmd(cmds, "class add", "dev eth0", "classid 1:3"); n != 1 {
		t.Errorf("下行类有 %d 个，期望 1 个", n)
	}
}

func TestPlanHandlesMultipleInbounds(t *testing.T) {
	cmds := onePlan(t,
		Limit{InboundId: 7, Port: 20002, DownMbit: 30},
		Limit{InboundId: 3, Port: 10011, DownMbit: 5},
	)
	findCmd(t, cmds, "class add", "dev eth0", "classid 1:3", "rate 5mbit")
	findCmd(t, cmds, "class add", "dev eth0", "classid 1:7", "rate 30mbit")
	// 顺序必须确定：同样的配置要生成同样的命令序列，否则没法用哈希判断
	// 「需不需要重新下发」，每轮都会推倒重建。
	lines := joined(cmds)
	i3, i7 := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "classid 1:3") && i3 < 0 {
			i3 = i
		}
		if strings.Contains(l, "classid 1:7") && i7 < 0 {
			i7 = i
		}
	}
	if i3 > i7 {
		t.Error("入站没有按 id 升序生成")
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	limits := []Limit{
		{InboundId: 7, Port: 20002, DownMbit: 30, UpMbit: 10},
		{InboundId: 3, Port: 10011, DownMbit: 5},
	}
	first, err := BuildApplyPlan("eth0", limits)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := BuildApplyPlan("eth0", limits)
		if err != nil {
			t.Fatal(err)
		}
		a, b := strings.Join(joined(first), "\n"), strings.Join(joined(again), "\n")
		if a != b {
			t.Fatalf("第 %d 次结果不同:\n%s\n---\n%s", i, a, b)
		}
	}
}

func TestBuildApplyPlanRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		iface  string
		limits []Limit
	}{
		{"网卡名为空", "", []Limit{{InboundId: 3, Port: 10011, DownMbit: 5}}},
		{"网卡名含空格", "eth 0", []Limit{{InboundId: 3, Port: 10011, DownMbit: 5}}},
		{"网卡名含分号", "eth0;reboot", []Limit{{InboundId: 3, Port: 10011, DownMbit: 5}}},
		{"没有任何限速", "eth0", []Limit{{InboundId: 3, Port: 10011}}},
		{"入站 id 为 0", "eth0", []Limit{{InboundId: 0, Port: 10011, DownMbit: 5}}},
		// 9999 是默认类。撞上它就等于把默认类的速率改成了这个用户的限速，
		// 整机未分类流量（含 SSH）会一起被限死。
		{"入站 id 撞上默认类", "eth0", []Limit{{InboundId: DefaultClassId, Port: 10011, DownMbit: 5}}},
		{"入站 id 超上限", "eth0", []Limit{{InboundId: 70000, Port: 10011, DownMbit: 5}}},
		{"端口越界", "eth0", []Limit{{InboundId: 3, Port: 70000, DownMbit: 5}}},
		{"速率为负", "eth0", []Limit{{InboundId: 3, Port: 10011, DownMbit: -1}}},
		{"速率超上限", "eth0", []Limit{{InboundId: 3, Port: 10011, DownMbit: 999999}}},
		{"入站 id 重复", "eth0", []Limit{
			{InboundId: 3, Port: 10011, DownMbit: 5},
			{InboundId: 3, Port: 10012, DownMbit: 5},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := BuildApplyPlan(c.iface, c.limits); err == nil {
				t.Error("期望报错，实际通过")
			}
		})
	}
}

func TestTeardownPlanIsAllIgnoreErrorAndRemovesIfb(t *testing.T) {
	cmds := BuildTeardownPlan("eth0")
	if len(cmds) == 0 {
		t.Fatal("拆除计划是空的")
	}
	for _, c := range cmds {
		if !c.IgnoreError {
			t.Errorf("拆除命令 %q 没有标记忽略错误", strings.Join(c.Args, " "))
		}
	}
	findCmd(t, cmds, "qdisc del", "dev eth0", "root")
	findCmd(t, cmds, "qdisc del", "dev eth0", "ingress")
	findCmd(t, cmds, "link del", IfbDevice)
}

func TestTeardownPlanRejectsBadInterface(t *testing.T) {
	// 拆除路径同样不能接受奇怪的网卡名：它一样会被拼进 exec 参数。
	if cmds := BuildTeardownPlan("eth0;reboot"); len(cmds) != 0 {
		t.Errorf("非法网卡名应当生成空计划，实际 %v", joined(cmds))
	}
}

func TestParseDefaultRoutePicksLowestMetric(t *testing.T) {
	// /proc/net/route 的目的地址是小端十六进制，00000000 就是默认路由。
	content := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth1	00000000	0100A8C0	0003	0	0	100	00000000	0	0	0
eth0	00000000	0100A8C0	0003	0	0	10	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`
	got, err := parseDefaultRoute(content)
	if err != nil {
		t.Fatalf("parseDefaultRoute: %v", err)
	}
	if got != "eth0" {
		t.Errorf("默认网卡 = %q，期望 metric 更小的 eth0", got)
	}
}

func TestParseDefaultRouteRejectsNoDefault(t *testing.T) {
	content := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`
	if _, err := parseDefaultRoute(content); err == nil {
		t.Error("没有默认路由时必须报错，猜一个网卡名去下限速规则太危险")
	}
}

func TestParseDefaultRouteRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "垃圾", "Iface\tDestination\n"} {
		if _, err := parseDefaultRoute(s); err == nil {
			t.Errorf("内容 %q 应当报错", s)
		}
	}
}

func TestPlanBuildsIfbTreeBeforeRedirectingTrafficToIt(t *testing.T) {
	cmds := onePlan(t, Limit{InboundId: 3, Port: 10011, UpMbit: 2})

	lines := joined(cmds)
	ifbRoot, redirect := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "qdisc add") && strings.Contains(l, "dev "+IfbDevice) && strings.Contains(l, "root") {
			ifbRoot = i
		}
		if strings.Contains(l, "mirred") {
			redirect = i
		}
	}
	if ifbRoot < 0 || redirect < 0 {
		t.Fatalf("命令不全：ifbRoot=%d redirect=%d", ifbRoot, redirect)
	}
	// 先把 ifb 的队列树建好，再把流量重定向过去。反过来的话，中间那一小段
	// 时间里全部入站流量会被送进一个还没配好的设备。
	if ifbRoot > redirect {
		t.Errorf("ifb 的 root qdisc(%d) 建在了重定向(%d)之后", ifbRoot, redirect)
	}
}

func TestIfbTeardownPlanNeverTouchesARealInterface(t *testing.T) {
	cmds := BuildIfbTeardownPlan()
	if len(cmds) == 0 {
		t.Fatal("计划是空的")
	}
	for _, c := range cmds {
		line := strings.Join(c.Args, " ")
		if !c.IgnoreError {
			t.Errorf("拆除命令 %q 没有标记忽略错误", line)
		}
		// 探测不出网卡时只能拆自己建的 ifb。这条计划里绝不能出现
		// 对真实网卡的操作——尤其不能是 lo。
		if strings.Contains(line, "dev lo") || strings.Contains(line, "dev eth") {
			t.Errorf("命令 %q 动到了真实网卡", line)
		}
		if !strings.Contains(line, IfbDevice) {
			t.Errorf("命令 %q 不是针对 ifb 设备的", line)
		}
	}
	findCmd(t, cmds, "link del", IfbDevice)
}

func TestTeardownPlanIncludesIfbCleanup(t *testing.T) {
	full := BuildTeardownPlan("eth0")
	ifbOnly := BuildIfbTeardownPlan()
	// 完整拆除必须包含 ifb 那一段，否则重定向会一直挂着。
	for _, want := range joined(ifbOnly) {
		found := false
		for _, got := range joined(full) {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("完整拆除计划里缺少 %q", want)
		}
	}
}
