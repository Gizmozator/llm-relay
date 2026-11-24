# llm-relay — AI agent integration guide

You are an AI agent reading a guide for an **llm-relay** server: a shared
local LLM relay. This page tells you how to check whether a model here is
free before your agent takes on delegated work, how your human gives you an
access token, and how to use the relay as an OpenAI-compatible endpoint for
inference.

The server you are reading this on is reachable at: `{{RELAY_URL}}`

## 1. Access token

All agent endpoints authenticate with a relay API key: a secret starting
with `sk-`, sent as `Authorization: Bearer sk-...`.

Your human creates one for you: open the relay **dashboard**{{DASHBOARD_URL}}
→ **My keys** → *Create key* (name it after you, e.g. `subagent`). The full
key is shown **once** — store it and give it to you via your config or
environment. The same key also works for inference
(`POST {{RELAY_URL}}/v1/chat/completions` and friends), so one token covers
both — see section 4 for how to use it.

## 2. Check utilization before delegating

### REST

```
curl -s {{RELAY_URL}}/v1/utilization \
  -H "Authorization: Bearer sk-YOUR_KEY"
```

Response:

```json
{
  "window_minutes": 5,
  "models": [
    {
      "id": 3,
      "name": "halogen-flash",
      "online": true,
      "active": 1,
      "concurrency_limit": 4,
      "utilization_pct": 37.5,
      "status": "free"
    }
  ]
}
```

| field | meaning |
|---|---|
| `id` | physical model row id — use it to tell rows apart (see below) |
| `online` | the backend answers its health endpoint |
| `draining` | admin is shutting this backend down: existing sessions may finish, new ones are refused |
| `active` | streams currently generating on this model (server-wide) |
| `concurrency_limit` | engine slots for this model (0 = unset) |
| `utilization_pct` | mean busy % of the last 5 minutes, including streams still running; >100 = parallel streams |
| `status` | derived verdict: `free`, `busy`, `draining` or `offline` |

A **model group** is one public name backed by several machines: the list then
contains several same-name rows — distinct members of one pool, possibly with
different `status` — and `id` is the only way to tell them apart. Requesting
the name lets the relay pick a member for you; each conversation stays pinned
to the member it started on.

`status` rules: `offline` = backend unreachable; `draining` = backend is
being taken down for maintenance — a request starting a **new** session gets
`503` (`Retry-After: 60`), while sessions active within the last 15 minutes
keep working; `busy` = `active` reached `concurrency_limit` (when set) **or**
`utilization_pct` at or above the model's admin-set threshold (default 80);
otherwise `free`.

### MCP (preferred for MCP-capable agents)

Add this to your agent's MCP server config (Claude Code: `claude mcp add
--transport http llm-relay {{RELAY_URL}}/mcp --header "Authorization: Bearer sk-YOUR_KEY"`):

```json
{
  "mcpServers": {
    "llm-relay": {
      "type": "http",
      "url": "{{RELAY_URL}}/mcp",
      "headers": { "Authorization": "Bearer sk-YOUR_KEY" }
    }
  }
}
```

It exposes one tool, `check_utilization`, returning the same payload (an
optional `model` argument filters to one model name).

## 3. Suggested decision logic

- `status == "free"` → go ahead, delegate here.
- `status == "busy"` → wait and re-check (e.g. every 30–60 s), or pick
  another model / server.
- `status == "draining"` → do not start new sessions here (they get `503`);
  if you are continuing an existing conversation on it, you may finish.
  Pick another model/server for anything new.
- `status == "offline"` → skip this server entirely.

The check is advisory: there is no reservation, so another client can take
the last slot between your check and your request. Re-check on 429/503.

## 4. Use the relay for inference

Once a model is `free`, the relay is a plain **OpenAI-compatible endpoint**:
point any OpenAI client (or `curl`) at it with the same `sk-` key you used to
check utilization.

### Gateway config

- **Base URL:** `{{RELAY_URL}}/v1`
- **Auth header:** `Authorization: Bearer sk-YOUR_KEY` (the same key as the
  utilization check above)

Everything below uses those two.

### List models

```
curl -s {{RELAY_URL}}/v1/models \
  -H "Authorization: Bearer sk-YOUR_KEY"
```

Response (OpenAI model list):

```json
{
  "object": "list",
  "data": [
    { "id": "halogen-flash", "object": "model", "owned_by": "llm-relay" }
  ]
}
```

Each `id` is a name you can request. A **model group** shows up as one name
here; requesting it lets the relay pick a member for you (see section 2).

### Generate

`POST /v1/chat/completions` (chat) and `POST /v1/completions` (text) take the
usual OpenAI request body. Minimal chat call:

```
curl -s {{RELAY_URL}}/v1/chat/completions \
  -H "Authorization: Bearer sk-YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "halogen-flash",
    "messages": [{ "role": "user", "content": "Hello!" }],
    "max_tokens": 100
  }'
```

Response (the backend's OpenAI `chat.completion`, passed through):

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "halogen-flash",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "Hi!" },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 9, "completion_tokens": 5, "total_tokens": 14 }
}
```

`POST /v1/completions` is the same shape with `prompt` instead of `messages`
and `choices[].text` instead of `message` (`object: text_completion`).

- `model` is required; a name the relay does not know returns `404`.
- `"stream": true` switches to SSE (`text/event-stream`) with the same fields;
  the relay turns on usage reporting for streaming chat automatically.
- The body is the backend's own reply, passed through untouched; the relay
  meters `usage.prompt_tokens` and `usage.completion_tokens` from it.
