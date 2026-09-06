package model

import "time"

// InboundIPHour 是某入站的某个来源 IP 在某个 UTC 整点小时内的活跃时长，
// 存在**独立的 SQLite 库**里（与 TrafficBucket 同库，见 database.InitTrafficDB）。
//
// 分库理由与 TrafficBucket 相同：这张表每 30 秒写一批，清理时又是大批量
// DELETE，而 SQLite 一个库只有一把写锁——混在主库里会让面板的每一次普通
// 操作都去和它抢锁。
//
// 它只有一个消费者：判定「同一小时内，不同省份的 IP 是否在同时使用这个
// 入站」。这个「并存」判据是区分「用户旅游」（位置迁移，不并存）与「节点
// 被转卖」（位置并存）的唯一可靠信号，见设计文档 §1。
type InboundIPHour struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变。相应地，删除入站时必须连带删掉这些行——SQLite 会复用被删除
	// 的自增 id，不删的话下一个建出来的入站会继承上一个用户的并存记录，
	// 而且因为引用不再悬空，任何「跳过悬空引用」式的防线都拦不住它。
	InboundId int    `json:"inboundId" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:1"`
	IP        string `json:"ip" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:2"`
	HourStart int64  `json:"t" gorm:"uniqueIndex:idx_inbound_ip_hour,priority:3"`

	// Province 是主判定省份，空串表示归属地未知（IPv6 来源、归属地库未加载、
	// 或库中查无此段）。空串的行照常入库：IP 维度的并存信息仍有价值，只是
	// 判定会降级成 IP 口径（见 service.computeCoexist）。
	Province string `json:"province"`

	ActiveSeconds int `json:"activeSeconds"`
}

// AlignHourUTC 把时刻对齐到它所在 UTC 小时的起点，返回 Unix 秒。
//
// 刻意**不用**面板时区，与 AlignHour 相反。这张表唯一的消费者是「同一
// 小时内是否并存」，该判定只关心两条记录落不落进同一个桶，桶的绝对位置
// 无关——UTC 与本地时区在此完全等价。既然等价，就不该背上 TrafficBucket
// 那个包袱：按本地时区对齐时，管理员改一次时区会让旧桶与重算出的新刻度
// 不相交，历史整段消失。展示时再按面板时区格式化标签即可。
func AlignHourUTC(t time.Time) int64 {
	return t.UTC().Truncate(time.Hour).Unix()
}
