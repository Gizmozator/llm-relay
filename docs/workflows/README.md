# Workflows — how work gets done in llm-relay

Runbooks. Read the one you need; do not read the folder.

| runbook | when |
|---|---|
| [review-protocol.md](review-protocol.md) | before every commit — how the `reviewer` subagent is used |
| [bug-fix-loop.md](bug-fix-loop.md) | fixing a reported bug (Gitea `bug` label) |
| [feature-dev-loop.md](feature-dev-loop.md) | building an enhancement / feature request |
| [remote-backend.md](remote-backend.md) | bringing up a remote-backend connector box and verifying it on the relay |
| [conventions.md](conventions.md) | commit messages, branches, issue references, doc updates |

## The invariant

Nothing lands unreviewed and nothing lands with stale docs.

```
plan (docs/plans/) → implement → make ci → reviewer subagent → commit → push
                                              ↑ CHANGES_REQUESTED loops back
```

## Agents used here

Agent roles, the slot budget and who owns git/Gitea writes: `~/.agents/AGENTS.md`.
In short — `worker` is the one writer; `scout`/`oracle`/`reviewer` are read-only;
the parent session owns every git and Gitea write.
