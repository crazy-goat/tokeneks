# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed
- Claude session summaries now retain the last nonempty user prompt from string or text-block content, without replacing it with tool results (#53)
- The session detail page loads the embedded Chart.js asset instead of a CDN, so charts work offline and use the same version as the dashboard (#74)
- The session detail page escapes the agent, model, date and duration values in the meta section before inserting them as HTML (#77)
- Unparseable JSONL lines (and OpenCode parts) are no longer dropped silently: the Claude and PI readers and the OpenCode detail reader now print a `warning: skipped N unparseable line(s) in <source>` message to stderr (again only when the count grows) (#52)
- The web dashboard validates the `--port` value before starting the server and reports a clear error for non-numeric or out-of-range ports, instead of failing later with a confusing listen error (#76)

## [0.1.0] - 2026-10-02

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
