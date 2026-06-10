# Resources & Memory

Resource-handling and lifetime problems.

---

## R2 — Fragile pointer-into-slice pattern in `ocSessionDetail`

**Where:** `web_detail.go:380` (`current = &steps[len(steps)-1]`) used at `web_detail.go:405`
(`current.ToolCalls = append(...)`)

**Problem:** `current` is a pointer into the `steps` slice's backing array. Tool parts mutate
`current.ToolCalls`. The code currently re-assigns `current` immediately after every `append`, so
it happens to stay valid — but the pattern is one edit away from a classic Go bug: if any future
change appends to `steps` between setting `current` and using it, `current` would point into a
stale (reallocated) backing array and the mutation would be silently lost.

**Why it matters:** This is a latent correctness/aliasing hazard. It is non-obvious and survives
only by luck of the current control flow.

**How to fix:** Track the current step by index (`curIdx`) and mutate `steps[curIdx]` directly, or
build the step's tool-call slice in a local and assign it at step-finish. Avoid holding a pointer
into a slice that is still being appended to.

**AC (test):** `TestOCSessionDetail_ToolCallsOnCorrectStep` — feed a sequence of parts (step-finish
then tool then step-finish then tool) via the parsing logic and assert each tool call lands on the
correct step, not the previous one.

---

## R3 — `msgRole` / `toolCallTimes` / `toolCallStart` maps grow per parse without bound

**Where:** `web_detail.go:269` (`msgRole`), `web_detail.go:458` (`toolCallTimes`),
`web_detail.go:629` (`toolCallStart`)

**Problem:** These maps accumulate one entry per message/tool call for the whole file. They are
freed when the function returns (so not a true leak), but for very large sessions they hold the
entire message-id/role and tool-id/timestamp sets in memory at once.

**Why it matters:** Memory usage scales with session size; combined with re-parsing on every web
request ([P6](03-performance.md#p6)) the peak allocation is repeated per request.

**How to fix:** `toolCallStart`/`toolCallTimes` entries are consumed when a matching result is seen
— `delete` them on match (Claude path already does this at `web_detail.go:673`; the PI path at
`web_detail.go:516` does not). Deleting consumed entries bounds the map to in-flight tool calls.

**AC (test):** `TestPISessionDetail_ToolCallMapClearedAfterMatch` — parse a JSONL with one
tool-call/tool-result pair and assert that `toolCallTimes` is empty after the result is processed
(no leaked entries).

---

## R4 — Ignored error leaves `db` query result lifetimes implicit

**Where:** `web_detail.go:255` (`_ = db.QueryRow(...).Scan(...)`), `opencode.go:159`
(`db.QueryRow(...).Scan(...)` error ignored)

**Problem:** Errors from the metadata `QueryRow().Scan()` are discarded. While `QueryRow` releases
its connection on `Scan`, ignoring the error means a failed/partial scan proceeds with zero-value
`title`/`model`, and any underlying connection error is invisible.

**Why it matters:** Silent failure produces blank titles/models downstream and hides real DB
problems; see also [C4](05-correctness-bugs.md#c4).

**How to fix:** Check and at least log the error, or propagate it. Decide explicitly whether a
missing session row is an error or an empty result.

**AC (test):** `TestOCSessionDetail_ScanErrorReturned` — use an in-memory SQLite DB with a
malformed session row; assert `ocSessionDetail` returns a non-nil error rather than silently
continuing with zero-value fields.
</content>
