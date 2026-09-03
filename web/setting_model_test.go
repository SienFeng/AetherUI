package web

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"a-ui/web/entity"
)

// 守住设置系统里最容易踩、后果又最不成比例的一条约定：
// entity.AllSetting 的每个字段都必须在前端 models.js 的 AllSetting 构造函数里
// 有同名属性。
//
// 漏掉的后果不是「新设置项不生效」这么轻：ObjectUtil.cloneProps 只克隆目标对象
// 已有的 key，服务端返回的值会被直接丢弃；而 updateAllSetting 提交的正是这个
// JS 对象，缺失的字段在提交体里根本不存在，Gin 绑成零值——若后端校验恰好拒绝
// 零值，**整个保存配置接口都会失败**，端口、证书路径等无关字段一起遭殃，
// 报错还只指向新字段，极具误导性。
func TestAllSettingFieldsExistInFrontendModel(t *testing.T) {
	data, err := assetsFS.ReadFile("assets/js/model/models.js")
	if err != nil {
		t.Fatalf("读取 models.js: %v", err)
	}
	body := allSettingConstructor(t, string(data))

	typ := reflect.TypeOf(entity.AllSetting{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if !regexp.MustCompile(`\bthis\.` + regexp.QuoteMeta(name) + `\s*=`).MatchString(body) {
			t.Errorf("entity.AllSetting 的字段 %q 在 models.js 的 AllSetting 构造函数里没有对应属性。"+
				"不补上的话，保存面板配置会整个失败。", name)
		}
	}
}

// allSettingConstructor 截取 class AllSetting 的 constructor 函数体。
func allSettingConstructor(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "class AllSetting")
	if start < 0 {
		t.Fatal("models.js 里找不到 class AllSetting，要么文件结构变了，要么本测试该更新了")
	}
	rest := src[start:]
	ctor := strings.Index(rest, "constructor(")
	if ctor < 0 {
		t.Fatal("class AllSetting 里找不到 constructor")
	}
	end := strings.Index(rest[ctor:], "\n    }")
	if end < 0 {
		t.Fatal("找不到 constructor 的结束位置")
	}
	return rest[ctor : ctor+end]
}
