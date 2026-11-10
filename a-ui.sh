#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

#consts for log check and clear,unit:M
declare -r DEFAULT_LOG_FILE_DELETE_TRIGGER=35

# consts for geo update
PATH_FOR_GEO_IP='/usr/local/a-ui/bin/geoip.dat'
PATH_FOR_CONFIG='/usr/local/a-ui/bin/config.json'
PATH_FOR_GEO_SITE='/usr/local/a-ui/bin/geosite.dat'
URL_FOR_GEO_IP='https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat'
URL_FOR_GEO_SITE='https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat'

#Add some basic function here
function LOGD() {
    echo -e "${yellow}[DEG] $* ${plain}"
}

function LOGE() {
    echo -e "${red}[ERR] $* ${plain}"
}

function LOGI() {
    echo -e "${green}[INF] $* ${plain}"
}
# check root
[[ $EUID -ne 0 ]] && LOGE "错误:  必须使用root用户运行此脚本!\n" && exit 1

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
    LOGE "未检测到系统版本，请联系脚本作者！\n" && exit 1
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
        LOGE "请使用 CentOS 7 或更高版本的系统！\n" && exit 1
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        LOGE "请使用 Ubuntu 16 或更高版本的系统！\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        LOGE "请使用 Debian 8 或更高版本的系统！\n" && exit 1
    fi
fi

confirm() {
    if [[ $# > 1 ]]; then
        echo && read -p "$1 [默认$2]: " temp
        if [[ x"${temp}" == x"" ]]; then
            temp=$2
        fi
    else
        read -p "$1 [y/n]: " temp
    fi
    if [[ x"${temp}" == x"y" || x"${temp}" == x"Y" ]]; then
        return 0
    else
        return 1
    fi
}

confirm_restart() {
    confirm "是否重启面板，重启面板也会重启 xray" "y"
    if [[ $? == 0 ]]; then
        restart
    else
        show_menu
    fi
}

before_show_menu() {
    echo && echo -n -e "${yellow}按回车返回主菜单: ${plain}" && read temp
    show_menu
}

install() {
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh)
    if [[ $? == 0 ]]; then
        if [[ $# == 0 ]]; then
            start
        else
            start 0
        fi
    fi
}

update() {
    confirm "本功能会强制重装当前最新版，数据不会丢失，是否继续?" "n"
    if [[ $? != 0 ]]; then
        LOGE "已取消"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 0
    fi
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh)
    if [[ $? == 0 ]]; then
        LOGI "更新完成，已自动重启面板 "
        exit 0
    fi
}

# 卸载有域名安装时 install.sh 的 install_cert_sync 装的三样东西。/root/cert/ 下的证书
# 不动：菜单 16 的 acme.sh 申请也写那里，分不清哪些是谁的。不用 disable --now：
# 本脚本声称支持自带 systemd 219 的 CentOS 7，它还不认识 --now。
remove_cert_sync() {
    local unit_dir=/etc/systemd/system found=0
    if [[ -f "${unit_dir}/a-ui-cert-sync.timer" || -f "${unit_dir}/a-ui-cert-sync.service" ]]; then
        systemctl stop a-ui-cert-sync.timer a-ui-cert-sync.service >/dev/null 2>&1
        systemctl disable a-ui-cert-sync.timer >/dev/null 2>&1
        rm -f "${unit_dir}/a-ui-cert-sync.timer" "${unit_dir}/a-ui-cert-sync.service"
        systemctl daemon-reload
        found=1
    fi
    if [[ -f /usr/local/bin/a-ui-cert-sync ]]; then
        rm -f /usr/local/bin/a-ui-cert-sync
        found=1
    fi
    [[ ${found} -eq 1 ]] && LOGI "已移除证书同步定时器 a-ui-cert-sync（/root/cert/ 下的证书文件保留）"
    return 0
}

# 从 Caddyfile 里删掉面板自己写入的站点，Caddy 软件包与其它站点一律不碰（卸载 Caddy
# 本身按设计只询问、不默认执行）。install.sh 的 write_caddyfile 用 BEGIN/END 两行包住
# 它写的站点块，只删这两行之间的内容；先备份，删完用 caddy validate 校验，不通过就
# 原样恢复，通过了才 reload。更早版本的向导没有写标记，那样的文件认不准哪部分是
# 面板写的，只提示、不改。参数是卸载前读到的面板端口（可为空），只用于这条提示。
remove_caddy_site() {
    local panel_port="$1" cf=/etc/caddy/Caddyfile
    [[ -f "${cf}" ]] || return 0
    if ! grep -q '^# BEGIN a-ui managed site' "${cf}"; then
        if grep -qE "reverse_proxy[[:space:]]+127\.0\.0\.1:${panel_port:-[0-9]+}([^0-9]|\$)" "${cf}"; then
            LOGD "${cf} 里有反代到本机面板端口的站点，像是旧版安装向导写入的。它没有面板的标记，为免误删你自己的配置，未做改动"
            LOGD "如不再需要：编辑 ${cf} 删掉该站点块，再执行 systemctl reload caddy"
        fi
        return 0
    fi
    local new backup
    if ! new=$(awk '
        /^# BEGIN a-ui managed site/ { if (skip) bad = 1; skip = 1; next }
        /^# END a-ui managed site/   { if (!skip) bad = 1; skip = 0; next }
        !skip { print }
        END { if (skip || bad) exit 3 }' "${cf}"); then
        LOGE "${cf} 里面板站点的 BEGIN/END 标记不成对，为免误删其它站点，未做改动，请手动处理"
        return 1
    fi
    backup="${cf}.a-ui-uninstall-$(date +%Y%m%d-%H%M%S).bak"
    if ! cp -p "${cf}" "${backup}"; then
        LOGE "备份 ${cf} 失败，未移除其中的面板站点"
        return 1
    fi
    printf '%s\n' "${new}" >"${cf}"
    if command -v caddy >/dev/null 2>&1 && ! caddy validate --config "${cf}" >/dev/null 2>&1; then
        cp -p "${backup}" "${cf}"
        LOGE "移除面板站点后 ${cf} 未通过 caddy validate，已恢复原文件，请手动处理"
        return 1
    fi
    if systemctl is-active --quiet caddy 2>/dev/null && ! systemctl reload caddy; then
        LOGE "Caddy 重新加载失败，请用 systemctl status caddy 查看"
    fi
    LOGI "已从 ${cf} 移除面板写入的站点（原文件备份在 ${backup}）"
    LOGI "Caddy 软件包本身未卸载；如不再需要：systemctl stop caddy && systemctl disable caddy，再用系统的包管理器卸载"
    local old_backup
    old_backup=$(ls -t /root/caddy-backup-*.tar.gz 2>/dev/null | head -n 1)
    [[ -n "${old_backup}" ]] && LOGI "安装向导停用原有 Caddy 时备份过它当时的配置：${old_backup}，如需找回原来的站点可从中取回"
    return 0
}

uninstall() {
    confirm "确定要卸载面板吗,xray 也会卸载?（面板写入的 Caddy 站点、证书同步定时器与定时任务也会一并移除，Caddy 本身保留）" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    # 面板端口要在删库之前读，只用于识别旧版向导写的 Caddyfile、给出提示
    local panel_port_for_caddy
    panel_port_for_caddy=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null | awk '/^Port:/ {print $2; exit}')
    systemctl stop a-ui
    systemctl disable a-ui
    rm /etc/systemd/system/a-ui.service -f
    systemctl daemon-reload
    systemctl reset-failed
    rm /etc/a-ui/ -rf
    rm /usr/local/a-ui/ -rf
    # install.sh 的 xray 备份目录是 /usr/local/a-ui/ 的兄弟目录，上面那行删不到它。
    # 安装正常走完时 restore_or_install_xray 会自己清掉，但中途被 Ctrl-C 或信号
    # 打断就会留下约 66MB 残留——它当初特意从 /tmp 挪到 /usr/local 来躲开 tmpfs，
    # 代价正是不再重启即清，所以卸载时要顺手扫掉。下载暂存目录（fetch_release）同理。
    rm -rf /usr/local/a-ui-xray-backup-*
    rm -rf /usr/local/a-ui-update-*
    # 菜单 17 加的定时任务（命令指向已不存在的面板）
    cron_drop_lines "a-ui geo" || LOGE "移除定时更新 geo 任务失败，请用 crontab -e 手动删除含 a-ui geo 的行"
    cron_drop_lines "a-ui clear" || LOGE "移除定时清除日志任务失败，请用 crontab -e 手动删除含 a-ui clear 的行"
    remove_cert_sync
    remove_caddy_site "${panel_port_for_caddy}"

    echo ""
    echo -e "卸载成功，如果你想删除此脚本，则退出脚本后运行 ${green}rm /usr/bin/a-ui -f${plain} 进行删除"
    echo ""

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

reset_user() {
    confirm "确定要将用户名和密码重置为 admin，并关闭两步验证吗" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    if ! a_ui_setting -username admin -password admin; then
        LOGE "重置用户名和密码失败，面板账号保持原样"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    # 单独一条命令，并且容忍失败：本脚本永远来自 main 分支，而二进制可能
    # 是回退后的旧版本，不认识 -disable2fa。与上一条合并的话，旧二进制会
    # 因一个未知参数整条报错，连账号密码都重置不了。
    /usr/local/a-ui/a-ui setting -disable2fa >/dev/null 2>&1 || true
    echo -e "用户名和密码已重置为 ${green}admin${plain}，两步验证已关闭，现在请重启面板"
    echo -e "${yellow}admin/admin 是公开的默认口令，登录后请立即在「面板设置 → 账号与密码」里改掉${plain}"
    confirm_restart
}

# 执行 /usr/local/a-ui/a-ui setting 并如实判断成败（判据与 install.sh 的 a_ui_setting
# 相同）：退出码之外再看输出——本脚本永远来自 main 分支，二进制可能是回退后的旧版本，
# 旧版本写库失败也退出 0、只打印一行 "... failed: ..."。失败时打印输出、返回 1。
a_ui_setting() {
    local out rc
    out=$(/usr/local/a-ui/a-ui setting "$@" 2>&1)
    rc=$?
    if [[ ${rc} -ne 0 || "${out}" == *failed* ]]; then
        echo -e "${red}${out}${plain}"
        return 1
    fi
    return 0
}

# 端口上是否已有 TCP 监听，与 install.sh 的 port_in_use 相同：ss 优先（不用 -H，CentOS 7
# 不认识），没有就退回 netstat，两者都没有时无从判断、按空闲处理。
port_in_use() {
    local port="$1" listeners
    if command -v ss >/dev/null 2>&1; then
        listeners=$(ss -ltn 2>/dev/null)
    elif command -v netstat >/dev/null 2>&1; then
        listeners=$(netstat -ltn 2>/dev/null)
    else
        return 1
    fi
    echo "${listeners}" | awk -v p="${port}" '{ n = split($4, a, ":"); if (a[n] == p) found = 1 } END { exit !found }'
}

# 面板此刻是否经 Caddy 反代对外（有域名安装）：Caddyfile 把请求反代到面板当前端口，
# 并且面板只监听回环（只能经 Caddy 访问）或 Caddy 正在运行。只监听回环、但没有 Caddy
# 反代（只走 SSH 隧道）不算——那种情况下改端口不会把人锁在外面；菜单 19 恢复直连之后
# 也不算（监听所有 IP、Caddy 已停用）。参数是 a-ui setting -show 的输出。
panel_behind_caddy() {
    local listen port
    listen=$(echo "$1" | awk '/^Listen:/ {print $2; exit}')
    port=$(echo "$1" | awk '/^Port:/ {print $2; exit}')
    [[ "${port}" =~ ^[0-9]+$ && -f /etc/caddy/Caddyfile ]] || return 1
    grep -qE "reverse_proxy[[:space:]]+(127\.0\.0\.1|localhost):${port}([^0-9]|\$)" /etc/caddy/Caddyfile || return 1
    case "${listen}" in
    127.* | ::1 | localhost) return 0 ;;
    esac
    systemctl is-active --quiet caddy 2>/dev/null
}

reset_config() {
    # 有域名安装时，重置会把监听地址、端口、根路径一并恢复成默认值：面板改为在所有 IP 的
    # 54321 端口、根路径 / 上直接对外，Caddy 的反代随之失效。确认之前先说清楚。
    local show_out
    if show_out=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null) && panel_behind_caddy "${show_out}"; then
        LOGD "注意：面板当前经 Caddy 反代访问（有域名安装）。重置后面板会改为在所有 IP 的 54321 端口、根路径 / 上直接对外，域名访问随之失效；如需重新隐藏面板，重置后请运行菜单 18「配置域名与伪装站」"
    fi
    confirm "确定要重置所有面板设置吗，账号数据不会丢失，用户名和密码不会改变，两步验证会被关闭" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    if ! a_ui_setting -reset; then
        LOGE "重置面板设置失败，设置保持原样"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    echo -e "所有面板设置已重置为默认值，现在请重启面板，并使用默认的 ${green}54321${plain} 端口访问面板"
    confirm_restart
}

check_config() {
    info=$(/usr/local/a-ui/a-ui setting -show true)
    if [[ $? != 0 ]]; then
        LOGE "get current settings error,please check logs"
        show_menu
    fi
    LOGI "${info}"
}

set_port() {
    echo && echo -n -e "输入端口号[1-65535]: " && read port
    if [[ -z "${port}" ]]; then
        LOGD "已取消"
        before_show_menu
        return 0
    fi
    # 先校验再交给 a-ui setting：0、超过 65535 的数都能原样落库，面板重启后绑不上端口；
    # 带前导 0 的数会被 Go 的 flag 按八进制解析（010 变成 8，08 直接报错）。
    if [[ ! "${port}" =~ ^[0-9]{1,5}$ ]] || ((10#${port} < 1 || 10#${port} > 65535)); then
        LOGE "端口无效：请输入 1-65535 之间的整数"
        before_show_menu
        return 1
    fi
    port=$((10#${port}))

    local show_out cur_port
    if ! show_out=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null); then
        LOGE "读取面板当前设置失败，未做任何改动"
        before_show_menu
        return 1
    fi
    cur_port=$(echo "${show_out}" | awk '/^Port:/ {print $2; exit}')
    # 有域名安装时面板只经 Caddy 反代访问，install.sh 写进 Caddyfile 的是固定的
    # reverse_proxy 127.0.0.1:<端口>。这里单独改面板端口，Caddy 仍转发到旧端口，管理员
    # 就被锁在面板外。刻意不在这里同步改 Caddyfile：那是两套配置的联动修改，两头各有
    # 失败与回滚，只成功一半就是锁死；菜单 18 的配置向导本来就是同时写 Caddy 与面板
    # 设置、并带完整失败处理的唯一入口。所以拒绝并指路。
    if panel_behind_caddy "${show_out}"; then
        LOGE "面板当前经 Caddy 反代访问（有域名安装），不能在这里单独改端口：Caddy 会继续把请求转发到旧端口 ${cur_port}，面板将无法从域名打开"
        LOGI "要重新配置域名与面板，请用菜单 18「配置域名与伪装站」（会同时更新 Caddy 与面板设置）"
        LOGI "要改为直连访问面板，请先用菜单 19「卸载 Caddy 并恢复面板直连」，再来改端口"
        before_show_menu
        return 1
    fi
    if [[ "${port}" == "${cur_port}" ]]; then
        LOGI "面板已经在使用端口 ${port}，无需修改"
        before_show_menu
        return 0
    fi
    if port_in_use "${port}"; then
        LOGE "端口 ${port} 已被其它程序占用，请换一个"
        before_show_menu
        return 1
    fi
    if ! a_ui_setting -port "${port}"; then
        LOGE "设置端口失败，面板端口保持不变"
        before_show_menu
        return 1
    fi
    echo -e "设置端口完毕，现在请重启面板，并使用新设置的端口 ${green}${port}${plain} 访问面板"
    confirm_restart
}

start() {
    check_status
    if [[ $? == 0 ]]; then
        echo ""
        LOGI "面板已运行，无需再次启动，如需重启请选择重启"
    else
        systemctl start a-ui
        sleep 2
        check_status
        if [[ $? == 0 ]]; then
            LOGI "a-ui 启动成功"
        else
            LOGE "面板启动失败，可能是因为启动时间超过了两秒，请稍后查看日志信息"
        fi
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

stop() {
    check_status
    if [[ $? == 1 ]]; then
        echo ""
        LOGI "面板已停止，无需再次停止"
    else
        systemctl stop a-ui
        sleep 2
        check_status
        if [[ $? == 1 ]]; then
            LOGI "a-ui 与 xray 停止成功"
        else
            LOGE "面板停止失败，可能是因为停止时间超过了两秒，请稍后查看日志信息"
        fi
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

restart() {
    systemctl restart a-ui
    sleep 2
    check_status
    if [[ $? == 0 ]]; then
        LOGI "a-ui 与 xray 重启成功"
    else
        LOGE "面板重启失败，可能是因为启动时间超过了两秒，请稍后查看日志信息"
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

status() {
    systemctl status a-ui -l
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

enable() {
    systemctl enable a-ui
    if [[ $? == 0 ]]; then
        LOGI "a-ui 设置开机自启成功"
    else
        LOGE "a-ui 设置开机自启失败"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

disable() {
    systemctl disable a-ui
    if [[ $? == 0 ]]; then
        LOGI "a-ui 取消开机自启成功"
    else
        LOGE "a-ui 取消开机自启失败"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_log() {
    journalctl -u a-ui.service -e --no-pager -f
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

migrate_v2_ui() {
    /usr/local/a-ui/a-ui v2-ui

    before_show_menu
}

install_bbr() {
    # temporary workaround for installing bbr
    bash <(curl -L -s https://raw.githubusercontent.com/teddysun/across/master/bbr.sh)
    echo ""
    before_show_menu
}

# 新脚本先下到 /usr/bin 下的临时文件、核验后再 mv 过去，不直接对着 /usr/bin/a-ui 写：
#   - 证书必须校验：这是以 root 执行的脚本，不能经一条可被中间人替换的连接下载。
#   - wget -O 会先清空目标再写，下载失败或拿到的是错误页，管理脚本本身就毁了。
#   - 被替换的正是此刻 bash 正在逐行读取的这个文件。原地改写会让它接着读到新
#     文件里错位的内容；mv 换上的是一个新 inode，正在跑的进程继续读旧的那份。
#     临时文件与目标同目录，mv 才是同一文件系统内的原子 rename。
update_shell() {
    local tmp
    tmp=$(mktemp /usr/bin/.a-ui-update.XXXXXX) || {
        LOGE "创建临时文件失败，未做任何改动"
        before_show_menu
        return 1
    }
    if ! wget -O "${tmp}" https://github.com/SienFeng/AetherUI/raw/main/a-ui.sh; then
        rm -f "${tmp}"
        echo ""
        LOGE "下载脚本失败，请检查本机能否连接 Github"
        before_show_menu
        return 1
    fi
    if [[ ! -s "${tmp}" ]] || ! head -n 1 "${tmp}" | grep -qE '^#!.*bash' || ! bash -n "${tmp}" 2>/dev/null; then
        rm -f "${tmp}"
        LOGE "下载到的不是完整的 bash 脚本（空文件、错误页或下载被截断），未做替换"
        before_show_menu
        return 1
    fi
    if ! chmod 755 "${tmp}" || ! mv -f "${tmp}" /usr/bin/a-ui; then
        rm -f "${tmp}"
        LOGE "替换 /usr/bin/a-ui 失败，原脚本保持不变"
        before_show_menu
        return 1
    fi
    LOGI "升级脚本成功，请重新运行脚本" && exit 0
}

# 0: running, 1: not running, 2: not installed
check_status() {
    if [[ ! -f /etc/systemd/system/a-ui.service ]]; then
        return 2
    fi
    temp=$(systemctl status a-ui | grep Active | awk '{print $3}' | cut -d "(" -f2 | cut -d ")" -f1)
    if [[ x"${temp}" == x"running" ]]; then
        return 0
    else
        return 1
    fi
}

check_enabled() {
    temp=$(systemctl is-enabled a-ui)
    if [[ x"${temp}" == x"enabled" ]]; then
        return 0
    else
        return 1
    fi
}

check_uninstall() {
    check_status
    if [[ $? != 2 ]]; then
        echo ""
        LOGE "面板已安装，请不要重复安装"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

check_install() {
    check_status
    if [[ $? == 2 ]]; then
        echo ""
        LOGE "请先安装面板"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

show_status() {
    check_status
    case $? in
    0)
        echo -e "面板状态: ${green}已运行${plain}"
        show_enable_status
        ;;
    1)
        echo -e "面板状态: ${yellow}未运行${plain}"
        show_enable_status
        ;;
    2)
        echo -e "面板状态: ${red}未安装${plain}"
        ;;
    esac
    show_xray_status
}

show_enable_status() {
    check_enabled
    if [[ $? == 0 ]]; then
        echo -e "是否开机自启: ${green}是${plain}"
    else
        echo -e "是否开机自启: ${red}否${plain}"
    fi
}

check_xray_status() {
    count=$(ps -ef | grep "xray-linux" | grep -v "grep" | wc -l)
    if [[ count -ne 0 ]]; then
        return 0
    else
        return 1
    fi
}

show_xray_status() {
    check_xray_status
    if [[ $? == 0 ]]; then
        echo -e "xray 状态: ${green}运行${plain}"
    else
        echo -e "xray 状态: ${red}未运行${plain}"
    fi
}

#this will be an entrance for ssl cert issue
#here we can provide two different methods to issue cert
#first.standalone mode second.DNS API mode
ssl_cert_issue() {
    local method=""
    echo -E ""
    LOGD "******使用说明******"
    LOGI "该脚本提供两种方式实现证书签发,证书安装路径均为/root/cert"
    LOGI "方式1:acme standalone mode,需要保持端口开放"
    LOGI "方式2:acme DNS API mode,需要提供Cloudflare Global API Key"
    LOGI "如域名属于免费域名,则推荐使用方式1进行申请"
    LOGI "如域名非免费域名且使用Cloudflare进行解析使用方式2进行申请"
    read -p "请选择你想使用的方式,输入数字1或者2后回车": method
    LOGI "你所使用的方式为${method}"

    if [ "${method}" == "1" ]; then
        ssl_cert_issue_standalone
    elif [ "${method}" == "2" ]; then
        ssl_cert_issue_by_cloudflare
    else
        LOGE "输入无效,请检查你的输入,脚本将退出..."
        exit 1
    fi
}

install_acme() {
    cd ~
    LOGI "开始安装acme脚本..."
    curl https://get.acme.sh | sh
    if [ $? -ne 0 ]; then
        LOGE "acme安装失败"
        return 1
    else
        LOGI "acme安装成功"
    fi
    return 0
}

#method for standalone mode
ssl_cert_issue_standalone() {
    #check for acme.sh first
    if ! command -v ~/.acme.sh/acme.sh &>/dev/null; then
        install_acme
        if [ $? -ne 0 ]; then
            LOGE "安装 acme 失败，请检查日志"
            exit 1
        fi
    fi
    #install socat second
    if [[ x"${release}" == x"centos" ]]; then
        yum install socat -y
    else
        apt install socat -y
    fi
    if [ $? -ne 0 ]; then
        LOGE "无法安装socat,请检查错误日志"
        exit 1
    else
        LOGI "socat安装成功..."
    fi
    #creat a directory for install cert
    certPath=/root/cert
    if [ ! -d "$certPath" ]; then
        mkdir $certPath
    fi
    #get the domain here,and we need verify it
    local domain=""
    read -p "请输入你的域名:" domain
    LOGD "你输入的域名为:${domain},正在进行域名合法性校验..."
    #here we need to judge whether there exists cert already
    local currentCert=$(~/.acme.sh/acme.sh --list | grep ${domain} | wc -l)
    if [ ${currentCert} -ne 0 ]; then
        local certInfo=$(~/.acme.sh/acme.sh --list)
        LOGE "域名合法性校验失败,当前环境已有对应域名证书,不可重复申请,当前证书详情:"
        LOGI "$certInfo"
        exit 1
    else
        LOGI "域名合法性校验通过..."
    fi
    #get needed port here
    local WebPort=80
    read -p "请输入你所希望使用的端口,如回车将使用默认80端口:" WebPort
    if [[ ${WebPort} -gt 65535 || ${WebPort} -lt 1 ]]; then
        LOGE "你所选择的端口${WebPort}为无效值,将使用默认80端口进行申请"
    fi
    LOGI "将会使用${WebPort}进行证书申请,请确保端口处于开放状态..."
    #NOTE:This should be handled by user
    #open the port and kill the occupied progress
    ~/.acme.sh/acme.sh --set-default-ca --server letsencrypt
    ~/.acme.sh/acme.sh --issue -d ${domain} --standalone --httpport ${WebPort}
    if [ $? -ne 0 ]; then
        LOGE "证书申请失败,原因请参见报错信息"
        rm -rf ~/.acme.sh/${domain}
        exit 1
    else
        LOGI "证书申请成功,开始安装证书..."
    fi
    #install cert
    ~/.acme.sh/acme.sh --installcert -d ${domain} --ca-file /root/cert/ca.cer \
        --cert-file /root/cert/${domain}.cer --key-file /root/cert/${domain}.key \
        --fullchain-file /root/cert/fullchain.cer

    if [ $? -ne 0 ]; then
        LOGE "证书安装失败,脚本退出"
        rm -rf ~/.acme.sh/${domain}
        exit 1
    else
        LOGI "证书安装成功,开启自动更新..."
    fi
    ~/.acme.sh/acme.sh --upgrade --auto-upgrade
    if [ $? -ne 0 ]; then
        LOGE "自动更新设置失败,脚本退出"
        ls -lah cert
        chmod 755 $certPath
        exit 1
    else
        LOGI "证书已安装且已开启自动更新,具体信息如下"
        ls -lah cert
        chmod 755 $certPath
    fi

}

#method for DNS API mode
ssl_cert_issue_by_cloudflare() {
    echo -E ""
    LOGD "******使用说明******"
    LOGI "该脚本将使用Acme脚本申请证书,使用时需保证:"
    LOGI "1.知晓Cloudflare 注册邮箱"
    LOGI "2.知晓Cloudflare Global API Key"
    LOGI "3.域名已通过Cloudflare进行解析到当前服务器"
    LOGI "4.该脚本申请证书默认安装路径为/root/cert目录"
    confirm "我已确认以上内容[y/n]" "y"
    if [ $? -eq 0 ]; then
        install_acme
        if [ $? -ne 0 ]; then
            LOGE "无法安装acme,请检查错误日志"
            exit 1
        fi
        CF_Domain=""
        CF_GlobalKey=""
        CF_AccountEmail=""
        certPath=/root/cert
        if [ ! -d "$certPath" ]; then
            mkdir $certPath
        fi
        LOGD "请设置域名:"
        read -p "Input your domain here:" CF_Domain
        LOGD "你的域名设置为:${CF_Domain},正在进行域名合法性校验..."
        #here we need to judge whether there exists cert already
        local currentCert=$(~/.acme.sh/acme.sh --list | grep ${CF_Domain} | wc -l)
        if [ ${currentCert} -ne 0 ]; then
            local certInfo=$(~/.acme.sh/acme.sh --list)
            LOGE "域名合法性校验失败,当前环境已有对应域名证书,不可重复申请,当前证书详情:"
            LOGI "$certInfo"
            exit 1
        else
            LOGI "域名合法性校验通过..."
        fi
        # Global API Key 能操作整个 Cloudflare 账户：输入不回显，读完也不打印——终端
        # 回滚、录屏、会话日志都会把它留下来。
        LOGD "请设置API密钥（输入时不显示）:"
        read -rs -p "Input your key here:" CF_GlobalKey
        echo ""
        LOGD "已读取API密钥"
        LOGD "请设置注册邮箱:"
        read -p "Input your email here:" CF_AccountEmail
        LOGD "你的注册邮箱为:${CF_AccountEmail}"
        ~/.acme.sh/acme.sh --set-default-ca --server letsencrypt
        if [ $? -ne 0 ]; then
            LOGE "修改默认CA为Lets'Encrypt失败,脚本退出"
            exit 1
        fi
        export CF_Key="${CF_GlobalKey}"
        export CF_Email=${CF_AccountEmail}
        ~/.acme.sh/acme.sh --issue --dns dns_cf -d ${CF_Domain} -d *.${CF_Domain} --log
        if [ $? -ne 0 ]; then
            LOGE "证书签发失败,脚本退出"
            rm -rf ~/.acme.sh/${CF_Domain}
            exit 1
        else
            LOGI "证书签发成功,安装中..."
        fi
        ~/.acme.sh/acme.sh --installcert -d ${CF_Domain} -d *.${CF_Domain} --ca-file /root/cert/ca.cer \
            --cert-file /root/cert/${CF_Domain}.cer --key-file /root/cert/${CF_Domain}.key \
            --fullchain-file /root/cert/fullchain.cer
        if [ $? -ne 0 ]; then
            LOGE "证书安装失败,脚本退出"
            rm -rf ~/.acme.sh/${CF_Domain}
            exit 1
        else
            LOGI "证书安装成功,开启自动更新..."
        fi
        ~/.acme.sh/acme.sh --upgrade --auto-upgrade
        if [ $? -ne 0 ]; then
            LOGE "自动更新设置失败,脚本退出"
            ls -lah cert
            chmod 755 $certPath
            exit 1
        else
            LOGI "证书已安装且已开启自动更新,具体信息如下"
            ls -lah cert
            chmod 755 $certPath
        fi
    else
        show_menu
    fi
}

#add for cron jobs,including sync geo data,check logs and restart a-ui
cron_jobs() {
    clear
    echo -e "
  ${green}定时任务管理${plain}
  ${green}0.${plain}  返回主菜单
  ${green}1.${plain}  开启定时更新geo
  ${green}2.${plain}  关闭定时更新geo
  ${green}3.${plain}  开启定时删除xray日志
  ${green}4.${plain}  关闭定时删除xray日志
  "
    echo && read -p "请输入选择 [0-4]: " num
    case "${num}" in
    0)
        show_menu
        ;;
    1)
        enable_auto_update_geo
        ;;
    2)
        disable_auto_update_geo
        ;;
    3)
        enable_auto_clear_log
        ;;
    4)
        disable_auto_clear_log
        ;;
    *)
        LOGE "请输入正确的数字 [0-4]"
        ;;
    esac
}

#update geo data
# 新数据先下到临时目录，用真实 xray 校验通过后才替换并重启面板；任何一步失败
# 都保留现有文件、不重启，返回非 0。
#
# 原先的写法是 curl -s -L 直接覆盖：不带 -f 时 GitHub 回错误页（限流、5xx）
# curl 照样退出 0，错误页被当成 dat 写进 bin/，紧接着的重启让 xray 加载不了
# geo 数据而起不来——整机节点断网；面板进程本身没有退出，a-ui.service 的
# Restart=on-failure 也救不了。定时任务每两天在凌晨跑一次，正是没人看着的时候。
#
# 替换前的校验拿的是 bin/config.json，而它可能是旧的：配置热更新（tryHotApply）
# 只经 gRPC 把改动下发进正在跑的核心，不重写这个文件，它只在核心整进程启动时才
# 重新生成。热更新加进去的分流规则若引用了新数据里没有的分类，这份旧文件校验
# 不出来，重启后 xray 照样起不来。所以替换、重启之后，等面板重新生成
# bin/config.json，再用它和新数据校验一次；只有在旧数据能通过、新数据通不过时
# 才认定是新数据的问题，换回旧数据再重启一次。没有 bin/config.json 时（面板还
# 没起过核心）没有什么能是旧的，跳过这一步。
#
# 本函数必须保持自包含：update_geo_script_test.go 把它原样抽出来单独运行。
update_geo() {
    local bin_dir install_dir xray_bin tmp_dir f
    bin_dir=$(dirname "${PATH_FOR_GEO_SITE}")
    install_dir=$(dirname "${bin_dir}")
    xray_bin=$(ls "${bin_dir}"/xray-linux-* 2>/dev/null | head -1)
    if [[ -z "${xray_bin}" ]]; then
        LOGE "找不到 xray 核心，无法校验新的 geo 数据，未做任何改动"
        return 1
    fi

    # 不用 /tmp：两个文件合计近 30MB，systemd 发行版的 /tmp 常是内存 tmpfs
    # （与 install.sh 的 backup_xray_assets 同理）；放在安装目录旁边还能保证
    # 下面的 mv 是同一文件系统内的原子 rename。
    tmp_dir=$(mktemp -d "${install_dir}/.geo-update-XXXXXX") || {
        LOGE "创建临时目录失败，未做任何改动"
        return 1
    }
    if ! curl -fsSL --retry 2 -o "${tmp_dir}/geoip.dat" "${URL_FOR_GEO_IP}" ||
        ! curl -fsSL --retry 2 -o "${tmp_dir}/geosite.dat" "${URL_FOR_GEO_SITE}"; then
        rm -rf "${tmp_dir}"
        LOGE "下载 geo 数据失败，保留现有文件，未重启面板"
        return 1
    fi

    # 配置里可能还引用了 bin/ 下别的数据文件（ext:xxx.dat），校验时换了资源
    # 目录就找不到它们，会把好数据误判成坏的，所以链接进来。
    for f in "${bin_dir}"/*.dat; do
        case "$(basename "${f}")" in
        geoip.dat | geosite.dat) ;;
        *) ln -s "${f}" "${tmp_dir}/" 2>/dev/null ;;
        esac
    done

    # 用当前配置校验，连「新数据删掉了配置正在用的分类」也能拦住。当前配置
    # 本身就跑不过（文件不存在或另有问题）时不能拿它当判据，否则数据永远
    # 更新不了，退回只检查两个文件能否被正常加载的最小配置。
    local check_cfg="${PATH_FOR_CONFIG}"
    if ! (cd "${install_dir}" && "${xray_bin}" run -test -c "${check_cfg}") >/dev/null 2>&1; then
        check_cfg="${tmp_dir}/check.json"
        cat >"${check_cfg}" <<'EOF'
{"outbounds":[{"protocol":"freedom","tag":"direct"}],
 "routing":{"rules":[{"type":"field","domain":["geosite:cn"],"outboundTag":"direct"},
                     {"type":"field","ip":["geoip:cn"],"outboundTag":"direct"}]}}
EOF
    fi
    if ! (cd "${install_dir}" && XRAY_LOCATION_ASSET="${tmp_dir}" "${xray_bin}" run -test -c "${check_cfg}") >/dev/null 2>&1; then
        rm -rf "${tmp_dir}"
        LOGE "新的 geo 数据未通过 xray 校验（下载内容可能不完整或不是 dat 文件），保留现有文件，未重启面板"
        return 1
    fi

    # 现有数据先留一份到 old/ 供回退（硬链接，同一文件系统内不复制数据；不行再复制），
    # 其它 .dat 照样链接进去，old/ 本身就能当一个完整的资源目录拿来校验。
    local old_dir="${tmp_dir}/old" have_old=1
    mkdir -p "${old_dir}"
    for f in "${bin_dir}"/*.dat; do
        case "$(basename "${f}")" in
        geoip.dat | geosite.dat) ;;
        *) ln -s "${f}" "${old_dir}/" 2>/dev/null ;;
        esac
    done
    for f in "${PATH_FOR_GEO_IP}" "${PATH_FOR_GEO_SITE}"; do
        if ! ln -f "${f}" "${old_dir}/$(basename "${f}")" 2>/dev/null &&
            ! cp -p "${f}" "${old_dir}/$(basename "${f}")" 2>/dev/null; then
            have_old=0
        fi
    done

    local had_cfg=0
    [[ -f "${PATH_FOR_CONFIG}" ]] && had_cfg=1
    mv -f "${tmp_dir}/geoip.dat" "${PATH_FOR_GEO_IP}" &&
        mv -f "${tmp_dir}/geosite.dat" "${PATH_FOR_GEO_SITE}"
    local mv_rc=$?
    if [[ ${mv_rc} -ne 0 ]]; then
        [[ ${have_old} -eq 1 ]] && mv -f "${old_dir}/geoip.dat" "${PATH_FOR_GEO_IP}" &&
            mv -f "${old_dir}/geosite.dat" "${PATH_FOR_GEO_SITE}"
        rm -rf "${tmp_dir}"
        LOGE "替换 geo 数据失败，请检查 ${bin_dir} 下的 geoip.dat 与 geosite.dat"
        return 1
    fi
    # 记下重启前的时刻，用来判断 bin/config.json 是不是重启后新写的。多睡一秒是因为
    # bash 4 的 -nt 只比较到整秒。
    if [[ ${had_cfg} -eq 1 ]]; then
        touch "${tmp_dir}/restart-stamp"
        sleep 1
    fi
    LOGI "geo 数据已更新并通过校验，正在重启面板"
    systemctl restart a-ui
    local restart_rc=$?
    if [[ ${had_cfg} -eq 1 ]]; then
        local i regenerated=0
        for ((i = 0; i < 30; i++)); do
            if [[ "${PATH_FOR_CONFIG}" -nt "${tmp_dir}/restart-stamp" ]]; then
                regenerated=1
                break
            fi
            sleep 1
        done
        if [[ ${regenerated} -eq 0 ]]; then
            rm -rf "${tmp_dir}"
            LOGE "geo 数据已替换并重启面板，但 30 秒内面板没有重新生成 ${PATH_FOR_CONFIG}，无法确认 xray 已用新数据正常启动，请用 a-ui status 与 a-ui log 检查"
            return 1
        fi
        if ! (cd "${install_dir}" && XRAY_LOCATION_ASSET="${bin_dir}" "${xray_bin}" run -test -c "${PATH_FOR_CONFIG}") >/dev/null 2>&1; then
            if [[ ${have_old} -eq 1 ]] &&
                (cd "${install_dir}" && XRAY_LOCATION_ASSET="${old_dir}" "${xray_bin}" run -test -c "${PATH_FOR_CONFIG}") >/dev/null 2>&1; then
                if mv -f "${old_dir}/geoip.dat" "${PATH_FOR_GEO_IP}" && mv -f "${old_dir}/geosite.dat" "${PATH_FOR_GEO_SITE}"; then
                    LOGE "面板重新生成的核心配置引用了新 geo 数据里没有的分类（多半是热更新加进去的分流规则），已换回旧数据并再次重启面板"
                    systemctl restart a-ui
                else
                    LOGE "面板重新生成的核心配置与新 geo 数据不兼容，换回旧数据也失败了，xray 可能起不来，请尽快检查 ${bin_dir}"
                fi
                rm -rf "${tmp_dir}"
                return 1
            fi
            # 旧数据同样通不过：是配置本身另有问题（比如证书文件不在了），与这次更新无关，
            # 新数据保留；但 xray 多半没在跑，如实返回失败
            rm -rf "${tmp_dir}"
            LOGE "面板重新生成的核心配置未通过 xray 校验，旧的 geo 数据同样通不过，问题不在这次更新的数据上（新数据已保留），请用 a-ui log 排查"
            return 1
        fi
    fi
    rm -rf "${tmp_dir}"
    return ${restart_rc}
}

# root 的 crontab 只经下面两个函数改动：现有内容经管道过滤后直接喂给 crontab -，
# 不落任何临时文件。原先的写法先把 crontab 写进固定的 /tmp/crontabTask.tmp 再
# crontab 它：/tmp 人人可写，本机任何用户都能抢先建好这个文件（受 protected_regular
# 保护时 root 的写入会失败，但随后的 crontab 照样把这个文件装上），他写进去的内容
# 就成了 root 的定时任务；每次「开启」还会再追加一行重复的任务。
#
# 标记用的是任务命令本身（a-ui geo / a-ui clear），旧版本脚本重复写进去的行也一并
# 收拢。crontab -l 失败而且不是"还没有 crontab"时不动它：否则会拿一份空内容把
# root 现有的定时任务整个覆盖掉。

# cron_replace_lines <标记> [行...]：删掉所有含标记的行，再追加给定的行。
cron_replace_lines() {
    local marker="$1" current
    shift
    if ! current=$(crontab -l 2>/dev/null); then
        if ! crontab -l 2>&1 | grep -qi 'no crontab'; then
            LOGE "读取 root 的 crontab 失败，未做任何改动"
            return 1
        fi
        current=""
    fi
    {
        [[ -n "${current}" ]] && printf '%s\n' "${current}" | grep -vF -- "${marker}"
        [[ $# -gt 0 ]] && printf '%s\n' "$@"
    } | crontab -
}

# cron_drop_lines <标记>：删掉所有含标记的行；本来就没有时不碰 crontab。
cron_drop_lines() {
    crontab -l 2>/dev/null | grep -qF -- "$1" || return 0
    cron_replace_lines "$1"
}

enable_auto_update_geo() {
    LOGI "正在开启自动更新geo数据..."
    if cron_replace_lines "a-ui geo" "00 4 */2 * * a-ui geo > /dev/null"; then
        LOGI "开启自动更新geo数据成功"
    else
        LOGE "开启自动更新geo数据失败"
    fi
}

disable_auto_update_geo() {
    if cron_drop_lines "a-ui geo"; then
        LOGI "取消a-ui 自动更新geo数据成功"
    else
        LOGE "取消a-ui 自动更新geo数据失败"
    fi
}

#clear xray log,need enable log in config template
#here we need input an absolute path for log
clear_log() {
    LOGI "清除xray日志中..."
    local filePath=''
    if [[ $# -gt 0 ]]; then
        filePath=$1
    else
        LOGE "未输入有效文件路径,脚本退出"
        exit 1
    fi
    LOGI "日志路径为:${filePath}"
    if [[ ! -f ${filePath} ]]; then
        LOGE "清除xray日志文件失败,${filePath}不存在,请确认"
        exit 1
    fi
    fileSize=$(ls -la ${filePath} --block-size=M | awk '{print $5}' | awk -F 'M' '{print$1}')
    if [[ ${fileSize} -gt ${DEFAULT_LOG_FILE_DELETE_TRIGGER} ]]; then
        rm $1
        if [[ $? -ne 0 ]]; then
            LOGE "清除xray日志文件:${filePath}失败"
        else
            LOGI "清除xray日志文件:${filePath}成功"
            systemctl restart a-ui
        fi
    else
        LOGI "当前日志大小为${fileSize}M,小于${DEFAULT_LOG_FILE_DELETE_TRIGGER}M,将不会清除"
    fi
}

#enable auto delete log，need file path as
enable_auto_clear_log() {
    LOGI "设置定时清除xray日志..."
    local accessfilePath=''
    local errorfilePath=''
    accessfilePath=$(cat ${PATH_FOR_CONFIG} | jq .log.access | tr -d '"')
    errorfilePath=$(cat ${PATH_FOR_CONFIG} | jq .log.error | tr -d '"')
    if [[ ! -n ${accessfilePath} && ! -n ${errorfilePath} ]]; then
        LOGI "配置文件中的日志文件路径无效,脚本退出"
        exit 1
    fi
    # 标记带上完整路径和其后的空格（"a-ui clear <路径> "）：只替换同一个日志的旧任务，
    # 另一个日志的任务不受影响——两个路径若共用 "a-ui clear" 标记，加第二条时会把刚加
    # 的第一条删掉；末尾的空格让 access.log 不会误中 access.log.1。路径会原样进 root 的
    # crontab、由 cron 交给 sh 执行，只放行普通字符组成的绝对路径——带空格、引号、$、%
    # 的路径要么让任务坏掉，要么成了以 root 执行的注入点。
    local p kind
    for kind in access error; do
        p="${accessfilePath}"
        [[ "${kind}" == error ]] && p="${errorfilePath}"
        if [[ ! -f ${p} ]]; then
            LOGE "${kind}log不存在,将不会为其设置定时清除"
        elif [[ ! "${p}" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
            LOGE "日志路径 ${p} 含有不能安全写进 crontab 的字符（只支持字母、数字与 . _ - /），将不会为其设置定时清除"
        elif cron_replace_lines "a-ui clear ${p} " "30 4 */2 * * a-ui clear ${p} > /dev/null"; then
            LOGI "设置定时清除xray日志:${p}成功"
        else
            LOGE "设置定时清除xray日志:${p}失败"
        fi
    done
}

#disable auto dlete log
disable_auto_clear_log() {
    if cron_drop_lines "a-ui clear"; then
        LOGI "取消 定时清除xray日志成功"
    else
        LOGE "取消 定时清除xray日志失败"
    fi
}

# 重新运行面板安全配置向导，用于首次安装时向导失败（证书没签发、伪装站
# 不可用）后重试，或者想更换域名/伪装站重新配置一次。向导逻辑不在这里
# 重复实现，直接调用最新的 install.sh 的向导部分，避免与 install.sh 漂移。
# --wizard-only 会跳过 install.sh 里 setup_wizard 的幂等探测并强制覆盖
# 现有配置——从菜单主动触发「重新配置」，用户的意图就是要覆盖。
reconfig_domain() {
    confirm "将重新运行配置向导，会覆盖现有的面板根路径与监听地址设置，确定继续吗" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install.sh) --wizard-only
    # 退出码必须检查：向导中途放弃（选伪装站时输入 q、证书没等到）会留下
    # 「面板已在 127.0.0.1 + Caddy 可能已被停用」的状态，不检查的话这一切
    # 会静默回到主菜单，用户只看到最后一句「已取消」，以为什么都没发生。
    # 向导自己会打印救援命令，这里只负责把"没跑完"这件事说清楚。
    local rc=$?
    if [[ ${rc} -ne 0 ]]; then
        LOGE "配置向导未正常完成（退出码 ${rc}），面板配置可能没有按预期更新"
        LOGE "请按上面的提示排查；若面板打不开，用向导打印的恢复命令处理"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

# show_listens_all 判断 `a-ui setting -show` 的输出是否表示面板监听所有 IP。监听地址为空时现在的二进制打印
# `Listen: all interfaces`，更早的打印 `Listen: `（空），两种都算；只取第二列会把前者读成 `all`、永远判失败。
show_listens_all() {
    local line listen
    line=$(echo "$1" | grep '^Listen:' | head -1)
    [[ -n "${line}" ]] || return 1
    listen=$(echo "${line}" | sed -e 's/^Listen:[[:space:]]*//' -e 's/[[:space:]]*$//')
    [[ -z "${listen}" || "${listen}" == "all interfaces" ]]
}

# 停用 Caddy 并把面板恢复为直连访问。只停用不卸载软件包——用户可能拿
# Caddy 跑其它站点。顺序不能颠倒：必须先把面板监听地址改回来、用真实
# 探活确认面板直连真的可用，再停 Caddy；反过来会出现一段时间两边都
# 进不去面板的窗口。
#
# 门槛不能只看 `systemctl status`（进程是否在跑）：main.go 的
# updateSetting 对 SetListen 写库失败只 fmt.Println，不 os.Exit，`a-ui
# setting` 整体仍返回 0——如果写库恰好失败（磁盘满、DB 被锁），面板会
# 用旧的 webListen 正常重启起来，只看进程状态会误判"改好了"，紧接着
# 停掉 Caddy，正好复现这个功能本要避免的"两边都进不去"。所以改成真的
# 发一次 HTTP 请求到面板实际的端口和根路径，这与 install.sh 的
# wait_for_panel_alive() 是同一个做法；但 a-ui.sh 是独立脚本、装机后
# 跑在独立进程里，不与 install.sh 共享函数，只能照抄一份等价逻辑。
restore_direct_panel() {
    confirm "将停用 Caddy 并把面板恢复为直连访问，域名访问会立即失效，确定继续吗" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi

    # 写入失败（库被锁、磁盘满）时不能往下走：下面的探活连 127.0.0.1，面板仍监听回环也照样通过，
    # 会在面板只能经 Caddy 访问的时候把 Caddy 停掉。
    if ! a_ui_setting -listen ""; then
        LOGE "恢复面板监听所有 IP 失败，本次不会停用 Caddy"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    systemctl restart a-ui

    # 面板端口和根路径都可能是随机改过的，不能假设 54321 或 "/"：直接
    # 问面板自己要——`a-ui setting -show` 是本次改动新增的只读入口，
    # 不依赖 sqlite3 这类不保证装了的命令行工具（install_base 只装了
    # wget/curl/tar/jq）。
    local show_out port basepath
    show_out=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null)
    if [[ $? != 0 ]]; then
        LOGE "读取面板当前设置失败，为避免彻底进不去面板，本次不会停用 Caddy"
        LOGE "请用 a-ui log 排查原因，确认面板可直连访问后再重新执行本操作"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    port=$(echo "${show_out}" | grep '^Port:' | awk '{print $2}')
    basepath=$(echo "${show_out}" | grep '^BasePath:' | awk '{print $2}')
    # 再确认一次真的写进去了（旧二进制写失败也退出 0）：监听地址必须是「所有 IP」。
    if ! show_listens_all "${show_out}"; then
        LOGE "面板仍未监听所有 IP，本次不会停用 Caddy"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi

    local alive=1 i
    if [[ -n "${port}" ]]; then
        for i in 1 2 3 4 5; do
            if curl -fsS -m 3 "http://127.0.0.1:${port}${basepath}" -o /dev/null 2>/dev/null; then
                alive=0
                break
            fi
            sleep 1
        done
    fi
    if [[ "${alive}" != "0" ]]; then
        LOGE "面板重启后探活失败，127.0.0.1:${port}${basepath} 没有响应"
        LOGE "为避免彻底进不去面板，本次不会停用 Caddy"
        LOGE "请用 a-ui log 排查原因，确认面板可直连访问后再重新执行本操作"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi

    systemctl stop caddy 2>/dev/null
    systemctl disable caddy 2>/dev/null
    LOGI "Caddy 已停用（软件包与配置均保留，systemctl enable --now caddy 可恢复）"
    LOGI "面板现可通过 http://<本机IP>:${port}${basepath} 直连访问"

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_usage() {
    echo "a-ui 管理脚本使用方法: "
    echo "------------------------------------------"
    echo "a-ui              - 显示管理菜单 (功能更多)"
    echo "a-ui start        - 启动 a-ui 面板"
    echo "a-ui stop         - 停止 a-ui 面板"
    echo "a-ui restart      - 重启 a-ui 面板"
    echo "a-ui status       - 查看 a-ui 状态"
    echo "a-ui enable       - 设置 a-ui 开机自启"
    echo "a-ui disable      - 取消 a-ui 开机自启"
    echo "a-ui log          - 查看 a-ui 日志"
    echo "a-ui v2-ui        - 迁移本机器的 v2-ui 账号数据至 a-ui"
    echo "a-ui update       - 更新 a-ui 面板"
    echo "a-ui install      - 安装 a-ui 面板"
    echo "a-ui uninstall    - 卸载 a-ui 面板"
    echo "a-ui clear        - 清除 a-ui 日志"
    echo "a-ui geo          - 更新 a-ui geo数据"
    echo "a-ui cron         - 配置 a-ui 定时任务"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}AetherUI 面板管理脚本${plain}
  ${green}0.${plain} 退出脚本
————————————————
  ${green}1.${plain} 安装 a-ui
  ${green}2.${plain} 更新 a-ui
  ${green}3.${plain} 卸载 a-ui
————————————————
  ${green}4.${plain} 重置用户名密码
  ${green}5.${plain} 重置面板设置
  ${green}6.${plain} 设置面板端口
  ${green}7.${plain} 查看当前面板信息
————————————————
  ${green}8.${plain} 启动 a-ui
  ${green}9.${plain} 停止 a-ui
  ${green}10.${plain} 重启 a-ui
  ${green}11.${plain} 查看 a-ui 状态
  ${green}12.${plain} 查看 a-ui 日志
————————————————
  ${green}13.${plain} 设置 a-ui 开机自启
  ${green}14.${plain} 取消 a-ui 开机自启
————————————————
  ${green}15.${plain} 一键安装 bbr (最新内核)
  ${green}16.${plain} 一键申请SSL证书(acme申请)
  ${green}17.${plain} 配置a-ui定时任务
————————————————
  ${green}18.${plain} 配置域名与伪装站
  ${green}19.${plain} 卸载 Caddy 并恢复面板直连
 "
    show_status
    echo && read -p "请输入选择 [0-19],查看面板登录信息请输入数字7:" num

    case "${num}" in
    0)
        exit 0
        ;;
    1)
        check_uninstall && install
        ;;
    2)
        check_install && update
        ;;
    3)
        check_install && uninstall
        ;;
    4)
        check_install && reset_user
        ;;
    5)
        check_install && reset_config
        ;;
    6)
        check_install && set_port
        ;;
    7)
        check_install && check_config
        ;;
    8)
        check_install && start
        ;;
    9)
        check_install && stop
        ;;
    10)
        check_install && restart
        ;;
    11)
        check_install && status
        ;;
    12)
        check_install && show_log
        ;;
    13)
        check_install && enable
        ;;
    14)
        check_install && disable
        ;;
    15)
        install_bbr
        ;;
    16)
        ssl_cert_issue
        ;;
    17)
        check_install && cron_jobs
        ;;
    18)
        check_install && reconfig_domain
        ;;
    19)
        check_install && restore_direct_panel
        ;;
    *)
        LOGE "请输入正确的数字 [0-19],查看面板登录信息请输入数字7"
        ;;
    esac
}

if [[ $# > 0 ]]; then
    case $1 in
    "start")
        check_install 0 && start 0
        ;;
    "stop")
        check_install 0 && stop 0
        ;;
    "restart")
        check_install 0 && restart 0
        ;;
    "status")
        check_install 0 && status 0
        ;;
    "enable")
        check_install 0 && enable 0
        ;;
    "disable")
        check_install 0 && disable 0
        ;;
    "log")
        check_install 0 && show_log 0
        ;;
    "v2-ui")
        check_install 0 && migrate_v2_ui 0
        ;;
    "update")
        check_install 0 && update 0
        ;;
    "install")
        check_uninstall 0 && install 0
        ;;
    "uninstall")
        check_install 0 && uninstall 0
        ;;
    "geo")
        check_install 0 && update_geo
        ;;
    "clear")
        check_install 0 && clear_log $2
        ;;
    "cron")
        check_install && cron_jobs
        ;;
    *) show_usage ;;
    esac
else
    show_menu
fi
