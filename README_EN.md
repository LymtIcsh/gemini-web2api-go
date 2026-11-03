# gemini-web2api-go

**Connect the Gemini web app to OpenAI-compatible clients, with browser accounts, quota tracking, and a built-in admin dashboard.**

[![Go](https://img.shields.io/badge/Go-1.26.8-00ADD8?logo=go)](go.mod)
[![Node.js](https://img.shields.io/badge/Node.js-%3E%3D22.15-339933?logo=nodedotjs)](browser-worker/package.json)
[![MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

[中文](README.md) · English · [API guide](docs/api.md) · [Browser worker](browser-worker/README.md) · [Changelog](CHANGELOG.md)

Based on [zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go), preserving upstream history and its MIT license. This edition adds persistent browser accounts, web quota tracking, and Windows launch management. It uses the Gemini web protocol, not Google's official API. No Google API key is required, but models, media capabilities, and quotas depend on actual account entitlements and web availability.

## Features

| Feature | Details |
| --- | --- |
| OpenAI-compatible API | Chat Completions, Responses, models, and incremental ordinary chat |
| Browser accounts | Separate Camoufox profiles, interactive login, identity confirmation, refresh, and batch management |
| Account scheduling | Automatic or pinned accounts, sticky or round-robin selection, five-hour and weekly quotas |
| Media | Image input, web image/music generation, asynchronous videos, downloads, image-to-video, and extension |
| Operations | Chinese/English dashboard, statistics, proxy pool, runtime settings, SQLite / MySQL / PostgreSQL |

```text
OpenAI-compatible client → Go API → Node + Camoufox → Gemini web app
                            └── dashboard / accounts / quotas / storage
```

Browser accounts send requests through real browser pages. Cookie-only and anonymous paths remain; their availability depends on upstream behavior. Browser accounts are the recommended setup below.

## Quick start

### 1. Prepare the environment

Install **Go 1.26.8 or a compatible newer version**, **Node.js ≥22.15 (24 LTS recommended)**, and Git. The machine must reach Gemini. Interactive login needs a graphical desktop session, including on remote hosts.

```bash
git clone https://github.com/LymtIcsh/gemini-web2api-go.git
cd gemini-web2api-go
cd browser-worker
npm ci
npx camoufox fetch
cd ..
```

The lockfile pins dependencies; Camoufox downloads the browser separately. Keep Node on PATH. The Go service starts the worker automatically.

### 2. Build and run

**Windows / PowerShell:**

```powershell
go build -trimpath -o gemini-web2api-go.exe .
.\start.bat
```

First launch requests an admin password of at least 12 characters, creates local `config.json`, and opens the dashboard. Existing configuration is preserved. Use `stop.bat` to stop this directory's service. To start without opening the dashboard:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\launcher.ps1 -Action Start -NoBrowser
```

**Linux / macOS:**

```bash
go build -trimpath -o gemini-web2api-go .
cp config.example.json config.json
# Edit host, admin_token, and default_model.
./gemini-web2api-go --config ./config.json
```

Dashboard: **http://127.0.0.1:8083/admin**. API Base URL: **http://127.0.0.1:8083/v1**.

The example listens on `0.0.0.0`; use `127.0.0.1` for local-only access. The Windows launcher initially selects `gemini-4-argon`, while the example uses `gemini-3.6-flash`. Select an entitled model in the dashboard or specify one in requests.

### 3. Sign in and connect a client

1. Open the dashboard and enter the admin password.
2. Add an account and complete Google login in its browser window.
3. Confirm login, verify readiness, and refresh quota. Close the visible window to release the default single browser slot.
4. Copy the service API key from the dashboard and configure your client's Base URL, key, and model.

The admin password and service API key are separate. If no API key is supplied at startup, one is generated and can be viewed and rotated in the dashboard.

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8083/v1",
    api_key="YOUR_SERVICE_API_KEY",
)
response = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "Introduce yourself in three sentences."}],
)
print(response.choices[0].message.content)
```

Install the example dependency with `python -m pip install openai`. See the [API guide](docs/api.md) for streaming, Responses, images, and videos.

## Accounts, quotas, and concurrency

- `sticky` selection reuses the current available account; `round_robin` rotates between requests. Accounts can also be pinned.
- A five-hour or weekly window at 100% excludes the account from automatic selection. Recovery requires a fresh positive web reading, not just an expired countdown.
- Existing conversations and pinned requests retain account identity. Manual disabling, login validity, and quotas remain separate.
- The default worker has one resident browser and a 60-second background idle timeout. Visible login windows stay open and can block other accounts until closed.

Unknown quota or a refresh error is not zero usage. More browser slots allow additional concurrency at the cost of memory.

## Configuration

See [config.example.json](config.example.json) and [config.go](internal/app/config.go). Rate limits, retries, default models, and account strategy are editable in the dashboard.

| Setting | Default / purpose |
| --- | --- |
| `host` / `port` | `0.0.0.0` / `8083`; first-run Windows setup uses `127.0.0.1` |
| `db_path` | `./data/gemini.db` |
| `admin_token` | Admin password; empty disables admin authentication |
| `default_model` | `gemini-3.6-flash`, subject to local/runtime settings |
| `account_strategy` | `sticky` or `round_robin` |
| `request_timeout_sec` | `180`; adjust for longer tasks |
| `proxy` | Seeds the proxy pool; manage exits and bindings in the dashboard |

| Environment variable | Purpose |
| --- | --- |
| `BROWSER_NODE` | Node executable, default `node` |
| `BROWSER_WORKER_PATH` | Normally `browser-worker/worker.mjs` |
| `BROWSER_PROFILE_ROOT` | Defaults to `browser-profiles` next to the configured database |
| `BROWSER_MAX_BROWSERS` | Resident browser limit, default `1` |
| `BROWSER_IDLE_TIMEOUT_MS` | Default `60000`; `0` disables the idle timer |
| `ADMIN_TOKEN` / `API_KEY` | Startup credentials; a supplied API key cannot be rotated in the dashboard |
| `SQL_DSN` | MySQL / PostgreSQL connection string; SQLite otherwise |

Migrate the database and browser profiles together. Local configuration, account data, cookies, logs, and diagnostic files are excluded from source uploads.

## Docker scope

The current [Dockerfile](Dockerfile) is a **Go-only distroless image** without Node, Camoufox, or a desktop environment. It cannot run browser accounts as supplied. Use native deployment for complete functionality.

For Go-only usage, build the current source:

```bash
docker compose up -d --build
```

[Compose](docker-compose.yml) uses `gemini-web2api-go:local` and a named volume. Upstream images and Releases do not contain this edition's extensions.

## API and limitations

| Endpoint | Behavior |
| --- | --- |
| `POST /v1/chat/completions` | Text, images, incremental ordinary chat; tool requests are buffered for parsing |
| `POST /v1/responses` | Compatible requests/events; streaming events are sent after generation |
| `GET /v1/models` | Exposed models, not proof of account entitlement |
| `/v1/videos` | Create, query, download, and extend; needs video entitlement |
| `/mcp` | HTTP MCP `web_search`, using the service API key |
| `GET /` | Service health/version, not Google login or generation verification |

Discover models through `/v1/models`. Source definitions include Flash, Pro, Argon, and thinking variants; actual capabilities and routing depend on upstream behavior. Function calling is prompt-based and `tool_choice` cannot guarantee execution. Parameters such as `temperature`, `top_p`, and `max_tokens` have no equivalent web controls.

Embeddings, standalone `/v1/images/*`, `/v1/audio/*`, and native Gemini `/v1beta/*` are not implemented. Request web image/music generation through conversation prompts.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| Missing Node or worker | Check `node --version`, keep `browser-worker/` beside the executable, install dependencies |
| Missing browser | Run `npx camoufox fetch` inside the worker directory |
| Account unavailable after login | Confirm login and check the actual email against the account binding |
| Account switching or quota refresh stalls | Close other visible login windows; the default limit is one browser |
| Unknown quota or connection reset | Inspect the error, check connectivity/proxy, then refresh |
| Video capacity/quota refusal | Preserve the upstream error and wait for availability; no video is implied |
| Client 401 | Use the service API key and include `/v1` in the Base URL |

## Development and verification

```bash
go test -count=1 ./...
go vet ./...
go build ./...
cd browser-worker
npm test
```

With Camoufox installed, run the local browser smoke inside the worker directory:

```powershell
$env:BROWSER_REAL_SMOKE = '1'
npm run smoke
```

Linux/macOS: `BROWSER_REAL_SMOKE=1 npm run smoke`. It uses temporary profiles and a local HTTP server, not Google or production accounts. See [verification notes](docs/verification.md) for actual results and boundaries.

`internal/app/` contains the Go service and embedded dashboard; `browser-worker/` contains the worker and tests; `launcher.ps1`, `start.bat`, and `stop.bat` provide Windows entry points.

## Credits and license

Based on [zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go), with [Camoufox](https://github.com/daijro/camoufox) and [Playwright](https://playwright.dev/). Distributed under the [MIT License](LICENSE), retaining original copyright notices.

Documentation edited on 2026-11-03 by Codex.
