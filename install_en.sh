#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)

# The port the panel is actually listening on right now, used by the
# security setup wizard to build the panel URL. config_after_install fills
# it in whenever it can determine the value; when it can't (e.g. an
# upgrade that keeps the existing port), this stays empty and the wizard
# decides whether to pass -port to bootstrap at all — better to omit it
# than to guess wrong.
panel_port=""

# Set by handle_existing_web_server once it successfully stops a web
# server, so domain_flow can reprint the rollback command if a later step
# (Caddy install / Caddyfile validation) fails — by then the user has
# likely scrolled past the confirmation prompt's hint, and 80/443 really
# have nothing listening on them, which is more urgent than the first hint.
stopped_web_svc=""

# Whether this install/update has already stopped the panel. Any failure
# exit after that point must bring it back up first: a-ui.service's
# Restart=on-failure only covers the process dying on its own — a unit
# stopped with systemctl stop stays stopped — so bailing out here leaves the
# machine with a stopped panel that will never come back on its own — while
# the user assumes "the update failed, so nothing changed", and only finds
# out when someone reports the nodes are down.
a_ui_stopped=0

# Staging directory created by fetch_release (under /usr/local, next to a-ui,
# not /tmp for the same reason as xray_backup_dir below): the downloaded
# package, its checksum file and the extracted tree all land here first, and
# the existing install is only touched once all of it has been verified.
# Empty means not created yet, or already cleaned up.
stage_dir=""

# The old install directory once it has been moved aside (inside stage_dir).
# Any failure exit after the new directory is in place and before the
# management script is installed makes die_restoring_panel move it back
# before restarting the panel: the machine ends up as it was before the
# update instead of on a half-installed new version. Cleared once the
# management script is installed; there is no rollback after that.
old_install_dir=""

# Set to 1 when config_after_install could not move the credentials off the
# default admin/admin; the end of the install repeats the warning, since the
# first one has usually scrolled away behind the wizard's output by then.
default_credentials_active=0

# Path written by backup_xray_assets and consumed by restore_or_install_xray;
# empty means there is no usable backup for this run (a fresh install, or
# /usr/local/a-ui/bin simply didn't exist yet). The two functions don't run
# within the same call — backup happens once inside install_a-ui() right
# before stop, restore happens once more after extraction — so this has to
# be a top-level variable shared across functions, not a local one.
xray_backup_dir=""

# Print an error and exit, restarting the panel if this script stopped it.
# Only touch systemctl when we really did stop it: on a fresh install,
# starting a service that isn't configured yet just adds a failure log line
# unrelated to the actual problem, sending troubleshooting down a dead end.
#
# If the new directory is already in place, move the old install back first
# (see old_install_dir). If that fails, keep the staging directory and print
# its path: it holds the only remaining copy of the old version.
die_restoring_panel() {
    trap - INT TERM HUP
    echo -e "${red}$1${plain}"
    local keep_stage=0
    if [[ -n "${old_install_dir}" ]]; then
        echo -e "${yellow}Restoring the install directory from before the update...${plain}"
        # the current directory is most likely the new one we are about to delete
        cd /usr/local || cd /
        if rm -rf /usr/local/a-ui && mv "${old_install_dir}" /usr/local/a-ui; then
            echo -e "${green}Restored the install directory from before the update${plain}"
        else
            keep_stage=1
            echo -e "${red}Restore failed, the old install directory is kept at ${old_install_dir}, move it back to /usr/local/a-ui by hand${plain}"
        fi
        old_install_dir=""
    fi
    if [[ -n "${stage_dir}" && ${keep_stage} -eq 0 ]]; then
        rm -rf "${stage_dir}"
        stage_dir=""
    fi
    if [[ ${a_ui_stopped} -eq 1 ]]; then
        echo -e "${yellow}The panel was stopped by this install, restarting it...${plain}"
        if systemctl start a-ui; then
            echo -e "${green}Panel restarted${plain}"
        else
            echo -e "${red}Failed to start the panel, please run: systemctl start a-ui${plain}"
        fi
    fi
    exit 1
}

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}Fatal error:${plain}please run this script with root privilege\n" && exit 1

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
    echo -e "${red}check system os failed,please contact with author!${plain}\n" && exit 1
fi

arch=$(arch)

if [[ $arch == "x86_64" || $arch == "x64" || $arch == "amd64" ]]; then
    arch="amd64"
elif [[ $arch == "aarch64" || $arch == "arm64" ]]; then
    arch="arm64"
else
    arch="amd64"
    echo -e "${red}fail to check system arch,will use default arch here: ${arch}${plain}"
fi

echo "架构: ${arch}"

if [ $(getconf WORD_BIT) != '32' ] && [ $(getconf LONG_BIT) != '64' ]; then
    echo "a-ui dosen't support 32bit(x86) system,please use 64 bit operating system(x86_64) instead,if there is something wrong,plz let me know"
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
        echo -e "${red}please use CentOS 7 or higher version${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}please use Ubuntu 16 or higher version${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}please use Debian 8 or higher version${plain}\n" && exit 1
    fi
fi

# openssl is not optional: check_reality_target's four criteria all go
# through openssl s_client, and without it the REALITY branch always
# concludes "target doesn't meet the requirements" — a reason that is
# entirely fabricated. Same for jq: print_result parses bootstrap's JSON
# output with it, and without it the whole "panel address" line silently
# disappears, right after the user moved the panel to a random path.
install_base() {
    if [[ x"${release}" == x"centos" ]]; then
        yum install wget curl tar jq openssl -y
    else
        apt install wget curl tar jq openssl -y
    fi
}

# Run /usr/local/a-ui/a-ui setting and report honestly whether it worked;
# on failure print its output and return 1.
#
# The exit code is the primary signal; but this script always comes from the
# main branch while the binary may be an older, rolled-back version, and old
# binaries exit 0 even when the database write fails, printing only a
# "... failed: ..." line — so look at the output too. Arguments must be
# passed through one by one: unquoted, an empty username makes -password the
# username's value and a value with a space gets split into several words —
# either way the script used to say "set down!" while nothing had changed.
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

# Read the username and password the admin wants. On success store them in
# cred_user / cred_pass and return 0; on EOF (no terminal, SSH dropped) or
# after 3 invalid attempts in a row return 1. Every read's return code is
# checked, otherwise after EOF the loop would spin with no I/O to wait on.
#
# The checks match what the panel actually accepts: not empty; no leading or
# trailing whitespace (IFS= keeps read from silently trimming it — trimmed,
# the admin could not log in with what he typed); a password of at most 72
# bytes, bcrypt's limit (util/crypto.MaxPasswordBytes), counted in bytes, not
# characters — one Chinese character is 3 bytes. The password is not echoed
# and has to be typed twice.
read_panel_credentials() {
    local attempt user pass pass2 pass_bytes
    cred_user=""
    cred_pass=""
    for attempt in 1 2 3; do
        if ! IFS= read -r -p "please set up your username:" user; then
            echo -e "\n${red}input ended, giving up${plain}"
            return 1
        fi
        if [[ -z "${user}" ]]; then
            echo -e "${red}the username cannot be empty${plain}"
            continue
        fi
        if [[ "${user}" =~ ^[[:space:]] || "${user}" =~ [[:space:]]$ ]]; then
            echo -e "${red}the username cannot start or end with a space or other whitespace${plain}"
            continue
        fi
        if ! IFS= read -r -s -p "please set up your password (not shown while typing):" pass; then
            echo -e "\n${red}input ended, giving up${plain}"
            return 1
        fi
        echo ""
        if [[ -z "${pass}" ]]; then
            echo -e "${red}the password cannot be empty${plain}"
            continue
        fi
        if [[ "${pass}" =~ ^[[:space:]] || "${pass}" =~ [[:space:]]$ ]]; then
            echo -e "${red}the password cannot start or end with a space or other whitespace${plain}"
            continue
        fi
        pass_bytes=$(printf '%s' "${pass}" | wc -c | tr -d ' ')
        if [[ ${pass_bytes} -gt 72 ]]; then
            echo -e "${red}the password cannot be longer than 72 bytes (about 24 Chinese characters), this one is ${pass_bytes} bytes${plain}"
            continue
        fi
        if ! IFS= read -r -s -p "please type the password again:" pass2; then
            echo -e "\n${red}input ended, giving up${plain}"
            return 1
        fi
        echo ""
        if [[ "${pass}" != "${pass2}" ]]; then
            echo -e "${red}the two passwords do not match${plain}"
            continue
        fi
        cred_user="${user}"
        cred_pass="${pass}"
        return 0
    done
    echo -e "${red}3 invalid attempts in a row, giving up${plain}"
    return 1
}

print_credentials_hint() {
    echo -e "${yellow}to change them: log in and use Panel Settings -> account, or run on this machine${plain}"
    echo -e "${yellow}  /usr/local/a-ui/a-ui setting -username '<username>' -password '<password>'${plain}"
}

# Whether something is already listening on the TCP port. ss first, netstat
# on old systems without ss (no ss -H: CentOS 7's iproute does not know it).
# Only the port after the last colon of the local-address column is
# compared, so the header line never matches. With neither tool there is no
# way to tell and the port is treated as free — a random port hitting a
# busy one is unlikely, and such a system is outside the supported range.
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

# Pick a random panel port that is free right now; return 1 if none found.
# Range 10000-32767: $RANDOM alone can be 0 or a privileged port below 1024;
# it also stays out of Linux's default ephemeral range (32768-60999), where
# outgoing connections grab ports at any time and the panel might fail to
# bind on its next restart.
pick_free_port() {
    local i port
    for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
        port=$((10000 + RANDOM % 22768))
        if ! port_in_use "${port}"; then
            echo "${port}"
            return 0
        fi
    done
    return 1
}

#This function will be called when user installed a-ui out of sercurity
config_after_install() {
    # Under the new topology the panel port is an internal implementation
    # detail and is no longer asked about here (see the security setup
    # wizard below). But the wizard needs to know the port actually in
    # effect after this run in order to build the panel URL: on a fresh
    # install it is the default 54321, rewritten below if the random branch
    # changes it; on an upgrade the port was never touched here, so the
    # value is unknown and the global panel_port is left unset (leaving it
    # to the caller to probe for the port itself, or simply omit it from the
    # URL).
    local is_fresh_install=1
    [[ -f "/etc/a-ui/a-ui.db" ]] && is_fresh_install=0
    [[ ${is_fresh_install} -eq 1 ]] && panel_port=54321

    # read_panel_credentials writes these two; local here keeps the password out of the globals
    local creds_set=0 cred_user="" cred_pass=""
    echo -e "${yellow}Install/update finished, need to modify the account and password out of security${plain}"
    read -p "are you continue,if you type n will skip this at this time[y/n]": config_confirm
    if [[ x"${config_confirm}" == x"y" || x"${config_confirm}" == x"Y" ]]; then
        if read_panel_credentials; then
            echo -e "${yellow}your username will be:${cred_user}${plain}"
            echo -e "${yellow}initializing,wait some time here...${plain}"
            if a_ui_setting -username "${cred_user}" -password "${cred_pass}"; then
                creds_set=1
                echo -e "${green}account name and password set down!${plain}"
            fi
        fi
        cred_pass=""
        if [[ ${creds_set} -eq 0 ]]; then
            if [[ ${is_fresh_install} -eq 1 ]]; then
                # A fresh install landing here still has the public default
                # admin/admin on 0.0.0.0:54321, and an aborted wizard would
                # leave it that way. Fall back to random credentials, as for n.
                echo -e "${red}the username and password could not be set from your input. so the panel does not run with the public default admin/admin, random ones are generated below${plain}"
            else
                echo -e "${red}the username and password could not be set, the panel keeps the credentials it had before the update${plain}"
                print_credentials_hint
            fi
        fi
    else
        echo -e "${red}cancel...${plain}"
        if [[ ${is_fresh_install} -eq 0 ]]; then
            echo -e "${red} this is your upgrade,will keep old settings. type a-ui then 7 for port/path/username; the password is stored hashed and cannot be shown,type 4 to reset it${plain}"
        fi
    fi

    if [[ ${is_fresh_install} -eq 1 && ${creds_set} -eq 0 ]]; then
        local usernameTemp passwordTemp portTemp random_creds_set=0
        usernameTemp=$(head -c 6 /dev/urandom | base64)
        passwordTemp=$(head -c 6 /dev/urandom | base64)
        a_ui_setting -username "${usernameTemp}" -password "${passwordTemp}" && random_creds_set=1
        if portTemp=$(pick_free_port) && a_ui_setting -port "${portTemp}"; then
            panel_port=${portTemp}
        else
            portTemp=""
        fi
        echo -e "this is a fresh installation,will generate random login info for security concerns:"
        echo -e "###############################################"
        if [[ ${random_creds_set} -eq 1 ]]; then
            echo -e "${green}user name:${usernameTemp}${plain}"
            echo -e "${green}user password:${passwordTemp}${plain}"
        else
            default_credentials_active=1
            echo -e "${red}setting random credentials failed: the panel still uses the public default username admin / password admin!${plain}"
        fi
        if [[ -n "${portTemp}" ]]; then
            echo -e "${red}web port:${portTemp}${plain}"
        else
            echo -e "${red}could not set a random port, the panel still uses the default port 54321${plain}"
        fi
        echo -e "###############################################"
        if [[ ${random_creds_set} -eq 1 ]]; then
            echo -e "${red}write the password above down now: it is stored hashed and cannot be shown again. type a-ui then 7 for port/path/username, or 4 to reset a forgotten password${plain}"
        else
            print_credentials_hint
        fi
    fi
}

# Generate the random string used for the panel's URL base path. This
# guards against internet-wide scanners locating panels in bulk via the
# x-ui-family default path /xui/ — this is one of the core problems this
# rework is meant to solve.
gen_random_path() {
    LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 12
}

# Return the process name occupying the given port; empty means nobody is
# listening on it.
#
# -p is mandatory: it's the flag that makes ss emit the
# users:(("processname",pid=...)) column — without it, ss's last column is
# just the peer address (e.g. 0.0.0.0:*), never a process name, which
# would break every process-name check below (nginx/apache/caddy) and
# always fall through to the "unknown process" branch.
port_user() {
    ss -ltnHp "sport = :$1" 2>/dev/null | grep -oP '(?<=users:\(\(")[^"]+' | head -1
}

# This server's public IP. Returns an empty string when it can't be
# determined; callers must tolerate that — the machine may be behind NAT,
# or outbound access to the probing services may be blocked, and neither
# should fail the installation.
public_ip() {
    curl -fsS -m 10 https://api.ipify.org 2>/dev/null || \
    curl -fsS -m 10 https://ifconfig.me 2>/dev/null || true
}

# The port the panel is currently actually listening on, used to build the
# port number in the URL the wizard prints. Prefers the value
# config_after_install already determined this run; when that's
# unavailable (e.g. --wizard-only triggers the wizard on its own while the
# panel service is already running), falls back to probing the port the
# running a-ui process is actually bound to; if neither works, returns
# failure and the caller omits -port, leaving bootstrap's webPort
# untouched rather than guessing a value that might be wrong.
current_panel_port() {
    if [[ -n "${panel_port}" ]]; then
        echo "${panel_port}"
        return 0
    fi
    local pid port
    # Not using `--value`: that option is only supported by systemd >=230,
    # and this script claims to support CentOS 7 which ships systemd 219,
    # where --value would fail outright as an unknown option.
    # `systemctl show`'s output is always `KEY=VALUE`, so cutting on the
    # equals sign is equivalent and works on older versions too.
    pid=$(systemctl show -p MainPID a-ui 2>/dev/null | cut -d= -f2)
    [[ -z "${pid}" || "${pid}" == "0" ]] && return 1
    port=$(ss -ltnp 2>/dev/null | grep -F "pid=${pid}," | awk '{print $4}' | awk -F: '{print $NF}' | head -1)
    [[ -z "${port}" ]] && return 1
    echo "${port}"
}

# Panel security setup wizard. Any failed step must leave the user still
# able to reach the panel: every failure path leaves webListen untouched,
# so the panel keeps listening on all interfaces. Locking the panel behind
# an unreachable 127.0.0.1 is worse than this feature not existing at all.
#
# When $1 is the literal string "force": skip the idempotency probe below
# (re-run the wizard even if already configured), and pass -force through
# to the later domain_flow/reality_flow `a-ui bootstrap` calls so the new
# configuration actually overwrites the old one. --wizard-only (the a-ui
# menu's "Configure domain and decoy site") uses this value: triggering
# "reconfigure" from the menu means the user's intent is explicitly to
# overwrite, so this idempotency check shouldn't block it.
setup_wizard() {
    local force="$1"
    if [[ "${force}" != "force" ]] && /usr/local/a-ui/a-ui bootstrap -check -json 2>/dev/null | grep -q '"skipped": true'; then
        echo -e "${yellow}Panel already configured, keeping existing settings and skipping the setup wizard${plain}"
        echo -e "${yellow}To reconfigure, after installation run a-ui and choose \"Configure domain and decoy site\"${plain}"
        return 0
    fi

    echo -e ""
    echo -e "${green}=== Panel security setup ===${plain}"
    echo -e "With a domain, this will automatically request a certificate, set up a Caddy decoy site, and hide the panel behind 443."
    echo -e "Without a domain, this will set up VLESS+Vision+REALITY, borrowing a major site's certificate as a disguise."
    echo -e ""

    local has_domain
    read -p "Do you have a domain pointing to this server? [y/n]: " has_domain
    if [[ x"${has_domain}" == x"y" || x"${has_domain}" == x"Y" ]]; then
        domain_flow "${force}"
    else
        reality_flow "${force}"
    fi
}

# Candidate REALITY masquerade targets. The four criteria (TLS1.3 / ALPN h2
# / X25519-family key exchange / a valid certificate) are documented in the
# comment at web/assets/js/model/xray.js:78, confirmed by real testing on
# 2026-09-03. Domains' TLS configuration changes over time, so this needs
# to be retested periodically.
REALITY_TARGETS=(
    "www.lovelive-anime.jp"
    "www.amazon.co.jp"
    "www.tesla.com"
    "www.cloudflare.com"
    "www.nicovideo.jp"
)

# Check whether a candidate target meets REALITY's requirements. Returns
# non-zero if any criterion fails.
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

    # Port detection moved to the very top: every failure branch below
    # needs it to build the SSH-tunnel port in the rescue hint on a force
    # rerun. Omit -port when it can't be determined: better to leave
    # bootstrap's webPort untouched than to silently change the user's
    # existing port with a guessed, possibly wrong, value.
    local port_args=()
    local detected_port
    if detected_port=$(current_panel_port); then
        port_args=(-port "${detected_port}")
    fi

    # A REALITY inbound always takes port 443, and neither existing guard
    # can see a port conflict: AddInbound's checkPortExist only looks at
    # the panel's own inbound table, and ValidateInboundReplacing runs
    # `xray run -test`, which never binds a port. So the inbound gets
    # stored anyway, and the next RestartXray fails to bind 443 — the
    # whole xray process won't come up, every existing inbound on the
    # machine goes down with it, and the panel's home page still says
    # running. Catch it here, the same way domain_flow handles 80/443.
    local occupant443
    occupant443=$(port_user 443)
    if [[ -n "${occupant443}" ]]; then
        echo -e "${red}Port 443 is already in use by ${occupant443}; a REALITY inbound cannot use it${plain}"
        echo -e "${yellow}Stop whatever is holding it (e.g. systemctl stop ${occupant443}) and run the wizard again${plain}"
        echo -e "${yellow}Skipping the setup wizard${plain}"
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

    echo -e ""
    echo -e "${green}Choose a REALITY masquerade target:${plain}"
    local i=1
    for t in "${REALITY_TARGETS[@]}"; do
        echo "  ${i}) ${t}"
        i=$((i + 1))
    done
    echo "  ${i}) Enter a custom domain"

    local choice target
    read -p "Enter a number: " choice
    if [[ "${choice}" == "${i}" ]]; then
        read -p "Enter the masquerade target domain (no port): " target
    elif [[ "${choice}" =~ ^[0-9]+$ ]] && [[ "${choice}" -ge 1 ]] && [[ "${choice}" -lt "${i}" ]]; then
        target="${REALITY_TARGETS[$((choice - 1))]}"
    else
        echo -e "${red}Invalid choice, skipping the setup wizard${plain}"
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

    echo -e "Checking whether ${target} meets REALITY's requirements (TLS1.3 / ALPN h2 / X25519)…"
    if ! check_reality_target "${target}"; then
        echo -e "${red}${target} doesn't meet the requirements, please choose a different target${plain}"
        # "The panel keeps its default configuration" only holds for a
        # first-time setup. force means this is a reconfigure: the panel's
        # listen address and base path were already changed by an earlier
        # successful run, so saying "default configuration" here is active
        # false reassurance.
        if [[ "${force}" == "force" ]]; then
            echo -e "${yellow}Skipping the setup wizard; nothing was changed this time (the previous configuration still applies)${plain}"
        else
            echo -e "${yellow}Skipping the setup wizard; the panel keeps its default configuration and can be reconfigured later by running a-ui${plain}"
        fi
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi
    echo -e "${green}Check passed${plain}"

    local basepath
    basepath="/$(gen_random_path)/"

    local force_args=()
    [[ "${force}" == "force" ]] && force_args=(-force)

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode reality \
            -reality-dest "${target}:443" -basepath "${basepath}" "${port_args[@]}" \
            "${force_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}Configuration failed: ${out}${plain}"
        echo -e "${yellow}The panel keeps its default configuration and remains reachable${plain}"
        # Usually true (the line above is accurate) — but if this is a
        # reconfigure that overwrites an existing setup (e.g. switching back
        # from domain mode to no-domain mode), webListen may already have
        # been set to 127.0.0.1 by an earlier successful run, and this
        # failed bootstrap call won't undo that. The rescue commands must
        # still be printed — better than betting once on "usually fine" and
        # swallowing the one case where it wasn't.
        print_rescue_hint "${detected_port}"
        return 1
    fi
    print_result "${out}" "reality" "${detected_port}"
}

# The single source of the two rescue commands, shared by print_result and
# every failure branch after the bootstrap call point — this is the last
# safety net of the "a failure must never lock the panel out" principle;
# duplicating this logic in two places would only drift. $1 is the panel
# port: pass it when known, empty otherwise — falls back to "check the port
# via menu option 7 first" instead of fabricating a number that might be wrong.
print_rescue_hint() {
    local port="$1"
    echo -e ""
    echo -e "${yellow}If the panel is unreachable, recover with either:${plain}"
    echo -e "  /usr/local/a-ui/a-ui setting -listen \"\"       # restore listening on all interfaces"
    if [[ -n "${port}" ]]; then
        echo -e "  ssh -L ${port}:127.0.0.1:${port} root@<server-IP>     # or an SSH tunnel"
    else
        # Can't fabricate a specific port number when detection failed —
        # a wrong guess would make this lifeline useless.
        echo -e "  SSH tunnel: first use a-ui menu option 7 to check the panel's actual port, then run"
        echo -e "  ssh -L <port>:127.0.0.1:<port> root@<server-IP>"
    fi
}

# Used by failure branches **before** the bootstrap call point, in place
# of an unconditional print_rescue_hint.
#
# The criterion is "is this a force rerun": force means the wizard already
# configured this panel successfully once, so webListen is certainly
# 127.0.0.1 by now and giving up midway does not change it back. Worse, on
# a rerun handle_existing_web_server may already have stopped Caddy (our
# own), leaving the panel completely unreachable from outside while the
# last line on screen may just say "Cancelled". On a fresh install (not
# force) webListen is still the default and the panel keeps listening on
# every interface, so printing these two commands is only noise.
#
# No attempt is made to tell "did this particular branch actually change
# anything": on a rerun, the state left behind by the previous run is not
# visible from here, and two extra lines of output cost far less than an
# unreachable panel.
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

    # "{SERVER_IP}" in panelUrl is a placeholder bootstrap leaves for this
    # script to fill in (the panel process doesn't know its own public
    # IP). Note: this placeholder is a literal string hardcoded in the Go
    # binary (bootstrap/bootstrap.go) and is not localized, so it must be
    # matched/replaced verbatim here too — translating it would break both
    # the substring match below and the actual text already inside ${url}.
    # When the IP can't be detected, keep the placeholder and print an
    # extra hint instead of silently showing an address that looks fine
    # but doesn't actually work.
    if [[ "${url}" == *"{SERVER_IP}"* ]]; then
        local ip
        ip=$(public_ip)
        [[ -n "${ip}" ]] && url="${url//\{SERVER_IP\}/${ip}}"
    fi

    echo -e ""
    echo -e "${green}=== Setup complete ===${plain}"
    if [[ -n "${url}" ]]; then
        echo -e "${green}Panel URL: ${url}${plain}"
    fi
    if [[ "${url}" == *"{SERVER_IP}"* ]]; then
        echo -e "${yellow}(Could not auto-detect this server's public IP; please replace {SERVER_IP} in the address with your server's public IP)${plain}"
    fi
    if [[ "${url}" == *":0/"* ]]; then
        echo -e "${yellow}(Could not determine the panel's current listening port; use a-ui menu option 7 to check the actual port)${plain}"
    fi
    if [[ "${mode}" == "reality" ]]; then
        echo -e "${green}A VLESS+Vision+REALITY inbound has been created (port 443); log in to the panel to view the share link and QR code${plain}"
    fi
    print_rescue_hint "${port}"
    echo -e ""
    echo -e "${yellow}Note: this setup hides the panel only. Inbound ports you create in the panel remain exposed,${plain}"
    echo -e "${yellow}with the same resistance to detection as before this setup.${plain}"
}

# Don't abort outright when 80/443 are occupied: on machines already set
# up by some other one-click script, the panel is often exposed exactly in
# plaintext HTTP — precisely the situation this rework needs to fix most.
#
# Stop rather than uninstall: freeing the ports only requires stopping the
# service; apt remove buys nothing extra besides being irreversible. If
# the user changes their mind, one `systemctl enable --now` brings it back.
handle_existing_web_server() {
    local occupant="$1"
    local svc="" confdir=""
    case "${occupant}" in
        *nginx*)  svc="nginx";  confdir="/etc/nginx" ;;
        *apache*|*httpd*) svc="apache2"; confdir="/etc/apache2"
                  [[ -d /etc/httpd ]] && svc="httpd" && confdir="/etc/httpd" ;;
        *caddy*)  svc="caddy";  confdir="/etc/caddy" ;;
        *)
            echo -e "${red}80/443 are occupied by an unknown process: ${occupant}${plain}"
            echo -e "${red}Please deal with it yourself before running this script again${plain}"
            return 1 ;;
    esac

    echo -e ""
    echo -e "${yellow}${svc} is currently using ports 80/443, currently serving the following site(s):${plain}"
    if [[ "${svc}" == "nginx" ]]; then
        grep -rhE "^\s*(server_name|root)\s" "${confdir}" 2>/dev/null | sed 's/^/    /' || \
            echo "    (unable to parse the configuration, please check it yourself)"
    else
        echo "    (please check the site configuration under ${confdir} yourself)"
    fi

    local backup="/root/${svc}-backup-$(date +%Y%m%d-%H%M%S).tar.gz"
    tar czf "${backup}" "${confdir}" 2>/dev/null && \
        echo -e "${green}Config backed up to ${backup}${plain}"

    echo -e ""
    echo -e "${red}Continuing will stop ${svc}; the site(s) above will become unreachable immediately.${plain}"
    echo -e "${yellow}Rollback command: systemctl enable --now ${svc}${plain}"
    local confirm
    read -p "Type the full word yes to confirm stopping it (anything else cancels): " confirm
    if [[ "${confirm}" != "yes" ]]; then
        echo -e "${yellow}Cancelled${plain}"
        return 1
    fi

    # A stop failure must be reported truthfully and abort — don't swallow
    # the error and keep going: 80/443 are still occupied at this point,
    # install_caddy/write_caddyfile will fail downstream, but the error
    # would point at Caddy instead of the real cause here, which is very
    # misleading.
    local stop_err
    if ! stop_err=$(systemctl stop "${svc}" 2>&1); then
        echo -e "${red}Failed to stop ${svc}; 80/443 may still be occupied, please check and retry:${plain}"
        echo -e "${red}${stop_err}${plain}"
        return 1
    fi
    local disable_err
    if ! disable_err=$(systemctl disable "${svc}" 2>&1); then
        echo -e "${yellow}${svc} has been stopped, but disabling it at boot failed; it may re-occupy 80/443 after a reboot:${plain}"
        echo -e "${yellow}${disable_err}${plain}"
    fi
    stopped_web_svc="${svc}"
    echo -e "${green}${svc} has been stopped (its package and config files are kept)${plain}"
    return 0
}

# If install_caddy/write_caddyfile fails after an old web server was
# already stopped above, nothing is listening on 80/443 at all at this
# point, so the rollback command must be reprinted.
print_stopped_svc_rollback_hint() {
    [[ -z "${stopped_web_svc}" ]] && return 0
    echo -e "${yellow}${stopped_web_svc} has already been stopped; nothing is currently listening on 80/443${plain}"
    echo -e "${yellow}To restore the old site first: systemctl enable --now ${stopped_web_svc}${plain}"
}

# If the web server handle_existing_web_server stopped was our own Caddy
# (on a machine that already ran this wizard, that is exactly what holds
# port 80), it has to be brought back before the wizard bails out midway:
# the panel is most likely listening on 127.0.0.1 only by then, so with
# Caddy down it disappears from the internet entirely — while the last
# line on screen may just say "Cancelled".
#
# Only Caddy is treated this way. nginx/apache are the user's own
# property, and the user explicitly confirmed stopping them moments ago;
# restarting them unasked would override that confirmation.
restart_own_caddy_if_stopped() {
    [[ "${stopped_web_svc}" != "caddy" ]] && return 0
    echo -e "${yellow}Restarting the Caddy that was stopped earlier, to restore external access to the panel and the decoy site…${plain}"
    if systemctl start caddy 2>/dev/null; then
        echo -e "${green}Caddy restarted${plain}"
        stopped_web_svc=""
        return 0
    fi
    echo -e "${red}Failed to restart Caddy; please run: systemctl start caddy${plain}"
    return 1
}

# The apt branch's commands have been verified on Ubuntu 20.04.6 aarch64
# (Caddy 2.11.4). The script runs as root (there's an EUID check up top),
# so sudo isn't added.
#
# Both return-0 branches must enable it: even if Caddy is already
# installed but was manually stopped/disabled (which is the state of the
# real-machine verification environment), it still needs to be brought
# under boot-time management — otherwise after a reboot a-ui comes back up
# via systemd but Caddy doesn't, nothing listens on 80/443, and the panel
# (already confined to 127.0.0.1) and the decoy site both vanish from the
# outside world together — more hidden than the setup wizard itself
# failing.
install_caddy() {
    if command -v caddy &>/dev/null; then
        echo -e "${green}Caddy is already installed: $(caddy version | head -1)${plain}"
        systemctl enable caddy 2>/dev/null
        return 0
    fi
    echo -e "Installing Caddy…"
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
        echo -e "${red}Failed to install Caddy${plain}"
        echo -e "${yellow}Please install it manually and retry: https://caddyserver.com/docs/install${plain}"
        return 1
    fi
    echo -e "${green}Caddy installed: $(caddy version | head -1)${plain}"
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

# Extract the host from a URL (no port, and doesn't distinguish a bare
# domain from its www subdomain), handling both https://host/path and
# https://host:port/path forms, for check_mask_site to decide whether a
# redirect is genuinely cross-domain — normalizing host to www.host
# doesn't count as changing domains.
_mask_site_url_host() {
    local u="${1#*://}"
    u="${u%%/*}"
    u="${u%%:*}"
    u="${u,,}"
    echo "${u#www.}"
}

# Five ways this check fails: the address carries a path; the root path
# redirects at all; a non-2xx status code; an unknown path redirects
# cross-domain; or the site is blocked by Cloudflare. A prober seeing a
# broken mirror site is far more suspicious than seeing a plain static page.
#
# On success this prints nothing and returns 0 — the caller reverse
# proxies exactly the URL it passed in. On failure, stdout carries the
# rejection reason. There is no "follow one hop and return the resolved
# address" logic any more: Caddy's reverse_proxy upstream only accepts
# scheme/host/port, while curl's %{redirect_url} almost always yields an
# absolute URL with at least a "/", so that branch could never have
# produced a config that passes caddy validate.
check_mask_site() {
    local url="$1"
    local ua="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    local resp code redirect headers after_scheme probe_url probe_resp probe_code probe_redirect

    # Reject an address with a path here rather than letting it fail in
    # write_caddyfile — by then the old web server may already be stopped
    # and nothing is listening on 80/443. Verified on Caddy 2.11.4:
    # reverse_proxy https://www.python.org/ → for now, URLs for proxy
    # upstreams only support scheme, host, and port components.
    # A trailing slash is legitimate input (it is what the browser address bar
    # hands you), so normalise it away rather than reject it. Normalising is not
    # optional though: caddy 2.11.4 rejects `reverse_proxy https://host/` with the
    # very same "upstreams only support scheme, host, and port" error, so the glob
    # is */* rather than */?* — the latter lets a trailing slash through and defers
    # the failure all the way to write_caddyfile.
    url="${url%/}"
    after_scheme="${url#*://}"
    if [[ "${after_scheme}" == */* ]]; then
        echo "the reverse-proxy target must not carry a path (Caddy upstreams only accept scheme/host/port)"
        return 1
    fi

    resp=$(curl -sS -o /dev/null -m 10 -A "${ua}" \
                -w '%{http_code} %{redirect_url}' "${url}" 2>/dev/null) || {
        echo "unable to connect"
        return 1
    }
    code=$(echo "${resp}" | awk '{print $1}')
    redirect=$(echo "${resp}" | awk '{print $2}')

    # Any redirect on the root path is rejected, same-domain ones included.
    # Two independently sufficient reasons:
    #   1. The redirect target carries a path, which cannot be written into
    #      a Caddy upstream (see above).
    #   2. If the reverse-proxy target itself redirects, Caddy forwards the
    #      upstream's Location header verbatim to visitors, whose browsers
    #      jump straight to the real decoy source — the address bar turns
    #      from the user's domain into someone else's, blowing the disguise
    #      on the spot.
    # Without -L, a 3xx response's http_code is itself non-2xx, so this
    # check must come before the status-code check or the rejection reason
    # would degrade into an uninformative "HTTP 301". Every preset
    # candidate was verified on 2026-09-04 from a Tokyo datacenter IP to
    # return 200 directly on the root path with no redirect, so this check
    # does not reject any of them.
    if [[ -n "${redirect}" ]]; then
        echo "root path redirects to ${redirect}"
        return 1
    fi

    if [[ ! "${code}" =~ ^2 ]]; then
        echo "HTTP ${code}"
        return 1
    fi

    # Unknown-path probe. Checking only the root path is nowhere near
    # enough: a decoy site is visited mostly on non-root paths. Verified:
    # www.wikipedia.org returns 200 on the root path but answers any
    # unknown path with a cross-domain 301 (location:
    # https://en.wikipedia.org/<original path>), which Caddy forwards
    # verbatim — one response that simultaneously says "this is not that
    # site's domain" and "it is bare-proxying that site".
    #
    # Only **cross-domain** redirects are rejected: same-domain absolute
    # redirects are covered by the header_down Location rewrite in
    # write_caddyfile (host swapped for the visitor's domain, path
    # unchanged, so the upstream still serves that path and nothing loops);
    # a cross-domain redirect, once rewritten, would come back to our own
    # domain, get proxied upstream again and redirect again — a redirect
    # loop — so it has to be rejected right here.
    #
    # A failed probe and a passing probe are two different things: warn and
    # skip the check rather than passing or rejecting, the same way the
    # Cloudflare check below behaves.
    probe_url="${url%/}/aui-probe-$(gen_random_path)/"
    probe_resp=$(curl -sS -o /dev/null -m 10 -A "${ua}" \
                     -w '%{http_code} %{redirect_url}' "${probe_url}" 2>/dev/null) || {
        echo -e "${yellow}Warning: could not complete the unknown-path probe, skipping it${plain}" >&2
        probe_resp=""
    }
    if [[ -n "${probe_resp}" ]]; then
        probe_code=$(echo "${probe_resp}" | awk '{print $1}')
        probe_redirect=$(echo "${probe_resp}" | awk '{print $2}')
        if [[ "${probe_code}" =~ ^3 ]] && [[ -n "${probe_redirect}" ]] && \
           [[ "$(_mask_site_url_host "${url}")" != "$(_mask_site_url_host "${probe_redirect}")" ]]; then
            echo "an unknown path returns a cross-domain redirect (${probe_code} → ${probe_redirect}), which would blow the disguise once proxied"
            return 1
        fi
    fi

    # A successful first GET probe doesn't guarantee this HEAD request
    # succeeds too (some sites/WAFs treat HEAD differently); a failed
    # probe and a probe result of "not blocked" are two different things
    # and must not both be treated as a pass without distinction — that
    # would silently swallow a genuine probe failure.
    headers=$(curl -sSI -m 10 -A "${ua}" "${url}" 2>/dev/null) || {
        echo -e "${yellow}Warning: could not complete the Cloudflare-block check, skipping it${plain}" >&2
        return 0
    }
    if echo "${headers}" | grep -qi "cf-mitigated"; then
        echo "blocked by Cloudflare"
        return 1
    fi
    return 0
}

# Candidates come from real-machine testing (2026-09-04, from a Tokyo
# datacenter IP): 2xx status code, no redirect, no Cloudflare block.
# Testing must be done from a datacenter IP — results from a residential
# IP don't count; it's very common for the same site to work fine from a
# residential IP while a datacenter IP gets a 403 or a CAPTCHA. Site
# policies change over time, so this needs to be retested periodically.
#
# Already tested and rejected — don't add these back:
#   - gnu.org (can't connect)
#   - tesla.com (403). Note it is only unusable as a **reverse-proxy
#     target**; it is perfectly fine as a REALITY dest — the two have
#     different criteria (REALITY is raw TCP passthrough, it never sends
#     an HTTP request).
#   - www.wikipedia.org (verified 2026-09-04): the root path is a clean
#     200, but any unknown path returns a cross-domain 301 →
#     https://en.wikipedia.org/<original path>. Caddy forwards that
#     Location verbatim to visitors — one response that says both "this
#     is not that site's domain" and "it is bare-proxying that site",
#     blowing the disguise on the spot, and a decoy site is visited mostly
#     on non-root paths.
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

# Outputs the selected URL on stdout; an empty string means use the local
# static page; a non-zero return means the user gave up (explicit q, or a
# read on /dev/tty failing — no controlling terminal, or an SSH session
# dropping mid-way, both land here; both read calls must check their
# return code and return, otherwise choice stays unchanged and the next
# loop iteration immediately triggers the same failure again, spinning
# forever with no actual I/O wait). All prompts go to stderr — the caller
# captures stdout via $(...) as the URL. read explicitly reads from
# /dev/tty: this function runs inside a command-substitution subshell, and
# without this it can't read terminal input.
choose_mask_site() {
    echo -e "" >&2
    echo -e "${green}Choose a decoy site:${plain}" >&2
    local i=1
    for s in "${MASK_SITES[@]}"; do
        echo "  ${i}) ${s}" >&2
        i=$((i + 1))
    done
    echo "  ${i}) Enter a custom URL" >&2
    echo "  0) No proxy, use the bundled static page" >&2
    echo "  q) Cancel" >&2

    local choice url reason
    while true; do
        if ! read -p "Enter a number: " choice </dev/tty; then
            echo -e "${red}Input ended, cancelling${plain}" >&2
            return 1
        fi
        if [[ "${choice}" == "q" || "${choice}" == "Q" ]]; then
            echo -e "${yellow}Cancelled${plain}" >&2
            return 1
        elif [[ "${choice}" == "0" ]]; then
            ensure_static_site
            echo ""
            return 0
        elif [[ "${choice}" == "${i}" ]]; then
            if ! read -p "Enter a URL (including https://): " url </dev/tty; then
                echo -e "${red}Input ended, cancelling${plain}" >&2
                return 1
            fi
            # Strip the trailing slash: it is what the browser address bar gives
            # you, and Caddy upstreams reject even a bare trailing slash. It has
            # to happen here — the normalisation inside check_mask_site only
            # touches its own local copy, while what reaches write_caddyfile is
            # the value echoed below.
            url="${url%/}"
        elif [[ "${choice}" =~ ^[0-9]+$ ]] && [[ "${choice}" -ge 1 ]] && [[ "${choice}" -lt "${i}" ]]; then
            url="${MASK_SITES[$((choice - 1))]}"
        else
            echo -e "${red}Invalid number${plain}" >&2
            continue
        fi

        echo -e "Testing whether ${url} can be proxied from this server…" >&2
        if reason=$(check_mask_site "${url}"); then
            # check_mask_site prints nothing when it passes: the
            # reverse-proxy target can only be the address the user picked
            # (already proven to answer 2xx directly on the root path with
            # no redirect), and Caddy upstreams only accept
            # scheme/host/port anyway.
            echo -e "${green}available${plain}" >&2
            echo "${url}"
            return 0
        fi
        # Don't silently fall back to the static page: that would make the
        # user think they're disguised as some site when they're not.
        echo -e "${red}${url} unavailable (${reason}), pick another${plain}" >&2
    done
}

# See spec §7 for the Caddyfile structure. Two key points:
#   1. The panel's reverse-proxy path must exactly match the webBasePath
#      written into the panel, or static assets will 404
#   2. The decoy site sits in the last handle, as the fallback
# Caddy occupies 80/443 directly; the certificate and the 80→443 redirect
# both rely on its automatic HTTPS, so https_port / bind aren't needed —
# verified by Task 0.
#
# The BEGIN/END lines around the site block are what a-ui.sh's uninstall
# looks for: it removes only what lies between them, so other sites the admin
# adds to this file later are left alone. The marker lines are identical in
# the Chinese and English scripts; change them together with the strings
# a-ui.sh / a-ui_en.sh match on.
write_caddyfile() {
    local domain="$1" basepath="$2" panel_port="$3" mask_url="$4"
    local mask_block

    if [[ -n "${mask_url}" ]]; then
        # header_down Location is the disguise's defence in depth.
        # check_mask_site already rejects sites that redirect on the root
        # path or on an unknown path, but it only probes those two paths,
        # and site policies change. The moment an upstream answers some
        # path with an absolute Location, Caddy forwards it to the visitor
        # verbatim (caddyserver/caddy#1011, #5141), the browser jumps to
        # the real decoy source, and the address bar turns from the user's
        # domain into someone else's — disguise blown on the spot.
        #
        # Swap the host of any absolute address for {host} (the domain in
        # the visitor's own request), leaving the path alone; a relative
        # address (Location: /foo) carries no domain, so the regex does not
        # match and it is left untouched. This does not match only the
        # upstream's own domain: in the real incident www.wikipedia.org
        # redirected to en.wikipedia.org, which pinning to the upstream
        # host would never have caught.
        #
        # The cost is that if an upstream genuinely wants to send visitors
        # to a third-party site, the rewrite turns it into a same-path
        # request on our own domain, proxied upstream again, redirected
        # again — a redirect loop. That is a deliberate trade: a page that
        # looks broken beats a single response that proves the whole
        # disguise is a proxy.
        mask_block="        reverse_proxy ${mask_url} {
            header_up Host {upstream_hostport}
            header_down Location \"^https?://[^/]+(/.*)?\$\" \"https://{host}\$1\"
        }"
    else
        mask_block="        root * /var/www/html
        file_server"
    fi

    cat > /etc/caddy/Caddyfile <<EOF
# BEGIN a-ui managed site: ${domain}
# Generated by the a-ui setup wizard. Uninstalling a-ui removes only what lies between the BEGIN/END lines; re-running the wizard (a-ui menu "configure domain and decoy site") rewrites this whole file, sites outside the markers included (the wizard backs up /etc/caddy to /root before stopping Caddy).
${domain} {
    handle ${basepath}* {
        reverse_proxy 127.0.0.1:${panel_port}
    }

    handle {
${mask_block}
    }
}
# END a-ui managed site
EOF

    if ! caddy validate --config /etc/caddy/Caddyfile 2>&1; then
        echo -e "${red}The generated Caddyfile failed validation${plain}"
        return 1
    fi
    systemctl restart caddy || return 1
    return 0
}

# Caddy requests certificates asynchronously — starting successfully
# doesn't mean the certificate is ready yet.
wait_for_cert() {
    local domain="$1"
    local i
    echo -e "Waiting for certificate issuance (up to 60s)…"
    for i in $(seq 1 30); do
        if curl -fsS -m 5 --resolve "${domain}:443:127.0.0.1" \
                "https://${domain}/" -o /dev/null 2>/dev/null; then
            echo -e "${green}Certificate is ready${plain}"
            return 0
        fi
        sleep 2
    done
    echo -e "${red}Could not reach it over HTTPS within 60 seconds; the certificate may not be issued yet${plain}"
    echo -e "${yellow}Troubleshoot: journalctl -u caddy -n 50${plain}"
    return 1
}

# a-ui.service is Type=simple: a successful `systemctl restart` only means
# the process was forked, not that it didn't immediately exit (e.g. leftover
# webCertFile/webKeyFile pointing at a certificate file that no longer
# exists — tls.LoadX509KeyPair fails, Server.Start() returns an error, and
# older main.go just logs a line and returns with exit code 0, which
# Restart=on-failure does not restart). This "came up and immediately died"
# case can't be seen from systemctl's return code, so it must be probed for
# real.
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

# Sync the certificate Caddy manages to a fixed path. The panel and each
# inbound use the fixed path /root/cert/, while Caddy's own certificate
# storage path embeds the ACME CA's directory name, which changes when the
# issuing CA is switched — pointing directly at it would silently break
# after some renewal.
#
# Task 0 already confirmed by testing that Caddy's event hooks aren't
# usable (the events.handlers.exec module isn't registered; caddy validate
# errors out immediately), so a systemd timer is the only option.
install_cert_sync() {
    local domain="$1"
    mkdir -p /root/cert

    cat > /usr/local/bin/a-ui-cert-sync <<'SYNCEOF'
#!/usr/bin/env bash
# Sync the certificate Caddy manages to a fixed path. The panel and each
# inbound both read these two files — Caddy's own storage path embeds the
# ACME CA's directory name, which changes when the issuing CA is switched,
# so it can't be referenced directly.
set -euo pipefail
domain="$1"

# /var/lib/caddy not existing at all is a perfectly normal state (a
# user-installed Caddy may keep its data in /root/.local/share/caddy, or
# Caddy may not have created a certificate store yet). Without this guard,
# under set -euo pipefail the failing find makes the whole run fail and
# systemd records it as failed — a normal state reported as a fault.
[[ -d /var/lib/caddy ]] || exit 0

# Locate the newest certificate directory by mtime first, then take both
# the cert and the key from **that same directory**. Two independent
# `find | head -1` calls are a trap: when a Caddy renewal falls back from
# Let's Encrypt to ZeroSSL, a second CA directory appears under
# certificates/ and the two finds may land in different directories — at
# best cert and key no longer match, at worst the same stale directory
# keeps winning, cmp reports "unchanged" every time, and the certificate
# in /root/cert/ stays frozen until it expires. That is the same shape as
# the "acme.sh renewed fine but nginx never picked up the new cert"
# incident recorded in spec §8, with a different mechanism.
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
        logger -t a-ui-cert-sync "Certificate synced to /root/cert"
    fi
fi

# The expiry check must run unconditionally, not only when something was
# actually copied: the freezing scenario above shows up precisely as
# **nothing changing**, so a check on the copy path would never catch it.
# It only warns and never tries to fix anything — renewal is Caddy's job
# and this script has no business deciding otherwise.
if command -v openssl >/dev/null 2>&1 && [[ -s /root/cert/fullchain.cer ]]; then
    if ! openssl x509 -in /root/cert/fullchain.cer -checkend 86400 -noout >/dev/null 2>&1; then
        logger -t a-ui-cert-sync "WARNING: /root/cert/fullchain.cer expires within 24 hours or has already expired; check Caddy's certificate renewal"
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
        echo -e "${yellow}Warning: could not enable the certificate sync timer; renewed certificates will not be synced to /root/cert automatically${plain}"
    fi
    # Run it once immediately, don't wait for the first period. The exit
    # code has to be reported honestly: the caller decides from it whether
    # to write the /root/cert/ paths into the panel's "default paths for
    # new inbounds" — writing them after a failed sync would leave the
    # admin with a default that inbound validation is guaranteed to reject.
    systemctl start a-ui-cert-sync.service || return 1
    return 0
}

# Detect whether acme.sh is also managing the domain that was just set up.
# This is common for users migrating from x-ui + acme.sh: acme.sh's cron
# keeps renewing the certificate for this domain, and its reloadcmd is very
# likely pointing at the nginx this wizard just stopped — the renewal
# request itself (http-01/dns-01) doesn't depend on nginx running and
# usually still succeeds, but the reloadcmd step will then fail; it also
# leaves two ACME clients redundantly managing the same domain. This only
# warns, it never touches the user's crontab or acme.sh config on its own —
# those are the user's property, and the script has no business deciding
# this for them.
check_acme_conflict() {
    local domain="$1"
    local acme_dir="${HOME:-/root}/.acme.sh"
    [[ -d "${acme_dir}" ]] || return 0
    local domain_conf
    domain_conf=$(find "${acme_dir}" -maxdepth 2 -type f -name "${domain}.conf" 2>/dev/null | head -1)
    [[ -z "${domain_conf}" ]] && return 0

    echo -e ""
    echo -e "${yellow}Detected that acme.sh is also managing the certificate for ${domain} (${domain_conf})${plain}"
    echo -e "${yellow}The certificate is now requested and renewed automatically by Caddy, so acme.sh renewing${plain}"
    echo -e "${yellow}the same domain is redundant, and if its reloadcmd points at a service that was just${plain}"
    echo -e "${yellow}stopped, renewal will error out. Pick one:${plain}"
    echo -e "${yellow}  1) Stop acme.sh's renewal for this domain: crontab -e and delete the acme.sh line${plain}"
    echo -e "${yellow}  2) Or keep acme.sh, but change its reloadcmd to reload Caddy instead:${plain}"
    echo -e "${yellow}     ${acme_dir}/acme.sh --install-cert -d ${domain} --reloadcmd \"systemctl reload caddy\"${plain}"
    echo -e "${yellow}This wizard will not modify your crontab or acme.sh configuration on its own.${plain}"
}

domain_flow() {
    local force="$1"

    # The panel's current port: on a force rerun, giving up midway can
    # leave the panel unreachable on 127.0.0.1, and the SSH tunnel in the
    # rescue hint needs the real port. Leave it empty when detection
    # fails — print_rescue_hint then degrades to "check the port via menu
    # option 7" rather than fabricating a number.
    local detected_port=""
    detected_port=$(current_panel_port) || detected_port=""

    local domain
    read -p "Enter your domain: " domain
    if [[ -z "${domain}" ]]; then
        echo -e "${red}Domain cannot be empty${plain}"
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    fi

    # DNS validation only warns, it doesn't block: the domain might sit
    # behind a CDN, or only have an AAAA record, and a hard block would
    # wrongly reject valid setups.
    local resolved myip
    resolved=$(getent ahostsv4 "${domain}" 2>/dev/null | awk '{print $1; exit}')
    myip=$(public_ip)
    if [[ -n "${resolved}" && -n "${myip}" && "${resolved}" != "${myip}" ]]; then
        echo -e "${yellow}Warning: ${domain} resolves to ${resolved}, which doesn't match this server's public IP ${myip}${plain}"
        echo -e "${yellow}This is fine if you're using a CDN; otherwise certificate issuance will fail${plain}"
        local go_on
        read -p "Continue? [y/n]: " go_on
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

    # The user changing their mind at the decoy-site prompt (typing q, or
    # read failing because the SSH session dropped) has to go through the
    # same full wind-down: handle_existing_web_server may already have
    # stopped Caddy, and install_caddy only enables — never starts — an
    # already-installed one, so nothing at all is listening on 80/443.
    # Returning after a bare "Cancelled" would leave a machine whose panel
    # is completely unreachable from outside, with the user believing they
    # did nothing.
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
    # write_caddyfile's trailing systemctl restart caddy has succeeded, so
    # something is listening on 80/443 again. If the service stopped
    # earlier was our own Caddy, this marker must be cleared, or later
    # failure branches would print "caddy has already been stopped;
    # nothing is currently listening on 80/443" — the opposite of the truth.
    [[ "${stopped_web_svc}" == "caddy" ]] && stopped_web_svc=""

    wait_for_cert "${domain}" || {
        if [[ "${force}" == "force" ]]; then
            # On a rerun, "to avoid locking you out, the listen address
            # will not be changed" is active false reassurance: webListen
            # was already set to 127.0.0.1 by an earlier successful run,
            # so not touching it now does not mean the panel is still
            # reachable from outside.
            echo -e "${yellow}Certificate not ready; the panel's listen address was not changed this time${plain}"
            echo -e "${yellow}Note this is a reconfigure: the panel very likely already listens on 127.0.0.1 only,${plain}"
            echo -e "${yellow}and Caddy is still running the previous configuration. If the panel is unreachable, recover with the commands below${plain}"
        else
            echo -e "${yellow}Certificate not ready; to avoid locking you out of the panel, the panel's listen address will not be changed${plain}"
        fi
        echo -e "${yellow}Once fixed, run a-ui and choose \"Configure domain and decoy site\" to retry${plain}"
        echo -e "${yellow}Troubleshoot: journalctl -u caddy -n 50${plain}"
        print_stopped_svc_rollback_hint
        print_rescue_hint_if_force "${force}" "${detected_port}"
        return 1
    }

    if ! install_cert_sync "${domain}"; then
        echo -e "${yellow}Warning: syncing the certificate to /root/cert/ did not succeed${plain}"
        echo -e "${yellow}Troubleshoot: systemctl status a-ui-cert-sync.service${plain}"
    fi

    # Whether the sync really worked is decided by looking at the files,
    # not at a return code: once these two paths go into the panel's
    # "defaults for new inbounds", every TLS inbound the admin creates from
    # then on gets rejected by ValidateInboundReplacing over a path they
    # never typed. Better to make them fill it in than to leave behind a
    # default that is guaranteed to be wrong.
    local cert_args=()
    if [[ -s /root/cert/fullchain.cer && -s "/root/cert/${domain}.key" ]]; then
        cert_args=(-cert-file /root/cert/fullchain.cer -key-file "/root/cert/${domain}.key")
    else
        echo -e "${yellow}Warning: no usable certificate files under /root/cert/, so the \"default certificate paths for new inbounds\" will not be written${plain}"
        echo -e "${yellow}Caddy's own certificate is unaffected; the panel and the decoy site keep working${plain}"
        echo -e "${yellow}When creating a TLS inbound, fill in the certificate paths yourself in the panel${plain}"
    fi

    local force_args=()
    [[ "${force}" == "force" ]] && force_args=(-force)

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode caddy -domain "${domain}" \
            -basepath "${basepath}" -listen 127.0.0.1 -port "${panel_port}" \
            "${cert_args[@]}" "${force_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}Failed to write the panel configuration: ${out}${plain}"
        # This particular call most likely didn't manage to set webListen to
        # 127.0.0.1 (bootstrap writes every other field first and -listen
        # last, so a mid-way failure usually never reaches it). But if this
        # is a reconfigure that overwrites an existing setup, webListen may
        # already have been 127.0.0.1 from an earlier successful run —
        # there's no way to tell "never touched" apart from "touched before,
        # untouched this time" from here, so print it either way rather than
        # bet once on a distinction we can't actually make.
        print_rescue_hint "${panel_port}"
        return 1
    fi
    systemctl restart a-ui
    if ! wait_for_panel_alive "${panel_port}" "${basepath}"; then
        echo -e "${red}Panel health check failed after restart; 127.0.0.1:${panel_port} is not responding${plain}"
        echo -e "${red}The panel configuration was written, but the process may have exited right after starting (a common cause: the certificate path doesn't exist or isn't readable)${plain}"
        echo -e "${yellow}Troubleshoot: journalctl -u a-ui -n 50${plain}"
        # bootstrap already succeeded by this point, so webListen has
        # already been changed to 127.0.0.1 — this is exactly the moment
        # these two commands exist for, not just a journalctl pointer.
        print_rescue_hint "${panel_port}"
        return 1
    fi
    # The port must be passed: the domain branch is the only one that locks
    # the panel to 127.0.0.1, and the only one where the port is certain
    # (it is the panel_port above). Omitting it degrades the rescue hint
    # into "check the port via menu option 7 first" — the downgraded
    # version, in exactly the scenario that most needs a copy-pasteable
    # SSH tunnel command.
    print_result "${out}" "caddy" "${panel_port}"
    check_acme_conflict "${domain}"

    # Only hint about the firewall, don't auto-change it: UFW/firewalld's
    # presence and rules vary too much, and auto-allowing can easily
    # backfire.
    if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        echo -e "${yellow}Detected that ufw is enabled; if not already allowed, run: ufw allow 80,443/tcp${plain}"
    fi
    if command -v firewall-cmd &>/dev/null && firewall-cmd --state &>/dev/null; then
        echo -e "${yellow}Detected that firewalld is enabled; if not already allowed, run:${plain}"
        echo -e "${yellow}  firewall-cmd --permanent --add-service={http,https} && firewall-cmd --reload${plain}"
    fi
}

# Back up the xray core and geo data so they can be restored after
# extraction. An empty xray_backup_dir means there is no backup.
#
# Why this exists: this script replaces the whole install directory with the
# one unpacked from the release tarball (the old one is deleted afterwards),
# and the tarball bundles the xray copy
# checked into this repo (see the packaging step in release.yml). Any core
# the admin upgraded from the panel, and the Loyalsoldier-enhanced geo data
# a-ui geo replaced it with, would otherwise be silently rolled back.
#
# Placed before systemctl stop: this step writes to disk and can fail, and a
# failure here must be able to exit cleanly while the panel is still up.
# Copying an executable that xray currently has open is safe — cp reads file
# contents and does not disturb the already-open inode.
#
# The backup directory deliberately avoids mktemp -d's default location
# (/tmp): on systemd distros /tmp is usually an in-memory tmpfs, and the
# xray core plus geoip.dat plus geosite.dat can add up to 60-70MB — and this
# copy happens before systemctl stop, while the panel and xray are still
# serving traffic, exactly the peak of this machine's memory usage. On a
# low-memory VPS, /tmp running out of space is this function's only fail
# close path, and it would leave the admin permanently unable to update the
# panel. Put it under /usr/local instead, as a sibling directory next to
# a-ui but with a different name: install_a-ui() below only swaps out and
# deletes the /usr/local/a-ui tree, which cannot reach it, and it does not
# eat into the tmpfs memory budget either.
backup_xray_assets() {
    xray_backup_dir=""
    [[ ! -d /usr/local/a-ui/bin ]] && return 0

    local dir
    dir=$(mktemp -d /usr/local/a-ui-xray-backup-XXXXXX) || die_restoring_panel "Failed to create the xray backup directory, update aborted (install directory left untouched)"

    local f
    for f in "xray-linux-${arch}" geoip.dat geosite.dat; do
        if [[ -f "/usr/local/a-ui/bin/${f}" ]]; then
            if ! cp -p "/usr/local/a-ui/bin/${f}" "${dir}/${f}"; then
                rm -rf "${dir}"
                die_restoring_panel "Failed to back up ${f}, update aborted (install directory left untouched)"
            fi
        fi
    done

    xray_backup_dir="${dir}"
}

# Restore the backed-up xray and geo data; when there is no core to keep,
# fetch the latest release from GitHub instead (the first entry of
# /releases, not the seemingly more "correct" /releases/latest -- xray-core
# marks nearly every release as prerelease, so that endpoint would return a
# build from half a year ago).
#
# The decision is based on whether the xray binary was backed up, not geo:
# the xray version is the core requirement, geo data just comes along for
# the ride, and the two can succeed only partially (the admin may have
# deleted one of the files, or the previous install was already broken).
# And precisely because that is the decision, the fetch branch is not the
# same thing as "a fresh install" -- see the wording note inside it.
#
# Both the restore and the fetch path fail open — falling back to the xray
# copy bundled in the release tarball and continuing the install. That copy
# works fine; the whole install should not fail just to get the latest core.
# On the fetch path that fallback has to be actively arranged (stash a seed
# first, copy it back on failure); it does not hold by itself, because
# extraction replaces the target file in place.
restore_or_install_xray() {
    # The release was already verified and unpacked in fetch_release and the
    # new directory is in place, so this cd only fails in extreme cases. At
    # this point the panel is stopped, and the core in the backup directory
    # is one more copy of the version the admin pinned -- delete it, and if
    # the old directory could not be moved back either, re-running the
    # installer will just take the fetch branch below and he can never get
    # that version back. So this path must keep the backup, and put its path
    # in the error message so he knows where it is.
    if ! cd /usr/local/a-ui; then
        if [[ -n "${xray_backup_dir}" ]]; then
            die_restoring_panel "Failed to enter the install directory; the existing xray core and geo data were kept in ${xray_backup_dir}, you can copy them back into /usr/local/a-ui/bin/ after reinstalling"
        fi
        die_restoring_panel "Failed to enter the install directory"
    fi

    if [[ -n "${xray_backup_dir}" && -f "${xray_backup_dir}/xray-linux-${arch}" ]]; then
        # Track whether the core and geo data were actually restored
        # separately, and only print — and word — the success line based on
        # what really happened. If all three cp calls failed, three yellow
        # warnings were already printed above; an unconditional green
        # "kept" line on top of that, or a "kept ... and geo data" line
        # when only the core was restored, would both be exactly the
        # "looks like it succeeded but actually failed" pattern this repo
        # guards against everywhere else.
        local restored_core=0 restored_geo=0
        local f
        for f in "xray-linux-${arch}" geoip.dat geosite.dat; do
            if [[ -f "${xray_backup_dir}/${f}" ]]; then
                if cp -pf "${xray_backup_dir}/${f}" "/usr/local/a-ui/bin/${f}"; then
                    if [[ "${f}" == "xray-linux-${arch}" ]]; then
                        restored_core=1
                    else
                        restored_geo=1
                    fi
                else
                    echo -e "${yellow}Warning: failed to restore ${f}, falling back to the bundled version${plain}"
                fi
            fi
        done
        if [[ ${restored_core} -eq 1 && ${restored_geo} -eq 1 ]]; then
            echo -e "${green}Kept the existing xray core and geo data${plain}"
        elif [[ ${restored_core} -eq 1 ]]; then
            echo -e "${green}Kept the existing xray core (geo data falls back to the bundled version)${plain}"
        elif [[ ${restored_geo} -eq 1 ]]; then
            echo -e "${green}Kept the existing geo data (xray core falls back to the bundled version)${plain}"
        fi
    else
        # The wording deliberately does not assume "fresh install": the
        # decision is whether an xray binary was backed up, not whether the
        # panel was ever installed. A machine whose /usr/local/a-ui/bin
        # exists but whose core file the admin deleted lands here too, and
        # this fetch happens after systemctl stop -- up to 600 seconds of
        # downtime. Calling it a fresh install would make him dismiss a very
        # real outage as something that cannot happen.
        echo "No xray core to keep was found, fetching the latest release..."
        # Stash the seed core bundled in the release tarball before fetching.
        # Extraction replaces the target file in place, so after a failed
        # fetch — or one that produced a core that cannot run — the line
        # "falling back to the bundled version" has to actually be true;
        # otherwise it is just a soothing message while the file on disk is
        # no longer that version. The seed goes into the backup directory
        # (creating one if there is none, deliberately reusing the same
        # prefix so both the cleanup at the end of this function and the
        # rm -rf in a-ui.sh's uninstall cover it).
        if [[ -z "${xray_backup_dir}" ]]; then
            xray_backup_dir=$(mktemp -d /usr/local/a-ui-xray-backup-XXXXXX 2>/dev/null) || xray_backup_dir=""
        fi
        local seed_core=""
        if [[ -n "${xray_backup_dir}" ]] && cp -pf "/usr/local/a-ui/bin/xray-linux-${arch}" "${xray_backup_dir}/seed-xray-linux-${arch}"; then
            seed_core="${xray_backup_dir}/seed-xray-linux-${arch}"
        fi

        # Add a timeout: if GitHub is being blackholed (packets dropped, not
        # a refused connection), http.Get would hang forever right when the
        # panel is stopped, the install directory has been wiped and
        # re-extracted, and the systemd unit isn't in place yet — hanging
        # here is exactly the half-dead state this whole feature exists to
        # avoid. 10 minutes is generous enough for a slow link to pull down
        # a ~37MB core; on timeout, fall through to the fail-open branch
        # below.
        local fetch_ok=0
        if command -v timeout >/dev/null 2>&1; then
            timeout 600 /usr/local/a-ui/a-ui xray -update latest && fetch_ok=1
        else
            /usr/local/a-ui/a-ui xray -update latest && fetch_ok=1
        fi

        # Exit code 0 only says the file was written, not that it can run.
        # A corrupted release or an architecture mismatch still exits 0,
        # that core is chmod +x'd right afterwards, and the panel's
        # Process.Start() never reports a startup failure back
        # (/server/status keeps returning running) — the admin sees nothing
        # until users report dead nodes. So actually run it once. chmod +x
        # first, otherwise "not executable" and "broken core" collapse into
        # the same failure and you cannot tell which one happened.
        if [[ ${fetch_ok} -eq 1 ]]; then
            chmod +x "/usr/local/a-ui/bin/xray-linux-${arch}" 2>/dev/null
            if ! "/usr/local/a-ui/bin/xray-linux-${arch}" -version >/dev/null 2>&1; then
                echo -e "${yellow}Warning: the fetched xray core cannot run, the release may be corrupted or the architecture may not match${plain}"
                fetch_ok=0
            fi
        fi

        if [[ ${fetch_ok} -ne 1 ]]; then
            # Copy the seed core back before printing the line, not after:
            # when the fetch never started this is a harmless no-op, and
            # when it wrote half a file or a broken one it is exactly what
            # makes the line true.
            [[ -n "${seed_core}" ]] && cp -pf "${seed_core}" "/usr/local/a-ui/bin/xray-linux-${arch}"
            chmod +x "/usr/local/a-ui/bin/xray-linux-${arch}" 2>/dev/null
            if "/usr/local/a-ui/bin/xray-linux-${arch}" -version >/dev/null 2>&1; then
                echo -e "${yellow}Warning: failed to fetch the latest xray, falling back to the bundled version${plain}"
                echo -e "${yellow}         You can upgrade manually later using \"切换版本\" (Switch Version) on the panel homepage${plain}"
            else
                echo -e "${red}Warning: failed to fetch the latest xray, and the bundled core cannot run either${plain}"
                echo -e "${red}         Reinstall the xray core from \"切换版本\" (Switch Version) on the panel homepage once it is up, otherwise no node will work${plain}"
            fi
        else
            echo -e "${green}Installed the latest xray core${plain}"
        fi
    fi

    chmod +x "/usr/local/a-ui/bin/xray-linux-${arch}"
    [[ -n "${xray_backup_dir}" ]] && rm -rf "${xray_backup_dir}"
    xray_backup_dir=""
}

# Download and verify the release package and unpack it into
# ${stage_dir}/a-ui. Nothing about the existing install is touched until all
# of this succeeds: the panel keeps running, and any failure simply exits
# (the panel has not been stopped yet, so die_restoring_panel only cleans up
# the staging directory). Must be called directly, never inside $(...) —
# there die_restoring_panel's exit would only leave the subshell.
#
# These files are executed as root afterwards, so there are three layers:
#   1. TLS: curl/wget always verify certificates, no -k /
#      --no-check-certificate.
#   2. SHA256SUMS: the checksum file release.yml attaches to every release.
#      It is another asset of the same release, so it guards against
#      corruption, truncation and substitution in transit and storage, not
#      against the repository itself being compromised. Releases older than
#      this mechanism have no such file (HTTP 404): warn and keep going (TLS
#      is still verified). Any other failure (network error, 5xx) must not be
#      treated as "there is none", or one induced download failure would skip
#      verification entirely — abort.
#   3. Contents: tar -tzf lists the whole archive, every entry is under
#      a-ui/, the key files are present; after unpacking, the new binary is
#      actually run with -v (prints the version, never touches the database).
#      A 200 error page from GitHub, a truncated archive or a binary for the
#      wrong architecture is stopped here — it used to surface only after the
#      old install had been rm -rf'd, leaving a machine with neither panel
#      nor xray.
#
# Unpacking uses --no-same-owner: as root, tar keeps the owner recorded in
# the archive by default, i.e. the uid of the CI packaging user (1001 on
# GitHub runners). If a regular user with that uid exists on this machine,
# he would own this root-run binary and could simply replace it.
fetch_release() {
    local version="$1" pkg="$2" download_fail_msg="$3"
    local base="https://github.com/SienFeng/AetherUI/releases/download/${version}"

    stage_dir=$(mktemp -d /usr/local/a-ui-update-XXXXXX) || {
        stage_dir=""
        die_restoring_panel "failed to create the download staging directory, aborted (existing install untouched)"
    }
    local archive="${stage_dir}/${pkg}"

    if ! wget -O "${archive}" "${base}/${pkg}"; then
        die_restoring_panel "${download_fail_msg}"
    fi

    local sums="${stage_dir}/SHA256SUMS" code
    code=$(curl -sSL --retry 2 -o "${sums}" -w '%{http_code}' "${base}/SHA256SUMS") || code="000"
    case "${code}" in
    200)
        local want got
        # Match the exact file name: the Chinese and English (-english)
        # packages differ only by a suffix. A "*" prefix is how sha256sum
        # marks binary mode.
        want=$(awk -v f="${pkg}" '$2 == f || $2 == "*" f { print $1; exit }' "${sums}" | tr 'A-F' 'a-f')
        if [[ ! "${want}" =~ ^[0-9a-f]{64}$ ]]; then
            die_restoring_panel "the SHA256SUMS of ${version} has no checksum for ${pkg}, cannot verify the download, aborted (existing install untouched)"
        fi
        got=$(sha256sum "${archive}" 2>/dev/null | awk '{print $1}')
        if [[ "${got}" != "${want}" ]]; then
            die_restoring_panel "SHA256 check of ${pkg} failed (expected ${want}, got ${got:-nothing}), the download may be corrupted or tampered with, aborted (existing install untouched)"
        fi
        echo -e "${green}SHA256 check of ${pkg} passed${plain}"
        ;;
    404)
        echo -e "${yellow}Warning: ${version} was released before SHA256SUMS checksum files were attached, the integrity of ${pkg} cannot be checked${plain}"
        echo -e "${yellow}         the download was still TLS-verified, and the package layout is checked and the new binary test-run below before anything is installed${plain}"
        ;;
    *)
        die_restoring_panel "failed to download the SHA256SUMS of ${version} (HTTP ${code}), cannot verify ${pkg}, aborted (existing install untouched), please try again later"
        ;;
    esac

    local listing
    if ! listing=$(tar -tzf "${archive}" 2>/dev/null); then
        die_restoring_panel "the downloaded ${pkg} is not a valid tar.gz archive (probably an error page or an incomplete download), aborted (existing install untouched)"
    fi
    # Hand-made packages may carry a ./ prefix, which unpacks the same; normalize it before checking
    listing=$(echo "${listing}" | sed -e 's#^\./##' -e '/^$/d')
    if [[ -z "${listing}" ]] || echo "${listing}" | grep -qvE '^a-ui(/|$)' ||
        echo "${listing}" | grep -qE '(^|/)\.\.(/|$)'; then
        die_restoring_panel "${pkg} does not have the expected a-ui/ layout, refusing to unpack it, aborted (existing install untouched)"
    fi
    local f
    for f in a-ui/a-ui a-ui/a-ui.service "a-ui/bin/xray-linux-${arch}"; do
        if ! echo "${listing}" | grep -qxF "${f}"; then
            die_restoring_panel "${pkg} is missing ${f}, aborted (existing install untouched)"
        fi
    done

    if ! tar --no-same-owner -xzf "${archive}" -C "${stage_dir}"; then
        die_restoring_panel "failed to unpack ${pkg}, aborted (existing install untouched)"
    fi
    rm -f "${archive}"
    chmod +x "${stage_dir}/a-ui/a-ui" "${stage_dir}/a-ui/bin/xray-linux-${arch}"

    local reported probe=("${stage_dir}/a-ui/a-ui" -v)
    command -v timeout >/dev/null 2>&1 && probe=(timeout 30 "${probe[@]}")
    if ! reported=$("${probe[@]}" 2>/dev/null) || [[ -z "${reported}" ]]; then
        die_restoring_panel "the new a-ui binary cannot run (the package may be corrupted, or not built for this machine's architecture ${arch}), aborted (existing install untouched)"
    fi
    echo -e "${green}the new version passed a test run (a-ui -v printed: $(echo "${reported}" | head -n 1))${plain}"
}

install_a-ui() {
    # The port probe must happen before stop: current_panel_port()'s probe
    # branch relies on systemctl show -p MainPID to find the running a-ui
    # process, and once the line below stops the service, the PID is gone
    # for good. A successful probe means this is an update to an existing
    # deployment, so record it directly; a failed probe means the machine
    # never had it installed, leaving config_after_install to determine it
    # separately in the fresh-install branch — no need to distinguish "why"
    # it failed here.
    local pre_stop_port
    if pre_stop_port=$(current_panel_port); then
        panel_port="${pre_stop_port}"
    fi

    cd /usr/local/

    local pkg="a-ui-linux-${arch}-english.tar.gz" download_fail_msg
    if [ $# == 0 ]; then
        last_version=$(curl -fsSL "https://api.github.com/repos/SienFeng/AetherUI/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$last_version" ]]; then
            die_restoring_panel "refresh a-ui version failed,it may due to Github API restriction,please try it later"
        fi
        echo -e "get a-ui latest version succeed:${last_version},begin to install..."
        download_fail_msg="download a-ui failed,please be sure that your server can access Github"
    else
        last_version=$1
        echo -e "begin to install a-ui $1 ..."
        download_fail_msg="download a-ui $1 failed,please check the verison exists"
    fi
    # The version goes into the download URL and local paths; same rule as
    # the panel's one-click update tag allowlist
    if [[ ! "${last_version}" =~ ^[A-Za-z0-9._-]{1,64}$ ]]; then
        die_restoring_panel "invalid version ${last_version} (only letters, digits, dots, underscores and dashes are allowed)"
    fi

    # Stopping the panel happens only after the download and every check
    # succeeded. Stopping first means that any failure in version detection
    # or download (flaky network, GitHub API rate limit, a version that does
    # not exist, a broken package) exits the script and leaves a machine whose
    # panel is stopped and will not come back on its own — and those are
    # precisely the steps that depend on external network and fail most often.
    # The port probe must still run before the stop; the block above already
    # did that, so reordering is safe.
    fetch_release "${last_version}" "${pkg}" "${download_fail_msg}"
    backup_xray_assets

    # Only record "stopped by this install" if the panel was actually running: on a fresh install, or a
    # panel the admin stopped on purpose, die_restoring_panel must not start it -- on a fresh install
    # config_after_install hasn't run yet, so it would come up as admin/admin on 0.0.0.0:54321.
    if systemctl is-active --quiet a-ui; then
        a_ui_stopped=1
    fi
    systemctl stop a-ui
    # From here to the commit point the panel is stopped: Ctrl-C, an SSH drop (SIGHUP) or kill all go
    # through the restore path instead of leaving the panel stopped and the old tree parked in staging.
    trap 'die_restoring_panel "installation interrupted, restoring the previous state"' INT TERM HUP

    # Swap directories: move the old one aside into the staging directory,
    # then move the new one in. Both are renames inside /usr/local, so after
    # either step the old directory can still be put back exactly as it was
    # (see die_restoring_panel).
    if [[ -e /usr/local/a-ui ]]; then
        if ! mv /usr/local/a-ui "${stage_dir}/old-a-ui"; then
            die_restoring_panel "failed to move the old install directory aside, update aborted"
        fi
        old_install_dir="${stage_dir}/old-a-ui"
    fi
    if ! mv "${stage_dir}/a-ui" /usr/local/a-ui; then
        die_restoring_panel "failed to move the new version into /usr/local/a-ui, update aborted"
    fi

    cd /usr/local/a-ui
    restore_or_install_xray
    cp -f a-ui.service /etc/systemd/system/
    # The unit file may have changed with the version (e.g. a new Restart=).
    # setup_wizard's domain branch runs systemctl restart a-ui before the
    # daemon-reload further down; without reloading here that restart would
    # still use the old unit, with only a "changed on disk" hint from systemd.
    systemctl daemon-reload
    # Download the management script to a temp file first, confirm it
    # succeeded and is non-empty, then move it into /usr/bin/a-ui.
    # wget -O truncates the target before writing: downloading straight into
    # /usr/bin/a-ui leaves an empty file on failure, and the chmod +x right
    # after still succeeds — every later `a-ui` invocation is then silently
    # a no-op, indistinguishable from "never installed" yet impossible to
    # diagnose from the symptom.
    local mgr_tmp
    mgr_tmp=$(mktemp) || die_restoring_panel "failed to create a temp file"
    # The panel is stopped; do not let wget hang on its defaults (20 tries, 900 s timeout).
    if ! wget --timeout=30 --tries=3 -O "${mgr_tmp}" https://raw.githubusercontent.com/SienFeng/AetherUI/main/a-ui_en.sh; then
        rm -f "${mgr_tmp}"
        die_restoring_panel "download of the a-ui management script failed, please be sure your server can reach raw.githubusercontent.com"
    fi
    if [[ ! -s "${mgr_tmp}" ]]; then
        rm -f "${mgr_tmp}"
        die_restoring_panel "the downloaded a-ui management script is empty, the connection was probably cut"
    fi
    # Non-empty is not enough: GitHub's error pages and truncated downloads are non-empty too
    if ! head -n 1 "${mgr_tmp}" | grep -qE '^#!.*bash' || ! bash -n "${mgr_tmp}" 2>/dev/null; then
        rm -f "${mgr_tmp}"
        die_restoring_panel "the downloaded a-ui management script is not a complete bash script, the connection was probably cut"
    fi
    if ! install -m 755 "${mgr_tmp}" /usr/bin/a-ui; then
        rm -f "${mgr_tmp}"
        die_restoring_panel "failed to install the a-ui management script to /usr/bin/a-ui"
    fi
    rm -f "${mgr_tmp}"
    # The new version is fully in place; no rollback from here on: delete the
    # staging directory (and the old directory swapped out into it)
    old_install_dir=""
    rm -rf "${stage_dir}"
    stage_dir=""
    trap - INT TERM HUP
    chmod +x /usr/local/a-ui/a-ui_en.sh
    config_after_install
    setup_wizard
    systemctl daemon-reload
    systemctl enable a-ui
    systemctl start a-ui
    echo -e "${green}a-ui ${last_version}${plain} install finished,it is working now..."
    echo -e ""
    echo -e "a-ui control menu usages: "
    echo -e "----------------------------------------------"
    echo -e "a-ui              - Enter     control menu"
    echo -e "a-ui start        - Start     a-ui "
    echo -e "a-ui stop         - Stop      a-ui "
    echo -e "a-ui restart      - Restart   a-ui "
    echo -e "a-ui status       - Show      a-ui status"
    echo -e "a-ui enable       - Enable    a-ui on system startup"
    echo -e "a-ui disable      - Disable   a-ui on system startup"
    echo -e "a-ui log          - Check     a-ui logs"
    echo -e "a-ui update       - Update    a-ui "
    echo -e "a-ui install      - Install   a-ui "
    echo -e "a-ui uninstall    - Uninstall a-ui "
    echo -e "a-ui geo          - Update    geo  data"
    echo -e "----------------------------------------------"
    if [[ ${default_credentials_active} -eq 1 ]]; then
        echo -e ""
        echo -e "${red}NOTE: the panel still uses the public default username admin / password admin, change it now!${plain}"
        print_credentials_hint
    fi
}

# Trigger the security setup wizard on its own, without doing the whole
# download/extract/install-systemd-service installation flow. Used to
# reconfigure after the panel is already installed (the a-ui menu's
# "Configure domain and decoy site" goes through here), and is also the
# entry point for testing the wizard itself during local development.
# Passing "force" skips setup_wizard's idempotency probe and overwrites
# any existing configuration -- triggering it from the menu already means
# the user explicitly wants to reconfigure, so it shouldn't be blocked by
# that check.
if [[ "$1" == "--wizard-only" ]]; then
    # This entry point skips install_base(), yet the wizard itself depends
    # on jq (print_result parses bootstrap's JSON output with it; without
    # it the whole "panel address" line silently disappears, right after
    # the user moved the panel to a random path) and on openssl
    # (check_reality_target's four criteria all go through it; without it
    # the check always fails, for an entirely fabricated reason).
    command -v jq >/dev/null 2>&1 && command -v openssl >/dev/null 2>&1 || install_base
    # The exit code must be reported honestly: a-ui's "Configure domain and
    # decoy site" menu entry uses it to tell whether the wizard actually
    # finished. Always returning 0 lets "gave up midway + panel already on
    # 127.0.0.1 + Caddy stopped" travel silently back to the main menu.
    setup_wizard force
    exit $?
fi

echo -e "${green}excuting...${plain}"
install_base
install_a-ui $1
