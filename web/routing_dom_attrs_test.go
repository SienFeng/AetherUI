package web

import (
	"regexp"
	"strings"
	"testing"
)

// 分流页是 in-DOM 模板：浏览器先按 HTML 规则把服务端吐出的页面解析成 DOM，Vue 再
// 编译 #app 那棵子树。HTML 解析器会把属性名一律转成小写，于是：
//
//   - @visibleChange 到 Vue 手里是 @visiblechange，而 antd 发出的事件叫 visibleChange，
//     监听器永远不会被调用；:overlayClassName 同理变成一个不存在的 prop。
//   - 写成 kebab-case 也没用：Vue 2 的 $emit 只按原名查监听器，不做大小写或连字符
//     转换，而 ant-design-vue 1.7.2 发出的事件名一律是 camelCase（pressEnter、
//     visibleChange、openChange……，没有一个带连字符）。@press-enter 同样是死代码。
//
// 两种写法都静默失效：页面照常渲染、控制台不报错，只是按回车没反应、弹层不聚焦。
// 要监听这类事件，只能改用原生事件（@keydown.enter.native）或监听 v-model 绑定的状态。
//
// 只检查分流页：其他页面不归这次改动，扫全站会因为别处的既有问题变红
// （access_log_modal.html 当年的 @pressEnter 是同一个问题，已改成原生 keydown）。
func TestRoutingPageHasNoDeadInDomAttributes(t *testing.T) {
	rendered := renderPage(t, parseAllTemplates(t), "routing.html")
	for _, re := range []*regexp.Regexp{scriptBlockPattern, styleBlockPattern, htmlCommentPattern} {
		rendered = re.ReplaceAllString(rendered, "")
	}
	for _, tag := range startTagPattern.FindAllStringSubmatch(rendered, -1) {
		name, attrs := tag[1], tag[2]
		for _, m := range attrNamePattern.FindAllStringSubmatch(attrs, -1) {
			attr := m[1]
			directive := strings.HasPrefix(attr, "@") || strings.HasPrefix(attr, ":") ||
				strings.HasPrefix(attr, "v-") || strings.HasPrefix(attr, "#")
			if (directive || strings.Contains(name, "-")) && strings.ToLower(attr) != attr {
				t.Errorf("<%s %s>：HTML 解析器会把属性名转成小写，Vue 拿到的是 %s，"+
					"camelCase 的事件/prop 因此永远对不上。改用 kebab-case 的 prop，"+
					"事件见本测试顶部的说明。", name, attr, strings.ToLower(attr))
				continue
			}
			if !strings.HasPrefix(name, "a-") {
				continue
			}
			event := ""
			switch {
			case strings.HasPrefix(attr, "@"):
				event = attr[1:]
			case strings.HasPrefix(attr, "v-on:"):
				event = attr[len("v-on:"):]
			default:
				continue
			}
			parts := strings.Split(event, ".")
			native := false
			for _, mod := range parts[1:] {
				native = native || mod == "native"
			}
			if strings.Contains(parts[0], "-") && !native {
				t.Errorf("<%s %s>：ant-design-vue 1.7.2 发出的事件名没有一个带连字符，"+
					"Vue 2 也不做 kebab/camel 转换，这个监听器永远不会被调用。", name, attr)
			}
		}
	}
}

var (
	scriptBlockPattern = regexp.MustCompile(`(?is)<script\b.*?</script>`)
	styleBlockPattern  = regexp.MustCompile(`(?is)<style\b.*?</style>`)
	htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)
	// 属性值里可能含 > 与引号（:class="a > b ? 'x' : 'y'"），逐段跳过带引号的部分。
	startTagPattern = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9-]*)((?:[^<>"']|"[^"]*"|'[^']*')*)>`)
	attrNamePattern = regexp.MustCompile(`\s([^\s"'=<>/]+)(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>]+))?`)
)
