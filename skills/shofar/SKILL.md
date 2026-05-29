---
name: shofar
description: >-
  Check whether the machine has enough RAM headroom before spawning another dev
  worktree or dev server, inspect memory pressure, and clean up safe-to-kill
  stale dev processes (orphaned dev servers, idle agent CLIs) before the machine
  runs out of memory. Use BEFORE creating a new git worktree or starting a dev
  server, when deciding how much parallel work the machine can take, or when the
  machine is slow / low on memory.
license: MIT
---

# shofar

`shofar` is a macOS CLI that turns raw memory state into decisions. Unlike
display-only monitors (htop, btop, Activity Monitor), it answers two actionable
questions an agent actually has:

1. **Can this machine take on more work right now?** (`shofar capacity`)
2. **What's safe to kill to reclaim memory?** (`shofar clean`)

All commands accept `--json` and return stable, machine-readable output. Read
commands never modify anything; `clean` only kills with an explicit `--kill`.

## Invocation

When invoked with no specific request (e.g. a bare `/shofar`), run
`shofar status` and summarize the result: current memory, whether there's
capacity for more work, and any safe-to-kill processes (without killing). Then
offer the obvious next step (`capacity`, `clean`, or `cleanup on/off`). When the
user's request maps to a specific command, run that instead.

## When to use this skill

- **Before spawning a new worktree or dev server** — gate on capacity so you
  don't push the machine into swap/OOM.
- **When the user reports the machine is slow or low on memory** — inspect with
  `status`, then propose `clean`.
- **When deciding how many parallel tasks/worktrees to run** — `room_for_n`
  tells you the budget.

## Prerequisite

The `shofar` binary must be on `PATH`. If `shofar version` fails, install it:

```sh
brew install astaub/tap/shofar      # or
go install github.com/astaub/shofar@latest
```

## Capacity gate (the primary agent workflow)

Before creating a worktree or starting a dev server, run:

```sh
shofar capacity --json
```

```json
{
  "ok": true,
  "pressure": "normal",
  "room_for_n": 3,
  "usable_headroom_bytes": 5950472192,
  "per_worktree_budget_bytes": 1572864000,
  "budget_source": "measured",
  "reason": "headroom for at least one more worktree at the current per-worktree budget"
}
```

Decision rule:

- **`ok: true`** → safe to proceed. `room_for_n` is how many more worktrees fit
  at the current per-worktree budget.
- **`ok: false`** → do NOT spawn more. Read `reason`. If `pressure` is
  `warning`/`critical`, free memory first (see cleanup below) and re-check.

`budget_source` is `measured` when shofar learned the per-worktree cost from
live worktrees, or `default` when it had nothing to measure (treat `default`
verdicts as rougher estimates).

## Inspect memory + worktrees

```sh
shofar status --json
```

Returns the memory snapshot, per-worktree RSS, and a count of safe-to-kill
processes. Use this to explain *why* capacity is low (e.g. which worktrees are
heavy).

## Cleanup (reclaim memory safely)

Always review before killing. The default is a dry run:

```sh
shofar clean --json          # dry run: lists candidates + reasons
shofar clean --kill --json   # SIGTERM the candidates
```

`clean` is conservative and will **never** kill:

- a process in its own ancestor chain (so calling it from inside your own
  agent session cannot kill that session);
- a process in an **active** worktree (running dev server or recent edits);
- a session younger than the configured minimum age;
- anything matching a configured `protect_patterns` entry.

Agent CLIs (claude/codex) are eligible only when their TTY is idle past the
window, or when they're truly orphaned (no TTY and reparented to launchd after
their parent exited) — a live no-TTY agent keeps its real parent and is left
alone. cursor-agent is eligible by runtime; dev servers/test runners only when
in a known but inactive worktree.

**Guidance for agents:** prefer `shofar clean` (dry run) and summarize the
candidates for the user before running `--kill`, unless the user has asked for
unattended cleanup.

## Scheduled cleanup toggle

```sh
shofar cleanup on|off|status
```

`on` installs a macOS LaunchAgent that runs `shofar clean --kill` hourly and
at login; `off` removes it. Only suggest enabling this if the user wants
unattended cleanup.

## JSON field reference

| Command            | Key fields |
|--------------------|------------|
| `capacity --json`  | `ok`, `pressure`, `room_for_n`, `usable_headroom_bytes`, `per_worktree_budget_bytes`, `budget_source`, `reason` |
| `status --json`    | `memory`, `capacity`, `worktrees[]`, `cleanup_candidates`, `cleanup_enabled` |
| `clean --json`     | `dry_run`, `candidates[]` (each with `proc` + `reason`), `killed[]`, `errors` |

## Configuration

Optional overrides live in `~/.config/shofar/config.json` (worktree base
dirs, idle windows, memory reserve, per-worktree default budget,
`protect_patterns`). All fields have sane defaults; the file is not required.
