# gh-issue-loop — packaging notes

An autonomous GitHub issue-working loop, packaged as one instruction body with entry points for
both **Claude Code** and **opencode**.

## Files

| Path | Purpose |
| :--- | :--- |
| `.claude/skills/gh-issue-loop/SKILL.md` | **The single instruction body.** The complete operating procedure. Everything else points at this. |
| `.claude/skills/gh-issue-loop/bin/loop-slots.sh` | Crash-safe slot accounting: reap, count, reserve, release, heartbeat. |
| `.claude/skills/gh-issue-loop/README.md` | This file. Not loaded into any model's context. |
| `.opencode/command/gh-issue-loop.md` | **Symlink** → `../../.claude/skills/gh-issue-loop/SKILL.md`. |

## Invocation

| Tool | Command |
| :--- | :--- |
| Claude Code | `/gh-issue-loop arm` · `/gh-issue-loop run` · `/gh-issue-loop status` · `/gh-issue-loop stop` |
| opencode (TUI) | `/gh-issue-loop arm` · `/gh-issue-loop run` · `/gh-issue-loop status` · `/gh-issue-loop stop` |
| opencode (headless) | `opencode run --command gh-issue-loop run` |

## STOPPING THE LOOP

**Claude Code** — paste this:

> Run CronList, then CronDelete the job whose prompt mentions gh-issue-loop.

**System cron (opencode)** — `crontab -e` and delete the `gh-issue-loop` line.

Stopping the schedule does not interrupt agents already running. To drain them too, wait, or clear
their leases with `bin/loop-slots.sh release <N>`.

## Why a symlink, and why one file

The requirement was one shared body, never two divergent copies. A symlink turned out to be
genuinely viable rather than a compromise, because both tools tolerate the other's frontmatter:

- **Claude Code** reads `.claude/skills/<dir>/SKILL.md` and derives the slash command from the
  **directory name** → `/gh-issue-loop`.
- **opencode** reads `.opencode/command/<file>.md`, **follows symlinks**, derives the command from
  the **file name** → `/gh-issue-loop`, takes `description` from frontmatter, and uses the entire
  markdown body as the command template.
- opencode's command loader accepts only `description`, `agent`, `model`, `variant`, `subtask`.
  The Claude-only keys (`name`, `argument-hint`, `allowed-tools`, `disallowed-tools`,
  `disable-model-invocation`) are **silently dropped rather than rejected** — verified empirically,
  not assumed.
- `$ARGUMENTS` is spelled identically in both tools, so the mode/parameter parsing in the body
  works unchanged either way.

Net result: **one file, two slash commands, zero duplication.** Editing `SKILL.md` updates both.

As a bonus, opencode *also* auto-loads `.claude/skills/**/SKILL.md` as a native skill, so the same
file is simultaneously an opencode skill. Caveat: opencode does not honour
`disable-model-invocation`, so its model *can* auto-invoke the skill. The `description` is
therefore written with a `Use ONLY when...` gate to discourage that.

### Verification performed

Nothing here is from memory. Against Claude Code 2.1.220 and opencode 1.18.3:

- Every frontmatter key checked against the current published field reference; all six used are valid.
- `opencode debug config` confirms `/gh-issue-loop` registers from the symlink with the full body
  as its template, frontmatter stripped and `$ARGUMENTS` intact.
- `opencode debug skill` confirms the same file also loads as an opencode skill.
- `opencode`'s published config schema confirms **no** cron/schedule/timer/interval key exists,
  which is why the opencode arming path documents system cron instead of inventing a feature.
- `loop-slots.sh` tested for ceiling arithmetic, both reap paths, the startup-grace window,
  `.git`-churn exclusion, suffixed (`issue-N-b`) worktree resolution, corrupt and truncated leases,
  non-integer and path-traversal arguments, registry identity when run from inside a worktree, an
  8-way concurrent reserve race (exactly 5 of 40 candidates reserved), and flock recovery after
  `SIGKILL`.
- The `env.sh` bootstrap was executed and re-sourced from a fresh shell to confirm the procedure
  survives Claude Code's per-call shell isolation.
- Every `jq` snippet in the skill was run against fixture data — which caught a malformed
  interpolation and a missing `\b` that made issue #42 match a PR closing #420.
- `git check-ignore` confirms the 30+ live worktrees stay ignored while the skill stages cleanly,
  and a throwaway index confirms git stores the symlink as mode `120000` and the helper as `100755`.

### Two traps worth knowing

**1. The flag that would silently break the schedule.** `disable-model-invocation: true` is set
deliberately — this skill merges PRs, and Claude must never decide to arm it on its own. But since
Claude Code v2.1.196 a scheduled fire only runs skills Claude may invoke itself; skills with that
flag **reach Claude as inert plain text**. So `CronCreate` with a `/gh-issue-loop` prompt, and
`/loop 5m /gh-issue-loop run`, would both appear to work and never actually run. The arming
procedure therefore schedules a *self-contained file-read prompt*. Do not "fix" this by removing
the flag.

**2. Do not symlink `.claude` itself.** Claude Code stores its scheduled-task list in the project's
`.claude` directory, and scheduling **fails with an error** if that directory or the task file
inside it is a symlink. Symlinking a skill *directory underneath* it is fine and supported.

## Placement: project vs global

This lives in the **project** `.claude/`, and `.gitignore` was updated so it is committable while
local runtime state stays ignored:

```gitignore
.claude/*
!.claude/skills/
.opencode/*
!.opencode/command/
```

`.claude/*` uses a single star, not `.claude/**` — the single star lets git descend into the
re-included subdirectory; the double star would not.

Project placement was chosen because:

1. The loop's durable state (`.claude/worktrees/`, `.claude/issue-loop/`) is already per-repository,
   so the mechanism and its state stay together.
2. Committing it versions the procedure alongside the code it operates on, and makes it reviewable.
3. Claude Code cloud sessions and scheduled routines **do not** read `~/.claude/skills/` from a
   local machine, but they do load project skills committed to the repository.
4. Both tools read project scope: Claude Code from `.claude/skills/`, opencode from
   `.opencode/command/` and `.claude/skills/`.

To also use it from any other repository, symlink it into personal scope — the body is
repo-agnostic and derives the repo root, owner, name, and default branch at runtime:

```bash
mkdir -p ~/.claude/skills
ln -s /home/william/Workspace/Personal/moirai/.claude/skills/gh-issue-loop \
      ~/.claude/skills/gh-issue-loop
```

Claude Code follows a symlinked skill directory and loads `SKILL.md` from the target, and if the
same target is reachable from two locations it loads the skill once.

For opencode's global scope, link **both** the command and the skill directory. The command alone
is not enough: it does not carry `bin/`, so the slot helper would be unreachable and the loop would
refuse to run.

```bash
mkdir -p ~/.config/opencode/command ~/.config/opencode/skills
ln -s /home/william/Workspace/Personal/moirai/.claude/skills/gh-issue-loop/SKILL.md \
      ~/.config/opencode/command/gh-issue-loop.md
ln -s /home/william/Workspace/Personal/moirai/.claude/skills/gh-issue-loop \
      ~/.config/opencode/skills/gh-issue-loop
```

The helper is looked up in three places in order — the repo copy, `~/.claude/skills/...`, then
`~/.config/opencode/skills/...` — so either personal install makes it resolvable from any repo.

**Platform note:** `loop-slots.sh` requires GNU coreutils/findutils and util-linux `flock`. It
checks for them at startup and exits with a clear error rather than miscounting. It is Linux-only
as written; macOS would need `gstat`/`gfind` or equivalents.

## Operating the loop

See **SKILL.md § 9 (Arming, checking, stopping)** and **§ 10 (Operating the queue)** for the full
human-facing guide: curating the queue, recovering a stuck issue, and reading a run report.

The one distinction worth repeating here, because conflating it was a real failure:

- **At capacity** — `AVAILABLE=0`, agents are working. Healthy saturation. Remedy: *nothing, wait.*
- **Queue blocked** — `AVAILABLE>0` but every eligible issue overlaps an open PR. Remedy: *merge the
  named PRs.*

Every pass reports the in-flight count and capacity so these are never confused.
