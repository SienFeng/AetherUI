package xray

import (
	"encoding/json"
	"os"
	"testing"

	"a-ui/util/json_util"
)

func rawf(s string) json_util.RawMessage { return json_util.RawMessage(s) }

// baseConfig 是一份最小可用配置：一个 api 入站、一个业务入站、一段出站、一段路由。
func baseConfig() *Config {
	return &Config{
		LogConfig: rawf(`{"loglevel":"warning"}`),
		API:       rawf(`{"services":["HandlerService","RoutingService"],"tag":"api"}`),
		Stats:     rawf(`{}`),
		Policy:    rawf(`{"levels":{"0":{"statsUserUplink":true}}}`),
		InboundConfigs: []InboundConfig{
			{Port: 62789, Protocol: "dokodemo-door", Tag: "api", Settings: rawf(`{"address":"127.0.0.1"}`)},
			{Port: 10001, Protocol: "vless", Tag: "inbound-10001", Settings: rawf(`{"clients":[{"id":"u1"}]}`)},
		},
		// 首位出站（默认出站）刻意不带 tag，与真实模板
		// （web/service/config.json）一致——RoutingInjector.buildOutbounds
		// 原样保留模板里的出站再往后追加，模板首位的 freedom 出站没有 tag。
		// 之前这里给它也写了 tag，形状与线上不符，掩盖了 C1 那个「出站热
		// 更新在真实配置上永不生效」的缺陷（decodeOutbounds 曾对任何空 tag
		// 一律判定必须重启）。
		OutboundConfigs: rawf(`[{"protocol":"freedom"},{"protocol":"blackhole","tag":"a-ui-block"}]`),
		RouterConfig:    rawf(`{"domainStrategy":"AsIs","rules":[{"type":"field","domain":["geosite:openai"],"outboundTag":"a-ui-block"}]}`),
	}
}

func TestComputeHotDiff(t *testing.T) {
	tests := []struct {
		name string
		// mutate 把 base 改成「新配置」
		mutate func(c *Config)
		wantOK bool
		// check 在 wantOK 为真时校验差分内容
		check func(t *testing.T, d *HotDiff)
	}{
		{
			name:   "完全相同则差分为空且可热应用",
			mutate: func(c *Config) {},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if !d.Empty() {
					t.Fatalf("期望空差分，实际 %+v", d)
				}
			},
		},
		{
			name: "只改路由规则可热应用",
			mutate: func(c *Config) {
				c.RouterConfig = rawf(`{"domainStrategy":"AsIs","rules":[{"type":"field","domain":["geosite:netflix"],"outboundTag":"a-ui-block"}]}`)
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.RoutingConfig) == 0 {
					t.Fatal("期望带上新的路由配置")
				}
				if len(d.AddedInbounds) != 0 || len(d.RemovedInboundTags) != 0 {
					t.Fatalf("不该动入站，实际 %+v", d)
				}
			},
		},
		{
			name: "改 domainStrategy 必须重启",
			mutate: func(c *Config) {
				c.RouterConfig = rawf(`{"domainStrategy":"IPIfNonMatch","rules":[{"type":"field","domain":["geosite:openai"],"outboundTag":"a-ui-block"}]}`)
			},
			wantOK: false,
		},
		{
			name: "新增业务入站可热应用",
			mutate: func(c *Config) {
				c.InboundConfigs = append(c.InboundConfigs, InboundConfig{
					Port: 10002, Protocol: "vless", Tag: "inbound-10002",
					Settings: rawf(`{"clients":[{"id":"u2"}]}`),
				})
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.AddedInbounds) != 1 {
					t.Fatalf("期望新增 1 个入站，实际 %d", len(d.AddedInbounds))
				}
				if len(d.RemovedInboundTags) != 0 {
					t.Fatalf("不该删入站，实际 %v", d.RemovedInboundTags)
				}
			},
		},
		{
			name: "删除业务入站可热应用",
			mutate: func(c *Config) {
				c.InboundConfigs = c.InboundConfigs[:1]
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.RemovedInboundTags) != 1 || d.RemovedInboundTags[0] != "inbound-10001" {
					t.Fatalf("期望删除 inbound-10001，实际 %v", d.RemovedInboundTags)
				}
			},
		},
		{
			name: "改动 api 入站必须重启",
			mutate: func(c *Config) {
				c.InboundConfigs[0].Port = 62790
			},
			wantOK: false,
		},
		{
			name: "改 api 段必须重启",
			mutate: func(c *Config) {
				c.API = rawf(`{"services":["HandlerService"],"tag":"api"}`)
			},
			wantOK: false,
		},
		{
			name: "改 policy 必须重启",
			mutate: func(c *Config) {
				c.Policy = rawf(`{"levels":{"0":{"statsUserUplink":false}}}`)
			},
			wantOK: false,
		},
		{
			name: "改 log（访问日志开关）必须重启",
			mutate: func(c *Config) {
				c.LogConfig = rawf(`{"loglevel":"warning","access":"bin/access.log"}`)
			},
			wantOK: false,
		},
		{
			name: "只重排 JSON 空白不算改动",
			mutate: func(c *Config) {
				c.Policy = rawf("{\n  \"levels\": {\n    \"0\": { \"statsUserUplink\": true }\n  }\n}")
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if !d.Empty() {
					t.Fatalf("仅空白差异不该产生操作，实际 %+v", d)
				}
			},
		},
		{
			name: "新增出站可热应用",
			mutate: func(c *Config) {
				// 前两个元素必须与 baseConfig 逐字节相同（尤其是无 tag 的
				// 默认出站首位）——否则这条用例实际测的是「默认出站变了还
				// 顺带加了一个出站」，而那必须整进程重启，不是这条用例的
				// 本意。之前这里把首位也写成带 tag 的 direct，与 baseConfig
				// 改前的形状凑巧一致，才没暴露这个问题。
				c.OutboundConfigs = rawf(`[{"protocol":"freedom"},{"protocol":"blackhole","tag":"a-ui-block"},{"protocol":"socks","tag":"a-ui-node1","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}]`)
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.AddedOutbounds) != 1 {
					t.Fatalf("期望新增 1 个出站，实际 %d", len(d.AddedOutbounds))
				}
			},
		},
		{
			name: "改动默认出站（数组首位）必须重启",
			mutate: func(c *Config) {
				c.OutboundConfigs = rawf(`[{"protocol":"blackhole","tag":"a-ui-block"},{"protocol":"freedom","tag":"direct"}]`)
			},
			wantOK: false,
		},
		{
			name: "入站启用 Reality 必须重启",
			mutate: func(c *Config) {
				c.InboundConfigs[1].StreamSettings = rawf(`{"network":"tcp","security":"reality","realitySettings":{"dest":"www.cloudflare.com:443"}}`)
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCfg := baseConfig()
			newCfg := baseConfig()
			tt.mutate(newCfg)

			diff, ok := ComputeHotDiff(oldCfg, newCfg)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v，期望 %v（diff=%+v）", ok, tt.wantOK, diff)
			}
			if ok && tt.check != nil {
				tt.check(t, diff)
			}
		})
	}
}

// nil 输入必须判定为「不能热应用」，而不是 panic。
func TestComputeHotDiffNilInput(t *testing.T) {
	if _, ok := ComputeHotDiff(nil, baseConfig()); ok {
		t.Fatal("oldCfg 为 nil 时不该判定可热应用")
	}
	if _, ok := ComputeHotDiff(baseConfig(), nil); ok {
		t.Fatal("newCfg 为 nil 时不该判定可热应用")
	}
}

// 新增入站的 JSON 必须能被 encoding/json 解回来，否则下发给核心必然失败。
func TestAddedInboundIsValidJSON(t *testing.T) {
	oldCfg := baseConfig()
	newCfg := baseConfig()
	newCfg.InboundConfigs = append(newCfg.InboundConfigs, InboundConfig{
		Port: 10002, Protocol: "vless", Tag: "inbound-10002",
		Settings: rawf(`{"clients":[{"id":"u2"}]}`),
	})

	diff, ok := ComputeHotDiff(oldCfg, newCfg)
	if !ok {
		t.Fatal("期望可热应用")
	}
	var parsed map[string]any
	if err := json.Unmarshal(diff.AddedInbounds[0], &parsed); err != nil {
		t.Fatalf("新增入站不是合法 JSON: %v", err)
	}
	if parsed["tag"] != "inbound-10002" {
		t.Fatalf("tag = %v，期望 inbound-10002", parsed["tag"])
	}
}

// TestComputeHotDiffAgainstRealDefaultTemplate 是 C1 的钉子测试：它不用
// baseConfig() 这个手写夹具，而是直接读真实的默认模板
// web/service/config.json（RoutingInjector.buildOutbounds 原样保留模板的
// 出站数组再往后追加），确保测试夹具的形状与线上永远一致。
//
// 这条用例的存在理由：修复前，baseConfig() 给数组首位的出站也写了 tag，
// 与真实模板（首位 freedom 出站没有 tag）形状不符，掩盖了 decodeOutbounds
// 对任何空 tag 一律判定「必须重启」的缺陷——每一份真实生成的配置首位都
// 无 tag，所以出站热更新在生产环境从未生效过。
//
// go test ./xray/ 的工作目录是 xray/ 包目录，所以用 ../web/service/config.json
// 这个相对路径读取（不依赖 web/service 包的 TestMain 那个仓库根 chdir）。
func TestComputeHotDiffAgainstRealDefaultTemplate(t *testing.T) {
	raw, err := os.ReadFile("../web/service/config.json")
	if err != nil {
		t.Fatalf("读取默认模板失败: %v", err)
	}

	oldCfg := &Config{}
	if err := json.Unmarshal(raw, oldCfg); err != nil {
		t.Fatalf("默认模板无法解析成 xray.Config: %v", err)
	}
	newCfg := &Config{}
	if err := json.Unmarshal(raw, newCfg); err != nil {
		t.Fatalf("默认模板无法解析成 xray.Config: %v", err)
	}

	// 复刻 RoutingInjector.buildOutbounds 的动作：把模板的出站数组解出来，
	// 追加一个新出站节点，再编回整段 RawMessage——这正是「新增一个出站
	// 节点」在真实生成配置里的样子。
	var outbounds []json.RawMessage
	if err := json.Unmarshal(newCfg.OutboundConfigs, &outbounds); err != nil {
		t.Fatalf("默认模板的 outbounds 段不是数组: %v", err)
	}
	outbounds = append(outbounds, json.RawMessage(
		`{"protocol":"socks","tag":"a-ui-node1","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}`,
	))
	encoded, err := json.Marshal(outbounds)
	if err != nil {
		t.Fatalf("序列化新出站数组失败: %v", err)
	}
	newCfg.OutboundConfigs = json_util.RawMessage(encoded)

	diff, ok := ComputeHotDiff(oldCfg, newCfg)
	if !ok {
		t.Fatal("对真实默认模板只新增一个出站节点，期望可热应用，实际判定必须整进程重启")
	}
	if len(diff.AddedOutbounds) != 1 {
		t.Fatalf("期望新增 1 个出站，实际 %d 个：%+v", len(diff.AddedOutbounds), diff.AddedOutbounds)
	}
	if len(diff.RemovedOutboundTags) != 0 {
		t.Fatalf("不该删除任何出站，实际 %v", diff.RemovedOutboundTags)
	}
}
