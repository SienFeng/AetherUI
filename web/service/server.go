package service

import (
	"archive/zip"
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"
	"io"
	"io/fs"
	"net/http"
	"os"
	"runtime"
	"time"
	"a-ui/logger"
	"a-ui/util/sys"
	"a-ui/xray"
)

type ProcessState string

const (
	Running ProcessState = "running"
	Stop    ProcessState = "stop"
	Error   ProcessState = "error"
)

type Status struct {
	T   time.Time `json:"-"`
	Cpu float64   `json:"cpu"`
	Mem struct {
		Current uint64 `json:"current"`
		Total   uint64 `json:"total"`
	} `json:"mem"`
	Swap struct {
		Current uint64 `json:"current"`
		Total   uint64 `json:"total"`
	} `json:"swap"`
	Disk struct {
		Current uint64 `json:"current"`
		Total   uint64 `json:"total"`
	} `json:"disk"`
	Xray struct {
		State    ProcessState `json:"state"`
		ErrorMsg string       `json:"errorMsg"`
		Version  string       `json:"version"`
	} `json:"xray"`
	Uptime   uint64    `json:"uptime"`
	Loads    []float64 `json:"loads"`
	TcpCount int       `json:"tcpCount"`
	UdpCount int       `json:"udpCount"`
	NetIO    struct {
		Up   uint64 `json:"up"`
		Down uint64 `json:"down"`
	} `json:"netIO"`
	NetTraffic struct {
		Sent uint64 `json:"sent"`
		Recv uint64 `json:"recv"`
	} `json:"netTraffic"`
}

type Release struct {
	TagName string `json:"tag_name"`
}

type ServerService struct {
	xrayService XrayService
}

func (s *ServerService) GetStatus(lastStatus *Status) *Status {
	now := time.Now()
	status := &Status{
		T: now,
	}

	percents, err := cpu.Percent(0, false)
	if err != nil {
		logger.Warning("get cpu percent failed:", err)
	} else {
		status.Cpu = percents[0]
	}

	upTime, err := host.Uptime()
	if err != nil {
		logger.Warning("get uptime failed:", err)
	} else {
		status.Uptime = upTime
	}

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		logger.Warning("get virtual memory failed:", err)
	} else {
		status.Mem.Current = memInfo.Used
		status.Mem.Total = memInfo.Total
	}

	swapInfo, err := mem.SwapMemory()
	if err != nil {
		logger.Warning("get swap memory failed:", err)
	} else {
		status.Swap.Current = swapInfo.Used
		status.Swap.Total = swapInfo.Total
	}

	distInfo, err := disk.Usage("/")
	if err != nil {
		logger.Warning("get dist usage failed:", err)
	} else {
		status.Disk.Current = distInfo.Used
		status.Disk.Total = distInfo.Total
	}

	avgState, err := load.Avg()
	if err != nil {
		logger.Warning("get load avg failed:", err)
	} else {
		status.Loads = []float64{avgState.Load1, avgState.Load5, avgState.Load15}
	}

	ioStats, err := net.IOCounters(false)
	if err != nil {
		logger.Warning("get io counters failed:", err)
	} else if len(ioStats) > 0 {
		ioStat := ioStats[0]
		status.NetTraffic.Sent = ioStat.BytesSent
		status.NetTraffic.Recv = ioStat.BytesRecv

		if lastStatus != nil {
			duration := now.Sub(lastStatus.T)
			seconds := float64(duration) / float64(time.Second)
			up := uint64(float64(status.NetTraffic.Sent-lastStatus.NetTraffic.Sent) / seconds)
			down := uint64(float64(status.NetTraffic.Recv-lastStatus.NetTraffic.Recv) / seconds)
			status.NetIO.Up = up
			status.NetIO.Down = down
		}
	} else {
		logger.Warning("can not find io counters")
	}

	status.TcpCount, err = sys.GetTCPCount()
	if err != nil {
		logger.Warning("get tcp connections failed:", err)
	}

	status.UdpCount, err = sys.GetUDPCount()
	if err != nil {
		logger.Warning("get udp connections failed:", err)
	}

	if s.xrayService.IsXrayRunning() {
		status.Xray.State = Running
		status.Xray.ErrorMsg = ""
	} else {
		err := s.xrayService.GetXrayErr()
		if err != nil {
			status.Xray.State = Error
		} else {
			status.Xray.State = Stop
		}
		status.Xray.ErrorMsg = s.xrayService.GetXrayResult()
	}
	status.Xray.Version = s.xrayService.GetXrayVersion()

	return status
}

func (s *ServerService) GetXrayVersions() ([]string, error) {
	url := "https://api.github.com/repos/XTLS/Xray-core/releases"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	buffer := bytes.NewBuffer(make([]byte, 8192))
	buffer.Reset()
	_, err = buffer.ReadFrom(resp.Body)
	if err != nil {
		return nil, err
	}

	releases := make([]Release, 0)
	err = json.Unmarshal(buffer.Bytes(), &releases)
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(releases))
	for _, release := range releases {
		versions = append(versions, release.TagName)
	}
	return versions, nil
}

func (s *ServerService) downloadXRay(version string) (string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch osName {
	case "darwin":
		osName = "macos"
	}

	switch arch {
	case "amd64":
		arch = "64"
	case "arm64":
		arch = "arm64-v8a"
	}

	fileName := fmt.Sprintf("Xray-%s-%s.zip", osName, arch)
	url := fmt.Sprintf("https://github.com/XTLS/Xray-core/releases/download/%s/%s", version, fileName)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	os.Remove(fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return "", err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return "", err
	}

	return fileName, nil
}

// openXrayZip 打开并验证一个 xray 发布 zip，返回 reader 与清理函数。
//
// 与解包分成两步，是为了让调用方能在「包已确认可读」和「开始写文件」之间
// 插入自己的动作——UpdateXray 正是在这个缝隙里停核心的：下载几十 MB 和
// 包损坏检测都发生在停机之前，用户完全不断流。
//
// 清理函数只关闭文件句柄，不删除 zip：zip 是谁下载的谁负责删。
func openXrayZip(zipPath string) (*zip.Reader, func(), error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	r, err := zip.NewReader(f, stat.Size())
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return r, func() { f.Close() }, nil
}

// extractXrayFiles 把 zip 里的 xray / geosite.dat / geoip.dat 解到三个显式
// 给出的路径。
//
// 目标路径是参数而不是直接取 xray.GetBinaryPath()：那些是相对路径，而本包
// 的测试会 chdir 到仓库根，写死就等于让测试覆盖仓库里真实的 xray 二进制。
//
// 条目不存在时在删除目标文件之前就返回，所以缺条目不会破坏已有的核心。
func extractXrayFiles(r *zip.Reader, binPath, geositePath, geoipPath string) error {
	copyZipFile := func(zipName string, fileName string) error {
		zipFile, err := r.Open(zipName)
		if err != nil {
			return err
		}
		defer zipFile.Close()
		os.Remove(fileName)
		file, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR|os.O_TRUNC, fs.ModePerm)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(file, zipFile)
		return err
	}

	if err := copyZipFile("xray", binPath); err != nil {
		return err
	}
	if err := copyZipFile("geosite.dat", geositePath); err != nil {
		return err
	}
	return copyZipFile("geoip.dat", geoipPath)
}

// UpdateXray 是面板「切换版本」按钮的入口：下载 → 验证 → 停核心 → 解包 → 重启。
//
// StopXray 必须排在 openXrayZip 之后：下载几十 MB 与包损坏检测都不该让用户
// 白断一次流。
func (s *ServerService) UpdateXray(version string) error {
	zipFileName, err := s.downloadXRay(version)
	if err != nil {
		return err
	}
	defer os.Remove(zipFileName)

	r, closeZip, err := openXrayZip(zipFileName)
	if err != nil {
		return err
	}
	defer closeZip()

	s.xrayService.StopXray()
	defer func() {
		err := s.xrayService.RestartXray(true)
		if err != nil {
			logger.Error("start xray failed:", err)
		}
	}()

	return extractXrayFiles(r, xray.GetBinaryPath(), xray.GetGeositePath(), xray.GetGeoipPath())
}

// GetNewX25519Cert 生成一对 REALITY 用的 X25519 密钥。
//
// 复刻 xray-core 的 main/commands/all/curve25519.go:38-58。两个易错点：
//   1. 私钥必须按 https://cr.yp.to/ecdh.html 做 clamping，漏掉会生成出
//      核心不接受的私钥；
//   2. 编码必须是 base64.RawURLEncoding，与核心 REALITYConfig.Build
//      （infra/conf/transport_security.go:100）的解码方式一致。用 StdEncoding
//      长度照样是 32 字节，但核心会拒绝整份配置。
//
// 不 exec `bin/xray x25519`：bin/xray-darwin-arm64 在 .gitignore 中，本地开发
// 环境没有该文件。密钥生成与配置校验不同，不能 fail open——生成不出来就是
// 生成不出来，不能放行一个空密钥。
func (s *ServerService) GetNewX25519Cert() (map[string]any, error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, err
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"privateKey": base64.RawURLEncoding.EncodeToString(priv),
		"publicKey":  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
	}, nil
}

// GetNewMldsa65 生成 REALITY 的 ML-DSA-65 后量子签名密钥对。
// 复刻 main/commands/all/mldsa65.go:30-46。seed 落进入站的 realitySettings.mldsa65Seed，
// verify 落进分享链接的 pqv 参数。
func (s *ServerService) GetNewMldsa65() (map[string]any, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	pub, _ := mldsa65.NewKeyFromSeed(&seed)
	return map[string]any{
		"seed":   base64.RawURLEncoding.EncodeToString(seed[:]),
		"verify": base64.RawURLEncoding.EncodeToString(pub.Bytes()),
	}, nil
}
