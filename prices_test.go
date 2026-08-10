package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"tokeneks/store"
)

// withTempStore points the global store singleton at a fresh database for the
// duration of one test, restoring whatever was there before.
func withTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	prev := getTokeneksStore()
	setTokeneksStore(st)
	t.Cleanup(func() {
		setTokeneksStore(prev)
		_ = st.Close()
		resetClaudePrices()
		resetResolvedPrices()
	})
	resetClaudePrices()
	resetResolvedPrices()
	return st
}

func TestClaudePrices_StoreOverlaysBuiltinTable(t *testing.T) {
	st := withTempStore(t)

	// claude-opus-4-6 is deliberately absent from the built-in table — before
	// a sync it has no price at all, which is what makes sessions on it drop
	// out of reports.
	if p := claudeGlobalModelPrices()["claude-opus-4-6"]; p.Input != 0 {
		t.Fatalf("expected no built-in price for claude-opus-4-6, got %+v", p)
	}

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{{
		Provider: "anthropic", Model: "claude-opus-4-6", Name: "Claude Opus 4.6",
		Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25, CacheWrite1h: 10,
		Source: priceSourceModelsDev, UpdatedAt: 1,
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resetClaudePrices()

	got := claudeGlobalModelPrices()["claude-opus-4-6"]
	if got.Input != 5 || got.Output != 25 || got.CacheRead != 0.5 || got.CacheCreation != 6.25 {
		t.Errorf("synced price not applied: %+v", got)
	}
	if !got.SupportsCacheCreation {
		t.Error("SupportsCacheCreation should be true when cache_write > 0")
	}
}

func TestClaudePrices_StoreBeatsBuiltinDefault(t *testing.T) {
	st := withTempStore(t)

	if got := claudeGlobalModelPrices()["claude-opus-5"].Input; got != 5.0 {
		t.Fatalf("built-in claude-opus-5 input = %v, want 5.0", got)
	}

	// A price change upstream must win over the compiled-in figure, otherwise
	// syncing is pointless for models that are already in the table.
	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{{
		Provider: "anthropic", Model: "claude-opus-5",
		Input: 7, Output: 35, CacheRead: 0.7, CacheWrite: 8.75, CacheWrite1h: 14,
		Source: priceSourceModelsDev, UpdatedAt: 2,
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resetClaudePrices()

	if got := claudeGlobalModelPrices()["claude-opus-5"].Input; got != 7 {
		t.Errorf("store price ignored: input = %v, want 7", got)
	}
}

func TestClaudePrices_NonAnthropicRowsIgnored(t *testing.T) {
	st := withTempStore(t)

	err := st.UpsertModelPrices(context.Background(), []store.ModelPrice{{
		Provider: "moonshotai", Model: "kimi-k2.6",
		Input: 0.95, Output: 4, CacheRead: 0.16,
		Source: priceSourceModelsDev, UpdatedAt: 3,
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resetClaudePrices()

	if _, ok := claudeGlobalModelPrices()["kimi-k2.6"]; ok {
		t.Error("non-anthropic model leaked into the Claude price table")
	}
}

func TestFetchModelsDevPrices_DerivesHourlyCacheWrite(t *testing.T) {
	// models.dev publishes only the 5m cache-write rate; the 1h rate is 2x
	// base input, so it has to be derived at fetch time.
	body := map[string]any{
		"anthropic": map[string]any{
			"name": "Anthropic",
			"models": map[string]any{
				"claude-opus-5": map[string]any{
					"name": "Claude Opus 5",
					"cost": map[string]any{
						"input": 5.0, "output": 25.0, "cache_read": 0.5, "cache_write": 6.25,
					},
				},
				"free-model": map[string]any{
					"name": "Free",
					"cost": map[string]any{"input": 0.0, "output": 0.0},
				},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	prev := modelsDevURL
	modelsDevURL = srv.URL
	defer func() { modelsDevURL = prev }()

	prices, err := fetchModelsDevPrices(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("expected the unpriced model to be skipped, got %d rows: %+v", len(prices), prices)
	}
	p := prices[0]
	if p.Model != "claude-opus-5" || p.Provider != "anthropic" {
		t.Fatalf("unexpected row: %+v", p)
	}
	if p.CacheWrite != 6.25 {
		t.Errorf("CacheWrite = %v, want 6.25 (the published 5m rate)", p.CacheWrite)
	}
	if p.CacheWrite1h != 10 {
		t.Errorf("CacheWrite1h = %v, want 10 (2x base input)", p.CacheWrite1h)
	}
	if p.UpdatedAt == 0 {
		t.Error("UpdatedAt not stamped")
	}
}
