package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsureRoutingServiceInTemplate(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantChanged bool
		wantErr     bool
		// wantServices 为 nil 表示不检查（错误用例）
		wantServices []string
	}{
		{
			name:         "缺少 RoutingService 时补齐并保持原有顺序",
			in:           `{"api":{"services":["HandlerService","LoggerService","StatsService"],"tag":"api"}}`,
			wantChanged:  true,
			wantServices: []string{"HandlerService", "LoggerService", "StatsService", "RoutingService"},
		},
		{
			name:         "已含 RoutingService 时不改动",
			in:           `{"api":{"services":["HandlerService","RoutingService"],"tag":"api"}}`,
			wantChanged:  false,
			wantServices: []string{"HandlerService", "RoutingService"},
		},
		{
			name:        "没有 api 段时不擅自创建",
			in:          `{"inbounds":[]}`,
			wantChanged: false,
		},
		{
			name:        "非法 JSON 报错而不是静默放行",
			in:          `{"api":`,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, err := ensureRoutingServiceInTemplate(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("期望报错，实际返回 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v，期望 %v", changed, tt.wantChanged)
			}
			if tt.wantServices == nil {
				return
			}
			var parsed struct {
				API struct {
					Services []string `json:"services"`
				} `json:"api"`
			}
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("输出不是合法 JSON: %v", err)
			}
			if len(parsed.API.Services) != len(tt.wantServices) {
				t.Fatalf("services = %v，期望 %v", parsed.API.Services, tt.wantServices)
			}
			for i, want := range tt.wantServices {
				if parsed.API.Services[i] != want {
					t.Fatalf("services[%d] = %q，期望 %q（完整: %v）", i, parsed.API.Services[i], want, parsed.API.Services)
				}
			}
		})
	}
}

// 迁移只能碰 api.services，管理员在模板里写的别的东西必须逐字节透传。
//
// 断言刻意不走 JSON 归一化：若把两边都 unmarshal 成 map[string]any 再比较，
// 一个「整份模板 map[string]any 往返」的坏实现也能通过——那种实现丢掉的恰恰
// 是本设计要保住的字节保真。所以这里直接在输出文本里找原始字节。
//
// 两个值的字面量刻意写成键序非字母序，且**不含任何多余空白**：
// encoding/json.Marshal 对任何实现了 MarshalJSON 的值（json.RawMessage 也
// 不例外）都会在写出前跑一遍 compact()，插入的空白一律会被压掉——这一步
// 与走不走 map[string]json.RawMessage 无关，两种实现都逃不掉，因此空白
// 差异守不住任何东西（曾用带空白的字面量试过：即使是本文件里正确的
// map[string]json.RawMessage 实现也会被判定为「改动了」，见 fix report 的
// 验证记录）。真正只有 RawMessage 透传才保得住、map[string]any 往返保不住
// 的，是嵌套对象内部的键序——map[string]any 在重新序列化时会把每一层的
// map key 按字母序重排，RawMessage 因为把子树当不透明字节块搬运，键序
// 原样不动。所以这里选的键序本身就不是字母序（handshake < statsUserUplink，
// protocol < tag），一旦被按字母序重排，子串匹配就会失败。
func TestEnsureRoutingServiceLeavesOtherKeysAlone(t *testing.T) {
	policyValue := `{"levels":{"0":{"statsUserUplink":true,"handshake":10}}}`
	outboundsValue := `[{"tag":"direct","protocol":"freedom"}]`
	in := `{"api":{"services":["HandlerService"],"tag":"api"},` +
		`"policy":` + policyValue + `,` +
		`"outbounds":` + outboundsValue + `}`

	out, changed, err := ensureRoutingServiceInTemplate(in)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !changed {
		t.Fatal("期望发生改动")
	}

	if !strings.Contains(out, policyValue) {
		t.Fatalf("policy 的原始字节未被保留\n输出: %s\n期望包含: %s", out, policyValue)
	}
	if !strings.Contains(out, outboundsValue) {
		t.Fatalf("outbounds 的原始字节未被保留\n输出: %s\n期望包含: %s", out, outboundsValue)
	}
	// api 本身必须确实被改过，否则一个「什么都不做」的实现会让上面两条假通过。
	if !strings.Contains(out, `"RoutingService"`) {
		t.Fatalf("api.services 未补上 RoutingService\n输出: %s", out)
	}
}

// 验证默认模板（web/service/config.json，经 go:embed 读入包变量
// xrayTemplateConfig）确实声明了 RoutingService——这是「面板启动后
// bin/config.json 里含 RoutingService」这条验收标准的等价物，用单元测试
// 覆盖是为了不用真实数据库起面板（会碰到用户机器上 /etc/a-ui/ 的真实库）。
//
// 顺带断言迁移函数对它是幂等的：默认模板已经含 RoutingService，
// ensureRoutingServiceInTemplate 不应再对其做任何改动。
func TestEnsureRoutingServiceLeavesDefaultTemplateUnchanged(t *testing.T) {
	out, changed, err := ensureRoutingServiceInTemplate(xrayTemplateConfig)
	if err != nil {
		t.Fatalf("默认模板解析失败: %v", err)
	}
	if changed {
		t.Fatal("默认模板应已经含 RoutingService，不应发生改动")
	}
	if out != xrayTemplateConfig {
		t.Fatal("未改动时应原样返回原始模板字符串")
	}

	var parsed struct {
		API struct {
			Services []string `json:"services"`
		} `json:"api"`
	}
	if err := json.Unmarshal([]byte(xrayTemplateConfig), &parsed); err != nil {
		t.Fatalf("默认模板不是合法 JSON: %v", err)
	}
	found := false
	for _, svc := range parsed.API.Services {
		if svc == "RoutingService" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("默认模板 api.services 缺少 RoutingService: %v", parsed.API.Services)
	}
}
