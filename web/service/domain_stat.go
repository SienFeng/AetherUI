package service

import (
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
// Up/Down 在第一期恒为 0，前端靠 TopDomainResult.Metered 决定是否显示这两列——
// 显示一列恒为 0 的「上传」会被当成「他没上传过」，比不显示更糟。
type TopDomainRow struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
}

// TopDomainResult 是榜单接口的返回体。
type TopDomainResult struct {
	// Metered 为 false 表示这批数据只有访问次数，没有字节数。第二期上线后
	// 才为 true。
	Metered bool           `json:"metered"`
	Range   string         `json:"range"` // 实际生效的档位，前端据此回显
	Limit   int            `json:"limit"`
	List    []TopDomainRow `json:"list"`
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
func (s *DomainStatService) TopDomains(inboundId int, r TopDomainRange, limit int, now time.Time) (*TopDomainResult, error) {
	inboundService := InboundService{}
	if _, err := inboundService.GetInbound(inboundId); err != nil {
		return nil, err
	}
	g, back, effective := topRangeSpec(r)
	if limit <= 0 {
		limit = 10
	}
	result := &TopDomainResult{
		Metered: false, // 第二期上线后改为真实的计量状态
		Range:   string(effective),
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
		// 次数相同时按域名字典序兜底，让同一份数据每次返回的顺序一致——
		// 顺序抖动会让自动刷新时榜单里的行无端跳动。
		Order("count desc, domain asc").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows != nil {
		result.List = rows
	}
	return result, nil
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
