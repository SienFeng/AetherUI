package service

import (
	"testing"

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
