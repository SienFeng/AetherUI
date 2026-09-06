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
// 位点是 (LastLogTime, LastLogId) 的复合序，**LastLogTime 是主序，LastLogId
// 只用来打破同一毫秒内多条记录的并列**——这个顺序不能反。
//
// access_log 的自增 id 是可复用的 rowid：GORM 的 sqlite 驱动对
// primaryKey;autoIncrement 生成的是裸 rowid 别名，没有 AUTOINCREMENT，新行的
// id 恒为「当前表内 max(rowid) + 1」。删除入站（AccessLogService.
// DeleteByInbound）、孤儿清理、保留期清理都会删行，一旦删掉的行占据了当时
// 最高的那批 id（甚至表被整个清空），后续新写入的行就会复用比旧位点更小的
// id——这意味着单靠 id 无法把它当成一个只增不减的序列来定位点：本项目在
// 这条路上先后试过「id 单调递增」「失效必然表现为 id>位点 读到空批次、此时
// 归零重来」「同样在空批次时对齐到 max(id)」三版实现，都被真实复现的反例
// 推翻——最后一版的反例最关键：删除入站腾出高位 id 之后，如果之后一段时间
// 内新写入的行数超过被释放的 id 数，新行的 id 会重新越过旧位点，
// Where(id > 位点) 会直接读到非空批次，连「位点已经失效」这件事都不会被
// 触发检测，这批新数据就此永久跳过且没有任何日志。
//
// AccessLog.Time 则没有这个问题：它是写入时刻，删除任何行都不会改变其余
// 行的 Time。以 Time 为位点主序之后，「id 是否失效」这个问题本身就不再
// 存在——查询条件变成 Where(time > 位点时间 OR (time = 位点时间 AND
// id > 位点id))，新行不管实际拿到的 id 是多少，只要写入时刻在位点之后就
// 会被读到，不需要任何自愈或回退逻辑；LastLogId 退化成只在同一毫秒内排序
// 用的次要字段（同一毫秒内 id 复用的概率可以忽略）。
//
// 残留代价（刻意接受，不做补偿）：系统时钟回拨（NTP 步进、DST 秋季回拨，
// 每年一次）期间写入的日志，其 Time 小于当时的位点时间，会被这个查询条件
// 永久跳过。后果是榜单少一段数据，方向是**漏数而不是重复计数**，落在安全
// 侧——这与本文件其它地方「宁可漏读也不能重复计入」的取舍一致。
//
// 迁移：AutoMigrate 给已存在的位点行加 LastLogTime 这一列时，该列是零值，
// 但 LastLogId 已经是一个真实的历史位点。DomainStatService.Aggregate 检测
// 到「LastLogTime 为 0 且 LastLogId > 0」这个只可能来自升级的组合时，把位点
// 对齐到当前 access_log 里 (time, id) 最大的那一行，跳过升级前的全部积压——
// 宁可欠一段历史数据，也不能让 time > 0 命中全表、把升级前已经聚合过的
// 数据重新聚合一遍。
type DomainStatCursor struct {
	Id          int `gorm:"primaryKey"`
	LastLogId   int64
	LastLogTime int64
}
