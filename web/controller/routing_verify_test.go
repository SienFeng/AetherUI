package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/service"
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

// postRouting 走真实路由发 POST，返回状态码与解码后的 JSON。
func postRouting(t *testing.T, path string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewRoutingController(r.Group("/"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s（路由未注册？）", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	return msg
}

func initTestDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	service.ResetAuditResultsForTest()
	t.Cleanup(service.ResetAuditResultsForTest)
}

// 直接落库造规则，绕过 RoutingRuleService.Add 的校验（这里只关心接口行为）。
func seedRule(t *testing.T, enable bool) int {
	t.Helper()
	rule := &model.RoutingRule{Remark: "t", InboundIds: "[]", DomainGroupIds: "[1]", Action: model.ActionBlock, Enable: true}
	if err := database.GetDB().Create(rule).Error; err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if !enable {
		if err := database.GetDB().Model(rule).Update("enable", false).Error; err != nil {
			t.Fatalf("disable rule: %v", err)
		}
	}
	return rule.Id
}

func TestRuleAuditEndpoint(t *testing.T) {
	initTestDB(t)
	id := seedRule(t, true)
	msg := postRouting(t, "/routing/rule/audit")
	if msg["success"] != true {
		t.Fatalf("success = %v, msg = %v", msg["success"], msg)
	}
	obj, _ := msg["obj"].(map[string]any)
	for _, k := range []string{"applyGen", "accessLogEnabled", "rules"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("obj 缺少 %s: %v", k, obj)
		}
	}
	rules, _ := obj["rules"].(map[string]any)
	if _, ok := rules[strconv.Itoa(id)]; !ok {
		t.Errorf("rules 缺少启用规则 %d: %v", id, rules)
	}
}

func TestAnomalyResetRejectsBadId(t *testing.T) {
	msg := postRouting(t, "/routing/rule/anomaly/reset/abc")
	if msg["success"] != false {
		t.Errorf("success = %v, want false, msg = %v", msg["success"], msg)
	}
}

func TestAnomalyResetOk(t *testing.T) {
	msg := postRouting(t, "/routing/rule/anomaly/reset/7")
	if msg["success"] != true {
		t.Errorf("success = %v, want true, msg = %v", msg["success"], msg)
	}
}

// 缓存代次等于当前代次且规则启用：原样返回缓存（CheckedAt 不变，不重新验证）。
func TestVerifyCachedReturnsCache(t *testing.T) {
	initTestDB(t)
	id := seedRule(t, true)
	const checkedAt = int64(1234567890123)
	service.StoreVerifyResultForTest(&service.RuleVerifyResult{
		RuleId: id, CheckedAt: checkedAt, ApplyGen: service.XrayApplyGeneration(),
	})
	msg := postRouting(t, "/routing/rule/verify/"+strconv.Itoa(id)+"?cached=1")
	obj, _ := msg["obj"].(map[string]any)
	if msg["success"] != true || obj["checkedAt"] != float64(checkedAt) {
		t.Errorf("want cached result, msg = %v", msg)
	}
}

// 缓存代次落后于当前代次：不能返回旧结果，必须现场验证。
func TestVerifyCachedIgnoresStaleGeneration(t *testing.T) {
	initTestDB(t)
	id := seedRule(t, true)
	const checkedAt = int64(1234567890124)
	service.StoreVerifyResultForTest(&service.RuleVerifyResult{
		RuleId: id, CheckedAt: checkedAt, ApplyGen: service.XrayApplyGeneration() + 1,
	})
	msg := postRouting(t, "/routing/rule/verify/"+strconv.Itoa(id)+"?cached=1")
	obj, _ := msg["obj"].(map[string]any)
	if msg["success"] != true || obj == nil || obj["checkedAt"] == float64(checkedAt) {
		t.Errorf("want 现场验证结果（success 且 checkedAt 不同于缓存）, msg = %v", msg)
	}
}

// 携带的发现：缓存要到下一轮审计才清理，规则停用后缓存仍在，
// cached=1 不得把它当成停用规则的结论返回，要走 Verify（它自己处理停用规则）。
func TestVerifyCachedIgnoresDisabledRule(t *testing.T) {
	initTestDB(t)
	id := seedRule(t, false)
	const checkedAt = int64(1234567890125)
	service.StoreVerifyResultForTest(&service.RuleVerifyResult{
		RuleId: id, CheckedAt: checkedAt, ApplyGen: service.XrayApplyGeneration(),
	})
	msg := postRouting(t, "/routing/rule/verify/"+strconv.Itoa(id)+"?cached=1")
	obj, _ := msg["obj"].(map[string]any)
	if obj == nil || obj["checkedAt"] == float64(checkedAt) {
		t.Fatalf("停用规则的缓存被返回: %v", msg)
	}
	core, _ := obj["core"].(map[string]any)
	if core["state"] != "error" {
		t.Errorf("want 现场 Verify 的停用结果, core = %v", core)
	}
}

// 开关读取失败时仍返回规则摘要，但必须带 accessLogError，不能伪装成「已关闭」。
func TestRuleAuditReportsAccessLogError(t *testing.T) {
	initTestDB(t)
	id := seedRule(t, true)
	if err := database.GetDB().Migrator().DropTable(&model.Setting{}); err != nil {
		t.Fatalf("drop settings: %v", err)
	}
	msg := postRouting(t, "/routing/rule/audit")
	obj, _ := msg["obj"].(map[string]any)
	if msg["success"] != true || obj == nil {
		t.Fatalf("msg = %v", msg)
	}
	if e, _ := obj["accessLogError"].(string); e == "" {
		t.Errorf("want accessLogError, obj = %v", obj)
	}
	rules, _ := obj["rules"].(map[string]any)
	if _, ok := rules[strconv.Itoa(id)]; !ok {
		t.Errorf("规则摘要丢失: %v", obj)
	}
}
