#!/usr/bin/env bash
#
# configure-firewall.sh — open the freepbx-exporter port for one Prometheus
# scrape source on a FreePBX/Asterisk host. Auto-detects which firewall
# stack is active (FreePBX Firewall, firewalld, ufw, or iptables) and
# applies the minimal rule needed.
#
# Usage:
#   sudo bash scripts/configure-firewall.sh -s 10.0.0.5
#   sudo bash scripts/configure-firewall.sh -s 10.0.0.0/24 -p 9810
#   sudo bash scripts/configure-firewall.sh -s 10.0.0.5 --port-only
#   sudo bash scripts/configure-firewall.sh -s 10.0.0.5 --dry-run
#
set -u

SOURCE=""
PORT=9810
MODE=trust          # trust | port-only
ASSUME_YES=0
DRY_RUN=0
FORCE_BACKEND=""

usage() {
    cat <<EOF
Usage: $0 -s SOURCE [options]

  -s, --source IP[/CIDR]     Prometheus host or network (required, e.g. 10.0.0.5 or 10.0.0.0/24)
  -p, --port PORT            Exporter port (default: 9810)
      --trust                FreePBX: add SOURCE as trusted (default)
      --port-only            Open ONLY the exporter port to SOURCE; do not trust the host
      --backend BACKEND      Force a backend: freepbx | firewalld | ufw | iptables
      --dry-run              Print the rules that would be applied; do not change anything
  -y, --yes                  Skip the confirmation prompt
  -h, --help                 Show this help

Detection order: freepbx -> firewalld -> ufw -> iptables.
Re-running is idempotent: existing equivalent rules are skipped.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        -s|--source)    SOURCE=$2; shift 2 ;;
        -p|--port)      PORT=$2; shift 2 ;;
        --trust)        MODE=trust; shift ;;
        --port-only)    MODE=port-only; shift ;;
        --backend)      FORCE_BACKEND=$2; shift 2 ;;
        --dry-run)      DRY_RUN=1; shift ;;
        -y|--yes)       ASSUME_YES=1; shift ;;
        -h|--help)      usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
    esac
done

# ---------- pretty print ----------
RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
[ -t 1 ] || { RED=; GREEN=; YELLOW=; BOLD=; OFF=; }
step()  { printf '\n%s== %s ==%s\n' "$BOLD" "$*" "$OFF"; }
ok()    { printf '%s[OK]%s %s\n'   "$GREEN" "$OFF" "$*"; }
warn()  { printf '%s[WARN]%s %s\n' "$YELLOW" "$OFF" "$*"; }
fail()  { printf '%s[FAIL]%s %s\n' "$RED" "$OFF" "$*"; }
info()  { printf '       %s\n' "$*"; }

run() {
    if [ "$DRY_RUN" = 1 ]; then
        printf '       (dry-run) %s\n' "$*"
        return 0
    fi
    info "+ $*"
    "$@"
}

# ---------- preflight ----------
if [ "$(id -u)" -ne 0 ]; then
    fail "Please run as root (sudo)."
    exit 1
fi
if [ -z "$SOURCE" ]; then
    fail "Missing --source"
    usage
    exit 2
fi

# Crude IP/CIDR sanity check (v4 only).
if ! printf '%s' "$SOURCE" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]{1,2})?$'; then
    fail "SOURCE '$SOURCE' is not a valid IPv4 address or CIDR"
    exit 2
fi
if ! printf '%s' "$PORT" | grep -qE '^[0-9]+$' || [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    fail "PORT '$PORT' is not a valid TCP port"
    exit 2
fi

# Always pass a CIDR to firewall tools that require it.
SOURCE_CIDR="$SOURCE"
[[ "$SOURCE_CIDR" == */* ]] || SOURCE_CIDR="${SOURCE_CIDR}/32"

# ---------- listener sanity ----------
step "Exporter listener check"
LISTENERS=$(ss -tlnp 2>/dev/null | awk -v p=":$PORT" '$4 ~ p {print $4, $7}')
if [ -z "$LISTENERS" ]; then
    warn "Nothing is currently listening on tcp/$PORT — opening the firewall now is fine but the exporter must also be running."
else
    info "$LISTENERS"
    if printf '%s' "$LISTENERS" | grep -q '^127\.0\.0\.1:'; then
        warn "exporter is bound to 127.0.0.1:$PORT — Prometheus on '$SOURCE' will NOT reach it even with firewall open."
        warn "Set FREEPBX_EXPORTER_LISTEN=:$PORT in /etc/default/freepbx-exporter and 'systemctl restart freepbx-exporter'."
    else
        ok "exporter is bound on a routable address"
    fi
fi

# ---------- backend detection ----------
detect_backend() {
    if [ -n "$FORCE_BACKEND" ]; then
        echo "$FORCE_BACKEND"
        return
    fi
    if command -v fwconsole >/dev/null 2>&1; then
        if fwconsole firewall status 2>/dev/null | grep -qi running; then
            echo freepbx
            return
        fi
    fi
    if command -v firewall-cmd >/dev/null 2>&1; then
        if firewall-cmd --state 2>/dev/null | grep -qi running; then
            echo firewalld
            return
        fi
    fi
    if command -v ufw >/dev/null 2>&1; then
        if ufw status 2>/dev/null | head -1 | grep -qi 'Status: active'; then
            echo ufw
            return
        fi
    fi
    if command -v iptables >/dev/null 2>&1; then
        echo iptables
        return
    fi
    echo none
}

step "Backend detection"
BACKEND=$(detect_backend)
case "$BACKEND" in
    freepbx)   ok "Using FreePBX Integrated Firewall (fwconsole)" ;;
    firewalld) ok "Using firewalld" ;;
    ufw)       ok "Using ufw" ;;
    iptables)  ok "Using plain iptables (changes are not persistent unless iptables-persistent is installed)" ;;
    none)      fail "No supported firewall found"; exit 1 ;;
    *)         fail "Unknown backend '$BACKEND'"; exit 2 ;;
esac

# ---------- plan ----------
step "Plan"
case "$BACKEND" in
    freepbx)
        if [ "$MODE" = trust ]; then
            info "fwconsole firewall trust $SOURCE_CIDR"
        else
            warn "The FreePBX firewall CLI does not expose per-port rules; falling back to iptables for the port-only mode."
            BACKEND=iptables-fallback
        fi
        ;;
esac
case "$BACKEND" in
    firewalld)
        info "firewall-cmd: open ${PORT}/tcp from $SOURCE_CIDR (rich rule, permanent)"
        ;;
    ufw)
        info "ufw allow proto tcp from $SOURCE_CIDR to any port $PORT"
        ;;
    iptables|iptables-fallback)
        info "iptables -I INPUT -p tcp -s $SOURCE_CIDR --dport $PORT -j ACCEPT"
        if command -v netfilter-persistent >/dev/null 2>&1; then
            info "netfilter-persistent save  (Debian/Ubuntu)"
        elif [ -f /etc/sysconfig/iptables ]; then
            info "iptables-save > /etc/sysconfig/iptables  (RHEL/Rocky)"
        else
            warn "No iptables persistence helper detected — rule will not survive reboot"
        fi
        ;;
esac

# ---------- confirmation ----------
if [ "$DRY_RUN" != 1 ] && [ "$ASSUME_YES" != 1 ]; then
    printf '\nApply these changes? [y/N] '
    read -r reply || reply=""
    case "$reply" in
        y|Y|yes|YES) ;;
        *) info "aborted"; exit 0 ;;
    esac
fi

# ---------- apply ----------
step "Applying"
applied=0
case "$BACKEND" in
    freepbx)
        # Idempotency: parse current trusted networks.
        if fwconsole firewall list 2>/dev/null \
            | awk '/Trusted/,/^$/' \
            | grep -qE "(^|[[:space:]])${SOURCE_CIDR%/32}([[:space:]]|/|$)"; then
            ok "$SOURCE_CIDR is already trusted; nothing to do"
        else
            run fwconsole firewall trust "$SOURCE_CIDR"
            applied=1
        fi
        ;;

    firewalld)
        SVC_NAME="freepbx-exporter-${PORT}"
        if ! firewall-cmd --permanent --info-service="$SVC_NAME" >/dev/null 2>&1; then
            run firewall-cmd --permanent --new-service="$SVC_NAME"
            run firewall-cmd --permanent --service="$SVC_NAME" --set-short="freepbx-exporter on tcp/$PORT"
            run firewall-cmd --permanent --service="$SVC_NAME" --add-port="${PORT}/tcp"
        else
            ok "service $SVC_NAME already defined"
        fi
        RICH="rule family=ipv4 source address=${SOURCE_CIDR} service name=${SVC_NAME} accept"
        if firewall-cmd --permanent --query-rich-rule="$RICH" >/dev/null 2>&1; then
            ok "rich rule already present"
        else
            run firewall-cmd --permanent --zone=public --add-rich-rule="$RICH"
            applied=1
        fi
        run firewall-cmd --reload
        ;;

    ufw)
        # ufw status numbered shows existing rules.
        if ufw status numbered 2>/dev/null | grep -q "${PORT}/tcp.*${SOURCE_CIDR}"; then
            ok "ufw rule already present"
        else
            run ufw allow proto tcp from "$SOURCE_CIDR" to any port "$PORT" comment 'freepbx-exporter'
            applied=1
        fi
        run ufw reload
        ;;

    iptables|iptables-fallback)
        if iptables -C INPUT -p tcp -s "$SOURCE_CIDR" --dport "$PORT" -j ACCEPT 2>/dev/null; then
            ok "iptables rule already present"
        else
            run iptables -I INPUT -p tcp -s "$SOURCE_CIDR" --dport "$PORT" \
                -m comment --comment "freepbx-exporter" -j ACCEPT
            applied=1
        fi
        if [ "$applied" = 1 ] && [ "$DRY_RUN" != 1 ]; then
            if command -v netfilter-persistent >/dev/null 2>&1; then
                run netfilter-persistent save
            elif [ -f /etc/sysconfig/iptables ]; then
                run sh -c "iptables-save > /etc/sysconfig/iptables"
            else
                warn "rule applied to running kernel only; install iptables-persistent for boot persistence"
            fi
        fi
        ;;
esac

# ---------- verify ----------
step "Verification"
case "$BACKEND" in
    freepbx)
        fwconsole firewall list 2>/dev/null | sed 's/^/       /' || true
        ;;
    firewalld)
        firewall-cmd --list-rich-rules | sed 's/^/       /' || true
        ;;
    ufw)
        ufw status numbered | grep -E "${PORT}|freepbx-exporter" | sed 's/^/       /' || true
        ;;
    iptables|iptables-fallback)
        iptables -nL INPUT --line-numbers | grep -E "tcp dpt:${PORT}|freepbx-exporter" | sed 's/^/       /' || true
        ;;
esac

ok "Done."
info "From your Prometheus host run:"
info "  curl -fsS http://$(hostname -I | awk '{print $1}'):${PORT}/metrics | head"
