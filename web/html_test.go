package web

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"html/template"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// 本文件守两条模板不变量。两者都无法被 go build 发现，且都会静默失效：
//
//  1. 模板必须能解析。web.go 的 getHtmlTemplate 把 ParseFS 的错误 `// ignore` 掉了，
//     一个语法错误的模板会被静默跳过，直到渲染时才报 "template not found"。
//  2. 带 Vue 指令的元素必须落在某个 Vue 根元素内。Vue 2 只编译 el 指向的那棵子树，
//     写在根元素之外的指令是死代码——页面照常渲染，点击却毫无反应，没有任何报错。
//     分流页的三个 <a-modal> 就曾整块落在 #app 之外，所有「添加 / 编辑」按钮全部失灵。

// 顶层页面。每个都以 <html> 开头，由 controller 直接渲染。
var topLevelPages = []string{
	"login.html",
	"index.html",
	"inbounds.html",
	"routing.html",
	"setting.html",
}

// vueRootPattern 抓取脚本里的 new Vue({ el: '#xxx' })。
// 一个页面可以有多个 Vue 根：inbounds.html 的各个弹窗模板就各自带一个。
var vueRootPattern = regexp.MustCompile(`el:\s*['"]#([\w-]+)['"]`)

// parseAllTemplates 与 getHtmlTemplate 走同样的目录遍历，但**不忽略**解析错误。
func parseAllTemplates(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.New("").Funcs(template.FuncMap{
		// web.go:256 注册的唯一自定义函数，解析期只需签名一致。
		"i18n": func(key string, params ...string) (string, error) { return key, nil },
	})
	err := fs.WalkDir(htmlFS, "html", func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		matches, _ := fs.Glob(htmlFS, path+"/*.html")
		if len(matches) == 0 {
			return nil
		}
		next, parseErr := tmpl.ParseFS(htmlFS, path+"/*.html")
		if parseErr != nil {
			t.Fatalf("模板解析失败 %s: %v", path, parseErr)
		}
		tmpl = next
		return nil
	})
	if err != nil {
		t.Fatalf("遍历模板目录: %v", err)
	}
	return tmpl
}

func renderPage(t *testing.T, tmpl *template.Template, name string) string {
	t.Helper()
	var buf bytes.Buffer
	data := map[string]any{
		"base_path":   "/",
		"cur_ver":     "test",
		"title":       "test",
		"host":        "localhost",
		"request_uri": "/aui/" + name,
	}
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("渲染 %s: %v", name, err)
	}
	return buf.String()
}

func TestAllTemplatesParse(t *testing.T) {
	tmpl := parseAllTemplates(t)
	for _, page := range topLevelPages {
		if tmpl.Lookup(page) == nil {
			t.Errorf("%s 没有出现在解析结果里", page)
		}
	}
}

// isVueDirective 判断一个属性是不是 Vue 指令。
// Vue 2 的三种写法：v-xxx、@event（v-on 简写）、:prop（v-bind 简写）。
func isVueDirective(key string) bool {
	return strings.HasPrefix(key, "v-") ||
		strings.HasPrefix(key, "@") ||
		strings.HasPrefix(key, ":")
}

func TestVueDirectivesLiveInsideAVueRoot(t *testing.T) {
	tmpl := parseAllTemplates(t)
	for _, page := range topLevelPages {
		page := page
		t.Run(page, func(t *testing.T) {
			rendered := renderPage(t, tmpl, page)

			roots := map[string]bool{}
			for _, m := range vueRootPattern.FindAllStringSubmatch(rendered, -1) {
				roots[m[1]] = true
			}
			if len(roots) == 0 {
				t.Fatalf("没在 %s 里找到任何 new Vue({el:'#...'})，"+
					"要么页面结构变了，要么 vueRootPattern 该更新了", page)
			}

			doc, err := html.Parse(strings.NewReader(rendered))
			if err != nil {
				t.Fatalf("解析渲染结果: %v", err)
			}

			// insideRoot 表示当前节点位于某个 Vue 根元素之内（含根元素自身）。
			var walk func(n *html.Node, insideRoot bool)
			walk = func(n *html.Node, insideRoot bool) {
				if n.Type == html.ElementNode {
					if !insideRoot {
						for _, a := range n.Attr {
							if a.Key == "id" && roots[a.Val] {
								insideRoot = true
								break
							}
						}
					}
					// 脚本内容不是模板，跳过整棵子树。
					if n.Data == "script" {
						return
					}
					if !insideRoot {
						for _, a := range n.Attr {
							if isVueDirective(a.Key) {
								t.Errorf("<%s %s=%q> 在所有 Vue 根元素之外，"+
									"Vue 不会编译它——渲染看着正常，交互完全失效。"+
									"把它移进根元素内，或按 inbound_modal.html 的做法"+
									"给它自己的根元素和 new Vue。",
									n.Data, a.Key, a.Val)
								break
							}
						}
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, insideRoot)
				}
			}
			walk(doc, false)
		})
	}
}

// selfClosingComponentPattern 抓 <a-xxx ... /> 这种自闭合写法。
// 属性里可能含 > 与引号（如 :type="a > b ? 'x' : 'y'"），所以逐段跳过带引号的部分。
var selfClosingComponentPattern = regexp.MustCompile(`<(a-[a-z0-9-]+)((?:[^<>"']|"[^"]*"|'[^']*')*?)\s*/>`)

// TestNoSelfClosingAntdComponents 守住「不许自闭合 antd 组件」。
//
// 这几个页面是 **in-DOM 模板**：服务端吐出 HTML，浏览器先按 HTML 规则解析成
// DOM，Vue 再拿 el 指向的那棵子树去编译。而 HTML **不支持**非 void 元素的
// 自闭合写法——<a-icon type="stop"/> 会被解析成一个**开标签**，紧跟其后的
// 内容于是全部变成它的子节点。
//
// 后果特别隐蔽：a-icon 这类组件不渲染默认插槽，被吞掉的文字就此人间蒸发，
// 而图标、背景、tooltip、点击统统正常。v1.29.0 的风险列就是这么坏的——
//
//	<a-icon v-if="..." :type="..."/>
//	[[ riskLabelOf(dbInbound.id) ]]
//
// 页面上是一排只有底色没有字的小药丸，hover 还能弹出正确的 tooltip，
// 没有任何一层报错。
//
// 注意这条规则**只对 in-DOM 模板成立**：Vue 自己的 parseHTML 认自闭合，
// 所以写在 JS 字符串模板里的组件自闭合是合法的。但本仓库两种模板混着用
// （component/inbound_info.html 是字符串模板，其余是 in-DOM），而它们最终
// 都会被这里的 renderPage 渲进同一份 HTML —— 与其让每个人先判断自己改的
// 是哪一种，不如一律写显式闭合：那在两种解析器下都成立。
func TestNoSelfClosingAntdComponents(t *testing.T) {
	tmpl := parseAllTemplates(t)
	for _, page := range topLevelPages {
		page := page
		t.Run(page, func(t *testing.T) {
			rendered := renderPage(t, tmpl, page)
			for _, m := range selfClosingComponentPattern.FindAllStringSubmatch(rendered, -1) {
				t.Errorf("%s 里有自闭合的 <%s%s/>。\n"+
					"HTML 不支持非 void 元素自闭合，浏览器会把它当成开标签，"+
					"紧随其后的内容会变成它的子节点——而 antd 组件多半不渲染"+
					"默认插槽，那段内容就此消失且不报错。\n"+
					"改成 <%s%s></%s>。", page, m[1], m[2], m[1], m[2], m[1])
			}
		})
	}
}

// TestRoutingRulesRenderAsTwoSections 守住「封禁规则与分流规则必须分两段渲染」。
//
// 生成期 block 规则整体排在所有分流规则之前（routing_inject.go 的 buildRules
// 返回两个独立切片，与 priority 无关）。规则列表改成拖拽排序之后，「越往上越
// 先匹配」是界面对管理员的一句承诺——一旦有人把两段合回一张表，管理员就能把
// 封禁规则拖到最后一行，而它实际仍然第一个匹配。xray 返回 Configuration OK，
// 面板显示 running，规则表渲染得完全正常，没有任何一层会报错。
//
// 两个独立的列表同时也是段间拖拽的唯一拦截手段。
//
// v4 改版把 a-table 换成了 <li v-for> 列表（左列表 + 右侧直接编辑），
// 这条不变量随之改成按列表的循环源来守，强度不变。
func TestRoutingRulesRenderAsTwoSections(t *testing.T) {
	tmpl := parseAllTemplates(t)
	rendered := renderPage(t, tmpl, "routing.html")

	for _, want := range []string{
		`v-for="(rule, index) in blockRules"`,
		`v-for="(rule, index) in routeRules"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("规则列表少了分段循环 %s", want)
		}
	}
	// 合并回一个列表的退化形态：无论是表格还是列表，都不能直接遍历全部规则。
	for _, merged := range []string{`:data-source="rules"`, ` in rules"`} {
		if strings.Contains(rendered, merged) {
			t.Errorf("规则列表回退成了单个列表（%s），封禁段不再独立", merged)
		}
	}
	// 两个列表各一个拖拽手柄——共享的 define 必须在两处都展开，
	// 漏掉一处那一段就静默地拖不动了。
	if got := strings.Count(rendered, `class="rule-drag-handle"`); got != 2 {
		t.Errorf("拖拽手柄出现 %d 次, want 2（封禁段与分流段各一）", got)
	}
}

// TestRealitySettingsAreReachableForTrojan 守住「Trojan 选得中 REALITY，就看得到 REALITY 的参数」。
//
// 安全层单选按 canEnableReality() 对 VLESS 与 Trojan 开放，而 REALITY 参数块曾被一层
// v-if="inbound.canEnableStream()" 包着——canEnableStream() 不含 Trojan。于是 Trojan
// 能选中 reality，却看不到伪装目标与 serverNames，保存出去的 target 为空，核心拒绝整份
// 配置（实测 "please fill in a valid value for \"target\""），没有任何一层提前报错。
//
// 判定：REALITY 参数表单（v-if="inbound.stream.isReality" 的 a-form）往上的每一层
// v-if / v-show 都必须是对 Trojan + tcp 成立的条件之一。
func TestRealitySettingsAreReachableForTrojan(t *testing.T) {
	tmpl := parseAllTemplates(t)
	doc, err := html.Parse(strings.NewReader(renderPage(t, tmpl, "inbounds.html")))
	if err != nil {
		t.Fatalf("解析渲染结果: %v", err)
	}
	var form *html.Node
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if form != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a-form" && attrOf(n, "v-if") == "inbound.stream.isReality" {
			form = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if form == nil {
		t.Fatal(`inbounds.html 里找不到 <a-form v-if="inbound.stream.isReality">（REALITY 参数表单），本测试该更新了`)
	}
	admitsTrojan := []string{"canEnableTls()", "canEnableReality()", "stream.isReality"}
	for p := form.Parent; p != nil; p = p.Parent {
		for _, key := range []string{"v-if", "v-show"} {
			cond := attrOf(p, key)
			if cond == "" {
				continue
			}
			ok := false
			for _, s := range admitsTrojan {
				if strings.Contains(cond, s) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("REALITY 参数表单被 <%s %s=%q> 挡着：这个条件对 Trojan 不成立，"+
					"Trojan 选中 reality 后看不到伪装目标与 serverNames，保存的配置必被核心拒绝", p.Data, key, cond)
			}
		}
	}
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

// TestLoginPasswordIsNotTrimmed 守住「密码输入框原样提交」。
//
// v-model.trim 会把首尾空白静默削掉：密码首尾带空格的账号（升级前设置的，或经
// a-ui setting -password 写入的）在登录页永远登不进去，而错误提示只会说密码错误。
// 用户名照常 trim——那是为了容忍粘贴时多带的空格。
func TestLoginPasswordIsNotTrimmed(t *testing.T) {
	tmpl := parseAllTemplates(t)
	doc, err := html.Parse(strings.NewReader(renderPage(t, tmpl, "login.html")))
	if err != nil {
		t.Fatalf("解析渲染结果: %v", err)
	}
	models := map[string]string{}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" {
			for _, a := range n.Attr {
				if strings.HasPrefix(a.Key, "v-model") {
					models[attrOf(n, "id")] = a.Key + "=" + a.Val
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if got := models["login-password"]; got != "v-model=user.password" {
		t.Errorf("密码输入框的绑定是 %q，want %q：任何修饰符都会改掉管理员输入的密码", got, "v-model=user.password")
	}
	if got := models["login-username"]; got != "v-model.trim=user.username" {
		t.Errorf("用户名输入框的绑定是 %q，want %q", got, "v-model.trim=user.username")
	}
}

// TestNoCamelCaseAttributesInInDomTemplates 守住「in-DOM 模板里不写 camelCase 的属性名」。
//
// 这几个页面是 in-DOM 模板（见 TestNoSelfClosingAntdComponents）：浏览器的 HTML 解析器
// 先把属性名转成小写，Vue 拿到的已经是 @pressenter、:rowkey。Vue 2.6 的 $emit 按原名
// 精确匹配监听器，而 antd 发的是 pressEnter / visibleChange，于是监听器永远不触发；
// camelCase 的 prop 小写后对不上组件的 props，落进 $attrs 当成普通 DOM 属性。两种都
// 不报任何错。事件改成 kebab-case 也没用（$emit 不做大小写转换），要换原生事件
// （@keydown.enter.native）或 watch；prop 写成 kebab-case（:row-key）即可。
//
// JS 字符串模板（component/inbound_info.html 那种）由 Vue 自己解析、保留大小写；它们
// 渲染在 <script> 里，分词器把 script 内容当文本，不会扫到。SVG 里由 HTML 解析器按
// 固定表还原大小写的属性（viewBox 等）是合法的。
func TestNoCamelCaseAttributesInInDomTemplates(t *testing.T) {
	tmpl := parseAllTemplates(t)
	seen := map[string]bool{}
	for _, page := range topLevelPages {
		for _, hit := range camelCaseAttrs(t, renderPage(t, tmpl, page)) {
			key := page + " " + hit
			if _, known := camelCaseAttrExceptions[key]; known {
				seen[key] = true
				continue
			}
			t.Errorf("%s 里有 camelCase 的属性 %s：in-DOM 模板的属性名会被 HTML 解析器转成小写，"+
				"camelCase 的事件监听永远不触发、prop 传不进组件，且不报错。"+
				"事件改用原生事件（如 @keydown.enter.native）或 watch，prop 改写成 kebab-case。", page, hit)
		}
	}
	for key := range camelCaseAttrExceptions {
		if !seen[key] {
			t.Errorf("例外 %q 已经不存在了（修好了或改了写法），把它从 camelCaseAttrExceptions 里删掉", key)
		}
	}
}

// camelCaseAttrExceptions 是由别处负责修复的文件里的已知问题，键为「页面 <标签 属性>」，
// 值是问题所在的源文件。对应文件修好之后删掉条目（上面的检查会提醒）。
// 目前没有例外：真需要一个时写清楚原因与负责改它的人。
var camelCaseAttrExceptions = map[string]string{}

// svgCamelCaseAttrs 是 HTML 解析器在 SVG 里还原大小写的属性名
// （HTML 标准 "adjust SVG attributes" 那张表），在 <svg> 内写 camelCase 是对的。
var svgCamelCaseAttrs = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range strings.Fields(`attributeName attributeType baseFrequency baseProfile calcMode
		clipPathUnits diffuseConstant edgeMode filterUnits glyphRef gradientTransform gradientUnits
		kernelMatrix kernelUnitLength keyPoints keySplines keyTimes lengthAdjust limitingConeAngle
		markerHeight markerUnits markerWidth maskContentUnits maskUnits numOctaves pathLength
		patternContentUnits patternTransform patternUnits pointsAtX pointsAtY pointsAtZ preserveAlpha
		preserveAspectRatio primitiveUnits refX refY repeatCount repeatDur requiredExtensions
		requiredFeatures specularConstant specularExponent spreadMethod startOffset stdDeviation
		stitchTiles surfaceScale systemLanguage tableValues targetX targetY textLength viewBox
		viewTarget xChannelSelector yChannelSelector zoomAndPan`) {
		m[a] = true
	}
	return m
}()

// camelCaseAttrs 返回渲染结果里所有含大写字母的属性名（形如 "<a-input @pressEnter>"）。
// html.Tokenizer 的 TagAttr 已经把键转成小写，所以属性名从 Raw() 原文里重新切出来，
// 再与 TagAttr 逐个核对，切法不对就当场失败而不是漏报。
func camelCaseAttrs(t *testing.T, rendered string) []string {
	t.Helper()
	var hits []string
	z := html.NewTokenizer(strings.NewReader(rendered))
	svgDepth := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return hits
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "svg" && svgDepth > 0 {
				svgDepth--
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			raw := string(z.Raw())
			name, hasAttr := z.TagName()
			tag := string(name)
			var keys []string
			for hasAttr {
				var key []byte
				key, _, hasAttr = z.TagAttr()
				keys = append(keys, string(key))
			}
			if tag == "svg" && tt == html.StartTagToken {
				svgDepth++
			}
			names := rawAttrNames(raw)
			if len(names) != len(keys) {
				t.Fatalf("属性切分与分词器不一致：%s\n切出 %q\n分词器 %q", raw, names, keys)
			}
			for i, attr := range names {
				if strings.ToLower(attr) != keys[i] {
					t.Fatalf("属性切分与分词器不一致：%s\n切出 %q\n分词器 %q", raw, names, keys)
				}
				if attr == keys[i] || (svgDepth > 0 && svgCamelCaseAttrs[attr]) {
					continue
				}
				hits = append(hits, "<"+tag+" "+attr+">")
			}
		}
	}
}

// rawAttrNames 按 HTML 分词规则从一个开标签原文里切出属性名（保留大小写）。
func rawAttrNames(raw string) []string {
	const space = " \t\n\r\f"
	s := strings.TrimPrefix(raw, "<")
	i := strings.IndexAny(s, space+"/>")
	if i < 0 {
		return nil
	}
	s = s[i:]
	var names []string
	for {
		s = strings.TrimLeft(s, space+"/")
		if s == "" || s[0] == '>' {
			return names
		}
		j := strings.IndexAny(s[1:], space+"/>=")
		if j < 0 {
			return append(names, s)
		}
		names = append(names, s[:j+1])
		s = strings.TrimLeft(s[j+1:], space)
		if !strings.HasPrefix(s, "=") {
			continue
		}
		s = strings.TrimLeft(s[1:], space)
		if s != "" && (s[0] == '"' || s[0] == '\'') {
			k := strings.IndexByte(s[1:], s[0])
			if k < 0 {
				return names
			}
			s = s[k+2:]
			continue
		}
		k := strings.IndexAny(s, space+">")
		if k < 0 {
			return names
		}
		s = s[k:]
	}
}

// TestRequestURIStaysOutOfAttributes 守住「请求路径只进 <script>，不进任何属性」。
//
// 侧栏曾写成 :selected-keys="['{{ .request_uri }}']"：html/template 只把它当普通属性值做
// HTML 转义，浏览器解码后原样进了 Vue 表达式——路径里一个反斜杠或引号就让整个表达式
// 编译失败；带查询串时 key 对不上任何菜单项，高亮直接消失。<script> 里 html/template
// 会按 JS 字符串转义，那才是它该去的地方。
func TestRequestURIStaysOutOfAttributes(t *testing.T) {
	tmpl := parseAllTemplates(t)
	const marker = `zzmarker`
	for _, page := range []string{"index.html", "inbounds.html", "routing.html", "setting.html"} {
		t.Run(page, func(t *testing.T) {
			var buf bytes.Buffer
			data := map[string]any{
				"base_path": "/", "cur_ver": "test", "title": "test", "host": "localhost",
				"request_uri": `/aui/inbounds?` + marker + `=1&x='\`,
			}
			if err := tmpl.ExecuteTemplate(&buf, page, data); err != nil {
				t.Fatalf("渲染 %s: %v", page, err)
			}
			doc, err := html.Parse(strings.NewReader(buf.String()))
			if err != nil {
				t.Fatalf("解析渲染结果: %v", err)
			}
			menus := 0
			var walk func(n *html.Node)
			walk = func(n *html.Node) {
				if n.Type == html.ElementNode {
					for _, a := range n.Attr {
						if strings.Contains(a.Val, marker) {
							t.Errorf("<%s %s=%q>：请求路径被拼进了属性", n.Data, a.Key, a.Val)
						}
					}
					if n.Data == "a-menu" && attrOf(n, ":selected-keys") != "" {
						menus++
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(doc)
			if menus != 2 {
				t.Errorf("找到 %d 个绑定了 :selected-keys 的菜单，want 2（侧栏与窄屏抽屉）", menus)
			}
		})
	}
}

// TestPassEndTextCoversEveryEndReason 守住「出差通行证的每个结束原因都有中文说明」。
//
// region_pass_modal.html 的 PASS_END_TEXT 查不到时原样显示原因的英文键——历史记录里会
// 冒出一个 relocated，管理员看不懂。结束原因定义在 database/model/region_pass.go，
// 这里直接解析那个文件逐个核对，新增原因时忘了配文案会当场变红。
func TestPassEndTextCoversEveryEndReason(t *testing.T) {
	reasons := regionPassEndReasons(t)
	if len(reasons) < 8 {
		t.Fatalf("只从 region_pass.go 解析出 %d 个结束原因 %v，解析方式大概该更新了", len(reasons), reasons)
	}
	src, err := htmlFS.ReadFile("html/xui/region_pass_modal.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "const PASS_END_TEXT = {")
	if start < 0 {
		t.Fatal("region_pass_modal.html 里找不到 const PASS_END_TEXT = {，本测试该更新了")
	}
	end := strings.Index(body[start:], "};")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(\w+):\s*'`).FindAllStringSubmatch(body[start:start+end], -1) {
		keys[m[1]] = true
	}
	for _, r := range reasons {
		if !keys[r] {
			t.Errorf("结束原因 %q 在 PASS_END_TEXT 里没有说明，历史记录会直接显示英文键", r)
		}
	}
}

// regionPassEndReasons 解析 database/model/region_pass.go 里 RegionPass 开头的字符串常量
// （RegionPassAll 是通行证内容的通配符，不是结束原因）。
func regionPassEndReasons(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../database/model/region_pass.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 region_pass.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "RegionPass") || name.Name == "RegionPassAll" || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", name.Name, err)
					}
					out = append(out, v)
				}
			}
		}
	}
	return out
}

// TestInboundFormIntegerInputsRejectDecimals 守住「入站表单里的整数字段不收小数」。
//
// 端口、并发限制、限速这些字段在 Go 侧是 int：表单提交 1.5，Gin 绑定时 strconv.ParseInt
// 报错，管理员只看到一条看不懂的保存失败。写进 settings / streamSettings 的那些（kcp 的
// mtu、fallback 的 xver、ocspStapling……）在核心里是无符号整数，小数让核心拒绝整份配置。
// a-input-number 加 :precision="0" 会在输入时就把值取整；<a-input type="number"> 做不到，
// 所以表单里的数值框一律用 a-input-number。只有按 GB 填的总流量允许小数。
func TestInboundFormIntegerInputsRejectDecimals(t *testing.T) {
	decimalOK := map[string]bool{"dbInbound.totalGB": true}
	tmpl := parseAllTemplates(t)
	doc, err := html.Parse(strings.NewReader(renderPage(t, tmpl, "inbounds.html")))
	if err != nil {
		t.Fatalf("解析渲染结果: %v", err)
	}
	var modal *html.Node
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if modal == nil && n.Type == html.ElementNode && attrOf(n, "id") == "inbound-modal" {
			modal = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if modal == nil {
		t.Fatal("inbounds.html 里找不到 #inbound-modal，本测试该更新了")
	}
	checked := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			model := attrOf(n, "v-model")
			if model == "" {
				model = attrOf(n, "v-model.number")
			}
			switch {
			case n.Data == "a-input" && attrOf(n, "type") == "number":
				t.Errorf("<a-input type=\"number\" v-model=%q>：换成 <a-input-number :precision=\"0\">，"+
					"原生数值框照样收小数", model)
			case n.Data == "a-input-number" && !decimalOK[model]:
				checked++
				if attrOf(n, ":precision") != "0" {
					t.Errorf("<a-input-number v-model=%q> 没有 :precision=\"0\"：小数会一路提交到服务端或核心", model)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(modal)
	if checked == 0 {
		t.Fatal("#inbound-modal 里一个整数数值框都没找到，本测试该更新了")
	}
}

// 「还在用默认密码」提示条：四个登录后的页面都要能显示，没有标记时一个字都不渲染。
func TestDefaultPasswordBannerOnEveryPage(t *testing.T) {
	tmpl := parseAllTemplates(t)
	for _, page := range []string{"index.html", "inbounds.html", "routing.html", "setting.html"} {
		for _, on := range []bool{true, false} {
			var buf bytes.Buffer
			data := map[string]any{
				"base_path": "/", "cur_ver": "test", "title": "test", "host": "localhost",
				"request_uri": "/aui/" + page, "default_password": on,
			}
			if err := tmpl.ExecuteTemplate(&buf, page, data); err != nil {
				t.Fatalf("渲染 %s: %v", page, err)
			}
			if got := strings.Contains(buf.String(), "default-password-banner"); got != on {
				t.Errorf("%s：default_password=%v 时提示条出现=%v", page, on, got)
			}
		}
	}
}
