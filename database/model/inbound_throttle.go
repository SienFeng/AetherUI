package model

// InboundThrottle 是「流量暴涨自动限速」：到 Until（毫秒）为止把这个入站的上下行限到 Mbps。
//
// 每个入站最多一行。到期（当地当天结束）或管理员关掉开关时由 SpikeThrottleService 删除，限速随之撤掉
// ——ShapingService.desiredLimits 只认还没到期的行。按入站 id 存：DelInbound 在同一个事务里删，
// 新建入站时 clearInboundLeftovers 再清一遍（SQLite 会复用被删除的自增 id）。
type InboundThrottle struct {
	InboundId int   `json:"inboundId" gorm:"primaryKey;autoIncrement:false"`
	Mbps      int   `json:"mbps"`
	Until     int64 `json:"until"`
	// TodayBytes / Baseline 是发这张单时的当天用量与近 14 天日常用量（中位数），给界面说明「为什么限速」。
	TodayBytes int64 `json:"todayBytes"`
	Baseline   int64 `json:"baseline"`
	CreatedAt  int64 `json:"createdAt"`
}
