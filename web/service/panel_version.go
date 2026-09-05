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
