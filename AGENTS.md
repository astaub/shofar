# Agents and contributors working on shofar

This is the operating protocol for everyone who touches this repo — human
contributors and coding agents alike (Claude Code, Codex, Cursor, OpenClaw,
Aider, Continue, or an LLM fetching this file via URL). Start here.

## What this is

shofar is a macOS RAM guard for dev-worktree workflows. It tells agents whether
the machine can take on another worktree (`shofar capacity`) and reaps
safe-to-kill stale dev processes (`shofar clean`) before memory runs out.
Single static Go binary, macOS-only (uses `vm_stat`, `sysctl`, `ps`, `lsof`,
`launchctl`).

**Boundary:** shofar *acts* on the machine's processes, but cleanup is
conservative by design — the cost of killing live work far exceeds a missed
stale process. Any change to `clean` eligibility is safety-critical; read the
"Cleanup safety" section of the README before touching `internal/cleaner`.

## Setup

```sh
git clone https://github.com/astaub/shofar ~/shofar && cd ~/shofar
go build -o shofar .   # build the binary
go test ./...          # run the full test suite
```

No external dependencies — the standard library only. Go 1.23+.

## Project layout

```
main.go              command dispatch + usage + shared helpers (fmtBytes)
cmd_*.go             one file per command (status, capacity, clean, cleanup, skill)
internal/sysinfo/    memory + VM pressure reads (vm_stat, sysctl)
internal/capacity/   the capacity verdict (headroom / per-worktree budget)
internal/proc/       process inventory (ps, lsof, TTY + ancestor chains)
internal/cleaner/    safe-to-kill eligibility — SAFETY-CRITICAL
internal/worktree/   worktree discovery + activity attribution
internal/config/     ~/.config/shofar/config.json load + defaults
internal/launchd/    scheduled-cleanup LaunchAgent install/remove
skills/shofar/       the Agent Skill (SKILL.md) shipped with the binary
```

## Conventions

- **Go style:** standard `gofmt`; run `go vet ./...` before pushing. Standard
  library only — do not add third-party deps without a maintainer's sign-off.
- **JSON by default for agents:** every read command takes `--json`. JSON output
  is pretty-printed (two-space indent via `emitJSON`) so it stays readable in a
  terminal and a pipe alike. Keep field names stable and snake_case — agents
  parse them.
- **Errors:** commands return an `error`; `main` prints `shofar: <message>` to
  stderr and exits non-zero (2 for usage, 1 for runtime). Keep messages short
  and actionable. There is no structured error envelope today; if you add one,
  make it stable and document it here.
- **Safety first in `clean`:** never broaden kill eligibility without a test that
  pins the new boundary. The existing protections (ancestor chain, active
  worktree, min session age, protect patterns) are load-bearing.

## Before you open a PR

```sh
go vet ./...
go build -o shofar .
go test ./...
```

Activate the pre-push hook once per clone so these run automatically:

```sh
git config core.hooksPath .githooks
```

There is no hosted CI — checks run locally via that pre-push hook. Make sure all
three pass before you push.

## PR & review

Contributors and outside agents **propose** PRs — they do **not** self-merge. A
maintainer reviews and merges. Open a PR against `main` with a clear description
of what changed and why, and the output of `go test ./...`. Do not push to
`main` directly and do not merge your own PR, even if you have write access.

## Security (MANDATORY — public repo)

- **Never commit secrets, tokens, API keys, or absolute paths from your machine.**
  No `~/Users/<you>/...`, no PATs, no credentials. The Homebrew tap token lives
  only as a GitHub repo secret, never in the tree.
- **Safety contract — state it plainly:** shofar's *read* surface (`status`,
  `capacity`, `clean` without `--kill`) only reads OS memory state and the
  process table; it mutates nothing. Its *write* surface is exactly two things:
  `clean --kill` sends signals to processes that pass every eligibility guard,
  and `cleanup on|off` installs/removes a user LaunchAgent. It writes one file:
  `~/.config/shofar/config.json`. **shofar makes no network calls** — it has no
  network surface at all. Any change that adds a write path, a kill path, or a
  network call must be called out explicitly in the PR.

## Forks

Fork freely (MIT). If you redistribute, repoint the install instructions and the
`.goreleaser.yaml` `release`/`brews` owner fields at your own org before cutting
a release.
