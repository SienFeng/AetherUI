package web

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDesignTokenContrast 守住设计 Token 的 WCAG 对比度。
//
// custom.css 的 :root 里每个色都在注释里标了实测比值，但注释拦不住任何东西：
// 某次「把这个灰调浅一点点」之后，比值悄悄跌破 4.5，而页面照常渲染、测试
// 照常通过，只有色弱用户和小屏用户读不出来——这正是本次改造要修的那类问题
// （改动前 .cell-sub 用的 rgba(0,0,0,.45) 只有 3.36:1，antd 出厂主色的白字
// 只有 3.24:1，两者都在代码里躺了很久）。
//
// 这里把「哪个前景色画在哪个背景上」写成可执行的断言。改 Token 时这条会
// 先红，要么调回去，要么连同这张表一起更新并说明理由。
func TestDesignTokenContrast(t *testing.T) {
	tokens := readDesignTokens(t)

	cases := []struct {
		what    string
		fg, bg  string // Token 名，或直接写 #RRGGBB
		minimum float64
	}{
		// 正文与次要文字：SC 1.4.3 对普通字号要求 4.5:1
		{"正文", "--aui-text", "--aui-surface", 4.5},
		{"次要正文", "--aui-text-2", "--aui-surface", 4.5},
		{"辅助文字（白底）", "--aui-text-3", "--aui-surface", 4.5},
		// 表格底是 antd 的 #fafafa，比纯白暗一档，辅助文字在这里最吃紧
		{"辅助文字（表格底）", "--aui-text-3", "#fafafa", 4.5},
		{"页面描述（页面底）", "--aui-text-2", "--aui-bg", 4.5},

		// 主操作按钮的三个状态都是白字，三档都要过
		{"主按钮白字", "#ffffff", "--aui-primary", 4.5},
		{"主按钮白字 hover", "#ffffff", "--aui-primary-hover", 4.5},
		{"主按钮白字 active", "#ffffff", "--aui-primary-active", 4.5},

		// 语义色当文字用（到期、错误提示等）
		{"成功文字", "--aui-success", "--aui-surface", 4.5},
		{"警告文字", "--aui-warning", "--aui-surface", 4.5},
		{"危险文字", "--aui-danger", "--aui-surface", 4.5},

		// 状态徽标：文字画在自己那档浅底上，不是白底
		{"徽标 ok", "--aui-success", "#f0fdf4", 4.5},
		{"徽标 warn", "--aui-warning", "#fffbeb", 4.5},
		{"徽标 danger", "--aui-danger", "#fef2f2", 4.5},
		{"徽标 neutral", "--aui-text-2", "#f1f5f9", 4.5},
		{"chip 文字", "--aui-text-2", "--aui-surface-soft", 4.5},

		// 非文本的 UI 部件按 SC 1.4.11 的 3:1
		{"进度条填充对槽", "--aui-primary", "--aui-line", 3.0},
		{"焦点环对白底", "--aui-primary", "--aui-surface", 3.0},
	}

	for _, c := range cases {
		fg := resolveColor(t, tokens, c.fg)
		bg := resolveColor(t, tokens, c.bg)
		got := contrastRatio(fg, bg)
		if got < c.minimum {
			t.Errorf("%s：%s(%s) 画在 %s(%s) 上只有 %.2f:1，低于 %.1f:1。\n"+
				"调回去，或者连同本表与 custom.css 的注释一起更新并说明理由。",
				c.what, c.fg, fg, c.bg, bg, got, c.minimum)
		}
	}
}

var tokenPattern = regexp.MustCompile(`(--aui-[\w-]+):\s*(#[0-9a-fA-F]{6})\s*;`)

func readDesignTokens(t *testing.T) map[string]string {
	t.Helper()
	raw, err := assetsFS.ReadFile("assets/css/custom.css")
	if err != nil {
		t.Fatalf("读取 custom.css: %v", err)
	}
	out := map[string]string{}
	for _, m := range tokenPattern.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = strings.ToLower(m[2])
	}
	if len(out) == 0 {
		t.Fatal("custom.css 里一个 --aui-* 颜色 Token 都没解析到，" +
			"要么 :root 块没了，要么 tokenPattern 该更新了")
	}
	return out
}

func resolveColor(t *testing.T, tokens map[string]string, ref string) string {
	t.Helper()
	if strings.HasPrefix(ref, "#") {
		return strings.ToLower(ref)
	}
	v, ok := tokens[ref]
	if !ok {
		t.Fatalf("custom.css 的 :root 里没有 %s（它可能被改名或删掉了）", ref)
	}
	return v
}

// relativeLuminance 按 WCAG 2.1 的定义算相对亮度。
func relativeLuminance(hex string) float64 {
	h := strings.TrimPrefix(hex, "#")
	ch := func(i int) float64 {
		v, err := strconv.ParseInt(h[i:i+2], 16, 0)
		if err != nil {
			panic(fmt.Sprintf("颜色 %q 不是 #RRGGBB", hex))
		}
		x := float64(v) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(0) + 0.7152*ch(2) + 0.0722*ch(4)
}

func contrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05)
}
