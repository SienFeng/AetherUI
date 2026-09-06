package service

import (
	"sort"
	"time"

	"a-ui/database"
	"a-ui/database/model"
)

// MeterPoolService 维护计量池：哪些 (入站, 注册域名) 对当前正在被计量。
//
// 与其它 service 一样是无状态空结构体，按值嵌入使用。
type MeterPoolService struct {
	settingService SettingService
}

// MeterEntry 是池里的一行，只含生成配置需要的两个字段。
type MeterEntry struct {
	InboundId int
	Domain    string
}

// Pool 返回当前在池的全部条目，按 (inboundId asc, domain asc) 排序。
//
// 排序不是可省的整洁工作：生成期用它产出计量出站与计量规则，而
// Config.Equals 对 OutboundConfigs / RouterConfig 按字节比较——顺序一抖动
// 就恒判不等，那个 10 秒的 cron 会不停重启 xray。
//
// 冷却中的行（CooldownUntil > now）不返回：它们已经退场，留在表里只是为了
// 记住冷却截止时刻。
//
// 用量库不可用时返回空切片而不是报错：整个计量功能自动停用，配置照常生成。
func (s *MeterPoolService) Pool(now time.Time) ([]MeterEntry, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return nil, nil
	}
	var rows []model.MeterDomain
	err := db.Where("cooldown_until <= ?", now.Unix()).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]MeterEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, MeterEntry{InboundId: r.InboundId, Domain: r.Domain})
	}
	// 在 Go 侧排序而不是交给 SQL 的 ORDER BY：排序键是生成期的不变量，
	// 让它和读取方式解耦，将来换查询条件也不会悄悄丢掉顺序。
	sort.Slice(out, func(i, j int) bool {
		if out[i].InboundId != out[j].InboundId {
			return out[i].InboundId < out[j].InboundId
		}
		return out[i].Domain < out[j].Domain
	})
	return out, nil
}

// DeleteByInbound 删除某入站的全部池行。
//
// 必须在删除入站时调用，理由见 PruneOrphans。
func (s *MeterPoolService) DeleteByInbound(inboundId int) error {
	db := database.GetTrafficDB()
	if db == nil {
		return nil
	}
	return db.Where("inbound_id = ?", inboundId).Delete(&model.MeterDomain{}).Error
}

// PruneOrphans 删除已不存在的入站遗留的池行，返回删除行数。
//
// 第二道防线，兜住 DelInbound 里那次删除失败或漏调的情况。两道都要有：
// SQLite 会复用被删除的自增 id，残留行会绑到下一个建出来的入站上，那时
// 引用不再悬空，面板会为一个全新用户生成一批指向别人域名的计量出站与规则，
// 而配置与界面都渲染得完全正常。
func (s *MeterPoolService) PruneOrphans() (int64, error) {
	db := database.GetTrafficDB()
	if db == nil {
		return 0, nil
	}
	var ids []int
	if err := database.GetDB().Model(model.Inbound{}).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	tx := db.Where("inbound_id != 0")
	if len(ids) > 0 {
		tx = tx.Where("inbound_id not in ?", ids)
	}
	result := tx.Delete(&model.MeterDomain{})
	return result.RowsAffected, result.Error
}
