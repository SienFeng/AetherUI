package bootstrap

import (
	"encoding/json"
	"os"
	"testing"
)

// golden 文件来自面板前端真实生成的入站（见计划 Task 5 Step 1）。
// 这个测试锁的是「Go 侧生成的 JSON 与前端模型逐字段一致」——xray 的配置
// 校验发现不了这类差异：字段名写错核心照样能跑，只有管理员在面板里打开
// 这个入站时才会看到表单错乱或值被吞掉。
func TestBuildRealityInboundMatchesFrontendModel(t *testing.T) {
	got, err := BuildRealityInbound(RealityParams{
		Port:       443,
		UUID:       "11111111-2222-3333-4444-555555555555",
		PrivateKey: "aGVsbG8td29ybGQtdGVzdC1wcml2YXRlLWtleTEyMw",
		PublicKey:  "aGVsbG8td29ybGQtdGVzdC1wdWJsaWMta2V5MTIzNDU",
		ShortID:    "0123456789abcdef",
		Target:     "www.tesla.com:443",
		ServerName: "www.tesla.com",
		Remark:     "REALITY",
	})
	if err != nil {
		t.Fatalf("BuildRealityInbound: %v", err)
	}

	raw, err := os.ReadFile("testdata/reality_inbound.golden.json")
	if err != nil {
		t.Fatalf("读 golden: %v", err)
	}
	// golden 里 settings/streamSettings/sniffing 三个键的值本身是 JSON
	// 字符串（gen_golden.js 用 toString(false) 生成，与数据库列的存法
	// 一致），不是内嵌对象。这里必须解到 string 字段：解到 json.RawMessage
	// 会拿到「一段带引号转义的字符串」这个 JSON 值本身，再次 Unmarshal
	// 出来的是 Go string 而不是 map，之后永远没法跟 got 那边解出来的
	// map/slice 比出相等——不管 Go 侧生成的内容对不对，这处比较恒为假。
	var want struct {
		Settings       string `json:"settings"`
		StreamSettings string `json:"streamSettings"`
		Sniffing       string `json:"sniffing"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("解析 golden: %v", err)
	}

	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"settings", got.Settings, want.Settings},
		{"streamSettings", got.StreamSettings, want.StreamSettings},
		{"sniffing", got.Sniffing, want.Sniffing},
	} {
		var g, w any
		if err := json.Unmarshal([]byte(c.got), &g); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", c.name, err)
		}
		if err := json.Unmarshal([]byte(c.want), &w); err != nil {
			t.Fatalf("golden 的 %s 不是合法 JSON: %v", c.name, err)
		}
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		if string(gb) != string(wb) {
			t.Errorf("%s 与前端模型不一致\n实际: %s\n期望: %s", c.name, gb, wb)
		}
	}

	if got.Port != 443 {
		t.Errorf("port 期望 443，实际 %d", got.Port)
	}
	if !got.Enable {
		t.Error("新建入站应为启用状态")
	}
}
