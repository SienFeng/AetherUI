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

install_base() {
    if [[ x"${release}" == x"centos" ]]; then
        yum install wget curl tar jq -y
    else
        apt install wget curl tar jq -y
    fi
}

#This function will be called when user installed a-ui out of sercurity
config_after_install() {
    # Under the new topology the panel port is an internal implementation
    # detail and is no longer asked about here (see the security setup
    # wizard below). But the wizard needs to know the port actually in
    # effect after this run in order to build the panel URL: a fresh
    # install can always determine it via the two branches below; on an
    # upgrade where the user chooses to keep existing settings, the port
    # was never touched here, so the value is unknown and the global
    # panel_port is left unset (leaving it to the caller to probe for the
    # port itself, or simply omit it from the URL).
    local is_fresh_install=1
    [[ -f "/etc/a-ui/a-ui.db" ]] && is_fresh_install=0

    echo -e "${yellow}Install/update finished, need to modify the account and password out of security${plain}"
    read -p "are you continue,if you type n will skip this at this time[y/n]": config_confirm
    if [[ x"${config_confirm}" == x"y" || x"${config_confirm}" == x"Y" ]]; then
        read -p "please set up your username:" config_account
        echo -e "${yellow}your username will be:${config_account}${plain}"
        read -p "please set up your password:" config_password
        echo -e "${yellow}your password will be:${config_password}${plain}"
        echo -e "${yellow}initializing,wait some time here...${plain}"
        /usr/local/a-ui/a-ui setting -username ${config_account} -password ${config_password}
        echo -e "${yellow}account name and password set down!${plain}"
        [[ ${is_fresh_install} -eq 1 ]] && panel_port=54321
    else
        echo -e "${red}cancel...${plain}"
        if [[ ${is_fresh_install} -eq 1 ]]; then
            local usernameTemp=$(head -c 6 /dev/urandom | base64)
            local passwordTemp=$(head -c 6 /dev/urandom | base64)
            local portTemp=$(echo $RANDOM)
            /usr/local/a-ui/a-ui setting -username ${usernameTemp} -password ${passwordTemp}
            /usr/local/a-ui/a-ui setting -port ${portTemp}
            panel_port=${portTemp}
            echo -e "this is a fresh installation,will generate random login info for security concerns:"
            echo -e "###############################################"
            echo -e "${green}user name:${usernameTemp}${plain}"
            echo -e "${green}user password:${passwordTemp}${plain}"
            echo -e "${red}web port:${portTemp}${plain}"
            echo -e "###############################################"
            echo -e "${red}if you forgot your login info,you can type a-ui and then type 7 to check after installation${plain}"
        else
            echo -e "${red} this is your upgrade,will keep old settings,if you forgot your login info,you can type a-ui and then type 7 to check${plain}"
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
        return 1
    fi

    echo -e "Checking whether ${target} meets REALITY's requirements (TLS1.3 / ALPN h2 / X25519)…"
    if ! check_reality_target "${target}"; then
        echo -e "${red}${target} doesn't meet the requirements, please choose a different target${plain}"
        echo -e "${yellow}Skipping the setup wizard; the panel keeps its default configuration and can be reconfigured later by running a-ui${plain}"
        return 1
    fi
    echo -e "${green}Check passed${plain}"

    local basepath
    basepath="/$(gen_random_path)/"

    # Omit -port when the current panel port can't be determined: better
    # to leave bootstrap's webPort untouched than to silently change the
    # user's existing port with a guessed, possibly wrong, value.
    local port_args=()
    local detected_port
    if detected_port=$(current_panel_port); then
        port_args=(-port "${detected_port}")
    fi

    local force_args=()
    [[ "${force}" == "force" ]] && force_args=(-force)

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode reality \
            -reality-dest "${target}:443" -basepath "${basepath}" "${port_args[@]}" \
            "${force_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}Configuration failed: ${out}${plain}"
        echo -e "${yellow}The panel keeps its default configuration and remains reachable${plain}"
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

    # "<服务器IP>" in panelUrl is a placeholder bootstrap leaves for this
    # script to fill in (the panel process doesn't know its own public
    # IP). Note: this placeholder is a literal string hardcoded in the Go
    # binary (bootstrap/bootstrap.go) and is not localized, so it must be
    # matched/replaced verbatim here too — translating it would break both
    # the substring match below and the actual text already inside ${url}.
    # When the IP can't be detected, keep the placeholder and print an
    # extra hint instead of silently showing an address that looks fine
    # but doesn't actually work.
    if [[ "${url}" == *"<服务器IP>"* ]]; then
        local ip
        ip=$(public_ip)
        [[ -n "${ip}" ]] && url="${url//<服务器IP>/${ip}}"
    fi

    echo -e ""
    echo -e "${green}=== Setup complete ===${plain}"
    if [[ -n "${url}" ]]; then
        echo -e "${green}Panel URL: ${url}${plain}"
    fi
    if [[ "${url}" == *"<服务器IP>"* ]]; then
        echo -e "${yellow}(Could not auto-detect this server's public IP; please replace <服务器IP> in the address with your server's public IP)${plain}"
    fi
    if [[ "${url}" == *":0/"* ]]; then
        echo -e "${yellow}(Could not determine the panel's current listening port; use a-ui menu option 7 to check the actual port)${plain}"
    fi
    if [[ "${mode}" == "reality" ]]; then
        echo -e "${green}A VLESS+Vision+REALITY inbound has been created (port 443); log in to the panel to view the share link and QR code${plain}"
    fi
    echo -e ""
    echo -e "${yellow}If the panel is unreachable, recover with either:${plain}"
    echo -e "  a-ui setting -listen \"\"                       # restore listening on all interfaces"
    if [[ -n "${port}" ]]; then
        echo -e "  ssh -L ${port}:127.0.0.1:${port} root@<server-IP>     # or an SSH tunnel"
    else
        # Can't fabricate a specific port number when detection failed —
        # a wrong guess would make this lifeline useless.
        echo -e "  SSH tunnel: first use a-ui menu option 7 to check the panel's actual port, then run"
        echo -e "  ssh -L <port>:127.0.0.1:<port> root@<server-IP>"
    fi
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

# Three ways this check fails: a non-2xx status code; a redirect to a
# different domain; or being blocked by Cloudflare. A prober seeing a
# broken mirror site is far more suspicious than seeing a plain static page.
#
# On success, stdout outputs the URL that should actually be reverse
# proxied: with no redirect it's ${url} unchanged; with a same-domain
# redirect (http→https, bare domain→www, etc.) it's the redirect target,
# not the original address — the reverse-proxy target itself must never be
# an address that redirects, or Caddy would forward the upstream's raw
# Location header straight to real visitors, whose browsers would jump
# right to the real domain behind the redirect, blowing the disguise on
# the spot. A cross-domain redirect is still rejected outright, and only
# this one hop is followed — it does not recursively probe whether the
# redirect target itself redirects further. On failure, stdout outputs the
# rejection reason.
check_mask_site() {
    local url="$1"
    local ua="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    local resp code redirect target headers

    resp=$(curl -sS -o /dev/null -m 10 -A "${ua}" \
                -w '%{http_code} %{redirect_url}' "${url}" 2>/dev/null) || {
        echo "unable to connect"
        return 1
    }
    code=$(echo "${resp}" | awk '{print $1}')
    redirect=$(echo "${resp}" | awk '{print $2}')
    target="${url}"

    # Without -L, curl's redirect response is itself a 3xx, which would be
    # caught by the "non-2xx status code" check below — so the
    # cross-domain check must come before the status code check, or this
    # branch would never be reached.
    if [[ -n "${redirect}" ]]; then
        if [[ "$(_mask_site_url_host "${url}")" != "$(_mask_site_url_host "${redirect}")" ]]; then
            echo "redirects to ${redirect}"
            return 1
        fi
        target="${redirect}"
        code=$(curl -sS -o /dev/null -m 10 -A "${ua}" -w '%{http_code}' "${target}" 2>/dev/null) || {
            echo "unable to connect"
            return 1
        }
    fi

    if [[ ! "${code}" =~ ^2 ]]; then
        echo "HTTP ${code}"
        return 1
    fi

    # A successful first GET probe doesn't guarantee this HEAD request
    # succeeds too (some sites/WAFs treat HEAD differently); a failed
    # probe and a probe result of "not blocked" are two different things
    # and must not both be treated as a pass without distinction — that
    # would silently swallow a genuine probe failure.
    headers=$(curl -sSI -m 10 -A "${ua}" "${target}" 2>/dev/null) || {
        echo -e "${yellow}Warning: could not complete the Cloudflare-block check, skipping it${plain}" >&2
        echo "${target}"
        return 0
    }
    if echo "${headers}" | grep -qi "cf-mitigated"; then
        echo "blocked by Cloudflare"
        return 1
    fi
    echo "${target}"
    return 0
}

# Candidates come from real-machine testing (2026-09-04, from a Tokyo
# datacenter IP): 2xx status code, no redirect, no Cloudflare block.
# Testing must be done from a datacenter IP — results from a residential
# IP don't count; it's very common for the same site to work fine from a
# residential IP while a datacenter IP gets a 403 or a CAPTCHA. Site
# policies change over time, so this needs to be retested periodically.
#
# Already tested and rejected — don't add these back: gnu.org (can't
# connect), tesla.com (403). Note that tesla.com is only unusable as a
# **reverse-proxy target**; it's perfectly fine as a REALITY dest — the
# two have different criteria (REALITY is raw TCP passthrough, it never
# sends an HTTP request).
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

    local choice url resolved
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
        elif [[ "${choice}" =~ ^[0-9]+$ ]] && [[ "${choice}" -ge 1 ]] && [[ "${choice}" -lt "${i}" ]]; then
            url="${MASK_SITES[$((choice - 1))]}"
        else
            echo -e "${red}Invalid number${plain}" >&2
            continue
        fi

        echo -e "Testing whether ${url} can be proxied from this server…" >&2
        if resolved=$(check_mask_site "${url}"); then
            # On success, check_mask_site outputs the address that should
            # actually be reverse proxied: with no redirect it's ${url}
            # itself, with a same-domain redirect (e.g. http→https) it's
            # the redirect target — it must not be overwritten with
            # ${url}, or the reverse-proxy target itself would redirect,
            # sending real visitors' browsers to the real domain behind
            # the redirect and blowing the disguise on the spot.
            echo -e "${green}available${plain}" >&2
            echo "${resolved}"
            return 0
        fi
        # Don't silently fall back to the static page: that would make the
        # user think they're disguised as some site when they're not.
        echo -e "${red}${url} unavailable (${resolved}), pick another${plain}" >&2
    done
}

# See spec §7 for the Caddyfile structure. Two key points:
#   1. The panel's reverse-proxy path must exactly match the webBasePath
#      written into the panel, or static assets will 404
#   2. The decoy site sits in the last handle, as the fallback
# Caddy occupies 80/443 directly; the certificate and the 80→443 redirect
# both rely on its automatic HTTPS, so https_port / bind aren't needed —
# verified by Task 0.
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

# a-ui.service is Type=simple with no Restart= configured: a successful
# `systemctl restart` only means the process was forked, not that it
# didn't immediately exit (e.g. leftover webCertFile/webKeyFile pointing
# at a certificate file that no longer exists — tls.LoadX509KeyPair fails,
# Server.Start() returns an error, and main.go just logs a line and
# returns). This "came up and immediately died" case can't be seen from
# systemctl's return code, so it must be probed for real.
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
src=$(find /var/lib/caddy -type f -name "${domain}.crt" 2>/dev/null | head -1)
key=$(find /var/lib/caddy -type f -name "${domain}.key" 2>/dev/null | head -1)
[[ -z "${src}" || -z "${key}" ]] && exit 0
changed=0
cmp -s "${src}" /root/cert/fullchain.cer || changed=1
cmp -s "${key}" "/root/cert/${domain}.key" || changed=1
[[ "${changed}" == "0" ]] && exit 0
install -m 644 "${src}" /root/cert/fullchain.cer
install -m 600 "${key}" "/root/cert/${domain}.key"
logger -t a-ui-cert-sync "Certificate synced to /root/cert"
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
    # Run it once immediately, don't wait for the first period
    systemctl start a-ui-cert-sync.service
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
    local domain
    read -p "Enter your domain: " domain
    if [[ -z "${domain}" ]]; then
        echo -e "${red}Domain cannot be empty${plain}"
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
        echo -e "${yellow}Certificate not ready; to avoid locking you out of the panel, the panel's listen address will not be changed${plain}"
        echo -e "${yellow}Once fixed, run a-ui and choose \"Configure domain and decoy site\" to retry${plain}"
        return 1
    }

    install_cert_sync "${domain}"

    local force_args=()
    [[ "${force}" == "force" ]] && force_args=(-force)

    local out
    out=$(/usr/local/a-ui/a-ui bootstrap -mode caddy -domain "${domain}" \
            -basepath "${basepath}" -listen 127.0.0.1 -port "${panel_port}" \
            -cert-file /root/cert/fullchain.cer \
            -key-file "/root/cert/${domain}.key" "${force_args[@]}" -json 2>&1)
    if [[ $? -ne 0 ]]; then
        echo -e "${red}Failed to write the panel configuration: ${out}${plain}"
        return 1
    fi
    systemctl restart a-ui
    if ! wait_for_panel_alive "${panel_port}" "${basepath}"; then
        echo -e "${red}Panel health check failed after restart; 127.0.0.1:${panel_port} is not responding${plain}"
        echo -e "${red}The panel configuration was written, but the process may have exited right after starting (a common cause: the certificate path doesn't exist or isn't readable)${plain}"
        echo -e "${yellow}Troubleshoot: journalctl -u a-ui -n 50${plain}"
        return 1
    fi
    print_result "${out}" "caddy"
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

    systemctl stop a-ui
    cd /usr/local/

    if [ $# == 0 ]; then
        last_version=$(curl -Ls "https://api.github.com/repos/SienFeng/AetherUI/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$last_version" ]]; then
            echo -e "${red}refresh a-ui version failed,it may due to Github API restriction,please try it later${plain}"
            exit 1
        fi
        echo -e "get a-ui latest version succeed:${last_version},begin to install..."
        wget -N --no-check-certificate -O /usr/local/a-ui-linux-${arch}-english.tar.gz https://github.com/SienFeng/AetherUI/releases/download/${last_version}/a-ui-linux-${arch}-english.tar.gz
        if [[ $? -ne 0 ]]; then
            echo -e "${red}dowanload a-ui failed,please be sure that your server can access Github{plain}"
            exit 1
        fi
    else
        last_version=$1
        url="https://github.com/SienFeng/AetherUI/releases/download/${last_version}/a-ui-linux-${arch}-english.tar.gz"
        echo -e "begin to install a-ui $1 ..."
        wget -N --no-check-certificate -O /usr/local/a-ui-linux-${arch}-english.tar.gz ${url}
        if [[ $? -ne 0 ]]; then
            echo -e "${red}dowanload a-ui $1 failed,please check the verison exists${plain}"
            exit 1
        fi
    fi

    if [[ -e /usr/local/a-ui/ ]]; then
        rm /usr/local/a-ui/ -rf
    fi

    tar zxvf a-ui-linux-${arch}-english.tar.gz
    rm a-ui-linux-${arch}-english.tar.gz -f
    cd a-ui
    chmod +x a-ui bin/xray-linux-${arch}
    cp -f a-ui.service /etc/systemd/system/
    wget --no-check-certificate -O /usr/bin/a-ui https://raw.githubusercontent.com/SienFeng/AetherUI/main/a-ui_en.sh
    chmod +x /usr/local/a-ui/a-ui_en.sh
    chmod +x /usr/bin/a-ui
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
    setup_wizard force
    exit 0
fi

echo -e "${green}excuting...${plain}"
install_base
install_a-ui $1
