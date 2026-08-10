package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokeneks/compute"
	"tokeneks/store"
)

// TestClaudePrices_Sonnet5DatedWindows verifies Sonnet 5's introductory
// pricing ($2/$10) applies to messages before the 2026-09-01 cutover and
// the standard rate ($3/$15) applies after, and that the derived cache
// rates (0.1x/1.25x/2x input) move with the base rate rather than staying
// pinned to whichever window a caller happened to hit first.
func TestClaudePrices_Sonnet5DatedWindows(t *testing.T) {
	withTempStore(t)

	before := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	got, ok := claudeModelPricesAt("claude-sonnet-5", before)
	if !ok {
		t.Fatal("expected a price for claude-sonnet-5 before the cutover")
	}
	want := compute.ModelPrices{
		Input: 2.0, CacheRead: 0.2, CacheCreation: 2.5, CacheCreation1h: 4.0, Output: 10.0, SupportsCacheCreation: true,
	}
	if got != want {
		t.Errorf("before cutover: got %+v, want %+v", got, want)
	}

	after := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	got, ok = claudeModelPricesAt("claude-sonnet-5", after)
	if !ok {
		t.Fatal("expected a price for claude-sonnet-5 after the cutover")
	}
	want = compute.ModelPrices{
		Input: 3.0, CacheRead: 0.3, CacheCreation: 3.75, CacheCreation1h: 6.0, Output: 15.0, SupportsCacheCreation: true,
	}
	if got != want {
		t.Errorf("after cutover: got %+v, want %+v", got, want)
	}
}

// TestClaudePrices_OpenEndedWindowIsTimeless checks that a model with a
// single open-ended window (i.e. everything except Sonnet 5) returns the
// same price regardless of when it's asked, since it has no scheduled
// change.
func TestClaudePrices_OpenEndedWindowIsTimeless(t *testing.T) {
	withTempStore(t)

	times := []time.Time{
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Now(),
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	var first compute.ModelPrices
	for i, at := range times {
		p, ok := claudeModelPricesAt("claude-opus-5", at)
		if !ok {
			t.Fatalf("expected a price for claude-opus-5 at %v", at)
		}
		if i == 0 {
			first = p
			continue
		}
		if p != first {
			t.Errorf("claude-opus-5 price changed across time: %+v vs %+v", p, first)
		}
	}
}

// TestClaudePrices_UnknownModel checks that a model absent from every layer
// reports a miss instead of a silent zero-value price that would otherwise
// be indistinguishable from "this model is free."
func TestClaudePrices_UnknownModel(t *testing.T) {
	withTempStore(t)
	resetClaudeUnknownWarnings()
	prev := claudeUnknownWarnOut
	var buf bytes.Buffer
	claudeUnknownWarnOut = &buf
	t.Cleanup(func() { claudeUnknownWarnOut = prev })

	p, ok := claudeModelPrices("claude-opus-4-99-does-not-exist")
	if ok {
		t.Fatalf("expected ok=false for an unknown model, got price %+v", p)
	}
	if p != (compute.ModelPrices{}) {
		t.Errorf("expected the zero value for an unknown model, got %+v", p)
	}
}

// TestClaudePrices_UnknownModelWarnsOnce checks the warning fires exactly
// once per model per process, not once per lookup — a session can run
// thousands of messages through this path.
func TestClaudePrices_UnknownModelWarnsOnce(t *testing.T) {
	withTempStore(t)
	resetClaudeUnknownWarnings()
	prev := claudeUnknownWarnOut
	var buf bytes.Buffer
	claudeUnknownWarnOut = &buf
	t.Cleanup(func() { claudeUnknownWarnOut = prev })

	for i := 0; i < 5; i++ {
		if _, ok := claudeModelPrices("claude-totally-made-up"); ok {
			t.Fatal("expected ok=false")
		}
	}
	// A different unknown model must still get its own warning.
	claudeModelPrices("claude-also-made-up")

	out := buf.String()
	gotFirst := strings.Count(out, `"claude-totally-made-up"`)
	gotSecond := strings.Count(out, `"claude-also-made-up"`)
	if gotFirst != 1 {
		t.Errorf("claude-totally-made-up warned %d times, want 1:\n%s", gotFirst, out)
	}
	if gotSecond != 1 {
		t.Errorf("claude-also-made-up warned %d times, want 1:\n%s", gotSecond, out)
	}
}

// TestClaudePrices_StoreOverlayWinsAtAnyTime_SingleWindowModel checks that
// for a model with a single, open-ended built-in window (the normal case —
// claude-opus-5 has no scheduled price change), the store overlay still wins
// at any timestamp, exactly as it did before dated windows existed. Syncing
// only has to fight the built-in table on the models where we've explicitly
// encoded history (see TestClaudePrices_MultiWindowBuiltinBeatsStore); this
// covers the far more common case, so it must not regress.
func TestClaudePrices_StoreOverlayWinsAtAnyTime_SingleWindowModel(t *testing.T) {
	st := withTempStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{{
		Provider: "anthropic", Model: "claude-opus-5", Name: "Claude Opus 5",
		Input: 99, Output: 999, CacheRead: 9.9, CacheWrite: 9, CacheWrite1h: 9,
		Source: priceSourceModelsDev, UpdatedAt: 1,
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resetClaudePrices()

	for _, at := range []time.Time{
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Now(),
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, ok := claudeModelPricesAt("claude-opus-5", at)
		if !ok {
			t.Fatalf("expected a price at %v", at)
		}
		if got.Input != 99 {
			t.Errorf("at %v: store overlay ignored, input = %v, want 99", at, got.Input)
		}
	}
}

// TestClaudePrices_MultiWindowBuiltinBeatsStore is the regression test for
// the bug the coordinator caught: claudeModelPricesAt used to consult the
// store overlay unconditionally, before ever looking at the dated built-in
// windows. Since the store already had a synced claude-sonnet-5 row, the
// dated windows were dead code from the moment they shipped, and the bug
// becomes visible (silently repricing every historical session) the moment
// models.dev republishes 3.00/15.00 as "current" after 2026-09-01.
//
// A model only gets more than one window here when the price history is
// known explicitly, so that must outrank a catalog sync that can only ever
// describe today.
func TestClaudePrices_MultiWindowBuiltinBeatsStore(t *testing.T) {
	st := withTempStore(t)

	// Simulate `prices update` having synced today's (post-cutover) rate,
	// same shape as the real store after 2026-09-01.
	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{{
		Provider: "anthropic", Model: "claude-sonnet-5", Name: "Claude Sonnet 5",
		Input: 3.0, Output: 15.0, CacheRead: 0.3, CacheWrite: 3.75, CacheWrite1h: 6.0,
		Source: priceSourceModelsDev, UpdatedAt: 1,
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resetClaudePrices()

	before := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	got, ok := claudeModelPricesAt("claude-sonnet-5", before)
	if !ok {
		t.Fatal("expected a price before the cutover")
	}
	if got.Input != 2.0 {
		t.Errorf("before cutover: input = %v, want 2.0 (built-in intro window must beat the synced store row)", got.Input)
	}

	after := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	got, ok = claudeModelPricesAt("claude-sonnet-5", after)
	if !ok {
		t.Fatal("expected a price after the cutover")
	}
	if got.Input != 3.0 {
		t.Errorf("after cutover: input = %v, want 3.0 (built-in standard window)", got.Input)
	}
}

// TestClaudePrices_JSONBeatsMultiWindowBuiltin checks that the hand-edited
// ~/.tokeneks/claude_models.json overlay still wins over a multi-window
// built-in model — unlike a store sync, it's explicit user intent, so
// nothing below it (built-in or store) should ever override it.
func TestClaudePrices_JSONBeatsMultiWindowBuiltin(t *testing.T) {
	withTempStore(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".tokeneks"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	jsonPath := filepath.Join(home, ".tokeneks", "claude_models.json")
	const body = `{"claude-sonnet-5": {"input": 42, "cacheCreation": 52.5, "cacheRead": 4.2, "output": 210}}`
	if err := os.WriteFile(jsonPath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	resetClaudePrices()

	for _, at := range []time.Time{
		time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	} {
		got, ok := claudeModelPricesAt("claude-sonnet-5", at)
		if !ok {
			t.Fatalf("expected a price at %v", at)
		}
		if got.Input != 42 {
			t.Errorf("at %v: JSON overlay ignored, input = %v, want 42", at, got.Input)
		}
	}
}
