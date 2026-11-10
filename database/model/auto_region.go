package model

// InboundAutoRegion 是「自动设置地区限制」对一个入站的处理状态，每入站一行。
//
// 没有行表示还在观察（或从未判定）。State 为 manual 表示管理员接手了：撤销过自动设置，
// 或者在编辑表单里改过地区。此后这个入站永不再被自动设置——自动化不能覆盖管理员的决定。
// adopted 表示入站在总开关打开时已经有地区限制，系统替它打开过一次自动出差放行。
//
// 存的是 inbound_id，删除入站时必须在同一个事务里删掉（SQLite 会复用 id）。
type InboundAutoRegion struct {
	InboundId int    `json:"inboundId" gorm:"primaryKey;autoIncrement:false"`
	State     string `json:"state"`
	Regions   string `json:"regions"` // JSON 省名数组：applied 时是写入的值
	// PrevRegions 是这次自动写入之前的地区（JSON 省名数组）。首次自动设置时为空；长住外地自动改地区时
	// 是原来的省——撤销要恢复它，过期页面把它写回来也要拦。
	PrevRegions string `json:"prevRegions"`
	Reason      string `json:"reason"`
	// RelocatedAt 是最近一次「长住外地、改去一个新省」的时刻（毫秒）；搬回原来的地区（PrevRegions）不刷新它。
	// 30 天内只自动改去一个新省：把账号一个接一个转给外省的人，每次都会被当成搬家。
	RelocatedAt int64 `json:"relocatedAt"`
	// AdoptedAt 是系统替这个入站打开自动出差放行的时刻（总开关接管、自动设置地区时一并打开；毫秒）。
	// 非 0 表示系统已经做过一次，之后管理员关掉就不再替他打开。
	AdoptedAt int64 `json:"adoptedAt"`
	// UpdatedAt 是毫秒。字段名叫 UpdatedAt，GORM 会在 Update/Updates 没带这一列时自动写入
	// 当前时刻，而 int64 默认按秒写——曾让一行变成「很久以前」，7 天内的「已自动设置」
	// 标签与过期写回的防护都失效。标成 milli 让自动写入与手写同一单位；只改原因、不该刷新
	// 时刻的地方用 UpdateColumn（它不触发自动写入）。
	UpdatedAt int64 `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

const (
	// AutoRegionPending 是上一期「等管理员确认」的状态。现在全部判断都自动做，不再产生；
	// 升级前留下的行每轮开头清掉。
	AutoRegionPending = "pending"
	AutoRegionApplied = "applied" // 已自动设置（首次设置，或长住外地自动改地区）
	AutoRegionManual  = "manual"  // 管理员接手，不再自动设置
	AutoRegionAdopted = "adopted" // 总开关打开时已有地区限制，系统替它打开过一次自动出差放行
)
