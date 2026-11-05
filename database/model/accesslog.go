package model

// AccessLog 是一条访问记录，存在**独立的 SQLite 库**里（见 database.InitAccessLogDB）。
//
// 分库的理由：访问日志是高频写入，而 SQLite 一个库只有一把写锁。混在主库里
// 会让面板的每一次普通操作都去和日志写入抢锁；保留期清理的大批量 DELETE
// 也会拖住主库。
type AccessLog struct {
	Id   int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	Time int64 `json:"time" gorm:"index"` // 毫秒

	// InboundId 在写入时由 tag 解析得到，0 表示当时找不到对应入站。
	// 查询按 id 而不是 tag：入站改端口会让 tag 变化，按 tag 查会丢历史。
	// 相应地，删除入站时必须连带删掉它的日志——SQLite 会复用自增 id，
	// 不删的话新建的入站会看到上一个用户的访问记录。
	InboundId  int    `json:"inboundId" gorm:"index"`
	InboundTag string `json:"inboundTag"`

	SourceIP string `json:"sourceIp" gorm:"index"`
	Network  string `json:"network"`
	Target   string `json:"target"`
	Route    string `json:"route"`
	Accepted bool   `json:"accepted"`

	// NoRuleHit 为真表示 xray 记的是 " >> "：核心没有任何路由规则命中，走了默认出站。
	// Target 是客户端请求的目标，核心分流看的却是嗅探出的域名，两者可能不同；流量走向
	// 验证靠这一列区分「规则走错」与「核心实际看的是另一个域名」，见 accesslog.Entry。
	// 升级前写入的行这一列为 false，按原口径处理。
	NoRuleHit bool `json:"noRuleHit" gorm:"default:false"`
}

// AccessLogCursor 是 xray 访问日志文件（bin/access.log）的读取进度，单行，Id 恒为 1。
// 与访问日志放在同一个独立库里，和它们在同一个事务里提交：只有日志落库了进度才前进。
//
// 修复前进度只在内存里，面板每次重启都从文件开头重新导入一遍，库里出现大批重复行
// （a-ui.sh 的 geo 定时更新每两天 04:00 会重启一次服务）。
type AccessLogCursor struct {
	Id int `gorm:"primaryKey"`
	// Offset 是下一次从文件的哪个字节开始读。
	Offset int64
	// Head 是读到 Offset 时文件的第一条完整行，用作文件身份：重启时首行对不上，说明文件
	// 在期间被截断或替换过，Offset 指向的已是另一份内容，必须从头读。
	Head string
}
