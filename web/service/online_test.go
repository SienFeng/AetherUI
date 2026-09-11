package service

import (
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"a-ui/database/model"
	"a-ui/util/ipdb"
	"a-ui/util/netdiag"
)

const testPort = 39001

var testPorts = map[int]bool{testPort: true}

func noLocation(net.IP) ipLocation { return ipLocation{} }

func conn(cookie uint64, port uint16, remote string, up, down uint64) netdiag.Conn {
	return netdiag.Conn{
		Cookie:     cookie,
		LocalIP:    net.ParseIP("10.0.0.5"),
		LocalPort:  port,
		RemoteIP:   net.ParseIP(remote),
		RemotePort: 50000 + uint16(cookie),
		BytesUp:    up,
		BytesDown:  down,
		HasBytes:   true,
	}
}

func onlyEntry(t *testing.T, list []OnlineIP) OnlineIP {
	t.Helper()
	if len(list) != 1 {
		t.Fatalf("在线条目数 = %d，期望 1：%+v", len(list), list)
	}
	return list[0]
}

func TestFirstSampleHasNoSpeed(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 5000, 9000)}, testPorts, now)

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.UpSpeed != 0 || e.DownSpeed != 0 {
		t.Errorf("首次采样没有基准，速率必须是 0，实际 %d/%d", e.UpSpeed, e.DownSpeed)
	}
	if e.IP != "1.2.3.4" {
		t.Errorf("IP = %q，期望 1.2.3.4", e.IP)
	}
	if e.Conns != 1 {
		t.Errorf("连接数 = %d，期望 1", e.Conns)
	}
}

func TestSpeedIsBytesDeltaOverElapsed(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1000, 2000)}, testPorts, now)
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1000+2048, 2000+1024000)}, testPorts, now.Add(2*time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.UpSpeed != 1024 {
		t.Errorf("上行速率 = %d，期望 1024 B/s（2048 字节 / 2 秒）", e.UpSpeed)
	}
	if e.DownSpeed != 512000 {
		t.Errorf("下行速率 = %d，期望 512000 B/s", e.DownSpeed)
	}
	// 累计量只算在线期间的增量，不含首次采样时连接已有的历史字节。
	if e.Up != 2048 || e.Down != 1024000 {
		t.Errorf("累计 = %d/%d，期望 2048/1024000", e.Up, e.Down)
	}
}

func TestNewConnectionCountsAllItsBytes(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 100, 100)}, testPorts, now)
	// cookie 2 是本轮才出现的连接，它的字节全部产生于这 2 秒内。
	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.2.3.4", 100, 100),
		conn(2, testPort, "1.2.3.4", 600, 800),
	}, testPorts, now.Add(2*time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.Conns != 2 {
		t.Errorf("连接数 = %d，期望 2", e.Conns)
	}
	if e.UpSpeed != 300 || e.DownSpeed != 400 {
		t.Errorf("速率 = %d/%d，期望 300/400", e.UpSpeed, e.DownSpeed)
	}
}

func TestDisappearedConnectionDoesNotProduceNegativeSpeed(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.2.3.4", 10000, 90000),
		conn(2, testPort, "1.2.3.4", 500, 600),
	}, testPorts, now)
	// cookie 1 断开了，只剩 cookie 2 且没有新增流量。
	tk.update([]netdiag.Conn{conn(2, testPort, "1.2.3.4", 500, 600)}, testPorts, now.Add(2*time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.UpSpeed != 0 || e.DownSpeed != 0 {
		t.Errorf("速率 = %d/%d，连接消失不应产生非零（更不能是负）速率", e.UpSpeed, e.DownSpeed)
	}
	if e.Conns != 1 {
		t.Errorf("连接数 = %d，期望 1", e.Conns)
	}
}

func TestIPWithNoConnectionsIsRemoved(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 100, 100)}, testPorts, now)
	// 全部断开，不再有任何连接：并发额度必须立刻释放。
	tk.update(nil, testPorts, now.Add(2*time.Second))

	if list := tk.snapshot(testPort, noLocation); len(list) != 0 {
		t.Errorf("断链后仍有 %d 条在线记录：%+v", len(list), list)
	}
}

func TestConnectionsOnUnknownPortAreIgnored(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	// 54321 是面板自己的端口，不属于任何入站，不能被统计进来。
	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.2.3.4", 100, 100),
		conn(2, 54321, "9.9.9.9", 100, 100),
	}, testPorts, now)

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.IP != "1.2.3.4" {
		t.Errorf("IP = %q，期望 1.2.3.4", e.IP)
	}
	if list := tk.snapshot(54321, noLocation); len(list) != 0 {
		t.Errorf("非入站端口不应有记录：%+v", list)
	}
}

func TestFirstSeenIsStableAcrossSamples(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 100, 100)}, testPorts, now)
	first := onlyEntry(t, tk.snapshot(testPort, noLocation)).FirstSeen

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 200, 200)}, testPorts, now.Add(2*time.Second))
	second := onlyEntry(t, tk.snapshot(testPort, noLocation)).FirstSeen

	if first != now.UnixMilli() {
		t.Errorf("FirstSeen = %d，期望 %d", first, now.UnixMilli())
	}
	if second != first {
		t.Errorf("FirstSeen 在第二次采样后变成了 %d，应保持 %d", second, first)
	}
}

func TestFirstSeenRestartsAfterReconnect(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 100, 100)}, testPorts, now)
	tk.update(nil, testPorts, now.Add(2*time.Second))
	tk.update([]netdiag.Conn{conn(9, testPort, "1.2.3.4", 50, 50)}, testPorts, now.Add(4*time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.FirstSeen != now.Add(4*time.Second).UnixMilli() {
		t.Errorf("重连后 FirstSeen = %d，期望本次上线时间 %d", e.FirstSeen, now.Add(4*time.Second).UnixMilli())
	}
	if e.Up != 50 || e.Down != 50 {
		t.Errorf("重连后累计量 = %d/%d，期望只含本次上线的 50/50，不能带上一次的量", e.Up, e.Down)
	}
}

func TestConnectionWithoutByteCountersStillCounts(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	c := conn(1, testPort, "1.2.3.4", 0, 0)
	c.HasBytes = false // 老内核的 tcp_info 里没有这两个计数器
	tk.update([]netdiag.Conn{c}, testPorts, now)
	tk.update([]netdiag.Conn{c}, testPorts, now.Add(2*time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.Conns != 1 {
		t.Errorf("连接数 = %d，拿不到字节数不代表连接不存在", e.Conns)
	}
	if e.UpSpeed != 0 || e.DownSpeed != 0 {
		t.Errorf("速率 = %d/%d，字节数不可用时不能编造速率", e.UpSpeed, e.DownSpeed)
	}
}

func TestIPv4MappedAddressIsNormalized(t *testing.T) {
	tk := newOnlineTracker()
	c := conn(1, testPort, "::ffff:203.0.113.9", 100, 100)
	tk.update([]netdiag.Conn{c}, testPorts, time.Unix(1000, 0))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.IP != "203.0.113.9" {
		t.Errorf("IP = %q，v4-mapped 地址应归一成点分十进制，否则归属地查不到", e.IP)
	}
}

func TestStaleGapDoesNotReportSpeed(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 0, 0)}, testPorts, now)
	// 中间停采了很久（页面关掉后采样器休眠），这段时间的平均值不是"实时网速"。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1<<30, 1<<30)}, testPorts, now.Add(10*time.Minute))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.UpSpeed != 0 || e.DownSpeed != 0 {
		t.Errorf("速率 = %d/%d，采样间隔过长时不应把长周期均值当成实时网速", e.UpSpeed, e.DownSpeed)
	}
}

func TestSnapshotFillsLocation(t *testing.T) {
	tk := newOnlineTracker()
	tk.update([]netdiag.Conn{conn(1, testPort, "114.114.114.114", 100, 100)}, testPorts, time.Unix(1000, 0))

	locate := func(ip net.IP) ipLocation {
		if ip.Equal(net.ParseIP("114.114.114.114")) {
			return ipLocation{Location: "中国 江苏省 南京市", ISP: "中国电信"}
		}
		return ipLocation{}
	}
	e := onlyEntry(t, tk.snapshot(testPort, locate))
	if e.Location != "中国 江苏省 南京市" {
		t.Errorf("归属地 = %q，期望 中国 江苏省 南京市", e.Location)
	}
	if e.ISP != "中国电信" {
		t.Errorf("运营商 = %q，期望 中国电信", e.ISP)
	}
}

func TestSnapshotIsSortedByIP(t *testing.T) {
	tk := newOnlineTracker()
	tk.update([]netdiag.Conn{
		conn(1, testPort, "9.9.9.9", 100, 100),
		conn(2, testPort, "1.1.1.1", 100, 100),
		conn(3, testPort, "5.5.5.5", 100, 100),
	}, testPorts, time.Unix(1000, 0))

	list := tk.snapshot(testPort, noLocation)
	if len(list) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(list))
	}
	// 顺序不稳定的话，页面每 2 秒刷新一次会不停跳行。
	if list[0].IP != "1.1.1.1" || list[1].IP != "5.5.5.5" || list[2].IP != "9.9.9.9" {
		t.Errorf("顺序 = %v/%v/%v，期望按 IP 升序", list[0].IP, list[1].IP, list[2].IP)
	}
}

func TestObservableTransports(t *testing.T) {
	cases := []struct {
		name       string
		stream     string
		observable bool
	}{
		{"tcp", `{"network":"tcp"}`, true},
		{"ws", `{"network":"ws"}`, true},
		{"grpc", `{"network":"grpc"}`, true},
		{"h2", `{"network":"h2"}`, true},
		{"http", `{"network":"http"}`, true},
		// mKCP / QUIC 是 UDP：xray 只在端口上开一个 socket 自己复用，
		// 内核连接表里看不到每个客户端，必须如实告诉管理员而不是显示"无人在线"。
		{"mkcp", `{"network":"kcp"}`, false},
		{"quic", `{"network":"quic"}`, false},
		{"domainsocket", `{"network":"domainsocket"}`, false},
		// 缺省即 tcp
		{"空配置", `{}`, true},
		{"空字符串", ``, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := transportObservable(tc.stream)
			if ok != tc.observable {
				t.Errorf("observable = %v，期望 %v", ok, tc.observable)
			}
			if !ok && reason == "" {
				t.Error("不可观测时必须给出原因，否则界面只能显示空白")
			}
		})
	}
}

func TestFormatLocationSkipsEmptyParts(t *testing.T) {
	cases := []struct {
		country, region, city string
		want                  string
	}{
		{"中国", "江苏省", "南京市", "中国 江苏省 南京市"},
		{"United States", "", "", "United States"},
		{"中国", "江苏省", "", "中国 江苏省"},
		{"", "", "", ""},
	}
	for _, tc := range cases {
		got := formatLocation(ipdb.Location{Country: tc.country, Region: tc.region, City: tc.city})
		if got != tc.want {
			t.Errorf("formatLocation(%q,%q,%q) = %q，期望 %q", tc.country, tc.region, tc.city, got, tc.want)
		}
	}
}

func TestReusedCookieDoesNotProduceNegativeDelta(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 10000, 20000)}, testPorts, now)
	// 内核可能把同一个 cookie 分配给新建的 socket，计数器随之回到很小的值。
	// 直接相减会得到负数，折算成速率就是个巨大的负值。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 500, 600)}, testPorts, now.Add(2*time.Second))

	e := onlyEntry(t, tk.snapshot(testPort, noLocation))
	if e.UpSpeed < 0 || e.DownSpeed < 0 {
		t.Fatalf("速率 = %d/%d，计数器回退不能算出负速率", e.UpSpeed, e.DownSpeed)
	}
	if e.UpSpeed != 250 || e.DownSpeed != 300 {
		t.Errorf("速率 = %d/%d，期望按全量重新计 250/300", e.UpSpeed, e.DownSpeed)
	}
}

func TestSnapshotShowsDisagreementBetweenSources(t *testing.T) {
	tk := newOnlineTracker()
	tk.update([]netdiag.Conn{conn(1, testPort, "114.114.114.114", 100, 100)}, testPorts, time.Unix(1000, 0))

	locate := func(ip net.IP) ipLocation {
		return ipLocation{
			Location:    "中国 江苏省 南京市",
			LocationAlt: "中国 山东省 济南市",
			ISP:         "中国电信",
			ISPAlt:      "中国联通",
		}
	}
	e := onlyEntry(t, tk.snapshot(testPort, locate))
	if e.Location != "中国 江苏省 南京市" {
		t.Errorf("主判定 = %q", e.Location)
	}
	// 两个离线库判定不一致时必须显示出来：这正是引入第二个数据源的意义，
	// 只显示其中一个等于把分歧藏起来了。
	if e.LocationAlt != "中国 山东省 济南市" {
		t.Errorf("次判定 = %q，期望显示另一个源的不同结论", e.LocationAlt)
	}
}

func TestSnapshotHidesAltWhenSourcesAgree(t *testing.T) {
	tk := newOnlineTracker()
	tk.update([]netdiag.Conn{conn(1, testPort, "114.114.114.114", 100, 100)}, testPorts, time.Unix(1000, 0))

	locate := func(ip net.IP) ipLocation {
		return ipLocation{Location: "中国 江苏省 南京市", ISP: "中国电信"}
	}
	e := onlyEntry(t, tk.snapshot(testPort, locate))
	if e.LocationAlt != "" {
		t.Errorf("次判定 = %q，两源一致时不该显示", e.LocationAlt)
	}
	if e.ISPAlt != "" {
		t.Errorf("运营商次判定 = %q，两源一致时不该显示", e.ISPAlt)
	}
}

// 闲置连接必须停止占用并发额度。
//
// TCP 连接不会因为没有流量就消失：客户端进程只要不退出，连接一直是
// ESTABLISHED，内核的 keepalive 只负责探测对端死没死，探测成功连接就继续
// 保持。因此「断链才释放额度」的原语义下，一个挂着不用的客户端会**永久**
// 占着名额，管理员看到实时网速 0 B/s 却怎么等都不释放。
func TestSnapshotMarksIdleAfterTimeout(t *testing.T) {
	tr := newOnlineTracker()
	base := time.Now()
	key := onlineKey{port: 1000, ip: "1.1.1.1"}
	tr.ips[key] = &onlineEntry{
		ip:           net.ParseIP("1.1.1.1"),
		firstSeen:    base,
		lastActiveAt: base,
		conns:        3,
	}

	// 还没到阈值
	list := tr.snapshotAt(1000, noLocate, 120*time.Second, base.Add(119*time.Second))
	if len(list) != 1 || list[0].Idle {
		t.Fatalf("119s 时不应判为闲置: %+v", list)
	}

	// 超过阈值
	list = tr.snapshotAt(1000, noLocate, 120*time.Second, base.Add(121*time.Second))
	if len(list) != 1 || !list[0].Idle {
		t.Fatalf("121s 时应判为闲置: %+v", list)
	}

	// 闲置的条目仍然要显示出来，而且 firstSeen 不能丢——丢了的话这人一恢复
	// 活跃就变成「最新来的」，在「保留最早 N 个」的判定里反而最先被踢。
	if list[0].FirstSeen != base.UnixMilli() {
		t.Errorf("firstSeen = %d, want %d（闲置不得重置首次观测时间）", list[0].FirstSeen, base.UnixMilli())
	}
	if list[0].Conns != 3 {
		t.Errorf("conns = %d, want 3（连接仍在，只是没有流量）", list[0].Conns)
	}
}

// idleAfter <= 0 表示关闭闲置判定，保持原有行为。
func TestSnapshotIdleDisabledWhenTimeoutNotPositive(t *testing.T) {
	tr := newOnlineTracker()
	base := time.Now()
	tr.ips[onlineKey{port: 1000, ip: "1.1.1.1"}] = &onlineEntry{
		ip: net.ParseIP("1.1.1.1"), firstSeen: base, lastActiveAt: base,
	}
	list := tr.snapshotAt(1000, noLocate, 0, base.Add(24*time.Hour))
	if len(list) != 1 || list[0].Idle {
		t.Fatalf("关闭闲置判定时不应标记 idle: %+v", list)
	}
}

// 闲置的 IP 不占额度：额度 1、两个来源，其中先来的那个已闲置时，
// 后来的活跃来源不该被拒。
func TestPlanRejectionsIgnoresIdle(t *testing.T) {
	base := time.Now().UnixMilli()
	list := []OnlineIP{
		{IP: "1.1.1.1", FirstSeen: base, Conns: 2, Idle: true},
		{IP: "2.2.2.2", FirstSeen: base + 1000, Conns: 1},
	}
	over := planRejections(list, 1)
	if len(over) != 0 {
		t.Errorf("被拒集合 = %v, want 空（闲置的 1.1.1.1 不该占用额度）", over)
	}
}

// 闲置的来源自己也不该被「拒绝」——它没有占额度，断它的连接毫无意义，
// 只会打断一个只是暂时没有流量的正常用户。
func TestPlanRejectionsNeverRejectsIdleItself(t *testing.T) {
	base := time.Now().UnixMilli()
	list := []OnlineIP{
		{IP: "1.1.1.1", FirstSeen: base, Conns: 1},
		{IP: "2.2.2.2", FirstSeen: base + 1000, Conns: 1},
		{IP: "3.3.3.3", FirstSeen: base + 2000, Conns: 5, Idle: true},
	}
	over := planRejections(list, 1)
	for _, ip := range over {
		if ip == "3.3.3.3" {
			t.Errorf("闲置来源 3.3.3.3 被拒了: %v", over)
		}
	}
	if len(over) != 1 || over[0] != "2.2.2.2" {
		t.Errorf("被拒集合 = %v, want [2.2.2.2]", over)
	}
}

// ---- 在线设备数（入站列表页那一列）----

// 在线设备数按「还有活连接的来源 IP」计数：一个 IP 一台设备，同一台设备
// 开多条连接不能算成多台。
func TestCountLiveCountsSourceIPsNotConnections(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.1.1.1", 100, 100),
		conn(2, testPort, "1.1.1.1", 100, 100),
		conn(3, testPort, "2.2.2.2", 100, 100),
	}, testPorts, now)

	if n := countLive(tk.snapshot(testPort, noLocation)); n != 2 {
		t.Errorf("在线设备数 = %d，期望 2（两个来源 IP，其中一个开了两条连接）", n)
	}

	tk.update(nil, testPorts, now.Add(2*time.Second))
	if n := countLive(tk.snapshot(testPort, noLocation)); n != 0 {
		t.Errorf("全部断开后在线设备数 = %d，期望 0", n)
	}
}

// 超额被拒 / 被封禁之后连接已断干净的条目只为界面展示而保留，它们不是
// 在线设备——算进去的话「已封禁」的人会永远显示为在线。
func TestCountLiveSkipsEntriesWithoutConnections(t *testing.T) {
	list := []OnlineIP{
		{IP: "1.1.1.1", Conns: 2},
		{IP: "2.2.2.2", Conns: 0, Blocked: true, RejectedAt: 1},
		{IP: "3.3.3.3", Conns: 0, Banned: true},
	}
	if n := countLive(list); n != 1 {
		t.Errorf("在线设备数 = %d，期望 1：连接已断干净的历史条目不是在线设备", n)
	}
}

// 闲置设备的连接还在，人还挂在线上，必须计入。
//
// 这与 liveOnly（并发额度口径）刻意不同：那里回答的是「谁占着名额」，
// 这里回答的是「有几台设备连着」。两个口径必须同时存在且互不替代。
func TestCountLiveIncludesIdleDevices(t *testing.T) {
	list := []OnlineIP{{IP: "1.1.1.1", Conns: 3, Idle: true}}

	if n := countLive(list); n != 1 {
		t.Errorf("在线设备数 = %d，期望 1：闲置只是暂时没有流量，连接还在", n)
	}
	if n := len(liveOnly(list)); n != 0 {
		t.Errorf("并发额度口径 = %d，期望 0；两个口径若一致，本用例就失去意义了", n)
	}
}

// 数不出来必须与「没人在线」区分开：前者要给出原因让界面显示 —，
// 后者才是 0。
func TestCountabilityDistinguishesUnknownFromZero(t *testing.T) {
	tcp := &model.Inbound{StreamSettings: `{"network":"tcp"}`}
	kcp := &model.Inbound{StreamSettings: `{"network":"kcp"}`}

	// platformSupported 显式传入而不是直接引用 netdiag.Supported：后者是
	// 编译期常量，在非 Linux 开发机上恒为 false，直接引用会让用例结果
	// 随开发机而变。
	if ok, reason := countabilityOf(tcp, false); ok || reason == "" {
		t.Errorf("非 Linux 平台 ok = %v reason = %q，必须判为数不出来并给出原因", ok, reason)
	}
	if ok, reason := countabilityOf(kcp, true); ok || reason == "" {
		t.Errorf("mKCP 入站 ok = %v reason = %q，UDP 在内核连接表里看不到单个客户端", ok, reason)
	}
	if ok, reason := countabilityOf(tcp, true); !ok || reason != "" {
		t.Errorf("tcp 入站 ok = %v reason = %q，期望可观测", ok, reason)
	}
}

// 入站列表那一列要的是「这个节点下所有设备合计跑多快」，也就是展开行里
// 每台设备那两个数字之和。这条从真实的两次采样走完整条链路，钉住的正是
// 这个等式——只对着构造出来的 []OnlineIP 求和，测不到 update 那一步。
func TestSumLiveSpeedEqualsSumOfPerDeviceSpeeds(t *testing.T) {
	tk := newOnlineTracker()
	now := time.Unix(1000, 0)

	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.1.1.1", 0, 0),
		conn(2, testPort, "2.2.2.2", 0, 0),
	}, testPorts, now)

	// 2 秒后：1.1.1.1 上行 2000 下行 4000，2.2.2.2 上行 1000 下行 6000。
	later := now.Add(2 * time.Second)
	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.1.1.1", 2000, 4000),
		conn(2, testPort, "2.2.2.2", 1000, 6000),
	}, testPorts, later)

	list := tk.snapshot(testPort, noLocation)
	if len(list) != 2 {
		t.Fatalf("期望 2 个来源，实际 %d", len(list))
	}

	var wantUp, wantDown int64
	for _, e := range list {
		wantUp += e.UpSpeed
		wantDown += e.DownSpeed
	}
	up, down := sumLiveSpeed(list)
	if up != wantUp || down != wantDown {
		t.Fatalf("合计速率 = %d/%d，期望 %d/%d（各设备之和）", up, down, wantUp, wantDown)
	}
	// 2 秒窗口：(2000+1000)/2 = 1500，(4000+6000)/2 = 5000。
	if up != 1500 || down != 5000 {
		t.Errorf("合计速率 = %d/%d B/s，期望 1500/5000", up, down)
	}
}

// 与 countLive 同一条口径：连接已断干净、仅为界面展示保留的历史条目不计入。
//
// 它们的速率字段本来就是零值，算不算结果都一样——这条测试钉的是「两列出自
// 同一条判据」，将来谁给历史条目补上残留速率时，这里会先红。
func TestSumLiveSpeedSkipsEntriesWithoutConnections(t *testing.T) {
	list := []OnlineIP{
		{IP: "1.1.1.1", Conns: 2, UpSpeed: 100, DownSpeed: 200},
		{IP: "2.2.2.2", Conns: 0, Blocked: true, UpSpeed: 999, DownSpeed: 999},
		{IP: "3.3.3.3", Conns: 0, Banned: true, UpSpeed: 999, DownSpeed: 999},
	}
	up, down := sumLiveSpeed(list)
	if up != 100 || down != 200 {
		t.Errorf("合计速率 = %d/%d，期望 100/200：连接已断干净的历史条目不该计入", up, down)
	}
	if n := countLive(list); n != 1 {
		t.Errorf("在线设备数 = %d，期望 1；两列口径若不一致，界面会出现「0 台设备」配非零速率", n)
	}
}

// 首次采样没有基准，速率一律为 0——此时合计也必须是 0，不能出现
// 「刚打开页面就显示一个凭空来的速率」。
func TestSumLiveSpeedIsZeroOnFirstSample(t *testing.T) {
	tk := newOnlineTracker()
	tk.update([]netdiag.Conn{
		conn(1, testPort, "1.1.1.1", 5_000_000, 9_000_000),
	}, testPorts, time.Unix(1000, 0))

	up, down := sumLiveSpeed(tk.snapshot(testPort, noLocation))
	if up != 0 || down != 0 {
		t.Errorf("首次采样合计速率 = %d/%d，期望 0/0", up, down)
	}
}

// 运营商与归属地走同一次判定：两个源不一致时把分歧带出来，不做仲裁。
func TestLocateWithIPDBReportsISPDisagreement(t *testing.T) {
	svc := ipdbServiceWithSources(t,
		buildISPTestDB(t, "中国电信"),
		buildISPTestDB(t, "中国联通"))

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if got.ISP != "中国电信" {
		t.Errorf("ISP = %q, want 中国电信", got.ISP)
	}
	if got.ISPAlt != "中国联通" {
		t.Errorf("ISPAlt = %q, want 中国联通（分歧必须显示出来，不做仲裁）", got.ISPAlt)
	}
}

// 两个源一致时不该报分歧，否则「存疑」标签会对几乎每个 IP 亮起。
func TestLocateWithIPDBReportsNoDisagreementWhenSourcesAgree(t *testing.T) {
	svc := ipdbServiceWithSources(t,
		buildISPTestDB(t, "中国电信"),
		buildISPTestDB(t, "中国电信"))

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if got.ISPAlt != "" {
		t.Errorf("ISPAlt = %q, 两个源一致时必须为空", got.ISPAlt)
	}
}

// 归属地与运营商各自独立判定分歧：两个源完全可能在城市名上有出入而运营商
// 一致（真实数据就是如此——123.171.5.200 在 ip2region 是「聊城市」、
// 纯真库是「聊城」，而两边的运营商都是电信）。
func TestLocateWithIPDBJudgesLocationAndISPIndependently(t *testing.T) {
	svc := ipdbServiceWithSources(t,
		buildISPTestDBWithCity(t, "聊城市", "中国电信"),
		buildISPTestDBWithCity(t, "聊城", "中国电信"))

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if got.LocationAlt == "" {
		t.Error("LocationAlt 为空，城市名不同应当报分歧")
	}
	if got.ISPAlt != "" {
		t.Errorf("ISPAlt = %q, 运营商相同时不该报分歧", got.ISPAlt)
	}
}

// buildISPTestDB 造一份只含一段的库：1.0.0.0-1.0.255.255，中国/江苏省/南京市，
// 运营商由调用方指定。
func buildISPTestDB(t *testing.T, isp string) *ipdb.DB {
	t.Helper()
	return buildISPTestDBWithCity(t, "南京市", isp)
}

func buildISPTestDBWithCity(t *testing.T, city, isp string) *ipdb.DB {
	t.Helper()
	var buf bytes.Buffer
	recs := []ipdb.Record{{
		Start: 0x01000000, End: 0x0100FFFF,
		Country: "中国", Region: "江苏省", City: city, ISP: isp,
	}}
	if err := ipdb.BuildRecords(recs, &buf, time.Unix(1788000000, 0)); err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}
	db, err := ipdb.Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return db
}

// ipdbServiceWithSources 把全局的数据源列表换成两个测试源，并塞入给定的两份库。
// 顺序即 Multi.Lookup 的返回顺序，也就决定了谁是主判定、谁是 alt。
func ipdbServiceWithSources(t *testing.T, primary, alt *ipdb.DB) IPDBService {
	t.Helper()
	dir := t.TempDir()
	sources := []ipdbSource{
		testSource(filepath.Join(dir, "primary.dat"), 1),
		testSource(filepath.Join(dir, "alt.dat"), 1),
	}
	sources[0].Key = "primary"
	sources[1].Key = "alt"
	useTestSources(t, sources)

	s := IPDBService{}
	s.setDB("primary", primary)
	s.setDB("alt", alt)
	return s
}

// namedDB 给测试用的数据源指定各自的显示名。ipdbServiceWithSources 把两个源
// 的 Name 都留成 "测试源"，分辨不出谁是谁。
type namedDB struct {
	name string
	db   *ipdb.DB
}

func ipdbServiceNamed(t *testing.T, a, b namedDB) IPDBService {
	t.Helper()
	dir := t.TempDir()
	sources := []ipdbSource{
		testSource(filepath.Join(dir, "a.dat"), 1),
		testSource(filepath.Join(dir, "b.dat"), 1),
	}
	sources[0].Key, sources[0].Name = "a", a.name
	sources[1].Key, sources[1].Name = "b", b.name
	useTestSources(t, sources)

	s := IPDBService{}
	s.setDB("a", a.db)
	s.setDB("b", b.db)
	return s
}

// 分歧必须能追到是哪个源说的，否则管理员没法判断该信谁。
//
// 生产上真实发生过：ip2region 把一个湖北的 IP 判成北京，而界面按 Sources()
// 顺序把 ip2region 的结论当作唯一答案显示，看起来像地区限制漏放了一个北京
// 的来源——实际上纯真库判对了，Multi.CIDRsOfProvinces 的并集也正确放行。
// 面板已经掌握「两个源分别怎么说」这个信息，却没有交给管理员。
func TestLocateWithIPDBCarriesSourceNames(t *testing.T) {
	svc := ipdbServiceNamed(t,
		namedDB{"ip2region", buildISPTestDBWithCity(t, "北京市", "中国移动")},
		namedDB{"纯真 IP 库", buildISPTestDBWithCity(t, "武汉市", "中国移动")})

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if len(got.Sources) != 2 {
		t.Fatalf("Sources = %+v，期望两个源各一条", got.Sources)
	}
	if got.Sources[0].Source != "ip2region" || !strings.Contains(got.Sources[0].Location, "北京市") {
		t.Errorf("Sources[0] = %+v，期望 ip2region 说北京市", got.Sources[0])
	}
	if got.Sources[1].Source != "纯真 IP 库" || !strings.Contains(got.Sources[1].Location, "武汉市") {
		t.Errorf("Sources[1] = %+v，期望纯真 IP 库说武汉市", got.Sources[1])
	}
	// 顺序必须与 Sources() 一致：主判定取的是第一个非空的，两者对不上的话
	// tooltip 里会把结论安到错误的源头上。
	if got.Location != got.Sources[0].Location {
		t.Errorf("主判定 %q 与 Sources[0] %q 不一致", got.Location, got.Sources[0].Location)
	}
}

// 只有一个源加载成功时，Sources 只有一条，界面不该显示成「两源不一致」。
func TestLocateWithIPDBSingleSourceHasNoDisagreement(t *testing.T) {
	dir := t.TempDir()
	sources := []ipdbSource{testSource(filepath.Join(dir, "only.dat"), 1)}
	sources[0].Key, sources[0].Name = "only", "ip2region"
	useTestSources(t, sources)
	svc := IPDBService{}
	svc.setDB("only", buildISPTestDBWithCity(t, "北京市", "中国移动"))

	got := locateWithIPDB(svc, net.ParseIP("1.0.0.1"))
	if len(got.Sources) != 1 {
		t.Fatalf("Sources = %+v，期望只有一条", got.Sources)
	}
	if got.LocationAlt != "" {
		t.Errorf("LocationAlt = %q，单源不该产生分歧", got.LocationAlt)
	}
}

// 低于门槛的流量不得刷新活跃时间。
//
// 这是「限额=1」能用的前提：不设门槛的话，一个挂在后台没关的客户端靠心跳
// 保活包（几十到几百字节/秒）就能永久保持活跃、一直占着并发额度，机主的
// 另一台设备永远连不上。生产实测见 minActiveRate 的注释。
func TestSubThresholdTrafficDoesNotRefreshActivity(t *testing.T) {
	tk := newOnlineTracker()
	base := time.Unix(1000, 0)
	key := onlineKey{port: testPort, ip: "1.2.3.4"}

	// 第一轮建立基准。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1000, 2000)}, testPorts, base)
	first := tk.ips[key].lastActiveAt

	// 1 秒后只涨了 200 字节（200 B/s，典型心跳量级）。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1100, 2100)}, testPorts, base.Add(time.Second))

	if got := tk.ips[key].lastActiveAt; !got.Equal(first) {
		t.Errorf("lastActiveAt 被刷新了，期望保持不变——200 B/s 属心跳量级，不该算在用")
	}
}

// 超过门槛的流量必须刷新活跃时间。
func TestAboveThresholdTrafficRefreshesActivity(t *testing.T) {
	tk := newOnlineTracker()
	base := time.Unix(1000, 0)
	key := onlineKey{port: testPort, ip: "1.2.3.4"}

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1000, 2000)}, testPorts, base)
	first := tk.ips[key].lastActiveAt

	// 1 秒后涨了 4 KB（4 KB/s），远高于门槛。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 1000+2048, 2000+2048)}, testPorts, base.Add(time.Second))

	if got := tk.ips[key].lastActiveAt; got.Equal(first) {
		t.Error("lastActiveAt 未被刷新，期望刷新——4 KB/s 是实打实的使用")
	}
}

// 门槛必须按**速率**判，不能按单轮的绝对字节数。
//
// 并发判定每秒跑一次，但页面轮询也会触发采样（onlineMinSampleInterval 是
// 500ms），所以采样间隔并不恒定。按绝对字节判的话，同一份流量在不同间隔下
// 会得出相反的活跃判定，而这个差异完全取决于当时有没有人开着面板页面。
func TestActivityThresholdIsRateNotAbsoluteBytes(t *testing.T) {
	base := time.Unix(1000, 0)
	key := onlineKey{port: testPort, ip: "1.2.3.4"}
	const chunk = 1500 // 1.5 KB

	// 间隔 1 秒 → 1.5 KB/s，过门槛。
	fast := newOnlineTracker()
	fast.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 0, 0)}, testPorts, base)
	fastFirst := fast.ips[key].lastActiveAt
	fast.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", chunk, 0)}, testPorts, base.Add(time.Second))
	if fast.ips[key].lastActiveAt.Equal(fastFirst) {
		t.Error("1 秒内传 1.5 KB（= 1.5 KB/s）应当算活跃")
	}

	// 同样 1.5 KB，间隔 5 秒 → 300 B/s，不过门槛。
	slow := newOnlineTracker()
	slow.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 0, 0)}, testPorts, base)
	slowFirst := slow.ips[key].lastActiveAt
	slow.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", chunk, 0)}, testPorts, base.Add(5*time.Second))
	if !slow.ips[key].lastActiveAt.Equal(slowFirst) {
		t.Error("5 秒内传 1.5 KB（= 300 B/s）不应算活跃")
	}
}

// 上下行分别低于门槛、合计超过门槛时算活跃：额度判的是这个来源在不在用，
// 不是某个方向在不在用。
func TestActivityThresholdUsesCombinedRate(t *testing.T) {
	tk := newOnlineTracker()
	base := time.Unix(1000, 0)
	key := onlineKey{port: testPort, ip: "1.2.3.4"}

	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 0, 0)}, testPorts, base)
	first := tk.ips[key].lastActiveAt

	// 上下行各 700 B/s，单看都不过门槛，合计 1400 B/s 过门槛。
	tk.update([]netdiag.Conn{conn(1, testPort, "1.2.3.4", 700, 700)}, testPorts, base.Add(time.Second))

	if got := tk.ips[key].lastActiveAt; got.Equal(first) {
		t.Error("上下行合计 1400 B/s 应当算活跃——门槛判的是合计速率")
	}
}
