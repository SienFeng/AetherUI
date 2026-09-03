# 依赖与工具链升级 + 配置热更新 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 AetherUI 迁到 Go 1.27 + 与运行中核心同版本的 `xray-core`，在此基础上让入站、出站、路由规则的改动通过 xray 的 gRPC API 热应用，不再重启进程断线。

**Architecture:** 分两个阶段。阶段一只做依赖与工具链迁移，交付物是「行为完全不变、测试全绿、跑在现代工具链上」，可单独上线。阶段二在新核心的 `infra/conf` 解析器之上新增 `xray/api.go`（gRPC 客户端封装）与 `xray/hot_diff.go`（新旧 `Config` 的差分计算），由 `XrayService.RestartXray` 在重启前先尝试热应用，任何一步不确定就退回原来的全量重启——**热更新是优化，不是新的失败点**。

**Tech Stack:** Go 1.27、`github.com/xtls/xray-core` 26.x（`app/proxyman/command` HandlerService、`app/router/command` RoutingService、`infra/conf` 配置构建器）、`google.golang.org/grpc`、标准库 `testing`。

**Spec:** `docs/superpowers/specs/2026-09-03-modernization-roadmap.md`（§2 决定性技术前提、§3 计划 01、§4 全局约束）

## Global Constraints

- 目标 Go 版本 **1.27.0**；`go.mod` 的 `go` 指令是唯一事实来源，CI 一律用 `go-version-file: go.mod`，不再硬编码版本号。
- `xray-core` 升到与 `bin/xray-*` 实际版本匹配的发行版，版本号在 Task 4 用 `go list -m -versions` 与 `./bin/xray-<GOOS>-<GOARCH> -version` 对照选定，**不凭记忆写死**。
- 除 `xray-core` 及其强制连带（grpc / protobuf / `golang.org/x/*`）外，**不主动升级其他依赖**。gin 1.7.1、gorm 1.21.9、gopsutil v3.21.3 保持原样，除非在新工具链下确实编译失败——那时才升，单独成任务、单独提交。
- 锁文件（`go.sum`）只能由 `go get` / `go mod tidy` 生成，不手工编辑。
- **生成逐字节确定**：出站按 id 升序、规则按 `priority asc, id asc`、`InboundIds` 升序去重。禁止遍历 map 产生数组顺序。破坏它会让 `Config.Equals` 恒为 false，那个 10 秒的重启 cron 会不停重启 xray。
- 新增会影响 xray 配置的字段时，必须同步扩展 `Config.Equals` / `InboundConfig.Equals`（`xray/config.go`）。
- 一个测试必须在没有其修复时失败。写完先 revert 实现看它变红，再恢复。
- `web/service` 的 `TestMain` 会 `os.Chdir` 到仓库根（`xray.GetBinaryPath()` 返回相对路径）。该包内新增依赖包内相对路径的测试要改用 `t.TempDir()` 或绝对路径。
- 临时验证产物一律放 `/private/tmp/claude-501/-Users-caryallen-Desktop-AetherUI-AetherUI-main/c74ccce5-9df3-4252-a475-129aaea8caf7/scratchpad`，不进仓库。任务完成前用 `git status` 与最终 diff 核对。
- Conventional Commits，中文正文说明「为什么」。

## 基线（2026-09-03，`main` @ `601a344`，务必先复现）

| 项 | 值 |
|---|---|
| `go version` | `go1.27.1 darwin/arm64` |
| `go.mod` 声明 | `go 1.21` |
| CI 构建 Go | `1.22`（`.github/workflows/release.yml:44`） |
| `CGO_ENABLED=1 go build -o /tmp/a-ui main.go` | 通过 |
| `go test ./...` | 全绿（8 个包有测试） |
| 未 strip 的本地二进制 | 33 MB |
| `xray-core` 唯一使用点 | `xray/process.go:11` `statsservice`，只用于 `GetTraffic` |

---

## 文件结构

**阶段一**

| 文件 | 动作 | 职责 |
|---|---|---|
| `Makefile` | 新建 | 一键构建 / 测试 / 验证门禁 |
| `.github/workflows/ci.yml` | 新建 | push / PR 时跑 `make verify` |
| `.github/workflows/release.yml` | 改 `:44` | Go 版本改从 `go.mod` 读 |
| `util/sys/psutil.go` | 重写 | 用标准库实现 `HostProc`，删掉 `//go:linkname` |
| `util/sys/psutil_test.go` | 新建 | `HostProc` 的表驱动测试 |
| `go.mod` / `go.sum` | 改 | Go 1.27.0 + xray-core 26.x |
| `xray/process.go` | 改 `:235` | 适配新版 grpc 客户端 API |

**阶段二**

| 文件 | 动作 | 职责 |
|---|---|---|
| `xray/api.go` | 新建 | gRPC 客户端封装：Init/Close、增删入站出站、下发路由 |
| `xray/api_test.go` | 新建 | 客户端未初始化时的错误路径 |
| `xray/hot_diff.go` | 新建 | 纯函数：比较新旧 `Config`，产出可热应用的操作集，或判定「必须重启」 |
| `xray/hot_diff_test.go` | 新建 | 差分计算的表驱动测试（本计划测试量最大的一块） |
| `xray/process.go` | 改 | 新增 `SetConfig()`，供热应用后同步进程内的配置快照（`GetAPIPort()` / `GetConfig()` 已存在） |
| `web/service/config.json` | 改 `:3` | 默认模板的 `api.services` 加 `RoutingService` |
| `web/service/setting.go` | 改 | 存量模板的幂等迁移 |
| `web/service/setting_api_services_test.go` | 新建 | 迁移的幂等性与不越权（不动用户业务配置） |
| `web/service/xray.go` | 改 `:103` | `RestartXray` 先试热应用，失败退回重启 |
| `web/service/xray_hot_apply_test.go` | 新建 | 热应用失败时确实退回重启 |

---

# 阶段一：依赖与工具链升级

阶段一结束时，行为与升级前完全一致。这是**可以单独发一个版本**的交付。

---

### Task 1: 建立验证门禁

升级最大的风险不是改错，是改错了看不出来——现在 CI 只 build 不 test，本地也没有一键入口。先把门禁立起来，后面每一步才有对照。

**Files:**
- Create: `Makefile`
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Produces: `make verify`（= `go vet ./...` + `go test ./...` + 构建），后续每个 Task 的验收都调它。

- [ ] **Step 1: 记录升级前的基线，存到 scratchpad**

```bash
cd /Users/caryallen/Desktop/AetherUI/AetherUI-main
SCRATCH=/private/tmp/claude-501/-Users-caryallen-Desktop-AetherUI-AetherUI-main/c74ccce5-9df3-4252-a475-129aaea8caf7/scratchpad
go version | tee "$SCRATCH/baseline-go-version.txt"
go test ./... 2>&1 | tee "$SCRATCH/baseline-test.txt"
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o "$SCRATCH/a-ui-before" main.go
ls -l "$SCRATCH/a-ui-before" | tee "$SCRATCH/baseline-size.txt"
```

期望：`baseline-test.txt` 里没有 `FAIL`。若有，**停下来先查清楚**——不能把一个本来就红的测试带进升级。

- [ ] **Step 2: 写 `Makefile`**

```makefile
# AetherUI 构建与验证入口。
#
# CGO 必须开启：gorm.io/driver/sqlite 依赖 mattn/go-sqlite3，
# CGO_ENABLED=0 的构建会在运行时打不开数据库。
export CGO_ENABLED := 1

BINARY := a-ui

.PHONY: help build test vet verify clean

help:
	@echo "make build   编译 $(BINARY)"
	@echo "make test    跑全部 Go 测试"
	@echo "make vet     go vet ./..."
	@echo "make verify  vet + test + build，提交前的门禁"
	@echo "make clean   删除构建产物"

build:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) main.go

# web/service 的 TestMain 会 chdir 到仓库根，需要 bin/xray-$(GOOS)-$(GOARCH) 在位。
test:
	go test ./...

vet:
	go vet ./...

verify: vet test build

clean:
	rm -f $(BINARY)
```

- [ ] **Step 3: 跑门禁，确认它能跑通**

```bash
make verify
```

期望：`go vet` 无输出（或只有既有告警——若有，抄进 `$SCRATCH/baseline-vet.txt` 作为基线，本计划不修它们），测试全绿，`./a-ui` 生成。

- [ ] **Step 4: 写 CI 工作流**

`.github/workflows/ci.yml`：

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:

jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      # Go 版本以 go.mod 为唯一事实来源，不在这里硬编码，
      # 否则抬升 go.mod 时会漏掉工作流，CI 用旧版本跑出假绿。
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: verify
        run: make verify
```

- [ ] **Step 5: 确认 CI 需要的 xray 二进制可执行**

`web/service` 的测试会调 `bin/xray-linux-amd64 run -test`。git 里该文件的 mode 必须是 `100755`：

```bash
git ls-files -s bin/xray-linux-amd64
```

期望：以 `100755` 开头。若是 `100644`，执行 `git update-index --chmod=+x bin/xray-linux-amd64` 并把它一起提交。

- [ ] **Step 6: 清理构建产物并提交**

```bash
make clean
git status   # 确认只有 Makefile 与 .github/workflows/ci.yml 两个新文件
git add Makefile .github/workflows/ci.yml
git commit -m "chore: 新增 Makefile 与 CI 验证门禁

升级工具链前先立对照。此前 CI 只编译不跑测试，本地也没有一键入口，
改动是否引入退化只能靠人工逐包跑。CI 的 Go 版本改从 go.mod 读取，
避免之后抬升 go.mod 时漏改工作流、拿旧版本跑出假绿。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 2: 消除侵入 gopsutil 内部包的 `//go:linkname`

CLAUDE.md 把「`util/sys/psutil.go` 用 `//go:linkname` 侵入 gopsutil 内部包，升级 gopsutil 时会断」记为已知偏差。查证 gopsutil v3.21.3 的 `internal/common/common.go:331`，这个函数体只有三件事：读 `HOST_PROC` 环境变量、缺省 `/proc`、`filepath.Join`。调用点只有 `util/sys/sys_linux.go:43` 和 `:58`。直接实现掉比在升级时维护它便宜得多，且永久消除这条偏差。

**Files:**
- Modify: `util/sys/psutil.go`（整文件重写）
- Create: `util/sys/psutil_test.go`

**Interfaces:**
- Produces: `sys.HostProc(combineWith ...string) string` — 签名与语义和替换前完全一致，`util/sys/sys_linux.go` 的两个调用点无需改动。

- [ ] **Step 1: 写失败的测试**

创建 `util/sys/psutil_test.go`：

```go
package sys

import "testing"

func TestHostProc(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		combineWith []string
		want        string
	}{
		{"缺省根", "", nil, "/proc"},
		{"缺省根拼一段", "", []string{"net/tcp"}, "/proc/net/tcp"},
		{"缺省根拼多段", "", []string{"net", "tcp6"}, "/proc/net/tcp6"},
		{"环境变量覆盖根", "/host/proc", nil, "/host/proc"},
		{"环境变量覆盖并拼接", "/host/proc", []string{"net/udp"}, "/host/proc/net/udp"},
		{"路径含冗余分隔符会被清理", "/host/proc/", []string{"/net/udp6"}, "/host/proc/net/udp6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOST_PROC", tt.env)
			if got := HostProc(tt.combineWith...); got != tt.want {
				t.Fatalf("HostProc(%q) = %q，期望 %q", tt.combineWith, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试，确认它以「当前实现不满足」的方式失败**

```bash
go test ./util/sys/ -run TestHostProc -v
```

期望：在 darwin 上因 `HostProc` 无函数体而链接失败，或在 linux 上通过（因为 linkname 指向的实现语义相同）。**两种结果都要记下来**——这个测试的作用是钉住语义，让 Step 3 的替换有对照。若在 linux 上通过，那正说明新旧实现等价，替换是安全的。

- [ ] **Step 3: 用标准库重写 `util/sys/psutil.go`**

```go
package sys

import (
	"os"
	"path/filepath"
)

// HostProc 返回 procfs 的挂载点，可选地再拼上若干路径片段。
//
// 原实现用 //go:linkname 借用 gopsutil 的 internal/common.HostProc，
// 把本包与 gopsutil 的内部布局绑死：换个大版本（v3 → v4 导入路径带 /v4）
// 符号就找不到，且报的是链接期错误，比编译错误难查。函数本体只有读环境
// 变量、缺省 /proc、Join 三件事，实现掉就把这条依赖断干净了。
//
// 变量名沿用 gopsutil 的 HOST_PROC：容器里把宿主机 /proc 挂到别处时，
// 本包与 gopsutil 采集到的是同一份数据，不会一个看容器、一个看宿主机。
func HostProc(combineWith ...string) string {
	root := os.Getenv("HOST_PROC")
	if root == "" {
		root = "/proc"
	}
	if len(combineWith) == 0 {
		return root
	}
	return filepath.Join(append([]string{root}, combineWith...)...)
}
```

- [ ] **Step 4: 跑测试确认通过，并确认 linkname 已彻底消失**

```bash
go test ./util/sys/ -run TestHostProc -v
grep -rnE '^[[:space:]]*//go:linkname' --include="*.go" . && echo "还有残留" || echo "已彻底移除"
make verify
```

期望：测试全绿；`grep` 输出「已彻底移除」；`make verify` 通过。

**说明：** 不能用不锚定的 `go:linkname` 作为 grep 模式，因为本任务在注释里提到了这个指令的名字，不锚定的 grep 会误伤注释内容，恒输出「还有残留」。只有以 `//go:` 开头独占一行的指令才是真正的 linkname 指令（可有前导空白），所以改用 `^[[:space:]]*//go:linkname` 来准确匹配。

- [ ] **Step 5: 提交**

```bash
git add util/sys/psutil.go util/sys/psutil_test.go
git commit -m "refactor(sys): 用标准库实现 HostProc，移除侵入 gopsutil 内部包的 linkname

//go:linkname 指向 github.com/shirou/gopsutil/internal/common.HostProc，
把本包和 gopsutil 的内部布局绑死——换大版本（v3 → v4 导入路径变化）符号
就找不到，且报的是链接期错误。查 gopsutil v3.21.3 的实现，函数体只有
读 HOST_PROC、缺省 /proc、Join 三件事，实现掉即可断开这条依赖。

环境变量名沿用 HOST_PROC，容器里改挂载点时本包与 gopsutil 仍看同一份数据。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 3: 抬升 Go 版本声明与发版工作流

**Files:**
- Modify: `go.mod:3`
- Modify: `.github/workflows/release.yml:44`

- [ ] **Step 1: 抬升 `go.mod` 的 go 指令**

把 `go.mod` 第 3 行的 `go 1.21` 改为：

```
go 1.27.0
```

- [ ] **Step 2: 发版工作流改从 `go.mod` 读版本**

`.github/workflows/release.yml` 第 42-45 行：

```yaml
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
          cache: false
```

改为：

```yaml
      - uses: actions/setup-go@v5
        with:
          # Go 版本以 go.mod 为唯一事实来源，避免抬升 go.mod 后
          # 发版工作流还在用旧版本编译。
          go-version-file: go.mod
          cache: false
```

- [ ] **Step 3: 验证**

```bash
make verify
```

期望：全绿。本机 Go 已是 1.27.1，抬升声明不应引入任何变化。若 `go vet` 冒出新告警（1.21 → 1.27 之间新增的检查），**逐条判断**：属于真实缺陷的当场修掉并在提交信息里说明；属于风格性的记进 `$SCRATCH/baseline-vet.txt`，不在本任务修。

- [ ] **Step 4: 提交**

```bash
git add go.mod .github/workflows/release.yml
git commit -m "chore: go.mod 抬到 1.27.0，工作流改从 go.mod 读 Go 版本

升级 xray-core 到与 bin/xray-* 同版本的 26.x 需要 Go 1.27。本机与 CI
runner 都已具备，先单独抬升版本声明并确认行为不变，把工具链变更与依赖
变更分成两次提交，出问题时能二分到具体一步。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 4: 升级 xray-core 至与运行中核心匹配的版本

这是阶段一风险最高的一步。`xray-core` 26.x 会把 `google.golang.org/grpc`、`google.golang.org/protobuf`、`golang.org/x/*` 一并拉高，且 `infra/conf` 会把几乎整个核心链接进 `a-ui` 二进制——**产物体积会明显增长**，这是预期内的代价，不是异常。

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `xray/process.go`（`:11` 导入、`:235` 的 `grpc.Dial`）

- [ ] **Step 1: 确定目标版本**

```bash
./bin/xray-darwin-arm64 -version | head -1     # 运行中核心的版本
go list -m -versions github.com/xtls/xray-core | tr ' ' '\n' | tail -20
```

选**不高于**运行中核心版本、且最接近它的那个发行版。原因：面板内的 `infra/conf` 解析器若比核心新，可能生成核心不认识的 typed message；比核心旧则只是用不到新特性，是安全方向。把选定的版本号记进 `$SCRATCH/xray-core-version.txt`。

参考：3x-ui 锁的是 `v1.260327.1-0.20260728075948-5ca6f4b7d4dc`（伪版本，钉在一个具体 commit）。

- [ ] **Step 2: 升级依赖**

```bash
VER=$(cat "$SCRATCH/xray-core-version.txt")
go get github.com/xtls/xray-core@"$VER"
go mod tidy
```

- [ ] **Step 3: 编译，收集全部编译错误**

```bash
go build ./... 2>&1 | tee "$SCRATCH/upgrade-build-errors.txt"
```

**预期会出现的错误与对应改法**（只改真正报错的地方，不做预防性修改）：

| 错误 | 位置 | 改法 |
|---|---|---|
| `grpc.Dial is deprecated` / 已移除 | `xray/process.go:235` | 见 Step 4 |
| `grpc.WithInsecure is deprecated` / 已移除 | `xray/process.go:235` | 见 Step 4 |
| gin / gorm / gopsutil 编译失败 | 对应包 | **单独成一次提交**：`go get` 升到能编译的最低版本，提交信息说明是被动升级 |

- [ ] **Step 4: 适配新版 grpc 客户端 API**

`xray/process.go` 的 `GetTraffic`（第 232-240 行附近）：

```go
	conn, err := grpc.Dial(fmt.Sprintf("127.0.0.1:%v", p.apiPort), grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
```

改为：

```go
	// 新版 grpc 移除了 Dial/WithInsecure。NewClient 不做阻塞式连接，
	// 首次 RPC 时才建连——对本地回环的 stats 查询没有区别。
	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%v", p.apiPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
```

并在 `xray/process.go` 的 import 块加：

```go
	"google.golang.org/grpc/credentials/insecure"
```

- [ ] **Step 5: 跑门禁**

```bash
make verify
```

期望：与 `$SCRATCH/baseline-test.txt` 逐包对照，**同样全绿**。任何一个包从绿变红都必须查清根因再继续，不得跳过。

- [ ] **Step 6: 记录产物体积变化**

```bash
ls -l a-ui | tee "$SCRATCH/after-size.txt"
cat "$SCRATCH/baseline-size.txt"
```

`infra/conf` 会把大半个核心链接进来，体积增长属预期。把增长量写进 Step 8 的提交信息——发版 tar.gz 会跟着变大，运维需要知道。

- [ ] **Step 7: 实跑面板，确认流量统计没有退化**

这是本任务真正的验收：`xray-core` 在本项目里**只**被用来查 stats，编译通过不等于 RPC 还能通。

```bash
XUI_DEBUG=true go run main.go
```

在另一个终端：

```bash
# 面板起来后，确认 xray 子进程在跑，且 api 端口被监听
pgrep -f 'bin/xray-' && lsof -nP -iTCP:62789 -sTCP:LISTEN
```

浏览器打开面板 → 建一个入站 → 用任意客户端过一点流量 → **入站列表里的上行/下行数字必须增长**。数字不动就说明 stats RPC 断了，检查 `logger` 输出里 `GetTraffic` 的报错。

把验证结果（增长前后的数值）记进 `$SCRATCH/traffic-verify.txt`。

- [ ] **Step 8: 清理并提交**

```bash
make clean
git status   # 确认只有 go.mod / go.sum / xray/process.go
git add go.mod go.sum xray/process.go
git commit -m "chore(deps): xray-core 升到 <VER>，适配新版 grpc 客户端 API

面板此前锁在 xray-core v1.4.2（2021 年），而实际运行的 bin/xray-* 是
26.7.28。两者的差距挡住了配置热更新：AddInbound 必须用 infra/conf 把
JSON 编译成 typed message，v1.4.2 的解析器编译不出 Reality / XHTTP 这类
现代入站；v1.4.2 的 RoutingService 也没有 AddRule，路由规则无法热重载。

grpc 新版移除了 Dial/WithInsecure，GetTraffic 改用 NewClient +
insecure.NewCredentials()。行为不变：NewClient 惰性建连，对本地回环的
stats 查询没有区别。

infra/conf 会把大半个核心链接进来，二进制从 <before> 增长到 <after>，
发版 tar.gz 会相应变大。

已验证：go test ./... 与升级前同样全绿；实跑面板，入站流量统计继续增长。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 5: 阶段一收尾——推分支，确认 CI 绿

- [ ] **Step 1: 确认 diff 干净**

```bash
git log --oneline main..HEAD
git diff main...HEAD --stat
```

期望：4 次提交（门禁、linkname、Go 版本、xray-core），没有调试残留、没有无关格式化、没有 scratchpad 文件混入。

- [ ] **Step 2: 推分支，等 CI**

```bash
git push -u origin <branch>
gh run watch
```

期望：`ci` 工作流绿。若因 `bin/xray-linux-amd64` 不可执行而红，回到 Task 1 Step 5 的 `git update-index --chmod=+x`。

- [ ] **Step 3: 阶段一交付确认**

到这里，行为与升级前完全一致，可以单独发一个版本。**阶段二的任何问题都不应该回退阶段一。**

---

# 阶段二：配置热更新

目标：改分流规则、增删入站出站时，xray 进程不重启。

设计底线：`ComputeHotDiff` 只在**完全确定**能热应用时返回 `ok=true`；任何拿不准的情形一律返回 `false`，让调用方走原来的全量重启。热应用中途失败也退回重启——重启会把部分应用的状态一起清掉，是天然的回滚。

---

### Task 6: 默认模板启用 RoutingService，并为存量部署做幂等迁移

`web/service/config.json` 的 `api.services` 现在只有 `HandlerService` / `LoggerService` / `StatsService`。路由规则的热重载与后续计划 03 的路由测试都要 `RoutingService`。

难点在存量部署：模板一旦被管理员改过就落库到 `settings` 表，改默认值对它们无效。

**Files:**
- Modify: `web/service/config.json`（`api.services` 数组）
- Modify: `web/service/setting.go`（新增迁移函数并在读取模板处调用）
- Create: `web/service/setting_api_services_test.go`

**Interfaces:**
- Produces: `ensureRoutingServiceInTemplate(template string) (string, bool, error)` — 返回补齐后的模板、是否发生了改动、解析错误。幂等：已含 `RoutingService` 时返回原串且 `changed=false`。

- [ ] **Step 1: 写失败的测试**

创建 `web/service/setting_api_services_test.go`：

```go
package service

import (
	"encoding/json"
	"testing"
)

func TestEnsureRoutingServiceInTemplate(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantChanged bool
		wantErr     bool
		// wantServices 为 nil 表示不检查（错误用例）
		wantServices []string
	}{
		{
			name:         "缺少 RoutingService 时补齐并保持原有顺序",
			in:           `{"api":{"services":["HandlerService","LoggerService","StatsService"],"tag":"api"}}`,
			wantChanged:  true,
			wantServices: []string{"HandlerService", "LoggerService", "StatsService", "RoutingService"},
		},
		{
			name:         "已含 RoutingService 时不改动",
			in:           `{"api":{"services":["HandlerService","RoutingService"],"tag":"api"}}`,
			wantChanged:  false,
			wantServices: []string{"HandlerService", "RoutingService"},
		},
		{
			name:        "没有 api 段时不擅自创建",
			in:          `{"inbounds":[]}`,
			wantChanged: false,
		},
		{
			name:        "非法 JSON 报错而不是静默放行",
			in:          `{"api":`,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, err := ensureRoutingServiceInTemplate(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("期望报错，实际返回 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v，期望 %v", changed, tt.wantChanged)
			}
			if tt.wantServices == nil {
				return
			}
			var parsed struct {
				API struct {
					Services []string `json:"services"`
				} `json:"api"`
			}
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("输出不是合法 JSON: %v", err)
			}
			if len(parsed.API.Services) != len(tt.wantServices) {
				t.Fatalf("services = %v，期望 %v", parsed.API.Services, tt.wantServices)
			}
			for i, want := range tt.wantServices {
				if parsed.API.Services[i] != want {
					t.Fatalf("services[%d] = %q，期望 %q（完整: %v）", i, parsed.API.Services[i], want, parsed.API.Services)
				}
			}
		})
	}
}

// 迁移只能碰 api.services，管理员在模板里写的别的东西一个字节都不能动。
func TestEnsureRoutingServiceLeavesOtherKeysAlone(t *testing.T) {
	in := `{"api":{"services":["HandlerService"],"tag":"api"},` +
		`"policy":{"levels":{"0":{"handshake":10}}},` +
		`"outbounds":[{"protocol":"freedom","tag":"direct"}]}`

	out, changed, err := ensureRoutingServiceInTemplate(in)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !changed {
		t.Fatal("期望发生改动")
	}

	var got, want map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatalf("输入不是合法 JSON: %v", err)
	}
	// 把 api 摘掉后，其余部分必须逐键相等。
	delete(got, "api")
	delete(want, "api")
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("api 之外的内容被改动了\n实际: %s\n期望: %s", gotJSON, wantJSON)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./web/service/ -run TestEnsureRoutingService -v
```

期望：`undefined: ensureRoutingServiceInTemplate` 编译失败。

- [ ] **Step 3: 实现迁移函数**

在 `web/service/setting.go` 追加：

```go
// ensureRoutingServiceInTemplate 往模板的 api.services 里补上 RoutingService。
//
// 路由规则的热重载与路由测试都走 RoutingService，模板里不声明它，xray 就
// 不会起这个 gRPC 服务，功能会静默不可用——不报错，只是永远连不上。
//
// 只在 api 段已存在时补齐：api 段整个缺失说明管理员刻意关掉了控制接口，
// 那种情况下流量统计本来就是坏的，不该由这里替他做决定。
//
// 幂等：已含 RoutingService 时原样返回。api 之外的键不做任何改动——
// 反序列化成 map[string]json.RawMessage 再序列化，其余键按原始字节透传。
func ensureRoutingServiceInTemplate(template string) (string, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(template), &root); err != nil {
		return "", false, common.NewError("xray 模板不是合法 JSON:", err)
	}

	rawAPI, ok := root["api"]
	if !ok {
		return template, false, nil
	}

	var api map[string]json.RawMessage
	if err := json.Unmarshal(rawAPI, &api); err != nil {
		return "", false, common.NewError("xray 模板的 api 段不是合法 JSON 对象:", err)
	}

	var services []string
	if rawServices, ok := api["services"]; ok {
		if err := json.Unmarshal(rawServices, &services); err != nil {
			return "", false, common.NewError("xray 模板的 api.services 不是字符串数组:", err)
		}
	}
	for _, s := range services {
		if s == routingServiceName {
			return template, false, nil
		}
	}

	// 追加到末尾而不是插入：顺序对 xray 无意义，但保持原有顺序能让
	// 管理员对比前后模板时只看到多出的一行。
	services = append(services, routingServiceName)
	encoded, err := json.Marshal(services)
	if err != nil {
		return "", false, err
	}
	api["services"] = encoded

	encodedAPI, err := json.Marshal(api)
	if err != nil {
		return "", false, err
	}
	root["api"] = encodedAPI

	out, err := json.Marshal(root)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

// routingServiceName 是 xray api.services 里 RoutingService 的名字。
const routingServiceName = "RoutingService"
```

确认 `web/service/setting.go` 的 import 块已含 `encoding/json` 与 `a-ui/util/common`，缺哪个补哪个。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./web/service/ -run TestEnsureRoutingService -v
```

期望：全部子测试 PASS。

- [ ] **Step 5: 在读取模板处接上迁移**

`web/service/setting.go:178-180` 现在是：

```go
func (s *SettingService) GetXrayConfigTemplate() (string, error) {
	return s.getString("xrayTemplateConfig")
}
```

改为（`getString` / `setString` 是本文件已有的私有读写方法，见 `:148` 与 `:162`）：

```go
func (s *SettingService) GetXrayConfigTemplate() (string, error) {
	template, err := s.getString("xrayTemplateConfig")
	if err != nil {
		return "", err
	}

	// 补 RoutingService。存量部署的模板一旦被管理员改过就落进 settings 表，
	// 改默认值对它们无效，所以在读取路径上补，读一次补一次（幂等）。
	//
	// 补完立刻写回，而不是每次读都在内存里补：写回后管理员在设置页看到的
	// 模板与实际生效的一致；只在内存补的话，他保存一次就把 RoutingService
	// 弄丢了，而且丢得毫无提示。
	patched, changed, err := ensureRoutingServiceInTemplate(template)
	if err != nil {
		// 模板本来就不合法是既有问题，不是本次改动造成的。原样返回让
		// 后续流程按老路径报错，不要在这里把管理员锁在门外。
		logger.Warning("xray 模板无法解析，跳过 RoutingService 补齐:", err)
		return template, nil
	}
	if changed {
		if err := s.setString("xrayTemplateConfig", patched); err != nil {
			logger.Warning("RoutingService 补齐后写回失败，本次仅在内存生效:", err)
		} else {
			logger.Info("已为 xray 模板补上 RoutingService，路由热更新与路由测试现在可用")
		}
	}
	return patched, nil
}
```

确认 `web/service/setting.go` 的 import 块已含 `a-ui/logger`，缺则补上。

- [ ] **Step 6: 更新默认模板**

`web/service/config.json` 第 3-7 行：

```json
  "api": {
    "services": [
      "HandlerService",
      "LoggerService",
      "StatsService"
    ],
    "tag": "api"
  },
```

改为：

```json
  "api": {
    "services": [
      "HandlerService",
      "LoggerService",
      "StatsService",
      "RoutingService"
    ],
    "tag": "api"
  },
```

- [ ] **Step 7: 全量验证 + 实跑确认 RoutingService 真的起来了**

```bash
make verify
XUI_DEBUG=true go run main.go
```

面板起来后，确认生成的配置里有它：

```bash
grep -A 8 '"api"' bin/config.json
```

期望：`services` 数组含 `RoutingService`。

- [ ] **Step 8: 提交**

```bash
git add web/service/config.json web/service/setting.go web/service/setting_api_services_test.go
git commit -m "feat(xray): 模板启用 RoutingService，并为存量部署补齐

路由规则热重载（本计划阶段二）与路由测试（计划 03）都走 RoutingService。
模板里不声明它，xray 不会起这个 gRPC 服务，面板连上去只会超时——不报错，
功能静默不可用。

存量部署的模板改过就落进 settings 表，改默认值对它们无效，因此在读取路径
上做幂等补齐并写回。写回而不是只在内存补：不写回的话管理员在设置页保存一次
就把 RoutingService 弄丢了，且毫无提示。

迁移只碰 api.services，其余键按原始字节透传（测试守着这条）。模板本身不合法
时原样返回并记日志，不在这里把管理员锁在门外。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 7: xray gRPC 客户端封装

**Files:**
- Create: `xray/api.go`
- Create: `xray/api_test.go`
- Modify: `xray/process.go`（新增 `GetAPIPort()` 与 `SetConfig()`）

**Interfaces:**
- Produces（阶段二 Task 9 与计划 03 都依赖这些签名）：
  - `type XrayAPI struct { HandlerServiceClient *command.HandlerServiceClient; RoutingServiceClient *routerService.RoutingServiceClient }`
  - `func (x *XrayAPI) Init(apiPort int) error`
  - `func (x *XrayAPI) Close()`
  - `func (x *XrayAPI) AddInbound(inbound []byte) error`
  - `func (x *XrayAPI) DelInbound(tag string) error`
  - `func (x *XrayAPI) AddOutbound(outbound []byte) error`
  - `func (x *XrayAPI) DelOutbound(tag string) error`
  - `func (x *XrayAPI) ApplyRoutingConfig(routing []byte) error`
  - `func (p *Process) SetConfig(c *Config)`（新增；`GetAPIPort()` 与 `GetConfig()` 已存在于 `xray/process.go:110` `:114`）
- Consumes: 无（只用 `xray-core` 的包）。

**注意**：`xray` 包目前没有任何测试文件，本任务的 `xray/api_test.go` 是第一个。`go test ./xray/` 的工作目录是包目录 `xray/`，而 `ensureXrayAssetLocation()` 用 `filepath.Abs("bin")` 会解析成 `xray/bin`。本任务的测试都在"客户端未初始化"就返回，触不到它；**将来在本包新增会走到 `ensureXrayAssetLocation` 的测试时，必须先 `t.Setenv("XRAY_LOCATION_ASSET", ...)` 指到真实路径**。

- [ ] **Step 1: 写失败的测试**

创建 `xray/api_test.go`。**这里只测不需要真核心的路径**——真核心的端到端验证放在 Task 10 的人工步骤：

```go
package xray

import (
	"strings"
	"testing"
)

// 未 Init 就调用必须返回明确错误，而不是 nil panic。
// 热应用失败要能被调用方识别并退回重启，静默的 nil 解引用会直接杀掉面板进程
// （cron 任务没有 panic 恢复）。
func TestXrayAPIUninitialized(t *testing.T) {
	tests := []struct {
		name string
		call func(x *XrayAPI) error
	}{
		{"AddInbound", func(x *XrayAPI) error { return x.AddInbound([]byte(`{}`)) }},
		{"DelInbound", func(x *XrayAPI) error { return x.DelInbound("t") }},
		{"AddOutbound", func(x *XrayAPI) error { return x.AddOutbound([]byte(`{}`)) }},
		{"DelOutbound", func(x *XrayAPI) error { return x.DelOutbound("t") }},
		{"ApplyRoutingConfig", func(x *XrayAPI) error { return x.ApplyRoutingConfig([]byte(`{}`)) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := &XrayAPI{}
			err := tt.call(x)
			if err == nil {
				t.Fatal("未初始化时期望报错，实际返回 nil")
			}
			if !strings.Contains(err.Error(), "not initialized") {
				t.Fatalf("错误信息应说明客户端未初始化，实际: %v", err)
			}
		})
	}
}

// Init 失败后 Close 不能 panic：热应用的 defer Close() 会在任何一条
// 失败路径上执行。
func TestXrayAPICloseIsSafeWithoutInit(t *testing.T) {
	x := &XrayAPI{}
	x.Close() // 不 panic 即通过
	x.Close() // 重复 Close 也不能 panic
}

func TestApplyRoutingConfigRejectsInvalidJSON(t *testing.T) {
	x := &XrayAPI{}
	// 客户端未初始化的检查先触发，所以这里断言的是「有错误」而非具体哪一种。
	if err := x.ApplyRoutingConfig([]byte(`{"rules":`)); err == nil {
		t.Fatal("非法 JSON 期望报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./xray/ -run 'TestXrayAPI|TestApplyRouting' -v
```

期望：`undefined: XrayAPI` 编译失败。

- [ ] **Step 3: 实现 `xray/api.go`**

```go
package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xtls/xray-core/app/proxyman/command"
	routerService "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/infra/conf"

	"a-ui/logger"
	"a-ui/util/common"
)

// rpcTimeout 是单次控制面 RPC 的上限。热应用整体发生在那个 10 秒的重启
// 消费任务里，单次调用拖太久会把后续任务一起堵住。
const rpcTimeout = 10 * time.Second

// routingRPCTimeout 比普通 RPC 宽：下发路由要在核心里重建整张规则表，
// 规则引用 geosite: 时还要现场读 dat 文件。
const routingRPCTimeout = 30 * time.Second

// XrayAPI 是运行中 xray 核心控制面（gRPC）的客户端。
//
// 与 process.go 里查流量用的那个连接分开：那条连接由 GetTraffic 每次现开
// 现关，而控制面调用需要在一次热应用里连续发多条命令，共用一个连接。
type XrayAPI struct {
	HandlerServiceClient *command.HandlerServiceClient
	RoutingServiceClient *routerService.RoutingServiceClient

	conn *grpc.ClientConn
}

// Init 连上本机的 xray gRPC 控制面。apiPort 来自 Process.GetAPIPort()。
func (x *XrayAPI) Init(apiPort int) error {
	if apiPort <= 0 {
		return common.NewError("xray api port wrong:", apiPort)
	}
	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%v", apiPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	x.conn = conn

	hsClient := command.NewHandlerServiceClient(conn)
	rsClient := routerService.NewRoutingServiceClient(conn)
	x.HandlerServiceClient = &hsClient
	x.RoutingServiceClient = &rsClient
	return nil
}

// Close 释放连接。可重复调用，也可以在 Init 失败后调用——热应用的每条
// 失败路径都会走到这里的 defer。
func (x *XrayAPI) Close() {
	if x.conn != nil {
		_ = x.conn.Close()
		x.conn = nil
	}
	x.HandlerServiceClient = nil
	x.RoutingServiceClient = nil
}

// AddInbound 把一段入站 JSON 加进运行中的核心。
//
// JSON 要先经 infra/conf 编译成 typed message——这正是面板必须与运行中
// 核心同版本的原因：老版本的解析器编译不出新协议的入站。
func (x *XrayAPI) AddInbound(inbound []byte) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ensureXrayAssetLocation()

	c := new(conf.InboundDetourConfig)
	if err := json.Unmarshal(inbound, c); err != nil {
		return common.NewError("入站配置无法解析:", err)
	}
	built, err := c.Build()
	if err != nil {
		return common.NewError("入站配置无法构建:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = (*x.HandlerServiceClient).AddInbound(ctx, &command.AddInboundRequest{Inbound: built})
	return err
}

// DelInbound 按 tag 摘掉一个入站。
func (x *XrayAPI) DelInbound(tag string) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err := (*x.HandlerServiceClient).RemoveInbound(ctx, &command.RemoveInboundRequest{Tag: tag})
	return err
}

// AddOutbound 把一段出站 JSON 加进运行中的核心。
func (x *XrayAPI) AddOutbound(outbound []byte) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ensureXrayAssetLocation()

	c := new(conf.OutboundDetourConfig)
	if err := json.Unmarshal(outbound, c); err != nil {
		return common.NewError("出站配置无法解析:", err)
	}
	built, err := c.Build()
	if err != nil {
		return common.NewError("出站配置无法构建:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err = (*x.HandlerServiceClient).AddOutbound(ctx, &command.AddOutboundRequest{Outbound: built})
	return err
}

// DelOutbound 按 tag 摘掉一个出站。
func (x *XrayAPI) DelOutbound(tag string) error {
	if x.HandlerServiceClient == nil {
		return common.NewError("xray HandlerServiceClient is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	_, err := (*x.HandlerServiceClient).RemoveOutbound(ctx, &command.RemoveOutboundRequest{Tag: tag})
	return err
}

// ApplyRoutingConfig 用整段 routing 配置替换核心里的规则表与 balancer。
//
// ShouldAppend=false 表示整体替换而非追加：面板每次都生成完整的规则集，
// 追加会让旧规则残留在核心里，删掉一条分流规则就变成删不掉。
//
// 注意这个 RPC 改不了 routing.domainStrategy / domainMatcher，那两个在
// 进程启动时固定——hot_diff.go 因此把它们归进「必须重启」的部分。
func (x *XrayAPI) ApplyRoutingConfig(routing []byte) error {
	if x.RoutingServiceClient == nil {
		return common.NewError("xray RoutingServiceClient is not initialized")
	}
	// 规则里的 geosite: / geoip: / ext: 要靠 dat 文件解析，把核心的资源
	// 目录指到面板的 bin/，否则规则构建会因找不到 geosite.dat 而失败。
	ensureXrayAssetLocation()

	routerConf := new(conf.RouterConfig)
	if err := json.Unmarshal(routing, routerConf); err != nil {
		return common.NewError("路由配置无法解析:", err)
	}
	built, err := routerConf.Build()
	if err != nil {
		return common.NewError("路由配置无法构建:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), routingRPCTimeout)
	defer cancel()
	_, err = (*x.RoutingServiceClient).AddRule(ctx, &routerService.AddRuleRequest{
		ShouldAppend: false,
		Config:       serial.ToTypedMessage(built),
	})
	return err
}

// ensureXrayAssetLocation 把 xray 的资源目录指到面板的 bin/。
//
// 面板进程内的 infra/conf 解析 geosite:/geoip: 时要读 dat 文件，而它默认
// 在可执行文件同目录找。面板的工作目录就是安装根目录（systemd 的
// WorkingDirectory=/usr/local/a-ui/），dat 在 bin/ 下。
//
// 已经设了就不覆盖：管理员可能刻意指到别处。
func ensureXrayAssetLocation() {
	if os.Getenv("XRAY_LOCATION_ASSET") != "" || os.Getenv("xray.location.asset") != "" {
		return
	}
	abs, err := filepath.Abs("bin")
	if err != nil {
		logger.Warning("无法解析 bin 目录的绝对路径，geosite/geoip 可能解析失败:", err)
		return
	}
	_ = os.Setenv("XRAY_LOCATION_ASSET", abs)
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./xray/ -run 'TestXrayAPI|TestApplyRouting' -v
```

期望：全部 PASS。

- [ ] **Step 5: 给 `Process` 加配置快照的写入口**

`xray/process.go` 已有 `GetAPIPort()`（`:110`）和 `GetConfig()`（`:114`），都定义在 `*Process` 上，本任务**不需要新增**。缺的只有写入口——在 `GetConfig()` 之后追加：

```go
// SetConfig 更新进程内的配置快照。
//
// 热应用成功后必须调它：RestartXray 是靠 GetConfig().Equals(新配置) 判断
// "配置有没有变"的，不同步的话下一轮 cron 仍认为两者不同，于是真的去重启
// 一次——热更新反而退化成延迟一轮的重启。
func (p *Process) SetConfig(c *Config) {
	p.config = c
}
```

`p.config` 是 `process` 结构体的字段（`xray/process.go:65`），`Process` 内嵌 `*process`，可直接写。

- [ ] **Step 6: 全量验证并提交**

```bash
make verify
git add xray/api.go xray/api_test.go xray/process.go
git commit -m "feat(xray): 新增 gRPC 控制面客户端封装

为配置热更新做准备。HandlerService 负责增删入站/出站，RoutingService 的
AddRule(ShouldAppend=false) 整体替换规则表——追加的话删一条分流规则会变成
删不掉，旧规则永远残留在核心里。

入站/出站 JSON 必须经 infra/conf 编译成 typed message，这就是面板要与运行中
核心同版本的原因（见本计划 Task 4）。规则里的 geosite:/geoip: 需要 dat 文件，
因此把 XRAY_LOCATION_ASSET 指到面板的 bin/，已设则不覆盖。

未初始化时每个方法都返回明确错误而非 nil 解引用：cron 任务没有 panic 恢复，
一次空指针会杀掉整个面板进程。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 8: 差分计算 `ComputeHotDiff`

本计划测试量最大的一块。这是**纯函数**，不碰网络也不碰进程，可以完全用表驱动测试覆盖。

**Files:**
- Create: `xray/hot_diff.go`
- Create: `xray/hot_diff_test.go`

**Interfaces:**
- Consumes: `xray.Config` / `xray.InboundConfig`（`xray/config.go`）。
- Produces:
  - `type HotDiff struct { RemovedInboundTags []string; AddedInbounds [][]byte; RemovedOutboundTags []string; AddedOutbounds [][]byte; RoutingConfig []byte }`
  - `func (d *HotDiff) Empty() bool`
  - `func ComputeHotDiff(oldCfg, newCfg *Config) (*HotDiff, bool)`

**AetherUI 与参考实现的两点关键差异**（实现前务必读懂）：

1. `Config.OutboundConfigs` 是**整块 `json_util.RawMessage`**，不是 `[]OutboundConfig`。出站 diff 要先把这块 blob 解成 `[]json.RawMessage` 再按各自的 `tag` 字段索引。
2. `Config` 只有 11 个字段，没有 `observatory` / `metrics` / `geodata` / `env`。模板里的这些键在 `GetXrayConfig` 反序列化时就被丢掉了，**不必也不能在 diff 里处理**。
3. 入站之间沿用现成的 `InboundConfig.Equals`（`xray/inbound.go:18`），它是**逐字节**比较而非归一化比较。这是刻意的：入站的 `settings` / `streamSettings` 直接来自数据库，同一份表单两次保存不会自发产生空白差异；万一 key 顺序真的抖动，结果是多做一次热交换（删+加），仍然好过一次重启，不影响正确性。归一化只用在模板来的那几个静态段上——那里管理员会手动重排格式。

- [ ] **Step 1: 写失败的测试**

创建 `xray/hot_diff_test.go`：

```go
package xray

import (
	"encoding/json"
	"testing"

	"a-ui/util/json_util"
)

func rawf(s string) json_util.RawMessage { return json_util.RawMessage(s) }

// baseConfig 是一份最小可用配置：一个 api 入站、一个业务入站、一段出站、一段路由。
func baseConfig() *Config {
	return &Config{
		LogConfig: rawf(`{"loglevel":"warning"}`),
		API:       rawf(`{"services":["HandlerService","RoutingService"],"tag":"api"}`),
		Stats:     rawf(`{}`),
		Policy:    rawf(`{"levels":{"0":{"statsUserUplink":true}}}`),
		InboundConfigs: []InboundConfig{
			{Port: 62789, Protocol: "dokodemo-door", Tag: "api", Settings: rawf(`{"address":"127.0.0.1"}`)},
			{Port: 10001, Protocol: "vless", Tag: "inbound-10001", Settings: rawf(`{"clients":[{"id":"u1"}]}`)},
		},
		OutboundConfigs: rawf(`[{"protocol":"freedom","tag":"direct"},{"protocol":"blackhole","tag":"a-ui-block"}]`),
		RouterConfig:    rawf(`{"domainStrategy":"AsIs","rules":[{"type":"field","domain":["geosite:openai"],"outboundTag":"a-ui-block"}]}`),
	}
}

func TestComputeHotDiff(t *testing.T) {
	tests := []struct {
		name string
		// mutate 把 base 改成「新配置」
		mutate func(c *Config)
		wantOK bool
		// check 在 wantOK 为真时校验差分内容
		check func(t *testing.T, d *HotDiff)
	}{
		{
			name:   "完全相同则差分为空且可热应用",
			mutate: func(c *Config) {},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if !d.Empty() {
					t.Fatalf("期望空差分，实际 %+v", d)
				}
			},
		},
		{
			name: "只改路由规则可热应用",
			mutate: func(c *Config) {
				c.RouterConfig = rawf(`{"domainStrategy":"AsIs","rules":[{"type":"field","domain":["geosite:netflix"],"outboundTag":"a-ui-block"}]}`)
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.RoutingConfig) == 0 {
					t.Fatal("期望带上新的路由配置")
				}
				if len(d.AddedInbounds) != 0 || len(d.RemovedInboundTags) != 0 {
					t.Fatalf("不该动入站，实际 %+v", d)
				}
			},
		},
		{
			name: "改 domainStrategy 必须重启",
			mutate: func(c *Config) {
				c.RouterConfig = rawf(`{"domainStrategy":"IPIfNonMatch","rules":[{"type":"field","domain":["geosite:openai"],"outboundTag":"a-ui-block"}]}`)
			},
			wantOK: false,
		},
		{
			name: "新增业务入站可热应用",
			mutate: func(c *Config) {
				c.InboundConfigs = append(c.InboundConfigs, InboundConfig{
					Port: 10002, Protocol: "vless", Tag: "inbound-10002",
					Settings: rawf(`{"clients":[{"id":"u2"}]}`),
				})
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.AddedInbounds) != 1 {
					t.Fatalf("期望新增 1 个入站，实际 %d", len(d.AddedInbounds))
				}
				if len(d.RemovedInboundTags) != 0 {
					t.Fatalf("不该删入站，实际 %v", d.RemovedInboundTags)
				}
			},
		},
		{
			name: "删除业务入站可热应用",
			mutate: func(c *Config) {
				c.InboundConfigs = c.InboundConfigs[:1]
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.RemovedInboundTags) != 1 || d.RemovedInboundTags[0] != "inbound-10001" {
					t.Fatalf("期望删除 inbound-10001，实际 %v", d.RemovedInboundTags)
				}
			},
		},
		{
			name: "改动 api 入站必须重启",
			mutate: func(c *Config) {
				c.InboundConfigs[0].Port = 62790
			},
			wantOK: false,
		},
		{
			name: "改 api 段必须重启",
			mutate: func(c *Config) {
				c.API = rawf(`{"services":["HandlerService"],"tag":"api"}`)
			},
			wantOK: false,
		},
		{
			name: "改 policy 必须重启",
			mutate: func(c *Config) {
				c.Policy = rawf(`{"levels":{"0":{"statsUserUplink":false}}}`)
			},
			wantOK: false,
		},
		{
			name: "改 log（访问日志开关）必须重启",
			mutate: func(c *Config) {
				c.LogConfig = rawf(`{"loglevel":"warning","access":"bin/access.log"}`)
			},
			wantOK: false,
		},
		{
			name: "只重排 JSON 空白不算改动",
			mutate: func(c *Config) {
				c.Policy = rawf("{\n  \"levels\": {\n    \"0\": { \"statsUserUplink\": true }\n  }\n}")
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if !d.Empty() {
					t.Fatalf("仅空白差异不该产生操作，实际 %+v", d)
				}
			},
		},
		{
			name: "新增出站可热应用",
			mutate: func(c *Config) {
				c.OutboundConfigs = rawf(`[{"protocol":"freedom","tag":"direct"},{"protocol":"blackhole","tag":"a-ui-block"},{"protocol":"socks","tag":"a-ui-node1","settings":{"servers":[{"address":"1.2.3.4","port":1080}]}}]`)
			},
			wantOK: true,
			check: func(t *testing.T, d *HotDiff) {
				if len(d.AddedOutbounds) != 1 {
					t.Fatalf("期望新增 1 个出站，实际 %d", len(d.AddedOutbounds))
				}
			},
		},
		{
			name: "改动默认出站（数组首位）必须重启",
			mutate: func(c *Config) {
				c.OutboundConfigs = rawf(`[{"protocol":"blackhole","tag":"a-ui-block"},{"protocol":"freedom","tag":"direct"}]`)
			},
			wantOK: false,
		},
		{
			name: "入站启用 Reality 必须重启",
			mutate: func(c *Config) {
				c.InboundConfigs[1].StreamSettings = rawf(`{"network":"tcp","security":"reality","realitySettings":{"dest":"www.cloudflare.com:443"}}`)
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCfg := baseConfig()
			newCfg := baseConfig()
			tt.mutate(newCfg)

			diff, ok := ComputeHotDiff(oldCfg, newCfg)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v，期望 %v（diff=%+v）", ok, tt.wantOK, diff)
			}
			if ok && tt.check != nil {
				tt.check(t, diff)
			}
		})
	}
}

// nil 输入必须判定为「不能热应用」，而不是 panic。
func TestComputeHotDiffNilInput(t *testing.T) {
	if _, ok := ComputeHotDiff(nil, baseConfig()); ok {
		t.Fatal("oldCfg 为 nil 时不该判定可热应用")
	}
	if _, ok := ComputeHotDiff(baseConfig(), nil); ok {
		t.Fatal("newCfg 为 nil 时不该判定可热应用")
	}
}

// 新增入站的 JSON 必须能被 encoding/json 解回来，否则下发给核心必然失败。
func TestAddedInboundIsValidJSON(t *testing.T) {
	oldCfg := baseConfig()
	newCfg := baseConfig()
	newCfg.InboundConfigs = append(newCfg.InboundConfigs, InboundConfig{
		Port: 10002, Protocol: "vless", Tag: "inbound-10002",
		Settings: rawf(`{"clients":[{"id":"u2"}]}`),
	})

	diff, ok := ComputeHotDiff(oldCfg, newCfg)
	if !ok {
		t.Fatal("期望可热应用")
	}
	var parsed map[string]any
	if err := json.Unmarshal(diff.AddedInbounds[0], &parsed); err != nil {
		t.Fatalf("新增入站不是合法 JSON: %v", err)
	}
	if parsed["tag"] != "inbound-10002" {
		t.Fatalf("tag = %v，期望 inbound-10002", parsed["tag"])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./xray/ -run TestComputeHotDiff -v
```

期望：`undefined: ComputeHotDiff` 编译失败。

- [ ] **Step 3: 实现 `xray/hot_diff.go`**

```go
package xray

import (
	"bytes"
	"encoding/json"

	"a-ui/logger"
	"a-ui/util/json_util"
)

// HotDiff 是把运行中的核心从一份配置搬到另一份所需的控制面操作。
// 只覆盖核心支持运行时重载的部分：入站、出站、路由规则。
type HotDiff struct {
	RemovedInboundTags  []string
	AddedInbounds       [][]byte
	RemovedOutboundTags []string
	AddedOutbounds      [][]byte
	// RoutingConfig 是整段新的 routing 配置，nil 表示路由没变。
	RoutingConfig []byte
}

// Empty 表示不需要做任何操作。
func (d *HotDiff) Empty() bool {
	return len(d.RemovedInboundTags) == 0 &&
		len(d.AddedInbounds) == 0 &&
		len(d.RemovedOutboundTags) == 0 &&
		len(d.AddedOutbounds) == 0 &&
		d.RoutingConfig == nil
}

// ComputeHotDiff 比较新旧配置，返回把核心从 old 搬到 new 的操作集。
//
// ok 为 false 表示这次改动碰到了没有运行时重载接口的东西（log / dns /
// policy / api / stats / …），必须整进程重启。
//
// 判定一律保守：拿不准就返回 false。热更新是省掉一次断线的优化，不是新的
// 失败点——误判成"能热应用"的代价是核心与面板认知不一致，比多重启一次严重
// 得多。
func ComputeHotDiff(oldCfg, newCfg *Config) (*HotDiff, bool) {
	if oldCfg == nil || newCfg == nil {
		return nil, false
	}

	// 没有重载接口的段必须语义等价。比较对 JSON 空白不敏感：管理员在设置页
	// 重新格式化一下模板，不该被当成真改动而触发重启。
	static := []struct {
		name     string
		old, new json_util.RawMessage
	}{
		{"log", oldCfg.LogConfig, newCfg.LogConfig},
		{"dns", oldCfg.DNSConfig, newCfg.DNSConfig},
		{"transport", oldCfg.Transport, newCfg.Transport},
		{"policy", oldCfg.Policy, newCfg.Policy},
		{"api", oldCfg.API, newCfg.API},
		{"stats", oldCfg.Stats, newCfg.Stats},
		{"reverse", oldCfg.Reverse, newCfg.Reverse},
		{"fakeDns", oldCfg.FakeDNS, newCfg.FakeDNS},
	}
	for _, section := range static {
		if !rawEqualNormalized(section.old, section.new) {
			logger.Debug("hot diff: [", section.name, "] 段有变化且没有重载接口，需要重启")
			return nil, false
		}
	}

	diff := &HotDiff{}
	if !diffInbounds(oldCfg, newCfg, diff) {
		return nil, false
	}
	if !diffOutbounds(oldCfg, newCfg, diff) {
		return nil, false
	}
	if !diffRouting(oldCfg, newCfg, diff) {
		return nil, false
	}
	return diff, true
}

// diffInbounds 计算入站的增删。改动过的入站按"先删后加"处理。
func diffInbounds(oldCfg, newCfg *Config, diff *HotDiff) bool {
	oldByTag, ok := inboundsByTag(oldCfg.InboundConfigs)
	if !ok {
		return false
	}
	newByTag, ok := inboundsByTag(newCfg.InboundConfigs)
	if !ok {
		return false
	}

	for i := range oldCfg.InboundConfigs {
		oldIb := &oldCfg.InboundConfigs[i]
		newIb, exists := newByTag[oldIb.Tag]
		if exists && oldIb.Equals(newIb) {
			continue
		}
		// api 入站承载着面板正在用的那条 gRPC 连接，动它等于自断手脚。
		if oldIb.Tag == "api" {
			logger.Debug("hot diff: api 入站有变化，需要重启")
			return false
		}
		// Reality 的鉴权器无法靠 gRPC 的删+加可靠重建（3x-ui 实测结论）。
		if exists && (inboundUsesReality(oldIb) || inboundUsesReality(newIb)) {
			logger.Debug("hot diff: 入站 [", oldIb.Tag, "] 涉及 Reality，需要重启")
			return false
		}
		diff.RemovedInboundTags = append(diff.RemovedInboundTags, oldIb.Tag)
		if exists {
			raw, err := json.Marshal(newIb)
			if err != nil {
				return false
			}
			diff.AddedInbounds = append(diff.AddedInbounds, raw)
		}
	}

	for i := range newCfg.InboundConfigs {
		newIb := &newCfg.InboundConfigs[i]
		if _, exists := oldByTag[newIb.Tag]; exists {
			continue
		}
		if newIb.Tag == "api" {
			logger.Debug("hot diff: 新增了 api 入站，需要重启")
			return false
		}
		if inboundUsesReality(newIb) {
			logger.Debug("hot diff: 新增入站 [", newIb.Tag, "] 使用 Reality，需要重启")
			return false
		}
		raw, err := json.Marshal(newIb)
		if err != nil {
			return false
		}
		diff.AddedInbounds = append(diff.AddedInbounds, raw)
	}
	return true
}

// inboundsByTag 按 tag 建索引。tag 为空或重复时返回 false——那种配置本身
// 就有问题，交给重启路径让核心去报错，别在这里替它拼凑。
func inboundsByTag(inbounds []InboundConfig) (map[string]*InboundConfig, bool) {
	byTag := make(map[string]*InboundConfig, len(inbounds))
	for i := range inbounds {
		ib := &inbounds[i]
		if ib.Tag == "" {
			return nil, false
		}
		if _, dup := byTag[ib.Tag]; dup {
			return nil, false
		}
		byTag[ib.Tag] = ib
	}
	return byTag, true
}

// inboundUsesReality 判断入站的 streamSettings.security 是否为 reality。
func inboundUsesReality(ib *InboundConfig) bool {
	if len(ib.StreamSettings) == 0 {
		return false
	}
	var ss struct {
		Security string `json:"security"`
	}
	if err := json.Unmarshal(ib.StreamSettings, &ss); err != nil {
		// 解不动就当它是，走重启：宁可多重启一次，也不要把一个自己都读不懂
		// 的入站热塞进核心。
		return true
	}
	return ss.Security == "reality"
}

// diffOutbounds 计算出站的增删。
//
// AetherUI 的 Config.OutboundConfigs 是整块 RawMessage（不是结构体切片），
// 所以先解成 []json.RawMessage 再按各自的 tag 索引。
//
// 数组首位是 xray 的默认出站，改动它会改变所有未命中规则的流量去向，而
// 控制面没有"换默认出站"的接口——必须重启。
func diffOutbounds(oldCfg, newCfg *Config, diff *HotDiff) bool {
	if bytes.Equal(oldCfg.OutboundConfigs, newCfg.OutboundConfigs) {
		return true
	}
	oldList, oldTags, ok := decodeOutbounds(oldCfg.OutboundConfigs)
	if !ok {
		return false
	}
	newList, newTags, ok := decodeOutbounds(newCfg.OutboundConfigs)
	if !ok {
		return false
	}
	if len(oldList) == 0 || len(newList) == 0 {
		return false
	}
	// 默认出站（首位）必须逐字节不变。
	if !rawEqualNormalized(oldList[0], newList[0]) {
		logger.Debug("hot diff: 默认出站有变化，需要重启")
		return false
	}

	oldByTag := map[string]json.RawMessage{}
	for i, tag := range oldTags {
		oldByTag[tag] = oldList[i]
	}
	newByTag := map[string]json.RawMessage{}
	for i, tag := range newTags {
		newByTag[tag] = newList[i]
	}

	// 按原数组顺序遍历，保证操作序列本身也是确定的。
	for i, tag := range oldTags {
		newOb, exists := newByTag[tag]
		if exists && rawEqualNormalized(oldList[i], newOb) {
			continue
		}
		diff.RemovedOutboundTags = append(diff.RemovedOutboundTags, tag)
		if exists {
			diff.AddedOutbounds = append(diff.AddedOutbounds, newOb)
		}
	}
	for i, tag := range newTags {
		if _, exists := oldByTag[tag]; exists {
			continue
		}
		diff.AddedOutbounds = append(diff.AddedOutbounds, newList[i])
	}
	return true
}

// decodeOutbounds 把出站数组解成逐条的原始 JSON 与对应的 tag。
// tag 为空或重复时返回 false。
func decodeOutbounds(raw json_util.RawMessage) ([]json.RawMessage, []string, bool) {
	if len(raw) == 0 {
		return nil, nil, false
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, false
	}
	tags := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		var head struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			return nil, nil, false
		}
		if head.Tag == "" || seen[head.Tag] {
			return nil, nil, false
		}
		seen[head.Tag] = true
		tags = append(tags, head.Tag)
	}
	return list, tags, true
}

// diffRouting 判断路由改动能否热应用。
//
// rules 与 balancers 有重载接口，其余键（主要是 domainStrategy /
// domainMatcher）在进程启动时固定，变了就必须重启。
func diffRouting(oldCfg, newCfg *Config, diff *HotDiff) bool {
	if rawEqualNormalized(oldCfg.RouterConfig, newCfg.RouterConfig) {
		return true
	}
	// 一侧完全没有 routing 段，说明运行中的核心可能压根没起路由模块，
	// 那样 RoutingService 也不在，只能重启。
	if len(oldCfg.RouterConfig) == 0 || len(newCfg.RouterConfig) == 0 {
		return false
	}
	oldRest, ok := routingWithoutReloadable(oldCfg.RouterConfig)
	if !ok {
		return false
	}
	newRest, ok := routingWithoutReloadable(newCfg.RouterConfig)
	if !ok {
		return false
	}
	if !bytes.Equal(oldRest, newRest) {
		logger.Debug("hot diff: routing 的不可重载部分有变化，需要重启")
		return false
	}
	diff.RoutingConfig = append([]byte(nil), newCfg.RouterConfig...)
	return true
}

// routingWithoutReloadable 把 routing 段里可运行时重载的键摘掉，
// 返回归一化后的剩余部分，用于比较"必须重启的那一半"。
func routingWithoutReloadable(raw []byte) ([]byte, bool) {
	parsed := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, false
	}
	delete(parsed, "rules")
	delete(parsed, "balancers")
	// encoding/json 对 map key 排序，输出因此是确定的。
	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rawEqualNormalized 比较两段 JSON 的语义是否相同，忽略空白与 key 顺序。
//
// 不能直接 bytes.Equal：设置页把模板重新格式化一遍是很常见的操作，那不该
// 被当成配置变化而触发一次全员断线的重启。
func rawEqualNormalized(a, b json_util.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	na, okA := normalizeJSON(a)
	nb, okB := normalizeJSON(b)
	if !okA || !okB {
		// 有一侧解不动就退回字节比较的结论（已知不等），走重启。
		return false
	}
	return bytes.Equal(na, nb)
}

// normalizeJSON 把一段 JSON 解出来再序列化回去，消除空白差异并让 map key
// 排序。空输入视作合法，归一化成空。
func normalizeJSON(raw json_util.RawMessage) ([]byte, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, true
	}
	var v any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}
```

- [ ] **Step 4: 跑测试确认全部通过**

```bash
go test ./xray/ -run 'TestComputeHotDiff|TestAddedInbound' -v
```

期望：全部子测试 PASS。若「只重排 JSON 空白不算改动」失败，检查 `rawEqualNormalized` 的归一化路径。

- [ ] **Step 5: 反向验证测试的有效性**

按 Global Constraints 的要求，逐个确认测试真的能变红。至少验证两条最关键的：

```bash
# 临时把 "改 policy 必须重启" 那条的 static 列表里的 policy 一行注释掉
# 跑测试 → 必须看到 "改 policy 必须重启" FAIL
# 恢复
```

把验证过程记进 `$SCRATCH/hot-diff-red-green.txt`。**这一步不能跳过**——一个两边都过的测试比没有测试更糟。

- [ ] **Step 6: 全量验证并提交**

```bash
make verify
git add xray/hot_diff.go xray/hot_diff_test.go
git commit -m "feat(xray): 新增新旧配置的热更新差分计算

纯函数，不碰网络与进程。判定一律保守：没有运行时重载接口的段（log/dns/
policy/api/stats/…）必须语义等价，api 入站、默认出站、routing 的
domainStrategy 有任何变化都直接判定需要重启。

三条与本项目特有结构相关的处理：
- Config.OutboundConfigs 是整块 RawMessage 而非结构体切片，出站 diff 先解
  成逐条 JSON 再按 tag 索引；
- 比较对 JSON 空白与 map key 顺序不敏感，管理员在设置页重新格式化一下模板
  不该触发一次全员断线的重启；
- Reality 入站强制走重启：gRPC 的删+加不能可靠重建 Reality 鉴权器。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 9: `RestartXray` 接入热应用

**Files:**
- Modify: `web/service/xray.go`（`RestartXray`，`:103-124`）
- Create: `web/service/xray_hot_apply_test.go`

**Interfaces:**
- Consumes: `xray.ComputeHotDiff`、`xray.XrayAPI`、`(*xray.Process).GetAPIPort()`、`(*xray.Process).SetConfig()`。
- Produces: `func (s *XrayService) tryHotApply(process *xray.Process, newCfg *xray.Config) bool` — 返回 true 表示运行中的核心已与 newCfg 一致；返回 false 时调用方走全量重启。

- [ ] **Step 1: 写失败的测试**

创建 `web/service/xray_hot_apply_test.go`：

```go
package service

import (
	"testing"

	"a-ui/util/json_util"
	"a-ui/xray"
)

func rawf(s string) json_util.RawMessage { return json_util.RawMessage(s) }

func hotApplyBaseConfig() *xray.Config {
	return &xray.Config{
		LogConfig: rawf(`{"loglevel":"warning"}`),
		API:       rawf(`{"services":["HandlerService","RoutingService"],"tag":"api"}`),
		Stats:     rawf(`{}`),
		InboundConfigs: []xray.InboundConfig{
			{Port: 62789, Protocol: "dokodemo-door", Tag: "api", Settings: rawf(`{"address":"127.0.0.1"}`)},
		},
		OutboundConfigs: rawf(`[{"protocol":"freedom","tag":"direct"}]`),
		RouterConfig:    rawf(`{"domainStrategy":"AsIs","rules":[]}`),
	}
}

// 拿不到 api 端口时必须判定为「不能热应用」，让调用方走重启，
// 而不是拿 0 端口去连然后卡在超时上。
func TestTryHotApplyWithoutAPIPort(t *testing.T) {
	s := &XrayService{}
	oldCfg := hotApplyBaseConfig()
	newCfg := hotApplyBaseConfig()
	newCfg.RouterConfig = rawf(`{"domainStrategy":"AsIs","rules":[{"type":"field","domain":["geosite:openai"],"outboundTag":"direct"}]}`)

	process := xray.NewProcess(oldCfg)
	// 进程没启动，apiPort 为 0。
	if s.tryHotApply(process, newCfg) {
		t.Fatal("拿不到 api 端口时不该判定热应用成功")
	}
}

// 差分判定为「必须重启」时，绝不能去连核心。
func TestTryHotApplyRefusesRestartOnlyChange(t *testing.T) {
	s := &XrayService{}
	oldCfg := hotApplyBaseConfig()
	newCfg := hotApplyBaseConfig()
	// policy 没有重载接口。
	newCfg.Policy = rawf(`{"levels":{"0":{"handshake":10}}}`)

	process := xray.NewProcess(oldCfg)
	if s.tryHotApply(process, newCfg) {
		t.Fatal("policy 变化必须走重启")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./web/service/ -run TestTryHotApply -v
```

期望：`s.tryHotApply undefined` 编译失败。（`rawf` 与 `hotApplyBaseConfig` 在 `web/service` 的现有测试里都不存在，不会撞名；该包已有一个同名的 `decodeOutbounds` 测试辅助函数，但那与 `xray` 包里的实现不在同一个包，互不影响。）

- [ ] **Step 3: 实现 `tryHotApply` 并接进 `RestartXray`**

`web/service/xray.go` 的 `RestartXray`（第 103-124 行）改为：

```go
func (s *XrayService) RestartXray(isForce bool) error {
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("restart xray, force:", isForce)

	xrayConfig, err := s.GetXrayConfig()
	if err != nil {
		return err
	}

	if p != nil && p.IsRunning() {
		if !isForce && p.GetConfig().Equals(xrayConfig) {
			logger.Debug("not need to restart xray")
			return nil
		}
		// 配置确实变了，但改动可能全都落在核心支持运行时重载的部分。
		// 能热应用就不重启——重启会掐断所有人的连接，而绝大多数改动
		// （加减分流规则、增删入站）本来不需要付这个代价。
		if !isForce && s.tryHotApply(p, xrayConfig) {
			logger.Info("xray 配置改动已通过控制面热应用，无需重启")
			return nil
		}
		p.Stop()
	}

	p = xray.NewProcess(xrayConfig)
	result = ""
	return p.Start()
}

// tryHotApply 尝试把运行中的核心调成 newCfg 的样子，成功返回 true。
//
// 任何一步失败都返回 false，调用方随即全量重启——重启会把已经应用的那部分
// 一起清掉，所以中途失败不需要单独回滚。
//
// 调用方必须持有包级的 lock。
func (s *XrayService) tryHotApply(process *xray.Process, newCfg *xray.Config) bool {
	diff, ok := xray.ComputeHotDiff(process.GetConfig(), newCfg)
	if !ok {
		return false
	}
	if diff.Empty() {
		// 配置只有格式差异（空白、key 顺序）。同步快照即可，否则下一轮
		// cron 还会认为配置不同，白重启一次。
		process.SetConfig(newCfg)
		return true
	}

	apiPort := process.GetAPIPort()
	if apiPort <= 0 {
		logger.Debug("热应用：拿不到 xray 控制面端口，退回重启")
		return false
	}

	// 专用连接：查流量那条是每次现开现关的，两者生命周期不同。
	api := xray.XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		logger.Debug("热应用：连接 xray 控制面失败，退回重启:", err)
		return false
	}
	defer api.Close()

	// 顺序有讲究：先删后加，避免同 tag 的新旧对象在核心里撞名；
	// 出站先于路由下发，否则新规则会引用还不存在的出站 tag。
	for _, tag := range diff.RemovedInboundTags {
		if err := api.DelInbound(tag); err != nil {
			logger.Debug("热应用：删除入站 [", tag, "] 失败，退回重启:", err)
			return false
		}
	}
	for _, raw := range diff.AddedInbounds {
		if err := api.AddInbound(raw); err != nil {
			logger.Debug("热应用：新增入站失败，退回重启:", err)
			return false
		}
	}
	for _, tag := range diff.RemovedOutboundTags {
		if err := api.DelOutbound(tag); err != nil {
			logger.Debug("热应用：删除出站 [", tag, "] 失败，退回重启:", err)
			return false
		}
	}
	for _, raw := range diff.AddedOutbounds {
		if err := api.AddOutbound(raw); err != nil {
			logger.Debug("热应用：新增出站失败，退回重启:", err)
			return false
		}
	}
	if diff.RoutingConfig != nil {
		if err := api.ApplyRoutingConfig(diff.RoutingConfig); err != nil {
			logger.Debug("热应用：下发路由配置失败，退回重启:", err)
			return false
		}
	}

	process.SetConfig(newCfg)
	return true
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./web/service/ -run TestTryHotApply -v
make verify
```

期望：全绿。

- [ ] **Step 5: 提交**

```bash
git add web/service/xray.go web/service/xray_hot_apply_test.go
git commit -m "feat(xray): 配置改动优先走控制面热应用，失败退回重启

此前任何改动都整进程重启 xray，所有人断线几秒——而那个 10 秒的去抖 cron
消费一次标志就断一次。绝大多数改动（增删分流规则、增删入站出站）落在核心
支持运行时重载的部分，本不必付这个代价。

失败一律退回重启：重启会把已经应用的那部分一起清掉，所以中途失败不需要
单独回滚。拿不到控制面端口、连不上、任何一条 RPC 报错，都直接走老路径。

热应用成功后同步进程内的配置快照。不同步的话 Config.Equals 下一轮仍判定
配置不同，热更新会退化成延迟一轮的重启。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"
```

---

### Task 10: 端到端验证与收尾

单元测试证明不了热更新真的生效——那要一个真核心。

**Files:** 无改动（纯验证）。若发现缺陷，回到对应 Task 修。

- [ ] **Step 1: 起面板，记下 xray 的 PID**

```bash
cd /Users/caryallen/Desktop/AetherUI/AetherUI-main
XUI_DEBUG=true go run main.go
```

另开终端：

```bash
XRAY_PID=$(pgrep -f 'bin/xray-')
echo "改动前 PID: $XRAY_PID" | tee "$SCRATCH/e2e-hot-reload.txt"
```

- [ ] **Step 2: 改一条分流规则，确认进程没重启**

在面板的分流页面加一条规则（任选一个域名组与动作），保存。等 15 秒让那个 10 秒的重启消费任务跑过。

```bash
sleep 15
NEW_PID=$(pgrep -f 'bin/xray-')
echo "改动后 PID: $NEW_PID" | tee -a "$SCRATCH/e2e-hot-reload.txt"
[ "$XRAY_PID" = "$NEW_PID" ] && echo "PASS: 进程未重启" || echo "FAIL: 进程被重启了"
```

期望：`PASS`。若 FAIL，看面板日志里 `热应用：` 开头的 Debug 行，它会说明是哪一步退回了重启。

- [ ] **Step 3: 确认规则真的生效了，而不是"没重启也没生效"**

这是本任务最关键的一步。**进程没重启 ≠ 新规则已经在核心里。**

```bash
grep -c 'outboundTag' bin/config.json      # 面板写出的配置里有这条规则
```

再确认核心侧：暂时没有路由测试接口（那是计划 03），所以用实际流量验证——从一个已配置的客户端访问该规则覆盖的域名，确认它按新规则的动作被处理（proxy 走指定节点 / block 不通）。

把验证方式与结果记进 `$SCRATCH/e2e-hot-reload.txt`。

> **计划 03 落地后**，这一步应改用 `RoutingService.TestRoute` 直接查询命中的 outboundTag，比走真实流量可靠得多。届时回头把这条验收改掉。

- [ ] **Step 4: 验证会重启的路径仍然正常**

改一个**没有重载接口**的东西——在设置页切换「访问日志开关」（它改 `log` 段）：

```bash
XRAY_PID=$(pgrep -f 'bin/xray-')
# 在面板里切换访问日志开关并保存
sleep 15
NEW_PID=$(pgrep -f 'bin/xray-')
[ "$XRAY_PID" != "$NEW_PID" ] && echo "PASS: 按预期重启了" || echo "FAIL: 该重启却没重启"
```

期望：`PASS`。**这条比 Step 2 更重要**——热更新最危险的失效模式不是"该热更新却重启了"（只是没优化到），而是"该重启却判定成热应用了"（核心与面板的认知从此不一致）。

- [ ] **Step 5: 确认流量统计没有被热更新破坏**

热应用会删掉再加回入站，stats 计数器可能被清零。过一点流量，确认入站的上行/下行仍在增长，且 `XrayTrafficJob` 累加进数据库的数字连续。

若发现热应用后统计归零，**这是需要记进 CLAUDE.md 的行为差异**，不是缺陷——xray 的 stats 绑在 handler 上，handler 重建计数器就重置。面板侧是累加存库，归零只影响"本次采样差值"，不影响累计值。确认这一点后写进 Step 7 的文档更新。

- [ ] **Step 6: 确认最终 diff 干净**

```bash
make clean
git status
git diff main...HEAD --stat
```

期望：只有计划内的文件；没有 scratchpad 文件、调试打印、无关格式化。

- [ ] **Step 7: 更新 CLAUDE.md**

本计划改变了两条 CLAUDE.md 已记载的事实，必须同步：

1. 「`util/sys/psutil.go` 用 `//go:linkname` 侵入 gopsutil 内部包，升级 gopsutil 时会断」——**已消除**，删掉这条。
2. 「`go.mod` 声明 `go 1.21`…CI 用 Go 1.22 构建；依赖版本较老（gin 1.7.1、xray-core v1.4.2 仅用于 gRPC stats 客户端，与实际运行的 `bin/xray-*` 版本无关）」——**已过时**，改为记录新的 Go 版本、xray-core 版本，并说明 xray-core 现在还承担控制面客户端与 `infra/conf` 配置构建，**必须与 `bin/xray-*` 保持同版本**。

同时在「重启去抖机制」那一段后面补上热更新的说明：`RestartXray` 现在先试 `tryHotApply`，以及"新增会影响 xray 配置的字段时除了扩展 `Config.Equals`，还要考虑它属于可热重载还是必须重启"。

- [ ] **Step 8: 提交文档更新，推分支**

```bash
git add CLAUDE.md
git commit -m "docs: 同步工具链升级与配置热更新带来的架构变化

三处过时描述：linkname 侵入 gopsutil 的已知偏差已消除；Go 与 xray-core 的
版本记录；xray-core 从'仅用于 gRPC stats、与 bin/xray-* 版本无关'变成
'承担控制面客户端与 infra/conf 配置构建，必须与 bin/xray-* 同版本'。

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FfVJvpLLYVCJ4yDEvGowES"

git push
gh run watch
```

---

## 风险与回退

| 风险 | 触发点 | 处置 |
|---|---|---|
| `xray-core` 26.x 拉高 `x/sys` 后 gopsutil v3.21.3 编译失败 | Task 4 Step 3 | 升 gopsutil 到能编译的最低版本，**单独一次提交**。`util/sys/psutil.go` 已在 Task 2 解耦，导入路径变化（v3 → `/v4`）只影响 `web/service/server.go` 与 `util/sys/sys_darwin.go` 的 import 行 |
| gin 1.7.1 在 Go 1.27 下编译失败 | Task 3 / Task 4 | 同上，单独提交。注意 gin 1.8+ 的 `c.BindWith` 等 API 有变化，改动面主要在 `web/controller/` |
| 二进制体积增长过多，发版包变大 | Task 4 Step 6 | 属预期代价，记录数值即可。若确实不可接受，唯一的替代是放弃热更新（回到 §2.1 的另一条路），那要重开计划 |
| 热应用判定过宽，核心与面板状态不一致 | Task 10 Step 4 | 这是最严重的失效模式。发现后立刻在 `ComputeHotDiff` 里把对应情形加进"必须重启"，补一条表驱动用例，**不要试图在热应用侧兜底** |
| 存量部署的模板迁移写坏了管理员的自定义模板 | Task 6 | `TestEnsureRoutingServiceLeavesOtherKeysAlone` 守着这条。上线前在一份真实的自定义模板上手工验证一次 |
| `AddInbound` 因 `"listen": null` 被核心拒绝 | Task 10 Step 2 | 入站不指定监听地址时 `GenXrayInboundConfig` 把 `Listen` 留空，而 `json_util.RawMessage.MarshalJSON` 对空值输出 `null`（`util/json_util/json.go:10`），热应用发出去的就是 `"listen":null`。`conf.InboundDetourConfig` 反序列化 null 应当是无操作，但**必须实测**：Task 10 Step 2 用一个没填监听地址的入站验证。若核心拒绝，在 `diffInbounds` 序列化前把空 `Listen` 字段剔掉，并补一条对应的表驱动用例 |

## 不在本计划范围

- 路由测试接口（`RoutingService.TestRoute`）→ 计划 03。本计划只把客户端接上。
- 出站探测、geodata 浏览器、出站订阅 → 计划 03。
- Reality 的入站表单与分享链接 → 计划 04。本计划只让 `ComputeHotDiff` 认出 Reality 入站并强制重启。
- 修复 `xray.Config` 丢弃未知顶层键的问题（路线图 §2.4）。属用户可见行为变更。
- 引入 `golangci-lint`。会一次性冒出大量既有告警，与本计划的改动混在一起无法评审；若要做，单独一个 chore 提交，先把基线记下来。
