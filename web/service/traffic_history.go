package service

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// TrafficHistoryService 负责用量历史的采集、清理与查询。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type TrafficHistoryService struct {
	settingService SettingService
}

// Record 把一轮流量增量写进用量历史库。
//
// 传进来的是 XrayTrafficJob 每 10 秒从 xray gRPC Stats 取回的**增量**
//（reset=true，取完 xray 侧清零），因此恒为非负：xray 重启后计数从 0
// 重新开始，不会出现负值。这也是不走「读累计值做差分」那条路的原因——
// 差分方案里，一次正常的「重置流量」和一次数据损坏长得一模一样。
//
// 库不可用时静默返回 nil：图表不可用不该让调用方出错，而调用方
//（InboundService.AddTraffic）承担的是计费用的累计流量。
func (s *TrafficHistoryService) Record(traffics []*xray.Traffic, now time.Time) error {
	db := database.GetTrafficDB()
	if db == nil || len(traffics) == 0 {
		return nil
	}
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return err
	}
	tagToId, err := inboundTagToId()
	if err != nil {
		return err
	}

	// 先按入站聚合。同一轮里同一个 tag 出现多次时应当相加，而不是发两次
	// UPSERT——结果虽然一样，但白白多一倍写入。
	type delta struct{ up, down int64 }
	deltas := make(map[int]*delta, len(traffics))
	for _, t := range traffics {
		if !t.IsInbound {
			continue
		}
		// 零增量不写行：挂机用户大部分小时没有任何流量，跳过它们能砍掉
		// 一多半行数。图上的 0 由前端补零画出来，而补零逻辑无论如何都要
		// 有——新建的入站在它存在之前同样没有行。
		if t.Up == 0 && t.Down == 0 {
			continue
		}
		// 找不到对应入站的 tag（模板里的 api 入站、已删除的入站）直接丢弃。
		// 落成 inbound_id=0 只会在图上多出一条没人认领的曲线。
		id, ok := tagToId[t.Tag]
		if !ok || id == 0 {
			continue
		}
		d := deltas[id]
		if d == nil {
			d = &delta{}
			deltas[id] = d
		}
		d.up += t.Up
		d.down += t.Down
	}
	if len(deltas) == 0 {
		return nil
	}

	hour := model.AlignHour(now, loc)
	day := model.AlignDay(now, loc)
	for id, d := range deltas {
		if err := upsertBucket(db, model.GranularityHour, id, hour, d.up, d.down); err != nil {
			return err
		}
		// 日桶独立累加，不由小时桶汇总而来：汇总方案要处理「小时桶已被
		// 清理但日桶还没算」的补算逻辑，独立累加天生免疫。日桶一年才
		// 365 行，多一次 UPSERT 的代价可以忽略。
		if err := upsertBucket(db, model.GranularityDay, id, day, d.up, d.down); err != nil {
			return err
		}
	}
	return nil
}

// upsertBucket 把增量累加进目标桶，桶不存在时创建。
//
// DoUpdates 用 gorm.Expr 做累加而不是 clause.AssignmentColumns（那是覆盖）：
// 同一个桶一小时会被写 360 次，覆盖会让每个桶只剩最后 10 秒的量。
func upsertBucket(db *gorm.DB, g model.TrafficGranularity, inboundId int, start, up, down int64) error {
	bucket := &model.TrafficBucket{
		Granularity: g, InboundId: inboundId, BucketStart: start, Up: up, Down: down,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "granularity"}, {Name: "inbound_id"}, {Name: "bucket_start"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"up":   gorm.Expr("traffic_buckets.up + ?", up),
			"down": gorm.Expr("traffic_buckets.down + ?", down),
		}),
	}).Create(bucket).Error
}

// Cleanup 删除某一级中早于保留期的桶，返回删除行数。
//
// 两级各有各的保留期，所以条件里必须带 granularity——不带的话，一次
// 「清理小时桶」会把同样早于该时刻的日桶一起删掉，长期趋势图会静默变空。
func (s *TrafficHistoryService) Cleanup(g model.TrafficGranularity, retentionDays int, now time.Time) (int64, error) {
	db := database.GetTrafficDB()
	if db == nil || retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	result := db.Where("granularity = ? and bucket_start < ?", g, cutoff).
		Delete(&model.TrafficBucket{})
	return result.RowsAffected, result.Error
}

// PruneOrphans 删除已不存在的入站遗留的桶，返回删除行数。
//
// 这是第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。第一道在
// DelInbound 内。两道都要有：SQLite 会复用被删除的自增 id，残留的桶会绑到
// 下一个建出来的入站上，那时引用不再悬空，图会渲染得非常合理，只是画的是
// 别人的数据。
func (s *TrafficHistoryService) PruneOrphans() (int64, error) {
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
	result := tx.Delete(&model.TrafficBucket{})
	return result.RowsAffected, result.Error
}

// DeleteByInbound 删除某入站的全部用量历史（两级都删）。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *TrafficHistoryService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.TrafficBucket{}).Error
}
