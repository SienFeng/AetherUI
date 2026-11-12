package xray

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/xtls/xray-core/app/router"
	routerService "github.com/xtls/xray-core/app/router/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeAddRule struct {
	appendMode bool
	rules      []string // ruleTag|outboundTag
	balancers  int
}

// fakeRoutingServer 记录收到的 RoutingService 请求；failRemove 指定的 ruleTag 删除时报错。
type fakeRoutingServer struct {
	routerService.UnimplementedRoutingServiceServer
	mu         sync.Mutex
	removed    []string
	adds       []fakeAddRule
	failRemove string
}

func (f *fakeRoutingServer) RemoveRule(_ context.Context, req *routerService.RemoveRuleRequest) (*routerService.RemoveRuleResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.RuleTag == f.failRemove {
		return nil, status.Error(codes.Internal, "boom")
	}
	f.removed = append(f.removed, req.RuleTag)
	return &routerService.RemoveRuleResponse{}, nil
}

func (f *fakeRoutingServer) AddRule(_ context.Context, req *routerService.AddRuleRequest) (*routerService.AddRuleResponse, error) {
	inst, err := req.Config.GetInstance()
	if err != nil {
		return nil, err
	}
	cfg, ok := inst.(*router.Config)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "not a router config")
	}
	add := fakeAddRule{appendMode: req.ShouldAppend, balancers: len(cfg.BalancingRule)}
	for _, r := range cfg.Rule {
		add.rules = append(add.rules, r.RuleTag+"|"+r.GetTag())
	}
	f.mu.Lock()
	f.adds = append(f.adds, add)
	f.mu.Unlock()
	return &routerService.AddRuleResponse{}, nil
}

func startFakeRouting(t *testing.T) (*fakeRoutingServer, *XrayAPI) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRoutingServer{}
	srv := grpc.NewServer()
	routerService.RegisterRoutingServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	api := &XrayAPI{}
	if err := api.Init(lis.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	return f, api
}

const appendTwoRules = `{"domainStrategy":"IPIfNonMatch","rules":[` +
	`{"type":"field","ruleTag":"a-ui-meter-1-x.com","inboundTag":["in-1"],"domain":["domain:x.com"],"outboundTag":"a-ui-meter-1-x.com"},` +
	`{"type":"field","ruleTag":"a-ui-meter-1-192.0.2.7","inboundTag":["in-1"],"ip":["192.0.2.7/32"],"outboundTag":"a-ui-meter-1-192.0.2.7"}]}`

func TestRemoveRulesSendsOneRemoveRulePerTag(t *testing.T) {
	f, api := startFakeRouting(t)
	if err := api.RemoveRules([]string{"a-ui-meter-1-x.com", "a-ui-meter-2-y.com"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.removed, ","); got != "a-ui-meter-1-x.com,a-ui-meter-2-y.com" {
		t.Fatalf("RemoveRule 收到 %q", got)
	}
}

func TestRemoveRulesStopsAtTheFirstError(t *testing.T) {
	f, api := startFakeRouting(t)
	f.failRemove = "b"
	if err := api.RemoveRules([]string{"a", "b", "c"}); err == nil {
		t.Fatal("删除失败应返回错误")
	}
	if got := strings.Join(f.removed, ","); got != "a" {
		t.Fatalf("出错之后不该继续删：%q", got)
	}
}

func TestAppendRoutingRulesAppendsInsteadOfReplacing(t *testing.T) {
	f, api := startFakeRouting(t)
	if err := api.AppendRoutingRules([]byte(appendTwoRules)); err != nil {
		t.Fatal(err)
	}
	if len(f.adds) != 1 || !f.adds[0].appendMode {
		t.Fatalf("应发一次追加模式的 AddRule：%+v", f.adds)
	}
	if got := strings.Join(f.adds[0].rules, ","); got != "a-ui-meter-1-x.com|a-ui-meter-1-x.com,a-ui-meter-1-192.0.2.7|a-ui-meter-1-192.0.2.7" {
		t.Fatalf("追加的规则 = %q", got)
	}
}

// 追加模式下 xray 会把段里的负载均衡器当成新的再加一遍，撞上已有的 tag 报错；在面板这边就拒绝，不发 RPC。
func TestAppendRoutingRulesRefusesBalancers(t *testing.T) {
	f, api := startFakeRouting(t)
	withBalancer := strings.Replace(appendTwoRules, `"rules":`, `"balancers":[{"tag":"b1","selector":["x"]}],"rules":`, 1)
	if err := api.AppendRoutingRules([]byte(withBalancer)); err == nil {
		t.Fatal("带 balancers 的追加段应被拒绝")
	}
	if len(f.adds) != 0 {
		t.Fatalf("被拒绝时不该发 AddRule：%+v", f.adds)
	}
}

func TestApplyRoutingConfigStillReplacesTheWholeTable(t *testing.T) {
	f, api := startFakeRouting(t)
	if err := api.ApplyRoutingConfig([]byte(appendTwoRules)); err != nil {
		t.Fatal(err)
	}
	if len(f.adds) != 1 || f.adds[0].appendMode {
		t.Fatalf("整表下发应发一次替换模式的 AddRule：%+v", f.adds)
	}
}

func TestRoutingRuleCallsNeedAnInitializedClient(t *testing.T) {
	api := &XrayAPI{}
	if err := api.RemoveRules([]string{"a"}); err == nil {
		t.Error("未初始化时 RemoveRules 应报错")
	}
	if err := api.AppendRoutingRules([]byte(appendTwoRules)); err == nil {
		t.Error("未初始化时 AppendRoutingRules 应报错")
	}
}
