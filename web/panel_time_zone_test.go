package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// 给人看的时间一律按面板时区，换算集中在 web/assets/js/util/date-util.js。
// 下面几条守的是「绕开 DateUtil」这一类静默失效：绕开的地方照样渲染出一个
// 看上去合理的时间，只是按浏览器所在机器的时区，与同一页上服务端生成的曲线
// 刻度差出整数个小时，而没有任何一层会报错。

const dateUtilPath = "assets/js/util/date-util.js"

// 裸的 moment()（不带参数）不在禁止之列：分流导出的文件名刻意用管理员自己
// 电脑上的下载时刻。
var (
	rawMomentFormat = regexp.MustCompile(`\bmoment\(\s*[^)\s]`)
	rawValueOf      = regexp.MustCompile(`\.valueOf\(\)`)
)

func TestFrontendTimesGoThroughDateUtil(t *testing.T) {
	check := func(fsys fs.FS, root, ext string) {
		err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ext) || path == dateUtilPath {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				if rawMomentFormat.MatchString(line) {
					t.Errorf("%s:%d 直接用 moment(…) 处理时间，会按浏览器时区显示。"+
						"显示改用 DateUtil.formatMillis，交给日期选择器改用 DateUtil.toPanelMoment。", path, i+1)
				}
				if rawValueOf.MatchString(line) {
					t.Errorf("%s:%d 直接 valueOf() 取毫秒。日期选择器给出的 moment 本地字段是面板时区的钟面时间，"+
						"必须经 DateUtil.fromPanelMoment 换算，否则存进去的时刻差出时区差。", path, i+1)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", root, err)
		}
	}
	check(htmlFS, "html", ".html")
	check(assetsFS, "assets/js", ".js")
}

// 带时间的日期选择器默认有个「此刻」按钮，它给出浏览器本地的 moment()，会被
// DateUtil.fromPanelMoment 当成面板时区的钟面时间——一点就差出时区差。
func TestDatePickersHideNowButton(t *testing.T) {
	err := fs.WalkDir(htmlFS, "html", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		data, err := fs.ReadFile(htmlFS, path)
		if err != nil {
			return err
		}
		src := string(data)
		for rest, offset := src, 0; ; {
			i := strings.Index(rest, "<a-date-picker")
			if i < 0 {
				break
			}
			end := strings.Index(rest[i:], ">")
			if end < 0 {
				t.Fatalf("%s: <a-date-picker 没有闭合的 >", path)
			}
			tag := rest[i : i+end+1]
			if !strings.Contains(tag, `:show-today="false"`) {
				line := strings.Count(src[:offset+i], "\n") + 1
				t.Errorf("%s:%d 的 a-date-picker 缺少 :show-today=\"false\"", path, line)
			}
			offset += i + end + 1
			rest = rest[i+end+1:]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历模板: %v", err)
	}
}

// date-util.js 在加载时不读 panelTimeZone，但它的函数第一次被调用时就要读到；
// 定义挪到它后面去，最早渲染的那批时间会缓存下「时区不可用」而整页回落本地时区。
func TestPanelTimeZoneDefinedBeforeDateUtil(t *testing.T) {
	data, err := htmlFS.ReadFile("html/common/js.html")
	if err != nil {
		t.Fatalf("读取 common/js.html: %v", err)
	}
	src := string(data)
	def := strings.Index(src, "const panelTimeZone = '{{ .time_zone }}'")
	// 按脚本路径找，不按文件名：定义旁边的注释里就提到了 date-util.js。
	util := strings.Index(src, "assets/js/util/date-util.js")
	if def < 0 {
		t.Fatal("common/js.html 里没有定义 panelTimeZone，DateUtil 会整体回落到浏览器本地时区")
	}
	if util < 0 || def > util {
		t.Fatal("panelTimeZone 必须定义在 date-util.js 之前")
	}
}
