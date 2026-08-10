package main

import (
	"sort"
	"testing"
)

// The registry replaced five hand-maintained lists of the same three agents.
// These tests pin the invariants that used to be enforced only by remembering
// to edit all five: that the canonical keys are exactly the ones already
// written into the store's `agent` column and the pricing specs, that no two
// agents collide on a key or a command name, and that every agent is complete
// enough to ingest.

func TestAgentRegistry_KeysMatchStoredAgentNames(t *testing.T) {
	// These are the values the store's `agent` column already holds and
	// buildPricingSpecs is keyed on. Changing a Key() without migrating the
	// store would orphan every existing row for that agent, so this list is
	// deliberately spelled out rather than derived from the registry.
	want := []string{"claude", "opencode", "pi"}

	got := agentRegistry.Keys()
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)

	if len(sorted) != len(want) {
		t.Fatalf("registry keys = %v, want %v", got, want)
	}
	for i := range want {
		if sorted[i] != want[i] {
			t.Fatalf("registry keys = %v (sorted %v), want %v", got, sorted, want)
		}
	}
}

func TestAgentRegistry_KeysMatchPricingSpecs(t *testing.T) {
	specs := buildPricingSpecs()
	for _, key := range agentRegistry.Keys() {
		if _, ok := specs[key]; !ok {
			t.Errorf("agent %q has no pricing spec; its sessions would price at zero", key)
		}
	}
	for key := range specs {
		if _, ok := agentRegistry.Get(key); !ok {
			t.Errorf("pricing spec %q has no registered agent", key)
		}
	}
}

func TestAgentRegistry_NoDuplicateKeysOrCommands(t *testing.T) {
	seenKey := map[string]bool{}
	seenCmd := map[string]bool{}
	for _, a := range agentRegistry.All() {
		if seenKey[a.Key()] {
			t.Errorf("duplicate agent key %q — Register silently overwrote the earlier one", a.Key())
		}
		if seenCmd[a.CommandName()] {
			t.Errorf("duplicate command name %q — one of these CLI subcommands is unreachable", a.CommandName())
		}
		seenKey[a.Key()] = true
		seenCmd[a.CommandName()] = true
	}
}

func TestAgentRegistry_EveryAgentCanIngest(t *testing.T) {
	for _, a := range agentRegistry.All() {
		if a.DisplayName() == "" {
			t.Errorf("agent %q has an empty DisplayName; the dashboard would render a blank cell", a.Key())
		}
		if a.Source() == nil {
			t.Errorf("agent %q has a nil Source; sync would skip it without an error", a.Key())
		}
	}

	// Parse is a method, so it can never be nil — what can go wrong is the
	// projection into the ingester's maps dropping or mis-keying an agent.
	sources, parsers := agentRegistry.Sources(), agentRegistry.Parsers()
	for _, key := range agentRegistry.Keys() {
		if _, ok := sources[key]; !ok {
			t.Errorf("Sources() has no entry for %q", key)
		}
		if _, ok := parsers[key]; !ok {
			t.Errorf("Parsers() has no entry for %q", key)
		}
	}
	if len(sources) != len(agentRegistry.Keys()) || len(parsers) != len(agentRegistry.Keys()) {
		t.Errorf("Sources()/Parsers() sizes = %d/%d, want %d", len(sources), len(parsers), len(agentRegistry.Keys()))
	}
}

// OpenCode is "oc" on the command line but "opencode" everywhere data is
// keyed. That split is intentional, and it is the one place where looking an
// agent up by the wrong name silently finds nothing, so pin both directions.
func TestAgentRegistry_OpenCodeCommandNameDiffersFromKey(t *testing.T) {
	byCmd, ok := agentRegistry.ByCommand("oc")
	if !ok {
		t.Fatal(`ByCommand("oc") found nothing; the CLI's oc subcommand is unwired`)
	}
	if byCmd.Key() != "opencode" {
		t.Fatalf(`ByCommand("oc").Key() = %q, want "opencode"`, byCmd.Key())
	}
	if _, ok := agentRegistry.ByCommand("opencode"); ok {
		t.Error(`ByCommand("opencode") resolved; the canonical key must not double as a CLI command`)
	}
	if _, ok := agentRegistry.Get("oc"); ok {
		t.Error(`Get("oc") resolved; the CLI command name must not double as a canonical key`)
	}
}

func TestAgentRegistry_DisplayNameFallsBackToKey(t *testing.T) {
	if got := agentRegistry.DisplayName("opencode"); got != "OpenCode" {
		t.Errorf(`DisplayName("opencode") = %q, want "OpenCode"`, got)
	}
	// A row written by an older build under a since-removed agent name must
	// still render as something, not as an empty cell.
	if got := agentRegistry.DisplayName("gemini"); got != "gemini" {
		t.Errorf(`DisplayName("gemini") = %q, want the key echoed back`, got)
	}
}

func TestAgentDisplayName_MatchesRegistry(t *testing.T) {
	for _, a := range agentRegistry.All() {
		if got := agentDisplayName(a.Key()); got != a.DisplayName() {
			t.Errorf("agentDisplayName(%q) = %q, want %q", a.Key(), got, a.DisplayName())
		}
	}
}
