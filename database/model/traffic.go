package model

import "time"

// TrafficGranularity 是用量桶的粒度。两级共用一张表，清理时按它套不同的
// 保留期：小时桶供近期看细节，日桶供长期看趋势。
type TrafficGranularity int8

const (
	GranularityHour TrafficGranularity = 1
	GranularityDay  TrafficGranularity = 2
)

// TrafficBucket 是某个入站在某个时间桶内的用量，存在**独立的 SQLite 库**里
//（见 database.InitTrafficDB）。
//
// 分库的理由与 AccessLog 相同：这张表每 10 秒写一次，清理时又是大批量
// DELETE，而 SQLite 一个库只有一把写锁——混在主库里会让面板的每一次普通
// 操作都去和它抢锁。
type TrafficBucket struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	Granularity TrafficGranularity `json:"-" gorm:"uniqueIndex:idx_traffic_bucket,priority:1"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变，存 tag 会让历史在改端口那一刻断掉。
	//
	// 相应地，删除入站时必须连带删掉它的桶——SQLite 会复用被删除的自增 id，
	// 不删的话下一个建出来的入站会看到上一个用户的曲线，而且因为引用不再
	// 悬空，任何「跳过悬空引用」式的防线都拦不住它。
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_traffic_bucket,priority:2"`

	// BucketStart 是桶起始时刻的 Unix 秒，按面板设置的时区对齐（见 AlignHour）。
	BucketStart int64 `json:"t" gorm:"uniqueIndex:idx_traffic_bucket,priority:3"`

	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// AlignHour 把时刻对齐到它所在小时的起点，返回 Unix 秒。
//
// 用面板设置的时区而不是 UTC。日桶尤其敏感：UTC+8 的管理员按 UTC 切日，
// 看到的「9 月 4 日用量」装的其实是 9 月 3 日 08:00 到 9 月 4 日 08:00 的
// 流量。这类错误不会报错，只会让人根据错的数据做判断。
func AlignHour(t time.Time, loc *time.Location) int64 {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, loc).Unix()
}

// AlignDay 把时刻对齐到它所在日的起点（当地 00:00:00），返回 Unix 秒。
func AlignDay(t time.Time, loc *time.Location) int64 {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc).Unix()
}
