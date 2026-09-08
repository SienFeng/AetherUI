package web

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"a-ui/database/model"
)

// 与 TestInboundFieldsExistInFrontendModel / TestAllSettingFieldsExistInFrontendModel
// 同源的一条约定：model.RoutingRule 的每个字段都必须在前端 routing.js 的
// RoutingRule 构造函数里有同名属性（json:"-" 标记的服务端专用字段除外）。
//
// 这条规则的前端构造函数现在有十一个位置参数：id、remark、inboundIds、
// domainGroupIds、action、outboundId、priority、enable、broken、
// groupsBroken、applyToNewInbounds。fromJson 里随便哪个参数错位，都会把
// 后面所有参数的值顺移一位静默塞进错的字段，没有任何东西会报红——这条
// 测试至少保证「字段本身存在」，堵住其中最容易犯的一类。
func TestRoutingRuleFieldsExistInFrontendModel(t *testing.T) {
	data, err := assetsFS.ReadFile("assets/js/model/routing.js")
	if err != nil {
		t.Fatalf("读取 routing.js: %v", err)
	}
	body := routingRuleConstructor(t, string(data))

	typ := reflect.TypeOf(model.RoutingRule{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if !regexp.MustCompile(`\bthis\.` + regexp.QuoteMeta(name) + `\s*=`).MatchString(body) {
			t.Errorf("model.RoutingRule 的字段 %q 在 routing.js 的 RoutingRule 构造函数里没有对应属性。"+
				"十一个位置参数里少一个，会让它后面所有参数的赋值全部错位。", name)
		}
	}
}

// routingRuleConstructor 截取 class RoutingRule 的 constructor 函数体。
func routingRuleConstructor(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "class RoutingRule")
	if start < 0 {
		t.Fatal("routing.js 里找不到 class RoutingRule，要么文件结构变了，要么本测试该更新了")
	}
	rest := src[start:]
	ctor := strings.Index(rest, "constructor(")
	if ctor < 0 {
		t.Fatal("class RoutingRule 里找不到 constructor")
	}
	end := strings.Index(rest[ctor:], "\n    }")
	if end < 0 {
		t.Fatal("找不到 constructor 的结束位置")
	}
	return rest[ctor : ctor+end]
}
