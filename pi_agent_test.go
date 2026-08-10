package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"tokeneks/compute"
	"tokeneks/store"
)

var totalLineRE = regexp.MustCompile(`(?s)Actual paid:\s+\$([0-9.]+).*?Ideal paid:\s+\$([0-9.]+)`)

// parseTotalActualIdeal extracts the "Actual paid: $X" / "Ideal paid: $Y"
// figures piDetail/claudeDetail print in their TOTAL section.
func parseTotalActualIdeal(t *testing.T, out string) (actual, ideal float64) {
	t.Helper()
	m := totalLineRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not find Actual/Ideal paid lines in output:\n%s", out)
	}
	actual, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parse actual: %v", err)
	}
	ideal, err = strconv.ParseFloat(m[2], 64)
	if err != nil {
		t.Fatalf("parse ideal: %v", err)
	}
	return actual, ideal
}

// piTestMessageLine builds one raw PI session-log line for an assistant
// message with the given model and token usage. cost is the agent's own
// logged cost for the step; 0 means "not logged" (piDetail/piList then have
// to fall back to a resolved rate, same as a real unpriced step would).
func piTestMessageLine(timestamp, model string, input, cacheRead, cacheWrite, output int, cost float64) string {
	total := input + cacheRead + cacheWrite + output
	return fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"assistant","provider":"test","model":%q,"usage":{"input":%d,"cacheRead":%d,"cacheWrite":%d,"output":%d,"totalTokens":%d,"cost":{"total":%v}}}}`,
		timestamp, model, input, cacheRead, cacheWrite, output, total, cost)
}

func writePISessionFile(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "2026-08-10T00-00-00-000Z_test.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPiDetail_CarryForwardAcrossModelSwitch is a regression test for the
// bug this fix exists for: piDetail used to run the ideal-cache pass once
// per model (groupStepsByModel), which throws away the carried-forward
// prompt-cache state at every model switch. Here the whole first turn's
// context (Input+Output) is read back verbatim under a *different* model on
// the very next turn — a perfect cache hit spanning the switch. Splitting by
// model makes that second turn look like a cold start, so its ideal cost is
// computed off the full input rate instead of the far cheaper cache-read
// rate, inflating Ideal past Actual for a session that is a full cache hit
// throughout and should report zero overpay.
func TestPiDetail_CarryForwardAcrossModelSwitch(t *testing.T) {
	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices {
		return map[string]compute.ModelPrices{
			// $/M rates chosen only so the two models differ; nothing here
			// needs to resemble a real model's price.
			"test-model-a": {Input: 5.0, CacheCreation: 6.25, CacheRead: 0.5, Output: 25.0, SupportsCacheCreation: true},
			"test-model-b": {Input: 1.0, CacheCreation: 1.25, CacheRead: 0.1, Output: 5.0, SupportsCacheCreation: true},
		}
	}
	t.Cleanup(func() { piPricesFunc = prevPrices })
	withTempStore(t)
	resetAgentPriceWarnings()

	fp := writePISessionFile(t, []string{
		// Turn 1, model A: 1M fresh input tokens, no cache activity yet.
		piTestMessageLine("2026-08-10T00:00:00.000Z", "test-model-a", 1_000_000, 0, 0, 100_000, 0),
		// Turn 2, a *different* model: reads back exactly turn 1's context
		// (1M input + 100k output = 1.1M) from cache. A correct continuous
		// ideal-cache pass recognizes this as a full hit; a pass restarted
		// per model sees 1.1M tokens of cache_read with no prior state and
		// prices them as if they were fresh input.
		piTestMessageLine("2026-08-10T00:05:00.000Z", "test-model-b", 0, 1_100_000, 0, 50_000, 0),
	})

	var err error
	out := captureStdout(t, func() { err = piDetail(fp, -1) })
	if err != nil {
		t.Fatalf("piDetail() = %v\noutput:\n%s", err, out)
	}

	actual, ideal := parseTotalActualIdeal(t, out)

	// Actual: 1M*5 + 100k*25 (turn 1, model A) + 1.1M*0.1 + 50k*5 (turn 2,
	// model B's cache-read and output rates), all /1e6.
	wantActual := (1_000_000*5.0+100_000*25.0)/1e6 + (1_100_000*0.1+50_000*5.0)/1e6
	// piDetail prints at 2 decimal places, so the round-trip through text
	// can be off by up to half a cent.
	const centTolerance = 0.006
	if math.Abs(actual-wantActual) > centTolerance {
		t.Errorf("Actual paid = $%.6f, want $%.6f", actual, wantActual)
	}
	// Ideal, computed correctly (continuous pass): turn 2 is a full cache
	// hit, so its ideal cost equals its actual cost — same total as Actual.
	if math.Abs(ideal-wantActual) > centTolerance {
		t.Errorf("Ideal paid = $%.6f, want $%.6f (a session that is a full cache hit throughout should have Ideal == Actual)", ideal, wantActual)
	}
	if actual < ideal-centTolerance {
		t.Errorf("Actual ($%.6f) < Ideal ($%.6f) — the impossible split-by-model result this fix exists to prevent", actual, ideal)
	}
}

// parseListTotalRow extracts the Paid and Ideal columns from piList's/
// claudeList's "TOTAL" footer row (the DateTime/SessionID/.../Paid/Ideal/...
// table, not piDetail/claudeDetail's "Actual paid: $X" / "Ideal paid: $Y"
// lines — hence the separate parser from parseTotalActualIdeal).
func parseListTotalRow(t *testing.T, out string, paidIdx, idealIdx int) (paid, ideal float64) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "TOTAL" {
			continue
		}
		var err error
		paid, err = strconv.ParseFloat(fields[paidIdx], 64)
		if err != nil {
			t.Fatalf("parse paid field %q: %v", fields[paidIdx], err)
		}
		ideal, err = strconv.ParseFloat(fields[idealIdx], 64)
		if err != nil {
			t.Fatalf("parse ideal field %q: %v", fields[idealIdx], err)
		}
		return paid, ideal
	}
	t.Fatalf("no TOTAL row found in output:\n%s", out)
	return 0, 0
}

// TestPiList_CarryForwardAcrossModelSwitch is the same regression as
// TestPiDetail_CarryForwardAcrossModelSwitch but through piList's aggregate
// path (the second of the two PI call sites that used to split by model).
func TestPiList_CarryForwardAcrossModelSwitch(t *testing.T) {
	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices {
		return map[string]compute.ModelPrices{
			"test-model-a": {Input: 5.0, CacheCreation: 6.25, CacheRead: 0.5, Output: 25.0, SupportsCacheCreation: true},
			"test-model-b": {Input: 1.0, CacheCreation: 1.25, CacheRead: 0.1, Output: 5.0, SupportsCacheCreation: true},
		}
	}
	t.Cleanup(func() { piPricesFunc = prevPrices })
	withTempStore(t)
	resetAgentPriceWarnings()

	baseDir := t.TempDir()
	sessionsDir := filepath.Join(baseDir, "proj")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(sessionsDir, "2026-08-10T00-00-00-000Z_test.jsonl")
	content := piTestMessageLine("2026-08-10T00:00:00.000Z", "test-model-a", 1_000_000, 0, 0, 100_000, 0) + "\n" +
		piTestMessageLine("2026-08-10T00:05:00.000Z", "test-model-b", 0, 1_100_000, 0, 50_000, 0) + "\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	prevDefault := defaultPISessions
	defaultPISessions = baseDir
	t.Cleanup(func() { defaultPISessions = prevDefault })

	var err error
	out := captureStdout(t, func() { err = piList(3650, "") })
	if err != nil {
		t.Fatalf("piList() = %v\noutput:\n%s", err, out)
	}

	// Columns: TOTAL, Tokens, Paid, Ideal, Overpay, %ideal, $/1M, i$/1M —
	// Paid is field[2], Ideal is field[3].
	wantActual := (1_000_000*5.0+100_000*25.0)/1e6 + (1_100_000*0.1+50_000*5.0)/1e6
	paid, ideal := parseListTotalRow(t, out, 2, 3)
	const centTolerance = 0.006
	if math.Abs(paid-wantActual) > centTolerance {
		t.Errorf("TOTAL Paid = $%.6f, want $%.6f", paid, wantActual)
	}
	if math.Abs(ideal-wantActual) > centTolerance {
		t.Errorf("TOTAL Ideal = $%.6f, want $%.6f (continuous carry-forward across the model switch, not the inflated split-by-model figure)", ideal, wantActual)
	}
	if paid < ideal-centTolerance {
		t.Errorf("TOTAL Paid ($%.6f) < Ideal ($%.6f) — the impossible split-by-model result this fix exists to prevent", paid, ideal)
	}
}

// TestResolvePISessionPath_FileFound covers the unchanged happy path:
// resolvePISessionPath must still resolve a bare session id to its file when
// the file is actually on disk.
func TestResolvePISessionPath_FileFound(t *testing.T) {
	fp := writePISessionFile(t, []string{
		piTestMessageLine("2026-08-10T00:00:00.000Z", "test-model-a", 10, 0, 0, 5, 0),
	})
	sessionID, err := piSessionIDFromFilename(filepath.Base(fp))
	if err != nil {
		t.Fatalf("piSessionIDFromFilename: %v", err)
	}

	prevBase := defaultPISessions
	defaultPISessions = filepath.Dir(fp)
	t.Cleanup(func() { defaultPISessions = prevBase })

	gotFP, gotID, err := resolvePISessionPath(sessionID, -1)
	if err != nil {
		t.Fatalf("resolvePISessionPath() = %v", err)
	}
	if gotFP != fp {
		t.Errorf("resolvePISessionPath() path = %q, want %q", gotFP, fp)
	}
	if gotID != sessionID {
		t.Errorf("resolvePISessionPath() id = %q, want %q", gotID, sessionID)
	}
}

// TestResolvePISessionPath_NotFound_ReturnsSentinelType pins down the
// distinguishable "clean miss" error type piDetail relies on to decide
// whether to fall back to the store — a plain fmt.Errorf here would make
// that decision indistinguishable from a genuine walk failure.
func TestResolvePISessionPath_NotFound_ReturnsSentinelType(t *testing.T) {
	baseDir := t.TempDir()
	prevBase := defaultPISessions
	defaultPISessions = baseDir
	t.Cleanup(func() { defaultPISessions = prevBase })

	_, _, err := resolvePISessionPath("nope", -1)
	var notFound *piSessionNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("resolvePISessionPath() error = %v (%T), want *piSessionNotFoundError", err, err)
	}
}

// ingestFakePISession writes a minimal PI session straight into the store,
// bypassing the file-based parser entirely — used to simulate "the source
// JSONL is gone but the session survived in the store" without touching real
// ingest code or real files.
func ingestFakePISession(t *testing.T, st *store.Store, sessionID, project string, msgs []store.Message) {
	t.Helper()
	ps := store.ParsedSession{
		Session: store.Session{
			Agent:        "pi",
			SessionID:    sessionID,
			Project:      project,
			CreatedAt:    msgs[0].CreatedAt,
			LastActivity: msgs[len(msgs)-1].CreatedAt,
		},
	}
	for i, m := range msgs {
		m.Agent = "pi"
		m.SessionID = sessionID
		m.MsgIndex = i
		m.Role = store.RoleAssistant
		ps.Messages = append(ps.Messages, store.ParsedMessage{Message: m})
	}
	if err := st.IngestSession(context.Background(), ps); err != nil {
		t.Fatalf("IngestSession: %v", err)
	}
}

// TestPiDetail_FallsBackToStoreWhenFileMissing is the core case this fix
// exists for: a session whose source JSONL has been rotated/deleted but is
// still cached in the local store (a real cache since commit 2486511) must
// still render, sourced from the store, with a visible note that the file is
// gone rather than silently pretending nothing changed.
func TestPiDetail_FallsBackToStoreWhenFileMissing(t *testing.T) {
	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices {
		return map[string]compute.ModelPrices{
			"test-model-a": {Input: 5.0, CacheCreation: 6.25, CacheRead: 0.5, Output: 25.0, SupportsCacheCreation: true},
		}
	}
	t.Cleanup(func() { piPricesFunc = prevPrices })
	st := withTempStore(t)
	resetAgentPriceWarnings()

	emptyDir := t.TempDir()
	prevBase := defaultPISessions
	defaultPISessions = emptyDir
	t.Cleanup(func() { defaultPISessions = prevBase })

	sessionID := "missing-pi-session-1"
	now := time.Now().UnixMilli()
	// The store's message.cost column is PI's own logged cost (see
	// stepsFromAssistantMessages / detailToStore). It's set here to a value
	// that deliberately differs from what the rate table would compute
	// (1.75, see wantIdeal below), so a correct Actual of 1.23 proves the
	// fallback really is preferring PI's logged cost — the same preference
	// computeSessionPricing gives the file-backed path — rather than
	// silently recomputing it from the rate table.
	wantCost := 1.23
	ingestFakePISession(t, st, sessionID, "work/pi-example", []store.Message{
		{Model: "test-model-a", InputTokens: 100_000, OutputTokens: 50_000, Cost: wantCost, CreatedAt: now},
	})

	var err error
	out := captureStdout(t, func() { err = piDetail(sessionID, -1) })
	if err != nil {
		t.Fatalf("piDetail() = %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "File:     (not found on disk)") {
		t.Errorf("output missing the 'not found on disk' File line:\n%s", out)
	}
	if !strings.Contains(out, "note:") || !strings.Contains(out, "store cache") {
		t.Errorf("output missing a visible fallback note telling the user the source is gone:\n%s", out)
	}
	if !strings.Contains(out, "work/pi-example") {
		t.Errorf("output missing the project recorded in the store:\n%s", out)
	}
	if !strings.Contains(out, "test-model-a") {
		t.Errorf("output missing the model recorded in the store:\n%s", out)
	}

	actual, ideal := parseTotalActualIdeal(t, out)
	const centTolerance = 0.006
	if math.Abs(actual-wantCost) > centTolerance {
		t.Errorf("Actual paid = $%.6f, want $%.6f (PI's own logged cost)", actual, wantCost)
	}
	// Ideal is never overridden by the logged cost — see
	// computeSessionPricing (web_store.go): only Paid prefers it. With no
	// cache activity and a single step, Ideal is just this step's tokens
	// priced straight from the rate table above (100_000*5.0 + 50_000*25.0)/1e6.
	wantIdeal := (100_000*5.0 + 50_000*25.0) / 1e6
	if math.Abs(ideal-wantIdeal) > centTolerance {
		t.Errorf("Ideal paid = $%.6f, want $%.6f (rate-derived, not the logged cost)", ideal, wantIdeal)
	}
}

// TestPiDetail_NotFoundAnywhere_ReturnsNotFoundError covers the case where
// neither the file walk nor the store know the session: the original
// "session not found" message must still surface, not some new fallback
// error, and it must not silently succeed.
func TestPiDetail_NotFoundAnywhere_ReturnsNotFoundError(t *testing.T) {
	withTempStore(t)

	emptyDir := t.TempDir()
	prevBase := defaultPISessions
	defaultPISessions = emptyDir
	t.Cleanup(func() { defaultPISessions = prevBase })

	err := piDetail("does-not-exist-anywhere", -1)
	if err == nil {
		t.Fatal("piDetail() = nil, want a not-found error")
	}
	if !strings.Contains(err.Error(), "PI session not found: does-not-exist-anywhere") {
		t.Errorf("piDetail() error = %v, want it to carry the original not-found message", err)
	}
}

// TestPiDetail_WalkFailureNotMisreportedAsNotFound guards against a real I/O
// error (here: the sessions root itself doesn't exist, which is what a
// permissions problem or a corrupt mount would also surface as) being
// swallowed into the friendlier-looking "session not found" / store-fallback
// path. Such an error must propagate as-is.
func TestPiDetail_WalkFailureNotMisreportedAsNotFound(t *testing.T) {
	withTempStore(t)

	prevBase := defaultPISessions
	defaultPISessions = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { defaultPISessions = prevBase })

	err := piDetail("anything", -1)
	if err == nil {
		t.Fatal("piDetail() = nil, want a walk error")
	}
	if strings.Contains(err.Error(), "session not found") {
		t.Errorf("piDetail() error = %v — a walk/stat failure must not be reported as \"session not found\"", err)
	}
}

// piTestTree lays out a project directory with a parent session and both
// subagent storage layouts PI uses:
//
//	<project>/2026-08-10T05-58-27-620Z_parent.jsonl        parent
//	<project>/2026-08-10T05-58-27-620Z_parent/nested/run-0/session.jsonl
//	<project>/2026-08-10T05-59-35-727Z_sibling.jsonl       parentSession: parent
//	<project>/2026-08-10T05-58-27-620Z_parent/empty/       created but never written
//
// It returns the project dir plus the parent, nested and sibling paths.
func piTestTree(t *testing.T) (projectDir, parentPath, nestedPath, siblingPath string) {
	t.Helper()
	projectDir = t.TempDir()

	parentPath = filepath.Join(projectDir, "2026-08-10T05-58-27-620Z_parent.jsonl")
	if err := os.WriteFile(parentPath, []byte(`{"type":"session","version":3,"id":"parent","timestamp":"2026-08-10T05:58:27.620Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	parentDir := filepath.Join(projectDir, "2026-08-10T05-58-27-620Z_parent")
	nestedDir := filepath.Join(parentDir, "nested", "run-0")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A subagent whose short-ID directory was created but never written — the
	// real-world marker of a sibling-layout run.
	if err := os.MkdirAll(filepath.Join(parentDir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	nestedPath = filepath.Join(nestedDir, "session.jsonl")
	if err := os.WriteFile(nestedPath, []byte(`{"type":"session","version":3,"id":"nested","timestamp":"2026-08-10T06:05:58.749Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	siblingPath = filepath.Join(projectDir, "2026-08-10T05-59-35-727Z_sibling.jsonl")
	header := fmt.Sprintf(`{"type":"session","version":3,"id":"sibling","timestamp":"2026-08-10T05:59:35.727Z","parentSession":%q}`, parentPath)
	if err := os.WriteFile(siblingPath, []byte(header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir, parentPath, nestedPath, siblingPath
}

func TestPiSubsessionPaths_IncludesSiblingLayout(t *testing.T) {
	_, parentPath, nestedPath, siblingPath := piTestTree(t)

	got := piSubsessionPaths(parentPath)
	want := []string{nestedPath, siblingPath}
	if len(got) != len(want) {
		t.Fatalf("piSubsessionPaths() = %v, want %v", got, want)
	}
	found := make(map[string]bool, len(got))
	for _, p := range got {
		found[p] = true
	}
	for _, p := range want {
		if !found[p] {
			t.Errorf("piSubsessionPaths() missing %s (got %v)", p, got)
		}
	}
}

func TestPiSubsessionPaths_SubsessionHasNoChildren(t *testing.T) {
	_, _, nestedPath, siblingPath := piTestTree(t)

	for _, fp := range []string{nestedPath, siblingPath} {
		if got := piSubsessionPaths(fp); len(got) != 0 {
			t.Errorf("piSubsessionPaths(%s) = %v, want none", fp, got)
		}
	}
}

func TestPiResolveParent_BothLayouts(t *testing.T) {
	_, parentPath, nestedPath, siblingPath := piTestTree(t)

	for _, fp := range []string{nestedPath, siblingPath} {
		gotPath, gotID, ok := piResolveParent(fp)
		if !ok {
			t.Fatalf("piResolveParent(%s) not resolved", fp)
		}
		if gotID != "parent" {
			t.Errorf("piResolveParent(%s) id = %q, want %q", fp, gotID, "parent")
		}
		if absPath(gotPath) != absPath(parentPath) {
			t.Errorf("piResolveParent(%s) path = %q, want %q", fp, gotPath, parentPath)
		}
	}

	if _, _, ok := piResolveParent(parentPath); ok {
		t.Errorf("piResolveParent(%s) resolved a parent, want none", parentPath)
	}
}

func TestPiSubsessionCount_CountsBothLayouts(t *testing.T) {
	_, parentPath, _, _ := piTestTree(t)

	cutoff := time.Now().AddDate(0, 0, -1)
	if got := piSubsessionCount(parentPath, cutoff, ""); got != 2 {
		t.Errorf("piSubsessionCount() = %d, want 2", got)
	}
	// A cutoff in the future excludes everything.
	if got := piSubsessionCount(parentPath, time.Now().Add(time.Hour), ""); got != 0 {
		t.Errorf("piSubsessionCount() with future cutoff = %d, want 0", got)
	}
	// Date filtering applies to both layouts alike.
	today := time.Now().UTC().Format("2006-01-02")
	if got := piSubsessionCount(parentPath, cutoff, today); got != 2 {
		t.Errorf("piSubsessionCount(date=%s) = %d, want 2", today, got)
	}
	if got := piSubsessionCount(parentPath, cutoff, "1999-01-01"); got != 0 {
		t.Errorf("piSubsessionCount(date=1999-01-01) = %d, want 0", got)
	}
}
