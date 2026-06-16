package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
	"tokeneks/ingest"
)

//go:embed web/index.html
var webIndexHTML []byte

//go:embed web/chart.umd.min.js
var chartJS []byte

type WebModelUsage struct {
	Model      string  `json:"model"`
	Provider   string  `json:"provider"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Cost       float64 `json:"cost"`
	Messages   int     `json:"messages"`
}

type WebSession struct {
	Agent           string          `json:"agent"`
	ID              string          `json:"id"`
	Date            string          `json:"date"`
	Project         string          `json:"project"`
	DominantModel   string          `json:"dominantModel"`
	LastMessage     string          `json:"lastMessage"`
	Models          []WebModelUsage `json:"models"`
	TotalInput      int             `json:"totalInput"`
	TotalOutput     int             `json:"totalOutput"`
	TotalCacheRead  int             `json:"totalCacheRead"`
	TotalCacheWrite int             `json:"totalCacheWrite"`
	TotalCost       float64         `json:"totalCost"`
	Messages        int             `json:"messages"`
	ToolCalls       int             `json:"toolCalls"`
	PromptInput     int             `json:"promptInput"`
	ParentID        string          `json:"parentId,omitempty"`
	ChildCount      int             `json:"childCount,omitempty"`
	IsSubsession    bool            `json:"isSubsession,omitempty"`
}

func piStepWebCost(step piSessionStep) float64 {
	return step.Cost
}

var sessionsCache struct {
	mu         sync.Mutex
	data       []WebSession
	err        error
	expires    time.Time
	cachedDays int
}

func getCachedSessions(days int) ([]WebSession, error) {
	sessionsCache.mu.Lock()
	cached := sessionsCache.cachedDays == days && time.Now().Before(sessionsCache.expires)
	if cached {
		data := sessionsCache.data
		err := sessionsCache.err
		sessionsCache.mu.Unlock()
		return data, err
	}
	sessionsCache.mu.Unlock()

	data, err := gatherWebSessions(days)

	sessionsCache.mu.Lock()
	sessionsCache.data = data
	sessionsCache.err = err
	sessionsCache.expires = time.Now().Add(30 * time.Second)
	sessionsCache.cachedDays = days
	sessionsCache.mu.Unlock()

	return data, err
}

// invalidateSessionsCache forces the next getCachedSessions call to
// refetch from the store. Called by the background watcher on every
// ingest event so the dashboard reflects source changes immediately
// instead of waiting for the 30s TTL to expire.
func invalidateSessionsCache() {
	sessionsCache.mu.Lock()
	sessionsCache.cachedDays = -1
	sessionsCache.expires = time.Time{}
	sessionsCache.mu.Unlock()
}

func gatherWebSessions(days int) ([]WebSession, error) {
	return gatherWebSessionsFromStore(context.Background(), days)
}

//go:embed web/detail.html
var webDetailHTML []byte

func parseWebSessionTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse("2006-01-02 15:04:05", value); err == nil {
		return ts, true
	}
	if ts, err := time.Parse("2006-01-02 15:04", value); err == nil {
		return ts, true
	}
	return time.Time{}, false
}

func filterWebSessionsByDateRange(sessions []WebSession, start, end string) []WebSession {
	if start == "" && end == "" {
		return sessions
	}
	var startTime, endTime time.Time
	var err error
	if start != "" {
		startTime, err = time.Parse("2006-01-02", start)
		if err != nil {
			return sessions
		}
	}
	if end != "" {
		endTime, err = time.Parse("2006-01-02", end)
		if err != nil {
			return sessions
		}
		endTime = endTime.Add(24*time.Hour - time.Nanosecond)
	}
	filtered := make([]WebSession, 0, len(sessions))
	for _, s := range sessions {
		dateTS, dateOK := parseWebSessionTime(s.Date)
		lastTS, lastOK := parseWebSessionTime(s.LastMessage)
		if !dateOK && !lastOK {
			continue
		}
		matchesRange := func(ts time.Time) bool {
			if ts.IsZero() {
				return false
			}
			if !startTime.IsZero() && ts.Before(startTime) {
				return false
			}
			if !endTime.IsZero() && ts.After(endTime) {
				return false
			}
			return true
		}
		if !matchesRange(dateTS) && !matchesRange(lastTS) {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func runWeb(port string, days int) error {
	st, err := openTokeneksStore()
	if err != nil {
		return err
	}
	defer st.Close()
	setTokeneksStore(st)

	// Initial ingest is handled by the watcher's initial sync (see
	// Watcher.Run). HTTP server comes up immediately; data appears as
	// soon as the watcher's initial sync completes.
	sources, parsers := buildAgentIO()

	// Background watcher keeps the store in sync with source files so
	// the SSE-driven detail auto-refresh fires and the main-page cache
	// gets invalidated as soon as sessions change.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := ingest.NewWatcher(st, sources, parsers, ingest.WatcherConfig{
		Logger: log.New(os.Stderr, "[web-watch] ", log.LstdFlags),
	})
	defer w.Close()
	go func() {
		if err := w.Run(ctx); err != nil {
			log.Printf("web watcher stopped: %v", err)
		}
	}()
	go func() {
		for {
			select {
			case ev := <-w.Events():
				log.Printf("[web-watch] %s %s/%s", ev.Kind, ev.Agent, ev.SessionID)
				invalidateSessionsCache()
			case <-ctx.Done():
				return
			}
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webIndexHTML)
	})

	mux.HandleFunc("/detail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webDetailHTML)
	})

	mux.HandleFunc("/static/chart.umd.min.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(chartJS)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		end := r.URL.Query().Get("end")
		effectiveDays := days
		if start != "" {
			if startTime, err := time.Parse("2006-01-02", start); err == nil {
				effectiveDays = int(time.Since(startTime).Hours()/24) + 2
				if effectiveDays < 1 {
					effectiveDays = 1
				}
			}
		}
		sessions, err := getCachedSessions(effectiveDays)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sessions = filterWebSessionsByDateRange(sessions, start, end)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=30")
		json.NewEncoder(w).Encode(sessions)
	})

	mux.HandleFunc("/api/session/", handleAPISessionDetail)
	mux.HandleFunc("/api/session-stream/", handleAPISessionStream)

	fmt.Printf("Web dashboard running on http://localhost:%s\n", port)
	return http.ListenAndServe(":"+port, mux)
}
