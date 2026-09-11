package model

// InboundRiskSnapshot 是某入站最近一次共享风险评估的结果，存在**独立的
// SQLite 库**里（与 InboundIPHour 同库，见 database.InitTrafficDB）。
//
// 放 traffic 库而不是主库：它是 InboundIPHour 的纯派生物。traffic 库丢了，
// 风险分就该跟着失效——留着一份算不出出处的分数比没有更糟。
//
// 存的是 inbound_id 主键，而 SQLite 会复用被删除的自增 id，因此入站被删除时
// 必须连带清掉它的 Snapshot（InboundService.DelInbound），否则下一个建出来的
// 入站会继承上一个用户的风险分——而这次继承来的还是一个带「严重」字样的
// 红色标记。另有 TrafficCleanupJob 里的 PruneOrphans 兜底。
type InboundRiskSnapshot struct {
	InboundId int `json:"inboundId" gorm:"primaryKey"`

	// EngineVersion 是产出这份结果的评分引擎版本。与当前版本不匹配时
	// **一律当作「尚未评估」处理，绝不显示旧分数**——权重改过之后，旧分数
	// 与新证据对不上，而管理员没有任何办法看出这个数是哪一版算的。
	EngineVersion int `json:"engineVersion"`

	Score      int `json:"score"`
	Confidence int `json:"confidence"`

	// Level 只在 State == ready 时非空。learning / degraded / unobservable
	// 三种状态一律不映射成 low——把「测不了」「还没测够」显示成「低风险」，
	// 是这个功能要防的首要错误。
	Level string `json:"level"`
	State string `json:"state"`

	// Reason 是 unobservable 时的人话原因，直接取自 countabilityOf，
	// 与在线明细列显示的是同一句，两处口径天然一致。
	Reason string `json:"reason"`

	// GeoCoverage 是有效行里能解析出国家的比例（0~100）。低于门槛即 degraded。
	GeoCoverage int `json:"geoCoverage"`

	AnalysisStart int64 `json:"analysisStart"`
	LastDataHour  int64 `json:"lastDataHour"`
	EvaluatedAt   int64 `json:"evaluatedAt"`

	// EvidenceJSON 是风险依据列表。**刻意不存画像列表**：那个只有明细页
	// 需要，而明细是单入站按需请求，现场算比给每一行 Snapshot 塞一个大
	// JSON 划算，拿到的还更新。
	EvidenceJSON string `json:"-"`
}
