package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
	if err := ValidateDomains([]string{"domain:openai.com", "geosite:openai", "full:chat.openai.com"}); err != nil {
		t.Errorf("valid domains were rejected: %v", err)
	}
}

func TestValidateDomainsRejectsUnknownGeositeCategory(t *testing.T) {
	requireXrayBinary(t)
	if err := ValidateDomains([]string{"geosite:definitely-not-a-category"}); err == nil {
		t.Error("expected an unknown geosite category to be rejected")
	}
}

func TestValidateDomainsRejectsBadRegexp(t *testing.T) {
	requireXrayBinary(t)
	if err := ValidateDomains([]string{`regexp:([a-z`}); err == nil {
		t.Error("expected a malformed regexp to be rejected")
	}
}
