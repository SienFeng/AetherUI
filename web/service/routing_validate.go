package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/xray"
)

// runXrayTest 把一份配置交给真实的 xray 做语法与语义校验。
//
// 采用 fail open：只有 xray 明确判定配置非法时才返回错误。二进制缺失、
// 命令行参数不被老版本识别、超时等情况一律返回 nil 并记日志——校验器
// 自身的故障绝不能变成用户无法保存配置的门禁。
func runXrayTest(cfg map[string]any) error {
	binaryPath := xray.GetBinaryPath()
	if _, err := os.Stat(binaryPath); err != nil {
		logger.Debug("skip config validation, xray binary not found:", err)
		return nil
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "a-ui-validate-*.json")
	if err != nil {
		logger.Warning("skip config validation, cannot create temp file:", err)
		return nil
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		logger.Warning("skip config validation, cannot write temp file:", err)
		return nil
	}
	file.Close()

	cmd := exec.Command(binaryPath, "run", "-test", "-c", file.Name())
	done := make(chan struct{})
	var output []byte
	var runErr error
	go func() {
		output, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		logger.Warning("skip config validation, xray -test timed out")
		return nil
	}

	text := string(output)
	if runErr == nil || strings.Contains(text, "Configuration OK") {
		return nil
	}
	// 老版本 xray 可能不认 "run -test" 这套参数，此时不是配置的问题。
	if strings.Contains(text, "unknown command") || strings.Contains(text, "flag provided but not defined") {
		logger.Warning("skip config validation, xray does not support 'run -test':", firstLine(text))
		return nil
	}
	return common.NewError("xray 校验未通过:", lastMeaningfulLine(text))
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return s
}

// lastMeaningfulLine 取最后一行非空、非版权横幅的输出，那里才是真正的报错。
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" ||
			strings.HasPrefix(line, "Xray ") ||
			strings.HasPrefix(line, "A unified platform") {
			continue
		}
		return line
	}
	return firstLine(s)
}

// ValidateOutbound 用一份最小配置把待验证的 outbound 包起来送去校验。
// 在落库之前调用，因此不需要事务回滚。
func ValidateOutbound(ob map[string]any) error {
	return runXrayTest(map[string]any{
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{}},
			ob,
		},
	})
}

// ValidateDomains 校验域名列表，能抓出不存在的 geosite 类别与非法正则。
func ValidateDomains(domains []string) error {
	return runXrayTest(map[string]any{
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
		},
		"routing": map[string]any{
			"rules": []any{
				map[string]any{"type": "field", "domain": domains, "outboundTag": "direct"},
			},
		},
	})
}
