package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	handlerService "github.com/xtls/xray-core/app/proxyman/command"
	routerService "github.com/xtls/xray-core/app/router/command"

	"a-ui/database/model"
	"a-ui/xray"
)

// rawXrayConfig 造一份能直接交给真 xray 的配置：控制面入站（apiPort）、一个 socks 入站（tag in-socks）、
// 默认 freedom 之后的若干 freedom 出站，以及控制面规则之后的若干规则。rules 的值是规则对象本身（不含 type）。
func rawXrayConfig(t *testing.T, apiPort, socksPort int, outbounds []orderOutbound, rules []map[string]any) *xray.Config {
	t.Helper()
	outs := []any{map[string]any{"protocol": "freedom", "settings": map[string]any{}}}
	for _, o := range outbounds {
		outs = append(outs, map[string]any{"protocol": "freedom", "tag": o.tag, "settings": map[string]any{"userLevel": o.ver}})
	}
	rs := []any{map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"}}
	for _, r := range rules {
		r["type"] = "field"
		rs = append(rs, r)
	}
	raw, err := json.Marshal(map[string]any{
		"api": map[string]any{"tag": "api", "services": []string{"HandlerService", "RoutingService"}},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "dokodemo-door", "settings": map[string]any{"address": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": socksPort, "protocol": "socks", "settings": map[string]any{"auth": "noauth", "udp": false}, "tag": "in-socks"},
		},
		"outbounds": outs,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rs},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &xray.Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startRawXray 用 cfg 起一个真 xray（不经过面板的生成链），返回连上它控制面的客户端。
func startRawXray(t *testing.T, cfg *xray.Config, apiPort int) *xray.XrayAPI {
	t.Helper()
	proc := xray.NewProcess(cfg)
	if err := proc.Start(); err != nil {
		t.Fatalf("启动 xray: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })
	// WaitReady 只连一次、连不上就报错（它是给「核心是否就绪」的判断用的），先等端口开始监听。
	waitForPort(t, apiPort)
	api := &xray.XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatalf("连接控制面: %v", err)
	}
	t.Cleanup(api.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := api.WaitReady(ctx); err != nil {
		t.Fatalf("控制面没有就绪: %v", err)
	}
	return api
}

// coreState 读运行中核心的出站 tag 集合与规则表（出站|ruleTag）。
func coreState(t *testing.T, api *xray.XrayAPI) (map[string]bool, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obs, err := (*api.HandlerServiceClient).ListOutbounds(ctx, &handlerService.ListOutboundsRequest{})
	if err != nil {
		t.Fatalf("ListOutbounds: %v", err)
	}
	tags := map[string]bool{}
	for _, o := range obs.Outbounds {
		tags[o.Tag] = true
	}
	rules, err := (*api.RoutingServiceClient).ListRule(ctx, &routerService.ListRuleRequest{})
	if err != nil {
		t.Fatalf("ListRule: %v", err)
	}
	var list []string
	for _, r := range rules.Rules {
		list = append(list, r.Tag+"|"+r.RuleTag)
	}
	return tags, list
}

// checkingCoreAPI 把每一次改动核心的调用原样转给真 xray，调用之后立刻读核心的出站与规则表，
// 记下指向不存在出站的规则（控制面规则的 api 出站是核心内部的，不在出站列表里）。
type checkingCoreAPI struct {
	*xray.XrayAPI
	t        *testing.T
	calls    *[]string
	dangling *[]string
}

func (c checkingCoreAPI) after(call string, err error) error {
	*c.calls = append(*c.calls, call)
	tags, rules := coreState(c.t, c.XrayAPI)
	for _, r := range rules {
		out := strings.SplitN(r, "|", 2)[0]
		if out != "api" && !tags[out] {
			*c.dangling = append(*c.dangling, call+" 之后："+r)
		}
	}
	return err
}

func (c checkingCoreAPI) DelInbound(tag string) error {
	return c.after("delInbound "+tag, c.XrayAPI.DelInbound(tag))
}
func (c checkingCoreAPI) AddInbound(raw []byte) error {
	return c.after("addInbound", c.XrayAPI.AddInbound(raw))
}
func (c checkingCoreAPI) DelOutbound(tag string) error {
	return c.after("delOutbound "+tag, c.XrayAPI.DelOutbound(tag))
}
func (c checkingCoreAPI) AddOutbound(raw []byte) error {
	var ob struct {
		Tag string `json:"tag"`
	}
	_ = json.Unmarshal(raw, &ob)
	return c.after("addOutbound "+ob.Tag, c.XrayAPI.AddOutbound(raw))
}
func (c checkingCoreAPI) ApplyRoutingConfig(routing []byte) error {
	return c.after("full", c.XrayAPI.ApplyRoutingConfig(routing))
}
func (c checkingCoreAPI) RemoveRules(tags []string) error {
	return c.after("removeRules "+strings.Join(tags, ","), c.XrayAPI.RemoveRules(tags))
}
func (c checkingCoreAPI) AppendRoutingRules(routing []byte) error {
	return c.after("appendRules", c.XrayAPI.AppendRoutingRules(routing))
}

// 真 xray：规则指向的出站不存在时，核心把命中它的连接直接断开，不回落默认出站——热应用必须先下发路由、
// 再删出站的根据。源码在 app/dispatcher/default.go 的 routedDispatch（"non existing outTag"，
// 注释写着 DO NOT CHANGE），26.7.28 与 26.11.9 相同。
//
// 这一条推翻了 CLAUDE.md 旧表里「规则引用已删除的出站：运行时静默回落默认出站（直连）」的结论。
func TestCoreDropsConnectionsRoutedToAMissingOutbound(t *testing.T) {
	requireXrayBinary(t)
	requireXrayRoutingService(t)
	echoPort := startEchoServer(t)
	apiPort, socksPort := freePort(t), freePort(t)
	cfg := rawXrayConfig(t, apiPort, socksPort,
		[]orderOutbound{{"a-ui-node-m", 0}},
		[]map[string]any{{"ruleTag": "r-m", "inboundTag": []string{"in-socks"}, "port": fmt.Sprint(echoPort), "outboundTag": "a-ui-node-m"}})
	api := startRawXray(t, cfg, apiPort)
	waitForPort(t, socksPort)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	roundTrip := func() string {
		c, err := socks5Dial(socksAddr, "127.0.0.1", echoPort)
		if err != nil {
			return "" // 握手就被断开，同样算没通
		}
		defer c.Close()
		return echoRoundTrip(c)
	}

	if got := roundTrip(); got != "ping" {
		t.Fatalf("对照：规则指向存在的出站时应当能通，读到 %q", got)
	}
	if err := api.DelOutbound("a-ui-node-m"); err != nil {
		t.Fatalf("DelOutbound: %v", err)
	}
	if got := roundTrip(); got == "ping" {
		t.Fatal("规则指向的出站删掉之后连接照样通了：核心回落了默认出站，与源码和本测试的前提不符——" +
			"先查 xray 版本是否改了 routedDispatch，再决定热应用的顺序还要不要讲究")
	}
	if err := api.RemoveRules([]string{"r-m"}); err != nil {
		t.Fatalf("RemoveRules: %v", err)
	}
	if got := roundTrip(); got != "ping" {
		t.Fatalf("对照：去掉那条规则、走默认出站时应当能通，读到 %q（说明断开不是默认出站本身不通造成的）", got)
	}
}

// 真 xray：sendHotDiff 每发一条 RPC 之后，核心里都没有指向不存在出站的规则（只有编辑出站时，删与加之间那一次调用例外）；
// 下发完的出站集合与规则表就是新配置。三种形态：换池（增量）、删节点连同规则再加节点连同规则（整表）、编辑出站。
func TestSendHotDiffNeverLeavesARuleOnAMissingOutboundAgainstRealXray(t *testing.T) {
	requireXrayBinary(t)
	requireXrayRoutingService(t)
	const (
		meterOld  = "a-ui-meter-1-old.com"
		meterKeep = "a-ui-meter-1-keep.com"
		meterNew  = "a-ui-meter-1-new.com"
	)
	rule := func(ruleTag, out string) map[string]any {
		return map[string]any{"ruleTag": ruleTag, "domain": []string{"domain:" + ruleTag + ".test"}, "outboundTag": out}
	}
	cases := []struct {
		name               string
		oldObs, newObs     []orderOutbound
		oldRules, newRules func() []map[string]any
		wantMeterOnly      bool
		wantDangling       []string
	}{
		{
			name:   "换池",
			oldObs: []orderOutbound{{"a-ui-node-a", 0}, {meterOld, 0}, {meterKeep, 0}},
			newObs: []orderOutbound{{"a-ui-node-a", 0}, {meterKeep, 0}, {meterNew, 0}},
			oldRules: func() []map[string]any {
				return []map[string]any{rule("r-a", "a-ui-node-a"), rule(meterOld, meterOld), rule(meterKeep, meterKeep)}
			},
			newRules: func() []map[string]any {
				return []map[string]any{rule("r-a", "a-ui-node-a"), rule(meterKeep, meterKeep), rule(meterNew, meterNew)}
			},
			wantMeterOnly: true,
		},
		{
			name:   "删节点连同规则再加节点连同规则",
			oldObs: []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-b", 0}},
			newObs: []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-c", 0}},
			oldRules: func() []map[string]any {
				return []map[string]any{rule("r-a", "a-ui-node-a"), rule("r-b", "a-ui-node-b")}
			},
			newRules: func() []map[string]any {
				return []map[string]any{rule("r-a", "a-ui-node-a"), rule("r-c", "a-ui-node-c")}
			},
		},
		{
			name:   "编辑出站",
			oldObs: []orderOutbound{{"a-ui-node-a", 0}, {"a-ui-node-b", 0}},
			newObs: []orderOutbound{{"a-ui-node-a", 1}, {"a-ui-node-b", 1}},
			oldRules: func() []map[string]any {
				return []map[string]any{rule("r-a", "a-ui-node-a"), rule("r-b", "a-ui-node-b")}
			},
			newRules: func() []map[string]any {
				return []map[string]any{rule("r-a", "a-ui-node-a"), rule("r-b", "a-ui-node-b")}
			},
			wantDangling: []string{"delOutbound a-ui-node-a 之后：a-ui-node-a|r-a", "delOutbound a-ui-node-b 之后：a-ui-node-b|r-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiPort, socksPort := freePort(t), freePort(t)
			oldCfg := rawXrayConfig(t, apiPort, socksPort, tc.oldObs, tc.oldRules())
			newCfg := rawXrayConfig(t, apiPort, socksPort, tc.newObs, tc.newRules())
			api := startRawXray(t, oldCfg, apiPort)

			diff, ok := xray.ComputeHotDiff(oldCfg, newCfg)
			if !ok {
				t.Fatal("这组改动应当能热应用")
			}
			meterOnly := xray.OnlyTouchesTags(oldCfg, newCfg, diff, model.IsMeterTag)
			if meterOnly != tc.wantMeterOnly {
				t.Fatalf("用例前提不对：meterOnly = %v", meterOnly)
			}
			var calls, dangling []string
			if _, err := sendHotDiff(checkingCoreAPI{XrayAPI: api, t: t, calls: &calls, dangling: &dangling}, oldCfg, newCfg, diff, meterOnly); err != nil {
				t.Fatalf("sendHotDiff: %v（调用：%v）", err, calls)
			}
			if !reflect.DeepEqual(dangling, tc.wantDangling) {
				t.Fatalf("下发途中核心里出现了指向不存在出站的规则：\n%s\n应为：%v\n调用顺序：%v", strings.Join(dangling, "\n"), tc.wantDangling, calls)
			}

			tags, rules := coreState(t, api)
			var gotTags []string
			for tag := range tags {
				if tag != "" && tag != "api" {
					gotTags = append(gotTags, tag)
				}
			}
			sort.Strings(gotTags)
			if want := sortedTags(tc.newObs); !reflect.DeepEqual(gotTags, want) {
				t.Errorf("下发完核心里的出站 = %v，应为 %v", gotTags, want)
			}
			wantRules := []string{"api|"}
			for _, r := range tc.newRules() {
				wantRules = append(wantRules, r["outboundTag"].(string)+"|"+r["ruleTag"].(string))
			}
			if !reflect.DeepEqual(rules, wantRules) {
				t.Errorf("下发完核心的规则表 = %v，应为 %v", rules, wantRules)
			}
		})
	}
}
