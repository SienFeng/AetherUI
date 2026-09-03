package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"a-ui/util/netdiag"
)

// 这两个接口同样走 urlencoded 请求体（见 inbound_renew_test.go 的说明），
// 踢人接口的 ip 参数必须用 form 标签绑定。

func decodeOnlineObj(t *testing.T, obj interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("重新编码响应体: %v", err)
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应体 %q: %v", raw, err)
	}
	return out
}

func TestOnlinesEndpointAlwaysReturnsSupportedFlag(t *testing.T) {
	r := newRenewRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/xui/inbound/onlines/"+itoa(in.Id), "")

	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeOnlineObj(t, msg.Obj)
	supported, ok := obj["supported"].(bool)
	if !ok {
		t.Fatalf("响应里没有 supported 字段: %v", obj)
	}
	if supported != netdiag.Supported {
		t.Errorf("supported = %v，期望与平台能力 %v 一致", supported, netdiag.Supported)
	}
	if !supported && obj["reason"] == "" {
		t.Error("不支持时必须给出 reason，否则界面无法区分「看不到」和「没人在线」")
	}
	if obj["list"] == nil {
		t.Error("list 不能是 null，前端会直接拿去渲染表格")
	}
}

func TestOnlinesEndpointRejectsUnknownInbound(t *testing.T) {
	r := newRenewRouter(t)

	msg := postForm(t, r, "/xui/inbound/onlines/99999", "")

	if msg.Success {
		t.Fatal("success = true，不存在的入站应当报错")
	}
	if !strings.Contains(msg.Msg, "在线明细") {
		t.Errorf("msg = %q，want 包含「在线明细」", msg.Msg)
	}
}

func TestKickEndpointBindsIPFromUrlencodedForm(t *testing.T) {
	r := newRenewRouter(t)
	in := createInbound(t, 0, true, 0, 0)

	// 合法 IP：能否真的踢掉取决于平台，但至少不能落到「IP 格式不正确」——
	// 那说明 form 绑定没生效。
	msg := postForm(t, r, "/xui/inbound/kick/"+itoa(in.Id), "ip=114.114.114.114")
	if strings.Contains(msg.Msg, "IP 格式不正确") {
		t.Errorf("msg = %q，合法 IP 却报格式错误，说明请求体没被 form 绑定读到", msg.Msg)
	}

	// 非法 IP 必须被挡住，而不是拿去和连接表比对。
	bad := postForm(t, r, "/xui/inbound/kick/"+itoa(in.Id), "ip=not-an-ip")
	if bad.Success {
		t.Fatal("success = true，非法 IP 应当报错")
	}
	if !strings.Contains(bad.Msg, "IP 格式不正确") {
		t.Errorf("msg = %q，want 包含「IP 格式不正确」", bad.Msg)
	}

	// 完全不传 ip 同样按格式非法处理。
	empty := postForm(t, r, "/xui/inbound/kick/"+itoa(in.Id), "")
	if empty.Success {
		t.Fatal("success = true，未指定 IP 应当报错")
	}
}
