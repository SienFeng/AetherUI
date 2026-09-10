package service

import (
	"net"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/domain"
)

const (
	// domainStatBatchSize 是单轮从访问日志读取的行数上限，防止一次把大量
	// 数据读进内存。
	domainStatBatchSize = 20000

	// domainStatMaxRounds 是单次 Aggregate 最多连跑几轮。首次启用时库里
	// 可能已有几十万条积压，一轮两万行、十分钟一次的话要跑几个小时才追平，
	// 这期间榜单是残缺的；连跑到追平即可，20 轮（40 万行）的上限则防止
	// 单次调用长时间占住 CPU 与两个库。
	domainStatMaxRounds = 20
)

// DomainStatService 负责域名统计的聚合、清理与查询。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type DomainStatService struct {
	settingService SettingService
}

// domainStatLock 防止 Aggregate 的重叠调用。首次启用时库里可能积压几十万行，
// domainStatMaxRounds 轮循环有可能跑过 cron 的调度周期（这里没有配
// SkipIfStillRunning，只有 cron.Recover）；不加锁的话下一次触发会在位点
// 推进前读到同一批行，两个事务各自提交，同一批访问日志被计两次。
var domainStatLock sync.Mutex

// Aggregate 把访问日志里位点之后的记录聚合成域名分时桶，返回本次消费的行数。
//
// 库不可用时静默返回 0：榜单不可用不该让调用方出错。
func (s *DomainStatService) Aggregate() (int, error) {
	domainStatLock.Lock()
	defer domainStatLock.Unlock()

	tdb := database.GetTrafficDB()
	adb := database.GetAccessLogDB()
	if tdb == nil || adb == nil {
		return 0, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}
	tagToId, err := inboundTagToId()
	if err != nil {
		return 0, err
	}
	// 反过来用：访问日志里已经存了 inbound_id，这里只需要它是不是仍然有效。
	validId := make(map[int]bool, len(tagToId))
	for _, id := range tagToId {
		validId[id] = true
	}

	total := 0
	for round := 0; round < domainStatMaxRounds; round++ {
		cursor, err := loadDomainStatCursor(tdb)
		if err != nil {
			return total, err
		}

		// 迁移处理，只在每次 Aggregate 调用的第一轮检查一次：LastLogTime
		// 是本次改动新加的列，AutoMigrate 给已存在的位点行加这一列时，
		// LastLogId 已经是一段真实的历史位点，LastLogTime 却是零值。下面
		// Where 子句一旦以 time > 0 去比较，会命中全表，把升级前已经聚合
		// 过的全部历史重新聚合一遍、计数静默翻倍——这比"漏一段历史"严重
		// 得多，所以处理方式是"从现在开始，不补算历史"：把位点直接对齐到
		// 当前 access_log 里 (time, id) 最大的那一行（表当前为空就对齐到
		// (0, 0)，等同全新安装），跳过升级前的全部积压，宁可欠一段数据
		// 也不能重复计入。修正后 LastLogTime 恒大于 0（或连同 LastLogId
		// 一起归零），这个分支往后不会再触发。
		if round == 0 && cursor.LastLogTime == 0 && cursor.LastLogId > 0 {
			var latest model.AccessLog
			err := adb.Model(&model.AccessLog{}).
				Order("time desc, id desc").
				First(&latest).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return total, err
			}
			logger.Warningf("域名统计位点(id=%d)是升级前的旧位点、缺少写入时间，已对齐到当前最新的访问日志(time=%d, id=%d)——升级前的历史不再补算", cursor.LastLogId, latest.Time, latest.Id)
			if err := saveDomainStatCursor(tdb, latest.Id, latest.Time); err != nil {
				return total, err
			}
			cursor.LastLogId = latest.Id
			cursor.LastLogTime = latest.Time
		}

		// 位点是 (LastLogTime, LastLogId) 的复合序，time 为主序、id 只用来
		// 打破同一毫秒内的并列——理由见 DomainStatCursor 上方的注释：
		// access_log 的自增 id 是可复用的 rowid，删除/清空都会让它倒退或
		// 原地复用，而 time 是写入时刻，删除任何行都不会改变其余行的
		// time，因此以 time 为主序之后，"id 是否失效"这件事不再存在，
		// 不需要任何自愈或回退逻辑。
		//
		// 写成 time >= ? AND (time > ? OR id > ?) 而不是更直白的
		// time > ? OR (time = ? AND id > ?)——两者逻辑完全等价（四种真值
		// 组合逐一核对过：time>位点 命中；time=位点 且 id>位点id 命中；
		// time=位点 且 id<=位点id 被内层排除；time<位点 被前导 time>=?
		// 排除），区别只在查询计划：后者（纯 OR）在**绑定参数**下会让
		// SQLite 放弃对 time 索引的定位式访问、退化成按索引顺序扫描全部
		// 历史行——SQLite 只有在能看见两个 time 比较项是同一个常量时才能
		// 把 OR 折成范围约束，绑定参数下看不见这一点，只有把参数内联成
		// 字面量执行 EXPLAIN 才会得到误导性的"能走索引"的结论（本项目
		// 曾经在这一点上判断错误，见设计文档 §4.3 的记录）。前导的
		// time >= ? 让 SQLite 能先用索引定位到位点附近，代价从 O(位点
		// 之前的全部历史行数) 降到 O(新增行数)，且不需要额外索引。
		var logs []model.AccessLog
		err = adb.Model(&model.AccessLog{}).
			Where("time >= ? AND (time > ? OR id > ?)", cursor.LastLogTime, cursor.LastLogTime, cursor.LastLogId).
			Order("time asc, id asc").
			Limit(domainStatBatchSize).
			Find(&logs).Error
		if err != nil {
			return total, err
		}
		if len(logs) == 0 {
			// 空批次有两种截然不同的原因，但目前被编码成了同一个返回值
			// (total, nil)，调用方（DomainStatJob）只在 n > 0 时才打日志，
			// 二者从外部完全无法区分：
			//   1. 已追平——位点确实是当前最新的，下一批数据还没写进来，
			//      这是正常、每 10 分钟都会发生一次的情形。
			//   2. 位点跑到了未来——系统时钟一度超前（NTP 故障、
			//      `date -s` 打错、虚拟机快照回滚后再前进）时写下的日志
			//      把位点顶到了未来某个时刻，时钟校正回来之后，
			//      time >= 位点 从此永远不可能被满足（真实时间还没追上
			//      那个未来时刻），聚合永久停摆——且这次停摆无界（幅度
			//      等于时钟当时前跳的量，可能是几小时到几天）、没有任何
			//      一行日志、也不会自愈：AccessLogCleanupJob 的清理条件
			//      是 time < cutoff，永远删不到这些"来自未来"的行，它们
			//      会一直留在表里但永远读不到。TopDomains 用
			//      bucket_start >= since 圈定榜单窗口，这意味着故障期间
			//      访问过的域名会永久钉在每一个档位的榜单里（因为它们
			//      从未被移出"最近"的窗口——它们的桶起点本来就是未来）。
			//
			// 这里不做自愈（不下调位点）——前三轮反复证明，任何"自动把
			// 位点往回调"的尝试都会在另一个场景下引入虚高或漏数，见
			// DomainStatCursor 上方注释记录的 v1~v3 迭代史。只做侦测：
			// 位点时间明显超前于当前真实时间时记一条 Warning，把"没有
			// 任何一层会说话"的静默失败变成看得见的失败，剩下的交给
			// 人工处理（清空 domain_stat_cursors 那一行，代价是重新
			// 聚合一遍历史——这是需要人判断"值不值得"的操作，不适合
			// 程序自己替管理员做主）。
			//
			// 24 小时的容差覆盖两类良性抖动，不是任意选的：面板与 xray
			// 各自独立启动，重启窗口内若系统时区被改动，time.Local 是
			// 进程内缓存、两个进程在这段窗口里对同一时刻的本地时间解读
			// 可能整体错位最多 26 小时（不同时区偏移之差的极值）；再加上
			// 普通的 NTP 抖动，24 小时是一个远超正常抖动、但仍能及时报出
			// 真实故障的阈值。
			if cursor.LastLogTime > 0 {
				if future := time.UnixMilli(cursor.LastLogTime); future.Sub(time.Now()) > 24*time.Hour {
					logger.Warningf("域名统计位点的时间(%s)超前于当前系统时间超过 24 小时，聚合已经停止且不会自愈——这通常是系统时钟曾经跳变导致的（NTP 故障、误设系统时间、虚拟机快照），需要人工确认后清空 domain_stat_cursors 表里 id=1 的那一行以重新开始聚合", future.Format(time.RFC3339))
				}
			}
			return total, nil
		}

		// 先在内存里按 (粒度, 入站, 域名, 桶) 聚合，再逐条 UPSERT。
		// 同一轮里同一个键出现几百次是常态，不合并就是白写几百次。
		type key struct {
			g     model.TrafficGranularity
			id    int
			dom   string
			start int64
		}
		counts := make(map[key]int64, len(logs))
		for i := range logs {
			row := &logs[i]
			// inbound_id = 0 是写入时就没匹配上任何入站的记录（api 入站
			// 就是这样）；已被删除的入站同样跳过——它的桶马上要被清掉。
			if row.InboundId == 0 || !validId[row.InboundId] {
				continue
			}
			dom := domain.Registrable(row.Target)
			if dom == "" {
				continue
			}
			// AccessLog.Time 是毫秒，桶起点是 Unix 秒。
			at := time.UnixMilli(row.Time)
			counts[key{model.GranularityHour, row.InboundId, dom, model.AlignHour(at, loc)}]++
			counts[key{model.GranularityDay, row.InboundId, dom, model.AlignDay(at, loc)}]++
		}
		// logs 按 (time asc, id asc) 读出，最后一条天然就是这批里 (time, id)
		// 复合序最大的那条——不需要再遍历一遍取 max，这也是新方案比旧方案
		// （按 id asc 读出、却要把其中某一条的 Time 单独当 max 用）更可靠的
		// 地方：排序键与位点字段完全对应，不存在"假设 Time 与 id 同序"这类
		// 需要额外证明的前提。
		last := logs[len(logs)-1]

		// 整轮包进一个事务：GORM 的 SkipDefaultTransaction 默认为 false，
		// 不包的话每条 UPSERT 自带一次 BEGIN/COMMIT，几百次提交对 SQLite
		// 是几百次 fsync。位点的推进也在同一个事务里——先写桶后推位点，
		// 中途失败则整轮不写，下一轮从原位点重来，不会丢也不会重。
		err = tdb.Transaction(func(tx *gorm.DB) error {
			for k, c := range counts {
				if err := upsertDomainStat(tx, k.g, k.id, k.dom, k.start, c); err != nil {
					return err
				}
			}
			return saveDomainStatCursor(tx, last.Id, last.Time)
		})
		if err != nil {
			return total, err
		}
		total += len(logs)

		// 没读满说明已经追平，不必再跑一轮。
		if len(logs) < domainStatBatchSize {
			return total, nil
		}
	}
	return total, nil
}

// upsertDomainStat 把次数累加进目标桶，桶不存在时创建。
//
// DoUpdates 用 gorm.Expr 做累加而不是覆盖：同一个桶在一小时内会被多轮聚合
// 写到，覆盖会让每个桶只剩最后一轮的量。
func upsertDomainStat(db *gorm.DB, g model.TrafficGranularity, inboundId int, dom string, start, count int64) error {
	row := &model.DomainStat{
		Granularity: g, InboundId: inboundId, Domain: dom, BucketStart: start, Count: count,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "granularity"}, {Name: "inbound_id"}, {Name: "domain"}, {Name: "bucket_start"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"count": gorm.Expr("domain_stats.count + ?", count),
		}),
	}).Create(row).Error
}

// loadDomainStatCursor 读位点，没有行时返回零值（LastLogId/LastLogTime 均为
// 0，从头开始）。
func loadDomainStatCursor(db *gorm.DB) (model.DomainStatCursor, error) {
	var c model.DomainStatCursor
	err := db.Where("id = ?", 1).First(&c).Error
	if err == gorm.ErrRecordNotFound {
		return model.DomainStatCursor{Id: 1}, nil
	}
	if err != nil {
		return model.DomainStatCursor{}, err
	}
	return c, nil
}

// saveDomainStatCursor 把位点推进到 (lastLogId, lastLogTime)。位点以
// lastLogTime 为主序、lastLogId 只在同一毫秒内当次序用（见 Aggregate 里
// 查询条件上方的注释），两者必须一起写：只推 id 不更新 time，或只推 time
// 不更新 id，都会让下一次查询的 (time, id) 复合序出现缺口或矛盾。
func saveDomainStatCursor(db *gorm.DB, lastLogId, lastLogTime int64) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_log_id", "last_log_time"}),
	}).Create(&model.DomainStatCursor{Id: 1, LastLogId: lastLogId, LastLogTime: lastLogTime}).Error
}

// TopDomainRange 是榜单的时间档位。
type TopDomainRange string

const (
	TopRange1h  TopDomainRange = "1h"
	TopRange6h  TopDomainRange = "6h"
	TopRange12h TopDomainRange = "12h"
	TopRange24h TopDomainRange = "24h"
	TopRange7d  TopDomainRange = "7d"
	TopRange15d TopDomainRange = "15d"
)

// TopDomainRow 是榜单里的一行。
//
// Up/Down 在未计量时恒为 0，前端靠 TopDomainResult.Metered 决定是否显示这两列——
// 显示一列恒为 0 的「上传」会被当成「他没上传过」，比不显示更糟。
type TopDomainRow struct {
	Domain string `json:"domain"`
	// Kind 是 "domain" 或 "ip"，由 Domain 的形态推导，不落库。
	//
	// 不加数据库列：DomainStat.Domain 从第一期起就「IP 字面量原样」存，
	// 形态本身已经是完备的判据；加一列只会多出一处需要与推导保持一致的
	// 真相源，而它们一旦漂移，界面上的类型标签会和实际生成的规则形态对不上。
	// 推导用的 net.ParseIP 与 buildMeterRules 选规则形态用的是同一个判据。
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}

// topDomainKind 由目标的形态推导它的类型。
func topDomainKind(d string) string {
	if net.ParseIP(d) != nil {
		return "ip"
	}
	return "domain"
}

// TopDomainOrder 是榜单的排序维度。
type TopDomainOrder string

const (
	TopOrderCount TopDomainOrder = "count"
	TopOrderUp    TopDomainOrder = "up"
	TopOrderDown  TopDomainOrder = "down"
)

// normalizeTopOrder 把档位翻译成 (实际生效值, ORDER BY 子句)。
//
// 未知值回落 count——这是个展示接口，一个拼错的参数不该变成报错弹窗。
// 排序键末位一律用域名字典序兜底：数值相同时顺序抖动会让自动刷新的榜单
// 里的行无端跳动。
func normalizeTopOrder(o TopDomainOrder) (TopDomainOrder, string) {
	switch o {
	case TopOrderUp:
		return TopOrderUp, "up desc, domain asc"
	case TopOrderDown:
		return TopOrderDown, "down desc, domain asc"
	default:
		return TopOrderCount, "count desc, domain asc"
	}
}

// TopDomainCoverage 说明这份榜单覆盖了该入站多大比例的流量。
//
// 分子来自 DomainStat 的字节列（计量出站数出来的），分母来自 TrafficBucket
// （入站计数器数出来的）。两者同库、同粒度、同对齐、同一次 GetTraffic 采集，
// 可比性是结构性的。
//
// 它仍然是**近似值**：入站计的是客户端与面板之间的加密流，出站计的是面板与
// 目标之间的流，两者相差一层协议开销与握手，所以覆盖率结构性地小于 100%。
// UI 必须如实标注为「约」。
//
// 差额有三部分：被管理员自己的分流规则带走的流量、走默认出站但不在计量池里
// 的域名、以及上面那层协议开销。比例低时榜单不可信，比例高时榜单就是答案——
// 这是覆盖度唯一的用途。
type TopDomainCoverage struct {
	MeteredBytes int64 `json:"meteredBytes"`
	TotalBytes   int64 `json:"totalBytes"`
	// Ratio 为 nil 表示分母为 0，界面上整行不显示。显示 0% 会被理解成
	// 「一点都没归因到」，那是另一回事。
	Ratio *float64 `json:"ratio"`
}

// meterOverheadRatio 是协议封装开销的估算比例。
//
// 入站计数器量的是 VMess+WS+TLS 封装后的加密流，出站计数器量的是解封装后
// 的明文流，两者结构性地差 3~8%。取中值 5%。
//
// 做成常量而不是设置项：新增设置项要同步改 5 处（漏掉 models.js 那处会让
// 整个保存配置接口失败），而这个数只影响一行展示文字，不值得那个代价。
// 不同协议的封装开销差别不小（vless+vision+reality 与 vmess+ws+tls 不是
// 一个量级），所以 UI 上必须标明它是估算。
const meterOverheadRatio = 0.05

// TopDomainBreakdown 把总用量拆成有名字的几块，而不是让差额无声消失在
// 一句「约 0% 已归因」里——那句话除了让人以为系统坏了之外没有任何信息量。
//
// 只有 TotalBytes 与 AttributedBytes 是精确值（都直接来自计数器）；
// OverheadBytes 是按比例估的，UnattributedBytes 是减法余项。前端必须在
// 视觉上把估算值与精确值分开，否则整份数据的可信度会被那个估算拖下水。
type TopDomainBreakdown struct {
	TotalBytes      int64 `json:"totalBytes"`      // 入站计数器，精确
	AttributedBytes int64 `json:"attributedBytes"` // 计量出站合计，精确
	// BlockedConns 是被封禁的连接数。**字节数不可得**，理由见
	// AccessLogService.CountByRoute。这些字节已经计在 TotalBytes 里。
	BlockedConns      int64 `json:"blockedConns"`
	OverheadBytes     int64 `json:"overheadBytes"`     // 协议封装开销，估算
	UnattributedBytes int64 `json:"unattributedBytes"` // 余项，减法得出
}

// TopDomainResult 是榜单接口的返回体。
type TopDomainResult struct {
	// Metered 为 false 表示这批数据只有访问次数，没有字节数。判据是「该入站
	// 在 meter_domains 里至少有一行未在冷却」——池表就是生成端读的那张表，
	// 用它两侧永远一致；用「配置里真的有计量规则」做判据则要反推一次配置生成
	// 的结果，那条通路不存在，硬造只会多一个会与生成端漂移的真相源。
	Metered bool           `json:"metered"`
	Range   string         `json:"range"` // 实际生效的档位，前端据此回显
	OrderBy string         `json:"orderBy"`
	Limit   int            `json:"limit"`
	List    []TopDomainRow `json:"list"`
	// Coverage 在 Metered 为 false 时为 nil。
	Coverage *TopDomainCoverage `json:"coverage"`
	// Breakdown 同样在 Metered 为 false 时为 nil：没有字节数就无从分解。
	Breakdown *TopDomainBreakdown `json:"breakdown"`
}

// topRangeSpec 把档位翻译成（粒度, 回溯时长）。未知档位回落 24h——
// 这是个展示接口，一个拼错的参数不该变成报错弹窗。
func topRangeSpec(r TopDomainRange) (model.TrafficGranularity, time.Duration, TopDomainRange) {
	switch r {
	case TopRange1h:
		return model.GranularityHour, time.Hour, r
	case TopRange6h:
		return model.GranularityHour, 6 * time.Hour, r
	case TopRange12h:
		return model.GranularityHour, 12 * time.Hour, r
	case TopRange24h:
		return model.GranularityHour, 24 * time.Hour, r
	case TopRange7d:
		return model.GranularityDay, 7 * 24 * time.Hour, r
	case TopRange15d:
		return model.GranularityDay, 15 * 24 * time.Hour, r
	default:
		return model.GranularityHour, 24 * time.Hour, TopRange24h
	}
}

// TopDomains 返回某入站在给定档位内访问次数最多的域名。
//
// 校验入站存在性：不校验的话，一个不存在的入站 id 会返回一张空榜单，
// 管理员会把它理解成「这个人没访问过任何网站」，而不是「你查的这个入站
// 不存在」。
//
// 起点按面板时区对齐后回溯，用的是与用量图（TrafficHistoryService.buildSlots）
// 相同的 AlignHour/AlignDay 对齐函数，但覆盖范围并不与它一致——见下面
// Where 子句上的注释，这里的「24 小时」实际是 25 个小时桶，用量图的
// 「24 小时」是恰好 24 个，两者刻意不同，不要把这句话理解成整体行为一致。
// 不对齐的话，「最近 24 小时」的起点会落在某个小时的中间，而桶是整点的，
// 边界那一桶要么整个漏掉要么整个算进来，取决于当前分钟数——同一个查询
// 在一小时内会给出两种结果。
func (s *DomainStatService) TopDomains(
	inboundId int, r TopDomainRange, order TopDomainOrder, limit int, now time.Time,
) (*TopDomainResult, error) {
	inboundService := InboundService{}
	if _, err := inboundService.GetInbound(inboundId); err != nil {
		return nil, err
	}
	g, back, effective := topRangeSpec(r)
	effectiveOrder, orderClause := normalizeTopOrder(order)
	if limit <= 0 {
		limit = 10
	}
	result := &TopDomainResult{
		Range:   string(effective),
		OrderBy: string(effectiveOrder),
		Limit:   limit,
		List:    make([]TopDomainRow, 0, limit), // 不能给前端 null
	}
	db := database.GetTrafficDB()
	if db == nil {
		return result, nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	var since int64
	if g == model.GranularityHour {
		since = model.AlignHour(now.Add(-back), loc)
	} else {
		since = model.AlignDay(now.Add(-back), loc)
	}

	var rows []TopDomainRow
	err = db.Model(&model.DomainStat{}).
		Select("domain, sum(count) as count, sum(up) as up, sum(down) as down").
		// since 和桶起点一样落在对齐边界上，用 ">=" 会把 since 自身那一桶
		// 也算进来，所以「最近 1 小时」实际覆盖的是 60~120 分钟，不是精确
		// 的 60 分钟。这是刻意的取舍，不是疏忽：改成 ">" 更贴字面，但整点
		// 刚过时就只剩当前这一个几乎为空的桶——聚合任务 @every 10m，日志
		// 还来不及聚合进去——榜单会在每小时开头都短暂显示"无数据"。这里
		// 要的是排名，多覆盖半个桶几乎不改变谁在前面，覆盖不足却会让功能
		// 每小时必崩一次，两害相权取范围略宽的这个。这条不止影响 1h 档：
		// 所有档位都多覆盖一个桶，7d 实际是 8 个日桶、15d 是 16 个。
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Group("domain").
		Order(orderClause).
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for i := range rows {
			rows[i].Kind = topDomainKind(rows[i].Domain)
		}
		result.List = rows
	}

	metered, err := s.inboundIsMetered(db, inboundId, now)
	if err != nil {
		return nil, err
	}
	result.Metered = metered
	if metered {
		coverage, err := s.coverage(db, g, inboundId, since)
		if err != nil {
			return nil, err
		}
		result.Coverage = coverage
		result.Breakdown = s.breakdown(inboundId, since, coverage)
	}
	return result, nil
}

// inboundIsMetered 判断这个入站当前是否有域名正在被计量。
//
// 已知的不精确处：入站被停用时不生成计量规则，但池行还在（生成期刻意不删，
// 入站可能只是临时停用），于是这里仍会返回 true。后果只是给一个没有流量的
// 入站显示了字节列与 0% 覆盖率，可以接受；反过来把它做精确，就要在查询路径上
// 引入一次入站启用状态的判断，而那个状态与「历史上这段时间是否被计量过」
// 根本不是一回事——榜单查的是过去 15 天，入站是此刻的状态。
//
// 判据必须与 MeterPoolService.Pool 完全一致（cooldown_until = 0，理由见那里）：
// 两处一旦漂移，界面会为一个其实已经全部退场的入站显示字节列，而那些列恒为 0。
// now 因此不参与查询，保留参数是为了让签名与调用点的语义一致。
func (s *DomainStatService) inboundIsMetered(db *gorm.DB, inboundId int, now time.Time) (bool, error) {
	var n int64
	err := db.Model(&model.MeterDomain{}).
		Where("inbound_id = ? and cooldown_until = 0", inboundId).
		Count(&n).Error
	return n > 0, err
}

// coverage 算出这份榜单覆盖了该入站多大比例的流量。
func (s *DomainStatService) coverage(
	db *gorm.DB, g model.TrafficGranularity, inboundId int, since int64,
) (*TopDomainCoverage, error) {
	var metered struct{ Up, Down int64 }
	err := db.Model(&model.DomainStat{}).
		Select("coalesce(sum(up),0) as up, coalesce(sum(down),0) as down").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Scan(&metered).Error
	if err != nil {
		return nil, err
	}
	var total struct{ Up, Down int64 }
	err = db.Model(&model.TrafficBucket{}).
		Select("coalesce(sum(up),0) as up, coalesce(sum(down),0) as down").
		Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, since).
		Scan(&total).Error
	if err != nil {
		return nil, err
	}
	out := &TopDomainCoverage{
		MeteredBytes: metered.Up + metered.Down,
		TotalBytes:   total.Up + total.Down,
	}
	if out.TotalBytes > 0 {
		// 钳到 [0,1]：协议开销在极端情况下可能让比值越界，显示一个 103%
		// 或负数会让整块数据失去可信度。
		ratio := float64(out.MeteredBytes) / float64(out.TotalBytes)
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		out.Ratio = &ratio
	}
	return out, nil
}

// breakdown 由 coverage 已经算好的两个精确值再拆出估算项与余项。
//
// 不重新查库取总量与已归因：两处独立取数会在并发写入下给出对不上的两组
// 数字，而这块 UI 的全部意义就是「账要平」。
//
// 不返回 error：被封禁的连接数取不到不算失败（访问日志是独立库，它不可用时
// 分解的其余部分仍然成立），显示一个 0 比整块不显示要好。
func (s *DomainStatService) breakdown(
	inboundId int, since int64, coverage *TopDomainCoverage,
) *TopDomainBreakdown {
	out := &TopDomainBreakdown{
		TotalBytes:      coverage.TotalBytes,
		AttributedBytes: coverage.MeteredBytes,
	}
	if n, err := (&AccessLogService{}).CountByRoute(inboundId, model.BlockOutboundTag, since); err != nil {
		logger.Warning("差额分解取不到封禁连接数:", err)
	} else {
		out.BlockedConns = n
	}
	out.OverheadBytes = int64(float64(out.TotalBytes) * meterOverheadRatio)
	out.UnattributedBytes = out.TotalBytes - out.AttributedBytes - out.OverheadBytes
	if out.UnattributedBytes < 0 {
		// 口径差的方向并不固定（采集窗口错位也会造成已归因超过总量）。显示
		// 一个负的「未归因」会让整块数据当场失去可信度，所以钳到 0，并把
		// 溢出量让给开销那一项——它本来就是估算，吸收误差是它的职责。
		out.OverheadBytes = out.TotalBytes - out.AttributedBytes
		if out.OverheadBytes < 0 {
			out.OverheadBytes = 0
		}
		out.UnattributedBytes = 0
	}
	return out
}

// Cleanup 删除某一级中早于保留期的行，返回删除行数。
//
// 两级各有各的保留期，所以条件里必须带 granularity——不带的话，一次
// 「清理小时桶」会把同样早于该时刻的日桶一起删掉，长期榜单会静默变空。
func (s *DomainStatService) Cleanup(g model.TrafficGranularity, retentionDays int, now time.Time) (int64, error) {
	db := database.GetTrafficDB()
	if db == nil || retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	result := db.Where("granularity = ? and bucket_start < ?", g, cutoff).
		Delete(&model.DomainStat{})
	return result.RowsAffected, result.Error
}

// PruneOrphans 删除已不存在的入站遗留的行，返回删除行数。
//
// 第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，
// 那时引用不再悬空，榜单会渲染得非常合理，只是列的是别人访问过的网站。
func (s *DomainStatService) PruneOrphans() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.Inbound{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	tx := db.Where("inbound_id != 0")
	if len(ids) > 0 {
		tx = tx.Where("inbound_id not in ?", ids)
	}
	result := tx.Delete(&model.DomainStat{})
	return result.RowsAffected, result.Error
}

// DeleteByRule 删除某条分流规则的全部 B 类计量数据（两级都删）。
//
// 必须在删除规则时调用。SQLite 会复用被删除的自增 id，不删的话下一条新建
// 的规则会继承上一条的字节数，而且因为引用不再悬空，任何「跳过悬空引用」
// 式的防线都拦不住它。
func (s *DomainStatService) DeleteByRule(ruleId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("domain = ?", model.RuleStatKey(ruleId)).Delete(&model.DomainStat{}).Error
}

// PruneOrphanRules 清掉规则表里已不存在的规则遗留的 B 类计量行，返回行数。
//
// DeleteByRule 的兜底：面板崩溃在删规则与删计量之间、或直接改库删规则，
// 都会留下孤儿。挂在 TrafficCleanupJob 里每小时跑一次。
func (s *DomainStatService) PruneOrphanRules() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.RoutingRule{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, model.RuleStatKey(id))
	}
	tx := db.Where("domain like ?", "rule:%")
	if len(keys) > 0 {
		tx = tx.Where("domain not in ?", keys)
	}
	result := tx.Delete(&model.DomainStat{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部域名统计（两级都删）。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *DomainStatService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.DomainStat{}).Error
}
