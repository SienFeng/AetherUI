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

// IsReservedTag 判定一个 tag 是否由注入器自己发出，因而不能分配给出站节点。
//
// 保留 tag 不在 outbound_nodes 表里，数据库的唯一约束管不到它们：备注写成
// 「block」（含 Block/BLOCK/block!/" block "，SlugRemark 会把它们归一到同一个
// slug）会让 SuggestTag 生成 a-ui-block，与注入器始终注入的黑洞出站撞名，
// xray 报 "existing tag found" 并拒绝启动——全员断网，而面板首页仍显示 running。
//
// 三个消费点都只认这一个判定，将来新增保留 tag 只需改这里：
// 分配端 OutboundNodeService.allocTag（不分配出去）、
// 生成端 RoutingInjector.buildOutbounds（修复前的脏数据不写进配置）、
// 校验端 removeOutboundByTag（校验时绝不把注入器的黑洞出站当成旧版本摘掉）。
func IsReservedTag(tag string) bool {
	return tag == BlockOutboundTag
}

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
