package service

import (
	"encoding/json"
	"errors"
	"go.uber.org/atomic"
	"sync"
	"time"

	"a-ui/database/model"
	"a-ui/logger"
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

type XrayService struct {
	inboundService  InboundService
	settingService  SettingService
	routingInjector RoutingInjector
	dnsInjector     DNSInjector
}

func (s *XrayService) IsXrayRunning() bool {
	return p != nil && p.IsRunning()
}

func (s *XrayService) GetXrayErr() error {
	if p == nil {
		return nil
	}
	return p.GetErr()
}

func (s *XrayService) GetXrayResult() string {
	if result != "" {
		return result
	}
	if s.IsXrayRunning() {
		return ""
	}
	if p == nil {
		return ""
	}
	result = p.GetResult()
	return result
}

func (s *XrayService) GetXrayVersion() string {
	if p == nil {
		return "Unknown"
	}
	return p.GetVersion()
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

	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	for _, inbound := range inbounds {
		if !inbound.Enable {
			continue
		}
		inboundConfig := inbound.GenXrayInboundConfig()
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

	return xrayConfig, nil
}

func (s *XrayService) GetXrayTraffic() ([]*xray.Traffic, error) {
	if !s.IsXrayRunning() {
		return nil, errors.New("xray is not running")
	}
	return p.GetTraffic(true)
}

func (s *XrayService) RestartXray(isForce bool) error {
	xrayChangeSeq.Inc()
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("restart xray, force:", isForce)

	xrayConfig, err := s.GetXrayConfig()
	if err != nil {
		return err
	}

	if p != nil && p.IsRunning() {
		if !isForce && p.GetConfig().Equals(xrayConfig) {
			logger.Debug("not need to restart xray")
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
			return nil
		}
		p.Stop()
	}

	p = xray.NewProcess(xrayConfig)
	result = ""
	if err := p.Start(); err != nil {
		return err
	}
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
	for _, raw := range diff.AddedInbounds {
		if err := api.AddInbound(raw); err != nil {
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
	if diff.RoutingConfig != nil {
		if err := api.ApplyRoutingConfig(diff.RoutingConfig); err != nil {
			logger.Debug("热应用：下发路由配置失败，退回重启:", err)
			return false
		}
	}

	if meterOnly {
		// 只换了计量池：不推进应用代次，分流审计的结论继续有效（见 xrayApplyGen 的说明）。
		process.SetConfig(newCfg)
		logger.Debug("xray 计量池改动已通过控制面下发，无需重启")
		return true
	}
	process.SetConfig(newCfg)
	xrayApplyGen.Inc()
	logger.Info("xray 配置改动已通过控制面下发，无需重启")
	return true
}

func (s *XrayService) StopXray() error {
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("stop xray")
	if s.IsXrayRunning() {
		return p.Stop()
	}
	return errors.New("xray is not running")
}

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
