# Architecture

Structural / design-level problems.

---

## A1 — Mutable package-global state for `days` and `dateFilter`

**Where:** `main.go:10-11`, used in `pi_agent.go:270` (`resolvePISessionPath` reads global `days`)

**Problem:** `days` and `dateFilter` are package-level globals. `dateFilter` is bound via
`StringVarP(&dateFilter, ...)` to the `-D` flag of THREE different commands (`oc list`, `pi list`,
`claude list`) — all sharing one variable. Worse, `resolvePISessionPath` reaches for the global
`days` instead of receiving it as a parameter.

**Why it matters:** Hidden global dependencies make functions impure and hard to test in isolation;
`resolvePISessionPath` silently behaves differently depending on the `--days` flag even though it
resolves a single session by ID. Shared globals also make the code unsafe to reuse concurrently
(e.g. from the web server).

**How to fix:** Pass `days`/`date` explicitly through function parameters. Use command-local
variables for flags (as is already done for `claudeModelFlag`) rather than package globals.
`resolvePISessionPath` should take `days` as an argument or default to scanning without a day
cutoff.

**AC (test):** `TestResolvePISessionPath_IgnoresGlobalDays` — set the global `days` to an extreme
value, call the function with an explicit days argument, and assert the result is governed by the
argument, not the global.

---

## A2 — File I/O performed at package initialization

**Where:** `claude.go:17` (`var claudePrices = initClaudePrices()`)

**Problem:** A package-level variable is initialized by a function that reads `~/.tokeneks/claude_models.json`
from disk. This runs at import time, before `main` and before any flag parsing.

**Why it matters:** Side effects (filesystem reads) at init time make the package impossible to
import without touching the disk, complicate testing, and hide failures (errors are swallowed).
It also fixes the prices at startup with no way to reload.

**How to fix:** Load prices lazily on first use with `sync.Once` (the pattern is already used
correctly in `pi_pricing.go:31` — `piGlobalModelPrices`). Make the two pricing loaders consistent.

**AC (test):** `TestClaudePrices_LoadedLazily` — confirm that accessing `claudePrices` before any
explicit load does not panic, and that calling the loader twice returns the same map instance
(via the `sync.Once` guarantee).

---

## A3 — No separation between data access, business logic and presentation

**Where:** `opencode.go:182-265` (`ocList`), `pi_agent.go:379-479` (`piList`),
`claude.go:388-510` (`claudeList`), `pi_agent.go:290-377` (`piDetail`)

**Problem:** Each of these functions opens a data source, parses it, runs the ideal/cost
computation, AND formats output with `fmt.Printf` — all in one function body.

**Why it matters:** The presentation (terminal tables) is tightly coupled to data loading and
computation. You cannot reuse the computation for the web dashboard without re-deriving it (which
is exactly why `web.go` re-implements aggregation — see [D9](01-duplication.md#d9)). Testing the
numbers requires capturing stdout.

**How to fix:** Split into layers: (1) loaders returning `[]StepData`/sessions, (2) a pure
computation layer returning summary structs, (3) renderers (CLI printer, JSON encoder) consuming
those structs. The web and CLI paths then share layers 1–2.

**AC (test):** `TestComputeLayer_PureFunction` — call the compute layer (Summarize / ComputeIdeal)
directly without involving any I/O or `fmt.Printf`; assert the returned struct has correct values.
This verifies the layer is decoupled from presentation.

---

## A4 — No common abstraction across the three agents

**Where:** `opencode.go`, `pi_agent.go`, `claude.go` (whole files)

**Problem:** OpenCode, PI and Claude each independently implement `*Sessions`, `*Messages/*Steps`,
`*List`, `*Detail`, `*SessionDetail`. They share the same conceptual shape but no interface ties
them together.

**Why it matters:** Adding a fourth agent means copying ~300 lines. `printTotal`, `gatherWebSessions`
and `handleAPISessionDetail` all switch/branch per agent by hand, so every new agent touches many
call sites.

**How to fix:** Define an `Agent` interface (e.g. `Sessions(days, date, model)`, `Steps(id)`,
`Prices(model)`, `Detail(id)`) and implement it three times. Generic `list`, `total` and web
handlers then iterate over a registry of agents.

**AC (test):** `TestAgentRegistry_AllThreeAgentsPresent` — build the registry and assert it
contains exactly three agents; assert each implements the interface (compile-time check is
sufficient).

---

## A5 — Routing on the `prices.CacheCreation == 0` heuristic

**Where:** `pi_agent.go:341`, `pi_agent.go:407`

**Problem:** The code decides whether to use `ComputeIdeal` (Kimi-style) or `ComputeIdealClaude`
(cache-creation-aware) by testing `if prices.CacheCreation == 0`.

**Why it matters:** This conflates "model has no cache-creation pricing configured" with "model has
genuinely zero cache-creation cost" and with "prices missing entirely". A model whose price file
genuinely lists `cacheWrite: 0`, or any model missing from the price map, is silently routed to the
wrong algorithm and mis-billed.

**How to fix:** Carry an explicit capability flag on the model/prices (e.g. `SupportsCacheCreation
bool`) rather than inferring it from a price value. After merging the two compute paths
([D3](01-duplication.md#d3)) this branch disappears entirely.

**AC (test):** `TestModelPrices_ZeroCacheCreationPriceNotMisrouted` — create a `ModelPrices` with
`CacheCreation == 0` but `SupportsCacheCreation == true`; assert the routing uses the
cache-creation-aware path.

---

## A7 — Three different cost sources in the web dashboard

**Where:** `web.go:56` (OC uses `sess.Cost` from DB), `web.go:97` (PI uses `step.Cost` from file),
`web.go:159-162` (Claude recomputes from `claudePrices`)

**Problem:** The same dashboard displays costs derived three different ways: OpenCode trusts the
DB-stored cost, PI trusts the per-message cost in the JSONL, Claude recomputes from the local price
table.

**Why it matters:** Numbers are not comparable across agents — Claude's figures track your local
price file while OC/PI track whatever the tools recorded. A price-file edit changes only Claude
rows. This is a correctness/consistency hazard for a tool whose entire purpose is cost comparison.

**How to fix:** Pick one strategy (recompute everything from a single price table, or display only
provider-reported costs) and apply it uniformly. If both are wanted, show them as clearly distinct
columns ("reported" vs "computed").

**AC (test):** `TestGatherWebSessions_CostStrategyConsistent` — for a session present in both OC
and the price map, assert the `TotalCost` field uses the same calculation method as the other
agents (all recomputed, or all provider-reported).

---

## A8 — Embedded dashboard depends on an external CDN

**Where:** `web/index.html:7` (`<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.1/...">`)

**Problem:** The dashboard HTML is embedded into the binary via `//go:embed`, yet it pulls Chart.js
from a public CDN at runtime.

**Why it matters:** The "self-contained binary" benefit of embedding is defeated — the dashboard is
blank offline or if the CDN is unreachable, and it leaks a request to a third party. For a local
cost-analysis tool that may run on machines without internet, this is a reliability issue.

**How to fix:** Vendor the Chart.js file into `web/` and embed it alongside the HTML, serving it
from a local route. (No package/technology change — it stays static assets + `net/http`.)

**AC (test):** `TestWebIndexHTML_NoExternalScriptTags` — read the embedded `webIndexHTML` bytes
and assert no `src` attribute points to an external domain (e.g. `cdn.jsdelivr.net`).
</content>
