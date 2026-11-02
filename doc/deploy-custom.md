# Custom Deployment Guide

This guide is for deploying `mms` (Memory MCP Server) **without** `scripts/deploy-vps.sh`: in Docker, behind Caddy or Traefik, next to other sites on an existing nginx, behind a tunnel, or with a static token. It is written for an AI coding agent that is told something like "deploy this on my Docker host behind Caddy". Read sections 1 to 4, copy one recipe from section 5, then run section 6. A human can follow it the same way.

Conventions: `mcp.example.com` is the public domain, `<PASSWORD>` and `<TOKEN>` are secrets you generate (rules in section 9), `BASE` is the public origin `https://mcp.example.com`. Everything marked "tested" was run on Docker Desktop 28.4 (Linux containers) with the versions named in [Verification status](#verification-status). Anything not run is marked **Unverified** or **not tested**.

## 1. Decision guide

| Situation | Use |
|-----------|-----|
| Fresh Debian, Ubuntu or RHEL-family VPS with root, ports 80/443 free, nothing else on it | **Use `scripts/deploy-vps.sh` instead** ([deploy.md](deploy.md)). Skip this guide. |
| Linux host with systemd, an existing nginx serving other sites | [5.1](#51-systemd--existing-nginx-shared-host) |
| Linux host with systemd, no web server, want automatic HTTPS | [5.2](#52-systemd--caddy-automatic-https) |
| Docker host, Caddy as the proxy | [5.3](#53-docker-compose--caddy) |
| Docker host that already runs Traefik v3 | [5.4](#54-docker-compose--traefik-v3) |
| No inbound ports can be opened (home server, NAT) | [5.5](#55-cloudflare-tunnel-cloudflared--unverified), **Unverified** |
| Only programmatic clients that can send a fixed `Authorization` header | [5.6](#56-static-bearer-token-no-oauth) (combine with any proxy recipe) |
| One user, the MCP client starts the process itself | [5.7](#57-local-only-stdio) |
| HAProxy, Apache, Kubernetes ingress, a cloud load balancer | No recipe. Apply the requirements in [section 4](#4-reverse-proxy-requirements) and pass [section 6](#6-acceptance-tests). |

Use `scripts/deploy-vps.sh` instead if: the machine is a fresh Debian/Ubuntu/RHEL-family VPS and nginx plus Let's Encrypt is acceptable. It already implements everything below.

## 2. Runtime contract

Every deployment must satisfy these invariants, whatever the proxy or orchestrator. Items are numbered so recipes and the failure table can refer to them.

**C1. One process.** Exactly one `mms` process owns the data files. No replicas, no rolling update with overlap, no two containers on one volume. Why: the stores are SQLite files written by one process (C5), the MCP sessions live in that process's memory (C9), and the login rate limiter is in memory.

**C2. Binary and build.** The binary is named `mms`. Pre-built archives are named `mms_<version>_linux_<amd64|arm64>.tar.gz` on the GitHub releases page of `okooo5km/memory-mcp-server-go`, with a `checksums.txt` (SHA-256). Building from source needs Go 1.25 or newer; `CGO_ENABLED=0` works (the SQLite driver is pure Go; the Dockerfile in 5.3 does exactly that). Before using a release, run `mms --help` and check that it lists `-public-url` (and `-trusted-proxies` if you run the proxy in another container or host). Options that an older binary does not know are not errors for the environment variables, they are silently ignored.

**C3. One public origin, at the root.** `MMS_PUBLIC_URL` is `scheme://host[:port]` with no path, no query, no userinfo (a path is rejected at startup). Subpaths (`https://example.com/mms/`) and several domains are **not supported**: the OAuth issuer is the origin itself, the metadata documents live at fixed root locations (`/.well-known/oauth-authorization-server`, `/authorize`, `/token`, ...), and the server accepts exactly one `Host` value (plus loopback). To serve two names, run a second instance with its own data directory.

**C4. What `MMS_PUBLIC_URL` does.** It is the single most important setting.
- It becomes the OAuth issuer and the base of the resource identifier `<public URL>/mcp`. Every URL in the metadata and in the `WWW-Authenticate` challenge is derived from it. Without it the issuer is guessed from the request's `Host` and `X-Forwarded-Proto` (only suitable for local use).
- It turns on the Host guard: `/mcp`, `/sse` and every OAuth route answer `403` (`Forbidden: invalid Host header`) unless the `Host` header equals the public host (with or without the default port) or is loopback. `/healthz` is exempt.
- It disables the SDK's DNS-rebinding check, which would otherwise reject proxied `Host` values on a loopback listener.
- It changes defaults: transport `http`, bind address `127.0.0.1`, and, if OAuth is enabled, the OAuth store at `oauth.db` next to the memory database (table in section 3).
- It must equal exactly what clients type: same scheme, same host, same port. `http://` on a non-loopback host triggers a startup warning.

**C5. State files.** Two SQLite databases in WAL mode:
- `memory.db` (the knowledge graph) at `MEMORY_FILE_PATH` / `--memory`. Use a path that ends in `.db`. A `.json` path selects the legacy JSONL backend for new installations.
- `oauth.db` (registered clients, hashed access and refresh tokens): the directory of the memory database, unless `OAUTH_STORE_PATH` says otherwise.
- Each database also has `-wal` and `-shm` files in the same directory. The directory must be **persistent**, **writable by the service user**, and on a **local filesystem**. NFS, SMB and other network filesystems do not support the shared memory WAL needs: do not use them. Back up with SQLite's backup command (section 8), not `cp` on live files.
- Relative paths are resolved against the directory of the executable, not the working directory. Always give absolute paths.

**C6. Permissions.** The service user needs read/write on the data directory and nothing else. `oauth.db` is forced to mode `0600`; `memory.db` follows the process umask (observed `0644` under umask 022). Set `UMask=0077` under systemd. Never run as root; the Docker recipes use uid 65532.

**C7. Ports.** One TCP port serves everything (`MMS_PORT`, default 8080). It speaks plain HTTP/1.1 and no TLS. The proxy terminates TLS. Bind it to `127.0.0.1` when the proxy runs on the same host (the default under `MMS_PUBLIC_URL`). When the proxy runs in another container or on another host, bind to a private interface (`MMS_HOST=0.0.0.0` inside a container whose port is **not** published) and tell the server which peers are proxies with `MMS_TRUSTED_PROXIES` (C10).

**C8. `/healthz`.** `GET /healthz` returns `200` with body `ok` (`text/plain`) without authentication and without the Host guard. It only proves that the HTTP listener is up; it does not touch the databases. It must be used only by local supervisors and must **not** be reachable from the internet (the allow-lists in section 5 omit it). Exposing it only tells the internet that the process is alive.

**C9. Graceful stop and sessions.** `SIGTERM` or `SIGINT` starts a graceful shutdown: the listener closes and in-flight requests get up to 5 seconds, then connections (including SSE streams) are closed. A stop timeout of 10 seconds or more is enough (systemd's default is longer; compose uses `stop_grace_period: 15s`). Streamable HTTP sessions (`Mcp-Session-Id`) are kept in memory with a 24 hour idle timeout. They are not shared between processes. A second replica would need sticky routing so each client always reaches the process holding its session, and it still could not share the SQLite files (C1, C5): do not run one.

**C10. Client address for rate limiting.** Failed logins are limited to 5 per 15 minutes per client address, and 50 per 15 minutes in total (the login page then answers `429` with `Retry-After`). The client address is the TCP peer, except that `X-Real-IP` is believed when the peer is loopback or inside `MMS_TRUSTED_PROXIES` (comma-separated CIDRs or bare IPs; an invalid entry stops the server at startup). `X-Forwarded-For` is never used. Consequences: (1) a proxy on the same host must still **overwrite** `X-Real-IP` with the real client address, because loopback is trusted; (2) a proxy in another container or host must be listed in `MMS_TRUSTED_PROXIES`, otherwise all users share the proxy's address and five bad logins lock everybody out for 15 minutes.

**C11. Restart behaviour.** After a restart all MCP sessions are gone: a request with an old `Mcp-Session-Id` gets `404` and the client must send `initialize` again. OAuth tokens and registered clients survive, provided `oauth.db` is on the persistent directory (tested: an access token issued before a restart was accepted by a new `initialize` afterwards). Without an OAuth store, every client must log in again after each restart.

**C12. Transports.** Use `http` (Streamable HTTP, endpoint `/mcp`) unless a client requires `sse` (endpoint `/sse`). `--http-endpoint` changes the MCP path and the path-suffixed metadata route with it (section 4).

## 3. Complete configuration reference

Flags win over environment variables. Flags with one dash or two are equivalent (`-port` and `--port`). Secrets are marked **secret**: pass them through the environment or an env file with mode `0600`, never in argv (visible in `ps` and in container metadata) and never in a file that is committed to git.

"Public-URL mode" is the column telling what changes when `MMS_PUBLIC_URL` (or `OAUTH_ISSUER`) is set and the option is not given explicitly.

| Flag | Environment | Default | Public-URL mode | Notes |
|------|-------------|---------|-----------------|-------|
| `-t`, `--transport` | `MMS_TRANSPORT` | `stdio` | becomes `http` | `stdio`, `http` (alias `streamable-http`) or `sse` |
| `-m`, `--memory` | `MEMORY_FILE_PATH` | `memory.json` next to the executable | no change | Use an absolute path ending in `.db`. The flag wins over the variable |
| `-p`, `--port` | `MMS_PORT` | `8080` | no change | 1 to 65535 |
| `--host` | `MMS_HOST` | all interfaces | becomes `127.0.0.1` | Bind address for `http` and `sse` |
| `--public-url` | `MMS_PUBLIC_URL` | empty | the switch itself | `scheme://host[:port]`, no path (C3, C4) |
| `--oauth-issuer` | `OAUTH_ISSUER` | empty | acts as the public URL | Alias. If both are set they must be equal |
| `--oauth-user` | `OAUTH_USER` | empty | no change | Login name. Must be set together with the password |
| `--oauth-pass` | `OAUTH_PASS` | empty | no change | **secret**. 12 or more characters (shorter triggers a warning) |
| `--oauth-store` | `OAUTH_STORE_PATH` | none: tokens in memory | becomes `<dir of memory db>/oauth.db` when OAuth is enabled | Durable clients and tokens |
| `--oauth-refresh-replay-grace` | `OAUTH_REFRESH_REPLAY_GRACE` | `75m` | no change | Go duration. Raise (for example `24h`) for clients with several workers sharing stale refresh tokens |
| `--auth-bearer` | `MMS_AUTH_BEARER` | empty | no change | **secret**. Static token. Mutually exclusive with OAuth: setting both is a startup error |
| `--trusted-proxies` | `MMS_TRUSTED_PROXIES` | empty (loopback only) | no change | CIDRs or IPs of proxies whose `X-Real-IP` is believed (C10). Invalid value is a startup error |
| `--http-endpoint`, `--http_ep` | `MMS_HTTP_ENDPOINT` | `/mcp` | no change | Starts with `/`, not `/`, not a built-in route. Ignored for `sse` |
| `--http-heartbeat` | none | `0` (off) | no change | Server-to-client ping interval, for example `30s`. Leave off: the SDK closes a session whose client does not answer pings |
| `--http-stateless` | none | off | no change | Streamable HTTP without sessions |
| `--cors-origin` | `MMS_CORS_ORIGIN` | `*` | no change | `*` or a comma-separated allow list. The server sets CORS headers itself (section 4) |
| `--storage` | none | auto-detected | no change | `sqlite` or `jsonl` |
| `--auto-migrate` | none | `true` | no change | Migrates a JSONL file to SQLite on start |
| `--migrate`, `--migrate-to`, `--dry-run`, `--force` | none | none | no change | One-shot JSONL to SQLite migration; the process exits afterwards. Not for services |
| `-v`, `--version` | none | | | Print the version and exit |
| `-h`, `--help` | none | | | Print usage and exit |

The only other environment variable the program reads is `MEMORY_FILE_PATH` (listed above). Environment variables set to an empty string count as unset.

Startup warnings (never containing secrets) are written to stderr: OAuth without a public URL on a non-loopback bind, a password shorter than 12 characters, an in-memory OAuth store, `http://` public URL on a non-loopback host, and OAuth enabled on a non-loopback bind with no trusted proxies.

## 4. Reverse proxy requirements

### 4.1 Routes

Proxy exactly these and answer `404` for everything else. The list is the OAuth route registration plus the MCP path.

| Path (exact match) | Methods | Present when |
|--------------------|---------|--------------|
| `/mcp` (or the `--http-endpoint` value; `/sse` for the `sse` transport) | `POST`, `GET`, `DELETE`, `OPTIONS` | always |
| `/.well-known/oauth-protected-resource` | `GET` | OAuth |
| `/.well-known/oauth-protected-resource/mcp` (the MCP path appended; `/.well-known/oauth-protected-resource/api/mcp` if the endpoint is `/api/mcp`) | `GET` | OAuth |
| `/.well-known/oauth-authorization-server` | `GET` | OAuth |
| `/register` | `POST` | OAuth |
| `/authorize` | `GET`, `POST` | OAuth |
| `/token` | `POST` | OAuth |
| `/revoke` | `POST` | OAuth |

Do **not** expose `/healthz`. In static bearer mode (5.6) only the MCP path exists; the OAuth routes return `404` from the server itself.

A custom `--http-endpoint /api/mcp` (tested) changes the challenge to `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/api/mcp"`, so the proxy allow-list must contain `/api/mcp` and `/.well-known/oauth-protected-resource/api/mcp`, and `verify-deploy.sh` takes `--mcp-path /api/mcp` (that option was not run against a custom endpoint).

### 4.2 Required behaviours

| # | Requirement | Why | Symptom when wrong |
|---|-------------|-----|--------------------|
| P1 | Preserve the client's `Host` header (do not rewrite it to the upstream name) | The Host guard compares it with `MMS_PUBLIC_URL` | `403`, body `Forbidden: invalid Host header`, on `/mcp` and all OAuth routes (tested). Fix: nginx `proxy_set_header Host $host;`; Caddy and Traefik preserve it by default |
| P2 | Set `X-Real-IP` to the real client address and **overwrite** any value the client sent | C10. If the peer is trusted and the header passes through untouched, a client can pick its own rate-limit key and dodge the lockout | Everyone locked out together (header missing or not trusted), or lockout trivially bypassed (header passed through). nginx: `proxy_set_header X-Real-IP $remote_addr;` Caddy: `header_up X-Real-IP {remote_host}` Traefik v3: automatic (tested, see 5.4) |
| P3 | Pass `Authorization` through | The bearer token is validated by `mms` | `401` on every call after login: clients loop through the OAuth flow |
| P4 | No response buffering on the MCP path | The standalone `GET` stream and POST responses are Server-Sent Events | Stream appears to hang; events arrive late or in bursts. nginx `proxy_buffering off;` Caddy `flush_interval -1`. Traefik does not buffer unless a Buffering middleware is attached |
| P5 | Upstream read timeout above the longest idle stream | An idle `GET` stream sends nothing for long periods | `504` or a closed stream after the timeout (nginx default `proxy_read_timeout` is 60 seconds). Use `1h` as in the nginx recipe. Caddy has no default read timeout; Traefik v3.7.17 with default settings kept an idle stream open for 200 seconds (tested) |
| P6 | Request body limit large enough | Knowledge-graph writes can be large; the OAuth endpoints are capped by the server at 64 KiB anyway | `413` from the proxy (nginx default `client_max_body_size` is 1 MB). The nginx recipe uses 8 MB on `/mcp` and 64 KB on OAuth routes |
| P7 | Speak HTTP/1.1 to the upstream | `mms` serves HTTP/1.1 only on its plain port (no h2c) | nginx defaults to HTTP/1.0 upstream: set `proxy_http_version 1.1;` and `proxy_set_header Connection "";`. Do not configure h2c upstreams in Caddy or Traefik |
| P8 | HTTP/2 (or HTTP/3) towards the client is fine | Terminated at the proxy | none (tested: HTTP/2 through nginx and Caddy; HTTP/3 not tested) |
| P9 | Do not strip or rewrite `WWW-Authenticate` or `Mcp-Session-Id` | The 401 challenge starts OAuth discovery; the session id keys the session | Clients cannot discover OAuth; every call after `initialize` fails (a stripped `Mcp-Session-Id` is not reproduced here) |
| P10 | Terminate TLS at the proxy and use `https` in `MMS_PUBLIC_URL` | The server serves no TLS; OAuth requires https off-loopback | Browser and client warnings, startup warning for `http://` |
| P11 | Do not add CORS headers or handle `OPTIONS` yourself | The server answers preflights and sets `Access-Control-*` itself | Duplicate `Access-Control-Allow-Origin` headers, which browsers reject |
| P12 | Allow-list the routes in 4.1 | Everything else (including `/healthz`) must stay private | A public `/healthz` (acceptance check 6 fails) |
| P13 | Send `X-Forwarded-Proto: https` only if `MMS_PUBLIC_URL` is **not** set | Without a public URL the issuer is derived from it | Metadata containing `http://` URLs (tested). With a public URL the header is ignored |
| P14 | Optionally answer unknown hostnames with a refusal | A catch-all stops the real certificate and app from answering for other names | Not required for correctness: the Host guard already returns `403` |

Rate limiting at the proxy is optional; the server limits logins and registrations (20 registrations per hour per client address) itself.

## 5. Recipes

All recipes assume DNS `A` (and `AAAA`) for `mcp.example.com` points at the host, and ports 80 and 443 are reachable for automatic certificates, unless the recipe says otherwise.

### 5.1 systemd + existing nginx (shared host)

Goal: add `mms` next to existing sites without touching nginx's global configuration. You add one file under `/etc/nginx/conf.d/` and reload. Ask the user before changing anything else (`nginx.conf`, other server blocks, `default_server`).

1. Install the binary: download and verify a release as described in C2, or build (`CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/mms .` in a checkout). Check `mms --version`.

2. User, directories, secrets:

```bash
useradd --system --user-group --home-dir /var/lib/mms --no-create-home --shell /usr/sbin/nologin mms
install -d -m 0700 -o mms -g mms /var/lib/mms
install -d -m 0755 /etc/mms
umask 077
PW="$(openssl rand -hex 18)"
cat > /etc/mms/mms.env <<EOF
MMS_PUBLIC_URL=https://mcp.example.com
MMS_PORT=8080
MEMORY_FILE_PATH=/var/lib/mms/memory.db
OAUTH_USER=admin
OAUTH_PASS=${PW}
EOF
chmod 600 /etc/mms/mms.env
unset PW   # the user reads OAUTH_PASS from this file themselves; never print it
```

3. `/etc/systemd/system/mms.service` (checked with `systemd-analyze verify`; same hardening as the script's unit):

```ini
[Unit]
Description=Memory MCP Server (mms)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mms
Group=mms
EnvironmentFile=/etc/mms/mms.env
ExecStart=/usr/local/bin/mms
WorkingDirectory=/var/lib/mms
Restart=on-failure
RestartSec=3
TimeoutStopSec=15
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
ReadWritePaths=/var/lib/mms

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now mms
curl -fsS http://127.0.0.1:8080/healthz    # prints: ok
```

4. nginx: use the "Manual nginx Configuration" block of [deploy.md](deploy.md#manual-nginx-configuration) unchanged (it is the configuration the script generates; it satisfies P1 to P12). Save it as `/etc/nginx/conf.d/mms-mcp.example.com.conf`, replace the domain and certificate paths, keep the zone names unique per domain (`limit_req_zone` names are global to nginx), and skip the optional catch-all if another server already holds `default_server`. Obtain the certificate first (`certbot certonly --webroot -w <webroot served on port 80> -d mcp.example.com`). Then `nginx -t && systemctl reload nginx`.

   The same block was tested with `proxy_pass` pointed at a separate container (`http://mms:8080`) instead of `127.0.0.1:8080`, on nginx 1.30.6 with a self-signed certificate: all acceptance checks passed, and with `MMS_TRUSTED_PROXIES` set to the nginx address, different clients were limited independently (section 6.4). When nginx is not on the same host as `mms`, set `MMS_HOST` to the private address `mms` should bind, keep that port unreachable from the internet, and set `MMS_TRUSTED_PROXIES` to the nginx address.

5. Run section 6.

### 5.2 systemd + Caddy (automatic HTTPS)

Steps 1 to 3 of 5.1 are identical. Then install Caddy (distribution package or the official repository) and put this block in `/etc/caddy/Caddyfile`. If the file already serves other sites, append the block; do not replace the file. Checked with `caddy validate` (Caddy 2.11.7); the same route block, with the upstream `mms:8080`, ran end to end in 5.3.

```caddyfile
mcp.example.com {
	@mms path /mcp /authorize /register /token /revoke /.well-known/oauth-protected-resource /.well-known/oauth-protected-resource/mcp /.well-known/oauth-authorization-server

	handle @mms {
		reverse_proxy 127.0.0.1:8080 {
			header_up X-Real-IP {remote_host}
			flush_interval -1
		}
	}

	handle {
		respond "Not found" 404
	}
}
```

```bash
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile && systemctl reload caddy
```

Caddy obtains and renews the certificate itself. That ACME step was **not** run here (it needs a public domain). `header_up X-Real-IP {remote_host}` is mandatory even on the same host: Caddy does not set `X-Real-IP`, and the loopback peer is trusted, so without it a client could send its own value.

### 5.3 Docker Compose + Caddy

Layout on the Docker host (all tested together):

```
/opt/mms/
  Dockerfile
  compose.yaml
  Caddyfile
  mms.env          # mode 0600, never committed
  src/             # git clone of the repository
```

```bash
mkdir -p /opt/mms && cd /opt/mms
git clone https://github.com/okooo5km/memory-mcp-server-go.git src
# optionally: git -C src checkout <release tag that lists -trusted-proxies in --help>
```

`Dockerfile` (builds from source, `CGO_ENABLED=0`, distroless runtime, uid 65532; built successfully from the current working tree):

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mms .
# Data directory owned by the distroless "nonroot" user (uid 65532). A new named
# volume mounted here inherits this ownership.
RUN mkdir /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mms /usr/local/bin/mms
COPY --from=build --chown=65532:65532 /out/data /data
ENV MEMORY_FILE_PATH=/data/memory.db
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/mms"]
```

`mms.env` (generate the password with `openssl rand -hex 18`; do not echo it):

```ini
MMS_PUBLIC_URL=https://mcp.example.com
OAUTH_USER=admin
OAUTH_PASS=<PASSWORD>
```

`compose.yaml`:

```yaml
name: mms

services:
  mms:
    build:
      context: ./src
      dockerfile: ../Dockerfile
    image: mms:local
    restart: unless-stopped
    env_file: mms.env
    environment:
      MMS_HOST: 0.0.0.0
      MMS_TRUSTED_PROXIES: 172.30.0.0/24
    volumes:
      - mms-data:/data
    stop_grace_period: 15s
    networks:
      - edge

  caddy:
    image: caddy:2
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config
    depends_on:
      - mms
    networks:
      - edge

networks:
  edge:
    ipam:
      config:
        - subnet: 172.30.0.0/24

volumes:
  mms-data:
  caddy-data:
  caddy-config:
```

`Caddyfile`:

```caddyfile
{
	# email you@example.com
	# local_certs
}

mcp.example.com {
	@mms path /mcp /authorize /register /token /revoke /.well-known/oauth-protected-resource /.well-known/oauth-protected-resource/mcp /.well-known/oauth-authorization-server

	handle @mms {
		reverse_proxy mms:8080 {
			header_up X-Real-IP {remote_host}
			flush_interval -1
		}
	}

	handle {
		respond "Not found" 404
	}
}
```

Start:

```bash
chmod 600 mms.env
docker compose run --rm --no-deps mms --help 2>&1 | grep trusted-proxies   # must print the flag; if empty, the source is too old
docker compose up -d --build
```

Notes, each tested unless stated:
- Only `caddy` publishes ports. `mms` is reachable only on the compose network `edge`, whose subnet is fixed to `172.30.0.0/24` so that `MMS_TRUSTED_PROXIES` can name it. If that subnet collides with an existing network (`docker network ls`, `docker network inspect <name>`), pick another `/24` and change it in both places. Trusting the whole subnet is safe only because nothing else but Caddy and `mms` is attached to `edge`; do not attach other containers to it.
- `MMS_HOST: 0.0.0.0` is required: the public-URL default (`127.0.0.1`) would make `mms` unreachable from the Caddy container.
- Volume permissions: the named volume `mms-data` inherits uid/gid 65532 from `/data` in the image, so the process can write (observed: `memory.db` and `oauth.db`, `oauth.db` with mode `0600`). With a bind mount (`./data:/data`) create the directory first and run `chown 65532:65532 ./data` (not tested).
- No `HEALTHCHECK`: a distroless image has no shell, `curl` or `wget`, and `mms` has no health subcommand. Options: (a) rely on `restart: unless-stopped` and the acceptance script; (b) probe from a sidecar on the same network, tested: `docker run --rm --network mms_edge curlimages/curl:latest -fsS http://mms:8080/healthz` prints `ok`; (c) use a runtime image that contains `wget`, tested with this tail instead of the distroless stage (`HEALTHCHECK` reported `healthy`):

```dockerfile
FROM alpine:3.22
RUN adduser -D -H -u 65532 mms && mkdir /data && chown mms:mms /data
COPY --from=build /out/mms /usr/local/bin/mms
ENV MEMORY_FILE_PATH=/data/memory.db
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/mms"]
```

- Certificates: with a public domain and ports 80/443 open, Caddy obtains a Let's Encrypt certificate automatically (that step was **not** run here). To test before DNS exists, uncomment `local_certs` in the global block (Caddy's internal CA; self-signed) and run the acceptance script with `--insecure --resolve mcp.example.com:443:127.0.0.1`. That is how this recipe was tested.
- Stop with `docker compose stop mms`; it exited in under one second and logged `Shutting down HTTP server...`.
- Updating: `git -C src pull && docker compose up -d --build`, after the backup in section 8.

### 5.4 Docker Compose + Traefik v3

Uses the same `Dockerfile`, `mms.env` and `src/` as 5.3. Tested with Traefik 3.7.17. `compose.yaml`:

```yaml
name: mms

services:
  mms:
    build:
      context: ./src
      dockerfile: ../Dockerfile
    image: mms:local
    restart: unless-stopped
    env_file: mms.env
    environment:
      MMS_HOST: 0.0.0.0
      MMS_TRUSTED_PROXIES: 172.30.0.0/24
    volumes:
      - mms-data:/data
    stop_grace_period: 15s
    networks:
      - edge
    labels:
      - traefik.enable=true
      - traefik.http.routers.mms.rule=Host(`mcp.example.com`) && (Path(`/mcp`) || Path(`/authorize`) || Path(`/register`) || Path(`/token`) || Path(`/revoke`) || Path(`/.well-known/oauth-protected-resource`) || Path(`/.well-known/oauth-protected-resource/mcp`) || Path(`/.well-known/oauth-authorization-server`))
      - traefik.http.routers.mms.entrypoints=websecure
      - traefik.http.routers.mms.tls=true
      - traefik.http.routers.mms.tls.certresolver=${CERT_RESOLVER-le}
      - traefik.http.services.mms.loadbalancer.server.port=8080

  traefik:
    image: traefik:v3
    restart: unless-stopped
    command:
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --entrypoints.web.address=:80
      - --entrypoints.web.http.redirections.entrypoint.to=websecure
      - --entrypoints.web.http.redirections.entrypoint.scheme=https
      - --entrypoints.websecure.address=:443
      - --certificatesresolvers.le.acme.email=you@example.com
      - --certificatesresolvers.le.acme.storage=/letsencrypt/acme.json
      - --certificatesresolvers.le.acme.tlschallenge=true
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - traefik-acme:/letsencrypt
    depends_on:
      - mms
    networks:
      - edge

networks:
  edge:
    ipam:
      config:
        - subnet: 172.30.0.0/24

volumes:
  mms-data:
  traefik-acme:
```

```bash
docker compose up -d --build                      # production: Let's Encrypt via the "le" resolver
CERT_RESOLVER= docker compose up -d --build       # test: no resolver, Traefik serves its default self-signed certificate
```

Notes:
- `Path()` takes one value in Traefik v3, hence the `||` chain. Requests that match no router (other paths, other hosts, `/healthz`) get Traefik's own `404` (tested).
- Real client address: Traefik v3 sets `X-Real-Ip` from the TCP peer and, with the entry point settings above (no `forwardedHeaders.trustedIPs`, the default), replaces a client-supplied value. Tested with two client containers: a client locked out by five bad logins stayed locked when it sent `X-Real-IP: 198.51.100.5`, a second client was not locked, and a fresh client claiming the first client's address was not locked. If Traefik itself sits behind another proxy or CDN, it sees that proxy's address; configure `forwardedHeaders.trustedIPs` for it (not tested).
- Streaming: no timeout or buffering options were needed. With default settings an idle `GET /mcp` event stream stayed open for 200 seconds, through Traefik's 180 second default idle timeout, and the session worked afterwards (tested).
- The Docker socket gives Traefik root-equivalent control over the host. Mounting it read-only does not remove that. If that is not acceptable, use a socket proxy or the file provider (not covered).
- The `le` resolver and `CERT_RESOLVER` interpolation: the default `le` is used when the variable is unset; an empty value (`CERT_RESOLVER=`) removes the resolver. Let's Encrypt issuance itself was **not** run here.

### 5.5 Cloudflare Tunnel (`cloudflared`) -- Unverified

**Unverified.** Nothing in this subsection was run: it needs a Cloudflare account and a domain. Treat every statement as a design, run section 6 against the result, and report what you could not check.

Design: `cloudflared` makes an outbound connection to Cloudflare; no inbound port is opened and no certificate is managed on the host. Cloudflare terminates TLS for `mcp.example.com`. Point the tunnel's public hostname `mcp.example.com` at `http://127.0.0.1:8080` (host install) or `http://mms:8080` (compose, with `cloudflared` on the same network), and run `mms` with `MMS_PUBLIC_URL=https://mcp.example.com` exactly as in 5.1 or 5.3.

Real client address, the part that needs care:
- `cloudflared` connects to `mms` from a local address, so the TCP peer is loopback (host install) or the `cloudflared` container.
- `cloudflared` does not set `X-Real-IP`. Cloudflare puts the visitor address in `Cf-Connecting-Ip` (and `X-Forwarded-For`); `mms` reads neither.
- If the peer is loopback or trusted, and nobody overwrites `X-Real-IP`, the client could choose its own rate-limit key by sending that header (assumed, since Cloudflare is not known to remove it). The global limit (50 failed logins per 15 minutes) still applies, but the per-address limit would be bypassable.
- Safer design: put a local Caddy between the tunnel and `mms` and derive `X-Real-IP` from `Cf-Connecting-Ip`; point the tunnel at `http://127.0.0.1:8081`. This Caddyfile was only syntax-checked with `caddy validate`:

```caddyfile
{
	auto_https off
}

http://:8081 {
	bind 127.0.0.1
	@mms path /mcp /authorize /register /token /revoke /.well-known/oauth-protected-resource /.well-known/oauth-protected-resource/mcp /.well-known/oauth-authorization-server

	handle @mms {
		reverse_proxy 127.0.0.1:8080 {
			header_up X-Real-IP {header.Cf-Connecting-Ip}
			flush_interval -1
		}
	}

	handle {
		respond "Not found" 404
	}
}
```

- Cloudflare closes connections idle for about 100 seconds (error 524, see [deploy.md](deploy.md#cloudflare)). Idle `GET` streams are therefore cut; clients reconnect.
- Add a Cloudflare WAF rate-limit rule on `/authorize` and `/register` if you want a limit that sees the real visitor address.

### 5.6 Static bearer token, no OAuth

For programmatic clients that can send `Authorization: Bearer <TOKEN>` on every request. Tested through nginx (5.1 configuration, separate container): `verify-deploy.sh` in bearer mode passed 13 checks.

Environment (any recipe; replace the OAuth lines):

```ini
MMS_PUBLIC_URL=https://mcp.example.com
MMS_AUTH_BEARER=<TOKEN>
```

Generate the token with `openssl rand -hex 32`. Facts:
- `MMS_AUTH_BEARER` and `OAUTH_USER`/`OAUTH_PASS` together are a startup error.
- The OAuth endpoints are **not served**: `/.well-known/oauth-authorization-server`, `/register`, `/authorize`, `/token` and `/revoke` return `404` from the server. MCP clients that rely on OAuth discovery cannot use this mode.
- There is no expiry, no revocation and no failed-attempt limiter in this mode; the token is compared in constant time. Use at least 32 random bytes and, if the endpoint is on the open internet, a rate limit at the proxy. Rotate by changing the variable and restarting.
- The `401` carries `WWW-Authenticate: Bearer` (no `resource_metadata`).
- `MMS_TRUSTED_PROXIES` has no effect here (no limiter), so the compose files do not need it.
- A client that supports custom headers, for example Claude Code: `claude mcp add --transport http memory https://mcp.example.com/mcp --header "Authorization: Bearer <TOKEN>"` (command shape not tested here; the token appears in that client's argv, so type it where that is acceptable).

### 5.7 Local-only (stdio)

`mms --memory /absolute/path/memory.db` started by the MCP client over stdio: no network, no proxy, no auth.

## 6. Acceptance tests

Run these before telling the user the deployment works. The script in 6.3 runs all of them; 6.1 and 6.2 explain each check by hand. Nothing here puts anything into the knowledge graph. Side effects: check 4 registers one throwaway client in `oauth.db`, and the authenticated flow creates tokens (revoked at the end).

Set up once. While the certificate is self-signed or DNS is not live, add `CURL_EXTRA='-k --resolve mcp.example.com:443:127.0.0.1'`:

```bash
BASE=https://mcp.example.com
C="curl -sS --max-time 20 ${CURL_EXTRA:-}"
```

### 6.1 Unauthenticated checks

**Check 1 -- 401 with the right `resource_metadata`.**

```bash
$C -D - -o /dev/null -X POST "$BASE/mcp" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' | grep -i -E '^(HTTP|www-authenticate)'
```

Expected: status `401` and a header `Www-Authenticate: Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp", scope="mcp"` (capitalisation varies with HTTP version). Failures: `403` means P1 or `MMS_PUBLIC_URL` is wrong; `200` means authentication is off; a different host or `http://` in `resource_metadata` means `MMS_PUBLIC_URL` is wrong; `502`/`504` means the upstream is unreachable.

**Check 2 -- protected resource metadata.**

```bash
$C "$BASE/.well-known/oauth-protected-resource/mcp"
```

Expected JSON containing `"resource":"https://mcp.example.com/mcp"` and `"authorization_servers":["https://mcp.example.com"]`. The `resource` value must be exactly the origin plus the MCP path.

**Check 3 -- authorization server metadata, all https.**

```bash
AS="$($C "$BASE/.well-known/oauth-authorization-server")"; echo "$AS"
echo "$AS" | grep -c 'http://'
```

Expected: `"issuer":"https://mcp.example.com"`, `authorization_endpoint`, `token_endpoint`, `registration_endpoint` and `revocation_endpoint` all starting with `https://mcp.example.com/`, and the count `0`. A non-zero count means the issuer was derived from the request (P13): set `MMS_PUBLIC_URL`.

**Check 4 -- dynamic client registration.**

```bash
REG="$($C -w '\nHTTP %{http_code}\n' -X POST "$BASE/register" -H 'Content-Type: application/json' \
  -d '{"client_name":"acceptance","redirect_uris":["http://localhost:8765/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}')"
echo "$REG"
CID="$(echo "$REG" | sed -n 's/.*"client_id":"\([^"]*\)".*/\1/p' | head -n 1)"
```

Expected: `HTTP 201` and a 32-character hexadecimal `client_id`. `404`/`405` means `/register` is not proxied; `403` is P1; `429` is the registration limit (20 per hour per client address).

**Check 5 -- unregistered redirect URI is refused, not followed.**

```bash
CHAL=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM
Q="client_id=$CID&response_type=code&code_challenge=$CHAL&code_challenge_method=S256&state=s&resource=$BASE/mcp"
$C -w '\nHTTP %{http_code}\n' "$BASE/authorize?$Q&redirect_uri=http%3A%2F%2Fevil.example%2Fcb"
```

Expected: `HTTP 400` and `{"error":"invalid_request","error_description":"redirect_uri does not match a registered redirect URI"}`. A `302` to `evil.example` would be an open redirect; stop and report it.

**Check 6 -- `/healthz` is not public.**

```bash
$C -w '\nHTTP %{http_code}\n' "$BASE/healthz"
```

Expected: anything except `200` with body `ok`. The recipes in this guide return `404`. If it returns `ok`, add the route allow-list (P12).

**Check 7 -- security headers on the login page.**

```bash
$C -D - -o /dev/null "$BASE/authorize?$Q&redirect_uri=http%3A%2F%2Flocalhost%3A8765%2Fcb" \
  | grep -i -E '^(HTTP|x-frame-options|x-content-type-options|cache-control|referrer-policy|content-security-policy)'
```

Expected: status `200`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, and a `Content-Security-Policy` starting with `default-src 'none'`. A missing header means the proxy strips or overrides response headers.

**Check 8 -- the login page renders for a registered client.**

```bash
$C "$BASE/authorize?$Q&redirect_uri=http%3A%2F%2Flocalhost%3A8765%2Fcb" | grep -c 'name="password"'
```

Expected: `1`.

**Check 9 -- a wrong `Host` is not served.**

```bash
$C -H 'Host: wrong-host.invalid' "$BASE/.well-known/oauth-protected-resource" | grep -c '"resource"'
```

Expected: `0`. Observed status codes: Caddy answers `200` with an empty body for an unknown host, nginx and Traefik `404`, `mms` itself `403`. Anything that returns the metadata JSON is a failure.

**Check 10 (optional) -- login lockout.** Locks the client address out of the login page for 15 minutes, so run it last and only when the user agrees. Five wrong passwords are accepted (HTTP `200` with an error page), the sixth attempt must return `429` with `Retry-After`:

```bash
for i in 1 2 3 4 5 6; do
  $C -o /dev/null -w "attempt $i: HTTP %{http_code}\n" -X POST "$BASE/authorize" \
    --data-urlencode "client_id=$CID" --data-urlencode redirect_uri=http://localhost:8765/cb --data-urlencode state=s \
    --data-urlencode "code_challenge=$CHAL" --data-urlencode code_challenge_method=S256 --data-urlencode response_type=code \
    --data-urlencode "resource=$BASE/mcp" --data-urlencode username=admin --data-urlencode password=wrong
done
```

Expected: attempts 1 to 5 `200`, attempt 6 `429`. To prove the per-client keying behind a proxy, repeat the six attempts from one machine and then try one attempt from a second machine (or a second container with its own address): the second must get `200`, not `429`. If it gets `429`, the proxy address is the key: fix `MMS_TRUSTED_PROXIES` and P2. Restart `mms` to clear the lockout.

### 6.2 Authenticated check (login, token, initialize, tools/list)

This is a self-contained flow: register, PKCE, log in, take the code from the response, exchange it, `initialize`, `tools/list`, revoke. The login response is an HTML page, not a redirect: the authorization code is in a script as `?code=<64 hex>`, so the flow reads it from the body. The password is read from the environment and handed to curl through a `0600` file, never through argv. Run it with `OAUTH_USER` and `OAUTH_PASS` exported (for example `read -rs OAUTH_PASS; export OAUTH_PASS`). Tested against Caddy (Git Bash on Windows) and nginx (Debian curl), expected last line `OK: authenticated, 13 tools listed`:

```bash
set -eu
BASE=https://mcp.example.com            # the public origin, no trailing slash
: "${OAUTH_USER:?set OAUTH_USER}" "${OAUTH_PASS:?set OAUTH_PASS}"
c() { curl -fsS --max-time 30 ${CURL_EXTRA:-} "$@"; }   # CURL_EXTRA="-k --resolve mcp.example.com:443:127.0.0.1" for tests

T=$(mktemp -d); trap 'rm -rf "$T"' EXIT; cd "$T"; umask 077
printf %s "$OAUTH_PASS" > pw            # the password reaches curl through a file, never through argv or echo
REDIR=http://localhost:8765/cb

# 1. dynamic client registration (leaves one throwaway client in oauth.db)
CID=$(c -X POST "$BASE/register" -H 'Content-Type: application/json' \
  -d "{\"client_name\":\"acceptance\",\"redirect_uris\":[\"$REDIR\"]}" | sed 's/.*"client_id":"\([^"]*\)".*/\1/')

# 2. PKCE S256
VER=$(openssl rand -hex 32)
CHAL=$(printf %s "$VER" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d =)

# 3. login: the response is an HTML page whose script contains the redirect URL with ?code=<64 hex>
CODE=$(c -X POST "$BASE/authorize" \
  --data-urlencode "client_id=$CID" --data-urlencode "redirect_uri=$REDIR" --data-urlencode state=acc \
  --data-urlencode "code_challenge=$CHAL" --data-urlencode code_challenge_method=S256 \
  --data-urlencode response_type=code --data-urlencode "resource=$BASE/mcp" \
  --data-urlencode "username=$OAUTH_USER" --data-urlencode password@pw \
  | sed -n 's/.*[?&]code=\([0-9a-f]\{64\}\).*/\1/p' | head -n 1)
[ -n "$CODE" ] || { echo "FAIL: login returned no authorization code (wrong credentials, or the limiter answered 429)"; exit 1; }

# 4. token
TOK=$(c -X POST "$BASE/token" --data-urlencode grant_type=authorization_code --data-urlencode "code=$CODE" \
  --data-urlencode "code_verifier=$VER" --data-urlencode "client_id=$CID" --data-urlencode "resource=$BASE/mcp" \
  | sed 's/.*"access_token":"\([^"]*\)".*/\1/')

# 5. initialize, then tools/list on the same session
H=(-H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream')
c -D hdrs -o /dev/null -X POST "$BASE/mcp" "${H[@]}" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"acceptance","version":"1"}}}'
SID=$(awk -F': ' 'tolower($1)=="mcp-session-id"{sub(/\r$/,"",$2);print $2}' hdrs)
c -o /dev/null -X POST "$BASE/mcp" "${H[@]}" -H "Mcp-Session-Id: $SID" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
N=$(c -X POST "$BASE/mcp" "${H[@]}" -H "Mcp-Session-Id: $SID" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | grep -o '"name":"[a-z_]*"' | sort -u | wc -l)
c -o /dev/null -X POST "$BASE/revoke" --data-urlencode "token=$TOK" --data-urlencode "client_id=$CID"
[ "$N" -eq 13 ] && echo "OK: authenticated, 13 tools listed" || { echo "FAIL: tools/list returned $N tools, expected 13"; exit 1; }
```

Failure reading: `curl: (22) ... 401` at `initialize` means `Authorization` did not reach the server (P3); `403` means P1; a missing `Mcp-Session-Id` (empty `SID`) means the header was stripped (P9) or `--http-stateless` is set; `tools/list` hanging until `--max-time` means response buffering (P4); `login returned no authorization code` is a wrong credential, a lockout (`429`, C10) or a `/authorize` route problem.

### 6.3 `scripts/verify-deploy.sh`

`scripts/verify-deploy.sh` runs checks 1 to 9, the authenticated flow above, and optionally check 10. It exits non-zero if any check fails and prints a hint per failure.

```bash
# OAuth deployment: load the env file into the environment without printing it
set -a; . /etc/mms/mms.env; set +a
scripts/verify-deploy.sh https://mcp.example.com
# self-signed certificate, domain not in DNS yet
OAUTH_USER=admin OAUTH_PASS=... scripts/verify-deploy.sh --insecure --resolve mcp.example.com:443:127.0.0.1 https://mcp.example.com
# add the lockout check (locks this address out for 15 minutes)
... scripts/verify-deploy.sh --rate-limit https://mcp.example.com
# static bearer token deployment
MMS_AUTH_BEARER=... scripts/verify-deploy.sh https://mcp.example.com
# custom --http-endpoint
... scripts/verify-deploy.sh --mcp-path /api/mcp https://mcp.example.com
```

Needs `bash`, `curl`, `openssl`, `sed`, `grep`, `awk`. **Side effect:** in OAuth mode it registers one throwaway client named `verify-deploy` in `oauth.db`, submits one wrong password (counted by the limiter until the next successful login from the same address) and revokes the tokens it obtained. If neither `OAUTH_PASS` nor `MMS_AUTH_BEARER` is set, it runs only the unauthenticated checks and prints `SKIP` for the rest; report that to the user.

### 6.4 What was verified with it

See [Verification status](#verification-status): 40 checks passed against Caddy and against Traefik, 40 against nginx (run from a Linux container), 13 in bearer mode, and 41 with `--rate-limit` on Caddy.

## 7. Failure symptoms

| Symptom | Cause | Fix |
|---------|-------|-----|
| `403`, body `Forbidden: invalid Host header` on `/mcp` or an OAuth route | The proxy rewrote `Host`, the client uses another name, or `MMS_PUBLIC_URL` differs from the name in use (tested) | Preserve `Host` (P1); make `MMS_PUBLIC_URL` the exact public origin |
| `403`, body `Forbidden: invalid Host header "<host>"` (the host is quoted) | `MMS_PUBLIC_URL` is not set, no auth is configured and the listener is on loopback: the SDK's DNS-rebinding protection rejects the proxied `Host` (tested) | Set `MMS_PUBLIC_URL` (or require auth) |
| Client loops: `401`, login, `401` again; or "cannot discover OAuth" | `resource_metadata` in the challenge points at the wrong origin (`MMS_PUBLIC_URL` mismatch), or the proxy drops `Authorization` or `WWW-Authenticate` | Check 1 and check 2; fix `MMS_PUBLIC_URL`, P3, P9 |
| `http://` URLs in the metadata | No `MMS_PUBLIC_URL`; the issuer is derived from the request and the proxy did not send `X-Forwarded-Proto: https` (tested) | Set `MMS_PUBLIC_URL` |
| `502 Bad Gateway` | `mms` is down; wrong upstream port; in a container `MMS_HOST` is still `127.0.0.1`; wrong network name | `systemctl status mms` or `docker compose logs mms`; for containers set `MMS_HOST=0.0.0.0` and check both services share a network |
| `504` or dropped connection on `GET /mcp` after about a minute | Proxy read timeout shorter than the idle stream (nginx default 60 s); or a CDN idle limit (Cloudflare about 100 s) | P5; raise `proxy_read_timeout`; CDN limits cannot be raised, clients reconnect |
| Streaming responses arrive late, in bursts, or never | Proxy buffers responses | P4: `proxy_buffering off`, `flush_interval -1`, no Buffering middleware |
| Everyone gets "Too many failed attempts" (login page `429`) after a few wrong passwords | All users share the proxy's address because it is not in `MMS_TRUSTED_PROXIES`, or the proxy does not send `X-Real-IP` (reproduced: client A's six failures locked client B) | Set `MMS_TRUSTED_PROXIES` to the proxy address or subnet; P2; restart `mms` to clear |
| Redirect with `error=invalid_target` | The client's `resource` is not `<MMS_PUBLIC_URL>/mcp` (or the custom endpoint) (tested) | Make the client use the public URL; fix `MMS_PUBLIC_URL` |
| `400` with `redirect_uri does not match a registered redirect URI` | The `redirect_uri` differs from the registered one, or the client registration was lost | Re-register; keep `oauth.db` on the persistent directory |
| `404` on a request with an old `Mcp-Session-Id` after a restart | Normal (tested): sessions are in memory | Client sends `initialize` again; tokens remain valid |
| `database is locked` in the logs or tool errors | A second process (replica, old container during a rolling update, another host through NFS) writes the same database | Run one instance (C1) on a local filesystem; wait out the old container before starting the new one |
| `oauth store schema version N is newer than the supported version M; upgrade mms or use another --oauth-store path` at startup | A newer `mms` upgraded `oauth.db` and an older binary was started afterwards | Run the newer binary again, or restore the pre-upgrade `oauth.db` backup, or delete `oauth.db` (clients re-login) |
| `Invalid configuration: invalid trusted proxies (...)` at startup | Malformed `MMS_TRUSTED_PROXIES` (name instead of IP, bad prefix length, empty element) (tested) | Use comma-separated CIDRs or IPs only |
| `413` from the proxy on large tool calls | Proxy body limit | P6 |
| Server cannot write its databases in a container | The data directory is not owned by uid 65532 (bind mount or pre-existing volume) | `chown 65532:65532` the host directory. Not reproduced here; the exact SQLite message was not captured |
| `mms --help` does not list `-trusted-proxies` | Binary or source predates the feature; `MMS_TRUSTED_PROXIES` is silently ignored | Use newer source or a newer release |

## 8. Security checklist, upgrade and rollback

Checklist (every item should be true before you report success):
- [ ] `https` only; TLS terminated at the proxy; `MMS_PUBLIC_URL` is the exact `https://` origin.
- [ ] The only published ports are the proxy's 80/443 (plus SSH). The `mms` port is not published (Docker) or listens on loopback or a private interface (systemd).
- [ ] The proxy exposes only the routes in 4.1; `/healthz` returns non-`200` publicly (check 6).
- [ ] Secrets only in a root-owned `0600` env file or a secret store; none in argv, in git, in the compose file, or in logs and chat.
- [ ] Password from `openssl rand -hex 18` (36 hex characters) or longer; bearer token from `openssl rand -hex 32`.
- [ ] The service runs as a non-root user with `UMask=0077` (systemd) or uid 65532 (Docker).
- [ ] `MMS_TRUSTED_PROXIES` lists only addresses that are really your proxies, and nothing else is attached to that network.
- [ ] The proxy overwrites `X-Real-IP` (P2).
- [ ] The data directory is on persistent local storage and is backed up (below).
- [ ] `scripts/verify-deploy.sh` passed with no failed check.

**Upgrade.**
1. Back up (below).
2. Install the new binary or rebuild the image (`git -C src pull && docker compose up -d --build`, or replace `/usr/local/bin/mms` and `systemctl restart mms`).
3. Run section 6.

The `oauth.db` migration to a newer schema runs on first start and is **one-way**: an older binary refuses the upgraded file (section 7). Memory data (`memory.db`) is not rewritten by the OAuth migration.

**Rollback.** Stop the service, put the old binary or image back, and restore the `oauth.db` backup taken before the upgrade (or delete `oauth.db`; clients then log in again). Restore `memory.db` from backup only if the new version changed it. Start, then run section 6.

**Backup (SQLite online backup, safe while running).** Do not copy live `*.db` files with `cp`. With the `sqlite3` CLI:

```bash
# systemd host
sudo -u mms sqlite3 /var/lib/mms/memory.db ".backup '/var/lib/mms/memory.backup.db'"
sudo -u mms sqlite3 /var/lib/mms/oauth.db  ".backup '/var/lib/mms/oauth.backup.db'"
```

A distroless container has no `sqlite3`. Stop the service and copy all database files from the volume, including the `-wal` and `-shm` files if present:

```bash
docker compose stop mms
docker run --rm -v mms_mms-data:/data:ro -v "$PWD/backup:/backup" busybox:1.37 sh -c 'cp /data/memory.db* /data/oauth.db* /backup/'
docker compose start mms
```

(The named volume is `<compose project name>_mms-data`, here `mms_mms-data`. The backup commands above were not run; the volume name and file names were checked with `ls` on the volume.) `oauth.db` is optional to back up: without it clients log in again. Keep the env file too. Delete temporary backup copies from the server after moving them off.

## 9. Rules for AI agents

1. Read sections 1 to 4 before choosing a recipe. If the situation is a fresh Debian/Ubuntu/RHEL VPS, propose `scripts/deploy-vps.sh` first.
2. Never put a password or token in argv, in a command line you print, in a file that is under version control, or in your reply. Write secrets to a `0600` file through a heredoc (as in 5.1) and tell the user where it is. Do not run `cat` on it.
3. Generate secrets with `openssl rand -hex 18` (OAuth password) and `openssl rand -hex 32` (bearer token). Do not invent passwords and do not reuse one the user already uses elsewhere.
4. `MMS_PUBLIC_URL` is `scheme://host[:port]` only. Never add a path or a trailing path segment. One domain per instance.
5. Never run more than one `mms` process or container on a data directory. Do not set `replicas`, do not scale the service, do not use a network filesystem.
6. Always set `MMS_TRUSTED_PROXIES` when the proxy is in another container or host, and verify the flag exists in the binary or source you deploy (`mms --help`). Never set it to a range that contains clients (`0.0.0.0/0` is wrong).
7. Do not publish the `mms` port, and do not proxy `/healthz`.
8. Ask the user before changing their existing proxy's global configuration (`nginx.conf`, other server blocks, `default_server`, Traefik static configuration, a shared Caddyfile beyond appending one site block). Add files, do not rewrite theirs. Ask before opening firewall ports, before stopping a service that holds ports 80/443, and before running the lockout check (check 10).
9. Run `scripts/verify-deploy.sh` (or section 6) after every deployment and after every upgrade. Do not say the deployment works until it passes. Take the backup before an upgrade.
10. Prefer tested recipes. For anything marked **Unverified** or **not tested**, say so in your report instead of presenting it as working.
11. Report at the end: what you deployed and where, the public URL, where the secrets are stored (path only), the results of every acceptance check, and everything you skipped or could not verify.

## Verification status

What was actually run while writing this guide (Docker Desktop 28.4.0, Compose 2.39.4, Windows host, Linux containers; Caddy 2.11.7, Traefik 3.7.17, nginx 1.30.6, `golang:1.25`, `gcr.io/distroless/static-debian12:nonroot`):

| What | Result |
|------|--------|
| Dockerfile (5.3) builds from the working tree, `CGO_ENABLED=0`, distroless | pass |
| Compose + Caddy (5.3) with `local_certs`, `mcp.example.com` via `--resolve`: `verify-deploy.sh` | pass (40 passed; 41 with `--rate-limit`) |
| Compose + Traefik (5.4) with its default certificate: `verify-deploy.sh` | pass (40 passed) |
| nginx container (deploy.md block, upstream `mms:8080`) in front of a separate `mms` container: `verify-deploy.sh` from a Linux container | pass (40 passed) |
| Trusted proxies behind nginx: 6 failed logins from client A locked A; client B (other address) stayed allowed; A spoofing `X-Real-IP` stayed locked; C spoofing A's address was not locked; direct requests to `mms` from an untrusted peer with six different `X-Real-IP` values were still limited as one client | pass |
| Same without `MMS_TRUSTED_PROXIES`: A's failures locked B | reproduced (the problem this setting fixes) |
| Same A/B/spoof test behind Caddy and behind Traefik | pass |
| Bearer mode (5.6) through nginx: `verify-deploy.sh` | pass (13 passed) |
| Idle `GET /mcp` SSE stream held 85 s through Caddy, 85 s and 200 s through Traefik, 85 s through nginx; session usable afterwards | pass |
| Restart: old session id `404`, old token accepted for a new `initialize` | pass |
| HTTP/2 to the client through nginx and Caddy | pass; HTTP/3 not tested |
| Healthcheck with `alpine:3.22` + `wget` | pass (`healthy`) |
| `caddy validate` for the systemd Caddyfile (5.2), the production form of the compose Caddyfile, and the cloudflared helper Caddyfile | valid |
| `systemd-analyze verify` for the unit (5.1) | no errors (a warning about the executable bit came from the test mount) |
| Let's Encrypt issuance (Caddy, Traefik, certbot), real DNS, firewall rules, bind-mount ownership, backups, cloudflared | **not run** |
| Cloudflare Tunnel (5.5) | **Unverified** |

`scripts/verify-deploy.sh` passes `bash -n` and shellcheck (`koalaman/shellcheck:stable`).
