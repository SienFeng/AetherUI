package service

import (
	"encoding/json"
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

// 迁移只能碰 api.services，管理员在模板里写的别的东西一个字节都不能动。
func TestEnsureRoutingServiceLeavesOtherKeysAlone(t *testing.T) {
	in := `{"api":{"services":["HandlerService"],"tag":"api"},` +
		`"policy":{"levels":{"0":{"handshake":10}}},` +
		`"outbounds":[{"protocol":"freedom","tag":"direct"}]}`

	out, changed, err := ensureRoutingServiceInTemplate(in)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !changed {
		t.Fatal("期望发生改动")
	}

	var got, want map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatalf("输入不是合法 JSON: %v", err)
	}
	// 把 api 摘掉后，其余部分必须逐键相等。
	delete(got, "api")
	delete(want, "api")
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("api 之外的内容被改动了\n实际: %s\n期望: %s", gotJSON, wantJSON)
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
