# xray 核心版本不随 a-ui 更新回退 设计文档

- 状态：设计
- 日期：2026-09-09
- 关联：`docs/superpowers/specs/2026-09-05-panel-version-update-design.md`（面板一键更新／回退，本文档要修正它的一个副作用）

## 1. 背景与目标

管理员在面板首页点「切换版本」把 xray 核心升到 26.9.9 之后，只要更新一次面板（或降级一次），核心就会**静默退回发版包里那份 26.7.28**。整条链路是：

1. `ServerService.UpdateXray` 把新核心写进 `bin/xray-linux-<arch>`（`web/service/server.go:243`）。
2. 任何一次面板更新／降级最终都执行 `install.sh` 的 `install_a-ui()`，它先 `rm -rf /usr/local/a-ui/`，再把发版包整个铺开（`install.sh:1171-1175`）。
3. 发版包的 `bin/` 里带着**仓库中那份** `bin/xray-linux-<arch>`（`.github/workflows/release.yml` 打包步骤）。

管理员对此没有任何提示：面板首页的版本号会安静地变回旧值，`Process.Start()` 又从不回传启动失败，出问题时首页照样显示 `running`。

同一个坑项目已经在 IP 库上踩过并修好了——`config/config.go:64-72` 把 ip2region 库挪到 `/etc/<name>/` 正是因为「安装目录会被 `rm -rf`」。xray 二进制不能照搬那个解法（它是可执行文件，且 `xray.GetBinaryPath()` 返回相对路径 `bin/xray-<GOOS>-<GOARCH>`，与 systemd 的 `WorkingDirectory=/usr/local/a-ui/` 绑定），需要另一条路。

### 目标

- 更新／降级 a-ui 之后，机器上原有的 xray 核心与 geo 数据**原样保留**。
- 全新安装时直接装 GitHub 上的**最新稳定版** xray，而不是发版包里的快照。
- 上述两件事都不引入「安装可能失败」的新路径：拉不到就退回发版包那份。

### 非目标

- **不做 xray 自动定时升级。** xray 升级要停核心重启，所有用户断流几秒；而 `Process.Start()` 把 `cmd.Run()` 丢进 goroutine 后直接返回 nil，启动失败不回传，面板首页仍显示 `running`。这种操作必须由管理员挑时间做，不能在半夜自动发生，也不该在管理员点「更新面板」时顺带发生。
- **不做版本号比较后取新。** 无条件保留即可。仓库 tag 格式不统一这件事已经在面板版本管理里坑过一次（见关联文档 §2），不再引入第二处语义化版本解析。面板上的「切换版本」本来就能切到任意版本，逃生口已经存在。
- **不改 `release.yml`。** 发版包继续带 xray，它的角色从「用户机器上实际跑的核心」变成「拉不到时的兜底种子」，与 `bin/ipdb.dat` 同构。
- 不动 `bin/config.json`：它每次 `RestartXray` 都重新生成，保留没有意义。

## 2. 已核实的事实

以下均从仓库代码读出，不是推断：

| 事实 | 出处 |
|---|---|
| 发版包的 `bin/` 里带着仓库中的 xray 与三个 dat | `release.yml` 打包步骤：`cp bin/xray-linux-${ARCH} bin/geoip.dat bin/geosite.dat bin/ipdb.dat dist/a-ui/bin/` |
| `install.sh` 先 `rm -rf /usr/local/a-ui/` 再解压发版包 | `install.sh:1171-1175` |
| 解压后 `cd a-ui`，此后 pwd 即 `/usr/local/a-ui` | `install.sh:1177` |
| 面板一键更新／回退下发的是 **main 分支**的 `install.sh` | `web/service/panel_version.go:31` `installScriptURL` |
| `a-ui update` 同样 `curl` main 分支的 `install.sh` | `a-ui.sh:107`、`a-ui.sh:126` |
| `die_restoring_panel` 在 `a_ui_stopped=1` 时会把面板重新拉起再 `exit 1` | `install.sh:30-41` |
| `UpdateXray` 会 `StopXray()` → 覆盖三个文件 → `defer RestartXray(true)` | `web/service/server.go:243-305` |
| `downloadXRay` 把 zip 落在**当前工作目录**（`os.Create(fileName)`），架构名映射 `amd64→64`、`arm64→arm64-v8a` | `web/service/server.go:204-241` |
| `GetXrayVersions` 拉的是 `/releases`，**含 prerelease** | `web/service/server.go:177-202` |
| `StopXray()` 在核心未运行时返回 error，但 `UpdateXray` 丢弃该返回值 | `web/service/xray.go:233-241` |
| xray 相关路径全是相对 `bin/` 的 | `xray/process.go:30-44` |
| 面板与核心的版本耦合是**单向**的 | `xray/api.go:76-78` 注释：「老版本的解析器编译不出新协议的入站」 |
| IP 库放在 `/etc/<name>/` 正是为了躲开 `rm -rf` | `config/config.go:64-72` 及其注释 |
| `a-ui geo` 拉的是 **Loyalsoldier/v2ray-rules-dat**，不是 xray 官方 geo 数据 | `a-ui.sh:15-16` |
| `a-ui` 菜单可开启 geo 数据的 cron 自动更新 | `a-ui.sh:687` `enable_auto_update_geo` |
| 子命令退出码 0 会被 `install.sh` 误判为成功，是本项目要严防的静默失败 | `main.go:321` 注释 |
| `clearTrafficShaping` 是「不连数据库的子命令」的现成样板 | `main.go:382-408` |
| CI 只跑 `make verify`（vet + test + build），不覆盖任何 shell 脚本 | `.github/workflows/ci.yml` |
| `web/service/server.go` 目前没有任何测试 | 仓库中不存在 `web/service/server*_test.go` |

### 2.1 单向耦合的方向（本设计成立的前提）

`xray/api.go` 的热应用把面板生成的 JSON 交给**面板进程内的** `infra/conf` 编译成 typed message，再经 gRPC 下发给运行中的核心。因此：

- **面板旧 + 核心新**：面板只会生成它自己认识的旧格式配置，核心（新）能接受。代价是核心的新协议／新字段在面板里用不上。**落在安全侧。**
- **面板新 + 核心旧**：面板可能生成核心不认识的 typed message。**这才是危险方向**，也是 `xray/api.go:76-78` 那句注释真正警告的东西。

本设计让核心可以领先于面板的 `go.mod`，方向正确。

需要标明的推断：xray 若在某个版本**改变了已有 message 的语义**（而非只增加新字段），旧解析器生成的 message 可能被新核心误解。xray 一般保持配置兼容，但这一点仓库里没有证据支撑，属于残余风险，见 §7。

## 3. 设计：一条判据分出两个分支

判据只有一条——**`bin/xray-linux-<arch>` 有没有被备份下来**。不看 geo 数据：核心诉求是 xray 版本，geo 是附带的，两者可能只成功了一半（管理员删过其中某个文件，或上一次安装本身就是坏的）。

```
下载发版包成功
  ↓
备份 bin/ 下存在的：xray-linux-<arch>、geoip.dat、geosite.dat  ← 新增，在 stop 之前
  ↓
systemctl stop a-ui   (a_ui_stopped=1)
  ↓
rm -rf /usr/local/a-ui/
  ↓
tar zxvf → cd a-ui
  ↓
┌─ 备份到了 xray（更新 / 降级）→ 逐个恢复备到的文件，不联网
└─ 没备份到 xray（全新安装）  → a-ui xray -update latest
  ↓
systemctl restart a-ui  (面板启动时自己拉起核心)
```

**备份放在 `systemctl stop` 之前**，与 `install.sh:1163-1168` 那段注释同源：最容易失败的步骤要排在停机之前，免得留下一台面板已停、又没起来的机器。复制正在被运行中进程使用的可执行文件在 Linux 上是安全的（`cp` 读的是文件内容，不影响已打开的 inode）。

这条分岔的关键性质：**更新 a-ui 绝不顺手改动 xray 版本**。升级时机仍归管理员，只是不再会被打回去。

## 4. Go 侧改动

### 4.1 拆分 `UpdateXray`

`web/service/server.go`：

拆成三层，**下载与解包必须分开**，否则替换逻辑无法脱离网络测试（§9.1）：

```go
// extractXrayFiles 把一个已经下载好的 zip 解到 bin/ 下，不联网、不碰进程。
// 这是唯一一处知道「zip 里的 xray / geosite.dat / geoip.dat 该落到哪」的代码，
// 也是本次改动中唯一可以脱网单测的一层。
func extractXrayFiles(zipPath string) error

// replaceXrayFiles = 下载 + 解包，不碰进程。
// 面板按钮与 a-ui xray 子命令共用它——架构名映射只应该存在于一处。
func (s *ServerService) replaceXrayFiles(version string) error {
	zipPath, err := s.downloadXRay(version)  // 已有实现，不动
	// ... defer 清理 zip
	return extractXrayFiles(zipPath)
}

// UpdateXray 是面板按钮的入口，对外行为不变：停核心 → 替换 → 重启核心。
func (s *ServerService) UpdateXray(version string) error {
	// StopXray / defer RestartXray(true) 保持原样
	return s.replaceXrayFiles(version)
}
```

`extractXrayFiles` 不挂在 `ServerService` 上：它不需要任何服务状态，做成包级函数能让测试直接调，不必构造 service。

`downloadXRay` 不动。它把 zip 落在当前工作目录，而 `install.sh` 走到调用点时 pwd 正好是 `/usr/local/a-ui`，`bin/` 相对路径天然对得上。

### 4.2 解析「最新稳定版」

新增一个只取最新稳定版的函数，**不复用 `GetXrayVersions`**——后者拉 `/releases`（含 prerelease、draft），面板上让管理员自己挑没问题，自动安装则不该装 pre-release。改用 GitHub 保证语义的 `/repos/XTLS/Xray-core/releases/latest`。

`GetXrayVersions` 与面板「切换版本」的行为完全不变。

### 4.3 新增子命令 `a-ui xray`

照 `clearTrafficShaping`（`main.go:382`）的样子写——**不连数据库**，因为 `replaceXrayFiles` 不需要：

```
a-ui xray -update latest      # 装 GitHub 最新稳定版
a-ui xray -update v26.9.9     # 装指定版本
```

三条约束：

1. **不重启核心。** 一次性进程用 `os/exec` 起的 xray 是它的子进程，进程一退核心跟着死。`install.sh` 随后的 `systemctl restart a-ui` 会让面板自己拉起核心，时序天然正确。
2. **失败退出码非 0**（`main.go:321` 的既有约束）。
3. 在 `flag.Usage` 的 Commands 列表与 `default` 分支的提示里补上这个子命令，与现有五个保持一致。

## 5. install.sh 改动

新增两个函数，插在 §3 流程图标出的两处：

- `backup_xray_assets()` — 把 `bin/` 下三个文件复制到 `mktemp -d` 出来的目录，路径记在一个全局变量里。目录不存在（全新安装）时不算失败，只是不产生备份。
- `restore_or_install_xray()` — 备份到了 xray 二进制就把**备到的文件逐个** `cp -f` 回 `bin/`（没备到的那个保持发版包那份）；没备到 xray 就调 `a-ui xray -update latest`。

**保留范围包含两个 geo 数据文件**，理由不是「配套」这么简单：`a-ui.sh:15-16` 显示 `a-ui geo` 拉的是 **Loyalsoldier/v2ray-rules-dat 增强版**，不是 xray 官方 geo 数据，而 `a-ui.sh:687` 还允许把它挂上 cron 定时更新。所以机器上那份很可能是管理员**刻意换掉**的，被发版包打回官方版是一次静默的意图改写——而且比 xray 被降级更隐蔽，因为 geo 数据的版本在面板上根本看不见。

代价：仓库里更新过的 geo 数据从此推不到已有机器。可接受，因为 `a-ui geo` 与面板「安装 xray」两个独立入口都能拿到。

## 6. 失败路径

| 环节 | 处理 | 理由 |
|---|---|---|
| 备份失败（磁盘满等） | `die_restoring_panel`，**不执行 `rm -rf`** | 唯一必须 fail close 的一步：删掉就找不回来了。此时尚未 `systemctl stop`，`a_ui_stopped=0`，面板从头到尾没停过 |
| 恢复失败（`cp` 报错） | 打印警告，保留发版包那份，继续安装 | 那份能用，不该为此中断整个安装 |
| `a-ui xray -update latest` 失败 | 打印警告，保留发版包那份，继续安装 | 同上。与 `routing_validate.go` 的 fail open 同取向：辅助手段自身故障不能把用户锁在门外 |

三条合起来的性质：**这次改动不新增任何「安装可能失败」的路径**，只新增一条「安装可能失败」的路径被堵死（备份失败时不再往下走）。

## 7. 一个正面的副作用与一处残余风险

**副作用（正面）**：按现在的行为，降级到 v1.2.8 之前的 a-ui 会把核心一起降到 Xray 1.4.x 时代的构建——那里面**没有 `RoutingService` 符号**，配置热更新必然连不上、静默退回整进程重启。改完之后核心保持不动，这个坑一并消失（§2.1 的单向耦合保证「面板旧 + 核心新」在安全侧）。

**残余风险**：§2.1 末尾那条推断——xray 若改变已有 message 的语义，旧面板生成的配置可能被新核心误解。缓解手段是现成的：面板「切换版本」可以随时把核心切回任意版本，包括与 `go.mod` 同版的那个。

## 8. 文档要改写

CLAUDE.md「运维脚本」一节里这句：

> 因此升级 `xray-core` 依赖时，必须同时把 `bin/xray-linux-amd64` 与 `bin/xray-linux-arm64` 换成同版本的官方构建，否则面板内的 `infra/conf` 与用户机器上实际运行的核心会错版。

需要改写：`bin/xray-*` 的角色变成**种子／兜底**，用户机器上实际运行的核心可能比它新。同时要把 §2.1 的单向耦合写进去——不写清方向，以后照旧理解的人会做出错误判断（比如为了「同版本」而去主动降级用户机器上的核心）。

「已知偏差与注意事项」一节里关于 `a-ui update` 会把 xray 降级的那段，改成描述新行为。

## 9. 测试与验证

### 9.1 Go 侧（可自动化）

- `a-ui xray` 的 flag 解析：照 `main_flags_test.go` 的形式加用例，覆盖 `-update latest`、`-update <版本>`、缺参数、未知参数。
- `extractXrayFiles`：喂一个当场构造的 zip（含 `xray` / `geosite.dat` / `geoip.dat` 三个条目），验证三个文件确实落到 `bin/` 下且内容正确。这一层脱网，是本次改动里唯一能被自动化覆盖的实质逻辑（§4.1 把下载单独分出去就是为了这个）。
- 回归：`UpdateXray` 拆分后对外行为不变，这一点靠上一条间接覆盖。

### 9.2 install.sh（无法自动化，必须人工验证）

**这是本设计唯一没有自动化防线的地方，必须写明。** CI 只跑 `make verify`，仓库里没有 shell 测试、没有 shellcheck，而这次改动恰好落在 `rm -rf` 前后——风险最高的一段。

验证必须在一台**干净的 VPS 或容器**上做完整链路，不能拿生产机当第一个试验场：

1. 全新安装 → 确认装上的是 GitHub 最新稳定版 xray，不是发版包里那份。
2. 面板里「切换版本」切到一个更旧的版本 → 更新 a-ui → 确认核心仍是那个旧版本（证明保留生效，且方向上不是「总是装最新」）。
3. 降级 a-ui 到上一个 tag → 确认核心不变。
4. 断网（或把 GitHub 域名指到黑洞）后全新安装 → 确认退回发版包那份且安装成功。
5. 备份目录只读／磁盘满的情况下更新 → 确认在 `rm -rf` 之前就 `die`，且 `/usr/local/a-ui/` 完好、面板照常运行。

第 5 项最关键——它验证的是唯一一条 fail close 的路径。

### 9.3 提交前门禁

`make verify`（vet + test + build），与现有流程一致。
