package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"regexp"
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
		"request_uri": "/xui/" + name,
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
