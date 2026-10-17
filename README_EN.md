# AnyProxyAi · Failover Edition

A fork of [cniu6/anyproxyai](https://github.com/cniu6/anyproxyai) v2.0.7 that turns the display-only *groups* into real **failover groups**, and fixes the **concurrency** problems seen when several sessions share one endpoint.

Everything from upstream is kept: OpenAI / Claude / Gemini / Claude Code / Cursor conversion, a single local gateway, usage statistics.

## What changed

### 1. Failover groups

A group is **one public model id** plus **N upstream endpoints** (URL + key + format) ordered by `priority`.

- The gateway tries endpoints in priority order and falls through on failure.
- Failover triggers on: network errors / timeouts, HTTP `5xx`, HTTP `401`/`403`.
- No failover on `429` or other `4xx` — those are returned to the client as-is.
- Cooldown is per group (default `600s`) and **sliding**: every request to the group re-arms the timer, so a failing endpoint stays skipped while traffic continues, and the group returns to the highest priority only after the group has been idle for the whole cooldown.
- If every endpoint is cooling, the group retries them in priority order rather than failing outright.
- `proxy_auto` (or your custom keyword) can redirect to a failover group **or** to a single ungrouped route.
- Ungrouped routes behave exactly as upstream: random load balancing across routes sharing a model.

### 2. Concurrency

The proxy path was already concurrent; the real bottleneck was SQLite logging (`database is locked (SQLITE_BUSY)`) when multiple sessions wrote at once. Now:

- WAL journal mode, `busy_timeout=10000`, `synchronous=NORMAL`;
- connection pool `MaxOpenConns=32` / `MaxIdleConns=16`;
- upstream `http.Transport` no longer caps per-host connections (`MaxConnsPerHost=0`, `MaxIdleConnsPerHost=100`);
- per-request full body logging removed.

Measured: three concurrent streaming requests through the same endpoint finish in the time of one.

## Quick start

Download `anyproxyai-windows-amd64.exe` from [Releases](https://github.com/kaka100330/anyproxyai-failover/releases) and run it. It listens on `http://127.0.0.1:5642` and creates `config.json` / `routes.db` next to the executable.

Create a group in **Models → New Group**, add endpoints under **Endpoints**, then point your client at the group's model id — or set the group as the redirect target for `proxy_auto`.

## Build

Requires Go 1.22+ and Node.js 18+.

```bash
cd frontend && npm install && npm run build && cd ..

go install github.com/akavel/rsrc@latest
rsrc -ico build/windows/icon.ico -manifest build/windows/anyproxyai.exe.manifest -o rsrc_windows_amd64.syso
go build -ldflags "-s -w -H windowsgui" -o build/bin/anyproxyai-windows-amd64.exe .
```

The frontend must be built first: `main.go` embeds `frontend/dist`.

## License

MIT. Original work © the AnyProxyAi authors; see [LICENSE](LICENSE).
