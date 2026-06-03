# Contributing to shofar

Thanks for helping out. shofar is a small, dependency-free Go tool, so getting
started is quick.

## Get it running

```sh
git clone https://github.com/astaub/shofar && cd shofar
go build -o shofar .
go test ./...
```

Then activate the pre-push hook (runs `go vet` + build + test before every push):

```sh
git config core.hooksPath .githooks
```

The full operating protocol — project layout, conventions, the security
contract, and how review works — lives in [`AGENTS.md`](./AGENTS.md). Read it
before you open a PR. The short version: contributors **propose** PRs against
`main`; a maintainer reviews and merges. Never commit secrets or machine paths
(this is a public repo).

## Welcome PRs

A few things that would genuinely help:

- **Nested worktree discovery** — only flat `base/<worktree>` layouts are found
  today; `base/<repo>/<branch>` is not yet discovered.
- **Merged-worktree pruning** — detecting and reaping directories for worktrees
  whose branches have merged.
- **More agent-CLI heuristics** — better idle/orphan detection for coding-agent
  processes beyond claude/codex/cursor-agent.
- **Tests that pin cleanup boundaries** — any test that makes the
  safe-to-kill rules harder to accidentally loosen.
- **Docs and examples** — clearer real-output examples, especially for the
  `--json` agent path.

Found a bug or have an idea? Open an issue first if it's a big change, so we can
agree on the shape before you write code.
