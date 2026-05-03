# Contributing to Versus Incident

Thanks for considering a contribution! This document explains how to get
your change merged with the least friction.

## Ways to contribute

- **Report bugs** — open an issue with reproduction steps, version, and
  deployment mode (Docker, Helm, source).
- **Request features** — open a discussion first if it's a significant
  change. Small additions can go straight to an issue.
- **Improve docs** — Markdown lives under [`src/`](src/) and is built with
  mdBook. The pre-rendered HTML in `book/` is generated; do not hand-edit.
- **Submit code** — see below.

## Before you start coding

1. **Search existing issues and PRs** — your idea may already be tracked.
2. **For non-trivial changes, open an issue first.** This avoids wasted
   work if the design needs to be different. Maintainers usually respond
   within a few days.
3. **Read the user docs under [`src/`](src/)** to understand the existing
   architecture, configuration model, and extension points before
   proposing changes.

## Development setup

```bash
# Requires Go 1.23.1+
go build ./...
go vet ./...
go test ./...

# Run locally (needs config/config.yaml relative to CWD)
go build -o run ./cmd
./run
```

Health check: `curl http://localhost:3000/healthz`.

For Docker / Helm flows see the README.

## Coding standards

- **Idiomatic Go.** Run `gofmt -w .` and `go vet ./...` before pushing.
- **No unused variables or parameters.** Use `_` with a comment when a
  parameter is intentionally unused.
- **Wrap errors** with `%w` (`fmt.Errorf("...: %w", err)`).
- **Match the existing import grouping** — stdlib, third-party, then
  `github.com/VersusControl/...` separated by blank lines.
- **Don't introduce new dependencies casually.** The project deliberately
  keeps a small surface (Fiber, Viper, AWS SDK v2, slack-go, redis, uuid).
  Justify new modules in the PR description.
- **Per-provider files** live in `pkg/common/<provider>.go` (one file
  per provider).
- **No phase references in code comments.** Roadmap phases belong in
  planning docs, not in code. Code comments describe what the code does
  today.

For extension points (new alert channel, on-call provider, queue
listener, signal source) follow the patterns in `pkg/common/` and
`pkg/signalsources/` and look at the existing factory files
(`factory_alert.go`, `factory_oncall.go`, `factory_listener.go`).

## Testing

- Tests live alongside code in `*_test.go` files.
- Use `t.TempDir()` for any file I/O — no hardcoded `/tmp` paths.
- Use `net/http/httptest` for HTTP-level tests; never make real network
  calls.
- Prefer table-driven tests for multiple input cases.
- Run `go test ./...` and confirm it passes before opening a PR.

## Commit messages

Follow Conventional Commits. The git history uses prefixes like:

```
feature: add detect service for ai sre agent
fix: handle empty regex pattern
docs: update AI agent configuration
```

Common types: `feature`, `fix`, `docs`, `refactor`, `test`, `chore`.

## Pull request process

1. Fork the repo and create a branch from `main`:
   `git checkout -b feature/short-description`
2. Make your change with a clear, focused commit history.
3. Add or update tests. PRs that change behavior without test coverage
   will be asked to add tests.
4. Update relevant docs under `src/` if user-facing behavior changes.
5. Update [`ROADMAP.md`](ROADMAP.md) if your PR closes a roadmap item.
6. Run `go test ./...`, `go vet ./...`, `gofmt -w .`.
7. Push your branch and open a PR. Fill in the PR template.
8. Be responsive to review comments. Maintainers will squash-merge once
   approved.

## What gets PRs rejected

- Unscoped refactors mixed in with the actual change.
- Hardcoded secrets, tokens, or webhook URLs in source or YAML.
- Disabling tests instead of fixing them.
- Adding a dependency without justification.
- Drive-by formatting of unrelated files.

## Security

If you find a security issue, **do not open a public PR or issue**. Follow
the process in [`SECURITY.md`](SECURITY.md).

## License

By contributing, you agree that your contributions will be licensed under
the [MIT License](LICENSE) that covers the project.

## Code of Conduct

This project follows the [Contributor Covenant Code of
Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold
it.

## Getting help

- GitHub Discussions:
  https://github.com/VersusControl/versus-incident/discussions
- Issues: https://github.com/VersusControl/versus-incident/issues
- Email: `supports@devopsvn.tech`
