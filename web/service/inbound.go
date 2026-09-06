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
	if err := checkTrafficResetMode(inbound); err != nil {
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
	// 用量历史同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来
	// 的入站会看到上一个用户的用量曲线。失败只告警不阻断，理由同访问日志，
	// 残留由每小时一次的 PruneOrphans 兜底。
	if err := (&TrafficHistoryService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的用量历史失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	// 共享检测的并存记录同样按入站 id 存，同样会被 id 复用坑到：不清的话
	// 下一个建出来的入站会继承上一个用户的并存记录，被标成「疑似共享」。
	// 失败只告警不阻断，理由同上，残留由每小时一次的 PruneOrphans 兜底。
	if err := (&SharingService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的共享检测记录失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	// 封禁同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来的
	// 入站会凭空继承上一个用户的封禁名单。这里失败要阻断——残留封禁会让
	// 新用户莫名其妙连不上，且没有定时任务兜底清理孤儿封禁。
	if err := (&IPBanService{}).DeleteByInbound(id); err != nil {
		return err
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
	if err := checkTrafficResetMode(inbound); err != nil {
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
	// 管理员碰过这条记录，「因超流量被自动停用」的理由就不再成立。不清的话，
	// 一个被手动停用的入站会在下一个重置周期自己活过来，而面板不会有任何提示。
	oldInbound.DisabledByTraffic = false
	// 切换重置周期时把时刻顶到当前，避免改一次设置就立刻触发一次计划外的
	// 清零：从「按订阅周期(28 号)」改到「每月 1 号」时，上次重置停在 8/28，
	// 而 9/1 这个新周期点已经过去，任务下一轮就会马上清一次。首次开启同理
	// ——那时 LastResetAt 还是 0，本周期的重置时刻必然在过去。
	//
	// 模式没变则不顶：每编辑一次入站就把周期往后推一次的话，重置永远轮不到。
	if oldInbound.TrafficResetMode != inbound.TrafficResetMode {
		oldInbound.LastResetAt = time.Now().Unix() * 1000
	}
	oldInbound.TrafficResetMode = inbound.TrafficResetMode
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
	// 先记一份分时历史，再走累加。两者写的是不同的库，主库的事务包不住
	// 时序库的写入，所以不放进同一个事务——硬凑只会得到一个原子性的假象。
	//
	// 失败只告警不阻断：inbounds.up/down 是限额与到期判定的输入，它停止
	// 累加的后果（用户超额不被停用）比图上少一段曲线严重得多。
	if err := (&TrafficHistoryService{}).Record(traffics, time.Now()); err != nil {
		logger.Warning("记录用量历史失败:", err)
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

// DisableInvalidInbounds 停用已超流量或已到期的入站，返回被停用的条数。
//
// 拆成两条语句而不是原来那一条带 OR 的：只有「因超流量被停用」的入站该在
// 下一个流量重置周期被自动拉回来（见 ResetDueTraffic），所以停用原因必须
// 落到 disabled_by_traffic 上。
//
// 顺序不能反。两者同时成立时，先执行的到期那条已经把 enable 置为 false，
// 超流量那条的 enable = true 条件不再匹配、标记也就不会打上——于是「过期」
// 这个更强的停用理由胜出，该入站不会在下一个周期自己活过来。
func (s *InboundService) DisableInvalidInbounds() (int64, error) {
	db := database.GetDB()
	now := time.Now().Unix() * 1000

	expired := db.Model(model.Inbound{}).
		Where("expiry_time > 0 and expiry_time <= ? and enable = ?", now, true).
		UpdateColumns(map[string]any{"enable": false})
	if expired.Error != nil {
		return 0, expired.Error
	}

	overQuota := db.Model(model.Inbound{}).
		Where("total > 0 and up + down >= total and enable = ?", true).
		UpdateColumns(map[string]any{"enable": false, "disabled_by_traffic": true})
	if overQuota.Error != nil {
		return expired.RowsAffected, overQuota.Error
	}
	return expired.RowsAffected + overQuota.RowsAffected, nil
}
