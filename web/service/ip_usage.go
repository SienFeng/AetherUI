package service

import (
	"bytes"
	"net"
	"sort"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// ipUsageMaxEntries 是返回给界面的条数上限。
//
// sharingMaxRowsPerHour 限的是「单入站单小时 50 个 IP」，30 天窗口下去重后
// 的 IP 总数没有上界——一次持续的端口扫描能攒出几千个。表格渲染几千行会把
// 页面卡死，而管理员真正要看的永远是用量最大的那几个。超出的部分**不丢弃**，
// 合计进 OtherCount/OtherUp/OtherDown，界面渲染成末尾一行：总量不能静默缩水。
const ipUsageMaxEntries = 200

// ipUsageBeyondRetention 是窗口起点超出 InboundIPHour 保留期时的说明。
//
// 必须有这句话：返回一张看起来正常的空表，管理员会以为这段时间没人用过。
//
// 文案说的是「更早的时段」而不是「更长的区间」——判据按起点而不是跨度
//（见 Query），一个跨度只有 10 天但整体落在 3 个月前的自定义区间同样会
// 降级，说成「更长的区间」会让管理员以为缩短区间就能看到，而那没有用。
const ipUsageBeyondRetention = "按来源 IP 的明细只保留 30 天，更早的时段只有入站合计用量"

// ipUsageNoSplitReason 是整批数据都来自升级前时的说明。
//
// 写成常量而不是 fmt.Sprintf 拼一个没有占位符的字符串——后者会被
// go vet 的 printf 检查判为多余，让 make verify 直接失败。
const ipUsageNoSplitReason = "该时段有升级前写入的数据，无法拆分上下行，显示的是合计用量"

// IPUsageEntry 是某个来源 IP 在窗口内的用量。
type IPUsageEntry struct {
	IP   string `json:"ip"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`

	// 归属地五件套与 OnlineIP 同名同义，由同一个 locateWithIPDB 产出。
	// 复用它而不是自己写一次单值 lookup，离线行才能和在线行一样显示
	//「存疑」标记，也不会出现同一个 IP 换一行就换个归属地的情形。
	Location    string             `json:"location"`
	LocationAlt string             `json:"locationAlt"`
	ISP         string             `json:"isp"`
	ISPAlt      string             `json:"ispAlt"`
	Sources     []ipSourceLocation `json:"sources"`

	// LastSeen 是窗口内最后一个有记录的小时（毫秒）。界面用它给离线行
	// 填「上线时间」那一列——对已经断开的来源，「最后活跃」比一个空值有用。
	LastSeen int64 `json:"lastSeen"`
}

// IPUsageResult 是按来源 IP 的分项查询结果。
type IPUsageResult struct {
	Entries []IPUsageEntry `json:"entries"`

	// Split 为 false 表示这批数据拆不出上下行（全是升级前写入的行），
	// 界面应显示合计数而不是「↑0 ↓0」——后者是「没有用量」的意思。
	Split bool `json:"split"`

	// BeyondRetention 为 true 表示窗口超出了保留期，Entries 必为空。
	BeyondRetention bool `json:"beyondRetention"`

	// OtherCount/OtherUp/OtherDown 是被长尾截断合并掉的那部分。
	OtherCount int   `json:"otherCount"`
	OtherUp    int64 `json:"otherUp"`
	OtherDown  int64 `json:"otherDown"`

	Reason string `json:"reason"`
}

// IPUsageService 查询按来源 IP 的分时段用量。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type IPUsageService struct {
	ipdbService IPDBService
}

// hasUnsplitBytes 判断这批行里是否存在「有字节、却拆不出上下行」的行。
//
// 存在即整批按合计口径显示：只要有一行拆不出，这个窗口的上下行拆分就不
// 可靠，此时给出「↑X ↓Y」这种看起来精确的数字，实际掺着别的行的合计，
// 比明确降级成一个合计数更糟——本项目一贯宁可降级也不给误导性数据。
//
// **必须跳过 ActiveBytes == 0 的行**：一条「满了 60 秒门槛但本小时零字节」
// 的新行，与升级前写入的老行在数值上完全无法区分（这正是 sharing_stat.go
// 里 hasActiveBytes 那段注释描述的同一个歧义）。零字节行在两种口径下都
// 贡献 0、不影响求和，也就不该影响口径判定；不跳过的话它会毒化判据，把
// 一批纯新数据也拖进降级。
func hasUnsplitBytes(rows []model.InboundIPHour) bool {
	for _, r := range rows {
		if r.ActiveBytes > 0 && r.ActiveUp == 0 && r.ActiveDown == 0 {
			return true
		}
	}
	return false
}

// Query 返回某入站在窗口内各来源 IP 的用量，按用量降序。
//
// 窗口超出保留期时整块降级（BeyondRetention），而不是返回一张空表：
//「看不到」和「没有」必须能区分开。
func (s *IPUsageService) Query(inboundId int, w TrafficWindow, now time.Time) (*IPUsageResult, error) {
	result := &IPUsageResult{Entries: []IPUsageEntry{}}

	// 判据看的是**起点**而不是跨度。
	//
	// 所有档位（today/3d/7d/30d/1y）的 End 都恒等于 now，所以对它们而言
	//「跨度 > 30 天」与「起点早于 30 天前」是同一件事；但自定义区间可以整个
	// 落在过去——选「3 个月前的那 10 天」时跨度只有 10 天，按跨度判不会降级，
	// 于是返回一个空 Entries 和空 Reason，界面把它渲染成「没有人用过」，而
	// 真相是那段时间的行早就被保留期清掉了。按起点判同时覆盖两种情形：
	// End ≤ now 恒成立（ParseWindow 的钳制五），所以凡跨度超 30 天的，
	// 起点必然也早于 30 天前，原来那条判据能拦下的现在一条不漏。
	if w.Start < now.AddDate(0, 0, -ipUsageMaxWindowDays).Unix() {
		result.BeyondRetention = true
		result.Reason = ipUsageBeyondRetention
		return result, nil
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}

	var rows []model.InboundIPHour
	err := db.Where("inbound_id = ? and hour_start >= ? and hour_start < ?",
		inboundId, w.Start, w.End).Find(&rows).Error
	if err != nil {
		return nil, err
	}

	result.Split = !hasUnsplitBytes(rows)

	type agg struct {
		up, down int64
		lastSeen int64
	}
	byIP := map[string]*agg{}
	for _, r := range rows {
		a := byIP[r.IP]
		if a == nil {
			a = &agg{}
			byIP[r.IP] = a
		}
		if result.Split {
			a.up += r.ActiveUp
			a.down += r.ActiveDown
		} else {
			// 降级：拆不出上下行时把合计全部计进 Down。界面在 Split 为
			// false 时只显示合计数，不显示箭头，所以计进哪一侧不影响显示；
			// 计进 Down 而不是对半分，是为了让「合计 = Up + Down」这个
			// 恒等式在任何情形下都成立，前端不必为降级另写一套求和。
			a.down += r.ActiveBytes
		}
		if hourMillis := r.HourStart * 1000; hourMillis > a.lastSeen {
			a.lastSeen = hourMillis
		}
	}

	entries := make([]IPUsageEntry, 0, len(byIP))
	for ip, a := range byIP {
		e := IPUsageEntry{IP: ip, Up: a.up, Down: a.down, LastSeen: a.lastSeen}
		// 离线 IP 不在连接表快照里，归属地必须重新查。走 locateWithIPDB
		// 是为了与在线明细、访问日志的来源列表共用同一套判定。
		if parsed := net.ParseIP(ip); parsed != nil {
			loc := locateWithIPDB(s.ipdbService, parsed)
			e.Location, e.LocationAlt = loc.Location, loc.LocationAlt
			e.ISP, e.ISPAlt = loc.ISP, loc.ISPAlt
			e.Sources = loc.Sources
		}
		entries = append(entries, e)
	}

	// 用量降序；同量时按 IP 字节序，保证同一份输入永远给出同一个次序
	//（与 onlineTracker.snapshotAt 的排序同源）。遍历 map 产生的顺序不定，
	// 不排的话每次刷新表格行都会跳。
	sort.Slice(entries, func(i, j int) bool {
		li, lj := entries[i].Up+entries[i].Down, entries[j].Up+entries[j].Down
		if li != lj {
			return li > lj
		}
		return bytes.Compare(
			net.ParseIP(entries[i].IP).To16(),
			net.ParseIP(entries[j].IP).To16(),
		) < 0
	})

	if len(entries) > ipUsageMaxEntries {
		for _, e := range entries[ipUsageMaxEntries:] {
			result.OtherUp += e.Up
			result.OtherDown += e.Down
		}
		result.OtherCount = len(entries) - ipUsageMaxEntries
		entries = entries[:ipUsageMaxEntries]
	}
	result.Entries = entries

	if !result.Split && len(entries) > 0 {
		result.Reason = ipUsageNoSplitReason
	}
	return result, nil
}
