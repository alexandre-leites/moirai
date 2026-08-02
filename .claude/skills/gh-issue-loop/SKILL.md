---
name: gh-issue-loop
description: Autonomous GitHub issue-working loop. Picks eligible labelled issues, claims them, runs one sub-agent per issue in its own git worktree, and drives each PR all the way to merge under a hard concurrency ceiling. Use ONLY when the user explicitly asks to arm, run, stop, or check this loop.
argument-hint: "[arm|run|status|stop] [key=value ...]"
disable-model-invocation: true
disallowed-tools: AskUserQuestion
allowed-tools: Bash Read Write Edit Grep Glob Agent SendMessage CronCreate CronList CronDelete WebFetch
---

# Autonomous GitHub issue-working loop

One pass: find eligible issues, claim them, work them concurrently in isolated worktrees, and
drive every resulting PR to **merge**. It is fired repeatedly by a scheduler, so a pass with
nothing to do must cost almost nothing.

This body is the single source of truth. Claude Code reads it as a skill; opencode reads the same
file as both a skill and a symlinked custom command. Do not fork it.

---

## 0. MODE

`$ARGUMENTS` selects the mode. Default when empty is `run`.

| Mode | Meaning |
| :--- | :--- |
| `run` | Execute exactly one pass (STEP -1 → STEP 5). This is what the scheduler fires. |
| `arm` | Register the recurring schedule, then immediately execute one `run` pass. |
| `status` | Report in-flight count, capacity, queue contents, and open loop PRs. Claim nothing, spawn nothing. |
| `stop` | Cancel the recurring schedule. See **STOPPING THE LOOP**. |

Any `key=value` tokens in `$ARGUMENTS` override the parameters below for this invocation.

## 1. PARAMETERS AND ENVIRONMENT

| Parameter | Default | Meaning |
| :--- | :--- | :--- |
| `QUEUE_LABEL` | `ai-doable` | The queue. Human-curated. |
| `WORKING_LABEL` | `ai-working` | The claim. Never removed, including on failure. |
| `FINISHED_LABEL` | `ai-finished` | Applied only when the work is genuinely complete. |
| `BATCH_SIZE` | `5` | Max issues selected in one pass. |
| `MAX_AGENTS` | `5` | Hard ceiling on simultaneously running issue agents. |
| `CRON` | `*/5 * * * *` | Schedule interval. |
| `REAP_AFTER_MIN` | `90` | Minutes without worktree write activity before a slot is reclaimed. |
| `STARTUP_GRACE_MIN` | `10` | Minutes a fresh reservation is protected before its worktree exists. |

### Shell state does NOT persist between tool calls

Each Bash tool call is a fresh shell. Variables set in one call are **gone** in the next — an
unguarded `git -C "$REPO"` in a later call expands to `git -C ""`, and `repos/$OWNER/$NAME`
becomes `repos//`. So the first call of every pass writes the derived environment to disk, and
every later call sources it.

**Once per pass**, derive and persist. Never hardcode a repository, owner, or branch name:

```bash
REPO="$(cd "$(git rev-parse --git-common-dir)" && cd .. && pwd -P)"
mkdir -p "$REPO/.claude/issue-loop"
SLUG="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
cat > "$REPO/.claude/issue-loop/env.sh" <<EOF
REPO="$REPO"
OWNER="${SLUG%%/*}"
NAME="${SLUG##*/}"
DEFAULT_BRANCH="$(gh repo view --json defaultBranchRef --jq .defaultBranchRef.name)"
LOOP_DIR="$REPO/.claude/issue-loop"
WORKTREE_DIR="$REPO/.claude/worktrees"
QUEUE_LABEL="ai-doable"
WORKING_LABEL="ai-working"
FINISHED_LABEL="ai-finished"
BATCH_SIZE=5
export MAX_AGENTS=5 REAP_AFTER_MIN=90 STARTUP_GRACE_MIN=10
for c in "$REPO/.claude/skills/gh-issue-loop/bin/loop-slots.sh" \
         "\$HOME/.claude/skills/gh-issue-loop/bin/loop-slots.sh" \
         "\$HOME/.config/opencode/skills/gh-issue-loop/bin/loop-slots.sh"; do
  [ -x "\$c" ] && SLOTS="\$c" && break
done
:
EOF
```

**Then persist the tunables to disk — this is mandatory, not bookkeeping:**

```bash
printf 'MAX_AGENTS=%s\nREAP_AFTER_MIN=%s\nSTARTUP_GRACE_MIN=%s\n' \
  "$MAX_AGENTS" "$REAP_AFTER_MIN" "$STARTUP_GRACE_MIN" > "$LOOP_DIR/tunables"
```

`env.sh` is sourced only by the loop. Sub-agents call the helper by absolute path to `touch`
and `reacquire` their own leases, in their own shells, having never sourced it — so an
*exported* `MAX_AGENTS` never reaches them and they fall back to the built-in default of 5.
Skip this write and the ceiling silently applies only to the parent.

The trailing `:` is **required**, not decoration. Without it the last statement in `env.sh` is the
`for` loop, which exits non-zero when the final `[ -x ... ]` test fails — so under `set -e`, or in
any `. env.sh && ...` chain, sourcing would silently abort the call before the FATAL check below
could report the real problem.

Apply any `key=value` overrides from `run` by editing that file before use.

**PREAMBLE — prepend this to every subsequent Bash call in the pass:**

```bash
. "$(cd "$(git rev-parse --git-common-dir)" && cd .. && pwd -P)/.claude/issue-loop/env.sh" \
  || { echo "FATAL: cannot load loop env (wrong directory?)"; exit 1; }
[ -n "${SLOTS:-}" ] || { echo "FATAL: loop-slots.sh not found"; exit 1; }
```

**Both guards belong in every call, not just the first.** If `git rev-parse` fails because the cwd
is outside the repo, the path collapses to `/.claude/issue-loop/env.sh`, every variable stays
unset, and an unguarded call sails straight on into `git -C "" ...` — the exact failure this
section exists to prevent.

If the helper is genuinely missing, say so and stop. Do not improvise ad-hoc counting —
unbounded agent spawning is the exact failure this mechanism exists to prevent.

*(Every snippet below assumes the preamble has run.)*

---

## 2. HARD RULES — absolute, no exceptions

- **NEVER add a `Co-Authored-By` trailer or any co-author to a commit.**
- **Never push to the default branch directly.** All changes land through a PR.
- **Never force-push, and never rebase a shared branch.** To integrate, merge the default
  branch *into* the feature branch.
- **Never commit secrets or local artifacts.** No `.env`, credentials, caches, or build output.
- Never remove `WORKING_LABEL` from an issue, including when the attempt failed.
- Never re-add `QUEUE_LABEL` to an issue a human removed it from.
- Never close an issue by hand.

## 3. ENVIRONMENT QUIRKS — do not rediscover these

- **`gh issue view` is broken on gh 2.45** in repos touched by the Projects-classic GraphQL
  deprecation. Use `gh api "repos/$OWNER/$NAME/issues/<N>"` for issue reads.
  `gh issue list/create/comment/edit` and all `gh pr` subcommands are fine.
- **`gh issue list --label ...` silently truncates and serves stale label data.** Ground truth
  for a single issue is always:
  ```bash
  gh api "repos/$OWNER/$NAME/issues/<N>" --jq '[.labels[].name]'
  ```
  Confirm labels that way before claiming any issue.
- **GitHub's `mergeable` field goes stale for ~30s after any push.** When it disagrees with
  `git merge-tree --write-tree --name-only "origin/$DEFAULT_BRANCH" "origin/<branch>"`, trust the
  local result and re-query before reporting.
- Prefer GitHub/GitLab **MCP tools** when a server is connected; fall back to `gh`/`glab` CLI
  otherwise. Never stall waiting on an MCP server.
- **CI can be down repo-wide for reasons unrelated to the code.** The signature is every job
  failing in 1–10s, on the default branch as much as on feature branches, with a message like
  *"The job was not started because recent account payments have failed or your spending limit
  needs to be increased"*. When that happens: say so plainly, fall back to local validation, and
  escalate to the human. Do **not** silently claim green, and do **not** burn runs trying to fix
  code that is not broken.

## 4. THE THREE-LABEL STATE MACHINE

- `QUEUE_LABEL` — the queue. The human curates it by hand between runs. Treat its current
  contents as authoritative.
- `WORKING_LABEL` — the claim. Added the instant an issue is picked. **Never removed, including
  on failure.** This is what makes the loop idempotent: an issue is picked up at most once, so a
  failing issue is not retried every interval forever.
- `FINISHED_LABEL` — added only once the work is genuinely complete (**merged**, see §7),
  alongside the audit-trail comment.

**Eligible** = open AND has `QUEUE_LABEL` AND has NEITHER `WORKING_LABEL` NOR `FINISHED_LABEL`.

**Blocked** = has `WORKING_LABEL`, lacks `FINISHED_LABEL`, and has **neither an open PR nor a
merged PR** referencing it. That needs a human. The third label exists for exactly this distinction.

> **Check for a merged PR, not just an open one.** Since merge-is-done, a PR that merged is no
> longer open. An issue whose PR merged but which was killed before `FINISHED_LABEL` was applied
> looks identical to a blocked one if you only test "no open PR" — and the remedy for blocked
> (clear `WORKING_LABEL`) would make it eligible again and the loop would **redo already-merged
> work**. Always confirm with:
> ```bash
> gh pr list --state all --search "<N>" --json number,state,mergedAt,body \
>   | jq --argjson n <N> '[.[] | select(.body | test("[Cc]loses #\($n)\\b")) | {number,state,mergedAt}]'
> ```
> The `\b` word boundary matters: without it, issue #42 matches a PR that closes #420.

Any issue the loop opens itself must carry `QUEUE_LABEL` if it is meant to be worked later.

---

## 5. CONCURRENCY — bounded, durable, crash-safe

A scheduled fire can overlap a previous pass that is still working. The loop must never spawn
more than `MAX_AGENTS` concurrent issue agents.

### Durable state

```
$LOOP_DIR/inflight/issue-<N>.lease   one flat key=value file per in-flight issue
$LOOP_DIR/run.lock                   flock target serialising the reserve section
$LOOP_DIR/last-run.json              the previous pass's observations (see STEP -1)
```

A lease records `issue`, `branch`, `worktree`, `started`, `agent`.

### Why a lease registry rather than deriving from worktrees plus labels

Deriving in-flight state from "worktree exists AND has `WORKING_LABEL` AND no merged PR" was the
obvious alternative. The registry wins because:

1. **It is lockable.** Reserving a slot must be atomic against an overlapping fire. A derived
   view cannot be locked; a real file under `flock` can.
2. **It is free to read.** Counting costs one `ls`, so the idle check stays cheap. The derived
   view needs a GitHub round-trip per candidate on every fire.
3. **It can represent the gap.** Between reserving a slot and creating the worktree there is no
   worktree — a derived view is blind there and would double-claim. A lease covers it.
4. **It has somewhere to put the metadata** reaping needs: start time, agent id, real branch.

The two answer different questions and both are kept: **the registry is authoritative for local
slots; the labels remain authoritative for global claims.**

### The exact commands

```bash
"$SLOTS" count                          # -> INFLIGHT=<n> CEILING=<n> AVAILABLE=<n>
"$SLOTS" list                           # -> table of live leases, liveness, and the counts
"$SLOTS" reap                           # -> release stale leases, print each release
"$SLOTS" reserve <N> [N...]             # -> reap, recount, reserve up to AVAILABLE candidates
"$SLOTS" bind <N> <branch> <worktree>   # -> record the REAL branch/worktree after STEP 3
"$SLOTS" reacquire <N> [branch] [wt]    # -> re-take a counted slot for a resumed agent
"$SLOTS" release <N> [N...]             # -> return slots; prints RELEASED or NOT-HELD
"$SLOTS" touch <N> [N...]               # -> heartbeat; reports NOSLOT if already reaped
```

`reserve` prints exactly:

```
INFLIGHT_BEFORE=<n> CEILING=<n> AVAILABLE=<n>
RESERVED=<numbers>              # claim ONLY these
ALREADY_HELD=<numbers>          # another pass owns them; skip silently
SKIPPED_NO_CAPACITY=<numbers>   # eligible but no slot; report as "at capacity"
INFLIGHT_AFTER=<n>
```

**Claim only the issues listed in `RESERVED`.** Never claim from `SKIPPED_NO_CAPACITY`. If
`reserve` prints an `ERROR:` line and no `RESERVED=` line, it reserved **nothing** — fix the
arguments and re-run; do not claim anything on that pass.

Override the ceiling per invocation with the environment:

```bash
MAX_AGENTS=3 "$SLOTS" reserve 101 104 107
```

### How stale entries are reaped

`reap` runs automatically inside `reserve` and `reacquire`. It releases a lease when either holds:

1. **No worktree found** — neither the recorded path nor `issue-<N>` nor any `issue-<N>-*`
   suffixed variant — and the lease is older than `STARTUP_GRACE_MIN`. This means a pass died
   between reserving a slot and creating the worktree. The grace window exists so a legitimately
   fresh reservation is not reaped in that gap.
2. **No write activity for `REAP_AFTER_MIN`**, where activity is the newest modification time of
   the lease file or of any file in the worktree, excluding `.git/` and heavy generated trees
   (`node_modules`, `.venv`, `__pycache__`, caches, `dist`, `build`, `target`). A live agent
   writes files constantly; a killed one writes nothing. Git's own index churn is excluded so it
   cannot fake liveness.

A lease with a corrupt or truncated `started` field reads as age zero, so it is reaped
immediately rather than consuming a slot forever.

Reaping returns the **local slot only**. Per FAILURE HANDLING the issue deliberately keeps
`WORKING_LABEL`, so a reaped issue is not silently re-picked next pass — it waits for a human.

### Why it is crash-safe

1. `flock(2)` is released by the kernel when the holding process exits, **including on SIGKILL**.
   The critical section can never deadlock.
2. A lease is a hint, never the authority on liveness. Any lease whose worktree vanished or went
   quiet ages out. **Maximum slot leak is `REAP_AFTER_MIN`, never forever** — including for
   corrupt leases, which are reaped on sight.
3. Leases are keyed by issue number and written atomically (temp file + rename), so a resumed or
   repeated pass rewrites the same path and a crash mid-write cannot leave a torn record.
   Double counting is structurally impossible.
4. `reap → count → reserve` happen inside **one process holding one flock**, so two overlapping
   fires can never both conclude the same slot is free.
5. The helper resolves the **main** repository via `git rev-parse --git-common-dir`, so running it
   from inside a worktree still reads the one true registry rather than creating a second one.

### At capacity is NOT the same as queue blocked

These are different states with different remedies. **Never conflate them.** Report the in-flight
count and capacity on every single pass.

| State | Signature | Remedy |
| :--- | :--- | :--- |
| **At capacity** | `AVAILABLE=0`, eligible issues exist | Nothing. Wait for agents to finish. Healthy. |
| **Queue blocked** | `AVAILABLE>0`, but every eligible issue is excluded by overlap | Merge the named PRs to unblock. Human action. |
| **Queue empty** | No eligible issues at all | Human curates more `QUEUE_LABEL` issues. |
| **Stuck issue** | `WORKING_LABEL`, no `FINISHED_LABEL`, no open **or merged** PR | Human investigates and clears the label deliberately. |

---

## 6. THE PROCEDURE

### STEP -1 — cheap idle check

A scheduled poll must cost almost nothing when there is nothing to do.

```bash
"$SLOTS" count
git -C "$REPO" fetch origin -q
git -C "$REPO" log --oneline -1 "origin/$DEFAULT_BRANCH"
gh pr list --state open --limit 30 --json number,headRefName,mergeable \
  --jq '.[] | select(.headRefName|startswith("issue-")) | "\(.headRefName) \(.mergeable)"'
gh issue list --state open --label "$QUEUE_LABEL" --limit 100 --json number,labels \
  | jq -r --arg w "$WORKING_LABEL" --arg f "$FINISHED_LABEL" \
      '[.[] | select((.labels|map(.name)|index($w))==null
             and (.labels|map(.name)|index($f))==null) | .number] | sort | join(",")'
```

Note the eligibility filter is piped to `jq` with `--arg` rather than using `gh --jq`, so that
overriding `WORKING_LABEL` or `FINISHED_LABEL` actually takes effect. Hardcoding those two inside
a single-quoted jq program silently ignores the override and re-picks claimed issues.

**Compare against the previous pass.** Nothing persists between passes on its own, so record the
observations at the end of every pass and read them back here:

Read it at the start of the pass:

```bash
cat "$LOOP_DIR/last-run.json" 2>/dev/null || echo '{}'
```

and write it at the end, substituting the three values **this pass actually observed** (they are
not shell variables set anywhere — fill them in literally from the STEP -1 output and your STEP 2
conclusion):

```bash
jq -n --arg head "<origin/DEFAULT_BRANCH sha from STEP -1>" \
      --arg eligible "<comma-separated eligible issue numbers, or empty>" \
      --arg conclusion "<one of: claimed | all-blocked | at-capacity | queue-empty>" \
  '{head:$head, eligible:$eligible, conclusion:$conclusion, at:(now|todate)}' \
  > "$LOOP_DIR/last-run.json"
```

The early exit above fires only when `head` and `eligible` both match the stored values **and**
the stored `conclusion` is `all-blocked`. Any other stored conclusion means the previous pass did
real work, so this pass must do a full evaluation rather than trusting the cache.

Exit early, cheaply, when:

- **`AVAILABLE=0`** → reply `At capacity: N agents in flight (ceiling M). No slots free.`, list the
  in-flight issues from `"$SLOTS" list`, and **claim nothing and spawn nothing**.
  **Still run STEP 0 first** — see below.
- The default branch is unchanged **AND** no loop PR is `CONFLICTING` **AND** the eligible set is
  unchanged **AND** `last-run.json` records that the previous pass concluded every eligible issue
  was blocked → reply `No change since last run; queue still blocked.` and STOP. Do not re-read
  issue bodies, do not run `gh pr diff`, do not spawn anything.

> **At capacity still runs STEP 0.** The capacity ceiling governs *claiming new work*, not
> *repairing existing work*. Conflicts are most frequent exactly when agents are saturated and
> pushing, and nothing but this loop repairs its own PRs — skipping STEP 0 while at capacity
> stalls the whole pipeline. Fix conflicts, then exit without claiming.

### STEP 0 — unblock before starting anything new

For every `issue-*` PR reporting `CONFLICTING`, fix it before considering new work. These PRs are
the loop's own output; nothing else will repair them. Conflicts **reappear** every time another PR
merges, so expect to redo this every pass.

```bash
git -C "$REPO" merge-tree --write-tree --name-only "origin/$DEFAULT_BRANCH" "origin/<branch>"
```

In practice the only file that ever conflicts is the shared append-only progress log (e.g.
`PROGRESS.md`), because every agent appends a section. **Resolve by keeping BOTH sides — never
drop another agent's entry.** In that branch's worktree, using **per-issue temp paths** so two
concurrent resolutions cannot clobber each other:

```bash
T="$(mktemp -d "/tmp/gh-issue-loop-<N>.XXXXXX")"
git merge "origin/$DEFAULT_BRANCH" --no-commit
git show :1:PROGRESS.md > "$T/base"
git show :2:PROGRESS.md > "$T/ours"
git show :3:PROGRESS.md > "$T/theirs"
git merge-file --union "$T/ours" "$T/base" "$T/theirs" && cp "$T/ours" PROGRESS.md
git add PROGRESS.md && git commit --no-edit && git push
rm -rf "$T"
```

Merge the default branch **into** the feature branch. Never rebase, never force-push. Before
committing, verify no conflict markers remain **AND** the merged file is **larger than both
inputs** — that is the cheap proof no one's section was dropped.

If a **source file** conflicts (not just the progress log), do **not** guess: report it and leave
that PR alone.

Skip any branch whose sub-agent is still running — check `"$SLOTS" list` for a live lease.

### STEP 1 — find eligible work (read only)

Use the STEP -1 listing. Cross-check it with `gh issue list --state all --limit 200`, because the
label filter truncates. Confirm any issue about to be claimed with `gh api`.

If nothing is eligible, reply exactly `No eligible <QUEUE_LABEL> issues.` and STOP.

**Reclaim worktrees left behind by earlier passes.** STEP 5 removes a worktree once its PR merges,
but a pass killed between the merge and that cleanup leaves one on disk forever, and they are full
checkouts. Sweep them at the start of every pass — this is the self-healing half of the rule, so a
crash costs disk rather than a permanent leak:

```bash
git -C "$REPO" worktree list --porcelain | awk '/^worktree /{print $2}' | while read -r WT; do
  case "$WT" in "$WORKTREE_DIR"/issue-*) ;; *) continue ;; esac      # never touch the main checkout
  N="${WT##*/issue-}"; N="${N%%-*}"
  # First column of `list` is the issue number; a slot still held means a live
  # agent owns this worktree and it must not be touched.
  "$SLOTS" list | awk 'NR>1 && $1 ~ /^[0-9]+$/ {print $1}' | grep -qx "$N" && continue
  git -C "$WT" diff --quiet && git -C "$WT" diff --cached --quiet || continue
  [ -z "$(git -C "$WT" log --branches --not --remotes --oneline)" ] || continue
  gh pr list --state merged --search "issue-$N" --json headRefName \
    --jq 'any(.headRefName == "issue-'"$N"'")' | grep -q true || continue
  git -C "$REPO" worktree remove "$WT" && git -C "$REPO" branch -d "issue-$N" 2>/dev/null || true
done
git -C "$REPO" worktree prune
```

Every `continue` is a refusal to delete: a slot still held, uncommitted or unpushed work, or a
branch with no merged PR all leave the worktree alone. Only a worktree whose work is demonstrably
in the default branch is removed.

### STEP 2 — select up to BATCH_SIZE non-conflicting issues, lowest number first

Build the exclusion set **first**: for every open unmerged PR, list its files with
`gh pr diff <N> --name-only`.

> **IMPORTANT CORRECTION — the original rule was too strict and cost ~10 consecutive no-op runs.**
> It excluded any issue touching *any* file an open PR touched. With 5 PRs open that excluded the
> entire queue. Evidence disproved it: when four PRs were finally merged, every single conflict was
> the shared progress log alone — zero source conflicts.
>
> **The corrected rule:** exclude on **genuine source-file overlap** with an open PR — i.e. the
> issue's scope requires editing a source file that PR actually modifies. Do **not** exclude
> because a PR merely touches a shared append-only file, and **never** exclude on the progress log.

Beyond that, two issues in the same batch conflict when they:

- **touch overlapping FILES** — not merely the same top-level module. Issues in one module coexist
  fine when their file sets are disjoint. If two need the same file in distant regions you may
  still batch them, but assign each agent explicit **function-level ownership**.
- **both change a shared contract**: `proto/`, `gen/`, `schemas/`, DB migrations, or a public API type.
- **one declares a dependency on the other**, or its body says "coordinate with" another issue.
  Read for it — such issues state it explicitly.

An issue may cite a file only as **evidence** rather than as something it must edit. Read the
"How to tackle" section, not just the bullet list, before excluding on that basis.

The shared progress log alone **never** disqualifies a pairing; STEP 0 cleans that up.

If **zero** issues are safe, claim nothing — do not force a batch. Say so plainly and list which
PRs must merge to unblock which issues. Always state which issues were grouped and why, and which
were excluded and why.

Then reserve slots for the survivors:

```bash
"$SLOTS" verify || { echo "FATAL: slot registry does not record what it reports"; exit 1; }
"$SLOTS" reserve <candidates in ascending order>
```

`verify` round-trips a reservation and checks the count actually moved. It runs **before any label
is touched**, because the `[ -x ... ]` guard in the preamble only proves the helper *exists*. A
helper that exists and lies is worse than a missing one: it passes that guard, the pass claims the
issue, and the claim is then stranded under `WORKING_LABEL` with no work done and no slot recorded.
That is not hypothetical — a stub helper did exactly this to #219, #226 and #195.

Proceed with `RESERVED` only. Report `SKIPPED_NO_CAPACITY` as *at capacity*, never as *blocked*.

### STEP 3 — claim, in this exact order, for each reserved issue N

```bash
gh issue edit <N> --add-label "$WORKING_LABEL"                       # a) claim FIRST
git -C "$REPO" fetch origin                                           # b)
                                                                      # c) resume, else start fresh
git -C "$REPO" worktree add "$WORKTREE_DIR/issue-<N>" "issue-<N>" \
  || git -C "$REPO" worktree add "$WORKTREE_DIR/issue-<N>" -b "issue-<N>" "origin/issue-<N>" \
  || git -C "$REPO" worktree add "$WORKTREE_DIR/issue-<N>" -b "issue-<N>" "origin/$DEFAULT_BRANCH"
git -C "$WORKTREE_DIR/issue-<N>" push -u origin "issue-<N>"           # d) push IMMEDIATELY
"$SLOTS" bind <N> "issue-<N>" "$WORKTREE_DIR/issue-<N>"               # e) record the real paths
gh issue comment <N> --body "<see the comment vocabulary below>"      # f)
```

A new branch always starts from `origin/$DEFAULT_BRANCH`, **never local HEAD**.

**An issue keeps one branch, `issue-<N>`, for its whole life.** When that branch already exists —
locally or on origin — resume it rather than opening `issue-<N>-retry` or `issue-<N>-b`. A killed
agent leaves its work committed there, and a suffixed branch abandons it: #193 accumulated
`issue-193`, `issue-193-retry` and `issue-193-b` for one issue, three passes each starting over
from the default branch while the previous attempt's commits sat unread on the branch beside them.
A suffix is only correct when the existing worktree is genuinely in use by a live agent, and in
that case this pass should not have been reserved a slot at all.

When a suffix is unavoidable the `bind` call is **mandatory**, because a lease still pointing at
the default path would let the reaper mistake a live agent for a dead one and hand its slot away.

The ordering matters: the slot reservation is the local mutex, the label is the global claim, so
label before worktree means an overlapping fire cannot double-pick; push before the comment means
the branch link in the comment is never dead.

**Comment vocabulary.** The comment is the only record of what happened, so it must say which of
these it is. Post exactly one per pass, per issue.

| Situation | Comment |
| :--- | :--- |
| Branch created | ``Claiming — branch `issue-<N>`: <url>`` |
| Branch already had commits | ``Resuming `issue-<N>` at `<sha>` (<n> commits from a prior attempt): <url>`` |
| Attempt ended without a merge | ``Failed: <one line of what broke>. Branch left at `<sha>`. Keeps `ai-working`; needs a human.`` |

Never post on an issue that already carries `FINISHED_LABEL` — check immediately before commenting,
not only during selection. #193 was merged and closed at 14:13 and picked up again at 14:15 by a
pass that had decided its candidates before the merge landed.

Twelve near-identical "Working on it" comments is what the old wording produced on #193, and none of
them recorded that an earlier attempt had been killed with work still on the branch. A resume that
does not say what it is resuming from is indistinguishable from a restart.

### STEP 4 — one sub-agent per reserved issue

Launch them **all in a single message** so they run concurrently (`general-purpose`, foreground).
Each brief must contain every point below.

**Isolation**
- Work ONLY inside `$WORKTREE_DIR/issue-<N>` (absolute paths, or `git -C <that path>`).
  NEVER modify the main checkout. NEVER touch a sibling worktree — other agents are working there.
- The branch `issue-<N>` already exists on the remote and is checked out in that worktree.

**Ownership**
- State explicitly which files this agent owns; which files sibling agents own (off-limits); and
  which files open unmerged PRs own (off-limits, naming the PR for each). If the fix genuinely
  requires editing a file it does not own, STOP and report rather than editing.
- The human merges PRs frequently, so an ownership constraint can go stale mid-task. Before
  treating a file as off-limits, re-check whether the PR that owned it has merged, and say so if
  acting on that.

**Context to read**
- `AGENTS.md` (especially "Definition of done"), `PROJECT.md`, `PROGRESS.md`, `README.md` and the
  `Makefile` in that worktree; any review document the issue cites a finding ID from; and the full
  issue text and comments via `gh api "repos/$OWNER/$NAME/issues/<N>"`.

**Specification discipline**
- Treat the issue body as the specification — it cites concrete files, scope, explicit
  out-of-scope, and acceptance criteria. Verify its claims against the real code; line numbers
  drift. If the issue is marked *(verify)*, **reproduce the defect with a failing test before
  fixing it.**
- Implement completely: every acceptance criterion and every definition-of-done item — behavior
  wired end to end, error cases handled, tests added/updated and passing, format/lint/type checks
  passing, configuration updated, docs updated, no temporary debug code, no secrets or local
  artifacts committed, progress log updated with evidence and the exact commands run. Respect
  stated out-of-scope.
- Append the progress-log entry as a **new self-contained section at the END**. Never restructure
  the file or touch another agent's section.
- Use the Makefile targets for build/test/lint.
- For UI work, follow the repo's approved design package if one exists; implement from its task
  list, treat its specification as the contract (spec wins over mockup on conflict), preserve the
  visual identity, do **not** redesign.

**Concurrency hazard**
- Any per-repo shared temp path is shared across ALL worktrees, and some targets delete it. Pass a
  per-issue value, e.g. `make typecheck MYPY_CACHE=/tmp/<repo>-mypy-cache-issue-<N>`. Bind any
  throwaway container to a unique port and remove it when done. Generalize this: **namespace every
  shared temp path per issue.**
- Heartbeat the lease at each major step so a long task is not mistaken for a dead one. If it
  replies `NOSLOT`, the lease was reaped — run `reacquire` before continuing so the work stays
  counted against the ceiling.
  > **Interpolate the helper's absolute path into the brief — never write `"$SLOTS"` there.** A
  > sub-agent is a separate agent with its own shell and has never sourced `env.sh`, so `$SLOTS`
  > expands to empty and the heartbeat silently fails with `command not found`. The lease then goes
  > quiet, gets reaped at `REAP_AFTER_MIN`, and a live agent's slot is handed to someone else.
  > Write the resolved path literally, e.g.:
  > ```
  > /abs/repo/.claude/skills/gh-issue-loop/bin/loop-slots.sh touch 123
  > /abs/repo/.claude/skills/gh-issue-loop/bin/loop-slots.sh reacquire 123
  > ```

**Adversarial self-review — load-bearing, not ceremony**
- Before finishing, spawn a short **adversarial review of your own diff**. In the origin session
  this caught: an agent reintroducing the exact bug it was fixing; an incomplete fix already
  reported as done; a newly-introduced race plus two false claims in the agent's own docs; a
  budget-accounting error that made review-approved runs end `blocked` with no PR; an unfenced
  lease; and a payload bound measured on raw instead of JSON-encoded bytes. **Fix what it finds
  before claiming done.**

**Landing the change**
- Commit with a clear message. **NEVER add a `Co-Authored-By` trailer or any co-author.**
- Push the branch and open a PR against the default branch whose body contains `Closes #<N>`.
- Do NOT push to the default branch directly. Do NOT close the issue by hand.
- **Drive the PR to merge — see §7. The agent owns its PR through to merge.**
- If the default branch moves under it and the branch goes `CONFLICTING`, merge `origin/<default>`
  in (never rebase, never force-push) and keep every progress-log section from both sides.

**Finishing — the agent does this itself**
- **On success:** post the audit-trail summary comment on the issue (§7 lists what it must
  contain, including the merge state and merged SHA), then add `FINISHED_LABEL`. Add that label
  **only** if the PR genuinely merged and every acceptance criterion is genuinely met.
- **On failure:** do **NOT** add `FINISHED_LABEL`, and do **NOT** remove `WORKING_LABEL`. Post a
  comment on the issue stating the exact blocker, and say clearly in the returned report that the
  issue **failed and needs a human**. A vague report with no comment is the worst outcome — be
  explicit about what was tried and where it stopped.

**Reporting**
- Return: the PR URL, what changed, each acceptance criterion with a met/not-met verdict and how it
  was verified, the exact validation commands with real pass/fail results, and the final merge
  state with the merged SHA.
- Report failures honestly — never claim a check passed if it did not, never claim a command was
  run that was skipped, and distinguish "CI passed" from "CI still pending". **If a PR has zero
  checks, suspect the branch is `CONFLICTING`** rather than assuming Actions is broken.

### STEP 5 — independently verify before reporting

**Never relay an agent's self-report as fact.** Check ground truth yourself:

```bash
gh api "repos/$OWNER/$NAME/issues/<N>" --jq '[.labels[].name]'
gh pr view <PR> --json state,mergedAt,mergeCommit,mergeable,statusCheckRollup
gh pr checks <PR>
```

Then, for every issue whose PR you just verified **merged**, remove its worktree before releasing
the slot:

```bash
# Only for an issue whose PR is confirmed merged by the check above.
WT="$WORKTREE_DIR/issue-<N>"
if [ -d "$WT" ]; then
  # Refuse to discard work that only exists on disk. A merged PR normally means
  # everything is pushed, but an agent killed after the merge can still leave a
  # stray edit, and `--force` would take it with the directory.
  if [ -n "$(git -C "$WT" status --porcelain)" ] \
     || [ -n "$(git -C "$WT" log --branches --not --remotes --oneline)" ]; then
    echo "SKIP issue-<N>: uncommitted or unpushed work; leaving the worktree for a human"
  else
    git -C "$REPO" worktree remove "$WT" && git -C "$REPO" branch -d "issue-<N>" 2>/dev/null || true
  fi
fi
git -C "$REPO" worktree prune
```

The worktree is disposable once the branch has merged — the merge commit is the durable artefact,
and a full checkout per completed issue is what turns a few dozen passes into tens of gigabytes and
a `git worktree list` nobody can read. Note this is `worktree remove` **without** `--force` plus an
explicit dirty check, and `branch -d` **not** `-D`: both refuse rather than discard if the state is
not what merge-is-done implies, and the guard above reports that case instead of hiding it.

Leave the worktree in place for anything **not** verified merged. §8 covers the failure path, where
the branch is the handover and removing the worktree can destroy the only copy of the work.

Then release the slots for every issue verified complete or verified failed:

```bash
"$SLOTS" release <N> [N...]
```

`NOT-HELD` in that output means the lease was already reaped — the agent had been running longer
than `REAP_AFTER_MIN` without writing, so treat it as an infrastructure kill and check §8.

Record the pass in `last-run.json` (see STEP -1), then report per issue: number and title, branch,
PR URL, what was implemented, true CI status, whether `FINISHED_LABEL` was applied, and **whether
it merged** (with the merge SHA). Report what STEP 0 fixed. Finally state the in-flight count, the
capacity, and which issues remain eligible.

---

## 7. MERGE IS DONE

> **Supersedes the older rule.** An earlier version of this procedure said *"open the PR but do NOT
> merge it."* **That rule is withdrawn and must not be followed.** Opening a PR is no longer done.
> The only surviving prohibitions are: never push to the default branch directly, never force-push,
> never rebase a shared branch, and never close the issue by hand.

A task is complete only when its PR is **merged**.

- The `FINISHED_LABEL` summary comment must explicitly record the PR's **merge state** — merged or
  not merged, and why.
- If the PR is not merged, drive it to merge:
  - **CI failing** → fix it. Push fixes to the branch and re-check.
  - **Merge conflict** → resolve it using the STEP 0 union recipe for the shared progress log, or,
    for a source conflict, resolve it properly rather than guessing. If it genuinely cannot be
    resolved safely, stop and escalate.
  - **Otherwise merge it**, then confirm it actually merged rather than assuming the command
    succeeded:
    ```bash
    gh pr merge <PR> --squash
    gh pr view <PR> --json state,mergedAt,mergeCommit
    ```
- **Document everything in the issue.** Every merge attempt, every CI failure and the fix applied,
  every conflict and how it was resolved, and the final merged SHA. The issue must be a complete
  audit trail, not just a "done" stamp.
- Add `FINISHED_LABEL` only when the PR is genuinely merged and every acceptance criterion is
  genuinely met.

**Ordering hazard:** because `Closes #<N>` auto-closes the issue on merge, a merged PR closes its
own issue. Post the audit-trail comment **before, or immediately after, merging — and verify the
comment actually landed.** Commenting on a freshly-closed issue still works, but a comment lost to
a failed call is invisible; re-read the issue to confirm.

---

## 8. FAILURE HANDLING

For any sub-agent that cannot finish:

1. **Leave `WORKING_LABEL` in place** so the loop does not retry it every interval.
2. Do **not** add `FINISHED_LABEL`.
3. Post the `Failed:` comment from the STEP 3 vocabulary — the blocker in one line, and the SHA the
   branch was left at. That comment is what the next attempt reads to orient itself, so "failed" on
   its own is not enough: it has to say what broke and where the work stopped.
4. **Commit and push whatever is on the branch first**, then remove the worktree:
   `git worktree remove --force "$WORKTREE_DIR/issue-<N>"`. The worktree is disposable; the branch
   is the handover. Removing a worktree with uncommitted work in it destroys the only thing a
   resume could have started from.
5. Release its slot: `"$SLOTS" release <N>`.
6. Say clearly in the reply that the issue is **blocked and needs a human**.

**One agent failing must not stop the others being reported.**

### Exception — killed by infrastructure, not failed

Learned the hard way: agents were killed mid-flight by a session/usage limit with ~80% of real work
already on disk. **Do NOT apply the "remove the worktree" rule in that case — it destroys real work.**

Instead:

```bash
git -C "$WORKTREE_DIR/issue-<N>" add -A
git -C "$WORKTREE_DIR/issue-<N>" commit -m "WIP: interrupted mid-implementation"
git -C "$WORKTREE_DIR/issue-<N>" push
```

Then resume the agent later (e.g. via `SendMessage` to the same agent id) once the limit resets.

**The slot needs deliberate handling here.** While the agent is suspended it writes nothing, so its
lease is reaped after `REAP_AFTER_MIN` and the slot correctly returns to the pool. But that means a
resumed agent would otherwise run **uncounted**, pushing real concurrency past the ceiling. So on
resume, the first thing the agent must do is:

```bash
"$SLOTS" reacquire <N>
```

which re-takes a counted slot, or prints `DENIED <N> at-capacity` — in which case wait for a slot
rather than proceeding.

**Distinguish "the agent failed" from "the agent was killed by infrastructure" before choosing.**

---

## 9. ARMING, CHECKING, AND STOPPING

### STOPPING THE LOOP — one step

**Claude Code:** run `CronList`, find the job whose prompt mentions `gh-issue-loop`, and call
`CronDelete` with its id. As a single instruction to paste:

> Run CronList, then CronDelete the job whose prompt mentions gh-issue-loop.

Or invoke `/gh-issue-loop stop`, which does exactly that. To kill *all* scheduling immediately,
including any job you cannot find, set `CLAUDE_CODE_DISABLE_CRON=1` in the environment and restart.

**opencode / system cron:** `crontab -e` and delete the `gh-issue-loop` line
(or `systemctl --user disable --now gh-issue-loop.timer`).

Stopping the schedule does **not** interrupt agents already running. To also drain the loop, wait
for in-flight agents or clear their leases with `"$SLOTS" release <N>`.

### Arming — Claude Code

`arm` mode registers the schedule with `CronCreate` and then runs one pass. The scheduled prompt
must be a **self-contained instruction to read this file**, not a slash command:

```
Read <ABS PATH TO THIS SKILL.md> in full and execute it in `run` mode
(one pass, STEP -1 through STEP 5). Parameters: QUEUE_LABEL=ai-doable
WORKING_LABEL=ai-working FINISHED_LABEL=ai-finished BATCH_SIZE=5 MAX_AGENTS=5.
Repository: <ABS REPO PATH>.
```

**Why a file-read prompt and not a slash command:** this skill sets
`disable-model-invocation: true`, and since Claude Code v2.1.196 a scheduled fire only runs skills
Claude is allowed to invoke on its own — skills with that flag **reach Claude as plain text
instead of executing**. That applies equally to `CronCreate` with a `/gh-issue-loop` prompt and to
`/loop 5m /gh-issue-loop run`: both would deliver inert text every interval and silently never run
the loop. A file-read prompt sidesteps it entirely and keeps one source of truth.

Do **not** "fix" this by removing the flag — the flag is what stops Claude from spontaneously
arming an autonomous loop that merges PRs.

Real constraints, taken from the current scheduled-tasks documentation:

- Tasks are **session-scoped**: they live in the current conversation and stop when you start a new
  one. Resuming with `claude --resume` or `--continue` restores tasks that have not expired.
  Claude Code stores the task list in the project's `.claude` directory (v2.1.216+), and scheduling
  **fails with an error if that directory or the task file inside it is a symlink** — so do not
  symlink `.claude` itself.
- Recurring tasks **auto-expire 7 days** after creation; they fire one final time, then delete.
- Tasks fire **only while Claude Code is running and idle**, between turns, never mid-response.
  There is **no catch-up** for missed fires — a long pass simply delays the next one, which is
  exactly why the concurrency ceiling exists rather than relying on timing.
- **Jitter:** recurring tasks fire up to 30 minutes late, *or up to half the interval for tasks
  more frequent than hourly*. For `*/5 * * * *` that is up to ~2.5 minutes.
- A session holds at most 50 scheduled tasks.

### Arming — opencode

**opencode has no built-in scheduler.** Stated plainly rather than invented: the published config
schema at `https://opencode.ai/config.json` contains no cron, schedule, timer, or interval key, and
`opencode --help` exposes no scheduling subcommand. Use the system scheduler against opencode's
headless mode:

```cron
*/5 * * * * cd /abs/repo && "$HOME/.opencode/bin/opencode" run --command gh-issue-loop run >> /tmp/gh-issue-loop.log 2>&1
```

Use `$HOME` (or a fully absolute path) rather than `$USER` — cron guarantees `HOME`, `LOGNAME`,
`SHELL`, and `PATH`, but **not** `USER`, so `/home/$USER/...` commonly expands to `/home//...`.

`opencode run --command <name> <args>` invokes a custom command headlessly. Equivalently, pass the
same self-contained file-read prompt as a positional message:

```bash
opencode run "Read /abs/repo/.claude/skills/gh-issue-loop/SKILL.md in full and execute it in run mode."
```

**Unattended permissions:** a headless run has no TTY to answer a permission prompt. Either
pre-approve the tools this loop needs in `opencode.json` under `permission`, or pass `--auto` to
auto-approve everything not explicitly denied. Prefer an explicit `permission` allowlist — `--auto`
on a loop that merges PRs grants very broad authority.

Because the ceiling lives on disk rather than in session memory, overlapping system-cron fires are
bounded exactly as they are under Claude Code.

### Checking

```bash
"$SLOTS" list     # who is in flight, how long, liveness, and remaining capacity
```

Plus `CronList` (Claude Code) or `crontab -l` (system cron) to confirm the schedule still exists.
Or invoke `/gh-issue-loop status`, which claims and spawns nothing.

---

## 10. OPERATING THE QUEUE

- **Curate by hand.** Add `QUEUE_LABEL` to issues you want worked. The loop treats the current
  contents as authoritative and never re-adds the label to an issue you removed it from.
- **Write issues as specifications.** The agents treat the body as the contract: concrete files,
  scope, explicit out-of-scope, and acceptance criteria. Mark unconfirmed defects *(verify)* so the
  agent reproduces them with a failing test first.
- **Recovering a stuck issue.** `WORKING_LABEL` is never removed automatically — that is
  deliberate, and it means a human must clear it. **First confirm no PR already merged for it**
  (§4), otherwise re-queuing makes the loop redo merged work. Then:
  ```bash
  gh issue edit <N> --remove-label "$WORKING_LABEL"
  git -C "$REPO" worktree remove --force "$WORKTREE_DIR/issue-<N>"   # if it still exists
  "$SLOTS" release <N>
  ```
- **Reading a run report.** Every pass reports the in-flight count and capacity. Check that number
  first: `AVAILABLE=0` with work waiting is *healthy saturation*, not a stall. Only when
  `AVAILABLE>0` and nothing is being picked up is the queue genuinely blocked — and then the report
  names which PRs must merge to unblock which issues.

---

## PROVENANCE

Distilled from roughly nine hours of continuous autonomous runs over several dozen issues on
`alexandre-leites/moirai`. Every trap in sections 3, 6, and 8 was discovered the hard way during
those runs. The procedure itself is repo-agnostic: it derives the repository root, owner, name, and
default branch at runtime.
