# 域名 + Caddy 伪装安装向导 设计文档

日期：2026-09-04
状态：已实现（Task 0–12 全部完成并真机验证，2026-09-04；端到端验收见 `.superpowers/sdd/2026-09-04-caddy-domain-bootstrap/task-11-report.md`）

## 1. 背景与目标

用户通过 `bash <(curl -Ls .../install.sh)` 一键安装。当前 `install.sh` 只做三件事：下载解压、设置账号密码、设置面板端口。装完之后的默认状态是：

- 面板监听所有 IP，**明文 HTTP**，`webBasePath` 为 `/`（即 `/xui/` 这个 x-ui 系标准指纹路径）
- 登录凭据在网络上明文传输。密码在库里也是明文存储，`web/controller/index.go` 登录失败还会打印明文用户名密码
- 有域名的管理员必须手动完成：申请证书 → 填面板证书路径 → 改 basePath → 给**每一个**入站填域名和证书路径
- 443 端口通常无人监听。一台有域名、有证书、443 却连不上的服务器，本身就是可观测的异常特征

其中**明文 HTTP 面板**是最严重的一条：面板密码泄露等于整机和全部用户配置一起丢，危害远大于单个节点被识别。

### 目标

1. 安装时询问域名，由 Caddy 自动申请证书并自动续期
2. Caddy 占 80/443，提供伪装站，443 上呈现为一个正常网站
3. 面板收编到 Caddy 之后：只监听 `127.0.0.1`，通过随机 `webBasePath` 反代访问，**面板不再占用任何对外端口**（管理员创建的入站端口不在此列，见「能力边界」）
4. 没有域名时走 REALITY 一键配置分支
5. 入站表单新建时自动带出域名与证书路径

### 非目标

以下均为独立的后续工作，本期不做：

- **入站收编到 Caddy 之后**（入站改明文 ws 监听 `127.0.0.1`，Caddy 按随机 path 分流，面板增删入站时自动生成并 reload Caddy 配置）。见 §12
- 多域名、泛域名、CDN 模式
- 面板界面里管理 Caddy 配置
- 卸载时连带卸载 Caddy（只询问，不默认执行）

### 明确的能力边界

本期**不提升节点自身的抗探测能力**。改造后 2886/2996 这类入站端口照样暴露在公网，用浏览器访问会得到 400 或断连——这个特征与改造前完全一致。443 上的伪装站保护不到它们。提升的是面板侧的暴露面与传输安全。这一点必须在安装完成的提示里如实告知用户，不得暗示节点也变安全了。

## 2. 外部依赖的实测结论

本设计的外部依赖原本都未实测。以下结论由 2026-09-04 在一台 Ubuntu 20.04.6 aarch64 的真实 VPS（东京机房）上逐项验证得出，**不是推断**：

| 验证项 | 结论 |
|---|---|
| Caddy 版本与安装方式 | **2.11.4**，官方 apt 源（cloudsmith）在 Ubuntu 20.04 aarch64 可用 |
| `events` / `cert_obtained` 钩子 | **不可用。**`caddy validate` 报 `getting module named 'events.handlers.exec': module not registered`——`events` 全局选项存在，但标准 Caddy 不带 `exec` 事件处理器，执行命令需要第三方插件。证书同步只能走 systemd timer（见 §8） |
| 最简 Caddyfile 下的 ACME 与跳转 | **开箱可用。**只写一个域名站点块，证书 8 秒内由 Let's Encrypt 生产 CA 签发；80 端口自动跳转 |
| 80→443 跳转的状态码 | **308** Permanent Redirect（不是 301），`Location` 不带端口 |
| xray 是否重读证书文件 | **会。**`transport/internet/tls/config.go:102-107` 起一个 goroutine 周期性重读 `certificateFile`/`keyFile`，间隔取 `ocspStapling`（默认 3600 秒）。**因此证书续期后不需要重启 xray**，最多一小时内自动生效 |
| Caddy 的证书存储路径 | `/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/<域名>/<域名>.crt` 与 `.key`，属主 `caddy:caddy`，权限 0600 |
| 伪装站候选（东京机房 IP 实测） | 可用：`dbku.tv` `wikipedia.org` `debian.org` `kernel.org` `nginx.org` `python.org` `apache.org` `bing.com` `apple.com` `microsoft.com` `amazon.co.jp` `nicovideo.jp` `lovelive-anime.jp`；拒绝：`gnu.org`（连不上）、`tesla.com`（403） |

两点需要留意：

- xray 那条热重载的判断条件是 `cert` **与** `key` **都**变化（`config.go:85` 用的是 `&&`）。正常续期两者都会换新，所以没问题；但若将来出现复用私钥只换证书的场景，热重载不会触发。
- `tesla.com` 作为**反代目标**被拒（403），但作为 **REALITY 的 `dest`** 完全可用——REALITY 是 TCP 透传，不发 HTTP 请求，两者判据不同。无域名分支沿用 `web/assets/js/model/xray.js:84` 的 `REALITY_TARGET_PRESETS`，该处注释写明"域名的 TLS 配置会变，隔一段时间要重测"，实现时按其要求复核四项判据。

仍未验证：Caddy 官方源在 CentOS 7 / Debian 8 / Ubuntu 16 等脚本声称支持的更低版本系统上是否可用——这决定二进制回退分支的必要性，在这些系统上跑安装脚本时验证。

## 3. 端口拓扑

### 有域名分支

```
:80                Caddy   ACME http-01 挑战 + 301 → https
:443               Caddy   TLS 终止
                             ├─ /<随机A>/*  → 127.0.0.1:54321   面板
                             └─ 其它        → 伪装站
127.0.0.1:54321    a-ui    明文 HTTP   webBasePath=/<随机A>/     外网不可达
:2886 :2996 …      xray    vmess+ws+tls，各自终止 TLS，共用同一份证书
```

对外开放：80、443，以及管理员自己创建的入站端口。

### 无域名分支

```
:443                    xray   VLESS+Vision+REALITY，dest 指向外部大站
:<随机高位端口>          a-ui   明文 HTTP，webBasePath=/<随机>/
```

不安装 Caddy。

## 4. 唯一的写库入口：`a-ui bootstrap`

安装脚本**不直接操作数据库**。所有配置写入通过一个新的 Go 子命令完成：

```
a-ui bootstrap
    -mode caddy|reality
    -domain example.com          mode=caddy 必填
    -basepath /Ab3xK9pQ/
    -listen 127.0.0.1
    -port 54321
    -cert-file /root/cert/fullchain.cer
    -key-file  /root/cert/example.com.key
    -reality-dest www.tesla.com:443    mode=reality 用
    -force                       覆盖已有配置
    -json                        机器可读输出
```

职责：写 settings（`webListen` / `webPort` / `webBasePath` / §7 的三个新增项）。`mode=caddy` **不创建任何入站**——节点由管理员在面板里按需创建；`mode=reality` 时额外调 `ServerService.GetNewX25519Cert()`（`web/service/server.go:307`，纯 Go 实现，不 exec xray 二进制）生成密钥对并建入站；输出面板 URL。

**不打印节点分享链接。**节点由管理员在面板里逐个创建，链接和二维码在面板上一直可复制。在 Go 侧重新实现一份 `genVLESSLink`（现仅存在于 `web/assets/js/model/xray.js:1206`）会形成两份必然漂移的实现——REALITY 新增参数时（如刚加的 `pqv`/ML-DSA-65）改了 JS 忘了改 Go，脚本印出的链接会静默缺参数。

### 为什么必须是 Go 子命令，而不是脚本写 SQLite

- 入站落库前要过 `ValidateInboundReplacing`（拿真实 xray 校验完整生成配置）。脚本写库会绕开这道防线——项目 CLAUDE.md 记载的"TLS 开了没填证书路径 → 整份配置加载失败 → 全员断网"事故正是它挡住的
- `settings` / `stream_settings` 的 JSON 结构由 `xray.js` 的模型定义，脚本手拼没有任何校验，写错只会在下次重启 xray 时静默失效
- schema 变更时脚本会静默写坏，Go 侧有 GORM 兜底
- 已有先例：`tc-clear`（`main.go:206`）、`v2-ui` 都是"不启面板、直接操作数据"的 CLI

### 代码位置

新建 `bootstrap/` 包，`main.go` 只做 flag 解析与调用。`tc-clear` 那种十几行的可以内联，bootstrap 要建入站、生成密钥、组装 JSON，内联会让 `main.go` 迅速膨胀。

### 幂等

`a-ui update` 会 `rm -rf /usr/local/a-ui/` 后重跑 `install.sh`。bootstrap 检测到面板已初始化（`webBasePath != "/"`，或已存在由 bootstrap 创建的入站）即原样退出并返回可识别的状态，除非显式 `-force`。`install.sh` 的向导同样先探测，已配置则整段跳过，不再重复提问。

### bootstrap 建的 REALITY 入站契约（仅 mode=reality）

```
protocol vless   port 443   listen 空
settings.clients[0] = { id: <uuid>, flow: "xtls-rprx-vision" }
settings.decryption = "none"
stream.network = "tcp"   stream.security = "reality"
reality = { dest, serverNames: [...], privateKey/publicKey, shortIds: [...],
            fingerprint: "chrome", show: false }
sniffing = { enabled: true, destOverride: ["http","tls","quic"] }
```

**这是整个改动最容易出错的地方。** Go 侧手写的 JSON 必须与 `xray.js` 的模型逐字段对齐：字段名差一个字母，xray 照样能跑，但用面板打开该入站时表单会错乱或吞掉值。特别注意 `xray.js:544`——`serverNames` 和 `shortIds` 在**前端模型里是逗号分隔字符串**，在数据库与 xray 配置里是**数组**，转换由 `toJson`/`fromJson` 负责；bootstrap 写的是数据库那份，必须是数组。

两道验证：走 `InboundService.AddInbound` 让真实 xray 校验完整配置，挡住结构错误；一个 golden 测试断言生成的 inbound JSON 与签入的期望文件逐字节一致，挡住"xray 认但前端模型不认"的字段差异——后者第一道防线看不见。

## 5. `a-ui setting` 新增两个 flag

```
a-ui setting -listen 127.0.0.1      写 webListen
a-ui setting -listen ""             清空 = 监听所有 IP（救援用）
a-ui setting -basepath /Ab3xK9pQ/   写 webBasePath，复用 entity.CheckValid 的首尾斜杠规范化
```

`flag` 包区分不了"没传 `-listen`"和"传了 `-listen ""`"，两者都是空字符串，而这两种语义**完全相反**：前者要保持原值，后者是救援时要清空监听地址。实现上用 `flag.Visit` 遍历实际出现过的 flag 来区分，不能靠零值判断。`updateSetting()` 的签名随之调整。

## 6. 安装脚本流程

```
install_base
install_a-ui                （不变）
config_after_install        （账号密码；去掉"面板访问端口"这一问，见下）
setup_wizard                （新增）
    │
    ├─ bootstrap 探测已配置 → 整段跳过
    │
    ├─「你有域名吗？」y ──▶ domain_flow
    │      1. 读取域名
    │      2. 解析校验：dig/getent 取 A 记录 ⟷ 本机公网 IP，不一致只警告不拦截
    │         （可能用了 CDN 或只有 AAAA 记录，硬拦会误伤）
    │      3. 检查 80/443 占用（ss -ltnp）；被 nginx/apache/caddy 占用时走
    │         「已有 web server 的处理」，未知进程则中止
    │      4. 安装 Caddy：官方源优先，失败回退 GitHub 二进制 + 自写 systemd 单元
    │      5. 选伪装站 → 预检 → 不通过则重选或回退本地静态页
    │      6. 生成 Caddyfile，启动 Caddy
    │      7. 轮询等待证书签发，最长 60s
    │      8. a-ui bootstrap -mode caddy …
    │      9. 打印面板 URL、账号密码、两条救援命令、节点端口仍暴露的告知
    │
    └─「你有域名吗？」n ──▶ reality_flow
           1. 选伪装目标（REALITY_TARGET_PRESETS 五个 + 自填）
           2. 预检四项判据
           3. a-ui bootstrap -mode reality …
           4. 打印
```

`config_after_install` 里"请设置面板访问端口"这一问要去掉：新拓扑下面板端口是内部实现细节，问用户只会造成困惑。有域名分支固定内部 `54321`，无域名分支沿用现有随机端口逻辑。

### 已有 web server 的处理

检测到 80/443 被占用时，不直接中止（那会让所有**已经用其它一键脚本搭过**的机器都用不上本方案——这类机器上面板往往正是明文 HTTP 暴露的状态，恰恰最需要这次改造）。

处理流程：

1. 用 `ss -ltnp` 识别占用者
2. 占用者是 nginx / apache / caddy 之一时，解析并列出它当前服务的站点（`server_name` 与 `root`），让用户看清会失去什么
3. 备份其配置目录到 `/root/<name>-backup-<时间戳>.tar.gz`
4. **停用而非卸载**：`systemctl stop` + `systemctl disable`，软件包与配置文件全部保留
5. 打印回滚命令，要求用户输入完整的 `yes`（不是 `y`）才继续
6. 占用者是未知进程时，中止并指出是谁占的，不做任何猜测性操作

停用而非卸载是刻意的：腾出 80/443 只需要停用，`apt remove` 除了不可逆之外没有任何额外收益。用户反悔时 `systemctl enable --now nginx` 一条命令即可恢复。

**已知风险（用户已知情并选择接受）**：nginx 里可能跑着与本面板无关的业务站点，停用后立即不可访问。脚本能做的只有"列出将失去什么 + 备份 + 二次确认 + 提供回滚"，无法自动迁移其配置到 Caddy——配置语义差异过大，自动转换出错的后果比不转换更严重。

### 伪装站预检

```
curl -sS -o /dev/null -m 10 -A "<常见浏览器 UA>" \
     -w '%{http_code} %{redirect_url}' https://候选站
```

判定不通过：状态码非 2xx；跳转到别的域名；响应头出现 `cf-mitigated`，或 `server: cloudflare` 且状态 403。

**不通过时不静默回退**，要说明原因并让用户重选。静默回退会让用户以为伪装成了某站，实际是一个静态页。

候选清单的具体站点**在实现阶段逐个实测后再定**。能否反代取决于 VPS 的机房 IP——同一个站，住宅 IP 正常、机房 IP 吃 403 或 Cloudflare 人机验证，非常常见。凭记忆列几个大站名字写进产品，等于把未验证的内容当成事实。

### 四个脚本成对维护

`install.sh` / `install_en.sh` 与 `a-ui.sh` / `a-ui_en.sh` 逻辑相同、文案不同。新增向导约二三百行，两份独立维护有漂移风险，但抽公共库会给 `bash <(curl -Ls …)` 这个分发方式增加一次网络依赖和一个失败点。**保持两份独立**（仓库既有约定），代价是每次改动必须同步两处。

`a-ui.sh` 菜单新增两项：「配置域名与伪装站」（事后跑同一个向导）、「卸载 Caddy 并恢复面板直连」。原有的「acme 申请 SSL」保留不动，避免破坏老用户习惯。

## 7. 入站表单自动填充

新增三个设置项，供入站表单新建时带出默认值：

```
defaultDomain     默认域名（TLS serverName / ws Host 头）
defaultCertFile   默认公钥文件路径
defaultKeyFile    默认密钥文件路径
```

按项目 CLAUDE.md 规定的五步走：`defaultValueMap` 加默认值 → `entity.AllSetting` 加字段 → `entity.CheckValid` 加校验 → 加 getter → **`web/assets/js/model/models.js` 的 `AllSetting` 构造函数里加同名字段**。反射只支持 `int` 和 `string`，这三个都是 string。

第五步不可省。漏掉的后果不是"新设置项不生效"：`ObjectUtil.cloneProps` 只克隆目标对象已有的 key，服务端返回值会被丢弃；提交时新字段在请求体里根本不存在，Gin 绑定成零值，若后端校验拒绝零值，**整个保存配置接口都会失败**，端口、证书路径、时区一起遭殃，而报错只指向新字段。

### CheckValid 里不要照抄 webCertFile 的校验

`entity.CheckValid` 现在对 `WebCertFile`/`WebKeyFile` 会调 `tls.LoadX509KeyPair` 实际加载。**这三个新字段不能这么做**：它们只是"新建入站时的默认填充值"，面板自己不使用；若证书尚未签发就填了路径，加载校验会失败，导致整个设置页保存不了。只做路径格式校验（非空时须以 `/` 开头）。

### 前端取值途径

`web/controller/util.go:73` 的 `html(c, name, title, data gin.H)` 第四个参数可注入模板变量。`XUIController.inbounds` 改为传入这三个值，`inbounds.html` 里渲染进 JS，`new Inbound()` 时读取。不新增接口。

## 8. 证书链路

```
Caddy 自动申请 / 续期
   └─▶ systemd timer（每小时）──▶ /root/cert/fullchain.cer
                                  /root/cert/<域名>.key    固定路径，不随 CA 目录名变化
                                        │
                           ┌────────────┴────────────┐
                  settings 里存一份                入站表单新建时带出
            （defaultDomain/CertFile/KeyFile）    域名、公钥路径、密钥路径
```

固定路径是必要的：Caddy 自己的证书存储路径含 ACME CA 的目录名（实测为 `.../certificates/acme-v02.api.letsencrypt.org-directory/<域名>/`），切换签发机构时会变，直接指过去会在某次续期后静默失效。

**同步机制用 systemd timer，不用 Caddy 事件钩子**——§2 实测确认标准 Caddy 不带 `events.handlers.exec` 模块，钩子方案根本无法通过 `caddy validate`。timer 每小时比对一次，内容有变才复制（`cmp` 比对不是多余的：没有它每小时都会重写文件）。

**证书续期后不需要重启 xray**：§2 实测确认 xray 自己会按 `ocspStapling` 间隔（默认 3600 秒）重读证书文件。所以同步脚本只复制文件，不碰 xray 进程。

这里有一条来自真实事故的教训。本设计验证期间，在那台测试用的生产机器上发现：nginx 正在使用一份**已过期 66 天**的证书，而 acme.sh 早在两个月前就成功续期了新证书——只是从未安装到 nginx 引用的路径，nginx 也从未 reload。后果是浏览器访问该域名直接报证书错误，伪装站完全失效，而真实用户因为客户端开了 `allowInsecure` 毫无察觉。**"证书续期成功"与"服务用上了新证书"是两件事**，本设计的 timer 必须验证的是后者。

## 9. 失败处理

**唯一原则：任何一步失败，都必须保证用户还能进面板。**

Caddy 装失败、证书 60 秒未签发、伪装站全部预检不过——一律**不修改 `webListen`**。面板保持监听所有 IP + 随机端口 + 随机 basePath，打印"域名配置未完成，面板仍可通过 `http://IP:端口/路径/` 访问，稍后可运行 `a-ui` 菜单的「配置域名与伪装站」重试"。把面板锁死在一个连不上的 `127.0.0.1` 上，比这个功能不存在糟糕得多。

因此 bootstrap 里**改 `webListen` 必须是最后一步**，排在 Caddy 已确认能正常反代之后。

安装完成的提示里必须打印两条救援路径：

```
a-ui setting -listen ""                        恢复监听所有 IP
ssh -L 54321:127.0.0.1:54321 root@<你的IP>     或走 SSH 隧道
```

### 防火墙

脚本不自动改防火墙。UFW/firewalld 的存在与规则差异过大，自动放行容易帮倒忙。检测到防火墙开启且 80/443 未放行时，**打印对应的放行命令**让用户自己执行。

## 10. 改动文件清单

| 文件 | 改动 |
|---|---|
| `install.sh` / `install_en.sh` | 新增 `setup_wizard`、`domain_flow`、`reality_flow`、Caddy 安装与配置生成、伪装站预检；`config_after_install` 去掉端口提问 |
| `a-ui.sh` / `a-ui_en.sh` | 菜单新增「配置域名与伪装站」「卸载 Caddy 并恢复面板直连」 |
| `main.go` | `setting` 子命令新增 `-listen`/`-basepath`（`flag.Visit` 区分未传与传空）；新增 `bootstrap` 子命令分派；`flag.Usage` 补充说明 |
| `bootstrap/`（新包） | bootstrap 全部逻辑 |
| `web/service/setting.go` | `defaultValueMap` 加三项；新增对应 getter 与 setter |
| `web/entity/entity.go` | `AllSetting` 加三个字段；`CheckValid` 加路径格式校验（**不做 LoadX509KeyPair**） |
| `web/controller/xui.go` | `inbounds` 渲染时注入三个默认值 |
| `web/html/xui/inbounds.html` | 接收模板变量并传给前端模型 |
| `web/assets/js/model/models.js` | `AllSetting` 构造函数加三个同名字段 |
| `web/assets/js/model/xray.js` | 新建入站时用注入的默认值填充域名与证书路径 |
| `CLAUDE.md` | 记录新子命令、新拓扑、Caddy 依赖、新增设置项 |

## 11. 测试策略

| 层次 | 手段 |
|---|---|
| bootstrap 生成的 inbound | golden 测试，逐字节比对签入的期望 JSON |
| `flag.Visit` 区分逻辑 | 单元测试，覆盖"不传"/"传空"/"传值"三种 |
| 新增设置项 | 现有 setting 测试补用例；确认零值不被 `CheckValid` 拒绝 |
| 模板 | `web/html_test.go` 的 `TestAllTemplatesParse` 与 `TestVueDirectivesLiveInsideAVueRoot` |
| 整体 | `make verify`（vet + test + build） |
| shell 脚本 | **无自动化手段。** 项目现无 shell 测试框架，本期也不引入。只做 `bash -n` 语法检查 |
| 端到端 | **必须在一台有域名的真实 VPS 上人工验证**：证书签发、伪装站、面板反代、随机 basePath、入站自动填充、续期钩子、失败回退路径 |

端到端这一条不可省略也无法替代。Caddy + ACME + 反代这条链路的绝大部分失败模式（DNS 未生效、机房 IP 被目标站拒绝、Caddy 版本差异、SELinux）在本地都复现不出来。**在真机验证通过之前，不得声称本功能可用。**

## 12. 风险

| 风险 | 缓解 |
|---|---|
| 面板被锁死在不可达的 `127.0.0.1` | §9 的失败处理原则；`webListen` 最后一步才改；打印两条救援命令 |
| `a-ui update` 重跑向导覆盖配置 | bootstrap 幂等 + 向导前置探测 |
| Go 侧手写 inbound JSON 与前端模型漂移 | golden 测试 + xray 完整配置校验 |
| 四个脚本漂移 | spec 明确约束；review 时逐条对照 |
| Caddy 版本差异导致 Caddyfile 语法不兼容 | §2 逐条真机验证；官方源安装以获得较新版本 |
| 证书续期后 xray 仍用旧证书 | §2 验证 `ocspStapling` 重读行为；必要时钩子内重启 xray |
| 用户误以为节点也变安全了 | §1 能力边界；安装完成提示里如实告知节点端口仍暴露 |
| `a-ui uninstall` 误删用户自用的 Caddy | 只询问，不默认执行；仅当检测到是本面板安装的才提议 |
| 停用 nginx 导致用户其它业务站点中断 | 列出将失去的站点 + 备份配置 + 要求输入完整 `yes` + 打印回滚命令；停用而非卸载，保证可逆 |

## 13. 下一期：入站收编到 Caddy 之后

本期完成并真机验证通过后，可再做入站收编。届时的形态：

```
入站改为 listen 127.0.0.1、明文 ws、安全层 none，用户之间以随机 path 区分
/etc/caddy/aui-inbounds.conf 由面板在增删入站时生成并 reload
    @ws1 { path /ws-<随机>  header Connection *Upgrade*  header Upgrade websocket }
    handle @ws1 { reverse_proxy 127.0.0.1:2886 }
```

收益：对外真正只剩 80/443；`header Upgrade websocket` 匹配条件使得即便 path 泄露，浏览器访问也只会看到伪装站，而不是 xray 直连时的 400/断连——这是 xray 直连**做不到**的能力；入站不再需要证书，`defaultCertFile`/`defaultKeyFile` 随之退役。

代价：所有现有用户的客户端配置必须重新分发（端口由各自端口改为 443，区分方式由端口改为 path）；面板与 Caddy 产生双向耦合；**Caddy 配置写坏会导致面板与全部节点一起挂**，必须先 `caddy validate` 通过才 reload、失败回滚到上一份并告警——思路与 `ValidateInboundReplacing` 一致，但是一整套新代码。

与本期方案互斥，不是叠加：本期入站自己终止 TLS 所以需要证书路径，下一期入站不终止 TLS 所以不需要。

## 14. 参考

- 本仓库 `web/service/server.go:307` `GetNewX25519Cert`
- 本仓库 `web/assets/js/model/xray.js:84` `REALITY_TARGET_PRESETS`（2026-09-03 实测）
- 本仓库 `web/assets/js/model/xray.js:1206` `genVLESSLink`
- 本仓库 `web/controller/util.go:73` `html()` 模板变量注入
- `docs/superpowers/specs/2026-09-03-inbound-anti-censorship-design.md`（REALITY / Vision / ECH 的既有设计）
