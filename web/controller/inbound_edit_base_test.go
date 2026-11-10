package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"a-ui/database/model"
)

// 编辑请求先整行绑定到 model.Inbound，再按 form 取页面打开时的 enable / autoTravel。两次绑定读的是
// 同一份已解析的表单；带了就是非 nil（true/false 都要区分出来），没带（升级前打开的页面）就是 nil。
func TestInboundEditBaseBindsAfterInboundBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		body             string
		enable, autoTrav *bool
	}{
		{"port=10011&protocol=vless&enable=true&enableBase=true&autoTravelBase=false", ptr(true), ptr(false)},
		{"port=10011&protocol=vless&enable=true", nil, nil},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req
		in := &model.Inbound{}
		if err := c.ShouldBind(in); err != nil {
			t.Fatalf("ShouldBind(inbound): %v", err)
		}
		if in.Port != 10011 {
			t.Fatalf("请求体没被解析（port=%d）", in.Port)
		}
		var base inboundEditBaseForm
		if err := c.ShouldBindWith(&base, binding.Form); err != nil {
			t.Fatalf("ShouldBindWith(base): %v", err)
		}
		if !sameBoolPtr(base.EnableBase, tc.enable) || !sameBoolPtr(base.AutoTravelBase, tc.autoTrav) {
			t.Errorf("%q: enableBase=%v autoTravelBase=%v", tc.body, show(base.EnableBase), show(base.AutoTravelBase))
		}
	}
}

func ptr(b bool) *bool { return &b }

func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func show(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
