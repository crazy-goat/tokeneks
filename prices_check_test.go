package main

import (
	"context"
	"testing"

	"tokeneks/compute"
)

func checkRowFor(t *testing.T, rows []checkRow, agent, model string) checkRow {
	t.Helper()
	for _, r := range rows {
		if r.Agent == agent && r.Model == model {
			return r
		}
	}
	t.Fatalf("no row for %s/%s in %+v", agent, model, rows)
	return checkRow{}
}

// opencode used to fall back to Kimi K2.6 pricing for any model missing
// from its table — pricing eleven unrelated models at a twelfth model's
// rate, which is what made the Ideal column fiction. A model absent from
// every layer (derived, models.dev, the built-in table) must come back
// MISSING now, never silently priced at some other model's rate.
func TestGatherCheckRows_OpenCodeUnresolvedModelIsMissing(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "opencode", "s1", []testStep{
		loggedStep("GLM 5.2", 10000, 1000, 2000, 1.5), // not in ocModelPrices, and this store has no models.dev sync
	})

	rows, err := gatherCheckRows(context.Background(), 30)
	if err != nil {
		t.Fatalf("gatherCheckRows: %v", err)
	}
	r := checkRowFor(t, rows, "opencode", "GLM 5.2")
	if r.RateSource != "MISSING" {
		t.Errorf("RateSource = %q, want %q (no Kimi K2.6 fallback anymore)", r.RateSource, "MISSING")
	}
	if r.HasComputed {
		t.Error("HasComputed = true, want false — Kimi's rate must not stand in for an unrelated model")
	}
}

// PI has no fallback table, so a model absent from piGlobalModelPrices must
// come back MISSING, not silently priced at some other model's rate.
func TestGatherCheckRows_PIMissingRate(t *testing.T) {
	st := withTempStore(t)

	prevPrices := piPricesFunc
	piPricesFunc = func() map[string]compute.ModelPrices { return map[string]compute.ModelPrices{} }
	t.Cleanup(func() { piPricesFunc = prevPrices })

	ingestTotalTestSession(t, st, "pi", "s1", []testStep{
		loggedStep("Kimi K2.6", 5000, 500, 200, 0.42),
	})

	rows, err := gatherCheckRows(context.Background(), 30)
	if err != nil {
		t.Fatalf("gatherCheckRows: %v", err)
	}
	r := checkRowFor(t, rows, "pi", "Kimi K2.6")
	if r.RateSource != "MISSING" {
		t.Errorf("RateSource = %q, want %q", r.RateSource, "MISSING")
	}
	if r.HasComputed {
		t.Error("HasComputed = true, want false")
	}
}

// The claude agent's logged cost is tokeneks' own recomputation, so
// comparing it against tokeneks' rate table proves nothing — it must be
// flagged Circular and never reported through a numeric error percentage.
func TestGatherCheckRows_ClaudeMarkedCircular(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "claude", "s1", []testStep{
		loggedStep("claude-sonnet-5", 10000, 0, 2000, 0.5),
	})

	rows, err := gatherCheckRows(context.Background(), 30)
	if err != nil {
		t.Fatalf("gatherCheckRows: %v", err)
	}
	r := checkRowFor(t, rows, "claude", "claude-sonnet-5")
	if !r.Circular {
		t.Error("Circular = false, want true for the claude agent")
	}
}

// Groups are sorted by logged cost descending so the biggest mismatches
// surface first.
func TestGatherCheckRows_SortedByLoggedCostDescending(t *testing.T) {
	st := withTempStore(t)

	ingestTotalTestSession(t, st, "pi", "s1", []testStep{
		loggedStep("cheap-model", 1000, 0, 100, 0.10),
	})
	ingestTotalTestSession(t, st, "pi", "s2", []testStep{
		loggedStep("expensive-model", 1000, 0, 100, 9.99),
	})

	rows, err := gatherCheckRows(context.Background(), 30)
	if err != nil {
		t.Fatalf("gatherCheckRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].Model != "expensive-model" || rows[1].Model != "cheap-model" {
		t.Errorf("order = [%s, %s], want [expensive-model, cheap-model]", rows[0].Model, rows[1].Model)
	}
}
