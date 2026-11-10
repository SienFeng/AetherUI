package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"weak"

	"a-ui/logger"
	"a-ui/util/geodat"
	"a-ui/xray"
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
	// geoDegradePartial：多个省里有的省在 IP 库里找不到（库更新后改了名或没了），又没有上一次的允许集可用，
	// 只放行其余省份。整组锁死会把那些省的用户一起挡在外面；当作没事则会悄悄缩小放行范围。
	geoDegradePartial = "partial"
)

// GeoDegradation 是一个入站本轮地区规则的降级状态。
type GeoDegradation struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

// geoGeneration 是一次配置生成里地区规则的结果。
type geoGeneration struct {
	Degraded map[int]GeoDegradation `json:"degraded,omitempty"`
	// Codes 是入站 id → 它的允许集在 dat 里的代码；锁死的入站没有。
	Codes map[int]string `json:"codes,omitempty"`
}

var (
	// 生成配置的不只是下发：保存时的校验、页面轮询的「待生效」判断、流量走向验证都会生成一份。只有核心
	// 真的用上的那一份（RestartXray 的热应用成功、进程启动、或与运行中配置一致）才是「已生效」——修复前
	// 每次生成都直接改写降级状态，原因消失后界面上的降级标记没了，核心却仍锁着这个入站，也没有任何东西
	// 触发重新下发。
	geoGenMu sync.Mutex
	// 按身份认配置、不让它活下去（weak）：带大规则集的配置一份几 MB，攥着 8 份在小内存的 VPS 上白占几十 MB。
	geoGenByCfg = map[weak.Pointer[xray.Config]]geoGeneration{}
	geoGenOrder []weak.Pointer[xray.Config]
	geoApplied  geoGeneration
	// geoLastFlagged 是上一次因「与核心不同」而置重启标志的生成结果：同一份结果不重复置，免得下发失败时
	// 每轮生成都再触发一次重启。
	geoLastFlagged string

	// geoLastGood 是每个允许集 tag 最近一次成功算出的 CIDR。冷启动时内存里
	// 什么都没有，从现有的 bin/a-ui-geo.dat 解码出来做种子（只做一次）。
	geoLastGoodMu     sync.Mutex
	geoLastGood       = map[string][]string{}
	geoLastGoodSeeded bool

	// geoRecentCodes 是最近几次写 dat 时本轮的代码（geoDatLock 保护）：并发的两次生成之间，先生成的那份配置
	// 可能还没启动，后写的那次也要替它留着代码。
	geoRecentCodes [3]map[string]bool
	geoRecentNext  int
)

// geoGenerationKeep 是等待提交的生成结果最多留几份。
const geoGenerationKeep = 8

func (g geoGeneration) fingerprint() string {
	b, _ := json.Marshal(g)
	return string(b)
}

func geoGenerationOf(plan *geoPlan) geoGeneration {
	g := geoGeneration{Degraded: map[int]GeoDegradation{}, Codes: map[int]string{}}
	for id, d := range plan.Degraded {
		g.Degraded[id] = d
	}
	for id, code := range plan.TagByInbound {
		g.Codes[id] = code
	}
	return g
}

// rememberGeoGeneration 记下一次生成的结果，等核心用上它时由 commitGeoApplied 提交。与核心正在用的不同
// （降级的原因消失了、或者刚出现），就置重启标志：靠它把恢复后的配置下发下去。
func rememberGeoGeneration(cfg *xray.Config, gen geoGeneration) {
	geoGenMu.Lock()
	defer geoGenMu.Unlock()
	key := weak.Make(cfg)
	if _, ok := geoGenByCfg[key]; !ok {
		geoGenOrder = append(geoGenOrder, key)
	}
	geoGenByCfg[key] = gen
	for len(geoGenOrder) > geoGenerationKeep {
		delete(geoGenByCfg, geoGenOrder[0])
		geoGenOrder = geoGenOrder[1:]
	}
	if sameGeoGeneration(gen, geoApplied) {
		geoLastFlagged = ""
		return
	}
	if fp := gen.fingerprint(); fp != geoLastFlagged {
		geoLastFlagged = fp
		(&XrayService{}).SetToNeedRestart()
	}
}

func sameGeoGeneration(a, b geoGeneration) bool {
	norm := func(g geoGeneration) geoGeneration {
		if len(g.Degraded) == 0 {
			g.Degraded = nil
		}
		if len(g.Codes) == 0 {
			g.Codes = nil
		}
		return g
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// commitGeoApplied 在核心用上 cfg 之后调用，把它的地区规则结果记为「已生效」。cfg 不是记过的生成（模板里
// 没有地区规则时也会记一份空的）就什么都不做。
func commitGeoApplied(cfg *xray.Config) {
	geoGenMu.Lock()
	gen, ok := geoGenByCfg[weak.Make(cfg)]
	if !ok {
		geoGenMu.Unlock()
		return
	}
	prev := geoApplied.Degraded
	geoApplied = gen
	geoLastFlagged = ""
	geoGenMu.Unlock()

	ids := make([]int, 0, len(gen.Degraded))
	for id := range gen.Degraded {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	// 只在状态变化时告警：生成每 10 秒走一次，同一句告警反复刷屏会把真正的告警淹没。
	for _, id := range ids {
		if prev[id] != gen.Degraded[id] {
			d := gen.Degraded[id]
			logger.Warning("地区规则降级, 入站 id:", id, "模式:", d.Mode, "原因:", d.Reason)
		}
	}
	for id := range prev {
		if _, ok := gen.Degraded[id]; !ok {
			logger.Info("地区规则已恢复正常, 入站 id:", id)
		}
	}
}

// GeoDegradedInbounds 返回核心正在用的配置里的降级状态（副本）。
func GeoDegradedInbounds() map[int]GeoDegradation {
	geoGenMu.Lock()
	defer geoGenMu.Unlock()
	out := make(map[int]GeoDegradation, len(geoApplied.Degraded))
	for k, v := range geoApplied.Degraded {
		out[k] = v
	}
	return out
}

// appliedGeo 返回核心正在用的地区规则结果（副本）。
func appliedGeo() geoGeneration {
	geoGenMu.Lock()
	defer geoGenMu.Unlock()
	out := geoGeneration{Degraded: map[int]GeoDegradation{}, Codes: map[int]string{}}
	for k, v := range geoApplied.Degraded {
		out.Degraded[k] = v
	}
	for k, v := range geoApplied.Codes {
		out.Codes[k] = v
	}
	return out
}

// lastGoodSnapshot 返回 last-known-good 的副本，必要时先从现有 dat 做种子。
func lastGoodSnapshot() map[string][]string {
	geoLastGoodMu.Lock()
	defer geoLastGoodMu.Unlock()
	if !geoLastGoodSeeded {
		geoLastGoodSeeded = true
		if entries, _, err := readGeoDat(geoDatPath); err == nil {
			// dat 里是代码（tag + 内容哈希），同一个 tag 可能有两份（本轮的与留给运行中配置的旧代码），
			// 取第一份：写入时本轮的排在前面。
			for _, e := range entries {
				if geoCodeIsPartial(e.Tag) {
					continue // 缺省份时剩下的那部分，不是完整的旧允许集
				}
				tag := geoTagOfCode(e.Tag)
				if _, ok := geoLastGood[tag]; !ok {
					geoLastGood[tag] = e.CIDRs
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

// rememberLastGood 记下本轮完整算出、真正写进 dat 的允许集（按 tag 记）。
func rememberLastGood(plan *geoPlan) {
	geoLastGoodMu.Lock()
	defer geoLastGoodMu.Unlock()
	for _, e := range plan.Entries {
		if tag := plan.TagOfCode[e.Tag]; plan.Complete[tag] {
			geoLastGood[tag] = e.CIDRs
		}
	}
}

// readGeoDat 读回现有 dat，返回内容与内容哈希（与 writeGeoDat 同一算法）。
func readGeoDat(path string) ([]geodatEntry, string, error) {
	// 与 writeGeoDat 用同一把锁：读到一半时被换掉的话，读出来的是新旧混合的内容。
	geoDatLock.Lock()
	defer geoDatLock.Unlock()
	return readGeoDatLocked(path)
}

// readGeoDatLocked 是 readGeoDat 的本体，调用方持有 geoDatLock。
func readGeoDatLocked(path string) ([]geodatEntry, string, error) {
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

// fallbackToExistingDat 处理写 dat 失败：现有文件里正好有同样内容（同一个代码）的入站照常；只有同一组省份的
// 旧内容的，改用旧代码并标 stale；什么都没有的入站锁死。连现有文件都读不出时，所有地区入站锁死。
func fallbackToExistingDat(plan *geoPlan, writeErr error) {
	reason := geoWriteFailureReason(writeErr)
	raw := fmt.Sprint(writeErr)
	entries, _, err := readGeoDat(geoDatPath)
	haveCode := map[string]bool{}
	codeOfTag := map[string]string{}
	if err == nil {
		for _, e := range entries {
			haveCode[e.Tag] = true
			if tag := geoTagOfCode(e.Tag); codeOfTag[tag] == "" {
				codeOfTag[tag] = e.Tag
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		reason += "；现有文件也读不出"
		raw += fmt.Sprint("；读取现有文件: ", err)
	}
	for id, code := range plan.TagByInbound {
		if haveCode[code] {
			continue // 文件里就是这份内容：这个入站不受影响（写失败本身记了日志）
		}
		if old := codeOfTag[plan.TagOfCode[code]]; old != "" {
			// 照用旧文件里的允许集，内容早于这一轮（例如 IP 库更新之前），必须标出来，不能静默。
			plan.TagByInbound[id] = old
			plan.Degraded[id] = GeoDegradation{Mode: geoDegradeStale, Reason: reason}
			continue
		}
		delete(plan.TagByInbound, id)
		plan.FailClosed[id] = reason
		plan.Degraded[id] = GeoDegradation{Mode: geoDegradeClosed, Reason: reason}
	}
	logGeoWriteFailure(reason, raw)
}

// geoWriteFailureReason 给「写 dat 失败」一个稳定的、给人看的原因；原始错误只进日志。
//
// 原因会进生成指纹（rememberGeoGeneration 按指纹判断要不要置重启标志），而 writeFileAtomic 的错误里带着
// 随机的临时文件名。原样带着的话，同一个写入故障每一轮都像一次新的生成：每 10 秒置一次重启标志、整份
// 配置重算一遍、一轮一条警告，磁盘满或 bin/ 只读时会一直转下去。
func geoWriteFailureReason(err error) string {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "写入地区数据文件失败：磁盘空间不足"
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EROFS):
		return "写入地区数据文件失败：没有写入权限（目录只读或权限不足）"
	default:
		return "写入地区数据文件失败（原始错误见面板日志）"
	}
}

var (
	geoWriteFailMu     sync.Mutex
	geoWriteFailLogged = map[string]time.Time{}
)

// logGeoWriteFailure 同一个原因 10 分钟内只记一条：每次生成配置都会走到这里（页面轮询、校验、下发）。
func logGeoWriteFailure(reason, raw string) {
	geoWriteFailMu.Lock()
	defer geoWriteFailMu.Unlock()
	now := time.Now()
	if last, ok := geoWriteFailLogged[reason]; ok && now.Sub(last) < 10*time.Minute {
		return
	}
	geoWriteFailLogged[reason] = now
	logger.Warning("地区规则：", reason, "：", raw)
}

// writeGeoDatRetaining 把本轮的允许集写进 dat，并保留正在运行的配置与最近几次生成引用的旧代码。
//
// 校验（xray run -test）、进程重启读的都是这一个文件，而生成不止下发这一条路：一次页面轮询生成的配置
// 先写了文件，另一份更早生成、还没启动的配置引用的代码就不能被它挤掉——按内容寻址之后，内容一变代码就变。
// 本轮的条目排在前面（冷启动时 lastGoodSnapshot 取第一份）。
func writeGeoDatRetaining(plan *geoPlan) error {
	running := runningGeoCodes()
	geoDatLock.Lock()
	defer geoDatLock.Unlock()
	referenced := map[string]bool{}
	for code := range running {
		referenced[code] = true
	}
	for _, recent := range geoRecentCodes {
		for code := range recent {
			referenced[code] = true
		}
	}
	existing, _, _ := readGeoDatLocked(geoDatPath)
	entries := append(append([]geodatEntry(nil), plan.Entries...), retainedGeoEntries(existing, plan.Entries, referenced)...)
	if _, err := writeGeoDatLocked(geoDatPath, entries); err != nil {
		return err
	}
	cur := make(map[string]bool, len(plan.Entries))
	for _, e := range plan.Entries {
		cur[e.Tag] = true
	}
	geoRecentCodes[geoRecentNext] = cur
	geoRecentNext = (geoRecentNext + 1) % len(geoRecentCodes)
	return nil
}

// retainedGeoEntries 是现有文件里还被引用（referenced）、又不在本轮里的条目，按代码排序。
func retainedGeoEntries(existing, current []geodatEntry, referenced map[string]bool) []geodatEntry {
	cur := make(map[string]bool, len(current))
	for _, e := range current {
		cur[e.Tag] = true
	}
	var keep []geodatEntry
	seen := map[string]bool{}
	for _, e := range existing {
		if cur[e.Tag] || seen[e.Tag] || !referenced[e.Tag] {
			continue
		}
		seen[e.Tag] = true
		keep = append(keep, e)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Tag < keep[j].Tag })
	return keep
}

// runningGeoCodes 是正在运行的核心的配置引用的 dat 代码。
func runningGeoCodes() map[string]bool {
	pr := currentProcess()
	if pr == nil {
		return map[string]bool{}
	}
	cfg := pr.GetConfig()
	if cfg == nil {
		return map[string]bool{}
	}
	return geoCodesInRouting(cfg.RouterConfig)
}

// geoCodesInRouting 从 routing 段里取出地区规则引用的 dat 代码（source 里的 ext:a-ui-geo.dat:!代码）。
func geoCodesInRouting(routing []byte) map[string]bool {
	out := map[string]bool{}
	var r struct {
		Rules []struct {
			Source []string `json:"source"`
		} `json:"rules"`
	}
	if len(routing) == 0 || json.Unmarshal(routing, &r) != nil {
		return out
	}
	prefix := "ext:" + geoDatRef + ":!"
	for _, rule := range r.Rules {
		for _, src := range rule.Source {
			if strings.HasPrefix(src, prefix) {
				out[strings.TrimPrefix(src, prefix)] = true
			}
		}
	}
	return out
}
