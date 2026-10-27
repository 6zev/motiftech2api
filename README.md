# motiftech2api

An OpenAI-compatible API in front of [chat.motiftech.io](https://chat.motiftech.io/chat).
Point any OpenAI client at it and talk to Motif's agent.

```
your client ──► /v1/chat/completions ──► chat.motiftech.io/api/v1/enterprise/chat
                (OpenAI JSON)             (Motif SSE)
```

Written in Go, standard library only: one static binary, no runtime, no dependencies.

## Quick start

```bash
cp .env.example .env            # optional: edit port, keys, timeout
go build -o motiftech2api ./cmd/motiftech2api
./motiftech2api                 # http://0.0.0.0:3000
```

Or download a prebuilt binary from the
[releases page](https://github.com/6zev/motiftech2api/releases) — statically linked,
no runtime needed:

```bash
chmod +x motiftech2api_v1.0.0_linux_amd64
./motiftech2api_v1.0.0_linux_amd64 --version
```

Verify a download against the release's `checksums.txt`:

```bash
sha256sum -c checksums.txt --ignore-missing
```

```bash
curl -s localhost:3000/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"motif-12-7b","messages":[{"role":"user","content":"hello"}]}'
```

Settings come from the environment. The server also reads `./.env` at startup if it
exists (see `.env.example`) — **real environment variables always win**, so a
systemd unit or `docker --env-file` is never shadowed by a stray file. Point
`DOTENV_PATH` elsewhere to load a different file. The startup log states which file
was read and which keys came from it:

```
env: /srv/motif/.env (3 setting(s): PORT, UPSTREAM_BASE, DEBUG)
motif gateway listening on http://0.0.0.0:4055
```

The parser is small and deliberately inert: it handles comments, `export `, quoted
values and inline comments, but never interpolates `$VAR` and never runs commands.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `3000` | Listen port |
| `HOST` | `0.0.0.0` | Listen address |
| `UPSTREAM_BASE` | `https://chat.motiftech.io` | Upstream service |
| `UPSTREAM_TIMEOUT_MS` | `120000` | Per-request upstream timeout |
| `GATEWAY_API_KEYS` | *(empty)* | Comma-separated bearer keys. Empty means **no auth** |
| `CONVERSATION_TTL_HOURS` | `168` | Idle lifetime of a stored conversation |
| `STORE_PATH` | `./.data/conversations.json` | Conversation cursor file |
| `DEBUG` | `0` | `1` enables verbose logs (never logs prompts or keys) |

### Auth

With `GATEWAY_API_KEYS` empty the server is open — fine for localhost. Set it before
exposing the port:

```bash
GATEWAY_API_KEYS=sk-your-key,sk-other ./motiftech2api
curl localhost:3000/v1/models -H 'authorization: Bearer sk-your-key'
```

Keys are compared by SHA-256 digest against every configured key, with no early exit.
`/health` is never gated.

## Endpoints

| Method | Path | Notes |
|---|---|---|
| `GET` | `/health` | `{"status":"ok"}` |
| `GET` | `/v1/models` | `motif-12-7b`, `motif-12-7b-reasoning` |
| `POST` | `/v1/chat/completions` | Stream and non-stream |

Errors use the OpenAI envelope:

```json
{"error":{"message":"...","type":"invalid_request_error","code":"model_not_found"}}
```

## Models

| Model | Behaviour |
|---|---|
| `motif-12-7b` | The plain model |
| `motif-12-7b-reasoning` | The reasoning model |

Both are the upstream's own frontend ids. Any other id is rejected with `400
model_not_found` — the upstream has no model routing of its own, so the request is
routed here:

- `gpt-*`, `o1*`, `o3*`, `o4*`, or anything containing `reasoning` → `motif-12-7b-reasoning`
- no model → `motif-12-7b`

`reasoning_effort` accepts `low` / `medium` / `high` and is translated to the
upstream's `small` / `medium` / `high`. The literal `low` is never forwarded, because
the upstream rejects it.

## Conversations

The upstream is an agent with its own server-side history keyed by a
`conversation_id`. The gateway maps that onto a caller key so multi-turn chat works
the way an OpenAI client expects:

1. `chat_id`, `session_id` or `conversation_id` in the request body, else
2. the `x-motif-conversation-id` header, else
3. `user:<value>` when the request has a `user` field, else
4. `default`

The gateway remembers the upstream id **and a cursor** (how many of your messages the
upstream has already been told about), so when you replay the whole `messages[]` array
each turn only the new part is sent. Persisted to `STORE_PATH` so a restart does not
lose conversations.

The cursor only advances when a turn finishes cleanly. If the stream breaks
mid-answer, the conversation id is kept (so context survives) but the cursor is not
advanced, so your next retry resends that turn instead of silently skipping it.

### Important limit

The upstream accepts at most 50000 characters of query. The gateway budgets 48000
(leaving room for role markers) and returns `400 context_length_exceeded` beyond that
rather than silently dropping earlier turns. In practice: keep the history you send
short, or start a new `chat_id` when a thread gets long. Because history is chained
upstream, you can usually send only the newest user message.

## No tool calling (read this)

Client-side tools, function calling and MCP are **not supported**, and this is not a
limitation that can be coded around. The upstream is an opaque agent:

- it never emits `tool_calls`, so the gateway has nothing to translate;
- its request body has no field for tool results, so a tool round-trip cannot be
  expressed;
- `disabled_tool_names` can only switch *its own* tools off, never add yours.

A request that carries `tools` is therefore rejected with `400 tools_not_supported`
rather than silently ignored, because ignoring it would leave a tool-using client
waiting forever for a call that never arrives.

If you need real tool calling or MCP, put a model provider that supports it in front
(any OpenAI-compatible provider with function calling), and use Motif for plain chat.

## Client examples

### OpenAI SDK (Go)

```go
client := openai.NewClient(option.WithBaseURL("http://localhost:3000/v1"),
    option.WithAPIKey("unused"))

res, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
    Model:    "motif-12-7b",
    Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
})
```

### OpenAI SDK (JS/TS)

```ts
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: process.env.GATEWAY_KEY ?? "unused",
  baseURL: "http://localhost:3000/v1",
});

const res = await client.chat.completions.create({
  model: "motif-12-7b",
  messages: [{ role: "user", content: "hello" }],
});
console.log(res.choices[0].message.content);
```

### OpenAI SDK (Python)

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:3000/v1", api_key="unused")
res = client.chat.completions.create(
    model="motif-12-7b",
    messages=[{"role": "user", "content": "hello"}],
)
print(res.choices[0].message.content)
```

### Open WebUI

Settings → Connections → OpenAI API:

- Base URL: `http://<host>:3000/v1`
- API key: any value if auth is disabled, otherwise a key from `GATEWAY_API_KEYS`

Models appear as `motif-12-7b` and `motif-12-7b-reasoning`.

### SillyTavern

API → Chat Completion → Custom (OpenAI-compatible):

- Custom endpoint: `http://<host>:3000/v1`
- Model: `motif-12-7b-reasoning`

For per-chat continuity, send your chat id in the `x-motif-conversation-id` header if
your client supports custom headers; otherwise the gateway falls back to `default`,
which means all chats share one upstream thread.

### curl, streaming

```bash
curl -N localhost:3000/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"motif-12-7b","stream":true,
       "messages":[{"role":"user","content":"count to 3"}]}'
```

## What is not supported

- **Tool / function calling and MCP** — see above. Requests with `tools` are rejected.
- Files, document upload, templates, projects, image generation.
- Embeddings, images, audio.
- Multiple model families: only the two Motif ids exist upstream.
- `usage` is approximated: the upstream reports none, so token counts are estimated
  from character counts (~4 chars per token).

## Development

```bash
go build -o motiftech2api ./cmd/motiftech2api
go vet ./...
go test ./...                      # 63 unit tests, no network
E2E=1 go test ./cmd/motiftech2api -run TestLive -v   # 3 live tests
```

### Releasing

Tag and push; CI builds every platform, verifies the version stamp, publishes a
GitHub release with checksums, and then smoke-tests the published asset:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Or run the **Release** workflow manually and give it a tag.

The smoke test is a job inside the same workflow, not a separate one: a release
published with `GITHUB_TOKEN` does not emit a `release: published` event, so a
separate workflow would never fire. It downloads the real asset, verifies its
checksum, and starts it to confirm `/health` and `/v1/models` respond. If any of
that fails, the release run fails with it.

`--version` is stamped at build time (`-X main.version`); a plain local build reports
`dev`.

Layout:

```
cmd/motiftech2api/main.go    startup, signal handling
internal/config              env parsing
internal/types               upstream event union + OpenAI shapes
internal/upstream            SSE client and decoder
internal/translate           model/effort/language mapping + the history cursor rule
internal/store               conversation cursor store (atomic JSON + mutex)
internal/server              routes, auth, streaming, error mapping
```

### Upstream protocol notes

Verified against production on 2026-10-27:

- `POST /api/v1/enterprise/chat`, `accept: text/event-stream`; **no auth required**.
  `Origin` must be `https://chat.motiftech.io` (its CORS allows only that).
- Body requires `query` **and** a `conversation_id` key (may be `null`). Omitting the
  key is a `400`.
- `model` is only legal together with `mode: "chat"`, otherwise
  `400 "enterprise mode does not accept model override"`.
- `language` is `ko` or `en` only; `reasoning_effort` is `small`/`medium`/`high`
  (`low` → 400); `disabled_tool_names` is accepted.
- Unknown conversation id → `404 {"detail":"conversation not found"}`.
- Query limit: 49000 chars accepted, 50001 rejected; 50000 is the schema value.
- **SSE framing**: exactly one complete JSON event per `data:` line. Do not join the
  data lines of a frame — they are separate events, and joining produces invalid JSON.
- **`shared.sign.tool` is a per-tool boundary, not the end of a turn.** It carries the
  same `[[HUB_STOP_SIGN]]` payload as the final `shared.sign`. Ending on it truncates
  every turn that used a tool.
- **Text streams before the step that classifies it.** A run of `chat.text` is
  narration until `chat.step {role:"narration"}` closes it, and is the answer when
  `chat.step {role:"answer"}` closes it. The gateway buffers per run and discards
  narration.

Two of those were bugs caught by tests during development; `internal/upstream` and
`internal/translate` each have regression tests naming them.

## Legal note

This is an unofficial gateway. It sends requests to a service you may not own, acting
on their infrastructure without a contract, and it deliberately mirrors a browser
request. Use it for personal experimentation only; running it at scale, reselling
access, or using it against the site's terms of service is your responsibility, and
the upstream can break or block it at any time without notice.
