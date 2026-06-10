# Code Review — tokeneks (`tokeneks`)

---

## ⚠️ AGENT EXECUTION INSTRUCTIONS — READ FIRST, EVERY SESSION

You are executing this code review plan. Rules — follow them without exception:

1. **Read this file at the start of every session** to find your current position (first unchecked `[ ]` item).
2. **Work exactly one task at a time.** Never group, batch, or parallelize tasks.
3. **For each task:**
   a. Read the linked details file (full problem description + fix guidance + AC).
   b. Implement the fix.
   c. Write a Go test that satisfies the Acceptance Criteria described in the issue.
   d. Run `go test ./...` and confirm it passes.
   e. Mark the task `[x]` in this file and add a one-line note of what you did on the line directly below it.
4. **Do not stop between tasks.** Move immediately to the next `[ ]` item.
5. **Do not stop until every single checkbox is `[x]`.**
6. Do not ask for confirmation. Do not wait for user input between tasks.
7. If a test fails after your fix, debug and fix before moving on — never skip.
8. After all tasks are done, run `go test ./...` one final time to confirm the full suite is green.

---

## Progress log

_(Append one line per completed task so the log is human-readable across sessions.)_

---

## About this codebase

Tool: `tokeneks` — TokenEKS / Token Efficiency Kontrol Suite for LLM session cost, cache, and efficiency analysis across OpenCode, PI Agent, and Claude Code.
Stack: Go 1.25, cobra, go-sqlite3, net/http. **No package or technology changes allowed.**

Detail files:
- [`01-duplication.md`](01-duplication.md)
- [`02-architecture.md`](02-architecture.md)
- [`03-performance.md`](03-performance.md)
- [`04-resources-and-leaks.md`](04-resources-and-leaks.md)
- [`05-correctness-bugs.md`](05-correctness-bugs.md)
- [`06-code-smells.md`](06-code-smells.md)

---

## Task list — work top to bottom, one at a time

### Code Duplication

- [ ] **D3** — `ComputeIdeal` vs `ComputeIdealClaude` are near-duplicates → [details](01-duplication.md#d3--computeideal-vs-computeidealclaude)
- [ ] **D4** — `Summarize` vs `SummarizeClaude` are near-duplicates → [details](01-duplication.md#d4--summarize-vs-summarizeclaude)
- [ ] **D5** — `printDetailRows` vs `printDetailRowsClaude` are near-duplicates → [details](01-duplication.md#d5--printdetailrows-vs-printdetailrowsclaude)
- [ ] **D6** — Duplicated types `IdealRow`/`ClaudeIdealRow`, `Summary`/`ClaudeSummary`, two `Note()` → [details](01-duplication.md#d6--duplicated-types-idealrowclaudeidealrow-summaryclaudesummary-two-note)
- [ ] **D7** — Dominant-model selection logic is still duplicated in PI and Claude flows → [details](01-duplication.md#d7--dominant-model-computation-duplicated-3)
- [ ] **D8** — Cost-per-1M computation duplicated across list rows and totals → [details](01-duplication.md#d8--cost-per-1m-tokens-computation-duplicated)
- [ ] **D9** — Per-model grouping/aggregation is duplicated in CLI and web paths → [details](01-duplication.md#d9--bymodel-aggregation--per-model-summarize-duplicated)
- [ ] **D10** — TOTAL-row / footer printing duplicated across 3 list functions → [details](01-duplication.md#d10--total-row--footer-printing-duplicated-across-3-list-functions)
- [ ] **D11** — Directory-walk session discovery duplicated (PI vs Claude) → [details](01-duplication.md#d11--session-discovery-via-directory-walking-duplicated)
- [ ] **D12** — Scanner buffer setup (magic numbers) duplicated 4× → [details](01-duplication.md#d12--duplicated-scanner-buffer-setup-magic-numbers)
- [ ] **D13** — sessionID-from-filename parsing duplicated 3× → [details](01-duplication.md#d13--sessionid-parsing-from-filename-duplicated)
- [ ] **D15** — Cost formula re-implemented in 4 places → [details](01-duplication.md#d15--cost-formula-re-implemented-in-several-places)
- [ ] **D16** — Title/prompt truncation snippet duplicated → [details](01-duplication.md#d16--duplicated-titleprompt-truncation)

### Architecture

- [ ] **A1** — Mutable package-global `days`/`dateFilter`; `resolvePISessionPath` reads global `days` → [details](02-architecture.md#a1--mutable-package-global-state-for-days-and-datefilter)
- [ ] **A2** — File I/O at package init (`var claudePrices = initClaudePrices()`) → [details](02-architecture.md#a2--file-io-performed-at-package-initialization)
- [ ] **A3** — No separation of data access / business logic / presentation → [details](02-architecture.md#a3--no-separation-between-data-access-business-logic-and-presentation)
- [ ] **A4** — No common `Agent` interface; 3 agents reimplement everything → [details](02-architecture.md#a4--no-common-abstraction-across-the-three-agents)
- [ ] **A5** — Algorithm routed by brittle `prices.CacheCreation == 0` heuristic → [details](02-architecture.md#a5--routing-on-the-pricescachecreation--0-heuristic)
- [ ] **A7** — Three different cost sources in the web dashboard → [details](02-architecture.md#a7--three-different-cost-sources-in-the-web-dashboard)
- [ ] **A8** — Embedded dashboard depends on an external CDN (Chart.js) → [details](02-architecture.md#a8--embedded-dashboard-depends-on-an-external-cdn)

### Performance

- [ ] **P1** — Every session file parsed 2–3 times → [details](03-performance.md#p1--every-session-file-is-parsed-twice-or-three-times)
- [ ] **P2** — `resolvePISessionPath` parses entire history to resolve one ID → [details](03-performance.md#p2--resolvepisessionpath-parses-every-session-file-to-resolve-one-id)
- [ ] **P3** — `resolveClaudeSessionPath` walks whole tree per request without early exit → [details](03-performance.md#p3--resolveclaudesessionpath-walks-the-whole-tree-per-detail-request)
- [ ] **P4** — `ocSteps` N+1 queries (new DB handle per session in loops) → [details](03-performance.md#p4--ocsteps-re-queries-the-db-per-session-inside-loops)
- [ ] **P5** — Redundant `os.Stat` in `getCreatedAt` → [details](03-performance.md#p5--redundant-osstat-in-getcreatedat)
- [ ] **P6** — `/api/sessions` re-parses everything on every request, no caching → [details](03-performance.md#p6--apisessions-re-parses-everything-on-every-request-no-caching)
- [ ] **P7** — 10 MB scanner buffer allocated per (re)parse → [details](03-performance.md#p7--10-mb-scanner-buffer-allocated-per-reparse)
- [ ] **P8** — `fillSessionStats` iterates steps twice → [details](03-performance.md#p8--fillsessionstats-iterates-the-steps-slice-twice)

### Resources & Memory

- [ ] **R2** — Fragile pointer-into-slice (`&steps[len-1]`) in `ocSessionDetail` → [details](04-resources-and-leaks.md#r2--fragile-pointer-into-slice-pattern-in-ocsessiondetail)
- [ ] **R3** — Tool-time maps grow per parse; PI path never `delete`s consumed entries → [details](04-resources-and-leaks.md#r3--msgrole--toolcalltimes--toolcallstart-maps-grow-per-parse-without-bound)
- [ ] **R4** — Ignored `QueryRow().Scan()` errors → [details](04-resources-and-leaks.md#r4--ignored-error-leaves-db-query-result-lifetimes-implicit)

### Correctness Bugs

- [ ] **C1** — Divide-by-zero still possible in list/total output paths → [details](05-correctness-bugs.md#c1--division-by-zero--naninf-in-percentage-and-per-1m-calculations)
- [ ] **C2** — Latent slice-index panics on filename format (`[:10]`, `SplitN[1]`) → [details](05-correctness-bugs.md#c2--latent-panics-from-slice-indexing-on-filename-format-assumptions)
- [ ] **C3** — `claudeList` header separator (179) ≠ footer (141) → [details](05-correctness-bugs.md#c3--mismatched-table-separator-widths-in-claudelist)
- [ ] **C4** — Silent error handling still hides partial failures and missing data → [details](05-correctness-bugs.md#c4--ignored-errors-swallow-data-and-hide-failures)
- [ ] **C5** — Tool-duration attributed to wrong tool call (no id match) → [details](05-correctness-bugs.md#c5--tool-duration-attribution-picks-the-wrong-tool-call)
- [ ] **C6** — OpenCode tool parts depend on arriving after `step-finish` → [details](05-correctness-bugs.md#c6--opencode-tool-parts-depend-on-arriving-after-their-step-finish)
- [ ] **C7** — DB-stored cost vs recomputed cost diverge (CLI vs web) → [details](05-correctness-bugs.md#c7--tokens_input-etc-counted-via-join-but-recomputed-differently-in-list)
- [ ] **C8** — `ocList` silently drops unpriced sessions from totals → [details](05-correctness-bugs.md#c8--oclist-silently-skips-sessions-with-no-configured-price-but-they-were-already-counted)

### Code Smells

- [ ] **S1** — `repeatByte` is dead code; `piStepActualCost` exists but is still not wired into callers → [details](06-code-smells.md#s1--dead-code)
- [ ] **S2** — Redundant `string(strings.Repeat(...))` conversion (4×) → [details](06-code-smells.md#s2--redundant-stringstringsrepeat-conversion)
- [ ] **S3** — Custom `max(float64)` shadows the Go 1.21+ builtin → [details](06-code-smells.md#s3--custom-max-shadows-the-go-121-builtin)
- [ ] **S4** — `idealIn` always 0 → meaningless `TotalIdealIn`/`i_in` column → [details](06-code-smells.md#s4--idealin-is-always-0-in-computeidealclaude-making-totalidealin-meaningless)
- [ ] **S5** — Ignored errors on `Scan`/`Unmarshal`/`UserHomeDir` → [details](06-code-smells.md#s5--ignored-errors-on-scan--unmarshal--userhomedir)
- [ ] **S6** — Hardcoded username in project-name cleaning → [details](06-code-smells.md#s6--hardcoded-username-in-project-name-cleaning)
- [ ] **S7** — Repository is not gofmt-clean (`algo.go`, `pi_agent.go`, `web.go`, `web_detail.go`) → [details](06-code-smells.md#s7--gofmt-violation-mis-indented-brace)
- [ ] **S8** — Scattered magic numbers (compact threshold, `1e6`, widths) → [details](06-code-smells.md#s8--scattered-magic-numbers)
- [ ] **S9** — Blanket `recover()` masks bugs instead of fixing them → [details](06-code-smells.md#s9--recover-used-as-a-catch-all-in-the-detail-handler)
- [ ] **S10** — `expandHome` only handles `~/`, ignores `UserHomeDir` error → [details](06-code-smells.md#s10--expandhome-only-handles-the--prefix)
- [ ] **S11** — `isErr` flags non-terminal tool statuses as errors → [details](06-code-smells.md#s11--iserr-heuristic-flags-non-terminal-statuses-as-errors)
</content>
