# Freebuff2API

[English](README.md) | [简体中文](README_zh.md)

Freebuff2API is an OpenAI-compatible proxy server for [Freebuff](https://freebuff.com). It translates standard OpenAI API requests into Freebuff's backend format, allowing you to use Freebuff's free models with any OpenAI-compatible client, SDK, or CLI tool.

## Features

- **OpenAI Compatible API** — Standard OpenAI endpoints; works with any compatible client out of the box.
- **Stealth Request Handling** — Dynamic, randomized client fingerprints that mimic official Freebuff SDK behavior.
- **Multi-Token Rotation** — Cycle through multiple auth tokens with automatic periodic rotation.
- **HTTP Proxy Support** — Route all outbound traffic through a configurable upstream proxy.
- **Admin WebUI** — Built-in `/admin` console to manage tokens, inspect health, and watch traffic in real time.

## Getting Auth Tokens

Freebuff2API requires one or more Freebuff **auth tokens**. There are two ways to obtain one:

### Method 1 — Web (Recommended)

Visit **[https://freebuff.llm.pm](https://freebuff.llm.pm)**, log in with your Freebuff account, and your auth token will be displayed directly on the page. Copy it as your **AUTH_TOKENS** — no local installation required.

### Method 2 — Freebuff CLI

Install the Freebuff CLI:

```bash
npm i -g freebuff
```

Run `freebuff` in your terminal — on first launch it will guide you through login.

After logging in, your token is saved to a local credentials file:

| OS | Credentials Path |
|---|---|
| Windows | `C:\Users\<username>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

The file looks like:

```json
{
  "default": {
    "id": "user_10293847",
    "name": "Zhang San",
    "email": "zhangsan@example.com",
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7",
    ...
  }
}
```

Only the `authToken` value is needed — copy it as your **AUTH_TOKENS**.

> **Tip:** Log in with multiple accounts and configure all their tokens for higher throughput.

## Configuration

Configuration is managed via a JSON file and/or environment variables. The JSON keys and environment variable names are identical. By default the app looks for `config.json` in the working directory; use `-config` to specify another path.

```json
{
  "LISTEN_ADDR": ":8080",
  "UPSTREAM_BASE_URL": "https://codebuff.com",
  "AUTH_TOKENS": ["eyJhb..."],
  "ROTATION_INTERVAL": "6h",
  "REQUEST_TIMEOUT": "15m",
  "API_KEYS": [],
  "HTTP_PROXY": "",
  "DATA_DIR": "",
  "ADMIN_PASSWORD": ""
}
```

### Reference

| Key / Env Var | Description |
|---|---|
| `LISTEN_ADDR` | Proxy listen address (default `:8080`; falls back to the platform-provided `PORT` when unset) |
| `UPSTREAM_BASE_URL` | Freebuff backend URL (default `https://codebuff.com`) |
| `AUTH_TOKENS` | Auth tokens used as the initial seed; once the admin UI is enabled, the on-disk store takes precedence |
| `ROTATION_INTERVAL` | Run rotation interval (default `6h`) |
| `REQUEST_TIMEOUT` | Upstream request timeout (default `15m`) |
| `API_KEYS` | Client API keys for proxy auth (empty = open access; does not affect `/admin`) |
| `HTTP_PROXY` | HTTP proxy for outbound requests |
| `DATA_DIR` | Directory where tokens and metrics are persisted (container default `/data`) |
| `ADMIN_PASSWORD` | Admin UI password; leaving it blank disables the `/admin` routes |

Environment variables override JSON values when both are set.

## Admin WebUI

Set `ADMIN_PASSWORD` and visit `http://<host>/admin` to manage the service:

- **Overview** — Cumulative requests, last 1m / 5m / 1h / 24h traffic, average latency, and token health.
- **Tokens** — Create / rename / enable / disable / delete tokens online; each row shows session status, cooldown, and the latest error.
- **Traffic** — Time-series chart over the last 1h / 6h / 24h and per-token ranking.
- **Settings** — Read-only view of the runtime configuration and the exposed API endpoints.

Managed tokens and cumulative traffic counters are stored in `${DATA_DIR}/tokens.json` and `${DATA_DIR}/metrics.json` so they survive restarts. The admin session uses a signed cookie (`SameSite=Lax`) and is invalidated when the server restarts.

## Deployment

### Docker

Pre-built multi-arch images are available on GHCR:

```bash
docker run -d --name Freebuff2API \
  -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  -e ADMIN_PASSWORD="change-me" \
  -v freebuff-data:/data \
  ghcr.io/quorinex/freebuff2api:latest
```

Build from source:

```bash
docker build -t Freebuff2API .
docker run -d -p 8080:8080 \
  -e AUTH_TOKENS="token1,token2" \
  -e ADMIN_PASSWORD="change-me" \
  -v freebuff-data:/data \
  Freebuff2API
```

The image sets `DATA_DIR=/data` by default, so mounting a volume there preserves tokens and traffic stats across restarts.

### Zeabur

1. Create a service from this repository and pick **Dockerfile** as the build method.
2. Set environment variables:
   - `AUTH_TOKENS` (optional seed; can be left blank and managed entirely through the WebUI)
   - `ADMIN_PASSWORD` (enables the admin WebUI)
   - `API_KEYS` if you want to authenticate external API callers
3. **Do not** set `LISTEN_ADDR`; the service automatically binds to the `PORT` Zeabur injects.
4. Attach a persistent volume mounted at `/data` so tokens and metrics survive restarts.
5. Use `/healthz` for the health-check path. After deployment, open `https://<your-domain>/admin` to log in.

### Build from Source

**Requirements:** Go 1.23+

```bash
git clone https://github.com/Quorinex/Freebuff2API.git
cd Freebuff2API
go build -o Freebuff2API .
./Freebuff2API -config config.json
```

## Links

- [linux.do](https://linux.do)

## Disclaimer

This project has no official affiliation with OpenAI, Codebuff, or Freebuff. All related trademarks and copyrights belong to their respective owners.

All contents within this repository are provided solely for communication, experimentation, and learning, and do not constitute production-ready services or professional advice. This project is provided on an "As-Is" basis, and users must use it at their own risk. The author assumes no liability for any direct or indirect damages resulting from the use, modification, or distribution of this project, nor provides any warranties of any kind, express or implied.

## License

MIT
