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
		if !isForce && s.tryHotApply(p, xrayConfig) {
			logger.Info("xray 配置改动已通过控制面热应用，无需重启")
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
		// cron 还会认为配置不同，白重启一次。
		process.SetConfig(newCfg)
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
