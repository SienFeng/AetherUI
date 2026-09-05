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
	// DefaultOutboundTag 补给模板里那个没有 tag 的首个出站（xray 的默认出站）。
	// xray 只在出站带 tag 时才往访问日志写 "[入站 -> 出站]"，裸出站会让所有
	// 走直连的记录不带方括号，被 accesslog.ParseLine 当作无法归属的行丢弃。
	DefaultOutboundTag = "a-ui-default"
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
	return tag == BlockOutboundTag || tag == DefaultOutboundTag
}

// DomainGroup 是一批可复用的域名集合。
type DomainGroup struct {
	Id     int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Remark string `json:"remark" form:"remark"`
	// Domains 是管理员手工录入的域名，JSON 字符串数组，元素为 xray 原生域名语法：
	// domain:openai.com / full:chat.openai.com / geosite:openai / regexp:.*\.oaistatic\.com
	Domains string `json:"domains" form:"domains"`

	// SubscribeUrl 为空表示这个组不订阅，行为与本功能上线前完全一致。
	SubscribeUrl string `json:"subscribeUrl" form:"subscribeUrl"`
	// SubscribedDomains 是上一次成功拉取并解析出的域名，JSON 字符串数组。
	// 与 Domains 物理隔离：订阅更新绝不覆盖管理员手工补的条目，
	// 两个字段各自只有一个写入方，永不交叉。
	SubscribedDomains string `json:"subscribedDomains" form:"subscribedDomains"`
	// LastUpdatedAt 是上一次「成功」更新的时刻，Unix 毫秒。0 表示从未成功过，
	// 调度会据此立即拉取一次，见 SubscriptionJob。
	LastUpdatedAt int64 `json:"lastUpdatedAt" form:"lastUpdatedAt"`
	// LastError 是上一次尝试的失败原因，成功时清空。必须显示在界面上——
	// 只进日志的话，管理员看到的是一个域名数量停在两周前却毫无提示的组。
	LastError string `json:"lastError" form:"lastError"`
	// LastSkipped 是上一次成功解析时跳过的非域名规则条数（IP-CIDR 等）。
	LastSkipped int `json:"lastSkipped" form:"lastSkipped"`
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
	// InboundIds 是这条规则覆盖的入站 id，JSON 整数数组，升序去重存储。
	// 空数组 [] 表示「所有用户」（含以后新建的入站）。
	//
	// 升序去重不是洁癖：buildRule 直接按这个顺序生成 inboundTag 数组，而
	// 「生成逐字节确定」是 Config.Equals 能正确判断配置是否变化的前提；
	// 顺序一抖动，那个 10 秒的重启 cron 就会不停重启 xray。
	InboundIds string `json:"inboundIds" form:"inboundIds"`
	// DomainGroupIds 是这条规则引用的域名组 id，JSON 整数数组，升序去重存储。
	//
	// 升序去重与 InboundIds 同理，是「生成逐字节确定」的一部分：buildRule
	// 按这个顺序逐组取域名再合并，顺序一抖动，Config.Equals 恒为 false，
	// 那个 10 秒的重启 cron 会不停重启 xray。
	//
	// 与 InboundIds 的空数组语义【相反】：这里的 [] 非法，绝不表示「所有
	// 域名组」。域名条件为空会让 xray 把规则当作「不限制」，从「这批域名走
	// B」退化成「该用户全部流量走 B」，且 Configuration OK、面板显示 running。
	DomainGroupIds string `json:"domainGroupIds" form:"domainGroupIds"`
	// DomainGroupId 是多域名组改造前的单值字段，新代码一律不再读写它。
	//
	// 有意保留不删（GORM 的 sqlite AutoMigrate 本来也不删列）：万一管理员
	// 回滚到旧版本二进制，旧代码读到的还是原值，单组规则行为完全正常；
	// 删掉列则每条规则都读成 0，buildRule 全部丢弃——分流静默全灭，而面板
	// 首页仍显示 running。改造后新建的多组规则该值为 0，旧代码会整条丢弃，
	// 即分流范围缩小而非放大，安全侧正确。
	DomainGroupId int    `json:"domainGroupId" form:"domainGroupId"`
	Action        string `json:"action" form:"action"`
	// OutboundId 仅在 Action 为 ActionProxy 时有意义。
	OutboundId int  `json:"outboundId" form:"outboundId"`
	Priority   int  `json:"priority" form:"priority"`
	Enable     bool `json:"enable" form:"enable"`
}
