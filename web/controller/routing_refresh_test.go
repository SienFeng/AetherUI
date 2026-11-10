package controller

import (
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

// 「立即更新」遇到一部分地址失败：成功的那部分已经写进库，必须置重启标志让它
// 下发；响应仍然如实报失败（管理员要知道有地址没拉到）。改动前只在 err == nil
// 时置标志，这种情况下新内容一直停在库里。
func TestRefreshDomainGroupFlagsRestartOnPartialSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	xs := &service.XrayService{}
	xs.IsNeedRestartAndSetFalse() // 消费掉别的用例残留的标志

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,a.example.com\n"))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	group := &model.DomainGroup{Remark: "订阅组", Domains: "[]", SubscribeUrl: good.URL + "\n" + bad.URL}
	if err := database.GetDB().Save(group).Error; err != nil {
		t.Fatalf("save group: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/aui/routing/domain-group/refresh/"+strconv.Itoa(group.Id), nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(group.Id)}}
	(&RoutingController{}).refreshDomainGroup(c)

	got, err := (&service.DomainGroupService{}).Get(group.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SubscribedDomains != `["domain:a.example.com"]` {
		t.Fatalf("SubscribedDomains = %q，前提不成立：成功的那个地址应已写入", got.SubscribedDomains)
	}
	if !xs.IsNeedRestartAndSetFalse() {
		t.Error("部分成功时没有置重启标志——已写进库的新内容不会下发到核心")
	}
}
