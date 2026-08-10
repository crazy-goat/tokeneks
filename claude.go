package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"tokeneks/compute"
	"tokeneks/store"
)

// defaultClaudeSessions is a var, not a const, so tests can point it at a
// temporary directory instead of the real ~/.claude/projects.
var defaultClaudeSessions = "~/.claude/projects"

const defaultClaudePricing = "~/.tokeneks/claude_models.json"

var (
	claudePricesMu   sync.Mutex
	claudePricesOnce sync.Once
	// claudeStoreOverlay and claudeJSONOverlay are the memoized sync/user
	// overlays, kept separate (rather than pre-merged into one map) because
	// they sit at different precedence relative to claudeBuiltinPriceWindows
	// — see claudeModelPricesAt.
	claudeStoreOverlay map[string]compute.ModelPrices
	claudeJSONOverlay  map[string]compute.ModelPrices
)

// claudeUnknownWarnOut is where the once-per-model "no price" warning goes.
// It's a var, not a direct os.Stderr call, so tests can capture it instead
// of writing to real stderr, and so production code has one obvious place
// that must never become stdout — stdout carries reports and must stay
// parseable.
var claudeUnknownWarnOut io.Writer = os.Stderr

var (
	claudeUnknownWarnMu sync.Mutex
	claudeUnknownWarned = map[string]bool{}
)

type claudePricesFile map[string]struct {
	Input         float64 `json:"input"`
	CacheCreation float64 `json:"cacheCreation"`
	// CacheCreation1h is a pointer so a missing key can default to 2x Input
	// (Anthropic's 1h-TTL rate) instead of silently pricing 1h writes at 0.
	CacheCreation1h *float64 `json:"cacheCreation1h"`
	CacheRead       float64  `json:"cacheRead"`
	Output          float64  `json:"output"`
}

// claudeGlobalModelPrices returns the effective Claude price table resolved
// at the current moment, applying the same precedence as claudeModelPricesAt
// to every model either layer knows about.
//
// This exists for the many callers outside this file that only want "the
// price right now" (e.g. a reference table) and can't easily be changed
// here. Anything pricing an actual message should prefer claudeModelPrices
// / claudeModelPricesAt below, which also report unknown models instead of
// silently resolving to the zero value.
func claudeGlobalModelPrices() map[string]compute.ModelPrices {
	now := time.Now()
	store := claudeStoreOverlayPrices()
	jsonOverlay := claudeJSONOverlayPrices()

	models := make(map[string]bool, len(claudeBuiltinPriceWindows)+len(store)+len(jsonOverlay))
	for model := range claudeBuiltinPriceWindows {
		models[model] = true
	}
	for model := range store {
		models[model] = true
	}
	for model := range jsonOverlay {
		models[model] = true
	}

	out := make(map[string]compute.ModelPrices, len(models))
	for model := range models {
		// Every model here is present in at least one layer, so this can
		// only warn (and return !ok) if that turns out false — it never
		// does in practice, since the loop is bounded by those same layers.
		if p, ok := claudeModelPricesAt(model, now); ok {
			out[model] = p
		}
	}
	return out
}

// claudeModelPrices resolves a single model's price at the current moment.
// The bool return distinguishes "no price configured" from a legitimately
// zero rate, which a plain map lookup can't — that ambiguity is exactly
// what let unknown models get priced at $0 without anyone noticing.
func claudeModelPrices(model string) (compute.ModelPrices, bool) {
	return claudeModelPricesAt(model, time.Now())
}

// claudeModelPricesAt resolves model's price as of "at", so a historical
// message keeps the rate that was in effect when it was created even after
// the built-in table gains a new dated window (e.g. Sonnet 5's introductory
// pricing expiring on 2026-09-01). On a miss it warns once per model per
// process (see warnUnknownClaudeModel) rather than returning a price that
// looks real but isn't.
//
// Precedence is JSON > (multi-window built-in) > store > (single-window
// built-in):
//
//   - The user's ~/.tokeneks/claude_models.json always wins — it's explicit,
//     hand-entered intent, not an automatic sync.
//   - A built-in entry with more than one window is only ever written when
//     we know that model's price history explicitly (e.g. Sonnet 5's
//     announced 2026-09-01 change), so that knowledge outranks the store.
//     models.dev (the store's source) publishes only the currently-active
//     rate — it has no way to express "this was the rate last month" — so
//     letting a synced row win here would silently reprice every past
//     session the next time `prices update` runs and picks up the new
//     current rate. That's not a future hazard: it already happens the
//     moment models.dev updates.
//   - Otherwise (a single, open-ended window, the normal case) the store
//     keeps winning as before — that's what makes syncing useful for models
//     missing or stale in the built-in table.
func claudeModelPricesAt(model string, at time.Time) (compute.ModelPrices, bool) {
	if p, ok := claudeJSONOverlayPrices()[model]; ok {
		return p, true
	}

	windows := claudeBuiltinPriceWindows[model]
	if len(windows) > 1 {
		if p, ok := resolveClaudeWindow(windows, at); ok {
			return p, true
		}
	}

	if p, ok := claudeStoreOverlayPrices()[model]; ok {
		return p, true
	}

	if p, ok := resolveClaudeWindow(windows, at); ok {
		return p, true
	}

	warnUnknownClaudeModel(model)
	return compute.ModelPrices{}, false
}

// warnUnknownClaudeModel prints one warning per unknown model per process.
// Sessions run thousands of messages through price lookups, so without the
// dedup this would flood stderr for a single unpriced model.
func warnUnknownClaudeModel(model string) {
	claudeUnknownWarnMu.Lock()
	defer claudeUnknownWarnMu.Unlock()
	if claudeUnknownWarned[model] {
		return
	}
	claudeUnknownWarned[model] = true
	fmt.Fprintf(claudeUnknownWarnOut, "warning: no price for Claude model %q; its tokens are excluded from cost\n", model)
}

// resetClaudeUnknownWarnings clears the warned-once set. Test-only: without
// it, whichever test looks up an unknown model first would swallow the
// warning for every later test that reuses the same model name.
func resetClaudeUnknownWarnings() {
	claudeUnknownWarnMu.Lock()
	defer claudeUnknownWarnMu.Unlock()
	claudeUnknownWarned = map[string]bool{}
}

// resetClaudePrices drops the memoized overlays so the next lookup rebuilds
// them. Called after a price sync, which would otherwise not be visible to a
// command running in the same process.
func resetClaudePrices() {
	claudePricesMu.Lock()
	defer claudePricesMu.Unlock()
	claudePricesOnce = sync.Once{}
	claudeStoreOverlay = nil
	claudeJSONOverlay = nil
}

// claudeStoreOverlayPrices returns the memoized prices synced into the store
// by `tokeneks prices update`, building the overlay on first use.
func claudeStoreOverlayPrices() map[string]compute.ModelPrices {
	claudePricesMu.Lock()
	defer claudePricesMu.Unlock()
	claudePricesOnce.Do(claudeInitOverlaysLocked)
	return claudeStoreOverlay
}

// claudeJSONOverlayPrices returns the memoized ~/.tokeneks/claude_models.json
// overlay, building it on first use.
func claudeJSONOverlayPrices() map[string]compute.ModelPrices {
	claudePricesMu.Lock()
	defer claudePricesMu.Unlock()
	claudePricesOnce.Do(claudeInitOverlaysLocked)
	return claudeJSONOverlay
}

// claudeInitOverlaysLocked populates claudeStoreOverlay and claudeJSONOverlay.
// Both come from initClaudeOverlays in one pass so a single sync.Once covers
// both — they're read together often enough (claudeModelPricesAt checks JSON
// first, then possibly falls through to store) that two independent Onces
// would just mean two locks for no benefit. Caller must hold claudePricesMu.
func claudeInitOverlaysLocked() {
	claudeStoreOverlay, claudeJSONOverlay = initClaudeOverlays()
}

// claudeStorePrices reads synced anthropic prices out of the store. Returns
// nil when the store is unavailable or has never been synced — callers fall
// back to the built-in table.
//
// This opens the store if no command did so already, so that pricing is the
// same whether you came in through `claude list` (which syncs first) or
// `claude detail <file>` (which reads a file directly).
func claudeStorePrices() map[string]compute.ModelPrices {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return nil
		}
		setTokeneksStore(st)
	}
	rows, err := st.GetModelPrices(context.Background(), "anthropic")
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make(map[string]compute.ModelPrices, len(rows))
	for _, r := range rows {
		out[r.Model] = compute.ModelPrices{
			Input:                 r.Input,
			CacheCreation:         r.CacheWrite,
			CacheCreation1h:       r.CacheWrite1h,
			CacheRead:             r.CacheRead,
			Output:                r.Output,
			SupportsCacheCreation: r.CacheWrite > 0,
		}
	}
	return out
}

// claudePriceWindow is one dated slice of a model's price history. From is
// inclusive, To is exclusive; a zero Time means unbounded on that side. Most
// models have exactly one, open-ended window — only models with an
// announced future price change (Sonnet 5's introductory rate) need more
// than one.
type claudePriceWindow struct {
	from, to time.Time
	prices   compute.ModelPrices
}

func (w claudePriceWindow) covers(at time.Time) bool {
	if !w.from.IsZero() && at.Before(w.from) {
		return false
	}
	if !w.to.IsZero() && !at.Before(w.to) {
		return false
	}
	return true
}

// resolveClaudeWindow picks the window covering "at" out of an ordered list.
func resolveClaudeWindow(windows []claudePriceWindow, at time.Time) (compute.ModelPrices, bool) {
	for _, w := range windows {
		if w.covers(at) {
			return w.prices, true
		}
	}
	return compute.ModelPrices{}, false
}

// claudeSonnet5PriceChange is when Sonnet 5's introductory pricing
// ($2 in / $10 out) reverts to standard ($3 in / $15 out). Anthropic
// announced this ahead of time, so it's hardcoded rather than something
// `prices update` can discover — models.dev only ever publishes the
// currently-active rate (see claudeModelPricesAt's comment on the overlay).
var claudeSonnet5PriceChange = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// claudeBuiltinPriceWindows is the compiled-in Claude price table, keyed by
// model, each with its dated windows oldest-first. CacheCreation1h is
// always 2x Input (Anthropic's 1-hour cache-write multiplier) and
// CacheCreation is 1.25x Input (the 5-minute one); CacheRead is 0.1x Input.
var claudeBuiltinPriceWindows = map[string][]claudePriceWindow{
	"claude-fable-5": {{prices: compute.ModelPrices{
		Input: 10.0, CacheCreation: 12.5, CacheCreation1h: 20.0, CacheRead: 1.0, Output: 50.0, SupportsCacheCreation: true,
	}}},
	"claude-opus-5": {{prices: compute.ModelPrices{
		Input: 5.0, CacheCreation: 6.25, CacheCreation1h: 10.0, CacheRead: 0.5, Output: 25.0, SupportsCacheCreation: true,
	}}},
	"claude-opus-4-8": {{prices: compute.ModelPrices{
		Input: 5.0, CacheCreation: 6.25, CacheCreation1h: 10.0, CacheRead: 0.5, Output: 25.0, SupportsCacheCreation: true,
	}}},
	"claude-opus-4-7": {{prices: compute.ModelPrices{
		Input: 5.0, CacheCreation: 6.25, CacheCreation1h: 10.0, CacheRead: 0.5, Output: 25.0, SupportsCacheCreation: true,
	}}},
	"claude-sonnet-5": {
		{
			to: claudeSonnet5PriceChange,
			prices: compute.ModelPrices{
				Input: 2.0, CacheCreation: 2.5, CacheCreation1h: 4.0, CacheRead: 0.2, Output: 10.0, SupportsCacheCreation: true,
			},
		},
		{
			from: claudeSonnet5PriceChange,
			prices: compute.ModelPrices{
				Input: 3.0, CacheCreation: 3.75, CacheCreation1h: 6.0, CacheRead: 0.3, Output: 15.0, SupportsCacheCreation: true,
			},
		},
	},
	"claude-sonnet-4-6": {{prices: compute.ModelPrices{
		Input: 3.0, CacheCreation: 3.75, CacheCreation1h: 6.0, CacheRead: 0.3, Output: 15.0, SupportsCacheCreation: true,
	}}},
	"claude-haiku-4-5-20251001": {{prices: compute.ModelPrices{
		Input: 1.0, CacheCreation: 1.25, CacheCreation1h: 2.0, CacheRead: 0.1, Output: 5.0, SupportsCacheCreation: true,
	}}},
	"claude-haiku-4-5": {{prices: compute.ModelPrices{
		Input: 1.0, CacheCreation: 1.25, CacheCreation1h: 2.0, CacheRead: 0.1, Output: 5.0, SupportsCacheCreation: true,
	}}},
}

// initClaudeOverlays builds the store and JSON overlay layers. They're
// returned separately, not merged, because claudeModelPricesAt gives them
// different precedence relative to the built-in table (see its doc comment):
// JSON always wins, but the store can lose to a built-in entry that already
// knows its own price history.
func initClaudeOverlays() (store, jsonOverlay map[string]compute.ModelPrices) {
	store = claudeStorePrices()

	b, err := os.ReadFile(expandHome(defaultClaudePricing))
	if err != nil {
		return store, nil
	}

	var file claudePricesFile
	if err := json.Unmarshal(b, &file); err != nil {
		return store, nil
	}

	jsonOverlay = make(map[string]compute.ModelPrices, len(file))
	for model, p := range file {
		// A user's override file predates the 1h-TTL split; default it to
		// 2x input rather than 0, or it would silently reintroduce the
		// underpricing bug for every model listed there.
		cc1h := 2 * p.Input
		if p.CacheCreation1h != nil {
			cc1h = *p.CacheCreation1h
		}
		jsonOverlay[model] = compute.ModelPrices{
			Input:                 p.Input,
			CacheCreation:         p.CacheCreation,
			CacheCreation1h:       cc1h,
			CacheRead:             p.CacheRead,
			Output:                p.Output,
			SupportsCacheCreation: p.CacheCreation > 0,
		}
	}
	return store, jsonOverlay
}

type claudeMessage struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			// CacheCreation splits the total above by TTL; only the 1h slice
			// is needed since 5m is whatever remains of the total.
			CacheCreation struct {
				Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"message"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

type claudeSessionStep struct {
	Model string
	Step  compute.StepData
	// CreatedAt is this message's own timestamp (ms epoch). It's threaded
	// through to sessionStep so claudeDetail/claudeList can run the
	// ideal-cache pass once over the session's full, untouched step order and
	// price each row at its own model afterward — see sessionStep.CreatedAt
	// (web_store.go) and computeSessionPricing's doc comment for why
	// splitting by model before running the ideal pass is wrong.
	CreatedAt int64
}

type claudeMessageResult struct {
	Steps          []claudeSessionStep
	Models         []string
	ToolCalls      int
	LastUserPrompt string
	LastActivity   time.Time
}

func claudeMessages(fp string) (claudeMessageResult, error) {
	f, err := os.Open(fp)
	if err != nil {
		return claudeMessageResult{}, err
	}
	defer f.Close()

	var steps []claudeSessionStep
	var models []string
	var toolCalls int
	var lastUserPrompt string
	var lastActivity time.Time
	// stepIndexByID collapses the several JSONL lines Claude Code writes for
	// one assistant message (one line per content block, each repeating the
	// identical message.usage) into the single step web_detail.go's
	// msgIndexByID already produces. Without it, a message with N content
	// blocks got its usage counted N times — see the bug this fixes.
	stepIndexByID := make(map[string]int)
	scanner := newJSONLScanner(f)

	for scanner.Scan() {
		var msg claudeMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		ts, tsErr := parseTimestamp(msg.Timestamp)
		if tsErr == nil && ts.After(lastActivity) {
			lastActivity = ts
		}
		if msg.Type == "user" {
			continue
		}
		if msg.Type != "assistant" || msg.Message.Model == "" {
			continue
		}
		if msg.Message.Usage.InputTokens+msg.Message.Usage.CacheCreationInputTokens+
			msg.Message.Usage.CacheReadInputTokens+msg.Message.Usage.OutputTokens == 0 {
			// A zero-usage line never registers in stepIndexByID (it continues
			// before reaching that logic below), so even if it happens to
			// share an id with a real line elsewhere in the file, it can
			// neither spawn a phantom step nor overwrite the real usage.
			continue
		}

		// toolCalls is per content block, not per message, and every
		// surviving line contributes its own blocks regardless of whether
		// this line ends up creating a step or merging into one — a message
		// split across three lines with one tool_use block each must still
		// report three tool calls after dedup.
		for _, c := range msg.Message.Content {
			if c.Type == "tool_use" {
				toolCalls++
			}
		}

		// message.id ties every content-block line of one assistant message
		// together. web_detail.go drops id-less lines outright (it's
		// attributing per-block detail — thinking/text/tool-calls — that
		// can't be merged without a key); here we're only summing usage for
		// cost, so dropping the line would silently lose real tokens instead
		// of just failing to merge them. An id-less line is therefore kept
		// as its own, undeduplicated step. In practice this doesn't seem to
		// come up: message.id has been present on every assistant line
		// sampled from real session files.
		id := msg.Message.ID
		if id != "" {
			if _, exists := stepIndexByID[id]; exists {
				// Later block of an already-seen message: usage is
				// identical on every line sharing this id, so it was
				// already counted when the step was created below.
				continue
			}
		}

		createdAt := ts.UnixMilli()
		if tsErr != nil {
			// No parseable timestamp: fall back to now rather than the zero
			// Time's UnixMilli() (year 1), which would look like a message
			// from before every dated rate window exists.
			createdAt = time.Now().UnixMilli()
		}
		models = append(models, msg.Message.Model)
		steps = append(steps, claudeSessionStep{
			Model: msg.Message.Model,
			Step: compute.StepData{
				Input:           msg.Message.Usage.InputTokens,
				CacheCreation:   msg.Message.Usage.CacheCreationInputTokens,
				CacheCreation1h: msg.Message.Usage.CacheCreation.Ephemeral1hInputTokens,
				CacheRead:       msg.Message.Usage.CacheReadInputTokens,
				Output:          msg.Message.Usage.OutputTokens,
			},
			CreatedAt: createdAt,
		})
		if id != "" {
			stepIndexByID[id] = len(steps) - 1
		}
	}
	return claudeMessageResult{Steps: steps, Models: models, ToolCalls: toolCalls, LastUserPrompt: lastUserPrompt, LastActivity: lastActivity}, scanner.Err()
}

type claudeSession struct {
	ID            string
	Filepath      string
	Project       string
	Date          string
	DominantModel string
	Msgs          int
	ToolCalls     int
	Birth         time.Time
	LastActivity  time.Time
	SubagentCount int
	Data          *claudeMessageResult
}

func claudeSessions(days int, date, modelFilter string) ([]claudeSession, error) {
	baseDir := expandHome(defaultClaudeSessions)
	cutoff := time.Now().AddDate(0, 0, -days)

	var sessions []claudeSession

	if err := walkSessionFiles(baseDir, func(fp string, info os.FileInfo) error {
		res, err := claudeMessages(fp)
		if err != nil || len(res.Models) == 0 {
			return nil
		}

		activity := res.LastActivity
		if activity.IsZero() {
			activity = info.ModTime()
		}
		if date != "" {
			// date is what the user typed on the command line (--date), a
			// local calendar day, not a UTC one — formatting activity in UTC
			// here would silently shift the comparison by the machine's UTC
			// offset (e.g. a session at 00:30 local on the 10th reads as the
			// 9th in UTC at UTC+2) and drop early-morning/late-night sessions
			// out of the requested day. See dashboardWindowMs (web.go) for
			// the same fix applied to the web dashboard's date range.
			if activity.Local().Format("2006-01-02") != date {
				return nil
			}
		} else if activity.Before(cutoff) {
			return nil
		}

		modelCount := make(map[string]int)
		for _, m := range res.Models {
			modelCount[m]++
		}
		primaryModel := dominantModel(modelCount)
		if modelFilter != "" && primaryModel != modelFilter {
			return nil
		}

		sessionName := filepath.Base(fp)
		sessionID := strings.TrimSuffix(sessionName, ".jsonl")
		project := cleanClaudeProjectName(filepath.Base(filepath.Dir(fp)))
		// Local, not UTC, for the same reason as the date filter above: this
		// is meant to be the calendar day a --date lookup matches against.
		fileDate := activity.Local().Format("2006-01-02")
		subagentCount := 0
		if subEntries, err := os.ReadDir(filepath.Join(filepath.Dir(fp), sessionID, "subagents")); err == nil {
			for _, subEntry := range subEntries {
				if !subEntry.IsDir() && filepath.Ext(subEntry.Name()) == ".jsonl" {
					subagentCount++
				}
			}
		}

		sessions = append(sessions, claudeSession{
			ID:            sessionID,
			Filepath:      fp,
			Project:       project,
			Date:          fileDate,
			DominantModel: primaryModel,
			Msgs:          len(res.Models),
			ToolCalls:     res.ToolCalls,
			Birth:         getCreatedAtFromInfo(info),
			LastActivity:  activity,
			SubagentCount: subagentCount,
			Data:          &res,
		})
		return nil
	}); err != nil {
		return nil, err
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Birth.Before(sessions[j].Birth)
	})

	return sessions, nil
}

func cleanClaudeProjectName(dirName string) string {
	// Convert -Users-username-work-project-name to work/project-name
	name := dirName
	if home, err := os.UserHomeDir(); err == nil {
		user := filepath.Base(home)
		dashedUser := strings.ReplaceAll(user, ".", "-")
		prefix1 := "-Users-" + dashedUser + "-"
		prefix2 := "-Users-" + dashedUser
		if strings.HasPrefix(name, prefix1) {
			name = name[len(prefix1):]
		} else if strings.HasPrefix(name, prefix2) {
			name = name[len(prefix2):]
		}
	}
	if name == "" {
		return "(root)"
	}
	return strings.ReplaceAll(name, "-", "/")
}

// claudeSessionNotFoundError marks a clean "no file matched" result from
// resolveClaudeSessionPath's walk, as opposed to the walk itself failing (a
// permissions problem, a corrupt directory, ...) — that case is returned
// as-is by resolveClaudeSessionPath and must never be mistaken for this one.
// claudeDetail uses errors.As to route only this case to the store fallback
// (see claudeDetailFromStore) and let every other error propagate untouched.
type claudeSessionNotFoundError struct {
	id string
}

func (e *claudeSessionNotFoundError) Error() string {
	return fmt.Sprintf("Claude session not found: %s", e.id)
}

func resolveClaudeSessionPath(input string) (string, string, error) {
	if strings.HasSuffix(input, ".jsonl") || strings.Contains(input, "/") {
		return input, "", nil
	}

	baseDir := expandHome(defaultClaudeSessions)
	var match string

	if err := walkSessionFiles(baseDir, func(fp string, info os.FileInfo) error {
		if match != "" {
			return nil
		}
		if strings.TrimSuffix(filepath.Base(fp), ".jsonl") == input {
			match = fp
		}
		return nil
	}); err != nil {
		return "", "", err
	}
	if match == "" {
		return "", "", &claudeSessionNotFoundError{id: input}
	}
	return match, input, nil
}

func claudeDetail(input string) error {
	fp, sessionID, err := resolveClaudeSessionPath(input)
	if err != nil {
		var notFound *claudeSessionNotFoundError
		if errors.As(err, &notFound) {
			// The walk completed cleanly and just found no file — the
			// session's source JSONL may have been rotated or deleted while
			// the session stayed in the store (a real cache since commit
			// 2486511). Fall back to it rather than failing outright. Any
			// other error from resolveClaudeSessionPath (a walk failure, a
			// permissions problem) skips this branch entirely and is
			// returned untouched below.
			return claudeDetailFromStore(notFound.id, err)
		}
		return err
	}

	res, err := claudeMessages(fp)
	if err != nil {
		return err
	}
	if len(res.Steps) == 0 {
		return fmt.Errorf("no Claude messages in %s", fp)
	}

	dirName := filepath.Base(filepath.Dir(fp))
	project := cleanClaudeProjectName(dirName)
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(fp), ".jsonl")
	}

	// buildPricingSpecs()["claude"] resolves every model "now" (see its own
	// doc comment) rather than at this session's LastActivity the way this
	// function used to — the same rule getSessionDetailFromStore
	// (web_store.go) already applies to the web dashboard's session detail
	// view, so the CLI and the dashboard price a Claude session identically
	// instead of one repricing a Sonnet-5 session's history through
	// 2026-09-01 and the other not.
	spec := buildPricingSpecs()["claude"]

	// One continuous ideal-cache pass over the session's untouched step
	// order — see computeSessionPricing's doc comment (web_store.go) for why
	// splitting by model first (the old byModel := groupStepsByModel(...)
	// here) inflates Ideal and can produce the impossible Paid < Ideal.
	steps := make([]sessionStep, len(res.Steps))
	for i, st := range res.Steps {
		steps[i] = sessionStep{Model: st.Model, Data: st.Step, CreatedAt: st.CreatedAt}
	}

	primaryModel, err := claudeDominantModelAndPriceCheck(steps, spec)
	if err != nil {
		return err
	}

	fmt.Printf("Session:  %s\n", sessionID)
	fmt.Printf("File:     %s\n", filepath.Base(fp))
	fmt.Printf("Project:  %s\n", project)
	fmt.Printf("Model:    %s\n", primaryModel)
	fmt.Printf("Messages: %d\n", len(res.Steps))
	fmt.Printf("ToolCalls: %d\n\n", res.ToolCalls)

	printClaudeDetailTableAndTotal(steps, spec)

	return nil
}

// claudeDominantModelAndPriceCheck returns steps' dominant model, after the
// same fail-fast price check claudeDetail has always done: bail before
// printing anything if the dominant model, or any other model appearing in
// the session, has no resolvable price. Shared between the file-backed path
// above and its store fallback (claudeDetailFromStore) so both fail exactly
// the same way on an unpriced model.
func claudeDominantModelAndPriceCheck(steps []sessionStep, spec agentPricingSpec) (string, error) {
	modelCount := make(map[string]int, len(steps))
	for _, s := range steps {
		modelCount[s.Model]++
	}
	primaryModel := dominantModel(modelCount)

	if _, ok := spec.PriceFunc(primaryModel, 0); !ok {
		return "", fmt.Errorf("no prices configured for model %s", primaryModel)
	}

	seenModels := map[string]bool{}
	for _, s := range steps {
		if seenModels[s.Model] {
			continue
		}
		seenModels[s.Model] = true
		if _, ok := spec.PriceFunc(s.Model, s.CreatedAt); !ok {
			return "", fmt.Errorf("no prices configured for model %s", s.Model)
		}
	}
	return primaryModel, nil
}

// printClaudeDetailTableAndTotal renders the per-step table and TOTAL
// section shared by claudeDetail's file-backed path and its store fallback
// (claudeDetailFromStore) — by the time either calls this, they already
// have an identically-shaped []sessionStep in hand, and everything past
// that point (the ideal-cache pass, per-row pricing, the TOTAL summary) is
// identical regardless of whether the steps came from the source JSONL or
// from the store.
func printClaudeDetailTableAndTotal(steps []sessionStep, spec agentPricingSpec) {
	tokens := make([]compute.StepData, len(steps))
	for i, s := range steps {
		tokens[i] = s.Data
	}
	rows := compute.ComputeIdealClaude(tokens, compute.ModelPrices{})

	// pricing[i] carries rows[i]'s own model's price so a session that
	// switches models mid-run prices each row correctly — see
	// detailRowPrice's doc comment (algo.go).
	pricing := make([]detailRowPrice, len(steps))
	for i, s := range steps {
		prices, ok := spec.PriceFunc(s.Model, s.CreatedAt)
		pricing[i] = detailRowPrice{Model: s.Model, Prices: prices, Priced: ok}
	}
	printDetailRows(rows, pricing, true)

	stepPrices := computeSessionPricing(steps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc)
	totalActual, totalIdeal := sumStepPricing(stepPrices)

	totalOverpay := totalActual - totalIdeal
	if totalOverpay < 0 {
		totalOverpay = 0
	}
	pctIdeal := 0.0
	if totalIdeal > 0 {
		pctIdeal = totalOverpay / totalIdeal * 100
	}

	fmt.Printf("\nTOTAL\n")
	fmt.Printf("Actual paid:  $%.2f\n", totalActual)
	fmt.Printf("Ideal paid:   $%.2f\n", totalIdeal)
	fmt.Printf("Overpay:      $%.2f (%.1f%% of ideal)\n", totalOverpay, pctIdeal)
}

// claudeDetailFromStore renders `claude detail` for a session whose source
// JSONL is no longer on disk but is still cached in the local store — every
// previously-ingested session is a real cache since 2486511 ("perf(ingest):
// use the store as a cache instead of re-parsing every session"), so a
// rotated or deleted source file no longer has to mean the session is gone.
//
// The per-step token/cost/model data the table below needs lives in the
// message table exactly as it would have come from the file (see
// store/messages.go's GetMessages and ingest_main.go's detailToStore, which
// is what put it there at ingest time), so this reproduces byte-for-byte
// what the file-backed path would have printed for the same session, with
// one visible difference: the "File:" line, since the store never recorded
// the file's own path, and an explicit note that the source is gone.
//
// notFound is resolveClaudeSessionPath's original "session not found"
// error. It is returned unchanged when the store doesn't know the session
// either (sql.ErrNoRows) — never silently swallowed — and a genuine store
// error (corrupt db, open failure) is wrapped around it instead of being
// misreported as "not found".
func claudeDetailFromStore(sessionID string, notFound error) error {
	st, err := ensureDetailStore()
	if err != nil {
		return fmt.Errorf("%w (also failed to open the store to check for a cached copy: %v)", notFound, err)
	}

	ctx := context.Background()
	sess, err := st.GetSession(ctx, "claude", sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return notFound
		}
		return fmt.Errorf("checking store for Claude session %s: %w", sessionID, err)
	}

	msgs, err := st.GetMessages(ctx, "claude", sessionID)
	if err != nil {
		return fmt.Errorf("reading store for Claude session %s: %w", sessionID, err)
	}
	steps := stepsFromAssistantMessages(msgs)
	if len(steps) == 0 {
		return fmt.Errorf("Claude session %s is in the store but has no assistant messages recorded", sessionID)
	}

	spec := buildPricingSpecs()["claude"]
	primaryModel, err := claudeDominantModelAndPriceCheck(steps, spec)
	if err != nil {
		return err
	}

	stats, err := st.SessionStats(ctx, "claude", sessionID)
	if err != nil {
		return fmt.Errorf("reading store stats for Claude session %s: %w", sessionID, err)
	}

	fmt.Printf("Session:  %s\n", sessionID)
	fmt.Println("File:     (not found on disk)")
	fmt.Printf("Project:  %s\n", sess.Project)
	fmt.Printf("Model:    %s\n", primaryModel)
	fmt.Printf("Messages: %d\n", len(steps))
	fmt.Printf("ToolCalls: %d\n\n", stats.ToolCallCount)
	fmt.Println("note: the source session file is missing from disk; the figures above and below are read from tokeneks' local store cache (as of this session's last ingest) instead of the raw JSONL.")
	fmt.Println()

	printClaudeDetailTableAndTotal(steps, spec)

	return nil
}

// ensureDetailStore returns the process-wide store, opening it if no earlier
// command (e.g. `sync`, `web`, `total`) already did. `claude detail`/`pi
// detail` don't normally touch the store at all — they read the source
// JSONL directly — so this only runs on the store-fallback path, once a
// lookup on disk has already come up empty.
func ensureDetailStore() (*store.Store, error) {
	if st := getTokeneksStore(); st != nil {
		return st, nil
	}
	st, err := openTokeneksStore()
	if err != nil {
		return nil, err
	}
	setTokeneksStore(st)
	return st, nil
}

// stepsFromAssistantMessages converts a session's stored messages into the
// []sessionStep shape the pricing/table-rendering code needs, keeping only
// role=assistant rows (role=user/tool messages carry no token usage) in
// their original msg_index order (GetMessages already orders by it). Shared
// between the Claude and PI store fallbacks — the store's message columns
// (input_tokens, cache_read, cache_write, cache_write_1h, output_tokens,
// model, cost, created_at) mean exactly the same thing for both agents.
func stepsFromAssistantMessages(msgs []store.Message) []sessionStep {
	steps := make([]sessionStep, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != store.RoleAssistant {
			continue
		}
		steps = append(steps, sessionStep{
			Model:      m.Model,
			Provider:   m.Provider,
			LoggedCost: m.Cost,
			CreatedAt:  m.CreatedAt,
			Data: compute.StepData{
				Input:           m.InputTokens,
				CacheCreation:   m.CacheWrite,
				CacheCreation1h: m.CacheWrite1h,
				CacheRead:       m.CacheRead,
				Output:          m.OutputTokens,
			},
		})
	}
	return steps
}

func claudeList(days int, date string) error {
	sessions, err := claudeSessions(days, date, "")
	if err != nil {
		return err
	}

	fmt.Printf("%19s  %-36s  %-14s  %-25s  %4s  %8s  %8s  %7s  %10s  %7s  %8s  %8s\n",
		"DateTime", "SessionID", "DominantModel", "Project", "Msgs", "Tokens", "Paid", "Ideal", "Overpay", "%ideal", "$/1M", "i$/1M")
	fmt.Println(strings.Repeat("-", separatorWidthClaudeMix))

	var totalActual, totalIdeal float64
	var totalIn, totalCC, totalCR, totalOut int

	spec := buildPricingSpecs()["claude"]

	for _, sess := range sessions {
		res := sess.Data
		if res == nil || len(res.Steps) == 0 {
			continue
		}

		valid := true
		seenModels := map[string]bool{}
		for _, st := range res.Steps {
			if seenModels[st.Model] {
				continue
			}
			seenModels[st.Model] = true
			// buildPricingSpecs()["claude"] resolves every model "now" (see
			// its own doc comment) rather than at sess.LastActivity as this
			// used to — the same "reprice at today's rates" rule
			// applySessionPricing (web_store.go) already applies to the web
			// dashboard's session list, so the two surfaces agree.
			if _, ok := spec.PriceFunc(st.Model, st.CreatedAt); !ok {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}

		// One continuous ideal-cache pass over this session's untouched step
		// order, not split by model first — see claudeDetail and
		// computeSessionPricing (web_store.go) for why splitting throws away
		// the carry-forward state at every model switch.
		steps := make([]sessionStep, len(res.Steps))
		var tokIn, tokCC, tokCR, tokOut int
		for i, st := range res.Steps {
			steps[i] = sessionStep{Model: st.Model, Data: st.Step, CreatedAt: st.CreatedAt}
			tokIn += st.Step.Input
			tokCC += st.Step.CacheCreation
			tokCR += st.Step.CacheRead
			tokOut += st.Step.Output
		}

		actual, ideal := sumStepPricing(computeSessionPricing(steps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc))
		overpay, pctIdeal, costPer1M, idealPer1M := footerTotals(actual, ideal, tokIn+tokCC+tokCR+tokOut)

		totalActual += actual
		totalIdeal += ideal
		totalIn += tokIn
		totalCC += tokCC
		totalCR += tokCR
		totalOut += tokOut

		timestamp := sess.Birth.UTC().Format("2006-01-02 15:04:05")
		project := sess.Project
		if len(project) > 25 {
			project = project[:23] + ".."
		}
		modelShort := claudeShortModelName(sess.DominantModel)
		tokens := tokIn + tokCC + tokCR + tokOut

		fmt.Printf("%19s  %-36s  %-14s  %-25s  %4d  %8s  %8.2f  %7.2f  %10.2f  %6.1f%%  %8.2f  %8.2f\n",
			timestamp, sess.ID, modelShort, project, sess.Msgs, formatTokens(tokens), actual, ideal, overpay, pctIdeal, costPer1M, idealPer1M)
	}

	fmt.Println(strings.Repeat("-", separatorWidthClaudeMix))
	totalTokens := totalIn + totalCC + totalCR + totalOut
	totalOverpay, pct, totalCostPer1M, totalIdealPer1M := footerTotals(totalActual, totalIdeal, totalTokens)

	fmt.Printf("%19s  %-36s  %-14s  %-25s  %4s  %8s  %8.2f  %7.2f  %10.2f  %6.1f%%  %8.2f  %8.2f\n",
		"TOTAL", "", "", "", "", formatTokens(totalTokens), totalActual, totalIdeal, totalOverpay, pct, totalCostPer1M, totalIdealPer1M)
	fmt.Println()
	prices := claudeGlobalModelPrices()
	for _, m := range claudeFooterModels {
		p := prices[m.model]
		fmt.Printf("%sIn=$%.2f/M  CC=$%.2f/M  CR=$%.2f/M  Out=$%.2f/M\n",
			m.label, p.Input, p.CacheCreation, p.CacheRead, p.Output)
	}

	return nil
}

// claudeFooterModels drives both the `claude list` price-summary footer and
// the DominantModel column's short display name. It's the one place to edit
// when adding a model to either — previously each needed its own hardcoded
// block/if-chain.
var claudeFooterModels = []struct {
	model string
	label string // footer line prefix, e.g. "Opus5:    " (kept verbatim, including its original spacing)
	short string // table column abbreviation, e.g. "opus-5"
}{
	{"claude-opus-5", "Opus5:    ", "opus-5"},
	{"claude-opus-4-7", "Opus4.7:  ", "opus-4.7"},
	{"claude-sonnet-4-6", "Sonnet4.6: ", "sonnet-4.6"},
	{"claude-sonnet-5", "Sonnet5:   ", "sonnet-5"},
	{"claude-fable-5", "Fable5:    ", "fable-5"},
}

// claudeShortModelName maps a Claude model name to the abbreviation used in
// `claude list`'s DominantModel column, or returns it unchanged if it's not
// one of the models claudeFooterModels tracks.
func claudeShortModelName(model string) string {
	for _, m := range claudeFooterModels {
		if m.model == model {
			return m.short
		}
	}
	return model
}
