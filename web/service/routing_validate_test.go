package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"a-ui/xray"
)

// xray.GetBinaryPath() 等函数返回的是相对路径（"bin/xray-..."），约定调用方的
// 工作目录是仓库根目录——面板进程实际就是这样启动的。而 `go test` 默认把
// 工作目录设为被测包所在目录（web/service），不是仓库根目录，会导致这里的
// 二进制探测和下面 runXrayTest 里的探测都定位不到 bin/ 下已经存在的二进制，
// 从而让本该真正执行的校验测试全部误判为「本机无二进制」而跳过。
// 这里把工作目录切到仓库根目录，让测试环境与生产运行时的假设保持一致。
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
