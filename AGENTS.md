# llm-relay

OpenAI-compatible relay for local LLMs: per-key token metering, pricing and a cost dashboard (Go + SQLite, Docker)

**Stack:** Go · **Web service:** yes · **Entry points:** `make`

## Update rule

Any change that alters behaviour, interfaces, config, or workflow must update these
**in the same commit**:

1. `README.md` — anything a user or operator would need to know.
2. `AGENTS.md` — only if the rules, layout, or entry points below changed.
3. `docs/workflows/*.md` — the runbook that describes the changed process.

A change that leaves the docs describing the old behaviour is not finished.

## Read on demand

Detail lives in `docs/` — read the one file you need, never the whole folder:

| I want to know… | read |
|---|---|
| how changes get reviewed | `docs/workflows/review-protocol.md` |
| how bugs get fixed here | `docs/workflows/bug-fix-loop.md` |
| how features get built here | `docs/workflows/feature-dev-loop.md` |
| commit / branch / issue conventions | `docs/workflows/conventions.md` |
| what is being built now | newest file in `docs/plans/` |
| how to run / deploy it | `README.md` + `make` |

## Verify before commit

`make ci` passes, then the `reviewer` subagent returns `VERDICT: APPROVE` (full
gate: `docs/workflows/review-protocol.md`). Never commit on "it looked fine".

## Current work

Newest plan in `docs/plans/`. When you finish or change scope, update that plan
file and add a dated entry to its Changelog section.
