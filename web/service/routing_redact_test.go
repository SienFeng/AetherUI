package service

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/op/go-logging"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/logger"
)

// captureLogs 把面板日志临时导到一个管道里，返回取出全部内容并恢复原状的函数。
// logger.InitLogger 在调用那一刻绑定 os.Stderr，所以要先换 os.Stderr 再重建一次。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	logger.InitLogger(logging.DEBUG)
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()
	restored := false
	restore := func() string {
		if !restored {
			restored = true
			w.Close()
			<-done
			os.Stderr = old
			logger.InitLogger(logging.INFO)
		}
		return buf.String()
	}
	t.Cleanup(func() { restore() })
	return restore
}

// 订阅地址的 query 里常带着访问令牌（机场/规则托管服务的 ?token=、?key=），
// userinfo 里则是账号密码。拉取失败时的错误文本会把整串地址带出来，而面板日志
// 会被拷去排查、贴进工单——日志里只能留下 scheme://host/path。
func TestRefreshFailureLogsDoNotLeakSubscriptionSecrets(t *testing.T) {
	const secret = "SECRET-TOKEN-5737"
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,a.example.com\n"))
	}))
	defer good.Close()
	secretURL := strings.Replace(failing.URL, "http://", "http://user:"+secret+"@", 1) + "/list?token=" + secret

	cases := []struct {
		name, subscribeUrl string
	}{
		{"全部失败", secretURL},
		{"部分失败", good.URL + "\n" + secretURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupDB(t)
			group := &model.DomainGroup{Remark: "订阅组", Domains: "[]", SubscribeUrl: c.subscribeUrl}
			if err := database.GetDB().Save(group).Error; err != nil {
				t.Fatalf("save group: %v", err)
			}
			logs := captureLogs(t)
			if _, err := (&DomainGroupService{}).RefreshDue(); err != nil {
				t.Fatalf("RefreshDue: %v", err)
			}
			out := logs()
			if strings.Contains(out, secret) {
				t.Fatalf("日志里出现了订阅地址的凭据/令牌：\n%s", out)
			}
			host := strings.TrimPrefix(failing.URL, "http://")
			if !strings.Contains(out, host+"/list") {
				t.Errorf("日志里应保留失败地址的 host/path 以便排查，实际：\n%s", out)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com/list":                     "https://example.com/list",
		"https://example.com/list?token=abc":           "https://example.com/list?***",
		"https://user:pass@example.com/a?x=1&y=2#frag": "https://***@example.com/a?***",
		"http://example.com:8080/rules.list?":          "http://example.com:8080/rules.list?***",
		"https://example.com/a#section":                "https://example.com/a",
		"no url here":                                  "no url here",
		"https://exa mple.com/?token=abc":              "https://exa mple.com/?***",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestRedactURLsInText(t *testing.T) {
	in := "1 个订阅地址失败（已保留它们上一次的内容）：https://u:p@example.com/a?token=abc（拉取订阅失败: " +
		`Get "https://u:p@example.com/a?token=abc": dial tcp 192.0.2.1:443: i/o timeout）；` +
		"http://example.com/b?k=v（订阅地址返回状态码: 500）"
	got := redactURLsInText(in)
	for _, leak := range []string{"token=abc", "u:p@", "k=v"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactURLsInText 没去掉 %q：%s", leak, got)
		}
	}
	for _, keep := range []string{"https://***@example.com/a?***（拉取订阅失败", `Get "https://***@example.com/a?***": dial tcp 192.0.2.1:443`, "http://example.com/b?***（订阅地址返回状态码: 500）"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redactURLsInText 应保留 %q，实际：%s", keep, got)
		}
	}
}

// 导入报告的失败消息同时写进日志（ImportReport.fail）。订阅地址不合法时消息里
// 带着原地址；报告是给导入者本人看的，日志那一份必须脱敏。
func TestImportFailureLogDoesNotLeakSubscriptionSecrets(t *testing.T) {
	const secret = "SECRET-TOKEN-5737"
	setupDB(t)
	raw := `{"kind":"a-ui-routing-export","version":1,"scope":["domainGroups"],` +
		`"domainGroups":[{"remark":"g","domains":["domain:a.example.com"],` +
		`"subscribeUrl":"ftp://user:` + secret + `@example.com/list?token=` + secret + `"}],` +
		`"outbounds":[],"rules":[]}`
	logs := captureLogs(t)
	report, err := (&RoutingPortableService{}).Import(raw)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	out := logs()
	if report.DomainGroups.Failed != 1 {
		t.Fatalf("前提不成立：非法订阅地址应让这个组导入失败，报告 %+v", report)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("导入失败的日志里出现了订阅地址的凭据/令牌：\n%s", out)
	}
	if !strings.Contains(out, "example.com/list") {
		t.Errorf("日志里应保留地址的 host/path，实际：\n%s", out)
	}
}
