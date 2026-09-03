package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/entity"
)

// 前端的 axios 拦截器（web/assets/js/axios-init.js）用 Qs.stringify 把请求体
// 转成 application/x-www-form-urlencoded，因此 gin 走的是 Form 绑定、认的是
// form 标签。把标签写成 json 的话请求体解析不出任何字段，续期会静默变成
// 「两者都没给」而报错——本测试就是守住这条。
func newRenewRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	r := gin.New()
	a := &InboundController{}
	a.initRouter(r.Group("/xui"))
	return r
}

func createInbound(t *testing.T, expiryTime int64, enable bool, up, down int64) *model.Inbound {
	t.Helper()
	in := &model.Inbound{
		UserId: 1, Port: 10011, Protocol: model.VLESS, Tag: "inbound-10011",
		Remark: "甲", Enable: enable, ExpiryTime: expiryTime, Up: up, Down: down,
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}",
	}
	if err := database.GetDB().Create(in).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	return in
}

func postForm(t *testing.T, r *gin.Engine, path, body string) entity.Msg {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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

func TestRenewEndpointBindsUrlencodedForm(t *testing.T) {
	r := newRenewRouter(t)
	old := time.Now().Unix()*1000 + 10*86400000
	in := createInbound(t, old, false, 123, 456)

	msg := postForm(t, r, "/xui/inbound/renew/"+itoa(in.Id), "days=30&expiryTime=0")

	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	got := &model.Inbound{}
	if err := database.GetDB().First(got, in.Id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if want := old + 30*86400000; got.ExpiryTime != want {
		t.Errorf("expiryTime = %d, want %d", got.ExpiryTime, want)
	}
	if got.Up != 0 || got.Down != 0 || !got.Enable {
		t.Errorf("up/down/enable = %d/%d/%v, want 0/0/true", got.Up, got.Down, got.Enable)
	}
}

func TestRenewEndpointBindsExplicitExpiryTime(t *testing.T) {
	r := newRenewRouter(t)
	in := createInbound(t, 0, true, 0, 0)
	want := time.Now().Unix()*1000 + 99*86400000

	msg := postForm(t, r, "/xui/inbound/renew/"+itoa(in.Id), "days=0&expiryTime="+itoa64(want))

	if !msg.Success {
		t.Fatalf("success = false, msg = %q", msg.Msg)
	}
	got := &model.Inbound{}
	if err := database.GetDB().First(got, in.Id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.ExpiryTime != want {
		t.Errorf("expiryTime = %d, want %d", got.ExpiryTime, want)
	}
}

func TestRenewEndpointReportsFailureWithoutChangingData(t *testing.T) {
	r := newRenewRouter(t)
	in := createInbound(t, 0, true, 11, 22)

	msg := postForm(t, r, "/xui/inbound/renew/"+itoa(in.Id), "days=0&expiryTime=0")

	if msg.Success {
		t.Fatal("success = true, want false（未指定天数也未指定日期）")
	}
	if !strings.Contains(msg.Msg, "续期失败") {
		t.Errorf("msg = %q, want 包含「续期失败」", msg.Msg)
	}
	got := &model.Inbound{}
	if err := database.GetDB().First(got, in.Id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.ExpiryTime != 0 || got.Up != 11 || got.Down != 22 {
		t.Errorf("失败时不该改动数据，got expiry=%d up=%d down=%d", got.ExpiryTime, got.Up, got.Down)
	}
}

func itoa(i int) string     { return strconv.Itoa(i) }
func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
