# AetherUI

基于 [Xray-core](https://github.com/XTLS/Xray-core) 的 Web 管理面板，二进制与模块名均为 `a-ui`。

## 一键安装

```bash
bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh)
```

英文版管理脚本：

```bash
bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install_en.sh)
```

安装完成后面板默认监听 **54321** 端口，默认账号密码均为 `admin`。**请立刻登录面板修改账号密码。**

> 安装脚本会拉取 GitHub Release 里最新的 `a-ui-linux-<架构>.tar.gz`，解压到 `/usr/local/a-ui/`，注册 systemd 服务并开机自启。

## 系统要求

- **Linux x86_64 / aarch64**，CentOS / Debian / Ubuntu
- **root 权限**（systemd 服务、内核连接表、`tc` 限速都需要）

## 管理命令

安装后可直接输入 `a-ui` 打开管理菜单，也可以用子命令：

```bash
a-ui start        # 启动
a-ui stop         # 停止
a-ui restart      # 重启
a-ui status       # 查看状态
a-ui log          # 查看日志
a-ui update       # 更新到最新版
a-ui uninstall    # 卸载
```

面板二进制自身还有几个子命令：

```bash
/usr/local/a-ui/a-ui setting -port 54321              # 改面板端口
/usr/local/a-ui/a-ui setting -username x -password y  # 改账号密码
/usr/local/a-ui/a-ui setting -reset                   # 重置全部设置
/usr/local/a-ui/a-ui tc-clear                         # 清除全部限速规则（救援用，见下）
```

## 功能

**入站管理**

- vmess / vless / trojan / shadowsocks / dokodemo-door / socks / http
- 流量统计、到期时间、启用开关、二维码与分享链接
- **一键续期**：7 天 / 30 天 / 2 个月 / 3 个月 / 半年 / 一年，或自定义日期。未到期则叠加，已过期则从今天算起；自动重新启用被停用的入站并重置已用流量

**域名分流**

- 域名组（支持订阅地址，可定时更新）× 入站 → 指定落地节点或直接封禁
- 出站节点支持从分享链接导入（vmess / vless / trojan / ss / hysteria2 / wireguard / socks）
- 配置在**落库前**交给真实 xray 校验，挡住会让 xray 起不来的配置

**在线明细**

- 点击入站列表任一行向下展开，显示每个来源 IP 的上线时间、归属地、连接数、实时上下行速度、本次上线用量
- 可手动踢下线
- 数据来自 Linux 内核连接表（netlink INET_DIAG），仅对 TCP 类传输有效（tcp / ws / grpc / h2）

**并发限制**

- 按入站限制**同时在线的不同来源 IP 数**（不是 TCP 连接数）
- 超额时拒绝后来的 IP，已在线的人不受影响；被拒的 IP 在展开行里有「超额被拒」标记

**地区限制**

- 按中国省级地区限制来源，可多选（例如只允许江苏 + 河南）
- 归属地数据为**本地离线库**，双源并存（ip2region + 纯真 IP 库），判定取并集
- 支持手动更新与**每日定时更新**（默认关闭）
- 启用后该入站只监听 IPv4；另有一条 `::/0` 拒绝规则作为纵深防御

**端口限速**

- 按入站分别限制上行 / 下行带宽（Mbps），由 Linux `tc` HTB + ifb 实现
- 未被限速的流量走**不限速的默认队列**，SSH 等不受影响
- 下发新配置后 2 分钟内若收不到任何面板请求，**自动撤销全部规则**

**访问日志**

- 记录每个用户在什么时间访问了什么目标、走的哪条路由
- 存在独立的 SQLite 库里，保留天数可配，超期每小时自动清理
- **默认关闭**，需在面板设置里显式开启

## 重要提醒

### 端口限速可能让机器失联

`tc` 限速要接管网卡的 root qdisc，**配错会影响整机全部流量，包括 SSH**。

请务必先在**能从服务商控制台（VNC / 串口）登录**的机器上验证。若限速导致网络中断、面板也打不开，从控制台登录后执行：

```bash
/usr/local/a-ui/a-ui tc-clear
```

这条命令不连数据库、不启动面板，直接清除本面板下发的全部 tc 规则。

### 关于内核相关功能的验证状态

**在线明细、并发限制、端口限速**这三项依赖 Linux 内核接口（netlink INET_DIAG、SOCK_DESTROY、tc）。它们的判定逻辑、命令生成、状态机都有完整的单元测试覆盖，但**内核调用本身尚未在生产环境实测**。首次启用请在非关键机器上验证。

其余功能（续期、域名分流、地区限制、访问日志、IP 归属地库）有跑真实 Xray 的端到端测试。

### 其它

- 密码在数据库中**明文存储**，这是继承自上游的既有实现
- 面板设置里的「安装 xray」会连带覆盖 `bin/geoip.dat` 与 `bin/geosite.dat`
- 面板首页报告的 xray 状态在极少数情况下可能滞后。排查「面板说正常但用不了」时，以 `pgrep xray` 和 `xray run -test -c bin/config.json` 为准

## 环境变量

| 变量 | 说明 |
|---|---|
| `XUI_DEBUG` | `true` 时模板与静态资源从磁盘读取，需在仓库根目录启动 |
| `XUI_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |

## 从源码构建

```bash
# CGO 必须开启（gorm.io/driver/sqlite 依赖 mattn/go-sqlite3）
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o a-ui main.go

# 交叉编译 linux/arm64（需 gcc-aarch64-linux-gnu）
CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o a-ui main.go

# 本地开发运行
XUI_DEBUG=true go run main.go
```

## 致谢与许可

代码谱系为 [vaxilu/x-ui](https://github.com/vaxilu/x-ui) → [FranzKafkaYu/x-ui](https://github.com/FranzKafkaYu/x-ui) 的分支。

`util/link` 移植自 [3x-ui](https://github.com/MHSanaei/3x-ui)。

IP 归属地数据来自 [ip2region](https://github.com/lionsoul2014/ip2region)（Apache-2.0 OR MIT）与纯真 IP 库。

本项目以 **GPL-3.0** 许可发布。
