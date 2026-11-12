package service

import (
	"encoding/json"
	"errors"
	"go.uber.org/atomic"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/xray"
)

var p *xray.Process
var lock sync.Mutex
var isNeedXrayRestart atomic.Bool
var result string

// xrayApplyGen 是「核心加载了新配置」的次数。只在核心确实拿到新配置的两处
// 递增：tryHotApply 带非空 diff 成功，以及 p.Start() 成功。
// 例外：热应用只换了计量出站与计量规则（xray.OnlyTouchesTags）时不递增。计量规则排在
// 所有规则之后，只接住本来会走默认出站的流量，不改变任何分流规则的结论；而计量池每小时
// 都会换几个域名，递增会让分流审计每小时把全部规则（含十几万条的大规则）重验一遍。
// 这个计数的读取方全是分流审计（验证缓存、列表摘要、异常扫描计划），所以按「会不会改变
// 分流结论」计数。空 diff 分支只是
// 同步面板侧快照、核心什么都没变；失败退回重启的路径由 Start 那一处计数，
// 热应用中途失败不单独计，避免一次生效被记两次。
// 读取方据此判断「某次观测之后核心是否换过配置」，由 XrayApplyGeneration 暴露。
var xrayApplyGen atomic.Uint64

// xrayChangeSeq 是「有人要求把库里的改动下发到核心」的次数：SetToNeedRestart 与
// RestartXray 入口各递增一次。它只用于让 Summaries 的待生效判定记忆（pendingMemo）
// 在改动发生的那一刻立即失效，不是给别处判断「有没有待生效」用的——
// isNeedXrayRestart 会被 10 秒的消费任务清掉，清掉之后重启失败的话核心仍持有旧配置，
// 拿它当「没有待生效」会误给绿；只增不减的计数没有这个问题。
var xrayChangeSeq atomic.Uint64

// XrayApplyGeneration 返回当前应用代次，可在不持 lock 时调用。
func XrayApplyGeneration() uint64 {
	return xrayApplyGen.Load()
}

// procMu 保护 p 与 result 这两个包级变量本身的读写。RestartXray / StopXray 在 lock 里改它们，
// 而面板首页的状态轮询（每 2 秒）、流量采集（每 10 秒）都不拿 lock 就读；result 是字符串，
// 不加保护的并发读写可能读到撕裂的值。进程自己的状态由 xray.Process 内部的锁保护。
var procMu sync.RWMutex

// currentProcess 返回当前的 xray 进程（可能为 nil）。不持有 lock 的调用方一律经它取。
func currentProcess() *xray.Process {
	procMu.RLock()
	defer procMu.RUnlock()
	return p
}

// swapProcess 换上新的 xray 进程，并清掉旧进程的输出缓存。调用方持有 lock。
func swapProcess(np *xray.Process) {
	procMu.Lock()
	p = np
	result = ""
	procMu.Unlock()
}

type XrayService struct {
	inboundService  InboundService
	settingService  SettingService
	routingInjector RoutingInjector
	dnsInjector     DNSInjector
}

func (s *XrayService) IsXrayRunning() bool {
	cur := currentProcess()
	return cur != nil && cur.IsRunning()
}

func (s *XrayService) GetXrayErr() error {
	cur := currentProcess()
	if cur == nil {
		return nil
	}
	return cur.GetErr()
}

func (s *XrayService) GetXrayResult() string {
	procMu.RLock()
	cached, cur := result, p
	procMu.RUnlock()
	if cached != "" {
		return cached
	}
	if cur == nil || cur.IsRunning() {
		return ""
	}
	out := cur.GetResult()
	procMu.Lock()
	if p == cur { // 期间换过进程的话，不把旧进程的输出记成新进程的
		result = out
	}
	procMu.Unlock()
	return out
}

func (s *XrayService) GetXrayVersion() string {
	cur := currentProcess()
	if cur == nil {
		return "Unknown"
	}
	return cur.GetVersion()
}

func (s *XrayService) GetXrayConfig() (*xray.Config, error) {
	templateConfig, err := s.settingService.GetXrayConfigTemplate()
	if err != nil {
		return nil, err
	}

	xrayConfig := &xray.Config{}
	err = json.Unmarshal([]byte(templateConfig), xrayConfig)
	if err != nil {
		return nil, err
	}
	// Config 不认识的顶层段在上面这一步被静默丢掉了，至少让管理员在日志里看到。
	xray.WarnDroppedTemplateKeys([]byte(templateConfig))

	// 「补全流量识别」：在生成时补，不改库里的入站（关掉开关就回到管理员原来的嗅探设置）。
	forceSniffing := s.settingService.GetForceSniffing()
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	for _, inbound := range inbounds {
		if !inbound.Enable {
			continue
		}
		inboundConfig := inbound.GenXrayInboundConfig()
		if forceSniffing {
			inboundConfig.Sniffing = []byte(forcedSniffing(string(inbound.Protocol), string(inboundConfig.Sniffing)))
		}
		xrayConfig.InboundConfigs = append(xrayConfig.InboundConfigs, *inboundConfig)
	}

	if err := s.routingInjector.Inject(xrayConfig); err != nil {
		return nil, err
	}

	// 必须排在 routingInjector 之后：那一步重写了整个 outbounds 数组，
	// 本注入器要往它的首位加 domainStrategy。
	if err := s.dnsInjector.Inject(xrayConfig); err != nil {
		return nil, err
	}
	// 必须排在 dnsInjector 之后：dokodemo 的直连副本要带上默认出站的 domainStrategy。
	if err := applyDokodemoDirect(xrayConfig); err != nil {
		return nil, err
	}

	// 访问日志开关变化会改动配置字节，Config.Equals 因此能察觉，
	// 那个 10 秒的重启消费任务会自动让它生效，不需要重启面板。
	accessLogEnable, err := s.settingService.GetAccessLogEnable()
	if err != nil {
		return nil, err
	}
	if err := injectAccessLog(xrayConfig, accessLogEnable, accessLogPath); err != nil {
		return nil, err
	}

	// 打开出站流量统计。没有它，计量出站的计数器根本不会被注册
	//（见 injectOutboundStats 的注释）。与访问日志开关一样，改动会体现在
	// 配置字节里，Config.Equals 能察觉；但 policy 段没有运行时重载接口，
	// 所以升级后的第一次配置变更会触发一次整进程重启。
	if err := injectOutboundStats(xrayConfig); err != nil {
		return nil, err
	}

	// 端口被本机其它程序占着的入站不下发（见 xray_port_conflict.go）。必须是最后一步。
	applyPortConflicts(xrayConfig)
	return xrayConfig, nil
}

func (s *XrayService) GetXrayTraffic() ([]*xray.Traffic, error) {
	cur := currentProcess()
	if cur == nil || !cur.IsRunning() {
		return nil, errors.New("xray is not running")
	}
	return cur.GetTraffic(true)
}

func (s *XrayService) RestartXray(isForce bool) error {
	xrayChangeSeq.Inc()
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("restart xray, force:", isForce)

	// 端口冲突：先注销已经空出来的，生成之后再探测运行中的核心没占着的端口（xray_port_conflict.go）。
	recoverPortConflicts()
	xrayConfig, err := s.GetXrayConfig()
	if err != nil {
		return err
	}
	excludeBusyInbounds(xrayConfig, runningConfigOf(p))

	if p != nil && p.IsRunning() {
		if !isForce && p.GetConfig().Equals(xrayConfig) {
			logger.Debug("not need to restart xray")
			// 核心跑的就是这一份：它的地区规则结果（降级状态、dat 代码）如实记为已生效。不提交的话，与
			// 核心字节相同、只有降级状态不同的生成会让每一轮都再置一次重启标志。
			commitGeoApplied(xrayConfig)
			return nil
		}
		// 配置确实变了，但改动可能全都落在核心支持运行时重载的部分。
		// 能热应用就不重启——重启会掐断所有人的连接，而绝大多数改动
		// （加减分流规则、增删入站）本来不需要付这个代价。
		//
		// 成功时的日志由 tryHotApply 自己打：diff 为空（只有格式差异）与
		// 真的发了 RPC 是两码事，混在一句「已通过控制面热应用」里会让
		// 「明明什么都没改却说下发了」的日志误导排查。
		if !isForce && s.tryHotApply(p, xrayConfig) {
			commitGeoApplied(xrayConfig)
			return nil
		}
		// 热应用中途登记的端口冲突（AddInbound 撞上端口占用之后又有别的 RPC 失败）也不能带进新核心。
		excludePortConflicts(xrayConfig)
		// 先把新配置写下去再停核心：写不下去（磁盘满、只读）时旧核心照常跑，而不是整机断网。
		if err := xray.WriteConfigFile(xrayConfig); err != nil {
			return err
		}
		s.drainTraffic(p)
		// 旧进程停不下来（SIGKILL 之后 5 秒还没退出）就不起新的：它还占着全部入站端口，新核心必然 bind
		// 失败；而换掉 p 之后面板再也不持有那个卡住的进程，它只能等 finalizer 碰巧去杀。留着它、返回错误，
		// 重启标志由调用方放回，下一轮再停一次。中途自己退出了（ErrNotRunning）则照常起新的。
		if err := stopCore(p); err != nil && !errors.Is(err, xray.ErrNotRunning) {
			return common.NewError("停止旧的 xray 进程失败，没有启动新进程:", err)
		}
	}

	swapProcess(xray.NewProcess(xrayConfig))
	if err := p.Start(); err != nil {
		return err
	}
	commitGeoApplied(xrayConfig)
	xrayApplyGen.Inc()
	return nil
}

// tryHotApply 尝试把运行中的核心调成 newCfg 的样子，成功返回 true。
//
// 任何一步失败都返回 false，调用方随即全量重启——重启会把已经应用的那部分
// 一起清掉，所以中途失败不需要单独回滚。
//
// 调用方必须持有包级的 lock。
func (s *XrayService) tryHotApply(process *xray.Process, newCfg *xray.Config) bool {
	diff, ok := xray.ComputeHotDiff(process.GetConfig(), newCfg)
	if !ok {
		return false
	}
	// 必须在 SetConfig 之前判定：要拿下发前核心持有的那份配置来比。
	meterOnly := xray.OnlyTouchesTags(process.GetConfig(), newCfg, diff, model.IsMeterTag)
	if diff.Empty() {
		// 配置只有格式差异（空白、key 顺序）。同步快照即可，否则下一轮
		// cron 还会认为配置不同，白重启一次。没有任何 RPC 发生，措辞上
		// 不能说成「已下发」。
		process.SetConfig(newCfg)
		logger.Info("xray 配置无实质变化，无需下发到控制面")
		return true
	}

	// 删除/修改的入站在旧端口上的已建立连接要断掉（见 xray_conn_kill.go）；本平台断不了就整进程重启。
	killPorts := hotDiffKillPorts(process.GetConfig(), diff)
	if len(killPorts) > 0 && !connKillSupported() {
		logger.Debug("热应用：本平台断不开被删除/修改入站上的已建立连接，退回重启")
		return false
	}

	apiPort := process.GetAPIPort()
	if apiPort <= 0 {
		logger.Debug("热应用：拿不到 xray 控制面端口，退回重启")
		return false
	}

	// 专用连接：查流量那条是每次现开现关的，两者生命周期不同。
	api := xray.XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		logger.Debug("热应用：连接 xray 控制面失败，退回重启:", err)
		return false
	}
	defer api.Close()

	// 顺序有讲究：先删后加，避免同 tag 的新旧对象在核心里撞名；
	// 出站先于路由下发，否则新规则会引用还不存在的出站 tag。
	for _, tag := range diff.RemovedInboundTags {
		if err := api.DelInbound(tag); err != nil {
			logger.Debug("热应用：删除入站 [", tag, "] 失败，退回重启:", err)
			return false
		}
	}
	// 端口被占的入站不退回重启（新核心同样 bind 不上，整机断网），登记冲突、其余改动照常下发。
	var busy map[string]bool
	for _, raw := range diff.AddedInbounds {
		if err := api.AddInbound(raw); err != nil {
			if tag, spec, ok := addedInboundSpec(raw); ok && isBindError(err) {
				registerPortConflict(tag, spec, err)
				if busy == nil {
					busy = map[string]bool{}
				}
				busy[tag] = true
				continue
			}
			logger.Debug("热应用：新增入站失败，退回重启:", err)
			return false
		}
	}
	// 出站先删后加：xray 的 AddHandler 对已存在的 tag 会报 existing tag
	// found，先加后删无法处理「编辑一个出站」这种同 tag 换内容的场景。
	// 代价是编辑出站会有一个窗口——DelOutbound 与 AddOutbound 之间——引用
	// 该 tag 的规则在核心里悬空；CLAUDE.md「xray 会静默接受错误配置」一节
	// 记载的实测结论是，xray 对悬空 outboundTag 不报错，运行时静默回落
	// 默认出站（直连）。这里刻意不做任何补偿（比如换个临时 tag 分两步搬）：
	// 正常情况窗口只有一次回环 RPC（毫秒级），若 AddOutbound 失败，窗口会
	// 拉长到退回重启完成（1~2 秒），但那已经在失败路径上——任何补偿逻辑
	// 都是在增加新的失败面，与本子系统「失败即退回重启，不做部分回滚」的
	// 设计原则冲突。
	for _, tag := range diff.RemovedOutboundTags {
		if err := api.DelOutbound(tag); err != nil {
			logger.Debug("热应用：删除出站 [", tag, "] 失败，退回重启:", err)
			return false
		}
	}
	for _, raw := range diff.AddedOutbounds {
		if err := api.AddOutbound(raw); err != nil {
			logger.Debug("热应用：新增出站失败，退回重启:", err)
			return false
		}
	}
	if _, err := applyRoutingDiff(&api, process.GetConfig(), newCfg, diff, meterOnly); err != nil {
		logger.Debug("热应用：下发路由配置失败，退回重启:", err)
		return false
	}
	if err := killInboundConns(killPorts); err != nil {
		logger.Warning("热应用：", err, "，退回整进程重启以保证旧连接断开")
		return false
	}

	// 核心实际持有的配置：没加进去的入站不能记成已下发，否则下一轮比对以为它已经在核心里了。
	applied := newCfg
	if len(busy) > 0 {
		applied = withoutInbounds(newCfg, busy)
	}
	if meterOnly {
		// 只换了计量池：不推进应用代次，分流审计的结论继续有效（见 xrayApplyGen 的说明）。
		process.SetConfig(applied)
		logger.Debug("xray 计量池改动已通过控制面下发，无需重启")
		return true
	}
	process.SetConfig(applied)
	xrayApplyGen.Inc()
	logger.Info("xray 配置改动已通过控制面下发，无需重启")
	return true
}

func (s *XrayService) StopXray() error {
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("stop xray")
	if s.IsXrayRunning() {
		s.drainTraffic(p)
		return stopCore(p)
	}
	return xray.ErrNotRunning
}

// stopCore 停掉核心进程。做成变量只为测试能模拟「SIGKILL 之后进程迟迟不退出」。
var stopCore = (*xray.Process).Stop

func (s *XrayService) SetToNeedRestart() {
	xrayChangeSeq.Inc()
	isNeedXrayRestart.Store(true)
}

func (s *XrayService) IsNeedRestartAndSetFalse() bool {
	return isNeedXrayRestart.CAS(true, false)
}

// XrayRuntime 是某一刻核心运行状态的只读快照，供路由验证使用。
type XrayRuntime struct {
	Running    bool   // 核心进程在运行
	Busy       bool   // 包级 lock 被占用（重启/热应用进行中），其余字段无效
	Pending    bool   // 库里合成的配置与核心当前持有的不一致（有改动尚未生效）
	APIPort    int    // 核心 gRPC 控制面端口
	DefaultTag string // 核心实际持有的配置里，数组首位出站的 tag
	ApplyGen   uint64 // 应用代次，在持锁那一瞬间读取，与上面的 p 状态一致
}

// RuntimeSnapshot 读取核心当前的运行状态，纯只读。
//
// lock 只用 TryLock 持有到读完 p 的那一刻：重启与热应用会长时间占着它，
// 验证请求不能被它们卡住，也不能在持锁时发 RPC 或合成配置（GetXrayConfig
// 要读库、exec xray）。拿不到锁就如实返回 Busy，由调用方提示稍后重试。
func (s *XrayService) RuntimeSnapshot() XrayRuntime {
	return s.runtimeSnapshot(nil)
}

// runtimeSnapshot 是 RuntimeSnapshot 的实现；memo 非 nil 时待生效判定（要合成一次配置）
// 走记忆，其余字段（Busy/Running/端口/代次）每次现读。
func (s *XrayService) runtimeSnapshot(memo *pendingMemo) XrayRuntime {
	// 先于读 p 取改动序号：判定期间若有新改动，记忆里存的是旧序号，下一次查询必然失效。
	changeSeq := xrayChangeSeq.Load()
	if !lock.TryLock() {
		return XrayRuntime{Busy: true}
	}
	process := p
	applyGen := xrayApplyGen.Load()
	var (
		running bool
		apiPort int
		current *xray.Config
	)
	if process != nil && process.IsRunning() {
		apiPort = process.GetAPIPort()
		// 进程在跑但拿不到控制面端口（api 入站缺失或尚未解析）时，调用方没法
		// 向核心发任何 RPC，等同于不可验证，按未运行处理。
		running = apiPort > 0
		current = process.GetConfig()
	}
	lock.Unlock()

	if !running {
		return XrayRuntime{ApplyGen: applyGen}
	}
	rt := XrayRuntime{
		Running:  true,
		APIPort:  apiPort,
		ApplyGen: applyGen,
		// 用运行中快照而不是新合成的配置：核心实际持有的是前者。
		DefaultTag: defaultOutboundTag(current),
	}
	compute := func() (bool, bool) {
		fresh, err := s.GetXrayConfig()
		if err != nil {
			// 无法判断是否有待生效改动，按无处理，不阻断验证；这种结论不进记忆。
			logger.Warning("路由验证：合成最新 xray 配置失败，无法判断待生效改动：", err)
			return false, false
		}
		return current != nil && !current.Equals(fresh), true
	}
	if memo == nil {
		rt.Pending, _ = compute()
	} else {
		rt.Pending = memo.get(pendingKey{applyGen: applyGen, changeSeq: changeSeq, current: current}, compute)
	}
	return rt
}

// pendingKey 是待生效判定记忆的键。三项任一变化都说明核心或库可能换了样子：
// 代次（核心载入了新配置）、改动序号（有人要求下发改动）、核心持有的配置对象
// （SetConfig 会换成新对象，含空 diff 的同步）。
type pendingKey struct {
	applyGen, changeSeq uint64
	current             *xray.Config
}

// pendingMemo 记住最近一次待生效判定，在 ttl 内且键不变时复用，省掉一次 GetXrayConfig
// （读库 + 合成整份配置）。只给规则列表/侧栏的 60 秒轮询用：开着几个标签页时，每一轮
// 每个页面都合成一次配置在小机器上不划算。
//
// 为什么不会误给绿：库里的改动只有经 SetToNeedRestart 或 RestartXray 才会下发，两者都先
// 递增 xrayChangeSeq，键随之失效，下一次查询现算；核心换配置会涨代次或换配置对象，同样
// 失效。剩下的只有「改了库却不要求下发」这种本身就不会生效的写入，它最多晚 ttl 才显示
// 待生效。判定出错的结论不记。
type pendingMemo struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time // nil 时用 time.Now，测试可注入
	valid   bool
	key     pendingKey
	at      time.Time
	pending bool
}

// get 返回键 key 的待生效判定：记忆有效则直接用，否则调 compute（返回判定与是否可记）。
// compute 在持 mu 时执行：并发的轮询排队等同一次合成，而不是各合成一次。mu 与包级 lock 无关。
func (m *pendingMemo) get(key pendingKey, compute func() (pending, ok bool)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	t := now()
	if m.valid && m.key == key && t.Sub(m.at) < m.ttl && !t.Before(m.at) {
		return m.pending
	}
	pending, ok := compute()
	m.valid = ok
	if ok {
		m.key, m.at, m.pending = key, t, pending
	}
	return pending
}

// summaryPendingMemo 是 Summaries 用的待生效判定记忆，5 秒内同键复用。
var summaryPendingMemo = &pendingMemo{ttl: 5 * time.Second}

// defaultOutboundTag 返回配置里数组首位出站的 tag；取不到返回空串。
// 首位出站是 xray 的默认出站，没有命中任何规则的流量走它。
func defaultOutboundTag(cfg *xray.Config) string {
	if cfg == nil {
		return ""
	}
	var outbounds []struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(cfg.OutboundConfigs, &outbounds); err != nil || len(outbounds) == 0 {
		return ""
	}
	return outbounds[0].Tag
}

// routingAPI 是热应用下发路由时用到的控制面调用；xray.XrayAPI 实现它，测试用假的。
type routingAPI interface {
	ApplyRoutingConfig(routing []byte) error
	RemoveRules(ruleTags []string) error
	AppendRoutingRules(routing []byte) error
}

// applyRoutingDiff 把 diff 里的路由改动下发给核心，返回是否走了增量。
//
// 只换了计量池（meterOnly）时增量下发：按 ruleTag 删掉退池的计量规则，只把新进的接到表尾，其余规则留在核心里
// 不重建（xray.TailRuleDelta）。整表下发会让 xray 把每条规则重建一遍：内联的域名与 IP 段不进它的缓存、每建一个
// 匹配器强制 GC 两次，计量规则带 IP 守卫，一条 4 次。计量池每小时都换，整表下发一次在 1 核的生产机上让 xray
// 满核约 9 秒（2026-11-12 香港生产机实测）；同版本 xray 本机实测整表 200 次强制 GC，增量 18 次。
//
// 算不出增量（生成的配置不满足 TailRuleDelta 的前提，比如计量规则后面还有别的规则、ruleTag 重名）或者增量的任何一步
// 失败，都退回整表下发：整表替换不依赖核心当前的状态，半途删掉的规则会被它补齐。不直接判热应用失败——那会整进程
// 重启 xray、掐断所有人的连接，而换池每小时都有。整表下发也失败才返回 error，由调用方退回重启。
func applyRoutingDiff(api routingAPI, oldCfg, newCfg *xray.Config, diff *xray.HotDiff, meterOnly bool) (bool, error) {
	if diff.RoutingConfig == nil {
		return false, nil
	}
	if meterOnly {
		delta, err := xray.TailRuleDelta(oldCfg, newCfg, model.IsMeterTag)
		if err != nil {
			logger.Info("计量池改动无法增量下发，改为整表下发路由：", err)
		} else if err := applyRuleDelta(api, delta); err != nil {
			logger.Warning("计量规则增量下发失败，改为整表下发路由：", err)
		} else {
			logger.Infof("xray 计量池改动已增量下发：删除 %d 条、追加 %d 条计量规则，无需重启", len(delta.Remove), delta.AppendCount)
			return true, nil
		}
	}
	return false, api.ApplyRoutingConfig(diff.RoutingConfig)
}

func applyRuleDelta(api routingAPI, delta *xray.RuleDelta) error {
	if len(delta.Remove) > 0 {
		if err := api.RemoveRules(delta.Remove); err != nil {
			return err
		}
	}
	if delta.AppendCount > 0 {
		return api.AppendRoutingRules(delta.AppendRouting)
	}
	return nil
}
