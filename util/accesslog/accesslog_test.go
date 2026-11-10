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
	if e.NoRuleHit {
		t.Error("\"->\" 表示命中了路由规则，NoRuleHit 应为 false")
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
	// ">>" 是 xray 的「没有任何规则命中，走默认出站」（app/dispatcher 的 routedDispatch）。
	if !e.NoRuleHit {
		t.Error("\">>\" 表示没有命中任何路由规则，NoRuleHit 应为 true")
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
	if e.NoRuleHit {
		t.Error("首个分隔符是 \"->\"，NoRuleHit 应为 false")
	}
}

// 真实的 rejected 行：核心（proxy/*/server.go 里每一处 log.AccessRejected）只填来源与原因，
// To 恒为空串、也没有 [detour]（Detour 只在 app/dispatcher 的放行路径上才写），所以
// "rejected" 之后是两个空格紧跟原因。认不出属于哪个入站，只能丢弃。
func TestParseLineRejected(t *testing.T) {
	line := `2026/09/03 01:00:00.000000 from 192.0.2.10:51234 rejected  proxy/vless/encoding: invalid request user id`

	if e, ok := ParseLine(line, time.UTC); ok {
		t.Fatalf("rejected 行没有入站信息，不该解析成功，却得到 %+v", e)
	}
}

// 原因里经常带着地址。IPv6 地址的方括号不是路由信息：修复前它被当成了入站 tag，
// 这条记录以 Inbound="2001:db8::5"、inbound_id=0 落库。
func TestParseLineRejectedReasonWithBracketedIPv6IsNotADetour(t *testing.T) {
	for _, line := range []string{
		`2026/09/03 01:00:00.000000 from tcp:[2001:db8::5]:51234 rejected  proxy/socks: failed to read request > read tcp [2001:db8::5]:51234->[2001:db8::1]:1080: i/o timeout`,
		// 原因的第二个词恰好以方括号开头：目标为空时这里不能把原因的第一个词当成目标。
		`2026/09/03 01:00:00.000000 from 192.0.2.10:51234 rejected  common/drain: [2001:db8::5]:443 drained connection`,
	} {
		if e, ok := ParseLine(line, time.UTC); ok {
			t.Errorf("行 %q 不该解析成功，却得到 Inbound=%q Route=%q", line, e.Inbound, e.Route)
		}
	}
}

// 方括号必须紧跟在目标之后（common/log/access.go 的 AccessMessage.String 只写一个空格）。
func TestParseLineRequiresDetourRightAfterTarget(t *testing.T) {
	line := `2026/09/03 01:00:00.000000 from tcp:192.0.2.10:1111 accepted tcp:www.example.com:443 something [inbound-1 -> direct]`
	if e, ok := ParseLine(line, time.UTC); ok {
		t.Errorf("目标后面不是方括号的行不该解析成功，却得到 %+v", e)
	}
}

// http 入站记的是请求的 URL（proxy/http/server.go 的 To: request.URL），不是 host:port：
// CONNECT 是 "//host:port"，普通代理请求是 "http://host/path"。不归一的话域名统计里
// 「http」会成为一个注册域名，按目标的查询与分流验证也都对不上。默认端口与核心一致：
// https 为 443，其余为 80。
func TestParseLineNormalizesHTTPProxyTargets(t *testing.T) {
	for _, c := range []struct{ logged, want string }{
		{"//www.example.com:443", "www.example.com:443"},
		{"http://www.example.com/index.html?q=1", "www.example.com:80"},
		{"http://www.example.com:8080/a/b", "www.example.com:8080"},
		{"https://www.example.com/", "www.example.com:443"},
		{"http://[2001:db8::1]:8080/", "[2001:db8::1]:8080"},
		{"//[2001:db8::1]:443", "[2001:db8::1]:443"},
		{"http://192.0.2.7/", "192.0.2.7:80"},
	} {
		line := `2026/09/03 01:00:00.000000 from 192.0.2.10:51234 accepted ` + c.logged + ` [inbound-1 -> direct]`
		e, ok := ParseLine(line, time.UTC)
		if !ok {
			t.Errorf("%q 解析失败", c.logged)
			continue
		}
		if e.Target != c.want {
			t.Errorf("%q 归一成 %q，期望 %q", c.logged, e.Target, c.want)
		}
		if e.Inbound != "inbound-1" || e.Route != "direct" || e.SourceIP != "192.0.2.10" {
			t.Errorf("%q 的其余字段不对: %+v", c.logged, e)
		}
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
	if truncated, err := tl.TruncateIfLargerThan(5); err != nil || !truncated {
		t.Fatalf("TruncateIfLargerThan = %v, %v，期望截断", truncated, err)
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
	if truncated, err := tl.TruncateIfLargerThan(1 << 20); err != nil || truncated {
		t.Fatalf("TruncateIfLargerThan = %v, %v，没超过阈值不该截断", truncated, err)
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

// Peek 不推进偏移：调用方把读到的行写库成功之后才 Commit，写库失败时下一轮会重读同一段，
// 不会丢。
func TestTailerPeekDoesNotAdvanceUntilCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "line1\nline2\n")

	tl := &Tailer{Path: path}
	lines, next, err := tl.Peek(1 << 20)
	if err != nil || len(lines) != 2 || next != int64(len("line1\nline2\n")) {
		t.Fatalf("Peek = %v, %d, %v", lines, next, err)
	}
	again, _, err := tl.Peek(1 << 20)
	if err != nil || len(again) != 2 {
		t.Fatalf("未 Commit 前再次 Peek 应读到同样两行，得到 %v, %v", again, err)
	}
	tl.Commit(next)
	if rest, _, _ := tl.Peek(1 << 20); len(rest) != 0 {
		t.Fatalf("Commit 之后不应再读到旧行，得到 %v", rest)
	}
}

// 进程重启：新 Tailer 用持久化的偏移与文件首行恢复，只读重启后新增的行。
// 修复前偏移只在内存里，每次重启都把整份日志重新导入一遍。
func TestTailerResumeAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "first\nsecond\n")

	old := &Tailer{Path: path}
	_, next, err := old.Peek(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	old.Commit(next)
	head, err := old.Head()
	if err != nil || head != "first" {
		t.Fatalf("Head = %q, %v，期望 first", head, err)
	}

	append_(t, path, "third\n")
	restarted := &Tailer{Path: path}
	if ok, err := restarted.Resume(next, head); err != nil || !ok {
		t.Fatalf("Resume = %v, %v，期望恢复成功", ok, err)
	}
	lines, _, err := restarted.Peek(1 << 20)
	if err != nil || len(lines) != 1 || lines[0] != "third" {
		t.Fatalf("重启后读到 %v, %v，期望只有 [third]", lines, err)
	}
}

// 文件已经换了（被截断后又写过、或被外部替换）：首行对不上就从头读，宁可重复也不丢。
func TestTailerResumeRejectsDifferentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "new-first\nnew-second\nnew-third\n")

	tl := &Tailer{Path: path}
	if ok, err := tl.Resume(5, "old-first"); err != nil || ok {
		t.Fatalf("Resume = %v, %v，首行不同不应恢复", ok, err)
	}
	if lines, _, _ := tl.Peek(1 << 20); len(lines) != 3 {
		t.Fatalf("未恢复时应从头读，得到 %v", lines)
	}
}

// 文件比记录的偏移还短：同样不恢复（截断判定与 Peek 一致）。
func TestTailerResumeRejectsShorterFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	write(t, path, "first\n")

	tl := &Tailer{Path: path}
	if ok, err := tl.Resume(100, "first"); err != nil || ok {
		t.Fatalf("Resume = %v, %v，文件比偏移短不应恢复", ok, err)
	}
}

// 还没有任何完整行时首行为空串，空串不能当作身份去匹配。
func TestTailerHeadNeedsCompleteLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	tl := &Tailer{Path: path}
	if head, err := tl.Head(); err != nil || head != "" {
		t.Fatalf("文件不存在时 Head = %q, %v，期望空串", head, err)
	}
	write(t, path, "partial")
	if head, err := tl.Head(); err != nil || head != "" {
		t.Fatalf("只有半行时 Head = %q, %v，期望空串", head, err)
	}
	if ok, _ := tl.Resume(0, ""); ok {
		t.Error("空首行不应恢复")
	}
}
