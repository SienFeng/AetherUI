package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"a-ui/database/model"
	"a-ui/xray"
)

// TestMain 把工作目录切到仓库根，因为 xray.GetBinaryPath() 返回的是相对路径
// bin/xray-<GOOS>-<GOARCH>，而 go test 的默认工作目录是包目录，解析不到该文件，
// 会让所有需要真实 xray 的测试静默 SKIP。切到仓库根也与生产环境一致——
// systemd 单元里 WorkingDirectory=/usr/local/a-ui/，面板运行时 cwd 即安装根目录。
//
// 注意这是进程级副作用，影响本包所有测试：若今后在 web/service 下新增依赖
// 包内相对路径（如 testdata/）的测试，请改用 t.TempDir() 或绝对路径，
// 否则路径会相对仓库根解析。
func TestMain(m *testing.M) {
	_, thisFile, _, ok := runtime.Caller(0)
	if ok {
		repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
		if err := os.Chdir(repoRoot); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

// 校验依赖真实的 xray 二进制，没有就跳过——CI 里可能没有。
func requireXrayBinary(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(xray.GetBinaryPath()); err != nil {
		t.Skipf("xray binary not available at %s", xray.GetBinaryPath())
	}
}

func TestValidateOutboundAcceptsValidSocks(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	ob := map[string]any{
		"tag":      "a-ui-probe",
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "1.2.3.4", "port": 1080}},
		},
	}
	if err := ValidateOutbound(ob); err != nil {
		t.Errorf("valid socks outbound was rejected: %v", err)
	}
}

func TestValidateOutboundRejectsUnknownProtocol(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	ob := map[string]any{
		"tag":      "a-ui-probe",
		"protocol": "definitely-not-a-protocol",
		"settings": map[string]any{},
	}
	if err := ValidateOutbound(ob); err == nil {
		t.Error("expected an unknown protocol to be rejected")
	}
}

func TestValidateDomainsAcceptsNativeSyntax(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	if err := ValidateDomains([]string{"domain:openai.com", "geosite:openai", "full:chat.openai.com"}); err != nil {
		t.Errorf("valid domains were rejected: %v", err)
	}
}

func TestValidateDomainsRejectsUnknownGeositeCategory(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	if err := ValidateDomains([]string{"geosite:definitely-not-a-category"}); err == nil {
		t.Error("expected an unknown geosite category to be rejected")
	}
}

func TestValidateDomainsRejectsBadRegexp(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	if err := ValidateDomains([]string{`regexp:([a-z`}); err == nil {
		t.Error("expected a malformed regexp to be rejected")
	}
}

// C1 之所以能通过全部审查，根因就在这里：只包住单个 outbound 的最小配置，
// 在原理上就发现不了「与注入器发出的 tag 重复」这类组合层面的冲突。
// 设计 §5.4.2 要求校验的是**完整生成配置**。
func TestValidateOutboundRejectsTagCollidingWithInjectedBlackhole(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	ob := map[string]any{
		"tag":      model.BlockOutboundTag,
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "1.2.3.4", "port": 1080}},
		},
	}
	if err := ValidateOutbound(ob); err == nil {
		t.Errorf("a tag colliding with the always-injected blackhole %q must be rejected",
			model.BlockOutboundTag)
	}
}

// fail open 的边界：改动之前配置就已经不合法，说明问题不是本次改动引入的。
// 若因此拒绝保存，管理员连修复用的操作都做不了，会被自己的面板锁在门外。
func TestValidateOutboundAllowsWhenBaselineTemplateIsAlreadyInvalid(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	broken := `{"outbounds":[{"protocol":"definitely-not-a-protocol","settings":{}}]}`
	if err := (&SettingService{}).setString("xrayTemplateConfig", broken); err != nil {
		t.Fatalf("setString: %v", err)
	}
	ob := map[string]any{
		"tag":      "a-ui-probe",
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "1.2.3.4", "port": 1080}},
		},
	}
	if err := ValidateOutbound(ob); err != nil {
		t.Errorf("a pre-existing template error must not block an unrelated save: %v", err)
	}
}

// 历史脏数据：库里可能还留着一个 tag 为 a-ui-block 的节点（C1 修复前分配的）。
// 生成端已经会跳过它，所以它在完整配置里根本不存在。校验时若把注入器的黑洞
// 出站当成「它的旧版本」摘掉，这次编辑就会「校验通过」，管理员以为改好了，
// 实际那个节点永远不生效。必须照旧判定为撞名。
func TestValidateOutboundReplacingNeverRemovesTheInjectedBlackhole(t *testing.T) {
	requireXrayBinary(t)
	setupDB(t)
	ob := map[string]any{
		"tag":      model.BlockOutboundTag,
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{map[string]any{"address": "1.2.3.4", "port": 1080}},
		},
	}
	if err := ValidateOutboundReplacing(ob, model.BlockOutboundTag); err == nil {
		t.Errorf("editing a node that carries the reserved tag %q must still be rejected",
			model.BlockOutboundTag)
	}
}

// 空列表直接放行，绝不构造探针规则：ip 为空数组时那条探针只剩 outboundTag
// 一个非条件字段，xray 会报 "this rule has no effective fields"
// （app/router/config.go:114）而整份配置被判非法——一个「这个组没有 IP 段」
// 的正常状态会变成保存失败。
func TestValidateCidrsAllowsEmpty(t *testing.T) {
	if err := ValidateCidrs(nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := ValidateCidrs([]string{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateCidrsAcceptsValidList(t *testing.T) {
	setupDB(t)
	if err := ValidateCidrs([]string{"1.2.3.0/24", "geoip:private"}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
