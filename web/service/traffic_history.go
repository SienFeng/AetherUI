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
