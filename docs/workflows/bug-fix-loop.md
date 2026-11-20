# Bug fix loop — llm-relay

Automated end-to-end fixing of Gitea `bug` issues lives in the
**`gitea-bug-fixer` skill** (`~/.agents/skills/gitea-bug-fixer/SKILL.md`).
Trigger it with "fix all gitea bugs" in this repo. This file records the
project-local shape of that loop.

```
list open bug issues → oldest first → worker fixes → reviewer reviews
  → APPROVE: commit "Fix: <summary> (#N)" + comment + close
  → CHANGES_REQUESTED: findings back to worker (max 3 rounds)
  → blocked: stash, comment on the issue, leave it open
```

## Project-local rules

1. **Label:** bugs carry the `bug` label on Gitea (`tea label list -r gizmoz/llm-relay`).
2. **One bug at a time.** A single `worker` writes this tree; the commit (or
   stash) happens before the next bug starts.
3. **Commit subject:** `Fix: <one-line summary> (#N)` — the `(#N)` is what links
   the commit to the issue in Gitea.
4. **Never close before the commit exists.** Comment first, then close.
5. **Tests:** the fix must keep `make ci` green, and a bug fix ships a regression
   test for the broken behaviour whenever the code is testable.
6. **Docs:** observable behaviour changed → the `AGENTS.md` update rule applies
   in this same commit.
7. **New defects found on the way** → new `bug` tickets after dedupe, never
   drive-by fixes in this commit.

## Manual (single issue, no skill)

```bash
tea issue N -r gizmoz/llm-relay > ~/.cache/llm-relay/issue-N.txt   # never into context
# → worker fixes from that file → reviewer reviews the diff file → commit → close
tea comment add N -r gizmoz/llm-relay --description "Fixed in <sha> — <summary>." > /dev/null
tea issue close N -r gizmoz/llm-relay > /dev/null
```
