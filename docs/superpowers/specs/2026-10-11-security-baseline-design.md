# 安全基线：密码哈希、会话、登录限速、CSRF、两步验证

日期：2026-10-11
状态：已实现
对应路线图：`2026-09-03-modernization-roadmap.md` 计划 02

## 1. 动机

面板经 Caddy 或直接暴露在公网上，而改动前：

| 问题 | 位置 |
|---|---|
| 密码明文存库 | `users.password` |
| 登录失败时把提交的用户名和密码写进日志 | `web/controller/index.go` |
| 会话 cookie 里带着整个 `User`（含明文密码），而 cookie 只签名不加密 | `web/session/session.go` + `cookie.NewStore(secret)` |
| cookie 没有 `HttpOnly` / `SameSite`，改密码后旧会话仍然有效 | 同上 |
| 登录没有任何限速 | — |
| 没有跨站请求防护 | — |
| 没有第二因素 | — |

第三条是读代码时才发现的，路线图里没有，但它决定了范围：只做哈希而不动会话，密码照样从 cookie 里漏出去。

## 2. 范围

做：bcrypt 哈希与存量迁移、会话内容与 cookie 属性、按来源 IP 的登录限速、CSRF 防护、TOTP 两步验证、相应的命令行救援入口与脚本文案。

不做：恢复码（SSH 就是恢复手段）、多用户与角色、登录审计页面、把 `/logout` 从 GET 改成 POST（被跨站触发的后果只是登出）、API Token 与订阅服务器（路线图计划 06，本次是它们的前置）。

## 3. 设计

### 3.1 密码哈希（`util/crypto`）

- bcrypt，默认 cost。`golang.org/x/crypto` 已在 `go.mod` 里，不新增依赖。
- 写入口三个，全部经 `HashPassword`：`UserService.UpdateUser`、`UpdateFirstUser`、`database.initUser`。
- 存量迁移 `UserService.MigratePlaintextPasswords` 在 `runWebServer` 里、`InitDB` 之后调用。不放进 `InitDB`：只读子命令 `setting -show` 也会调它。启动时全量迁移而不是登录时惰性升级，避免长期不登录的账号一直是明文。更新带 `password = 旧值` 条件，不覆盖启动期间经命令行改过的行。
- 判定「是不是哈希」用 `bcrypt.Cost` 能否解析，不用前缀匹配。
- bcrypt 上限 72 字节：新密码超长直接拒绝；存量超长密码哈希不了，原样保留并告警，`CheckPassword` 对非哈希值按明文做常数时间比较——宁可留一行没保护的数据，也不把管理员锁在门外。
- 用户名不存在时仍跑一次等价耗时的 bcrypt（`BurnCheck`），不让响应时间泄露用户名是否存在。
- 登录失败日志只记用户名与来源 IP。
- `a-ui setting -show` 不再输出密码，改为给出重设命令；`Port:` / `BasePath:` 等行格式不变（`a-ui.sh` 在解析它们）。

### 3.2 会话（`web/session`）

- 会话里只存 `id`、`用户名`、`指纹 = sha256(用户名 ‖ 库里的密码值)` 的前 16 字节。
- 每个请求回库核对指纹，结果在 `gin.Context` 上缓存到请求结束。用户名或密码变更后所有旧会话立即失效；发起修改的那个会话由 `updateUser` 重新签发。
- cookie：`HttpOnly`、`SameSite=Lax`、有效期保持 30 天；`Secure` 在请求为 HTTPS 时加上——直连 TLS，或对端是本机回环且 `X-Forwarded-Proto: https`。
- 升级前签发的旧格式 cookie 解不出这三个 key，按未登录处理：升级后所有人重新登录一次。

### 3.3 登录限速（`web/controller/login_limiter.go`）

- 按来源 IP：10 分钟内失败 5 次，锁 15 分钟；锁定期内正确的凭据也拒绝。登录成功清零。
- 不按用户名锁：单管理员下那是一个把管理员锁在门外的拒绝服务入口。
- 来源 IP 由 `getRemoteIp` 给出：对端是回环才读 `X-Forwarded-For` 的最后一段，否则只认 TCP 对端。IPv6 按 /64 归并。
- 只存内存，上限 10000 条；满了只回收过期记录，不解除仍在生效的锁定。包级单例，面板 SIGHUP「重启」不清空，进程重启清空。
- 验证码错误与密码错误同样计数。

### 3.4 CSRF（`web/csrf.go`）

备选方案是同步 token（会话里存 token，模板注入，axios 带上）。没选它：面板所有请求已经由 axios 发出并带 `X-Requested-With`，要求这个头即可达到同样的防护，而不用改任何模板。

- 非 GET/HEAD/OPTIONS 请求必须带 `X-Requested-With: XMLHttpRequest`，且 `Sec-Fetch-Site` 不为 `cross-site`，否则 403。
- 配合 `SameSite=Lax` 的会话 cookie。
- 代价：今后任何不经 axios 的非 GET 请求都要自己带这个头，由两条测试守着。

### 3.5 两步验证（`util/totp`、`web/service/two_factor.go`）

- RFC 6238：HMAC-SHA1、6 位、30 秒、±1 步。标准库实现，二维码用仓库已有的 `qrious`，不新增依赖。
- 状态在 settings 表的三个 key 里，不进 `AllSetting`，不下发前端：`twoFactorSecret`（非空即开启）、`twoFactorPending`、`twoFactorLastStep`。
- 开启：输入当前密码 → 生成待确认密钥（密钥只在这一个响应里出现一次）→ 用一个有效验证码确认 → 开启。已开启时拒绝重新生成。
- 登录：密码与验证码同一次请求提交；是否显示验证码输入框由登录页模板的 `two_factor` 决定。两种失败对外同一句话。
- 重放防护：记住上次接受的时间步，只接受更新的，互斥锁串行化。
- 关闭：密码 + 有效验证码；或命令行 `a-ui setting -disable2fa`。
- 读不出开关状态时拒绝登录。
- `a-ui setting -reset` 清空 settings 表，顺带关闭两步验证；`a-ui.sh` 菜单第 4 项「重置用户名密码」同时关闭它。

## 4. 兼容性与回退

- 升级：无需人工操作。启动时迁移明文密码，所有人重新登录一次。
- **从面板里点「更新」跨到本版本时，更新弹窗会立刻显示「3 分钟内版本号仍未变化」**：那一刻运行的还是旧版本的前端脚本，它把「会话失效」当成超时。更新其实已经成功，刷新页面重新登录即可。本版本起前端为这种情形加了独立的 `relogin` 状态（`panel-version.js`），今后的回退不再出现这句误导性的提示。
- 无域名安装（面板走明文 HTTP）的机器，升级后应改一次密码：旧 cookie 里的明文密码可能已在链路上暴露过。
- **回退到本次改动之前的版本后无法登录**：旧二进制拿明文比哈希。救法：`a-ui setting -username <名> -password <新密码>`。回退的二次确认框已加入这条提示。再次升级时启动迁移会把旧二进制写回的明文重新哈希。
- 管理脚本永远来自 main 分支而二进制可能是旧版：`reset_user` 里 `-disable2fa` 是独立且允许失败的一条命令。

## 5. 已知残留

- `/logout` 是 GET，可被跨站触发。
- 一个本身恰好是合法 bcrypt 串的明文密码会被误判为哈希，只能命令行重设。
- 限速状态不跨进程重启保留。
- 存量超过 72 字节的密码保持明文，直到管理员重设。

## 6. 测试

每条都做过变异确认（破坏实现后测试变红）：

- `util/crypto`、`util/totp`：往返、加盐、超长拒绝、哈希本身不能当密码、RFC 6238 测试向量、时间窗边界。
- `web/service/user_test.go`：默认账号是哈希、迁移及其幂等、超长旧密码仍可登录。
- `web/service/two_factor_test.go`：完整生命周期、重放、密钥不出现在 `AllSetting`。
- `web/controller/login_test.go`：cookie 属性与内容、改密码（含命令行改）使旧会话失效、锁定不可被伪造的转发头绕过、反代后按最后一跳计数、验证码错误计入限速、两步验证登录全流程。
- `web/controller/login_limiter_test.go`：窗口、锁定到期、IPv6 /64、容量上限、不回收生效中的锁定。
- `web/csrf_test.go`、`web/login_page_test.go`：中间件判定、真实中间件链 + 真实登录接口、axios 请求头前提、无绕开 axios 的状态变更请求、验证码输入框按开关渲染。

登录页与设置页的改动未在浏览器里实测。
