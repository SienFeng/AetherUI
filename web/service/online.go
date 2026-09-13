package service

import (
	"bytes"
	"encoding/json"
	"net"
	"sort"
	"sync"
	"time"

	"a-ui/database/model"
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

	// minActiveRate 是「这个来源还在用」的最低速率（字节/秒），低于它的采样
	// 轮次不刷新 lastActiveAt，于是该来源会按 concurrencyIdleTimeout 被判闲置、
	// 不再占用并发额度。
	//
	// 不设这道门槛的话，任何一个字节都算活跃，而一个挂在后台没关的客户端靠
	// 心跳保活包（几十到几百字节/秒）就能永久保持活跃、一直占着额度——把额度
	// 设成 1 时，机主自己的另一台设备就永远连不上。
	//
	// 1 KB/s 这个值来自生产实测（香港节点 5 个真实在用的入站，120 秒逐秒采样）：
	// 真实代理流量是「长静默 + 突发」，中位速率为 0、67~92% 的采样秒低于 1 KB/s，
	// 而 p90 落在 0~6.5 KB/s。1 KB/s 能把心跳与真实使用分开；若定到 8 KB/s，
	// 那五个用户几乎每一秒都会被判成闲置，并发限制整个失效。
	//
	// 判的是**速率**不是单轮字节数：采样间隔并不恒定（并发判定每秒一次，而
	// 页面轮询也会触发采样，下限 onlineMinSampleInterval 是 500ms），按绝对
	// 字节判会让同一份流量因为「当时有没有人开着面板」而得出相反的结论。
	//
	// 刻意不做成设置项：新增设置项要同步改 5 处，漏掉 models.js 那处会让整个
	// 保存配置接口失败，为一个经验值付这个代价不划算。要调就改这里重新编译。
	minActiveRate = 1024

	// 超额被拒的 IP 在连接被断开后仍然在展开行里保留这么久。
	// 不留的话"有人正在被拒绝"这件事对管理员完全不可见——被拒的 IP
	// 恰恰是连接表里没有的那个。
	onlineRejectRetention = 5 * time.Minute
)

// onlineUnsupportedReason 是本平台读不到内核连接表时的统一说明。在线明细与
// 在线设备数共用一份措辞，避免两处对同一件事给出不同的解释。
const onlineUnsupportedReason = "在线明细依赖 Linux 内核连接表，当前系统不支持"

// OnlineIP 是某个入站上一个来源 IP 的在线明细，对应展开行里的一行。
type OnlineIP struct {
	IP       string `json:"ip"`
	Location string `json:"location"`
	// LocationAlt 是另一个数据源给出的、与主判定**不同**的结论。
	// 两个源一致时为空。多源的价值就在于把分歧显示出来，藏起来等于白加。
	LocationAlt string `json:"locationAlt"`
	// ISP 是来源 IP 的运营商，已归一（"中国电信" / "腾讯云" / "Google"）。
	// 境外 IP 只在命中知名 IDC / 云厂商时非空——那正是「这个来源是机房」
	// 这个判断所需的全部信息，而它与家宽来源的含义完全不同。
	ISP string `json:"isp"`
	// ISPAlt 与 LocationAlt 同理：另一个源给出的、与主判定不同的运营商。
	ISPAlt string `json:"ispAlt"`
	// Sources 是各数据源各自的结论，界面用它说明「是谁说的」——只说
	// 「另一个源认为是 X」，管理员没法判断该信哪一个。
	Sources []ipSourceLocation `json:"sources"`
	// Evidence 回答「这个判定有多少源同意」。Sources 列出了每个源怎么说，
	// 但源多起来之后管理员没法一眼数清支持与反对各几个，尤其是「主判定只有
	// 一个源支持、另外三个源一致认为是别的省」这种最该被看见的情形。
	Evidence  provinceEvidence `json:"evidence"`
	Conns     int              `json:"conns"`
	FirstSeen int64            `json:"firstSeen"` // 毫秒；面板首次观测到该 IP 的时间
	UpSpeed   int64            `json:"upSpeed"`   // B/s
	DownSpeed int64            `json:"downSpeed"`
	Up        int64            `json:"up"` // 本次在线期间的累计字节
	Down      int64            `json:"down"`

	// Idle 为 true 表示该 IP 的连接还在，但已经连续 idleAfter 没有任何字节
	// 往来。闲置来源不占用并发额度：TCP 连接不会因为没有流量就消失，客户端
	// 只要不退出就一直是 ESTABLISHED，不判闲置的话一个挂着不用的客户端会
	// 永久占着名额。
	Idle bool `json:"idle"`
	// Banned 为 true 表示该 IP 处于封禁期内，连接每轮都会被断开。
	// 与 Blocked 的区别：Blocked 是并发额度算出来的、额度一腾出来就消失；
	// Banned 是管理员显式设的，到期或手动解封才结束。
	Banned bool `json:"banned"`
	// BanExpiresAt 是封禁到期时间（毫秒），0 且 Banned 为 true 表示永久封禁。
	BanExpiresAt int64 `json:"banExpiresAt"`
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
			seconds := elapsed.Seconds()
			e.upSpeed = int64(float64(a.up) / seconds)
			e.downSpeed = int64(float64(a.down) / seconds)
			// 按上下行**合计**速率判门槛：额度判的是这个来源在不在用，不是
			// 某一个方向在不在用。顺序也不能反——速率要先算出来才能判。
			if e.upSpeed+e.downSpeed >= minActiveRate {
				e.lastActiveAt = now
			}
			e.up += a.up
			e.down += a.down
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
func (t *onlineTracker) snapshot(port int, locate func(net.IP) ipLocation) []OnlineIP {
	return t.snapshotAt(port, locate, 0, time.Now())
}

// snapshotIdle 按 idleAfter 判定闲置。idleAfter <= 0 表示关闭该判定。
func (t *onlineTracker) snapshotIdle(port int, locate func(net.IP) ipLocation, idleAfter time.Duration) []OnlineIP {
	return t.snapshotAt(port, locate, idleAfter, time.Now())
}

// snapshotAt 是核心实现，now 由调用方给出以便测试。
func (t *onlineTracker) snapshotAt(port int, locate func(net.IP) ipLocation, idleAfter time.Duration, now time.Time) []OnlineIP {
	t.mu.Lock()
	defer t.mu.Unlock()

	list := make([]OnlineIP, 0, len(t.ips))
	for key, e := range t.ips {
		if key.port != port {
			continue
		}
		loc := locate(e.ip)
		list = append(list, OnlineIP{
			IP:          key.ip,
			Location:    loc.Location,
			LocationAlt: loc.LocationAlt,
			ISP:         loc.ISP,
			ISPAlt:      loc.ISPAlt,
			Sources:     loc.Sources,
			Evidence:    loc.Evidence,
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
		loc := locate(net.ParseIP(key.ip))
		list = append(list, OnlineIP{
			IP:          key.ip,
			Location:    loc.Location,
			LocationAlt: loc.LocationAlt,
			ISP:         loc.ISP,
			ISPAlt:      loc.ISPAlt,
			Sources:     loc.Sources,
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
	banService     IPBanService
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

// ipLocation 是一次归属地判定的完整结果。
//
// 不用四个 string 返回值：Location / LocationAlt / ISP / ISPAlt 类型相同，
// 调用点写反了不会有任何编译错误，只会让界面上的主判定与分歧对调。
type ipLocation struct {
	Location    string
	LocationAlt string
	ISP         string
	ISPAlt      string

	// Sources 是每个加载成功的数据源各自的结论，顺序与 Multi.Sources() 一致。
	//
	// 上面四个字段只回答「主判定是什么、有没有别的说法」，回答不了「是谁说的」。
	// 生产上真实吃过亏：ip2region 把一个湖北的 IP 判成北京，界面按顺序把它当作
	// 唯一答案显示，看起来像地区限制漏放了一个北京来源——实际上纯真库判对了、
	// 并集也正确放行。管理员只能自己去查第三方才搞清楚，而面板本来就掌握着
	// 「两个源分别怎么说」这个信息。
	Sources []ipSourceLocation

	// Evidence 是省级判定的多源证据，只用于展示，不参与仲裁。
	Evidence provinceEvidence
}

// ipSourceLocation 是单个数据源对一个 IP 的结论。
type ipSourceLocation struct {
	Source   string `json:"source"`
	Location string `json:"location"`
	ISP      string `json:"isp"`
}

// provinceEvidence 是省级判定的多源证据，**只用于展示，不参与仲裁**。
//
// 主判定仍由 ipdbSourceList 的顺序决定（顺序的依据是各源的兜底率，见该处注释），
// 这里回答的是另一个问题：这个判定有多少源同意。两者分开是刻意的——实测四个
// 源里有两个会把大批判不准的段兜底到广东省，朴素多数票会让那批兜底值顶掉前面
// 源的正确判定，而界面上完全看不出来。
//
// **三个计数必须分开，绝不能把 Silent 并进 Support。** 「两个源支持江苏，两个
// 源没有数据」与「四个源都支持江苏」在界面上必须是两句话：前者的可信度明显更
// 低，而合并之后一个只有单源覆盖的判定会显示成「4/4 一致」，把最不该被信任的
// 那类判定包装成最可信的。
type provinceEvidence struct {
	// Province 是得票最多的省份。与主判定不一定相同——不同的时候恰恰最该看。
	Province string `json:"province"`
	// Support 是给出这个省份的源数，Conflict 是给出**别的**省份的源数，
	// Silent 是没有省级结论的源数（没收录该 IP，或收录了但没有省份，比如境外段）。
	Support  int `json:"support"`
	Conflict int `json:"conflict"`
	Silent   int `json:"silent"`
	// Total 是当前加载成功的源数。前端要显示 "3/4" 这种分母，而它不等于
	// Support+Conflict+Silent 之外的任何一个常数——管理员可能只启用了两个源。
	Total int `json:"total"`
}

// evidenceOf 按各源的省级结论统计证据。locs 只含**收录了该 IP** 的源，
// 所以 Silent 要用总源数减出来，不能靠遍历 locs 数。
func evidenceOf(locs []ipdb.SourceLocation, total int) provinceEvidence {
	votes := map[string]int{}
	withRegion := 0
	for _, sl := range locs {
		if sl.Location.Region == "" {
			continue
		}
		withRegion++
		votes[sl.Location.Region]++
	}
	ev := provinceEvidence{Total: total, Silent: total - withRegion}
	// 平票时取字节序最小的那个，只为让同一份库对同一个 IP 永远给出同一个
	// 展示结果——这里不承担仲裁职责，取谁都不影响主判定。
	for province, n := range votes {
		if n > ev.Support || (n == ev.Support && province < ev.Province) {
			ev.Province, ev.Support = province, n
		}
	}
	ev.Conflict = withRegion - ev.Support
	return ev
}

// locate 返回主判定与「另一个源给出的不同结论」。
//
// 不做仲裁：实测两个离线库对同一批 IP 互有出入，谁也不是权威。把分歧原样
// 显示给管理员，比替他挑一个更有用。
func (s *OnlineService) locate(ip net.IP) ipLocation {
	return locateWithIPDB(s.ipdbService, ip)
}

// locateWithIPDB 判定 IP 的归属地与运营商，并把两个源的分歧一并带出来。
// 在线明细与访问日志的来源列表共用同一套判定，避免三处结论对不上。
//
// 归属地与运营商各自独立判定分歧：两个源完全可能在城市名上有出入而运营商
// 一致（真实数据就是如此），把它们绑成一个判定会让「存疑」失去指向。
func locateWithIPDB(svc IPDBService, ip net.IP) ipLocation {
	var out ipLocation
	db := svc.DB()
	if db == nil {
		return out
	}
	// 主判定对应的原始 Location。分歧要按字段逐级判，不能拿 formatLocation
	// 拼出来的串比——「南京市」与「南京」是同一个地方的两种写法。
	var primary ipdb.Location
	found := db.Lookup(ip)
	out.Evidence = evidenceOf(found, db.Len())
	for _, sl := range found {
		text := formatLocation(sl.Location)
		out.Sources = append(out.Sources, ipSourceLocation{
			Source:   ipdbSourceName(sl.Source),
			Location: text,
			ISP:      sl.Location.ISP,
		})
		if text != "" {
			if out.Location == "" {
				out.Location, primary = text, sl.Location
			} else if out.LocationAlt == "" && !sameLocation(primary, sl.Location) {
				out.LocationAlt = text
			}
		}
		if isp := sl.Location.ISP; isp != "" {
			if out.ISP == "" {
				out.ISP = isp
			} else if isp != out.ISP && out.ISPAlt == "" {
				out.ISPAlt = isp
			}
		}
	}
	return out
}

// sameLocation 判断两个数据源说的是不是同一个地方。
//
// 逐级比较而不是比 formatLocation 拼出来的整串，两处与直觉不同的口径各自
// 防着一类假阳性——实测两份真实库对同一批中国 IP 有 87.9% 会被判成分歧，
// 而真正的分歧只有 17.6%，「存疑」几乎对每个 IP 都亮，等于把这个提示废掉：
//
//   - 城市按 ipdb.CanonicalCity 归一后再比。ip2region 写「南京市」、纯真库写
//     「南京」，占了全部假阳性的三分之二。
//   - 某一级一方为空时视为相同。那是两个源的精度不同（纯真库对一批段只给到
//     省），不是它们对同一个问题给出了两个答案。地区限制只看省份，城市这一级
//     的取舍也影响不到放行判定。
//
// 反过来，归一后仍然不同就必须如实报出来：漏报比误报难查得多——管理员看到的
// 会是一个被当成唯一答案的错误归属地，而面板本来掌握着「另一个源不这么认为」
// 这个信息。
func sameLocation(a, b ipdb.Location) bool {
	sameField := func(x, y string) bool { return x == "" || y == "" || x == y }
	return sameField(a.Country, b.Country) &&
		sameField(a.Region, b.Region) &&
		sameField(ipdb.CanonicalCity(a.City), ipdb.CanonicalCity(b.City))
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
		return &OnlineResult{Reason: onlineUnsupportedReason, List: []OnlineIP{}}, nil
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
	list := onlineTrackerInstance.snapshotIdle(inbound.Port, s.locate,
		idleTimeoutOrZero(s.settingService.GetConcurrencyIdleTimeout()))
	// 封禁标记必须在这里补：被封的 IP 连接已经断干净，连接表里查不到它，
	// 不补的话管理员在界面上看不到自己封了谁，也就无从解封。
	bans, err := s.banService.ActiveBans(inbound.Id, time.Now())
	if err != nil {
		logger.Warning("读取封禁名单失败, 入站", inbound.Id, ":", err)
	} else if len(bans) > 0 {
		list = markBanned(list, bannedIPSet(bans))
	}
	return &OnlineResult{Supported: true, List: list}, nil
}

// OnlineCount 是入站列表页「在线设备」那一列的数据。
//
// Supported 为 false 时 Count 恒为 0，界面必须显示 Reason 而不是 0——
// 与 OnlineResult 同一条铁律：「看不到」不能渲染成「没人在线」。
type OnlineCount struct {
	InboundId int    `json:"inboundId"`
	Count     int    `json:"count"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`

	// UpSpeed/DownSpeed 是该入站上所有在线来源的实时速率之和（B/s），
	// 也就是展开行里每台设备那两个数字的合计。与 Count 同一条铁律：
	// Supported 为 false 时它们恒为 0，界面必须显示 Reason 而不是 0 B/s——
	// 把「读不到连接表」渲染成「一点流量都没有」比不显示更糟。
	UpSpeed   int64 `json:"upSpeed"`
	DownSpeed int64 `json:"downSpeed"`
}

// countLive 统计在线设备数：还有活连接的来源 IP 个数。一个 IP 视为一台设备，
// NAT 后的多台设备会合并成一台，这是 IP 维度统计的固有限制，与并发额度同口径。
//
// 与 liveOnly 刻意不同，两处回答的是不同的问题：
//   - 闲置计入。连接还在，人还挂在线上；liveOnly 排除它是因为那里问的是
//     「谁占着并发名额」，一个暂时没流量的正常用户不该占名额、也不该被断开。
//   - 封禁中但连接尚未被断掉的来源计入。它此刻确实连着，下一轮判定才会被断开；
//     为了把它剔掉而每 2 秒查一次封禁表不值得，那是个转瞬即逝的状态。
//
// Conns == 0 的条目是超额被拒 / 被封禁后连接已断干净、仅为界面展示保留的
// 历史记录，它们不是在线设备。
func countLive(list []OnlineIP) int {
	n := 0
	for _, e := range list {
		if e.Conns > 0 {
			n++
		}
	}
	return n
}

// sumLiveSpeed 汇总某入站上所有在线来源的实时速率（B/s）。
//
// 口径与 countLive 严格一致——只算还有活连接的来源。Conns == 0 的条目是
// 超额被拒 / 被封禁后连接已断干净、仅为界面展示保留的历史记录，它们的速率
// 字段恒为零值，算不算进来结果都一样；仍然显式排除，是为了让「在线 N 台」
// 与「合计多少 B/s」这两列出自同一条判据，而不是依赖另一处实现的零值巧合。
//
// 速率本身由 onlineTracker.update 每轮重算：某个来源这一轮没有字节往来时
// 它会被写成 0，不会留着上一轮的值，所以这里直接求和不需要再判新鲜度。
func sumLiveSpeed(list []OnlineIP) (up, down int64) {
	for _, e := range list {
		if e.Conns > 0 {
			up += e.UpSpeed
			down += e.DownSpeed
		}
	}
	return up, down
}

// countabilityOf 判断某入站的在线设备数能不能数出来，数不出来时给出原因。
//
// platformSupported 由调用方传入而不是就地引用 netdiag.Supported：后者是
// 编译期常量，在非 Linux 上恒为 false，就地引用会让这段口径判定无法在
// 开发机上被测到。
func countabilityOf(inbound *model.Inbound, platformSupported bool) (bool, string) {
	if !platformSupported {
		return false, onlineUnsupportedReason
	}
	if ok, reason := transportObservable(inbound.StreamSettings); !ok {
		return false, reason
	}
	return true, ""
}

// CountAll 返回该用户每个入站当前的在线设备数，供入站列表页那一列使用。
//
// 做成批量而不是让前端对每个入站各调一次 GetOnlines：sample() 本来就是一次
// dump 整张连接表、更新所有端口，按入站逐个请求只是把同一份快照切开来取，
// 白白多出 N-1 次往返。
func (s *OnlineService) CountAll(userId int) ([]OnlineCount, error) {
	inbounds, err := s.inboundService.GetInbounds(userId)
	if err != nil {
		return nil, err
	}

	counts := make([]OnlineCount, 0, len(inbounds))
	// 采样推迟到确实有入站需要它时才做：全部停用、或全是 mKCP/QUIC、
	// 或平台根本不支持时，这个每 2 秒一次的接口一次系统调用都不产生。
	sampled := false
	for _, inbound := range inbounds {
		if ok, reason := countabilityOf(inbound, netdiag.Supported); !ok {
			counts = append(counts, OnlineCount{InboundId: inbound.Id, Reason: reason})
			continue
		}
		if !inbound.Enable {
			// 停用的入站不在 xray 配置里，没有连接可言，0 在这里是诚实的。
			counts = append(counts, OnlineCount{InboundId: inbound.Id, Supported: true})
			continue
		}
		if !sampled {
			if err := s.sample(); err != nil {
				return nil, err
			}
			sampled = true
		}
		// 只取一次快照，在同一份 list 上同时算设备数与速率合计：snapshotAt
		// 会遍历全表并排序，取两次既是白做一遍，也让两列有落在不同瞬间的
		// 可能——「在线 0 台」配上一个非零速率会让人以为哪里坏了。
		list := onlineTrackerInstance.snapshot(inbound.Port, noLocate)
		up, down := sumLiveSpeed(list)
		counts = append(counts, OnlineCount{
			InboundId: inbound.Id,
			Supported: true,
			Count:     countLive(list),
			UpSpeed:   up,
			DownSpeed: down,
		})
	}
	return counts, nil
}

// KickAndBan 断开连接并按 banSeconds 封禁该来源。
//
// banSeconds == 0 时行为与 Kick 完全一致（只断当前连接，客户端下一秒就能
// 重连）；> 0 封禁指定秒数；< 0 永久封禁。
//
// 先写封禁再断连接：反过来的话，两步之间客户端重连进来的那一瞬间是不设防的。
func (s *OnlineService) KickAndBan(inboundId int, ipStr string, banSeconds int) (int, error) {
	if banSeconds != 0 {
		if net.ParseIP(ipStr) == nil {
			return 0, common.NewError("IP 格式不正确:", ipStr)
		}
		duration := time.Duration(banSeconds) * time.Second
		if banSeconds < 0 {
			duration = 0 // 0 表示永久
		}
		if err := s.banService.Ban(inboundId, ipStr, duration, time.Now()); err != nil {
			return 0, err
		}
	}
	return s.Kick(inboundId, ipStr)
}

// Unban 解除封禁。
func (s *OnlineService) Unban(inboundId int, ipStr string) error {
	return s.banService.Unban(inboundId, ipStr)
}

// Kick 断开某入站上指定来源 IP 的全部 TCP 连接，返回实际断开的连接数。
//
// 它只"断开当前连接"，不改变任何状态：并发额度判定每轮重算，客户端下一秒
// 重连、额度够了就照样放行。要让踢下线真正留下效果，用 KickAndBan。
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
