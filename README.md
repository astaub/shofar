# shofar

**Take back your RAM**

A macOS RAM guard for dev-worktree workflows. Unlike display-only monitors
(htop, btop, glances), shofar **acts**: it tells agents whether the machine
can take on another worktree, and it cleans up safe-to-kill stale dev processes
before the machine runs out of memory.

## Why

Running many git worktrees with their own dev servers, plus long-lived AI agent
CLIs (Claude Code, Codex, cursor-agent), quietly piles up RAM until the machine
crawls. Existing OSS either only *displays* memory (htop/btop/glances/vmstat) or
is a Linux-only reactive OOM killer (earlyoom/nohang/systemd-oomd) with no
concept of "this is a stale dev server, safe to kill" or "can I start another
one." shofar fills that gap, macOS-first and agent-native.

## Install

```sh
brew install astaub/tap/shofar          # Homebrew (recommended)
go install github.com/astaub/shofar@latest   # or via Go
```

Then ask the machine if it can take another worktree:

```sh
$ shofar capacity
YES — headroom for at least one more worktree at the current per-worktree budget
  pressure:        normal
  available:       8.5 GB
  reserve:         3.0 GB
  usable headroom: 5.5 GB
  worktree budget: 1.5 GB (measured from 3 worktree(s))
  room for:        3 more worktree(s)
```

Build from source instead:

```sh
go build -o shofar . && mv shofar /usr/local/bin/
```

Requires macOS (uses `vm_stat`, `sysctl`, `ps`, `lsof`, `launchctl`).

## Use it as a skill in any coding agent

shofar ships an [Agent Skill](https://github.com/anthropics/skills) playbook
(`skills/shofar/SKILL.md`) — the open format read by Claude Code, Codex,
Cursor, Gemini CLI, Copilot, Goose, and ~30 other tools. The binary can install
it into an agent's skills directory:

```sh
shofar skill install --agent claude   # -> ~/.claude/skills/shofar/SKILL.md
shofar skill install --dir <path>      # any other agent's skills directory
shofar skill print                     # write it wherever you need
shofar skill path                      # show known per-agent locations
```

The skill teaches an agent to gate on `shofar capacity --json` before
spawning a worktree and to propose `shofar clean` when memory is tight.

## Commands

```
shofar status   [--json]            Memory + worktree + cleanup overview
shofar capacity [--json]            Can this machine take another worktree?
shofar clean    [--kill] [--json]   Show (default) or kill safe stale procs
shofar cleanup  on|off|status       Toggle the scheduled auto-cleanup agent
shofar skill    print|path|install  Install the Agent Skill into a coding agent
```

Every read command supports `--json` for agent use. `clean` defaults to a dry
run; nothing is killed without `--kill`.

### `capacity` — the agent gate

```sh
shofar capacity --json
```
```json
{
  "ok": true,
  "pressure": "normal",
  "available_bytes": 9171697664,
  "reserve_bytes": 3221225472,
  "usable_headroom_bytes": 5950472192,
  "per_worktree_budget_bytes": 1572864000,
  "budget_source": "measured",
  "room_for_n": 3,
  "reason": "headroom for at least one more worktree at the current per-worktree budget"
}
```

An agent can check `ok` before spawning another worktree. The verdict combines:

- **VM pressure** (`kern.memorystatus_vm_pressure_level`) as a hard gate —
  anything but `normal` returns `ok: false`.
- **Usable headroom** = available memory − a reserve held for the OS.
- **Per-worktree budget** — *measured* from the average footprint of worktrees
  that currently have processes, or a configured default when there is nothing
  to measure.

`room_for_n = usable_headroom / per_worktree_budget`.

## Cleanup safety

`clean` is conservative by design — the cost of killing live work far exceeds a
missed stale process. It **never**:

- kills a process in its own ancestor chain (so running it from inside a Claude
  session can't kill that session);
- kills a process attributed to an **active** worktree (running dev server or
  recent edits);
- kills a session younger than `min_session_minutes`;
- kills anything matching a `protect_patterns` entry.

Agent CLIs (claude/codex) are eligible only when their controlling TTY has been
idle past the window, or when truly orphaned — no TTY *and* reparented to
launchd (PPID 1) after their parent exited. A live no-TTY agent (e.g. an
editor-spawned session) keeps its real parent, so it's protected. cursor-agent
is eligible by runtime; dev servers and test runners only when attributed to a
known but inactive worktree.

## Scheduled cleanup

```sh
shofar cleanup on      # install a LaunchAgent: `shofar clean --kill` hourly + at login
shofar cleanup off     # remove it
shofar cleanup status  # is it on, and does launchd agree?
```

## Config

`~/.config/shofar/config.json` (all fields optional; defaults shown):

```json
{
  "worktree_bases": ["~/code/worktrees"],
  "active_subdirs": ["app", "config", "src", "lib", "apps", "packages"],
  "active_minutes": 1440,
  "claude_idle_hours": 6,
  "min_session_minutes": 30,
  "stale_agent_minutes": 120,
  "reserve_bytes": 3221225472,
  "default_worktree_budget_bytes": 1572864000,
  "protect_patterns": [],
  "cleanup_enabled": false
}
```

## Development

No hosted CI — checks run locally via a pre-push git hook (zero cost). Activate
it once per clone:

```sh
git config core.hooksPath .githooks   # runs go vet + build + test before each push
```

Cut a release locally (no CI needed): `goreleaser release --clean` on a `vX.Y.Z`
tag, or `goreleaser build --snapshot --clean` for unsigned local binaries.

## Status

v0.1.0. Known follow-ups: nested worktree layouts (e.g.
`base/<repo>/<branch>`) are not yet discovered — only flat
`base/<worktree>` layouts; merged-worktree directory pruning is not yet ported.

## License

MIT
