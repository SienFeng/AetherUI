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
[[ $EUID -ne 0 ]] && LOGE "${red}fatal error:please run this script with root privilege${plain}\n" && exit 1

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
    LOGE "check system os failed,please contact with author!\n" && exit 1
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
        LOGE "${red}please use CentOS 7 or higher version${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        LOGE "${red}please use Ubuntu 16 or higher version${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        LOGE "${red}please use Debian 8 or higher version${plain}\n" && exit 1
    fi
fi

confirm() {
    if [[ $# > 1 ]]; then
        echo && read -p "$1 [default:$2]: " temp
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
    confirm "confirm to restart a-ui,xray service will be restart" "y"
    if [[ $? == 0 ]]; then
        restart
    else
        show_menu
    fi
}

before_show_menu() {
    echo && echo -n -e "${yellow}enter to return to the control menu: ${plain}" && read temp
    show_menu
}

install() {
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install_en.sh)
    if [[ $? == 0 ]]; then
        if [[ $# == 0 ]]; then
            start
        else
            start 0
        fi
    fi
}

update() {
    confirm "will upgrade to the latest,continue?" "n"
    if [[ $? != 0 ]]; then
        LOGE "cancelled..."
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 0
    fi
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install_en.sh)
    if [[ $? == 0 ]]; then
        LOGI "upgrade finished,restart completed"
        exit 0
    fi
}

# Remove the three things install.sh's install_cert_sync sets up on a domain
# install. The certificates in /root/cert/ are left alone: menu 16's acme.sh
# issuance writes there too, and there is no telling whose is whose. No
# disable --now: this script claims to support CentOS 7, whose systemd 219
# does not know --now yet.
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
    [[ ${found} -eq 1 ]] && LOGI "removed the a-ui-cert-sync certificate sync timer (certificate files in /root/cert/ are kept)"
    return 0
}

# Remove the site the panel itself wrote from the Caddyfile; the Caddy package
# and any other site are never touched (uninstalling Caddy itself is by design
# only ever offered, never done by default). install.sh's write_caddyfile
# wraps the site block it writes in BEGIN/END lines, and only what lies
# between them is removed; the file is backed up first, checked with
# caddy validate afterwards and restored as it was if that fails, and Caddy
# is reloaded only when it passes. Older wizards wrote no markers; in such a
# file there is no telling reliably which part is the panel's, so it is only
# pointed out, never edited. The argument is the panel port read before the
# uninstall (may be empty), used only for that hint.
remove_caddy_site() {
    local panel_port="$1" cf=/etc/caddy/Caddyfile
    [[ -f "${cf}" ]] || return 0
    if ! grep -q '^# BEGIN a-ui managed site' "${cf}"; then
        if grep -qE "reverse_proxy[[:space:]]+127\.0\.0\.1:${panel_port:-[0-9]+}([^0-9]|\$)" "${cf}"; then
            LOGD "${cf} has a site reverse-proxying to this machine's panel port, apparently written by an older setup wizard. It carries no a-ui markers, so it was left unchanged to avoid deleting your own configuration"
            LOGD "if you no longer need it: edit ${cf} to delete that site block, then run systemctl reload caddy"
        fi
        return 0
    fi
    local new backup
    if ! new=$(awk '
        /^# BEGIN a-ui managed site/ { if (skip) bad = 1; skip = 1; next }
        /^# END a-ui managed site/   { if (!skip) bad = 1; skip = 0; next }
        !skip { print }
        END { if (skip || bad) exit 3 }' "${cf}"); then
        LOGE "the BEGIN/END markers of the panel site in ${cf} do not pair up; left unchanged to avoid deleting other sites, please clean it up by hand"
        return 1
    fi
    backup="${cf}.a-ui-uninstall-$(date +%Y%m%d-%H%M%S).bak"
    if ! cp -p "${cf}" "${backup}"; then
        LOGE "failed to back up ${cf}, the panel site in it was not removed"
        return 1
    fi
    printf '%s\n' "${new}" >"${cf}"
    if command -v caddy >/dev/null 2>&1 && ! caddy validate --config "${cf}" >/dev/null 2>&1; then
        cp -p "${backup}" "${cf}"
        LOGE "after removing the panel site ${cf} failed caddy validate, the original file was restored, please clean it up by hand"
        return 1
    fi
    if systemctl is-active --quiet caddy 2>/dev/null && ! systemctl reload caddy; then
        LOGE "reloading Caddy failed, see systemctl status caddy"
    fi
    LOGI "removed the site written by the panel from ${cf} (the original file is backed up at ${backup})"
    LOGI "the Caddy package itself was not uninstalled; if you no longer need it: systemctl stop caddy && systemctl disable caddy, then remove it with the system package manager"
    local old_backup
    old_backup=$(ls -t /root/caddy-backup-*.tar.gz 2>/dev/null | head -n 1)
    [[ -n "${old_backup}" ]] && LOGI "the setup wizard backed up the configuration of the Caddy it disabled back then: ${old_backup}, your original sites can be recovered from it"
    return 0
}

uninstall() {
    confirm "sure you want to uninstall a-ui? (the Caddy site, certificate sync timer and cron jobs the panel added are removed too, Caddy itself is kept)" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    # The panel port must be read before the database is deleted; it is only
    # used to recognize a Caddyfile written by an older wizard, for a hint
    local panel_port_for_caddy
    panel_port_for_caddy=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null | awk '/^Port:/ {print $2; exit}')
    systemctl stop a-ui
    systemctl disable a-ui
    rm /etc/systemd/system/a-ui.service -f
    systemctl daemon-reload
    systemctl reset-failed
    rm /etc/a-ui/ -rf
    rm /usr/local/a-ui/ -rf
    # install.sh's xray backup directory is a sibling of /usr/local/a-ui/, so the
    # line above does not reach it. A normal install cleans it up in
    # restore_or_install_xray, but a Ctrl-C or a signal in the middle leaves
    # ~66MB behind — it was deliberately moved out of /tmp into /usr/local to
    # avoid tmpfs, and the price of that is it no longer disappears on reboot.
    # The download staging directory (fetch_release) is the same.
    rm -rf /usr/local/a-ui-xray-backup-*
    rm -rf /usr/local/a-ui-update-*
    # Cron jobs added by menu 17 (they point at a panel that no longer exists)
    cron_drop_lines "a-ui geo" || LOGE "failed to remove the geo update job, delete the line containing a-ui geo with crontab -e"
    cron_drop_lines "a-ui clear" || LOGE "failed to remove the log clearing jobs, delete the lines containing a-ui clear with crontab -e"
    remove_cert_sync
    remove_caddy_site "${panel_port_for_caddy}"

    echo ""
    echo -e "uninstall a-ui succeed,you can delete this script by ${green}rm /usr/bin/a-ui -f${plain}"
    echo ""

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

reset_user() {
    confirm "are you sure you want to reset the username and password to ${green}admin${plain} and turn off two-factor authentication ?" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    if ! a_ui_setting -username admin -password admin; then
        LOGE "failed to reset the username and password, the account is unchanged"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    # A separate command that is allowed to fail: this script always comes
    # from the main branch while the binary may be an older, rolled-back
    # version that does not know -disable2fa. Merged into the line above, an
    # old binary would reject the whole command and reset nothing.
    /usr/local/a-ui/a-ui setting -disable2fa >/dev/null 2>&1 || true
    echo -e "your username and password are reset to ${green}admin${plain},two-factor authentication is off,restart a-ui to take effect"
    echo -e "${yellow}admin/admin is a publicly known default,change it right after login (Panel Settings -> account)${plain}"
    confirm_restart
}

# Run /usr/local/a-ui/a-ui setting and report honestly whether it worked
# (same rule as install.sh's a_ui_setting): look at the output as well as the
# exit code — this script always comes from the main branch while the binary
# may be an older, rolled-back version, which exits 0 even when the database
# write fails and only prints a "... failed: ..." line. On failure print the
# output and return 1.
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

# Whether something is already listening on the TCP port, same as install.sh's
# port_in_use: ss first (no -H, CentOS 7 does not know it), netstat when there
# is no ss, and treated as free when neither exists.
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

# Whether the panel is currently reached through Caddy (domain install): the
# Caddyfile proxies requests to the panel's current port, and the panel either
# listens on loopback only (reachable only through Caddy) or Caddy is running.
# Loopback-only without a Caddy proxy (SSH tunnel only) does not count —
# changing the port there locks nobody out; neither does the state after menu
# 19 restored direct access (listening on every IP, Caddy disabled). The
# argument is the output of a-ui setting -show.
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
    # On a domain install, a reset puts the listen address, port and base path
    # back to their defaults: the panel is then served directly on port 54321
    # of every IP at base path /, and Caddy's reverse proxy stops working. Say
    # so before asking.
    local show_out
    if show_out=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null) && panel_behind_caddy "${show_out}"; then
        LOGD "Note: the panel is currently reached through Caddy (domain install). After the reset it will be served directly on port 54321 of every IP at base path /, and access through the domain stops working; to hide the panel again, run menu 18 (Configure domain and decoy site) after the reset"
    fi
    confirm "are you sure you want to reset all settings,user data will not be lost,two-factor authentication will be turned off" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    if ! a_ui_setting -reset; then
        LOGE "failed to reset the settings, they are unchanged"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    echo -e "all settings are reset to default,please restart a-ui,and use default port ${green}54321${plain} to access panel"
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
    echo && echo -n -e "please set a port[1-65535]: " && read port
    if [[ -z "${port}" ]]; then
        LOGD "cancelled..."
        before_show_menu
        return 0
    fi
    # Validate before handing it to a-ui setting: 0 and numbers above 65535 are
    # stored as they are and the panel cannot bind after the restart; numbers
    # with a leading 0 are parsed as octal by Go's flag package (010 becomes 8,
    # 08 is an error).
    if [[ ! "${port}" =~ ^[0-9]{1,5}$ ]] || ((10#${port} < 1 || 10#${port} > 65535)); then
        LOGE "invalid port: please enter a whole number between 1 and 65535"
        before_show_menu
        return 1
    fi
    port=$((10#${port}))

    local show_out cur_port
    if ! show_out=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null); then
        LOGE "failed to read the current panel settings, nothing changed"
        before_show_menu
        return 1
    fi
    cur_port=$(echo "${show_out}" | awk '/^Port:/ {print $2; exit}')
    # On a domain install the panel is only reached through Caddy, and
    # install.sh wrote a fixed reverse_proxy 127.0.0.1:<port> into the
    # Caddyfile. Changing the panel port alone leaves Caddy forwarding to the
    # old port and locks the admin out. Deliberately not rewriting the
    # Caddyfile here: that is a coordinated change of two configurations,
    # each with its own failure and rollback, and getting only half of it done
    # is a lockout; the menu 18 wizard is already the one entry point that
    # writes Caddy and the panel settings together with full failure handling.
    # So refuse and point there.
    if panel_behind_caddy "${show_out}"; then
        LOGE "the panel is reached through Caddy (domain install), its port cannot be changed here on its own: Caddy would keep forwarding to the old port ${cur_port} and the panel would no longer open via the domain"
        LOGI "to reconfigure the domain and the panel, use menu 18 (Configure domain and decoy site), which updates Caddy and the panel settings together"
        LOGI "to access the panel directly instead, first use menu 19 (Uninstall Caddy and restore direct panel access), then change the port"
        before_show_menu
        return 1
    fi
    if [[ "${port}" == "${cur_port}" ]]; then
        LOGI "the panel already uses port ${port}, nothing to change"
        before_show_menu
        return 0
    fi
    if port_in_use "${port}"; then
        LOGE "port ${port} is already in use by another program, please pick another one"
        before_show_menu
        return 1
    fi
    if ! a_ui_setting -port "${port}"; then
        LOGE "setting the port failed, the panel port is unchanged"
        before_show_menu
        return 1
    fi
    echo -e "set port done,please restart a-ui,and use this new port ${green}${port}${plain} to access panel"
    confirm_restart
}

start() {
    check_status
    if [[ $? == 0 ]]; then
        echo ""
        LOGI "a-ui is running,no need to start agin"
    else
        systemctl start a-ui
        sleep 2
        check_status
        if [[ $? == 0 ]]; then
            LOGI "start a-ui  succeed"
        else
            LOGE "start a-ui failed,please check logs"
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
        LOGI "a-ui is stopped,no need to stop again"
    else
        systemctl stop a-ui
        sleep 2
        check_status
        if [[ $? == 1 ]]; then
            LOGI "stop a-ui succeed"
        else
            LOGE "stop a-ui failed,please check logs"
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
        LOGI "restart a-ui succeed"
    else
        LOGE "stop a-ui failed,please check logs"
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
        LOGI "enable a-ui on system startup succeed"
    else
        LOGE "enable a-ui on system startup failed"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

disable() {
    systemctl disable a-ui
    if [[ $? == 0 ]]; then
        LOGI "disable a-ui on system startup succeed"
    else
        LOGE "disable a-ui on system startup failed"
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

# Download the new script to a temp file under /usr/bin, check it, then mv it
# over /usr/bin/a-ui instead of writing to /usr/bin/a-ui directly:
#   - certificates must be verified: this script runs as root and must not
#     come over a connection a man in the middle could swap.
#   - wget -O truncates the target before writing, so a failed download or an
#     error page would destroy the management script itself.
#   - the file being replaced is the very one bash is reading line by line
#     right now. Rewriting it in place makes the running bash continue with
#     misaligned bytes of the new file; mv puts in a new inode and the
#     running process keeps reading the old one. The temp file sits in the
#     target's directory so the mv is an atomic same-filesystem rename.
update_shell() {
    local tmp
    tmp=$(mktemp /usr/bin/.a-ui-update.XXXXXX) || {
        LOGE "failed to create a temp file, nothing changed"
        before_show_menu
        return 1
    }
    if ! wget -O "${tmp}" https://github.com/SienFeng/AetherUI/raw/main/a-ui_en.sh; then
        rm -f "${tmp}"
        echo ""
        LOGE "update shell script failed,please check whether your server can access github"
        before_show_menu
        return 1
    fi
    if [[ ! -s "${tmp}" ]] || ! head -n 1 "${tmp}" | grep -qE '^#!.*bash' || ! bash -n "${tmp}" 2>/dev/null; then
        rm -f "${tmp}"
        LOGE "the download is not a complete bash script (empty, an error page or truncated), nothing replaced"
        before_show_menu
        return 1
    fi
    if ! chmod 755 "${tmp}" || ! mv -f "${tmp}" /usr/bin/a-ui; then
        rm -f "${tmp}"
        LOGE "failed to replace /usr/bin/a-ui, the current script is unchanged"
        before_show_menu
        return 1
    fi
    LOGI "update shell script succeed" && exit 0
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
        LOGE "a-ui is installed already"
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
        LOGE "please install a-ui first"
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
        echo -e "a-ui status: ${green}running${plain}"
        show_enable_status
        ;;
    1)
        echo -e "a-ui status: ${yellow}stopped${plain}"
        show_enable_status
        ;;
    2)
        echo -e "a-ui status: ${red}not installed${plain}"
        ;;
    esac
    show_xray_status
}

show_enable_status() {
    check_enabled
    if [[ $? == 0 ]]; then
        echo -e "enable on system startup: ${green}yes${plain}"
    else
        echo -e "enable on system startup: ${red}no${plain}"
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
        echo -e "xray status: ${green}running${plain}"
    else
        echo -e "xray status: ${red}stopped${plain}"
    fi
}

#this will be an entrance for ssl cert issue
#here we can provide two different methods to issue cert
#first.standalone mode second.DNS API mode
ssl_cert_issue() {
    local method=""
    echo -E ""
    LOGD "********Usage********"
    LOGI "this shell script will use acme to help issue certs."
    LOGI "here we provide two methods for issuing certs:"
    LOGI "method 1:acme standalone mode,need to keep port:80 open"
    LOGI "method 2:acme DNS API mode,need provide Cloudflare Global API Key"
    LOGI "for a free domain, method 1 is recommended"
    LOGI "for a paid domain resolved through Cloudflare, use method 2"
    LOGI "certs will be installed in /root/cert directory"
    read -p "please choose which method do you want,type 1 or 2": method
    LOGI "you choosed method:${method}"

    if [ "${method}" == "1" ]; then
        ssl_cert_issue_standalone
    elif [ "${method}" == "2" ]; then
        ssl_cert_issue_by_cloudflare
    else
        LOGE "invalid input,please check it..."
        exit 1
    fi
}

install_acme() {
    cd ~ || return 1
    LOGI "install acme..."
    # Download first, then run: the status of curl https://get.acme.sh | sh is sh's, and when the download fails
    # sh reads empty input and still exits 0.
    local installer
    installer=$(mktemp) || return 1
    if ! curl -fsSL https://get.acme.sh -o "${installer}" || ! sh "${installer}"; then
        rm -f "${installer}"
        LOGE "install acme failed"
        return 1
    fi
    rm -f "${installer}"
    if [ ! -x ~/.acme.sh/acme.sh ]; then
        LOGE "install acme failed"
        return 1
    fi
    LOGI "install acme succeed"
    return 0
}

# Helpers for the certificate menu.
#
# valid_cert_domain: the domain used to be unvalidated and unquoted. Pressing Enter gave grep no pattern, the count
# was 0, the "already exists" check passed, and the cleanup after the failed issue became rm -rf ~/.acme.sh/ --
# deleting acme.sh itself, the account keys, and every other domain's certificates and renewal configs, so the
# certificates of existing TLS inbounds silently expired within 90 days.
# acme_cert_exists: exact match on the first column of --list (Main_Domain); the old substring match refused
# example.com whenever sub.example.com existed.
# remove_acme_domain: cleanup after a failed issue. acme.sh >= 3.0.6 defaults to ECC, stored in <domain>_ecc; only
# removing <domain> left it in --list, so every retry was refused as "already exists".
# caddy_managed_domain: the domain of the Caddy certificate sync set up by the install wizard (a-ui-cert-sync writes
# its certificate into /root/cert every hour).
valid_cert_domain() {
    [[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]
}

acme_cert_exists() {
    ~/.acme.sh/acme.sh --list 2>/dev/null | awk 'NR > 1 { print $1 }' | grep -qxF -- "$1"
}

remove_acme_domain() {
    valid_cert_domain "$1" || return 0
    ~/.acme.sh/acme.sh --remove -d "$1" >/dev/null 2>&1
    ~/.acme.sh/acme.sh --remove -d "$1" --ecc >/dev/null 2>&1
    rm -rf "${HOME}/.acme.sh/$1" "${HOME}/.acme.sh/$1_ecc"
}

caddy_managed_domain() {
    sed -n 's#^ExecStart=/usr/local/bin/a-ui-cert-sync ##p' /etc/systemd/system/a-ui-cert-sync.service 2>/dev/null | head -1
}

#method for standalone mode
ssl_cert_issue_standalone() {
    #check for acme.sh first
    if ! command -v ~/.acme.sh/acme.sh &>/dev/null; then
        echo "acme.sh could not be found. we will install it"
        install_acme
        if [ $? -ne 0 ]; then
            LOGE "install acme failed, please check logs"
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
        LOGE "install socat failed, please check logs"
        exit 1
    else
        LOGI "install socat succeed..."
    fi
    #creat a directory for install cert
    certPath=/root/cert
    if [ ! -d "$certPath" ]; then
        mkdir $certPath
    fi
    #get the domain here,and we need verify it
    local domain=""
    read -p "please input your domain:" domain
    if ! valid_cert_domain "${domain}"; then
        LOGE "invalid domain (must not be empty; only letters, digits, - and . are allowed): ${domain}"
        exit 1
    fi
    if [[ "$(caddy_managed_domain | tr '[:upper:]' '[:lower:]')" == "$(printf '%s' "${domain}" | tr '[:upper:]' '[:lower:]')" ]]; then
        LOGE "the certificate for this domain is issued by Caddy and synced to /root/cert (a-ui-cert-sync); no need to issue it with acme"
        exit 1
    fi
    LOGD "your domain is:${domain},check it..."
    #here we need to judge whether there exists cert already
    if acme_cert_exists "${domain}"; then
        local certInfo=$(~/.acme.sh/acme.sh --list)
        LOGE "system already have certs here,can not issue again,current certs details:"
        LOGI "$certInfo"
        exit 1
    else
        LOGI "your domain is ready for issuing cert now..."
    fi
    #get needed port here
    local WebPort=80
    read -p "please choose which port do you use,default will be 80 port:" WebPort
    # Enter (empty) uses 80; anything that is not a number in 1-65535 is reported and really replaced with 80
    # (it used to only print the error and still pass the value to --httpport).
    if ! [[ "${WebPort}" =~ ^[0-9]{1,5}$ ]] || (( 10#${WebPort} < 1 || 10#${WebPort} > 65535 )); then
        [[ -n "${WebPort}" ]] && LOGE "your input ${WebPort} is invalid,will use default port"
        WebPort=80
    fi
    WebPort=$((10#${WebPort}))
    LOGI "will use port:${WebPort} to issue certs,please make sure this port is open..."
    #NOTE:This should be handled by user
    #open the port and kill the occupied progress
    ~/.acme.sh/acme.sh --set-default-ca --server letsencrypt
    ~/.acme.sh/acme.sh --issue -d "${domain}" --standalone --httpport "${WebPort}"
    if [ $? -ne 0 ]; then
        LOGE "issue certs failed,please check logs"
        remove_acme_domain "${domain}"
        exit 1
    else
        LOGI "issue certs succeed,installing certs..."
    fi
    #install cert
    ~/.acme.sh/acme.sh --installcert -d "${domain}" --ca-file "/root/cert/${domain}.ca.cer" \
        --cert-file "/root/cert/${domain}.cer" --key-file "/root/cert/${domain}.key" \
        --fullchain-file "/root/cert/${domain}.fullchain.cer"

    if [ $? -ne 0 ]; then
        LOGE "install certs failed,exit"
        remove_acme_domain "${domain}"
        exit 1
    else
        LOGI "install certs succeed,enable auto renew..."
    fi
    ~/.acme.sh/acme.sh --upgrade --auto-upgrade
    if [ $? -ne 0 ]; then
        LOGE "auto renew failed,certs details:"
        ls -lah "$certPath"
        chmod 755 $certPath
        exit 1
    else
        LOGI "auto renew succeed,certs details:"
        LOGI "certificate: /root/cert/${domain}.fullchain.cer  key: /root/cert/${domain}.key (use these paths in the inbound TLS settings)"
        ls -lah "$certPath"
        chmod 755 $certPath
    fi

}

#method for DNS API mode
ssl_cert_issue_by_cloudflare() {
    echo -E ""
    LOGD "******Preconditions******"
    LOGI "1.need Cloudflare account associated email"
    LOGI "2.need Cloudflare Global API Key"
    LOGI "3.your domain use Cloudflare as resolver"
    confirm "I have confirmed all these info above[y/n]" "y"
    if [ $? -eq 0 ]; then
        install_acme
        if [ $? -ne 0 ]; then
            LOGE "install acme failed,please check logs"
            exit 1
        fi
        CF_Domain=""
        CF_GlobalKey=""
        CF_AccountEmail=""
        certPath=/root/cert
        if [ ! -d "$certPath" ]; then
            mkdir $certPath
        fi
        LOGD "please input your domain:"
        read -p "Input your domain here:" CF_Domain
        if ! valid_cert_domain "${CF_Domain}"; then
            LOGE "invalid domain (must not be empty; only letters, digits, - and . are allowed): ${CF_Domain}"
            exit 1
        fi
        if [[ "$(caddy_managed_domain | tr '[:upper:]' '[:lower:]')" == "$(printf '%s' "${CF_Domain}" | tr '[:upper:]' '[:lower:]')" ]]; then
            LOGE "the certificate for this domain is issued by Caddy and synced to /root/cert (a-ui-cert-sync); no need to issue it with acme"
            exit 1
        fi
        LOGD "your domain is:${CF_Domain},check it..."
        #here we need to judge whether there exists cert already
        if acme_cert_exists "${CF_Domain}"; then
            local certInfo=$(~/.acme.sh/acme.sh --list)
            LOGE "system already have certs here,can not issue again,current certs details:"
            LOGI "$certInfo"
            exit 1
        else
            LOGI "your domain is ready for issuing cert now..."
        fi
        # The Global API Key controls the whole Cloudflare account: not echoed
        # while typing and never printed afterwards — terminal scrollback,
        # screen recordings and session logs would all keep it.
        LOGD "please inout your cloudflare global API key (not shown while typing):"
        read -rs -p "Input your key here:" CF_GlobalKey
        echo ""
        LOGD "cloudflare global API key read"
        LOGD "please input your cloudflare account email:"
        read -p "Input your email here:" CF_AccountEmail
        LOGD "your cloudflare account email:${CF_AccountEmail}"
        ~/.acme.sh/acme.sh --set-default-ca --server letsencrypt
        if [ $? -ne 0 ]; then
            LOGE "change the default CA to Lets'Encrypt failed,exit"
            exit 1
        fi
        export CF_Key="${CF_GlobalKey}"
        export CF_Email=${CF_AccountEmail}
        ~/.acme.sh/acme.sh --issue --dns dns_cf -d "${CF_Domain}" -d "*.${CF_Domain}" --log
        if [ $? -ne 0 ]; then
            LOGE "issue cert failed,exit"
            remove_acme_domain "${CF_Domain}"
            exit 1
        else
            LOGI "issue cert succeed,installing..."
        fi
        ~/.acme.sh/acme.sh --installcert -d "${CF_Domain}" -d "*.${CF_Domain}" --ca-file "/root/cert/${CF_Domain}.ca.cer" \
            --cert-file "/root/cert/${CF_Domain}.cer" --key-file "/root/cert/${CF_Domain}.key" \
            --fullchain-file "/root/cert/${CF_Domain}.fullchain.cer"
        if [ $? -ne 0 ]; then
            LOGE "install cert failed,exit"
            remove_acme_domain "${CF_Domain}"
            exit 1
        else
            LOGI "install cert succeed,enable auto renew..."
        fi
        ~/.acme.sh/acme.sh --upgrade --auto-upgrade
        if [ $? -ne 0 ]; then
            LOGE "enable auto renew failed,exit"
            ls -lah "$certPath"
            chmod 755 $certPath
            exit 1
        else
            LOGI "enable auto renew succeed,cert details:"
            LOGI "certificate: /root/cert/${CF_Domain}.fullchain.cer  key: /root/cert/${CF_Domain}.key (use these paths in the inbound TLS settings)"
            ls -lah "$certPath"
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
  ${green}a-ui cron jobs${plain}
  ${green}0.${plain}  return main menu
  ${green}1.${plain}  enable automatically update geo data
  ${green}2.${plain}  disable automatically update geo data 
  ${green}3.${plain}  enable automatically clear xray log
  ${green}4.${plain}  disable automatically clear xray log
  "
    echo && read -p "plz input your choice [0-4]: " num
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
        LOGE "plz input a valid choice [0-4]"
        ;;
    esac
}

#update geo data
# New data is downloaded to a temp dir and checked by the real xray before it
# replaces anything and the panel restarts; any failure keeps the current files,
# skips the restart and returns non-zero.
#
# The old code overwrote bin/ with curl -s -L: without -f, an error page from
# GitHub (rate limit, 5xx) still exits 0 and gets written as the .dat file, and
# the restart that follows leaves xray unable to load geo data - every node on
# the machine goes down; the panel process itself keeps running, so
# a-ui.service's Restart=on-failure cannot recover it either. The cron job runs
# every two days at night, exactly when nobody is watching.
#
# The check before the swap uses bin/config.json, which may be stale: a hot
# apply (tryHotApply) only pushes changes into the running core over gRPC and
# does not rewrite that file, which is regenerated only when the core is
# started as a whole process. If a routing rule added by a hot apply refers to
# a category the new data lacks, the stale file cannot catch it and xray still
# fails to start after the restart. So after swapping and restarting, wait for
# the panel to regenerate bin/config.json and check it against the new data
# once more; only when the old data passes and the new data fails is the new
# data to blame, and then the old data is put back and the panel restarted
# again. Without a bin/config.json (the panel never started the core yet)
# nothing can be stale, so this step is skipped.
#
# This function must stay self-contained: update_geo_script_test.go extracts it
# verbatim and runs it on its own.
update_geo() {
    local bin_dir install_dir xray_bin tmp_dir f
    bin_dir=$(dirname "${PATH_FOR_GEO_SITE}")
    install_dir=$(dirname "${bin_dir}")
    xray_bin=$(ls "${bin_dir}"/xray-linux-* 2>/dev/null | head -1)
    if [[ -z "${xray_bin}" ]]; then
        LOGE "xray core not found, cannot check new geo data, nothing changed"
        return 1
    fi

    # Not /tmp: the two files are ~30MB and /tmp is often an in-memory tmpfs on
    # systemd distros (same reason as backup_xray_assets in install.sh); staying
    # next to the install dir also keeps the mv below an atomic rename.
    tmp_dir=$(mktemp -d "${install_dir}/.geo-update-XXXXXX") || {
        LOGE "failed to create temp dir, nothing changed"
        return 1
    }
    if ! curl -fsSL --retry 2 -o "${tmp_dir}/geoip.dat" "${URL_FOR_GEO_IP}" ||
        ! curl -fsSL --retry 2 -o "${tmp_dir}/geosite.dat" "${URL_FOR_GEO_SITE}"; then
        rm -rf "${tmp_dir}"
        LOGE "failed to download geo data, kept current files, panel not restarted"
        return 1
    fi

    # The config may reference other data files in bin/ (ext:xxx.dat); with the
    # asset dir swapped they would be missing and good data would be rejected,
    # so link them in.
    for f in "${bin_dir}"/*.dat; do
        case "$(basename "${f}")" in
        geoip.dat | geosite.dat) ;;
        *) ln -s "${f}" "${tmp_dir}/" 2>/dev/null ;;
        esac
    done

    # Checking against the current config also catches new data that dropped a
    # category the config uses. If the current config fails on its own (missing
    # or broken for another reason) it cannot be the judge, or the data could
    # never be updated; fall back to a minimal config that only loads both files.
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
        LOGE "new geo data failed the xray check (incomplete download or not a .dat file), kept current files, panel not restarted"
        return 1
    fi

    # Keep the current data in old/ for a rollback (hard links, so nothing is
    # copied within one filesystem; copy if that fails), with the other .dat
    # files linked in as well, so old/ is itself a complete asset directory to
    # check against.
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
        LOGE "failed to replace geo data, check geoip.dat and geosite.dat in ${bin_dir}"
        return 1
    fi
    # Note the moment before the restart, to tell whether bin/config.json was
    # written after it. The extra second is because bash 4's -nt only compares
    # whole seconds.
    if [[ ${had_cfg} -eq 1 ]]; then
        touch "${tmp_dir}/restart-stamp"
        sleep 1
    fi
    LOGI "geo data updated and checked, restarting panel"
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
            LOGE "geo data was replaced and the panel restarted, but the panel did not regenerate ${PATH_FOR_CONFIG} within 30 seconds, so it is not confirmed that xray started with the new data; check with a-ui status and a-ui log"
            return 1
        fi
        if ! (cd "${install_dir}" && XRAY_LOCATION_ASSET="${bin_dir}" "${xray_bin}" run -test -c "${PATH_FOR_CONFIG}") >/dev/null 2>&1; then
            if [[ ${have_old} -eq 1 ]] &&
                (cd "${install_dir}" && XRAY_LOCATION_ASSET="${old_dir}" "${xray_bin}" run -test -c "${PATH_FOR_CONFIG}") >/dev/null 2>&1; then
                if mv -f "${old_dir}/geoip.dat" "${PATH_FOR_GEO_IP}" && mv -f "${old_dir}/geosite.dat" "${PATH_FOR_GEO_SITE}"; then
                    LOGE "the core config regenerated by the panel refers to a category missing from the new geo data (most likely a routing rule added by a hot apply); the old data was put back and the panel restarted again"
                    systemctl restart a-ui
                else
                    LOGE "the core config regenerated by the panel does not work with the new geo data and putting the old data back failed too, xray may not start, check ${bin_dir} now"
                fi
                rm -rf "${tmp_dir}"
                return 1
            fi
            # The old data fails as well: the config itself has another problem
            # (e.g. a certificate file is gone), unrelated to this update; keep
            # the new data, but xray is most likely not running, so report failure
            rm -rf "${tmp_dir}"
            LOGE "the core config regenerated by the panel fails the xray check, and the old geo data fails it as well, so the problem is not the updated data (the new data was kept); check a-ui log"
            return 1
        fi
    fi
    rm -rf "${tmp_dir}"
    return ${restart_rc}
}

# root's crontab is only changed through the two functions below: the current
# contents are filtered and piped straight into crontab -, no temp file is
# ever written. The old code wrote the crontab to the fixed path
# /tmp/crontabTask.tmp and then ran crontab on it: /tmp is world-writable, so
# any local user could create that file first (with protected_regular root's
# write to it fails, but the crontab call that follows installs the file
# anyway), and whatever he put in it became root's cron jobs; every "enable"
# also appended another duplicate line.
#
# The marker is the job's command itself (a-ui geo / a-ui clear), so
# duplicates written by older versions of this script are collapsed too. If
# crontab -l fails for a reason other than "no crontab yet", leave it alone:
# otherwise an empty read would overwrite all of root's existing jobs.

# cron_replace_lines <marker> [line...]: drop every line containing the
# marker, then append the given lines.
cron_replace_lines() {
    local marker="$1" current
    shift
    if ! current=$(crontab -l 2>/dev/null); then
        if ! crontab -l 2>&1 | grep -qi 'no crontab'; then
            LOGE "failed to read root's crontab, nothing changed"
            return 1
        fi
        current=""
    fi
    {
        [[ -n "${current}" ]] && printf '%s\n' "${current}" | grep -vF -- "${marker}"
        [[ $# -gt 0 ]] && printf '%s\n' "$@"
    } | crontab -
}

# cron_drop_lines <marker>: drop every line containing the marker; does not
# touch the crontab when there is none.
cron_drop_lines() {
    crontab -l 2>/dev/null | grep -qF -- "$1" || return 0
    cron_replace_lines "$1"
}

enable_auto_update_geo() {
    LOGI "enable automatically update geo data..."
    if cron_replace_lines "a-ui geo" "00 4 */2 * * a-ui geo > /dev/null"; then
        LOGI "enable automatically update geo data succeed"
    else
        LOGE "enable automatically update geo data failed"
    fi
}

disable_auto_update_geo() {
    if cron_drop_lines "a-ui geo"; then
        LOGI "cancel a-ui automatically update geo data succeed"
    else
        LOGE "cancel a-ui automatically update geo data failed"
    fi
}

#clear xray log,need enable log in config template
#here we need input an absolute path for log
clear_log() {
    LOGI "clear xray logs..."
    local filePath=''
    if [[ $# -gt 0 ]]; then
        filePath=$1
    else
        LOGE "invalid file path,will exit"
        exit 1
    fi
    LOGI "log file:${filePath}"
    if [[ ! -f ${filePath} ]]; then
        LOGE "clear xray log failed,${filePath} didn't exist,plz check it"
        exit 1
    fi
    fileSize=$(ls -la ${filePath} --block-size=M | awk '{print $5}' | awk -F 'M' '{print$1}')
    if [[ ${fileSize} -gt ${DEFAULT_LOG_FILE_DELETE_TRIGGER} ]]; then
        rm $1
        if [[ $? -ne 0 ]]; then
            LOGE "clear xray log :${filePath} failed"
        else
            LOGI "clear xray log :${filePath} succeed"
            systemctl restart a-ui
        fi
    else
        LOGI "current size of xray log is:${fileSize}M,smaller that ${DEFAULT_LOG_FILE_DELETE_TRIGGER}M,won't clear"
    fi
}

#enable auto delete log，need file path as
enable_auto_clear_log() {
    LOGI "enable automatically clear xray logs..."
    local accessfilePath=''
    local errorfilePath=''
    accessfilePath=$(cat ${PATH_FOR_CONFIG} | jq .log.access | tr -d '"')
    errorfilePath=$(cat ${PATH_FOR_CONFIG} | jq .log.error | tr -d '"')
    if [[ ! -n ${accessfilePath} && ! -n ${errorfilePath} ]]; then
        LOGI "current configuration didn't set valid logs,will exited"
        exit 1
    fi
    # The marker carries the full path plus the space after it
    # ("a-ui clear <path> "): only the previous job for the same log is
    # replaced and the other log's job is left alone — with a shared
    # "a-ui clear" marker, adding the second job would delete the first one
    # just added; the trailing space keeps access.log from matching
    # access.log.1. The path goes verbatim into root's crontab and cron hands
    # it to sh, so only absolute paths made of plain characters are allowed —
    # a path with spaces, quotes, $ or % would either break the job or become
    # an injection point executed as root.
    local p kind
    for kind in access error; do
        p="${accessfilePath}"
        [[ "${kind}" == error ]] && p="${errorfilePath}"
        if [[ ! -f ${p} ]]; then
            LOGE "${kind}log didn't existed,won't automatically clear it"
        elif [[ ! "${p}" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
            LOGE "log path ${p} contains characters that cannot safely go into a crontab (only letters, digits and . _ - / are supported), won't automatically clear it"
        elif cron_replace_lines "a-ui clear ${p} " "30 4 */2 * * a-ui clear ${p} > /dev/null"; then
            LOGI "enable automatically clear xray log:${p} succeed"
        else
            LOGE "enable automatically clear xray log:${p} failed"
        fi
    done
}

#disable auto dlete log
disable_auto_clear_log() {
    if cron_drop_lines "a-ui clear"; then
        LOGI "cancel  automatically clear xray logs succeed"
    else
        LOGE "cancel  automatically clear xray logs failed"
    fi
}

# Re-run the panel security setup wizard. Useful for retrying after the
# initial install wizard failed (cert not issued, decoy site unreachable),
# or to reconfigure with a different domain/decoy site. The wizard logic
# isn't reimplemented here -- this calls straight into the latest
# install_en.sh's wizard part, so this doesn't drift from install_en.sh.
# --wizard-only skips install_en.sh's setup_wizard idempotency probe and
# forces an overwrite of the existing configuration -- triggering
# "reconfigure" from the menu already means the user wants to overwrite.
reconfig_domain() {
    confirm "this will re-run the setup wizard, overwriting the panel's current base path and listen address, continue" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    bash <(curl -Ls https://raw.githubusercontent.com/SienFeng/AetherUI/main/install_en.sh) --wizard-only
    # The exit code has to be checked: giving up midway (typing q at the
    # decoy-site prompt, or the certificate never arriving) leaves the
    # machine with "panel already on 127.0.0.1 + Caddy possibly stopped",
    # and without this check all of that travels silently back to the main
    # menu while the user only ever sees a final "Cancelled" and assumes
    # nothing happened. The wizard prints the rescue commands itself; this
    # only makes the "it did not finish" part explicit.
    local rc=$?
    if [[ ${rc} -ne 0 ]]; then
        LOGE "the setup wizard did not finish successfully (exit code ${rc}); the panel configuration may not have been updated as expected"
        LOGE "follow the messages above to troubleshoot; if the panel is unreachable, use the recovery commands the wizard printed"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

# show_listens_all tells whether `a-ui setting -show` output means the panel listens on all IPs. With an empty
# listen address current binaries print `Listen: all interfaces` and older ones `Listen: ` (empty); both count.
# Taking only the second column would read the former as `all` and always fail.
show_listens_all() {
    local line listen
    line=$(echo "$1" | grep '^Listen:' | head -1)
    [[ -n "${line}" ]] || return 1
    listen=$(echo "${line}" | sed -e 's/^Listen:[[:space:]]*//' -e 's/[[:space:]]*$//')
    [[ -z "${listen}" || "${listen}" == "all interfaces" ]]
}

# Stop Caddy and restore direct access to the panel. Only stops the
# service, doesn't remove the package -- the user might be running other
# sites on it. The order matters: the panel's listen address must be
# restored and a real health probe (not just process status) must confirm
# direct access actually works, before Caddy is stopped -- doing it the
# other way round leaves a window where the panel is unreachable through
# either path.
#
# The bar can't be just `systemctl status` (whether the process is
# running): main.go's updateSetting only fmt.Println's on a SetListen
# write failure, it doesn't os.Exit -- `a-ui setting` as a whole still
# returns 0. If the write happens to fail (disk full, DB locked), the
# panel restarts fine with the old webListen still in effect, so checking
# only process status would wrongly conclude "fixed" and go on to stop
# Caddy right after -- reproducing exactly the "unreachable through
# either path" situation this feature exists to avoid. So this actually
# fires an HTTP request at the panel's real port and base path instead,
# the same approach as install.sh's wait_for_panel_alive(); but a-ui.sh
# is a standalone script running in its own process after install, it
# doesn't share functions with install.sh, so this just re-implements an
# equivalent check.
restore_direct_panel() {
    confirm "this will stop Caddy and restore direct access to the panel, domain access will stop working immediately, continue" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi

    # A failed write (locked DB, full disk) must stop here: the probe below hits 127.0.0.1, which also
    # succeeds while the panel still listens on loopback only, and Caddy would then be stopped while it
    # is the only way in.
    if ! a_ui_setting -listen ""; then
        LOGE "failed to restore the panel to listen on all IPs; Caddy will not be stopped this time"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    systemctl restart a-ui

    # The panel's port and base path may have both been randomized, don't
    # assume 54321 or "/" -- ask the panel itself. `a-ui setting -show`
    # is a new readonly entry point added along with this change, so this
    # doesn't depend on sqlite3, a tool that isn't guaranteed to be
    # installed (install_base only installs wget/curl/tar/jq).
    local show_out port basepath
    show_out=$(/usr/local/a-ui/a-ui setting -show 2>/dev/null)
    if [[ $? != 0 ]]; then
        LOGE "failed to read the panel's current settings; to avoid locking you out entirely, Caddy will not be stopped this time"
        LOGE "check a-ui log to find out why, then retry this once the panel is reachable directly"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi
    port=$(echo "${show_out}" | grep '^Port:' | awk '{print $2}')
    basepath=$(echo "${show_out}" | grep '^BasePath:' | awk '{print $2}')
    # Confirm it really took (an old binary exits 0 on a failed write): the listen address must be "all IPs".
    if ! show_listens_all "${show_out}"; then
        LOGE "the panel still does not listen on all IPs; Caddy will not be stopped this time"
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
        LOGE "panel health check failed after restart; 127.0.0.1:${port}${basepath} is not responding"
        LOGE "to avoid locking you out entirely, Caddy will not be stopped this time"
        LOGE "check a-ui log to find out why, then retry this once the panel is reachable directly"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    fi

    systemctl stop caddy 2>/dev/null
    systemctl disable caddy 2>/dev/null
    LOGI "Caddy stopped (the package and its config are kept; systemctl enable --now caddy restores it)"
    LOGI "the panel is now reachable directly at http://<this server's IP>:${port}${basepath}"

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_usage() {
    echo "a-ui control menu usages: "
    echo "------------------------------------------"
    echo -e "a-ui              - Enter control menu"
    echo -e "a-ui start        - Start a-ui "
    echo -e "a-ui stop         - Stop  a-ui "
    echo -e "a-ui restart      - Restart a-ui "
    echo -e "a-ui status       - Show a-ui status"
    echo -e "a-ui enable       - Enable a-ui on system startup"
    echo -e "a-ui disable      - Disable a-ui on system startup"
    echo -e "a-ui log          - Check a-ui logs"
    echo -e "a-ui update       - Update a-ui "
    echo -e "a-ui install      - Install a-ui "
    echo -e "a-ui uninstall    - Uninstall a-ui "
    echo "a-ui geo             - Update a-ui geo "
    echo "a-ui cron            - Cron a-ui jobs"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}AetherUI control menu${plain}
  ${green}0.${plain} exit
————————————————
  ${green}1.${plain} install   a-ui
  ${green}2.${plain} update    a-ui
  ${green}3.${plain} uninstall a-ui
————————————————
  ${green}4.${plain} reset username
  ${green}5.${plain} reset panel
  ${green}6.${plain} reset panel port
  ${green}7.${plain} check panel info
————————————————
  ${green}8.${plain} start a-ui
  ${green}9.${plain} stop  a-ui
  ${green}10.${plain} restart a-ui
  ${green}11.${plain} check a-ui status
  ${green}12.${plain} check a-ui logs
————————————————
  ${green}13.${plain} enable  a-ui on system startup
  ${green}14.${plain} disable a-ui on system startup
————————————————
  ${green}15.${plain} enable bbr 
  ${green}16.${plain} issuse certs
  ${green}17.${plain} a-ui cron jobs
————————————————
  ${green}18.${plain} configure domain and decoy site
  ${green}19.${plain} uninstall Caddy and restore direct panel access
 "
    show_status
    echo && read -p "please input a legal number[0-19],input 7 for checking login info:" num

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
        LOGE "please input a legal number[0-19],input 7 for checking login info"
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
