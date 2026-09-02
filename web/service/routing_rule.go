package service

import (
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
)

type RoutingRuleService struct {
	domainGroupService DomainGroupService
	outboundService    OutboundNodeService
}

func (s *RoutingRuleService) GetAll() ([]*model.RoutingRule, error) {
	db := database.GetDB()
	rules := make([]*model.RoutingRule, 0)
	err := db.Model(model.RoutingRule{}).Order("priority asc, id asc").Find(&rules).Error
	if err != nil {
		return nil, err
	}
	return rules, nil
}

// GetEnabled 按 priority、id 升序返回启用的规则。
// 这个顺序是配置生成确定性的前提，不可改成不稳定的排序。
func (s *RoutingRuleService) GetEnabled() ([]*model.RoutingRule, error) {
	db := database.GetDB()
	rules := make([]*model.RoutingRule, 0)
	err := db.Model(model.RoutingRule{}).Where("enable = ?", true).
		Order("priority asc, id asc").Find(&rules).Error
	if err != nil {
		return nil, err
	}
	return rules, nil
}

func (s *RoutingRuleService) Get(id int) (*model.RoutingRule, error) {
	db := database.GetDB()
	rule := &model.RoutingRule{}
	err := db.Model(model.RoutingRule{}).First(rule, id).Error
	if err != nil {
		return nil, err
	}
	return rule, nil
}

// validate 在写库前挡住会生成残缺规则的输入。
func (s *RoutingRuleService) validate(rule *model.RoutingRule) error {
	if _, err := s.domainGroupService.Get(rule.DomainGroupId); err != nil {
		return common.NewError("域名组不存在:", rule.DomainGroupId)
	}
	switch rule.Action {
	case model.ActionBlock:
		return nil
	case model.ActionProxy:
		if rule.OutboundId <= 0 {
			return common.NewError("走节点的规则必须指定出站节点")
		}
		if _, err := s.outboundService.Get(rule.OutboundId); err != nil {
			return common.NewError("出站节点不存在:", rule.OutboundId)
		}
		return nil
	default:
		return common.NewError("未知的动作:", rule.Action)
	}
}

func (s *RoutingRuleService) Add(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Save(rule).Error
}

func (s *RoutingRuleService) Update(rule *model.RoutingRule) error {
	if err := s.validate(rule); err != nil {
		return err
	}
	old, err := s.Get(rule.Id)
	if err != nil {
		return err
	}
	old.Remark = rule.Remark
	old.InboundId = rule.InboundId
	old.DomainGroupId = rule.DomainGroupId
	old.Action = rule.Action
	old.OutboundId = rule.OutboundId
	old.Priority = rule.Priority
	old.Enable = rule.Enable
	db := database.GetDB()
	return db.Save(old).Error
}

func (s *RoutingRuleService) Del(id int) error {
	db := database.GetDB()
	return db.Delete(model.RoutingRule{}, id).Error
}

// CheckDomainGroupRefs 在删除域名组前调用。域名组一旦消失，引用它的规则
// domain 会变成空列表——而 xray 把空条件当作「无限制」，规则会从
// 「访问这批域名走某节点」退化成「该入站全部流量走某节点」，且不报错。
func (s *RoutingRuleService) CheckDomainGroupRefs(groupId int) error {
	db := database.GetDB()
	var count int64
	err := db.Model(model.RoutingRule{}).Where("domain_group_id = ?", groupId).Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return common.NewError("该域名组仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}

// CheckOutboundRefs 在删除出站节点前调用。出站消失后规则会静默回落到
// 默认出站（直连），封禁与分流都会失效且无任何报错。
func (s *RoutingRuleService) CheckOutboundRefs(outboundId int) error {
	db := database.GetDB()
	var count int64
	err := db.Model(model.RoutingRule{}).
		Where("outbound_id = ? and action = ?", outboundId, model.ActionProxy).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		return common.NewError("该出站节点仍被", count, "条分流规则引用，请先删除这些规则")
	}
	return nil
}
