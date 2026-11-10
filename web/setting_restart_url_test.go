package web

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 设置页「重启面板」之后跳转的地址（panelUrlAfterRestart）。曾经只要地址栏里带端口就换成面板端口：经 SSH
// 隧道（-L 9000:127.0.0.1:54321）、Docker 端口映射、8443 上的反代访问的人，重启后被送到一个打不开的地址。
// 只有地址里的端口就是这次连接实际到达的面板端口（直连）才换——不能用库里存的端口：先保存新端口、刷新页面、
// 再点重启时，库里已是新端口，面板还在旧端口上，那样会把直连的人留在旧端口。用 node 跑模板里这个方法本身，没有 node 时跳过。
func TestPanelUrlAfterRestartKeepsForeignPorts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node")
	}
	b, err := os.ReadFile("html/xui/setting.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "panelUrlAfterRestart() {")
	if start < 0 {
		t.Fatal("setting.html 里找不到 panelUrlAfterRestart")
	}
	end := strings.Index(s[start:], "\n            },")
	method := s[start : start+end+len("\n            }")]
	script := `
const PANEL_BASE_PATH = '/abc/';
let PANEL_LOCAL_PORT = '';
const obj = { ` + method + ` };
function run(href, localPort, saved) {
  global.location = { href };
  PANEL_LOCAL_PORT = localPort;
  return obj.panelUrlAfterRestart.call(Object.assign({ oldAllSetting: saved }, obj));
}
const saved = { webPort: 54321, webBasePath: '/abc/', webCertFile: '', webKeyFile: '' };
const cases = [
  ['http://203.0.113.5:54321/abc/aui/setting', '54321', Object.assign({}, saved, { webPort: 60000 }), 'http://203.0.113.5:60000/abc/aui/setting'],
  ['http://127.0.0.1:9000/abc/aui/setting', '54321', saved, 'http://127.0.0.1:9000/abc/aui/setting'],
  ['https://example.com:8443/abc/aui/setting', '54321', saved, 'https://example.com:8443/abc/aui/setting'],
  ['https://example.com/abc/aui/setting', '54321', Object.assign({}, saved, { webBasePath: '/xyz/' }), 'https://example.com/xyz/aui/setting'],
  // 改端口保存后刷新过页面：库里已是 60000，面板还在 54321 上（连接到达的端口），照样换。
  ['http://203.0.113.5:54321/abc/aui/setting', '54321', Object.assign({}, saved, { webPort: 60000 }), 'http://203.0.113.5:60000/abc/aui/setting'],
];
for (const [href, atLoad, sv, want] of cases) {
  const got = run(href, atLoad, sv);
  if (got !== want) { console.error(href + ' -> ' + got + ' want ' + want); process.exit(1); }
}
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
