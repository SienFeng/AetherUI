package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"a-ui/config"
	"a-ui/logger"
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
	upgradeLogPath = "/var/log/a-ui-update.log"
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
	Releases          []ReleaseBrief `json:"releases"`  // 最多 rollbackListSize 条
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
	// 脚本落到 mktemp -d 建的私有目录而不是固定的 /tmp 路径：固定路径 +
	// 全局可写的 /tmp + root 执行，等于给本机非特权用户一条 TOCTOU 提权路径
	// （在 curl 与 bash 之间改写文件内容）。mktemp -d 的目录是 0700 且名字不可预测。
	inner := fmt.Sprintf(
		"d=$(mktemp -d) && curl -fLso \"$d/install.sh\" %s && bash \"$d/install.sh\" %s </dev/null; rc=$?; rm -rf \"$d\"; exit $rc",
		installScriptURL, tag,
	)
	inner = fmt.Sprintf("{ %s ; } >%s 2>&1", inner, upgradeLogPath)
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
	// 一台装过面板的机器上用 go run 做开发时，上面四条都会通过，而更新会真的去
	// rm -rf 掉 /usr/local/a-ui/ 那份生产安装。要求当前进程就是那份安装本身。
	exe, err := os.Executable()
	if err != nil {
		return false, "无法确定当前程序路径，出于安全考虑不允许一键更新"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if exe != panelBinaryPath {
		return false, "当前运行的不是标准安装路径下的面板（" + exe + "），一键更新只对 " + panelBinaryPath + " 生效"
	}
	return true, ""
}

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
}

var panelVersionCache versionCache

type PanelVersionService struct{}

func fetchReleases() ([]ReleaseBrief, error) {
	resp, err := panelVersionHTTPClient.Get(panelReleasesURL)
	if err != nil {
		return nil, common.NewError("拉取 GitHub 发布列表失败:", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, common.NewErrorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	// 限速时 GitHub 返回的是 {"message": "..."} 这样的对象而不是数组，
	// 直接 Unmarshal 进切片会报错——这正是我们要的，不能静默当成空列表。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, common.NewError("拉取 GitHub 发布列表失败:", err)
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
		rollback = rollback[:rollbackListSize:rollbackListSize]
	}
	updatable, reason := checkUpdatable()

	panelVersionCache.mu.Lock()
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
	// Go 的 nil 切片会被 encoding/json 编成 null，而前端模板里
	// `panelVersion.releases.length` 会对 null 抛 TypeError——那段模板编译在
	// #app 根实例的 render 函数里，一次异常就让整页停止响应式更新。
	// 契约固定为数组，不把这个坑留给每一个消费方。
	if info.Releases == nil {
		info.Releases = []ReleaseBrief{}
	}
	return info
}

// allowedTags 是 tag 白名单，等于回退列表。
func (s *PanelVersionService) allowedTags() []ReleaseBrief {
	panelVersionCache.mu.RLock()
	defer panelVersionCache.mu.RUnlock()
	return panelVersionCache.info.Releases
}

const upgradeLogTailLines = 200

// 这三个变量供测试打桩。upgradeLogPathForTest 初值就是真实路径，
// 生产代码路径上它与常量 upgradeLogPath 等价。
var (
	upgradeLogPathForTest = upgradeLogPath
	// forceUpdatableForTest 为 true 时跳过环境前置检查。只有测试会设它，
	// 生产代码永远走 checkUpdatable。
	forceUpdatableForTest = false
	runUpgradeCommand     = func(argv []string) error {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			// systemd-run 的 stderr 是这里唯一有用的诊断（比如
			// "Unit a-ui-update.service already exists"），不能只透传 exit status。
			return common.NewErrorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
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
	// unit 名固定而不带时间戳：systemd 会拒绝创建同名 unit，这正是本功能唯一
	// 需要的并发互斥——超时文案让管理员以为可以重试时，第二次点击会被 systemd
	// 挡下，而不是两个 install.sh 并发 rm -rf + tar 同一个目录。
	// --collect 保证 unit 结束后名字自动释放，固定名不会长期占用。
	const unitName = "a-ui-update"
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
		return nil, common.NewError("读取更新日志失败:", err)
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
		return nil, common.NewError("读取更新日志失败:", err)
	}
	return ring, nil
}
