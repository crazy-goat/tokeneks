# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `LICENSE` (MIT)
- `docs/workflow.md` and `docs/release-workflow.md` follow the shared crazy-goat templates; project commands live in the new `AGENTS.md`
- `bin/` has the shared issue and worktree helper scripts, plus `worktree-setup.sh`
- `bin/lint.sh` runs `go vet`, golangci-lint v2 (with the gofmt and goimports formatters) and shellcheck; `make lint` calls it
- CI: lint, `go test -race` and `go build` with the aggregate `ci-ok` check; heavy jobs are skipped for documentation-only changes
- Release workflow: pushing a `vX.Y.Z` tag builds `tokeneks` for linux and darwin (amd64, arm64) and publishes a GitHub release with notes from this file
- Dependabot for Go modules and GitHub Actions

### Changed
- Lint findings fixed: a few unchecked errors, a capitalized error string, simplified conditions

### Removed
- Unused internal helpers (`fileMTime`, `dbFileMTime`, `nowMs`, `ocToolCalls`, `ocSessionCost`, `piMessages`, `storeModelPricesMap`)
