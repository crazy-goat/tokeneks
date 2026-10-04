package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
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
	// Ideal is the counterfactual cost with optimal prompt-cache reuse,
	// computed by the same engine totalsByAgent uses (computeSessionPricing
	// in web_store.go) so this number agrees with `total`'s. Overpay is
	// max(TotalCost-Ideal, 0) and OverpayPct is Overpay/Ideal*100.
	Ideal      float64 `json:"ideal"`
	Overpay    float64 `json:"overpay"`
	OverpayPct float64 `json:"overpayPct"`
	// UnpricedTokens/PartiallyUnpriced flag a session containing steps
	// whose model had no resolvable rate — Overpay for those tokens is
	// either assumed zero (when the agent logged a real cost for them) or
	// entirely absent (when it didn't), never a measured figure, so a
	// session with these set should not be read as "0% overpay" without
	// the caveat.
	UnpricedTokens    int    `json:"unpricedTokens,omitempty"`
	PartiallyUnpriced bool   `json:"partiallyUnpriced,omitempty"`
	Messages          int    `json:"messages"`
	ToolCalls         int    `json:"toolCalls"`
	PromptInput       int    `json:"promptInput"`
	ParentID          string `json:"parentId,omitempty"`
	ChildCount        int    `json:"childCount,omitempty"`
	IsSubsession      bool   `json:"isSubsession,omitempty"`
}

var sessionsCache struct {
	mu      sync.Mutex
	data    []WebSession
	err     error
	expires time.Time
	// fromMs/toMs are the exact [fromMs, toMs) window data/err were fetched
	// for. Keying the cache on this rather than a day count means two
	// requests only ever share a cache entry when they asked for the exact
	// same window — see dashboardWindowMs's doc comment for what used to go
	// wrong keying on a guessed day count instead.
	fromMs int64
	toMs   int64
}

func getCachedSessions(fromMs, toMs int64) ([]WebSession, error) {
	sessionsCache.mu.Lock()
	cached := sessionsCache.fromMs == fromMs && sessionsCache.toMs == toMs && time.Now().Before(sessionsCache.expires)
	if cached {
		data := sessionsCache.data
		err := sessionsCache.err
		sessionsCache.mu.Unlock()
		return data, err
	}
	sessionsCache.mu.Unlock()

	data, err := gatherWebSessions(fromMs, toMs)

	sessionsCache.mu.Lock()
	sessionsCache.data = data
	sessionsCache.err = err
	sessionsCache.expires = time.Now().Add(30 * time.Second)
	sessionsCache.fromMs = fromMs
	sessionsCache.toMs = toMs
	sessionsCache.mu.Unlock()

	return data, err
}

// invalidateSessionsCache forces the next getCachedSessions call to
// refetch from the store. Called by the background watcher on every
// ingest event so the dashboard reflects source changes immediately
// instead of waiting for the 30s TTL to expire.
// sessionsStreamBroker manages SSE clients that want to be notified
// when the session list changes. Used by the dashboard main page.
type sessionsStreamBroker struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

func (b *sessionsStreamBroker) subscribe() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 1)
	b.clients[ch] = struct{}{}
	return ch
}

func (b *sessionsStreamBroker) unsubscribe(ch chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, ch)
}

func (b *sessionsStreamBroker) broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

var sessionsStreamBrokerInstance = &sessionsStreamBroker{clients: make(map[chan struct{}]struct{})}

func invalidateSessionsCache() {
	sessionsCache.mu.Lock()
	sessionsCache.expires = time.Time{}
	sessionsCache.mu.Unlock()
	sessionsStreamBrokerInstance.broadcast()
}

func gatherWebSessions(fromMs, toMs int64) ([]WebSession, error) {
	return gatherWebSessionsFromStore(context.Background(), fromMs, toMs)
}

//go:embed web/detail.html
var webDetailHTML []byte

// dashboardWindowMs turns the dashboard's --days default and the optional
// start/end (YYYY-MM-DD) query params from /api/sessions into an explicit
// [fromMs, toMs) millisecond range over session.last_activity — the same
// column and comparison aggregateSessionsFromStore uses for the CLI's own
// --days N, so a dashboard window and the CLI window it's meant to mirror
// select the exact same sessions instead of two approximations of "the same"
// window.
//
// With no start/end at all, this is exactly the CLI's rolling window for the
// same days value: fromMs = now-days*24h, unbounded above. Once start and/or
// end is given, the window becomes calendar-anchored at *local* midnight of
// each date instead — start/end used to only round-trip through
// filterWebSessionsByDateRange (an exact, correct filter) after first
// over-fetching via a *rolling* window guessed to be wide enough
// (`effectiveDays = daysSinceStart + 2`) to contain it. That guessed number
// was never justified, and worse, doubled as the sessions cache's key
// (getCachedSessions), so a request that only differed in `end` — needing a
// wider or narrower range than a previously cached one — could silently
// reuse the wrong cached window. Computing the real range once here and
// threading it through both the fetch and the cache key removes the guess
// entirely.
//
// Local, not UTC: the dashboard's date picker (web/index.html,
// applyQuickRange) builds these YYYY-MM-DD strings from local-midnight
// `Date` objects in the browser, so a string like "2026-06-01" means "June 1
// where the user is sitting", not "June 1 UTC". The dashboard only ever runs
// on localhost, so the browser's local timezone and this server's
// time.Local are the same machine — parsing in time.Local is what makes the
// two sides agree on what day was actually picked. Parsing in UTC instead
// silently shifts the window by the machine's UTC offset (e.g. "today" at
// UTC+2 becomes [02:00 today, 02:00 tomorrow) local time), misattributing
// early-morning sessions to the wrong day.
//
// end is exclusive of the *next* day, i.e. inclusive of all of the named end
// date — a picked "today" must include everything that happened today, not
// stop at local midnight this morning. AddDate(0, 0, 1) rather than
// Add(24*time.Hour) so this stays correct across DST transitions, where a
// local calendar day is not always exactly 24 hours.
func dashboardWindowMs(days int, start, end string) (fromMs, toMs int64) {
	fromMs = time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	toMs = unboundedToMs
	if start != "" {
		if t, err := time.ParseInLocation("2006-01-02", start, time.Local); err == nil {
			fromMs = t.UnixMilli()
		}
	}
	if end != "" {
		if t, err := time.ParseInLocation("2006-01-02", end, time.Local); err == nil {
			toMs = t.AddDate(0, 0, 1).UnixMilli()
		}
	}
	return fromMs, toMs
}

// validatePort accepts only a plain decimal number in the 1..65535 range.
// strconv.Atoi alone would also accept a leading "+", and net.Listen happily
// binds ":+8080", but the dashboard then prints http://localhost:+8080, which
// no client can open, so the digits are checked explicitly (#94). Leading
// zeros stay accepted: "0080" is in range and browsers parse
// http://localhost:0080 as port 80.
func validatePort(port string) error {
	for _, r := range port {
		if r < '0' || r > '9' {
			return invalidPortError(port)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return invalidPortError(port)
	}
	return nil
}

func invalidPortError(port string) error {
	return fmt.Errorf("invalid port %q: must be an integer between 1 and 65535", port)
}

func runWeb(port string, days int) error {
	if err := validatePort(port); err != nil {
		return err
	}
	st, err := openTokeneksStore()
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()
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
	defer func() { _ = w.Close() }()
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
		_, _ = w.Write(webIndexHTML)
	})

	mux.HandleFunc("/detail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(webDetailHTML)
	})

	mux.HandleFunc("/static/chart.umd.min.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(chartJS)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		end := r.URL.Query().Get("end")
		fromMs, toMs := dashboardWindowMs(days, start, end)
		sessions, err := getCachedSessions(fromMs, toMs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body, err := json.Marshal(sessions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=30")
		_, _ = w.Write(append(body, '\n'))
	})

	mux.HandleFunc("/api/sessions-stream", handleAPISessionsStream)
	mux.HandleFunc("/api/session/", handleAPISessionDetail)
	mux.HandleFunc("/api/session-markdown/", handleAPISessionMarkdown)
	mux.HandleFunc("/api/session-stream/", handleAPISessionStream)

	fmt.Printf("Web dashboard running on http://localhost:%s\n", port)
	return http.ListenAndServe(net.JoinHostPort("", port), mux)
}

func handleAPISessionsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := sessionsStreamBrokerInstance.subscribe()
	defer sessionsStreamBrokerInstance.unsubscribe(ch)

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ch:
			_, _ = fmt.Fprint(w, "event: changed\ndata: {}\n\n")
			flusher.Flush()
		}
	}
}
