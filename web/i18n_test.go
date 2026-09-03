package web

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// C3 回归：本分支把 BurntSushi/toml 从 v0.3.1 经 MVS 强制抬到 v1.4.1-pre，
// v1.x 对重复键等更严格。initI18n 报错会一路传到 Server.Start()，面板直接
// 起不来——而 web/html_test.go 里的 i18n 模板函数是打了桩的
// （`return key, nil`），完全没走真实的 bundle.ParseMessageFileBytes 路径。
//
// 这条测试走 initI18n 的真实路径：initI18n 的方法体完全不读 s 的任何字段，
// 所以零值 &Server{} 就够用，不需要构造数据库或其他 service 依赖。
func TestInitI18nLoadsRealTranslationFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	if err := (&Server{}).initI18n(engine); err != nil {
		t.Fatalf("initI18n: %v", err)
	}

	// engine.FuncMap["i18n"] 与中间件里的 localizer 是 initI18n 内部同一个
	// 闭包变量：要拿到某个语言下的真实翻译，必须先让中间件按
	// Accept-Language 跑一遍把 localizer 换成对应语言的，再在同一次请求里
	// 调用这个函数——两者不能脱离请求上下文单独调用。
	fn, ok := engine.FuncMap["i18n"].(func(string, ...string) (string, error))
	if !ok {
		t.Fatalf("engine.FuncMap[\"i18n\"] 类型不是预期的函数签名，实际: %T", engine.FuncMap["i18n"])
	}

	// 三个 .toml 都被 bundle 收下的证据：对同一个 key，三种 Accept-Language
	// 各自解析出三个不同且确切的值——如果某个语言的文件没被加载，matcher
	// 会退回 bundle 的默认语言（zh-Hans），得到的值就不会是这里断言的那个。
	tests := []struct {
		acceptLanguage string
		want           string
	}{
		{"zh-Hans", "用户名"},
		{"zh-Hant", "用戶名"},
		{"en-US", "username"},
	}

	for _, tt := range tests {
		t.Run(tt.acceptLanguage, func(t *testing.T) {
			var got string
			var callErr error
			path := "/probe-" + tt.acceptLanguage
			engine.GET(path, func(c *gin.Context) {
				got, callErr = fn("username")
				c.Status(200)
			})

			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Accept-Language", tt.acceptLanguage)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if callErr != nil {
				t.Fatalf("i18n(\"username\") 出错: %v", callErr)
			}
			if got != tt.want {
				t.Fatalf("Accept-Language=%s 时 i18n(\"username\") = %q，期望 %q", tt.acceptLanguage, got, tt.want)
			}
		})
	}
}
