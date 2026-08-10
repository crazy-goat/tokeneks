package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"tokeneks/compute"
)

const defaultClaudeSessions = "~/.claude/projects"
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
}

func (s claudeSessionStep) modelKey() string           { return s.Model }
func (s claudeSessionStep) stepData() compute.StepData { return s.Step }

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
		if ts, err := parseTimestamp(msg.Timestamp); err == nil && ts.After(lastActivity) {
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
			if activity.UTC().Format("2006-01-02") != date {
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
		fileDate := activity.UTC().Format("2006-01-02")
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
		return "", "", fmt.Errorf("Claude session not found: %s", input)
	}
	return match, input, nil
}

func claudeDetail(input string) error {
	fp, sessionID, err := resolveClaudeSessionPath(input)
	if err != nil {
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

	modelCount := make(map[string]int)
	for _, m := range res.Models {
		modelCount[m]++
	}
	primaryModel := dominantModel(modelCount)

	// Price as of when this session actually happened, not when this report
	// runs, so a July session keeps costing what it cost in July. LastActivity
	// is the same timestamp claudeSessions already uses to bucket a session
	// into a date/day-range, so this stays consistent with that.
	at := res.LastActivity
	if at.IsZero() {
		at = time.Now()
	}

	if _, ok := claudeModelPricesAt(primaryModel, at); !ok {
		return fmt.Errorf("no prices configured for model %s", primaryModel)
	}

	fmt.Printf("Session:  %s\n", sessionID)
	fmt.Printf("File:     %s\n", filepath.Base(fp))
	fmt.Printf("Project:  %s\n", project)
	fmt.Printf("Model:    %s\n", primaryModel)
	fmt.Printf("Messages: %d\n", len(res.Steps))
	fmt.Printf("ToolCalls: %d\n\n", res.ToolCalls)

	byModel := groupStepsByModel(res.Steps)

	modelNames := make([]string, 0, len(byModel))
	for model := range byModel {
		modelNames = append(modelNames, model)
	}
	sort.Strings(modelNames)

	var totalActual, totalIdeal float64
	for i, model := range modelNames {
		prices, ok := claudeModelPricesAt(model, at)
		if !ok {
			return fmt.Errorf("no prices configured for model %s", model)
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("=== %s (%d messages) ===\n\n", model, len(byModel[model]))
		rows := compute.ComputeIdealClaude(byModel[model], prices)
		printDetailRows(rows, prices, true)
		s := compute.SummarizeClaude(rows, prices)
		totalActual += s.Actual
		totalIdeal += s.Ideal
		fmt.Printf("\nSubtotal actual: $%.2f\n", s.Actual)
		fmt.Printf("Subtotal ideal:  $%.2f\n", s.Ideal)
		fmt.Printf("Subtotal overpay: $%.2f (%.1f%% of ideal)\n", s.Overpay, s.PctIdeal)
	}

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

	return nil
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

	for _, sess := range sessions {
		res := sess.Data
		if res == nil || len(res.Steps) == 0 {
			continue
		}

		byModel := groupStepsByModel(res.Steps)
		valid := true
		for model := range byModel {
			// Price this session's models as of when the session happened
			// (LastActivity), not "now" — otherwise a Sonnet 5 session from
			// before 2026-09-01 would silently reprice itself at the
			// post-cutover rate once that date passes.
			if _, ok := claudeModelPricesAt(model, sess.LastActivity); !ok {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}

		var s compute.Summary
		for model, modelSteps := range byModel {
			prices, _ := claudeModelPricesAt(model, sess.LastActivity)
			rows := compute.ComputeIdealClaude(modelSteps, prices)
			part := compute.SummarizeClaude(rows, prices)
			s.TotalCC += part.TotalCC
			s.TotalCR += part.TotalCR
			s.TotalIn += part.TotalIn
			s.TotalOut += part.TotalOut
			s.TotalIdealCR += part.TotalIdealCR
			s.TotalIdealIn += part.TotalIdealIn
			s.TotalIdealCC += part.TotalIdealCC
			s.TotalWaste += part.TotalWaste
			s.Actual += part.Actual
			s.Ideal += part.Ideal
		}
		s.Overpay = s.Actual - s.Ideal
		if s.Overpay < 0 {
			s.Overpay = 0
		}
		if s.Ideal > 0 {
			s.PctIdeal = s.Overpay / s.Ideal * 100
		}

		totalActual += s.Actual
		totalIdeal += s.Ideal
		totalIn += s.TotalIn
		totalCC += s.TotalCC
		totalCR += s.TotalCR
		totalOut += s.TotalOut

		timestamp := sess.Birth.UTC().Format("2006-01-02 15:04:05")
		project := sess.Project
		if len(project) > 25 {
			project = project[:23] + ".."
		}
		modelShort := claudeShortModelName(sess.DominantModel)

		tokens := s.TotalIn + s.TotalCC + s.TotalCR + s.TotalOut
		costPer1M := compute.PerMillion(s.Actual, tokens)
		idealPer1M := compute.PerMillion(s.Ideal, tokens)

		fmt.Printf("%19s  %-36s  %-14s  %-25s  %4d  %8s  %8.2f  %7.2f  %10.2f  %6.1f%%  %8.2f  %8.2f\n",
			timestamp, sess.ID, modelShort, project, sess.Msgs, formatTokens(tokens), s.Actual, s.Ideal, s.Overpay, s.PctIdeal, costPer1M, idealPer1M)
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
