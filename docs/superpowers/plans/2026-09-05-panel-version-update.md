# 面板版本显示与一键更新 / 回退 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在侧边栏底部常驻显示面板版本号，有新版时打红点，点击可一键更新到最新版或回退到最近 5 个版本。

**Architecture:** 后端新增一个 `PanelVersionService`，用包级变量 + `sync.RWMutex` 缓存 GitHub releases 列表，由一个 6 小时的 cron job 刷新。更新执行不由面板自己做，而是用 `systemd-run` 起一个独立的 transient unit 去跑 `install.sh <tag>`——面板 `os/exec` 的子进程与面板同在一个 cgroup，而 `install.sh` 会 `systemctl stop a-ui`，`KillMode=control-group` 会把更新脚本一起杀掉。前端用一个 Vue mixin 把版本区注入四个共用侧边栏的页面。

**Tech Stack:** Go 1.27 / Gin 1.7.1 / GORM+SQLite / Vue 2.6.12 + ant-design-vue 1.7.2（无打包工具，服务端模板）

**Spec:** `docs/superpowers/specs/2026-09-05-panel-version-update-design.md`

## Global Constraints

- 构建必须开 CGO：`export CGO_ENABLED=1`（`gorm.io/driver/sqlite` 依赖 `mattn/go-sqlite3`）。
- 提交前门禁：`make verify`（= `go vet ./...` + `go test ./...` + `go build`）。
- 本仓库测试只用标准库 `testing`，**不引入任何断言库**。断言写法照抄 `web/service/routing_rule_test.go`：`if got != want { t.Errorf(...) }`。
- `web/service` 包的 `TestMain` 已在 `routing_validate_test.go:21` 定义并 `os.Chdir` 到仓库根。**不要再写第二个 `TestMain`**（同包重复定义会编译失败）。
- 新增 job 的 `Run()` 首行必须是 `defer common.Recover("<任务名>")`（`util/common/err.go`）。
- 错误一律用 `common.NewError(...)` / `common.NewErrorf(format, ...)`，不用 `fmt.Errorf`。`NewError` 走 `fmt.Sprintln`，参数之间会插空格；要精确控制文案时用 `NewErrorf`。
- 日志用 `a-ui/logger` 的 `Debug/Info/Warning/Error`。
- controller 响应一律走 `jsonMsg` / `jsonObj`（`web/controller/util.go`）。
- 改动 `web/html/**` 后必须跑 `go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot'`——`web.go` 的 `getHtmlTemplate` 吞掉 `ParseFS` 错误，`go build` 发现不了模板语法错误。
- 改动 `web/assets/js/**` 后本地必须用 `XUI_DEBUG=true go run main.go` 验证，否则浏览器会命中 `max-age=31536000` 的强缓存拿到旧文件。
- 模板里 Vue 插值分隔符是 `[[ ]]`（避开 Go 模板的 `{{ }}`）。
- **所有 Vue 指令（`v-*` / `@*` / `:*`）必须写在 `<a-layout id="app">` 内部。** Vue 2 只编译 `el` 指向的子树，写在外面的指令是完全静默的死代码。
- 仓库地址硬编码为 `SienFeng/AetherUI`。

---

## File Structure

新增：

| 文件 | 职责 |
|---|---|
| `web/service/panel_version.go` | 版本判定纯逻辑、GitHub 拉取、缓存、更新执行、日志读取 |
| `web/service/panel_version_test.go` | 上述全部的单测 |
| `web/job/panel_version_job.go` | 6 小时刷新一次缓存 |
| `web/assets/js/util/panel-version.js` | Vue mixin，供四个页面复用 |

修改：

| 文件 | 改动 |
|---|---|
| `web/controller/server.go` | 4 个新路由与 handler |
| `web/web.go` | `startTask` 注册 job + 延迟 10 秒的首次触发 |
| `web/html/xui/common_sider.html` | 侧栏底部版本区 + popover |
| `web/html/common/js.html` | 引入 `panel-version.js` |
| `web/assets/css/custom.css` | 版本区贴底样式 |
| `web/html/xui/index.html` / `inbounds.html` / `routing.html` / `setting.html` | `new Vue` 挂 mixin |
| `CLAUDE.md` | 新增小节 |

---

### Task 1: 版本判定与命令构造的纯逻辑

这一整块不碰网络、不碰文件系统、不碰数据库，全部是纯函数，因此可以先把最容易出错的判定逻辑用测试钉死。

**Files:**
- Create: `web/service/panel_version.go`
- Test: `web/service/panel_version_test.go`

**Interfaces:**
- Consumes: `a-ui/config`（`config.GetVersion()`）、`a-ui/util/common`
- Produces:
  - `type ReleaseBrief struct { TagName string; PublishedAt int64; HtmlUrl string }`
  - `type PanelVersionInfo struct { ... }`（见下方代码）
  - `func toBriefs(raw []githubRelease) []ReleaseBrief`
  - `func computeVersionState(current string, all []ReleaseBrief) (latest string, hasUpdate bool, knownCurrent bool)`
  - `func validateUpgradeTag(tag string, allowed []ReleaseBrief) error`
  - `func buildUpgradeCommand(tag, unitName string) []string`
  - `func checkUpdatable() (bool, string)`
  - 包级可打桩变量 `panelBinaryPath`、`panelServicePath`

- [ ] **Step 1: 写失败的测试**

创建 `web/service/panel_version_test.go`：

```go
package service

import (
	"runtime"
	"strings"
	"testing"
)

func briefs(tags ...string) []ReleaseBrief {
	list := make([]ReleaseBrief, 0, len(tags))
	for _, t := range tags {
		list = append(list, ReleaseBrief{TagName: t})
	}
	return list
}

func TestToBriefsFiltersDraftAndPrerelease(t *testing.T) {
	raw := []githubRelease{
		{TagName: "v1.5.0", PublishedAt: "2026-09-05T04:08:48Z", HtmlUrl: "u1"},
		{TagName: "v1.6.0-rc1", PublishedAt: "2026-09-05T05:00:00Z", Prerelease: true},
		{TagName: "v1.7.0", PublishedAt: "2026-09-05T06:00:00Z", Draft: true},
		{TagName: "v1.4.1", PublishedAt: "2026-09-05T01:04:22Z", HtmlUrl: "u2"},
	}
	got := toBriefs(raw)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].TagName != "v1.5.0" || got[1].TagName != "v1.4.1" {
		t.Errorf("顺序或内容不对: %+v", got)
	}
	// 2026-09-05T04:08:48Z 的 Unix 毫秒
	if got[0].PublishedAt == 0 {
		t.Error("PublishedAt 未解析成 Unix 毫秒")
	}
	if got[0].HtmlUrl != "u1" {
		t.Errorf("HtmlUrl = %q", got[0].HtmlUrl)
	}
}

func TestToBriefsToleratesUnparsablePublishedAt(t *testing.T) {
	// 时间解析失败不能让整条 release 消失——版本列表比发布日期重要得多
	got := toBriefs([]githubRelease{{TagName: "v1.5.0", PublishedAt: "not-a-time"}})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].PublishedAt != 0 {
		t.Errorf("PublishedAt = %d, want 0", got[0].PublishedAt)
	}
}

func TestComputeVersionStateAlreadyLatest(t *testing.T) {
	latest, hasUpdate, known := computeVersionState("v1.5.0", briefs("v1.5.0", "v1.4.1", "v1.4.0"))
	if latest != "v1.5.0" {
		t.Errorf("latest = %q", latest)
	}
	if hasUpdate {
		t.Error("hasUpdate = true, want false")
	}
	if !known {
		t.Error("knownCurrent = false, want true")
	}
}

func TestComputeVersionStateHasUpdate(t *testing.T) {
	latest, hasUpdate, known := computeVersionState("v1.4.0", briefs("v1.5.0", "v1.4.1", "v1.4.0"))
	if latest != "v1.5.0" {
		t.Errorf("latest = %q", latest)
	}
	if !hasUpdate {
		t.Error("hasUpdate = false, want true")
	}
	if !known {
		t.Error("knownCurrent = false, want true")
	}
}

// 本地开发时 config/version 是 0.3.2，不在任何发布列表里。
// 这种情况既不能冒充「有更新」，也不能谎报「已是最新」。
func TestComputeVersionStateUnknownCurrent(t *testing.T) {
	latest, hasUpdate, known := computeVersionState("0.3.2", briefs("v1.5.0", "v1.4.1"))
	if latest != "v1.5.0" {
		t.Errorf("latest = %q", latest)
	}
	if hasUpdate {
		t.Error("hasUpdate = true, want false —— 版本不在列表里时不该打红点")
	}
	if known {
		t.Error("knownCurrent = true, want false")
	}
}

func TestComputeVersionStateEmptyList(t *testing.T) {
	latest, hasUpdate, known := computeVersionState("v1.5.0", nil)
	if latest != "" || hasUpdate || known {
		t.Errorf("空列表应返回零值, got (%q, %v, %v)", latest, hasUpdate, known)
	}
}

// tag 会被拼进 bash -c 的字符串，这是一条 root 权限的命令注入路径。
func TestValidateUpgradeTagRejectsInjection(t *testing.T) {
	allowed := briefs("v1.5.0", "v1.4.1")
	bad := []string{
		"v1.0.0; rm -rf /",
		"v1.0.0 && curl evil.sh | bash",
		"$(whoami)",
		"`id`",
		"v1.0.0\nrm -rf /",
		"../../etc/passwd",
		"",
	}
	for _, tag := range bad {
		if err := validateUpgradeTag(tag, allowed); err == nil {
			t.Errorf("tag %q 应被拒绝", tag)
		}
	}
}

// 格式合法但不在发布列表里，同样必须拒绝——白名单是第二道防线，
// 也是「只能回退到最近 5 个版本」这条约束的实现方式。
func TestValidateUpgradeTagRejectsUnlistedTag(t *testing.T) {
	if err := validateUpgradeTag("v9.9.9", briefs("v1.5.0", "v1.4.1")); err == nil {
		t.Error("不在列表中的 tag 应被拒绝")
	}
}

// 缓存为空（从未成功检查过）时任何 tag 都拒绝：连版本列表都拉不到的网络，
// 更新也必然失败在下载那一步，早拒绝比在 systemd unit 里失败可读得多。
func TestValidateUpgradeTagRejectsWhenListEmpty(t *testing.T) {
	if err := validateUpgradeTag("v1.5.0", nil); err == nil {
		t.Error("列表为空时应拒绝")
	}
}

func TestValidateUpgradeTagAcceptsListed(t *testing.T) {
	if err := validateUpgradeTag("v1.4.1", briefs("v1.5.0", "v1.4.1")); err != nil {
		t.Errorf("合法 tag 被拒: %v", err)
	}
}

func TestBuildUpgradeCommandShape(t *testing.T) {
	argv := buildUpgradeCommand("v1.4.1", "a-ui-update-123")
	if argv[0] != "systemd-run" {
		t.Fatalf("argv[0] = %q, want systemd-run", argv[0])
	}
	joined := strings.Join(argv, " ")
	// --collect：unit 退出后自动清理，否则失败的 unit 会以 failed 状态残留
	if !strings.Contains(joined, "--collect") {
		t.Error("缺少 --collect")
	}
	if !strings.Contains(joined, "--unit=a-ui-update-123") {
		t.Error("缺少 --unit")
	}
	// </dev/null 让 install.sh 的 read -p 立即返回非零、走不写库的分支
	if !strings.Contains(joined, "</dev/null") {
		t.Error("缺少 </dev/null —— install.sh 的 read -p 会挂住")
	}
	// curl -f：HTTP 错误码时返回非零，否则 404 页面会被当成脚本执行
	if !strings.Contains(joined, "curl -fLso") {
		t.Error("curl 缺少 -f")
	}
	if !strings.Contains(joined, "v1.4.1") {
		t.Error("命令里没有目标版本")
	}
	// 日志用 > 覆盖而不是 >> 追加，只保留最近一次
	if strings.Contains(joined, ">>"+upgradeLogPath) {
		t.Error("更新日志应覆盖写，不应追加")
	}
	if !strings.Contains(joined, upgradeLogPath) {
		t.Error("没有把输出重定向到更新日志")
	}
}

func TestCheckUpdatableReportsMissingBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		// 非 Linux 上第一道检查就会拦下，测不到这一条
		ok, reason := checkUpdatable()
		if ok {
			t.Error("非 Linux 系统不该判定为可更新")
		}
		if !strings.Contains(reason, runtime.GOOS) {
			t.Errorf("原因里应点明当前系统: %q", reason)
		}
		return
	}
	origBin := panelBinaryPath
	defer func() { panelBinaryPath = origBin }()
	panelBinaryPath = "/nonexistent/a-ui"
	ok, reason := checkUpdatable()
	if ok {
		t.Error("面板二进制不存在时不该判定为可更新")
	}
	if !strings.Contains(reason, "/nonexistent/a-ui") {
		t.Errorf("原因里应点明具体路径，便于管理员排查: %q", reason)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestToBriefs|TestComputeVersionState|TestValidateUpgradeTag|TestBuildUpgradeCommand|TestCheckUpdatable' -v
```

预期：编译失败，`undefined: ReleaseBrief` 等。

- [ ] **Step 3: 写最小实现**

创建 `web/service/panel_version.go`：

```go
package service

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"time"

	"a-ui/util/common"
)

// 这几个都抽成变量而不是常量，为的是单测能打桩。
var (
	// panelReleasesURL 拉 10 条而不是 5 条：回退列表只给前 5 条，但
	// KnownCurrent 用全部 10 条判定。落后 6~10 个版本的管理员恰恰是最需要
	// 看到红点的人，用 5 条判定会把他们归进「未在发布列表中」而不给任何提示。
	panelReleasesURL = "https://api.github.com/repos/SienFeng/AetherUI/releases?per_page=10"
	// installScriptURL 与 a-ui.sh 的 update() 用的是同一个地址。
	installScriptURL = "https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh"
	panelBinaryPath  = "/usr/local/a-ui/a-ui"
	panelServicePath = "/etc/systemd/system/a-ui.service"
)

const (
	upgradeScriptPath = "/tmp/a-ui-install.sh"
	upgradeLogPath    = "/var/log/a-ui-update.log"
	// rollbackListSize 是回退列表长度，也是 tag 白名单的长度——
	// 「只能回退到最近 5 个版本」这条约束就是靠白名单只有 5 条实现的。
	rollbackListSize = 5
)

// tagPattern 是 tag 的第一道防线。tag 会被拼进 bash -c 的字符串并以 root
// 执行，所以这里与 routing_validate.go 的 fail open 取向**相反**：那里放行
// 的是「没法证明非法」的配置，最坏后果是 xray 拒绝启动；这里放行的是一段
// 会以 root 跑的字符串，最坏后果是任意命令执行。
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var panelVersionHTTPClient = &http.Client{Timeout: 20 * time.Second}

// githubRelease 是 GitHub releases 接口的原始形状，只取用得上的字段。
type githubRelease struct {
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	HtmlUrl     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
}

type ReleaseBrief struct {
	TagName     string `json:"tagName"`
	PublishedAt int64  `json:"publishedAt"` // Unix 毫秒，0 表示解析失败
	HtmlUrl     string `json:"htmlUrl"`
}

type PanelVersionInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"hasUpdate"`
	// KnownCurrent 为 false 表示当前版本不在拉回来的发布列表里（本地开发版，
	// 或落后太多已经翻页）。此时既不打红点也不显示「已是最新」。
	KnownCurrent      bool           `json:"knownCurrent"`
	Releases          []ReleaseBrief `json:"releases"` // 最多 rollbackListSize 条
	CheckedAt         int64          `json:"checkedAt"` // 0 表示从未成功
	LastError         string         `json:"lastError"`
	Updatable         bool           `json:"updatable"`
	UnsupportedReason string         `json:"unsupportedReason"`
}

// toBriefs 过滤 draft/prerelease 并把发布时间转成 Unix 毫秒。
//
// prerelease 不参与「最新版」判定，也不进回退列表：当前仓库全是正式
// release，行为不变；将来若打 prerelease，不会因此误报「有新版可更新」。
//
// 时间解析失败只把 PublishedAt 留成 0，不丢弃整条——版本号比发布日期重要
// 得多，为一个显示用的时间戳丢掉一整个可回退的版本不划算。
func toBriefs(raw []githubRelease) []ReleaseBrief {
	briefs := make([]ReleaseBrief, 0, len(raw))
	for _, r := range raw {
		if r.Draft || r.Prerelease {
			continue
		}
		var ms int64
		if t, err := time.Parse(time.RFC3339, r.PublishedAt); err == nil {
			ms = t.UnixMilli()
		}
		briefs = append(briefs, ReleaseBrief{
			TagName:     r.TagName,
			PublishedAt: ms,
			HtmlUrl:     r.HtmlUrl,
		})
	}
	return briefs
}

// computeVersionState 按 releases 列表的天然顺序判断当前版本的处境。
//
// 刻意不做语义化版本解析：仓库现有 tag 里 0.3.4.4 与 v1.2.10 并存，字符串
// 比较会把 v1.2.9 > v1.2.10 判反，而自己写一个「够用」的 semver 解析器等于
// 埋一个只在特定版本号下才发作的错判。GitHub 的 releases 接口默认按创建
// 时间降序，这个顺序本身就是答案。
func computeVersionState(current string, all []ReleaseBrief) (string, bool, bool) {
	if len(all) == 0 {
		return "", false, false
	}
	latest := all[0].TagName
	for i, r := range all {
		if r.TagName == current {
			return latest, i > 0, true
		}
	}
	return latest, false, false
}

// validateUpgradeTag 是命令注入的两道防线，都必须硬拒绝，绝不 fail open。
func validateUpgradeTag(tag string, allowed []ReleaseBrief) error {
	if !tagPattern.MatchString(tag) {
		return common.NewErrorf("版本号格式非法：%q", tag)
	}
	for _, r := range allowed {
		if r.TagName == tag {
			return nil
		}
	}
	return common.NewErrorf("版本 %s 不在最近的发布列表中，请先点刷新再试", tag)
}

// buildUpgradeCommand 组装 systemd-run 的完整 argv。
//
// 必须用 systemd-run 起独立 transient unit：面板 os/exec 的子进程与面板
// 同在 /system.slice/a-ui.service 这个 cgroup 里，而 install.sh 会
// systemctl stop a-ui，KillMode=control-group（默认）会把整个 cgroup 杀光，
// 更新脚本死在 rm -rf /usr/local/a-ui/ 前后，留下一台面板已删一半、服务已停、
// 且因 Restart=no 不会自愈的机器。
//
// 2026-09-05 实测：systemd-run 的 unit 在父 service 被 stop 后存活；
// 用了 setsid 的对照组仍被杀死。**不要把这里改成 setsid 或 nohup。**
//
// unitName 由调用方传入而不是在函数内取时间戳，为的是这个函数可测。
func buildUpgradeCommand(tag, unitName string) []string {
	inner := fmt.Sprintf(
		"{ curl -fLso %s %s && bash %s %s </dev/null ; } >%s 2>&1",
		upgradeScriptPath, installScriptURL, upgradeScriptPath, tag, upgradeLogPath,
	)
	return []string{
		"systemd-run",
		"--unit=" + unitName,
		"--collect",
		"--description=AetherUI 面板更新",
		"/bin/bash", "-c", inner,
	}
}

// checkUpdatable 判断本机是否具备一键更新的条件。
//
// 在 Docker 里跑 install.sh 是纯粹的破坏：它会 systemctl（不存在）、
// rm -rf /usr/local/a-ui/（容器里就是应用本体）。这不是防御性编程，
// 是防止一个明确会毁掉环境的操作被点到。
//
// 返回的原因要具体到哪一条没过，前端原样显示——只说「不支持」会让管理员
// 无从下手。
func checkUpdatable() (bool, string) {
	if runtime.GOOS != "linux" {
		return false, "一键更新只支持 Linux，当前系统是 " + runtime.GOOS
	}
	if _, err := os.Stat(panelBinaryPath); err != nil {
		return false, "未在标准安装路径找到面板程序（" + panelBinaryPath + "），可能不是通过 install.sh 安装的"
	}
	if _, err := os.Stat(panelServicePath); err != nil {
		return false, "未找到 systemd 单元（" + panelServicePath + "），可能运行在容器或非 systemd 系统中"
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false, "系统缺少 systemd-run，无法安全地在后台执行更新"
	}
	return true, ""
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestToBriefs|TestComputeVersionState|TestValidateUpgradeTag|TestBuildUpgradeCommand|TestCheckUpdatable' -v
```

预期：全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add web/service/panel_version.go web/service/panel_version_test.go
git commit -m "feat(version): 面板版本判定与更新命令构造的纯逻辑

按 GitHub releases 的天然顺序判断版本处境，不做语义化解析——仓库现有 tag
里 0.3.4.4 与 v1.2.10 并存，字符串比较会把 v1.2.9 > v1.2.10 判反。

tag 白名单是两道硬拒绝（正则 + 必须在发布列表内），与 routing_validate.go
的 fail open 取向相反且必须相反：那里放行的是没法证明非法的配置，这里放行
的是一段会以 root 执行的字符串。"
```

---

### Task 2: GitHub 拉取与缓存

**Files:**
- Modify: `web/service/panel_version.go`（追加）
- Modify: `web/service/panel_version_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `toBriefs` / `computeVersionState` / `checkUpdatable` / `panelReleasesURL`
- Produces:
  - `type PanelVersionService struct{}`
  - `func (s *PanelVersionService) Refresh() error`
  - `func (s *PanelVersionService) Get() PanelVersionInfo`
  - `func (s *PanelVersionService) allowedTags() []ReleaseBrief`
  - 包级缓存 `panelVersionCache`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/panel_version_test.go`（同时补上 import：`encoding/json`、`net/http`、`net/http/httptest`）：

```go
// resetPanelVersionCache 让每个用例从干净状态开始。缓存是包级变量，
// 用例之间会互相污染。
func resetPanelVersionCache(t *testing.T) {
	t.Helper()
	panelVersionCache.mu.Lock()
	panelVersionCache.info = PanelVersionInfo{}
	panelVersionCache.all = nil
	panelVersionCache.mu.Unlock()
}

func stubReleasesServer(t *testing.T, releases []githubRelease) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(releases)
	}))
	orig := panelReleasesURL
	panelReleasesURL = srv.URL
	t.Cleanup(func() {
		panelReleasesURL = orig
		srv.Close()
	})
}

func TestRefreshPopulatesCache(t *testing.T) {
	resetPanelVersionCache(t)
	stubReleasesServer(t, []githubRelease{
		{TagName: "v1.5.0", PublishedAt: "2026-09-05T04:08:48Z", HtmlUrl: "u1"},
		{TagName: "v1.4.1", PublishedAt: "2026-09-05T01:04:22Z", HtmlUrl: "u2"},
	})
	s := PanelVersionService{}
	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	info := s.Get()
	if info.Latest != "v1.5.0" {
		t.Errorf("Latest = %q", info.Latest)
	}
	if info.CheckedAt == 0 {
		t.Error("CheckedAt 应被写入")
	}
	if info.LastError != "" {
		t.Errorf("LastError = %q, want empty", info.LastError)
	}
	if len(info.Releases) != 2 {
		t.Errorf("Releases len = %d, want 2", len(info.Releases))
	}
}

// 回退列表只给前 rollbackListSize 条，这既是 UI 约束也是 tag 白名单。
func TestRefreshTruncatesRollbackList(t *testing.T) {
	resetPanelVersionCache(t)
	raw := make([]githubRelease, 0, 10)
	for i := 0; i < 10; i++ {
		raw = append(raw, githubRelease{TagName: "v1." + string(rune('0'+i)) + ".0"})
	}
	stubReleasesServer(t, raw)
	s := PanelVersionService{}
	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := len(s.Get().Releases); got != rollbackListSize {
		t.Errorf("Releases len = %d, want %d", got, rollbackListSize)
	}
	if got := len(s.allowedTags()); got != rollbackListSize {
		t.Errorf("allowedTags len = %d, want %d", got, rollbackListSize)
	}
}

// KnownCurrent 用全部拉回来的 release 判定，而不是被截断的回退列表：
// 落后 6~10 个版本的人恰恰最需要看到红点。
func TestKnownCurrentUsesFullListNotTruncated(t *testing.T) {
	resetPanelVersionCache(t)
	raw := []githubRelease{
		{TagName: "v1.9.0"}, {TagName: "v1.8.0"}, {TagName: "v1.7.0"},
		{TagName: "v1.6.0"}, {TagName: "v1.5.0"}, {TagName: "v1.4.0"},
		{TagName: "v1.3.0"},
	}
	stubReleasesServer(t, raw)
	origVersion := panelCurrentVersion
	defer func() { panelCurrentVersion = origVersion }()
	// v1.3.0 排第 7，超出了 5 条的回退列表
	panelCurrentVersion = func() string { return "v1.3.0" }

	s := PanelVersionService{}
	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	info := s.Get()
	if !info.KnownCurrent {
		t.Error("KnownCurrent = false —— 落后 6 个版本的人被误判成「不在发布列表中」")
	}
	if !info.HasUpdate {
		t.Error("HasUpdate = false, want true")
	}
}

// 拉取失败必须保留上一次成功的数据，只更新 LastError。
// 清空的话界面会从「有新版可更新」变成「尚未检查」，而且更新按钮会因为
// 白名单变空而全部失效——一次网络抖动就把功能整个关掉了。
func TestRefreshFailureKeepsLastGoodData(t *testing.T) {
	resetPanelVersionCache(t)
	stubReleasesServer(t, []githubRelease{{TagName: "v1.5.0"}})
	s := PanelVersionService{}
	if err := s.Refresh(); err != nil {
		t.Fatalf("首次 Refresh: %v", err)
	}
	firstCheckedAt := s.Get().CheckedAt

	// 换成一个必然失败的地址
	orig := panelReleasesURL
	panelReleasesURL = "http://127.0.0.1:1/nope"
	defer func() { panelReleasesURL = orig }()

	if err := s.Refresh(); err == nil {
		t.Fatal("期望 Refresh 返回错误")
	}
	info := s.Get()
	if len(info.Releases) != 1 || info.Releases[0].TagName != "v1.5.0" {
		t.Errorf("失败后丢掉了上次成功的数据: %+v", info.Releases)
	}
	if info.LastError == "" {
		t.Error("LastError 应被写入")
	}
	if info.CheckedAt != firstCheckedAt {
		t.Error("CheckedAt 不该被失败的刷新改写——它表示「上次成功检查」的时刻")
	}
}

// GitHub 限速时返回的是一个 JSON 对象而不是数组，不能 panic。
func TestRefreshHandlesNonArrayResponse(t *testing.T) {
	resetPanelVersionCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()
	orig := panelReleasesURL
	panelReleasesURL = srv.URL
	defer func() { panelReleasesURL = orig }()

	s := PanelVersionService{}
	if err := s.Refresh(); err == nil {
		t.Error("限速响应应返回错误而不是静默成功")
	}
}

func TestRefreshRejectsNon200(t *testing.T) {
	resetPanelVersionCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	orig := panelReleasesURL
	panelReleasesURL = srv.URL
	defer func() { panelReleasesURL = orig }()

	if err := (&PanelVersionService{}).Refresh(); err == nil {
		t.Error("HTTP 403 应返回错误")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestRefresh|TestKnownCurrent' -v
```

预期：`undefined: panelVersionCache`、`undefined: panelCurrentVersion`。

- [ ] **Step 3: 写实现**

追加到 `web/service/panel_version.go`（import 补 `encoding/json`、`io`、`sync`、`a-ui/config`、`a-ui/logger`）：

```go
// panelCurrentVersion 抽成变量供测试打桩。
var panelCurrentVersion = config.GetVersion

// versionCache 是外部世界的一份快照。
//
// 刻意不落库：落库要走「新增设置项的五步」（defaultValueMap /
// entity.AllSetting / CheckValid / getter / models.js），漏掉最后一步会让
// 整个保存配置接口失败。为一份重启后几分钟内必然自愈的缓存付这个代价
// 不划算。代价是面板重启后 UI 上会短暂显示「尚未检查」。
type versionCache struct {
	mu   sync.RWMutex
	info PanelVersionInfo
	// all 是拉回来的全部 release（最多 per_page 条），只用于 KnownCurrent
	// 判定；info.Releases 是被截断到 rollbackListSize 的回退列表。
	all []ReleaseBrief
}

var panelVersionCache versionCache

type PanelVersionService struct{}

func fetchReleases() ([]ReleaseBrief, error) {
	resp, err := panelVersionHTTPClient.Get(panelReleasesURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, common.NewErrorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	// 限速时 GitHub 返回的是 {"message": "..."} 这样的对象而不是数组，
	// 直接 Unmarshal 进切片会报错——这正是我们要的，不能静默当成空列表。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var raw []githubRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, common.NewError("解析 GitHub 响应失败:", err)
	}
	return toBriefs(raw), nil
}

// Refresh 重新拉取并更新缓存。
//
// 失败时保留上一次成功的数据，只写 LastError——与域名组订阅刷新同一个
// 原则。清空的话界面会从「有新版可更新」退回「尚未检查」，且 tag 白名单
// 变空会让更新按钮全部失效，一次网络抖动就把功能整个关掉。
func (s *PanelVersionService) Refresh() error {
	all, err := fetchReleases()
	if err != nil {
		panelVersionCache.mu.Lock()
		panelVersionCache.info.LastError = err.Error()
		panelVersionCache.mu.Unlock()
		return err
	}

	current := panelCurrentVersion()
	latest, hasUpdate, known := computeVersionState(current, all)
	rollback := all
	if len(rollback) > rollbackListSize {
		rollback = rollback[:rollbackListSize]
	}
	updatable, reason := checkUpdatable()

	panelVersionCache.mu.Lock()
	panelVersionCache.all = all
	panelVersionCache.info = PanelVersionInfo{
		Current:           current,
		Latest:            latest,
		HasUpdate:         hasUpdate,
		KnownCurrent:      known,
		Releases:          rollback,
		CheckedAt:         time.Now().UnixMilli(),
		LastError:         "",
		Updatable:         updatable,
		UnsupportedReason: reason,
	}
	panelVersionCache.mu.Unlock()
	return nil
}

// Get 返回缓存快照。从未成功检查过时，Current 与 Updatable 仍要是真的——
// 版本号本来就在本地，没有理由因为拉不到 GitHub 就不显示。
func (s *PanelVersionService) Get() PanelVersionInfo {
	panelVersionCache.mu.RLock()
	info := panelVersionCache.info
	panelVersionCache.mu.RUnlock()

	if info.Current == "" {
		info.Current = panelCurrentVersion()
	}
	if info.CheckedAt == 0 {
		info.Updatable, info.UnsupportedReason = checkUpdatable()
	}
	return info
}

// allowedTags 是 tag 白名单，等于回退列表。
func (s *PanelVersionService) allowedTags() []ReleaseBrief {
	panelVersionCache.mu.RLock()
	defer panelVersionCache.mu.RUnlock()
	return panelVersionCache.info.Releases
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestRefresh|TestKnownCurrent|TestToBriefs|TestComputeVersionState|TestValidateUpgradeTag' -v
go vet ./web/service/
```

预期：全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add web/service/panel_version.go web/service/panel_version_test.go
git commit -m "feat(version): GitHub releases 拉取与包级缓存

拉取失败保留上一次成功的数据、只写 LastError：清空会让界面从「有新版可
更新」退回「尚未检查」，且 tag 白名单变空会让更新按钮全部失效，一次网络
抖动就把功能整个关掉。

KnownCurrent 用拉回来的全部 release 判定，回退列表只取前 5 条——落后
6~10 个版本的管理员恰恰是最需要看到红点的人。"
```

---

### Task 3: 执行更新与读取更新日志

**Files:**
- Modify: `web/service/panel_version.go`（追加）
- Modify: `web/service/panel_version_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `validateUpgradeTag` / `buildUpgradeCommand` / `checkUpdatable`，Task 2 的 `allowedTags`
- Produces:
  - `func (s *PanelVersionService) Upgrade(tag string) error`
  - `func (s *PanelVersionService) UpgradeLog() ([]string, error)`
  - 包级可打桩变量 `runUpgradeCommand func(argv []string) error`

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/panel_version_test.go`（import 补 `os`、`path/filepath`）：

```go
func TestUpgradeRejectsWhenNotUpdatable(t *testing.T) {
	resetPanelVersionCache(t)
	stubReleasesServer(t, []githubRelease{{TagName: "v1.5.0"}})
	if err := (&PanelVersionService{}).Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	origBin := panelBinaryPath
	defer func() { panelBinaryPath = origBin }()
	panelBinaryPath = "/nonexistent/a-ui"

	var called bool
	origRun := runUpgradeCommand
	defer func() { runUpgradeCommand = origRun }()
	runUpgradeCommand = func(argv []string) error { called = true; return nil }

	if err := (&PanelVersionService{}).Upgrade("v1.5.0"); err == nil {
		t.Error("环境不支持时应拒绝")
	}
	if called {
		t.Error("环境不支持时绝不能真的去执行命令")
	}
}

func TestUpgradeRejectsInjectionBeforeExec(t *testing.T) {
	resetPanelVersionCache(t)
	stubReleasesServer(t, []githubRelease{{TagName: "v1.5.0"}})
	if err := (&PanelVersionService{}).Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	var called bool
	origRun := runUpgradeCommand
	defer func() { runUpgradeCommand = origRun }()
	runUpgradeCommand = func(argv []string) error { called = true; return nil }
	origUpdatable := forceUpdatableForTest
	defer func() { forceUpdatableForTest = origUpdatable }()
	forceUpdatableForTest = true

	if err := (&PanelVersionService{}).Upgrade("v1.5.0; rm -rf /"); err == nil {
		t.Error("注入串应被拒绝")
	}
	if called {
		t.Error("注入串绝不能到达执行阶段")
	}
}

func TestUpgradePassesValidatedTagToCommand(t *testing.T) {
	resetPanelVersionCache(t)
	stubReleasesServer(t, []githubRelease{{TagName: "v1.5.0"}, {TagName: "v1.4.1"}})
	if err := (&PanelVersionService{}).Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	origUpdatable := forceUpdatableForTest
	defer func() { forceUpdatableForTest = origUpdatable }()
	forceUpdatableForTest = true

	var gotArgv []string
	origRun := runUpgradeCommand
	defer func() { runUpgradeCommand = origRun }()
	runUpgradeCommand = func(argv []string) error { gotArgv = argv; return nil }

	if err := (&PanelVersionService{}).Upgrade("v1.4.1"); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(gotArgv) == 0 {
		t.Fatal("命令没有被执行")
	}
	if gotArgv[0] != "systemd-run" {
		t.Errorf("argv[0] = %q", gotArgv[0])
	}
	if !strings.Contains(strings.Join(gotArgv, " "), "v1.4.1") {
		t.Error("命令里没有目标版本")
	}
}

func TestUpgradeLogReturnsTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update.log")
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("line-")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	orig := upgradeLogPathForTest
	defer func() { upgradeLogPathForTest = orig }()
	upgradeLogPathForTest = path

	lines, err := (&PanelVersionService{}).UpgradeLog()
	if err != nil {
		t.Fatalf("UpgradeLog: %v", err)
	}
	if len(lines) != upgradeLogTailLines {
		t.Fatalf("len = %d, want %d", len(lines), upgradeLogTailLines)
	}
	if lines[len(lines)-1] != "line-499" {
		t.Errorf("最后一行 = %q, want line-499", lines[len(lines)-1])
	}
}

// 从没更新过时日志文件不存在，这不是错误——不能让「查看日志」按钮
// 弹一个红色的失败提示，那会让管理员以为更新出了问题。
func TestUpgradeLogMissingFileIsNotAnError(t *testing.T) {
	orig := upgradeLogPathForTest
	defer func() { upgradeLogPathForTest = orig }()
	upgradeLogPathForTest = filepath.Join(t.TempDir(), "nope.log")

	lines, err := (&PanelVersionService{}).UpgradeLog()
	if err != nil {
		t.Errorf("文件不存在不该报错: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("应返回空切片, got %v", lines)
	}
}
```

在测试文件顶部 import 里补 `strconv`。

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./web/service/ -run 'TestUpgrade' -v
```

预期：`undefined: runUpgradeCommand`、`undefined: forceUpdatableForTest`、`undefined: upgradeLogPathForTest`、`undefined: upgradeLogTailLines`。

- [ ] **Step 3: 写实现**

追加到 `web/service/panel_version.go`（import 补 `bufio`、`strings`、`errors`）：

```go
const upgradeLogTailLines = 200

// 这三个变量供测试打桩。upgradeLogPathForTest 初值就是真实路径，
// 生产代码路径上它与常量 upgradeLogPath 等价。
var (
	upgradeLogPathForTest = upgradeLogPath
	// forceUpdatableForTest 为 true 时跳过环境前置检查。只有测试会设它，
	// 生产代码永远走 checkUpdatable。
	forceUpdatableForTest = false
	runUpgradeCommand     = func(argv []string) error {
		return exec.Command(argv[0], argv[1:]...).Run()
	}
)

// Upgrade 把更新交给一个独立的 systemd transient unit 后立即返回。
//
// 必须先返回响应再让更新开始：面板自己马上就要被 systemctl stop 掉，
// 等更新结果等不到。systemd-run 是 fire-and-forget——它把 unit 交给
// systemd 后立即返回，此时更新脚本还没开始跑。所以：
//   - systemd-run 返回非零 → 连 unit 都没起来 → 如实报错，面板毫发无损
//   - systemd-run 返回 0   → 只说明「已经开始」，不代表会成功
func (s *PanelVersionService) Upgrade(tag string) error {
	if !forceUpdatableForTest {
		if ok, reason := checkUpdatable(); !ok {
			return common.NewError("当前环境不支持一键更新:", reason)
		}
	}
	if err := validateUpgradeTag(tag, s.allowedTags()); err != nil {
		return err
	}
	unitName := fmt.Sprintf("a-ui-update-%d", time.Now().Unix())
	argv := buildUpgradeCommand(tag, unitName)
	logger.Info("面板更新：交给 systemd unit", unitName, "目标版本", tag)
	if err := runUpgradeCommand(argv); err != nil {
		return common.NewError("启动更新任务失败:", err)
	}
	return nil
}

// UpgradeLog 返回更新日志的末尾若干行。
//
// 路径硬编码、不接受任何参数——这是一个读文件的接口，参数化就是路径穿越。
//
// 文件不存在返回空切片而不是错误：从没更新过是正常状态，报错会让「查看
// 日志」弹一个红色失败提示，管理员会以为更新出了问题。
func (s *PanelVersionService) UpgradeLog() ([]string, error) {
	f, err := os.Open(upgradeLogPathForTest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	// 环形缓冲，避免把整个日志读进内存。
	ring := make([]string, 0, upgradeLogTailLines)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if len(ring) < upgradeLogTailLines {
			ring = append(ring, line)
		} else {
			ring = append(ring[1:], line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return ring, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./web/service/ -run 'TestUpgrade|TestRefresh|TestCompute|TestValidate|TestBuild|TestToBriefs|TestCheckUpdatable|TestKnownCurrent' -v
go vet ./web/service/
```

- [ ] **Step 5: 提交**

```bash
git add web/service/panel_version.go web/service/panel_version_test.go
git commit -m "feat(version): 经 systemd-run 执行更新，并可读回更新日志

Upgrade 先过环境检查再过 tag 白名单，两道都不通过就绝不到达执行阶段。
UpgradeLog 的路径硬编码不接受参数——读文件的接口参数化就是路径穿越；
文件不存在返回空而非报错，从没更新过是正常状态。"
```

---

### Task 4: 定时刷新 job

**Files:**
- Create: `web/job/panel_version_job.go`
- Modify: `web/web.go`（`startTask`，当前在 298-336 行）

**Interfaces:**
- Consumes: Task 2 的 `service.PanelVersionService.Refresh()`
- Produces: `func job.NewPanelVersionJob() *PanelVersionJob`，实现 `cron.Job` 的 `Run()`

- [ ] **Step 1: 创建 job**

创建 `web/job/panel_version_job.go`：

```go
package job

import (
	"a-ui/logger"
	"a-ui/util/common"
	"a-ui/web/service"
)

// PanelVersionJob 每 6 小时刷新一次面板版本缓存。
//
// GitHub 未认证 API 限速 60 次/小时/IP，6 小时一次每天只用 4 次，
// 加上管理员手动刷新（controller 侧另有 1 分钟节流）绰绰有余。
//
// 拉取失败只记日志、不打扰：LastError 已经写进缓存，界面上会显示。
type PanelVersionJob struct {
	panelVersionService service.PanelVersionService
}

func NewPanelVersionJob() *PanelVersionJob {
	return new(PanelVersionJob)
}

func (j *PanelVersionJob) Run() {
	defer common.Recover("检查面板版本")
	if err := j.panelVersionService.Refresh(); err != nil {
		logger.Warning("检查面板版本失败:", err)
	}
}
```

- [ ] **Step 2: 在 startTask 里注册**

在 `web/web.go` 的 `startTask()` 末尾（`NewShapingJob` 那行之后）追加：

```go
	// 每 6 小时检查一次面板是否有新版本。
	s.cron.AddJob("@every 6h", job.NewPanelVersionJob())

	// cron.AddJob 的首次执行在一个完整周期之后，不做延迟触发的话新装的
	// 面板要等 6 小时才显示版本状态。延迟 10 秒是为了避开面板刚启动时
	// 和 xray 启动抢网络。
	go func() {
		time.Sleep(time.Second * 10)
		job.NewPanelVersionJob().Run()
	}()
```

- [ ] **Step 3: 编译与静态检查**

```bash
go build ./... && go vet ./...
```

预期：无输出。若报 `time` 未使用，检查 `web/web.go` 顶部 import——该文件已经 import 了 `time`（`XrayTrafficJob` 的延迟注册用到）。

- [ ] **Step 4: 手动验证 job 真的会跑**

```bash
XUI_DEBUG=true XUI_LOG_LEVEL=debug go run main.go 2>&1 | head -40
```

预期：启动 10 秒后不再有 `检查面板版本失败` 的 warning（本机能连 GitHub 时）；若断网，应看到一条 warning 而**面板继续正常运行**。确认后 Ctrl-C。

- [ ] **Step 5: 提交**

```bash
git add web/job/panel_version_job.go web/web.go
git commit -m "feat(version): 每 6 小时刷新一次版本缓存，启动后 10 秒先跑一次

cron.AddJob 的首次执行在一个完整周期之后，不做延迟触发的话新装的面板要
等 6 小时才显示版本状态。Run 首行的 common.Recover 让 panic 带上任务名，
不必只靠 cron 那层通用兜底。"
```

---

### Task 5: 四个 HTTP 接口

**Files:**
- Modify: `web/controller/server.go`

**Interfaces:**
- Consumes: `service.PanelVersionService` 的 `Get` / `Refresh` / `Upgrade` / `UpgradeLog`
- Produces: 四个路由
  - `POST /server/panelVersion` → `PanelVersionInfo`
  - `POST /server/refreshPanelVersion` → `PanelVersionInfo`
  - `POST /server/upgradePanel`（form: `version`）→ 消息
  - `POST /server/upgradeLog` → `{ "lines": [...] }`

- [ ] **Step 1: 给 ServerController 加字段与节流时间戳**

在 `web/controller/server.go` 的 `ServerController` 结构体里追加（现有字段 `lastGetVersionsTime` 之后）：

```go
	panelVersionService service.PanelVersionService

	lastRefreshPanelVersionTime time.Time
```

- [ ] **Step 2: 注册路由**

在 `initRouter` 里 `g.POST("/getNewEchCert", a.getNewEchCert)` 之后追加：

```go
	g.POST("/panelVersion", a.panelVersion)
	g.POST("/refreshPanelVersion", a.refreshPanelVersion)
	g.POST("/upgradePanel", a.upgradePanel)
	g.POST("/upgradeLog", a.upgradeLog)
```

- [ ] **Step 3: 写四个 handler**

在文件末尾追加：

```go
func (a *ServerController) panelVersion(c *gin.Context) {
	jsonObj(c, a.panelVersionService.Get(), nil)
}

// refreshPanelVersion 强制重查。节流 1 分钟，防的是管理员连点刷新撞上
// GitHub 60 次/小时/IP 的未认证限速——照抄 getXrayVersion 的既有做法。
//
// 撞到节流时返回缓存而不是报错：管理员点刷新看到的是「版本信息」，
// 一个红色的「太频繁」提示既没用又像出了故障。
func (a *ServerController) refreshPanelVersion(c *gin.Context) {
	if time.Since(a.lastRefreshPanelVersionTime) <= time.Minute {
		jsonObj(c, a.panelVersionService.Get(), nil)
		return
	}
	a.lastRefreshPanelVersionTime = time.Now()
	if err := a.panelVersionService.Refresh(); err != nil {
		// 不返回错误：Refresh 失败时上一次成功的数据仍在缓存里，且失败
		// 原因已经写进 LastError，前端会显示。这里再报一次错会让界面同时
		// 弹红条又显示旧数据，反而看不懂。
		logger.Warning("手动检查面板版本失败:", err)
	}
	jsonObj(c, a.panelVersionService.Get(), nil)
}

func (a *ServerController) upgradePanel(c *gin.Context) {
	version := c.PostForm("version")
	if err := a.panelVersionService.Upgrade(version); err != nil {
		jsonMsg(c, "启动更新", err)
		return
	}
	// 更新已经交给独立的 systemd unit，面板马上会被 stop 掉。
	// 这条响应必须在那之前发出去。
	jsonMsgObj(c, "", gin.H{
		"started": true,
		"version": version,
	}, nil)
}

func (a *ServerController) upgradeLog(c *gin.Context) {
	lines, err := a.panelVersionService.UpgradeLog()
	if err != nil {
		jsonMsg(c, "读取更新日志", err)
		return
	}
	jsonObj(c, gin.H{"lines": lines}, nil)
}
```

在 `web/controller/server.go` 顶部 import 里补 `"a-ui/logger"`。

- [ ] **Step 4: 编译并手工打一次接口**

```bash
go build ./... && go vet ./...
```

然后：

```bash
XUI_DEBUG=true go run main.go
```

另开一个终端（先用浏览器登录 `http://127.0.0.1:54321/` 拿到会话 cookie，或用 curl 带 cookie）：

```bash
# 未登录应被 checkLogin 拦下并重定向，说明鉴权生效
curl -si -X POST http://127.0.0.1:54321/server/panelVersion | head -3
```

预期：非 200 的 JSON，或重定向到登录页。登录后再打应返回含 `current` 的 JSON。

- [ ] **Step 5: 提交**

```bash
git add web/controller/server.go
git commit -m "feat(version): 版本查询/刷新/更新/日志四个接口

refreshPanelVersion 撞到 1 分钟节流时返回缓存而不是报错：管理员点刷新
想看到的是版本信息，一个红色的「太频繁」提示既没用又像出了故障。
Refresh 内部失败同样不向前端报错——LastError 已在缓存里，前端会显示，
再弹一次红条会与同时显示的旧数据自相矛盾。"
```

---

### Task 6: 前端 mixin 与侧边栏版本区

**Files:**
- Create: `web/assets/js/util/panel-version.js`
- Modify: `web/html/common/js.html`
- Modify: `web/html/xui/common_sider.html`
- Modify: `web/assets/css/custom.css`
- Modify: `web/html/xui/index.html`、`inbounds.html`、`routing.html`、`setting.html`

**Interfaces:**
- Consumes: Task 5 的四个接口、全局 `HttpUtil`（`web/assets/js/util/utils.js`）
- Produces: 全局常量 `panelVersionMixin`，提供 data `panelVersion` / `versionPopoverVisible` / `rollbackTag` / `upgradeState` / `upgradeLogLines`，methods `loadPanelVersion` / `refreshPanelVersion` / `confirmUpgrade` / `openUpgradeLog` / `formatReleaseDate`

- [ ] **Step 1: 创建 mixin**

创建 `web/assets/js/util/panel-version.js`：

```js
// 侧边栏版本区的共享逻辑。
//
// common_sider.html 被 index / inbounds / routing / setting 四个页面共用，
// 但每个页面各有一个 new Vue({el:'#app'})，data 互不相干。版本区的指令写在
// sider 里，这些 data 就必须四个实例都有——少一个，那个页面会引用 undefined。
// 抄四遍将来必然漏改，所以做成 mixin。
//
// mixin 的 data 必须是函数（Vue 2 规则）；根实例的 data 是对象字面量，
// Vue 的 mergeDataOrFn 会正确合并两者，不用改现有页面的 data 写法。
const panelVersionMixin = {
    data() {
        return {
            panelVersion: {
                current: '',
                latest: '',
                hasUpdate: false,
                knownCurrent: false,
                releases: [],
                checkedAt: 0,
                lastError: '',
                updatable: false,
                unsupportedReason: '',
            },
            versionPopoverVisible: false,
            versionRefreshing: false,
            rollbackOpen: false,
            rollbackTag: '',
            // idle | starting | restarting | done | timeout
            upgradeState: 'idle',
            upgradeTarget: '',
            upgradeLogVisible: false,
            upgradeLogLines: [],
        };
    },
    methods: {
        async loadPanelVersion() {
            const msg = await HttpUtil.post('/server/panelVersion');
            if (msg.success && msg.obj) {
                this.panelVersion = msg.obj;
            }
        },
        async refreshPanelVersion() {
            this.versionRefreshing = true;
            const msg = await HttpUtil.post('/server/refreshPanelVersion');
            this.versionRefreshing = false;
            if (msg.success && msg.obj) {
                this.panelVersion = msg.obj;
            }
        },
        formatReleaseDate(ms) {
            if (!ms) return '';
            return moment(ms).format('YYYY/M/D');
        },
        // 版本区的状态文案。三种状态必须能区分开：
        // 「已是最新」「有新版」「当前版本不在发布列表里」。
        versionStateText() {
            const v = this.panelVersion;
            if (!v.checkedAt) return v.lastError ? '检查失败' : '尚未检查';
            if (!v.knownCurrent) return '未在发布列表中';
            return v.hasUpdate ? ('有新版本 ' + v.latest) : '已是最新版本';
        },
        confirmUpgrade(tag, isRollback) {
            const content = isRollback
                ? ('确定要回退到 ' + tag + ' 吗？\n\n'
                    + '回退会一并把 xray 核心换成该版本携带的构建，新版新增的功能会失效。'
                    + '数据库和已有配置不会丢失。')
                : ('确定要更新到 ' + tag + ' 吗？\n\n面板会在更新过程中短暂不可用。');
            this.$confirm({
                title: isRollback ? '回退到旧版本' : '更新面板',
                content: h => h('div', content.split('\n').map(line => h('p', line))),
                okText: '确定',
                cancelText: '取消',
                onOk: () => this.startUpgrade(tag),
            });
        },
        async startUpgrade(tag) {
            this.upgradeTarget = tag;
            this.upgradeState = 'starting';
            const msg = await HttpUtil.post('/server/upgradePanel', { version: tag });
            if (!msg.success) {
                this.upgradeState = 'idle';
                return;
            }
            this.pollUpgrade(tag, Date.now());
        },
        // 轮询直到 current 变成目标版本。面板会在中途被 stop，请求会失败，
        // 那正是「正在重启」的信号，要继续重试而不是报错退出。
        async pollUpgrade(tag, startedAt) {
            const TIMEOUT_MS = 3 * 60 * 1000;
            if (Date.now() - startedAt > TIMEOUT_MS) {
                this.upgradeState = 'timeout';
                return;
            }
            await PromiseUtil.sleep(3000);
            let reached = false;
            try {
                const msg = await HttpUtil.post('/server/panelVersion');
                if (msg.success && msg.obj) {
                    this.panelVersion = msg.obj;
                    reached = msg.obj.current === tag;
                }
                if (!reached) this.upgradeState = 'starting';
            } catch (e) {
                // 面板正在重启，连不上是预期内的
                this.upgradeState = 'restarting';
            }
            if (reached) {
                this.upgradeState = 'done';
                return;
            }
            this.pollUpgrade(tag, startedAt);
        },
        async openUpgradeLog() {
            const msg = await HttpUtil.post('/server/upgradeLog');
            if (msg.success && msg.obj) {
                this.upgradeLogLines = msg.obj.lines || [];
                this.upgradeLogVisible = true;
            }
        },
    },
    mounted() {
        this.loadPanelVersion();
    },
};
```

**注意 `PromiseUtil.sleep`**：先确认它存在——

```bash
grep -n "class PromiseUtil" -A 12 web/assets/js/util/utils.js
```

若没有 `sleep` 方法，把 `await PromiseUtil.sleep(3000)` 换成 `await new Promise(r => setTimeout(r, 3000))`。

- [ ] **Step 2: 引入脚本**

在 `web/html/common/js.html` 的 `models.js` 那行之后追加：

```html
<script src="{{ .base_path }}assets/js/util/panel-version.js?{{ .cur_ver }}"></script>
```

放 `js.html` 而不是四个页面各引一遍：它是四个内页共用的，抄四遍将来必然漏改。代价是 `login.html` 也 include 了 `js.html`，登录页会白下载这几 KB——与已经在里面、登录页同样用不到且体积大得多的 `xray.js` 同级。Chart.js 之所以没进 `js.html` 是因为那一份体积不小，量级不同。

- [ ] **Step 3: 在侧边栏加版本区**

在 `web/html/xui/common_sider.html` 的 `commonSider` 定义里，把 `<a-layout-sider>` 改成：

```html
<a-layout-sider id="sider" collapsible breakpoint="md" collapsed-width="0">
    <a-menu theme="dark" mode="inline" :selected-keys="['{{ .request_uri }}']"
            @click="({key}) => key.startsWith('http') ? window.open(key) : location.href = key">
        {{template "menuItems" .}}
    </a-menu>
    <div class="sider-version">
        <a-popover v-model="versionPopoverVisible" trigger="click" placement="topRight"
                   overlay-class-name="version-popover">
            <template slot="content">
                <div class="version-panel">
                    <div class="version-panel-head">
                        <span>当前版本</span>
                        <a-icon type="sync" :spin="versionRefreshing"
                                @click="refreshPanelVersion()"></a-icon>
                    </div>
                    <div class="version-panel-body">
                        <div class="version-current">
                            [[ panelVersion.current ]]
                            <a-icon v-if="panelVersion.knownCurrent && !panelVersion.hasUpdate"
                                    type="check-circle" theme="filled" style="color:#52c41a"></a-icon>
                        </div>
                        <div class="version-state">[[ versionStateText() ]]</div>
                        <div v-if="panelVersion.lastError" class="version-error">
                            [[ panelVersion.lastError ]]
                        </div>
                        <a-button v-if="panelVersion.hasUpdate && panelVersion.updatable"
                                  type="primary" size="small" style="margin-top:8px"
                                  @click="confirmUpgrade(panelVersion.latest, false)">
                            更新到 [[ panelVersion.latest ]]
                        </a-button>
                        <div v-if="!panelVersion.updatable && panelVersion.unsupportedReason"
                             class="version-unsupported">
                            [[ panelVersion.unsupportedReason ]]
                        </div>
                        <div style="margin-top:8px">
                            <a :href="'https://github.com/SienFeng/AetherUI/releases'"
                               target="_blank" rel="noopener">
                                <a-icon type="github"></a-icon> 查看发布
                            </a>
                        </div>
                    </div>
                    <div v-if="panelVersion.updatable && panelVersion.releases.length"
                         class="version-rollback">
                        <div class="version-rollback-head" @click="rollbackOpen = !rollbackOpen">
                            <span><a-icon type="clock-circle"></a-icon> 版本回退</span>
                            <a-icon :type="rollbackOpen ? 'up' : 'down'"></a-icon>
                        </div>
                        <div v-if="rollbackOpen">
                            <div class="version-rollback-tip">
                                选择要回退到的版本（近 [[ panelVersion.releases.length ]] 个版本）
                            </div>
                            <a-radio-group v-model="rollbackTag" style="width:100%">
                                <div v-for="r in panelVersion.releases" :key="r.tagName"
                                     class="version-rollback-item">
                                    <a-radio :value="r.tagName">[[ r.tagName ]]</a-radio>
                                    <span class="version-rollback-date">
                                        [[ formatReleaseDate(r.publishedAt) ]]
                                    </span>
                                </div>
                            </a-radio-group>
                            <a-button size="small" :disabled="!rollbackTag"
                                      style="margin-top:8px;width:100%"
                                      @click="confirmUpgrade(rollbackTag, true)">
                                回退到选中版本
                            </a-button>
                        </div>
                    </div>
                </div>
            </template>
            <a-badge :dot="panelVersion.hasUpdate">
                <span class="sider-version-tag">[[ panelVersion.current || '—' ]]</span>
            </a-badge>
        </a-popover>
    </div>
</a-layout-sider>
```

**这段整体位于 `<a-layout id="app">` 内部**（`commonSider` 模板是在 `#app` 里被 include 的），所以 Vue 会编译它。挪到外面会变成完全静默的死代码。

- [ ] **Step 4: 加样式**

在 `web/assets/css/custom.css` 末尾追加：

```css
/* 侧栏底部的版本区。ant-design-vue 1.x 的 sider 内部包一层
   .ant-layout-sider-children，把它变成纵向 flex 才能让版本区贴底。 */
#sider .ant-layout-sider-children {
    display: flex;
    flex-direction: column;
}

#sider .ant-layout-sider-children > .ant-menu {
    flex: 1 1 auto;
}

.sider-version {
    flex: 0 0 auto;
    padding: 12px 16px;
    text-align: center;
}

.sider-version-tag {
    display: inline-block;
    padding: 2px 10px;
    border-radius: 10px;
    font-size: 12px;
    color: rgba(255, 255, 255, .65);
    background: rgba(255, 255, 255, .08);
    cursor: pointer;
    user-select: none;
}

.sider-version-tag:hover {
    color: #fff;
    background: rgba(255, 255, 255, .16);
}

.version-panel {
    width: 260px;
}

.version-panel-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding-bottom: 8px;
    border-bottom: 1px solid #f0f0f0;
}

.version-panel-head .anticon {
    cursor: pointer;
    color: #8c8c8c;
}

.version-panel-body {
    padding: 12px 0;
    text-align: center;
}

.version-current {
    font-size: 20px;
    font-weight: 600;
}

.version-state {
    color: #8c8c8c;
    font-size: 12px;
    margin-top: 4px;
}

.version-error,
.version-unsupported {
    color: #ff4d4f;
    font-size: 12px;
    margin-top: 6px;
    word-break: break-all;
}

.version-unsupported {
    color: #8c8c8c;
}

.version-rollback {
    border-top: 1px solid #f0f0f0;
    padding-top: 8px;
}

.version-rollback-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    cursor: pointer;
    color: #595959;
}

.version-rollback-tip {
    font-size: 12px;
    color: #8c8c8c;
    margin: 6px 0;
}

.version-rollback-item {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 6px 8px;
    border: 1px solid #f0f0f0;
    border-radius: 6px;
    margin-bottom: 6px;
}

.version-rollback-date {
    font-size: 12px;
    color: #8c8c8c;
}
```

- [ ] **Step 5: 四个页面挂 mixin**

在 `index.html:301`、`inbounds.html:383`、`routing.html:368`、`setting.html:182` 的 `new Vue({` 之后、`delimiters` 之前，各加一行：

```js
        mixins: [panelVersionMixin],
```

例如 `index.html` 改成：

```js
    const app = new Vue({
        mixins: [panelVersionMixin],
        delimiters: ['[[', ']]'],
        el: '#app',
        data: {
```

- [ ] **Step 6: 跑模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

预期：两个都 PASS。若 `TestVueDirectivesLiveInsideAVueRoot` 失败，说明有 `v-*` / `@*` / `:*` 落在 `#app` 之外——检查 Step 3 的插入位置。

- [ ] **Step 7: 目视验证**

```bash
XUI_DEBUG=true go run main.go
```

浏览器打开 `http://127.0.0.1:54321/`，登录后逐个访问系统状态 / 入站列表 / 分流管理 / 面板设置四个页面。每页都应：

1. 侧栏底部显示版本号（本地是 `0.3.2`）
2. 点击弹出面板，显示「未在发布列表中」（本地版本号不在 GitHub 发布列表里，这是**正确行为**）
3. 因为本地是 macOS/非标准安装，应显示 `unsupportedReason`，且**没有**更新按钮和回退区
4. 控制台无报错

- [ ] **Step 8: 提交**

```bash
git add web/assets/js/util/panel-version.js web/html/common/js.html \
        web/html/xui/common_sider.html web/assets/css/custom.css \
        web/html/xui/index.html web/html/xui/inbounds.html \
        web/html/xui/routing.html web/html/xui/setting.html
git commit -m "feat(version): 侧栏底部版本区与版本弹窗

用 Vue mixin 而不是在四个页面各抄一遍：common_sider.html 是共用的，但四个
页面各有自己的 new Vue，data 互不相干，少给一个页面就会引用 undefined。

版本区整体位于 <a-layout id=app> 内——Vue 2 只编译 el 指向的子树，写在
外面的指令是完全静默的死代码（分流页的三个弹窗踩过这个坑）。"
```

---

### Task 7: 更新进度与日志弹窗

版本弹窗里点了更新之后，面板马上要自杀。没有这一层，管理员面对的是一个正在重启的面板和一片空白，更新失败时的表现与「点了没反应」完全一致。

**Files:**
- Modify: `web/html/xui/common_sider.html`

**Interfaces:**
- Consumes: Task 6 的 `upgradeState` / `upgradeTarget` / `upgradeLogVisible` / `upgradeLogLines` / `openUpgradeLog`

- [ ] **Step 1: 在 commonSider 模板里加两个 modal**

在 `common_sider.html` 的 `</a-layout-sider>` 之后、`<a-drawer id="sider-drawer"` 之前插入：

```html
<a-modal v-model="upgradeState !== 'idle'" title="面板更新"
         :closable="false" :mask-closable="false" :footer="null" :width="460">
    <div style="text-align:center;padding:12px 0">
        <template v-if="upgradeState === 'starting'">
            <a-spin size="large"></a-spin>
            <p style="margin-top:16px">正在更新到 [[ upgradeTarget ]]…</p>
            <p style="color:#8c8c8c;font-size:12px">
                更新在后台独立进程中执行，请不要关闭此页面
            </p>
        </template>
        <template v-else-if="upgradeState === 'restarting'">
            <a-spin size="large"></a-spin>
            <p style="margin-top:16px">面板重启中…</p>
            <p style="color:#8c8c8c;font-size:12px">这是更新过程中的正常现象</p>
        </template>
        <template v-else-if="upgradeState === 'done'">
            <a-icon type="check-circle" theme="filled"
                    style="font-size:48px;color:#52c41a"></a-icon>
            <p style="margin-top:16px">更新完成，当前版本 [[ panelVersion.current ]]</p>
            <a-button type="primary" @click="location.reload()">刷新页面</a-button>
        </template>
        <template v-else-if="upgradeState === 'timeout'">
            <a-icon type="exclamation-circle" theme="filled"
                    style="font-size:48px;color:#faad14"></a-icon>
            <p style="margin-top:16px">3 分钟内版本号仍未变化，更新可能失败</p>
            <p style="color:#8c8c8c;font-size:12px">
                面板本身未受影响。查看日志可以看到具体原因。
            </p>
            <a-button style="margin-right:8px" @click="openUpgradeLog()">查看日志</a-button>
            <a-button @click="upgradeState = 'idle'">关闭</a-button>
        </template>
    </div>
</a-modal>

<a-modal v-model="upgradeLogVisible" title="更新日志" :footer="null" :width="800">
    <p style="color:#8c8c8c;font-size:12px">
        服务器上的 /var/log/a-ui-update.log，只显示最后 200 行
    </p>
    <pre class="upgrade-log"><template v-for="line in upgradeLogLines">[[ line ]]
</template></pre>
    <div v-if="!upgradeLogLines.length" style="color:#8c8c8c">
        日志为空，说明还没有执行过更新
    </div>
</a-modal>
```

⚠️ `v-model="upgradeState !== 'idle'"` 在 Vue 2 里不合法（`v-model` 需要可写表达式）。改用 `:visible`：

```html
<a-modal :visible="upgradeState !== 'idle'" title="面板更新"
         :closable="false" :mask-closable="false" :footer="null" :width="460">
```

- [ ] **Step 2: 加日志样式**

在 `web/assets/css/custom.css` 末尾追加：

```css
.upgrade-log {
    max-height: 420px;
    overflow: auto;
    background: #1f1f1f;
    color: #d9d9d9;
    padding: 12px;
    border-radius: 4px;
    font-size: 12px;
    line-height: 1.6;
    white-space: pre-wrap;
    word-break: break-all;
}
```

- [ ] **Step 3: 跑模板测试**

```bash
go test ./web/ -run 'TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot' -v
```

两个 modal 必须落在 `#app` 内部——`commonSider` 模板整体就在 `#app` 里，插入位置在 `</a-layout-sider>` 之后仍然在 `<a-layout id="app">` 内，测试会验证这一点。

- [ ] **Step 4: 目视验证**

```bash
XUI_DEBUG=true go run main.go
```

本地不可更新（`updatable=false`），所以看不到更新按钮。用浏览器控制台强制打开进度框验证渲染：

```js
app.upgradeState = 'starting'; app.upgradeTarget = 'v1.5.0';
// 依次改成 'restarting' / 'done' / 'timeout' 检查四种状态
app.upgradeState = 'idle';   // 收尾
```

再验证日志弹窗：

```js
app.upgradeLogLines = ['line 1', 'line 2']; app.upgradeLogVisible = true;
```

- [ ] **Step 5: 提交**

```bash
git add web/html/xui/common_sider.html web/assets/css/custom.css
git commit -m "feat(version): 更新进度框与更新日志弹窗

点下更新后面板马上要自杀。没有这一层，管理员面对的是一个正在重启的面板
和一片空白；而更新失败时 install.sh 的 die_restoring_panel 会把面板重启
回来，表现与「点了没反应」完全一致，真正的原因只写在一个没人知道的文件里。

轮询期间请求失败不是错误，那正是「正在重启」的信号，要继续重试。"
```

---

### Task 8: 文档与全量门禁

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: 在 CLAUDE.md 里加一节**

在「## 运维脚本」之前插入：

```markdown
## 面板版本与一键更新

侧栏底部常驻版本号（`config.GetVersion()`），有新版打红点，点开可更新或回退到最近 5 个版本。设计文档在 `docs/superpowers/specs/2026-09-05-panel-version-update-design.md`。

**版本判定不做语义化解析。** 仓库 tag 格式不统一（`0.3.4.4` 与 `v1.2.10` 并存），字符串比较会把 `v1.2.9 > v1.2.10` 判反。改用 GitHub releases 列表的天然顺序：当前版本等于第 0 条即最新，在列表里且下标 > 0 即有更新，**不在列表里则既不打红点也不显示「已是最新」**（本地开发版 `config/version` 就是这种情况）。拉 `per_page=10` 但回退列表只给前 5 条——`KnownCurrent` 用全部 10 条判定，落后 6~10 个版本的管理员恰恰最需要看到红点。

**更新必须经 `systemd-run` 起独立 transient unit 执行，不能直接 `os/exec`。** 面板的子进程与面板同在 `/system.slice/a-ui.service` 这个 cgroup 里（实测确认 xray 就在里面），而 `install.sh` 会 `systemctl stop a-ui`，默认 `KillMode=control-group` 会把更新脚本一起杀掉——脚本死在 `rm -rf /usr/local/a-ui/` 前后，留下一台面板已删一半、服务已停、且因 `Restart=no` 不会自愈的机器，只能 SSH 上去手动重装。2026-09-05 实测：`systemd-run` 的 unit 在父 service 被 stop 后存活，而**加了 `setsid` 的对照组仍被杀死**——`setsid` 改的是会话不是 cgroup，不要拿它替代。

**tag 是命令注入路径，两道防线都必须硬拒绝。** 它会被拼进 `bash -c` 的字符串并以 root 执行：先过 `^[A-Za-z0-9._-]{1,64}$`，再必须精确出现在缓存的发布列表里。第二道顺带实现了「只能回退到最近 5 个版本」。这与 `routing_validate.go` 的 fail open 取向**相反且必须相反**：那里放行的是「没法证明非法」的配置，最坏后果是 xray 拒绝启动；这里放行的是一段以 root 执行的字符串。

**`Updatable` 前置检查挡住 Docker 与本地开发**（非 Linux / 找不到 `/usr/local/a-ui/a-ui` / 找不到 systemd 单元 / 没有 `systemd-run`）。在容器里跑 `install.sh` 是纯粹的破坏。`UnsupportedReason` 要具体到哪一条没过并原样显示给管理员。

**回退有两个后果，必须写进二次确认框**：① xray 核心会跟着回退——`install.sh` 解压的发版包带着 `bin/xray-linux-<arch>`，会覆盖机器上现有的那份（v1.2.8 之前的包里是 Xray 1.4.x 构建，没有 `RoutingService` 符号，配置热更新会静默失效）；② 数据库不回滚，`AutoMigrate` 只加列不删列，数据不丢但新功能失效。另有一条不写进 UI 但要记住的偏差：`install.sh` 无论装哪个版本，它自己和 `/usr/bin/a-ui` 都是从 **main 分支**拉的最新版，所以回退得到的是「旧二进制 + 新管理脚本」——改动 `a-ui bootstrap` / `a-ui setting` 的参数时要考虑这一点。

**版本缓存不落库。** 新增设置项要同步改 5 处，漏掉 `models.js` 那一处会让**整个保存配置接口失败**；为一份重启后 10 秒内自愈的缓存付这个代价不划算。`PanelVersionJob` 每 6 小时刷新，`Server.startTask` 里另有一个延迟 10 秒的首次触发——`cron.AddJob` 的首次执行在一个完整周期之后，不做延迟触发新装的面板要等 6 小时才显示版本状态。

**拉取失败保留上一次成功的数据，只写 `LastError`。** 清空会让界面从「有新版可更新」退回「尚未检查」，且 tag 白名单变空会让更新按钮全部失效——一次网络抖动就把功能整个关掉。同理 `CheckedAt` 表示「上次**成功**检查」的时刻，不被失败的刷新改写。

**前端版本区走 Vue mixin（`web/assets/js/util/panel-version.js`）。** `common_sider.html` 被四个页面共用，但每个页面各有一个 `new Vue({el:'#app'})`，data 互不相干——少给一个页面挂 mixin，那个页面就会引用 undefined。mixin 的 `data` 是函数、根实例的 `data` 是对象，Vue 的 `mergeDataOrFn` 会正确合并，不用改现有页面的写法。
```

- [ ] **Step 2: 跑全量门禁**

```bash
make verify
```

预期：`go vet ./...` 无输出，`go test ./...` 全绿，`go build` 产出 `a-ui`。

若 `web/service` 的测试因缺少 `bin/xray-darwin-arm64` 而 skip，那是既有行为（该文件在 `.gitignore` 里），不是本次改动引入的。

- [ ] **Step 3: 清理构建产物**

```bash
make clean
git status --short
```

预期：只有本次改动的文件，没有 `a-ui` 二进制、没有临时文件。

- [ ] **Step 4: 提交**

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md 增加面板版本与一键更新小节

记下三条最容易被后人改坏的约束：不做语义化版本解析（tag 格式不统一）、
更新必须经 systemd-run（setsid 实测无效）、tag 白名单与 routing_validate
的 fail open 取向相反且必须相反。"
```

- [ ] **Step 5: 生产机端到端验证**

这一步需要一个可接受的断网窗口——`install.sh` 会 `systemctl stop a-ui`，连带杀死同 cgroup 的 xray。

在测试机（或征得同意后在 `140.245.92.141`）上：

```bash
# 部署本分支构建出的二进制后
# 1. 面板里点「更新到最新版」，观察进度框走完 starting → restarting → done
# 2. 验收
sudo /usr/local/a-ui/a-ui -v                    # 版本号已变
sudo systemctl is-active a-ui                    # active
pgrep -a xray                                    # xray 恢复运行
sudo /usr/local/a-ui/a-ui setting -show          # basePath / 端口 / 账号未被改动
sudo ls -la /etc/a-ui/                           # 数据库文件未被触碰
systemctl is-active caddy                        # Caddy 配置未受影响
sudo tail -30 /var/log/a-ui-update.log           # 日志可读
# 3. 再点一次回退到上一个版本，重复上述验收
```

---

## Self-Review

**Spec 覆盖核对：**

| Spec 章节 | 落在哪个 Task |
|---|---|
| §3 版本比较（三种状态 + per_page=10） | Task 1 `computeVersionState`、Task 2 截断与 `KnownCurrent` |
| §3 过滤 draft/prerelease | Task 1 `toBriefs` |
| §4 缓存结构、不落库、延迟首次触发 | Task 2 `versionCache`、Task 4 |
| §5.1 systemd-run | Task 1 `buildUpgradeCommand`、Task 3 `Upgrade` |
| §5.2 内层脚本（`curl -f`、`</dev/null`、覆盖写日志） | Task 1，测试逐条断言 |
| §5.3 `Updatable` 四项前置检查 | Task 1 `checkUpdatable` |
| §5.4 tag 白名单双重校验 | Task 1 `validateUpgradeTag`、Task 3 执行前拦截 |
| §5.5 响应先于执行 | Task 3 注释 + Task 5 `upgradePanel` |
| §5.6 更新日志接口 | Task 3 `UpgradeLog`、Task 5 路由 |
| §6.1 Vue mixin | Task 6 |
| §6.2 两个既有陷阱 | Task 6 Step 6/7 的模板测试与 `XUI_DEBUG` 验证 |
| §6.3 布局贴底 | Task 6 Step 4 CSS |
| §6.4 弹出层结构 | Task 6 Step 3 |
| §6.5 更新过程可见性 | Task 7 |
| §7 回退的两个后果 | Task 6 `confirmUpgrade` 文案、Task 8 CLAUDE.md |
| §8 四个接口 | Task 5 |
| §9 四个决策 | 分散在各 Task 注释 + Task 8 文档 |
| §11 测试策略 | Task 1/2/3 的测试、Task 8 Step 5 端到端 |

无遗漏。

**类型一致性核对：** `ReleaseBrief` / `PanelVersionInfo` 字段名在 Task 1 定义，Task 2、5、6 一致引用（`tagName` / `publishedAt` / `htmlUrl` / `hasUpdate` / `knownCurrent` / `updatable` / `unsupportedReason`）。`panelVersionCache` 在 Task 2 定义、Task 3 经 `allowedTags()` 使用。`runUpgradeCommand` / `forceUpdatableForTest` / `upgradeLogPathForTest` / `upgradeLogTailLines` 在 Task 3 同时定义与使用。前端 `panelVersionMixin` 在 Task 6 定义，Task 7 只消费其 data。

**已知的执行期风险：** Task 6 Step 1 依赖 `PromiseUtil.sleep` 是否存在，该步已内嵌 grep 检查与替代写法。Task 7 Step 1 的 `v-model` 写法在同一步内已修正为 `:visible`。
