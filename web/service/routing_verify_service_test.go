package service

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/geodata"

	"a-ui/database"
	"a-ui/database/model"
)

// fakeRoute 记录调用次数并按 fn 返回结果，用来替代真实的 TestRoute。
type fakeRoute struct {
	mu    sync.Mutex
	calls []string // inboundTag|target
	fn    func(ctx context.Context, inboundTag, domain string, ip net.IP) (string, bool, error)
}

func (f *fakeRoute) route(ctx context.Context, inboundTag, domain string, ip net.IP) (string, bool, error) {
	target := domain
	if target == "" && ip != nil {
		target = ip.String()
	}
	f.mu.Lock()
	f.calls = append(f.calls, inboundTag+"|"+target)
	f.mu.Unlock()
	return f.fn(ctx, inboundTag, domain, ip)
}

func (f *fakeRoute) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func returnTag(tag string) func(context.Context, string, string, net.IP) (string, bool, error) {
	return func(context.Context, string, string, net.IP) (string, bool, error) { return tag, false, nil }
}

func okRuntime() XrayRuntime {
	return XrayRuntime{Running: true, APIPort: 1, DefaultTag: model.DefaultOutboundTag}
}

func newVerifyService(f *fakeRoute, rt XrayRuntime) *RoutingVerifyService {
	return &RoutingVerifyService{
		route:   f.route,
		runtime: func() XrayRuntime { return rt },
		now:     func() time.Time { return time.UnixMilli(1793520000000) },
	}
}

// verifyFixture 建一个带两个启用入站、一个域名组、一个出站节点的库。
type verifyFixture struct {
	a, b  *model.Inbound
	group *model.DomainGroup
	node  *model.OutboundNode
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	setupAccessLogDB(t)
	f := &verifyFixture{
		a:     newVerifyInbound(t, 41001, "用户A", true),
		b:     newVerifyInbound(t, 41002, "用户B", true),
		group: newTestGroupWithDomains(t, "ChatGPT", []string{"full:chatgpt.com", "domain:openai.com", "keyword:gpt"}),
	}
	// 直接落库，不经 persist：单测不依赖真实 xray 校验。
	f.node = &model.OutboundNode{
		Tag: "a-ui-hk", Remark: "香港节点", Protocol: "socks", Enable: true,
		Config: `{"protocol":"socks","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`,
	}
	if err := database.GetDB().Save(f.node).Error; err != nil {
		t.Fatalf("save node: %v", err)
	}
	return f
}

const sniffingOn = `{"enabled":true,"destOverride":["http","tls"]}`

func newVerifyInbound(t *testing.T, port int, remark string, enable bool) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: port, Protocol: model.VLESS, Enable: true, Remark: remark,
		Tag:      "inbound-" + strconv.Itoa(port),
		// 与新建入站的默认值一致：sniffing 开启且 destOverride 含 http/tls，
		// 否则 sniffingBlind 会把域名样例全部剔掉。关 sniffing 的场景单独构造。
		Settings: vlessSettings(), StreamSettings: plainTCPStream, Sniffing: sniffingOn,
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	if !enable {
		// Save 时 Enable=false 会被 gorm 当零值忽略，单独更新。
		if err := database.GetDB().Model(in).Update("enable", false).Error; err != nil {
			t.Fatalf("disable inbound: %v", err)
		}
		in.Enable = false
	}
	return in
}

func (f *verifyFixture) addRule(t *testing.T, action string, inboundIds []int) *model.RoutingRule {
	t.Helper()
	rule := &model.RoutingRule{
		Remark: "verify", InboundIds: mustEncodeIds(t, inboundIds),
		DomainGroupIds: mustEncodeGroupIds(t, []int{f.group.Id}),
		Action:         action, Enable: true,
	}
	if action == model.ActionProxy {
		rule.OutboundId = f.node.Id
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	return rule
}

func findUser(t *testing.T, res *RuleVerifyResult, inboundId int) UserVerify {
	t.Helper()
	for _, u := range res.Users {
		if u.InboundId == inboundId {
			return u
		}
	}
	t.Fatalf("结果里没有入站 %d: %+v", inboundId, res.Users)
	return UserVerify{}
}

func setAccessLog(t *testing.T, on bool) {
	t.Helper()
	if err := (&SettingService{}).SetAccessLogEnable(on); err != nil {
		t.Fatalf("SetAccessLogEnable: %v", err)
	}
}

func TestVerifyVerified(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	setAccessLog(t, true)
	now := time.UnixMilli(1793520000000)
	if err := database.GetAccessLogDB().Create(&model.AccessLog{
		Time: now.Add(-time.Hour).UnixMilli(), InboundId: f.a.Id, InboundTag: f.a.Tag,
		Target: "api.openai.com:443", Route: f.node.Tag, Accepted: true,
	}).Error; err != nil {
		t.Fatalf("insert access log: %v", err)
	}

	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateOK {
		t.Fatalf("Core.State = %q，期望 ok: %+v", res.Core.State, res.Core)
	}
	if res.Expected.Action != model.ActionProxy || res.Expected.Tag != f.node.Tag || res.Expected.Label != "香港节点" {
		t.Errorf("Expected = %+v", res.Expected)
	}
	if res.CheckedAt != now.UnixMilli() {
		t.Errorf("CheckedAt = %d", res.CheckedAt)
	}
	if len(res.Users) != 1 {
		t.Fatalf("规则只覆盖 A，Users 应只有 1 个: %+v", res.Users)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictVerified {
		t.Fatalf("A 应为 verified: %+v", u)
	}
	if u.Name != "用户A" {
		t.Errorf("Name = %q", u.Name)
	}
	// keyword:gpt 推不出样例，只剩 full: 与 domain: 两个。
	if len(u.Samples) != 2 || u.Samples[0].Target != "chatgpt.com" || u.Samples[1].Target != "openai.com" {
		t.Errorf("Samples = %+v", u.Samples)
	}
	if u.Samples[0].ActualLabel != "香港节点" || !u.Samples[0].Match || !u.Samples[0].HitByRule {
		t.Errorf("Samples[0] = %+v", u.Samples[0])
	}
	if !res.AccessLog.Enabled || res.AccessLog.WindowHours != 24 {
		t.Errorf("AccessLog = %+v", res.AccessLog)
	}
	if u.Log == nil || u.Log.Matched != 1 {
		t.Errorf("Log = %+v", u.Log)
	}
	if fr.count() != 2 {
		t.Errorf("route 调用 %d 次，期望 2", fr.count())
	}
	for _, c := range fr.calls {
		if c[:len(f.a.Tag)] != f.a.Tag {
			t.Errorf("TestRoute 的 inboundTag 应为 %s: %s", f.a.Tag, c)
		}
	}
}

func TestVerifyMismatch(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	res, err := newVerifyService(&fakeRoute{fn: returnTag(model.DefaultOutboundTag)}, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictMismatch {
		t.Fatalf("应为 mismatch: %+v", u)
	}
	if u.Samples[0].ActualTag != model.DefaultOutboundTag || u.Samples[0].ActualLabel == "" {
		t.Errorf("Samples[0] = %+v", u.Samples[0])
	}
	if u.Samples[0].ActualLabel != "直连（默认出站）" {
		t.Errorf("默认出站的 ActualLabel = %q", u.Samples[0].ActualLabel)
	}
}

func TestVerifyNoMatchLabel(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionBlock, []int{f.a.Id})
	fr := &fakeRoute{fn: func(context.Context, string, string, net.IP) (string, bool, error) { return "", true, nil }}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Expected.Tag != model.BlockOutboundTag || res.Expected.Label != "封禁" {
		t.Errorf("Expected = %+v", res.Expected)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictMismatch || u.Samples[0].ActualLabel != "默认出站（未命中任何规则）" {
		t.Errorf("未命中的样例: %+v", u)
	}
}

func TestVerifyDirectExpectsRuntimeDefaultTag(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionDirect, []int{f.a.Id})
	rt := okRuntime()
	rt.DefaultTag = "my-freedom"
	res, err := newVerifyService(&fakeRoute{fn: returnTag("my-freedom")}, rt).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Expected.Tag != "my-freedom" || res.Expected.Label != "直连" {
		t.Errorf("Expected = %+v", res.Expected)
	}
	if u := findUser(t, res, f.a.Id); u.Verdict != verdictVerified {
		t.Errorf("应为 verified: %+v", u)
	}
}

func TestVerifyPending(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	rt := okRuntime()
	rt.Pending = true
	fr := &fakeRoute{fn: returnTag(model.DefaultOutboundTag)}
	res, err := newVerifyService(fr, rt).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStatePending {
		t.Fatalf("Core.State = %q", res.Core.State)
	}
	if u := findUser(t, res, f.a.Id); u.Verdict != verdictPending || len(u.Samples) == 0 {
		t.Errorf("等待下发时用户应为 pending 且仍带样例明细: %+v", u)
	}
	if fr.count() == 0 {
		t.Error("等待下发时仍应执行 TestRoute")
	}
}

func TestVerifyNotRunning(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, XrayRuntime{}).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateNotRunning {
		t.Fatalf("Core.State = %q", res.Core.State)
	}
	if u := findUser(t, res, f.a.Id); u.Verdict != verdictUnverifiable {
		t.Errorf("应为 unverifiable: %+v", u)
	}
	if fr.count() != 0 {
		t.Errorf("核心未运行时不应调用 route，实际 %d 次", fr.count())
	}
}

func TestVerifyBusySnapshot(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, XrayRuntime{Busy: true}).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateBusy {
		t.Fatalf("Core.State = %q", res.Core.State)
	}
	if u := findUser(t, res, f.a.Id); u.Verdict != verdictUnverifiable {
		t.Errorf("应为 unverifiable: %+v", u)
	}
	if fr.count() != 0 {
		t.Errorf("核心忙时不应调用 route，实际 %d 次", fr.count())
	}
}

func TestVerifyRejectsConcurrentRun(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	verifyMu.Lock()
	defer verifyMu.Unlock()
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateBusy || res.Core.Message != "已有验证在进行，请稍后" {
		t.Errorf("Core = %+v", res.Core)
	}
	if res.Users == nil || len(res.Users) != 0 {
		t.Errorf("Users 应为空数组: %#v", res.Users)
	}
	if fr.count() != 0 {
		t.Errorf("不应调用 route，实际 %d 次", fr.count())
	}
}

func TestVerifyDisabledInboundIsUnverifiable(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id, f.b.Id})
	if err := database.GetDB().Model(f.b).Update("enable", false).Error; err != nil {
		t.Fatalf("disable B: %v", err)
	}
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if u := findUser(t, res, f.a.Id); u.Verdict != verdictVerified {
		t.Errorf("A 应为 verified: %+v", u)
	}
	b := findUser(t, res, f.b.Id)
	if b.Verdict != verdictUnverifiable || b.Reason != "该用户已停用，规则不会对其生效" || !b.Disabled {
		t.Errorf("B = %+v", b)
	}
	// 停用的 B 不参与规则级结论：A 已验证，整条规则就是已验证
	if v, _ := ruleVerdictOf(res); v != verdictVerified {
		t.Errorf("规则级结论 = %s，期望 verified", v)
	}
	for _, c := range fr.calls {
		if len(c) >= len(f.b.Tag) && c[:len(f.b.Tag)] == f.b.Tag {
			t.Errorf("不应对停用的 B 调 route: %s", c)
		}
	}
}

func TestVerifyTimeoutIsUnverifiable(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	fr := &fakeRoute{fn: func(ctx context.Context, _, _ string, _ net.IP) (string, bool, error) {
		<-ctx.Done()
		return "", false, ctx.Err()
	}}
	s := newVerifyService(fr, okRuntime())
	s.rpcTimeout = 50 * time.Millisecond
	res, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictUnverifiable {
		t.Fatalf("应为 unverifiable: %+v", u)
	}
	if len(u.Samples) != 2 {
		t.Fatalf("应有 2 个样例: %+v", u.Samples)
	}
	for _, smp := range u.Samples {
		if smp.Error != "超时" {
			t.Errorf("Sample.Error = %q，期望「超时」", smp.Error)
		}
	}
}

func TestVerifyBudgetExhaustedMarksRemainingAsTimeout(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	fr := &fakeRoute{fn: func(ctx context.Context, _, _ string, _ net.IP) (string, bool, error) {
		<-ctx.Done()
		return "", false, ctx.Err()
	}}
	s := newVerifyService(fr, okRuntime())
	s.rpcTimeout = time.Second
	s.budget = 30 * time.Millisecond
	res, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if len(u.Samples) != 2 || u.Samples[0].Error != "超时" || u.Samples[1].Error != "超时" {
		t.Errorf("Samples = %+v", u.Samples)
	}
	if fr.count() != 1 {
		t.Errorf("预算用尽后不应再调 route，实际 %d 次", fr.count())
	}
}

func TestVerifyGlobalRuleTruncates(t *testing.T) {
	f := newVerifyFixture(t)
	for i := 0; i < 50; i++ {
		newVerifyInbound(t, 42000+i, "批量", true)
	}
	newVerifyInbound(t, 43000, "停用", false)
	rule := f.addRule(t, model.ActionProxy, []int{})
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Truncated || len(res.Users) != 50 {
		t.Fatalf("52 个启用入站应截到 50: Truncated=%v len=%d", res.Truncated, len(res.Users))
	}
	if res.Users[0].InboundId != f.a.Id || res.Users[1].InboundId != f.b.Id {
		t.Errorf("应按 id 升序: %d %d", res.Users[0].InboundId, res.Users[1].InboundId)
	}
	for _, u := range res.Users {
		if u.Name == "停用" {
			t.Errorf("全局规则不应包含停用入站: %+v", u)
		}
	}
}

func TestVerifyAccessLogDisabled(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id, f.b.Id})
	setAccessLog(t, false)
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.AccessLog.Enabled {
		t.Errorf("AccessLog = %+v", res.AccessLog)
	}
	for _, u := range res.Users {
		if u.Log != nil {
			t.Errorf("未开启访问日志时 Log 应为 nil: %+v", u)
		}
	}
}

func TestVerifyAccessLogOtherOutboundIsMismatch(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	setAccessLog(t, true)
	now := time.UnixMilli(1793520000000)
	rows := []model.AccessLog{
		// 走了直连：属于本规则 → 进 Others。
		{Time: now.Add(-time.Hour).UnixMilli(), InboundId: f.a.Id, Target: "chatgpt.com:443", Route: model.DefaultOutboundTag, Accepted: true},
		// 超出 24h 窗口：不计。
		{Time: now.Add(-25 * time.Hour).UnixMilli(), InboundId: f.a.Id, Target: "openai.com:443", Route: "x", Accepted: true},
	}
	if err := database.GetAccessLogDB().Create(&rows).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictMismatch || u.Log == nil || len(u.Log.Others) != 1 {
		t.Fatalf("日志有其他出站应为 mismatch: %+v log=%+v", u, u.Log)
	}
	if o := u.Log.Others[0]; o.Route != model.DefaultOutboundTag || o.Label != "直连（默认出站）" || o.Count != 1 {
		t.Errorf("Others[0] = %+v", o)
	}
}

func TestVerifyDisabledRule(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	if err := database.GetDB().Model(rule).Update("enable", false).Error; err != nil {
		t.Fatalf("disable rule: %v", err)
	}
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateError || res.Core.Message == "" {
		t.Errorf("Core = %+v", res.Core)
	}
	if fr.count() != 0 {
		t.Errorf("不应调用 route，实际 %d 次", fr.count())
	}
}

func TestVerifyDroppedRuleIsError(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	if err := database.GetDB().Model(f.node).Update("enable", false).Error; err != nil {
		t.Fatalf("disable node: %v", err)
	}
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateError || res.Core.Message == "" {
		t.Errorf("节点已停用、规则不会写进配置时应为 error: %+v", res.Core)
	}
	if fr.count() != 0 {
		t.Errorf("不应调用 route，实际 %d 次", fr.count())
	}
}

func TestVerifyMissingRuleReturnsError(t *testing.T) {
	setupAccessLogDB(t)
	if _, err := newVerifyService(&fakeRoute{fn: returnTag("x")}, okRuntime()).Verify(9999); err == nil {
		t.Fatal("规则不存在应返回错误")
	}
}

// 真实 route 连不上控制面时，用户行要带上具体原因，而不是笼统的「核心验证出错」。
func TestVerifyCoreConnectFailureCarriesMessage(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	s := &RoutingVerifyService{
		runtime: func() XrayRuntime {
			return XrayRuntime{Running: true, APIPort: -1, DefaultTag: model.DefaultOutboundTag}
		},
	}
	res, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Core.State != coreStateError || res.Core.Message == "" {
		t.Fatalf("Core = %+v", res.Core)
	}
	u := findUser(t, res, f.a.Id)
	if u.Verdict != verdictUnverifiable || u.Reason != "核心验证出错："+res.Core.Message {
		t.Errorf("用户行应带具体原因: %+v", u)
	}
}

// 开关读不出时要报出来，不能伪装成「未开启」；核心那一路照常给结论。
func TestVerifyAccessLogSettingErrorIsReported(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	s := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	s.accessLogEnabled = func() (bool, error) { return false, errors.New("database is locked") }
	res, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.AccessLog.Enabled || !strings.Contains(res.AccessLog.Error, "database is locked") {
		t.Errorf("AccessLog = %+v", res.AccessLog)
	}
	u := findUser(t, res, f.a.Id)
	if u.Log != nil || u.Verdict != verdictVerified {
		t.Errorf("开关读取失败不应影响核心结论: %+v", u)
	}
}

// 单个用户的日志查询失败：带 Error 的非 nil 证据，既不让用户变红也不让它凭日志变绿。
func TestVerifyAccessLogQueryErrorIsReported(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	setAccessLog(t, true)
	queryErr := func(int, int64, int) ([]TargetRouteCount, bool, error) {
		return nil, false, errors.New("disk I/O error")
	}

	s := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	s.groupLogs = queryErr
	res, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.AccessLog.Enabled || res.AccessLog.Error != "" {
		t.Errorf("AccessLog = %+v", res.AccessLog)
	}
	u := findUser(t, res, f.a.Id)
	if u.Log == nil || !strings.Contains(u.Log.Error, "disk I/O error") {
		t.Fatalf("Log 应带查询错误: %+v", u.Log)
	}
	if u.Verdict != verdictVerified {
		t.Errorf("日志查询失败不应单独让用户变成 %s: %+v", u.Verdict, u)
	}

	// 核心未运行时，日志查询失败也不能产生 verified 或 mismatch。
	s = newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, XrayRuntime{})
	s.groupLogs = queryErr
	res, err = s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if u := findUser(t, res, f.a.Id); u.Verdict != verdictUnverifiable || u.Log == nil || u.Log.Error == "" {
		t.Errorf("核心未运行 + 日志失败应为 unverifiable 且带日志错误: %+v", u)
	}
}

// 全局规则覆盖一个关了 sniffing 的 vmess 入站：核心拿不到它的域名，域名样例在
// TestRoute 里命中也不算证据，没有 IP 样例时只能 unverifiable，且不发域名 RPC。
// 前提是「补全流量识别」关着——开着时生成配置会替它打开嗅探（见下一个测试）。
func TestVerifyGlobalRuleSniffingOffInboundIsUnverifiable(t *testing.T) {
	f := newVerifyFixture(t)
	if err := (&SettingService{}).setInt("forceSniffing", 0); err != nil {
		t.Fatal(err)
	}
	off := newVerifyInbound(t, 41003, "嗅探关闭", true)
	if err := database.GetDB().Model(off).Updates(map[string]any{"protocol": "vmess", "sniffing": "{}"}).Error; err != nil {
		t.Fatalf("update inbound: %v", err)
	}
	rule := f.addRule(t, model.ActionProxy, []int{})
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, off.Id)
	if u.Verdict != verdictUnverifiable || u.Reason != reasonSniffingOff || len(u.Samples) != 0 {
		t.Fatalf("关 sniffing 的 vmess 入站应 unverifiable: %+v", u)
	}
	for _, c := range fr.calls {
		if strings.HasPrefix(c, off.Tag+"|") {
			t.Errorf("不应对关 sniffing 的入站发域名样例: %s", c)
		}
	}
	if a := findUser(t, res, f.a.Id); a.Verdict != verdictVerified {
		t.Errorf("开着 sniffing 的入站不受影响: %+v", a)
	}
}

// socks / http 代理入站不需要嗅探就拿得到域名（socks5h、CONNECT），照常验证。
func TestVerifyGlobalRuleSniffingOffSocksStillVerified(t *testing.T) {
	f := newVerifyFixture(t)
	socks := newVerifyInbound(t, 41003, "socks", true)
	if err := database.GetDB().Model(socks).Updates(map[string]any{
		"protocol": "socks", "sniffing": "{}", "settings": `{"auth":"noauth","udp":false}`,
	}).Error; err != nil {
		t.Fatalf("update inbound: %v", err)
	}
	rule := f.addRule(t, model.ActionProxy, []int{})
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if u := findUser(t, res, socks.Id); u.Verdict != verdictVerified || len(u.Samples) != 2 {
		t.Fatalf("关 sniffing 的 socks 入站应照常 verified: %+v", u)
	}
}

// 组里同时有域名与 IP 段：关 sniffing 的入站只剔掉域名样例，IP 样例照测并可变绿。
func TestVerifySniffingOffStillTestsIPSamples(t *testing.T) {
	f := newVerifyFixture(t)
	cidrs, err := EncodeCidrs([]string{"1.2.3.0/24"})
	if err != nil {
		t.Fatalf("EncodeCidrs: %v", err)
	}
	if err := database.GetDB().Model(f.group).Update("cidrs", cidrs).Error; err != nil {
		t.Fatalf("update group: %v", err)
	}
	off := newVerifyInbound(t, 41003, "嗅探关闭", true)
	if err := database.GetDB().Model(off).Updates(map[string]any{"protocol": "trojan", "sniffing": `{"enabled":true,"destOverride":["quic"]}`}).Error; err != nil {
		t.Fatalf("update inbound: %v", err)
	}
	rule := f.addRule(t, model.ActionProxy, []int{})
	fr := &fakeRoute{fn: returnTag(f.node.Tag)}
	res, err := newVerifyService(fr, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, off.Id)
	if u.Verdict != verdictVerified || len(u.Samples) != 1 || u.Samples[0].Target != "1.2.3.1" || u.Reason != reasonSniffingIPOnly {
		t.Fatalf("应只测 IP 样例并注明: %+v", u)
	}
	// 开着 sniffing 的入站：1 个域名 + 1 个 IP（IP 预留名额）。
	if a := findUser(t, res, f.a.Id); len(a.Samples) != 2 || a.Samples[0].Target != "chatgpt.com" || a.Samples[1].Target != "1.2.3.1" {
		t.Errorf("A 的样例 = %+v", a.Samples)
	}
}

// 访问日志里被接受却没记出站的行（Route 为空）：标签是「未知出站」而不是空白。
func TestVerifyLogEmptyRouteLabel(t *testing.T) {
	f := newVerifyFixture(t)
	rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
	setAccessLog(t, true)
	s := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime())
	s.groupLogs = func(int, int64, int) ([]TargetRouteCount, bool, error) {
		return []TargetRouteCount{{Target: "chatgpt.com:443", Route: "", Accepted: true, Count: 2, LastAt: 1}}, false, nil
	}
	res, err := s.Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	u := findUser(t, res, f.a.Id)
	if u.Log == nil || len(u.Log.Others) != 1 || u.Log.Others[0].Label != "未知出站" || u.Log.Others[0].Route != "" {
		t.Fatalf("空 Route 应标为未知出站: %+v", u.Log)
	}
}

// 被更靠前的规则抢先命中：结论仍是 mismatch（设计如此），但样例与日志分组标出抢先的规则，
// Reason 点名它；与抢先规则出站不同的走错不归因。
func TestVerifyAttributesShadowingRule(t *testing.T) {
	f := newVerifyFixture(t)
	setAccessLog(t, true)
	us := &model.OutboundNode{
		Tag: "a-ui-us", Remark: "美区家宽", Protocol: "socks", Enable: true,
		Config: `{"protocol":"socks","settings":{"servers":[{"address":"5.6.7.8","port":1080}]}}`,
	}
	if err := database.GetDB().Save(us).Error; err != nil {
		t.Fatalf("save node: %v", err)
	}
	openai := newTestGroupWithDomains(t, "OpenAI", []string{"full:o33249.ingest.us.sentry.io"})
	claude := newTestGroupWithDomains(t, "Claude", []string{"full:claude.ai", "domain:sentry.io"})
	special := &model.RoutingRule{
		Remark: "OpenAI(特殊)", InboundIds: mustEncodeIds(t, []int{f.a.Id}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{openai.Id}),
		Action:         model.ActionProxy, OutboundId: us.Id, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(special); err != nil {
		t.Fatalf("add special: %v", err)
	}
	normal := &model.RoutingRule{
		Remark: "Anthropic(常规)", InboundIds: mustEncodeIds(t, []int{}),
		DomainGroupIds: mustEncodeGroupIds(t, []int{claude.Id}),
		Action:         model.ActionProxy, OutboundId: f.node.Id, Enable: true,
	}
	if err := (&RoutingRuleService{}).Add(normal); err != nil {
		t.Fatalf("add normal: %v", err)
	}
	now := time.UnixMilli(1793520000000)
	rows := []model.AccessLog{
		{Time: now.Add(-time.Hour).UnixMilli(), InboundId: f.a.Id, Target: "o33249.ingest.us.sentry.io:443", Route: us.Tag, Accepted: true},
		{Time: now.Add(-time.Hour).UnixMilli(), InboundId: f.b.Id, Target: "o33249.ingest.us.sentry.io:443", Route: model.DefaultOutboundTag, Accepted: true},
	}
	if err := database.GetAccessLogDB().Create(&rows).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(normal.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	a := findUser(t, res, f.a.Id)
	if a.Verdict != verdictMismatch || a.Log == nil || len(a.Log.Others) != 1 {
		t.Fatalf("被抢先命中仍应为 mismatch: %+v log=%+v", a, a.Log)
	}
	if sb := a.Log.Others[0].ShadowedBy; sb == nil || sb.RuleId != special.Id || sb.Remark != "OpenAI(特殊)" {
		t.Errorf("用户A 的日志分组应归因到 OpenAI(特殊)，得到 %+v", sb)
	}
	if !strings.Contains(a.Reason, "OpenAI(特殊)") || !strings.Contains(a.Reason, "抢先命中") {
		t.Errorf("用户A 的 Reason 应点名抢先的规则，得到 %q", a.Reason)
	}

	// 用户B 不在 OpenAI(特殊) 的覆盖范围内、出站也不同：不归因，Reason 保持原样。
	b := findUser(t, res, f.b.Id)
	if b.Verdict != verdictMismatch || b.Log == nil || len(b.Log.Others) != 1 {
		t.Fatalf("用户B 应为 mismatch: %+v log=%+v", b, b.Log)
	}
	if sb := b.Log.Others[0].ShadowedBy; sb != nil {
		t.Errorf("用户B 不应归因，得到 %+v", sb)
	}
	if b.Reason != "访问日志里有连接走了其他出站" {
		t.Errorf("用户B 的 Reason 不应被改写，得到 %q", b.Reason)
	}
}

// templatePrivateRoute 模拟默认模板：目标 IP 落在 geoip:private 就先被那条
// geoip:private → blocked 截走，其余才轮到注入的规则（这里一律命中 tag）。
func templatePrivateRoute(tag string) func(context.Context, string, string, net.IP) (string, bool, error) {
	private := geodata.GetPrivateIPMatcher()
	return func(_ context.Context, _ string, _ string, ip net.IP) (string, bool, error) {
		if ip != nil && private.Match(ip) {
			return "blocked", false, nil
		}
		return tag, false, nil
	}
}

// 规则写的是 0.0.0.0/0（或 ::/0）：它对公网目标完全生效，改动前却拿 0.0.0.1 去测，
// 被模板的私网屏蔽截走后判成「不符」——一条工作正常的规则在列表里亮红灯。
// 整段都是私网（10.0.0.0/8）时同样不能报「不符」：那不是本规则配错，是模板的
// 安全规则按设计排在前面，样例根本轮不到本规则，应是「无法验证」并说清原因。
func TestVerifyIPSamplesAvoidTemplatePrivateBlock(t *testing.T) {
	cases := []struct {
		name, cidr string
		verdict    string
	}{
		{"整网 IPv4", "0.0.0.0/0", verdictVerified},
		{"整网 IPv6", "::/0", verdictVerified},
		{"整段私网", "10.0.0.0/8", verdictUnverifiable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newVerifyFixture(t)
			cidrs, err := EncodeCidrs([]string{c.cidr})
			if err != nil {
				t.Fatalf("EncodeCidrs: %v", err)
			}
			// 只留推不出样例的关键词：这条规则的结论完全取决于 IP 样例。
			if err := database.GetDB().Model(f.group).Updates(map[string]any{
				"domains": `["keyword:gpt"]`, "cidrs": cidrs,
			}).Error; err != nil {
				t.Fatalf("update group: %v", err)
			}
			rule := f.addRule(t, model.ActionProxy, []int{f.a.Id})
			res, err := newVerifyService(&fakeRoute{fn: templatePrivateRoute(f.node.Tag)}, okRuntime()).Verify(rule.Id)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			u := findUser(t, res, f.a.Id)
			if u.Verdict != c.verdict {
				t.Fatalf("%s：结论 = %s（%s），期望 %s；样例 %+v", c.cidr, u.Verdict, u.Reason, c.verdict, u.Samples)
			}
			if c.verdict == verdictUnverifiable && !strings.Contains(u.Reason, "geoip:private") {
				t.Errorf("整段私网时原因要点明 geoip:private，实际 %q", u.Reason)
			}
		})
	}
}

// 「补全流量识别」开着（默认）：同一个关了 sniffing 的 vmess 入站在核心里拿得到域名，域名样例照常算证据。
func TestVerifyGlobalRuleForcedSniffingInboundUsesDomainSamples(t *testing.T) {
	f := newVerifyFixture(t)
	if err := (&SettingService{}).setInt("forceSniffing", 1); err != nil {
		t.Fatal(err)
	}
	off := newVerifyInbound(t, 41004, "嗅探关闭但被补全", true)
	if err := database.GetDB().Model(off).Updates(map[string]any{"protocol": "vmess", "sniffing": "{}"}).Error; err != nil {
		t.Fatalf("update inbound: %v", err)
	}
	rule := f.addRule(t, model.ActionProxy, []int{})
	res, err := newVerifyService(&fakeRoute{fn: returnTag(f.node.Tag)}, okRuntime()).Verify(rule.Id)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if u := findUser(t, res, off.Id); u.Verdict != verdictVerified || len(u.Samples) == 0 {
		t.Fatalf("补全流量识别开着时应按域名样例验证: %+v", u)
	}
}
