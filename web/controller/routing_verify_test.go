package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"a-ui/database"
)

// 走真实路由发请求，同时钉住「路由确实注册在 /routing/rule/verify/:id」。
func postVerify(t *testing.T, id string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewRoutingController(r.Group("/"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/routing/rule/verify/"+id, nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s（路由未注册？）", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	return msg
}

// id 不是数字时必须直接报错，不能落到 service 里按 0 号规则去查。
func TestVerifyRuleRejectsBadId(t *testing.T) {
	msg := postVerify(t, "abc")
	if msg["success"] != false {
		t.Errorf("success = %v, want false, msg = %v", msg["success"], msg)
	}
}

// 规则不存在是 service 返回的错误，要原样走 success=false，而不是伪装成一份空结果。
func TestVerifyRuleUnknownRuleFails(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	msg := postVerify(t, "99999")
	if msg["success"] != false {
		t.Errorf("success = %v, want false, msg = %v", msg["success"], msg)
	}
}
