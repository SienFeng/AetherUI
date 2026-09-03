package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xtls/xray-core/app/proxyman/command"
	routerService "github.com/xtls/xray-core/app/router/command"
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
func (x *XrayAPI) ApplyRoutingConfig(routing []byte) error {
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

	ctx, cancel := context.WithTimeout(context.Background(), routingRPCTimeout)
	defer cancel()
	_, err = (*x.RoutingServiceClient).AddRule(ctx, &routerService.AddRuleRequest{
		ShouldAppend: false,
		Config:       serial.ToTypedMessage(built),
	})
	return err
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
