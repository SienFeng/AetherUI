package model

// InboundRegionPass 是一张出差通行证：在入站存储的地区限制之外，临时多允许
// 几个省份，或者暂停地区限制（Regions 为 ["*"]）。
//
// 单独建表而不是加在 Inbound 上：UpdateInbound 用编辑表单提交的值整行写回，
// 通行证若在 Inbound 表上，一个打开较早的编辑表单就能把刚发的通行证清零，或把
// 刚到期清掉的复活，而界面上毫无提示。
//
// 「活动」只看 EndedAt == 0，不看 Until：配置生成期不能读时间（生成必须是库状态
// 的纯函数，否则 Config.Equals 会随时钟变化），到期由 CheckInboundJob 改库完成。
//
// 存的是 inbound_id 外键，而 SQLite 会复用被删除的自增 id，因此删除入站时必须在
// 同一个事务里连带删除（InboundService.DelInbound），否则下一个建出来的入站会
// 继承上一个用户的通行证。
type InboundRegionPass struct {
	Id        int    `json:"id" gorm:"primaryKey;autoIncrement"`
	InboundId int    `json:"inboundId" gorm:"index"`
	Regions   string `json:"regions"` // JSON 省名数组；["*"] 表示暂停地区限制
	Auto      bool   `json:"auto"`    // 由自动出差放行发放
	GrantedAt int64  `json:"grantedAt"`
	Until     int64  `json:"until"`
	EndedAt   int64  `json:"endedAt"` // 0 表示仍在生效
	EndReason string `json:"endReason"`
}

// RegionPassAll 是「暂停地区限制」通行证的唯一元素。
const RegionPassAll = "*"

// 通行证的结束原因。
const (
	RegionPassExpired   = "expired"
	RegionPassRevoked   = "revoked"
	RegionPassReplaced  = "replaced"
	RegionPassHome      = "home"      // 自动通行证：家里恢复使用
	RegionPassCoexist   = "coexist"   // 自动通行证：家里与外地同时在用
	RegionPassCleared   = "cleared"   // 管理员在编辑里清空了地区限制
	RegionPassAutoOff   = "auto_off"  // 管理员关掉了入站的自动出差放行（只结束自动通行证）
	RegionPassRelocated = "relocated" // 长住外地，地区已改成这个省，通行证不再需要
)
