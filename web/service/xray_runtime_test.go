package service

import (
	"strconv"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/xray"
)

// 回写包级 p，避免污染同包其他测试。
func withProcess(t *testing.T, proc *xray.Process) {
	t.Helper()
	old := p
	p = proc
	t.Cleanup(func() { p = old })
}

func TestRuntimeSnapshotNotRunning(t *testing.T) {
	withProcess(t, nil)
	got := (&XrayService{}).RuntimeSnapshot()
	if got.Running || got.Busy {
		t.Fatalf("未运行时应为 Running=false Busy=false，得到 %+v", got)
	}
}

func TestRuntimeSnapshotBusy(t *testing.T) {
	withProcess(t, nil)
	lock.Lock()
	t.Cleanup(lock.Unlock)
	got := (&XrayService{}).RuntimeSnapshot()
	if !got.Busy {
		t.Fatalf("lock 被占用时应返回 Busy=true，得到 %+v", got)
	}
}

func TestDefaultOutboundTag(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"首个出站的 tag", `[{"protocol":"freedom","tag":"a-ui-default"},{"tag":"x"}]`, "a-ui-default"},
		{"空数组", `[]`, ""},
		{"首位无 tag", `[{"protocol":"freedom"},{"tag":"x"}]`, ""},
		{"非法 JSON", `{`, ""},
	}
	for _, c := range cases {
		cfg := &xray.Config{OutboundConfigs: []byte(c.raw)}
		if got := defaultOutboundTag(cfg); got != c.want {
			t.Errorf("%s: 期望 %q，得到 %q", c.name, c.want, got)
		}
	}
	if got := defaultOutboundTag(nil); got != "" {
		t.Errorf("nil 配置应返回空串，得到 %q", got)
	}
}

// 用真实 xray 覆盖运行中路径：Running/APIPort/DefaultTag，以及 Pending 的真假两面。
func TestRuntimeSnapshotAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	requireXrayAPIPortFree(t)
	setupDB(t)

	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		if p != nil {
			_ = p.Stop()
			p = nil
		}
		result = ""
	})

	socksPort := freePort(t)
	in := &model.Inbound{
		UserId: 1, Port: socksPort, Protocol: model.Protocol("socks"), Enable: true,
		Tag:            "inbound-" + strconv.Itoa(socksPort),
		Listen:         "127.0.0.1",
		Settings:       `{"auth":"noauth","udp":false}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Sniffing:       "{}",
	}
	if err := database.GetDB().Save(in).Error; err != nil {
		t.Fatalf("save inbound: %v", err)
	}
	encoded, err := EncodeDomains([]string{"full:localhost"})
	if err != nil {
		t.Fatalf("EncodeDomains: %v", err)
	}
	group := &model.DomainGroup{Remark: "runtime-snapshot", Domains: encoded}
	if err := (&DomainGroupService{}).Add(group); err != nil {
		t.Fatalf("add domain group: %v", err)
	}
	// 规则先禁用，启动后再启用，制造“库里的配置领先于核心”的状态。
	rule := &model.RoutingRule{
		Remark: "runtime-snapshot", InboundIds: mustEncodeIds(t, []int{in.Id}),
		DomainGroupId: group.Id, DomainGroupIds: mustEncodeGroupIds(t, []int{group.Id}),
		Action: model.ActionBlock, Enable: false,
	}
	if err := (&RoutingRuleService{}).Add(rule); err != nil {
		t.Fatalf("add rule: %v", err)
	}

	xs := &XrayService{}
	if err := xs.RestartXray(true); err != nil {
		t.Fatalf("RestartXray(true): %v", err)
	}
	waitForPort(t, socksPort)

	lock.Lock()
	wantTag := defaultOutboundTag(p.GetConfig())
	lock.Unlock()
	if wantTag == "" {
		t.Fatal("运行中配置的默认出站 tag 为空，测试前提不成立")
	}

	got := xs.RuntimeSnapshot()
	if !got.Running || got.Busy || got.APIPort <= 0 {
		t.Fatalf("运行中快照异常: %+v", got)
	}
	if got.DefaultTag != wantTag {
		t.Errorf("DefaultTag = %q，期望 %q", got.DefaultTag, wantTag)
	}
	if got.Pending {
		t.Errorf("刚启动时 Pending 应为 false: %+v", got)
	}

	if err := database.GetDB().Model(&model.RoutingRule{}).Where("id = ?", rule.Id).
		Update("enable", true).Error; err != nil {
		t.Fatalf("enable rule: %v", err)
	}
	got = xs.RuntimeSnapshot()
	if !got.Running || !got.Pending {
		t.Errorf("启用规则后（未重启）应 Running 且 Pending: %+v", got)
	}
	if got.DefaultTag != wantTag {
		t.Errorf("Pending 状态下 DefaultTag 应仍取运行中配置: %q", got.DefaultTag)
	}
}
