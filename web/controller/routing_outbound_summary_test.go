package controller

import (
	"encoding/json"
	"testing"

	"a-ui/database/model"
	"a-ui/web/service"
)

// 列表项把 model.OutboundNode 匿名嵌入，靠 encoding/json 的字段提升让响应
// 形状与加连通性列之前逐字节一致——前端的 OutboundNode.fromJson 读的是
// json.id / json.tag / json.remark 这些顶层键。
//
// 把嵌入改成命名字段（Node *model.OutboundNode `json:"node"`）Go 侧照常编译、
// 测试也照常跑，但前端整张出站节点表会变成一片空行：每个字段都读到
// undefined，而控制台不报任何错。这条测试守的就是这个。
func TestOutboundSummaryKeepsNodeFieldsAtTopLevel(t *testing.T) {
	summary := &outboundSummary{
		OutboundNode: &model.OutboundNode{
			Id: 3, Tag: "a-ui-美国", Remark: "美国", Protocol: "socks", Enable: true,
		},
		Probe: &service.ProbeResult{Status: service.ProbeOK, LatencyMs: 412, CheckedAt: 1757000000},
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"id", "tag", "remark", "protocol", "enable"} {
		if _, ok := got[key]; !ok {
			t.Errorf("key %q missing from top level: %s", key, data)
		}
	}
	if _, ok := got["OutboundNode"]; ok {
		t.Errorf("embedded struct leaked as its own key: %s", data)
	}

	probe, ok := got["probe"].(map[string]any)
	if !ok {
		t.Fatalf("probe is not an object: %s", data)
	}
	if probe["status"] != service.ProbeOK {
		t.Errorf("probe.status = %v, want %q", probe["status"], service.ProbeOK)
	}
	if probe["latencyMs"] != float64(412) {
		t.Errorf("probe.latencyMs = %v, want 412", probe["latencyMs"])
	}
	// ProbeResult 内部用一份 tag 挡住 SQLite 的 id 复用，它不该出现在响应里。
	if _, leaked := probe["tag"]; leaked {
		t.Errorf("internal tag leaked into the response: %s", data)
	}
}

// 未测试过的节点必须给出 probe: null，前端据此显示「未测试」。
func TestOutboundSummaryEmitsNullProbeWhenUntested(t *testing.T) {
	data, err := json.Marshal(&outboundSummary{
		OutboundNode: &model.OutboundNode{Id: 4, Tag: "a-ui-x"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	value, present := got["probe"]
	if !present {
		t.Fatalf("probe key missing entirely: %s", data)
	}
	if value != nil {
		t.Errorf("probe = %v, want null: %s", value, data)
	}
}
