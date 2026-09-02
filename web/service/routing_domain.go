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

// xray 支持的域名匹配前缀。不带前缀的裸域名 xray 也接受（等价于子串匹配），
// 但容易误伤，这里要求显式前缀。
var domainPrefixes = []string{"domain:", "full:", "geosite:", "regexp:", "ext:"}

// ParseDomains 把用户在 textarea 中一行一条录入的域名解析成列表。
func ParseDomains(raw string) ([]string, error) {
	lines := strings.Split(raw, "\n")
	list := make([]string, 0, len(lines))
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		ok := false
		for _, p := range domainPrefixes {
			if strings.HasPrefix(item, p) && len(item) > len(p) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, common.NewError("域名格式不支持，必须以 domain: / full: / geosite: / regexp: / ext: 开头:", item)
		}
		list = append(list, item)
	}
	if len(list) == 0 {
		return nil, common.NewError("域名列表不能为空")
	}
	return list, nil
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

	// 只写实际要改的列，不用 Save 写整行。整行写入会把 Get 那一刻捕获的
	// SubscribedDomains/LastUpdatedAt 一并写回，而这中间可能有一次成功的
	// Refresh 已经更新了它们——那次刷新会被静默回滚，无错误无日志。
	// 注意：将来给 DomainGroup 加字段时，要更新的字段必须同时加进这个 map。
	fields := map[string]any{
		"remark":  group.Remark,
		"domains": group.Domains,
	}

	// 订阅地址变了：旧订阅内容来自另一个来源，继续拿它分流是「用错误的数据
	// 生效」，比规则暂时不生效更危险。清空并把 LastUpdatedAt 置 0，
	// SubscriptionJob 的「从未成功过」分支会在下一个检查窗口拉取新地址。
	if old.SubscribeUrl != group.SubscribeUrl {
		fields["subscribe_url"] = group.SubscribeUrl
		fields["subscribed_domains"] = ""
		fields["last_updated_at"] = 0
		fields["last_error"] = ""
		fields["last_skipped"] = 0
	}

	return database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Updates(fields).Error
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
	domains, skipped, err := ParseSubscription(raw)
	if err != nil {
		return s.recordFailure(group, err)
	}
	// 落库前过真实 xray 校验。ValidateDomains 自身是 fail open 的：
	// 二进制缺失、超时等一律放行，只有 xray 明确判定非法才拦。
	if err := ValidateDomains(domains); err != nil {
		return s.recordFailure(group, err)
	}
	encoded, err := EncodeDomains(domains)
	if err != nil {
		return s.recordFailure(group, err)
	}

	// 用 map 而不是 struct：GORM 的 struct 更新会跳过零值，
	// LastError 与 LastSkipped 清不掉。
	return database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Updates(map[string]any{
			"subscribed_domains": encoded,
			"last_updated_at":    time.Now().UnixMilli(),
			"last_error":         "",
			"last_skipped":       skipped,
		}).Error
}

// recordFailure 只写失败原因，绝不动 SubscribedDomains 与 LastUpdatedAt。
//
// 清空订阅域名会让合并结果为空 → buildRule 跳过整条规则 → 本该走指定节点或被
// 封禁的流量静默退回直连。上游改格式、URL 失效返回 404 页面、CDN 返回空响应
// 都会走到这里，而它们都不该导致分流失效。宁可用旧数据。
func (s *DomainGroupService) recordFailure(group *model.DomainGroup, cause error) error {
	err := database.GetDB().Model(model.DomainGroup{}).Where("id = ?", group.Id).
		Update("last_error", cause.Error()).Error
	if err != nil {
		logger.Warning("record subscription failure err:", err)
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

// MergeDomains 把手工域名与订阅域名合并去重。
//
// 顺序是确定的：手工在前、订阅在后，各自保持原顺序，重复项保留首次出现。
// 这一点不能含糊——注入器的第四条不变量要求生成逐字节确定，顺序一旦不稳定，
// Config.Equals 恒为 false，每 10 秒的重启 cron 会不停重启 xray。
func MergeDomains(manual, subscribed []string) []string {
	merged := make([]string, 0, len(manual)+len(subscribed))
	seen := make(map[string]bool, len(manual)+len(subscribed))
	for _, list := range [][]string{manual, subscribed} {
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
