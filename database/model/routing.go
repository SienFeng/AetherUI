package model

// 分流规则的动作。
const (
	ActionProxy = "proxy"
	ActionBlock = "block"
)

// 生成的出站 tag 一律带此前缀，与用户手写模板的 tag 隔离。
const (
	OutboundTagPrefix = "a-ui"
	BlockOutboundTag  = "a-ui-block"
)

// DomainGroup 是一批可复用的域名集合。
type DomainGroup struct {
	Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Remark string `json:"remark" form:"remark"`
	// Domains 是 JSON 字符串数组，元素为 xray 原生域名语法：
	// domain:openai.com / full:chat.openai.com / geosite:openai / regexp:.*\.oaistatic\.com
	Domains string `json:"domains" form:"domains"`
}

// OutboundNode 是一个落地代理服务器。
type OutboundNode struct {
	Id       int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Tag      string `json:"tag" form:"tag" gorm:"unique"`
	Remark   string `json:"remark" form:"remark"`
	Protocol string `json:"protocol" form:"protocol"`
	// Config 是完整的 xray outbound JSON，tag 字段以本表的 Tag 为准。
	Config string `json:"config" form:"config"`
	Enable bool   `json:"enable" form:"enable"`
}

// RoutingRule 把「哪个入站访问哪个域名组」连到「哪个出站或黑洞」。
type RoutingRule struct {
	Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Remark string `json:"remark" form:"remark"`
	// InboundId 为 0 表示对所有入站生效。
	InboundId     int    `json:"inboundId" form:"inboundId"`
	DomainGroupId int    `json:"domainGroupId" form:"domainGroupId"`
	Action        string `json:"action" form:"action"`
	// OutboundId 仅在 Action 为 ActionProxy 时有意义。
	OutboundId int  `json:"outboundId" form:"outboundId"`
	Priority   int  `json:"priority" form:"priority"`
	Enable     bool `json:"enable" form:"enable"`
}
