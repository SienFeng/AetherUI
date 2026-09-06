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
// 位点主体用 access_log 的自增 id 而不是时间戳：AccessLogService.Query 本来
// 就依赖 id 定序保证翻页稳定，Where("id > 位点") 也比按时间窗重算简单直接。
// 但 id 不是自己就能说明一切——GORM 的 sqlite 驱动对 primaryKey;autoIncrement
// 生成的是裸 rowid 别名，没有 AUTOINCREMENT，新行的 id 恒为「当前表内
// max(rowid) + 1」：删除入站（AccessLogService.DeleteByInbound）、孤儿清理、
// 保留期清理都会删行，一旦删掉的行占据了当时最高的那批 id，后续新写入的行
// 就会复用比位点更小的 id，单靠 id 无法分辨一行现存的低位 id 记录，究竟是
// 删除前就已经聚合过的历史行，还是删除后落进同一 id 区间的全新数据。
//
// LastLogTime 就是为此而加的：记上次成功聚合那一批里最后一条记录的
// AccessLog.Time（毫秒，同单位）。Time 是写入时刻，只会随时间推进，不因
// 删除而倒退或复用，因此可以拿它问一个 id 回答不了的问题——「库里还有没有
// 比我上次聚合的那一刻更晚的记录，却没被 id > 位点 读到？」——
// DomainStatService.Aggregate 里的自愈逻辑正是靠这个判据检测位点是否失效
// 并回退重新聚合，见该函数注释；只看 max(id) 与位点比较这条已经证明过是
// 假前提（部分删除后又有新数据落回同一 id 区间的混合场景下，max(id) 可能
// 又被顶回位点以上，连"位点已失效"都判断不出来）。
//
// AutoMigrate 给已存在的位点行加这一列时，LastLogTime 会是零值——自愈逻辑
// 对「LastLogTime 为 0 但 LastLogId 已经 > 0」这个组合做了特判，当作位点
// 仍然有效，否则会把这个从未跑过新判据的旧位点误判成失效、回退到 0 重新
// 聚合已经聚合过的全部历史数据。
type DomainStatCursor struct {
	Id          int `gorm:"primaryKey"`
	LastLogId   int64
	LastLogTime int64
}
