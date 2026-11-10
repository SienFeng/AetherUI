package service

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"a-ui/logger"
	"a-ui/xray"
)

// 运行时的端口冲突：端口被本机其它程序占着的入站，从下发给核心的配置里摘掉，其余入站照常。
//
// xray 加载配置是全有或全无的：一个入站 bind 失败，整个核心退出、所有入站一起断，而 Start 从不
// 回传失败、RestartXray 照样返回 nil。保存时的探测（inbound_port.go）挡住了大部分，挡不住的是
// 「保存之后才被占」——比如后来装的 Caddy 占了 :443、面板重启时别的服务先起来了。
//
// 设计：
//   - 登记表在内存里，按入站 tag 记它当时的监听规格与失败原因。GetXrayConfig 最后一步按表摘掉这些
//     入站（excludePortConflicts）；摘除是登记表的纯函数，同一张表生成的配置逐字节相同，Config.Equals
//     的空操作分支照常成立，不会形成重启循环，规则验证的「待生效」判定也与核心一致。
//   - 登记只发生在持 lock 的下发路径上：整进程启动之前探测运行配置里没有的入站（核心在跑时它自己
//     占着的端口不探），以及热应用里 AddInbound 报端口占用（探测与 bind 之间被抢）。
//   - 解除：每次下发开头重新探测已登记的，空出来的注销；CheckInboundJob 每 30 秒探一次，有空出来的
//     就置重启标志，10 秒的消费任务随即把入站加回去。
//   - 控制面 api 入站从不摘：摘了面板就失去流量统计与热应用，限额从此静默失效。它的端口被占时核心
//     照旧起不来，与修复前一样。
//   - 登记表不落库：面板重启后第一次下发会重新探测，结论自然重建。
//   - 管理员能看到的地方：每次新登记记一条 Warning（同一冲突只记一次），PortConflicts() 给出当前
//     清单。状态页与入站列表不在本次改动范围内，接入时直接读它。

// PortConflict 是一个因端口被本机其它程序占用、暂时没有下发给核心的入站。
type PortConflict struct {
	Tag    string `json:"tag"`
	Listen string `json:"listen"`
	Port   int    `json:"port"`
	Reason string `json:"reason"`
	Since  int64  `json:"since"` // 毫秒时间戳
}

type portConflictEntry struct {
	PortConflict
	spec inboundListenSpec
}

var (
	portConflictMu sync.Mutex
	portConflicts  = map[string]portConflictEntry{}
)

// PortConflicts 返回当前因端口被占而没有下发的入站（按 tag 排序）。
func (s *XrayService) PortConflicts() []PortConflict {
	portConflictMu.Lock()
	defer portConflictMu.Unlock()
	out := make([]PortConflict, 0, len(portConflicts))
	for _, e := range portConflicts {
		out = append(out, e.PortConflict)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// registerPortConflict 登记一个冲突。同一个入站、同样的监听已经登记过时只更新原因，不重复告警。
func registerPortConflict(tag string, spec inboundListenSpec, reason error) {
	portConflictMu.Lock()
	prev, existed := portConflicts[tag]
	entry := portConflictEntry{
		PortConflict: PortConflict{Tag: tag, Listen: spec.Listen, Port: spec.Port,
			Reason: reason.Error(), Since: time.Now().UnixMilli()},
		spec: spec,
	}
	if existed && prev.spec == spec {
		entry.Since = prev.Since
	}
	portConflicts[tag] = entry
	portConflictMu.Unlock()
	if !existed || prev.spec != spec {
		logger.Warningf("入站 [%s] 的端口 %d 被本机其它程序占用（%v），已暂时不下发给 xray，其余入站照常运行；"+
			"腾出端口后约 30 秒内自动恢复", tag, spec.Port, reason)
	}
}

// applyPortConflicts 是 GetXrayConfig 的最后一步：先清掉已经不适用的登记（入站删了、停用了、改了
// 监听——否则 PortConflicts 一直报一个不存在的冲突），再摘掉仍然冲突的入站。cfg 必须是刚生成、
// 还没摘过的完整配置：摘过之后被摘的入站不在里面，会被误当成「已经不存在」。
func applyPortConflicts(cfg *xray.Config) {
	if cfg == nil {
		return
	}
	portConflictMu.Lock()
	if len(portConflicts) > 0 {
		present := make(map[string]inboundListenSpec, len(cfg.InboundConfigs))
		for i := range cfg.InboundConfigs {
			if spec, ok := listenSpecOf(&cfg.InboundConfigs[i]); ok {
				present[cfg.InboundConfigs[i].Tag] = spec
			}
		}
		for tag, e := range portConflicts {
			if spec, ok := present[tag]; !ok || spec != e.spec {
				delete(portConflicts, tag)
			}
		}
	}
	portConflictMu.Unlock()
	excludePortConflicts(cfg)
}

// excludePortConflicts 从配置里摘掉已登记冲突的入站。只摘 tag 与监听规格都与登记时一致的：
// 管理员改了端口或地址，旧登记不再适用，下一次下发会重新探测。
func excludePortConflicts(cfg *xray.Config) {
	portConflictMu.Lock()
	defer portConflictMu.Unlock()
	if len(portConflicts) == 0 || cfg == nil {
		return
	}
	kept := cfg.InboundConfigs[:0:0]
	for i := range cfg.InboundConfigs {
		ib := &cfg.InboundConfigs[i]
		if e, ok := portConflicts[ib.Tag]; ok && ib.Tag != xray.APIInboundTag {
			if spec, ok := listenSpecOf(ib); ok && spec == e.spec {
				continue
			}
		}
		kept = append(kept, *ib)
	}
	cfg.InboundConfigs = kept
}

// recoverPortConflicts 重新探测已登记的冲突，空出来的注销。调用方持有 lock。
func recoverPortConflicts() {
	portConflictMu.Lock()
	entries := make([]portConflictEntry, 0, len(portConflicts))
	for _, e := range portConflicts {
		entries = append(entries, e)
	}
	portConflictMu.Unlock()
	for _, e := range entries {
		if probeListenFn(e.spec) != nil {
			continue
		}
		portConflictMu.Lock()
		if cur, ok := portConflicts[e.Tag]; ok && cur.spec == e.spec {
			delete(portConflicts, e.Tag)
		}
		portConflictMu.Unlock()
		logger.Info("入站 [", e.Tag, "] 的端口", e.spec.Port, "已空出，恢复下发")
	}
}

// excludeBusyInbounds 在下发之前探测新配置里「运行中的核心没占着」的入站端口，被占的登记并摘掉。
// running 为 nil（核心没在跑）时探测全部。调用方持有 lock。
//
// 探测失败一律摘：与保存时只认 EADDRINUSE 不同，这里探不过的端口核心同样 bind 不上，留着它就是
// 整个核心起不来。
func excludeBusyInbounds(cfg *xray.Config, running *xray.Config) {
	if cfg == nil {
		return
	}
	heldTCP, heldUDP := heldListeners(running)
	found := false
	for i := range cfg.InboundConfigs {
		ib := &cfg.InboundConfigs[i]
		if ib.Tag == xray.APIInboundTag {
			continue
		}
		spec, ok := listenSpecOf(ib)
		if !ok {
			continue
		}
		probe, ok := unheldPart(spec, heldTCP, heldUDP)
		if !ok {
			continue
		}
		// 登记用入站完整的监听规格：GetXrayConfig 按它与生成的入站比对来摘。
		if err := probeListenFn(probe); err != nil {
			registerPortConflict(ib.Tag, spec, err)
			found = true
		}
	}
	if found {
		excludePortConflicts(cfg)
	}
}

// runningConfigOf 返回运行中的核心持有的配置；核心没在跑时为 nil。
func runningConfigOf(proc *xray.Process) *xray.Config {
	if proc == nil || !proc.IsRunning() {
		return nil
	}
	return proc.GetConfig()
}

// RecheckPortConflicts 由 CheckInboundJob 每 30 秒调一次：有登记的端口空出来了就置重启标志，交给
// 10 秒的消费任务把入站加回去（注销发生在那次下发的开头）。拿不到 lock（重启或热应用进行中）就等下一轮：
// 探测本身会占一瞬间端口，不能与核心的 bind 撞上。
func (s *XrayService) RecheckPortConflicts() {
	portConflictMu.Lock()
	specs := make([]inboundListenSpec, 0, len(portConflicts))
	for _, e := range portConflicts {
		specs = append(specs, e.spec)
	}
	portConflictMu.Unlock()
	if len(specs) == 0 || !lock.TryLock() {
		return
	}
	freed := false
	for _, spec := range specs {
		if probeListenFn(spec) == nil {
			freed = true
			break
		}
	}
	lock.Unlock()
	if freed {
		s.SetToNeedRestart()
	}
}

// isBindError 判断 AddInbound 的失败是不是端口被占。错误经 gRPC 跨进程后只剩文本，
// errors.Is 认不出 syscall.EADDRINUSE。
func isBindError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

// addedInboundSpec 从热应用要新增的入站 JSON 里取出 tag 与监听规格。
func addedInboundSpec(raw []byte) (string, inboundListenSpec, bool) {
	var ib xray.InboundConfig
	if err := json.Unmarshal(raw, &ib); err != nil || ib.Tag == "" || ib.Tag == xray.APIInboundTag {
		return "", inboundListenSpec{}, false
	}
	spec, ok := listenSpecOf(&ib)
	return ib.Tag, spec, ok
}

// withoutInbounds 返回去掉指定 tag 的入站之后的配置副本，其余字段共享。热应用里有入站因端口被占
// 没加进去时，核心持有的就是这一份——记成完整的新配置的话，下一轮比对会以为它已经在核心里了。
func withoutInbounds(cfg *xray.Config, tags map[string]bool) *xray.Config {
	out := *cfg
	out.InboundConfigs = make([]xray.InboundConfig, 0, len(cfg.InboundConfigs))
	for _, ib := range cfg.InboundConfigs {
		if !tags[ib.Tag] {
			out.InboundConfigs = append(out.InboundConfigs, ib)
		}
	}
	return &out
}
