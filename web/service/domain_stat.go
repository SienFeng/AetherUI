package service

import (
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
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
		var logs []model.AccessLog
		err = adb.Model(&model.AccessLog{}).
			Where("id > ?", cursor).
			Order("id asc").
			Limit(domainStatBatchSize).
			Find(&logs).Error
		if err != nil {
			return total, err
		}
		if len(logs) == 0 {
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
		maxId := cursor
		for i := range logs {
			row := &logs[i]
			if row.Id > maxId {
				maxId = row.Id
			}
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
			return saveDomainStatCursor(tx, maxId)
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

// loadDomainStatCursor 读位点，没有行时返回 0（从头开始）。
func loadDomainStatCursor(db *gorm.DB) (int64, error) {
	var c model.DomainStatCursor
	err := db.Where("id = ?", 1).First(&c).Error
	if err == gorm.ErrRecordNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return c.LastLogId, nil
}

func saveDomainStatCursor(db *gorm.DB, lastLogId int64) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_log_id"}),
	}).Create(&model.DomainStatCursor{Id: 1, LastLogId: lastLogId}).Error
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
// 起点按面板时区对齐后回溯，与用量图的刻度算法一致：不对齐的话，
// 「最近 24 小时」的起点会落在某个小时的中间，而桶是整点的，边界那一桶
// 要么整个漏掉要么整个算进来，取决于当前分钟数——同一个查询在一小时内
// 会给出两种结果。
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
		// 每小时必崩一次，两害相权取范围略宽的这个。
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
