package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"tokeneks/store"
)

// modelsDevURL is the pricing catalog tokeneks syncs from. It publishes the
// same model IDs the agents write into their logs (claude-opus-5,
// claude-sonnet-4-6, ...), so rows join to message.model without mapping.
// A var rather than a const so tests can point it at a local server.
var modelsDevURL = "https://models.dev/api.json"

const priceSourceModelsDev = "models.dev"

// priceSourceDerived marks rows `prices derive` fitted from an agent's own
// logged costs rather than fetched from a catalog. See prices_derive.go.
const priceSourceDerived = "derived"

// modelsDevAPI is the subset of models.dev/api.json tokeneks reads.
type modelsDevAPI map[string]struct {
	Name   string `json:"name"`
	Models map[string]struct {
		Name string `json:"name"`
		Cost struct {
			Input      float64 `json:"input"`
			Output     float64 `json:"output"`
			CacheRead  float64 `json:"cache_read"`
			CacheWrite float64 `json:"cache_write"`
		} `json:"cost"`
	} `json:"models"`
}

// fetchModelsDevPrices downloads the catalog and flattens it to price rows.
//
// models.dev publishes a single cache_write figure, which is the 5-minute-TTL
// rate. The 1-hour rate is a fixed 2x base input across Anthropic's table
// (vs 1.25x for 5m), so it's derived here rather than left at zero — the
// agents' logs do distinguish the two TTLs.
func fetchModelsDevPrices(ctx context.Context) ([]store.ModelPrice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", modelsDevURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", modelsDevURL, resp.StatusCode)
	}

	var api modelsDevAPI
	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
		return nil, fmt.Errorf("decode %s: %w", modelsDevURL, err)
	}

	now := time.Now().UnixMilli()
	var out []store.ModelPrice
	for providerID, provider := range api {
		for modelID, model := range provider.Models {
			c := model.Cost
			if c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0 {
				continue // free/unpriced model — nothing to record
			}
			cacheWrite1h := 0.0
			if c.Input > 0 {
				cacheWrite1h = c.Input * 2
			}
			out = append(out, store.ModelPrice{
				Provider:     providerID,
				Model:        modelID,
				Name:         model.Name,
				Input:        c.Input,
				Output:       c.Output,
				CacheRead:    c.CacheRead,
				CacheWrite:   c.CacheWrite,
				CacheWrite1h: cacheWrite1h,
				Source:       priceSourceModelsDev,
				UpdatedAt:    now,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// runPricesUpdate fetches the catalog and upserts it into the store.
// An empty provider syncs every provider models.dev knows about.
func runPricesUpdate(provider string) error {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return err
		}
		setTokeneksStore(st)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("fetching %s ...\n", modelsDevURL)
	prices, err := fetchModelsDevPrices(ctx)
	if err != nil {
		return err
	}

	if provider != "" {
		filtered := prices[:0:0]
		for _, p := range prices {
			if p.Provider == provider {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("no models for provider %q — check the provider id at %s", provider, modelsDevURL)
		}
		prices = filtered
	}

	if err := st.UpsertModelPrices(ctx, prices); err != nil {
		return fmt.Errorf("store prices: %w", err)
	}

	providers := make(map[string]int)
	for _, p := range prices {
		providers[p.Provider]++
	}
	fmt.Printf("stored %d model prices across %d provider(s)\n", len(prices), len(providers))

	// The anthropic rows are the ones the claude commands price against, so
	// show them rather than making the user run `prices list` to see if the
	// sync did anything useful.
	if n, ok := providers["anthropic"]; ok {
		fmt.Printf("  anthropic: %d models\n", n)
	}

	// Claude, pi and opencode prices are all cached per process; drop the
	// memos so a later command in the same run doesn't keep using the
	// pre-sync table — this sync can change the models.dev "opencode"
	// provider rows resolveAgentPrices reads (see prices_resolve.go).
	resetClaudePrices()
	resetResolvedPrices()
	return nil
}

// runPricesList prints what the store currently holds.
func runPricesList(provider string) error {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return err
		}
		setTokeneksStore(st)
	}

	prices, err := st.GetModelPrices(context.Background(), provider)
	if err != nil {
		return err
	}
	if len(prices) == 0 {
		fmt.Println("no prices stored — run `tokeneks prices update` first")
		return nil
	}

	// Width 18 rather than 14: derived rows are namespaced "derived:pi" /
	// "derived:opencode" (see derivedProvider in prices_derive.go) specifically
	// so they can't collide with a models.dev provider id, and that prefix
	// makes the longest provider string longer than any plain models.dev id.
	fmt.Printf("%-18s  %-32s  %8s  %8s  %8s  %9s  %9s  %-12s  %s\n",
		"provider", "model", "in/M", "out/M", "cr/M", "cw5m/M", "cw1h/M", "source", "updated")
	for _, p := range prices {
		updated := time.UnixMilli(p.UpdatedAt).Format("2006-01-02")
		fmt.Printf("%-18s  %-32s  %8.2f  %8.2f  %8.2f  %9.2f  %9.2f  %-12s  %s\n",
			p.Provider, p.Model, p.Input, p.Output, p.CacheRead,
			p.CacheWrite, p.CacheWrite1h, p.Source, updated)
	}
	fmt.Printf("\n%d model(s)\n", len(prices))
	return nil
}
