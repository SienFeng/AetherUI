package service

import (
	"time"

	"gorm.io/gorm"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
)

// InboundEditBase 是浏览器打开编辑框（或拨动开关）那一刻看到的、会被后台任务改写的字段。
//
// 入站列表只在打开页面和管理员自己操作之后才刷新，而 enable 会被到期 / 超流量任务改写、
// autoTravel 会被总开关接管改写。整行写回会把这些字段倒回页面打开时的样子：被停的人被放回来、
// 自动接管被悄悄撤销。所以请求里的值只有在与 base 不同（管理员确实动了它）时才写入，否则保留库里
// 此刻的值。字段为 nil 表示请求没带 base（升级前打开的页面、直接调用）：按请求里的值写，保持改动前的行为。
type InboundEditBase struct {
	Enable     *bool
	AutoTravel *bool
}

// pick 返回这个字段该写入的值：管理员动过（请求值 ≠ base）或没有 base 时用请求值，否则保留库里的值。
func (InboundEditBase) pick(base *bool, requested, stored bool) bool {
	if base != nil && *base == requested {
		return stored
	}
	return requested
}

// SetInboundEnable 只改启用状态。列表上的开关走这里：改动前它把页面上整行 dbInbound 原样提交，
// 用量、地区、自动出差会一起被写回页面打开时的值。
//
// 「因超流量停用」的标记与 UpdateInbound 同一口径：启用则作废；从启用拨到停用是手动停用，也作废
// （不能在下个周期自己活过来）；状态本来就一样时什么都不改。
func (s *InboundService) SetInboundEnable(id int, enable bool) error {
	if enable {
		stored, err := s.GetInbound(id)
		if err != nil {
			return err
		}
		if !stored.Enable {
			if err := s.checkRevivable(stored); err != nil {
				return err
			}
		}
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		var in model.Inbound
		if err := tx.Select("id", "enable").First(&in, id).Error; err != nil {
			return err
		}
		if in.Enable == enable {
			return nil
		}
		return tx.Model(&model.Inbound{}).Where("id = ?", id).
			UpdateColumns(map[string]any{"enable": enable, "disabled_by_traffic": false}).Error
	})
}

// ResetInboundTraffic 把已用流量清零。只因超流量被停、还没到期的入站，停用的理由随之消失，
// 与周期重置（ResetDueTraffic）同一口径拉回来；返回是否拉回来了。已过期的、手动停用的不动。
// 不碰 LastResetAt：手动清零不改变周期重置的节奏。
func (s *InboundService) ResetInboundTraffic(id int, now time.Time) (bool, error) {
	// 会拉回来的话先校验（xray 校验要 exec，不放进事务）；事务里再按同一条件判一次，没校验过的不拉回。
	checked := false
	if stored, err := s.GetInbound(id); err == nil && stored.DisabledByTraffic && !stored.Enable && !expiredAt(stored, now) {
		if err := s.checkRevivable(stored); err != nil {
			return false, err
		}
		checked = true
	}
	revived := false
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var in model.Inbound
		if err := tx.First(&in, id).Error; err != nil {
			return err
		}
		fields := map[string]any{"up": 0, "down": 0}
		revived = checked && in.DisabledByTraffic && !in.Enable && !expiredAt(&in, now)
		if revived {
			fields["enable"] = true
			fields["disabled_by_traffic"] = false
		}
		res := tx.Model(&model.Inbound{}).Where("id = ?", id).UpdateColumns(fields)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return common.NewError("入站不存在:", id)
		}
		return nil
	})
	return revived, err
}

// checkRevivable 在把一个停用的入站重新启用之前，按启用后的样子过一遍保存时的两道校验：端口可用
// （checkInboundPortUsable）与真实 xray（validateInbound，fail open 的边界与保存时相同）。列表开关、
// 重置流量、续期都不经过编辑表单；停用期间核心可能升级过（26.x 拒绝 mKCP 的 header / seed），放过去的话
// 核心按一份它拒绝的配置整进程重启，所有用户一起断网而面板首页仍显示 running。
func (s *InboundService) checkRevivable(stored *model.Inbound) error {
	candidate := *stored
	candidate.Enable = true
	if err := s.checkInboundPortUsable(&candidate, stored); err != nil {
		return err
	}
	if err := validateInbound(&candidate, stored.Tag); err != nil {
		return common.NewError("这个入站的配置当前核心不接受，请先在编辑里修正后再启用:", err)
	}
	return nil
}
