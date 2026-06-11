package main

// Agent provides session listing and details for a supported AI provider.
type Agent interface {
	// Name returns the provider name (e.g., "PI", "Claude", "OpenCode").
	Name() string

	// List prints a formatted session list to stdout.
	List(days int, date string) error

	// Detail prints detailed session info to stdout.
	// Some agents use days while resolving an input to a session.
	Detail(id string, days int) error
}

// piAgent adapts PI functions to the Agent interface.
type piAgent struct{}

func (piAgent) Name() string { return "PI" }
func (piAgent) List(days int, date string) error {
	return piList(days, date)
}
func (piAgent) Detail(id string, days int) error {
	return piDetail(id, days)
}

// claudeAgent adapts Claude functions to the Agent interface.
type claudeAgent struct{}

func (claudeAgent) Name() string { return "Claude" }
func (claudeAgent) List(days int, date string) error {
	return claudeList(days, date)
}
func (claudeAgent) Detail(id string, days int) error {
	return claudeDetail(id)
}

// ocAgent adapts OpenCode functions to the Agent interface.
type ocAgent struct{}

func (ocAgent) Name() string { return "OpenCode" }
func (ocAgent) List(days int, date string) error {
	return ocList(days, date)
}
func (ocAgent) Detail(id string, days int) error {
	return ocDetail(id)
}

var agents = map[string]Agent{
	"oc":     ocAgent{},
	"pi":     piAgent{},
	"claude": claudeAgent{},
}
