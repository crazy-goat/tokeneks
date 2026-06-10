# Code Duplication

Detailed description of code duplication problems. Each item contains the location, the problem,
why it matters, and a suggested fix (without implementation).

---

## D3 — `ComputeIdeal` vs `ComputeIdealClaude`

**Where:** `algo.go:264-307` vs `algo.go:72-128`

**Problem:** The two functions computing "ideal cache read" have a nearly identical loop and logic
(compact detection, `idealCR > totalCtx`, waste). The Claude version is a superset (it adds
`CacheCreation`).

**Why it matters:** A fix in the algorithm (e.g. the 80% compact threshold) must be applied twice;
the versions already differ subtly (the order of conditions when `i == 0`).

**How to fix:** Unify into one function operating on a shared row model where `CacheCreation` is
simply 0 for Kimi. The "no cache-creation" variant becomes a special case of the general one.

**AC (test):** `TestComputeIdeal_EquivalentToClaudeWhenNoCacheCreation` — run both functions on
the same `[]StepData` (all `CacheCreation == 0`) and assert `IdealCR`, `Waste`, and `IsCompact`
are identical for every row.

---

## D4 — `Summarize` vs `SummarizeClaude`

**Where:** `algo.go:214-234` vs `algo.go:184-212`

**Problem:** Identical sum aggregation and the same cost formula, differing only by the presence of
`CacheCreation` terms.

**How to fix:** One summarize function over a shared row type (after merging [D6](#d6)). The
cache-creation term zeroes out by itself when the price/value is 0.

**AC (test):** `TestSummarize_EquivalentToClaudeWhenNoCacheCreation` — feed the same rows (no CC)
to both functions and assert `Actual`, `Ideal`, `Overpay`, and `TotalCR` are equal.

---

## D5 — `printDetailRows` vs `printDetailRowsClaude`

**Where:** `algo.go:309-332` vs `algo.go:236-261`

**Problem:** The two table-printing functions differ only by two extra columns (`c.write`, `i_cc`).
The `Printf` formats, separators and `$` row are rewritten from scratch.

**How to fix:** One function with a flag/parameter "show cache-creation columns" or build rows from
a column list. This removes the risk of column-width drift between variants.

**AC (test):** `TestPrintDetailRows_NoCacheCreation_MatchesClaude` — capture stdout from both
print functions with the same input (no CC) and assert the numeric columns are identical.

---

## D6 — Duplicated types: `IdealRow`/`ClaudeIdealRow`, `Summary`/`ClaudeSummary`, two `Note()`

**Where:** `algo.go:46-56` + `algo.go:147-155`; `algo.go:131-144` + `algo.go:171-182`;
`algo.go:58-69` + `algo.go:157-168`

**Problem:** `ClaudeIdealRow` is `IdealRow` + CC fields. `ClaudeSummary` is `Summary` + CC fields.
The `Note()` method is nearly identical in both row types.

**How to fix:** One row type and one summary type with optional cache-creation fields (0 for Kimi).
One `Note()` method.

**AC (test):** `TestNote_AllBranches` — cover COMPACT / HIT / PARTIAL / MISS for the unified
type; assert each case returns the correct string. Both the old `IdealRow` and `ClaudeIdealRow`
call sites must use the same type after the fix.

---

## D7 — Dominant-model selection logic is still duplicated

**Where:** `pi_agent.go` computes `DominantModel` with an inline count/tie-break loop, and Claude
has the same selection logic repeated in both `claudeSessions` and `claudeDetail`.

**Problem:** The rule "pick the most frequent model, break ties lexicographically" still exists in
multiple places. The exact locations shifted, but the duplication remains.

**How to fix:** Extract a helper like `dominantModel(counts map[string]int) string` and reuse it in
PI and Claude flows.

**AC (test):** `TestDominantModel_MostFrequent` and `TestDominantModel_TieBreakLexicographic` —
call the helper directly; assert it returns the correct model in both cases.

---

## D8 — Cost-per-1M-token computation duplicated across row and total output

**Where:** `opencode.go`, `pi_agent.go`, and `claude.go` each compute per-session and total
`$/1M`/`i$/1M` values inline.

**Problem:** The same guarded `cost / float64(tokens) * 1e6` pattern is repeated across list rows
and footer totals. This duplicates both the arithmetic and the zero-token guard.

**How to fix:** Introduce a shared helper such as `perMillion(cost float64, tokens int) float64`
and use it everywhere these values are rendered.

**AC (test):** `TestPerMillion_ZeroTokens` — assert `perMillion(1.0, 0) == 0.0` (no `NaN`/`Inf`).
`TestPerMillion_NonZero` — assert `perMillion(1.0, 1_000_000) == 1.0`.

---

## D9 — Per-model grouping/aggregation is duplicated in CLI and web paths

**Where:** `pi_agent.go` and `claude.go` build `byModel` maps for detail/list calculations, and
`web.go` independently groups usage by model again for PI and Claude web sessions.

**Problem:** The grouping logic has evolved, but the same "bucket by model, then aggregate per
bucket" pattern is still repeated in multiple CLI and web code paths.

**How to fix:** Extract a shared grouping helper (and, where possible, shared per-model cost/
summary helpers) so the web and CLI layers stop re-deriving the same aggregates independently.

**AC (test):** `TestGroupByModel_AggregatesCorrectly` — pass a slice of steps with two different
model names, assert the map has two keys with the correct sub-slices.

---

## D10 — TOTAL-row / footer printing duplicated across 3 list functions

**Where:** `opencode.go:241-262`, `pi_agent.go:461-477`, `claude.go:478-507`

**Problem:** Computing `totalOverpay`, `pct`, `totalCostPer1M`, `totalIdealPer1M` and printing the
TOTAL row is nearly identical in `ocList`, `piList`, `claudeList`.

**How to fix:** A shared helper that prints the totals footer from the aggregated sums.

**AC (test):** `TestPrintTotalsFooter_ConsistentFormat` — capture stdout from the new helper with
known inputs and assert all three agents produce the same column layout.

---

## D11 — Session discovery via directory walking duplicated

**Where:** `pi_agent.go:176-251` vs `claude.go:157-246`

**Problem:** `piSessions` and `claudeSessions` have a nearly identical structure: `ReadDir` of the
base dir → iterate subdirs → `ReadDir` → filter `.jsonl` → `os.Stat` → filter by date/mtime → build
session struct → `sort.Slice` by `Birth`.

**How to fix:** Extract a shared directory iterator (e.g. a function taking a per-file callback),
and pass the differences (filename format, parser) as parameters.

**AC (test):** `TestWalkSessionFiles_SkipsNonJSONL` — create a temp dir with mixed files, assert
only `.jsonl` files are passed to the callback. `TestWalkSessionFiles_FiltersByDate` — assert
files outside the date range are skipped.

---

## D12 — Duplicated scanner buffer setup (magic numbers)

**Where:** `pi_agent.go:87`, `claude.go:104`, `web_detail.go:467`, `web_detail.go:638`

**Problem:** `scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)` with the same magic numbers
repeated 4×.

**How to fix:** Constants `scannerInitBuf`, `scannerMaxBuf` + a helper
`newJSONLScanner(r io.Reader) *bufio.Scanner`.

**AC (test):** `TestNewJSONLScanner_ScansBeyondDefaultBuffer` — write a line longer than the
default 64 KB `bufio.Scanner` limit, pass it through `newJSONLScanner`, assert it scans without
error.

---

## D13 — sessionID parsing from filename duplicated

**Where:** `pi_agent.go:232`, `pi_agent.go:310`, `web_detail.go:571-572`

**Problem:** `strings.TrimSuffix(strings.SplitN(name, "_", 2)[1], ".jsonl")` repeated 3× (and it is
a potential panic — see [C2](05-correctness-bugs.md#c2)).

**How to fix:** A helper `piSessionIDFromFilename(name string) (string, error)` with safe parsing,
used everywhere.

**AC (test):** `TestPISessionIDFromFilename` — table-driven: normal filename → correct ID, no
underscore → error (no panic), filename shorter than 10 chars → error (no panic).

---

## D15 — Cost formula re-implemented in several places

**Where:** `pi_pricing.go:66-71`, `algo.go:196-203`, `web.go:159-162`, `web_detail.go:703-706`

**Problem:** `float64(in)*prices.Input/1e6 + float64(cc)*prices.CacheCreation/1e6 + ...` is
re-implemented at least four times.

**How to fix:** One function `stepCost(step StepData, prices ModelPrices) float64` (it already
exists as `piStepActualCost`, but is unused — see [S1](06-code-smells.md#s1)). Use it everywhere.

**AC (test):** `TestStepCost_MatchesSummarizeActual` — compute the sum of `stepCost` over a slice
of steps and assert it equals `Summarize(...).Actual`.

---

## D16 — Duplicated title/prompt truncation

**Where:** `pi_agent.go` truncates the first user prompt for session titles, and `web_detail.go`
contains the same inline title-truncation pattern in the OpenCode and PI detail builders.

**Problem:** The same `len(...) > 80` / `[:77] + "..."` style truncation logic is still repeated,
with similar ad-hoc variants elsewhere for project display widths.

**How to fix:** Extract a small helper such as `truncate(s string, max int) string` and reuse it
for title/prompt truncation sites.

**AC (test):** `TestTruncate` — string shorter than max → returned unchanged; string equal to max
→ returned unchanged; string longer than max → truncated with `"..."` suffix and length == max.
</content>
