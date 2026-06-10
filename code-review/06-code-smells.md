# Code Smells

Smaller issues: dead code, redundancy, naming, magic numbers, style.

---

## S1 — One symbol is dead code, another is orphaned utility code

**Where:** `helpers.go` contains unused `repeatByte`, and `pi_pricing.go` contains `piStepActualCost`.

**Problem:** `repeatByte` is dead code. `piStepActualCost` is not dead in the same sense — it has a
real purpose and tests — but it is still not wired into production callers, so it behaves like an
orphaned utility instead of a canonical helper.

**How to fix:** Delete `repeatByte`. Either make `piStepActualCost` the shared production cost
helper (preferred, see [D15](01-duplication.md#d15)) or remove/rename it if another helper becomes
canonical.

**AC (test):** After deleting `repeatByte`: `go build ./...` must pass with no undefined-symbol
errors. For `piStepActualCost`: keep a test asserting it matches production cost calculations and
then wire it into real callers or replace it with the canonical helper.

---

## S2 — Redundant `string(strings.Repeat(...))` conversion

**Where:** `algo.go:239`, `algo.go:248`, `algo.go:312`, `algo.go:321`

**Problem:** `strings.Repeat` already returns a `string`; wrapping it in `string(...)` is a no-op
conversion.

**How to fix:** Drop the redundant `string(...)` wrapper.

**AC (test):** `go vet ./...` must pass. Grep the source for `string(strings.Repeat` — must return
zero matches.

---

## S3 — Custom `max` shadows the Go 1.21+ builtin

**Where:** `helpers.go:18-23` (`func max(a, b float64) float64`); module is `go 1.25`
(`go.mod:3`)

**Problem:** Since Go 1.21 there is a generic builtin `max`/`min`. Defining a package-level
float64-only `max` shadows the builtin within the package, is less general, and is surprising to
readers.

**How to fix:** Remove the custom `max` and use the builtin (works for `float64` directly). If a
named helper is desired for clarity, name it distinctly (e.g. `clampZero`) since most uses are
`max(x, 0)`.

**AC (test):** `TestMax_ReturnsLarger` — verify the expected results before removal. After
removing the custom function, `go build ./...` must pass (confirming the builtin is used).

---

## S4 — `idealIn` is always 0 in `ComputeIdealClaude`, making `TotalIdealIn` meaningless

**Where:** `algo.go:103` (`idealIn := 0`), summed at `algo.go:193`, printed at `algo.go:250`/`252`

**Problem:** In the Claude ideal model, `idealIn` is hardcoded to 0, so `TotalIdealIn` is always 0,
yet it is summed, stored and printed as an `i_in` column.

**Why it matters:** A column/field that is structurally always zero is noise; it suggests an
incomplete model and wastes a table column.

**How to fix:** Either remove the always-zero `i_in` column for the Claude variant, or document why
it is intentionally zero. After merging the compute paths ([D3](01-duplication.md#d3)) the field
becomes meaningful only for the Kimi case.

**AC (test):** `TestComputeIdealClaude_IdealInIsAlwaysZero` — run `ComputeIdealClaude` on 3+
steps and assert every `IdealIn == 0`. After the fix this test either verifies the field is removed
or explicitly documents the intentional design.

---

## S5 — Ignored errors on `Scan` / `Unmarshal` / `UserHomeDir`

**Where:** `opencode.go:159`, `web_detail.go:255`, `web_detail.go:713`
(`json.Unmarshal(... &contentItems)`), `helpers.go:12`

**Problem:** Errors returned by these calls are discarded with `_` or not checked at all. (Tracked
also under [C4](05-correctness-bugs.md#c4) for the data-loss angle; listed here as a pervasive
style smell.)

**How to fix:** Handle or log every returned error; enable `errcheck` in linting.

**AC (test):** `go vet ./...` must produce no new warnings. Run `errcheck ./...` — zero
unhandled errors in non-test code.

---

## S6 — Hardcoded username in project-name cleaning

**Where:** `pi_agent.go:255` (`prefix := "Users-piotr.halas-"`), `claude.go:251-254`
(`"-Users-piotr-halas-"`, `"-Users-piotr-halas"`)

**Problem:** The path-cleaning helpers hardcode the developer's username, and the Claude one strips
two near-identical prefixes back-to-back as an ad-hoc fix. They also duplicate the same concept
([D11](01-duplication.md#d11)).

**Why it matters:** The tool only de-noises paths for this one user; for anyone else the project
column shows the raw encoded path. Not portable.

**How to fix:** Derive the home/prefix dynamically (e.g. from `os.UserHomeDir` encoded the same way
the agents encode cwd) instead of a string literal. Share one cleaner across PI and Claude.

**AC (test):** `TestCleanProjectName_DynamicHome` — assert that the function strips a prefix
derived from the current user's home directory, not a hardcoded string. Run the test under a
different `HOME` env var and assert it still strips correctly.

---

## S7 — Repository is not gofmt-clean

**Where:** Currently `gofmt -l .` reports at least `algo.go`, `pi_agent.go`, `web.go`, and
`web_detail.go`.

**Problem:** This is no longer just one mis-indented brace. Multiple files are not formatted with
`gofmt`, which suggests formatting is not enforced consistently.

**Why it matters:** It creates noisy diffs, hides real changes in formatting churn, and makes the
single-file complaint from the original ticket too narrow.

**How to fix:** Run `gofmt -w` on the repository and add a format check to CI.

**AC (test):** `gofmt -l .` must produce no output (zero files need formatting).

---

## S8 — Scattered magic numbers

**Where:** `algo.go:82`/`algo.go:274` (`*80/100` compact threshold), truncation widths
(`80`/`77`, `30`/`28`, `25`/`23`), `1e6` everywhere, separator widths (`88`, `108`, `141`, `154`,
`173`, `179`), scanner buffer sizes ([D12](01-duplication.md#d12))

**Problem:** Behaviour-defining constants are inlined as literals throughout.

**Why it matters:** The 80% compact threshold and the per-1M divisor are domain rules buried as
literals; changing them means hunting through the code. Separator widths drift from their format
strings (see [C3](05-correctness-bugs.md#c3)).

**How to fix:** Promote to named constants (`compactThresholdPct`, `tokensPerMillion`, column
widths derived from the format spec).

**AC (test):** `TestCompactDetection_UsesNamedThreshold` — assert that the compact threshold is
expressed as a named constant; change the constant to 70 in the test and assert the boundary
moves accordingly (i.e. the constant is actually used).

---

## S9 — `recover()` used as a catch-all in the detail handler

**Where:** `web_detail.go:200-204`

**Problem:** `handleAPISessionDetail` wraps its whole body in `recover()` and returns the panic
text as an HTTP 500. This masks the latent panics from [C2](05-correctness-bugs.md#c2) rather than
fixing them, and turns programming errors into runtime 500s.

**Why it matters:** A blanket recover hides bugs (the slice-index panics) and makes them
data-dependent surprises instead of being fixed at the source.

**How to fix:** Fix the underlying panic sources ([C2](05-correctness-bugs.md#c2)). Keep recover
only as a last-resort guard with proper logging, not as the primary error strategy.

**AC (test):** `TestHandleAPISessionDetail_BadPath_Returns400` — send a request to
`/api/session/` (no agent/id) via `httptest`; assert HTTP 400 is returned cleanly, without
relying on `recover()`.

---

## S10 — `expandHome` only handles the `~/` prefix

**Where:** `helpers.go:10-16`

**Problem:** It expands `~/foo` but not a bare `~`, and silently ignores the `UserHomeDir` error
(returning `filepath.Join("", path[2:])` on failure).

**Why it matters:** Edge-case paths resolve incorrectly and silently; minor but combines with
[C4](05-correctness-bugs.md#c4).

**How to fix:** Handle bare `~`, and handle/propagate the `UserHomeDir` error instead of joining
onto an empty string.

**AC (test):** `TestExpandHome_TildeSlash` — assert `~/foo` is expanded to an absolute path.
`TestExpandHome_BareTilde` — assert bare `~` is expanded to the home directory (not returned
as-is).

---

## S11 — `isErr` heuristic flags non-terminal statuses as errors

**Where:** `web_detail.go:389` (`isErr := status != "completed" && status != ""`)

**Problem:** Any tool status other than `completed` or empty is treated as an error. Transient
states such as `running`/`pending` (if ever persisted) would be reported as tool errors.

**Why it matters:** Inflates the `ToolErrors` stat with non-error states.

**How to fix:** Match explicitly against known error statuses, or invert: treat only an explicit
`error`/`failed` status as an error.

**AC (test):** `TestToolCallIsError_KnownStatuses` — assert `status == "error"` → `isErr = true`;
`status == "completed"` → `false`; `status == "running"` → `false` (not an error); `status == ""`
→ `false`.

---

</content>
