# EutherOxide Integration

EutherPunk should run as an internal service on the EutherOxide host:

```text
127.0.0.1:8787
```

Ollama should stay internal:

```text
127.0.0.1:11434
```

EutherOxide should be the authenticated public/LAN gateway for:

```text
GET  /eutherpunk
GET  /api/eutherpunk/status
GET  /api/eutherpunk/models
GET  /api/eutherpunk/users
POST /api/eutherpunk/chat
POST /api/eutherpunk/chat/stream
GET  /downloads/eutherpunk-cli/{platform}
```

## Deploy EutherPunk

Build local Linux artifacts:

```bash
scripts/build.sh
```

Deploy to the LAN server after the SSH key is unlocked:

```bash
scripts/deploy-server.sh
```

The script installs:

```text
/home/nichlas/EutherPunk/bin/eutherpunkd
/home/nichlas/EutherPunk/dist/cli/eutherpunk-linux-amd64
/home/nichlas/.config/eutherpunk/config.toml
/home/nichlas/.config/systemd/user/eutherpunkd.service
```

## Separate Caddy Front

The safer first deployment is to keep EutherPunk separate from the EutherHost request loop and let Caddy proxy selected routes directly to `eutherpunkd`.

LAN routes currently intended for Caddy:

```text
http://192.168.32.186:8080/eutherpunk
http://192.168.32.186:8080/api/eutherpunk/status
http://192.168.32.186:8080/api/eutherpunk/models
http://192.168.32.186:8080/api/eutherpunk/users
http://192.168.32.186:8080/api/eutherpunk/chat
http://192.168.32.186:8080/api/eutherpunk/chat/stream
http://192.168.32.186:8080/api/eutherpunk/tts
http://192.168.32.186:8080/downloads/eutherpunk-cli/linux-amd64
```

Caddy handles to add before the general EutherHost fallback in the LAN `:8080` block:

```caddy
handle /eutherpunk* {
	reverse_proxy 127.0.0.1:8787
}

handle /api/eutherpunk* {
	reverse_proxy 127.0.0.1:8787
}

handle /web/* {
	reverse_proxy 127.0.0.1:8787
}

handle /downloads/eutherpunk-cli* {
	reverse_proxy 127.0.0.1:8787
}
```

This avoids the EutherHost active request guard. A previous EutherHost in-process proxy attempt caused `server busy` on mobile login because closed Caddy upstream sockets accumulated in EutherHost.

## EutherOxide Proxy Later

The server-side EutherOxide checkout is the source of truth. Add proxy handlers in root `src/main.rs`, not under a Tauri path.

Proxy target:

```text
http://127.0.0.1:8787
```

Keep EutherOxide responsible for:

- auth
- LAN/WAN routing
- public host config
- download links
- user identity

Keep EutherPunk responsible for:

- model gateway
- TOML config
- per-user model/safety policy
- chat streaming
- thin web client assets

Do not reintroduce this proxy path until EutherHost request lifecycle handling has been hardened and tested with mobile login/browser connection churn.

## Verification

On the server:

```bash
systemctl --user status eutherpunkd.service
curl -fsS http://127.0.0.1:8787/api/eutherpunk/status
curl -fsS http://127.0.0.1:8787/eutherpunk
```

Through EutherOxide on LAN:

```bash
curl -fsS http://192.168.32.186:8080/api/eutherpunk/status
curl -fsS http://192.168.32.186:8080/eutherpunk
```

Through public routing:

```bash
curl -fsS https://apothictech.se/api/eutherpunk/status
curl -fsS https://apothictech.se/eutherpunk
```
