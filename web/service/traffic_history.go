package service

import (
	"fmt"
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
	// xray 的 QueryStats 对每个已注册的计数器都会返回一行，包括值为 0 的——
	// 只要 xray 在跑，traffics 每轮都非空，一个完全空闲的面板同样每 10 秒
	// 收到一整份全零的增量。提前扫一遍，一条非零的入站增量都没有就直接
	// 返回，省掉下面这两步：GetTimeLocation（一次 settings 表查询 + 一次
	// time.LoadLocation——Go 不缓存后者，每次都要重新读 tzdata 文件）与
	// inboundTagToId（SELECT * FROM inbounds，取的是完整行，包含
	// settings/streamSettings/sniffing 这些 JSON 大字段）。一个空闲面板
	// 每天约 8640 轮（86400s / 10s），这两步不做的话就是白跑一次全表查询。
	hasInboundTraffic := false
	for _, t := range traffics {
		if t.IsInbound && (t.Up != 0 || t.Down != 0) {
			hasInboundTraffic = true
			break
		}
	}
	if !hasInboundTraffic {
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
	// 整个循环包进一个事务：GORM 的 SkipDefaultTransaction 默认是 false
	// （本项目从没设过它），意味着每一次 Create 都自带一个独立的
	// BEGIN...COMMIT。不包事务的话，N 个活跃入站每 10 秒就是 2N 次独立
	// 提交——SQLite 默认回滚日志下每次提交约 2 次 fsync，且与主库、访问
	// 日志库共享同一块盘，N=15 时约每轮 60 次、持续约 6 次/秒。成本在于
	// 提交次数而非语句数（设计文档 §4.3 只算了语句数）。包成一个事务后
	// 2N 次提交变 1 次，副作用是这一轮采集变成原子的：磁盘中途出错是
	// 整轮不写，而不是写了一半。
	return db.Transaction(func(tx *gorm.DB) error {
		for id, d := range deltas {
			if err := upsertBucket(tx, model.GranularityHour, id, hour, d.up, d.down); err != nil {
				return err
			}
			// 日桶独立累加，不由小时桶汇总而来：汇总方案要处理「小时桶已被
			// 清理但日桶还没算」的补算逻辑，独立累加天生免疫。日桶一年才
			// 365 行，多一次 UPSERT 的代价可以忽略。
			if err := upsertBucket(tx, model.GranularityDay, id, day, d.up, d.down); err != nil {
				return err
			}
		}
		return nil
	})
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

// TrafficRange 是用量图上的时间范围档位。
type TrafficRange string

const (
	Range24h TrafficRange = "24h"
	Range7d  TrafficRange = "7d"
	Range30d TrafficRange = "30d"
	Range1y  TrafficRange = "1y"
)

// trafficDBUnavailable 是库没打开时给界面的统一说明。「看不到」和「没有」
// 必须能被区分开——返回一张看起来正常的空图，管理员会以为这个人没用流量。
const trafficDBUnavailable = "用量历史库不可用，图表暂时无法显示。请检查面板日志与磁盘空间"

// TrafficPoint 是单入站图上的一个点。
type TrafficPoint struct {
	T    int64 `json:"t"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// TrafficSeries 是全局图上的一条线。Points 与 TrafficOverviewResult.Labels
// 等长，每个元素是该刻度上的 up+down。
type TrafficSeries struct {
	InboundId int     `json:"inboundId"`
	Remark    string  `json:"remark"`
	Points    []int64 `json:"points"`
}

type TrafficHistoryResult struct {
	Granularity string         `json:"granularity"`
	Labels      []string       `json:"labels"`
	Points      []TrafficPoint `json:"points"`
	Reason      string         `json:"reason"`
}

type TrafficOverviewResult struct {
	Granularity string          `json:"granularity"`
	Labels      []string        `json:"labels"`
	Series      []TrafficSeries `json:"series"`
	Reason      string          `json:"reason"`
}

// rangeSpec 把档位翻译成粒度与点数。认不出的档位回落到 24 小时：前端传错
// 时给一张能看的图，比报错或空图有用。
func rangeSpec(r TrafficRange) (model.TrafficGranularity, int) {
	switch r {
	case Range7d:
		return model.GranularityHour, 24 * 7
	case Range30d:
		return model.GranularityHour, 24 * 30
	case Range1y:
		return model.GranularityDay, 365
	default:
		return model.GranularityHour, 24
	}
}

func granularityName(g model.TrafficGranularity) string {
	if g == model.GranularityDay {
		return "day"
	}
	return "hour"
}

// buildSlots 生成范围内全部刻度的桶起点，升序，最后一个是当前所在的桶。
//
// 小时用算术递减（小时桶按定义就是对齐到整小时的，UTC 偏移含半小时的时区
// 也保持一致）；日必须用 AddDate 递减，因为一天不总是 86400 秒。
func buildSlots(g model.TrafficGranularity, now time.Time, loc *time.Location, count int) []int64 {
	slots := make([]int64, count)
	if g == model.GranularityDay {
		day := time.Unix(model.AlignDay(now, loc), 0).In(loc)
		for i := count - 1; i >= 0; i-- {
			slots[i] = day.Unix()
			day = day.AddDate(0, 0, -1)
		}
		return slots
	}
	end := model.AlignHour(now, loc)
	for i := 0; i < count; i++ {
		slots[i] = end - int64(count-1-i)*3600
	}
	return slots
}

// formatLabels 在服务端把刻度格式化成 x 轴文字。放在服务端是因为时区也在
// 服务端：让前端拿时间戳自己格式化，浏览器所在时区一变，图上的时间就和
// 面板设置的时区对不上了。
func formatLabels(g model.TrafficGranularity, slots []int64, loc *time.Location) []string {
	layout := "01-02 15:00"
	if g == model.GranularityDay {
		layout = "2006-01-02"
	}
	labels := make([]string, len(slots))
	for i, s := range slots {
		labels[i] = time.Unix(s, 0).In(loc).Format(layout)
	}
	return labels
}

// History 返回单个入站在指定范围内的分时用量，刻度稠密（缺失的桶补零）。
func (s *TrafficHistoryService) History(inboundId int, r TrafficRange, now time.Time) (*TrafficHistoryResult, error) {
	g, count := rangeSpec(r)
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	slots := buildSlots(g, now, loc, count)
	result := &TrafficHistoryResult{
		Granularity: granularityName(g),
		Labels:      formatLabels(g, slots, loc),
		Points:      make([]TrafficPoint, count),
	}
	for i, start := range slots {
		result.Points[i] = TrafficPoint{T: start}
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}
	var rows []model.TrafficBucket
	err = db.Where("granularity = ? and inbound_id = ? and bucket_start >= ?", g, inboundId, slots[0]).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	index := make(map[int64]int, len(slots))
	for i, start := range slots {
		index[start] = i
	}
	for _, row := range rows {
		if i, ok := index[row.BucketStart]; ok {
			result.Points[i].Up = row.Up
			result.Points[i].Down = row.Down
		}
	}
	return result, nil
}

// Overview 返回范围内用量最大的前 topN 个入站的分时曲线，按总量降序。
func (s *TrafficHistoryService) Overview(r TrafficRange, topN int, now time.Time) (*TrafficOverviewResult, error) {
	g, count := rangeSpec(r)
	loc, err := s.settingService.GetTimeLocation()
	if err != nil {
		return nil, err
	}
	slots := buildSlots(g, now, loc, count)
	result := &TrafficOverviewResult{
		Granularity: granularityName(g),
		Labels:      formatLabels(g, slots, loc),
		Series:      []TrafficSeries{},
	}

	db := database.GetTrafficDB()
	if db == nil {
		result.Reason = trafficDBUnavailable
		return result, nil
	}
	if topN <= 0 {
		topN = 12
	}

	// 先只算出 Top N 的 id，再取这几个的明细。一次把范围内所有行拉进内存
	// 也能算，但入站变多之后那是一个没有上限的读取量。
	type topRow struct {
		InboundId int
		Total     int64
	}
	var tops []topRow
	err = db.Model(&model.TrafficBucket{}).
		Select("inbound_id, sum(up + down) as total").
		Where("granularity = ? and bucket_start >= ?", g, slots[0]).
		Group("inbound_id").
		// 次级排序按 id：总量相同时顺序也要稳定，否则每次刷新图例都在跳。
		Order("total desc, inbound_id asc").
		Limit(topN).
		Scan(&tops).Error
	if err != nil {
		return nil, err
	}
	if len(tops) == 0 {
		return result, nil
	}

	ids := make([]int, 0, len(tops))
	for _, t := range tops {
		ids = append(ids, t.InboundId)
	}

	var inbounds []*model.Inbound
	if err := database.GetDB().Model(model.Inbound{}).Where("id in ?", ids).Find(&inbounds).Error; err != nil {
		return nil, err
	}
	remarks := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		remarks[in.Id] = in.Remark
	}

	var rows []model.TrafficBucket
	err = db.Where("granularity = ? and bucket_start >= ? and inbound_id in ?", g, slots[0], ids).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	index := make(map[int64]int, len(slots))
	for i, start := range slots {
		index[start] = i
	}
	byInbound := make(map[int][]int64, len(ids))
	for _, id := range ids {
		byInbound[id] = make([]int64, count)
	}
	for _, row := range rows {
		points, ok := byInbound[row.InboundId]
		if !ok {
			continue
		}
		if i, ok := index[row.BucketStart]; ok {
			points[i] = row.Up + row.Down
		}
	}

	// 按 tops 的顺序输出，Top N 的排序结果就是图例的顺序。
	for _, t := range tops {
		remark := remarks[t.InboundId]
		if remark == "" {
			// 图例上留一个空标签，管理员分不出这条线是谁的。
			remark = fmt.Sprintf("#%d", t.InboundId)
		}
		result.Series = append(result.Series, TrafficSeries{
			InboundId: t.InboundId,
			Remark:    remark,
			Points:    byInbound[t.InboundId],
		})
	}
	return result, nil
}
