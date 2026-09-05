package service

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
)

// xray 支持的域名匹配前缀，见 common/geodata/rule_parser.go:226
// parseCustomDomainRule 与 parseGeoSiteRule。
// ext-domain: / ext-site: 是 ext: 的别名。
//
// 顺序无关：各前缀互不为前缀（ext: 与 ext-domain: 在第 4 个字符上就分岔）。
var domainPrefixes = []string{
	"domain:", "full:", "keyword:", "regexp:", "dotless:",
	"geosite:", "ext:", "ext-domain:", "ext-site:",
}

// lowercaseValuePrefixes 里的前缀，其值必须转小写才可能命中。
//
// xray 只把「目标域名」转小写，不归一化配置里的模式
// （app/router/condition.go:59），所以 domain:OpenAI.com 是一条永不命中的
// 哑规则，且没有任何一层会报错——不是洁癖，是防一个静默失效。
//
// regexp: 与 dotless: 刻意不在此列：它们会被编译成正则，转小写会把 \D 变成
// \d 这种意义完全相反的东西。geosite: / ext:* 的 code 由 xray 自己 ToUpper
// （rule_parser.go:211），同样不该在这里动。
var lowercaseValuePrefixes = map[string]bool{
	"domain:": true, "full:": true, "keyword:": true,
}

// ParseDomains 把用户在 textarea 中一行一条录入的域名解析成入库列表。
//
// 按输入行序输出，不排序：顺序是「生成逐字节确定」不变量的一部分。
func ParseDomains(raw string) ([]string, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		normalized, err := normalizeDomainRule(item)
		if err != nil {
			return nil, err
		}
		list = append(list, normalized)
	}
	if len(list) == 0 {
		return nil, common.NewError("域名列表不能为空")
	}
	return list, nil
}

// normalizeDomainRule 把一行录入归一成入库形态，或说明它为什么不合法。
func normalizeDomainRule(item string) (string, error) {
	for _, p := range domainPrefixes {
		if !strings.HasPrefix(item, p) {
			continue
		}
		value := item[len(p):]
		if value == "" {
			return "", common.NewError("域名格式不支持，前缀后面没有内容:", item)
		}
		if lowercaseValuePrefixes[p] {
			return p + strings.ToLower(value), nil
		}
		return item, nil
	}

	// 带冒号却没匹配上任何前缀：几乎必然是前缀拼错。放行的话 xray 会把整串
	// 当子串匹配（infra/conf/router.go:175 传的 defaultType 是 Domain_Substr），
	// 而 SNI/Host 里不含冒号——一条永不命中的哑规则，且 Configuration OK。
	if strings.Contains(item, ":") {
		return "", common.NewError("域名格式不支持，无法识别的前缀:", item,
			"可用前缀:", strings.Join(domainPrefixes, " / "))
	}

	// 无前缀的裸串在 xray 的 routing 规则里是子串匹配，但在 geosite 数据文件
	// （domain-list-community）里是后缀匹配——同一份文本两种含义。含点的裸串
	// 两种解读都说得通，放行等于让从 geosite 列表复制来的 openai.com 静默变成
	// 能命中 notopenai.com.evil.net 的规则。拒绝是廉价的：报错里点名三种写法，
	// 补个前缀即可。
	if strings.Contains(item, ".") {
		return "", common.NewError("域名写法有歧义:", item,
			"——不带前缀时 xray 按子串匹配。请明确写成 domain:"+item+
				"（含子域名）、full:"+item+"（精确匹配）或 keyword:"+item+
				"（确实要子串匹配）")
	}

	// 不含点也不含冒号：不可能是域名，意图唯一是关键词（对应 Surge/Clash 的
	// DOMAIN-KEYWORD）。归一成显式的 keyword: 存库，让域名组列表里的标签
	// 自己说清楚它在做什么，也让手工与订阅两条路径的存储形态一致。
	if !isValidKeyword(item) {
		return "", common.NewError("关键词含有非法字符:", item)
	}
	return "keyword:" + strings.ToLower(item), nil
}

// EncodeDomains 把域名列表序列化为入库格式。
func EncodeDomains(list []string) (string, error) {
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeDomains 是 EncodeDomains 的逆操作。库中数据损坏时返回错误而非空列表，
// 避免生成条件残缺的路由规则。
func DecodeDomains(encoded string) ([]string, error) {
	var list []string
	if err := json.Unmarshal([]byte(encoded), &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DecodeSubscribedDomains 容忍空字符串——没订阅过的组这个字段本来就是空的，
// 直接交给 DecodeDomains 会得到一个 json 语法错误，进而被 buildRule 当成
// 「数据损坏」丢弃整条规则。
func DecodeSubscribedDomains(encoded string) ([]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	return DecodeDomains(encoded)
}

type DomainGroupService struct {
}

func (s *DomainGroupService) GetAll() ([]*model.DomainGroup, error) {
	db := database.GetDB()
	groups := make([]*model.DomainGroup, 0)
	err := db.Model(model.DomainGroup{}).Order("id asc").Find(&groups).Error
	if err != nil {
		return nil, err
	}
	return groups, nil
}

func (s *DomainGroupService) Get(id int) (*model.DomainGroup, error) {
	db := database.GetDB()
	group := &model.DomainGroup{}
	err := db.Model(model.DomainGroup{}).First(group, id).Error
	if err != nil {
		return nil, err
	}
	return group, nil
}

func (s *DomainGroupService) Add(group *model.DomainGroup) error {
	db := database.GetDB()
	return db.Save(group).Error
}

func (s *DomainGroupService) Update(group *model.DomainGroup) error {
	old, err := s.Get(group.Id)
	if err != nil {
		return err
	}

	return database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Updates(updateFieldsFor(old, group)).Error
}

// updateFieldsFor 返回 DomainGroupService.Update 要写的列。
//
// 只写实际要改的列，不用 Save 写整行。整行写入会把 Get 那一刻捕获的
// SubscribedDomains/LastUpdatedAt 一并写回，而这中间可能有一次成功的
// Refresh 已经更新了它们——那次刷新会被静默回滚，无错误无日志。
// 注意：将来给 DomainGroup 加字段时，要更新的字段必须同时加进这个 map，
// 否则新字段会静默地无法通过编辑接口更新。
//
// 抽成纯函数是为了能直接断言「订阅地址没变时不碰订阅列」这条不变量——
// Update 现在是单条原子语句，没有窗口可供行为测试插入验证。
func updateFieldsFor(old, next *model.DomainGroup) map[string]any {
	fields := map[string]any{
		"remark":  next.Remark,
		"domains": next.Domains,
		"cidrs":   next.Cidrs,
	}

	// 订阅地址变了：旧订阅内容来自另一个来源，继续拿它分流是「用错误的数据
	// 生效」，比规则暂时不生效更危险。域名与 IP 两侧必须一起清，只清一侧会
	// 留下一个「域名是新地址的、IP 还是旧地址的」的混合体。
	if old.SubscribeUrl != next.SubscribeUrl {
		fields["subscribe_url"] = next.SubscribeUrl
		fields["subscribed_domains"] = ""
		fields["subscribed_cidrs"] = ""
		fields["last_updated_at"] = 0
		fields["last_error"] = ""
		fields["last_skipped"] = 0
	}

	return fields
}

// subscriptionMu 串行化所有订阅更新。定时任务与管理员点「立即更新」可能同时
// 发生。更新是分钟级的低频操作，不值得做更细的按组加锁。
var subscriptionMu sync.Mutex

// Refresh 立即更新一个域名组的订阅内容。
func (s *DomainGroupService) Refresh(id int) error {
	subscriptionMu.Lock()
	defer subscriptionMu.Unlock()

	group, err := s.Get(id)
	if err != nil {
		return err
	}
	return s.refreshLocked(group)
}

// refreshLocked 假定调用方已持有 subscriptionMu。
func (s *DomainGroupService) refreshLocked(group *model.DomainGroup) error {
	if group.SubscribeUrl == "" {
		return common.NewError("该域名组没有配置订阅地址, id:", group.Id)
	}

	raw, err := fetchSubscription(group.SubscribeUrl)
	if err != nil {
		return s.recordFailure(group, err)
	}
	domains, cidrs, skipped, err := ParseSubscription(raw)
	if err != nil {
		return s.recordFailure(group, err)
	}
	// 落库前过真实 xray 校验。两个 Validate 自身都是 fail open 的：
	// 二进制缺失、超时等一律放行，只有 xray 明确判定非法才拦。
	//
	// 空列表不送检：探针规则的条件为空数组时 xray 会报
	// "this rule has no effective fields"，把「这一侧没有内容」这个正常
	// 状态变成整次刷新失败。ValidateCidrs 自己挡了空，ValidateDomains
	// 没有（它的既有调用点保证了非空），所以在这里显式判。
	if len(domains) > 0 {
		if err := ValidateDomains(domains); err != nil {
			return s.recordFailure(group, err)
		}
	}
	if err := ValidateCidrs(cidrs); err != nil {
		return s.recordFailure(group, err)
	}
	encodedDomains, err := EncodeDomains(domains)
	if err != nil {
		return s.recordFailure(group, err)
	}
	encodedCidrs, err := EncodeCidrs(cidrs)
	if err != nil {
		return s.recordFailure(group, err)
	}

	// 用 map 而不是 struct：GORM 的 struct 更新会跳过零值，
	// LastError 与 LastSkipped 清不掉。
	//
	// 两侧【都】写，哪怕其中一个是空——这不与「失败时绝不清空」冲突，
	// 那条约束的是失败路径。成功路径上，订阅源真的不再列 IP 了，保留上一次
	// 的 IP 就是拿过期数据分流，比 IP 条件消失更危险。
	//
	// Where 里带上 subscribe_url：拉取耗时可达 30s，一批组更是分钟级，
	// 这期间管理员可能已经把订阅地址改成了别的（Update 不取 subscriptionMu）。
	// 不加这个条件，本次用旧地址拉到的内容会被当成新地址的结果写回——
	// 组的 URL 是新的，内容却是旧地址的，界面还显示「刚刚更新」，
	// 比规则单纯不生效更危险（见 spec §5.5）。
	res := database.GetDB().Model(model.DomainGroup{}).
		Where("id = ? AND subscribe_url = ?", group.Id, group.SubscribeUrl).
		Updates(map[string]any{
			"subscribed_domains": encodedDomains,
			"subscribed_cidrs":   encodedCidrs,
			"last_updated_at":    time.Now().UnixMilli(),
			"last_error":         "",
			"last_skipped":       skipped,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		logger.Warning("refresh subscription discarded: subscribe url changed or group deleted during fetch, id:",
			group.Id, "remark:", group.Remark)
		return common.NewError("订阅地址在拉取期间已变化或该组已被删除，本次结果已作废, id:", group.Id)
	}
	return nil
}

// recordFailure 只写失败原因，绝不动 SubscribedDomains 与 LastUpdatedAt。
//
// 清空订阅域名会让合并结果为空 → buildRule 跳过整条规则 → 本该走指定节点或被
// 封禁的流量静默退回直连。上游改格式、URL 失效返回 404 页面、CDN 返回空响应
// 都会走到这里，而它们都不该导致分流失效。宁可用旧数据。
func (s *DomainGroupService) recordFailure(group *model.DomainGroup, cause error) error {
	// 同样带 subscribe_url 条件：拉取期间地址被改掉的话，这次失败是旧地址
	// 造成的，不该把 last_error 钉在管理员刚改好的新地址上。零行受影响不算
	// 额外错误——原始失败原因（cause）仍然如实返回给调用方。
	res := database.GetDB().Model(model.DomainGroup{}).
		Where("id = ? AND subscribe_url = ?", group.Id, group.SubscribeUrl).
		Update("last_error", cause.Error())
	if res.Error != nil {
		logger.Warning("record subscription failure err:", res.Error)
	} else if res.RowsAffected == 0 {
		logger.Warning("subscribe url changed or group deleted during failed fetch, not pinning stale error, id:",
			group.Id, "remark:", group.Remark)
	}
	logger.Warning("refresh subscription failed, id:", group.Id,
		"remark:", group.Remark, "err:", cause)
	return cause
}

func (s *DomainGroupService) Del(id int) error {
	ruleService := RoutingRuleService{}
	if err := ruleService.CheckDomainGroupRefs(id); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Delete(model.DomainGroup{}, id).Error
}

// RefreshDue 更新所有到点的订阅域名组，返回成功更新的个数。
//
// 单个组失败不影响其余组：失败原因已由 recordFailure 落库，返回 error 会让
// 一个坏掉的订阅地址把整批更新都挡住。只有取配置这种全局性失败才返回 error。
func (s *DomainGroupService) RefreshDue() (int, error) {
	settingService := SettingService{}
	raw, err := settingService.GetSubscriptionUpdateTime()
	if err != nil {
		return 0, err
	}
	at, err := time.Parse("15:04", raw)
	if err != nil {
		return 0, common.NewError("订阅更新时间格式不正确:", raw, "err:", err)
	}
	loc, err := settingService.GetTimeLocation()
	if err != nil {
		return 0, err
	}

	groups, err := s.GetAll()
	if err != nil {
		return 0, err
	}

	subscriptionMu.Lock()
	defer subscriptionMu.Unlock()

	now := time.Now().In(loc)
	updated := 0
	for _, group := range groups {
		if group.SubscribeUrl == "" {
			continue
		}
		if !ShouldUpdateNow(now, group.LastUpdatedAt, at.Hour(), at.Minute()) {
			continue
		}
		if err := s.refreshLocked(group); err != nil {
			continue // 失败原因已落库并记日志
		}
		updated++
	}
	return updated, nil
}

// MergeDomains 按传入顺序合并多个字符串列表，去重并保留首次出现。
// 域名与 IP 段两类规则值都用它——它只是一个有序去重，与值的语义无关。
//
// 顺序确定是「生成逐字节确定」不变量的一部分：调用方按固定顺序传入
// （手工在前、订阅在后；跨组则按域名组 id 升序），本函数不重排。
func MergeDomains(lists ...[]string) []string {
	total := 0
	for _, l := range lists {
		total += len(l)
	}
	merged := make([]string, 0, total)
	seen := make(map[string]bool, total)
	for _, list := range lists {
		for _, d := range list {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			merged = append(merged, d)
		}
	}
	return merged
}
