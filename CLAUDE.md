# CLAUDE.md

Guidelines for AI coding agents working in this repository.

## Rule 0

If Goku gives a direct instruction, follow it. Project-local guidance exists to encode standing preferences, not to overrule the user.

## Integrating with Beads (dependency-aware task planning)

Beads provides a lightweight, dependency-aware issue database and a CLI (`br`) for selecting "ready work," setting priorities, and tracking status.

**Important:** `br` is non-invasive—it NEVER executes git commands. After `br sync --flush-only`, you must manually run `git add .beads/ && git commit`.

### Workflow Pattern

1. **Start**: Run `br ready` to find actionable work
2. **Claim**: Use `br update <id> --status=in_progress`
3. **Work**: Implement the task
4. **Complete**: Use `br close <id>`
5. **Sync**: Run `br sync --flush-only` then manually commit

### Key Concepts

- **Dependencies**: Issues can block other issues. `br ready` shows only unblocked work.
- **Priority**: P0=critical, P1=high, P2=medium, P3=low, P4=backlog (use numbers, not words)
- **Types**: task, bug, feature, epic, question, docs
- **Blocking**: `br dep add <issue> <depends-on>` to add dependencies

### Commands

```bash
# Essentials
br create "Title" --description "..."      # Create an issue
br update <id> --acceptance-criteria "..." # Add or update acceptance criteria
br update <id> --claim --json              # Claim work
br comments add <id> --message "..."       # Add comments
br close <id> --reason "..."               # Complete
br reopen <id>                             # Reopen if needed

# Querying (agents should always use --json)
br ready --json                            # Actionable work (not blocked)
br list --json                             # All issues
br blocked --json                          # What's blocked
br search "keyword"                        # Full-text search
br show <id> --json                        # Issue details (array)

# Dependencies
br dep add <child> <parent>                # Child depends on parent
br dep cycles                              # MUST be empty!
br dep tree <id>                           # Visualize dependencies

# Sync
br sync --flush-only                       # DB → JSONL
br sync --import-only                      # JSONL → DB

# Health
br doctor                                  # Health check
```

### Stale Claims and Reclaiming Abandoned Work

`br ready` excludes `in_progress` beads, so a crashed or abandoned session can
hide work indefinitely. Do not treat every old claim as free work. Reclaim only
after you have evidence from the bead metadata and coordination trail.

Use this rule of thumb:

- Agent swarm claim: stale candidate after two hours without an `updated_at`
  change, unless the human operator explicitly says the pane/session is dead.
- Human or unclear claim: stale candidate after one business day.
- Any claim with live agent reservations, recent comments, or visible dirty
  work in the same files is not abandoned.

### Best Practices

- Use `--json` to get structured output for parsing
- Check `br ready` at session start to find available work
- Update status as you work (in_progress → closed)
- Create detailed issues with `br create` when you discover tasks
- Use descriptive titles and appropriate priority/type/labels
- Always `br sync --flush-only && git add .beads/` before ending session

### Session Protocol

**Before ending any session, run this checklist:**

```bash
git status              # Check what changed
git add <files>         # Stage code changes
br sync --flush-only    # Export beads to JSONL
git add .beads/         # Stage beads changes
git commit -m "..."     # Commit everything together
git push                # Push to remote
```

## UBS — Ultimate Bug Scanner

**Golden Rule:** `ubs <changed-files>` before every commit. Exit 0 = safe. Exit >0 = fix & re-run.

### Commands

```bash
ubs file1 file2                         # Specific files (< 1s) — USE THIS
ubs $(git diff --name-only --cached)    # Staged files — before commit
ubs --ci --fail-on-warning .            # CI mode — before PR
ubs .                                   # Whole project (ignores target/, Cargo.lock)
```

### Output Format

```
⚠️  Category (N errors)
    file.ts:42:5 – Issue description
    💡 Suggested fix
Exit code: 1
```

Parse: `file:line:col` → location | 💡 → how to fix | Exit 0/1 → pass/fail

### Fix Workflow

1. Read finding → category + fix suggestion
2. Navigate `file:line:col` → view context
3. Verify real issue (not false positive)
4. Fix root cause (not symptom)
5. Re-run `ubs <file>` → exit 0
6. Commit

### Bug Severity

- **Critical (always fix):** Memory safety, use-after-free, data races, SQL injection
- **Important (production):** Unwrap panics, resource leaks, overflow checks
- **Contextual (judgment):** TODO/FIXME, println! debugging

## Code Editing Discipline

Work happens on `main`. Do not create feature branches unless the user explicitly asks for one.

### No Script-Based Changes

**NEVER** run a script that processes/changes code files in this repo. Brittle regex-based transformations create far more problems than they solve.

- **Always make code changes manually**, even when there are many instances
- For many simple changes: use parallel subagents
- For subtle/complex changes: do them methodically yourself

## Cross-session Messaging

Coordinate with the `SendMessage` and `ListAgents` tools in multi-agent sessions. Treat unrecognized working-tree changes as peer work.
Do not revert or overwrite them.

## Landing the Plane (Session Completion)

**When ending a work session**, you MUST complete ALL steps below.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Update issue status** - Close finished work, update in-progress items
3. **Sync beads** - `br sync --flush-only` to export to JSONL
4. **Commit and push** - `git add .beads/` alongside your code changes, then commit and push. The flush alone changes nothing for anyone else: `issues.jsonl` is tracked, and leaving it uncommitted is what leaves every other box stale. Full sequence in § Session Protocol.

## Note on Built-in TODO Functionality

Also, if I ask you to explicitly use your built-in TODO functionality, don't complain about this and say you need to use beads. You can use built-in TODOs if I tell you specifically to do so. Always comply with such orders.
