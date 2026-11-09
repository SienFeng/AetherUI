package service

import (
	"encoding/json"
	"strings"
	"time"

	"gorm.io/gorm"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
)

// RegionPassService 管理出差通行证（model.InboundRegionPass）。
//
// 无状态；置重启标志由调用方（controller / job）负责。
type RegionPassService struct{}

// effectiveRegions 是入站实际生效的地区：存储的地区 ∪ 活动通行证。
// suspended 为真表示通行证暂停了地区限制，本轮不为该入站生成地区规则。
//
// 只用于生成规则与判断拦截；listen 的处理（model.GenXrayInboundConfig）只看存储
// 的 Regions，发通行证不改监听拓扑。
func effectiveRegions(in *model.Inbound, pass *model.InboundRegionPass) ([]string, bool, error) {
	stored, err := DecodeRegions(in.Regions)
	if err != nil || pass == nil || len(stored) == 0 {
		return stored, false, err
	}
	extra, err := DecodeRegions(pass.Regions)
	if err != nil {
		return nil, false, common.NewError("通行证的地区数据损坏:", err)
	}
	for _, r := range extra {
		if r == model.RegionPassAll {
			return nil, true, nil
		}
	}
	return normalizeRegions(append(stored, extra...)), false, nil
}

// Grant 为入站发一张通行证，同一事务里以 replaced 结束旧的活动通行证。
// regions 只能是 ["*"]，或者全部是 IP 库里的省名。
func (s *RegionPassService) Grant(inboundId int, regions []string, until int64, auto bool, now time.Time) (*model.InboundRegionPass, error) {
	return s.grant(inboundId, regions, until, auto, false, now)
}

// GrantAuto 是自动出差放行的发放：比较后设置，入站此刻已有任何活动通行证就放弃。
// 自动放行从判定到发放之间要读好几次库，管理员可能正好在这期间发了手动通行证。
func (s *RegionPassService) GrantAuto(inboundId int, province string, until int64, now time.Time) (*model.InboundRegionPass, error) {
	return s.grant(inboundId, []string{province}, until, true, true, now)
}

func (s *RegionPassService) grant(inboundId int, regions []string, until int64, auto, requireNone bool, now time.Time) (*model.InboundRegionPass, error) {
	in, err := (&InboundService{}).GetInbound(inboundId)
	if err != nil {
		return nil, err
	}
	if !model.HasRegions(in.Regions) {
		return nil, common.NewError("该入站没有设置地区限制，不需要通行证")
	}
	if until <= now.UnixMilli() {
		return nil, common.NewError("通行证的到期时间必须晚于现在")
	}
	regions = normalizeRegions(regions)
	if len(regions) == 0 {
		return nil, common.NewError("请选择通行证允许的地区")
	}
	if !(len(regions) == 1 && regions[0] == model.RegionPassAll) {
		if err := checkPassProvinces(regions); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(regions)
	if err != nil {
		return nil, err
	}
	pass := &model.InboundRegionPass{
		InboundId: inboundId, Regions: string(encoded), Auto: auto,
		GrantedAt: now.UnixMilli(), Until: until,
	}
	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		if requireNone {
			var n int64
			if err := tx.Model(&model.InboundRegionPass{}).
				Where("inbound_id = ? AND ended_at = 0", inboundId).Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return common.NewError("入站已有生效中的通行证，放弃自动发放")
			}
		}
		if err := endActive(tx, inboundId, model.RegionPassReplaced, now); err != nil {
			return err
		}
		return tx.Create(pass).Error
	})
	if err != nil {
		return nil, err
	}
	return pass, nil
}

func checkPassProvinces(regions []string) error {
	db := (&IPDBService{}).DB()
	if db == nil {
		return common.NewError("IP 归属地库未加载，只能发「暂停地区限制」的通行证")
	}
	known := map[string]bool{}
	for _, p := range db.Provinces() {
		known[p] = true
	}
	var unknown []string
	for _, r := range regions {
		if !known[r] {
			unknown = append(unknown, r)
		}
	}
	if len(unknown) > 0 {
		return common.NewError("这些地区不在 IP 归属地库里:", strings.Join(unknown, "、"))
	}
	return nil
}

func endActive(tx *gorm.DB, inboundId int, reason string, now time.Time) error {
	return tx.Model(&model.InboundRegionPass{}).
		Where("inbound_id = ? AND ended_at = 0", inboundId).
		Updates(map[string]any{"ended_at": now.UnixMilli(), "end_reason": reason}).Error
}

// Revoke 结束入站的活动通行证，返回是否真的结束了一张。
func (s *RegionPassService) Revoke(inboundId int, reason string, now time.Time) (bool, error) {
	res := database.GetDB().Model(&model.InboundRegionPass{}).
		Where("inbound_id = ? AND ended_at = 0", inboundId).
		Updates(map[string]any{"ended_at": now.UnixMilli(), "end_reason": reason})
	return res.RowsAffected > 0, res.Error
}

// RevokePass 只结束指定的那张通行证（且仍在生效时），返回是否真的结束了。
// 自动出差放行用它：按判定时看到的那张收回，不会连带收回管理员刚发的手动通行证。
func (s *RegionPassService) RevokePass(passId int, reason string, now time.Time) (bool, error) {
	res := database.GetDB().Model(&model.InboundRegionPass{}).
		Where("id = ? AND ended_at = 0", passId).
		Updates(map[string]any{"ended_at": now.UnixMilli(), "end_reason": reason})
	return res.RowsAffected > 0, res.Error
}

// Renew 把活动通行证的到期时间改为 until。
func (s *RegionPassService) Renew(passId int, until int64) error {
	return database.GetDB().Model(&model.InboundRegionPass{}).
		Where("id = ? AND ended_at = 0", passId).Update("until", until).Error
}

// Active 返回所有活动通行证（入站 id → 通行证）。
func (s *RegionPassService) Active() (map[int]*model.InboundRegionPass, error) {
	db := database.GetDB()
	if db == nil {
		return nil, nil
	}
	var rows []*model.InboundRegionPass
	if err := db.Where("ended_at = 0").Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int]*model.InboundRegionPass, len(rows))
	for _, r := range rows {
		out[r.InboundId] = r // 按 id 升序，万一有多张以最新的为准
	}
	return out, nil
}

// ActiveOf 返回某入站的活动通行证，没有时为 nil。
func (s *RegionPassService) ActiveOf(inboundId int) (*model.InboundRegionPass, error) {
	var rows []model.InboundRegionPass
	err := database.GetDB().Where("inbound_id = ? AND ended_at = 0", inboundId).
		Order("id desc").Limit(1).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// List 返回某入站最近的通行证记录，新的在前。
func (s *RegionPassService) List(inboundId, limit int) ([]model.InboundRegionPass, error) {
	rows := []model.InboundRegionPass{}
	err := database.GetDB().Where("inbound_id = ?", inboundId).
		Order("id desc").Limit(limit).Find(&rows).Error
	return rows, err
}

// Since 返回某入站在 since（毫秒）之后发放或仍在生效的通行证，供自动放行计算频率。
func (s *RegionPassService) Since(inboundId int, since int64) ([]model.InboundRegionPass, error) {
	rows := []model.InboundRegionPass{}
	err := database.GetDB().
		Where("inbound_id = ? AND (granted_at >= ? OR ended_at = 0 OR ended_at >= ?)", inboundId, since, since).
		Order("id").Find(&rows).Error
	return rows, err
}

// ExpireDue 以 expired 结束所有已到期的活动通行证，返回结束的张数。
func (s *RegionPassService) ExpireDue(now time.Time) (int64, error) {
	res := database.GetDB().Model(&model.InboundRegionPass{}).
		Where("ended_at = 0 AND until <= ?", now.UnixMilli()).
		Updates(map[string]any{"ended_at": now.UnixMilli(), "end_reason": model.RegionPassExpired})
	return res.RowsAffected, res.Error
}

// DeleteByInbound 在删除入站的事务里删掉它的全部通行证。
func (s *RegionPassService) DeleteByInbound(tx *gorm.DB, inboundId int) error {
	return tx.Where("inbound_id = ?", inboundId).Delete(&model.InboundRegionPass{}).Error
}

// activePassesOrNil 供生成期使用：读不出时按「没有通行证」处理并告警。
// 失败方向是收紧（客户按存储的地区被限制），不会把限制静默放开。
func activePassesOrNil() map[int]*model.InboundRegionPass {
	passes, err := (&RegionPassService{}).Active()
	if err != nil {
		logger.Warning("读取出差通行证失败，本轮按无通行证生成地区规则:", err)
		return nil
	}
	return passes
}
