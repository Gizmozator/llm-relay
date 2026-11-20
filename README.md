# llm-relay

OpenAI-compatible relay for local LLMs: per-key token metering, pricing and a cost dashboard (Go + SQLite, Docker)

- **Stack:** Go
- **Web service:** yes
- **App URL:** http://localhost:8080 — default host port, override with `PORT=<n>`
- **Repo (SSH):** `ssh://git@gitea-host:8022/gizmoz/llm-relay.git`
- **Repo (web):** http://gitea-host:3000/gizmoz/llm-relay

## Quickstart

```bash
make build      # -> ./llm-relay
make run        # local run
make ci       # fmt + vet + tests
```

## Common tasks

| task | command |
|---|---|
| list all targets | `make` |
| build | `make build` |
| test + vet | `make test` |
| full check (fmt + vet + tests) | `make ci` |
| run locally | `make run` |
| build container | `make docker-build` |
| start stack | `make docker-up` |
| tail logs | `make docker-logs` |
Run `make` with no arguments for the annotated target list.

## Configuration

All overridable via environment or a local `.env` file (see `.env.example`):

| var | default | meaning |
|---|---|---|
| `PORT` | `8080` | **host** port you browse to. The container-internal port stays `8080` (own netns, cannot collide). |
| `IMAGE` | `llm-relay:latest` | Container image tag. |
| `TZ` | `Europe/Helsinki` | Timezone (tz database is copied into the runtime image). |

```bash
make run PORT=9100
make docker-up PORT=9100
```

## Layout

```
llm-relay
├── AGENTS.md            agent rules for this repo (keep it lean)
├── Makefile             every build/run/test entry point
├── Dockerfile           multi-stage build
├── docker-compose.yml   local stack
├── docs/
│   ├── workflows/       how we work: review, bug loop, feature loop, conventions
│   └── plans/           implementation plans (current + archived)
└── cmd/  internal/  (Go layout)
```

## Documentation

Start at `AGENTS.md` for working rules, then `docs/workflows/README.md` for the
runbooks, and `docs/plans/` for what is being built right now.
