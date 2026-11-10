package service

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"a-ui/util/common"
	"a-ui/xray"
)

// 「切换版本」之前，先用新核心校验当前会下发的配置。
//
// 修复前 UpdateXray 是「下载 → 停核心 → 换文件 → 重启」，从不问新核心认不认当前配置。xray 的大版本
// 会删字段（26.x 删掉了 mKCP 的 header/seed，旧配置直接 Failed to start），换上去就是整机断网，而
// Process.Start 从不回传启动失败，面板首页照样显示 running。现在先把 zip 里的 xray 解到 bin/ 下的临时
// 文件，让它 run -test 一份当前生成的配置，过了才动正在跑的核心。
//
// 与保存时的 run -test（routing_validate.go，fail open）取向相反，这里 fail closed：起不来（架构不对、
// 包损坏）、不认 run -test、超时、没打出 Configuration OK，一律拒绝切换。那边放行的是「证明不了非法」的
// 配置，最坏是核心拒绝启动；这边放行的是一次换核心，放错了就是整机断网，拒绝的代价只是这次没换成。

// downloadXrayFn 是 UpdateXray 的下载实现，测试可替换成本地 zip。
var downloadXrayFn = func(s *ServerService, version string) (string, error) {
	return s.downloadXRay(version)
}

// checkCandidateCore 用 zip 里的新核心校验当前生成的配置（计量出站摘掉，理由同 validateWithFullConfig）。
func (s *ServerService) checkCandidateCore(r *zip.Reader) error {
	return checkCandidateCoreIn(r, filepath.Dir(xray.GetBinaryPath()), func() (map[string]any, error) {
		cfg, err := s.xrayService.GetXrayConfig()
		if err != nil {
			return nil, err
		}
		return configAsMap(cfg)
	})
}

// checkCandidateCoreIn 是 checkCandidateCore 的实现。binDir 是候选核心的落脚目录：必须与 geo 数据同目录，
// xray 从可执行文件所在目录找 geoip.dat / geosite.dat / a-ui-geo.dat，放到别处会因为找不到数据而误判。
func checkCandidateCoreIn(r *zip.Reader, binDir string, configFn func() (map[string]any, error)) error {
	candidate, err := stageCandidateCore(r, binDir)
	if err != nil {
		return common.NewError("解出新版本核心失败，已取消切换:", err)
	}
	defer os.Remove(candidate)

	cfg, err := configFn()
	if err != nil {
		return common.NewError("生成当前配置失败，无法确认新版本核心能否加载它，已取消切换:", err)
	}
	stripMeterOutbounds(cfg)
	return runCandidateTest(candidate, cfg)
}

// configAsMap 把配置转成 map，供 stripMeterOutbounds 处理。
func configAsMap(cfg *xray.Config) (map[string]any, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return decodeConfig(data)
}

// stageCandidateCore 把 zip 里的 xray 解成 binDir 下一个只有属主可执行的临时文件，返回其路径。
func stageCandidateCore(r *zip.Reader, binDir string) (path string, err error) {
	src, err := r.Open("xray")
	if err != nil {
		return "", err
	}
	defer src.Close()
	f, err := os.CreateTemp(binDir, ".xray-candidate-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = io.Copy(f, src); err != nil {
		return "", err
	}
	if err = f.Chmod(0o700); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// runCandidateTest 让候选核心 run -test 一份配置；只有打出 Configuration OK 才算通过。
func runCandidateTest(binary string, cfg map[string]any) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	// 配置里有全部用户的凭据：CreateTemp 建出来就是 0600，用完即删。
	file, err := os.CreateTemp("", "a-ui-candidate-*.json")
	if err != nil {
		return common.NewError("无法写出待校验的配置，已取消切换:", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return common.NewError("无法写出待校验的配置，已取消切换:", err)
	}
	if err := file.Close(); err != nil {
		return common.NewError("无法写出待校验的配置，已取消切换:", err)
	}

	cmd := exec.Command(binary, "run", "-test", "-c", file.Name())
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return common.NewError("新版本核心在本机无法运行（架构不对或安装包损坏），已取消切换:", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(xrayTestTimeout):
		_ = cmd.Process.Kill()
		<-done
		return common.NewErrorf("新版本核心校验当前配置超时（%v），无法确认它能加载，已取消切换", xrayTestTimeout)
	}
	text := output.String()
	if strings.Contains(text, "Configuration OK") {
		return nil
	}
	if strings.Contains(text, "unknown command") || strings.Contains(text, "flag provided but not defined") {
		return common.NewError("新版本核心不支持 run -test，无法确认它能加载当前配置，已取消切换:", firstLine(text))
	}
	return common.NewError("新版本核心不接受当前配置，已取消切换，当前核心保持不变:", lastMeaningfulLine(text))
}
