# 面板版本显示与一键更新 / 回退 设计文档

- 状态：设计
- 日期：2026-09-05
- 关联：`docs/superpowers/specs/2026-09-03-modernization-roadmap.md` 计划 05（面板运维能力）

## 1. 背景与目标

面板界面上目前**没有任何地方显示自己的版本号**。`config.GetVersion()` 只被用来给静态资源 URL 拼缓存 buster（`web/controller/util.go:85` 的 `cur_ver`），管理员要知道装的是哪一版，只能 SSH 上去看。

后果有两个：

1. 出了问题不知道自己在哪一版，提 issue 和排查都缺一个最基本的坐标。
2. 有新版发布时没有任何提示，除非管理员主动去 GitHub 看。而 `a-ui update` 又是无条件强制重装最新版，管理员在"不知道有没有新版"和"装了才知道变了什么"之间没有中间态。

目标：

- 侧边栏底部常驻显示当前版本号。
- 有新版本时在版本号上打红点。
- 点击版本号弹出面板：当前版本 / 是否最新 / 手动重查 / 查看 GitHub 发布页 / 最近 5 个版本的回退列表。
- 一键更新到最新版，一键回退到最近 5 个版本中的任意一个。
- 更新过程可见：状态与日志能在面板里看到，不必 SSH。

### 非目标

- 不做增量更新、不做二进制差分。整包替换，复用 `install.sh`。
- 不做自动更新。任何版本变更都必须管理员点击并二次确认。
- 不做「回退时一并回滚数据库」。回退不动数据库，见 §7。
- 不做更新前的自动备份。`install.sh` 已经保证 `/etc/<name>/` 下的数据库不被触碰。
- 不提供「关闭更新检查」的设置项，见 §9 决策 1。

## 2. 已核实的事实

以下均从仓库代码读出，不是推断：

| 事实 | 出处 |
|---|---|
| `config/version` 由 CI 在打 tag 时写入，本地是 `0.3.2`，线上是 tag 名 | `.github/workflows/release.yml` 的「写入版本号」步骤 |
| 发布产物是 `a-ui-linux-<arch>.tar.gz` 与 `a-ui-linux-<arch>-english.tar.gz` | `release.yml` 打包步骤 |
| `install.sh` 已支持指定版本：`bash install.sh v1.4.0` | `install.sh` 末行 `install_a-ui $1`，函数内 `last_version=$1` 分支 |
| `install.sh` 会 `systemctl stop a-ui` 并 `rm -rf /usr/local/a-ui/` | `install.sh:1169-1176` |
| `a-ui.service` 是 `Type=simple`，没有 `Restart=` | `a-ui.service` |
| `setup_wizard` 对已配置的机器会跳过 | `install.sh:216`，靠 `a-ui bootstrap -check -json` 输出 `"skipped": true` |
| `config_after_install` 有无条件的 `read -p`，没有非交互旁路 | `install.sh:130` |
| `install.sh` 会把 `/usr/bin/a-ui` 覆盖成 **main 分支**的管理脚本 | `install.sh:1186-1196`，URL 写死 `raw.githubusercontent.com/.../main/a-ui.sh` |
| 仓库 tag 格式不统一：`0.3.4.4` 与 `v1.2.10` 并存 | `git tag` |
| 现有的 xray 版本切换是同类功能的参照实现 | `ServerController.getXrayVersion` / `installXray`，`ServerService.GetXrayVersions` / `UpdateXray` |

### 2.1 真机实测结论（2026-09-05）

测试机：Oracle Cloud 东京，Ubuntu 20.04.6 LTS，aarch64，systemd 245，面板 v1.5.0，走 Caddy 域名分支（面板监听 `127.0.0.1:54321`，basePath `/pYjjUckGKS52/`）。

**① `systemd-run` 的 transient unit 确实独立于父 service 的 cgroup。**

先坐实前提——生产面板的 cgroup 树里，xray 就在里面：

```
CGroup: /system.slice/a-ui.service
        ├─716632 /usr/local/a-ui/a-ui
        └─716641 bin/xray-linux-arm64 -c bin/config.json
Type=simple  Restart=no  KillMode=control-group
```

用一个模拟 `a-ui.service` 的 dummy unit 做逃逸测试（不触碰生产面板——stop 它会连带杀死 xray，所有用户断网）：

| 子进程的起法 | 所在 cgroup | 父 service 被 `systemctl stop` 后 |
|---|---|---|
| `systemd-run --unit=X --collect` | `/system.slice/X.service` | **存活** |
| `setsid sleep 300 &`（对照组） | `/system.slice/<父>.service` | **被杀死** |

对照组特意加了 `setsid` 仍被杀，印证了 `setsid` 改的是会话而不是 cgroup。**将来不要把 `systemd-run` "优化"成 `setsid` 或 `nohup`，那两个都逃不掉。**

`--collect` 在 systemd 245 上可用（236+ 引入）。

**② `setup_wizard` 确实会跳过。** 判定点是只读命令，直接实测：

```
$ a-ui bootstrap -check -json
{ "mode": "", "panelUrl": "", "skipped": true }
```

`install.sh:216` 的 `grep -q '"skipped": true'` 命中，向导整段跳过，已配好的域名 / basePath / Caddy 不会被覆盖。

**③ `config_after_install` 在 stdin 为 `/dev/null` 时走安全分支。**

```
read -p "..." config_confirm  </dev/null   →  返回码 1，变量为空
→ 不等于 y/Y → 走 else 分支（不写库）
→ else 内判 is_fresh_install：/etc/a-ui/a-ui.db 存在 → 0
→ 只 echo 一行「当前属于版本升级，保留之前设置项」，不执行任何 a-ui setting
```

⚠️ **这一条的验证有一个坑，记下来免得复现时再踩**：`[[ -f /etc/a-ui/a-ui.db ]]` 必须**以 root 执行**。该目录权限是 `d---------`（000），非 root 用户 stat 不到，判断会得到 `is_fresh_install=1` 这个**完全相反且后果严重**的结论（那个分支会重置账号密码和面板端口）。`install.sh` 本身以 root 运行，所以真实行为是安全的那一支。

**④ 网络与 API 字段核对**（同机实测）：

```
api.github.com/repos/SienFeng/AetherUI/releases?per_page=5   → 200，5 条
  tag_name / published_at(RFC3339) / html_url / draft / prerelease 字段齐全，当前全部 draft=false prerelease=false
x-ratelimit-limit: 60   remaining: 59            → 6 小时一次的 job 远够用
raw.githubusercontent.com/.../main/install.sh    → 200, 61777 bytes, 0.19s
releases/download/v1.5.0/a-ui-linux-arm64.tar.gz → 302（正常重定向）
/var/log 可写
```

**尚未做的一项**：完整跑一遍 `install.sh <tag>` 的端到端更新。它会 `systemctl stop a-ui`，连带杀死同 cgroup 的 xray——这台是有真实流量的生产机，需要一个可接受的断网窗口。安排在实现完成后进行。上面 ①②③ 已覆盖该流程中全部三个不确定点，剩下的是验证而非探索。

## 3. 版本比较：不做语义化解析

仓库现有 tag 里 `0.3.4.4`、`v1.2.10`、`v1.5.0` 并存。字符串比较会把 `v1.2.9 > v1.2.10` 判反；自己写 semver 解析处理不了 `0.3.4.4` 这种四段式，而写一个"够用"的解析器等于埋一个只在特定版本号下才发作的错判。

改用 **GitHub releases 列表的天然顺序**（API 默认按创建时间降序）：

| 情况 | 判定 | UI |
|---|---|---|
| `Current == releases[0].TagName` | 已是最新 | 绿色对勾 |
| `Current` 在列表里且下标 > 0 | 有更新 | 红点 + 「有新版本 vX」 |
| `Current` 不在列表里 | 未知 | 灰字「未在发布列表中」，**不打红点** |

第三行同时覆盖两种真实情况：本地开发（`config/version` = `0.3.2`）、当前版本太老已经翻出前 5 条。这两种都既不该冒充"有更新"，也不该谎报"已是最新"。此时更新按钮仍然可用——管理员知道自己在做什么。

**只取 `draft == false && prerelease == false` 的 release。** 当前仓库全是正式 release，行为不变；将来若打 prerelease，不会因此误报"有新版可更新"。

**拉 `?per_page=10`，回退列表只取前 5，但 `KnownCurrent` 用全部 10 条判定。** 两个列表长度不同是有意的：落后 6~10 个版本的管理员恰恰是最需要看到红点的人，用 5 条判定会把他们归进"未在发布列表中"而不给任何提示。回退列表仍只给 5 条——回退是应急手段，不是时间机器。

## 4. 数据结构与缓存

新增 `web/service/panel_version.go`。

```go
type ReleaseBrief struct {
    TagName     string `json:"tagName"`
    PublishedAt int64  `json:"publishedAt"` // Unix 毫秒
    HtmlUrl     string `json:"htmlUrl"`
}

type PanelVersionInfo struct {
    Current           string         `json:"current"`
    Latest            string         `json:"latest"`
    HasUpdate         bool           `json:"hasUpdate"`
    KnownCurrent      bool           `json:"knownCurrent"` // Current 是否出现在 releases 里
    Releases          []ReleaseBrief `json:"releases"`     // 最近 5 个
    CheckedAt         int64          `json:"checkedAt"`    // 0 = 从未成功
    LastError         string         `json:"lastError"`
    Updatable         bool           `json:"updatable"`
    UnsupportedReason string         `json:"unsupportedReason"`
}
```

缓存是**包级变量 + `sync.RWMutex`**，与本项目 service 一律无状态、跨请求状态放包级变量的既有形态一致（见 `web/service/xray.go` 的 `p` 与 `isNeedXrayRestart`）。

缓存**不落库**。它是一份可以随时重建的外部世界快照，落库要走「新增设置项的五步」（`defaultValueMap` / `entity.AllSetting` / `CheckValid` / getter / `models.js`），漏掉最后一步会让整个保存配置接口失败——为一份重启后 6 小时内必然自愈的缓存付这个代价不划算。代价是面板重启后 UI 上会短暂显示「尚未检查」，直到 job 首次跑完。

**首次检查在 `Server.startTask` 里延迟触发**：`cron.AddJob("@every 6h", ...)` 注册后第一次执行是 6 小时后，不做延迟触发的话新装的面板要等 6 小时才显示版本状态。参照 `XrayTrafficJob` 的做法，起一个 `go func(){ time.Sleep(10s); job.Run() }()`——延迟 10 秒是为了避开面板刚启动时和 xray 启动抢网络。

**`PanelVersionService.Get()` 保证 `Releases` 永远序列化成 JSON 数组，绝不是 `null`。** `PanelVersionInfo` 零值的 `Releases` 是 nil 切片，`encoding/json` 会把它编成 `"releases":null`；而缓存为空（面板刚启动前 10 秒、连不上 `api.github.com`、或一键更新成功后的那一刻）是完全常见的状态，`CheckedAt == 0` 时 `Get()` 仍会照常重算 `Updatable`。前端模板 `common_sider.html` 里 `panelVersion.updatable && panelVersion.releases.length` 一旦对 `null.length` 求值就会抛 `TypeError`——而这段 `<template slot="content">` 因为没有 `slot-scope`，Vue 2.6 把它编译进了 `#app` 根实例的 render 函数，不需要 popover 被打开就会参与每次求值，一次异常会让 `_update` 变成 no-op，整页从此停止响应式更新（状态轮询、入站列表、更新进度框全部冻结）。`Get()` 因此在返回前把 nil 规整为 `[]ReleaseBrief{}`，前端 `(panelVersion.releases || []).length` 是第二道防线，两处都不能省。

## 5. 更新执行

### 5.1 必须用 systemd-run 脱离 cgroup

`a-ui.service` 是 `Type=simple`，面板通过 `os/exec` 起的子进程和面板在**同一个 cgroup**。`install.sh:1169` 的 `systemctl stop a-ui` 在默认 `KillMode=control-group` 下会把整个 cgroup 杀光——更新脚本死在 `systemctl stop` 与 `tar zxvf` 之间的某处，留下一台：

- `/usr/local/a-ui/` 已被 `rm -rf`（或删了一半）
- `a-ui.service` 已停止
- 没有 `Restart=`，systemd 不会救

的机器。**面板从此彻底消失，且没有任何东西会把它拉回来**，只能 SSH 上去手动重装。这是本功能唯一的灾难性失败模式。

```
systemd-run --unit=a-ui-update --collect \
            --description="AetherUI 面板更新" \
            /bin/bash -c '<脚本>'
```

- transient unit 有自己的 cgroup，`systemctl stop a-ui` 碰不到它。
- `--collect`：unit 退出后自动清理，名字随之释放，不会长期占用。
- **unit 名固定为 `a-ui-update`，不带时间戳，这是有意的。** 3 分钟的前端超时不代表更新真的失败或结束——发版包已涨到约 40MB，慢速 VPS 上 `install.sh` 可能仍在 `wget` 或刚进 `rm -rf /usr/local/a-ui/`。超时框的「关闭」按钮会把前端状态打回 `idle`、重新点亮更新/回退按钮，此时若第二次点击生成一个带新时间戳的 unit 名，systemd 不会拒绝——两个 `install.sh` 会并发跑，同时下载同一个文件、同时 `rm -rf` + `tar` 同一个目录，结果是一台安装损坏且不会自愈的机器。固定 unit 名把互斥判断交给 systemd 本身：第二次 `systemd-run` 在第一个 unit 还在跑时会直接报错（`Unit a-ui-update.service already exists`），这条报错通过 `runUpgradeCommand` 的 `CombinedOutput` 原样透传给管理员，而不是被更「优化」的写法（比如带时间戳、或先 `systemctl reset-failed` 再起）悄悄绕开。曾经的判断（「固定名在上一次更新的 unit 尚未清理时会冲突」）把这个冲突当成缺点——它其实是本功能唯一需要的并发保护，`--collect` 保证的是正常结束后的自动清理，不是为了让固定名"可以重来"。

### 5.2 内层脚本

```bash
d=$(mktemp -d) && curl -fLso "$d/install.sh" https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh \
  && bash "$d/install.sh" <tag> </dev/null; rc=$?; rm -rf "$d"; exit $rc
```

整体输出重定向到 `/var/log/a-ui-update.log`，用 `>` **覆盖**而不是 `>>` 追加：只保留最近一次更新的日志，排查够用，也不会无限增长（`tar zxvf` 会列出包里每个文件，单次输出几十 KB）。

- `curl -f`：HTTP 错误码时返回非零。不加的话 GitHub 返回 404 页面会被当成脚本内容执行。
- `</dev/null`：走 `config_after_install` 的 else 分支，不写库。见 §2.1 ③（已实测）。
- 不传 `--wizard-only`、不传 `force`：让 `setup_wizard` 的幂等判断生效。
- **脚本落到 `mktemp -d` 建的私有目录，不是固定路径 `/tmp/a-ui-install.sh`。** 固定路径 + 全局可写的 `/tmp` + 以 root 执行，等于给本机任一非特权用户一条 TOCTOU 提权路径：预先在该路径放一个自己拥有的文件，在 `curl` 与 `bash` 之间把内容换成任意脚本即可拿到 root 代码执行。现代发行版的 `fs.protected_regular` 能缓解，但 `install.sh` 明确支持 CentOS（3.10 内核无此保护），不能依赖这条防线。`mktemp -d` 生成的目录默认 `0700` 且名字不可预测，脚本结束时 `rm -rf` 清理，不留痕迹。这是本次改造相对既有 `a-ui.sh` 的 `update()`（`bash <(curl -Ls ...)`，根本不落盘）新增的暴露面，必须补上。

### 5.3 前置检查（`Updatable`）

任一不满足则 `Updatable = false`，前端只显示版本号和「查看发布」链接，不显示任何更新/回退按钮：

| 检查 | 挡住的是 |
|---|---|
| `runtime.GOOS == "linux"` | macOS 本地开发 |
| `/usr/local/a-ui/a-ui` 存在 | 非标准路径安装、源码直接跑 |
| `/etc/systemd/system/a-ui.service` 存在 | Docker（仓库有 `Dockerfile`）、非 systemd 系统 |
| `exec.LookPath("systemd-run")` 成功 | systemd 版本过老或裁剪过的镜像 |

在 Docker 里跑 `install.sh` 是纯粹的破坏——它会 `systemctl`（不存在）、`rm -rf /usr/local/a-ui/`（容器里就是应用本体）。这一条不是防御性编程，是防止一个明确会毁掉环境的操作被点到。

`UnsupportedReason` 要具体到哪一条没过，前端原样显示。只说「不支持」会让管理员无从下手。

### 5.4 tag 白名单：命令注入防线

tag 会被拼进 `bash -c` 的字符串。**两道校验，都是硬拒绝，绝不 fail open**：

1. 正则 `^[A-Za-z0-9._-]{1,64}$`
2. 必须精确出现在当前缓存的 `Releases` 列表里

第 2 条顺带实现了「只能回退到最近 5 个版本」——列表本身只有 5 条。

这与 `routing_validate.go` 的 fail open 策略是**相反**的取向，且必须相反：那里放行的是"我们没法证明它非法"的配置，最坏后果是 xray 拒绝启动；这里放行的是一段会以 root 执行的字符串，最坏后果是任意命令执行。

缓存为空（从未成功检查过）时，任何 tag 都不在列表里 → 更新一律拒绝，错误信息提示先点刷新。这是正确行为：连版本列表都拉不到的网络环境，更新也必然失败在下载那一步，早拒绝比在 systemd unit 里失败可读得多。

### 5.5 响应必须先于执行

面板自己马上就要被 `systemctl stop` 掉，等更新结果等不到。`systemd-run` 是 fire-and-forget：它把 unit 交给 systemd 后立即返回，此时更新还没开始跑。所以：

1. `systemd-run` 返回非零 → 说明连 unit 都没起来 → 如实返回失败，面板毫发无损。
2. `systemd-run` 返回 0 → 返回「更新已在后台开始」，**不代表更新会成功**。

文案必须诚实：「更新已在后台开始，约 1 分钟后面板会自动重启。若 3 分钟后版本号仍未变化，请查看更新日志。」

### 5.6 更新日志接口

`POST /server/upgradeLog` 返回 `/var/log/a-ui-update.log` 的**末尾 200 行**。

不做这个接口的话，更新失败的表现是：`install.sh` 的 `die_restoring_panel` 把面板重启回来，管理员看到面板正常、版本号没变、点击时的提示已经消失——完全等同于「点了没反应」，而真正的原因（GitHub 拉不通 / 指定版本不存在 / 磁盘满）只写在一个没人知道的文件里。

路径硬编码，不接受任何参数——这是一个读文件的接口，参数化就是路径穿越。

## 6. 前端

### 6.1 版本区必须走 Vue mixin

`common_sider.html` 被 `index` / `inbounds` / `routing` / `setting` 四个页面共用，但每个页面各有一个 `new Vue({el:'#app'})`，data 互不相干。版本区的 `v-if` / `@click` 写在 sider 里，`version` 这个 data 就必须**四个实例都有**——少一个，那个页面就会引用 undefined。

新增 `web/assets/js/util/panel-version.js`，导出一个 Vue mixin：

```js
const panelVersionMixin = {
    data() { return { panelVersion: {...}, versionPopoverVisible: false, ... }; },
    methods: { loadPanelVersion(), refreshPanelVersion(), upgradePanel(tag), ... },
    mounted() { this.loadPanelVersion(); },
};
```

四个页面 `new Vue({ mixins: [panelVersionMixin], ... })`。Vue 2 的 mixin 会把 data / methods / mounted 合并，不用抄四遍，将来改也只改一处。脚本在 `common/js.html` 里引入（四个页面都 include 了它）。

### 6.2 两个既有陷阱

- **弹窗必须留在 `<a-layout id="app">` 内。** Vue 2 只编译 `el` 指向的那棵子树。分流页的三个 `a-modal` 曾整块落在 `#app` 之后——页面渲染完全正常、数据照常加载、控制台不报错，但所有按钮点了毫无反应。`web/html_test.go` 的 `TestVueDirectivesLiveInsideAVueRoot` 守着这条，改完模板必跑。
- **改了 `web/assets/js/**` 而 `config/version` 没变，浏览器会命中 `max-age=31536000` 的强缓存拿旧文件。** 本地开发用 `XUI_DEBUG=true`；部署时版本号由 CI 写入，自然会变。

### 6.3 布局

`<a-layout-sider id="sider">` 底部固定一块版本区。ant-design-vue 1.x 的 sider 内部结构是 `.ant-layout-sider-children`，给它 `display:flex; flex-direction:column`，版本区 `margin-top:auto` 贴底。样式加在 `web/assets/css/custom.css`（已有 `#sider` 相关规则）。

侧栏 `collapsed-width="0"`，md 以下（<768px）完全收起，版本区随之不可见。移动端改走 `#sider-drawer`（`a-drawer`，见 6.4 之外的独立抽屉）唤出导航菜单，但**刻意不在 drawer 里放版本区副本**：一是 drawer 是临时浮层，常驻信息放在里面语义不对；二是一键更新在移动端本就是高风险操作——更新过程中面板会重启，移动网络的连接稳定性又明显弱于桌面。代价是**移动端完全没有版本入口**：看不到版本号，也看不到是否有新版可更新，更新和回退功能对移动端管理员不可用；需要更新时得切到桌面浏览器或 SSH 上机操作。这是本次改造刻意接受的功能空白，不是遗漏。

### 6.4 弹出层结构

`a-popover`，`trigger="click"`，照截图的层次：

```
当前版本                                    [↻ 刷新]
────────────────────────────────────────────────
              v1.5.0  ✓
            已是最新版本
         [ 🐙 查看发布 ]
────────────────────────────────────────────────
🕐 版本回退                                    [∨]
   选择要回退到的版本（近 5 个版本）
   ○ v1.4.1      2026/09/04
   ○ v1.4.0      2026/09/03
   ○ v1.3.0      2026/09/02
                              [ 回退到选中版本 ]
```

有新版时中间区域变成 `v1.4.0 → v1.5.0` 和一个「更新到 v1.5.0」按钮。

### 6.5 更新过程的可见性

点击更新 / 回退 → `a-modal` 二次确认（回退时文案额外写明 §7 的两条后果）→ POST → 弹出一个不可关闭的进度模态框：

1. 「正在启动更新…」
2. 每 3 秒 POST `/server/panelVersion`（读缓存，极轻）。请求失败 = 面板正在重启，显示「面板重启中…」并继续重试。
3. `current` 变成目标版本 → 「更新完成，当前版本 vX」+ 「刷新页面」按钮。
4. 3 分钟仍未变化 → 「更新可能失败」+ 「查看日志」按钮（拉 `/server/upgradeLog`，用已有的 `text_modal.html` 展示）。

没有这一层，管理员点完按钮后面对的是一个正在自杀的面板和一片空白。

## 7. 回退的两个后果

二次确认框里必须写明，这两条都是从代码读出来的事实：

1. **xray 核心会跟着回退。** `install.sh` 解压的发版包里带着 `bin/xray-linux-<arch>`（`release.yml` 打包步骤），会覆盖机器上现有的那份。管理员先前通过面板「安装 xray」升级过的核心也一并被覆盖。
   - 具体风险：v1.2.8 之前的发版包里是 Xray 1.4.x 时代的构建，**没有 `RoutingService` 符号**，配置热更新会连不上并静默退回整进程重启。当前「最近 5 个版本」都在 v1.2.8 之后，暂时不触发，但只要回退列表往前滑就会遇到。
2. **数据库不回滚。** `AutoMigrate` 只加列不删列，回退后旧代码读到多余的列会忽略——GORM 按 struct 读，多余列无害。但**新版本写进旧列的新语义无法还原**。数据不会丢，新版本新加的功能会失效。

确认文案：「回退到旧版本可能导致新版新增的功能失效，xray 核心也会一并回退到该版本携带的构建。数据库和已有配置不会丢失。」

**另有一条不写进 UI 但要记在这里的偏差**：`install.sh` 无论装哪个版本，`/usr/bin/a-ui` 管理脚本和 `install.sh` 本身都是从 **main 分支**拉的最新版（`install.sh:1189`）。所以回退得到的是「旧二进制 + 新管理脚本」。管理脚本主要是运维菜单，这个组合基本无害；但若将来 `a-ui.sh` 调用了旧二进制没有的子命令或 flag，对应的菜单项会报错。**改动 `a-ui bootstrap` / `a-ui setting` 的参数时要记得这一点。**

## 8. 接口

```
POST /server/panelVersion         → PanelVersionInfo（只读缓存，不发外网请求）
POST /server/refreshPanelVersion  → PanelVersionInfo（强制重查，1 分钟节流）
POST /server/upgradePanel         body: version=<tag>  → 立即返回，见 §5.5
POST /server/upgradeLog           → { lines: [...] }   末尾 200 行
```

挂在 `ServerController` 下，`g.Use(a.checkLogin)` 已覆盖鉴权。

节流参照 `getXrayVersion` 的既有写法（`lastGetVersionsTime`，1 分钟），防的是管理员连点刷新撞上 GitHub 60 次/小时/IP 的未认证限速。

## 9. 已确认的决策

1. **不加「关闭更新检查」的设置项。** 面板本来就会主动连 GitHub（`getXrayVersion`）。新增设置项要同步改 5 处，漏掉 `models.js` 那一处会让**整个保存配置接口失败**（`ObjectUtil.cloneProps` 只克隆目标已有的 key，新字段在提交体里根本不存在 → Gin 绑成零值 → 若校验拒绝零值则整个 `updateAllSetting` 报错，端口、证书路径一起遭殃）。真有需求再加。
2. **不做自动更新。** 任何版本变更都要管理员点击 + 二次确认。
3. **回退开放到最近 5 个 release。** 既能应急，又不至于随手回到半年前那种行为无法预测的版本。
4. **更新日志覆盖写而非追加。** 只保留最近一次。

## 10. 改动文件清单

新增：
```
web/service/panel_version.go        PanelVersionService + 包级缓存
web/service/panel_version_test.go
web/job/panel_version_job.go        PanelVersionJob（@every 6h）
web/assets/js/util/panel-version.js  Vue mixin（引入点见下）
```

修改：
```
web/controller/server.go     4 个新路由与 handler
web/web.go                   startTask 注册 job + 延迟 10s 的首次触发
web/html/xui/common_sider.html  sider 底部版本区 + a-popover
web/html/common/js.html      引入 panel-version.js
```

`panel-version.js` 放进 `common/js.html` 而不是四个页面各引一遍：它是四个内页共用的，抄四遍将来必然漏改。代价是 `login.html` 也 include 了 `js.html`，登录页会白下载这几 KB——与已经在里面的 `xray.js`（登录页同样用不到，体积大得多）同级，可接受。Chart.js 之所以没进 `js.html`，是因为那一份体积不小，量级不同。

```
web/assets/css/custom.css    版本区贴底样式
web/html/xui/index.html      new Vue 挂 mixin
web/html/xui/inbounds.html   同上
web/html/xui/routing.html    同上
web/html/xui/setting.html    同上
CLAUDE.md                    新增小节
```

## 11. 测试策略

Go 单测（`web/service/panel_version_test.go`）：

| 用例 | 断言 |
|---|---|
| `Current == releases[0]` | `HasUpdate == false`，`KnownCurrent == true` |
| `Current` 在列表下标 2 | `HasUpdate == true`，`Latest == releases[0]` |
| `Current` 不在列表 | `HasUpdate == false`，`KnownCurrent == false` |
| releases 含 draft / prerelease | 被过滤，不进列表也不参与判定 |
| `upgradePanel("v1.0.0; rm -rf /")` | 拒绝（正则） |
| `upgradePanel("v9.9.9")`（合法格式但不在列表） | 拒绝（白名单） |
| 缓存为空时任意 tag | 拒绝 |
| `Updatable` 各前置条件缺失 | `UnsupportedReason` 指出具体哪一条 |

GitHub API 用 `httptest.Server` 打桩，端点 URL 抽成包级变量供测试覆盖。**不在单测里真的 exec `systemd-run`**——命令行的组装抽成纯函数 `buildUpgradeCommand(tag) []string` 单独断言。

模板：`web/html_test.go` 的 `TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot`。

真机验证：§2.1 的 ①②③④ 已于 2026-09-05 实测完毕。剩余一项待实现后进行——

- 在生产机上完整走一遍面板内更新与回退（`install.sh <tag>` 会 `systemctl stop a-ui` 并连带杀死同 cgroup 的 xray，需要一个可接受的断网窗口）。验收点：面板版本号变化、xray 恢复运行、Caddy 配置与 basePath 未被改动、`/etc/a-ui/*.db` 未被触碰、账号密码未被重置。

## 12. 风险

| 风险 | 等级 | 处置 |
|---|---|---|
| `install.sh` 在无 tty 下卡住或走错分支 | ~~高~~ 已排除 | §2.1 ③ 实测：`read` 返回 1、走 else、`is_fresh_install=0`、不写库。**但这个结论依赖 `/etc/<name>/<name>.db` 存在**——若将来改了数据库路径或 `config/name`，判断会翻转到「重置账号密码和端口」那一支，改动这两处时必须重测 |
| 更新脚本被自己杀死 | ~~高~~ 已排除 | §2.1 ① 实测：`systemd-run` 的 unit 在父 service 被 stop 后存活；对照组（含 `setsid`）被杀 |
| tag 命令注入 | 高 | 正则 + 白名单双重硬拒绝 |
| Docker 内点到更新 | 中 | `Updatable` 前置检查 |
| GitHub API 限速 | 低 | 6 小时 job + 1 分钟节流；失败只记 `LastError`，不打扰 |
| 回退后 xray 核心一并降级 | 中 | 二次确认文案写明 |
| 更新失败无从排查 | 中 | `/server/upgradeLog` + 前端进度框超时提示 |

## 13. 参考

- `.github/workflows/release.yml` — 发布产物与版本写入
- `install.sh:1130-1215` — `install_a-ui`，指定版本安装的入口
- `install.sh:214-235` — `setup_wizard` 的幂等判断
- `web/service/server.go` — xray 版本切换，同类功能的参照
- `docs/superpowers/specs/2026-09-04-caddy-domain-bootstrap-design.md` §9 — 「失败不锁面板」原则
