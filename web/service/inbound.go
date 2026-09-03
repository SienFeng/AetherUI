package service

import (
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"time"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/xray"
)

type InboundService struct {
}

func (s *InboundService) GetInbounds(userId int) ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Where("user_id = ?", userId).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return inbounds, nil
}

func (s *InboundService) GetAllInbounds() ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Find(&inbounds).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}
	return inbounds, nil
}

func (s *InboundService) checkPortExist(port int, ignoreId int) (bool, error) {
	db := database.GetDB()
	db = db.Model(model.Inbound{}).Where("port = ?", port)
	if ignoreId > 0 {
		db = db.Where("id != ?", ignoreId)
	}
	var count int64
	err := db.Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// normalizeInboundRegions 把地区字段归一成排序去重后的规范形式，
// 顺便挡住损坏的数据。
//
// 在落库时做而不是在生成配置时做：等到生成配置才报错的话，错误指向的是
// 「配置生成失败」，管理员很难联想到是某个入站的地区字段坏了。
func normalizeInboundRegions(inbound *model.Inbound) error {
	regions, err := DecodeRegions(inbound.Regions)
	if err != nil {
		return common.NewError("地区数据格式不正确:", err)
	}
	encoded, err := EncodeRegions(regions)
	if err != nil {
		return err
	}
	inbound.Regions = encoded
	return nil
}

// validateInbound 把入站交给真实 xray 校验。停用的入站同样要校验：
// 现在放过去，等管理员某天启用它，炸的是所有用户。
func validateInbound(inbound *model.Inbound, replacedTag string) error {
	encoded, err := json.Marshal(inbound.GenXrayInboundConfig())
	if err != nil {
		return err
	}
	var ib map[string]any
	if err := json.Unmarshal(encoded, &ib); err != nil {
		return err
	}
	return ValidateInboundReplacing(ib, replacedTag)
}

func (s *InboundService) AddInbound(inbound *model.Inbound) error {
	if err := normalizeInboundRegions(inbound); err != nil {
		return err
	}
	exist, err := s.checkPortExist(inbound.Port, 0)
	if err != nil {
		return err
	}
	if exist {
		return common.NewError("端口已存在:", inbound.Port)
	}
	// 校验放在落库之前，因此不需要事务回滚。
	if err := validateInbound(inbound, ""); err != nil {
		return err
	}
	db := database.GetDB()
	return db.Save(inbound).Error
}

func (s *InboundService) AddInbounds(inbounds []*model.Inbound) error {
	for _, inbound := range inbounds {
		exist, err := s.checkPortExist(inbound.Port, 0)
		if err != nil {
			return err
		}
		if exist {
			return common.NewError("端口已存在:", inbound.Port)
		}
	}

	db := database.GetDB()
	tx := db.Begin()
	var err error
	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()

	for _, inbound := range inbounds {
		err = tx.Save(inbound).Error
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *InboundService) DelInbound(id int) error {
	// 分流规则存的是入站 id 外键，而 SQLite 会复用被删除的自增 id：
	// 不拦住这里，孤儿规则会在下一个入站建出来时静默绑到新用户身上。
	ruleService := RoutingRuleService{}
	if err := ruleService.CheckInboundRefs(id); err != nil {
		return err
	}
	// 访问日志按入站 id 存，同样会被 id 复用坑到：不清掉的话，
	// 下一个建出来的入站会看到上一个用户访问过哪些网站。
	//
	// 清理失败只告警不阻断：日志库磁盘满、只读这类问题不该把管理员卡在
	// 删不掉入站的状态里。残留记录由每小时一次的 PruneOrphans 兜底清除
	// （面板启动时也跑一次），所以最坏情况是一个小时的窗口。
	accessLogService := AccessLogService{}
	if err := accessLogService.DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的访问日志失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	db := database.GetDB()
	return db.Delete(model.Inbound{}, id).Error
}

func (s *InboundService) GetInbound(id int) (*model.Inbound, error) {
	db := database.GetDB()
	inbound := &model.Inbound{}
	err := db.Model(model.Inbound{}).First(inbound, id).Error
	if err != nil {
		return nil, err
	}
	return inbound, nil
}

func (s *InboundService) UpdateInbound(inbound *model.Inbound) error {
	if err := normalizeInboundRegions(inbound); err != nil {
		return err
	}
	exist, err := s.checkPortExist(inbound.Port, inbound.Id)
	if err != nil {
		return err
	}
	if exist {
		return common.NewError("端口已存在:", inbound.Port)
	}

	oldInbound, err := s.GetInbound(inbound.Id)
	if err != nil {
		return err
	}
	oldInbound.Up = inbound.Up
	oldInbound.Down = inbound.Down
	oldInbound.Total = inbound.Total
	oldInbound.Remark = inbound.Remark
	oldInbound.Enable = inbound.Enable
	oldInbound.ExpiryTime = inbound.ExpiryTime
	oldInbound.ConcurrencyLimit = inbound.ConcurrencyLimit
	oldInbound.Regions = inbound.Regions
	oldInbound.UpMbit = inbound.UpMbit
	oldInbound.DownMbit = inbound.DownMbit
	oldInbound.Listen = inbound.Listen
	oldInbound.Port = inbound.Port
	oldInbound.Protocol = inbound.Protocol
	oldInbound.Settings = inbound.Settings
	oldInbound.StreamSettings = inbound.StreamSettings
	oldInbound.Sniffing = inbound.Sniffing
	// 改端口时 tag 会跟着变，摘除完整配置里那份旧的必须按**旧** tag。
	replacedTag := oldInbound.Tag
	oldInbound.Tag = fmt.Sprintf("inbound-%v", inbound.Port)

	if err := validateInbound(oldInbound, replacedTag); err != nil {
		return err
	}

	db := database.GetDB()
	return db.Save(oldInbound).Error
}

func (s *InboundService) AddTraffic(traffics []*xray.Traffic) (err error) {
	if len(traffics) == 0 {
		return nil
	}
	db := database.GetDB()
	db = db.Model(model.Inbound{})
	tx := db.Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	for _, traffic := range traffics {
		if traffic.IsInbound {
			err = tx.Where("tag = ?", traffic.Tag).
				UpdateColumn("up", gorm.Expr("up + ?", traffic.Up)).
				UpdateColumn("down", gorm.Expr("down + ?", traffic.Down)).
				Error
			if err != nil {
				return
			}
		}
	}
	return
}

func (s *InboundService) DisableInvalidInbounds() (int64, error) {
	db := database.GetDB()
	now := time.Now().Unix() * 1000
	result := db.Model(model.Inbound{}).
		Where("((total > 0 and up + down >= total) or (expiry_time > 0 and expiry_time <= ?)) and enable = ?", now, true).
		Update("enable", false)
	err := result.Error
	count := result.RowsAffected
	return count, err
}
