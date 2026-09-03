package accesslog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 下面这些样本全部来自仓库内 Xray 26.7.28 的真实输出，不是照文档臆造的。
// 两处必须留意：来源地址的 "tcp:" 前缀有时有有时无；入站与出站之间的
// 分隔符有 "->" 和 ">>" 两种，命中路由规则与走默认出站各用一种。

func TestParseLineAcceptedWithArrowSeparator(t *testing.T) {
	line := `2026/09/03 01:47:48.214901 from tcp:127.0.0.1:59509 accepted tcp:www.example.com:80 [inbound-39101 -> a-ui-block]`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	want := time.Date(2026, 9, 3, 1, 47, 48, 214901000, time.UTC)
	if !e.Time.Equal(want) {
		t.Errorf("Time = %v，期望 %v", e.Time, want)
	}
	if e.SourceIP != "127.0.0.1" {
		t.Errorf("SourceIP = %q，期望 127.0.0.1", e.SourceIP)
	}
	if e.Network != "tcp" {
		t.Errorf("Network = %q，期望 tcp", e.Network)
	}
	if e.Target != "www.example.com:80" {
		t.Errorf("Target = %q，期望 www.example.com:80", e.Target)
	}
	if e.Inbound != "inbound-39101" {
		t.Errorf("Inbound = %q，期望 inbound-39101", e.Inbound)
	}
	if e.Route != "a-ui-block" {
		t.Errorf("Route = %q，期望 a-ui-block", e.Route)
	}
	if !e.Accepted {
		t.Error("Accepted = false，期望 true")
	}
}

func TestParseLineAcceptedWithDoubleAngleSeparator(t *testing.T) {
	line := `2026/09/03 01:47:45.266611 from tcp:127.0.0.1:59495 accepted tcp:1.1.1.1:443 [inbound-39101 >> direct]`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	if e.Inbound != "inbound-39101" || e.Route != "direct" {
		t.Errorf("Inbound/Route = %q/%q，期望 inbound-39101/direct", e.Inbound, e.Route)
	}
}

func TestParseLineWithoutNetworkPrefixOnSource(t *testing.T) {
	// 实测这一行的来源地址没有 "tcp:" 前缀。
	line := `2026/09/02 22:43:15.584934 from 127.0.0.1:60766 accepted tcp:www.bing.com:443 [inbound-39010 >> direct] email: u1@a-ui`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	if e.SourceIP != "127.0.0.1" {
		t.Errorf("SourceIP = %q，期望 127.0.0.1", e.SourceIP)
	}
	if e.Target != "www.bing.com:443" {
		t.Errorf("Target = %q，期望 www.bing.com:443", e.Target)
	}
	if e.Email != "u1@a-ui" {
		t.Errorf("Email = %q，期望 u1@a-ui", e.Email)
	}
}

func TestParseLineIPv6Source(t *testing.T) {
	line := `2026/09/03 01:00:00.000000 from tcp:[2408:8207::1]:51234 accepted tcp:example.org:443 [inbound-39101 >> direct]`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	if e.SourceIP != "2408:8207::1" {
		t.Errorf("SourceIP = %q，期望 2408:8207::1（要去掉方括号，否则和在线明细里的 IP 对不上）", e.SourceIP)
	}
}

func TestParseLineUDP(t *testing.T) {
	line := `2026/09/03 01:00:00.000000 from udp:10.0.0.9:5353 accepted udp:8.8.8.8:53 [inbound-39101 >> direct]`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	if e.Network != "udp" {
		t.Errorf("Network = %q，期望 udp", e.Network)
	}
}

func TestParseLineThreeSegmentDetour(t *testing.T) {
	// 经过中间出站时 xray 会输出三段。取第一段作为入站，其余整体作为路由，
	// 不能只取最后一段——那样会丢掉"经过了谁"这个信息。
	line := `2026/09/03 01:00:00.000000 from tcp:1.2.3.4:1111 accepted tcp:a.com:443 [inbound-1 -> proxy-hk -> direct]`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	if e.Inbound != "inbound-1" {
		t.Errorf("Inbound = %q，期望 inbound-1", e.Inbound)
	}
	if e.Route != "proxy-hk -> direct" {
		t.Errorf("Route = %q，期望 proxy-hk -> direct", e.Route)
	}
}

func TestParseLineRejected(t *testing.T) {
	line := `2026/09/03 01:00:00.000000 from tcp:1.2.3.4:1111 rejected tcp:a.com:443 [inbound-1] some error`

	e, ok := ParseLine(line, time.UTC)
	if !ok {
		t.Fatal("解析失败")
	}
	if e.Accepted {
		t.Error("Accepted = true，rejected 行应为 false")
	}
	if e.Inbound != "inbound-1" {
		t.Errorf("Inbound = %q，期望 inbound-1", e.Inbound)
	}
}

func TestParseLineRejectsGarbage(t *testing.T) {
	for _, line := range []string{
		"",
		"   ",
		"2026/09/03 01:00:00.000000 [Warning] core: Xray 26.7.28 started",
		"from tcp:1.2.3.4:1 accepted tcp:a.com:443 [inbound-1 >> direct]",      // 缺时间
		"2026/09/03 01:00:00.000000 from tcp:1.2.3.4:1 accepted tcp:a.com:443", // 缺方括号
		"not a log line at all",
	} {
		if e, ok := ParseLine(line, time.UTC); ok {
			t.Errorf("行 %q 不该解析成功，却得到 %+v", line, e)
		}
	}
}

func TestTailerReadsOnlyCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "line1\nline2\npartial")

	tl := &Tailer{Path: path}
	lines, err := tl.Read(1 << 20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 2 || lines[0] != "line1" || lines[1] != "line2" {
		t.Fatalf("读到 %v，期望只读走两条完整行", lines)
	}

	// 补完那一行后应当能读到完整的第三行，且不重复前两行。
	append_(t, path, " tail\n")
	lines, err = tl.Read(1 << 20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 1 || lines[0] != "partial tail" {
		t.Fatalf("读到 %v，期望 [partial tail]", lines)
	}
}

func TestTailerHandlesTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "aaaa\nbbbb\n")

	tl := &Tailer{Path: path}
	if _, err := tl.Read(1 << 20); err != nil {
		t.Fatalf("Read: %v", err)
	}

	// 文件被截断（我们自己截的，或者管理员清了日志）后，偏移必须回到 0，
	// 否则会一直读不到新内容，日志采集静默停摆。
	write(t, path, "cccc\n")
	lines, err := tl.Read(1 << 20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 1 || lines[0] != "cccc" {
		t.Fatalf("截断后读到 %v，期望 [cccc]", lines)
	}
}

func TestTailerMissingFileIsNotAnError(t *testing.T) {
	tl := &Tailer{Path: filepath.Join(t.TempDir(), "never-created.log")}
	lines, err := tl.Read(1 << 20)
	if err != nil {
		t.Errorf("文件还没被 xray 创建出来不算错误，得到 %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("读到 %v，期望空", lines)
	}
}

func TestTailerCapsReadSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	var big string
	for i := 0; i < 1000; i++ {
		big += "0123456789\n" // 11 字节一行
	}
	write(t, path, big)

	tl := &Tailer{Path: path}
	lines, err := tl.Read(110) // 只允许读 10 行
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 10 {
		t.Fatalf("读到 %d 行，期望被上限截到 10 行——不设上限会把整个日志一次性读进内存", len(lines))
	}
	// 下一轮要从断点继续，不能重复也不能跳过。
	lines, err = tl.Read(110)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 10 {
		t.Fatalf("第二轮读到 %d 行，期望 10", len(lines))
	}
}

func TestTailerTruncateResetsOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "aaaa\nbbbb\n")

	tl := &Tailer{Path: path}
	if _, err := tl.Read(1 << 20); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := tl.TruncateIfLargerThan(5); err != nil {
		t.Fatalf("TruncateIfLargerThan: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Errorf("文件大小 = %d，期望被截成 0", st.Size())
	}
	append_(t, path, "cccc\n")
	lines, err := tl.Read(1 << 20)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) != 1 || lines[0] != "cccc" {
		t.Fatalf("截断后读到 %v，期望 [cccc]", lines)
	}
}

func TestTailerDoesNotTruncateSmallFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "aaaa\n")
	tl := &Tailer{Path: path}
	if _, err := tl.Read(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := tl.TruncateIfLargerThan(1 << 20); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Size() == 0 {
		t.Error("文件没超过阈值却被截断了")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func append_(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}
