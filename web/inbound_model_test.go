package web

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"a-ui/database/model"
)

// 与 TestAllSettingFieldsExistInFrontendModel 同源的一条约定：
// model.Inbound 的每个可编辑字段都必须在前端 models.js 的 DBInbound
// 构造函数里有同名属性。
//
// ObjectUtil.cloneProps 只克隆目标对象已有的 key。漏掉一个字段，服务端
// 返回的值会被直接丢弃，表单永远显示初始值；提交时该字段也不在请求体里，
// Gin 绑成零值写回数据库——表现是「改了保存后没生效，而且原来的值被清零」，
// 界面上没有任何报错。
func TestInboundFieldsExistInFrontendModel(t *testing.T) {
	data, err := assetsFS.ReadFile("assets/js/model/models.js")
	if err != nil {
		t.Fatalf("读取 models.js: %v", err)
	}
	body := dbInboundConstructor(t, string(data))

	typ := reflect.TypeOf(model.Inbound{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if !regexp.MustCompile(`\bthis\.` + regexp.QuoteMeta(name) + `\s*=`).MatchString(body) {
			t.Errorf("model.Inbound 的字段 %q 在 models.js 的 DBInbound 构造函数里没有对应属性。"+
				"不补上的话，该字段在编辑入站时会被静默清零。", name)
		}
	}
}

func dbInboundConstructor(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "class DBInbound")
	if start < 0 {
		t.Fatal("models.js 里找不到 class DBInbound，要么文件结构变了，要么本测试该更新了")
	}
	rest := src[start:]
	ctor := strings.Index(rest, "constructor(")
	if ctor < 0 {
		t.Fatal("class DBInbound 里找不到 constructor")
	}
	end := strings.Index(rest[ctor:], "\n    }")
	if end < 0 {
		t.Fatal("找不到 constructor 的结束位置")
	}
	return rest[ctor : ctor+end]
}
