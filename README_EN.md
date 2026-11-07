# AnyProxyAi · Failover Edition

A fork of [cniu6/anyproxyai](https://github.com/cniu6/anyproxyai) v2.0.7 that turns the display-only *groups* into real **failover groups**, and fixes the **concurrency** problems seen when several sessions share one endpoint.

Everything from upstream is kept: OpenAI / Claude / Gemini / Claude Code / Cursor conversion, a single local gateway, usage statistics.

## What changed

### 1. Failover groups

A group is **one public model id** plus **N upstream endpoints** (URL + key + format) ordered by `priority`.

Each group picks one **strategy**:

| Strategy | Value | Behavior |
| --- | --- | --- |
| **Failover** (default) | `failover` | Strict priority order; falls through only on failure |
| **Balanced** | `balanced` | Picks a random *available* endpoint per request; still falls through on failure |
| **Polling** | `polling` | Rotates through *available* endpoints in priority order; still falls through on failure |

All three share the same cooldown / fallthrough / fallback logic — only the first pick differs.

- Failover triggers on: network errors / timeouts, HTTP `5xx`, HTTP `401`/`403`.
- No failover on `429` or other `4xx` — those are returned to the client as-is.
- Cooldown is per group (default `600s`) and **sliding**: every request to the group resets the timer to the full cooldown, so a failing endpoint stays skipped while traffic continues, and becomes available again only after the group has been idle for the whole cooldown. The remaining time shown in the UI slides the same way.
- If every endpoint is cooling, the group retries them in priority order rather than failing outright.
- A **Reset Cooldown** button on each group card clears that group's cooldown and polling state immediately (greyed out when nothing is cooling).
- `proxy_auto` (or your custom keyword) can redirect to a failover group **or** to a single ungrouped route.
- Ungrouped routes behave exactly as upstream: random load balancing across routes sharing a model.

### Stream truncation detection

Some relay services cut a single stream short in the middle of "long reasoning + a multi-KB `tool_calls` argument" and all the client gets is `{"error":{"message":"Upstream request failed"}}` — no `finish_reason`, no `[DONE]`.

Every stream is now checked at EOF for a real completion marker (OpenAI `[DONE]` / `finish_reason`, Claude `message_stop` / `stop_reason`, Gemini `finishReason`, non-zero `completion_tokens`):

| Case | Behaviour |
| --- | --- |
| Marker present | Passed through unchanged, logged as success |
| No marker (truncated) | Logged as `success=false` with the reason; endpoint marked failed and cooled down; no fake end event is synthesised |
| Truncated before any byte was forwarded | Fails over to the next endpoint in the group |
| Truncated after content was forwarded | No retry (would duplicate content); the client gets one `code: "STREAM_TRUNCATED"` error frame plus `[DONE]`, keeping the upstream message in `upstream_message` |

An `error` object injected into the stream by the upstream is normalised into the same single `STREAM_TRUNCATED` frame, so clients never see two errors.

### 2. Concurrency

The proxy path was already concurrent; the real bottleneck was SQLite logging (`database is locked (SQLITE_BUSY)`) when multiple sessions wrote at once. Now:

- WAL journal mode, `busy_timeout=10000`, `synchronous=NORMAL`;
- connection pool `MaxOpenConns=32` / `MaxIdleConns=16`;
- upstream `http.Transport` no longer caps per-host connections (`MaxConnsPerHost=0`, `MaxIdleConnsPerHost=100`);
- per-request full body logging removed.

Measured: three concurrent streaming requests through the same endpoint finish in the time of one.

### 3. Response sanitizing (think-as-content repair)

Some OpenAI-compatible upstreams mangle native DeepSeek/Claude output:

| Symptom | What the client sees | Why it matters |
| --- | --- | --- |
| reasoning copied into content | `content` byte-identical to `reasoning_content` | no `tool_calls`, `finish_reason=stop` — an agent treats half a reasoning trace as the final answer and the turn ends |
| raw `<thinking>` tags | `content` starts with `<thinking>…` | chain of thought shown as chat text |
| DSML tool calls as text | `<｜｜DSML｜｜ invoke name="…">` inside `content` | tool calls never happen |

The built-in sanitizer (`internal/sanitize`) repairs all three before forwarding: drops duplicated content, moves tagged text back into `reasoning_content`, and parses DSML back into real `tool_calls` (with `finish_reason=tool_calls`). It works for streaming and non-streaming, passes clean responses through untouched, retries once only when a response is provably broken, and fails loudly with `THINK_AS_CONTENT` instead of inventing an answer.

Toggle: **Settings → Repair thinking leaks**, config key `sanitize_thinking` (default `true`). Only the OpenAI pass-through path is affected; Claude/Gemini adapter paths are unchanged.

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
