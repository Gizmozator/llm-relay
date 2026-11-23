# Review protocol

Every diff gets reviewed by the `reviewer` subagent before it is committed.
The reviewer has no shell: it reads a diff **file** you materialize first.

## 1. Materialize the diff (never paste it into a prompt)

```bash
git diff            > ~/.cache/llm-relay/review.diff
git diff --cached   > ~/.cache/llm-relay/review-cached.diff   # if anything is staged
git status --porcelain                                          # a few lines only
```

## 2. Launch the reviewer (fresh context, read-only)

```
# Legacy API
subagent({ agent: "reviewer", cwd: "<repo root>", context: "fresh", async: false,
           timeoutMs: 7200000, task: <TASK> })

# Current API (Agent tool) — no cwd param: the session must already be started
# in the repo root; max_turns replaces the wall-clock budget
Agent({ subagent_type: "reviewer", run_in_background: false, max_turns: 80,
      description: "review changes", prompt: <TASK> })
```

Task wording matters (the launcher classifies intent) — the full read-only
wording contract is in `~/.agents/skills/_shared/subagent-launch.md`:

- KEEP the sentence: `You must NOT edit, commit, or stage files.`
- AVOID `must fix` / `implement` — say `needs to change`.
- Point at the diff file; never inline the diff.

Ask about: correctness against the stated goal, minimality, consistency with the
codebase, regressions, missing edge cases, test gaps.

## 3. Verdict contract

The reviewer returns exactly one of:

```
VERDICT: APPROVE
```
```
VERDICT: CHANGES_REQUESTED
1. <file>: <what needs to change>
2. ...
```

## 4. Act on it

| verdict | action |
|---|---|
| APPROVE | commit (see `conventions.md`), then push |
| CHANGES_REQUESTED | send the numbered findings back to the implementer, re-review. Max 3 rounds. |
| 3 failed rounds | stop, stash the work, record the blockers in the plan file, ask the human |

## 5. OCR gate

After the reviewer returns `APPROVE`, run **one** `ocr` pass on the diff
(`ocr review --concurrency 1 --audience agent -f json`). Fix the adopted
findings, then have the reviewer re-verify the amended diff and commit.
No `ocr` re-runs per task; a whole-repo `ocr scan` runs once at the end
of a feature epic.

## 6. Extra defects found during review

Do **not** fix them in the current change. Dedupe and file them as new `bug`
tickets per the **`gitea-issues`** skill, *Dedupe baseline + filing new bug
tickets* — an unrelated fix inside a commit is a review nightmare.
