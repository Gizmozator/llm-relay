# Conventions — llm-relay

## Remotes and addresses

| what | value |
|---|---|
| Gitea owner/repo | `gizmoz/llm-relay` |
| GitHub (public mirror) | `https://github.com/Gizmozator/llm-relay` |

The git remote points at the owner's Gitea instance (host varies per network);
`tea` talks to the API URL from `~/.config/tea/config.yml`. When parsing a remote
URL for `owner/repo`, ignore the host and take the last two path segments.

## Commits

```
Fix:  handle nil pointer in auth middleware (#12)
Feat: add CSV export to the reports page (#31)
Chore: bump Go to 1.26 and refresh deps
Docs: document the /api/v1/health endpoint
```

- Imperative, one line, ≤ 72 chars, issue ref in parentheses when there is one.
- Body only when the *why* is not obvious from the subject. No changelog-style
  essays — the diff says what, the body says why.
- Stage explicit paths (`git add <file>`). Never `git add -A` / `git add .`:
  unrelated files must not ride along.
- Never commit secrets, `.env`, local scratch files, or agent caches.

## Branches

- `main` is the working trunk; small, reviewed changes go straight to it.
- Long-running or risky work: `feature/<slug>`, `fix/<slug>`, merged back by PR
  once green.
- Never force-push `main`.

## Issues (Gitea via `tea`)

All issue operations go through the **`gitea-issues` skill** (commands, output
projection, retry policy, dedupe + ticket filing). Two local habits worth
repeating here: keep issue bodies in files (never re-sent through context), and
redirect the output of writes — `tea` echoes whole bodies back.

## Doc updates

One rule, stated once in `AGENTS.md` §Update rule — behaviour/interface/config/
workflow changes update `README.md`, `AGENTS.md` and the affected
`docs/workflows/*.md` in the same commit.

## Local scratch

Machine-local artifacts go to `~/.cache/llm-relay/` (diffs for review, issue
dumps, logs) — never inside the repo. The repo stays cloneable and clean.
