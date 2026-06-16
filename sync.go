package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"tokeneks/ingest"
	"tokeneks/store"
)

const defaultTokeneksDB = "~/.local/share/tokeneks/tokeneks.db"

// runSync performs a full ingest and (optionally) starts a watcher that
// re-ingests sessions on file change.
func runSync(watch bool) error {
	st, err := openTokeneksStore()
	if err != nil {
		return err
	}
	defer st.Close()

	sources, parsers := buildAgentIO()

	dotCount := 0
	progress := func(agent string, current, total int) {
		if dotCount == 0 {
			fmt.Printf("  %s: ", agent)
		}
		fmt.Print(".")
		dotCount++
		if current == total {
			fmt.Printf(" %d/%d\n", current, total)
			dotCount = 0
		}
	}

	ing := &ingest.Ingestor{
		Store:       st,
		Agents:      []string{"claude", "pi", "opencode"},
		SourceFor:   sources,
		ParserFor:   parsers,
		Log:         log.New(os.Stderr, "[sync] ", log.LstdFlags),
		OnProgress:  progress,
	}
	res, err := ing.Sync(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("sync: discovered=%d ingested=%d skipped=%d errors=%d\n", res.Discovered, res.Ingested, res.Skipped, res.Errors)

	if !watch {
		return nil
	}

	w := ingest.NewWatcher(st, sources, parsers, ingest.WatcherConfig{
		Logger: log.New(os.Stderr, "[watch] ", log.LstdFlags),
	})
	defer w.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		for ev := range w.Events() {
			fmt.Printf("[watch] %s %s/%s\n", ev.Kind, ev.Agent, ev.SessionID)
		}
	}()

	fmt.Println("watch: monitoring for changes (Ctrl-C to stop)")
	return w.Run(ctx)
}

func openTokeneksStore() (*store.Store, error) {
	path := expandHome(defaultTokeneksDB)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	return store.Open(path)
}

func buildAgentIO() (map[string]ingest.Source, map[string]ingest.Parser) {
	home, _ := os.UserHomeDir()
	claudeRoot := filepath.Join(home, ".claude", "projects")
	piRoot := filepath.Join(home, ".pi", "agent", "sessions")
	ocDB := filepath.Join(home, ".local", "share", "opencode", "opencode.db")

	sources := map[string]ingest.Source{
		"claude":   ingest.NewClaudeSource(claudeRoot),
		"pi":       ingest.NewPiSource(piRoot),
		"opencode": ingest.NewOpenCodeSource(ocDB),
	}
	parsers := map[string]ingest.Parser{
		"claude":   claudeParser,
		"pi":       piParser,
		"opencode": ocParser,
	}
	return sources, parsers
}
