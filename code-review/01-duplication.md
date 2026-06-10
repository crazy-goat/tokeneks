# Code Duplication

Detailed description of code duplication problems. Each item contains the location, the problem,
why it matters, and a suggested fix (without implementation).

---

## D1 — Repeated SQLite connection boilerplate

**Where:** `opencode.go:15-18`, `opencode.go:49-52`, `opencode.go:80-84`, `opencode.go:153-156`, `web_detail.go:246-251`

**Problem:** The block
```
dbPath := expandHome(defaultDB)
db, err := sql.Open("sqlite3", dbPath)
if err != nil { ... }
defer db.Close()
```
is duplicated across 5 functions (`ocSteps`, `ocToolCalls`, `ocSessions`, `ocDetail`, `ocSessionDetail`).

**Why it matters:** Any change to how the connection is opened (adding `?_journal_mode`, a busy
timeout, read-only mode) requires editing 5 places. It is easy to drift (e.g. `ocDetail` closes
manually via `db.Close()` at line 160, the rest use `defer`).

**How to fix:** Introduce a single helper `openOCDB() (*sql.DB, error)` (or, better, one shared
long-lived `*sql.DB` — see [R1](04-resources-and-leaks.md#r1)) and use it in all OpenCode
functions. Unify closing on `defer`.

**AC (test):** Call the new helper twice in a test and assert it returns the same `*sql.DB`
instance (or that `sql.Open` is not called a second time). All five previously-duplicated call
sites must be gone from the source.

---

## D2 — Nearly identical SQL queries in `ocSessions`

**Where:** `opencode.go:97-124`

**Problem:** The `if date != ""` branch and the `else` branch contain the same ~10-line
SELECT/JOIN/GROUP BY, differing ONLY in one WHERE condition (`date(...) = ?` vs
`s.time_created > ...`).

**Why it matters:** Two copies of the same query must be maintained in parallel; changing the
column list in one branch and forgetting the other produces a mismatch.

**How to fix:** Build the static body of the query as one constant string, and append the
differentiating WHERE clause and its argument conditionally (e.g. assemble the `args` slice and a
`whereClause` fragment). One SELECT remains.

**AC (test):** `TestOCSessions_DateAndRangeBranchesUseOneQuery` — mock or inspect the SQL string
built by both code paths and assert they share the same SELECT/JOIN/GROUP BY skeleton; only the
WHERE suffix differs.

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

## D7 — "Dominant model" computation duplicated 3×

**Where:** `pi_agent.go:141-147`, `claude.go:212-219`, `claude.go:320-327`

**Problem:** The loop "find the model with the most occurrences, break ties lexicographically" is
written three times.

**How to fix:** Extract a helper `dominantModel(counts map[string]int) string` and call it in all
three places.

**AC (test):** `TestDominantModel_MostFrequent` and `TestDominantModel_TieBreakLexicographic` —
call the helper directly; assert it returns the correct model in both cases.

---

## D8 — Cost-per-1M-tokens computation duplicated

**Where:** `opencode.go:223-231`, `opencode.go:245-253`, `pi_agent.go:447-455`,
`pi_agent.go:465-473`, `claude.go:464-472`, `claude.go:485-493`

**Problem:** The pattern `if tokens > 0 { x = cost / float64(tokens) * 1e6 }` for `costPer1M` and
`idealPer1M` repeats in every list function and again for the totals.

**How to fix:** A helper `perMillion(cost float64, tokens int) float64` with a built-in
divide-by-zero guard.

**AC (test):** `TestPerMillion_ZeroTokens` — assert `perMillion(1.0, 0) == 0.0` (no `NaN`/`Inf`).
`TestPerMillion_NonZero` — assert `perMillion(1.0, 1_000_000) == 1.0`.

---

## D9 — `byModel` aggregation + per-model summarize duplicated

**Where:** `pi_agent.go:399-430`, `claude.go:407-435`, `web.go:86-111`, `web.go:142-170`

**Problem:** The scheme "group steps by model → compute summary per model → fold into one
`Summary`" is repeated in four places with minor differences (`Summary` once, `ClaudeSummary` once,
`WebModelUsage` twice).

**How to fix:** A shared `groupByModel(steps) map[string][]StepData` plus one per-model summarize
function (after merging [D4](#d4)).

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

## D14 — Duplicated `parseTS` closure

**Where:** `web_detail.go:459-464` vs `web_detail.go:630-635`

**Problem:** An identical `parseTS` closure (RFC3339Nano → fallback "2006-01-02 15:04:05") is
defined twice in `piSessionDetail` and `claudeSessionDetail`.

**How to fix:** One package-level function `parseTimestamp(s string) (time.Time, error)`.

**AC (test):** `TestParseTimestamp` — assert RFC3339Nano input is parsed correctly, and a
`"2006-01-02 15:04:05"` fallback string is also parsed correctly.

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

**Where:** `pi_agent.go:105-107`, `web_detail.go:337-338`, `web_detail.go:486-490`

**Problem:** The pattern `if len(t) > 80 { t = t[:77] + "..." }` (and the analogous `> 30`/`> 25`
for projects) repeats.

**How to fix:** A helper `truncate(s string, max int) string`.

**AC (test):** `TestTruncate` — string shorter than max → returned unchanged; string equal to max
→ returned unchanged; string longer than max → truncated with `"..."` suffix and length == max.
</content>
