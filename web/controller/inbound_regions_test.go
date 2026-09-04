package controller

import (
	"testing"
)

func TestProvincesEndpointAlwaysReturnsUsableShape(t *testing.T) {
	r := newRenewRouter(t)

	msg := postForm(t, r, "/aui/inbound/provinces", "")

	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeOnlineObj(t, msg.Obj)
	if _, ok := obj["loaded"].(bool); !ok {
		t.Fatalf("响应里没有 loaded 字段: %v", obj)
	}
	// IP 库没加载时也必须给出空数组而不是 null，否则表单的下拉框会直接报错。
	list, ok := obj["list"].([]any)
	if !ok {
		t.Fatalf("list 不是数组: %v", obj["list"])
	}
	if obj["loaded"] == true && len(list) == 0 {
		t.Error("库已加载却返回空的地区列表")
	}
}
