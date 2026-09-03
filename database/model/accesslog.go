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
}
