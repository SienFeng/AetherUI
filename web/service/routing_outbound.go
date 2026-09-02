package service

import (
	"encoding/json"
	"fmt"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
	"a-ui/util/link"
)

type OutboundNodeService struct {
}

func (s *OutboundNodeService) GetAll() ([]*model.OutboundNode, error) {
	db := database.GetDB()
	nodes := make([]*model.OutboundNode, 0)
	err := db.Model(model.OutboundNode{}).Order("id asc").Find(&nodes).Error
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// GetEnabled 按 Id 升序返回启用的节点。顺序固定是配置生成确定性的前提。
func (s *OutboundNodeService) GetEnabled() ([]*model.OutboundNode, error) {
	db := database.GetDB()
	nodes := make([]*model.OutboundNode, 0)
	err := db.Model(model.OutboundNode{}).Where("enable = ?", true).Order("id asc").Find(&nodes).Error
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

func (s *OutboundNodeService) Get(id int) (*model.OutboundNode, error) {
	db := database.GetDB()
	node := &model.OutboundNode{}
	err := db.Model(model.OutboundNode{}).First(node, id).Error
	if err != nil {
		return nil, err
	}
	return node, nil
}

// allocTag 生成一个未被占用的、带 a-ui- 前缀的 tag。
//
// 不能用自增 Id 拼 tag：Tag 有唯一约束，必须在 INSERT 之前就确定，
// 而那一刻 Id 尚未分配。
//
// 注意 link.SuggestTag 只在 remark 为空时才拿 idx 兜底；remark 非空时
// 它对任何 idx 都返回同一个值，所以重名必须靠这里自己追加序号。
// 又，link.SlugRemark 的正则是 [^\p{L}\p{N}]+，会保留中文，生成的 tag
// 形如 a-ui-香港-b-节点 —— 已实测 xray 接受非 ASCII tag，无需转写。
func (s *OutboundNodeService) allocTag(remark string) (string, error) {
	db := database.GetDB()
	base := link.SuggestTag(model.OutboundTagPrefix, remark, 1)
	for idx := 1; idx < 1000; idx++ {
		candidate := base
		if idx > 1 {
			candidate = fmt.Sprintf("%s-%d", base, idx)
		}
		var count int64
		if err := db.Model(model.OutboundNode{}).Where("tag = ?", candidate).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
	}
	return "", common.NewError("无法为该备注分配唯一 tag，请更换备注")
}

// persist 把解析好的 outbound 写库，并把 tag 强制改写成本表分配的值。
func (s *OutboundNodeService) persist(ob map[string]any, protocol, remark string) (*model.OutboundNode, error) {
	tag, err := s.allocTag(remark)
	if err != nil {
		return nil, err
	}
	ob["tag"] = tag
	encoded, err := json.Marshal(ob)
	if err != nil {
		return nil, err
	}
	node := &model.OutboundNode{
		Tag:      tag,
		Remark:   remark,
		Protocol: protocol,
		Config:   string(encoded),
		Enable:   true,
	}
	db := database.GetDB()
	if err := db.Save(node).Error; err != nil {
		return nil, err
	}
	return node, nil
}

// AddFromLink 解析分享链接并落库。
func (s *OutboundNodeService) AddFromLink(rawLink string, remark string) (*model.OutboundNode, error) {
	result, err := link.ParseLink(rawLink)
	if err != nil {
		return nil, common.NewError("解析分享链接失败:", err)
	}
	protocol, _ := result.Outbound["protocol"].(string)
	if protocol == "" {
		return nil, common.NewError("解析结果缺少 protocol 字段")
	}
	return s.persist(map[string]any(result.Outbound), protocol, remark)
}

// AddFromJSON 直接接收一段 xray outbound JSON（高级模式）。
func (s *OutboundNodeService) AddFromJSON(cfg string, remark string) (*model.OutboundNode, error) {
	var ob map[string]any
	if err := json.Unmarshal([]byte(cfg), &ob); err != nil {
		return nil, common.NewError("outbound JSON 格式错误:", err)
	}
	protocol, _ := ob["protocol"].(string)
	if protocol == "" {
		return nil, common.NewError("outbound JSON 缺少 protocol 字段")
	}
	return s.persist(ob, protocol, remark)
}

// Update 只允许改备注、启用状态和配置内容，Tag 一经分配不可变——
// 改 tag 会让所有引用它的规则悬空，而 xray 对此不报错。
func (s *OutboundNodeService) Update(node *model.OutboundNode) error {
	old, err := s.Get(node.Id)
	if err != nil {
		return err
	}
	if node.Config != "" && node.Config != old.Config {
		var ob map[string]any
		if err := json.Unmarshal([]byte(node.Config), &ob); err != nil {
			return common.NewError("outbound JSON 格式错误:", err)
		}
		ob["tag"] = old.Tag
		encoded, err := json.Marshal(ob)
		if err != nil {
			return err
		}
		old.Config = string(encoded)
		if p, ok := ob["protocol"].(string); ok && p != "" {
			old.Protocol = p
		}
	}
	old.Remark = node.Remark
	old.Enable = node.Enable
	db := database.GetDB()
	return db.Save(old).Error
}

func (s *OutboundNodeService) Del(id int) error {
	ruleService := RoutingRuleService{}
	if err := ruleService.CheckOutboundRefs(id); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Delete(model.OutboundNode{}, id).Error
}
