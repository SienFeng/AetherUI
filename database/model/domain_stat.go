package model

// DomainStat 是某个入站在某个时间桶内、对某个注册域名的访问统计，
// 存在**独立的 SQLite 库**里（与 TrafficBucket 同库，见 database.InitTrafficDB）。
//
// 分库的理由与 TrafficBucket 相同：高频写入不该和面板的普通操作抢主库
// 那把 SQLite 写锁。
//
// Count 由第一期的访问日志聚合写入；Up/Down 留给第二期的出站计量填，
// 在此之前恒为 0。两期共用一张表，第一期就把时区对齐、清理、孤儿清除
// 一次做对，第二期只补两列。
type DomainStat struct {
	Id int64 `json:"-" gorm:"primaryKey;autoIncrement"`

	// 复用 TrafficBucket 那套粒度常量，不另定义一套——两张表的清理都要
	// 按它套各自的保留期。
	Granularity TrafficGranularity `json:"-" gorm:"uniqueIndex:idx_domain_stat,priority:1"`

	// InboundId 而不是 tag：入站 tag 是 inbound-<端口> 算出来的，用户改端口
	// tag 就变，存 tag 会让历史在改端口那一刻断掉。
	//
	// 相应地，删除入站时必须连带删掉它的行——SQLite 会复用被删除的自增 id，
	// 不删的话下一个建出来的入站会看到上一个用户访问过哪些网站，而且因为
	// 引用不再悬空，任何「跳过悬空引用」式的防线都拦不住它。
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_domain_stat,priority:2"`

	// BucketStart 是桶起始时刻的 Unix **秒**，按面板设置的时区对齐（AlignHour）。
	// 注意 AccessLog.Time 是**毫秒**，聚合时要转换。
	//
	// 排在 Domain 之前（而不是 TrafficBucket 那种「domain 在前」的直觉顺序）：
	// 榜单查询是 WHERE granularity=? AND inbound_id=? AND bucket_start>=? 再
	// GROUP BY domain，范围条件排第 3 位才能走索引前缀——1h 档的选择性是
	// 1/720，排在第 4 位时这个范围条件用不上索引，要扫掉该入站 30 天的
	// 全部小时行。代价是 GROUP BY domain 需要一棵临时 B 树（这个列序下
	// domain 不再自然有序），换来的扫描量节省远大于这棵临时树的开销。
	BucketStart int64 `json:"t" gorm:"uniqueIndex:idx_domain_stat,priority:3"`

	// Domain 是归并后的注册域名（util/domain.Registrable），IP 字面量原样。
	Domain string `json:"domain" gorm:"uniqueIndex:idx_domain_stat,priority:4"`

	Count int64 `json:"count"` // 连接次数
	Up    int64 `json:"up"`    // 上传字节，第二期填
	Down  int64 `json:"down"`  // 下载字节，第二期填
}

// DomainStatCursor 记「聚合任务上次读到访问日志的哪一条」，恒定单行（Id=1）。
//
// 不存进 settings：新增设置项要同步改 5 处（defaultValueMap / entity.AllSetting /
// entity.CheckValid / getter / models.js 的 AllSetting 构造函数），漏掉最后一处
// 会让整个保存配置接口失败，端口、证书路径一起遭殃。一个纯内部的位点不值得
// 付这个代价。
//
// 位点用 access_log 的自增 id 而不是时间戳：id 单调递增（AccessLogService.Query
// 本来就依赖这一点来保证翻页稳定），面板停机再久也只是补算，既不会重复计算
// 也不会跳过；按时间窗重算则需要「重算最近 N 小时」的启发式，停机超过 N 小时
// 就静默丢数据。
//
// 前提是 access_log 的自增 id 单调不回退——但 GORM 的 sqlite 驱动对
// primaryKey;autoIncrement 生成的是裸 rowid 别名，没有 AUTOINCREMENT，表被
// 清空后（保留期清理或手工删库）新行的 id 会从 1 重新开始。DomainStatService.
// Aggregate 里有一段自愈逻辑检测并纠正这种情形（见该函数注释），否则位点会
// 永久超前、聚合永久停摆。
type DomainStatCursor struct {
	Id        int `gorm:"primaryKey"`
	LastLogId int64
}
