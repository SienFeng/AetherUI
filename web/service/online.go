package service

import (
	"bytes"
	"encoding/json"
	"net"
	"sort"
	"sync"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/ipdb"
	"a-ui/util/netdiag"
)

const (
	// 两次采样之间的最小间隔。页面每 2 秒轮询一次、并发判定每 1 秒跑一次，
	// 取半秒是为了让两者都不会被这道闸门系统性地挡掉，同时多个管理员
	// 同时开着页面也不会把内核连接表打穿。
	onlineMinSampleInterval = 500 * time.Millisecond

	// 采样间隔超过这个值就只重建基准、不出速率：页面关掉一段时间后再打开，
	// 那段时间的平均值不是"实时网速"。
	onlineMaxSampleGap = 10 * time.Second

	// 超额被拒的 IP 在连接被断开后仍然在展开行里保留这么久。
	// 不留的话"有人正在被拒绝"这件事对管理员完全不可见——被拒的 IP
	// 恰恰是连接表里没有的那个。
	onlineRejectRetention = 5 * time.Minute
)

// OnlineIP 是某个入站上一个来源 IP 的在线明细，对应展开行里的一行。
type OnlineIP struct {
	IP       string `json:"ip"`
	Location string `json:"location"`
	// LocationAlt 是另一个数据源给出的、与主判定**不同**的结论。
	// 两个源一致时为空。多源的价值就在于把分歧显示出来，藏起来等于白加。
	LocationAlt string `json:"locationAlt"`
	Conns       int    `json:"conns"`
	FirstSeen   int64  `json:"firstSeen"` // 毫秒；面板首次观测到该 IP 的时间
	UpSpeed     int64  `json:"upSpeed"`   // B/s
	DownSpeed   int64  `json:"downSpeed"`
	Up          int64  `json:"up"` // 本次在线期间的累计字节
	Down        int64  `json:"down"`

	// Idle 为 true 表示该 IP 的连接还在，但已经连续 idleAfter 没有任何字节
	// 往来。闲置来源不占用并发额度：TCP 连接不会因为没有流量就消失，客户端
	// 只要不退出就一直是 ESTABLISHED，不判闲置的话一个挂着不用的客户端会
	// 永久占着名额。
	Idle bool `json:"idle"`
	// Blocked 为 true 表示该 IP 当前超出并发额度、正被拒绝。
	Blocked bool `json:"blocked"`
	// RejectedAt 是最近一次被判超额的时间（毫秒），0 表示从未被拒。
	RejectedAt int64 `json:"rejectedAt"`
}

// OnlineResult 是接口返回体。supported 为 false 时 list 一定为空，
// 界面必须显示 reason，不能把"看不到"渲染成"没人在线"。
type OnlineResult struct {
	Supported bool       `json:"supported"`
	Reason    string     `json:"reason"`
	List      []OnlineIP `json:"list"`
}

type onlineKey struct {
	port int
	ip   string
}

type connBytes struct {
	up   uint64
	down uint64
}

type onlineEntry struct {
	ip        net.IP
	firstSeen time.Time
	// lastActiveAt 是最近一次观测到字节增长的时间，闲置判定以它为准。
	// 与 firstSeen 分开维护：闲置不重置 firstSeen，否则这人一恢复活跃就
	// 变成「最新来的」，在「保留最早 N 个」的判定里反而最先被踢。
	lastActiveAt time.Time
	conns        int
	up           int64
	down         int64
	upSpeed      int64
	downSpeed    int64
}

// onlineTracker 把两次内核连接表快照的差值折算成每个来源 IP 的实时网速。
// 它是纯内存状态，不落库：在线信息是瞬时的，重启面板后重新观测即可。
type onlineTracker struct {
	mu         sync.Mutex
	lastSample time.Time
	prev       map[uint64]connBytes
	ips        map[onlineKey]*onlineEntry

	// rejectedNow 是"本轮判定为超额"的集合，每轮整体替换：额度腾出来之后
	// 标记必须立刻消失，不能靠过期慢慢褪掉。
	rejectedNow map[onlineKey]bool
	// rejectedAt 是最近一次被判超额的时间，按 onlineRejectRetention 过期，
	// 用于在连接已被断开后仍然把这个 IP 显示出来。
	rejectedAt map[onlineKey]time.Time
}

func newOnlineTracker() *onlineTracker {
	return &onlineTracker{
		prev:        map[uint64]connBytes{},
		ips:         map[onlineKey]*onlineEntry{},
		rejectedNow: map[onlineKey]bool{},
		rejectedAt:  map[onlineKey]time.Time{},
	}
}

// setRejected 记录某入站本轮被判超额的 IP 集合。
func (t *onlineTracker) setRejected(port int, ips []string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for key := range t.rejectedNow {
		if key.port == port {
			delete(t.rejectedNow, key)
		}
	}
	for _, ip := range ips {
		key := onlineKey{port: port, ip: ip}
		t.rejectedNow[key] = true
		t.rejectedAt[key] = now
	}
	t.purgeRejectedLocked(now)
}

func (t *onlineTracker) purgeRejectedLocked(now time.Time) {
	for key, at := range t.rejectedAt {
		if now.Sub(at) > onlineRejectRetention {
			delete(t.rejectedAt, key)
		}
	}
}

// normalizeIP 把 v4-mapped 的 v6 地址（::ffff:1.2.3.4）折回四字节形式。
// 双栈监听时内核给的就是这种地址，不归一的话既显示得难看，归属地也查不到。
func normalizeIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func (t *onlineTracker) update(conns []netdiag.Conn, ports map[int]bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	elapsed := now.Sub(t.lastSample)
	// 没有上一轮基准、或者间隔过长时，这一轮只重建基准。
	hasBaseline := !t.lastSample.IsZero() && elapsed > 0 && elapsed <= onlineMaxSampleGap

	type aggregate struct {
		ip    net.IP
		conns int
		up    int64
		down  int64
	}
	aggs := map[onlineKey]*aggregate{}
	cur := make(map[uint64]connBytes, len(conns))

	for _, c := range conns {
		port := int(c.LocalPort)
		if !ports[port] {
			continue
		}
		ip := normalizeIP(c.RemoteIP)
		if len(ip) == 0 {
			continue
		}
		key := onlineKey{port: port, ip: ip.String()}
		a := aggs[key]
		if a == nil {
			a = &aggregate{ip: ip}
			aggs[key] = a
		}
		a.conns++

		if !c.HasBytes {
			continue
		}
		cur[c.Cookie] = connBytes{up: c.BytesUp, down: c.BytesDown}
		if !hasBaseline {
			continue
		}
		// 上一轮没见过这个 cookie，说明连接是这个周期内新建的，它的字节
		// 全部产生于本周期，按全量计入。
		prev, seen := t.prev[c.Cookie]
		a.up += int64(deltaBytes(c.BytesUp, prev.up, seen))
		a.down += int64(deltaBytes(c.BytesDown, prev.down, seen))
	}

	for key, a := range aggs {
		e := t.ips[key]
		if e == nil {
			// 首次观测按活跃起算：还没有基准，拿不到字节增量，此时判成闲置
			// 会让刚连上的人立刻被当作不占额度。
			e = &onlineEntry{ip: a.ip, firstSeen: now, lastActiveAt: now}
			t.ips[key] = e
		}
		e.conns = a.conns
		if hasBaseline {
			if a.up > 0 || a.down > 0 {
				e.lastActiveAt = now
			}
			e.up += a.up
			e.down += a.down
			seconds := elapsed.Seconds()
			e.upSpeed = int64(float64(a.up) / seconds)
			e.downSpeed = int64(float64(a.down) / seconds)
		} else {
			e.upSpeed, e.downSpeed = 0, 0
		}
	}

	// 本轮没出现的 IP 立刻移除——这正是"断链、没有数据传输之后立即释放并发额度"
	// 所依赖的语义，不能为了界面好看而做延迟淘汰。
	for key := range t.ips {
		if _, ok := aggs[key]; !ok {
			delete(t.ips, key)
		}
	}

	t.purgeRejectedLocked(now)

	t.prev = cur
	t.lastSample = now
}

// deltaBytes 计算一条连接在本周期内新增的字节。内核计数器只会单调增长，
// 出现回退只可能是 cookie 被复用，此时按全量计入，绝不产生负增量。
func deltaBytes(now, prev uint64, seen bool) uint64 {
	if !seen || now < prev {
		return now
	}
	return now - prev
}

// snapshot 不做闲置判定，等价于 snapshotAt(port, locate, 0, now)。
func (t *onlineTracker) snapshot(port int, locate func(net.IP) (primary, alt string)) []OnlineIP {
	return t.snapshotAt(port, locate, 0, time.Now())
}

// snapshotIdle 按 idleAfter 判定闲置。idleAfter <= 0 表示关闭该判定。
func (t *onlineTracker) snapshotIdle(port int, locate func(net.IP) (primary, alt string), idleAfter time.Duration) []OnlineIP {
	return t.snapshotAt(port, locate, idleAfter, time.Now())
}

// snapshotAt 是核心实现，now 由调用方给出以便测试。
func (t *onlineTracker) snapshotAt(port int, locate func(net.IP) (primary, alt string), idleAfter time.Duration, now time.Time) []OnlineIP {
	t.mu.Lock()
	defer t.mu.Unlock()

	list := make([]OnlineIP, 0, len(t.ips))
	for key, e := range t.ips {
		if key.port != port {
			continue
		}
		primary, alt := locate(e.ip)
		list = append(list, OnlineIP{
			IP:          key.ip,
			Location:    primary,
			LocationAlt: alt,
			Conns:       e.conns,
			FirstSeen:   e.firstSeen.UnixMilli(),
			UpSpeed:     e.upSpeed,
			DownSpeed:   e.downSpeed,
			Up:          e.up,
			Down:        e.down,
			Idle:        idleAfter > 0 && now.Sub(e.lastActiveAt) > idleAfter,
			Blocked:     t.rejectedNow[key],
			RejectedAt:  millisOrZero(t.rejectedAt[key]),
		})
	}
	// 被拒的 IP 连接已经被断开，连接表里查不到它，但管理员必须能看到
	// 是谁在被挡。
	for key, at := range t.rejectedAt {
		if key.port != port {
			continue
		}
		if _, live := t.ips[key]; live {
			continue
		}
		primary, alt := locate(net.ParseIP(key.ip))
		list = append(list, OnlineIP{
			IP:          key.ip,
			Location:    primary,
			LocationAlt: alt,
			Blocked:     true,
			RejectedAt:  at.UnixMilli(),
		})
	}
	// 按地址字节升序。顺序不稳定的话页面每 2 秒刷新就会不停跳行。
	sort.Slice(list, func(i, j int) bool {
		return bytes.Compare(net.ParseIP(list[i].IP).To16(), net.ParseIP(list[j].IP).To16()) < 0
	})
	return list
}

func millisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (t *onlineTracker) sampleAge(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastSample.IsZero() {
		return time.Duration(1<<62 - 1)
	}
	return now.Sub(t.lastSample)
}

// 跨请求状态按项目惯例放包级变量，service 本身保持无状态空结构体。
var (
	onlineTrackerInstance = newOnlineTracker()
	onlineSampleLock      sync.Mutex
	// IPv6 查询失败（内核没编译 IPv6）只需要提示一次，否则每次采样都刷日志。
	onlineIPv6Warned bool
)

type OnlineService struct {
	inboundService InboundService
	ipdbService    IPDBService
	settingService SettingService
}

func formatLocation(loc ipdb.Location) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{loc.Country, loc.Region, loc.City} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}

// locate 返回主判定与「另一个源给出的不同结论」。
//
// 不做仲裁：实测两个离线库对同一批 IP 互有出入，谁也不是权威。把分歧原样
// 显示给管理员，比替他挑一个更有用。
func (s *OnlineService) locate(ip net.IP) (primary, alt string) {
	db := s.ipdbService.DB()
	if db == nil {
		return "", ""
	}
	for _, sl := range db.Lookup(ip) {
		text := formatLocation(sl.Location)
		if text == "" {
			continue
		}
		if primary == "" {
			primary = text
			continue
		}
		if text != primary && alt == "" {
			alt = text
		}
	}
	return primary, alt
}

// transportObservable 判断某入站的传输方式能否从内核连接表里看到每个客户端。
// mKCP / QUIC 走 UDP，xray 在端口上只开一个 socket 自己做复用，内核里没有
// 每个客户端的连接条目。
func transportObservable(streamSettings string) (bool, string) {
	network := "tcp"
	if streamSettings != "" {
		var stream struct {
			Network string `json:"network"`
		}
		if err := json.Unmarshal([]byte(streamSettings), &stream); err == nil && stream.Network != "" {
			network = stream.Network
		}
	}
	switch network {
	case "tcp", "ws", "websocket", "grpc", "gun", "h2", "http":
		return true, ""
	case "kcp", "mkcp":
		return false, "mKCP 走 UDP，内核连接表里没有单个客户端的连接记录，无法统计在线明细"
	case "quic":
		return false, "QUIC 走 UDP，内核连接表里没有单个客户端的连接记录，无法统计在线明细"
	default:
		return false, "传输方式 " + network + " 无法从内核连接表观测在线明细"
	}
}

func (s *OnlineService) inboundPorts() (map[int]bool, error) {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	ports := make(map[int]bool, len(inbounds))
	for _, inbound := range inbounds {
		ports[inbound.Port] = true
	}
	return ports, nil
}

func dumpAllConns() ([]netdiag.Conn, error) {
	conns, err := netdiag.Dump(netdiag.FamilyIPv4)
	if err != nil {
		return nil, err
	}
	v6, err := netdiag.Dump(netdiag.FamilyIPv6)
	if err != nil {
		// IPv4 已经拿到了，IPv6 失败（内核关了 IPv6）不该让整个功能不可用。
		if !onlineIPv6Warned {
			onlineIPv6Warned = true
			logger.Warning("读取 IPv6 连接表失败，在线明细将只统计 IPv4:", err)
		}
		return conns, nil
	}
	return append(conns, v6...), nil
}

// sample 采一次内核连接表。距上次采样不足 onlineMinSampleInterval 时直接跳过，
// 复用上一轮结果。
func (s *OnlineService) sample() error {
	onlineSampleLock.Lock()
	defer onlineSampleLock.Unlock()

	now := time.Now()
	if onlineTrackerInstance.sampleAge(now) < onlineMinSampleInterval {
		return nil
	}
	ports, err := s.inboundPorts()
	if err != nil {
		return err
	}
	conns, err := dumpAllConns()
	if err != nil {
		return err
	}
	onlineTrackerInstance.update(conns, ports, time.Now())
	return nil
}

// GetOnlines 返回某入站当前的在线明细。
func (s *OnlineService) GetOnlines(inboundId int) (*OnlineResult, error) {
	inbound, err := s.inboundService.GetInbound(inboundId)
	if err != nil {
		return nil, err
	}
	if !netdiag.Supported {
		return &OnlineResult{Reason: "在线明细依赖 Linux 内核连接表，当前系统不支持", List: []OnlineIP{}}, nil
	}
	if ok, reason := transportObservable(inbound.StreamSettings); !ok {
		return &OnlineResult{Reason: reason, List: []OnlineIP{}}, nil
	}
	if !inbound.Enable {
		return &OnlineResult{Supported: true, Reason: "该入站已停用", List: []OnlineIP{}}, nil
	}
	if err := s.sample(); err != nil {
		return nil, err
	}
	return &OnlineResult{
		Supported: true,
		List:      onlineTrackerInstance.snapshotIdle(inbound.Port, s.locate, idleTimeoutOrZero(s.settingService.GetConcurrencyIdleTimeout())),
	}, nil
}

// Kick 断开某入站上指定来源 IP 的全部 TCP 连接，返回实际断开的连接数。
//
// 注意它只是"断开当前连接"：客户端通常会立刻重连。真正的封禁属于并发限制
// 子项目（运行时下发 xray 路由规则），这里不做。
func (s *OnlineService) Kick(inboundId int, ipStr string) (int, error) {
	inbound, err := s.inboundService.GetInbound(inboundId)
	if err != nil {
		return 0, err
	}
	// 参数校验放在能力判断之前：请求本身写错了，应当先如实说出来。
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0, common.NewError("IP 格式不正确:", ipStr)
	}
	if !netdiag.Supported {
		return 0, netdiag.ErrUnsupported
	}
	target := normalizeIP(ip)

	// 重新 dump 一次而不是用页面上的快照：cookie 是内核给 socket 的标识，
	// 拿过期的 cookie 去 destroy 只会失败或误伤。
	conns, err := dumpAllConns()
	if err != nil {
		return 0, err
	}

	killed := 0
	var firstErr error
	for _, c := range conns {
		if int(c.LocalPort) != inbound.Port {
			continue
		}
		if !normalizeIP(c.RemoteIP).Equal(target) {
			continue
		}
		if err := netdiag.Destroy(c); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		killed++
	}
	if killed == 0 && firstErr != nil {
		return 0, firstErr
	}
	if firstErr != nil {
		logger.Warning("踢下线时部分连接关闭失败:", firstErr)
	}
	return killed, nil
}
