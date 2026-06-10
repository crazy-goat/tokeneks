# Correctness Bugs

Behavioural defects: crashes, wrong numbers, inconsistent output.

---

## C1 — Division by zero still exists in list/total output paths

**Where:** `opencode.go` totals/footer percentage calculation, `pi_agent.go` totals/footer
percentage calculation, and `main.go` total report output. The pure summarize helpers already guard
`PctIdeal`, but some CLI presentation paths still divide by `ideal` totals directly.

**Problem:** This issue is partially fixed. `Summarize` and `SummarizeClaude` already avoid
`NaN`/`Inf`, but list/total printers still contain direct divisions like `totalOverpay / totalIdeal`
without always guarding the denominator.

**Why it matters:** Empty histories or filtered-out date ranges can still produce `+Inf%`/`NaN%`
in CLI output even though the lower-level summarize helpers are safe.

**How to fix:** Guard every remaining presentation-layer percentage calculation (`if denom > 0`)
and centralize it in a helper so CLI output and summary structs behave consistently.

**AC (test):** Keep `TestSummarize_NoDivisionByZeroOnEmptyRows` and
`TestSummarizeClaude_NoDivisionByZeroOnEmptyRows`, and add coverage for CLI totals (especially
`printTotal`) so zero-session output does not print `NaN%`.

---

## C2 — Latent panics from slice indexing on filename format assumptions

**Where:** `pi_agent.go:210` (`fileEntry.Name()[:10]`), `pi_agent.go:232`
(`strings.SplitN(..., "_", 2)[1]`), `web_detail.go:572` (`strings.SplitN(sessID, "_", 2)[1]`)

**Problem:** These assume every filename is at least 10 chars and contains a `_`. A `.jsonl` file
whose name is shorter than 10 chars, or has no underscore, will panic with an index-out-of-range.
Only the web handler is wrapped in `recover()` (`web_detail.go:200-204`); the CLI paths
(`piSessions`, `piDetail`) have no such guard and will crash the program.

**Why it matters:** A single malformed/unexpected filename in the sessions directory crashes
`pi list` / `pi detail`. The presence of `recover()` in the web handler (see [S?]) is itself a
signal the authors knew these can panic.

**How to fix:** Validate length and the presence of `_` before slicing; skip or report files that
don't match the expected naming scheme. Consolidate into the safe
`piSessionIDFromFilename` helper ([D13](01-duplication.md#d13)).

**AC (test):** `TestPISessionIDFromFilename_DoesNotPanic` — table-driven test with a short
filename and a filename without `_`; assert both return an error and do not panic.

---

## C3 — Mismatched table separator widths in `claudeList`

**Where:** `claude.go:396` (`strings.Repeat("-", 179)` header) vs `claude.go:478`
(`strings.Repeat("-", 141)` footer)

**Problem:** The header separator is 179 dashes; the footer separator is 141. The table borders
don't line up.

**Why it matters:** Purely cosmetic but visible — the footer rule is visibly shorter than the
header rule in a tool whose output is a formatted table.

**How to fix:** Use a single shared width constant for both separators (and ideally derive it from
the format string).

**AC (test):** `TestClaudeList_SeparatorWidths` — capture stdout from `claudeList` on a minimal
in-memory data set; assert the first and last separator lines have equal length.

---

## C4 — Silent error handling still hides partial failures and missing data

**Where:** `web.go` still skips whole agent groups with `if err == nil`, `web_detail.go` and
`opencode.go` still ignore some `Scan`/`Unmarshal` failures, and `helpers.go` still ignores
`UserHomeDir` failure in `expandHome`.

**Problem:** The exact call sites moved slightly, but the behavior is the same: several failures are
still treated as "just skip it" rather than being surfaced. In the web aggregator this means one
broken source can quietly disappear from the dashboard.

**Why it matters:** Totals can be understated with no visible warning, and debugging missing data is
hard because the failure path is silent.

**How to fix:** Surface partial errors explicitly (log, collect, or return them alongside partial
results). At minimum, avoid silently dropping whole agent datasets and ignored path/DB parse errors.

**AC (test):** `TestGatherWebSessions_PartialErrorLogged` — make one agent's loader return an
error; assert the other agents' sessions are still returned AND the error is surfaced (not silently
dropped).

---

## C5 — Tool-duration attribution picks the wrong tool call

**Where:** `web_detail.go:664-670` (Claude detail)

**Problem:** When a `tool_result` arrives, the code finds the matching duration by scanning steps
backwards and assigning to the *first* tool call with `DurationMs == 0`. It does not match on the
`tool_use_id`. With multiple concurrent/sequential tool calls in a step, the duration is attached
to whichever call happens to still be zero, not the one the result belongs to.

**Why it matters:** Per-tool duration stats (`ToolDurations`, avg/max) are computed from
mis-attributed durations, giving misleading numbers in the dashboard.

**How to fix:** Store the `tool_use_id` on each `ToolCallInfo` when created, and match the result's
`tool_use_id` to the exact tool call when assigning `DurationMs` (the `toolCallStart` map already
keys by id — carry that id through to the stored tool call).

**AC (test):** `TestClaudeSessionDetail_ToolDurationByID` — parse a JSONL with two tool calls in
one step; assert that each `ToolCallInfo` receives its own correct `DurationMs`, not the other's.

---

## C6 — OpenCode tool parts depend on arriving after their `step-finish`

**Where:** `web_detail.go:382-407` (`case "tool"` attaches to `current`, which is only set by
`case "step-finish"` at line 380)

**Problem:** Tool parts are attached to `current`, the most recent step-finish. If a `tool` part is
ordered (by `time_created`) before the first `step-finish`, `current` is `nil` and the tool call is
silently dropped; if tool parts belong to the step that *follows* them, they are attached to the
previous step.

**Why it matters:** Tool-call lists per step can be incomplete or shifted depending on the exact
ordering of parts in the OpenCode DB, affecting the detail view and tool stats.

**How to fix:** Group parts by `message_id` (already available) rather than relying on global
time-ordering, and associate tool parts with their owning message/step explicitly.

**AC (test):** `TestOCSessionDetail_ToolPartBeforeStepFinish` — feed a synthetic DB where a `tool`
part appears before its `step-finish` part; assert the tool call is still attached to the correct
step.

---

## C7 — `tokens_input` etc. counted via JOIN but recomputed differently in list

**Where:** `opencode.go:99-100`/`113-114` (session row carries `tokens_input`, `cost`, ...) vs
`opencode.go:209` (`ocList` recomputes everything from `ocSteps` and ignores `sess.Cost`)

**Problem:** `ocSessions` selects `s.cost`, `s.tokens_input`, etc. into the struct, but `ocList`
ignores them and recomputes actual/ideal from `ocSteps`. Meanwhile `web.go:56` *does* use
`sess.Cost`. The DB-stored cost and the recomputed cost can differ.

**Why it matters:** CLI and web show different cost numbers for the same OpenCode session; see also
[A7](02-architecture.md#a7).

**How to fix:** Decide on one cost source and use it in both paths. If recomputation is canonical,
don't select the DB cost (or display it explicitly as a separate "reported" value).

**AC (test):** `TestOCCostConsistency_CLIvsWeb` — use the same OC session data; assert that the
cost produced by `ocList` and the cost shown in the web `WebSession.TotalCost` are computed the
same way (both from price table, or both from DB).

---

## C8 — `ocList` silently skips sessions with no configured price, but they were already counted

**Where:** `opencode.go:204-206` (`if prices.Input == 0 { continue }`)

**Problem:** `ocSessions` returns all sessions matching the model filter (the JOIN/count includes
them). `ocList` then skips any whose model has no price (`continue`), so they vanish from the table
and totals — but there is no message that they were dropped.

**Why it matters:** The displayed total under-reports usage whenever an unpriced model is present,
with no indication to the user.

**How to fix:** Either fall back to a default price (as `printTotal` does for OC at `main.go:152`)
or print a visible note listing skipped models, consistently with how `printTotal` handles it.

**AC (test):** `TestOCList_UnpricedSessionsReported` — create a session with a model not in
`ocModelPrices`; capture stdout from `ocList`; assert it prints a warning or includes the session
(not silently omits it).
</content>
