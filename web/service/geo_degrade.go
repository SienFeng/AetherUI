package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"

	"a-ui/logger"
	"a-ui/util/geodat"
)

// 地区规则的降级只按**入站**发生，绝不让整份配置生成失败。
//
// 修复前任何一步失败（某个入站的省名对不上、IP 库没加载、写 dat 失败）都让
// GetXrayConfig 报错。而 10 秒的重启 cron 已经先消费了重启标志，于是到期停用、
// 超额停用、增删入站全都进不了核心，直到下一次无关改动；面板冷启动时则全部
// 入站都起不来。一个客户的地区数据出问题，把整机的下发都冻住了。
//
// 现在按入站收窄：
//   - 有上一次成功生成的允许集（last-known-good）就沿用，状态记为 stale；
//   - 没有就只锁死这一个入站（来源 0.0.0.0/0 与 ::/0 一律进黑洞），状态记为 closed。
//
// 锁死而不是放行：空的或缺失的允许集若退化成「不限制」，界面显示他只能在江苏用，
// 实际全世界都能连，而且没有任何一层会报错。锁死至少是看得见的——降级状态经
// 「需要留意」下发到入站列表。
const (
	geoDegradeStale  = "stale"
	geoDegradeClosed = "closed"
)

// GeoDegradation 是一个入站本轮地区规则的降级状态。
type GeoDegradation struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

var (
	geoDegradedMu sync.Mutex
	geoDegraded   = map[int]GeoDegradation{}

	// geoLastGood 是每个允许集 tag 最近一次成功算出的 CIDR。冷启动时内存里
	// 什么都没有，从现有的 bin/a-ui-geo.dat 解码出来做种子（只做一次）。
	geoLastGoodMu     sync.Mutex
	geoLastGood       = map[string][]string{}
	geoLastGoodSeeded bool
)

// GeoDegradedInbounds 返回最近一次生成的降级状态（副本）。
func GeoDegradedInbounds() map[int]GeoDegradation {
	geoDegradedMu.Lock()
	defer geoDegradedMu.Unlock()
	out := make(map[int]GeoDegradation, len(geoDegraded))
	for k, v := range geoDegraded {
		out[k] = v
	}
	return out
}

// setGeoDegraded 整体替换降级状态，只在状态变化时告警：生成每 10 秒走一次，
// 同一句告警反复刷屏会把真正的告警淹没。
func setGeoDegraded(next map[int]GeoDegradation) {
	geoDegradedMu.Lock()
	prev := geoDegraded
	geoDegraded = next
	geoDegradedMu.Unlock()

	ids := make([]int, 0, len(next))
	for id := range next {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if prev[id] != next[id] {
			d := next[id]
			logger.Warning("地区规则降级, 入站 id:", id, "模式:", d.Mode, "原因:", d.Reason)
		}
	}
	for id := range prev {
		if _, ok := next[id]; !ok {
			logger.Info("地区规则已恢复正常, 入站 id:", id)
		}
	}
}

// lastGoodSnapshot 返回 last-known-good 的副本，必要时先从现有 dat 做种子。
func lastGoodSnapshot() map[string][]string {
	geoLastGoodMu.Lock()
	defer geoLastGoodMu.Unlock()
	if !geoLastGoodSeeded {
		geoLastGoodSeeded = true
		if entries, _, err := readGeoDat(geoDatPath); err == nil {
			for _, e := range entries {
				if _, ok := geoLastGood[e.Tag]; !ok {
					geoLastGood[e.Tag] = e.CIDRs
				}
			}
		}
	}
	out := make(map[string][]string, len(geoLastGood))
	for k, v := range geoLastGood {
		out[k] = v
	}
	return out
}

// rememberLastGood 记下本轮真正写进 dat 的允许集。
func rememberLastGood(entries []geodatEntry) {
	geoLastGoodMu.Lock()
	defer geoLastGoodMu.Unlock()
	for _, e := range entries {
		geoLastGood[e.Tag] = e.CIDRs
	}
}

// readGeoDat 读回现有 dat，返回内容与内容哈希（与 writeGeoDat 同一算法）。
func readGeoDat(path string) ([]geodatEntry, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	entries, err := geodat.Decode(data)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return entries, hex.EncodeToString(sum[:4]), nil
}

// fallbackToExistingDat 处理写 dat 失败：现有文件里已有的 tag 照用（规则引用的
// 哈希取现有文件的），缺的入站并入锁死。连现有文件都读不出时，所有地区入站锁死。
// 返回规则里要用的哈希。
func fallbackToExistingDat(plan *geoPlan, writeErr error) string {
	reason := fmt.Sprint("写入地区数据文件失败: ", writeErr)
	entries, hash, err := readGeoDat(geoDatPath)
	have := map[string]bool{}
	if err == nil {
		for _, e := range entries {
			have[e.Tag] = true
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		reason += fmt.Sprint("；现有文件也读不出: ", err)
	}
	for id, tag := range plan.TagByInbound {
		if have[tag] {
			// 照用旧文件里的允许集，内容可能早于这一轮（例如 IP 库更新之前），而且写失败
			// 本身需要管理员处理：必须标出来，不能静默。
			plan.Degraded[id] = GeoDegradation{Mode: geoDegradeStale, Reason: reason}
			continue
		}
		delete(plan.TagByInbound, id)
		plan.FailClosed[id] = reason
		plan.Degraded[id] = GeoDegradation{Mode: geoDegradeClosed, Reason: reason}
	}
	return hash
}
