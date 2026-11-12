package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/xtls/xray-core/app/proxyman/command"
	routerService "github.com/xtls/xray-core/app/router/command"
	xcommon "github.com/xtls/xray-core/common"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/infra/conf"

	"a-ui/logger"
	"a-ui/util/common"
)

// rpcTimeout 是单次控制面 RPC 的上限。热应用整体发生在那个 10 秒的重启
// 消费任务里，单次调用拖太久会把后续任务一起堵住。
const rpcTimeout = 10 * time.Second

// routingRPCTimeout 比普通 RPC 宽：下发路由要在核心里重建整张规则表，
// 规则引用 geosite: 时还要现场读 dat 文件。
const routingRPCTimeout = 30 * time.Second

// XrayAPI 是运行中 xray 核心控制面（gRPC）的客户端。
//
// 与 process.go 里查流量用的那个连接分开：那条连接由 GetTraffic 每次现开
// 现关，而控制面调用需要在一次热应用里连续发多条命令，共用一个连接。
type XrayAPI struct {
	HandlerServiceClient *command.HandlerServiceClient
	RoutingServiceClient *routerService.RoutingServiceClient

	conn *grpc.ClientConn
}

// Init 连上本机的 xray gRPC 控制面。apiPort 来自 Process.GetAPIPort()。
func (x *XrayAPI) Init(apiPort int) error {
	if apiPort <= 0 {
		return common.NewError("xray api port wrong:", apiPort)
	}
	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%v", apiPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	x.conn = conn

	hsClient := command.NewHandlerServiceClient(conn)
	rsClient := routerService.NewRoutingServiceClient(conn)
	x.HandlerServiceClient = &hsClient
	x.RoutingServiceClient = &rsClient
	return nil
}

// WaitReady 主动建立连接并等到它就绪，ctx 到期或连接失败（如端口上没人监听）返回 error。
//
// grpc.NewClient 是惰性的：Init 成功不代表核心在听，核心刚拉起还没监听控制面端口时，
// Init 照样成功、之后每次 RPC 才报 Unavailable。调用方要区分「核心未就绪」与
// 「核心对某个目标给出了结论」时，先调它。
func (x *XrayAPI) WaitReady(ctx context.Context) error {
	if x.conn == nil {
		return common.NewError("xray api is not initialized")
	}
	x.conn.Connect()
	for {
		state := x.conn.GetState()
		switch state {
		case connectivity.Ready:
			return nil
		case connectivity.TransientFailure, connectivity.Shutdown:
			return common.NewError("xray 控制面连接失败:", state.String())
		}
		if !x.conn.WaitForStateChange(ctx, state) {
			return common.NewError("等待 xray 控制面就绪超时:", ctx.Err())
		}
	}
}

// Close 释放连接。可重复调用，也可以在 Init 失败后调用——热应用的每条
// 失败路径都会走到这里的 defer。
func (x *XrayAPI) Close() {
	if x.conn != nil {
		_ = x.conn.Close()
		x.conn = nil
	}
	x.HandlerServiceClient = nil
	x.RoutingServiceClient = nil
}

// AddInbound 把一段入站 JSON 加进运行中的核心。
//
// JSON 要先经 infra/conf 编译成 typed message——这正是面板必须与运行中
// 核心同版本的原因：老版本的解析器编译不出新协议的入站。
func (x *XrayAPI) AddInbound(inbound []byte) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ensureXrayAssetLocation()

	c := new(conf.InboundDetourConfig)
	if err := json.Unmarshal(inbound, c); err != nil {
		return common.NewError("入站配置无法解析:", err)
	}
	built, err := c.Build()
	if err != nil {
		return common.NewError("入站配置无法构建:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = (*x.HandlerServiceClient).AddInbound(ctx, &command.AddInboundRequest{Inbound: built})
	return err
}

// DelInbound 按 tag 摘掉一个入站。
func (x *XrayAPI) DelInbound(tag string) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err := (*x.HandlerServiceClient).RemoveInbound(ctx, &command.RemoveInboundRequest{Tag: tag})
	return err
}

// AddOutbound 把一段出站 JSON 加进运行中的核心。
func (x *XrayAPI) AddOutbound(outbound []byte) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ensureXrayAssetLocation()

	c := new(conf.OutboundDetourConfig)
	if err := json.Unmarshal(outbound, c); err != nil {
		return common.NewError("出站配置无法解析:", err)
	}
	built, err := c.Build()
	if err != nil {
		return common.NewError("出站配置无法构建:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = (*x.HandlerServiceClient).AddOutbound(ctx, &command.AddOutboundRequest{Outbound: built})
	return err
}

// DelOutbound 按 tag 摘掉一个出站。
func (x *XrayAPI) DelOutbound(tag string) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err := (*x.HandlerServiceClient).RemoveOutbound(ctx, &command.RemoveOutboundRequest{Tag: tag})
	return err
}

// ApplyRoutingConfig 用整段 routing 配置替换核心里的规则表与 balancer。
//
// ShouldAppend=false 表示整体替换而非追加：面板每次都生成完整的规则集，
// 追加会让旧规则残留在核心里，删掉一条分流规则就变成删不掉。
//
// 注意这个 RPC 改不了 routing.domainStrategy / domainMatcher，那两个在
// 进程启动时固定——hot_diff.go 因此把它们归进「必须重启」的部分。
//
// 代价：核心会把每条规则重建一遍，内联的域名与 IP 段不进 xray 的缓存、每建一个匹配器强制 GC 两次。
// 只换了计量规则时用 RemoveRules + AppendRoutingRules 增量下发（xray.TailRuleDelta）。
func (x *XrayAPI) ApplyRoutingConfig(routing []byte) error {
	return x.addRoutingRules(routing, false)
}

// AppendRoutingRules 把一段 routing 配置里的规则编译后接到核心规则表的末尾（AddRule，shouldAppend=true），
// 已有的规则原样留在核心里、不重建。段里不能带 balancers：追加模式下 xray 会把它们当成新的负载均衡器
// 再加一遍，撞上已有的 tag 就报错，所以这里直接拒绝。
func (x *XrayAPI) AppendRoutingRules(routing []byte) error {
	return x.addRoutingRules(routing, true)
}

func (x *XrayAPI) addRoutingRules(routing []byte, shouldAppend bool) error {
	if x.RoutingServiceClient == nil {
		return common.NewError("xray RoutingServiceClient is not initialized")
	}
	// 规则里的 geosite: / geoip: / ext: 要靠 dat 文件解析，把核心的资源
	// 目录指到面板的 bin/，否则规则构建会因找不到 geosite.dat 而失败。
	ensureXrayAssetLocation()

	routerConf := new(conf.RouterConfig)
	if err := json.Unmarshal(routing, routerConf); err != nil {
		return common.NewError("路由配置无法解析:", err)
	}
	built, err := routerConf.Build()
	if err != nil {
		return common.NewError("路由配置无法构建:", err)
	}
	if shouldAppend && len(built.BalancingRule) > 0 {
		return common.NewError("追加的路由段不能带 balancers")
	}

	ctx, cancel := context.WithTimeout(context.Background(), routingRPCTimeout)
	defer cancel()
	_, err = (*x.RoutingServiceClient).AddRule(ctx, &routerService.AddRuleRequest{
		ShouldAppend: shouldAppend,
		Config:       serial.ToTypedMessage(built),
	})
	return err
}

// RemoveRules 按 ruleTag 逐条删掉核心里的路由规则（RoutingService.RemoveRule）。核心里没有某个 tag 时
// xray 不报错。遇到第一个错误就停下返回，调用方据此退回整表下发。
func (x *XrayAPI) RemoveRules(ruleTags []string) error {
	if x.RoutingServiceClient == nil {
		return common.NewError("xray RoutingServiceClient is not initialized")
	}
	for _, tag := range ruleTags {
		ctx, cancel := context.WithTimeout(context.Background(), routingRPCTimeout)
		_, err := (*x.RoutingServiceClient).RemoveRule(ctx, &routerService.RemoveRuleRequest{RuleTag: tag})
		cancel()
		if err != nil {
			return common.NewError("删除路由规则 [", tag, "] 失败:", err)
		}
	}
	return nil
}

// TestRoute 向运行中的核心询问：来自 inboundTag 的、目标为 domain 或 ip 的
// 一条 TCP/443 连接，会被路由到哪个出站。
//
// domain 与 ip 二选一，非空/非 nil 的那个生效（都给时 domain 优先）。
// 只读：PublishResult=false，不会往路由统计里发布事件。端口与网络固定为
// TCP/443，因为分流规则几乎不以端口区分，这里要的是「规则表怎么走」。
//
// 核心没有任何规则命中（ErrNoClue）时返回 ("", true, nil)，调用方应按
// 「走默认出站」处理；其余错误原样返回。超时由调用方的 ctx 控制。
func (x *XrayAPI) TestRoute(ctx context.Context, inboundTag, domain string, ip net.IP) (tag string, noMatch bool, err error) {
	if x.RoutingServiceClient == nil {
		return "", false, common.NewError("xray RoutingServiceClient is not initialized")
	}
	rc := &routerService.RoutingContext{
		InboundTag: inboundTag,
		Network:    xnet.Network_TCP,
		TargetPort: 443,
	}
	if domain != "" {
		rc.TargetDomain = domain
	} else if ip != nil {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		rc.TargetIPs = [][]byte{ip}
	}
	resp, err := (*x.RoutingServiceClient).TestRoute(ctx, &routerService.TestRouteRequest{
		RoutingContext: rc,
		FieldSelectors: []string{"outbound"},
		PublishResult:  false,
	})
	if err != nil {
		if isNoClueErr(err) {
			return "", true, nil
		}
		return "", false, err
	}
	return resp.GetOutboundTag(), false, nil
}

// isNoClueErr 判断 err 是不是核心的 common.ErrNoClue（没有规则可判断）。
//
// 错误经 gRPC 跨进程后只剩 status 里的消息文本，errors.Is 已经认不出原始
// 的 sentinel，比对消息文本是唯一可行的识别方式。
func isNoClueErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(status.Convert(err).Message(), xcommon.ErrNoClue.Error())
}

// ensureXrayAssetLocation 把 xray 的资源目录指到面板的 bin/。
//
// 面板进程内的 infra/conf 解析 geosite:/geoip: 时要读 dat 文件，而它默认
// 在可执行文件同目录找。面板的工作目录就是安装根目录（systemd 的
// WorkingDirectory=/usr/local/a-ui/），dat 在 bin/ 下。
//
// 已经设了就不覆盖：管理员可能刻意指到别处。
func ensureXrayAssetLocation() {
	if os.Getenv("XRAY_LOCATION_ASSET") != "" || os.Getenv("xray.location.asset") != "" {
		return
	}
	abs, err := filepath.Abs("bin")
	if err != nil {
		logger.Warning("无法解析 bin 目录的绝对路径，geosite/geoip 可能解析失败:", err)
		return
	}
	_ = os.Setenv("XRAY_LOCATION_ASSET", abs)
}

// EnsureAssetLocation 供包外（路由验证的域名/IP 匹配器）在加载 geosite:/ext: 条目前
// 把 xray 资源目录指到 bin/。
func EnsureAssetLocation() {
	ensureXrayAssetLocation()
}
