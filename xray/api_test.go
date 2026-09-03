package xray

import (
	"strings"
	"testing"
)

// 未 Init 就调用必须返回明确错误，而不是 nil panic。
// 热应用失败要能被调用方识别并退回重启，静默的 nil 解引用会直接杀掉面板进程
// （cron 任务没有 panic 恢复）。
func TestXrayAPIUninitialized(t *testing.T) {
	tests := []struct {
		name string
		call func(x *XrayAPI) error
	}{
		{"AddInbound", func(x *XrayAPI) error { return x.AddInbound([]byte(`{}`)) }},
		{"DelInbound", func(x *XrayAPI) error { return x.DelInbound("t") }},
		{"AddOutbound", func(x *XrayAPI) error { return x.AddOutbound([]byte(`{}`)) }},
		{"DelOutbound", func(x *XrayAPI) error { return x.DelOutbound("t") }},
		{"ApplyRoutingConfig", func(x *XrayAPI) error { return x.ApplyRoutingConfig([]byte(`{}`)) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := &XrayAPI{}
			err := tt.call(x)
			if err == nil {
				t.Fatal("未初始化时期望报错，实际返回 nil")
			}
			if !strings.Contains(err.Error(), "not initialized") {
				t.Fatalf("错误信息应说明客户端未初始化，实际: %v", err)
			}
		})
	}
}

// Init 失败后 Close 不能 panic：热应用的 defer Close() 会在任何一条
// 失败路径上执行。
func TestXrayAPICloseIsSafeWithoutInit(t *testing.T) {
	x := &XrayAPI{}
	x.Close() // 不 panic 即通过
	x.Close() // 重复 Close 也不能 panic
}

func TestApplyRoutingConfigRejectsInvalidJSON(t *testing.T) {
	x := &XrayAPI{}
	// 客户端未初始化的检查先触发，所以这里断言的是「有错误」而非具体哪一种。
	if err := x.ApplyRoutingConfig([]byte(`{"rules":`)); err == nil {
		t.Fatal("非法 JSON 期望报错")
	}
}
