# 域名 + Caddy 伪装安装向导 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `install.sh` 一键完成「域名 → Caddy 自动证书 → 伪装站 → 面板收编到 443 之后」，并让入站表单自动带出域名与证书路径。

**Architecture:** Caddy 占 80/443 终止 TLS 并托管伪装站，面板只监听 `127.0.0.1` 由 Caddy 按随机 basePath 反代；安装脚本不碰数据库，所有配置写入经由新增的 `a-ui bootstrap` 子命令，入站创建走既有的 `InboundService.AddInbound`（保留其 xray 校验防线）。

**Tech Stack:** Go 1.27 + GORM/SQLite、Gin、Vue 2 + ant-design-vue（服务端模板，无打包工具）、Bash、Caddy 2、Xray-core 26.7.28

**Spec:** `docs/superpowers/specs/2026-09-04-caddy-domain-bootstrap-design.md`

## Global Constraints

- **CGO 必须开启**：`gorm.io/driver/sqlite` 依赖 `mattn/go-sqlite3`。构建命令 `CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o a-ui main.go`
- **提交前门禁**：`make verify`（= `go vet ./...` + `go test ./...` + build）
- **`web/service` 包的测试会 `os.Chdir` 到仓库根**（进程级副作用）。该包内新增测试不得依赖包内相对路径，要用 `t.TempDir()` 或绝对路径
- **改完 `web/html/**` 必须跑 `go test ./web/`**：`getHtmlTemplate` 吞掉 `ParseFS` 错误，`go build` 发现不了模板语法错误
- **新增设置项必须五步齐全**：`defaultValueMap` + `entity.AllSetting` + `entity.CheckValid` + getter + `web/assets/js/model/models.js` 的 `AllSetting` 构造函数。漏掉第五步会导致**整个保存配置接口失败**，不止是新项不生效
- **四个 shell 脚本成对维护**：`install.sh`/`install_en.sh`、`a-ui.sh`/`a-ui_en.sh`。逻辑相同、文案不同，每次改动同步两处
- **失败不锁面板**：任何一步失败都不得修改 `webListen`。面板保持监听所有 IP
- **不打印节点分享链接**：`genVLESSLink` 只存在于 `web/assets/js/model/xray.js:1206`，不在 Go 侧重复实现
- **仓库现有 spec 与代码注释使用简体中文**，新增注释与用户可见文案保持一致

---

## Task 0: 外部依赖真机验证 — ✅ 已完成（2026-09-04）

在一台 Ubuntu 20.04.6 aarch64 的真实 VPS（东京机房，域名已解析）上逐项验证完毕。
**完整结论见 spec §2**，此处只列直接改变后续任务写法的三条：

1. **`cert_obtained` 钩子不可用**——`caddy validate` 报 `getting module named 'events.handlers.exec': module not registered`。标准 Caddy 2.11.4 不带 exec 事件处理器。**Task 7 只走 systemd timer 方案，方案（a）已被证伪、从计划中删除。**
2. **80→443 是 308 不是 301**——Task 11 的验收清单按 308 核对。
3. **xray 会重读证书文件**（`transport/internet/tls/config.go:102-107`，间隔 = `ocspStapling`，默认 3600s）——证书同步脚本只复制文件，**不重启 xray**。

Caddy 版本 2.11.4，官方 apt 源在 Ubuntu 20.04 aarch64 可用；证书存储路径 `/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/<域名>/`，属主 `caddy:caddy` 权限 0600；伪装站候选清单已实测产出，见 Task 8 Step 3。

未验证项：Caddy 官方源在 CentOS 7 / Debian 8 / Ubuntu 16 上是否可用——在这些系统上跑安装脚本时验证，决定二进制回退分支的必要性。

---

## Task 1: `a-ui setting` 新增 `-listen` 与 `-basepath`

**Files:**
- Modify: `web/service/setting.go`（在 `GetListen` 之后加 `SetListen`；在 `GetBasePath` 之后加 `SetBasePath`）
- Modify: `main.go:90-119`（`updateSetting`）、`main.go:137-145`（flag 定义）、`main.go:186-196`（`setting` case）
- Test: `main_flags_test.go`（新建，package main）

**Interfaces:**
- Consumes: 无
- Produces:
  - `func (s *SettingService) SetListen(listen string) error`
  - `func (s *SettingService) SetBasePath(basePath string) error`
  - `func parseSettingFlags(args []string) (settingFlags, error)`，其中
    `type settingFlags struct { Reset bool; Port int; Username, Password string; Listen, BasePath *string }`
    ——指针为 nil 表示"该 flag 未出现"，非 nil 指向实际值（含空串）

- [ ] **Step 1: 写失败测试**

新建 `main_flags_test.go`：

```go
package main

import "testing"

func TestParseSettingFlagsDistinguishesUnsetFromEmpty(t *testing.T) {
	t.Run("未传 -listen 时为 nil", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-port", "8080"})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen != nil {
			t.Fatalf("未传 -listen，期望 nil，实际 %q", *f.Listen)
		}
		if f.Port != 8080 {
			t.Fatalf("port 期望 8080，实际 %d", f.Port)
		}
	})

	t.Run("传 -listen 空串时非 nil", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-listen", ""})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen == nil {
			t.Fatal("传了 -listen \"\"，期望非 nil（救援用：清空监听地址）")
		}
		if *f.Listen != "" {
			t.Fatalf("期望空串，实际 %q", *f.Listen)
		}
	})

	t.Run("传 -listen 有值", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-listen", "127.0.0.1", "-basepath", "/Ab3xK9pQ/"})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen == nil || *f.Listen != "127.0.0.1" {
			t.Fatalf("listen 期望 127.0.0.1，实际 %v", f.Listen)
		}
		if f.BasePath == nil || *f.BasePath != "/Ab3xK9pQ/" {
			t.Fatalf("basepath 期望 /Ab3xK9pQ/，实际 %v", f.BasePath)
		}
	})
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./ -run TestParseSettingFlags -v`
Expected: FAIL，`undefined: parseSettingFlags`

- [ ] **Step 3: 实现 `parseSettingFlags`**

在 `main.go` 中新增（放在 `updateSetting` 之前）：

```go
// settingFlags 是 `a-ui setting` 解析后的参数。
//
// Listen / BasePath 用指针：flag 包区分不了「没传 -listen」和「传了
// -listen ""」，两者都是空字符串，而这两种语义完全相反——前者要保持
// 原值不动，后者是面板被锁在 127.0.0.1 上时的救援入口（清空监听地址
// = 监听所有 IP）。靠 flag.Visit 遍历实际出现过的 flag 来区分。
type settingFlags struct {
	Reset    bool
	Port     int
	Username string
	Password string
	Listen   *string
	BasePath *string
}

func parseSettingFlags(args []string) (settingFlags, error) {
	cmd := flag.NewFlagSet("setting", flag.ContinueOnError)
	var f settingFlags
	var listen, basePath string
	cmd.BoolVar(&f.Reset, "reset", false, "reset all setting")
	cmd.IntVar(&f.Port, "port", 0, "set panel port")
	cmd.StringVar(&f.Username, "username", "", "set login username")
	cmd.StringVar(&f.Password, "password", "", "set login password")
	cmd.StringVar(&listen, "listen", "", "set panel listen ip, empty means all")
	cmd.StringVar(&basePath, "basepath", "", "set panel url base path")
	if err := cmd.Parse(args); err != nil {
		return f, err
	}
	cmd.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "listen":
			f.Listen = &listen
		case "basepath":
			f.BasePath = &basePath
		}
	})
	return f, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./ -run TestParseSettingFlags -v`
Expected: PASS（三个子测试全过）

- [ ] **Step 5: 加 SettingService 的两个 setter**

`web/service/setting.go`，紧跟在 `GetListen` 之后：

```go
func (s *SettingService) SetListen(listen string) error {
	return s.setString("webListen", listen)
}
```

紧跟在 `GetBasePath` 之后：

```go
// SetBasePath 写入面板 URL 根路径，按 entity.CheckValid 的规则补齐首尾斜杠。
func (s *SettingService) SetBasePath(basePath string) error {
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	return s.setString("webBasePath", basePath)
}
```

- [ ] **Step 6: 接进 `updateSetting` 与 `main` 的 setting 分支**

把 `main.go` 的 `updateSetting` 签名改为接收 `settingFlags`：

```go
func updateSetting(f settingFlags) {
	err := database.InitDB(config.GetDBPath())
	if err != nil {
		fmt.Println(err)
		return
	}

	settingService := service.SettingService{}

	if f.Port > 0 {
		err := settingService.SetPort(f.Port)
		if err != nil {
			fmt.Println("set port failed:", err)
		} else {
			fmt.Printf("set port %v success\n", f.Port)
		}
	}
	if f.Listen != nil {
		err := settingService.SetListen(*f.Listen)
		if err != nil {
			fmt.Println("set listen failed:", err)
		} else if *f.Listen == "" {
			fmt.Println("set listen to all interfaces success")
		} else {
			fmt.Printf("set listen %v success\n", *f.Listen)
		}
	}
	if f.BasePath != nil {
		err := settingService.SetBasePath(*f.BasePath)
		if err != nil {
			fmt.Println("set base path failed:", err)
		} else {
			fmt.Printf("set base path %v success\n", *f.BasePath)
		}
	}
	if f.Username != "" || f.Password != "" {
		userService := service.UserService{}
		err := userService.UpdateFirstUser(f.Username, f.Password)
		if err != nil {
			fmt.Println("set username and password failed:", err)
		} else {
			fmt.Println("set username and password success")
		}
	}
}
```

`main()` 里删掉原来的 `settingCmd` 一组 flag 定义，`case "setting"` 改为：

```go
	case "setting":
		f, err := parseSettingFlags(os.Args[2:])
		if err != nil {
			fmt.Println(err)
			return
		}
		if f.Reset {
			resetSetting()
		} else {
			updateSetting(f)
		}
```

`flag.Usage` 里 `setting` 那行补充说明：

```go
		fmt.Println("    setting        set settings（-port/-username/-password/-listen/-basepath/-reset）")
```

原来 `main()` 末尾 `default` 分支里的 `settingCmd.Usage()` 一并删掉（`settingCmd` 变量已不存在）。

- [ ] **Step 7: 全量验证**

Run: `make verify`
Expected: vet 无输出，测试全过，build 成功

- [ ] **Step 8: 提交**

```bash
git add main.go main_flags_test.go web/service/setting.go
git commit -m "feat(cli): a-ui setting 支持 -listen 与 -basepath"
```

---

## Task 2: 三个新增设置项（后端）

**Files:**
- Modify: `web/service/setting.go:25-41`（`defaultValueMap`）、getter 区
- Modify: `web/entity/entity.go:30-50`（`AllSetting`）、`entity.go:93-98`（`CheckValid`）
- Test: `web/service/setting_defaults_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 的 `setString`（已有）
- Produces:
  - `func (s *SettingService) GetDefaultDomain() (string, error)`
  - `func (s *SettingService) SetDefaultDomain(v string) error`
  - `func (s *SettingService) GetDefaultCertFile() (string, error)`
  - `func (s *SettingService) SetDefaultCertFile(v string) error`
  - `func (s *SettingService) GetDefaultKeyFile() (string, error)`
  - `func (s *SettingService) SetDefaultKeyFile(v string) error`
  - `entity.AllSetting` 新增字段 `DefaultDomain` / `DefaultCertFile` / `DefaultKeyFile`（均为 string）

- [ ] **Step 1: 写失败测试**

新建 `web/service/setting_defaults_test.go`：

```go
package service

import (
	"testing"

	"a-ui/web/entity"
)

func TestDefaultInboundSettingsRoundTrip(t *testing.T) {
	setupDB(t)
	s := SettingService{}

	if v, err := s.GetDefaultDomain(); err != nil || v != "" {
		t.Fatalf("默认值应为空串，得到 %q err=%v", v, err)
	}
	if err := s.SetDefaultDomain("example.com"); err != nil {
		t.Fatalf("SetDefaultDomain: %v", err)
	}
	if err := s.SetDefaultCertFile("/root/cert/fullchain.cer"); err != nil {
		t.Fatalf("SetDefaultCertFile: %v", err)
	}
	if err := s.SetDefaultKeyFile("/root/cert/example.com.key"); err != nil {
		t.Fatalf("SetDefaultKeyFile: %v", err)
	}

	if v, _ := s.GetDefaultDomain(); v != "example.com" {
		t.Fatalf("domain 期望 example.com，实际 %q", v)
	}
	if v, _ := s.GetDefaultCertFile(); v != "/root/cert/fullchain.cer" {
		t.Fatalf("certFile 实际 %q", v)
	}
	if v, _ := s.GetDefaultKeyFile(); v != "/root/cert/example.com.key" {
		t.Fatalf("keyFile 实际 %q", v)
	}
}

// 这三个字段只是「新建入站时的默认填充值」，面板自己不使用它们。
// 若照抄 WebCertFile 的 tls.LoadX509KeyPair 校验，证书尚未签发时保存
// 设置页会整个失败——包括端口、时区等无关字段。
func TestCheckValidDoesNotLoadDefaultCertPair(t *testing.T) {
	s := &entity.AllSetting{
		WebPort:                54321,
		WebBasePath:            "/",
		XrayTemplateConfig:     "{}",
		TimeLocation:           "Asia/Shanghai",
		SubscriptionUpdateTime: "04:00",
		AccessLogRetentionDays: 7,
		ConcurrencyIdleTimeout: 120,
		DefaultDomain:          "example.com",
		DefaultCertFile:        "/root/cert/does-not-exist.cer",
		DefaultKeyFile:         "/root/cert/does-not-exist.key",
	}
	if err := s.CheckValid(); err != nil {
		t.Fatalf("默认证书路径不存在时不应报错，实际: %v", err)
	}
}

func TestCheckValidRejectsRelativeDefaultCertPath(t *testing.T) {
	s := &entity.AllSetting{
		WebPort:                54321,
		WebBasePath:            "/",
		XrayTemplateConfig:     "{}",
		TimeLocation:           "Asia/Shanghai",
		SubscriptionUpdateTime: "04:00",
		AccessLogRetentionDays: 7,
		ConcurrencyIdleTimeout: 120,
		DefaultCertFile:        "relative/path.cer",
	}
	if err := s.CheckValid(); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./web/service/ -run "TestDefaultInboundSettingsRoundTrip|TestCheckValid.*Default" -v`
Expected: FAIL，`s.GetDefaultDomain undefined` 等

- [ ] **Step 3: `defaultValueMap` 加三项**

`web/service/setting.go` 的 `defaultValueMap` 里，`"tcInterface": ""` 之后加：

```go
	"defaultDomain":          "",
	"defaultCertFile":        "",
	"defaultKeyFile":         "",
```

- [ ] **Step 4: 加六个 getter/setter**

`web/service/setting.go`，放在 `GetKeyFile` 之后：

```go
// defaultDomain / defaultCertFile / defaultKeyFile 是**新建入站时**表单的
// 默认填充值，面板自身不使用（面板自己的证书是 webCertFile/webKeyFile）。
// 安装脚本配置好域名与 Caddy 之后由 a-ui bootstrap 写入，此后管理员每建
// 一个入站都不必再手填域名和证书路径——手填出错的代价是整份 xray 配置
// 加载失败，机器上全部用户一起断网。
func (s *SettingService) GetDefaultDomain() (string, error) {
	return s.getString("defaultDomain")
}

func (s *SettingService) SetDefaultDomain(v string) error {
	return s.setString("defaultDomain", v)
}

func (s *SettingService) GetDefaultCertFile() (string, error) {
	return s.getString("defaultCertFile")
}

func (s *SettingService) SetDefaultCertFile(v string) error {
	return s.setString("defaultCertFile", v)
}

func (s *SettingService) GetDefaultKeyFile() (string, error) {
	return s.getString("defaultKeyFile")
}

func (s *SettingService) SetDefaultKeyFile(v string) error {
	return s.setString("defaultKeyFile", v)
}
```

- [ ] **Step 5: `entity.AllSetting` 加字段并加校验**

`web/entity/entity.go` 的 `AllSetting` 里，`TcInterface` 之后加：

```go
	DefaultDomain   string `json:"defaultDomain" form:"defaultDomain"`
	DefaultCertFile string `json:"defaultCertFile" form:"defaultCertFile"`
	DefaultKeyFile  string `json:"defaultKeyFile" form:"defaultKeyFile"`
```

`CheckValid` 里，紧跟在 `WebCertFile`/`WebKeyFile` 那段之后加：

```go
	// 只校验路径格式，不做 tls.LoadX509KeyPair。这三个是「新建入站时的默认
	// 填充值」，面板自己不加载它们；证书尚未签发就填了路径是正常状态，
	// 在这里做加载校验会让整个设置页保存失败，连带端口、时区一起遭殃。
	for _, p := range []struct {
		name  string
		value string
	}{
		{"default cert file", s.DefaultCertFile},
		{"default key file", s.DefaultKeyFile},
	} {
		if p.value != "" && !strings.HasPrefix(p.value, "/") {
			return common.NewErrorf("%v must be an absolute path: %v", p.name, p.value)
		}
	}
```

- [ ] **Step 6: 运行确认通过**

Run: `go test ./web/service/ ./web/entity/ -run "Default" -v`
Expected: PASS

- [ ] **Step 7: 全量验证**

Run: `make verify`

- [ ] **Step 8: 提交**

```bash
git add web/service/setting.go web/service/setting_defaults_test.go web/entity/entity.go
git commit -m "feat(setting): 新增 defaultDomain/defaultCertFile/defaultKeyFile 三项"
```

---

## Task 3: 前端设置模型、设置页表单与入站自动填充

CLAUDE.md 规定的新增设置项五步里的第五步在这里完成。漏掉它的后果不是「新项不生效」：`ObjectUtil.cloneProps` 只克隆目标对象已有的 key，服务端返回值会被丢弃；提交时新字段在请求体里根本不存在，Gin 绑定成零值，**整个保存配置接口都会失败**。

**Files:**
- Modify: `web/assets/js/model/models.js:177-203`（`AllSetting` 构造函数）
- Modify: `web/html/xui/setting.html:83-87`（「xray 相关设置」tab）
- Modify: `web/controller/xui.go:39-41`（`inbounds` 方法）
- Modify: `web/html/xui/inbounds.html`（注入全局常量）
- Modify: `web/html/xui/inbound_modal.html:21-37`（`show()` 的新建分支）
- Test: `web/html_test.go` 既有用例覆盖

**Interfaces:**
- Consumes: Task 2 的 `GetDefaultDomain` / `GetDefaultCertFile` / `GetDefaultKeyFile`
- Produces: 页面全局常量 `PanelDefaults = { domain, certFile, keyFile }`（`inbounds.html` 中定义，`inbound_modal.html` 消费）

**只填 TLS serverName 与两个证书路径，不自动填 ws Host 头。** ws 的 Host 常常要填 CDN 域名而不是源站域名，自动填成源站域名是错的，宁可留空让管理员自己填。

- [ ] **Step 1: `models.js` 加三个同名字段**

`web/assets/js/model/models.js`，在 `this.tcInterface = "";` 之后：

```js
        this.defaultDomain = "";
        this.defaultCertFile = "";
        this.defaultKeyFile = "";
```

- [ ] **Step 2: 设置页加三个输入框**

`web/html/xui/setting.html` 的「xray 相关设置」tab，在 `xrayTemplateConfig` 那条之后：

```html
                                <setting-list-item type="text" title="新建入站默认域名" desc="新建入站时自动填入 TLS 域名，留空则不填充" v-model.trim="allSetting.defaultDomain"></setting-list-item>
                                <setting-list-item type="text" title="新建入站默认证书公钥路径" desc="填写一个 '/' 开头的绝对路径，新建入站时自动填入" v-model.trim="allSetting.defaultCertFile"></setting-list-item>
                                <setting-list-item type="text" title="新建入站默认证书密钥路径" desc="填写一个 '/' 开头的绝对路径，新建入站时自动填入" v-model.trim="allSetting.defaultKeyFile"></setting-list-item>
```

- [ ] **Step 3: 后端注入模板变量**

`web/controller/xui.go` 的 `inbounds` 方法改为：

```go
func (a *XUIController) inbounds(c *gin.Context) {
	// 新建入站时自动填充域名与证书路径，省掉逐个手填——手填错的代价是
	// 整份 xray 配置加载失败，机器上全部用户一起断网。
	// 取不到就当没有默认值，不影响页面渲染。
	settingService := service.SettingService{}
	domain, err := settingService.GetDefaultDomain()
	if err != nil {
		logger.Warning("get default domain failed:", err)
	}
	certFile, err := settingService.GetDefaultCertFile()
	if err != nil {
		logger.Warning("get default cert file failed:", err)
	}
	keyFile, err := settingService.GetDefaultKeyFile()
	if err != nil {
		logger.Warning("get default key file failed:", err)
	}
	html(c, "inbounds.html", "入站列表", gin.H{
		"default_domain":    domain,
		"default_cert_file": certFile,
		"default_key_file":  keyFile,
	})
}
```

若 `web/controller/xui.go` 尚未 import `a-ui/logger` 与 `a-ui/web/service`，补上。

- [ ] **Step 4: 页面里定义全局常量**

`web/html/xui/inbounds.html`，在 `{{template "inboundModal"}}` 这一组之前插入：

```html
<script>
    // 模板引擎是 html/template，script 上下文里的 {{ }} 会自动做 JS 字符串
    // 转义并补齐引号，所以这里不要自己加引号。
    const PanelDefaults = {
        domain: {{ .default_domain }},
        certFile: {{ .default_cert_file }},
        keyFile: {{ .default_key_file }},
    };
</script>
```

注意插入位置必须在引用 `PanelDefaults` 的 `inboundModal` 模板**之前**。

- [ ] **Step 5: 新建入站时应用默认值**

`web/html/xui/inbound_modal.html` 的 `show()` 里，把 `this.inbound = new Inbound();` 改为：

```js
            } else {
                this.inbound = new Inbound();
                this.applyPanelDefaults(this.inbound);
            }
```

并在 `inModal` 对象里新增方法（放在 `show` 之后）：

```js
        // 只对**新建**的入站生效：编辑既有入站时用的是它自己存下来的值，
        // 覆盖会静默改掉管理员之前的配置。
        applyPanelDefaults(inbound) {
            if (typeof PanelDefaults === 'undefined') {
                return;
            }
            if (PanelDefaults.domain) {
                inbound.stream.tls.server = PanelDefaults.domain;
            }
            const cert = inbound.stream.tls.certs[0];
            if (cert) {
                if (PanelDefaults.certFile) {
                    cert.certFile = PanelDefaults.certFile;
                }
                if (PanelDefaults.keyFile) {
                    cert.keyFile = PanelDefaults.keyFile;
                }
            }
        },
```

- [ ] **Step 6: 跑模板测试**

Run: `go test ./web/ -v -run "TestAllTemplatesParse|TestVueDirectivesLiveInsideAVueRoot"`
Expected: PASS。`getHtmlTemplate` 会吞掉 `ParseFS` 错误，`go build` 发现不了模板语法错误，这个测试是唯一的守卫。

- [ ] **Step 7: 人工验证（必做，自动化测试覆盖不到）**

```bash
XUI_DEBUG=true go run main.go
```
浏览器打开面板 →「面板设置 → xray 相关设置」填入三个值 → 保存配置（**确认保存成功，这一步验证的正是第五步没漏**）→ 回到入站列表 → 点「+」→ 安全层切到 tls → 确认域名与两个证书路径已自动填好 → 再打开一个既有入站，确认它的值**没有**被覆盖。

- [ ] **Step 8: 全量验证并提交**

```bash
make verify
git add web/assets/js/model/models.js web/html/xui/setting.html web/html/xui/inbounds.html web/html/xui/inbound_modal.html web/controller/xui.go
git commit -m "feat(inbound): 新建入站自动填充域名与证书路径"
```

---

## Task 4: `bootstrap` 包与 `mode=caddy`

**Files:**
- Create: `bootstrap/bootstrap.go`
- Create: `bootstrap/bootstrap_test.go`
- Modify: `main.go`（新增 `bootstrap` 子命令分派与 `flag.Usage` 说明）

**Interfaces:**
- Consumes: Task 1 的 `SetListen`/`SetBasePath`、Task 2 的三个 setter
- Produces:
  - `type Options struct { Mode, Domain, BasePath, Listen, CertFile, KeyFile, RealityDest string; Port int; Force, JSON, Check bool }`
  - `type Result struct { Mode string `json:"mode"`; PanelURL string `json:"panelUrl"`; Skipped bool `json:"skipped"`; Reason string `json:"reason,omitempty"` }`
  - `func Run(opts Options) (*Result, error)`
  - `func alreadyInitialized() (bool, error)`

`mode=caddy` **不创建任何入站**——节点由管理员在面板里按需创建。

- [ ] **Step 1: 写失败测试**

新建 `bootstrap/bootstrap_test.go`：

```go
package bootstrap

import (
	"testing"

	"a-ui/database"
	"a-ui/web/service"
)

func setupDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(t.TempDir() + "/test.db"); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func TestRunCaddyModeWritesSettings(t *testing.T) {
	setupDB(t)

	res, err := Run(Options{
		Mode:     "caddy",
		Domain:   "example.com",
		BasePath: "/Ab3xK9pQ/",
		Listen:   "127.0.0.1",
		Port:     54321,
		CertFile: "/root/cert/fullchain.cer",
		KeyFile:  "/root/cert/example.com.key",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("全新数据库不应被跳过")
	}
	if want := "https://example.com/Ab3xK9pQ/"; res.PanelURL != want {
		t.Fatalf("PanelURL 期望 %q，实际 %q", want, res.PanelURL)
	}

	s := service.SettingService{}
	if v, _ := s.GetListen(); v != "127.0.0.1" {
		t.Fatalf("webListen 实际 %q", v)
	}
	if v, _ := s.GetBasePath(); v != "/Ab3xK9pQ/" {
		t.Fatalf("webBasePath 实际 %q", v)
	}
	if v, _ := s.GetDefaultDomain(); v != "example.com" {
		t.Fatalf("defaultDomain 实际 %q", v)
	}
	if v, _ := s.GetDefaultCertFile(); v != "/root/cert/fullchain.cer" {
		t.Fatalf("defaultCertFile 实际 %q", v)
	}
}

// a-ui update 会 rm -rf /usr/local/a-ui 后重跑 install.sh。没有这道幂等
// 判断的话，每次面板更新都会把管理员改过的 basePath、监听地址覆盖回去。
func TestRunSkipsWhenAlreadyInitialized(t *testing.T) {
	setupDB(t)

	first := Options{Mode: "caddy", Domain: "example.com", BasePath: "/first/",
		Listen: "127.0.0.1", Port: 54321}
	if _, err := Run(first); err != nil {
		t.Fatalf("首次 Run: %v", err)
	}

	second := first
	second.BasePath = "/second/"
	res, err := Run(second)
	if err != nil {
		t.Fatalf("二次 Run: %v", err)
	}
	if !res.Skipped {
		t.Fatal("已初始化时应被跳过")
	}

	s := service.SettingService{}
	if v, _ := s.GetBasePath(); v != "/first/" {
		t.Fatalf("basePath 被覆盖了，实际 %q", v)
	}
}

func TestRunForceOverwrites(t *testing.T) {
	setupDB(t)

	first := Options{Mode: "caddy", Domain: "example.com", BasePath: "/first/",
		Listen: "127.0.0.1", Port: 54321}
	if _, err := Run(first); err != nil {
		t.Fatalf("首次 Run: %v", err)
	}

	second := first
	second.BasePath = "/second/"
	second.Force = true
	res, err := Run(second)
	if err != nil {
		t.Fatalf("force Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("-force 时不应跳过")
	}

	s := service.SettingService{}
	if v, _ := s.GetBasePath(); v != "/second/" {
		t.Fatalf("basePath 期望 /second/，实际 %q", v)
	}
}

// 安装脚本靠 -check 做幂等探测。它必须是只读的：若探测本身产生写入，
// 全新机器上第一次探测就会把探测参数变成实际配置。
func TestCheckIsReadOnly(t *testing.T) {
	setupDB(t)

	res, err := Run(Options{Check: true})
	if err != nil {
		t.Fatalf("Run(-check): %v", err)
	}
	if res.Skipped {
		t.Fatal("全新数据库应报告未初始化")
	}

	s := service.SettingService{}
	if v, _ := s.GetBasePath(); v != "/" {
		t.Fatalf("-check 不应写入任何设置，basePath 实际 %q", v)
	}

	if _, err := Run(Options{Mode: "caddy", Domain: "example.com",
		BasePath: "/Ab3xK9pQ/", Listen: "127.0.0.1", Port: 54321}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	res, err = Run(Options{Check: true})
	if err != nil {
		t.Fatalf("Run(-check) 二次: %v", err)
	}
	if !res.Skipped {
		t.Fatal("已初始化后应报告已初始化")
	}
}

func TestRunRejectsCaddyModeWithoutDomain(t *testing.T) {
	setupDB(t)
	if _, err := Run(Options{Mode: "caddy", BasePath: "/x/", Port: 54321}); err == nil {
		t.Fatal("mode=caddy 缺 -domain 应报错")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./bootstrap/ -v`
Expected: FAIL，包不存在

- [ ] **Step 3: 实现 `bootstrap/bootstrap.go`**

```go
// Package bootstrap 是安装脚本写入面板配置的唯一入口。
//
// 安装脚本不直接操作 SQLite：入站落库前要过 InboundService 的 xray 校验，
// settings/streamSettings 的 JSON 结构由前端模型定义，schema 也会随版本
// 变化——脚本手写这些只会在某次重启 xray 时静默失效。
package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"

	"a-ui/web/service"
)

type Options struct {
	Mode        string // caddy | reality
	Domain      string // mode=caddy 必填
	BasePath    string
	Listen      string
	Port        int
	CertFile    string
	KeyFile     string
	RealityDest string // mode=reality 必填
	Force       bool
	JSON        bool
	// Check 只查询是否已初始化，不做任何写入。安装脚本用它做幂等探测——
	// 拿一次真实的 Run 去「试探」会在全新机器上真的写进去，把探测用的
	// 假域名变成实际配置。
	Check bool
}

type Result struct {
	Mode     string `json:"mode"`
	PanelURL string `json:"panelUrl"`
	Skipped  bool   `json:"skipped"`
	Reason   string `json:"reason,omitempty"`
}

// alreadyInitialized 判断面板是否已被本命令配置过。
//
// 判据是 webBasePath 不再是默认的 "/"：随机根路径是本流程必定写入的一项，
// 而全新安装时它一定是 "/"。不拿 webListen 判断——它默认就是空串，
// 与「监听所有 IP」这个合法配置无法区分。
func alreadyInitialized() (bool, error) {
	s := service.SettingService{}
	basePath, err := s.GetBasePath()
	if err != nil {
		return false, err
	}
	return basePath != "/", nil
}

func Run(opts Options) (*Result, error) {
	// Check 分支必须排在 validate 之前：探测时不带 -mode/-domain。
	if opts.Check {
		done, err := alreadyInitialized()
		if err != nil {
			return nil, err
		}
		return &Result{Skipped: done}, nil
	}

	if err := validate(opts); err != nil {
		return nil, err
	}

	if !opts.Force {
		done, err := alreadyInitialized()
		if err != nil {
			return nil, err
		}
		if done {
			return &Result{
				Mode:    opts.Mode,
				Skipped: true,
				Reason:  "面板已配置过，保留现有设置（需要覆盖请加 -force）",
			}, nil
		}
	}

	s := service.SettingService{}

	if opts.Port > 0 {
		if err := s.SetPort(opts.Port); err != nil {
			return nil, fmt.Errorf("写入面板端口失败: %w", err)
		}
	}
	if err := s.SetBasePath(opts.BasePath); err != nil {
		return nil, fmt.Errorf("写入面板根路径失败: %w", err)
	}
	if opts.Domain != "" {
		if err := s.SetDefaultDomain(opts.Domain); err != nil {
			return nil, fmt.Errorf("写入默认域名失败: %w", err)
		}
	}
	if opts.CertFile != "" {
		if err := s.SetDefaultCertFile(opts.CertFile); err != nil {
			return nil, fmt.Errorf("写入默认证书路径失败: %w", err)
		}
	}
	if opts.KeyFile != "" {
		if err := s.SetDefaultKeyFile(opts.KeyFile); err != nil {
			return nil, fmt.Errorf("写入默认密钥路径失败: %w", err)
		}
	}

	// 监听地址放在最后写：改成 127.0.0.1 之后面板就只能经由 Caddy 访问，
	// 前面任何一步失败都必须保持原样，否则会把管理员锁在门外。
	if err := s.SetListen(opts.Listen); err != nil {
		return nil, fmt.Errorf("写入监听地址失败: %w", err)
	}

	return &Result{Mode: opts.Mode, PanelURL: panelURL(opts)}, nil
}

func validate(opts Options) error {
	switch opts.Mode {
	case "caddy":
		if opts.Domain == "" {
			return fmt.Errorf("mode=caddy 需要 -domain")
		}
	case "reality":
		if opts.RealityDest == "" {
			return fmt.Errorf("mode=reality 需要 -reality-dest")
		}
	default:
		return fmt.Errorf("未知的 -mode: %v（应为 caddy 或 reality）", opts.Mode)
	}
	if opts.BasePath == "" {
		return fmt.Errorf("需要 -basepath")
	}
	return nil
}

func panelURL(opts Options) string {
	if opts.Mode == "caddy" {
		return fmt.Sprintf("https://%v%v", opts.Domain, normalizedBasePath(opts.BasePath))
	}
	return fmt.Sprintf("http://<服务器IP>:%v%v", opts.Port, normalizedBasePath(opts.BasePath))
}

func normalizedBasePath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	if p[len(p)-1] != '/' {
		p += "/"
	}
	return p
}

// Print 把结果输出给安装脚本。-json 时输出机器可读格式供 jq 取值。
func (r *Result) Print(asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	if r.Skipped {
		fmt.Println("跳过:", r.Reason)
		return
	}
	fmt.Println("面板地址:", r.PanelURL)
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./bootstrap/ -v`
Expected: 四个测试全 PASS

- [ ] **Step 5: 接进 `main.go`**

在 `switch os.Args[1]` 里新增：

```go
	case "bootstrap":
		runBootstrap(os.Args[2:])
```

并新增函数：

```go
func runBootstrap(args []string) {
	cmd := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	var opts bootstrap.Options
	cmd.StringVar(&opts.Mode, "mode", "", "caddy | reality")
	cmd.StringVar(&opts.Domain, "domain", "", "域名（mode=caddy 必填）")
	cmd.StringVar(&opts.BasePath, "basepath", "", "面板 url 根路径")
	cmd.StringVar(&opts.Listen, "listen", "", "面板监听地址，留空为所有 IP")
	cmd.IntVar(&opts.Port, "port", 0, "面板监听端口")
	cmd.StringVar(&opts.CertFile, "cert-file", "", "新建入站默认证书公钥路径")
	cmd.StringVar(&opts.KeyFile, "key-file", "", "新建入站默认证书密钥路径")
	cmd.StringVar(&opts.RealityDest, "reality-dest", "", "REALITY 伪装目标（mode=reality 必填）")
	cmd.BoolVar(&opts.Force, "force", false, "覆盖已有配置")
	cmd.BoolVar(&opts.Check, "check", false, "只查询是否已初始化，不做任何写入")
	cmd.BoolVar(&opts.JSON, "json", false, "以 JSON 输出结果")
	if err := cmd.Parse(args); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if err := database.InitDB(config.GetDBPath()); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	res, err := bootstrap.Run(opts)
	if err != nil {
		fmt.Println("bootstrap 失败:", err)
		os.Exit(1)
	}
	res.Print(opts.JSON)
}
```

import 加 `"a-ui/bootstrap"`。`flag.Usage` 的 Commands 列表加一行：

```go
		fmt.Println("    bootstrap      安装脚本用：写入面板配置并按需创建入站")
```

- [ ] **Step 6: 手工验证 CLI**

```bash
CGO_ENABLED=1 go build -o /tmp/a-ui-test main.go
XUI_DEBUG=true /tmp/a-ui-test bootstrap -mode caddy -domain example.com \
    -basepath /Ab3xK9pQ/ -listen 127.0.0.1 -port 54321 -json
```
Expected: 输出含 `"panelUrl": "https://example.com/Ab3xK9pQ/"` 的 JSON。再跑一次，Expected: `"skipped": true`。

**验证完把测试库改回去**：这条命令会写真实数据库（`/etc/a-ui/a-ui.db` 或 debug 下的路径），验证后用 `a-ui setting -reset` 或删掉测试库，不要把开发机的面板配置留成这个状态。

- [ ] **Step 7: 提交**

```bash
make verify
git add bootstrap/ main.go
git commit -m "feat(cli): 新增 a-ui bootstrap 子命令（mode=caddy）"
```

---

## Task 5: `bootstrap` 的 `mode=reality`（建入站 + golden 契约测试）

无域名分支要建一个 VLESS+Vision+REALITY 入站。**Go 侧手写的 JSON 必须与 `web/assets/js/model/xray.js` 的模型逐字段一致**：字段名差一个字母 xray 照样能跑，但管理员在面板里打开该入站时表单会错乱或吞值。

注意两个易错点：REALITY 的伪装目标字段在本项目模型里叫 **`target`** 而不是 `dest`（`xray.js:558`）；`serverNames` 与 `shortIds` 在**前端模型里是逗号分隔字符串**，在数据库与 xray 配置里是**数组**（`xray.js:544` 与 `:622`），bootstrap 写的是数据库那份，必须是数组。

**Files:**
- Modify: `bootstrap/bootstrap.go`
- Create: `bootstrap/reality.go`
- Create: `bootstrap/reality_test.go`
- Create: `bootstrap/testdata/reality_inbound.golden.json`

**Interfaces:**
- Consumes: `service.ServerService.GetNewX25519Cert() (map[string]any, error)`、`service.InboundService.AddInbound(*model.Inbound) error`
- Produces:
  - `type RealityParams struct { Port int; UUID, PrivateKey, PublicKey, ShortID, Target, ServerName, Remark string }`
  - `func BuildRealityInbound(p RealityParams) (*model.Inbound, error)`

密钥、UUID、shortId 由 `Run` 生成后**作为参数传入** `BuildRealityInbound`，不在其内部生成——否则输出不确定，没法做 golden 比对。

- [ ] **Step 1: 用面板生成 golden 文件（不要手写它）**

golden 文件必须来自前端真实生成的结果，手写等于用一份猜测去校验另一份猜测。

```bash
XUI_DEBUG=true go run main.go
```

浏览器建一个入站：协议 vless、端口 `443`、监听留空、流控 `xtls-rprx-vision`、传输 `tcp`、安全层 `reality`、目标 `www.tesla.com:443`、serverNames `www.tesla.com`、指纹 `chrome`、shortIds 填 `0123456789abcdef`、sniffing 保持默认（enabled + http/tls/quic）。保存后导出：

```bash
sqlite3 /etc/a-ui/a-ui.db \
  "SELECT json_object('settings', settings, 'streamSettings', stream_settings, 'sniffing', sniffing) FROM inbounds WHERE port=443;" \
  | python3 -m json.tool > bootstrap/testdata/reality_inbound.golden.json
```

打开该文件，把 `privateKey`、`publicKey`、客户端 `id` 三个随机值替换成测试里要用的固定值：

```
privateKey  →  aGVsbG8td29ybGQtdGVzdC1wcml2YXRlLWtleTEyMw
publicKey   →  aGVsbG8td29ybGQtdGVzdC1wdWJsaWMta2V5MTIzNDU
id          →  11111111-2222-3333-4444-555555555555
```

- [ ] **Step 2: 写失败测试**

新建 `bootstrap/reality_test.go`：

```go
package bootstrap

import (
	"encoding/json"
	"os"
	"testing"
)

// golden 文件来自面板前端真实生成的入站（见计划 Task 5 Step 1）。
// 这个测试锁的是「Go 侧生成的 JSON 与前端模型逐字段一致」——xray 的配置
// 校验发现不了这类差异：字段名写错核心照样能跑，只有管理员在面板里打开
// 这个入站时才会看到表单错乱或值被吞掉。
func TestBuildRealityInboundMatchesFrontendModel(t *testing.T) {
	got, err := BuildRealityInbound(RealityParams{
		Port:       443,
		UUID:       "11111111-2222-3333-4444-555555555555",
		PrivateKey: "aGVsbG8td29ybGQtdGVzdC1wcml2YXRlLWtleTEyMw",
		PublicKey:  "aGVsbG8td29ybGQtdGVzdC1wdWJsaWMta2V5MTIzNDU",
		ShortID:    "0123456789abcdef",
		Target:     "www.tesla.com:443",
		ServerName: "www.tesla.com",
		Remark:     "REALITY",
	})
	if err != nil {
		t.Fatalf("BuildRealityInbound: %v", err)
	}

	raw, err := os.ReadFile("testdata/reality_inbound.golden.json")
	if err != nil {
		t.Fatalf("读 golden: %v", err)
	}
	var want struct {
		Settings       json.RawMessage `json:"settings"`
		StreamSettings json.RawMessage `json:"streamSettings"`
		Sniffing       json.RawMessage `json:"sniffing"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("解析 golden: %v", err)
	}

	for _, c := range []struct {
		name string
		got  string
		want json.RawMessage
	}{
		{"settings", got.Settings, want.Settings},
		{"streamSettings", got.StreamSettings, want.StreamSettings},
		{"sniffing", got.Sniffing, want.Sniffing},
	} {
		var g, w any
		if err := json.Unmarshal([]byte(c.got), &g); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", c.name, err)
		}
		if err := json.Unmarshal(c.want, &w); err != nil {
			t.Fatalf("golden 的 %s 不是合法 JSON: %v", c.name, err)
		}
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		if string(gb) != string(wb) {
			t.Errorf("%s 与前端模型不一致\n实际: %s\n期望: %s", c.name, gb, wb)
		}
	}

	if got.Port != 443 {
		t.Errorf("port 期望 443，实际 %d", got.Port)
	}
	if !got.Enable {
		t.Error("新建入站应为启用状态")
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./bootstrap/ -run TestBuildRealityInbound -v`
Expected: FAIL，`undefined: BuildRealityInbound`

- [ ] **Step 4: 实现 `bootstrap/reality.go`**

```go
package bootstrap

import (
	"encoding/json"
	"fmt"

	"a-ui/database/model"
)

// RealityParams 的密钥、UUID、shortId 由调用方生成后传入，不在这里生成：
// 内部生成会让输出不确定，无法用 golden 文件锁住「与前端模型一致」这件事。
type RealityParams struct {
	Port       int
	UUID       string
	PrivateKey string
	PublicKey  string
	ShortID    string
	Target     string // 形如 www.tesla.com:443
	ServerName string
	Remark     string
}

// BuildRealityInbound 组装一个 VLESS + Vision + REALITY 入站。
//
// 字段名以 web/assets/js/model/xray.js 的模型为准，不是以 xray-core 的
// 配置文档为准——两者大部分重合，但本项目的模型有自己的约定：
// REALITY 的伪装目标叫 target（xray.js:558），serverNames/shortIds 在
// 数据库里是数组而在表单模型里是逗号分隔串（xray.js:544）。
func BuildRealityInbound(p RealityParams) (*model.Inbound, error) {
	settings := map[string]any{
		"clients": []map[string]any{{
			"id":   p.UUID,
			"flow": "xtls-rprx-vision",
		}},
		"decryption": "none",
		"fallbacks":  []any{},
	}
	stream := map[string]any{
		"network":  "tcp",
		"security": "reality",
		"realitySettings": map[string]any{
			"show":         false,
			"xver":         0,
			"target":       p.Target,
			"serverNames":  []string{p.ServerName},
			"privateKey":   p.PrivateKey,
			"mldsa65Seed":  "",
			"minClientVer": "",
			"maxClientVer": "",
			"maxTimeDiff":  0,
			"shortIds":     []string{p.ShortID},
			"settings": map[string]any{
				"publicKey":     p.PublicKey,
				"fingerprint":   "chrome",
				"serverName":    "",
				"spiderX":       "/",
				"mldsa65Verify": "",
			},
		},
		"tcpSettings": map[string]any{
			"acceptProxyProtocol": false,
			"header":              map[string]any{"type": "none"},
		},
	}
	sniffing := map[string]any{
		"enabled":      true,
		"destOverride": []string{"http", "tls", "quic"},
	}

	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("序列化 settings: %w", err)
	}
	streamJSON, err := json.Marshal(stream)
	if err != nil {
		return nil, fmt.Errorf("序列化 streamSettings: %w", err)
	}
	sniffingJSON, err := json.Marshal(sniffing)
	if err != nil {
		return nil, fmt.Errorf("序列化 sniffing: %w", err)
	}

	return &model.Inbound{
		Enable:         true,
		Remark:         p.Remark,
		Listen:         "",
		Port:           p.Port,
		Protocol:       model.VLESS,
		Settings:       string(settingsJSON),
		StreamSettings: string(streamJSON),
		Sniffing:       string(sniffingJSON),
		// Tag 由 InboundService.UpdateInbound 按端口生成；新增时这里给出
		// 同样形态的值，与面板里手工新建的入站保持一致。
		Tag: fmt.Sprintf("inbound-%v", p.Port),
	}, nil
}
```

**若测试失败，以 golden 文件为准修改这里的字段**，不要反过来改 golden——golden 是前端真实输出，它才是契约。特别检查 `tcpSettings` 与 `fallbacks` 这两处，前端在 `network='tcp'` 时会输出 `tcpSettings`（`xray.js:743`），其确切结构以 golden 为准。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./bootstrap/ -run TestBuildRealityInbound -v`
Expected: PASS

- [ ] **Step 6: 把 reality 分支接进 `Run`**

`bootstrap/bootstrap.go` 的 `Run` 中，在写完 settings、**写 Listen 之前**插入：

```go
	if opts.Mode == "reality" {
		if err := createRealityInbound(opts); err != nil {
			return nil, err
		}
	}
```

并新增：

```go
// createRealityInbound 走 InboundService.AddInbound 而不是直接写库：
// 它内部会用真实 xray 校验完整生成配置，这道防线挡住的正是「配置非法
// 导致整份 bin/config.json 加载失败、机器上全部用户一起断网」。
func createRealityInbound(opts Options) error {
	serverName, _, ok := strings.Cut(opts.RealityDest, ":")
	if !ok || serverName == "" {
		return fmt.Errorf("-reality-dest 应形如 www.example.com:443，实际 %q", opts.RealityDest)
	}

	serverService := service.ServerService{}
	keys, err := serverService.GetNewX25519Cert()
	if err != nil {
		return fmt.Errorf("生成 REALITY 密钥失败: %w", err)
	}
	privateKey, _ := keys["privateKey"].(string)
	publicKey, _ := keys["publicKey"].(string)
	if privateKey == "" || publicKey == "" {
		return fmt.Errorf("生成 REALITY 密钥失败: 返回了空值")
	}

	shortID, err := randomHex(8)
	if err != nil {
		return fmt.Errorf("生成 shortId 失败: %w", err)
	}

	inbound, err := BuildRealityInbound(RealityParams{
		Port:       443,
		UUID:       uuid.New().String(),
		PrivateKey: privateKey,
		PublicKey:  publicKey,
		ShortID:    shortID,
		Target:     opts.RealityDest,
		ServerName: serverName,
		Remark:     "REALITY-" + serverName,
	})
	if err != nil {
		return err
	}

	inboundService := service.InboundService{}
	if err := inboundService.AddInbound(inbound); err != nil {
		return fmt.Errorf("创建 REALITY 入站失败: %w", err)
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
```

import 补 `"crypto/rand"`、`"encoding/hex"`、`"strings"`、`"github.com/google/uuid"`、`"a-ui/web/service"`。若 `github.com/google/uuid` 不在 `go.mod` 中，改用项目已有的 UUID 生成方式——先 `grep -rn "uuid" go.mod database/ web/service/` 确认，**不要为此新增依赖**。

- [ ] **Step 7: 加 reality 模式的集成测试**

在 `bootstrap/bootstrap_test.go` 追加：

```go
func TestRunRealityModeCreatesInbound(t *testing.T) {
	setupDB(t)

	res, err := Run(Options{
		Mode:        "reality",
		BasePath:    "/Zz9Yy8/",
		Port:        45678,
		RealityDest: "www.tesla.com:443",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped {
		t.Fatal("全新数据库不应被跳过")
	}

	inboundService := service.InboundService{}
	inbounds, err := inboundService.GetAllInbounds()
	if err != nil {
		t.Fatalf("GetAllInbounds: %v", err)
	}
	if len(inbounds) != 1 {
		t.Fatalf("期望 1 个入站，实际 %d", len(inbounds))
	}
	if inbounds[0].Port != 443 {
		t.Fatalf("入站端口期望 443，实际 %d", inbounds[0].Port)
	}
}
```

若 `InboundService` 没有 `GetAllInbounds`，先 `grep -n "func (s \*InboundService) Get" web/service/inbound.go` 找到实际的列表方法名并替换。此测试会触发 `AddInbound` 内的 xray 校验，若本机 `bin/xray-<GOOS>-<GOARCH>` 不存在，校验按项目既定的 fail open 策略放行，测试仍应通过。

- [ ] **Step 8: 全量验证并提交**

```bash
make verify
git add bootstrap/
git commit -m "feat(cli): bootstrap 支持 mode=reality，含前后端模型契约测试"
```

---

## Task 6: `install.sh` 向导骨架与无域名（REALITY）分支

本任务完成后，无域名分支即可端到端跑通，是第一个可独立交付验证的成果。

**Files:**
- Modify: `install.sh`（`config_after_install` 去掉端口提问；新增向导函数；`install_a-ui` 末尾调用向导）

**Interfaces:**
- Consumes: Task 4/5 的 `a-ui bootstrap`
- Produces: shell 函数 `gen_random_path`、`port_user`、`setup_wizard`、`reality_flow`、`print_result`

- [ ] **Step 1: 去掉端口提问**

新拓扑下面板端口是内部实现细节，问用户只会造成困惑。`config_after_install` 中删掉这三行：

```bash
        read -p "请设置面板访问端口:" config_port
        echo -e "${yellow}您的面板访问端口将设定为:${config_port}${plain}"
```
以及
```bash
        /usr/local/a-ui/a-ui setting -port ${config_port}
        echo -e "${yellow}面板端口设定完成${plain}"
```

「已取消设定」那个分支里的随机端口逻辑**保留**：无域名分支仍然用随机高位端口。

- [ ] **Step 2: 加基础工具函数**

在 `install_a-ui()` 之前插入：

```bash
# 生成面板 url 根路径用的随机串。防的是全网扫描器按 /xui/ 这个 x-ui 系
# 默认路径批量定位面板——这是本次改造要解决的核心问题之一。
gen_random_path() {
    LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 12
}

# 返回占用指定端口的进程名，空表示没人占用。
port_user() {
    ss -ltnH "sport = :$1" 2>/dev/null | awk '{print $NF}' | head -1
}

# 本机公网 IP。取不到返回空串，调用方必须容忍这种情况——机器可能在
# NAT 后面，或者到探测服务的出站被墙，都不该因此让安装失败。
public_ip() {
    curl -fsS -m 10 https://api.ipify.org 2>/dev/null || \
    curl -fsS -m 10 https://ifconfig.me 2>/dev/null || true
}
```

- [ ] **Step 3: 加向导入口与结果打印**

```bash
# 面板配置向导。任何一步失败都必须保证用户还能进面板：失败路径一律
# 不修改 webListen，面板保持监听所有 IP。把面板锁死在一个连不上的
# 127.0.0.1 上，比这个功能不存在糟糕得多。
setup_wizard() {
    if /usr/local/a-ui/a-ui bootstrap -check -json 2>/dev/null | grep -q '"skipped": true'; then
        echo -e "${yellow}检测到面板已配置过，保留现有设置，跳过配置向导${plain}"
        echo -e "${yellow}如需重新配置，安装完成后运行 a-ui 并选择「配置域名与伪装站」${plain}"
        return 0
    fi

    echo -e ""
    echo -e "${green}=== 面板安全配置向导 ===${plain}"
    echo -e "有域名的话，将自动申请证书、配置 Caddy 伪装站，并把面板隐藏在 443 后面。"
    echo -e "没有域名的话，将配置 VLESS+Vision+REALITY，借用大站证书伪装。"
    echo -e ""

    local has_domain
    read -p "你有已经解析到本机的域名吗？[y/n]: " has_domain
    if [[ x"${has_domain}" == x"y" || x"${has_domain}" == x"Y" ]]; then
        domain_flow
    else
        reality_flow
    fi
}
```

探测用的是 Task 4 的 `-check` 只读标志，不带 `-mode`/`-domain`。**不要改成拿一次真实的 `bootstrap` 调用去试探**：全新机器上那次调用不会被跳过，会真的把试探用的假域名和假路径写进配置。

- [ ] **Step 4: 实现 REALITY 分支**

```bash
# REALITY 伪装目标候选。四项判据（TLS1.3 / ALPN h2 / X25519 系密钥交换 /
# 证书有效）见 web/assets/js/model/xray.js:78 的注释，那里的列表是 2026-09-03
# 实测确认过的。域名的 TLS 配置会变，隔一段时间要重测。
REALITY_TARGETS=(
    "www.lovelive-anime.jp"
    "www.amazon.co.jp"
    "www.tesla.com"
    "www.cloudflare.com"
    "www.nicovideo.jp"
)

# 检查候选目标是否满足 REALITY 的要求。任一不满足就返回非 0。
check_reality_target() {
    local host="$1"
    local out
    out=$(timeout 15 openssl s_client -connect "${host}:443" -servername "${host}" \
            -alpn h2 -tls1_3 </dev/null 2>&1) || return 1
    echo "${out}" | grep -q "TLSv1.3" || return 1
    echo "${out}" | grep -q "ALPN protocol: h2" || return 1
    echo "${out}" | grep -qE "X25519" || return 1
    return 0
}

reality_flow() {
    echo -e ""
    echo -e "${green}请选择 REALITY 伪装目标：${plain}"
    local i=1
    for t in "${REALITY_TARGETS[@]}"; do
        echo "  ${i}) ${t}"
        i=$((i + 1))
    done
    echo "  ${i}) 自己填一个域名"

    local choice target
    read -p "请输入序号: " choice
    if [[ "${choice}" == "${i}" ]]; then
        read -p "请输入伪装目标域名（不带端口）: " target
    elif [[ "${choice}" =~ ^[0-9]+$ ]] && [[ "${choice}" -ge 1 ]] && [[ "${choice}" -lt "${i}" ]]; then
        target="${REALITY_TARGETS[$((choice - 1))]}"
    else
        echo -e "${red}无效的选择，跳过配置向导${plain}"
        return 1
    fi

    echo -e "正在检查 ${target} 是否满足 REALITY 要求（TLS1.3 / ALPN h2 / X25519）…"
    if ! check_reality_target "${target}"; then
        echo -e "${red}${target} 不满足要求，请换一个目标${plain}"
        echo -e "${yellow}跳过配置向导，面板保持默认配置，可稍后运行 a-ui 重新配置${plain}"
        return 1
    fi
    echo -e "${green}检查通过${plain}"

    local basepath
    basepath="/$(gen_random_path)/"
    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode reality \
            -reality-dest "${target}:443" -basepath "${basepath}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}配置失败：${out}${plain}"
        echo -e "${yellow}面板保持默认配置，仍可正常访问${plain}"
        return 1
    fi
    print_result "${out}" "reality"
}
```

- [ ] **Step 5: 实现结果打印**

```bash
print_result() {
    local json="$1"
    local mode="$2"
    local url
    url=$(echo "${json}" | jq -r '.panelUrl // empty' 2>/dev/null)

    echo -e ""
    echo -e "${green}=== 配置完成 ===${plain}"
    if [[ -n "${url}" ]]; then
        echo -e "${green}面板地址: ${url}${plain}"
    fi
    if [[ "${mode}" == "reality" ]]; then
        echo -e "${green}已创建 VLESS+Vision+REALITY 入站（443 端口），登录面板即可查看分享链接与二维码${plain}"
    fi
    echo -e ""
    echo -e "${yellow}如果面板打不开，用以下任一方式恢复：${plain}"
    echo -e "  a-ui setting -listen \"\"                       # 恢复监听所有 IP"
    echo -e "  ssh -L 54321:127.0.0.1:54321 root@<本机IP>     # 或走 SSH 隧道"
    echo -e ""
    echo -e "${yellow}注意：本次配置隐藏的是面板。你在面板里创建的入站端口仍然对外暴露，${plain}"
    echo -e "${yellow}其抗探测能力与配置前相同。${plain}"
}
```

最后一段告知是必须的：spec §1 明确要求不得暗示节点也变安全了。

- [ ] **Step 6: 在安装流程里调用**

`install_a-ui()` 中 `config_after_install` 之后、`systemctl daemon-reload` 之前插入：

```bash
    setup_wizard
```

注意 `setup_wizard` 需要面板二进制已就位（`/usr/local/a-ui/a-ui`），所以必须放在解压之后。向导返回非 0 不能中断安装——`set -e` 未启用，函数返回值不会导致退出，保持这个行为。

- [ ] **Step 7: 语法检查**

Run: `bash -n install.sh`
Expected: 无输出

- [ ] **Step 8: 真机验证（无域名分支）**

在一台干净的 VPS 上：

```bash
bash <(curl -Ls https://raw.githubusercontent.com/<你的分支>/install.sh)
# 向导选 n（无域名）→ 选一个 REALITY 目标
```
验证点：向导能正常交互；REALITY 预检能正确通过/拒绝；面板 URL 带随机路径且能打开；面板里能看到自动创建的 443 REALITY 入站；用客户端连该入站能正常上网；再跑一次安装脚本，向导应提示"已配置过"并跳过。

- [ ] **Step 9: 提交**

```bash
git add install.sh
git commit -m "feat(install): 新增面板安全配置向导与无域名 REALITY 分支"
```

---

## Task 7: `install.sh` 有域名分支（Caddy + 已有 web server 处理）

**Files:**
- Modify: `install.sh`

**Interfaces:**
- Consumes: Task 0 验证过的 Caddy 安装命令与 Caddyfile 语法；Task 6 的 `gen_random_path`/`port_user`/`public_ip`/`print_result`
- Produces: shell 函数 `handle_existing_web_server`、`install_caddy`、`write_caddyfile`、`wait_for_cert`、`domain_flow`

**本任务的所有 Caddy 相关命令与 Caddyfile 语法，必须使用 Task 0 中真机验证过的版本**，不得凭记忆或本计划中的示例直接照抄——示例只给结构。

- [ ] **Step 1: 已有 web server 的处理**

```bash
# 检测到 80/443 被占时不直接中止：已经用其它一键脚本搭过的机器上，面板
# 往往正是明文 HTTP 暴露的状态，恰恰最需要这次改造。
#
# 停用而不是卸载：腾出端口只需要停用，apt remove 除了不可逆之外没有任何
# 额外收益。用户反悔时一条 systemctl enable --now 就能恢复。
handle_existing_web_server() {
    local occupant="$1"
    local svc="" confdir=""
    case "${occupant}" in
        *nginx*)  svc="nginx";  confdir="/etc/nginx" ;;
        *apache*|*httpd*) svc="apache2"; confdir="/etc/apache2"
                  [[ -d /etc/httpd ]] && svc="httpd" && confdir="/etc/httpd" ;;
        *caddy*)  svc="caddy";  confdir="/etc/caddy" ;;
        *)
            echo -e "${red}80/443 被未知进程占用：${occupant}${plain}"
            echo -e "${red}请先自行处理后再运行本脚本${plain}"
            return 1 ;;
    esac

    echo -e ""
    echo -e "${yellow}检测到 ${svc} 正在占用 80/443，当前为以下站点服务：${plain}"
    if [[ "${svc}" == "nginx" ]]; then
        grep -rhE "^\s*(server_name|root)\s" "${confdir}" 2>/dev/null | sed 's/^/    /' || \
            echo "    （无法解析配置，请自行确认）"
    else
        echo "    （请自行确认 ${confdir} 下的站点配置）"
    fi

    local backup="/root/${svc}-backup-$(date +%Y%m%d-%H%M%S).tar.gz"
    tar czf "${backup}" "${confdir}" 2>/dev/null && \
        echo -e "${green}已备份配置到 ${backup}${plain}"

    echo -e ""
    echo -e "${red}继续将停用 ${svc}，上述站点会立即无法访问。${plain}"
    echo -e "${yellow}回滚命令：systemctl enable --now ${svc}${plain}"
    local confirm
    read -p "确认停用请输入完整的 yes（其它任何输入都会取消）: " confirm
    if [[ "${confirm}" != "yes" ]]; then
        echo -e "${yellow}已取消${plain}"
        return 1
    fi

    systemctl stop "${svc}" 2>/dev/null
    systemctl disable "${svc}" 2>/dev/null
    echo -e "${green}${svc} 已停用（软件包与配置文件均保留）${plain}"
    return 0
}
```

- [ ] **Step 2: Caddy 安装**

```bash
# apt 分支的命令已在 Ubuntu 20.04.6 aarch64 上实测通过（Caddy 2.11.4）。
# 脚本以 root 运行（开头有 EUID 检查），所以不加 sudo。
install_caddy() {
    if command -v caddy &>/dev/null; then
        echo -e "${green}检测到已安装 Caddy: $(caddy version | head -1)${plain}"
        return 0
    fi
    echo -e "正在安装 Caddy…"
    if [[ x"${release}" == x"centos" ]]; then
        dnf install -y "dnf-command(copr)" >/dev/null 2>&1 || yum install -y yum-plugin-copr >/dev/null 2>&1
        dnf copr enable -y @caddy/caddy >/dev/null 2>&1 || yum copr enable -y @caddy/caddy >/dev/null 2>&1
        dnf install -y caddy >/dev/null 2>&1 || yum install -y caddy >/dev/null 2>&1
    else
        apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl >/dev/null 2>&1
        curl -1sLf "https://dl.cloudsmith.io/public/caddy/stable/gpg.key" \
            | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>/dev/null
        curl -1sLf "https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt" \
            > /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq >/dev/null 2>&1
        apt-get install -y -qq caddy >/dev/null 2>&1
    fi
    if ! command -v caddy &>/dev/null; then
        echo -e "${red}Caddy 安装失败${plain}"
        echo -e "${yellow}请手动安装后重试：https://caddyserver.com/docs/install${plain}"
        return 1
    fi
    echo -e "${green}Caddy 安装完成: $(caddy version | head -1)${plain}"
    return 0
}
```

apt 分支实测通过。**CentOS 的 copr 分支未经验证**——在 CentOS 机器上首次运行时确认，失败则按错误提示补二进制回退分支（下载 GitHub release 的 `caddy_*_linux_arm64.tar.gz` 并自写 systemd 单元）。

- [ ] **Step 3: 加伪装站桩函数（Task 8 会替换成完整版）**

`domain_flow` 需要 `choose_mask_site`，它的完整实现（候选清单 + 可反代性预检）在 Task 8。本任务先放一个只用本地静态页的最小版本，好让有域名分支现在就能独立跑通并验证：

```bash
ensure_static_site() {
    mkdir -p /var/www/html
    [[ -f /var/www/html/index.html ]] && return 0
    cat > /var/www/html/index.html <<'HTMLEOF'
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Welcome</title></head>
<body><h1>Welcome</h1><p>This site is under construction.</p></body></html>
HTMLEOF
}

# 桩：Task 8 用完整实现（候选清单 + 预检 + 自填 URL）替换它。
# stdout 输出空串表示使用本地静态页。
choose_mask_site() {
    ensure_static_site
    echo ""
}
```

- [ ] **Step 4: 生成 Caddyfile**

```bash
# Caddyfile 结构见 spec §7。两处关键点：
#   1. 面板反代路径必须与写进面板的 webBasePath 完全一致，否则静态资源 404
#   2. 伪装站放在最后的 handle，作为兜底
# Caddy 直接占 80/443，证书与 80→443 跳转都靠它的自动 HTTPS，
# 不需要 https_port / bind——这一点由 Task 0 验证过。
write_caddyfile() {
    local domain="$1" basepath="$2" panel_port="$3" mask_url="$4"
    local mask_block

    if [[ -n "${mask_url}" ]]; then
        mask_block="        reverse_proxy ${mask_url} {
            header_up Host {upstream_hostport}
        }"
    else
        mask_block="        root * /var/www/html
        file_server"
    fi

    cat > /etc/caddy/Caddyfile <<EOF
${domain} {
    handle ${basepath}* {
        reverse_proxy 127.0.0.1:${panel_port}
    }

    handle {
${mask_block}
    }
}
EOF

    if ! caddy validate --config /etc/caddy/Caddyfile 2>&1; then
        echo -e "${red}生成的 Caddyfile 未通过校验${plain}"
        return 1
    fi
    systemctl restart caddy || return 1
    return 0
}
```

若 Task 0 的验证结论要求显式的 `http://` 站点块或 `bind` 指令，在此模板中补上。**先 `caddy validate` 再 restart 是硬要求**：配置写坏会让 443 整个挂掉，而面板此时已经藏在 Caddy 后面。

- [ ] **Step 5: 等待证书签发**

```bash
# Caddy 是异步申请证书的，启动成功不代表证书已就绪。
wait_for_cert() {
    local domain="$1"
    local i
    echo -e "正在等待证书签发（最长 60 秒）…"
    for i in $(seq 1 30); do
        if curl -fsS -m 5 --resolve "${domain}:443:127.0.0.1" \
                "https://${domain}/" -o /dev/null 2>/dev/null; then
            echo -e "${green}证书已就绪${plain}"
            return 0
        fi
        sleep 2
    done
    echo -e "${red}60 秒内未能通过 HTTPS 访问，证书可能尚未签发${plain}"
    echo -e "${yellow}排查：journalctl -u caddy -n 50${plain}"
    return 1
}
```

- [ ] **Step 6: 安装证书同步机制**

面板与入站用的是固定路径 `/root/cert/`，而 Caddy 自己的证书存储路径含 ACME CA 的目录名，切换签发机构时会变——直接指过去会在某次续期后静默失效。所以要把证书同步到固定路径。

**Task 0 已实测确认 Caddy 事件钩子不可用**（`events.handlers.exec` 模块未注册，`caddy validate` 直接报错），所以只有 systemd timer 一条路：

```bash
install_cert_sync() {
    local domain="$1"
    mkdir -p /root/cert

    cat > /usr/local/bin/a-ui-cert-sync <<'SYNCEOF'
#!/usr/bin/env bash
# 把 Caddy 管理的证书同步到固定路径。面板与各入站都读这两个文件——
# Caddy 自己的存储路径含 ACME CA 目录名，签发机构一换就变，不能直接引用。
set -euo pipefail
domain="$1"
src=$(find /var/lib/caddy -type f -name "${domain}.crt" 2>/dev/null | head -1)
key=$(find /var/lib/caddy -type f -name "${domain}.key" 2>/dev/null | head -1)
[[ -z "${src}" || -z "${key}" ]] && exit 0
changed=0
cmp -s "${src}" /root/cert/fullchain.cer || changed=1
cmp -s "${key}" "/root/cert/${domain}.key" || changed=1
[[ "${changed}" == "0" ]] && exit 0
install -m 644 "${src}" /root/cert/fullchain.cer
install -m 600 "${key}" "/root/cert/${domain}.key"
logger -t a-ui-cert-sync "证书已同步到 /root/cert"
SYNCEOF
    chmod +x /usr/local/bin/a-ui-cert-sync

    cat > /etc/systemd/system/a-ui-cert-sync.service <<EOF
[Unit]
Description=Sync Caddy certificates to /root/cert for a-ui

[Service]
Type=oneshot
ExecStart=/usr/local/bin/a-ui-cert-sync ${domain}
EOF

    cat > /etc/systemd/system/a-ui-cert-sync.timer <<'EOF'
[Unit]
Description=Sync Caddy certificates hourly

[Timer]
OnBootSec=2min
OnUnitActiveSec=1h

[Install]
WantedBy=timers.target
EOF

    systemctl daemon-reload
    systemctl enable --now a-ui-cert-sync.timer
    # 立刻跑一次，别等第一个周期
    systemctl start a-ui-cert-sync.service
}
```

同步脚本里的 `cmp` 比对不是多余的：没有它每小时都会重写一次文件，日志噪音之外，也让"证书到底什么时候换的"无从查证。

**脚本里不要重启 xray**——Task 0 已实测确认 xray 自己会按 `ocspStapling` 间隔（默认 3600 秒）重读 `certificateFile`/`keyFile`（`transport/internet/tls/config.go:102-107`），续期后最多一小时自动生效。

这个 timer 防的是一类真实事故：验证期间在测试机上发现，acme.sh 早在两个月前就成功续期了证书，但从未安装到 nginx 引用的路径，nginx 也从未 reload——nginx 就这样用着一份**过期 66 天**的证书对外服务，浏览器访问直接报证书错误、伪装站完全失效，而真实用户因为客户端开了 `allowInsecure` 毫无察觉。**「证书续期成功」和「服务用上了新证书」是两件事。**

`domain_flow` 对它的调用在下一步的代码里（`wait_for_cert` 成功之后、调 `bootstrap` 之前）。

- [ ] **Step 7: 组装有域名分支**

```bash
domain_flow() {
    local domain
    read -p "请输入你的域名: " domain
    if [[ -z "${domain}" ]]; then
        echo -e "${red}域名不能为空${plain}"
        return 1
    fi

    # 解析校验只警告不拦截：域名可能挂在 CDN 后面，或者只有 AAAA 记录，
    # 硬拦会误伤合法配置。
    local resolved myip
    resolved=$(getent ahostsv4 "${domain}" 2>/dev/null | awk '{print $1; exit}')
    myip=$(public_ip)
    if [[ -n "${resolved}" && -n "${myip}" && "${resolved}" != "${myip}" ]]; then
        echo -e "${yellow}警告：${domain} 解析到 ${resolved}，与本机公网 IP ${myip} 不一致${plain}"
        echo -e "${yellow}若使用了 CDN 可忽略；否则证书申请会失败${plain}"
        local go_on
        read -p "继续？[y/n]: " go_on
        [[ x"${go_on}" != x"y" && x"${go_on}" != x"Y" ]] && return 1
    fi

    local occupant
    occupant=$(port_user 80)
    [[ -z "${occupant}" ]] && occupant=$(port_user 443)
    if [[ -n "${occupant}" ]]; then
        handle_existing_web_server "${occupant}" || return 1
    fi

    install_caddy || return 1

    local mask_url
    mask_url=$(choose_mask_site) || return 1

    local basepath panel_port
    basepath="/$(gen_random_path)/"
    panel_port=54321

    write_caddyfile "${domain}" "${basepath}" "${panel_port}" "${mask_url}" || return 1
    wait_for_cert "${domain}" || {
        echo -e "${yellow}证书未就绪，为避免把你锁在面板外，不修改面板监听地址${plain}"
        echo -e "${yellow}修好之后运行 a-ui 选择「配置域名与伪装站」重试${plain}"
        return 1
    }

    install_cert_sync "${domain}"

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode caddy -domain "${domain}" \
            -basepath "${basepath}" -listen 127.0.0.1 -port "${panel_port}" \
            -cert-file /root/cert/fullchain.cer \
            -key-file "/root/cert/${domain}.key" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}写入面板配置失败：${out}${plain}"
        return 1
    fi
    systemctl restart a-ui
    print_result "${out}" "caddy"

    # 防火墙只提示不自动改：UFW/firewalld 的存在与规则差异过大，
    # 自动放行容易帮倒忙。
    if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        echo -e "${yellow}检测到 ufw 已启用，如未放行请执行: ufw allow 80,443/tcp${plain}"
    fi
    if command -v firewall-cmd &>/dev/null && firewall-cmd --state &>/dev/null; then
        echo -e "${yellow}检测到 firewalld 已启用，如未放行请执行:${plain}"
        echo -e "${yellow}  firewall-cmd --permanent --add-service={http,https} && firewall-cmd --reload${plain}"
    fi
}
```

证书路径 `/root/cert/fullchain.cer` 与 `/root/cert/<域名>.key` 由上一步的 systemd timer 从 Caddy 的存储目录同步而来。

- [ ] **Step 8: 语法检查与真机验证**

```bash
bash -n install.sh
```

真机验证（需要一台有域名的 VPS）：全新机器跑通有域名分支；已装 Nginx 的机器上验证停用分支（含备份文件生成、输入 `y` 应当取消、输入 `yes` 才继续）；故意填一个未解析的域名验证证书等待超时后**面板仍可通过原地址访问**。

- [ ] **Step 9: 提交**

```bash
git add install.sh
git commit -m "feat(install): 有域名分支——Caddy 伪装站与面板收编"
```

---

## Task 8: 伪装站选择与预检

Task 7 里 `choose_mask_site` 与 `ensure_static_site` 是只用本地静态页的桩，本任务用完整实现替换 `choose_mask_site`（`ensure_static_site` 沿用，只是这里给出带样式的完整页面）。

能否反代成功**取决于 VPS 的机房 IP**：同一个站，住宅 IP 访问正常、机房 IP 吃 403 或 Cloudflare 人机验证，非常常见。所以候选清单必须来自 Task 0 的真机实测，且安装时还要再预检一次。

**Files:**
- Modify: `install.sh`

**Interfaces:**
- Consumes: Task 0 产出的候选清单
- Produces: `choose_mask_site`（stdout 输出选定的 URL，空表示用本地静态页；返回非 0 表示用户放弃）、`check_mask_site`、`ensure_static_site`

- [ ] **Step 1: 预检函数**

```bash
# 判定不通过的三种情况：状态码非 2xx；跳转到别的域名；被 Cloudflare 拦截。
# 探测者看到一个坏掉的镜像站，比看到一个朴素的静态页可疑得多。
check_mask_site() {
    local url="$1"
    local ua="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    local resp code redirect headers

    resp=$(curl -sS -o /dev/null -m 10 -A "${ua}" \
                -w '%{http_code} %{redirect_url}' "${url}" 2>/dev/null) || {
        echo "无法连接"
        return 1
    }
    code=$(echo "${resp}" | awk '{print $1}')
    redirect=$(echo "${resp}" | awk '{print $2}')

    if [[ ! "${code}" =~ ^2 ]]; then
        echo "HTTP ${code}"
        return 1
    fi
    if [[ -n "${redirect}" ]]; then
        echo "跳转到 ${redirect}"
        return 1
    fi

    headers=$(curl -sSI -m 10 -A "${ua}" "${url}" 2>/dev/null)
    if echo "${headers}" | grep -qi "cf-mitigated"; then
        echo "被 Cloudflare 拦截"
        return 1
    fi
    return 0
}
```

- [ ] **Step 2: 本地静态页兜底**

```bash
ensure_static_site() {
    mkdir -p /var/www/html
    if [[ -f /var/www/html/index.html ]]; then
        return 0
    fi
    cat > /var/www/html/index.html <<'HTMLEOF'
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Welcome</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
       max-width: 42rem; margin: 6rem auto; padding: 0 1.5rem; line-height: 1.7;
       color: #24292f; }
h1 { font-size: 1.5rem; font-weight: 600; }
p { color: #57606a; }
</style>
</head>
<body>
<h1>Welcome</h1>
<p>This site is under construction.</p>
</body>
</html>
HTMLEOF
}
```

- [ ] **Step 3: 选择菜单**

`MASK_SITES` 的内容来自 Task 0 的真机实测（2026-09-04，东京机房 IP）：

```bash
# 候选来自真机实测（2026-09-04 于东京机房 IP）：状态码 2xx、无跳转、无 CF 拦截。
# 必须从机房 IP 测，住宅 IP 的结果不作数——同一个站，住宅 IP 正常、
# 机房 IP 吃 403 或人机验证非常常见。站点策略会变，隔一段时间要重测。
#
# 已实测拒绝、不要加回来的：gnu.org（连不上）、tesla.com（403）。
# 注意 tesla.com 只是不能作为**反代目标**；它作为 REALITY 的 dest 完全可用，
# 两者判据不同（REALITY 是 TCP 透传，不发 HTTP 请求）。
MASK_SITES=(
    "https://www.wikipedia.org"
    "https://www.bing.com"
    "https://www.microsoft.com"
    "https://www.apple.com"
    "https://www.amazon.co.jp"
    "https://www.nicovideo.jp"
    "https://www.python.org"
    "https://www.debian.org"
    "https://www.kernel.org"
    "https://nginx.org"
)

# stdout 输出选定的 URL；输出空串表示使用本地静态页；返回非 0 表示用户放弃。
choose_mask_site() {
    echo -e "" >&2
    echo -e "${green}请选择伪装站点：${plain}" >&2
    local i=1
    for s in "${MASK_SITES[@]}"; do
        echo "  ${i}) ${s}" >&2
        i=$((i + 1))
    done
    echo "  ${i}) 自己填一个网址" >&2
    echo "  0) 不反代，使用自带静态页" >&2

    local choice url reason
    while true; do
        read -p "请输入序号: " choice </dev/tty
        if [[ "${choice}" == "0" ]]; then
            ensure_static_site
            echo ""
            return 0
        elif [[ "${choice}" == "${i}" ]]; then
            read -p "请输入网址（含 https://）: " url </dev/tty
        elif [[ "${choice}" =~ ^[0-9]+$ ]] && [[ "${choice}" -ge 1 ]] && [[ "${choice}" -lt "${i}" ]]; then
            url="${MASK_SITES[$((choice - 1))]}"
        else
            echo -e "${red}无效的序号${plain}" >&2
            continue
        fi

        echo -e "正在从本机测试 ${url} 是否可反代…" >&2
        if reason=$(check_mask_site "${url}"); then
            echo -e "${green}可用${plain}" >&2
            echo "${url}"
            return 0
        fi
        # 不静默回退到静态页：那会让用户以为伪装成了某站，实际不是。
        echo -e "${red}${url} 不可用（${reason}），请另选${plain}" >&2
    done
}
```

`choose_mask_site` 的提示全部写到 stderr，只有选定的 URL 写到 stdout——因为调用方用 `$(...)` 取它的返回值。`read` 显式从 `/dev/tty` 读，避免在命令替换的子 shell 里读不到终端输入。

- [ ] **Step 4: 语法检查**

Run: `bash -n install.sh`

- [ ] **Step 5: 单独验证预检逻辑**

在目标 VPS 上直接测函数（不跑完整安装）：

```bash
source <(sed -n '/^check_mask_site()/,/^}/p' install.sh)
check_mask_site https://example.com && echo OK || echo "拒绝原因见上"
```
用 `https://www.tesla.com` 验证拒绝路径（Task 0 实测该站对机房 IP 返回 403），用 `https://www.wikipedia.org` 验证通过路径。

- [ ] **Step 6: 提交**

```bash
git add install.sh
git commit -m "feat(install): 伪装站选择与可反代性预检"
```

---

## Task 9: `install_en.sh` 同步

四个脚本成对维护是仓库既有约定。抽公共库会给 `bash <(curl -Ls …)` 这个分发方式增加一次网络依赖和一个失败点，所以保持两份独立，代价是每次改动同步两处。

**Files:**
- Modify: `install_en.sh`

**Interfaces:**
- Consumes: Task 6/7/8 在 `install.sh` 中的全部函数
- Produces: 同名同签名的英文版函数

- [ ] **Step 1: 逐函数移植**

把 Task 6、7、8 加入 `install.sh` 的全部函数原样移植到 `install_en.sh`，**只翻译用户可见文案，逻辑与函数名保持完全一致**。需要翻译的文案清单：

| 中文 | English |
|---|---|
| 面板安全配置向导 | Panel security setup |
| 你有已经解析到本机的域名吗？ | Do you have a domain pointing to this server? |
| 请输入你的域名 | Enter your domain |
| 请选择伪装站点 | Choose a decoy site |
| 自己填一个网址 | Enter a custom URL |
| 不反代，使用自带静态页 | No proxy, use the bundled static page |
| 正在从本机测试 … 是否可反代 | Testing whether … can be proxied |
| 不可用（…），请另选 | unavailable (…), pick another |
| 检测到 … 正在占用 80/443 | … is currently using ports 80/443 |
| 确认停用请输入完整的 yes | Type the full word yes to confirm |
| 已备份配置到 … | Config backed up to … |
| 回滚命令 | Rollback command |
| 正在等待证书签发（最长 60 秒） | Waiting for certificate issuance (up to 60s) |
| 如果面板打不开，用以下任一方式恢复 | If the panel is unreachable, recover with either |
| 注意：本次配置隐藏的是面板。你在面板里创建的入站端口仍然对外暴露 | Note: this setup hides the panel only. Inbound ports you create remain exposed |

- [ ] **Step 2: 逐行比对两份脚本的逻辑**

```bash
diff <(grep -vE '^\s*#|echo|LOGI|LOGE|LOGD|read -p' install.sh) \
     <(grep -vE '^\s*#|echo|LOGI|LOGE|LOGD|read -p' install_en.sh)
```
剥掉注释与文案后，两者的控制流应当**完全一致**。有差异就是漂移，必须消除。

- [ ] **Step 3: 语法检查并提交**

```bash
bash -n install_en.sh
git add install_en.sh
git commit -m "feat(install): install_en.sh 同步配置向导"
```

---

## Task 10: `a-ui.sh` / `a-ui_en.sh` 菜单新增两项

装完之后要能重新配置——尤其是首次安装时向导失败（证书没签发、伪装站都不可用）的情况，用户需要一个重试入口。

**Files:**
- Modify: `a-ui.sh`、`a-ui_en.sh`

**Interfaces:**
- Consumes: Task 6/7/8 的向导函数
- Produces: 菜单项「配置域名与伪装站」与「卸载 Caddy 并恢复面板直连」

- [ ] **Step 1: 加「配置域名与伪装站」**

向导逻辑不在 `a-ui.sh` 里重复实现——直接调用最新的 `install.sh`，避免两份漂移：

```bash
# 重新运行配置向导。不在这里重复实现向导逻辑，直接跑 install.sh 的
# 向导部分，避免与 install.sh 漂移。
reconfig_domain() {
    LOGI "将重新运行配置向导（会覆盖现有的面板路径与监听地址设置）"
    confirm "确认继续?" "n"
    [[ $? != 0 ]] && return
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh) --wizard-only
}
```

对应地，`install.sh` 末尾的入口要支持这个参数：

```bash
if [[ "$1" == "--wizard-only" ]]; then
    setup_wizard
    exit 0
fi

echo -e "${green}开始安装${plain}"
install_base
install_a-ui $1
```

注意 `--wizard-only` 会走到 `setup_wizard` 的幂等判断而被跳过。这里要传 `-force` 语义：在 `setup_wizard` 里加一个可选参数，`--wizard-only` 时跳过幂等检查，并把 `-force` 透传给 `a-ui bootstrap`。

- [ ] **Step 2: 加「卸载 Caddy 并恢复面板直连」**

```bash
# 恢复面板直连。用户可能拿 Caddy 跑别的站，所以只停用不卸载软件包，
# 且必须先把面板监听地址改回来——否则停掉 Caddy 就再也进不去面板了。
restore_direct_panel() {
    LOGI "将停用 Caddy 并把面板恢复为直连访问"
    confirm "确认继续?" "n"
    [[ $? != 0 ]] && return

    # 顺序很重要：先恢复监听地址，再停 Caddy。反过来会有一段时间两边都进不去。
    /usr/local/a-ui/a-ui setting -listen ""
    systemctl restart a-ui
    systemctl stop caddy 2>/dev/null
    systemctl disable caddy 2>/dev/null

    local port basepath
    port=$(/usr/local/a-ui/a-ui setting 2>/dev/null | grep -oE '[0-9]+' | head -1)
    LOGI "Caddy 已停用（软件包保留，systemctl enable --now caddy 可恢复）"
    LOGI "面板现可通过 http://<本机IP>:<面板端口><面板根路径> 访问"
    LOGI "面板端口与根路径可在面板设置页查看；若忘记，用菜单第 7 项查看面板信息"
}
```

**先恢复监听地址再停 Caddy** 这个顺序不能颠倒：反过来会出现一段时间两边都进不去面板的窗口。

- [ ] **Step 3: 挂进菜单**

在 `a-ui.sh` 的菜单显示与 `case` 分派里各加两项，编号接在现有最大项之后。同时更新脚本顶部的 `show_menu` 帮助文本。

- [ ] **Step 4: 同步 `a-ui_en.sh` 并做逻辑比对**

```bash
bash -n a-ui.sh && bash -n a-ui_en.sh
diff <(grep -vE '^\s*#|echo|LOGI|LOGE|LOGD|read -p' a-ui.sh) \
     <(grep -vE '^\s*#|echo|LOGI|LOGE|LOGD|read -p' a-ui_en.sh)
```

- [ ] **Step 5: 真机验证**

在已完成 Task 7 配置的机器上：跑「卸载 Caddy 并恢复面板直连」，确认面板能用 `http://IP:端口/路径/` 打开；再跑「配置域名与伪装站」，确认能重新配置回去。

- [ ] **Step 6: 提交**

```bash
git add a-ui.sh a-ui_en.sh install.sh
git commit -m "feat(script): 管理菜单新增域名配置与恢复面板直连"
```

---

## Task 11: 文档更新与端到端验收

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/superpowers/specs/2026-09-04-caddy-domain-bootstrap-design.md`（状态改为「已实现」）

**Interfaces:**
- Consumes: 前面全部任务
- Produces: 无代码产出

- [ ] **Step 1: 更新 `CLAUDE.md`**

在「常用命令」的子命令列表里加：

```
./a-ui setting -listen 127.0.0.1         # 改面板监听地址（-listen "" 恢复监听所有 IP）
./a-ui setting -basepath /xxx/           # 改面板 url 根路径
./a-ui bootstrap -mode caddy ...         # 安装脚本用：写入面板配置并按需建入站
```

在「设置系统」一节补充三个新设置项与「`CheckValid` 里刻意不对它们做 `LoadX509KeyPair`」的理由。

新增一节「安装向导与 Caddy 拓扑」，写明：端口拓扑、`bootstrap` 是脚本写库的唯一入口、失败不锁面板的原则、两条救援命令、以及**本改造不提升节点抗探测能力**这条边界。

在「运维脚本」一节补充 `install.sh` 现在会安装并接管 Caddy、以及停用已有 web server 的行为。

- [ ] **Step 2: 端到端验收清单（在真实 VPS 上逐条打勾）**

有域名分支：

- [ ] 全新机器安装，向导选 y，证书正常签发
- [ ] `https://<域名>/` 显示伪装站，证书由浏览器认可
- [ ] `https://<域名>/<随机路径>/` 能打开面板并登录
- [ ] `curl -I http://<域名>` 返回 **308** 到 `https://<域名>/`，Location 不带端口（Caddy 用 308 而非 301）
- [ ] `ss -ltn` 确认 54321 只监听在 `127.0.0.1`
- [ ] 从外网 `curl http://<IP>:54321/` 连不上
- [ ] 面板里新建一个入站，域名与两个证书路径已自动填好
- [ ] 该入站用 vmess+ws+tls 能正常连通
- [ ] 面板设置页保存配置成功（验证新增设置项的第五步没漏）
- [ ] `a-ui setting -listen ""` 后能用 `http://<IP>:54321/<随机路径>/` 进面板

已有 Nginx 的机器：

- [ ] 正确列出 nginx 当前服务的站点
- [ ] 生成了 `/root/nginx-backup-*.tar.gz`
- [ ] 输入 `y` 被拒绝，输入 `yes` 才继续
- [ ] 停用后 `systemctl enable --now nginx` 能完整恢复

失败路径：

- [ ] 填一个未解析的域名 → 证书等待超时 → **面板仍可通过原地址访问**
- [ ] 所有伪装站都预检失败 → 能选 0 用静态页，或放弃后面板不受影响
- [ ] 重复运行 `install.sh` → 向导提示已配置并跳过，现有设置未被覆盖

无域名分支：

- [ ] 向导选 n，REALITY 预检通过，443 入站自动创建
- [ ] 客户端用该入站能正常连通
- [ ] `curl -I https://<IP>` 返回的是伪装目标站的响应

- [ ] **Step 3: 更新 spec 状态并提交**

```bash
git add CLAUDE.md docs/superpowers/specs/2026-09-04-caddy-domain-bootstrap-design.md
git commit -m "docs: 记录安装向导与 Caddy 拓扑"
```
