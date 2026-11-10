package job

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/web/service"
)

// 一个地址成功、另一个失败时，成功那一半已经写进了合并结果——配置变了，必须置
// 重启标志。改动前只在「全部成功」时才计数，新内容停在库里不下发，而规则列表
// 因为库与核心不一致，每条规则都挂着「等待下发」。RestartXray 在配置没变时本来
// 就是空操作，多置一次标志没有代价。
func TestSubscriptionJobFlagsRestartWhenOnlySomeURLsSucceed(t *testing.T) {
	setupJobDB(t)
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

	NewSubscriptionJob().Run()

	got, err := (&service.DomainGroupService{}).Get(group.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SubscribedDomains != `["domain:a.example.com"]` {
		t.Fatalf("SubscribedDomains = %q，前提不成立：成功的那个地址应已写入", got.SubscribedDomains)
	}
	if !(&service.XrayService{}).IsNeedRestartAndSetFalse() {
		t.Error("合并结果已经变了却没有置重启标志——新内容不会下发到核心")
	}
}
