# xray 核心版本不随 a-ui 更新回退 设计文档

- 状态：设计
- 日期：2026-09-09
- 关联：`docs/superpowers/specs/2026-09-05-panel-version-update-design.md`（面板一键更新／回退，本文档要修正它的一个副作用）

## 1. 背景与目标

管理员在面板首页点「切换版本」把 xray 核心升到 26.9.9 之后，只要更新一次面板（或降级一次），核心就会**静默退回发版包里那份 26.7.28**。整条链路是：

1. `ServerService.UpdateXray` 把新核心写进 `bin/xray-linux-<arch>`（设计阶段读到的位置是 `web/service/server.go:243`——那是改动前的单一大函数，Task 1 已把它拆成 `openXrayZip`/`extractXrayFiles`/`UpdateXray` 三层，见 §4.1，当前源码里已经没有一段连续代码对应这里描述的行为）。
2. 任何一次面板更新／降级最终都执行 `install.sh` 的 `install_a-ui()`，它先 `rm -rf /usr/local/a-ui/`，再把发版包整个铺开（设计阶段读到的位置是 `install.sh:1171-1175`；这段代码本身没变，只是因为 §5 新增的 `backup_xray_assets`/`restore_or_install_xray` 插在它前面，当前位置是 `install.sh:1278-1282`）。
3. 发版包的 `bin/` 里带着**仓库中那份** `bin/xray-linux-<arch>`（`.github/workflows/release.yml` 打包步骤）。

管理员对此没有任何提示：面板首页的版本号会安静地变回旧值，`Process.Start()` 又从不回传启动失败，出问题时首页照样显示 `running`。

同一个坑项目已经在 IP 库上踩过并修好了——`config/config.go:64-72` 把 ip2region 库挪到 `/etc/<name>/` 正是因为「安装目录会被 `rm -rf`」。xray 二进制不能照搬那个解法（它是可执行文件，且 `xray.GetBinaryPath()` 返回相对路径 `bin/xray-<GOOS>-<GOARCH>`，与 systemd 的 `WorkingDirectory=/usr/local/a-ui/` 绑定），需要另一条路。

### 目标

- 更新／降级 a-ui 之后，机器上原有的 xray 核心与 geo 数据**原样保留**。
- 全新安装时直接装 GitHub 上的**最新发布版** xray，而不是发版包里的快照。
  > **这条目标在实施中被 §4.2 的实测数据修正过**：最初写的是「最新稳定版」，设想用 GitHub 语义上代表稳定发布的 `/releases/latest`。2026-09-09 实测发现 xray-core 几乎把所有发布都标记成 `prerelease`（最近 15 个里 14 个），`/releases/latest` 因此会稳定给出一个比发版包自带核心（26.7.28）还旧的版本——「装到最新稳定版」这个目标本身就建立在一个不成立的前提上（这个项目没有「经常发布的稳定版」）。目标改写为「与面板『切换版本』列表首项同源的最新发布版」，实现细节见 §4.2。
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
| `install.sh` 先 `rm -rf /usr/local/a-ui/` 再解压发版包 | 设计阶段：`install.sh:1171-1175`；实施后（同一段代码，被 §5 新增函数顶到后面）：`install.sh:1278-1282` |
| 解压后 `cd a-ui`，此后 pwd 即 `/usr/local/a-ui` | 设计阶段：`install.sh:1177`；实施后：`install.sh:1284` |
| 面板一键更新／回退下发的是 **main 分支**的 `install.sh` | `web/service/panel_version.go:31` `installScriptURL` |
| `a-ui update` 同样 `curl` main 分支的 `install.sh` | `a-ui.sh:107`、`a-ui.sh:126` |
| `die_restoring_panel` 在 `a_ui_stopped=1` 时会把面板重新拉起再 `exit 1` | 设计阶段：`install.sh:30-41`；实施后：`install.sh:37-48` |
| **（设计阶段的旧实现）** `UpdateXray` 会 `StopXray()` → 覆盖三个文件 → `defer RestartXray(true)` | `web/service/server.go:243-305`（改动前的单函数实现；Task 1 已拆成 `openXrayZip`（251）/`extractXrayFiles`（276）/`UpdateXray`（306），当前没有一段连续代码对应这一行描述的行为，见 §4.1） |
| `downloadXRay` 把 zip 落在**当前工作目录**（`os.Create(fileName)`），架构名映射 `amd64→64`、`arm64→arm64-v8a` | 设计阶段：`web/service/server.go:204-241`；实施后（新增 `archive/zip` import 使全文件整体下移一行）：`web/service/server.go:205-242` |
| `GetXrayVersions` 拉的是 `/releases`，**含 prerelease** | 设计阶段：`web/service/server.go:177-202`；实施后：`web/service/server.go:178-203` |
| `StopXray()` 在核心未运行时返回 error，但 `UpdateXray` 丢弃该返回值 | `web/service/xray.go:233-241` |
| xray 相关路径全是相对 `bin/` 的 | `xray/process.go:30-44` |
| 面板与核心的版本耦合是**单向**的 | `xray/api.go:76-78` 注释：「老版本的解析器编译不出新协议的入站」 |
| IP 库放在 `/etc/<name>/` 正是为了躲开 `rm -rf` | `config/config.go:64-72` 及其注释 |
| `a-ui geo` 拉的是 **Loyalsoldier/v2ray-rules-dat**，不是 xray 官方 geo 数据 | `a-ui.sh:15-16` |
| `a-ui` 菜单可开启 geo 数据的 cron 自动更新 | `a-ui.sh:687` `enable_auto_update_geo` |
| 子命令退出码 0 会被 `install.sh` 误判为成功，是本项目要严防的静默失败 | `main.go:321` 注释 |
| `clearTrafficShaping` 是「不连数据库的子命令」的现成样板 | `main.go:391-417`（Fix Round 1 更正：原写 `382-408`，是本次改动之外的位移，与 xray 版本保留无关） |
| CI 只跑 `make verify`（vet + test + build），不覆盖任何 shell 脚本 | `.github/workflows/ci.yml` |
| `web/service/server.go` 目前没有任何测试 | 仓库中不存在 `web/service/server*_test.go`（实施后已有 `web/service/server_xray_update_test.go`，见 §9.1） |

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

**备份放在 `systemctl stop` 之前**，与 `install.sh:1163-1168`（实施后同一段注释被顶到了 `install.sh:1268-1272`）那段注释同源：最容易失败的步骤要排在停机之前，免得留下一台面板已停、又没起来的机器。复制正在被运行中进程使用的可执行文件在 Linux 上是安全的（`cp` 读的是文件内容，不影响已打开的 inode）。

这条分岔的关键性质：**更新 a-ui 绝不顺手改动 xray 版本**。升级时机仍归管理员，只是不再会被打回去。

## 4. Go 侧改动

### 4.1 拆分 `UpdateXray`

`web/service/server.go`：**实现比这份草稿早期版本多拆了一层**——最初设想的两层（下载、解包）会把「包已下载」和「包已验证可读」混在一起，`StopXray` 要插的缝隙其实是「验证完、还没开始写文件」，不是「下载完」。所以最终是三层：

```go
// openXrayZip 打开并验证一个 xray 发布 zip，返回 reader 与清理函数。
//
// 与解包分成两步，是为了让调用方能在「包已确认可读」和「开始写文件」之间
// 插入自己的动作——UpdateXray 正是在这个缝隙里停核心的：下载几十 MB 和
// 包损坏检测都发生在停机之前，用户完全不断流。
//
// 清理函数只关闭文件句柄，不删除 zip：zip 是谁下载的谁负责删。
func openXrayZip(zipPath string) (*zip.Reader, func(), error)

// extractXrayFiles 把 zip 里的 xray / geosite.dat / geoip.dat 解到三个显式
// 给出的路径。
//
// 目标路径是参数而不是直接取 xray.GetBinaryPath()：那些是相对路径，而本包
// 的测试会 chdir 到仓库根，写死就等于让测试覆盖仓库里真实的 xray 二进制。
//
// 条目不存在时在删除目标文件之前就返回，所以缺条目不会破坏已有的核心。
func extractXrayFiles(r *zip.Reader, binPath, geositePath, geoipPath string) error

// UpdateXray 是面板「切换版本」按钮的入口：下载 → 验证 → 停核心 → 解包 → 重启。
//
// StopXray 必须排在 openXrayZip 之后：下载几十 MB 与包损坏检测都不该让用户
// 白断一次流。
func (s *ServerService) UpdateXray(version string) error {
	zipFileName, err := s.downloadXRay(version)
	// ... defer os.Remove(zipFileName)

	r, closeZip, err := openXrayZip(zipFileName)
	// ... defer closeZip()

	s.xrayService.StopXray()
	defer func() { s.xrayService.RestartXray(true) }()

	return extractXrayFiles(r, xray.GetBinaryPath(), xray.GetGeositePath(), xray.GetGeoipPath())
}

// ReplaceXrayFiles 下载指定版本并替换 bin/ 下的三个文件，不停也不启核心。
//
// 导出（而不是像早期草稿设想的那样是包内私有），因为它要被 main 包的
// a-ui xray 子命令跨包调用。供该子命令使用：那是个一次性进程，它用
// os/exec 起的 xray 会随进程退出一起死掉，所以绝不能在这里重启核心。
// 安装脚本随后的 systemctl restart a-ui 会让面板自己把核心拉起来。
func (s *ServerService) ReplaceXrayFiles(version string) error {
	zipFileName, err := s.downloadXRay(version)
	// ... defer os.Remove(zipFileName)

	r, closeZip, err := openXrayZip(zipFileName)
	// ... defer closeZip()

	return extractXrayFiles(r, xray.GetBinaryPath(), xray.GetGeositePath(), xray.GetGeoipPath())
}
```

`openXrayZip` / `extractXrayFiles` 都不挂在 `ServerService` 上：都不需要任何服务状态，做成包级函数能让测试直接调，不必构造 service；`extractXrayFiles` 收显式路径参数而不是内部调 `xray.GetBinaryPath()`，是为了让测试能重定向到临时目录（见函数注释）。

`downloadXRay` 不动。它把 zip 落在当前工作目录，而 `install.sh` 走到调用点时 pwd 正好是 `/usr/local/a-ui`，`bin/` 相对路径天然对得上。

### 4.2 解析「最新发布版」

**这一节的初始设计被实测数据推翻了，把过程留下来——它比结论本身更值得后人复核。**（标题用的是最终结论「最新发布版」；下面这段「最初设想」用的是被推翻前的措辞「最新稳定版」，两者不是同一件事，别被字面相似绕进去。）

最初设想是新增一个只取最新稳定版的函数，不复用 `GetXrayVersions`——后者拉 `/releases`（含 prerelease、draft），面板上让管理员自己挑没问题，自动安装则不该装 pre-release，于是打算改用 GitHub 保证语义的 `/repos/XTLS/Xray-core/releases/latest`（该端点只返回 `prerelease=false` 且非 draft 的最新一条）。

实现前先拿真实数据核了一遍（2026-09-09 实测）：

| 端点 | 返回 | `prerelease` | 发布时间 |
|---|---|---|---|
| `/releases/latest` | `v26.3.27` | `false` | 2026-03-27 |
| `/releases`（首条） | `v26.9.9` | `true` | 2026-09-08 |

再往前翻，xray-core 最近 15 个发布里 **14 个标记为 `prerelease`**，唯一的正式版就是那个半年前的 `v26.3.27`。也就是说 `/releases/latest` 在这个仓库上不是「偶尔滞后」，而是**稳定地**给出一个比发版包自带的兜底核心（26.7.28）还旧的版本——全新安装装到的核心反而比不做这个功能更旧，直接违反 §1 的目标。根子在于 xray-core 把 `prerelease` 当成发布流程的常规标记而不是「不稳定」的信号，这个项目的惯例与 `/releases/latest` 端点的语义假设（prerelease=测试版、非 prerelease=推荐版）不匹配。

**改用 `GetXrayVersions()` 取首项**（`firstReleaseTag`，`web/service/server.go`），与面板「切换版本」列表同源、同口径：两者看到的「最新」是同一个东西，不会出现「子命令装的版本」与「面板列表第一项」对不上的怪现象。新增的只有 `firstReleaseTag` 这一层薄壳，专门挡两种 `GetXrayVersions` 本身不会报错但会导致下载 404 的空值：列表为空（GitHub 限流时响应是对象不是数组，`GetXrayVersions` 会在 `Unmarshal` 处报错，这里挡不到；但限流之外还有别的空列表可能）、首项是空字符串。

`GetXrayVersions` 与面板「切换版本」的行为完全不变。

### 4.3 新增子命令 `a-ui xray`

照 `clearTrafficShaping`（`main.go:391`）的样子写——**不连数据库**，因为 `ReplaceXrayFiles` 不需要：

```
a-ui xray -update latest      # 装 GitHub 最新发布版（§4.2 改用 GetXrayVersions() 首项，与面板「切换版本」列表同源，可能是 prerelease）
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

### 5.1 实施中新增的三处细化（草稿阶段未预见）

- **`install_en.sh` 必须同步改。** CLAUDE.md「运维脚本」一节明文要求四个 shell 脚本成对维护——`install.sh`/`install_en.sh` 一对，`a-ui.sh`/`a-ui_en.sh` 另一对。这两个函数只加进 `install.sh` 的话，英文安装包用户完全拿不到本设计的功能，而且是静默的（脚本不报错，只是行为退回改动前）。最终两个脚本都加了同构的 `backup_xray_assets` / `restore_or_install_xray`。
- **拉取要有超时。** `web/service/server.go` 里 `downloadXRay`／`GetXrayVersions` 用的是 Go 默认 `http.Client`，没有 `Timeout`。GitHub 只是被丢包（不是拒绝连接）时，`http.Get` 会永久挂住——而 `restore_or_install_xray` 走到调用 `a-ui xray -update latest` 这一步时，面板已经停了、`/usr/local/a-ui/` 已经删了重铺、systemd 单元还没起来，挂住就是把机器留在这个半死状态里出不来。调用点因此包一层 `timeout 600`（`command -v timeout` 判断该命令是否存在，不存在就退化成直接调用——不能让「没有 `timeout` 命令」这种边缘情况变成安装失败）。600 秒足够慢速网络拉完约 37MB 的核心；超时后走 §6 表里「拉取失败」那一行的既有 fail open 分支。
- **备份目录不能落在 `/tmp`。** `mktemp -d` 默认给的路径在 systemd 发行版上通常在 `/tmp`，而多数发行版把 `/tmp` 挂成内存 tmpfs。三个待备份文件（xray 核心 + geoip.dat + geosite.dat）合计能到 60~70MB，这次拷贝还发生在 `systemctl stop` **之前**——面板与 xray 都在正常提供服务，是这台机器内存占用的峰值时刻。小内存 VPS 上 `/tmp` 装不下，会让 `backup_xray_assets` 本身失败，而它是 §6 表里唯一 fail close 的一步，代价是管理员从此彻底无法更新面板。改用 `mktemp -d /usr/local/a-ui-xray-backup-XXXXXX`：与 `/usr/local/a-ui/` 同级但不同名的兄弟目录，不占 tmpfs 配额，也不会被 `rm /usr/local/a-ui/ -rf`（结尾的 `/` 只删这一棵目录树）误删。

## 6. 失败路径

| 环节 | 处理 | 理由 |
|---|---|---|
| 备份失败（磁盘满等） | `die_restoring_panel`，**不执行 `rm -rf`** | 唯一必须 fail close 的一步：删掉就找不回来了。此时尚未 `systemctl stop`，`a_ui_stopped=0`，面板从头到尾没停过 |
| 恢复失败（`cp` 报错） | 打印警告，保留发版包那份，继续安装 | 那份能用，不该为此中断整个安装 |
| `a-ui xray -update latest` 失败 | 打印警告，保留发版包那份，继续安装 | 同上。与 `routing_validate.go` 的 fail open 同取向：辅助手段自身故障不能把用户锁在门外 |
| 目标机器装的是 v1.6.0 之前的旧 `a-ui` 二进制（没有 `xray` 子命令） | `a-ui xray -update latest` 落进 `main.go` 的 `default:` 分支，**必须以非 0 退出** | 实施中发现的第四处裁决：`default:` 分支原本 `return`（退出码 0）。旧二进制吃到未知子命令时打印一段 usage 提示就正常退出，`install.sh` 的 `if ! a-ui xray -update latest` 会把这个 0 判成「拉取成功」——退回上面「同上」那一行的 fail open 警告一句都不会打，静默留下一份根本没被替换的 xray，且没有任何提示。改成 `os.Exit(1)` 后，这种情况会正确落进上面那两行「拉取/更新失败」的 fail open 分支，打印警告、保留发版包那份 |

四条合起来的性质：**这次改动不新增任何「安装可能失败」的路径**，只新增一条「安装可能失败」的路径被堵死（备份失败时不再往下走），并堵上一个旧二进制场景下会误判成功的退出码空子。

## 7. 一个正面的副作用与一处残余风险

**副作用（正面）**：按现在的行为，降级到 v1.2.8 之前的 a-ui 会把核心一起降到 Xray 1.4.x 时代的构建——那里面**没有 `RoutingService` 符号**，配置热更新必然连不上、静默退回整进程重启。改完之后核心保持不动，这个坑一并消失（§2.1 的单向耦合保证「面板旧 + 核心新」在安全侧）。

**残余风险**：§2.1 末尾那条推断——xray 若改变已有 message 的语义，旧面板生成的配置可能被新核心误解。缓解手段是现成的：面板「切换版本」可以随时把核心切回任意版本，包括与 `go.mod` 同版的那个。

## 8. 文档要改写

CLAUDE.md「运维脚本」一节里这句：

> 因此升级 `xray-core` 依赖时，必须同时把 `bin/xray-linux-amd64` 与 `bin/xray-linux-arm64` 换成同版本的官方构建，否则面板内的 `infra/conf` 与用户机器上实际运行的核心会错版。

需要改写：`bin/xray-*` 的角色变成**种子／兜底**，用户机器上实际运行的核心可能比它新。同时要把 §2.1 的单向耦合写进去——不写清方向，以后照旧理解的人会做出错误判断（比如为了「同版本」而去主动降级用户机器上的核心）。

「已知偏差与注意事项」一节里关于 `a-ui update` 会把 xray 降级的那段，改成描述新行为。

**已落实**（Task 5）：CLAUDE.md「运维脚本」一节这两段都已改写，并补上 §4.2 那次推翻 `/releases/latest` 的实测结论；「面板版本与一键更新」一节「回退有两个后果」也一并改写为一条（xray 核心不再是回退的后果）；`web/assets/js/util/panel-version.js` 的二次确认框文案同步改写。「已知偏差与注意事项」一节实际没有相关段落——`a-ui update` 那段原文落在「运维脚本」节内，一并处理。

## 9. 测试与验证

### 9.1 Go 侧（可自动化）

- `a-ui xray` 的 flag 解析：照 `main_flags_test.go` 的形式加用例，覆盖 `-update latest`、`-update <版本>`、缺参数、未知参数。
- `openXrayZip` / `extractXrayFiles`：喂一个当场构造的 zip（含 `xray` / `geosite.dat` / `geoip.dat` 三个条目），验证 `openXrayZip` 能读出 reader、`extractXrayFiles` 把三个文件解到显式传入的路径且内容正确；另覆盖条目缺失、zip 本身损坏两种情况。这两层脱网，是本次改动里唯一能被自动化覆盖的实质逻辑（§4.1 把下载单独分出去就是为了这个），落在 `web/service/server_xray_update_test.go`。
- 回归：`UpdateXray` / `ReplaceXrayFiles` 拆分后对外行为不变，这一点靠上一条间接覆盖。
- `firstReleaseTag`：覆盖空列表、首项空字符串、正常首项三种情况（§4.2）。

### 9.2 install.sh（无法自动化，必须人工验证）

**这是本设计唯一没有自动化防线的地方，必须写明。** CI 只跑 `make verify`，仓库里没有 shell 测试、没有 shellcheck，而这次改动恰好落在 `rm -rf` 前后——风险最高的一段。

验证必须在一台**干净的 VPS 或容器**上做完整链路，不能拿生产机当第一个试验场：

1. 全新安装 → 确认装上的是 GitHub 最新**发布版** xray（不是「最新稳定版」——xray-core 把大多数发布都标记成 `prerelease`，这是这个项目的发布惯例而不是不稳定的信号，装到 `prerelease` 是设计要的行为，见 §4.2），不是发版包里那份。核对方法：`go version -m /usr/local/a-ui/bin/xray-linux-<arch>` 读出的版本，应与 GitHub API `/repos/XTLS/Xray-core/releases`（**不是** `/releases/latest`）返回列表的**首条** `tag_name` 一致——也是面板首页「切换版本」列表的第一项。
2. 面板里「切换版本」切到一个更旧的版本 → 更新 a-ui → 确认核心仍是那个旧版本（证明保留生效，且方向上不是「总是装最新」）。
3. 降级 a-ui 到上一个 tag → 确认核心不变。
4. 断网（或把 GitHub 域名指到黑洞）后全新安装 → 确认退回发版包那份且安装成功。
5. 备份目录只读／磁盘满的情况下更新 → 确认在 `rm -rf` 之前就 `die`，且 `/usr/local/a-ui/` 完好、面板照常运行。

第 5 项最关键——它验证的是唯一一条 fail close 的路径。

### 9.3 提交前门禁

`make verify`（vet + test + build），与现有流程一致。
