package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"

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

// asciiOnlyValuePrefixes 里的前缀拿嗅探出的域名做子串/正则匹配，而真实流量里的域名
// 一律是 ASCII（国际化域名是 punycode 的 xn-- 形态）：值里带非 ASCII 字符就永远
// 匹配不到，Configuration OK、没有任何一层报错。domain:/full: 不在此列——它们的值
// 是一个确定的域名，能转换（见 domainToASCII），所以转换而不是拒绝。
var asciiOnlyValuePrefixes = map[string]bool{
	"keyword:": true, "regexp:": true, "dotless:": true,
}

// nonASCIIPatternHint 是拒绝非 ASCII 关键词/正则时的说明。
const nonASCIIPatternHint = "——真实流量里的域名一律是 ASCII，中文等国际化域名是 punycode（xn-- 开头）形态，" +
	"含非 ASCII 字符的关键词/正则永远匹配不到。中文域名请写成 domain:例子.cn 或 full:例子.cn" +
	"（保存时自动转成 xn-- 形态），或直接写 xn-- 形态"

// domainToASCII 把含非 ASCII 字符的域名逐个标签转成 punycode（xn--）形态，纯 ASCII
// 的标签原样保留。
//
// xray 构建域名匹配器时不做 IDN 转换，只比较字节；而嗅探出的 SNI/Host 永远是
// punycode。写成「例子.cn」的规则能加载、run -test 照样 Configuration OK，却永远
// 命中不了任何连接——流量走向验证拿同一个字符串去 TestRoute 还会亮绿灯。
//
// 只转含非 ASCII 字符的标签：idna.Lookup 按 STD3 规则拒绝下划线，整串交给它会让
// _dmarc.example.com 这类既有的合法写法变成保存失败。Lookup 同时做 UTS #46 映射
// （大小写、全角句点「。」等），与浏览器把地址栏里的中文域名转成 SNI 的口径一致。
func domainToASCII(value string) (string, error) {
	if isASCII(value) {
		return value, nil
	}
	labels := strings.Split(value, ".")
	for i, label := range labels {
		if isASCII(label) {
			continue
		}
		ascii, err := idna.Lookup.ToASCII(label)
		if err != nil {
			return "", err
		}
		labels[i] = ascii
	}
	return strings.Join(labels, "."), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// ParseDomains 把用户在 textarea 中一行一条录入的域名解析成入库列表，
// 返回（列表, 去掉的重复条数, 错误）。
//
// 按输入行序输出，不排序：顺序是「生成逐字节确定」不变量的一部分。去重保留
// 首次出现，同样是为了这条不变量。
//
// 去重发生在归一化【之后】：domain:OpenAI.com 与 domain:openai.com 在框里是
// 两行不同的文本，归一后才是同一条规则——那恰恰是管理员自己完全看不出来的
// 一类重复。放在归一化之前只能去掉字面完全相同的那些，最需要帮忙的一类反而
// 一条都去不掉。
//
// 只做字面去重，不做语义包含去重：domain:example.com 覆盖 domain:sub.example.com
// 这类冗余要靠语义分析才能判定，删错了就是改写管理员的意图。
//
// 生成期的 MergeDomains 本来就会去重，所以这一步不改变 xray 拿到的配置；
// 它消掉的是编辑框回显、列表计数、导出文件与送检列表里的噪音。
func ParseDomains(raw string) ([]string, int, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		normalized, err := normalizeDomainRule(item)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, normalized)
	}
	if len(list) == 0 {
		return nil, 0, common.NewError("域名列表不能为空")
	}
	deduped := MergeDomains(list)
	return deduped, len(list) - len(deduped), nil
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
		if p == "domain:" || p == "full:" {
			ascii, err := domainToASCII(value)
			if err != nil {
				return "", common.NewError("国际化域名无法转换成 punycode（xn-- 形态）:", item, "err:", err)
			}
			value = ascii
		} else if asciiOnlyValuePrefixes[p] && !isASCII(value) {
			return "", common.NewError("不支持含非 ASCII 字符的写法:", item+nonASCIIPatternHint)
		}
		if lowercaseValuePrefixes[p] {
			return p + strings.ToLower(value), nil
		}
		return p + value, nil
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
	if !isASCII(item) {
		return "", common.NewError("不支持含非 ASCII 字符的关键词:", item+nonASCIIPatternHint)
	}
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

	fields := updateFieldsFor(old, group)
	urlsChanged := old.SubscribeUrl != group.SubscribeUrl
	if urlsChanged {
		// 地址串变了不等于内容全都作废：仍在列表里的地址，它们的历史结果本来就是
		// 从这些地址拉来的，按新顺序立刻重新合并，盖掉 updateFieldsFor 的清空。
		// 只清空不重合并的话，删掉多个地址中的一个、或者只是调了顺序，这个组就会
		// 空到剩下的地址下一次到期（多半是明天）——引用它的封禁/分流规则被
		// buildRule 整条跳过，流量静默退回直连。
		//
		// 不取 subscriptionMu：Update 本来就不取它（拉取可达分钟级），写回仍是
		// 下面这一条 Updates。与并发刷新交错时，那次刷新带的是旧地址串，比对
		// subscribe_url 必然失败、整次作废；这里合并的又只有仍列出的地址自己的
		// 结果——最坏是用了某个地址早一次拉到的内容，不会混进别的来源。
		remerged, err := s.remergeListedSubscriptions(group)
		if err != nil {
			// 合并不出来就退回只清空：这次编辑照样生效，RefreshDue 下一轮看到
			// last_updated_at=0 会按历史结果补一次合并（最多 10 分钟）。
			logger.Warning("按保留的订阅地址重新合并失败，先清空等下一轮补上, group:", group.Id, "err:", err)
		}
		for k, v := range remerged {
			fields[k] = v
		}
	}

	if err := database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Updates(fields).Error; err != nil {
		return err
	}
	if urlsChanged {
		// 被移除的地址的结果行一并删掉：单行可达数 MB，留着既占库，地址重新加回来
		// 又恰好拉取失败时，它删除前的旧内容还会被当成「上一次的结果」合并回来。
		// 删不掉不算这次编辑失败——下一次成功刷新时 refreshLockedURLs 还会再清。
		if err := s.pruneUnlistedSubscriptions(group.Id, ParseSubscribeURLs(group.SubscribeUrl)); err != nil {
			logger.Warning("删除已移除订阅地址的结果行失败, group:", group.Id, "err:", err)
		}
	}
	return nil
}

// remergeListedSubscriptions 用 group.SubscribeUrl 里仍列出的地址各自的历史结果
// 重新合成合并结果，返回要盖到 updateFieldsFor 结果上的列。没有任何地址贡献
// 内容时返回 nil，保持 updateFieldsFor 的清空（等 RefreshDue 去拉）。
//
// last_updated_at 取参与合并的地址里最近一次成功拉取的时刻，不取当前时刻：这次
// 没有拉取任何东西，写「刚刚更新」是在骗人。last_error 按各地址自己记下的失败
// 原因重新拼，口径与 refreshLockedURLs 相同——仍在列表里的地址若上次就失败了，
// 组卡片不能因为一次编辑就变回绿色。
func (s *DomainGroupService) remergeListedSubscriptions(group *model.DomainGroup) (map[string]any, error) {
	urls := ParseSubscribeURLs(group.SubscribeUrl)
	if len(urls) == 0 {
		return nil, nil
	}
	states, err := s.subscriptionStates(group.Id)
	if err != nil {
		return nil, err
	}
	merged := mergeSubscriptionStates(group.Id, urls, states)
	if len(merged.domains) == 0 && len(merged.cidrs) == 0 {
		return nil, nil
	}
	encodedDomains, err := EncodeDomains(merged.domains)
	if err != nil {
		return nil, err
	}
	encodedCidrs, err := EncodeCidrs(merged.cidrs)
	if err != nil {
		return nil, err
	}
	failures := make([]string, 0)
	for _, u := range urls {
		if st := states[u]; st != nil && st.LastError != "" {
			failures = append(failures, u+"（"+st.LastError+"）")
		}
	}
	return map[string]any{
		"subscribed_domains": encodedDomains,
		"subscribed_cidrs":   encodedCidrs,
		"last_updated_at":    merged.latest,
		"last_error":         partialFailureText(failures),
		"last_skipped":       merged.skipped,
	}, nil
}

// pruneUnlistedSubscriptions 删掉该组里地址已不在 urls 中的订阅结果行；urls 为空
// （取消订阅）时删掉该组全部的行。
func (s *DomainGroupService) pruneUnlistedSubscriptions(groupId int, urls []string) error {
	q := database.GetDB().Where("group_id = ?", groupId)
	if len(urls) > 0 {
		q = q.Where("url NOT IN ?", urls)
	}
	return q.Delete(model.DomainGroupSubscription{}).Error
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
//
// 地址变了时这里给出的是「清空」；Update 随后会用 remergeListedSubscriptions
// 按仍列出的地址重新合并、盖掉这几列（同一条 Updates 写入），只有一个地址
// 都贡献不了内容时清空才真正落库。
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

// Refresh 立即更新一个域名组的订阅内容。部分地址失败时返回 *PartialRefreshError：
// 内容已经写进库，调用方照样要下发配置（见 IsPartialRefresh）。
func (s *DomainGroupService) Refresh(id int) error {
	subscriptionMu.Lock()
	defer subscriptionMu.Unlock()

	group, err := s.Get(id)
	if err != nil {
		return err
	}
	return s.refreshLocked(group)
}

// SubscriptionState 是一个订阅地址对外可见的状态摘要。
//
// 只带计数不带内容：单个地址的域名可达十几万条，详情接口把它们全量吐给浏览器
// 既没有意义，渲染也会卡死页面——这与域名组列表只回摘要是同一个理由。
type SubscriptionState struct {
	Url           string `json:"url"`
	LastUpdatedAt int64  `json:"lastUpdatedAt"`
	LastError     string `json:"lastError"`
	LastSkipped   int    `json:"lastSkipped"`
	DomainCount   int    `json:"domainCount"`
	CidrCount     int    `json:"cidrCount"`
}

// SubscriptionStates 按 SubscribeUrl 里的地址顺序返回每个地址的状态摘要。
//
// 界面必须逐个地址显示状态：一个地址失败时它上一次的内容仍在参与分流，只有
// 把「这个地址拉取失败，正在用 3 天前的内容」摊开写出来，管理员才知道该去修
// 哪一行、以及在修好之前生效的是什么。组级那一行 LastError 只说得清「有几个
// 失败了」。
func (s *DomainGroupService) SubscriptionStates(group *model.DomainGroup) ([]SubscriptionState, error) {
	urls := ParseSubscribeURLs(group.SubscribeUrl)
	if len(urls) == 0 {
		return []SubscriptionState{}, nil
	}
	states, err := s.subscriptionStates(group.Id)
	if err != nil {
		return nil, err
	}
	out := make([]SubscriptionState, 0, len(urls))
	for _, u := range urls {
		item := SubscriptionState{Url: u}
		if st := states[u]; st != nil {
			item.LastUpdatedAt = st.LastUpdatedAt
			item.LastError = st.LastError
			item.LastSkipped = st.LastSkipped
			// 解码失败只当作 0 条：这里是展示路径，为了一个计数让整个详情
			// 接口失败，管理员连编辑弹窗都打不开，比数字不准严重得多。
			if d, err := DecodeSubscribedDomains(st.Domains); err == nil {
				item.DomainCount = len(d)
			}
			if c, err := DecodeSubscribedCidrs(st.Cidrs); err == nil {
				item.CidrCount = len(c)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// subscriptionStates 取一个域名组下每个订阅地址各自的上次结果，按地址索引。
func (s *DomainGroupService) subscriptionStates(groupId int) (map[string]*model.DomainGroupSubscription, error) {
	rows := make([]*model.DomainGroupSubscription, 0)
	err := database.GetDB().Model(model.DomainGroupSubscription{}).
		Where("group_id = ?", groupId).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]*model.DomainGroupSubscription, len(rows))
	for _, r := range rows {
		out[r.Url] = r
	}
	return out, nil
}

// fetchOne 拉取并解析一个订阅地址，成功时把结果写进该地址自己那一行。
//
// 校验（真实 xray）逐个地址做，不放到合并之后：合并后再校验的话，一个地址
// 的坏内容会让整份合并结果被判非法，其余地址跟着一起写不进去——那就退回成
// 「全有或全无」了。代价是 N 次 exec，而订阅刷新是每天一次的低频操作。
func (s *DomainGroupService) fetchOne(groupId int, url string) (*model.DomainGroupSubscription, error) {
	raw, err := fetchSubscription(url)
	if err != nil {
		return nil, err
	}
	domains, cidrs, skipped, err := ParseSubscription(raw)
	if err != nil {
		return nil, err
	}
	// 空列表不送检：探针规则的条件为空数组时 xray 会报
	// "this rule has no effective fields"，把「这一侧没有内容」这个正常状态
	// 变成一次失败。ValidateCidrs 自己挡了空，ValidateDomains 没有。
	if len(domains) > 0 {
		if err := ValidateDomains(domains); err != nil {
			return nil, err
		}
	}
	if err := ValidateCidrs(cidrs); err != nil {
		return nil, err
	}
	encodedDomains, err := EncodeDomains(domains)
	if err != nil {
		return nil, err
	}
	encodedCidrs, err := EncodeCidrs(cidrs)
	if err != nil {
		return nil, err
	}

	next := &model.DomainGroupSubscription{
		GroupId: groupId, Url: url,
		Domains: encodedDomains, Cidrs: encodedCidrs,
		LastUpdatedAt: time.Now().UnixMilli(), LastError: "", LastSkipped: skipped,
	}
	if err := s.upsertSubscription(next); err != nil {
		return nil, err
	}
	return next, nil
}

// upsertSubscription 按 (group_id, url) 写入或更新一行。
//
// 用 map 而不是 struct 更新：GORM 的 struct 更新跳过零值，LastError 清不掉——
// 一个恢复正常的地址会永远挂着上次的错误。
func (s *DomainGroupService) upsertSubscription(next *model.DomainGroupSubscription) error {
	db := database.GetDB()
	res := db.Model(model.DomainGroupSubscription{}).
		Where("group_id = ? AND url = ?", next.GroupId, next.Url).
		Updates(map[string]any{
			"domains":         next.Domains,
			"cidrs":           next.Cidrs,
			"last_updated_at": next.LastUpdatedAt,
			"last_error":      next.LastError,
			"last_skipped":    next.LastSkipped,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return db.Create(next).Error
}

// recordURLFailure 只写这个地址自己的失败原因，绝不动它的 Domains/Cidrs 与
// LastUpdatedAt——保留它上一次的内容正是本表存在的目的。
//
// 该地址还没有行时（新加的地址第一次就失败）也要建一行：不建的话界面上这个
// 地址旁边什么都不显示，管理员会以为它一切正常，只是「还没轮到」。
func (s *DomainGroupService) recordURLFailure(groupId int, url string, cause error) {
	db := database.GetDB()
	res := db.Model(model.DomainGroupSubscription{}).
		Where("group_id = ? AND url = ?", groupId, url).
		Update("last_error", cause.Error())
	if res.Error != nil {
		logger.Warning("记录订阅地址失败原因时出错, group:", groupId, "url:", redactURL(url), "err:", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		return
	}
	row := &model.DomainGroupSubscription{
		GroupId: groupId, Url: url, Domains: "", Cidrs: "",
		LastUpdatedAt: 0, LastError: cause.Error(),
	}
	if err := db.Create(row).Error; err != nil {
		logger.Warning("新建订阅地址状态行失败, group:", groupId, "url:", redactURL(url), "err:", err)
	}
}

// refreshLocked 拉取该组的全部订阅地址。假定调用方已持有 subscriptionMu。
func (s *DomainGroupService) refreshLocked(group *model.DomainGroup) error {
	return s.refreshLockedURLs(group, ParseSubscribeURLs(group.SubscribeUrl))
}

// refreshLockedURLs 拉取 targets 里列出的地址，与该组其余地址的历史结果合并后
// 写回。假定调用方已持有 subscriptionMu。
//
// **每个地址相互独立**：targets 里某个地址失败时，只记它自己的 LastError，
// 它上一次的内容原样参与合并；其余地址照常更新到最新。不在 targets 里的地址
// （定时任务里今天已经拉过的那些）不重拉，直接用历史结果参与合并——合并结果
// 因此逐字节不变，Config.Equals 判定无变化，不会白重启一次 xray。
//
// 唯一一处「整次不写」的例外在末尾：合并结果两侧都空时绝不写回。那会清空上
// 一次的合并结果，buildRule 因「域名组为空」跳过整条规则，本该走指定节点或被
// 封禁的流量静默退回直连。
//
// targets 为空时不拉取任何地址，只按历史结果重新合并、写回（RefreshDue 用它
// 修复合并结果被清空、而各地址今天都已拉过的组）。
//
// 写回成功但有地址失败时返回 *PartialRefreshError：合并结果已经变了，调用方
// 必须当作「更新了」去下发配置；其余非 nil 的 error 表示这次什么都没写。
func (s *DomainGroupService) refreshLockedURLs(group *model.DomainGroup, targets []string) error {
	urls := ParseSubscribeURLs(group.SubscribeUrl)
	if len(urls) == 0 {
		return common.NewError("该域名组没有配置订阅地址, id:", group.Id)
	}
	states, err := s.subscriptionStates(group.Id)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(targets))
	for _, u := range targets {
		wanted[u] = true
	}

	failures := make([]string, 0)
	for _, u := range urls {
		if !wanted[u] {
			continue
		}
		fresh, fetchErr := s.fetchOne(group.Id, u)
		if fetchErr != nil {
			// 失败的地址保留它上一次的结果（states 里那一行原样不动）参与合并。
			failures = append(failures, u+"（"+fetchErr.Error()+"）")
			s.recordURLFailure(group.Id, u, fetchErr)
			continue
		}
		states[u] = fresh
	}

	merged := mergeSubscriptionStates(group.Id, urls, states)
	domains, cidrs, skipped := merged.domains, merged.cidrs, merged.skipped
	if len(domains) == 0 && len(cidrs) == 0 {
		cause := common.NewError("所有订阅地址都没有可用内容")
		if len(failures) > 0 {
			cause = common.NewError("订阅地址全部失败：", strings.Join(failures, "；"))
		}
		return s.recordFailure(group, cause)
	}

	encodedDomains, err := EncodeDomains(domains)
	if err != nil {
		return s.recordFailure(group, err)
	}
	encodedCidrs, err := EncodeCidrs(cidrs)
	if err != nil {
		return s.recordFailure(group, err)
	}

	lastError := partialFailureText(failures)

	// 用 map 而不是 struct：GORM 的 struct 更新会跳过零值，
	// LastError 与 LastSkipped 清不掉。
	//
	// 两侧【都】写，哪怕其中一个是空——那不与「失败时绝不清空」冲突：这里
	// 走到的前提是至少有一个地址贡献了内容。成功路径上订阅源真的不再列 IP
	// 了，保留上一次的 IP 就是拿过期数据分流，比 IP 条件消失更危险。
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
			"last_error":         lastError,
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
	// 写回成功，说明 urls 就是这一刻库里的地址列表：顺手删掉不在其中的结果行
	// （升级前删地址留下的、拉取期间被改掉地址后又被写回的）。删不掉只记日志，
	// 合并已经只认列表里的地址，残留行不会参与分流。
	if err := s.pruneUnlistedSubscriptions(group.Id, urls); err != nil {
		logger.Warning("删除已移除订阅地址的结果行失败, group:", group.Id, "err:", err)
	}
	// 部分失败要如实返回：内容确实更新了一部分，但管理员必须知道有地址没拉到，
	// 否则他会以为这次刷新完全成功。文案里已说明失败地址保留了上次的内容。
	// 用 PartialRefreshError 而不是普通 error：成功的那部分已经改了配置，调用方
	// 据此仍要置重启标志。日志里的地址脱敏（见 redactURL）。
	if lastError != "" {
		logger.Warning("refresh subscription partially failed, id:", group.Id,
			"remark:", group.Remark, "err:", redactURLsInText(lastError))
		return &PartialRefreshError{msg: common.NewError(lastError).Error()}
	}
	return nil
}

// PartialRefreshError 表示这次刷新已经把合并结果写进了库，但有订阅地址失败
// （失败的地址保留了上一次的内容）。
//
// 它首先是一个成功：配置已经变了。只看 err == nil 的调用方会漏掉这一种——
// 改动前「立即更新」与 SubscriptionJob 都是这样，结果是成功那一半的新内容一直
// 停在库里不下发，规则列表因为库与核心不一致，每条规则都挂着「等待下发」。
type PartialRefreshError struct {
	msg string
}

func (e *PartialRefreshError) Error() string { return e.msg }

// IsPartialRefresh 报告 err 是不是「内容已写入、但有地址失败」。
func IsPartialRefresh(err error) bool {
	var partial *PartialRefreshError
	return errors.As(err, &partial)
}

// partialFailureText 把失败地址拼成组级 LastError 的文案，没有失败时为空串。
func partialFailureText(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	return strconv.Itoa(len(failures)) + " 个订阅地址失败（已保留它们上一次的内容）：" +
		strings.Join(failures, "；")
}

// subscriptionMerge 是把一个组各地址的结果按地址顺序合并后的产物。
type subscriptionMerge struct {
	domains, cidrs []string
	skipped        int
	// latest 是参与合并的地址里最近一次成功拉取的时刻（毫秒）。
	latest int64
}

// mergeSubscriptionStates 按 urls 的顺序合并各地址在 states 里的结果。
//
// 按 urls 的顺序合并，不是按表里的行序：顺序确定是「生成逐字节确定」的一部分，
// 换个顺序会让 Config.Equals 恒为 false，那个 10 秒的 cron 会不停重启 xray。
// 只认 urls 里的地址：表里残留的、已不在列表里的行绝不参与合并。
func mergeSubscriptionStates(groupId int, urls []string, states map[string]*model.DomainGroupSubscription) subscriptionMerge {
	domainLists := make([][]string, 0, len(urls))
	cidrLists := make([][]string, 0, len(urls))
	out := subscriptionMerge{}
	for _, u := range urls {
		st := states[u]
		if st == nil {
			// 这个地址从未成功过、这次也没成功，它不贡献任何内容。
			continue
		}
		d, decodeErr := DecodeSubscribedDomains(st.Domains)
		if decodeErr != nil {
			logger.Warning("订阅结果域名列解码失败，本次跳过该地址, group:", groupId, "url:", redactURL(u), "err:", decodeErr)
			continue
		}
		c, decodeErr := DecodeSubscribedCidrs(st.Cidrs)
		if decodeErr != nil {
			logger.Warning("订阅结果 IP 段列解码失败，本次跳过该地址, group:", groupId, "url:", redactURL(u), "err:", decodeErr)
			continue
		}
		domainLists = append(domainLists, d)
		cidrLists = append(cidrLists, c)
		out.skipped += st.LastSkipped
		out.latest = max(out.latest, st.LastUpdatedAt)
	}
	out.domains = MergeDomains(domainLists...)
	out.cidrs = MergeDomains(cidrLists...)
	return out
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
	// 失败原因里嵌着订阅地址（net/http 的错误更是带着整串 query），日志只留脱敏后的。
	logger.Warning("refresh subscription failed, id:", group.Id,
		"remark:", group.Remark, "err:", redactURLsInText(cause.Error()))
	return cause
}

func (s *DomainGroupService) Del(id int) error {
	ruleService := RoutingRuleService{}
	if err := ruleService.CheckDomainGroupRefs(id); err != nil {
		return err
	}
	db := database.GetDB()
	// 先删订阅结果行，再删组本身。顺序不能反：SQLite 会复用被删除的自增 id，
	// 先删组、后一步失败的话就留下一批孤儿行，它们会绑到下一个新建的域名组
	// 上——那个组莫名其妙带着别人的订阅内容参与分流，而引用不再悬空，
	// 生成期那道「跳过不存在的引用」的防线根本拦不住。反过来的中途失败只是
	// 让这个组的订阅结果需要重拉一次，无害。
	if err := db.Where("group_id = ?", id).
		Delete(model.DomainGroupSubscription{}).Error; err != nil {
		return err
	}
	return db.Delete(model.DomainGroup{}, id).Error
}

// PruneSubscriptionOrphans 删掉所属域名组已经不存在的订阅结果行，返回删除条数。
//
// Del 已经在正常路径上连带删除了，这里是兜底：手工改库、Del 中途失败都会留下
// 孤儿行，而 SQLite 的 id 复用会让它们绑到下一个新建的组上。与用量历史的
// PruneOrphans 同一个理由、同一套做法。
func (s *DomainGroupService) PruneSubscriptionOrphans() (int64, error) {
	res := database.GetDB().
		Where("group_id NOT IN (SELECT id FROM domain_groups)").
		Delete(model.DomainGroupSubscription{})
	return res.RowsAffected, res.Error
}

// RefreshDue 更新所有到点的订阅域名组，返回合并结果被写回的组数——含部分地址
// 失败、但成功的那部分已经写进库的组（调用方据此置重启标志，见 PartialRefreshError）。
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
		urls := ParseSubscribeURLs(group.SubscribeUrl)
		if len(urls) == 0 {
			continue
		}
		// 到期判定按**地址**各做一次，不看组级的 LastUpdatedAt：今天已经拉
		// 成功的地址不该被反复重拉，而失败的那个必须每轮都重试。用组级时间
		// 判的话两者只能二选一——取最近一次成功就是「一个地址成功了，失败
		// 的那个今天不再重试」，取最早一次就是「有个地址一直失败，其余地址
		// 每 10 分钟被白拉一遍」。
		states, err := s.subscriptionStates(group.Id)
		if err != nil {
			logger.Warning("读取订阅地址状态失败, group:", group.Id, "err:", err)
			continue
		}
		due := make([]string, 0, len(urls))
		for _, u := range urls {
			last := int64(0)
			if st := states[u]; st != nil {
				last = st.LastUpdatedAt
			}
			if ShouldUpdateNow(now, last, at.Hour(), at.Minute()) {
				due = append(due, u)
			}
		}
		// 没有到期的地址时，只有合并结果被清空过（last_updated_at=0）的组还要处理：
		// 按各地址的历史结果重新合并一次，不拉取。升级前「改订阅地址就清空」留下
		// 的组、Update 当时没合并出来的组都靠这一步补上，否则要空到明天。
		if len(due) == 0 && group.LastUpdatedAt != 0 {
			continue
		}
		if err := s.refreshLockedURLs(group, due); err != nil && !IsPartialRefresh(err) {
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
