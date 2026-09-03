package service

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/tcshape"
)

// fakeShaper 让限速的状态机能在开发机上跑起来。真正的 tc 执行是 Linux
// 专有的，但「什么时候下发、什么时候自动撤销」这套逻辑才是危险所在，
// 必须测到。
type fakeShaper struct {
	applied   [][]tcshape.Command
	tornDown  int
	iface     string
	runErr    error
	active    bool
	detectErr error
}

func (f *fakeShaper) Supported() bool { return true }
func (f *fakeShaper) Run(cmds []tcshape.Command) error {
	if f.runErr != nil {
		return f.runErr
	}
	// 只有拆除命令的那次算一次「撤销」。
	onlyTeardown := true
	for _, c := range cmds {
		if !c.IgnoreError {
			onlyTeardown = false
			break
		}
	}
	if onlyTeardown {
		f.tornDown++
	} else {
		f.applied = append(f.applied, cmds)
	}
	return nil
}
func (f *fakeShaper) DetectInterface() (string, error) {
	if f.detectErr != nil {
		return "", f.detectErr
	}
	return f.iface, nil
}
func (f *fakeShaper) IsActive() bool { return f.active }

func useFakeShaper(t *testing.T) *fakeShaper {
	t.Helper()
	f := &fakeShaper{iface: "eth0"}
	old := shaper
	shaper = f
	t.Cleanup(func() {
		shaper = old
		resetShapingState()
	})
	resetShapingState()
	return f
}

func setupShapingDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "shaping.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func mkShapedInbound(t *testing.T, port int, enable bool, up, down int) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Tag: "inbound-" + itoaS(port),
		Enable: enable, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
		UpMbit: up, DownMbit: down,
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	return in
}

func TestReconcileDoesNothingWhenNobodyIsLimited(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	mkShapedInbound(t, 40001, true, 0, 0)

	if err := (&ShapingService{}).Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// §4.5：没有任何入站配限速时，一条规则都不下。管理员可能自己配了
	// fq_codel/cake，擅自接管 root qdisc 会把他的配置冲掉。
	if len(f.applied) != 0 || f.tornDown != 0 {
		t.Errorf("没人配限速却动了 tc：applied=%d tornDown=%d", len(f.applied), f.tornDown)
	}
}

func TestReconcileTearsDownStaleRulesWhenLimitsRemoved(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	f.active = true // 上一次运行留下的 tc 规则还在内核里
	mkShapedInbound(t, 40001, true, 0, 0)

	if err := (&ShapingService{}).Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// 面板重启后内存状态没了，但内核里的规则还在。靠专属 ifb 设备是否存在
	// 来判断「限速正由本面板接管」，是的话才拆。
	if f.tornDown != 1 {
		t.Errorf("撤销次数 = %d，期望 1", f.tornDown)
	}
}

func TestReconcileAppliesAndSkipsWhenUnchanged(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}

	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.applied) != 1 {
		t.Fatalf("下发次数 = %d，期望 1", len(f.applied))
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	// 这个任务每 10 秒跑一次。配置没变还重新下发的话，每次都要推倒重建，
	// 期间流量不受整形。
	if len(f.applied) != 1 {
		t.Errorf("配置没变却又下发了一次，总次数 = %d", len(f.applied))
	}
}

func TestReconcileIgnoresDisabledInbounds(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	mkShapedInbound(t, 40001, false, 2, 5)

	if err := (&ShapingService{}).Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// 停用的入站根本不在 xray 配置里，端口上没有流量，给它下规则毫无意义，
	// 还会占一个 classid。
	if len(f.applied) != 0 {
		t.Errorf("给停用的入站下发了限速")
	}
}

func TestFirstApplyOnStartupDoesNotArmRollback(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}

	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	// 面板启动时把库里既有的配置重新下发一遍，那是一套已经在用的配置，
	// 不该因为管理员没在看面板就被自动撤销。
	s.CheckRollback(time.Now().Add(time.Hour))
	if f.tornDown != 0 {
		t.Errorf("启动时的首次下发被自动撤销了")
	}
}

func TestChangedConfigArmsRollbackAndRevertsWhenPanelGoesSilent(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	in := mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}
	if err := s.Reconcile(); err != nil { // 启动首发，不武装
		t.Fatal(err)
	}

	// 管理员改了限速值 → 这是运行中的变更，要武装自动撤销。
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("down_mbit", 50).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != 2 {
		t.Fatalf("下发次数 = %d，期望 2", len(f.applied))
	}

	// 超时之内不该动。
	s.CheckRollback(time.Now())
	if f.tornDown != 0 {
		t.Fatalf("还没超时就撤销了")
	}
	// 超时之后：面板一直没被访问过，说明很可能已经失联，自动撤销。
	s.CheckRollback(time.Now().Add(shapingRollbackTimeout + time.Second))
	if f.tornDown != 1 {
		t.Fatalf("超时未确认却没有自动撤销，tornDown = %d", f.tornDown)
	}
	if st := s.Status(); !st.RolledBack {
		t.Error("撤销之后状态里没有标记，管理员看不到发生过什么")
	}
}

func TestHeartbeatConfirmsAndCancelsRollback(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	in := mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("down_mbit", 50).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// 管理员的浏览器还在正常访问面板，说明网络没断。
	s.Heartbeat()
	s.CheckRollback(time.Now().Add(shapingRollbackTimeout + time.Second))
	if f.tornDown != 0 {
		t.Error("已经确认过面板可访问，却仍然自动撤销了")
	}
}

func TestRolledBackConfigIsNotReappliedUntilChanged(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	in := mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("down_mbit", 50).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	s.CheckRollback(time.Now().Add(shapingRollbackTimeout + time.Second))
	applied := len(f.applied)

	// 撤销之后不能马上又下发同一套配置——那会变成「下发、撤销、再下发」
	// 的死循环，网络反复抖动。
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != applied {
		t.Errorf("被撤销的配置又被下发了")
	}

	// 管理员改了配置（哈希变了）之后应当重新尝试。
	if err := database.GetDB().Model(model.Inbound{}).Where("id = ?", in.Id).
		UpdateColumn("down_mbit", 8).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != applied+1 {
		t.Errorf("改了配置之后没有重新下发")
	}
}

func TestReconcileReportsFailureWithoutMarkingApplied(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	f.runErr = errors.New("tc 挂了")
	mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}

	if err := s.Reconcile(); err == nil {
		t.Fatal("下发失败却没有报错")
	}
	// 失败不能记成「已生效」，否则下一轮会以为无需重试，限速永远不生效
	// 而面板显示一切正常。
	f.runErr = nil
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != 1 {
		t.Errorf("失败之后没有重试，下发次数 = %d", len(f.applied))
	}
}

func TestTeardownClearsStateSoLimitsCanBeReapplied(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	mkShapedInbound(t, 40001, true, 2, 5)
	s := ShapingService{}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}

	if err := s.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if f.tornDown != 1 {
		t.Errorf("撤销次数 = %d，期望 1", f.tornDown)
	}
	// 手动清除之后，下一轮应当按库里的配置重新下发，而不是以为「已生效」。
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(f.applied) != 2 {
		t.Errorf("手动清除后没有重新下发，下发次数 = %d", len(f.applied))
	}
}

func TestReconcileUsesConfiguredInterfaceOverAutoDetect(t *testing.T) {
	setupShapingDB(t)
	f := useFakeShaper(t)
	f.detectErr = errors.New("探测不出来")
	mkShapedInbound(t, 40001, true, 0, 5)
	if err := (&SettingService{}).setString("tcInterface", "ens18"); err != nil {
		t.Fatal(err)
	}

	if err := (&ShapingService{}).Reconcile(); err != nil {
		t.Fatalf("配置了网卡名就不该再去自动探测: %v", err)
	}
	if len(f.applied) != 1 {
		t.Fatalf("下发次数 = %d，期望 1", len(f.applied))
	}
	found := false
	for _, c := range f.applied[0] {
		for _, a := range c.Args {
			if a == "ens18" {
				found = true
			}
		}
	}
	if !found {
		t.Error("生成的命令里没有用上配置的网卡名")
	}
}

func TestUpdateInboundPersistsRateLimits(t *testing.T) {
	setupShapingDB(t)
	s := InboundService{}
	in := &model.Inbound{
		UserId: 1, Port: 40011, Protocol: model.VLESS, Tag: "inbound-40011",
		Enable: true, Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: "{}",
	}
	if err := s.AddInbound(in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	in.UpMbit = 3
	in.DownMbit = 20
	if err := s.UpdateInbound(in); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}
	got, err := s.GetInbound(in.Id)
	if err != nil {
		t.Fatal(err)
	}
	// UpdateInbound 是逐字段复制的，漏掉新字段会让"改了保存后没生效"静默发生。
	if got.UpMbit != 3 || got.DownMbit != 20 {
		t.Errorf("上/下行限速 = %d/%d，期望 3/20", got.UpMbit, got.DownMbit)
	}
}
