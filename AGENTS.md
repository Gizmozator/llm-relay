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

## Public repo — no private info

This repo is published to GitHub (`github` remote, `github.com/Gizmozator/llm-relay`,
**public**). Nothing homelab-specific or personal may enter it: no home hostnames,
domains or IPs (`*.rantaeskola.fi`, `10.0.1.x`, …), usernames, emails, tokens or
real deployment URLs — in code, docs, screenshots or commit messages. The one
allowed exception is `gitea.rantaeskola.fi` (public style repo + image registry)
and its `gizmoz` org paths. Use generic examples (`relay.example.fi`) in docs and
overridable placeholders in config. Plan docs use `**Owner:** maintainer`, never
a personal name. Before pushing, scan the diff's added lines (AGENTS.md is
excluded — it names the patterns):

```bash
git fetch github && git diff github/main -- ':!AGENTS.md' \
  | grep -iP '^\+.*((?<!gitea\.)rantaeskola|10\.0\.1\.|\bantti\b)'
```

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
