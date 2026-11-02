#!/usr/bin/env bash
# deploy-vps.sh - one-command HTTPS + OAuth deployment of mms (Memory MCP Server)
# on a fresh Linux VPS: binary + systemd unit + nginx reverse proxy + Let's Encrypt.
#
# Author: okooo5km(十里)
#
# Idempotent: re-running upgrades the binary, unit and nginx config and keeps
# the existing password and data (unless --password is given).
set -euo pipefail

APP_NAME="mms"
REPO="okooo5km/memory-mcp-server-go"

BIN_PATH="/usr/local/bin/${APP_NAME}"
SERVICE_USER="mms"
DATA_DIR="/var/lib/mms"
CONF_DIR="/etc/mms"
ENV_FILE="${CONF_DIR}/mms.env"
UNIT_FILE="/etc/systemd/system/mms.service"
ACME_ROOT="/var/www/mms-acme"
DEPLOY_HOOK="/etc/letsencrypt/renewal-hooks/deploy/mms-nginx.sh"

# Options (set by parse_args)
DOMAIN=""
EMAIL=""
OAUTH_USER_ARG=""
OAUTH_PASS_ARG="${MMS_OAUTH_PASS:-}"
PORT_ARG=""
VERSION="latest"
LOCAL_BINARY=""
CERT_PATH=""
KEY_PATH=""
STAGING=false

# Runtime state
PKG=""
ARCH=""
HAS_IPV6=0
H2_MODE="directive"
PORT="8080"
OAUTH_USER_VAL="admin"
PASS_GENERATED=false
WARNINGS=0
WORKDIR=""

usage() {
  cat <<USAGE
${APP_NAME} VPS deployer (nginx + Let's Encrypt + systemd + OAuth)

Usage:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/deploy-vps.sh \\
    | sudo bash -s -- --domain mcp.example.com --email me@example.com

Options:
  --domain NAME     Public hostname (required). Its DNS must already point here.
  --email ADDR      Email for Let's Encrypt expiry notices (optional).
  --user NAME       OAuth login username. Default: admin (existing value kept on re-run)
  --password PASS   OAuth login password. Prefer the MMS_OAUTH_PASS env var over argv.
                    Default: generated on first install, never regenerated afterwards.
  --port N          Local port the server binds on 127.0.0.1. Default: 8080
  --version TAG     Release to install (e.g. v0.2.3). Default: latest
  --binary PATH     Install this local binary instead of downloading a release.
  --cert PATH       Use an existing certificate chain (PEM) instead of certbot.
  --key PATH        Private key for --cert (PEM). Requires --cert.
  --staging         Use the Let's Encrypt staging CA (untrusted certs, for testing).
  -h, --help        Show this help
USAGE
}

log()  { echo "==> $*"; }
warn() { echo "[WARN] $*" >&2; WARNINGS=$((WARNINGS + 1)); }
err()  { echo "[ERROR] $*" >&2; }
die()  { err "$*"; exit 1; }

cleanup() {
  if [ -n "${WORKDIR}" ] && [ -d "${WORKDIR}" ]; then
    rm -rf "${WORKDIR}"
  fi
}

# ---------------------------------------------------------------------------
# Argument handling
# ---------------------------------------------------------------------------

need_val() {
  # need_val FLAG ARGC
  if [ "$2" -lt 2 ]; then
    die "Option $1 requires a value"
  fi
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --domain)   need_val "$1" $#; DOMAIN="$2"; shift 2;;
      --email)    need_val "$1" $#; EMAIL="$2"; shift 2;;
      --user)     need_val "$1" $#; OAUTH_USER_ARG="$2"; shift 2;;
      --password) need_val "$1" $#; OAUTH_PASS_ARG="$2"; shift 2;;
      --port)     need_val "$1" $#; PORT_ARG="$2"; shift 2;;
      --version)  need_val "$1" $#; VERSION="$2"; shift 2;;
      --binary)   need_val "$1" $#; LOCAL_BINARY="$2"; shift 2;;
      --cert)     need_val "$1" $#; CERT_PATH="$2"; shift 2;;
      --key)      need_val "$1" $#; KEY_PATH="$2"; shift 2;;
      --staging)  STAGING=true; shift;;
      -h|--help)  usage; exit 0;;
      *) err "Unknown argument: $1"; usage >&2; exit 1;;
    esac
  done
}

validate_args() {
  [ -n "${DOMAIN}" ] || { usage >&2; die "--domain is required"; }
  DOMAIN=$(printf '%s' "${DOMAIN}" | tr '[:upper:]' '[:lower:]')
  DOMAIN="${DOMAIN%.}"
  if [ "${#DOMAIN}" -gt 253 ] || ! [[ "${DOMAIN}" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
    die "Invalid domain '${DOMAIN}'. Use a fully qualified hostname (use punycode for IDN); IP addresses are not supported."
  fi

  if [ -n "${EMAIL}" ] && ! [[ "${EMAIL}" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]; then
    die "Invalid email address: ${EMAIL}"
  fi

  if [ -n "${OAUTH_USER_ARG}" ] && ! [[ "${OAUTH_USER_ARG}" =~ ^[A-Za-z0-9._@-]+$ ]]; then
    die "--user may only contain letters, digits and . _ @ -"
  fi

  if [ -n "${PORT_ARG}" ]; then
    if ! [[ "${PORT_ARG}" =~ ^[0-9]+$ ]] || [ "${PORT_ARG}" -lt 1024 ] || [ "${PORT_ARG}" -gt 65535 ]; then
      die "--port must be a number between 1024 and 65535"
    fi
  fi

  if [ -n "${OAUTH_PASS_ARG}" ]; then
    if [ "${#OAUTH_PASS_ARG}" -lt 12 ]; then
      die "Password must be at least 12 characters"
    fi
    # The value is written unquoted into a systemd EnvironmentFile.
    if [[ "${OAUTH_PASS_ARG}" =~ [[:space:]\"\'\\\`\$#] ]]; then
      die "Password must not contain whitespace or any of: \" ' \\ \` \$ #"
    fi
  fi

  if [ -n "${CERT_PATH}" ] || [ -n "${KEY_PATH}" ]; then
    [ -n "${CERT_PATH}" ] && [ -n "${KEY_PATH}" ] || die "--cert and --key must be given together"
    case "${CERT_PATH}" in /*) ;; *) die "--cert must be an absolute path";; esac
    case "${KEY_PATH}" in /*) ;; *) die "--key must be an absolute path";; esac
    [ -r "${CERT_PATH}" ] || die "Certificate not readable: ${CERT_PATH}"
    [ -r "${KEY_PATH}" ] || die "Private key not readable: ${KEY_PATH}"
    if [ "${STAGING}" = true ]; then
      warn "--staging is ignored when --cert is used"
    fi
  fi

  if [ -n "${LOCAL_BINARY}" ] && [ ! -f "${LOCAL_BINARY}" ]; then
    die "--binary file not found: ${LOCAL_BINARY}"
  fi
}

# ---------------------------------------------------------------------------
# Environment detection and preflight
# ---------------------------------------------------------------------------

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    die "This script must run as root (try: curl ... | sudo bash -s -- ...)"
  fi
}

detect_platform() {
  local os machine
  os=$(uname -s)
  [ "${os}" = "Linux" ] || die "Unsupported OS: ${os} (Linux only)"

  machine=$(uname -m | tr '[:upper:]' '[:lower:]')
  case "${machine}" in
    x86_64|amd64)  ARCH="amd64";;
    aarch64|arm64) ARCH="arm64";;
    *) die "Unsupported architecture: ${machine} (amd64 and arm64 only)";;
  esac

  if command -v apt-get >/dev/null 2>&1; then
    PKG="apt"
  elif command -v dnf >/dev/null 2>&1; then
    PKG="dnf"
  elif command -v yum >/dev/null 2>&1; then
    PKG="yum"
  else
    die "No supported package manager found (need apt, dnf or yum)"
  fi

  [ -d /run/systemd/system ] || die "systemd is required but is not running as init"

  if [ -e /proc/net/if_inet6 ]; then
    HAS_IPV6=1
  fi
}

# port_holders PORT -> prints sorted unique process names listening on PORT
# ("unknown" if a listener exists but its owner cannot be determined).
port_holders() {
  local out names
  out=$(ss -H -ltnp "( sport = :$1 )" 2>/dev/null || true)
  [ -n "${out}" ] || return 0
  names=$(printf '%s\n' "${out}" | grep -o '(("[^"]*"' | sed -E 's/^\(\("//; s/"$//' | sort -u || true)
  if [ -n "${names}" ]; then
    printf '%s\n' "${names}"
  else
    echo "unknown"
  fi
}

check_port_free() {
  # check_port_free PORT ALLOWED_PROCESS_NAME DESCRIPTION
  local holders bad
  holders=$(port_holders "$1")
  [ -n "${holders}" ] || return 0
  bad=$(printf '%s\n' "${holders}" | grep -vx "$2" || true)
  if [ -n "${bad}" ]; then
    die "Port $1 ($3) is already in use by: $(printf '%s' "${bad}" | tr '\n' ' ')- free it or choose another port"
  fi
}

public_ip() {
  # public_ip 4|6
  curl "-$1" -fsS --max-time 5 https://api.ipify.org 2>/dev/null \
    || curl "-$1" -fsS --max-time 5 https://ifconfig.me 2>/dev/null \
    || true
}

preflight() {
  log "Preflight checks"

  if command -v ss >/dev/null 2>&1; then
    check_port_free 80 nginx "HTTP / ACME challenge"
    check_port_free 443 nginx "HTTPS"
    check_port_free "${PORT}" "${APP_NAME}" "mms local port"
  else
    warn "'ss' not found; skipping port availability checks"
  fi

  if ! command -v curl >/dev/null 2>&1; then
    return 0
  fi

  local resolved ip4 ip6 match=false ip
  resolved=$(getent ahosts "${DOMAIN}" 2>/dev/null | awk '{print $1}' | sort -u || true)
  if [ -z "${resolved}" ]; then
    warn "${DOMAIN} does not resolve yet. Create the A/AAAA record before requesting a certificate."
    return 0
  fi
  ip4=$(public_ip 4)
  if [ "${HAS_IPV6}" -eq 1 ]; then
    ip6=$(public_ip 6)
  else
    ip6=""
  fi
  if [ -z "${ip4}${ip6}" ]; then
    warn "Could not determine this machine's public IP; skipping DNS check"
    return 0
  fi
  for ip in ${ip4} ${ip6}; do
    if printf '%s\n' "${resolved}" | grep -qxF "${ip}"; then
      match=true
    fi
  done
  if [ "${match}" = false ]; then
    warn "${DOMAIN} resolves to $(printf '%s' "${resolved}" | tr '\n' ' ' | sed 's/ $//') but this machine's public IP is ${ip4:-n/a} ${ip6}. Expected if you use a CDN/proxy; otherwise certificate issuance will fail."
  fi
}

# ---------------------------------------------------------------------------
# Packages
# ---------------------------------------------------------------------------

install_packages() {
  log "Installing packages"
  case "${PKG}" in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq
      apt-get install -y -qq nginx curl ca-certificates openssl iproute2 >/dev/null
      if [ -z "${CERT_PATH}" ]; then
        apt-get install -y -qq certbot >/dev/null
      fi
      ;;
    dnf|yum)
      "${PKG}" install -y -q nginx curl ca-certificates openssl iproute >/dev/null
      if [ -z "${CERT_PATH}" ] && ! command -v certbot >/dev/null 2>&1; then
        if ! "${PKG}" install -y -q certbot >/dev/null 2>&1; then
          log "certbot not in base repos; enabling EPEL"
          "${PKG}" install -y -q epel-release >/dev/null \
            || die "Could not enable EPEL. Install certbot manually or pass --cert/--key."
          "${PKG}" install -y -q certbot >/dev/null \
            || die "Could not install certbot. Install it manually or pass --cert/--key."
        fi
      fi
      ;;
  esac
  command -v nginx >/dev/null 2>&1 || die "nginx installation failed"
  if [ -z "${CERT_PATH}" ]; then
    command -v certbot >/dev/null 2>&1 || die "certbot installation failed"
  fi
}

# ---------------------------------------------------------------------------
# Binary
# ---------------------------------------------------------------------------

resolve_version() {
  if [ "${VERSION}" = "latest" ]; then
    local final
    final=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest") \
      || die "Failed to resolve latest release from GitHub"
    VERSION="${final##*/}"
    [ -n "${VERSION}" ] && [ "${VERSION}" != "latest" ] || die "Could not determine the latest release tag"
  fi
  case "${VERSION}" in v*) ;; *) VERSION="v${VERSION}";; esac
}

install_binary() {
  WORKDIR=$(mktemp -d)
  local src

  if [ -n "${LOCAL_BINARY}" ]; then
    log "Using local binary ${LOCAL_BINARY}"
    src="${LOCAL_BINARY}"
  else
    resolve_version
    local ver asset base
    ver="${VERSION#v}"
    asset="${APP_NAME}_${ver}_linux_${ARCH}.tar.gz"
    base="https://github.com/${REPO}/releases/download/${VERSION}"

    log "Downloading ${asset} (${VERSION})"
    curl -fsSL --retry 3 --connect-timeout 15 -o "${WORKDIR}/${asset}" "${base}/${asset}" \
      || die "Download failed: ${base}/${asset}"
    curl -fsSL --retry 3 --connect-timeout 15 -o "${WORKDIR}/checksums.txt" "${base}/checksums.txt" \
      || die "Download failed: ${base}/checksums.txt"

    log "Verifying SHA-256 checksum"
    local line
    line=$(grep -E "[[:space:]]\*?${asset//./\\.}\$" "${WORKDIR}/checksums.txt" || true)
    [ -n "${line}" ] || die "${asset} not listed in checksums.txt"
    ( cd "${WORKDIR}" && printf '%s\n' "${line}" | sha256sum -c --status - ) \
      || die "Checksum verification FAILED for ${asset}; refusing to install"

    mkdir "${WORKDIR}/x"
    tar -xzf "${WORKDIR}/${asset}" -C "${WORKDIR}/x"
    src="${WORKDIR}/x/${APP_NAME}"
    [ -f "${src}" ] || die "Binary '${APP_NAME}' not found in archive"
  fi

  log "Installing ${BIN_PATH}"
  install -m 0755 -o root -g root "${src}" "${BIN_PATH}.new"
  mv -f "${BIN_PATH}.new" "${BIN_PATH}"
}

# ---------------------------------------------------------------------------
# User, directories, env file
# ---------------------------------------------------------------------------

setup_user_dirs() {
  if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    log "Creating system user '${SERVICE_USER}'"
    local nologin
    nologin=$(command -v nologin || echo /usr/sbin/nologin)
    useradd --system --user-group --home-dir "${DATA_DIR}" --no-create-home \
      --shell "${nologin}" "${SERVICE_USER}"
  fi
  install -d -m 0700 -o "${SERVICE_USER}" -g "${SERVICE_USER}" "${DATA_DIR}"
  install -d -m 0755 -o root -g root "${CONF_DIR}"
}

env_get() {
  # env_get KEY -> value from the env file (empty if absent)
  [ -f "${ENV_FILE}" ] || return 0
  grep -m1 "^$1=" "${ENV_FILE}" | cut -d= -f2- || true
}

env_set() {
  # env_set KEY VALUE ; value goes through the environment, never argv
  local tmp
  tmp=$(mktemp "${ENV_FILE}.XXXXXX")
  KEY="$1" VAL="$2" awk '
    BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VAL"]; done = 0 }
    index($0, k "=") == 1 { print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }
  ' "${ENV_FILE}" > "${tmp}"
  chown root:root "${tmp}"
  chmod 0600 "${tmp}"
  mv -f "${tmp}" "${ENV_FILE}"
}

write_env() {
  local existing_pass existing_user existing_port existing_mem
  if [ -f "${ENV_FILE}" ]; then
    log "Updating ${ENV_FILE} (existing password and data preserved)"
  else
    log "Creating ${ENV_FILE}"
    ( umask 077; : > "${ENV_FILE}" )
    chown root:root "${ENV_FILE}"
    chmod 0600 "${ENV_FILE}"
  fi

  existing_pass=$(env_get OAUTH_PASS)
  existing_user=$(env_get OAUTH_USER)
  existing_port=$(env_get MMS_PORT)
  existing_mem=$(env_get MEMORY_FILE_PATH)

  # Port: explicit flag > existing > default
  PORT="${PORT_ARG:-${existing_port:-8080}}"
  # User: explicit flag > existing > default
  OAUTH_USER_VAL="${OAUTH_USER_ARG:-${existing_user:-admin}}"

  local pass
  if [ -n "${OAUTH_PASS_ARG}" ]; then
    pass="${OAUTH_PASS_ARG}"
  elif [ -n "${existing_pass}" ]; then
    pass="${existing_pass}"
  else
    pass=$(openssl rand -hex 18)
    PASS_GENERATED=true
  fi

  env_set MMS_PUBLIC_URL "https://${DOMAIN}"
  env_set MMS_PORT "${PORT}"
  env_set MEMORY_FILE_PATH "${existing_mem:-${DATA_DIR}/memory.db}"
  env_set OAUTH_USER "${OAUTH_USER_VAL}"
  env_set OAUTH_PASS "${pass}"
}

# ---------------------------------------------------------------------------
# systemd
# ---------------------------------------------------------------------------

write_unit() {
  log "Writing ${UNIT_FILE}"
  cat > "${UNIT_FILE}" <<UNIT
[Unit]
Description=Memory MCP Server (mms)
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
ExecStart=${BIN_PATH}
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}
Restart=on-failure
RestartSec=3
UMask=0077
LimitNOFILE=65536

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
UNIT
  chmod 0644 "${UNIT_FILE}"
}

start_service() {
  log "Starting ${APP_NAME}.service"
  systemctl daemon-reload
  systemctl enable "${APP_NAME}.service" >/dev/null 2>&1
  systemctl restart "${APP_NAME}.service"

  for _ in $(seq 1 15); do
    if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
      log "Local health check OK (127.0.0.1:${PORT}/healthz)"
      return 0
    fi
    sleep 1
  done
  err "mms did not become healthy on 127.0.0.1:${PORT} within 15s. Last log lines:"
  journalctl -u "${APP_NAME}" -n 30 --no-pager >&2 || true
  exit 1
}

# ---------------------------------------------------------------------------
# nginx
# ---------------------------------------------------------------------------

nginx_conf_path() { echo "/etc/nginx/conf.d/mms-${DOMAIN}.conf"; }

detect_h2_mode() {
  local ver
  ver=$(nginx -v 2>&1 | sed -nE 's#.*nginx/([0-9]+\.[0-9]+\.[0-9]+).*#\1#p')
  if [ -z "${ver}" ]; then
    H2_MODE="directive"
    return 0
  fi
  if [ "$(printf '%s\n1.25.1\n' "${ver}" | sort -V | head -n1)" = "1.25.1" ]; then
    H2_MODE="directive"
  else
    H2_MODE="listen"
  fi
}

render_nginx_acme() {
  local l80 l80v6=""
  l80="    listen 80;"
  if [ "${HAS_IPV6}" -eq 1 ]; then
    l80v6=$'\n'"    listen [::]:80;"
  fi
  cat <<EOF
# Managed by deploy-vps.sh (phase 1: ACME bootstrap). Will be replaced.
server {
${l80}${l80v6}
    server_name ${DOMAIN};

    location /.well-known/acme-challenge/ {
        root ${ACME_ROOT};
    }

    location / {
        return 404;
    }
}
EOF
}

# Uses globals: DOMAIN PORT CERT_PATH KEY_PATH H2_MODE HAS_IPV6 ACME_ROOT
render_nginx_final() {
  local safe cert key l80 l443 h2dir="" h2l="" proxy tpl
  safe="${DOMAIN//[^a-zA-Z0-9]/_}"

  if [ -n "${CERT_PATH}" ]; then
    cert="${CERT_PATH}"
    key="${KEY_PATH}"
  else
    cert="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
    key="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"
  fi

  if [ "${H2_MODE}" = "directive" ]; then
    h2dir="    http2 on;"
  else
    h2l=" http2"
  fi

  l80="    listen 80;"
  l443="    listen 443 ssl${h2l};"
  if [ "${HAS_IPV6}" -eq 1 ]; then
    l80+=$'\n'"    listen [::]:80;"
    l443+=$'\n'"    listen [::]:443 ssl${h2l};"
  fi

  proxy=$(cat <<'PROXY'
        proxy_pass http://127.0.0.1:@PORT@;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
PROXY
)

  tpl=$(cat <<'TPL'
# Managed by deploy-vps.sh - changes will be overwritten on the next run.
# Rate limit zones (this file lives in conf.d, i.e. http context).
limit_req_zone $binary_remote_addr zone=mms_auth_@SAFE@:1m rate=10r/m;
limit_req_zone $binary_remote_addr zone=mms_token_@SAFE@:1m rate=30r/m;

server {
@L80@
    server_name @DOMAIN@;

    location /.well-known/acme-challenge/ {
        root @ACME_ROOT@;
    }

    location / {
        return 301 https://@DOMAIN@$request_uri;
    }
}

server {
@L443@
@H2DIR@
    server_name @DOMAIN@;
    server_tokens off;

    ssl_certificate     @CERT@;
    ssl_certificate_key @KEY@;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:MMS_SSL_@SAFE@:10m;
    ssl_session_timeout 1d;
    ssl_session_tickets off;

    add_header Strict-Transport-Security "max-age=31536000" always;

    # MCP endpoint (streamable HTTP / SSE): no buffering, long timeouts.
    location /mcp {
        client_max_body_size 8m;
@PROXY@
        proxy_buffering off;
        proxy_cache off;
        proxy_request_buffering off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }

    # OAuth discovery (no rate limit)
    location = /.well-known/oauth-protected-resource {
@PROXY@
    }
    location = /.well-known/oauth-protected-resource/mcp {
@PROXY@
    }
    location = /.well-known/oauth-authorization-server {
@PROXY@
    }

    # OAuth endpoints (rate limited)
    location = /authorize {
        limit_req zone=mms_auth_@SAFE@ burst=20 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
@PROXY@
    }
    location = /register {
        limit_req zone=mms_auth_@SAFE@ burst=20 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
@PROXY@
    }
    location = /token {
        limit_req zone=mms_token_@SAFE@ burst=30 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
@PROXY@
    }
    location = /revoke {
        limit_req zone=mms_token_@SAFE@ burst=30 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
@PROXY@
    }

    # Everything else (including /healthz) is not exposed.
    location / {
        return 404;
    }
}
TPL
)

  proxy="${proxy//@PORT@/"${PORT}"}"
  tpl="${tpl//@PROXY@/"${proxy}"}"
  tpl="${tpl//@L80@/"${l80}"}"
  tpl="${tpl//@L443@/"${l443}"}"
  tpl="${tpl//@H2DIR@/"${h2dir}"}"
  tpl="${tpl//@SAFE@/"${safe}"}"
  tpl="${tpl//@DOMAIN@/"${DOMAIN}"}"
  tpl="${tpl//@ACME_ROOT@/"${ACME_ROOT}"}"
  tpl="${tpl//@CERT@/"${cert}"}"
  tpl="${tpl//@KEY@/"${key}"}"
  printf '%s\n' "${tpl}"
}

nginx_apply() {
  if ! nginx -t >/dev/null 2>&1; then
    err "nginx configuration test failed:"
    nginx -t >&2 || true
    exit 1
  fi
  systemctl enable nginx >/dev/null 2>&1 || true
  if systemctl is-active --quiet nginx; then
    systemctl reload nginx
  else
    systemctl start nginx
  fi
}

ensure_hash_bucket() {
  # Long domains need a bigger server_names_hash_bucket_size (http context).
  [ "${#DOMAIN}" -gt 40 ] || return 0
  if nginx -T 2>/dev/null | grep -q 'server_names_hash_bucket_size'; then
    return 0
  fi
  log "Long domain name: setting server_names_hash_bucket_size 128"
  printf 'server_names_hash_bucket_size 128;\n' > /etc/nginx/conf.d/00-mms-hash.conf
}

setup_nginx() {
  local conf
  conf=$(nginx_conf_path)
  detect_h2_mode
  install -d -m 0755 -o root -g root "${ACME_ROOT}"
  install -d -m 0755 /etc/nginx/conf.d
  ensure_hash_bucket

  if [ -z "${CERT_PATH}" ]; then
    if [ -s "/etc/letsencrypt/live/${DOMAIN}/fullchain.pem" ]; then
      log "Existing Let's Encrypt certificate found for ${DOMAIN}; reusing it"
    else
      log "Phase 1: nginx on port 80 for the ACME challenge"
      # Keep any working config so a failed certbot run can't take HTTPS down.
      local had_conf=false
      if [ -f "${conf}" ]; then
        cp -f "${conf}" "${conf}.bak"
        had_conf=true
      fi
      render_nginx_acme > "${conf}"
      nginx_apply

      local args=(certonly --webroot -w "${ACME_ROOT}" -d "${DOMAIN}" --cert-name "${DOMAIN}"
        --non-interactive --agree-tos --keep-until-expiring)
      if [ -n "${EMAIL}" ]; then
        args+=(--email "${EMAIL}")
      else
        args+=(--register-unsafely-without-email)
      fi
      if [ "${STAGING}" = true ]; then
        args+=(--staging)
      fi
      log "Requesting certificate for ${DOMAIN}"
      if ! certbot "${args[@]}"; then
        err "certbot failed. Check that ${DOMAIN} resolves to this server and port 80 is reachable from the internet."
        err "Details: /var/log/letsencrypt/letsencrypt.log"
        if [ "${had_conf}" = true ]; then
          err "Restoring the previous nginx configuration."
          mv -f "${conf}.bak" "${conf}"
          nginx_apply
        fi
        exit 1
      fi
      rm -f "${conf}.bak"
    fi
    install_renewal_hook
  fi

  log "Phase 2: final nginx configuration (${conf})"
  render_nginx_final > "${conf}"
  chmod 0644 "${conf}"
  nginx_apply
}

install_renewal_hook() {
  install -d -m 0755 "$(dirname "${DEPLOY_HOOK}")"
  cat > "${DEPLOY_HOOK}" <<'HOOK'
#!/bin/sh
# Managed by deploy-vps.sh: reload nginx after a certificate renewal.
systemctl reload nginx
HOOK
  chmod 0755 "${DEPLOY_HOOK}"

  local t
  for t in certbot.timer certbot-renew.timer snap.certbot.renew.timer; do
    if systemctl list-unit-files "${t}" 2>/dev/null | grep -q "^${t}"; then
      systemctl enable --now "${t}" >/dev/null 2>&1 || warn "Could not enable ${t}"
      return 0
    fi
  done
  warn "No certbot renewal timer found; certificates will NOT renew automatically. Add a cron job running 'certbot renew'."
}

# ---------------------------------------------------------------------------
# Firewall / SELinux
# ---------------------------------------------------------------------------

setup_firewall() {
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
    log "Opening ports 80, 443 in ufw"
    ufw allow 80/tcp >/dev/null
    ufw allow 443/tcp >/dev/null
  fi
  if command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld; then
    log "Opening ports 80, 443 in firewalld"
    firewall-cmd --permanent --add-service=http >/dev/null
    firewall-cmd --permanent --add-service=https >/dev/null
    firewall-cmd --reload >/dev/null
  fi
}

setup_selinux() {
  if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce 2>/dev/null)" = "Enforcing" ]; then
    if command -v setsebool >/dev/null 2>&1; then
      log "SELinux enforcing: allowing nginx to connect to the local backend"
      setsebool -P httpd_can_network_connect 1 || warn "setsebool httpd_can_network_connect failed; nginx may return 502"
    else
      warn "SELinux is enforcing but setsebool is missing; nginx may return 502"
    fi
  fi
}

# ---------------------------------------------------------------------------
# Verification and summary
# ---------------------------------------------------------------------------

verify_public() {
  log "Verifying the public endpoint"
  local resolve="${DOMAIN}:443:127.0.0.1"
  local meta="https://${DOMAIN}/.well-known/oauth-protected-resource/mcp"
  local trusted=true code

  if ! curl -fsS --max-time 10 --resolve "${resolve}" -o /dev/null "${meta}" 2>/dev/null; then
    if curl -fsS -k --max-time 10 --resolve "${resolve}" -o /dev/null "${meta}" 2>/dev/null; then
      trusted=false
      warn "HTTPS works but the certificate is not trusted by this host (expected for self-signed or --staging certificates)."
    else
      warn "Could not fetch ${meta} through nginx. Check: nginx -t, journalctl -u nginx, journalctl -u mms."
      return 0
    fi
  fi

  local k=()
  if [ "${trusted}" = false ]; then
    k=(-k)
  fi
  code=$(curl -sS "${k[@]}" --max-time 10 --resolve "${resolve}" -o /dev/null -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' -d '{}' "https://${DOMAIN}/mcp" 2>/dev/null || true)
  if [ "${code}" = "401" ]; then
    log "Unauthenticated POST /mcp -> 401 (OAuth enforced)"
  else
    warn "Expected 401 from unauthenticated POST https://${DOMAIN}/mcp, got '${code:-no response}'."
  fi
}

print_summary() {
  local pass_note
  echo
  echo "=============================================================="
  echo " mms deployed"
  echo "=============================================================="
  echo " MCP URL   : https://${DOMAIN}/mcp"
  echo " Username  : ${OAUTH_USER_VAL}"
  if [ "${PASS_GENERATED}" = true ]; then
    pass_note=$(env_get OAUTH_PASS)
    echo " Password  : ${pass_note}"
    echo "             (generated; stored in ${ENV_FILE}, shown only this once)"
  else
    echo " Password  : unchanged (see ${ENV_FILE}, readable by root only)"
  fi
  echo
  echo " Claude (web/desktop): Settings > Connectors > Add custom connector"
  echo "   and enter https://${DOMAIN}/mcp"
  echo " Claude Code:"
  echo "   claude mcp add --transport http memory https://${DOMAIN}/mcp"
  echo
  echo " Logs      : journalctl -u mms -f"
  echo " Upgrade   : re-run this script (password and data are kept)"
  if [ "${WARNINGS}" -gt 0 ]; then
    echo
    echo " Completed with ${WARNINGS} warning(s) - see messages above."
  fi
}

main() {
  # Never let child processes consume the rest of a piped script.
  exec </dev/null
  trap cleanup EXIT

  parse_args "$@"
  validate_args
  require_root
  detect_platform
  PORT="${PORT_ARG:-$(env_get MMS_PORT)}"
  PORT="${PORT:-8080}"
  preflight
  install_packages
  install_binary
  setup_user_dirs
  write_env
  write_unit
  start_service
  setup_selinux
  setup_nginx
  setup_firewall
  verify_public
  print_summary
}

main "$@"
