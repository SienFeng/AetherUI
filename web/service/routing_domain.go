package service

import (
	"encoding/json"
	"strings"

	"a-ui/database"
	"a-ui/database/model"
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
	old.Remark = group.Remark
	old.Domains = group.Domains
	db := database.GetDB()
	return db.Save(old).Error
}

func (s *DomainGroupService) Del(id int) error {
	db := database.GetDB()
	return db.Delete(model.DomainGroup{}, id).Error
}
