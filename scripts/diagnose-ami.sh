#!/usr/bin/env bash
#
# diagnose-ami.sh — troubleshoot freepbx-exporter AMI authentication.
#
# Run as root on the FreePBX/Asterisk host:
#
#   sudo bash scripts/diagnose-ami.sh
#   sudo bash scripts/diagnose-ami.sh -u prometheus -H 127.0.0.1 -p 5038
#   sudo bash scripts/diagnose-ami.sh --show-secret    # do not mask
#
# The script is read-only except for one `manager reload` call to make sure
# Asterisk's loaded config matches what's on disk before testing.
set -u

# ---------- defaults ----------
AMI_USER=prometheus
AMI_HOST=127.0.0.1
AMI_PORT=5038
ENV_FILE=/etc/default/freepbx-exporter
SHOW_SECRET=0

usage() {
    cat <<EOF
Usage: $0 [options]
  -u, --user USER          AMI username to test (default: prometheus)
  -H, --host HOST          AMI host (default: 127.0.0.1)
  -p, --port PORT          AMI port (default: 5038)
  -e, --env-file PATH      exporter env file (default: /etc/default/freepbx-exporter)
      --show-secret        do not mask the secret in output
  -h, --help               this help
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        -u|--user)        AMI_USER=$2; shift 2 ;;
        -H|--host)        AMI_HOST=$2; shift 2 ;;
        -p|--port)        AMI_PORT=$2; shift 2 ;;
        -e|--env-file)    ENV_FILE=$2; shift 2 ;;
        --show-secret)    SHOW_SECRET=1; shift ;;
        -h|--help)        usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
    esac
done

# ---------- pretty print ----------
RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
[ -t 1 ] || { RED=; GREEN=; YELLOW=; BOLD=; OFF=; }

step()  { printf '\n%s== %s ==%s\n' "$BOLD" "$*" "$OFF"; }
ok()    { printf '%s[OK]%s %s\n'     "$GREEN" "$OFF" "$*"; }
warn()  { printf '%s[WARN]%s %s\n'   "$YELLOW" "$OFF" "$*"; }
fail()  { printf '%s[FAIL]%s %s\n'   "$RED" "$OFF" "$*"; }
info()  { printf '       %s\n' "$*"; }

mask() {
    local s=$1
    [ "$SHOW_SECRET" = 1 ] && { printf '%s' "$s"; return; }
    local n=${#s}
    if [ "$n" -le 8 ]; then
        printf '%s' "$(printf '%*s' "$n" '' | tr ' ' '*')"
    else
        printf '%s****%s' "${s:0:4}" "${s: -4}"
    fi
}

# require root for /etc/default and /etc/asterisk reads
if [ "$(id -u)" -ne 0 ]; then
    fail "Please run as root (sudo)."
    exit 1
fi

VERDICT=()

# ---------- 1. Asterisk reachable ----------
step "1. Asterisk + AMI reachability"
if ! command -v asterisk >/dev/null; then
    fail "'asterisk' binary not found on PATH"
    exit 1
fi
ASTERISK_VERSION=$(asterisk -rx "core show version" 2>/dev/null | head -1 || true)
[ -n "$ASTERISK_VERSION" ] && ok "$ASTERISK_VERSION" || warn "could not query Asterisk via CLI"

if exec 9<>"/dev/tcp/$AMI_HOST/$AMI_PORT" 2>/dev/null; then
    read -r -t 2 BANNER <&9 || BANNER=""
    exec 9>&-
    if printf '%s' "$BANNER" | grep -q '^Asterisk Call Manager'; then
        ok "AMI banner: ${BANNER%$'\r'}"
    else
        fail "TCP open at $AMI_HOST:$AMI_PORT but no AMI banner — service may be wrong"
        VERDICT+=("AMI banner missing — check 'manager show settings' enabled=yes")
    fi
else
    fail "Cannot connect to $AMI_HOST:$AMI_PORT"
    VERDICT+=("AMI port not reachable — check 'manager show settings' bindaddr/port and firewall")
    exit 1
fi

# ---------- 2. User loaded? ----------
step "2. AMI user '$AMI_USER' loaded in Asterisk"
USER_INFO=$(asterisk -rx "manager show user $AMI_USER" 2>/dev/null || true)
if printf '%s' "$USER_INFO" | grep -q "No such user"; then
    fail "Asterisk does not know about user '$AMI_USER'"
    VERDICT+=("User missing in loaded config — add to manager_custom.conf and 'manager reload'")
    printf '%s\n' "$USER_INFO"
    exit 1
fi
printf '%s\n' "$USER_INFO" | sed 's/^/       /'
if printf '%s' "$USER_INFO" | grep -q 'secret: <Set>'; then
    ok "user is loaded with a secret"
else
    warn "user has no secret set — Asterisk will reject any login"
    VERDICT+=("User has no secret set in loaded config")
fi

# ---------- 3. ACL covers the source IP ----------
step "3. ACL evaluation for $AMI_HOST"
ACL_BLOCK=$(printf '%s' "$USER_INFO" | awk '/^ACL:/{flag=1; next} /^---------/{next} flag')
if [ -z "$ACL_BLOCK" ]; then
    warn "no ACL section parsed — assuming no IP restriction"
else
    info "ACL rules:"
    printf '%s\n' "$ACL_BLOCK" | sed 's/^/         /'
fi

# ---------- 4. Find on-disk definition + invisible chars ----------
step "4. On-disk definition of [$AMI_USER]"
MATCHES=$(grep -RIlE "^\[$AMI_USER\]" /etc/asterisk/manager*.conf 2>/dev/null || true)
if [ -z "$MATCHES" ]; then
    fail "No [$AMI_USER] section found under /etc/asterisk/manager*.conf"
    VERDICT+=("User loaded but not visible on disk — Asterisk may have cached an old definition")
    DISK_SECRET=""
else
    info "Found in:"
    printf '%s\n' "$MATCHES" | sed 's/^/       - /'
    NMATCH=$(printf '%s\n' "$MATCHES" | wc -l)
    if [ "$NMATCH" -gt 1 ]; then
        warn "multiple files declare [$AMI_USER] — last include wins, may shadow earlier ones"
        VERDICT+=("Multiple [$AMI_USER] sections in $MATCHES")
    fi

    # Pull the secret from the first matching file. Stop at the next [section].
    FIRST_FILE=$(printf '%s\n' "$MATCHES" | head -1)
    DISK_SECRET=$(awk -v u="$AMI_USER" '
        $0 ~ "^\\["u"\\]" {f=1; next}
        f && /^\[/        {exit}
        f && /^[ \t]*secret[ \t]*=/ {
            sub(/^[ \t]*secret[ \t]*=[ \t]*/, "")
            print
            exit
        }' "$FIRST_FILE")

    info "secret in $FIRST_FILE: [$(mask "$DISK_SECRET")]  length=${#DISK_SECRET}"

    # Detect invisible/control characters.
    if printf '%s' "$DISK_SECRET" | LC_ALL=C grep -q '[^[:print:]]'; then
        fail "secret contains non-printable characters (e.g. \\r, \\t)"
        printf '%s' "$DISK_SECRET" | cat -A | sed 's/^/       /'
        VERDICT+=("Strip non-printable chars from secret in $FIRST_FILE")
    elif printf '%s' "$DISK_SECRET" | LC_ALL=C grep -qE '[[:space:]]$'; then
        fail "secret has trailing whitespace"
        VERDICT+=("Trim trailing whitespace from secret in $FIRST_FILE")
    else
        ok "secret looks clean"
    fi
fi

# ---------- 5. Compare with /etc/default/freepbx-exporter ----------
step "5. Compare with $ENV_FILE"
if [ -f "$ENV_FILE" ]; then
    ENV_SECRET=$(awk -F= '$1=="FREEPBX_EXPORTER_AMI_SECRET"{
        sub(/^[^=]*=/, "")
        gsub(/^["'\'']|["'\'']$/, "")
        print; exit
    }' "$ENV_FILE")
    ENV_USER=$(awk -F= '$1=="FREEPBX_EXPORTER_AMI_USERNAME"{
        sub(/^[^=]*=/, "")
        gsub(/^["'\'']|["'\'']$/, "")
        print; exit
    }' "$ENV_FILE")

    info "exporter username: ${ENV_USER:-<unset>}"
    info "exporter secret  : [$(mask "$ENV_SECRET")]  length=${#ENV_SECRET}"

    if [ -n "$ENV_USER" ] && [ "$ENV_USER" != "$AMI_USER" ]; then
        warn "exporter is configured for user '$ENV_USER' but we are testing '$AMI_USER'"
    fi
    if [ -n "$DISK_SECRET" ] && [ -n "$ENV_SECRET" ]; then
        if [ "$DISK_SECRET" = "$ENV_SECRET" ]; then
            ok "secrets match"
        else
            fail "secret mismatch between disk and exporter env file"
            VERDICT+=("Update FREEPBX_EXPORTER_AMI_SECRET in $ENV_FILE to match $FIRST_FILE")
        fi
    fi
else
    warn "$ENV_FILE not found — skipping env comparison"
fi

# ---------- 6. Force reload + login probe ----------
step "6. Reload AMI and probe Login with on-disk secret"
asterisk -rx "manager reload" >/dev/null 2>&1 || warn "'manager reload' failed"
ok "manager reload issued"

probe_login() {
    local user=$1 secret=$2
    {
        printf 'Action: Login\r\nUsername: %s\r\nSecret: %s\r\nEvents: off\r\n\r\nAction: Logoff\r\n\r\n' \
            "$user" "$secret"
        sleep 1
    } | timeout 5 bash -c "exec 3<>/dev/tcp/$AMI_HOST/$AMI_PORT; cat >&3; cat <&3" 2>/dev/null
}

if [ -n "$DISK_SECRET" ]; then
    PROBE_OUT=$(probe_login "$AMI_USER" "$DISK_SECRET" || true)
    info "raw response:"
    printf '%s\n' "$PROBE_OUT" | sed 's/^/       /'
    if printf '%s' "$PROBE_OUT" | grep -q '^Response: Success'; then
        ok "AMI login with on-disk secret SUCCEEDED"
    elif printf '%s' "$PROBE_OUT" | grep -q 'Authentication failed'; then
        fail "AMI login with on-disk secret rejected — Asterisk has not loaded this secret"
        VERDICT+=("Asterisk loaded a different secret. Check FreePBX did not regenerate manager_additional.conf after edit")
    else
        fail "unexpected probe output (see above)"
    fi
else
    warn "skipping probe — no on-disk secret extracted"
fi

# ---------- 7. Tail recent Asterisk auth log lines ----------
step "7. Recent AMI auth log lines"
LOG=/var/log/asterisk/full
if [ -r "$LOG" ]; then
    grep -iE 'manager|authenticat|prometheus' "$LOG" 2>/dev/null \
      | tail -n 15 \
      | sed 's/^/       /' \
      || info "no matching log lines"
else
    warn "$LOG not readable — cannot show Asterisk auth log"
fi

# ---------- verdict ----------
step "Verdict"
if [ ${#VERDICT[@]} -eq 0 ]; then
    ok "No issues detected. If the exporter still reports 'Authentication failed',"
    ok "restart it: 'systemctl restart freepbx-exporter' and re-run this script."
else
    fail "Found ${#VERDICT[@]} issue(s):"
    for v in "${VERDICT[@]}"; do
        printf '  - %s\n' "$v"
    done
fi
