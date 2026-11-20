# Feature development loop — llm-relay

Automated implementation of Gitea `enhancement` issues lives in the
**`gitea-feature-dev` skill**
(`~/.agents/skills/gitea-feature-dev/SKILL.md`). Trigger it with "implement all
gitea enhancements" in this repo. This file records the project-local shape.

```
pick enhancement (oldest first) → oracle PLANS (writes docs/plans/<date>-<slug>-v1.md)
  → worker IMPLEMENTS from the plan → reviewer REVIEWS diff vs plan
  → APPROVE: commit "Feat: <summary> (#N)" + close
  → CHANGES_REQUESTED: back to worker (max 3 rounds, one oracle re-plan)
```

## Plan first, always

Every feature starts as a file in `docs/plans/` named
`YYYY-MM-DD-<slug>-v1.md`, from `docs/plans/TEMPLATE.md`. No plan file, no
implementation. The plan is the contract the reviewer checks the diff against.

Bump the version (`-v2.md`) instead of silently rewriting a plan mid-flight, and
add a dated line to the Changelog of the plan you are working in.

## Project-local rules

1. **Commit subject:** `Feat: <one-line summary> (#N)`.
2. **One writer per tree**; the plan and review happen around that single writer.
3. **`make ci` green** before the reviewer is launched — do not spend a review
   round on a build that does not compile.
4. **Docs ship with the feature:** the `AGENTS.md` update rule applies, plus the
   plan file updated to reflect what was actually built.
5. **Scope discipline:** the diff does only what the plan says. Anything else is
   a new issue, not a drive-by improvement.
6. **Deferred decisions** go into the plan's Open Questions section rather than
   being improvised in code.
