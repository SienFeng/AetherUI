package service

import (
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"strings"
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

// provinceLister 是 checkRegionsUsable 需要的 IP 库能力，测试里可替换。
type provinceLister interface {
	Provinces() []string
}

// checkRegionsUsable 在保存时拦住「下发时才会出问题」的地区集合。
//
// 只在地区集合真的变了时才校验：省名可能因 IP 库更新而失效，那种存量入站
// 若连改个备注都被拒，管理员就被卡在一个与他这次操作无关的错误上。
// db 为 nil 表示 IP 库未加载。
func checkRegionsUsable(newRegions, oldRegions string, db provinceLister, src provinceCIDRSource) error {
	next, err := DecodeRegions(newRegions)
	if err != nil {
		return common.NewError("地区数据格式不正确:", err)
	}
	prev, _ := DecodeRegions(oldRegions)
	if len(next) == 0 || strings.Join(next, "\x00") == strings.Join(prev, "\x00") {
		return nil
	}
	if db == nil {
		return common.NewError("IP 归属地库未加载，无法设置地区限制；请先到「面板设置 → IP 归属地库」更新")
	}
	known := map[string]bool{}
	for _, p := range db.Provinces() {
		known[p] = true
	}
	var unknown []string
	for _, p := range next {
		if !known[p] {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		return common.NewError("这些地区不在 IP 归属地库里:", strings.Join(unknown, "、"))
	}
	if len(src.CIDRsOfProvinces(next)) == 0 {
		return common.NewError("这些地区在当前合并口径下没有任何 IP 段:", strings.Join(next, "、"))
	}
	return nil
}

// regionsUsable 用当前 IP 库与合并口径调 checkRegionsUsable。
func regionsUsable(newRegions, oldRegions string) error {
	db := (&IPDBService{}).DB()
	if db == nil {
		return checkRegionsUsable(newRegions, oldRegions, nil, nil)
	}
	return checkRegionsUsable(newRegions, oldRegions, db, regionCIDRSource(db, &SettingService{}))
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
	if err := regionsUsable(inbound.Regions, ""); err != nil {
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
	if err := s.checkInboundPortUsable(inbound, nil); err != nil {
		return err
	}
	// 校验放在落库之前，因此不需要事务回滚。
	if err := validateInbound(inbound, ""); err != nil {
		return err
	}
	db := database.GetDB()
	// 事务包住「落库 + 扩散」：扩散失败若放行，就是一个用户静默地没进规则，
	// 正是「自动应用于新增用户」这个功能要消灭的失效。宁可整个失败让管理员
	// 重试。inbound.Id 是自增的，必须 Save 之后才能拿来扩散。
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(inbound).Error; err != nil {
			return err
		}
		return (&RoutingRuleService{}).AttachInbound(tx, inbound.Id)
	})
	if err == nil {
		clearInboundLeftovers(inbound.Id)
	}
	return err
}

// clearInboundLeftovers 在新入站拿到 id 之后，清掉其它库里这个 id 的残留行。
//
// 删除入站时这些库的清理失败只告警（由每小时的 PruneOrphans 兜底），而 SQLite 会复用自增 id：这一小时里新建的
// 入站拿到同一个 id，残留行就不再是孤儿，PruneOrphans 也救不回来了——新用户继承上一个用户的访问记录、
// 用量曲线、按来源用量与共享风险分，自动设置地区还会拿上一个用户的省份来设置这个新用户。新入站此刻还
// 没进核心，不会有属于它的新数据被误删。失败只告警：这不是创建入站本身的一部分。
func clearInboundLeftovers(id int) {
	cleanups := []struct {
		what string
		run  func(int) error
	}{
		{"访问记录", (&AccessLogService{}).DeleteByInbound},
		{"用量历史", (&TrafficHistoryService{}).DeleteByInbound},
		{"域名统计", (&DomainStatService{}).DeleteByInbound},
		{"计量池", (&MeterPoolService{}).DeleteByInbound},
		{"按来源用量", (&SharingService{}).DeleteByInbound},
		{"共享风险快照", (&SharingRiskService{}).DeleteByInbound},
		{"流量暴涨限速", func(id int) error {
			return database.GetDB().Where("inbound_id = ?", id).Delete(&model.InboundThrottle{}).Error
		}},
	}
	for _, c := range cleanups {
		if err := c.run(id); err != nil {
			logger.Warning("新建入站：清理复用 id 的残留", c.what, "失败, id:", id, "err:", err)
		}
	}
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
		if err := s.checkInboundPortUsable(inbound, nil); err != nil {
			return err
		}
	}

	// 事务提交之后才清理残留（clearInboundLeftovers 动的是其它库），所以用 Transaction 而不是 defer 提交。
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, inbound := range inbounds {
			if err := tx.Save(inbound).Error; err != nil {
				return err
			}
			// v2ui 迁移路径。只挂 AddInbound 会漏掉它，spec §4.3 三条路径要齐。
			if err := (&RoutingRuleService{}).AttachInbound(tx, inbound.Id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, inbound := range inbounds {
		clearInboundLeftovers(inbound.Id)
	}
	return nil
}

func (s *InboundService) DelInbound(id int) error {
	// 分流规则存的是入站 id 外键，而 SQLite 会复用被删除的自增 id：删完不把
	// 引用清干净，孤儿规则会在下一个入站建出来时静默绑到新用户身上。清理由
	// 下面那个事务里的 DetachInbound 完成。
	//
	// 这里先算一次只为快速失败：规则数据损坏时无从判断引用关系，必须赶在下面
	// 那一串清理动手之前退出，否则会留下「访问日志、用量历史、封禁都已经删了，
	// 入站却还在」这种谁都没要求过的状态。真正生效的判定是事务里的那一次。
	ruleService := RoutingRuleService{}
	if _, err := ruleService.PlanInboundDetach(id); err != nil {
		return err
	}
	db := database.GetDB()
	// 清理规则引用与删除入站必须原子：只成一半的两种残留——规则没了入站还在、
	// 入站没了规则还指着它——都不会有任何一层报错。
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := ruleService.DetachInbound(tx, id); err != nil {
			return err
		}
		// 封禁同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来的入站会凭空继承上一个用户的封禁名单。
		// 放在同一个事务里：改动前它在事务之前删，事务失败时入站还在、封禁却没了，被封的 IP 直接又能连上。
		if err := tx.Where("inbound_id = ?", id).Delete(&model.IPBan{}).Error; err != nil {
			return err
		}
		// 出差通行证同样按入站 id 存：不删的话下一个建出来的入站会继承上一个
		// 用户的放行地区。放在同一个事务里，失败即阻断。
		if err := (&RegionPassService{}).DeleteByInbound(tx, id); err != nil {
			return err
		}
		if err := (&AutoRegionService{}).DeleteByInbound(tx, id); err != nil {
			return err
		}
		// 流量暴涨限速单同样按入站 id 存：不删的话下一个建出来的入站会继承上一个用户的限速。
		if err := tx.Where("inbound_id = ?", id).Delete(&model.InboundThrottle{}).Error; err != nil {
			return err
		}
		return tx.Delete(model.Inbound{}, id).Error
	})
	if err != nil {
		return err
	}
	// 其他库里按入站 id 存的数据在入站真的删掉之后再清：改动前先清，事务失败时入站还在、历史却没了。
	// 这里失败只告警（下面各条说明），新建入站时 clearInboundLeftovers 与每小时的 PruneOrphans 兜底。
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
	// 域名统计同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来
	// 的入站会看到上一个用户访问过哪些网站。失败只告警不阻断，理由同上，
	// 残留由每小时一次的 PruneOrphans 兜底。
	if err := (&DomainStatService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的域名统计失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	// 计量池同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个建出来的
	// 入站会继承上一个用户的计量域名，生成出一批指向别人域名的计量出站与
	// 规则。失败只告警不阻断，理由同上，残留由每小时一次的 PruneOrphans 兜底。
	if err := (&MeterPoolService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的计量池失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	// 共享检测的并存记录同样按入站 id 存，同样会被 id 复用坑到：不清的话
	// 下一个建出来的入站会继承上一个用户的并存记录，被标成「疑似共享」。
	// 失败只告警不阻断，理由同上，残留由每小时一次的 PruneOrphans 兜底。
	if err := (&SharingService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的共享检测记录失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	// 共享风险快照同样按入站 id 存，同样会被 id 复用坑到：不清的话下一个
	// 建出来的入站会继承上一个用户的风险分——而这次继承来的还是一个带
	// 「严重」字样的红色标记。失败只告警不阻断，理由同上，残留由每小时一次
	// 的 PruneOrphans 兜底。
	if err := (&SharingRiskService{}).DeleteByInbound(id); err != nil {
		logger.Warning("清理入站的共享风险快照失败, 将由定时清理兜底, id:", id, "err:", err)
	}
	return nil
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
	return s.UpdateInboundFrom(inbound, InboundEditBase{})
}

// UpdateInboundFrom 写入编辑表单。base 是页面打开那一刻看到的、会被任务改写的字段（见 InboundEditBase）：
// 请求里的值与 base 相同，说明管理员没动它，保留库里此刻的值。
func (s *InboundService) UpdateInboundFrom(inbound *model.Inbound, base InboundEditBase) error {
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
	if err := s.checkInboundPortUsable(inbound, oldInbound); err != nil {
		return err
	}
	orig := *oldInbound
	// up/down 从不取自表单：流量任务每 10 秒在累加，页面上的是打开那一刻的快照，写回去就是把用量倒回去
	// ——超额被停的人被放回来、周期重置之后又被旧用量停掉。清零走 ResetInboundTraffic。
	oldInbound.Total = inbound.Total
	oldInbound.Remark = inbound.Remark
	oldEnable := oldInbound.Enable
	oldInbound.Enable = base.pick(base.Enable, inbound.Enable, oldInbound.Enable)
	oldInbound.ExpiryTime = inbound.ExpiryTime
	oldInbound.ConcurrencyLimit = inbound.ConcurrencyLimit
	oldInbound.AllowServerSources = inbound.AllowServerSources
	newAutoTravel := base.pick(base.AutoTravel, inbound.AutoTravel, oldInbound.AutoTravel)
	autoTravelTurnedOff := oldInbound.AutoTravel && !newAutoTravel
	oldInbound.AutoTravel = newAutoTravel
	if err := regionsUsable(inbound.Regions, oldInbound.Regions); err != nil {
		return err
	}
	regionsChanged := !sameRegions(inbound.Regions, oldInbound.Regions)
	hadRegions := model.HasRegions(oldInbound.Regions)
	oldInbound.Regions = inbound.Regions
	oldInbound.UpMbit = inbound.UpMbit
	oldInbound.DownMbit = inbound.DownMbit
	// 只有启用状态被管理员改动过（或现在是启用的），「因超流量被自动停用」的理由才不再成立：
	// 手动停用（开→关）之后不能在下一个重置周期自己活过来；重新启用后标记自然作废。
	// 只改备注、限速等其他字段时保留标记——修复前任何编辑都清掉它，管理员改个备注，
	// 这个入站就悄悄失去了下个周期自动恢复的资格，列表上也看不出区别。
	// 想让它保持停用、不再自动恢复：先启用再停用一次即可（列表上会显示「手动停用」）。
	if oldInbound.Enable || oldInbound.Enable != oldEnable {
		oldInbound.DisabledByTraffic = false
	}
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
	// 模式没变、订阅周期的日号随到期时间变了（28 号改成 5 号）：同样不能当场清一次，见 raisedResetFloor。
	// 读不出面板时区就不顶——ResetDueTraffic 那时同样读不出、不会清。
	if orig.TrafficResetMode == oldInbound.TrafficResetMode {
		if loc, err := (&SettingService{}).GetTimeLocation(); err == nil {
			oldInbound.LastResetAt = raisedResetFloor(&orig, loc, oldInbound, loc, time.Now())
		}
	}
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
	now := time.Now()
	return db.Transaction(func(tx *gorm.DB) error {
		// 在事务里读：任务可能刚在 GetInbound 之后自动设置了这个入站。
		if err := checkStaleAutoRegionWrite(tx, oldInbound.Id, oldInbound.Regions, now); err != nil {
			return err
		}
		if err := tx.Save(oldInbound).Error; err != nil {
			return err
		}
		// 关掉了自动出差放行：生效中的自动通行证一并结束——之后再没有谁按「家里恢复使用」去收回
		// 或续期它，它会不受管地一直放行到到期。管理员手动发的不动。
		if autoTravelTurnedOff {
			if err := tx.Model(&model.InboundRegionPass{}).
				Where("inbound_id = ? AND ended_at = 0 AND auto = ?", oldInbound.Id, true).
				Updates(map[string]any{"ended_at": now.UnixMilli(), "end_reason": model.RegionPassAutoOff}).Error; err != nil {
				return err
			}
		}
		if !regionsChanged {
			return nil
		}
		// 清空了地区限制：生效中的通行证一并结束。没有地区时它本来就不起作用，但留着的话
		// 列表上还挂着一张「到期后恢复地区限制」的通行证，以后再设地区时它还会接着放行。
		if hadRegions && !model.HasRegions(oldInbound.Regions) {
			if err := endActive(tx, oldInbound.Id, model.RegionPassCleared, now); err != nil {
				return err
			}
		}
		// 管理员亲手改过地区（包括清空自动设置的那份），自动设置就不再碰这个入站。
		return (&AutoRegionService{}).MarkManual(tx, oldInbound.Id, now)
	})
}

// sameRegions 比较两个地区字段规范化之后是否相同；解析不了的按「不同」处理。
func sameRegions(a, b string) bool {
	x, errA := DecodeRegions(a)
	y, errB := DecodeRegions(b)
	return errA == nil && errB == nil && strings.Join(x, "\x00") == strings.Join(y, "\x00")
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
	// 计量出站的字节同样来自这一次拉取（reset=true 已经清零 xray 侧计数器，
	// 另起一次独立拉取会让两条链路互相偷数据）。失败只告警不阻断，理由同上。
	if err := (&DomainStatService{}).RecordMetered(traffics, time.Now()); err != nil {
		logger.Warning("记录域名计量流量失败:", err)
	}
	// 主库累加失败时增量暂存、下一轮补写，见 traffic_pending.go。
	return s.addInboundTraffic(traffics)
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
