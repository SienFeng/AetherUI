package web

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件守住两条无法被 go build 或模板测试发现的资源层不变量。

const antdCSSPath = "assets/ant-design-vue@1.7.2/antd.min.css"

// antd 1.7.2 出厂的主色调色板。这份 CSS 是 Less 编译产物，而本项目没有构建链，
// 改主色的唯一办法就是直接改这份 vendored 文件（v1.x.x 起，302 处）。
//
// 代价是它不会自愈：哪天有人重新下载一份 antd 1.7.2 覆盖上去，全站配色会
// 整体退回 #1890FF，而**没有任何一层会报错**——页面照常渲染、测试照常通过、
// 面板照常可用，只是 primary 按钮的白字重新掉回 3.24:1（不达 WCAG AA），
// 而且会和登录页的 #1266D6 变成两种蓝并存。这条测试就是为了把那个静默失效
// 变成一次响亮的失败。
//
// 如果将来确实要换主色，改的是下面 wantPrimary 与替换脚本，不是删掉这条测试。
var antdStockPalette = map[string]string{
	"#1890ff":         "primary-6 基色",
	"#40a9ff":         "primary-5 hover",
	"#096dd9":         "primary-7 active",
	"#91d5ff":         "primary-4 边框",
	"#bae7ff":         "primary-3 浅边框",
	"#e6f7ff":         "primary-1 浅底",
	"rgba(24,144,255": "焦点环 rgba 形式",
}

// wantPrimary 必须与 web/assets/css/custom.css 里的 --aui-primary 一致。
const wantPrimary = "#1266d6"

func TestAntdPrimaryPaletteIsRecolored(t *testing.T) {
	raw, err := assetsFS.ReadFile(antdCSSPath)
	if err != nil {
		t.Fatalf("读取 %s: %v", antdCSSPath, err)
	}
	css := string(raw)

	for stock, role := range antdStockPalette {
		if n := strings.Count(css, stock); n != 0 {
			t.Errorf("%s 里仍有 %d 处 antd 出厂色 %s（%s）。\n"+
				"多半是有人重新下载了一份 antd 覆盖上去——那会让全站配色静默退回，"+
				"primary 按钮白字重新掉到 3.24:1，且与登录页的 %s 变成两种蓝并存。\n"+
				"重做替换，不要改这条测试。", antdCSSPath, n, stock, role, wantPrimary)
		}
	}

	if !strings.Contains(css, wantPrimary) {
		t.Errorf("%s 里找不到新主色 %s，主色覆盖没有生效", antdCSSPath, wantPrimary)
	}
}

var stylesheetLinkPattern = regexp.MustCompile(`<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"`)

// TestStylesheetsAreCacheBusted 守住「样式表 URL 必须带版本号」。
//
// web.go 给 assets 下的一切发 Cache-Control: max-age=31536000，而缓存键只能
// 靠 URL 上的 ?<版本号>。少带一个的后果不是「改了下次生效」，而是**一年之内
// 都不生效**：存量部署的浏览器会一直用手里那份旧 CSS。
//
// antd.min.css 此前就是这样——它从来没带过版本号，因为在主色改到这份文件里
// 之前，它确实从未变过。这条测试把「哪天又往 vendored 样式表里写东西」这件事
// 和缓存绑在一起，不必再指望有人记得。
func TestStylesheetsAreCacheBusted(t *testing.T) {
	tmpl := parseAllTemplates(t)
	for _, page := range topLevelPages {
		page := page
		t.Run(page, func(t *testing.T) {
			rendered := renderPage(t, tmpl, page)
			matches := stylesheetLinkPattern.FindAllStringSubmatch(rendered, -1)
			if len(matches) == 0 {
				t.Fatalf("%s 里一个 <link rel=\"stylesheet\"> 都没找到，"+
					"要么页面结构变了，要么正则该更新了", page)
			}
			for _, m := range matches {
				href := m[1]
				if !strings.Contains(href, "assets/") {
					continue // 外链样式表不归这条管（本项目目前一个都没有）
				}
				if !strings.Contains(href, "?") {
					t.Errorf("%s 引用的 %s 没带版本号。\n"+
						"assets 下的一切都是 max-age=31536000 的强缓存，"+
						"不带版本号意味着存量部署一年内都拿不到新内容。\n"+
						"在 href 末尾补上 ?{{ .cur_ver }}。", page, href)
				}
			}
		})
	}
}
