package service

import (
	"bytes"
	"net"
	"sort"
	"sync"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/netdiag"
)

// selectOverQuota 按「先来后到」挑出超出额度、应当被断开的来源 IP。
//
// limit <= 0 表示不限制。保留最早上线的 limit 个，其余全部拒绝——这是
// 与管理员确认过的语义：拒绝后来的，不动已经在线的人。
//
// 次序必须是确定的：面板刚重启时所有 IP 的首次观测时间相同，若此时次序
// 随机，每一轮踢掉的人都不一样，结果是谁都用不成。因此首次观测时间相同
// 时按 IP 字节升序做二次排序。
func selectOverQuota(list []OnlineIP, limit int) []string {
	if limit <= 0 || len(list) <= limit {
		return nil
	}

	sorted := make([]OnlineIP, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].FirstSeen != sorted[j].FirstSeen {
			return sorted[i].FirstSeen < sorted[j].FirstSeen
		}
		return bytes.Compare(
			net.ParseIP(sorted[i].IP).To16(),
			net.ParseIP(sorted[j].IP).To16(),
		) < 0
	})

	over := make([]string, 0, len(sorted)-limit)
	for _, e := range sorted[limit:] {
		over = append(over, e.IP)
	}
	return over
}

// 上一轮各入站被拒的 IP 集合，只用于决定要不要写日志：
// 判定每秒跑一次，不去重的话一个反复重连的客户端会把日志刷爆。
var (
	concurrencyLogLock sync.Mutex
	concurrencyLogged  = map[int]map[string]bool{}
)

type ConcurrencyService struct {
	inboundService InboundService
	onlineService  OnlineService
	settingService SettingService
}

// limitedInbounds 返回真正需要做并发判定的入站：设了额度且处于启用状态。
// 停用的入站根本不在 xray 配置里，没有连接可管。
func (s *ConcurrencyService) limitedInbounds() ([]*model.Inbound, error) {
	var inbounds []*model.Inbound
	err := database.GetDB().Model(model.Inbound{}).
		Where("concurrency_limit > 0 and enable = ?", true).
		Order("id asc").
		Find(&inbounds).Error
	if err != nil {
		return nil, err
	}
	return inbounds, nil
}

// Enforce 跑一轮并发判定。
//
// 它是**幂等收敛**的：每轮重新计算超额集合并对其执行断开，不依赖任何
// 跳变事件。因此漏掉一轮、面板重启、客户端重连都不会让状态跑偏。
func (s *ConcurrencyService) Enforce() error {
	inbounds, err := s.limitedInbounds()
	if err != nil {
		return err
	}
	if len(inbounds) == 0 {
		// 没人设额度就一次系统调用都不做，空转成本为零。
		return nil
	}
	if !netdiag.Supported {
		return netdiag.ErrUnsupported
	}
	if err := s.onlineService.sample(); err != nil {
		return err
	}

	// 读不到配置时退回 0（关闭闲置判定），保持改动前的行为：宁可额度释放
	// 得慢，也不要因为读配置失败就把所有人都当成闲置、并发限制整个失效。
	idleAfter := idleTimeoutOrZero(s.settingService.GetConcurrencyIdleTimeout())

	now := time.Now()
	for _, inbound := range inbounds {
		list := onlineTrackerInstance.snapshotIdle(inbound.Port, noLocate, idleAfter)
		over := planRejections(list, inbound.ConcurrencyLimit)
		onlineTrackerInstance.setRejected(inbound.Port, over, now)
		s.disconnect(inbound, over)
	}
	return nil
}

// noLocate 用于判定路径：这里不需要归属地，省掉每轮的查库开销。
func noLocate(net.IP) (string, string) { return "", "" }

// planRejections 决定某入站本轮应当拒绝哪些来源 IP：先剔除已断连的历史
// 条目，再按先来后到挑出超额的部分。两步必须按这个顺序，理由见 liveOnly。
func planRejections(list []OnlineIP, limit int) []string {
	return selectOverQuota(liveOnly(list), limit)
}

// liveOnly 剔除"已被拒、连接已断"的历史条目。
//
// 这些条目只为界面展示而存在。把它们算进在线数会让被拒的 IP 永久占着一个
// 名额：额度 1 时第一个被拒的人会把所有人（包括他自己）永远挡在门外，
// 而且这个状态在保留期内自愈不了。
// liveOnly 挑出真正占用并发额度的来源。
//
// 闲置来源被排除在外，两个方向都成立：它不占别人的额度，自己也不会被判超额
// 而遭断开——断一个只是暂时没有流量的正常用户毫无意义。
func liveOnly(list []OnlineIP) []OnlineIP {
	live := make([]OnlineIP, 0, len(list))
	for _, e := range list {
		if e.Conns > 0 && !e.Idle {
			live = append(live, e)
		}
	}
	return live
}

func (s *ConcurrencyService) disconnect(inbound *model.Inbound, over []string) {
	logged := takeLoggedSet(inbound.Id, over)
	for _, ip := range over {
		killed, err := s.onlineService.Kick(inbound.Id, ip)
		if err != nil {
			logger.Warning("并发超额断开失败, 入站", inbound.Id, "IP", ip, ":", err)
			continue
		}
		// 只在这个 IP 是本轮新被拒时记一条，避免反复重连刷日志。
		if !logged[ip] && killed > 0 {
			logger.Warning("并发超额: 入站", inbound.Id, "额度", inbound.ConcurrencyLimit,
				"拒绝来源 IP", ip, "断开", killed, "条连接")
		}
	}
}

// takeLoggedSet 返回上一轮该入站已记过日志的 IP 集合，并把本轮集合存下来。
func takeLoggedSet(inboundId int, over []string) map[string]bool {
	concurrencyLogLock.Lock()
	defer concurrencyLogLock.Unlock()

	prev := concurrencyLogged[inboundId]
	if prev == nil {
		prev = map[string]bool{}
	}
	next := make(map[string]bool, len(over))
	for _, ip := range over {
		next[ip] = true
	}
	concurrencyLogged[inboundId] = next
	return prev
}

// idleTimeoutOrZero 把设置项的秒数折算成 Duration，出错时返回 0（关闭判定）。
func idleTimeoutOrZero(seconds int, err error) time.Duration {
	if err != nil {
		logger.Warning("读取并发闲置超时失败，本轮不做闲置判定:", err)
		return 0
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
