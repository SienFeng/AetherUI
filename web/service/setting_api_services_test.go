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
// 两个值的字面量刻意写成键序非字母序：真正只有 RawMessage 透传才保得住、
// map[string]any 往返保不住的，是嵌套对象内部的键序——map[string]any 在
// 重新序列化时会把每一层的 map key 按字母序重排，RawMessage 因为把子树
// 当不透明字节块搬运，键序原样不动。所以这里选的键序本身就不是字母序
// （handshake < statsUserUplink，protocol < tag），一旦被按字母序重排，
// 下面 stripJSONWhitespace 之后的子串匹配就会失败。
//
// 断言用 stripJSONWhitespace 去掉空白再比较，而不是直接找原始字节：C2 把
// 输出从「encoding/json.Marshal 压出的单行」改成了 json.Indent 缩进两格，
// 这是本次修复刻意要的行为变化（模板不再被压成一行），所以空白差异不该
// 再被这条用例判定为「改动了字节」；但键序仍必须原样保留，这正是这条
// 用例真正要守住的不变量，用「去空白后子串匹配」既能容忍新增的缩进，
// 又不会像 map[string]any 归一化那样把键序一起吞掉。
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

	stripped := stripJSONWhitespace(out)
	if !strings.Contains(stripped, policyValue) {
		t.Fatalf("policy 的原始键序未被保留\n输出: %s\n期望包含: %s", out, policyValue)
	}
	if !strings.Contains(stripped, outboundsValue) {
		t.Fatalf("outbounds 的原始键序未被保留\n输出: %s\n期望包含: %s", out, outboundsValue)
	}
	// api 本身必须确实被改过，否则一个「什么都不做」的实现会让上面两条假通过。
	if !strings.Contains(out, `"RoutingService"`) {
		t.Fatalf("api.services 未补上 RoutingService\n输出: %s", out)
	}
}

// stripJSONWhitespace 去掉空格、制表符、换行与回车，用于在断言里比较
// 「结构性字节（键序、取值）是否被保留」而不被 json.Indent 加的缩进影响。
// 本文件测试固件里的字符串字面量都不含空白字符，所以这个朴素实现不会
// 误伤字符串内部的空白——不能拿它当通用的 JSON 空白剥离工具用。
func stripJSONWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
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

// C2 回归：ensureRoutingServiceInTemplate 曾直接 json.Marshal(root) 写回，
// 而 encoding/json 对任何实现了 MarshalJSON 的值（root 的值都是
// json.RawMessage）会在写出前跑 compact()，把整份模板压成不含空白的单行，
// 还会把 <、>、& 转义成 < 等——模板里的出站/订阅地址常见带 & 的 URL。
// 这份串随后经 setString 落库、GetAllSetting 直接读回设置页，管理员保存过
// 一次设置的部署升级后打开设置页就会看到模板变成一整行。
//
// 这条用例断言修复后的两点：输出是多行缩进的，且 < / & 未被转义——用一个
// 显式含这两个字符的出站地址触发迁移来验证。
func TestEnsureRoutingServiceOutputIsIndentedAndDoesNotEscapeHTML(t *testing.T) {
	// remark 里塞一个待转义字符、地址里塞一个 & 查询参数，模拟真实模板
	// 常见形状。
	in := `{"api":{"services":["HandlerService"],"tag":"api"},` +
		`"outbounds":[{"tag":"a-ui-node1","protocol":"vmess",` +
		`"settings":{"remark":"a<b","url":"https://example.com/x?a=1&b=2"}}]}`

	out, changed, err := ensureRoutingServiceInTemplate(in)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !changed {
		t.Fatal("期望发生改动（补上 RoutingService）")
	}

	if !strings.Contains(out, "\n  ") {
		t.Fatalf("期望输出是多行缩进的（含 \"\\n  \"），实际:\n%s", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("输出不是合法 JSON:\n%s", out)
	}

	if strings.Contains(out, `\u003c`) || strings.Contains(out, `\u0026`) {
		t.Fatalf("< 或 & 被转义成了 \\uXXXX，实际:\n%s", out)
	}
	if !strings.Contains(out, `a<b`) {
		t.Fatalf("< 未被原样保留，实际:\n%s", out)
	}
	if !strings.Contains(out, `a=1&b=2`) {
		t.Fatalf("& 未被原样保留，实际:\n%s", out)
	}

	// 反序列化回去确认语义没被破坏——只是格式变了，字段值必须还原成原样。
	var parsed struct {
		Outbounds []struct {
			Settings struct {
				Remark string `json:"remark"`
				URL    string `json:"url"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("输出无法解析回结构体: %v", err)
	}
	if len(parsed.Outbounds) != 1 {
		t.Fatalf("outbounds 数量 = %d，期望 1", len(parsed.Outbounds))
	}
	if parsed.Outbounds[0].Settings.Remark != "a<b" {
		t.Fatalf("remark = %q，期望 a<b", parsed.Outbounds[0].Settings.Remark)
	}
	if parsed.Outbounds[0].Settings.URL != "https://example.com/x?a=1&b=2" {
		t.Fatalf("url = %q，期望 https://example.com/x?a=1&b=2", parsed.Outbounds[0].Settings.URL)
	}
}
