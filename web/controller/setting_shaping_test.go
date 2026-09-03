package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/util/tcshape"
	"a-ui/web/entity"
)

// SettingController 的构造函数要 global.GetWebServer()，测试里绕开它，
// 直接注册路由（与 inbound_renew_test.go 同一套做法）。
func newSettingRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	r := gin.New()
	(&SettingController{}).initRouter(r.Group("/xui"))
	return r
}

func postSetting(t *testing.T, r *gin.Engine, path string) entity.Msg {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var msg entity.Msg
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("解析响应 %q: %v", w.Body.String(), err)
	}
	return msg
}

func TestShapingStatusEndpointReportsPlatformSupport(t *testing.T) {
	r := newSettingRouter(t)

	msg := postSetting(t, r, "/xui/setting/shapingStatus")

	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	obj := decodeOnlineObj(t, msg.Obj)
	supported, ok := obj["supported"].(bool)
	if !ok {
		t.Fatalf("响应里没有 supported 字段: %v", obj)
	}
	if supported != tcshape.Supported {
		t.Errorf("supported = %v，期望与平台能力 %v 一致", supported, tcshape.Supported)
	}
	if !supported && obj["reason"] == "" {
		t.Error("不支持时必须给出 reason")
	}
}

func TestClearShapingEndpointExists(t *testing.T) {
	r := newSettingRouter(t)

	msg := postSetting(t, r, "/xui/setting/clearShaping")

	// 「清除全部限速规则」是 §4.5 要求的手动入口：tc 出问题时管理员
	// 必须有一个不依赖任何前置条件的撤销手段。
	// 非 Linux 上它会失败，但接口必须存在且给出明确原因。
	if msg.Success && !tcshape.Supported {
		t.Error("当前系统不支持 tc，却报告清除成功")
	}
	if !msg.Success && msg.Msg == "" {
		t.Error("失败时必须说明原因")
	}
}
