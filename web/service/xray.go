package service

import (
	"encoding/json"
	"errors"
	"go.uber.org/atomic"
	"sync"
	"a-ui/logger"
	"a-ui/xray"
)

var p *xray.Process
var lock sync.Mutex
var isNeedXrayRestart atomic.Bool
var result string

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

	return xrayConfig, nil
}

func (s *XrayService) GetXrayTraffic() ([]*xray.Traffic, error) {
	if !s.IsXrayRunning() {
		return nil, errors.New("xray is not running")
	}
	return p.GetTraffic(true)
}

func (s *XrayService) RestartXray(isForce bool) error {
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
	return p.Start()
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

	process.SetConfig(newCfg)
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
	isNeedXrayRestart.Store(true)
}

func (s *XrayService) IsNeedRestartAndSetFalse() bool {
	return isNeedXrayRestart.CAS(true, false)
}
