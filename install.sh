#!/usr/bin/env bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)

# 面板当前实际生效的端口，供安全配置向导拼面板 URL 用。config_after_install
# 在能确定这个值的分支里会填它；确定不了（比如版本升级且保留原端口）就
# 留空，向导据此决定要不要把 -port 传给 bootstrap，宁可不传也不去猜。
panel_port=""

# handle_existing_web_server 成功停用某个 web server 后记下它的服务名，
# 供 domain_flow 在后续步骤（Caddy 装失败/Caddyfile 校验不过）失败时
# 重复打印回滚命令——用户此时多半已经翻过"确认停用"那一步打印的提示，
# 且此时 80/443 上是真的什么都不监听了，比第一次提示时更紧急。
stopped_web_svc=""

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}错误：${plain} 必须使用root用户运行此脚本！\n" && exit 1

# check os
if [[ -f /etc/redhat-release ]]; then
    release="centos"
elif cat /etc/issue | grep -Eqi "debian"; then
    release="debian"
elif cat /etc/issue | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /etc/issue | grep -Eqi "centos|red hat|redhat"; then
    release="centos"
elif cat /proc/version | grep -Eqi "debian"; then
    release="debian"
elif cat /proc/version | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /proc/version | grep -Eqi "centos|red hat|redhat"; then
    release="centos"
else
    echo -e "${red}未检测到系统版本，请联系脚本作者！${plain}\n" && exit 1
fi

arch=$(arch)

if [[ $arch == "x86_64" || $arch == "x64" || $arch == "s390x" || $arch == "amd64" ]]; then
    arch="amd64"
elif [[ $arch == "aarch64" || $arch == "arm64" ]]; then
    arch="arm64"
else
    arch="amd64"
    echo -e "${red}检测架构失败，使用默认架构: ${arch}${plain}"
fi

echo "架构: ${arch}"

if [ $(getconf WORD_BIT) != '32' ] && [ $(getconf LONG_BIT) != '64' ]; then
    echo "本软件不支持 32 位系统(x86)，请使用 64 位系统(x86_64)，如果检测有误，请联系作者"
    exit -1
fi

os_version=""

# os version
if [[ -f /etc/os-release ]]; then
    os_version=$(awk -F'[= ."]' '/VERSION_ID/{print $3}' /etc/os-release)
fi
if [[ -z "$os_version" && -f /etc/lsb-release ]]; then
    os_version=$(awk -F'[= ."]+' '/DISTRIB_RELEASE/{print $2}' /etc/lsb-release)
fi

if [[ x"${release}" == x"centos" ]]; then
    if [[ ${os_version} -le 6 ]]; then
        echo -e "${red}请使用 CentOS 7 或更高版本的系统！${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}请使用 Ubuntu 16 或更高版本的系统！${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}请使用 Debian 8 或更高版本的系统！${plain}\n" && exit 1
    fi
fi

install_base() {
    if [[ x"${release}" == x"centos" ]]; then
        yum install wget curl tar jq -y
    else
        apt install wget curl tar jq -y
    fi
}

#This function will be called when user installed a-ui out of sercurity
config_after_install() {
    # 新拓扑下面板端口是内部实现细节，不再问用户（见下方安全配置向导），
    # 但向导拼面板 URL 需要知道本次实际生效的端口：全新安装时下面两条
    # 分支都能确定；版本升级且用户选择保留原设置时，端口本来就没被
    # 这里改过，不确定实际值，不写入全局的 panel_port（留给调用方
    # 自行探测或干脆不在 URL 里带端口）。
    local is_fresh_install=1
    [[ -f "/etc/a-ui/a-ui.db" ]] && is_fresh_install=0

    echo -e "${yellow}出于安全考虑，安装/更新完成后需要强制修改账户密码${plain}"
    read -p "确认是否继续,如选择n则跳过本次账户密码设定[y/n]": config_confirm
    if [[ x"${config_confirm}" == x"y" || x"${config_confirm}" == x"Y" ]]; then
        read -p "请设置您的账户名:" config_account
        echo -e "${yellow}您的账户名将设定为:${config_account}${plain}"
        read -p "请设置您的账户密码:" config_password
        echo -e "${yellow}您的账户密码将设定为:${config_password}${plain}"
        echo -e "${yellow}确认设定,设定中${plain}"
        /usr/local/a-ui/a-ui setting -username ${config_account} -password ${config_password}
        echo -e "${yellow}账户密码设定完成${plain}"
        [[ ${is_fresh_install} -eq 1 ]] && panel_port=54321
    else
        echo -e "${red}已取消设定...${plain}"
        if [[ ${is_fresh_install} -eq 1 ]]; then
            local usernameTemp=$(head -c 6 /dev/urandom | base64)
            local passwordTemp=$(head -c 6 /dev/urandom | base64)
            local portTemp=$(echo $RANDOM)
            /usr/local/a-ui/a-ui setting -username ${usernameTemp} -password ${passwordTemp}
            /usr/local/a-ui/a-ui setting -port ${portTemp}
            panel_port=${portTemp}
            echo -e "检测到您属于全新安装,出于安全考虑已自动为您生成随机用户与端口:"
            echo -e "###############################################"
            echo -e "${green}面板登录用户名:${usernameTemp}${plain}"
            echo -e "${green}面板登录用户密码:${passwordTemp}${plain}"
            echo -e "${red}面板登录端口:${portTemp}${plain}"
            echo -e "###############################################"
            echo -e "${red}如您遗忘了面板登录相关信息,可在安装完成后输入a-ui,输入选项7查看面板登录信息${plain}"
        else
            echo -e "${red}当前属于版本升级,保留之前设置项,登录方式保持不变,可输入a-ui后键入数字7查看面板登录信息${plain}"
        fi
    fi
}

# 生成面板 url 根路径用的随机串。防的是全网扫描器按 /xui/ 这个 x-ui 系
# 默认路径批量定位面板——这是本次改造要解决的核心问题之一。
gen_random_path() {
    LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 12
}

# 返回占用指定端口的进程名，空表示没人占用。
#
# 必须带 -p 才会输出 users:(("进程名",pid=...)) 这一列——不带 -p 时
# ss 的最后一列是对端地址（如 0.0.0.0:*），永远不含进程名，会让所有
# 基于进程名的判断（nginx/apache/caddy）失效，一律落进"未知进程"分支。
port_user() {
    ss -ltnHp "sport = :$1" 2>/dev/null | grep -oP '(?<=users:\(\(")[^"]+' | head -1
}

# 本机公网 IP。取不到返回空串，调用方必须容忍这种情况——机器可能在
# NAT 后面，或者到探测服务的出站被墙，都不该因此让安装失败。
public_ip() {
    curl -fsS -m 10 https://api.ipify.org 2>/dev/null || \
    curl -fsS -m 10 https://ifconfig.me 2>/dev/null || true
}

# 面板当前实际监听的端口，用于给向导打印的 URL 拼端口号。优先用
# config_after_install 在本次运行里已经确定的值；确定不了时（比如
# --wizard-only 单独触发向导，面板服务本来就在跑）改为探测正在运行的
# a-ui 进程实际监听的端口；两者都拿不到就返回失败，调用方不传
# -port，让 bootstrap 保持 webPort 原样，不去猜一个可能是错的值。
current_panel_port() {
    if [[ -n "${panel_port}" ]]; then
        echo "${panel_port}"
        return 0
    fi
    local pid port
    # 不用 `--value`：该选项 systemd >=230 才支持，而本脚本声称支持
    # 自带 systemd 219 的 CentOS 7，用 --value 在那上面会因未知选项直接报错。
    # `systemctl show` 的输出固定是 `KEY=VALUE`，cut 取等号后半段等价且兼容更旧版本。
    pid=$(systemctl show -p MainPID a-ui 2>/dev/null | cut -d= -f2)
    [[ -z "${pid}" || "${pid}" == "0" ]] && return 1
    port=$(ss -ltnp 2>/dev/null | grep -F "pid=${pid}," | awk '{print $4}' | awk -F: '{print $NF}' | head -1)
    [[ -z "${port}" ]] && return 1
    echo "${port}"
}

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

    # 拿不到当前面板端口就不传 -port：让 bootstrap 保持 webPort 原样，
    # 总比传一个猜错的值把用户已有端口悄悄改掉安全。
    local port_args=()
    local detected_port
    if detected_port=$(current_panel_port); then
        port_args=(-port "${detected_port}")
    fi

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode reality \
            -reality-dest "${target}:443" -basepath "${basepath}" "${port_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}配置失败：${out}${plain}"
        echo -e "${yellow}面板保持默认配置，仍可正常访问${plain}"
        return 1
    fi
    print_result "${out}" "reality" "${detected_port}"
}

print_result() {
    local json="$1"
    local mode="$2"
    local port="$3"
    local url
    url=$(echo "${json}" | jq -r '.panelUrl // empty' 2>/dev/null)

    # panelUrl 里的 "<服务器IP>" 是 bootstrap 留给脚本填的占位符（面板
    # 进程不知道自己的公网 IP）；探测不到就保留占位符并额外提示，
    # 不能默默打印一个看起来正常、实际打不开的地址。
    if [[ "${url}" == *"<服务器IP>"* ]]; then
        local ip
        ip=$(public_ip)
        [[ -n "${ip}" ]] && url="${url//<服务器IP>/${ip}}"
    fi

    echo -e ""
    echo -e "${green}=== 配置完成 ===${plain}"
    if [[ -n "${url}" ]]; then
        echo -e "${green}面板地址: ${url}${plain}"
    fi
    if [[ "${url}" == *"<服务器IP>"* ]]; then
        echo -e "${yellow}（未能自动探测服务器公网 IP，请把地址中的 <服务器IP> 换成你的服务器公网 IP）${plain}"
    fi
    if [[ "${url}" == *":0/"* ]]; then
        echo -e "${yellow}（未能确定面板当前监听的端口，请用 a-ui 菜单选项 7 查看实际端口）${plain}"
    fi
    if [[ "${mode}" == "reality" ]]; then
        echo -e "${green}已创建 VLESS+Vision+REALITY 入站（443 端口），登录面板即可查看分享链接与二维码${plain}"
    fi
    echo -e ""
    echo -e "${yellow}如果面板打不开，用以下任一方式恢复：${plain}"
    echo -e "  a-ui setting -listen \"\"                       # 恢复监听所有 IP"
    if [[ -n "${port}" ]]; then
        echo -e "  ssh -L ${port}:127.0.0.1:${port} root@<本机IP>     # 或走 SSH 隧道"
    else
        # 端口没探测到就不能编出一个具体数字——猜错了这条救命通道就是废的。
        echo -e "  ssh 隧道：先用 a-ui 菜单第 7 项查看面板实际端口，再执行"
        echo -e "  ssh -L <端口>:127.0.0.1:<端口> root@<本机IP>"
    fi
    echo -e ""
    echo -e "${yellow}注意：本次配置隐藏的是面板。你在面板里创建的入站端口仍然对外暴露，${plain}"
    echo -e "${yellow}其抗探测能力与配置前相同。${plain}"
}

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

    # stop 失败必须如实报告并中止：不能吞掉错误接着往下走——80/443 这时
    # 仍被占用，后面 install_caddy/write_caddyfile 会顺着失败，但报错会
    # 指向 Caddy 而不是这里的真正原因，非常误导。
    local stop_err
    if ! stop_err=$(systemctl stop "${svc}" 2>&1); then
        echo -e "${red}停用 ${svc} 失败，80/443 可能仍被占用，请检查后重试：${plain}"
        echo -e "${red}${stop_err}${plain}"
        return 1
    fi
    local disable_err
    if ! disable_err=$(systemctl disable "${svc}" 2>&1); then
        echo -e "${yellow}${svc} 已停止运行，但取消开机自启失败，重启机器后可能重新占用 80/443：${plain}"
        echo -e "${yellow}${disable_err}${plain}"
    fi
    stopped_web_svc="${svc}"
    echo -e "${green}${svc} 已停用（软件包与配置文件均保留）${plain}"
    return 0
}

# install_caddy/write_caddyfile 失败时，若上面已经停用过旧的 web server，
# 此时 80/443 上完全没有服务在监听，必须重复提示回滚命令。
print_stopped_svc_rollback_hint() {
    [[ -z "${stopped_web_svc}" ]] && return 0
    echo -e "${yellow}${stopped_web_svc} 已经被停用，此时 80/443 上没有任何服务在监听${plain}"
    echo -e "${yellow}如需先恢复旧站点：systemctl enable --now ${stopped_web_svc}${plain}"
}

# apt 分支的命令已在 Ubuntu 20.04.6 aarch64 上实测通过（Caddy 2.11.4）。
# 脚本以 root 运行（开头有 EUID 检查），所以不加 sudo。
#
# 两个 return 0 分支都要 enable：已安装但曾被手动停用/禁用（真机验证环境
# 就是这个状态）同样要收编开机自启，否则重启后 a-ui 已按 systemd 自启但
# Caddy 不起来，80/443 上什么都不监听，面板（已收编到 127.0.0.1）与
# 伪装站一起从外网彻底消失——比配置向导本身失败更隐蔽。
install_caddy() {
    if command -v caddy &>/dev/null; then
        echo -e "${green}检测到已安装 Caddy: $(caddy version | head -1)${plain}"
        systemctl enable caddy 2>/dev/null
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
    systemctl enable caddy 2>/dev/null
    return 0
}

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

# a-ui.service 是 Type=simple 且没有配 Restart=：`systemctl restart` 返回
# 成功只代表进程被 fork 出来了，不代表它没有立刻退出（比如遗留的
# webCertFile/webKeyFile 指向一个已经不存在的证书文件，tls.LoadX509KeyPair
# 失败、Server.Start() 返回 error，main.go 只 log 一行就 return）。这种
# "起来了又立刻死了"从 systemctl 的返回码看不出来，必须真的探活。
wait_for_panel_alive() {
    local panel_port="$1" basepath="$2"
    local i
    for i in $(seq 1 5); do
        if curl -fsS -m 3 "http://127.0.0.1:${panel_port}${basepath}" -o /dev/null 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# 把 Caddy 管理的证书同步到固定路径。面板与各入站用的是固定路径
# /root/cert/，而 Caddy 自己的证书存储路径含 ACME CA 的目录名，切换
# 签发机构时会变——直接指过去会在某次续期后静默失效。
#
# Task 0 已实测确认 Caddy 事件钩子不可用（events.handlers.exec 模块
# 未注册，caddy validate 直接报错），所以只有 systemd timer 一条路。
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

    if ! install_caddy; then
        print_stopped_svc_rollback_hint
        return 1
    fi

    local mask_url
    mask_url=$(choose_mask_site) || return 1

    local basepath panel_port
    basepath="/$(gen_random_path)/"
    panel_port=54321

    if ! write_caddyfile "${domain}" "${basepath}" "${panel_port}" "${mask_url}"; then
        print_stopped_svc_rollback_hint
        return 1
    fi
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
    if ! wait_for_panel_alive "${panel_port}" "${basepath}"; then
        echo -e "${red}面板重启后探活失败，127.0.0.1:${panel_port} 没有响应${plain}"
        echo -e "${red}面板配置已写入，但进程可能启动后又退出了（常见原因：证书路径不存在或不可读）${plain}"
        echo -e "${yellow}排查：journalctl -u a-ui -n 50${plain}"
        return 1
    fi
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

install_a-ui() {
    # 端口探测必须在 stop 之前做：current_panel_port() 的探测分支靠
    # systemctl show -p MainPID 找正在跑的 a-ui 进程，服务一旦被下面这行
    # stop 掉就永远拿不到 PID 了。探测成功说明是给已有部署做更新，直接
    # 记下来；探测失败就是机器上本来没装过，交给 config_after_install
    # 在全新安装的分支里另行确定，这里不用去区分"为什么失败"。
    local pre_stop_port
    if pre_stop_port=$(current_panel_port); then
        panel_port="${pre_stop_port}"
    fi

    systemctl stop a-ui
    cd /usr/local/

    if [ $# == 0 ]; then
        last_version=$(curl -Lsk "https://api.github.com/repos/SienFeng/AetherUI/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$last_version" ]]; then
            echo -e "${red}检测 a-ui 版本失败，可能是超出 Github API 限制，请稍后再试，或手动指定 a-ui 版本安装${plain}"
            exit 1
        fi
        echo -e "检测到 a-ui 最新版本：${last_version}，开始安装"
        wget -N --no-check-certificate -O /usr/local/a-ui-linux-${arch}.tar.gz https://github.com/SienFeng/AetherUI/releases/download/${last_version}/a-ui-linux-${arch}.tar.gz
        if [[ $? -ne 0 ]]; then
            echo -e "${red}下载 a-ui 失败，请确保你的服务器能够下载 Github 的文件${plain}"
            exit 1
        fi
    else
        last_version=$1
        url="https://github.com/SienFeng/AetherUI/releases/download/${last_version}/a-ui-linux-${arch}.tar.gz"
        echo -e "开始安装 a-ui $1"
        wget -N --no-check-certificate -O /usr/local/a-ui-linux-${arch}.tar.gz ${url}
        if [[ $? -ne 0 ]]; then
            echo -e "${red}下载 a-ui $1 失败，请确保此版本存在${plain}"
            exit 1
        fi
    fi

    if [[ -e /usr/local/a-ui/ ]]; then
        rm /usr/local/a-ui/ -rf
    fi

    tar zxvf a-ui-linux-${arch}.tar.gz
    rm a-ui-linux-${arch}.tar.gz -f
    cd a-ui
    chmod +x a-ui bin/xray-linux-${arch}
    cp -f a-ui.service /etc/systemd/system/
    wget --no-check-certificate -O /usr/bin/a-ui https://raw.githubusercontent.com/SienFeng/AetherUI/main/a-ui.sh
    chmod +x /usr/local/a-ui/a-ui.sh
    chmod +x /usr/bin/a-ui
    config_after_install
    #echo -e "如果是全新安装，默认网页端口为 ${green}54321${plain}，用户名和密码默认都是 ${green}admin${plain}"
    #echo -e "请自行确保此端口没有被其他程序占用，${yellow}并且确保 54321 端口已放行${plain}"
    #    echo -e "若想将 54321 修改为其它端口，输入 a-ui 命令进行修改，同样也要确保你修改的端口也是放行的"
    #echo -e ""
    #echo -e "如果是更新面板，则按你之前的方式访问面板"
    #echo -e ""
    setup_wizard
    systemctl daemon-reload
    systemctl enable a-ui
    systemctl start a-ui
    echo -e "${green}a-ui ${last_version}${plain} 安装完成，面板已启动，"
    echo -e ""
    echo -e "a-ui 管理脚本使用方法: "
    echo -e "----------------------------------------------"
    echo -e "a-ui              - 显示管理菜单 (功能更多)"
    echo -e "a-ui start        - 启动 a-ui 面板"
    echo -e "a-ui stop         - 停止 a-ui 面板"
    echo -e "a-ui restart      - 重启 a-ui 面板"
    echo -e "a-ui status       - 查看 a-ui 状态"
    echo -e "a-ui enable       - 设置 a-ui 开机自启"
    echo -e "a-ui disable      - 取消 a-ui 开机自启"
    echo -e "a-ui log          - 查看 a-ui 日志"
    echo -e "a-ui v2-ui        - 迁移本机器的 v2-ui 账号数据至 a-ui"
    echo -e "a-ui update       - 更新 a-ui 面板"
    echo -e "a-ui install      - 安装 a-ui 面板"
    echo -e "a-ui uninstall    - 卸载 a-ui 面板"
    echo -e "a-ui geo          - 更新 geo  数据"
    echo -e "----------------------------------------------"
}

# 单独触发安全配置向导，不做下载/解压/装 systemd 服务那一整套安装流程。
# 用于面板已经装好之后重新配置（a-ui 菜单的「配置域名与伪装站」走这里），
# 也是本地开发时验证向导本身的入口。
if [[ "$1" == "--wizard-only" ]]; then
    setup_wizard
    exit 0
fi

echo -e "${green}开始安装${plain}"
install_base
install_a-ui $1
