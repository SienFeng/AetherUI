package controller

import (
	"path/filepath"
	"strings"
	"testing"

	"a-ui/database"
)

func TestAccessLogsEndpointReturnsEnabledFlagAndList(t *testing.T) {
	r := newRenewRouter(t)
	if err := database.InitAccessLogDB(filepath.Join(t.TempDir(), "access.db")); err != nil {
		t.Fatalf("InitAccessLogDB: %v", err)
	}
	in := createInbound(t, 0, true, 0, 0)

	msg := postForm(t, r, "/aui/inbound/accessLogs/"+itoa(in.Id), "page=1&pageSize=20")

	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeOnlineObj(t, msg.Obj)
	if _, ok := obj["enabled"].(bool); !ok {
		t.Fatalf("响应里没有 enabled 字段: %v", obj)
	}
	if obj["list"] == nil {
		t.Error("list 不能是 null，前端会直接拿去渲染表格")
	}
	if _, ok := obj["total"]; !ok {
		t.Error("缺少 total，前端无法分页")
	}
}

func TestAccessLogsEndpointBindsUrlencodedFilters(t *testing.T) {
	r := newRenewRouter(t)
	if err := database.InitAccessLogDB(filepath.Join(t.TempDir(), "access.db")); err != nil {
		t.Fatalf("InitAccessLogDB: %v", err)
	}
	in := createInbound(t, 0, true, 0, 0)

	// 过滤条件必须能从 urlencoded 请求体里读到。绑定标签写错的话这里
	// 不会报错，只是过滤条件被静默忽略——所以断言的是「非法页码被纠正」
	// 这种只有真读到参数才会发生的行为。
	msg := postForm(t, r, "/aui/inbound/accessLogs/"+itoa(in.Id),
		"ip=1.2.3.4&key=example.com&page=0&pageSize=99999")
	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeOnlineObj(t, msg.Obj)
	if got := obj["pageSize"]; got != float64(50) {
		t.Errorf("pageSize = %v，超限值应被纠正为默认的 50（说明参数确实被读到了）", got)
	}
	if got := obj["page"]; got != float64(1) {
		t.Errorf("page = %v，0 应被纠正为 1", got)
	}
}

func TestAccessLogsEndpointRejectsUnknownInbound(t *testing.T) {
	r := newRenewRouter(t)

	msg := postForm(t, r, "/aui/inbound/accessLogs/99999", "page=1&pageSize=20")

	if msg.Success {
		t.Fatal("success = true，不存在的入站应当报错")
	}
	if !strings.Contains(msg.Msg, "访问日志") {
		t.Errorf("msg = %q，want 包含「访问日志」", msg.Msg)
	}
}
