# VPS Deployment Guide

`scripts/deploy-vps.sh` takes a fresh Linux VPS and a domain to a working HTTPS MCP server with OAuth 2.1: the `mms` binary under systemd, nginx as the TLS-terminating reverse proxy, and a Let's Encrypt certificate that renews itself.

## Quick Start

```bash
curl -fsSL https://raw.githubusercontent.com/okooo5km/memory-mcp-server-go/main/scripts/deploy-vps.sh \
  | sudo bash -s -- --domain mcp.example.com --email me@example.com
```

When it finishes it prints the MCP URL (`https://mcp.example.com/mcp`), the username and a generated password. The password is shown once; it stays in `/etc/mms/mms.env`.

Then add the server:

- Claude (web/desktop): Settings, Connectors, Add custom connector, enter `https://mcp.example.com/mcp`, and log in with the printed credentials.
- Claude Code: `claude mcp add --transport http memory https://mcp.example.com/mcp`

To choose your own password without putting it on the command line, pass it through the environment:

```bash
curl -fsSL https://raw.githubusercontent.com/okooo5km/memory-mcp-server-go/main/scripts/deploy-vps.sh \
  | sudo MMS_OAUTH_PASS='a-long-password' bash -s -- --domain mcp.example.com
```

## Prerequisites

- A Linux server (amd64 or arm64) with systemd and root access. Debian/Ubuntu (apt) and the RHEL family (dnf/yum; EPEL is enabled for certbot if needed) are supported.
- A DNS A record (and AAAA, if the server has IPv6) for the domain pointing at the server, already propagated.
- Ports 80 and 443 reachable from the internet and not used by another web server. The script stops if a non-nginx process holds them.

The script checks DNS and ports first. A DNS mismatch only produces a warning (it is expected behind a CDN proxy); a port conflict stops the run.

## What the Script Does

1. Preflight: DNS vs. this machine's public IP, ports 80/443 and the local port.
2. Installs nginx, certbot (unless `--cert` is used), curl and openssl.
3. Downloads `mms_<version>_linux_<arch>.tar.gz` from GitHub Releases, verifies it against the release's `checksums.txt` (SHA-256), and installs `/usr/local/bin/mms` atomically. With `--binary` it installs your local file instead.
4. Creates the `mms` system user (no login shell), `/var/lib/mms` (0700) and `/etc/mms/mms.env` (0600, root).
5. Installs and starts `mms.service` (hardened: `NoNewPrivileges`, `ProtectSystem=strict`, `PrivateTmp`, `UMask=0077` and more) and waits for `http://127.0.0.1:PORT/healthz`.
6. Configures nginx in two phases: a port-80-only config that answers the ACME challenge, then `certbot certonly --webroot`, then the final config. certbot never edits the nginx config.
7. Installs a certbot deploy hook that reloads nginx and enables the certbot renewal timer.
8. Opens ports 80/443 if ufw or firewalld is active. On SELinux-enforcing systems it enables `httpd_can_network_connect` so nginx can reach the backend.
9. Verifies `https://DOMAIN/.well-known/oauth-protected-resource/mcp` and that an unauthenticated `POST /mcp` returns `401`.

Exposed through nginx: `/mcp` and the OAuth paths (`/.well-known/oauth-protected-resource`, `/.well-known/oauth-protected-resource/mcp`, `/.well-known/oauth-authorization-server`, `/authorize`, `/token`, `/register`, `/revoke`). Everything else, including `/healthz`, returns 404. `/authorize` and `/register` are limited to about 10 requests/minute per IP, `/token` and `/revoke` to 30 requests/minute (with a burst allowance, then `429`).

Files written:

| Path | Purpose |
|------|---------|
| `/usr/local/bin/mms` | Server binary |
| `/etc/mms/mms.env` | Runtime configuration (secrets, root-only) |
| `/var/lib/mms/` | Data: `memory.db`, `oauth.db` |
| `/etc/systemd/system/mms.service` | systemd unit |
| `/etc/nginx/conf.d/mms-<domain>.conf` | nginx site and rate-limit zones |
| `/var/www/mms-acme/` | ACME webroot |
| `/etc/letsencrypt/renewal-hooks/deploy/mms-nginx.sh` | Reloads nginx after renewal |

## Runtime Configuration

The whole configuration is `/etc/mms/mms.env` (systemd reads it as root; the service runs as `mms`):

```ini
MMS_PUBLIC_URL=https://mcp.example.com
MMS_PORT=8080
MEMORY_FILE_PATH=/var/lib/mms/memory.db
OAUTH_USER=admin
OAUTH_PASS=<generated>
```

`MMS_PUBLIC_URL` selects the HTTP transport bound to `127.0.0.1`, is used as the OAuth issuer / resource URL, and places the OAuth store at `oauth.db` next to the memory database. The unit's `ExecStart` is just `/usr/local/bin/mms`.

## Flags

| Flag | Description |
|------|-------------|
| `--domain NAME` | Public hostname (required, validated as a hostname) |
| `--email ADDR` | Let's Encrypt contact email; without it certbot registers without one |
| `--user NAME` | OAuth username. Default `admin`; an existing value is kept on re-run |
| `--password PASS` | OAuth password (12+ chars, no whitespace and none of `" ' \ $ #` or backtick). Prefer the `MMS_OAUTH_PASS` env var, since argv is visible in the process list. Default: 36 random hex chars on first install |
| `--port N` | Local port on 127.0.0.1 (1024-65535). Default `8080`; an existing value is kept on re-run |
| `--version TAG` | Release to install, e.g. `v0.2.3`. Default: latest |
| `--binary PATH` | Install a local binary instead of downloading |
| `--cert PATH`, `--key PATH` | Use an existing certificate and key (PEM, absolute paths) and skip certbot |
| `--staging` | Use the Let's Encrypt staging CA (untrusted certificates; for testing the flow) |
| `-h`, `--help` | Show help |

## Upgrade

Re-run the same command. The script is idempotent: it replaces the binary, unit and nginx config, and restarts the service. An existing password, username, port and all data are kept unless you pass the corresponding flag (`--password` or `MMS_OAUTH_PASS` replaces the password). To pin a version use `--version vX.Y.Z`.

If a Let's Encrypt certificate for the domain already exists it is reused as-is. To switch from `--staging` to a real certificate, run `certbot delete --cert-name mcp.example.com` and re-run the script.

## Change the Password

```bash
sudo nano /etc/mms/mms.env      # edit OAUTH_PASS
sudo systemctl restart mms
```

Already issued tokens stay valid until they expire or are revoked; the password is only checked at login. To force every client to log in again, stop the service, delete `/var/lib/mms/oauth.db` (and any `oauth.db-wal` / `oauth.db-shm`), and start it again. Clients then re-register and re-authenticate.

## Backup

The data is two SQLite databases in WAL mode. Use SQLite's online backup rather than copying live files:

```bash
sudo -u mms sqlite3 /var/lib/mms/memory.db ".backup '/var/lib/mms/memory.backup.db'"
sudo -u mms sqlite3 /var/lib/mms/oauth.db  ".backup '/var/lib/mms/oauth.backup.db'"
```

Copy the `.backup.db` files off the server and delete them afterwards. The script does not install the `sqlite3` CLI. Alternatively, stop the service (`systemctl stop mms`) and copy `memory.db` and `oauth.db`. Backing up `oauth.db` is optional; losing it only forces clients to log in again. Also keep `/etc/mms/mms.env`.

## Cloudflare

- Using an origin certificate: create a Cloudflare Origin CA certificate, save it on the server, set the SSL mode to Full (strict), and run the script with it:

  ```bash
  ... --domain mcp.example.com --cert /etc/ssl/cf-origin.pem --key /etc/ssl/cf-origin.key
  ```

  certbot is skipped. The script's own post-install HTTPS check will warn that the certificate is not trusted by the server itself; that is expected for an origin certificate.
- With a proxied (orange-cloud) record the DNS preflight warns, because the domain resolves to Cloudflare rather than to your server.
- Streaming (SSE) responses work through the proxy, but Cloudflare closes connections that are idle for about 100 seconds (error 524). Keep long-lived streams active, or use a DNS-only (grey-cloud) record.

## Manual nginx Configuration

For an existing nginx setup, put this in `/etc/nginx/conf.d/` (http context, because of `limit_req_zone`). Replace `mcp.example.com`, the certificate paths and `8080` as needed. `http2 on;` is for nginx 1.25.1 or newer; on older versions remove that line and add `http2` to the two `listen ... ssl` lines. Remove the `[::]` listen lines if the host has no IPv6. Obtain the certificate first, for example with `certbot certonly --webroot -w /var/www/mms-acme -d mcp.example.com` (the webroot must be served by the port-80 server block).

```nginx
# /etc/nginx/conf.d/mms-mcp.example.com.conf
limit_req_zone $binary_remote_addr zone=mms_auth_mcp_example_com:1m rate=10r/m;
limit_req_zone $binary_remote_addr zone=mms_token_mcp_example_com:1m rate=30r/m;

server {
    listen 80;
    listen [::]:80;
    server_name mcp.example.com;

    location /.well-known/acme-challenge/ {
        root /var/www/mms-acme;
    }

    location / {
        return 301 https://mcp.example.com$request_uri;
    }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name mcp.example.com;
    server_tokens off;

    ssl_certificate     /etc/letsencrypt/live/mcp.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mcp.example.com/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:MMS_SSL_mcp_example_com:10m;
    ssl_session_timeout 1d;
    ssl_session_tickets off;

    add_header Strict-Transport-Security "max-age=31536000" always;

    # MCP endpoint (streamable HTTP / SSE): no buffering, long timeouts.
    location /mcp {
        client_max_body_size 8m;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_buffering off;
        proxy_cache off;
        proxy_request_buffering off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }

    # OAuth discovery (no rate limit)
    location = /.well-known/oauth-protected-resource {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
    location = /.well-known/oauth-protected-resource/mcp {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
    location = /.well-known/oauth-authorization-server {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }

    # OAuth endpoints (rate limited)
    location = /authorize {
        limit_req zone=mms_auth_mcp_example_com burst=20 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
    location = /register {
        limit_req zone=mms_auth_mcp_example_com burst=20 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
    location = /token {
        limit_req zone=mms_token_mcp_example_com burst=30 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
    location = /revoke {
        limit_req zone=mms_token_mcp_example_com burst=30 nodelay;
        limit_req_status 429;
        client_max_body_size 64k;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }

    # Everything else (including /healthz) is not exposed.
    location / {
        return 404;
    }
}
```

The server must run with `MMS_PUBLIC_URL=https://mcp.example.com` (matching the public URL exactly) and `MMS_PORT` equal to the proxy port.

## Docker

The script installs a native binary and does not use Docker. It must run as root; there is no rootless mode.

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| Script stops with "Port 80/443 ... already in use" | Another web server holds the port. Stop it, or put mms behind it using the manual config above. |
| Preflight warns that DNS does not match | The record is missing, not propagated, or proxied by a CDN. Fix DNS before certificate issuance (`dig +short mcp.example.com`). |
| certbot fails | Check DNS, that port 80 is reachable from outside (cloud firewall / security group, not only ufw), and `/var/log/letsencrypt/letsencrypt.log`. Let's Encrypt rate limits apply; use `--staging` to experiment. |
| `403` mentioning the Host | The server rejected the `Host` header. It must be the public domain; keep `proxy_set_header Host $host` and do not rewrite it in a front proxy. |
| Client loops on `401` / login never completes | `MMS_PUBLIC_URL` does not match the URL the client uses (scheme, host or port). Check `/etc/mms/mms.env`, then compare the output of `curl https://mcp.example.com/.well-known/oauth-protected-resource/mcp`. |
| `502 Bad Gateway` | mms is not running or listens on a different port. `systemctl status mms`, `journalctl -u mms -n 50`, `curl http://127.0.0.1:8080/healthz`. On SELinux systems check `getsebool httpd_can_network_connect`. |
| `429 Too Many Requests` | Rate limit on the OAuth endpoints; wait a minute. |
| Service fails to start | `journalctl -u mms -n 50`. Confirm `/var/lib/mms` is owned by `mms` and `/etc/mms/mms.env` is well-formed. |
| `nginx -t` fails | Read its message. Very long domain names may need `server_names_hash_bucket_size`; the script sets it for names over 40 characters. |

Useful commands:

```bash
journalctl -u mms -f                 # server logs
systemctl status mms nginx
sudo nginx -t && sudo systemctl reload nginx
sudo certbot renew --dry-run
```

## Verification Status

The script passes `bash -n` and `shellcheck`, and the nginx configuration it generates was checked with `nginx -t` for both HTTP/2 forms. A full end-to-end run on a real server (including the server-side `MMS_PUBLIC_URL` and `/healthz` support) is not covered by those checks.

See [oauth.md](oauth.md) for the OAuth flow, token lifetimes and server flags.
