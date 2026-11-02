#!/usr/bin/env bash
# Copyright (c) 2025 okooo5km(十里)
# Licensed under the MIT License.
#
# verify-deploy.sh - acceptance checks for a deployed Memory MCP Server.
#
# Usage:
#   OAUTH_USER=admin OAUTH_PASS=... scripts/verify-deploy.sh https://mcp.example.com
#   MMS_AUTH_BEARER=... scripts/verify-deploy.sh https://mcp.example.com     (bearer mode)
#
# Options:
#   --insecure              accept a self-signed certificate (curl -k)
#   --resolve HOST:PORT:IP  pass curl --resolve (test a domain before DNS exists)
#   --mcp-path PATH         MCP endpoint path (default /mcp; must match --http-endpoint)
#   --rate-limit            also run the login lockout check. WARNING: it locks the
#                           client address out of the login page for 15 minutes.
#   -h, --help              show this help
#
# Side effects: the OAuth mode registers one throwaway client ("verify-deploy")
# in oauth.db, submits one wrong password (counts toward the login limiter until
# the next successful login from the same address) and revokes the tokens it
# obtains. Nothing is written to the knowledge graph.
#
# Requires: bash, curl, openssl, sed, grep, awk. Exit status is non-zero when
# any check fails.

set -u -o pipefail

INSECURE=0
RESOLVE=""
MCP_PATH="/mcp"
RATE_LIMIT=0
BASE=""

usage() { sed -n '/^# verify-deploy.sh/,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --insecure) INSECURE=1; shift ;;
    --resolve) [ $# -ge 2 ] || { echo "--resolve needs a value" >&2; exit 2; }; RESOLVE="$2"; shift 2 ;;
    --mcp-path) [ $# -ge 2 ] || { echo "--mcp-path needs a value" >&2; exit 2; }; MCP_PATH="$2"; shift 2 ;;
    --rate-limit) RATE_LIMIT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown option: $1" >&2; exit 2 ;;
    *) [ -z "$BASE" ] || { echo "unexpected argument: $1" >&2; exit 2; }; BASE="$1"; shift ;;
  esac
done
[ -n "$BASE" ] || { usage >&2; exit 2; }
BASE="${BASE%/}"
case "$BASE" in
  http://*|https://*) ;;
  *) echo "BASE must start with http:// or https://" >&2; exit 2 ;;
esac
case "$MCP_PATH" in
  /*) ;;
  *) echo "--mcp-path must start with /" >&2; exit 2 ;;
esac
for tool in curl openssl sed grep awk; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
trap 'cd / && rm -rf "$TMP"' EXIT
# Work inside the temp dir and use relative file names, so curl (which may be a
# native build on Windows/MSYS) never has to resolve an MSYS-style /tmp path.
cd "$TMP" || exit 2
HDR="hdr"
BODY="body"
PWFILE="pw"
STATUS=""

PASS=0
FAILS=0
SKIPS=0

ok()   { PASS=$((PASS + 1)); echo "PASS  $1"; }
fail() { FAILS=$((FAILS + 1)); echo "FAIL  $1"; [ -z "${2:-}" ] || echo "      -> $2"; }
skip() { SKIPS=$((SKIPS + 1)); echo "SKIP  $1"; }

# req METHOD URL [curl args...]: sets STATUS, writes $HDR and $BODY.
req() {
  local method="$1" url="$2"
  shift 2
  local args=(-sS --max-time 30 -o "$BODY" -D "$HDR" -w '%{http_code}' -X "$method")
  [ "$INSECURE" -eq 0 ] || args+=(-k)
  [ -z "$RESOLVE" ] || args+=(--resolve "$RESOLVE")
  : >"$BODY"
  : >"$HDR"
  if ! STATUS="$(curl "${args[@]}" "$@" "$url" 2>err)"; then
    STATUS="000"
    echo "      (curl: $(tr -d '\r\n' <err))" >&2
  fi
}

# hdr NAME: value of the last response header NAME (case-insensitive), CR stripped.
hdr() {
  awk -v n="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" '
    { sub(/\r$/, "") }
    tolower(substr($0, 1, length(n) + 1)) == n ":" { v = substr($0, length(n) + 2); sub(/^[ \t]+/, "", v) }
    END { print v }' "$HDR"
}

# jstr KEY: first string value of "KEY" in the compact JSON body.
jstr() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p' "$BODY" | head -n 1; }

expect_status() { # name want [hint]
  if [ "$STATUS" = "$2" ]; then ok "$1 -> $2"; else fail "$1: expected HTTP $2, got $STATUS" "${3:-}"; fi
}

expect_eq() { # name got want [hint]
  if [ "$2" = "$3" ]; then ok "$1"; else fail "$1: got '$2', want '$3'" "${4:-}"; fi
}

JSON_HDRS=(-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream')
INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"verify-deploy","version":"1"}}}'
MCP_URL="$BASE$MCP_PATH"

if [ -n "${OAUTH_PASS:-}" ]; then
  MODE=oauth
  if [ -z "${OAUTH_USER:-}" ]; then echo "OAUTH_USER must be set together with OAUTH_PASS" >&2; exit 2; fi
elif [ -n "${MMS_AUTH_BEARER:-}" ]; then
  MODE=bearer
else
  MODE=none
fi
echo "Target: $BASE   endpoint: $MCP_PATH   mode: $MODE"
echo

# Runs initialize + notifications/initialized + tools/list + DELETE with a bearer
# token and checks for 13 tools.
mcp_session_checks() { # token
  local token="$1" sid n
  req POST "$MCP_URL" "${JSON_HDRS[@]}" -H "Authorization: Bearer $token" -d "$INIT"
  expect_status "initialize with token" 200 "401 means the token or Authorization header did not reach the server; 403 means a Host mismatch (see doc/deploy-custom.md section 7)"
  sid="$(hdr Mcp-Session-Id)"
  if grep -q '"protocolVersion"' "$BODY"; then ok "initialize result contains protocolVersion"; else fail "initialize result has no protocolVersion" "response body: $(head -c 200 "$BODY")"; fi
  if [ -z "$sid" ]; then
    fail "initialize returned no Mcp-Session-Id header" "the proxy stripped it, or the server runs with --http-stateless"
    return
  fi
  ok "Mcp-Session-Id header present"
  req POST "$MCP_URL" "${JSON_HDRS[@]}" -H "Authorization: Bearer $token" -H "Mcp-Session-Id: $sid" \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  expect_status "notifications/initialized" 202
  req POST "$MCP_URL" "${JSON_HDRS[@]}" -H "Authorization: Bearer $token" -H "Mcp-Session-Id: $sid" \
    -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  expect_status "tools/list" 200 "response buffering or a broken stream shows up as a timeout (000)"
  n="$(grep -o '"name":"[a-z_]*"' "$BODY" | sort -u | wc -l | tr -d ' ')"
  expect_eq "tools/list returns 13 tools" "$n" "13" "body: $(head -c 200 "$BODY")"
  req DELETE "$MCP_URL" -H "Authorization: Bearer $token" -H "Mcp-Session-Id: $sid"
  case "$STATUS" in 200|202|204) ok "DELETE session -> $STATUS" ;; *) fail "DELETE session: HTTP $STATUS" ;; esac
}

# --- Checks common to every mode -------------------------------------------

req POST "$MCP_URL" "${JSON_HDRS[@]}" -d "$INIT"
if [ "$MODE" = none ]; then
  expect_status "POST $MCP_PATH without credentials" 401 "the server must require authentication on a public origin; 000 means the proxy or TLS is not reachable"
else
  expect_status "POST $MCP_PATH without credentials" 401 "000: proxy or TLS unreachable; 403: Host header rejected; 502/504: server down; 200: authentication is OFF"
fi
WWW="$(hdr WWW-Authenticate)"
if [ -n "$WWW" ]; then ok "401 carries WWW-Authenticate"; else fail "401 has no WWW-Authenticate header" "the proxy strips it, or authentication is not enabled"; fi

req GET "$BASE/healthz"
if [ "$STATUS" = 200 ] && [ "$(tr -d '\r\n' <"$BODY")" = ok ]; then
  fail "/healthz is publicly reachable" "block /healthz at the proxy (route allow-list)"
else
  ok "/healthz is not exposed (HTTP $STATUS)"
fi

req GET "$BASE/.well-known/oauth-protected-resource" -H 'Host: wrong-host.invalid'
if [ "$STATUS" = 200 ] && grep -q '"resource"' "$BODY"; then
  fail "a request with a wrong Host header was served" "add a catch-all that refuses unknown hosts"
else
  ok "wrong Host header is not served (HTTP $STATUS)"
fi

# --- Bearer / unauthenticated modes ----------------------------------------

if [ "$MODE" != oauth ]; then
  req GET "$BASE/.well-known/oauth-authorization-server"
  expect_status "OAuth endpoints are not served in this mode" 404 "bearer mode does not serve OAuth; if this is 200, OAuth is enabled and you should use OAUTH_USER/OAUTH_PASS"
  if [ "$MODE" = bearer ]; then
    req POST "$MCP_URL" "${JSON_HDRS[@]}" -H 'Authorization: Bearer wrong-token' -d "$INIT"
    expect_status "wrong bearer token" 401
    mcp_session_checks "$MMS_AUTH_BEARER"
  else
    skip "authenticated checks: set OAUTH_USER and OAUTH_PASS, or MMS_AUTH_BEARER"
  fi
  echo
  echo "Result: $PASS passed, $FAILS failed, $SKIPS skipped"
  [ "$FAILS" -eq 0 ]
  exit $?
fi

# --- OAuth mode ------------------------------------------------------------

PRM_EXPECTED="$BASE/.well-known/oauth-protected-resource$MCP_PATH"
case "$WWW" in
  *"resource_metadata=\"$PRM_EXPECTED\""*) ok "WWW-Authenticate resource_metadata is $PRM_EXPECTED" ;;
  *) fail "WWW-Authenticate does not contain resource_metadata=\"$PRM_EXPECTED\"" "got: $WWW. MMS_PUBLIC_URL does not equal $BASE (scheme, host or port), or --http-endpoint differs from --mcp-path" ;;
esac
case "$WWW" in
  *'scope="mcp"'*) ok "WWW-Authenticate declares scope mcp" ;;
  *) fail "WWW-Authenticate has no scope=\"mcp\"" "got: $WWW" ;;
esac

req GET "$PRM_EXPECTED"
expect_status "GET protected resource metadata (path-suffixed)" 200 "the proxy does not route /.well-known/oauth-protected-resource$MCP_PATH"
expect_eq "PRM resource" "$(jstr resource)" "$MCP_URL" "MMS_PUBLIC_URL must be exactly $BASE with no path"
AS0="$(sed -n 's/.*"authorization_servers":\["\([^"]*\)".*/\1/p' "$BODY" | head -n 1)"
expect_eq "PRM authorization_servers[0]" "$AS0" "$BASE"

req GET "$BASE/.well-known/oauth-protected-resource"
expect_status "GET protected resource metadata (root)" 200

req GET "$BASE/.well-known/oauth-authorization-server"
expect_status "GET authorization server metadata" 200 "the proxy does not route /.well-known/oauth-authorization-server"
expect_eq "AS issuer" "$(jstr issuer)" "$BASE"
bad=""
for k in authorization_endpoint token_endpoint registration_endpoint revocation_endpoint; do
  v="$(jstr "$k")"
  case "$v" in "$BASE"/*) ;; *) bad="$bad $k=$v" ;; esac
done
if [ -z "$bad" ]; then ok "AS endpoints all start with $BASE/"; else fail "AS endpoints not under $BASE:$bad"; fi
case "$BASE" in
  https://*)
    if grep -q 'http://' "$BODY"; then fail "AS metadata contains http:// URLs" "the issuer is derived from the request: set MMS_PUBLIC_URL"; else ok "AS metadata has no http:// URL"; fi ;;
esac

REDIR="http://localhost:8765/cb"
req POST "$BASE/register" -H 'Content-Type: application/json' \
  -d '{"client_name":"verify-deploy","redirect_uris":["'"$REDIR"'"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}'
expect_status "POST /register (dynamic client registration)" 201 "429 is the registration limit (20/hour per client address); 404/405: /register not proxied; 403: Host mismatch"
CID="$(jstr client_id)"
if [ -n "$CID" ]; then ok "registration returned client_id"; else fail "registration returned no client_id"; fi

req POST "$BASE/register" -H 'Content-Type: application/json' -d '{"client_name":"bad","redirect_uris":["javascript:alert(1)"]}'
expect_status "POST /register with javascript: redirect is refused" 400

VERIFIER="$(openssl rand -hex 32)"
CHALLENGE="$(printf '%s' "$VERIFIER" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
STATE="$(openssl rand -hex 8)"
authz_query() { # redirect_uri
  printf 'client_id=%s&response_type=code&code_challenge=%s&code_challenge_method=S256&state=%s&resource=%s&redirect_uri=%s' \
    "$CID" "$CHALLENGE" "$STATE" "$MCP_URL" "$1"
}

req GET "$BASE/authorize?$(authz_query 'http%3A%2F%2Fevil.example%2Fcb')"
expect_status "/authorize with an unregistered redirect_uri" 400 "302 would be an open redirect; the server must show an error instead"
if grep -q 'redirect_uri does not match' "$BODY"; then ok "error says redirect_uri does not match"; else fail "error body lacks 'redirect_uri does not match'" "body: $(head -c 200 "$BODY")"; fi

req GET "$BASE/authorize?$(authz_query 'http%3A%2F%2Flocalhost%3A8765%2Fcb')"
expect_status "GET /authorize login page" 200
for pair in "X-Frame-Options:DENY" "X-Content-Type-Options:nosniff" "Cache-Control:no-store" "Referrer-Policy:no-referrer"; do
  name="${pair%%:*}"; want="${pair#*:}"
  got="$(hdr "$name")"
  case "$got" in
    *"$want"*) ok "login page header $name: $got" ;;
    *) fail "login page header $name is '$got', want '$want'" "the proxy rewrites or strips security headers" ;;
  esac
done
case "$(hdr Content-Security-Policy)" in
  *"default-src 'none'"*) ok "login page Content-Security-Policy present" ;;
  *) fail "login page Content-Security-Policy missing" "the proxy strips response headers" ;;
esac
if grep -q 'name="password"' "$BODY"; then ok "login page has a password field"; else fail "login page has no password field"; fi

# post_login PASSWORD_FILE: submits the login form, result in STATUS/BODY.
post_login() {
  req POST "$BASE/authorize" \
    --data-urlencode "client_id=$CID" --data-urlencode "redirect_uri=$REDIR" \
    --data-urlencode "state=$STATE" --data-urlencode "code_challenge=$CHALLENGE" \
    --data-urlencode "code_challenge_method=S256" --data-urlencode "response_type=code" \
    --data-urlencode "resource=$MCP_URL" --data-urlencode "username=$OAUTH_USER" \
    --data-urlencode "password@$1"
}

umask 077
printf '%s' "wrong-$(openssl rand -hex 6)" >badpw
post_login badpw
if [ "$STATUS" = 200 ] && grep -q 'Invalid username or password' "$BODY"; then
  ok "wrong password is rejected"
else
  fail "wrong password: HTTP $STATUS" "429 means this client address (or, behind a proxy, the proxy address) is locked out: see MMS_TRUSTED_PROXIES; body: $(head -c 120 "$BODY")"
fi

# Real login. The password reaches curl through a 0600 file, never through argv.
printf '%s' "$OAUTH_PASS" >"$PWFILE"
post_login "$PWFILE"
rm -f "$PWFILE"
CODE=""
if [ "$STATUS" = 200 ]; then
  CODE="$(sed -n 's/.*[?&]code=\([0-9a-f]\{64\}\).*/\1/p' "$BODY" | head -n 1)"
fi
if [ -n "$CODE" ]; then
  ok "login succeeded and returned an authorization code"
else
  fail "login did not return an authorization code (HTTP $STATUS)" "wrong OAUTH_USER/OAUTH_PASS, or the limiter has locked this address (429)"
fi

ACCESS=""
REFRESH=""
if [ -n "$CODE" ]; then
  req POST "$BASE/token" --data-urlencode "grant_type=authorization_code" --data-urlencode "code=$CODE" \
    --data-urlencode "code_verifier=$VERIFIER" --data-urlencode "client_id=$CID" \
    --data-urlencode "redirect_uri=$REDIR" --data-urlencode "resource=$MCP_URL"
  expect_status "POST /token (authorization_code + PKCE)" 200 "400 invalid_target: resource does not equal $MCP_URL; invalid_grant: PKCE or code problem"
  ACCESS="$(jstr access_token)"
  REFRESH="$(jstr refresh_token)"
  if [ -n "$ACCESS" ] && [ -n "$REFRESH" ]; then ok "token response has access_token and refresh_token"; else fail "token response lacks tokens"; fi
fi

if [ -n "$ACCESS" ]; then
  mcp_session_checks "$ACCESS"
  req POST "$BASE/revoke" --data-urlencode "token=$REFRESH" --data-urlencode "client_id=$CID"
  expect_status "POST /revoke refresh token" 200
  req POST "$BASE/revoke" --data-urlencode "token=$ACCESS" --data-urlencode "client_id=$CID"
  expect_status "POST /revoke access token" 200
  req POST "$MCP_URL" "${JSON_HDRS[@]}" -H "Authorization: Bearer $ACCESS" -d "$INIT"
  expect_status "revoked access token is refused" 401
else
  skip "authenticated MCP checks (no access token)"
fi

if [ "$RATE_LIMIT" -eq 1 ]; then
  echo
  echo "Rate limit check: submitting wrong passwords until the limiter answers (locks this address out for 15 minutes)"
  got429=0
  for i in 1 2 3 4 5 6 7; do
    post_login badpw
    if [ "$STATUS" = 429 ]; then
      got429=$i
      break
    fi
  done
  if [ "$got429" -ge 5 ] && [ "$got429" -le 6 ]; then
    ok "login limiter answered 429 on attempt $got429 (Retry-After: $(hdr Retry-After))"
  else
    fail "login limiter: first 429 on attempt '$got429' (expected 5 or 6)" "0 means no limiting happened"
  fi
else
  skip "login lockout check (run with --rate-limit; it locks this address out for 15 minutes)"
fi

echo
echo "Result: $PASS passed, $FAILS failed, $SKIPS skipped"
[ "$FAILS" -eq 0 ]
