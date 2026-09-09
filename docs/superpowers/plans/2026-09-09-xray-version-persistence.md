# xray 核心版本不随 a-ui 更新回退 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让管理员升级过的 xray 核心与 geo 数据在 a-ui 更新／降级后原样保留，全新安装则直接装 GitHub 最新发布版（可能标着 prerelease，见 Task 2 的更正批注与 spec §4.2）。

**Architecture:** `install.sh` 在 `rm -rf` 安装目录之前把 `bin/` 下的 xray 与两个 geo 数据文件备份出来，解压后按「xray 二进制有没有备到」分岔：备到了就恢复，没备到（全新安装）就调用新增的 `a-ui xray -update latest` 子命令拉最新发布版。Go 侧把现有 `ServerService.UpdateXray` 的下载／解包／停机三件事拆开，让解包成为一个接受显式目标路径的包级函数，供面板按钮与新子命令共用，并且能脱网单测。

**Tech Stack:** Go 1.27（`go.mod` 为唯一事实来源）、`archive/zip`、`flag`、Bash（install.sh）

**Spec:** `docs/superpowers/specs/2026-09-09-xray-version-persistence-design.md`

## Global Constraints

- 构建必须 `CGO_ENABLED=1`（`gorm.io/driver/sqlite` 依赖 `mattn/go-sqlite3`）。
- 提交前门禁：`make verify`（= vet + test + build）。
- **不改 `.github/workflows/release.yml`**：发版包继续带 xray，角色是「拉不到时的兜底种子」。
- **不做 xray 自动定时升级**，不做版本号语义化比较。
- **提交必须按路径 `git add <具体文件>`**，这个仓库有并行会话，禁止 `git add -A` / `git add .`。
- 注释用简体中文，解释「为什么」而非复述代码；与仓库现有风格一致。
- `web/service` 包的 `TestMain`（`web/service/routing_validate_test.go:21`）会 `os.Chdir` 到**仓库根**。该包内任何写文件的测试都必须用 `t.TempDir()` 的绝对路径，**绝不能写相对路径 `bin/`**——那会覆盖仓库里真实的 xray 二进制。
- 子命令失败必须返回非 0 退出码（`main.go:321` 的既有约束：退出码 0 会被 `install.sh` 误判为成功）。

---

### Task 1: 把解包逻辑抽成可脱网测试的包级函数

现有 `UpdateXray`（`web/service/server.go:243-305`）把「下载 → 打开并验证 zip → 停核心 → 解包 → 重启核心」揉在一个函数里，解包那一段写死了 `xray.GetBinaryPath()` 等相对路径，没法测。这个任务只抽函数、不改行为。

**注意一处必须保留的既有语义**：`StopXray()` 排在 `zip.NewReader` **之后**（`server.go:267`）。下载几十 MB 和 zip 损坏检测都发生在停机之前，所以下载失败或包损坏时用户完全不断流。抽函数时必须保持这个顺序——spec §4.1 的伪代码把它简化成了 `StopXray; defer Restart; replaceXrayFiles(version)`，那会把停机提到下载之前，是行为退化。Task 5 会同步修正 spec 里那段伪代码。

**Files:**
- Modify: `web/service/server.go:243-305`
- Create: `web/service/server_xray_update_test.go`

**Interfaces:**
- Consumes: 无（本任务是起点）
- Produces:
  - `func openXrayZip(zipPath string) (*zip.Reader, func(), error)` — 打开并验证 zip，返回 reader 与清理函数（关闭文件句柄，**不删除** zip 文件）
  - `func extractXrayFiles(r *zip.Reader, binPath, geositePath, geoipPath string) error` — 把 `xray` / `geosite.dat` / `geoip.dat` 三个条目解到显式给出的三个路径

- [ ] **Step 1: 写失败的测试**

创建 `web/service/server_xray_update_test.go`：

```go
package service

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// writeTestZip 造一个内容可控的 zip，用来脱网验证解包逻辑。
func writeTestZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "xray.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建测试 zip 失败: %v", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	for name, content := range entries {
		e, err := w.Create(name)
		if err != nil {
			t.Fatalf("写 zip 条目 %s 失败: %v", name, err)
		}
		if _, err := e.Write([]byte(content)); err != nil {
			t.Fatalf("写 zip 条目 %s 内容失败: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭测试 zip 失败: %v", err)
	}
	return path
}

// 目标路径必须显式传入：本包的 TestMain 会 chdir 到仓库根，用相对路径
// bin/ 会直接覆盖仓库里真实的 xray 二进制。
func TestExtractXrayFilesWritesAllThreeTargets(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"xray":        "FAKE-XRAY-BINARY",
		"geosite.dat": "FAKE-GEOSITE",
		"geoip.dat":   "FAKE-GEOIP",
	})

	r, closeZip, err := openXrayZip(zipPath)
	if err != nil {
		t.Fatalf("openXrayZip: %v", err)
	}
	defer closeZip()

	dir := t.TempDir()
	binPath := filepath.Join(dir, "xray-linux-amd64")
	geositePath := filepath.Join(dir, "geosite.dat")
	geoipPath := filepath.Join(dir, "geoip.dat")

	if err := extractXrayFiles(r, binPath, geositePath, geoipPath); err != nil {
		t.Fatalf("extractXrayFiles: %v", err)
	}

	for path, want := range map[string]string{
		binPath:     "FAKE-XRAY-BINARY",
		geositePath: "FAKE-GEOSITE",
		geoipPath:   "FAKE-GEOIP",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("%s 内容期望 %q，实际 %q", path, want, string(got))
		}
	}
}

// zip 里缺 xray 条目时必须报错，且不能破坏已经存在的目标文件——
// 这是「宁可不更新，也不留下半个核心」。
func TestExtractXrayFilesMissingBinaryLeavesTargetsIntact(t *testing.T) {
	zipPath := writeTestZip(t, map[string]string{
		"geosite.dat": "FAKE-GEOSITE",
		"geoip.dat":   "FAKE-GEOIP",
	})

	r, closeZip, err := openXrayZip(zipPath)
	if err != nil {
		t.Fatalf("openXrayZip: %v", err)
	}
	defer closeZip()

	dir := t.TempDir()
	binPath := filepath.Join(dir, "xray-linux-amd64")
	if err := os.WriteFile(binPath, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatalf("预置旧二进制: %v", err)
	}

	err = extractXrayFiles(r, binPath, filepath.Join(dir, "geosite.dat"), filepath.Join(dir, "geoip.dat"))
	if err == nil {
		t.Fatal("zip 缺 xray 条目，期望报错，实际成功")
	}

	got, readErr := os.ReadFile(binPath)
	if readErr != nil {
		t.Fatalf("读取旧二进制: %v", readErr)
	}
	if string(got) != "OLD-BINARY" {
		t.Fatalf("解包失败时旧二进制被破坏，实际内容 %q", string(got))
	}
}

// 损坏的 zip 必须在 openXrayZip 这一步就被拒绝——UpdateXray 靠这一步
// 排在 StopXray 之前，才能做到「包坏了就不停核心」。
func TestOpenXrayZipRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.zip")
	if err := os.WriteFile(path, []byte("this is not a zip"), 0o644); err != nil {
		t.Fatalf("写损坏文件: %v", err)
	}

	if _, _, err := openXrayZip(path); err == nil {
		t.Fatal("损坏的 zip 期望报错，实际成功")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run 'TestExtractXrayFiles|TestOpenXrayZip' -v`
Expected: 编译失败，`undefined: openXrayZip`、`undefined: extractXrayFiles`

- [ ] **Step 3: 实现两个包级函数**

在 `web/service/server.go` 中 `UpdateXray` 之前插入：

```go
// openXrayZip 打开并验证一个 xray 发布 zip，返回 reader 与清理函数。
//
// 与解包分成两步，是为了让调用方能在「包已确认可读」和「开始写文件」之间
// 插入自己的动作——UpdateXray 正是在这个缝隙里停核心的：下载几十 MB 和
// 包损坏检测都发生在停机之前，用户完全不断流。
//
// 清理函数只关闭文件句柄，不删除 zip：zip 是谁下载的谁负责删。
func openXrayZip(zipPath string) (*zip.Reader, func(), error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	r, err := zip.NewReader(f, stat.Size())
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return r, func() { f.Close() }, nil
}

// extractXrayFiles 把 zip 里的 xray / geosite.dat / geoip.dat 解到三个显式
// 给出的路径。
//
// 目标路径是参数而不是直接取 xray.GetBinaryPath()：那些是相对路径，而本包
// 的测试会 chdir 到仓库根，写死就等于让测试覆盖仓库里真实的 xray 二进制。
//
// 条目不存在时在删除目标文件之前就返回，所以缺条目不会破坏已有的核心。
func extractXrayFiles(r *zip.Reader, binPath, geositePath, geoipPath string) error {
	copyZipFile := func(zipName string, fileName string) error {
		zipFile, err := r.Open(zipName)
		if err != nil {
			return err
		}
		defer zipFile.Close()
		os.Remove(fileName)
		file, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR|os.O_TRUNC, fs.ModePerm)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(file, zipFile)
		return err
	}

	if err := copyZipFile("xray", binPath); err != nil {
		return err
	}
	if err := copyZipFile("geosite.dat", geositePath); err != nil {
		return err
	}
	return copyZipFile("geoip.dat", geoipPath)
}
```

- [ ] **Step 4: 让 `UpdateXray` 改用这两个函数，保持停机时机不变**

把 `web/service/server.go:243-305` 的 `UpdateXray` 整体替换为：

```go
// UpdateXray 是面板「切换版本」按钮的入口：下载 → 验证 → 停核心 → 解包 → 重启。
//
// StopXray 必须排在 openXrayZip 之后：下载几十 MB 与包损坏检测都不该让用户
// 白断一次流。
func (s *ServerService) UpdateXray(version string) error {
	zipFileName, err := s.downloadXRay(version)
	if err != nil {
		return err
	}
	defer os.Remove(zipFileName)

	r, closeZip, err := openXrayZip(zipFileName)
	if err != nil {
		return err
	}
	defer closeZip()

	s.xrayService.StopXray()
	defer func() {
		err := s.xrayService.RestartXray(true)
		if err != nil {
			logger.Error("start xray failed:", err)
		}
	}()

	return extractXrayFiles(r, xray.GetBinaryPath(), xray.GetGeositePath(), xray.GetGeoipPath())
}
```

- [ ] **Step 5: 运行测试确认通过，并确认没有遗留未使用的 import**

Run: `go test ./web/service/ -run 'TestExtractXrayFiles|TestOpenXrayZip' -v && go vet ./web/service/`
Expected: 三个测试全 PASS，vet 无输出

- [ ] **Step 6: 跑一次全量门禁**

Run: `make verify`
Expected: 通过。若有失败，先确认是否本次改动引入——`git stash` 后重跑可以区分。

- [ ] **Step 7: 提交**

```bash
git add web/service/server.go web/service/server_xray_update_test.go
git commit -m "refactor(server): 拆出 openXrayZip/extractXrayFiles，让解包可脱网单测

解包目标路径改为显式参数，不再写死 xray.GetBinaryPath() 那几个相对路径:
本包的 TestMain 会 chdir 到仓库根，写死等于让测试覆盖仓库里真实的 xray
二进制。

UpdateXray 的行为不变，尤其保持 StopXray 排在 zip 验证之后——下载几十 MB
与包损坏检测都不该让用户白断一次流。"
```

---

### Task 2: 加「下载 + 解包但不碰进程」与「最新稳定版」两个入口

> **实施后更正（裁决 1，见 task-5-report）**：下面这个 Task 的 `/releases/latest` + `parseLatestTag` 方案在实现阶段被实测数据推翻，**没有落地**。2026-09-09 实测 `/releases/latest` 返回 `v26.3.27`（`prerelease=false`，2026-03-27 发布），而 xray-core 最近 15 个发布里 14 个标记为 `prerelease`——`/releases/latest` 会稳定给出一个比发版包自带核心（26.7.28）还旧的版本，直接违反本任务的目标。最终实现改为 `LatestXrayVersion()` 复用已有的 `GetXrayVersions()`（`/releases`）取首项（`firstReleaseTag`），与面板「切换版本」列表同源，见 `web/service/server.go`。下面的 Step 1/3/4 描述的 `parseLatestTag`／`latestXrayReleaseURL`／`/releases/latest` 均为**未采用的历史设计**，保留原文只为存执行记录，不代表最终行为——最终行为以 `docs/superpowers/specs/2026-09-09-xray-version-persistence-design.md` §4.2 与 `web/service/server.go` 为准。

**Files:**
- Modify: `web/service/server.go`（在 Task 1 新增的两个函数之后追加）
- Modify: `web/service/server_xray_update_test.go`

**Interfaces:**
- Consumes: `openXrayZip`、`extractXrayFiles`（Task 1）
- Produces:
  - `func (s *ServerService) ReplaceXrayFiles(version string) error` — 下载并解包，**不停也不启**核心
  - `func (s *ServerService) LatestXrayVersion() (string, error)` — 取 GitHub 最新稳定版 tag
  - `func parseLatestTag(body []byte) (string, error)` — 纯解析函数，供测试用

- [ ] **Step 1: 写失败的测试**

追加到 `web/service/server_xray_update_test.go`：

```go
// GitHub /releases/latest 的响应里 tag_name 是唯一需要的字段。
func TestParseLatestTag(t *testing.T) {
	t.Run("正常响应", func(t *testing.T) {
		got, err := parseLatestTag([]byte(`{"tag_name":"v26.9.9","name":"Xray v26.9.9"}`))
		if err != nil {
			t.Fatalf("parseLatestTag: %v", err)
		}
		if got != "v26.9.9" {
			t.Fatalf("期望 v26.9.9，实际 %q", got)
		}
	})

	t.Run("tag_name 为空要报错", func(t *testing.T) {
		// GitHub 限流时会返回一个带 message 字段的 JSON 对象而不是 release，
		// 解析本身不会失败，必须靠这一条挡住——否则会拿空字符串去拼下载 URL。
		if _, err := parseLatestTag([]byte(`{"message":"API rate limit exceeded"}`)); err == nil {
			t.Fatal("tag_name 缺失，期望报错，实际成功")
		}
	})

	t.Run("非 JSON 要报错", func(t *testing.T) {
		if _, err := parseLatestTag([]byte(`<html>502</html>`)); err == nil {
			t.Fatal("非 JSON，期望报错，实际成功")
		}
	})
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./web/service/ -run TestParseLatestTag -v`
Expected: 编译失败，`undefined: parseLatestTag`

- [ ] **Step 3: 实现三个函数**

在 `web/service/server.go` 中 `UpdateXray` 之后追加：

```go
// latestXrayReleaseURL 取的是 /releases/latest 而不是 GetXrayVersions 用的
// /releases：后者含 prerelease 与 draft，面板上让管理员自己挑没问题，
// 自动安装则不该把 pre-release 装到生产机上。
const latestXrayReleaseURL = "https://api.github.com/repos/XTLS/Xray-core/releases/latest"

// parseLatestTag 从 /releases/latest 的响应里取出 tag。
//
// 单独成函数是为了能脱网测试。空 tag 必须报错：GitHub 限流时返回的是一个
// 带 message 字段的 JSON 对象，反序列化不会失败，放过去就会拿空字符串去拼
// 下载 URL，最终得到一个 404 页面被当成 zip。
func parseLatestTag(body []byte) (string, error) {
	release := new(Release)
	if err := json.Unmarshal(body, release); err != nil {
		return "", err
	}
	if release.TagName == "" {
		return "", common.NewError("GitHub 未返回 xray 版本号，可能是超出 API 限制:", string(body))
	}
	return release.TagName, nil
}

// LatestXrayVersion 返回 xray 的最新稳定版 tag。
func (s *ServerService) LatestXrayVersion() (string, error) {
	resp, err := http.Get(latestXrayReleaseURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseLatestTag(body)
}

// ReplaceXrayFiles 下载指定版本并替换 bin/ 下的三个文件，不停也不启核心。
//
// 供 a-ui xray 子命令使用：那是个一次性进程，它用 os/exec 起的 xray 会随
// 进程退出一起死掉，所以绝不能在这里重启核心。安装脚本随后的
// systemctl restart a-ui 会让面板自己把核心拉起来。
func (s *ServerService) ReplaceXrayFiles(version string) error {
	zipFileName, err := s.downloadXRay(version)
	if err != nil {
		return err
	}
	defer os.Remove(zipFileName)

	r, closeZip, err := openXrayZip(zipFileName)
	if err != nil {
		return err
	}
	defer closeZip()

	return extractXrayFiles(r, xray.GetBinaryPath(), xray.GetGeositePath(), xray.GetGeoipPath())
}
```

`web/service/server.go` 目前**没有** import `a-ui/util/common`（已核实），在文件顶部 import 块的 `"a-ui/logger"` 之后加一行 `"a-ui/util/common"`。该包已有 `func NewError(a ...interface{}) error`，签名对得上。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./web/service/ -run 'TestParseLatestTag|TestExtractXrayFiles|TestOpenXrayZip' -v && go vet ./web/service/`
Expected: 全部 PASS，vet 无输出

- [ ] **Step 5: 提交**

```bash
git add web/service/server.go web/service/server_xray_update_test.go
git commit -m "feat(server): 加 ReplaceXrayFiles 与 LatestXrayVersion

ReplaceXrayFiles 下载并解包但不碰进程，供后续的 a-ui xray 子命令使用——
一次性进程用 os/exec 起的 xray 会随进程退出一起死掉。

LatestXrayVersion 取 /releases/latest 而不是 GetXrayVersions 用的 /releases:
后者含 prerelease，自动安装不该把 pre-release 装到生产机上。tag 为空必须
报错，GitHub 限流时返回的是带 message 字段的 JSON，放过去会拿空字符串去拼
下载 URL。"
```

---

### Task 3: 新增 `a-ui xray` 子命令

**Files:**
- Modify: `main.go`（`flag.Usage` 的 Commands 列表、`switch os.Args[1]` 的 case 与 default 提示、文件末尾追加两个函数）
- Modify: `main_flags_test.go`

**Interfaces:**
- Consumes: `(*service.ServerService).ReplaceXrayFiles`、`(*service.ServerService).LatestXrayVersion`（Task 2）
- Produces:
  - `func parseXrayFlags(args []string) (xrayFlags, error)`，其中 `type xrayFlags struct { Update string }`
  - 命令行接口：`a-ui xray -update latest` / `a-ui xray -update v26.9.9`

- [ ] **Step 1: 写失败的测试**

追加到 `main_flags_test.go`：

```go
func TestParseXrayFlags(t *testing.T) {
	t.Run("update latest", func(t *testing.T) {
		f, err := parseXrayFlags([]string{"-update", "latest"})
		if err != nil {
			t.Fatalf("parseXrayFlags: %v", err)
		}
		if f.Update != "latest" {
			t.Fatalf("期望 latest，实际 %q", f.Update)
		}
	})

	t.Run("update 指定版本", func(t *testing.T) {
		f, err := parseXrayFlags([]string{"-update", "v26.9.9"})
		if err != nil {
			t.Fatalf("parseXrayFlags: %v", err)
		}
		if f.Update != "v26.9.9" {
			t.Fatalf("期望 v26.9.9，实际 %q", f.Update)
		}
	})

	t.Run("未传 -update 时为空", func(t *testing.T) {
		f, err := parseXrayFlags(nil)
		if err != nil {
			t.Fatalf("parseXrayFlags: %v", err)
		}
		if f.Update != "" {
			t.Fatalf("期望空串，实际 %q", f.Update)
		}
	})

	t.Run("未知参数要报错", func(t *testing.T) {
		if _, err := parseXrayFlags([]string{"-nope"}); err == nil {
			t.Fatal("未知参数期望报错，实际成功")
		}
	})
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test . -run TestParseXrayFlags -v`
Expected: 编译失败，`undefined: parseXrayFlags`

- [ ] **Step 3: 实现 flag 解析与执行函数**

在 `main.go` 末尾追加：

```go
type xrayFlags struct {
	Update string
}

// parseXrayFlags 用 ContinueOnError 而不是 ExitOnError，是为了让上面那组
// 测试能拿到 error 而不是让进程直接退出；退出码语义由调用方 runXrayUpdate
// 按错误类型补齐，与 parseSettingFlags 的处理方式一致。
func parseXrayFlags(args []string) (xrayFlags, error) {
	var f xrayFlags
	cmd := flag.NewFlagSet("xray", flag.ContinueOnError)
	cmd.StringVar(&f.Update, "update", "", "更新 xray 核心，值为 latest 或具体版本号（如 v26.9.9）")
	if err := cmd.Parse(args); err != nil {
		return f, err
	}
	return f, nil
}

// runXrayUpdate 是安装脚本用来装 xray 核心的入口。
//
// 不连数据库：ReplaceXrayFiles 不需要。也刻意不重启核心——这是个一次性
// 进程，它起的 xray 会随进程退出一起死掉，安装脚本随后的
// systemctl restart a-ui 会让面板自己把核心拉起来。
//
// 依赖当前工作目录：xray.GetBinaryPath() 返回的是相对路径 bin/xray-…，
// 与 systemd 的 WorkingDirectory=/usr/local/a-ui/ 一致，所以调用方必须
// 先 cd 到安装根目录。
func runXrayUpdate(args []string) {
	f, err := parseXrayFlags(args)
	if err != nil {
		// flag 包在 ContinueOnError 下已经打印过错误与 usage，这里只补退出码。
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}

	if f.Update == "" {
		fmt.Println("用法: a-ui xray -update latest | -update <版本号>")
		os.Exit(2)
	}

	var serverService service.ServerService
	version := f.Update
	if version == "latest" {
		version, err = serverService.LatestXrayVersion()
		if err != nil {
			fmt.Println("获取 xray 最新版本失败:", err)
			os.Exit(1)
		}
	}

	if err := serverService.ReplaceXrayFiles(version); err != nil {
		fmt.Println("更新 xray 失败:", err)
		os.Exit(1)
	}
	fmt.Println("xray 核心已更新到", version)
}
```

确认 `main.go` 顶部已 import `"a-ui/web/service"`（`main_flags_test.go` 已经 import 了它，主文件若没有则补上）。

- [ ] **Step 4: 挂上子命令分派与帮助文案**

在 `main.go` 的 `flag.Usage` 里，`bootstrap` 那行之后追加：

```go
		fmt.Println("    xray           安装/更新 xray 核心（-update latest | -update <版本号>）")
```

在 `switch os.Args[1]` 中 `case "bootstrap":` 之后追加：

```go
	case "xray":
		runXrayUpdate(os.Args[2:])
```

把 `default` 分支的提示改为：

```go
		fmt.Println("except 'run' or 'v2-ui' or 'setting' or 'tc-clear' or 'bootstrap' or 'xray' subcommands")
```

- [ ] **Step 5: 运行测试与构建确认通过**

Run: `go test . -run TestParseXrayFlags -v && CGO_ENABLED=1 go build -o /tmp/a-ui-plan-check main.go && /tmp/a-ui-plan-check xray && rm -f /tmp/a-ui-plan-check`
Expected: 测试 PASS；构建成功；无参数执行 `xray` 子命令打印用法并以退出码 2 结束（`echo $?` 为 2）

- [ ] **Step 6: 跑一次全量门禁**

Run: `make verify`
Expected: 通过

- [ ] **Step 7: 提交**

```bash
git add main.go main_flags_test.go
git commit -m "feat(cli): 加 a-ui xray 子命令，供安装脚本装最新核心

不连数据库，也刻意不重启核心：这是个一次性进程，它用 os/exec 起的 xray
会随进程退出一起死掉，安装脚本随后的 systemctl restart a-ui 会让面板自己
把核心拉起来。

依赖当前工作目录——xray.GetBinaryPath() 是相对路径 bin/xray-…，调用方
必须先 cd 到安装根目录。"
```

---

### Task 4: install.sh 备份与恢复

这个任务**没有自动化测试**（spec §9.2）：CI 只跑 `make verify`，仓库里没有 shell 测试也没有 shellcheck。改动又恰好落在 `rm -rf` 前后，是整个脚本风险最高的一段。因此本任务的验收靠 `bash -n` 语法检查加一份人工验证清单，且**必须先在干净的 VPS 或容器上验证，不能直接上生产机**。

**Files:**
- Modify: `install.sh`（新增两个函数；在 `systemctl stop a-ui` 之前与 `chmod +x` 之后各加一个调用点）

**Interfaces:**
- Consumes: `a-ui xray -update latest`（Task 3）
- Produces: shell 函数 `backup_xray_assets`、`restore_or_install_xray`，共享全局变量 `xray_backup_dir`

- [ ] **Step 1: 加两个函数**

在 `install.sh` 的 `install_a-ui()` 函数**之前**插入：

```bash
# 备份 xray 核心与 geo 数据，供解压后恢复。xray_backup_dir 为空表示没有备份。
#
# 存在的理由：install.sh 会 rm -rf 整个安装目录再铺开发版包，而发版包里带着
# 仓库中那份 xray（见 release.yml 打包步骤）。管理员在面板里升级过的核心，
# 以及 a-ui geo 换成的 Loyalsoldier 增强版 geo 数据，都会被静默打回。
#
# 放在 systemctl stop 之前：这一步要写磁盘、可能失败，而失败必须能在面板
# 尚未停机时干净退出。复制正在被 xray 使用的可执行文件是安全的——cp 读的是
# 文件内容，不影响已经打开的 inode。
backup_xray_assets() {
    xray_backup_dir=""
    [[ ! -d /usr/local/a-ui/bin ]] && return 0

    local dir
    dir=$(mktemp -d) || die_restoring_panel "创建 xray 备份目录失败，已中止更新（安装目录未被改动）"

    local f
    for f in "xray-linux-${arch}" geoip.dat geosite.dat; do
        if [[ -f "/usr/local/a-ui/bin/${f}" ]]; then
            if ! cp -p "/usr/local/a-ui/bin/${f}" "${dir}/${f}"; then
                rm -rf "${dir}"
                die_restoring_panel "备份 ${f} 失败，已中止更新（安装目录未被改动）"
            fi
        fi
    done

    xray_backup_dir="${dir}"
}

# 恢复备份的 xray 与 geo 数据；全新安装则装 GitHub 最新稳定版。
#
# 判据是 xray 二进制有没有备到，不看 geo：核心诉求是 xray 版本，geo 是附带的，
# 两者可能只成功一半（管理员删过其中某个文件，或上一次安装本身就是坏的）。
#
# 恢复与拉取两条路径都 fail open——退回发版包里那份 xray 继续安装。它是能用的，
# 不该为了「装到最新」而让整个安装失败。
restore_or_install_xray() {
    cd /usr/local/a-ui || die_restoring_panel "进入安装目录失败"

    if [[ -n "${xray_backup_dir}" && -f "${xray_backup_dir}/xray-linux-${arch}" ]]; then
        local f
        for f in "xray-linux-${arch}" geoip.dat geosite.dat; do
            if [[ -f "${xray_backup_dir}/${f}" ]]; then
                cp -pf "${xray_backup_dir}/${f}" "/usr/local/a-ui/bin/${f}" \
                    || echo -e "${yellow}警告: 恢复 ${f} 失败，将使用安装包内自带的版本${plain}"
            fi
        done
        echo -e "${green}已保留原有的 xray 核心与 geo 数据${plain}"
    else
        echo "全新安装，正在获取最新版 xray 核心..."
        if ! /usr/local/a-ui/a-ui xray -update latest; then
            echo -e "${yellow}警告: 获取最新版 xray 失败，将使用安装包内自带的版本${plain}"
            echo -e "${yellow}      装好后可在面板首页「切换版本」手动升级${plain}"
        fi
    fi

    chmod +x "/usr/local/a-ui/bin/xray-linux-${arch}"
    [[ -n "${xray_backup_dir}" ]] && rm -rf "${xray_backup_dir}"
    xray_backup_dir=""
}
```

- [ ] **Step 2: 加第一个调用点（备份，在停机之前）**

`install.sh:1168` 现在是 `systemctl stop a-ui`。在它**之前**插入一行（保持缩进为 4 个空格）：

```bash
    backup_xray_assets

    systemctl stop a-ui
    a_ui_stopped=1
```

- [ ] **Step 3: 加第二个调用点（恢复，在解压之后）**

`install.sh:1178` 现在是 `chmod +x a-ui bin/xray-linux-${arch}`。在它**之后**插入一行：

```bash
    chmod +x a-ui bin/xray-linux-${arch}
    restore_or_install_xray
    cp -f a-ui.service /etc/systemd/system/
```

- [ ] **Step 4: 语法检查**

Run: `bash -n install.sh && echo "语法 OK"`
Expected: 打印「语法 OK」

- [ ] **Step 5: 静态自查三条易错点**

逐条确认，不满足就回到 Step 1~3 修：

1. `backup_xray_assets` 的调用点确实在 `systemctl stop a-ui` **之前**（此时 `a_ui_stopped` 仍是 0，`die_restoring_panel` 不会去重启一个从没停过的面板）。
2. `restore_or_install_xray` 的调用点在 `cd a-ui` **之后**，因此 `a-ui xray -update latest` 执行时 cwd 是 `/usr/local/a-ui`（函数里那句 `cd /usr/local/a-ui` 是双保险）。
3. 两个函数定义都在 `install_a-ui()` **之前**，且在 `arch` 变量赋值（`install.sh:65`）之后被调用。

- [ ] **Step 6: 提交**

```bash
git add install.sh
git commit -m "feat(install): 更新面板时保留已有的 xray 核心与 geo 数据

install.sh 会 rm -rf 整个安装目录再铺开发版包，而发版包里带着仓库中那份
xray。管理员在面板里升过的核心、以及 a-ui geo 换成的 Loyalsoldier 增强版
geo 数据，此前都会被静默打回，面板上还看不出来。

判据是 xray 二进制有没有备到:备到了就恢复（更新/降级），没备到就装 GitHub
最新稳定版（全新安装）。这样更新面板绝不顺手改动 xray 版本——升级要停核心
重启，而 Process.Start() 不回传启动失败，这种操作必须由管理员挑时间做。

备份失败是唯一 fail close 的一步:不执行 rm -rf，走 die_restoring_panel，
此时面板尚未停机。恢复失败与拉取失败都退回发版包那份继续安装。"
```

- [ ] **Step 7: 人工验证（在干净的 VPS 或容器上，不要用生产机）**

按 spec §9.2 逐项跑，每项记录实际结果：

1. 全新安装 → `go version -m /usr/local/a-ui/bin/xray-linux-<arch>` 或面板首页确认装上的是 GitHub 最新**发布版**（不是「最新稳定版」——xray-core 几乎所有发布都标 prerelease，核对应与 `/releases` 首条比对，不是 `/releases/latest`；以 spec §9.2 现在的说法为准），不是发版包里那份。
2. 面板「切换版本」切到一个**更旧**的版本 → `a-ui update` → 确认核心仍是那个旧版本。这一项证明的是「保留」而不是「总是装最新」。
3. 降级 a-ui 到上一个 tag → 确认核心不变。
4. 把 `api.github.com` 指到黑洞（`echo '0.0.0.0 api.github.com' >> /etc/hosts`）后全新安装 → 确认打印警告、退回发版包那份、**安装照常成功**；验证完记得把这行删掉。
5. 让备份失败（`mount -o ro` 一个只读 tmpfs 到 `/tmp`，或直接把 `mktemp -d` 改成必然失败的路径试一次）→ 确认在 `rm -rf` **之前**就退出，`/usr/local/a-ui/` 完好，面板照常运行。

第 5 项最关键——它验证的是唯一一条 fail close 的路径。

---

### Task 5: 同步文档

**Files:**
- Modify: `CLAUDE.md`（「运维脚本」一节、「已知偏差与注意事项」一节）
- Modify: `docs/superpowers/specs/2026-09-09-xray-version-persistence-design.md`（§4.1 伪代码）

**Interfaces:**
- Consumes: 前四个任务的最终实现
- Produces: 无代码接口

- [ ] **Step 1: 修正 spec §4.1 的伪代码**

spec 里 `UpdateXray` 那段写成了 `StopXray; defer Restart; replaceXrayFiles(version)`，会把停机提到下载之前。改成 Task 1 Step 4 的真实实现，并把函数签名对齐为 `openXrayZip` / `extractXrayFiles(r *zip.Reader, binPath, geositePath, geoipPath string)` / `ReplaceXrayFiles`（导出，子命令要跨包调用）。§9.1 里的函数名一并对齐。

- [ ] **Step 2: 改写 CLAUDE.md「运维脚本」一节**

把这句：

> 因此升级 `xray-core` 依赖时，必须同时把 `bin/xray-linux-amd64` 与 `bin/xray-linux-arm64` 换成同版本的官方构建，否则面板内的 `infra/conf` 与用户机器上实际运行的核心会错版。

改写为（要点，措辞随上下文调整）：`bin/xray-*` 的角色已变成**发版包里的兜底种子**——全新安装会装 GitHub 最新稳定版，更新／降级则保留机器上原有的核心，两条路径都不会让用户机器上实际运行的核心退回到这一份。仍然建议它与 `go.mod` 的 `xray-core` 同版本，但必须写清耦合方向：面板内的 `infra/conf` 把配置编译成 typed message 下发，所以「面板旧 + 核心新」只是用不上核心的新协议（安全侧），「面板新 + 核心旧」才会生成核心不认识的配置。不写清方向，以后有人会为了「同版本」去主动降级用户机器上的核心。

- [ ] **Step 3: 改写 CLAUDE.md「已知偏差与注意事项」一节**

那段以「**`a-ui update` 会把仓库里的 `bin/xray-*` 覆盖到用户机器上…**」开头的文字，描述的是本次改动之前的行为，改写为新行为：更新／降级会保留机器上原有的核心与 geo 数据，全新安装装最新稳定版；发版包里那份只在拉取失败时兜底。顺带记下一个被一并修掉的坑：降级到 v1.2.8 之前的面板不再会把核心一起降到没有 `RoutingService` 符号的 Xray 1.4.x。

- [ ] **Step 4: 确认文档与实现一致**

Run: `grep -n "extractXrayFiles\|ReplaceXrayFiles\|openXrayZip" docs/superpowers/specs/2026-09-09-xray-version-persistence-design.md web/service/server.go`
Expected: spec 里出现的函数名与签名和 `server.go` 里的实现完全一致（导出与否、参数个数都要对上）

- [ ] **Step 5: 提交**

```bash
git add CLAUDE.md docs/superpowers/specs/2026-09-09-xray-version-persistence-design.md
git commit -m "docs: 同步 xray 核心保留机制，写清面板与核心的耦合方向

bin/xray-* 的角色变成发版包里的兜底种子。仍建议与 go.mod 同版本，但必须
写清方向:面板内的 infra/conf 把配置编译成 typed message 下发，所以
「面板旧 + 核心新」只是用不上新协议（安全侧），「面板新 + 核心旧」才危险。
不写清方向，以后会有人为了「同版本」去主动降级用户机器上的核心。

顺带修正 spec §4.1 的伪代码:它把 StopXray 简化到了下载之前，那会让用户在
下载几十 MB 期间白断流。"
```

---

## 完成标准

- `make verify` 通过。
- `bash -n install.sh` 通过。
- spec §9.2 的五项人工验证全部跑过且记录了结果，**第 5 项（备份失败时不执行 `rm -rf`）必须实测**。
- `git log` 里五次提交各自独立、信息完整。
- 未改动 `.github/workflows/release.yml`。
