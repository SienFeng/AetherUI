package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/util/tcshape"
)

// shapingRollbackTimeout 是「下发了新的限速配置后，多久没能确认面板仍可访问
// 就自动撤销」。
//
// tc 配错会掐掉整机流量，包括 SSH——那意味着这台机器只能从服务商控制台救。
// 这道自动撤销是最后一层保险：最坏情况从「丢掉机器」降级成「等两分钟」。
const shapingRollbackTimeout = 2 * time.Minute

// platformShaper 把平台相关的三个动作收成一个接口。
//
// 这是本包里唯一一处为可测性开的口子：真正的 tc 执行只能在 Linux 上跑，
// 但「什么时候下发、什么时候自动撤销」这套状态机才是危险所在，必须能测。
type platformShaper interface {
	Supported() bool
	Run([]tcshape.Command) error
	DetectInterface() (string, error)
	IsActive() bool
}

type realShaper struct{}

func (realShaper) Supported() bool                  { return tcshape.Supported }
func (realShaper) Run(c []tcshape.Command) error    { return tcshape.Run(c) }
func (realShaper) DetectInterface() (string, error) { return tcshape.DetectInterface() }
func (realShaper) IsActive() bool                   { return tcshape.IsActive() }

// 跨请求状态按项目惯例放包级变量，service 本身保持无状态空结构体。
var (
	shaper platformShaper = realShaper{}

	shapingLock sync.Mutex
	// shapingAppliedHash 是当前已生效配置的哈希，空表示没下发过。
	shapingAppliedHash string
	// shapingStarted 为 false 表示还没做过第一次下发。启动时把库里既有的
	// 配置重新下发一遍不武装自动撤销——那是一套已经在用的配置。
	shapingStarted bool
	// shapingPendingSince 非零表示有一次变更还没被确认。
	shapingPendingSince time.Time
	// shapingRejectedHash 是被自动撤销过的那套配置。不记住它的话，
	// 下一轮又会下发同一套，变成「下发、撤销、再下发」的死循环。
	shapingRejectedHash string
	shapingRolledBack   bool
)

// resetShapingState 只在测试之间清状态。
func resetShapingState() {
	shapingLock.Lock()
	defer shapingLock.Unlock()
	shapingAppliedHash = ""
	shapingStarted = false
	shapingPendingSince = time.Time{}
	shapingRejectedHash = ""
	shapingRolledBack = false
}

// ShapingStatus 供界面显示当前限速状态。
type ShapingStatus struct {
	Supported bool   `json:"supported"`
	Active    bool   `json:"active"`
	Interface string `json:"interface"`
	// RolledBack 为 true 表示上一次下发因为无法确认面板可访问而被自动撤销。
	RolledBack bool   `json:"rolledBack"`
	Reason     string `json:"reason"`
}

type ShapingService struct {
	settingService SettingService
	inboundService InboundService
}

// desiredLimits 返回当前应当生效的限速集合。
func (s *ShapingService) desiredLimits() ([]tcshape.Limit, error) {
	var inbounds []*model.Inbound
	err := database.GetDB().Model(model.Inbound{}).
		Where("enable = ? and (up_mbit > 0 or down_mbit > 0)", true).
		Order("id asc").
		Find(&inbounds).Error
	if err != nil {
		return nil, err
	}
	limits := make([]tcshape.Limit, 0, len(inbounds))
	for _, in := range inbounds {
		limits = append(limits, tcshape.Limit{
			InboundId: in.Id, Port: in.Port,
			UpMbit: in.UpMbit, DownMbit: in.DownMbit,
		})
	}
	return limits, nil
}

func limitsHash(iface string, limits []tcshape.Limit) string {
	var sb strings.Builder
	sb.WriteString(iface)
	for _, l := range limits {
		fmt.Fprintf(&sb, "|%d:%d:%d:%d", l.InboundId, l.Port, l.UpMbit, l.DownMbit)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:8])
}

// resolveInterface 优先用管理员配置的网卡名，留空才自动探测。
func (s *ShapingService) resolveInterface() (string, error) {
	configured, err := s.settingService.GetTCInterface()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured), nil
	}
	return shaper.DetectInterface()
}

// Reconcile 让内核里的限速规则与库里的配置一致。
//
// 幂等：每轮重新计算期望状态，与已生效的哈希比较，不同才推倒重建。
func (s *ShapingService) Reconcile() error {
	limits, err := s.desiredLimits()
	if err != nil {
		return err
	}

	shapingLock.Lock()
	defer shapingLock.Unlock()

	if len(limits) == 0 {
		// §4.5：没有任何入站配限速时不碰 root qdisc。管理员可能自己配了
		// fq_codel/cake，擅自接管会把他的配置冲掉。
		// 但如果限速正由本面板接管（专属 ifb 设备还在），就得拆干净——
		// 面板重启后内存状态没了，内核里的规则还在。
		if shapingAppliedHash == "" && !shaper.IsActive() {
			shapingStarted = true
			return nil
		}
		if err := s.teardownLocked(); err != nil {
			return err
		}
		shapingStarted = true
		return nil
	}

	if !shaper.Supported() {
		return tcshape.ErrUnsupported
	}

	iface, err := s.resolveInterface()
	if err != nil {
		return common.NewError("无法确定要限速的网卡:", err)
	}
	hash := limitsHash(iface, limits)
	if hash == shapingAppliedHash {
		return nil
	}
	if hash == shapingRejectedHash {
		// 这套配置刚被自动撤销过，改之前不再重试。
		return nil
	}

	cmds, err := tcshape.BuildApplyPlan(iface, limits)
	if err != nil {
		return err
	}
	if err := shaper.Run(cmds); err != nil {
		// 失败绝不记成「已生效」，否则下一轮以为无需重试，限速永远不生效
		// 而面板显示一切正常。
		return err
	}

	shapingAppliedHash = hash
	shapingRolledBack = false
	if shapingStarted {
		// 运行中的变更才武装自动撤销：这时管理员就在面板前面。
		shapingPendingSince = time.Now()
	}
	shapingStarted = true
	logger.Warning("已下发端口限速规则, 网卡:", iface, "入站数:", len(limits))
	return nil
}

// Heartbeat 由已登录的面板请求调用，表示管理员仍能访问面板。
func (s *ShapingService) Heartbeat() {
	shapingLock.Lock()
	defer shapingLock.Unlock()
	shapingPendingSince = time.Time{}
}

// CheckRollback 在超时仍未确认面板可访问时自动撤销限速。
func (s *ShapingService) CheckRollback(now time.Time) {
	shapingLock.Lock()
	defer shapingLock.Unlock()

	if shapingPendingSince.IsZero() || now.Sub(shapingPendingSince) < shapingRollbackTimeout {
		return
	}
	rejected := shapingAppliedHash
	if err := s.teardownLocked(); err != nil {
		logger.Error("自动撤销限速失败, 机器可能已失联:", err)
		return
	}
	shapingRejectedHash = rejected
	shapingRolledBack = true
	logger.Warning("下发限速后在", shapingRollbackTimeout,
		"内没有收到任何面板请求，已自动撤销全部限速规则。若确认网络正常，请修改限速配置后重试")
}

// Teardown 清除全部限速规则。
func (s *ShapingService) Teardown() error {
	shapingLock.Lock()
	defer shapingLock.Unlock()
	return s.teardownLocked()
}

func (s *ShapingService) teardownLocked() error {
	iface, err := s.resolveInterface()
	if err != nil {
		// 探测不出网卡时也要尽力拆掉 ifb 那半边，否则重定向会一直挂着。
		iface = ""
	}
	cmds := tcshape.BuildTeardownPlan(iface)
	if len(cmds) == 0 {
		// 网卡名不合法/探测不出来时绝不拿一个可疑的名字去执行删除，
		// 但至少要把自己建的 ifb 拆掉，否则重定向会一直挂着。
		logger.Warning("无法确定要清除限速的网卡, 只清除本面板专用的 ifb 设备:", err)
		cmds = tcshape.BuildIfbTeardownPlan()
	}
	if err := shaper.Run(cmds); err != nil {
		return err
	}
	shapingAppliedHash = ""
	shapingPendingSince = time.Time{}
	return nil
}

// Status 返回界面要显示的限速状态。
func (s *ShapingService) Status() *ShapingStatus {
	shapingLock.Lock()
	rolledBack := shapingRolledBack
	applied := shapingAppliedHash != ""
	shapingLock.Unlock()

	st := &ShapingStatus{
		Supported:  shaper.Supported(),
		Active:     applied,
		RolledBack: rolledBack,
	}
	if !st.Supported {
		st.Reason = "端口限速依赖 Linux 的 tc，当前系统不支持"
		return st
	}
	if rolledBack {
		st.Reason = "上次下发限速后未能确认面板可访问，已自动撤销全部规则。请检查网络后修改限速配置再试"
	}
	if iface, err := s.resolveInterface(); err == nil {
		st.Interface = iface
	}
	return st
}
