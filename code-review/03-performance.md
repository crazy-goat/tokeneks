# Performance

Performance and scalability problems.

---

## P1 — Every session file is parsed twice (or three times)

**Where:** PI — `pi_agent.go:219` (`piSessions` parses each file) then `pi_agent.go:394`
(`piList` parses it again); Claude — `claude.go:203` (`claudeSessions`) then `claude.go:402`
(`claudeList`); web — `web.go:82` and `web.go:138` parse a third time.

**Problem:** `piSessions`/`claudeSessions` already fully parse every `.jsonl` file (to filter by
model and count steps) and discard the parsed data, returning only metadata. The list/web layer
then re-opens and re-parses every file to compute usage.

**Why it matters:** For N session files of size S, this is 2–3× the I/O and JSON unmarshalling.
With the 10 MB scanner buffer (see [P6](#p6)) and large transcripts, this dominates runtime.

**How to fix:** Have the discovery functions return the parsed usage/steps alongside the metadata
(or cache parsed results keyed by filepath), so downstream code reuses them instead of re-parsing.

**AC (test):** `TestPISessions_UsageIncludedInResult` — assert that the `piSession` structs
returned by `piSessions` already contain the step data needed by `piList`, so `piList` does not
need to call `piSessionUsage` again.

---

## P2 — `resolvePISessionPath` parses every session file to resolve one ID

**Where:** `pi_agent.go:265-288` (calls `piSessions(days, "", "")` at line 270)

**Problem:** To resolve a single session ID to a filepath, the function calls `piSessions`, which
walks the entire sessions tree AND fully parses every `.jsonl` file in it (see [P1](#p1)), just to
match one `sess.ID`.

**Why it matters:** `pi detail <id>` and the web `/api/session/PI/<id>` endpoint pay the cost of
parsing the whole history to open one file. This scales linearly with total history size for a
single-item lookup.

**How to fix:** Resolve the ID from filenames alone (the ID is encoded in the filename) without
parsing file contents — walk directory entries and match the ID-from-filename. Parse only the one
matched file.

**AC (test):** `TestResolvePISessionPath_OnlyOpensMatchedFile` — create a temp sessions directory
with 3 files, call the resolver for one ID, and assert only one file was opened (use a counter
wrapper around `os.Open`).

---

## P3 — `resolveClaudeSessionPath` walks the whole tree per detail request

**Where:** `claude.go:263-294` (`filepath.WalkDir` over `~/.claude/projects`)

**Problem:** Every Claude detail lookup walks the entire projects directory tree looking for
`<id>.jsonl`.

**Why it matters:** O(total files) per single-session request. On a large history each detail page
load re-walks everything.

**How to fix:** This is acceptable if rare, but consider stopping the walk early once the match is
found (return a sentinel error from the walk callback to short-circuit instead of walking to the
end and collecting all matches).

**AC (test):** `TestResolveClaudeSessionPath_StopsAfterFirstMatch` — place the target file as the
first entry in a temp project dir that also has many other files; assert the walk visits no files
after the match.

---

## P4 — `ocSteps` re-queries the DB per session inside loops

**Where:** `opencode.go:197` (in `ocList`), `main.go:146` (in `printTotal`)

**Problem:** `ocSessions` already JOINs `part` and GROUP BYs to count step-finish parts per
session. Then the loop calls `ocSteps(sess.ID)` for each session — a fresh `sql.Open` + query per
session (see also [R1](04-resources-and-leaks.md#r1)).

**Why it matters:** N+1 query pattern: one query for the session list, then one query per session,
each opening a new DB handle. For many sessions this is a lot of round-trips and handle churn.

**How to fix:** Fetch all step-finish token rows for the candidate sessions in a single query
(e.g. `WHERE session_id IN (...) ORDER BY session_id, time_created`) and group in memory, or reuse
one shared `*sql.DB`.

**AC (test):** `TestOCAllSteps_SingleQuery` — using an in-memory SQLite DB seeded with 3 sessions,
assert that fetching steps for all 3 sessions issues exactly 1 SQL query (not 3).

---

## P5 — Redundant `os.Stat` in `getCreatedAt`

**Where:** `birthtime.go:9-21`, called from `pi_agent.go:239` and `claude.go:236`

**Problem:** The discovery loops already call `os.Stat(fp)` (`pi_agent.go:204`, `claude.go:188`) to
read mtime, then call `getCreatedAt(fp)` which does `os.Stat(fp)` a second time for the birth time.

**Why it matters:** Two stat syscalls per file where one suffices; multiplied across the whole
history.

**How to fix:** Pass the already-obtained `os.FileInfo` into the birth-time extractor instead of
re-statting, or have it return both mtime and birthtime in one call.

**AC (test):** `TestGetCreatedAtFromInfo_UsesProvidedInfo` — pass a known `os.FileInfo` to the
updated function and assert it does not call `os.Stat` again (use a flag or check the return value
matches the input info).

---

## P6 — `/api/sessions` re-parses everything on every request, no caching

**Where:** `web.go:220-229` (`/api/sessions` calls `gatherWebSessions(days)` every time)

**Problem:** Each dashboard load / reload re-runs `gatherWebSessions`, which re-discovers and
re-parses all OpenCode/PI/Claude sessions from scratch (compounding [P1](#p1)). The response even
sets `Cache-Control: no-cache, no-store`.

**Why it matters:** Every browser refresh triggers a full re-scan and re-parse of the entire
history; with double-parsing this is the heaviest operation in the program, on the hot path.

**How to fix:** Cache the gathered result in memory with a short TTL or invalidate on file mtime
changes; recompute only what changed. At minimum, avoid the explicit `no-store` on data that is
expensive to regenerate.

**AC (test):** `TestAPISessionsHandler_SecondCallUsesCachedResult` — call the handler twice in
quick succession using `httptest`; assert that the underlying `gatherWebSessions` is invoked only
once (wrap it with a counter).

---

## P7 — 10 MB scanner buffer allocated per (re)parse

**Where:** `pi_agent.go:87`, `claude.go:104`, `web_detail.go:467`, `web_detail.go:638`

**Problem:** Each parse allocates a scanner with a 10 MB max buffer. Combined with double/triple
parsing ([P1](#p1)) and per-request re-parsing ([P6](#p6)), this creates significant allocation
and GC pressure.

**Why it matters:** Large transient allocations on a hot path increase GC work and latency.

**How to fix:** Reduce duplicate parsing first (the biggest win). Consider a `sync.Pool` of scanner
buffers if profiling shows allocation pressure, and only grow the buffer when a line actually needs
it.

**AC (test):** `TestNewJSONLScanner_UsesConstants` — assert that the `newJSONLScanner` helper (from
D12) uses the named constants `scannerInitBuf` and `scannerMaxBuf`; grep the source to confirm
the magic numbers `1024*1024` and `10*1024*1024` no longer appear as literals.

---

## P8 — `fillSessionStats` iterates the steps slice twice

**Where:** `web_detail.go:84-131` then `web_detail.go:158-173`

**Problem:** The function loops over `d.Steps` once for the main stats, then a second time over
`d.Steps` (and nested `ToolCalls`) just to accumulate tool durations.

**Why it matters:** Minor, but the tool-duration accumulation could be folded into the first pass;
two passes over potentially large step/toolcall data.

**How to fix:** Accumulate the `durByTool` map in the same loop that already iterates steps and
tool calls.

**AC (test):** `TestFillSessionStats_ToolDurations` — build a `SessionDetail` with known tool-call
durations; call `fillSessionStats`; assert `ToolDurations` contains correct avg/max/count per
tool name.
</content>
