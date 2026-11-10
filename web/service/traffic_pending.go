package service

import (
	"sync"

	"gorm.io/gorm"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/xray"
)

// 入站用量的写库暂存。
//
// 流量是用 GetTraffic(reset=true) 拉的：拉到手的那一刻核心侧已经清零。主库那次累加一旦失败
// （SQLite 锁超时、磁盘满），这一轮所有用户的用量就凭空消失了——而 inbounds.up/down 是限额与到期
// 判定的输入，丢的方向是「超额不被停用」。所以失败时把这一轮的入站增量按 tag 合并暂存，下一轮与
// 新增量一起写；写成功才清空。
//
// 只暂存主库这一份：用量历史与计量统计（TrafficHistoryService.Record / RecordMetered）在
// AddTraffic 里已经按本轮的新增量各写过一次，它们的失败本来就只告警；把它们也放进重试，
// 成功过的那份会被再记一遍。
var (
	pendingTrafficMu sync.Mutex
	pendingTraffic   []*xray.Traffic
)

// pendingTrafficMaxTags 是暂存的 tag 数上限。按 tag 合并之后条目数不会超过入站个数，
// 这只是防御：真到了上限，超出的那部分记日志丢弃，而不是让内存无界增长。
const pendingTrafficMaxTags = 4096

// addInboundTraffic 把暂存与本轮的入站增量合并后写进主库；失败则整批暂存，留给下一轮。
// 持 pendingTrafficMu 写库：流量任务与重启前的最后一次拉取可能同时到达，两边都要先取走暂存。
func (s *InboundService) addInboundTraffic(traffics []*xray.Traffic) error {
	pendingTrafficMu.Lock()
	defer pendingTrafficMu.Unlock()

	batch := mergeInboundTraffic(pendingTraffic, traffics)
	if len(batch) == 0 {
		pendingTraffic = nil
		return nil
	}
	if err := writeInboundTraffic(batch); err != nil {
		if len(batch) > pendingTrafficMaxTags {
			logger.Warning("入站用量暂存超过上限，丢弃", len(batch)-pendingTrafficMaxTags, "个 tag 的增量")
			batch = batch[:pendingTrafficMaxTags]
		}
		pendingTraffic = batch
		return err
	}
	pendingTraffic = nil
	return nil
}

// mergeInboundTraffic 按 tag 合并两批入站增量（出站条目忽略），顺序为首次出现的顺序。
// 返回的是新对象，不改动入参。
func mergeInboundTraffic(pending, fresh []*xray.Traffic) []*xray.Traffic {
	out := make([]*xray.Traffic, 0, len(pending)+len(fresh))
	byTag := make(map[string]*xray.Traffic, len(pending)+len(fresh))
	for _, list := range [][]*xray.Traffic{pending, fresh} {
		for _, t := range list {
			if t == nil || !t.IsInbound {
				continue
			}
			if acc, ok := byTag[t.Tag]; ok {
				acc.Up += t.Up
				acc.Down += t.Down
				continue
			}
			acc := &xray.Traffic{IsInbound: true, Tag: t.Tag, Up: t.Up, Down: t.Down}
			byTag[t.Tag] = acc
			out = append(out, acc)
		}
	}
	return out
}

// writeInboundTraffic 在一个事务里把每个 tag 的增量累加到 inbounds.up/down。
// 提交失败同样算失败：修复前不看 Commit 的返回值，提交时撞上锁超时这一轮就静默丢了。
func writeInboundTraffic(batch []*xray.Traffic) (err error) {
	tx := database.GetDB().Model(model.Inbound{}).Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit().Error
	}()
	for _, traffic := range batch {
		err = tx.Where("tag = ?", traffic.Tag).
			UpdateColumn("up", gorm.Expr("up + ?", traffic.Up)).
			UpdateColumn("down", gorm.Expr("down + ?", traffic.Down)).
			Error
		if err != nil {
			return err
		}
	}
	return nil
}

// drainTraffic 在整进程重启（或停止）之前最后拉一次计数器并累加。
//
// 整进程重启会清零核心里的计数器，而采集每 10 秒一次：不在这里拉，距上次采集以来所有用户的
// 用量都会丢——每次改了没有运行时重载接口的配置都是一次。拉不到只告警，不阻断重启：
// 重启往往正是因为核心出了问题。调用方持有 lock。
func (s *XrayService) drainTraffic(proc *xray.Process) {
	if proc == nil || !proc.IsRunning() {
		return
	}
	traffics, err := proc.GetTraffic(true)
	if err != nil {
		logger.Warning("重启前最后一次拉取流量失败，距上次采集以来的用量会丢失:", err)
		return
	}
	if err := s.inboundService.AddTraffic(traffics); err != nil {
		logger.Warning("重启前累加流量失败（入站增量已暂存，下一轮采集时重试）:", err)
	}
}
