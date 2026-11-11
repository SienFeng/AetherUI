package xray

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Workiva/go-datastructures/queue"
	statsservice "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"a-ui/util/common"
)

var trafficRegex = regexp.MustCompile("(inbound|outbound)>>>([^>]+)>>>traffic>>>(downlink|uplink)")

func GetBinaryName() string {
	return fmt.Sprintf("xray-%s-%s", runtime.GOOS, runtime.GOARCH)
}

func GetBinaryPath() string {
	return "bin/" + GetBinaryName()
}

func GetConfigPath() string {
	return "bin/config.json"
}

func GetGeositePath() string {
	return "bin/geosite.dat"
}

func GetGeoipPath() string {
	return "bin/geoip.dat"
}

// stopProcess 是 Process 的 finalizer：只发 SIGKILL、不等退出。finalizer 跑在 GC 的那个
// goroutine 里，在这里阻塞（Stop 会有界地等旧进程退出）会卡住整个进程的 finalizer 队列。
func stopProcess(p *Process) {
	p.kill()
}

type Process struct {
	*process
}

func NewProcess(xrayConfig *Config) *Process {
	p := &Process{newProcess(xrayConfig)}
	runtime.SetFinalizer(p, stopProcess)
	return p
}

type process struct {
	// mu 保护下面这些字段。面板首页每 2 秒查一次状态、流量任务每 10 秒取一次端口、
	// CheckXrayRunningJob 每 30 秒查一次是否在跑，它们都不拿 XrayService 的 lock；而子进程的
	// 启动与退出发生在 Start 起的 goroutine 里。修复前 IsRunning / Stop 直接读 cmd.Process 与
	// cmd.ProcessState——那两个字段由后台的 cmd.Run 写入，竞态检测器在每一个带真实 xray 的
	// 测试里都抓得到。现在 exec.Cmd 只在那个 goroutine 里碰，别处一律读这里记下的状态。
	mu       sync.Mutex
	starting bool        // Start 已经占位、后台 goroutine 还没把子进程起来（或报出失败）
	proc     *os.Process // 子进程启动成功后才有
	exited   bool        // 子进程已经退出，或者根本没能启动
	exitErr error
	// done 在后台 goroutine 确认子进程退出（或根本没能启动）时关闭，Stop 靠它等旧进程真的退出。
	done chan struct{}
	version string
	apiPort int
	config  *Config

	lines *queue.Queue // 自带锁
}

func newProcess(config *Config) *process {
	return &process{
		version: "Unknown",
		config:  config,
		lines:   queue.New(100),
	}
}

func (p *process) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.proc != nil && !p.exited
}

func (p *process) GetErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

func (p *process) setExitErr(err error) {
	p.mu.Lock()
	p.exitErr = err
	p.mu.Unlock()
}

func (p *process) GetResult() string {
	if exitErr := p.GetErr(); p.lines.Empty() && exitErr != nil {
		return exitErr.Error()
	}
	items, _ := p.lines.TakeUntil(func(item interface{}) bool {
		return true
	})
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, item.(string))
	}
	return strings.Join(lines, "\n")
}

func (p *process) GetVersion() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.version
}

func (p *Process) GetAPIPort() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apiPort
}

func (p *Process) GetConfig() *Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config
}

// SetConfig 更新进程内的配置快照。
//
// 热应用成功后必须调它：RestartXray 是靠 GetConfig().Equals(新配置) 判断
// "配置有没有变"的，不同步的话下一轮 cron 仍认为两者不同，于是真的去重启
// 一次——热更新反而退化成延迟一轮的重启。
func (p *Process) SetConfig(c *Config) {
	p.mu.Lock()
	p.config = c
	p.mu.Unlock()
}

// APIInboundTag 是模板里预置的控制面入站（dokodemo-door，127.0.0.1）的 tag，
// 面板靠它找 gRPC 端口；它产生的访问记录是面板自己的轮询，不是用户流量。
const APIInboundTag = "api"

func (p *process) refreshAPIPort() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, inbound := range p.config.InboundConfigs {
		if inbound.Tag == "api" {
			p.apiPort = inbound.Port
			break
		}
	}
}

func (p *process) refreshVersion() {
	version := "Unknown"
	cmd := exec.Command(GetBinaryPath(), "-version")
	if data, err := cmd.Output(); err == nil {
		if datas := bytes.Split(data, []byte(" ")); len(datas) > 1 {
			version = string(datas[1])
		}
	}
	p.mu.Lock()
	p.version = version
	p.mu.Unlock()
}

func (p *process) Start() (err error) {
	// 同步占位：子进程要等后台 goroutine 起来，「是否在跑」在那之前一直为假——只靠 IsRunning
	// 判断的话，同时到达的两次 Start 都会通过，起两个 xray。
	p.mu.Lock()
	if p.starting || (p.proc != nil && !p.exited) {
		p.mu.Unlock()
		return errors.New("xray is already running")
	}
	p.starting = true
	p.proc, p.exited = nil, false
	done := make(chan struct{})
	p.done = done
	p.mu.Unlock()

	defer func() {
		if err != nil {
			p.mu.Lock()
			p.starting = false
			p.exitErr = err
			p.mu.Unlock()
			close(done)
		}
	}()

	p.mu.Lock()
	config := p.config
	p.mu.Unlock()
	if err = WriteConfigFile(config); err != nil {
		return err
	}
	configPath := GetConfigPath()

	cmd := exec.Command(GetBinaryPath(), "-c", configPath)
	// 访问日志的时间是不带时区的钟面时间：按本地时间写的话，夏令时结束那一小时会重复，
	// 面板按 UTC 解析（AccessLogService.Collect）才没有歧义。重复的键以最后一个为准。
	cmd.Env = append(os.Environ(), "TZ=UTC")

	stdReader, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errReader, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	go func() {
		defer func() {
			common.Recover("")
			stdReader.Close()
		}()
		reader := bufio.NewReaderSize(stdReader, 8192)
		for {
			line, _, err := reader.ReadLine()
			if err != nil {
				return
			}
			if p.lines.Len() >= 100 {
				p.lines.Get(1)
			}
			p.lines.Put(string(line))
		}
	}()

	go func() {
		defer func() {
			common.Recover("")
			errReader.Close()
		}()
		reader := bufio.NewReaderSize(errReader, 8192)
		for {
			line, _, err := reader.ReadLine()
			if err != nil {
				return
			}
			if p.lines.Len() >= 100 {
				p.lines.Get(1)
			}
			p.lines.Put(string(line))
		}
	}()

	// 子进程在这个 goroutine 里启动并等待退出：Start 不等它起来，启动失败只记进 exitErr
	// （与改动前一致，见 CLAUDE.md「面板报告的 xray 状态可能是假的」）。cmd 的字段只在这里读写。
	go func() {
		defer close(done)
		err := cmd.Start()
		p.mu.Lock()
		p.starting = false
		if err != nil {
			p.exited = true
			p.exitErr = err
			p.mu.Unlock()
			return
		}
		p.proc = cmd.Process
		p.mu.Unlock()

		err = cmd.Wait()
		p.mu.Lock()
		p.exited = true
		if err != nil {
			p.exitErr = err
		}
		p.mu.Unlock()
	}()

	p.refreshVersion()
	p.refreshAPIPort()

	return nil
}

// ErrNotRunning 是 Stop 遇到进程本来就没在跑（或在 IsRunning 与 Stop 之间自己退出了）时返回的错误。
// 调用方据此区分「没什么可停的」与「停不下来」：前者可以接着起新进程，后者不行。
var ErrNotRunning = errors.New("xray is not running")

// stopWaitTimeout 是 Stop 等旧进程退出的上限。SIGKILL 之后内核回收通常是毫秒级，
// 等满这个时长说明进程卡在不可中断的状态里，那时继续等也没有意义。做成变量是为了测试。
var stopWaitTimeout = 5 * time.Second

// Stop 杀掉子进程，并等它真的退出（有界）再返回。
//
// 必须等：调用方（RestartXray）紧接着就起新进程，旧进程还占着入站端口的话，新 xray 会因
// bind: address already in use 起不来，整机所有入站一起断——而 Start 从不回传这种失败。
// 修复前这里发完 SIGKILL 就返回，回收发生在 Start 起的那个 goroutine 里。
func (p *process) Stop() error {
	p.mu.Lock()
	proc, running, done := p.proc, p.proc != nil && !p.exited, p.done
	p.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	// os.Process 的 Kill 与另一个 goroutine 里正在进行的 Wait 可以同时调用。
	killErr := proc.Kill()
	select {
	case <-done:
		// 进程已经退出：哪怕 Kill 报了「进程已结束」，目的也已经达到。
		return nil
	case <-time.After(stopWaitTimeout):
		if killErr != nil {
			return killErr
		}
		return common.NewErrorf("等待 xray 进程退出超时（%v）", stopWaitTimeout)
	}
}

// kill 只发 SIGKILL，不等退出，给 finalizer 用。
func (p *process) kill() {
	p.mu.Lock()
	proc, running := p.proc, p.proc != nil && !p.exited
	p.mu.Unlock()
	if running {
		_ = proc.Kill()
	}
}

func (p *process) GetTraffic(reset bool) ([]*Traffic, error) {
	p.mu.Lock()
	apiPort := p.apiPort
	p.mu.Unlock()
	if apiPort == 0 {
		return nil, common.NewError("xray api port wrong:", apiPort)
	}
	// 新版 grpc 移除了 Dial/WithInsecure。NewClient 不做阻塞式连接，
	// 首次 RPC 时才建连——对本地回环的 stats 查询没有区别。
	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%v", apiPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := statsservice.NewStatsServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	request := &statsservice.QueryStatsRequest{
		Reset_: reset,
	}
	resp, err := client.QueryStats(ctx, request)
	if err != nil {
		return nil, err
	}
	return parseTraffics(resp.GetStat()), nil
}

// parseTraffics 把 xray 的 stats 条目按 tag 聚合成流量，无法识别的条目一律跳过。
//
// xray 返回的条目不止 inbound/outbound 两类：policy 开启用户级统计时会产生
// user>>>xxx>>>traffic>>>uplink，与 x-ui 等同源面板共用同一个 api 端口时，读到的
// 统计里同样会混入这类条目。trafficRegex 不匹配时 FindStringSubmatch 返回 nil，
// 直接取下标会 panic；而本函数经 XrayTrafficJob 跑在 cron 里，cron 未配 Recover，
// 一次 panic 就会带走整个面板进程。
func parseTraffics(stats []*statsservice.Stat) []*Traffic {
	tagTrafficMap := map[string]*Traffic{}
	traffics := make([]*Traffic, 0)
	for _, stat := range stats {
		matchs := trafficRegex.FindStringSubmatch(stat.Name)
		if len(matchs) < 4 {
			continue
		}
		isInbound := matchs[1] == "inbound"
		tag := matchs[2]
		isDown := matchs[3] == "downlink"
		if tag == "api" {
			continue
		}
		traffic, ok := tagTrafficMap[tag]
		if !ok {
			traffic = &Traffic{
				IsInbound: isInbound,
				Tag:       tag,
			}
			tagTrafficMap[tag] = traffic
			traffics = append(traffics, traffic)
		}
		if isDown {
			traffic.Down = stat.Value
		} else {
			traffic.Up = stat.Value
		}
	}

	return traffics
}
