package service

import (
	"net"
	"testing"
	"time"

	"a-ui/util/ipdb"
	"a-ui/util/netdiag"
)

const testPort = 39001

var testPorts = map[int]bool{testPort: true}

func noLocation(net.IP) (string, string) { return "", "" }

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

	locate := func(ip net.IP) (string, string) {
		if ip.Equal(net.ParseIP("114.114.114.114")) {
			return "中国 江苏省 南京市", ""
		}
		return "", ""
	}
	e := onlyEntry(t, tk.snapshot(testPort, locate))
	if e.Location != "中国 江苏省 南京市" {
		t.Errorf("归属地 = %q，期望 中国 江苏省 南京市", e.Location)
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

	locate := func(ip net.IP) (string, string) {
		return "中国 江苏省 南京市", "中国 山东省 济南市"
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

	locate := func(ip net.IP) (string, string) { return "中国 江苏省 南京市", "" }
	e := onlyEntry(t, tk.snapshot(testPort, locate))
	if e.LocationAlt != "" {
		t.Errorf("次判定 = %q，两源一致时不该显示", e.LocationAlt)
	}
}
