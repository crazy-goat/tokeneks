# AGENTS.md

Project commands and specifics for tokeneks. The development process (issue,
worktree, review, PR, merge) is in [docs/workflow.md](docs/workflow.md), the release
process in [docs/release-workflow.md](docs/release-workflow.md).

Everything is written in English (code, comments, docs, commits, issues).

tokeneks is a Go CLI and web dashboard that analyzes LLM session cost, token usage and
cache efficiency for OpenCode, PI Agent and Claude Code. It is a single Go module,
`tokeneks`, with a `main` package at the repository root.

## Layout

| Path | Content |
|---|---|
| `main.go`, `sync.go`, `ingest_main.go` | CLI entry point (cobra) and commands |
| `claude*.go`, `opencode.go`, `oc_parse.go`, `pi_*.go`, `agent.go` | Per-agent session readers |
| `prices*.go` | Model price lookup, derivation and checks |
| `web*.go`, `web/` | Web dashboard (server code and embedded HTML/JS) |
| `compute/` | Cost and cache-efficiency calculations |
| `ingest/` | Session ingestion and file watcher |
| `store/` | SQLite store (`go-sqlite3`, needs cgo) |
| `docs/` | Process docs |

Read `README.md` before changing commands or flags. They are the public interface.

## Commands

Go version: from `go.mod` (CI uses `go-version-file: go.mod`). `go-sqlite3` needs cgo,
so a C compiler must be available.

```bash
make build                     # go build -o tokeneks .
make run                       # go run .
make web                       # go run . web (http://localhost:8080)
make test                      # go test ./...
go test -race -count=1 ./...   # what CI runs

make lint                      # same as bin/lint.sh
bin/lint.sh                    # go vet, golangci-lint v2 (.golangci.yml), shellcheck; check only
bin/lint.sh --fix              # golangci-lint fmt first, then the same checks
```

Tests must not depend on fixed ports or on the home directory; use `httptest`,
`t.TempDir()` and port `0`, so several worktrees can run tests in parallel.
`bin/worktree-setup.sh` runs `go mod download` for a new worktree.

The built binary (`/tokeneks`) and `dist/` are generated and must never be committed.

## CI

`.github/workflows/tests.yaml` runs on pull requests and pushes to `main`. The `changes`
job detects documentation-only changes; the `docs` job checks them fast. The `lint` job
(`bin/lint.sh`), `go test -race` and `go build` run only for code changes. The required
check is `ci-ok`.

## Release

Follow [docs/release-workflow.md](docs/release-workflow.md). Pushing a `vX.Y.Z` tag runs
`.github/workflows/release.yaml`: it builds `tokeneks` for linux and darwin (amd64, arm64)
with cgo on native runners and publishes a GitHub release with the `CHANGELOG.md` notes
and `checksums.txt`. The version is not stored in the source tree.

## Conventions

- Conventional Commits. Scopes: `web`, `store`, `ingest`, `compute`, `prices`, `claude`,
  `opencode`, `pi`, `docs`, `ci`. Example: `fix(store): roll back on failed insert (#12)`.
- `errcheck` stays on. Best-effort cleanup calls (`Close`, `Rollback`, `fmt.Fprint*`,
  `ResponseWriter.Write`, `Encoder.Encode`) are excluded by name in `.golangci.yml`; handle
  every other error that changes behaviour.
- Milestone numbers are not versions. Use the milestone title (`vX.Y.Z`).
