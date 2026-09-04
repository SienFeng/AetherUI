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

# openssl 不是可选项：check_reality_target 的四项判据全靠 openssl s_client，
# 缺了它 REALITY 分支必然判"目标不满足要求"，而这个理由完全是假的。
# jq 同理——print_result 靠它解析 bootstrap 的 JSON 输出，缺了它"面板地址"
# 那一整行会静默消失，用户刚把面板挪到一个随机路径上却拿不到地址。
install_base() {
    if [[ x"${release}" == x"centos" ]]; then
        yum install wget curl tar jq openssl -y
    else
        apt install wget curl tar jq openssl -y
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
#
# 参数 $1 传字面量 "force" 时：跳过下面的幂等探测（即使已配置过也重新
# 走一遍向导），并把 -force 透传给后续 domain_flow/reality_flow 的
# `a-ui bootstrap` 调用，让新配置真正覆盖旧的。--wizard-only 单独触发
# （a-ui 菜单「配置域名与伪装站」）时用这个值——从菜单主动发起"重新
# 配置"，用户的意图就是要覆盖，不该被这道幂等判断拦下。
setup_wizard() {
    local force="$1"
    if [[ "${force}" != "force" ]] && /usr/local/a-ui/a-ui bootstrap -check -json 2>/dev/null | grep -q '"skipped": true'; then
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
        domain_flow "${force}"
    else
        reality_flow "${force}"
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
    local force="$1"

    # 端口探测提到最前面：下面每一条失败分支在 force 重跑时都要用它拼
    # 救援命令里的 SSH 隧道端口。拿不到就不传 -port，让 bootstrap 保持
    # webPort 原样，总比传一个猜错的值把用户已有端口悄悄改掉安全。
    local port_args=()
    local detected_port
    if detected_port=$(current_panel_port); then
        port_args=(-port "${detected_port}")
    fi

    # REALITY 入站固定占 443，而落库前的两道防线都发现不了端口冲突：
    # AddInbound 的 checkPortExist 只查面板自己的 inbound 表；
    # ValidateInboundReplacing 走的 `xray run -test` 根本不 bind 端口。
    # 于是入站照常落库，直到下一次 RestartXray 时 xray bind 443 失败、
    # 整个进程起不来——机器上所有既有入站一起断网，而面板首页照样显示
    # running。必须在这里拦下，做法与 domain_flow 处理 80/443 一致。
    local occupant443
    occupant443=$(port_user 443)
    if [[ -n "${occupant443}" ]]; then
        echo -e "${red}443 端口已被 ${occupant443} 占用，REALITY 入站无法使用该端口${plain}"
        echo -e "${yellow}请先停用占用它的服务（例如 systemctl stop ${occupant443}）后重新运行向导${plain}"
        echo -e "${yellow}跳过配置向导${plain}"
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

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
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

    echo -e "正在检查 ${target} 是否满足 REALITY 要求（TLS1.3 / ALPN h2 / X25519）…"
    if ! check_reality_target "${target}"; then
        echo -e "${red}${target} 不满足要求，请换一个目标${plain}"
        # "面板保持默认配置"只在全新配置时成立。force 是重新配置，面板的
        # 监听地址/根路径在更早一次成功的配置里就已经被改过了，这里再说
        # "保持默认配置"是主动的错误安慰。
        if [[ "${force}" == "force" ]]; then
            echo -e "${yellow}跳过配置向导，本次没有改动面板配置（沿用上一次配置的结果）${plain}"
        else
            echo -e "${yellow}跳过配置向导，面板保持默认配置，可稍后运行 a-ui 重新配置${plain}"
        fi
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi
    echo -e "${green}检查通过${plain}"

    local basepath
    basepath="/$(gen_random_path)/"

    local force_args=()
    [[ "${force}" == "force" ]] && force_args=(-force)

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode reality \
            -reality-dest "${target}:443" -basepath "${basepath}" "${port_args[@]}" \
            "${force_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}配置失败：${out}${plain}"
        echo -e "${yellow}面板保持默认配置，仍可正常访问${plain}"
        # 通常这条分支面板确实没被动过（上面那句提示是对的）；但如果这是
        # 一次覆盖已有配置的重装（比如从有域名模式切回无域名模式），
        # webListen 完全可能在上一次成功配置里就已经被改成了 127.0.0.1，
        # 这次 bootstrap 失败不会把它改回来——救援命令必须照样打印，
        # 不能因为“通常情况下不需要”就省掉这唯一一次判断失误的兜底。
        print_rescue_hint "${detected_port}"
        return 1
    fi
    print_result "${out}" "reality" "${detected_port}"
}

# 两条救援命令的唯一出处，print_result 与 bootstrap 调用点之后的每一条
# 失败分支共用——这是「失败不锁面板」原则的最后一道保险，两份逻辑各写
# 一遍必然漂移。参数 $1 是面板端口：拿得到就传，拿不到传空串，退化成
# 「先用菜单第 7 项查看端口」的提示，不编造一个可能是错的数字。
print_rescue_hint() {
    local port="$1"
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
}

# bootstrap 调用点**之前**的失败分支用这个，而不是无条件的 print_rescue_hint。
#
# 判据是"本次是不是 force 重跑"：force 意味着面板此前已经被这个向导成功
# 配置过一次，webListen 一定已经是 127.0.0.1，而中途放弃并不会把它改回来；
# 更要命的是重跑路径上 handle_existing_web_server 可能已经把 Caddy（我们
# 自己那个）停掉，此时面板从外网完全不可达，屏幕上最后一句却可能只是
# "已取消"。全新安装（非 force）时 webListen 还是默认值、面板照常监听所有
# IP，打印这两条命令只是噪音。
#
# 不去区分"这条分支之前有没有真的动过什么"：重跑时上一次运行留下的状态
# 从这里根本看不出来，多打两行提示的代价，远低于留下一个够不着的面板。
print_rescue_hint_if_force() {
    [[ "$1" == "force" ]] || return 0
    print_rescue_hint "$2"
}

print_result() {
    local json="$1"
    local mode="$2"
    local port="$3"
    local url
    url=$(echo "${json}" | jq -r '.panelUrl // empty' 2>/dev/null)

    # panelUrl 里的 "{SERVER_IP}" 是 bootstrap 留给脚本填的占位符（面板
    # 进程不知道自己的公网 IP）；探测不到就保留占位符并额外提示，
    # 不能默默打印一个看起来正常、实际打不开的地址。占位符是语言无关的
    # 字面量，中英两份脚本匹配的是同一个串（见 bootstrap/bootstrap.go 的
    # panelURL）。
    if [[ "${url}" == *"{SERVER_IP}"* ]]; then
        local ip
        ip=$(public_ip)
        [[ -n "${ip}" ]] && url="${url//\{SERVER_IP\}/${ip}}"
    fi

    echo -e ""
    echo -e "${green}=== 配置完成 ===${plain}"
    if [[ -n "${url}" ]]; then
        echo -e "${green}面板地址: ${url}${plain}"
    fi
    if [[ "${url}" == *"{SERVER_IP}"* ]]; then
        echo -e "${yellow}（未能自动探测服务器公网 IP，请把地址中的 {SERVER_IP} 换成你的服务器公网 IP）${plain}"
    fi
    if [[ "${url}" == *":0/"* ]]; then
        echo -e "${yellow}（未能确定面板当前监听的端口，请用 a-ui 菜单选项 7 查看实际端口）${plain}"
    fi
    if [[ "${mode}" == "reality" ]]; then
        echo -e "${green}已创建 VLESS+Vision+REALITY 入站（443 端口），登录面板即可查看分享链接与二维码${plain}"
    fi
    print_rescue_hint "${port}"
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

# handle_existing_web_server 停用的若正是我们自己的 Caddy（重新配置一台
# 已经跑过本向导的机器时，80 上占着的就是它），向导中途失败退出前必须把
# 它拉回来：此时面板多半已经只监听 127.0.0.1，Caddy 一停，面板从外网彻底
# 消失，而屏幕上最后一句可能只是"已取消"。
#
# 只对 caddy 做这件事。nginx/apache 是用户自己的资产，用户刚刚才明确确认
# 过要停用它们，替他们擅自拉回来是在推翻那次确认。
restart_own_caddy_if_stopped() {
    [[ "${stopped_web_svc}" != "caddy" ]] && return 0
    echo -e "${yellow}正在把先前停用的 Caddy 重新启动，以恢复面板与伪装站的外网访问…${plain}"
    if systemctl start caddy 2>/dev/null; then
        echo -e "${green}Caddy 已重新启动${plain}"
        stopped_web_svc=""
        return 0
    fi
    echo -e "${red}Caddy 重新启动失败，请手动执行：systemctl start caddy${plain}"
    return 1
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

# 从 URL 取出 host（不含端口，也不区分裸域名与 www 子域名），处理
# https://host/path 与 https://host:port/path 两种形态，供 check_mask_site
# 判断跳转是否真的跨域——host → www.host 这类规范化不算换了域名。
_mask_site_url_host() {
    local u="${1#*://}"
    u="${u%%/*}"
    u="${u%%:*}"
    u="${u,,}"
    echo "${u#www.}"
}

# 判定不通过的五种情况：地址带路径；根路径上有任何跳转；状态码非 2xx；
# 未知路径上跨域跳转；被 Cloudflare 拦截。探测者看到一个坏掉的镜像站，
# 比看到一个朴素的静态页可疑得多。
#
# 成功时不输出任何东西、返回 0，调用方直接拿传进来的那个 URL 去反代；
# 失败时 stdout 输出拒绝原因。不再有"跟随一跳后返回解析地址"那套逻辑：
# Caddy 的 reverse_proxy upstream 只接受 scheme/host/port，而 curl 的
# %{redirect_url} 输出的绝对地址几乎总是至少带一个 "/"，那条分支从落地
# 起就写不出一份能通过 caddy validate 的配置。
check_mask_site() {
    local url="$1"
    local ua="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    local resp code redirect headers after_scheme probe_url probe_resp probe_code probe_redirect

    # 带路径的地址在这里就拦下，不留到 write_caddyfile 才失败——那时旧的
    # web server 可能已经被停用，80/443 上什么都不监听了。Caddy 2.11.4 实测：
    # reverse_proxy https://www.python.org/ → for now, URLs for proxy upstreams
    # only support scheme, host, and port components。
    # 尾斜杠是合法输入（浏览器地址栏复制出来就带），规范化掉而不是拒绝。
    # 但它必须被规范化：caddy 2.11.4 实测 `reverse_proxy https://host/` 同样报
    # "upstreams only support scheme, host, and port"——尾斜杠正是它拒绝的东西，
    # 所以判据用 */* 而不是 */?*（后者会放行尾斜杠，让失败推迟到 write_caddyfile）。
    url="${url%/}"
    after_scheme="${url#*://}"
    if [[ "${after_scheme}" == */* ]]; then
        echo "反代目标不能带路径（Caddy 的 upstream 只接受 scheme/host/port）"
        return 1
    fi

    resp=$(curl -sS -o /dev/null -m 10 -A "${ua}" \
                -w '%{http_code} %{redirect_url}' "${url}" 2>/dev/null) || {
        echo "无法连接"
        return 1
    }
    code=$(echo "${resp}" | awk '{print $1}')
    redirect=$(echo "${resp}" | awk '{print $2}')

    # 根路径上任何跳转一律拒绝，同域跳转也不放过。两条理由各自都充分：
    #   1. 跳转后的地址带路径，写不进 Caddy 的 upstream（见上）。
    #   2. 反代目标本身会跳转的话，Caddy 默认把上游的 Location 原样转发给
    #      访客，访客浏览器直接跳到真实的伪装源，地址栏从用户的域名变成
    #      别人的域名，伪装当场败露。
    # curl 不带 -L 时 3xx 响应的 http_code 本身就非 2xx，所以这条判断必须
    # 排在状态码判断之前，否则拒绝原因会退化成一个没信息量的 "HTTP 301"。
    # 全部预置候选 2026-09-04 于东京机房 IP 实测都是根路径直接 200 且无
    # 跳转，这条判据不会误伤它们。
    if [[ -n "${redirect}" ]]; then
        echo "根路径跳转到 ${redirect}"
        return 1
    fi

    if [[ ! "${code}" =~ ^2 ]]; then
        echo "HTTP ${code}"
        return 1
    fi

    # 未知路径探测。只看根路径远远不够：伪装站被访问最多的恰恰是非根路径。
    # 实测 www.wikipedia.org 根路径 200、任意未知路径却回跨域 301
    # （location: https://en.wikipedia.org/<原路径>），Caddy 把它原样转发给
    # 访客，一条响应同时说明"这不是那个站的域名"和"它在裸反代那个站"。
    #
    # 只拒**跨域**跳转：同域的绝对跳转由 write_caddyfile 里的
    # header_down Location 重写兜住（host 换成访客请求的域名、路径不变，
    # 上游照样能提供该路径，不会回环）；跨域跳转被重写后会打回本域、再次
    # 被反代到上游、再次跳转，变成重定向死循环，所以必须在这里直接拒掉。
    #
    # 探测本身失败（网络抖动）与"探测通过"是两回事，只警告不放行也不拒绝，
    # 与下面的 Cloudflare 检测保持同一种处理方式。
    probe_url="${url%/}/aui-probe-$(gen_random_path)/"
    probe_resp=$(curl -sS -o /dev/null -m 10 -A "${ua}" \
                     -w '%{http_code} %{redirect_url}' "${probe_url}" 2>/dev/null) || {
        echo -e "${yellow}警告：无法完成未知路径探测，已跳过该项${plain}" >&2
        probe_resp=""
    }
    if [[ -n "${probe_resp}" ]]; then
        probe_code=$(echo "${probe_resp}" | awk '{print $1}')
        probe_redirect=$(echo "${probe_resp}" | awk '{print $2}')
        if [[ "${probe_code}" =~ ^3 ]] && [[ -n "${probe_redirect}" ]] && \
           [[ "$(_mask_site_url_host "${url}")" != "$(_mask_site_url_host "${probe_redirect}")" ]]; then
            echo "未知路径回了跨域跳转（${probe_code} → ${probe_redirect}），反代后会当场暴露伪装"
            return 1
        fi
    fi

    # 第一次 GET 探测成功不保证这次 HEAD 也成功（部分站点/WAF 对 HEAD 方法
    # 策略不同）；探测失败与探测结果为"无拦截"是两回事，不能不做区分地
    # 都当成通过——那样会把真正的探测故障悄悄吞掉。
    headers=$(curl -sSI -m 10 -A "${ua}" "${url}" 2>/dev/null) || {
        echo -e "${yellow}警告：无法完成 Cloudflare 拦截检测，已跳过该项${plain}" >&2
        return 0
    }
    if echo "${headers}" | grep -qi "cf-mitigated"; then
        echo "被 Cloudflare 拦截"
        return 1
    fi
    return 0
}

# 候选来自真机实测（2026-09-04 于东京机房 IP）：状态码 2xx、无跳转、无 CF 拦截。
# 必须从机房 IP 测，住宅 IP 的结果不作数——同一个站，住宅 IP 正常、
# 机房 IP 吃 403 或人机验证非常常见。站点策略会变，隔一段时间要重测。
#
# 已实测拒绝、不要加回来的：
#   - gnu.org（连不上）
#   - tesla.com（403）。注意它只是不能作为**反代目标**；作为 REALITY 的 dest
#     完全可用，两者判据不同（REALITY 是 TCP 透传，不发 HTTP 请求）。
#   - www.wikipedia.org（2026-09-04 实测）：根路径 200 没问题，但任意未知
#     路径回一个跨域 301 → https://en.wikipedia.org/<原路径>。Caddy 把这个
#     Location 原样转发给访客，一条响应同时说明"这不是那个站的域名"和
#     "它在裸反代那个站"——伪装当场败露，而伪装站被访问最多的恰恰是非根路径。
MASK_SITES=(
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

# stdout 输出选定的 URL；输出空串表示使用本地静态页；返回非 0 表示用户放弃
# （显式输入 q，或 read 在 /dev/tty 上失败——没有控制终端、SSH 中途断线都会
# 走到这里；两处 read 都必须检查返回码并 return，否则 choice 保持不变、
# 下一轮循环立刻再次触发同样的失败，会在没有任何 I/O 等待的情况下死循环）。
# 提示全部写到 stderr——调用方用 $(...) 捕获 stdout 作为 URL。
# read 显式从 /dev/tty 读：本函数跑在命令替换的子 shell 里，不这样写读不到终端输入。
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
    echo "  q) 放弃配置" >&2

    local choice url reason
    while true; do
        if ! read -p "请输入序号: " choice </dev/tty; then
            echo -e "${red}输入已结束，放弃配置${plain}" >&2
            return 1
        fi
        if [[ "${choice}" == "q" || "${choice}" == "Q" ]]; then
            echo -e "${yellow}已取消${plain}" >&2
            return 1
        elif [[ "${choice}" == "0" ]]; then
            ensure_static_site
            echo ""
            return 0
        elif [[ "${choice}" == "${i}" ]]; then
            if ! read -p "请输入网址（含 https://）: " url </dev/tty; then
                echo -e "${red}输入已结束，放弃配置${plain}" >&2
                return 1
            fi
            # 削掉尾斜杠：浏览器地址栏复制出来的地址天然带它，而 Caddy 的
            # upstream 连一个尾斜杠都不接受。必须在这里规范化——check_mask_site
            # 里那次规范化只作用于它自己的局部变量，传给 write_caddyfile 的是
            # 下面 echo 出去的这一份。
            url="${url%/}"
        elif [[ "${choice}" =~ ^[0-9]+$ ]] && [[ "${choice}" -ge 1 ]] && [[ "${choice}" -lt "${i}" ]]; then
            url="${MASK_SITES[$((choice - 1))]}"
        else
            echo -e "${red}无效的序号${plain}" >&2
            continue
        fi

        echo -e "正在从本机测试 ${url} 是否可反代…" >&2
        if reason=$(check_mask_site "${url}"); then
            # check_mask_site 通过时不输出任何东西：反代目标只能是用户选中
            # 的这个地址本身（它已经被证实在根路径上直接 2xx、不跳转），
            # Caddy 的 upstream 也只接受 scheme/host/port。
            echo -e "${green}可用${plain}" >&2
            echo "${url}"
            return 0
        fi
        # 不静默回退到静态页：那会让用户以为伪装成了某站，实际不是。
        echo -e "${red}${url} 不可用（${reason}），请另选${plain}" >&2
    done
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
        # header_down Location 是伪装的防御纵深。check_mask_site 已经拒掉了
        # 根路径与未知路径上会跳转的站，但它只探这两个路径，站点策略还会变。
        # 上游一旦在某个路径上回了带绝对地址的 Location，Caddy 默认原样转发
        # 给访客（caddyserver/caddy#1011、#5141），访客浏览器就跳到真实的
        # 伪装源，地址栏从用户的域名变成别人的域名——伪装当场败露。
        #
        # 把任何绝对地址的 host 一律换成 {host}（访客请求里的域名），路径
        # 保持不变；相对地址（Location: /foo）本来就不带域名，正则不匹配、
        # 不动它。不只匹配上游那一个域名：真实事故里 www.wikipedia.org 跳的
        # 是 en.wikipedia.org，只盯上游域名根本拦不住。
        #
        # 代价是上游若真想把访客送去第三方站点，重写后会变成本域的同路径
        # 请求、再次被反代、再次跳转，形成重定向回环。这是刻意接受的取舍：
        # 一个看起来坏掉的页面，远好过一条把整套伪装当场证伪的响应。
        mask_block="        reverse_proxy ${mask_url} {
            header_up Host {upstream_hostport}
            header_down Location \"^https?://[^/]+(/.*)?\$\" \"https://{host}\$1\"
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

# /var/lib/caddy 整体不存在是完全正常的状态（用户自己装的 Caddy 数据目录
# 可能在 /root/.local/share/caddy，或者 Caddy 还没建立过证书存储）。不加
# 这道判断的话，set -euo pipefail 下 find 报错会让本次运行以失败告终，
# systemd 把它记成 failed——一个正常状态被报成故障。
[[ -d /var/lib/caddy ]] || exit 0

# 先按 mtime 定位最新的那个证书目录，再从**同一个目录**里取 cert 与 key。
# 两次独立的 `find | head -1` 是个陷阱：Caddy 某次续期从 Let's Encrypt 回落
# 到 ZeroSSL 时，certificates/ 下会出现第二个 CA 目录，两次 find 可能各自
# 命中不同目录——轻则 cert 与 key 配不上对，重则一直命中旧目录，cmp 每次
# 都判"没变"，/root/cert/ 下的证书就此冻结到过期。这与 spec §8 记的那次
# "acme.sh 续期成功但 nginx 从没用上新证书"是同一个形状，只是换了套机制。
certdir=$(find /var/lib/caddy -type f -name "${domain}.crt" -printf '%T@ %h\n' 2>/dev/null \
            | sort -rn | head -1 | cut -d' ' -f2-)
if [[ -n "${certdir}" && -f "${certdir}/${domain}.crt" && -f "${certdir}/${domain}.key" ]]; then
    src="${certdir}/${domain}.crt"
    key="${certdir}/${domain}.key"
    changed=0
    cmp -s "${src}" /root/cert/fullchain.cer || changed=1
    cmp -s "${key}" "/root/cert/${domain}.key" || changed=1
    if [[ "${changed}" == "1" ]]; then
        install -m 644 "${src}" /root/cert/fullchain.cer
        install -m 600 "${key}" "/root/cert/${domain}.key"
        logger -t a-ui-cert-sync "证书已同步到 /root/cert"
    fi
fi

# 有效期检查必须无条件跑，不能只在"这次真的复制了"时才跑：上面那种
# 冻结场景的表征恰恰是**什么都没变**，只靠复制路径上的检查永远发现不了。
# 只告警不做任何补救——续期是 Caddy 的事，这个脚本没有权限替它决定。
if command -v openssl >/dev/null 2>&1 && [[ -s /root/cert/fullchain.cer ]]; then
    if ! openssl x509 -in /root/cert/fullchain.cer -checkend 86400 -noout >/dev/null 2>&1; then
        logger -t a-ui-cert-sync "警告：/root/cert/fullchain.cer 将在 24 小时内过期或已过期，请检查 Caddy 的证书续期状态"
    fi
fi
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
    if ! systemctl enable --now a-ui-cert-sync.timer 2>/dev/null; then
        echo -e "${yellow}警告：证书同步定时器启用失败，证书续期后不会自动同步到 /root/cert${plain}"
    fi
    # 立刻跑一次，别等第一个周期。返回码要如实透出：调用方据此决定要不要
    # 把 /root/cert/ 下的路径写进面板的"新建入站默认值"——同步没成功却照写，
    # 等于给管理员留了一个必然会被入站校验拒绝的默认值。
    systemctl start a-ui-cert-sync.service || return 1
    return 0
}

# 检测 acme.sh 是否也在管理刚配置好的这个域名。从 x-ui + acme.sh 迁移过来的
# 用户很常见：acme.sh 的 cron 会继续为该域名续期，而它的 reloadcmd 十有八九
# 指向刚被本向导停用的 nginx——续期请求本身（http-01/dns-01）与 nginx 是否在跑
# 无关，通常仍会成功，但 reloadcmd 那一步必然失败；同时形成两套 ACME 客户端
# 管理同一域名的冗余状态。只提示，绝不自动改用户的 crontab 或 acme.sh 配置——
# 那是用户的资产，脚本没有权限替用户做这个决定。
check_acme_conflict() {
    local domain="$1"
    local acme_dir="${HOME:-/root}/.acme.sh"
    [[ -d "${acme_dir}" ]] || return 0
    local domain_conf
    domain_conf=$(find "${acme_dir}" -maxdepth 2 -type f -name "${domain}.conf" 2>/dev/null | head -1)
    [[ -z "${domain_conf}" ]] && return 0

    echo -e ""
    echo -e "${yellow}检测到 acme.sh 也在管理 ${domain} 的证书（${domain_conf}）${plain}"
    echo -e "${yellow}证书现已改由 Caddy 自动申请与续期，acme.sh 继续为同一域名续期已是多余，${plain}"
    echo -e "${yellow}且它的 reloadcmd 若指向了刚被停用的服务，续期时会报错。任选其一处理：${plain}"
    echo -e "${yellow}  1) 停掉 acme.sh 对这个域名的续期：crontab -e 删掉含 acme.sh 的那一行${plain}"
    echo -e "${yellow}  2) 或保留 acme.sh，把它的 reloadcmd 改成重载 Caddy：${plain}"
    echo -e "${yellow}     ${acme_dir}/acme.sh --install-cert -d ${domain} --reloadcmd \"systemctl reload caddy\"${plain}"
    echo -e "${yellow}本向导不会自动修改你的 crontab 或 acme.sh 配置。${plain}"
}

domain_flow() {
    local force="$1"

    # 面板当前端口：force 重跑时若中途放弃，面板可能已经在 127.0.0.1 上
    # 够不着了，救援提示里的 SSH 隧道要带真实端口。探测不到就留空串，
    # print_rescue_hint 会退化成"先用菜单第 7 项查端口"，不编一个数字。
    local detected_port=""
    detected_port=$(current_panel_port) || detected_port=""

    local domain
    read -p "请输入你的域名: " domain
    if [[ -z "${domain}" ]]; then
        echo -e "${red}域名不能为空${plain}"
        print_rescue_hint_if_force "${force}" "${detected_port}"
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
        if [[ x"${go_on}" != x"y" && x"${go_on}" != x"Y" ]]; then
            print_rescue_hint_if_force "${force}" "${detected_port}"
            return 1
        fi
    fi

    local occupant
    occupant=$(port_user 80)
    [[ -z "${occupant}" ]] && occupant=$(port_user 443)
    if [[ -n "${occupant}" ]]; then
        if ! handle_existing_web_server "${occupant}"; then
            print_rescue_hint_if_force "${force}" "${detected_port}"
            return 1
        fi
    fi

    if ! install_caddy; then
        restart_own_caddy_if_stopped
        print_stopped_svc_rollback_hint
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

    # 用户在选伪装站时改主意（输入 q、或 SSH 断线导致 read 失败）同样要
    # 走完整的收尾：此时 handle_existing_web_server 可能已经把 Caddy 停了，
    # install_caddy 对"已安装"的情形只 enable、不 start，80/443 上是真的
    # 什么都没有。只打一句"已取消"就返回，会留下一台面板从外网彻底不可达
    # 的机器，而用户以为自己什么都没做。
    local mask_url
    if ! mask_url=$(choose_mask_site); then
        restart_own_caddy_if_stopped
        print_stopped_svc_rollback_hint
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

    local basepath panel_port
    basepath="/$(gen_random_path)/"
    panel_port=54321

    if ! write_caddyfile "${domain}" "${basepath}" "${panel_port}" "${mask_url}"; then
        restart_own_caddy_if_stopped
        print_stopped_svc_rollback_hint
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi
    # write_caddyfile 末尾的 systemctl restart caddy 已经成功，80/443 上重新
    # 有人监听了。先前停用的若正是我们自己的 Caddy，这个标记必须清掉，
    # 否则后面的失败分支会打印"caddy 已被停用、80/443 上没有任何服务在
    # 监听"这句与事实相反的提示。
    [[ "${stopped_web_svc}" == "caddy" ]] && stopped_web_svc=""

    wait_for_cert "${domain}" || {
        if [[ "${force}" == "force" ]]; then
            # 重跑场景里"为避免把你锁在面板外，不修改监听地址"是主动的
            # 错误安慰：webListen 在更早一次成功的配置里就已经是 127.0.0.1，
            # 这次不动它并不代表面板还能从外网进得去。
            echo -e "${yellow}证书未就绪，本次没有改动面板监听地址${plain}"
            echo -e "${yellow}注意这是一次重新配置，面板此前很可能已经只监听 127.0.0.1；${plain}"
            echo -e "${yellow}Caddy 现在用的还是上一次的配置，若面板打不开请用下面的命令恢复${plain}"
        else
            echo -e "${yellow}证书未就绪，为避免把你锁在面板外，不修改面板监听地址${plain}"
        fi
        echo -e "${yellow}修好之后运行 a-ui 选择「配置域名与伪装站」重试${plain}"
        echo -e "${yellow}排查：journalctl -u caddy -n 50${plain}"
        print_stopped_svc_rollback_hint
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    }

    if ! install_cert_sync "${domain}"; then
        echo -e "${yellow}警告：证书同步到 /root/cert/ 未成功${plain}"
        echo -e "${yellow}排查：systemctl status a-ui-cert-sync.service${plain}"
    fi

    # 同步是否真的成功，要看文件而不是看返回码：这两个路径一旦写进面板的
    # "新建入站默认值"，管理员此后新建任何 TLS 入站都会被
    # ValidateInboundReplacing 用一个他从没输入过的路径拒掉。宁可让他自己
    # 填，也不留一个必然是错的默认值。
    local cert_args=()
    if [[ -s /root/cert/fullchain.cer && -s "/root/cert/${domain}.key" ]]; then
        cert_args=(-cert-file /root/cert/fullchain.cer -key-file "/root/cert/${domain}.key")
    else
        echo -e "${yellow}警告：/root/cert/ 下没有可用的证书文件，本次不写入「新建入站的默认证书路径」${plain}"
        echo -e "${yellow}Caddy 自己的证书不受影响，面板与伪装站照常工作；${plain}"
        echo -e "${yellow}要新建 TLS 入站时请在面板里自行填写证书路径${plain}"
    fi

    local force_args=()
    [[ "${force}" == "force" ]] && force_args=(-force)

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode caddy -domain "${domain}" \
            -basepath "${basepath}" -listen 127.0.0.1 -port "${panel_port}" \
            "${cert_args[@]}" "${force_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}写入面板配置失败：${out}${plain}"
        # 这次调用本身多半没能把 webListen 改成 127.0.0.1（bootstrap 按
        # 「先写完其余字段、最后才写监听地址」的顺序执行，中途出错时 -listen
        # 那一步大概率没跑到）。但如果这是一次覆盖已有配置的重装，webListen
        # 完全可能在更早一次成功的配置里就已经是 127.0.0.1 了——这里没有
        # 办法区分"从未改过"和"之前改过、这次没改动"，所以两种情况都打印，
        # 让用户自己判断要不要用，比赌一次判断失误、把救援命令吞掉安全。
        print_rescue_hint "${panel_port}"
        return 1
    fi
    systemctl restart a-ui
    if ! wait_for_panel_alive "${panel_port}" "${basepath}"; then
        echo -e "${red}面板重启后探活失败，127.0.0.1:${panel_port} 没有响应${plain}"
        echo -e "${red}面板配置已写入，但进程可能启动后又退出了（常见原因：证书路径不存在或不可读）${plain}"
        echo -e "${yellow}排查：journalctl -u a-ui -n 50${plain}"
        # 走到这里 bootstrap 已经成功返回，webListen 已经被改成了
        # 127.0.0.1——这是整个改造里最需要这两条命令的时刻，不能只给
        # journalctl 排查提示。
        print_rescue_hint "${panel_port}"
        return 1
    fi
    # 端口必须传：域名分支是唯一把面板锁到 127.0.0.1 的分支，也是唯一
    # 端口百分之百确定的（就是上面这个 panel_port）。不传的话救援提示会
    # 退化成"先用菜单第 7 项查端口"，恰恰在最需要一条可直接复制的 SSH
    # 隧道命令的场景里给出了降级版本。
    print_result "${out}" "caddy" "${panel_port}"
    check_acme_conflict "${domain}"

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
# 也是本地开发时验证向导本身的入口。传 "force" 跳过 setup_wizard 的幂等
# 探测并覆盖已有配置——从菜单主动触发就是明确要重新配置，不该被幂等
# 判断挡住。
if [[ "$1" == "--wizard-only" ]]; then
    # 这条入口不走 install_base()，但向导本身依赖 jq（print_result 解析
    # bootstrap 的 JSON 输出，缺了它"面板地址"整行会静默不打印——用户刚把
    # 面板挪到一个随机路径上却拿不到地址）与 openssl（check_reality_target
    # 的四项判据全靠它，缺了它必然报"目标不满足要求"，理由完全是假的）。
    command -v jq >/dev/null 2>&1 && command -v openssl >/dev/null 2>&1 || install_base
    # 退出码必须如实透出：a-ui 菜单的「配置域名与伪装站」据此判断向导是不是
    # 真的跑完了。恒返回 0 会让"中途放弃 + 面板已在 127.0.0.1 + Caddy 被停用"
    # 这种状态一路静默回到主菜单。
    setup_wizard force
    exit $?
fi

echo -e "${green}开始安装${plain}"
install_base
install_a-ui $1
