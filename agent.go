package main

import (
	"context"
	"os"
	"path/filepath"

	"tokeneks/ingest"
	"tokeneks/store"
)

// Agent is everything the rest of the program needs to know about one
// supported AI coding agent. Before this existed as a full description, the
// same three agents were spelled out by hand in five places — the CLI command
// map, agentDisplayName's switch, two copies of []string{"claude","pi",
// "opencode"}, and buildAgentIO's source/parser maps — so adding a fourth
// agent meant finding all five, and the compiler could not tell you when you
// missed one. Now a new agent is one Register call.
type Agent interface {
	// Key is the canonical identifier, used everywhere data is stored or
	// looked up by agent: the store's `agent` column, the pricing specs in
	// buildPricingSpecs, the ingest source/parser lookup, and the
	// /api/session/<agent>/<id> URL path.
	Key() string

	// CommandName is the CLI subcommand, which is not always Key():
	// OpenCode is `tokeneks oc ...` on the command line but "opencode"
	// everywhere else. That mismatch used to be an unexplained difference
	// between the agents map and every other agent list; naming both here
	// makes it a deliberate, visible choice.
	CommandName() string

	// DisplayName is the human-facing name ("OpenCode", "PI", "Claude").
	DisplayName() string

	// List prints a formatted session list to stdout.
	List(days int, date string) error

	// Detail prints detailed session info to stdout.
	// Some agents use days while resolving an input to a session.
	Detail(id string, days int) error

	// Source enumerates this agent's sessions for the ingester.
	Source() ingest.Source

	// Parse reads one session from its source into store form.
	Parse(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error)
}

// AgentRegistry is the single source of truth for which agents exist.
type AgentRegistry struct {
	byKey     map[string]Agent
	byCommand map[string]Agent
	order     []Agent
}

// Register adds an agent. Registration order is preserved by Keys/All so
// output that iterates agents (sync progress, the total command) stays
// stable across runs instead of following Go's randomized map order.
func (r *AgentRegistry) Register(a Agent) {
	if r.byKey == nil {
		r.byKey = make(map[string]Agent)
		r.byCommand = make(map[string]Agent)
	}
	r.byKey[a.Key()] = a
	r.byCommand[a.CommandName()] = a
	r.order = append(r.order, a)
}

// Get returns the agent with the given canonical Key.
func (r *AgentRegistry) Get(key string) (Agent, bool) {
	a, ok := r.byKey[key]
	return a, ok
}

// ByCommand returns the agent bound to the given CLI subcommand.
func (r *AgentRegistry) ByCommand(name string) (Agent, bool) {
	a, ok := r.byCommand[name]
	return a, ok
}

// All returns every registered agent in registration order.
func (r *AgentRegistry) All() []Agent { return r.order }

// Keys returns every agent's canonical Key in registration order — what the
// ingester wants for its Agents field.
func (r *AgentRegistry) Keys() []string {
	keys := make([]string, 0, len(r.order))
	for _, a := range r.order {
		keys = append(keys, a.Key())
	}
	return keys
}

// DisplayName maps a canonical key to its human-facing name, falling back to
// the key itself for anything unregistered (rows written by an older build,
// say) rather than rendering an empty cell.
func (r *AgentRegistry) DisplayName(key string) string {
	if a, ok := r.byKey[key]; ok {
		return a.DisplayName()
	}
	return key
}

// Sources and Parsers project the registry into the two maps the ingester
// and watcher take.
func (r *AgentRegistry) Sources() map[string]ingest.Source {
	m := make(map[string]ingest.Source, len(r.order))
	for _, a := range r.order {
		m[a.Key()] = a.Source()
	}
	return m
}

func (r *AgentRegistry) Parsers() map[string]ingest.Parser {
	m := make(map[string]ingest.Parser, len(r.order))
	for _, a := range r.order {
		m[a.Key()] = a.Parse
	}
	return m
}

// homeJoin builds an absolute path under the user's home directory. A failing
// UserHomeDir yields a relative path that simply finds no sessions, which is
// the same outcome as an agent that isn't installed — the ingester already
// tolerates a source with nothing in it.
func homeJoin(parts ...string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(append([]string{home}, parts...)...)
}

// piAgent adapts PI functions to the Agent interface.
type piAgent struct{}

func (piAgent) Key() string         { return "pi" }
func (piAgent) CommandName() string { return "pi" }
func (piAgent) DisplayName() string { return "PI" }
func (piAgent) List(days int, date string) error {
	return piList(days, date)
}
func (piAgent) Detail(id string, days int) error {
	return piDetail(id, days)
}
func (piAgent) Source() ingest.Source {
	return ingest.NewPiSource(expandHome(defaultPISessions))
}
func (piAgent) Parse(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error) {
	return piParser(ctx, ref)
}

// claudeAgent adapts Claude functions to the Agent interface.
type claudeAgent struct{}

func (claudeAgent) Key() string         { return "claude" }
func (claudeAgent) CommandName() string { return "claude" }
func (claudeAgent) DisplayName() string { return "Claude" }
func (claudeAgent) List(days int, date string) error {
	return claudeList(days, date)
}
func (claudeAgent) Detail(id string, days int) error {
	return claudeDetail(id)
}
func (claudeAgent) Source() ingest.Source {
	return ingest.NewClaudeSource(expandHome(defaultClaudeSessions))
}
func (claudeAgent) Parse(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error) {
	return claudeParser(ctx, ref)
}

// ocAgent adapts OpenCode functions to the Agent interface.
type ocAgent struct{}

func (ocAgent) Key() string         { return "opencode" }
func (ocAgent) CommandName() string { return "oc" }
func (ocAgent) DisplayName() string { return "OpenCode" }
func (ocAgent) List(days int, date string) error {
	return ocList(days, date)
}
func (ocAgent) Detail(id string, days int) error {
	return ocDetail(id)
}
func (ocAgent) Source() ingest.Source {
	return ingest.NewOpenCodeSource(homeJoin(".local", "share", "opencode", "opencode.db"))
}
func (ocAgent) Parse(ctx context.Context, ref ingest.SessionRef) (store.ParsedSession, error) {
	return ocParser(ctx, ref)
}

// agentRegistry is the process-wide registry. Order here is the order agents
// appear in sync progress output and anywhere else the registry is iterated.
var agentRegistry = func() *AgentRegistry {
	r := &AgentRegistry{}
	r.Register(claudeAgent{})
	r.Register(piAgent{})
	r.Register(ocAgent{})
	return r
}()
