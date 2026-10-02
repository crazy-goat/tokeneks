# TokenEKS

**TokenEKS** — **Token Efficiency Kontrol Suite**

A Go CLI and web dashboard for analyzing LLM session cost, token usage, cache efficiency, and ideal vs actual spend across OpenCode, PI Agent, and Claude Code.

## Build

Default `go build` output name depends on the current directory name. To always produce the expected binary name, build with `-o`:

```bash
go build -o tokeneks .
```

Or use:

```bash
make build
```

## Run

CLI:

```bash
./tokeneks --help
```

Web dashboard:

```bash
./tokeneks web
```

Default web address:

```text
http://localhost:8080
```

## Commands

- `tokeneks oc list`
- `tokeneks oc detail <session-id>`
- `tokeneks pi list`
- `tokeneks pi detail <session-id|filepath>`
- `tokeneks claude list`
- `tokeneks claude detail <session-id|filepath>`
- `tokeneks total`
- `tokeneks web`

## Configuration

Claude model pricing is loaded from:

```text
~/.tokeneks/claude_models.json
```

If the file is missing, built-in defaults are used.

## Naming notes

- Cobra command name is `tokeneks`.
- The Go module name is `tokeneks`.
- Web UI branding uses `TokenEKS`.
- If you run plain `go build` without `-o`, the produced binary name is derived from the folder name, not from the Cobra command name.

## Development

See [AGENTS.md](AGENTS.md) for build, test, lint and release commands, and
[docs/workflow.md](docs/workflow.md) for the development process. `go-sqlite3` needs cgo,
so building requires a C compiler. Run the checks with `bin/lint.sh` and
`go test -race ./...`. Licensed under the MIT license, see [LICENSE](LICENSE).
