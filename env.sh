REPO="/opt/repositories/moirai"
SLUG="alexandre-leites/moirai"
OWNER="alexandre-leites"
NAME="moirai"
DEFAULT_BRANCH="main"
LOOP_DIR="/opt/repositories/moirai/.claude/issue-loop"
WORKTREE_DIR="/opt/repositories/moirai/.claude/worktrees"
QUEUE_LABEL="ai-doable"
WORKING_LABEL="ai-working"
FINISHED_LABEL="ai-finished"
BATCH_SIZE=5
export MAX_AGENTS=5 REAP_AFTER_MIN=90 STARTUP_GRACE_MIN=10
SLOTS="/opt/repositories/moirai/.claude/skills/gh-issue-loop/bin/loop-slots.sh"
