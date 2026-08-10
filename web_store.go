package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"tokeneks/compute"
	"tokeneks/ingest"
	"tokeneks/store"
)

// tokeneksStore is the process-wide store used by web and CLI handlers.
// Opened by openTokeneksStore, closed at process exit.
var (
	tokeneksStoreMu sync.Mutex
	tokeneksStore   *store.Store
)

// setTokeneksStore installs the global store (used by web and CLI).
func setTokeneksStore(s *store.Store) {
	tokeneksStoreMu.Lock()
	defer tokeneksStoreMu.Unlock()
	tokeneksStore = s
}

func getTokeneksStore() *store.Store {
	tokeneksStoreMu.Lock()
	defer tokeneksStoreMu.Unlock()
	return tokeneksStore
}

// agentDisplayName maps the lowercase store agent name to the display name
// expected by WebSession / SessionDetail (e.g. "opencode" -> "OpenCode").
func agentDisplayName(agent string) string {
	return agentRegistry.DisplayName(agent)
}

// unboundedToMs is the upper bound of a [fromMs, toMs) session window when
// the caller wants no upper limit at all — mirrors aggregateSessionsFromStore's
// own `last_activity >= cutoff` query (no upper bound), which is what the
// CLI's --days N has always meant: "N days ago through now", never "N days
// ago through some earlier now". session.last_activity is stored in
// MILLISECONDS (UnixMilli), never seconds — every fromMs/toMs value flowing
// into these queries must be milliseconds too, or it silently matches every
// row (or none).
const unboundedToMs = int64(math.MaxInt64)

// gatherWebSessionsFromStore returns the list of sessions to display in the
// web dashboard, computed entirely from the local store, restricted to
// sessions whose last_activity falls in [fromMs, toMs).
//
// This used to take a `days int` and derive its own rolling cutoff
// (`time.Now().Add(-days*24h)`), same formula as aggregateSessionsFromStore's
// CLI-side cutoff. That was fine for the CLI's own --days N flag, but the web
// dashboard's date picker sends calendar start/end dates, not a day count —
// the handler (web.go) used to paper over the mismatch by picking a rolling
// `days` value guessed to be "wide enough" to cover the requested calendar
// range, then filtering the (over-fetched) result down to the exact range in
// Go. That meant a calendar-day request and the CLI's rolling-day request for
// "the same" window could disagree on which sessions were in it, and the
// session cache (keyed on that guessed day count) could hand back a window
// narrower than the one actually requested. Taking an explicit millisecond
// range here instead removes the guesswork: the caller (web.go) decides the
// range — rolling for the CLI-equivalent default view, calendar-anchored once
// the user picks dates — and this function (and everything it calls) just
// honors it.
//
// Sessions with no messages are excluded: the watcher keeps them in the
// store purely as a mtime-filter baseline (so the watcher doesn't keep
// re-parsing them on every poll), but they're never meant to surface in
// any list — see ingest/watcher.go reingestRef.
func gatherWebSessionsFromStore(ctx context.Context, fromMs, toMs int64) ([]WebSession, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}

	rows, err := st.DB().QueryContext(ctx, `
		SELECT
		  s.agent, s.session_id, s.project, COALESCE(s.parent_id, ''),
		  s.created_at, s.last_activity,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.input_tokens  END), 0) AS in_tok,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.output_tokens END), 0) AS out_tok,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.cache_read   END), 0) AS cr,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.cache_write  END), 0) AS cw,
		  COALESCE(SUM(CASE WHEN m.role='assistant' THEN m.cost         END), 0) AS cost,
		  COUNT(CASE WHEN m.role='assistant' THEN 1 END) AS msg_count
		FROM session s
		LEFT JOIN message m ON m.agent = s.agent AND m.session_id = s.session_id
		WHERE s.last_activity >= ? AND s.last_activity < ?
		  AND EXISTS (SELECT 1 FROM message m2
		              WHERE m2.agent = s.agent AND m2.session_id = s.session_id)
		GROUP BY s.agent, s.session_id
		ORDER BY s.last_activity DESC
	`, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessionsByKey := make(map[sessKey]*WebSession)
	keys := []sessKey{}

	for rows.Next() {
		var (
			agent, id, project, parentID string
			created, last                int64
			inT, outT, cr, cw            int
			cost                         float64
			msgCount                     int
		)
		if err := rows.Scan(&agent, &id, &project, &parentID, &created, &last,
			&inT, &outT, &cr, &cw, &cost, &msgCount); err != nil {
			return nil, err
		}
		ws := &WebSession{
			Agent:           agentDisplayName(agent),
			ID:              id,
			Date:            time.UnixMilli(created).UTC().Format("2006-01-02 15:04"),
			Project:         project,
			DominantModel:   "",
			LastMessage:     time.UnixMilli(last).UTC().Format("2006-01-02 15:04:05"),
			TotalInput:      inT,
			TotalOutput:     outT,
			TotalCacheRead:  cr,
			TotalCacheWrite: cw,
			TotalCost:       cost,
			Messages:        msgCount,
			PromptInput:     inT + cw,
			ParentID:        parentID,
			IsSubsession:    parentID != "",
			Models:          []WebModelUsage{},
		}
		sessionsByKey[sessKey{agent, id}] = ws
		keys = append(keys, sessKey{agent, id})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return []WebSession{}, nil
	}

	// Tool-count and child-count are computed for ALL sessions in the DB
	// (filtered by date) and joined in Go. Passing a per-session
	// `(agent=?, session_id=?) OR ...` list with hundreds of keys was the
	// reason "last 30 days" used to run for many seconds.
	toolCounts, err := toolCallCounts(ctx, st, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	childCounts, err := childSessionCounts(ctx, st)
	if err != nil {
		return nil, err
	}
	// One bulk query for every step of every session in the window, same
	// reasoning as toolCounts/childCounts above: a per-session query here
	// (one per key) is exactly the N+1 pattern that made this page slow
	// before, just relocated to the ideal-cache computation. perModelUsage
	// reuses this same per-step data (see its own doc comment) so the
	// per-model breakdown and the session total can never source their
	// numbers from two different places.
	stepsByKey, err := sessionStepsForPricing(ctx, st, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	specsByAgent := buildPricingSpecs()
	perModel := perModelUsage(stepsByKey, specsByAgent)

	out := make([]WebSession, 0, len(keys))
	for _, k := range keys {
		ws := sessionsByKey[k]
		models := perModel[k]
		if models == nil {
			models = []WebModelUsage{}
		}
		ws.Models = models
		ws.ToolCalls = toolCounts[k]
		ws.ChildCount = childCounts[k]
		if len(ws.Models) > 0 {
			ws.DominantModel = ws.Models[0].Model
		}
		if spec, ok := specsByAgent[k.agent]; ok {
			applySessionPricing(ws, stepsByKey[k], spec)
		}
		out = append(out, *ws)
	}
	return out, nil
}

// applySessionPricing prices one session's steps via computeSessionPricing
// and fills in ws's Paid/Ideal/Overpay/unpriced fields. TotalCost is
// overwritten with the freshly computed Paid rather than left at whatever
// the caller summed straight from the store's cost column — for OpenCode
// and PI those two already agree (their message.cost IS the logged cost
// computeSessionPricing prefers), but for Claude the store's cost column is
// tokeneks' own recomputation as of ingest time, which goes stale the
// moment `prices update`/`prices derive` changes a rate; computeSessionPricing
// always re-resolves the rate for "now", so this keeps the dashboard's cost
// column in agreement with `total` instead of one silently drifting behind
// the other.
func applySessionPricing(ws *WebSession, steps []sessionStep, spec agentPricingSpec) {
	stepPrices := computeSessionPricing(steps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc)
	paid, ideal := sumStepPricing(stepPrices)
	ws.TotalCost = paid
	ws.Ideal = ideal
	ws.Overpay = math.Max(paid-ideal, 0)
	if ideal > 0 {
		ws.OverpayPct = ws.Overpay / ideal * 100
	}
	for _, sp := range stepPrices {
		if sp.Priced {
			continue
		}
		ws.UnpricedTokens += sp.Tokens
		ws.PartiallyUnpriced = true
	}
}

// sessionStepsForPricing returns, for every session active within
// [fromMs, toMs), the same per-step data (model, provider, tokens, logged
// cost, created-at) computeSessionPricing needs — across every agent in one
// query, so callers over the whole dashboard's session list don't pay for
// one round trip per session.
func sessionStepsForPricing(ctx context.Context, st *store.Store, fromMs, toMs int64) (map[sessKey][]sessionStep, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT m.agent, m.session_id, COALESCE(m.model, ''), COALESCE(m.provider, ''), m.cost, m.created_at,
		       m.input_tokens, m.cache_read, m.cache_write, m.cache_write_1h, m.output_tokens
		FROM message m
		JOIN session s ON s.agent = m.agent AND s.session_id = m.session_id
		WHERE m.role = 'assistant' AND s.last_activity >= ? AND s.last_activity < ?
		ORDER BY m.agent, m.session_id, m.msg_index ASC
	`, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[sessKey][]sessionStep)
	for rows.Next() {
		var agent, id, model, provider string
		var cost float64
		var createdAt int64
		var in, cr, cw, cw1h, o int
		if err := rows.Scan(&agent, &id, &model, &provider, &cost, &createdAt, &in, &cr, &cw, &cw1h, &o); err != nil {
			return nil, err
		}
		k := sessKey{agent, id}
		out[k] = append(out[k], sessionStep{
			Model:      model,
			Provider:   provider,
			LoggedCost: cost,
			CreatedAt:  createdAt,
			Data:       compute.StepData{Input: in, CacheRead: cr, CacheCreation: cw, CacheCreation1h: cw1h, Output: o},
		})
	}
	return out, rows.Err()
}

type sessKey struct{ agent, id string }

// perModelUsage groups each session's already-ordered steps (stepsByKey,
// from sessionStepsForPricing) by model and prices them through
// computeSessionPricing — the same engine applySessionPricing uses for the
// session-level Paid — so a session's per-model Cost rows always sum to
// exactly that session's own TotalCost.
//
// This used to instead run its own SQL query summing the store's logged
// cost column (SUM(m.cost)) grouped by (agent, session_id, model). That
// column is a fine source of truth for PI and OpenCode, which log the
// provider's real bill per message, but for Claude it's tokeneks' own
// ingest-time recomputation — it goes stale the moment `prices
// update`/`prices derive` changes a rate, and the session-level Paid shown
// right above these rows in the dashboard is always the freshly recomputed
// figure (applySessionPricing), not that stale column. Reusing stepsByKey
// rather than re-querying also keeps the two totals sourced from identical
// per-step data instead of two independent queries that could drift apart.
//
// A step whose model has no resolvable rate is priced the same way
// computeSessionPricing prices it at the session level: at its logged cost
// (zero overpay) when the agent logs one, at 0 when it doesn't — see
// stepPricing's own doc comment and unpricedModel in main.go.
func perModelUsage(stepsByKey map[sessKey][]sessionStep, specsByAgent map[string]agentPricingSpec) map[sessKey][]WebModelUsage {
	out := make(map[sessKey][]WebModelUsage)
	for k, steps := range stepsByKey {
		if len(steps) == 0 {
			continue
		}
		spec, ok := specsByAgent[k.agent]
		if !ok {
			continue
		}
		stepPrices := computeSessionPricing(steps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc)

		byModel := make(map[string]*WebModelUsage)
		var order []string
		for i, sp := range stepPrices {
			s := steps[i]
			mu, exists := byModel[sp.Model]
			if !exists {
				mu = &WebModelUsage{Model: sp.Model, Provider: s.Provider}
				byModel[sp.Model] = mu
				order = append(order, sp.Model)
			}
			mu.Input += s.Data.Input
			mu.Output += s.Data.Output
			mu.CacheRead += s.Data.CacheRead
			mu.CacheWrite += s.Data.CacheCreation
			mu.Cost += sp.Paid
			mu.Messages++
		}

		models := make([]WebModelUsage, 0, len(order))
		for _, m := range order {
			models = append(models, *byModel[m])
		}
		sort.Slice(models, func(i, j int) bool { return models[i].Cost > models[j].Cost })
		out[k] = models
	}
	return out
}

func toolCallCounts(ctx context.Context, st *store.Store, fromMs, toMs int64) (map[sessKey]int, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT m.agent, m.session_id, COUNT(tc.id)
		FROM message m
		JOIN session s ON s.agent = m.agent AND s.session_id = m.session_id
		LEFT JOIN tool_call tc ON tc.message_id = m.id
		WHERE m.role = 'assistant' AND s.last_activity >= ? AND s.last_activity < ?
		GROUP BY m.agent, m.session_id
	`, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[sessKey]int)
	for rows.Next() {
		var agent, id string
		var n int
		if err := rows.Scan(&agent, &id, &n); err != nil {
			return nil, err
		}
		out[sessKey{agent, id}] = n
	}
	return out, rows.Err()
}

func childSessionCounts(ctx context.Context, st *store.Store) (map[sessKey]int, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT agent, parent_id, COUNT(*)
		FROM session
		WHERE parent_id IS NOT NULL AND parent_id != ''
		GROUP BY agent, parent_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[sessKey]int)
	for rows.Next() {
		var agent, parent string
		var n int
		if err := rows.Scan(&agent, &parent, &n); err != nil {
			return nil, err
		}
		out[sessKey{agent, parent}] = n
	}
	return out, rows.Err()
}

// getSessionDetailFromStore returns the SessionDetail for one session,
// reading entirely from the local store.
func getSessionDetailFromStore(ctx context.Context, agent, sessionID string) (*SessionDetail, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}
	agent = strings.ToLower(agent)
	sess, err := st.GetSession(ctx, agent, sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := st.GetMessages(ctx, agent, sessionID)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no messages for session %s/%s", agent, sessionID)
	}

	detail := &SessionDetail{
		Agent:   agentDisplayName(agent),
		ID:      sessionID,
		Project: sess.Project,
		Date:    time.UnixMilli(sess.CreatedAt).UTC().Format("2006-01-02 15:04"),
	}

	// Convert messages to steps. Ingested sessions store each user prompt
	// immediately before the assistant response it belongs to, so keep it
	// pending until that assistant message is encountered.
	steps := []StepInfo{}
	// pricingSteps lines up 1:1 with steps (both built from the same
	// assistant messages, in the same order) — kept separate because
	// sessionStep is compute's provider-agnostic step shape, not the API's
	// StepInfo, per sessionStep's own doc comment above.
	var pricingSteps []sessionStep
	pendingUserPrompt := ""
	for _, m := range msgs {
		switch m.Role {
		case store.RoleUser:
			pendingUserPrompt = m.Content
		case store.RoleAssistant:
			s := StepInfo{
				Step:         len(steps) + 1,
				Timestamp:    time.UnixMilli(m.CreatedAt).UTC().Format(time.RFC3339),
				Model:        m.Model,
				Input:        m.InputTokens,
				Output:       m.OutputTokens,
				CacheRead:    m.CacheRead,
				CacheWrite:   m.CacheWrite,
				CacheWrite1h: m.CacheWrite1h,
				Cost:         m.Cost,
				Thinking:     m.Thinking,
				Response:     m.Response,
				UserPrompt:   pendingUserPrompt,
				StopReason:   m.StopReason,
			}
			pendingUserPrompt = ""
			tcs, err := st.GetToolCalls(ctx, m.ID)
			if err != nil {
				return nil, err
			}
			for _, tc := range tcs {
				s.ToolCalls = append(s.ToolCalls, ToolCallInfo{
					Name:       tc.Name,
					ID:         tc.CallID,
					Input:      json.RawMessage(tc.Input),
					Error:      tc.Error,
					Status:     tc.Status,
					DurationMs: tc.DurationMs,
				})
				// attach tool result from the matching role=tool message
				if tc.CallID != "" {
					for _, m2 := range msgs {
						if m2.Role == store.RoleTool && m2.ToolCallID == tc.CallID {
							s.ToolCalls[len(s.ToolCalls)-1].Output = json.RawMessage(m2.Content)
							break
						}
					}
				}
			}
			steps = append(steps, s)
			pricingSteps = append(pricingSteps, sessionStep{
				Model:      m.Model,
				LoggedCost: m.Cost,
				CreatedAt:  m.CreatedAt,
				Data:       compute.StepData{Input: m.InputTokens, CacheRead: m.CacheRead, CacheCreation: m.CacheWrite, CacheCreation1h: m.CacheWrite1h, Output: m.OutputTokens},
			})
		}
	}

	// Reprice every step at today's rates rather than trusting the stored
	// m.Cost column, which for Claude is tokeneks' own ingest-time
	// recomputation and goes stale the moment `prices update`/`prices
	// derive` changes a rate — see applySessionPricing for the parallel
	// reasoning on the session-list side. This also fills in Ideal/Overpay,
	// which the stored column has no equivalent for at all.
	if spec, ok := buildPricingSpecs()[agent]; ok {
		stepPrices := computeSessionPricing(pricingSteps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc)
		for i, sp := range stepPrices {
			steps[i].Cost = sp.Paid
			steps[i].Ideal = sp.Ideal
			detail.Ideal += sp.Ideal
			if sp.Priced {
				continue
			}
			detail.UnpricedTokens += sp.Tokens
			detail.PartiallyUnpriced = true
		}
	}
	detail.Steps = steps

	var totalCost float64
	for _, s := range steps {
		totalCost += s.Cost
	}
	detail.TotalCost = totalCost
	detail.Overpay = math.Max(detail.TotalCost-detail.Ideal, 0)
	if detail.Ideal > 0 {
		detail.OverpayPct = detail.Overpay / detail.Ideal * 100
	}

	// children
	childRows, err := st.DB().QueryContext(ctx, `
		SELECT s.session_id, COALESCE(s.project, ''),
		       COALESCE((SELECT m.model FROM message m
		                 WHERE m.agent = s.agent AND m.session_id = s.session_id
		                 AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1), ''),
		       s.created_at,
		       (SELECT COUNT(*) FROM message m
		        WHERE m.agent = s.agent AND m.session_id = s.session_id
		        AND m.role = 'assistant')
		FROM session s
		WHERE s.agent = ? AND s.parent_id = ?
		ORDER BY s.created_at ASC
	`, agent, sessionID)
	if err == nil {
		defer childRows.Close()
		for childRows.Next() {
			var child SessionLink
			var childCreated int64
			child.Agent = agentDisplayName(agent)
			// Steps is the assistant-message count so it matches the step
			// list on the subsession's own detail page.
			if err := childRows.Scan(&child.ID, &child.Title, &child.Model, &childCreated, &child.Steps); err != nil {
				continue
			}
			child.Date = time.UnixMilli(childCreated).UTC().Format("2006-01-02 15:04")
			stats, _ := st.SessionStats(ctx, agent, child.ID)
			child.TotalCost = stats.TotalCost
			child.TotalInput = stats.InputTokens
			child.TotalOutput = stats.OutputTokens
			child.TotalCacheRead = stats.CacheRead
			child.TotalCacheWrite = stats.CacheWrite
			if stats.InputTokens+stats.CacheRead > 0 {
				child.CacheHitRate = float64(stats.CacheRead) / float64(stats.InputTokens+stats.CacheRead) * 100
			}
			detail.Children = append(detail.Children, child)
		}
	}

	// Aggregate cost of the whole session tree (own steps + all descendant
	// subsessions, recursively). Kept separate from TotalCost.
	detail.TotalCostInclChildren = detail.TotalCost
	for _, child := range detail.Children {
		detail.TotalCostInclChildren += sessionTreeTotalCost(ctx, st, agent, child.ID)
	}

	// parent
	if sess.ParentID != "" {
		var parentTitle, parentModel string
		err := st.DB().QueryRowContext(ctx, `
			SELECT COALESCE(s.project, ''),
			       COALESCE((SELECT m.model FROM message m
			                 WHERE m.agent = s.agent AND m.session_id = s.session_id
			                 AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1), '')
			FROM session s
			WHERE s.agent = ? AND s.session_id = ?
		`, agent, sess.ParentID).Scan(&parentTitle, &parentModel)
		if err == nil {
			detail.Parent = &SessionLink{Agent: agentDisplayName(agent), ID: sess.ParentID, Title: parentTitle, Model: parentModel, Project: parentTitle}
		}
	}

	fillSessionStats(detail)
	return detail, nil
}

// sessionTreeTotalCost returns the total cost of a session including all
// descendant subsessions, recursively. Cycle-safe via the seen set.
func sessionTreeTotalCost(ctx context.Context, st *store.Store, agent, sessionID string) float64 {
	return sessionTreeTotalCostDepth(ctx, st, agent, sessionID, make(map[string]bool), 0)
}

func sessionTreeTotalCostDepth(ctx context.Context, st *store.Store, agent, sessionID string, seen map[string]bool, depth int) float64 {
	if depth > 32 || seen[sessionID] {
		return 0
	}
	seen[sessionID] = true
	stats, err := st.SessionStats(ctx, agent, sessionID)
	if err != nil {
		return 0
	}
	total := stats.TotalCost
	rows, err := st.DB().QueryContext(ctx,
		`SELECT session_id FROM session WHERE agent = ? AND parent_id = ?`,
		agent, sessionID)
	if err != nil {
		return total
	}
	defer rows.Close()
	for rows.Next() {
		var childID string
		if rows.Scan(&childID) == nil {
			total += sessionTreeTotalCostDepth(ctx, st, agent, childID, seen, depth+1)
		}
	}
	return total
}

// sessionRevisionFromStore returns a revision hash for change detection.
// Hashes (last_activity, message_count, tool_count) — these change when the
// session is modified.
func sessionRevisionFromStore(ctx context.Context, agent, sessionID string) (string, error) {
	st := getTokeneksStore()
	if st == nil {
		return "", fmt.Errorf("store not open")
	}
	agent = strings.ToLower(agent)
	sess, err := st.GetSession(ctx, agent, sessionID)
	if err != nil {
		return "", err
	}
	stats, err := st.SessionStats(ctx, agent, sessionID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%d-%d", sess.LastActivity, stats.MessageCount, stats.ToolCallCount), nil
}

// CLISession is a store-backed session summary used by the CLI commands
// (oc/pi/claude list/detail, total). It carries enough pre-aggregated data
// to format the per-agent tables without re-reading the source.
type CLISession struct {
	Agent        string
	ID           string
	Title        string
	Project      string
	CreatedAt    int64
	LastActivity int64
	Model        string
	Provider     string
	ParentID     string
	StepCount    int
	Cost         float64
	TokensIn     int
	TokensOut    int
	CacheRead    int
	CacheWrite   int
	Steps        []sessionStep
}

// stepPricing is one step's own Paid and Ideal contribution — the per-step
// output of computeSessionPricing. It carries enough detail that both
// totalsByAgent's per-model "unpriced" report and the web dashboard's
// per-session Ideal/Overpay can be built directly from a slice of these,
// without either one re-deriving pricing from the input steps itself.
type stepPricing struct {
	Model string
	Paid  float64
	Ideal float64
	// Priced is false when Model had no resolvable rate at the time this
	// step ran. Paid/Ideal are still populated in that case whenever the
	// step's agent logs its own cost and did so for this step: both are set
	// to that logged cost, mirroring a real bill at zero overpay (see
	// unpricedModel in main.go). Otherwise Paid/Ideal are 0 — the step
	// carries no cost information at all and callers should exclude it.
	Priced     bool
	LoggedCost float64
	// Tokens is Input+CacheCreation+CacheRead+Output for this step. Only
	// meaningful when !Priced — it's what totalsByAgent's unpriced report
	// counts.
	Tokens int
}

// computeSessionPricing prices one session's steps, in the order they
// actually happened, and returns one stepPricing per step. It is the shared
// engine behind totalsByAgent (the `total` CLI command, see its doc comment
// in main.go for the full rationale) and the web dashboard's session
// list/detail views, so the two surfaces can never disagree about a
// session's Paid/Ideal. In short, this function must never:
//
//   - split steps by model before running the ideal-cache algorithm — that
//     throws away the carried-forward cache state at every model switch,
//     inflating Ideal and producing the impossible Paid < Ideal;
//   - price a step at any rate but its own model's, resolved at its own
//     CreatedAt rather than "the current rate";
//   - invent a rate for a model priceFunc doesn't know about.
func computeSessionPricing(steps []sessionStep, claudeStyle, costIsLogged bool, priceFunc func(model string, at int64) (compute.ModelPrices, bool)) []stepPricing {
	if len(steps) == 0 {
		return nil
	}
	tokens := make([]compute.StepData, len(steps))
	for i, s := range steps {
		tokens[i] = s.Data
	}

	// ComputeIdealClaude's prices argument only shapes the IdealCC/IdealIn
	// split (which depends on CacheCreation, not on any rate), so a
	// zero-value table here is fine — real pricing happens per row below.
	var idealRows []compute.IdealRow
	if claudeStyle {
		idealRows = compute.ComputeIdealClaude(tokens, compute.ModelPrices{})
	} else {
		idealRows = compute.ComputeIdeal(tokens)
	}

	out := make([]stepPricing, len(idealRows))
	for i, row := range idealRows {
		s := steps[i]
		prices, priced := priceFunc(s.Model, s.CreatedAt)
		if !priced {
			sp := stepPricing{Model: s.Model, Tokens: row.Input + row.CacheCreation + row.CacheRead + row.Output}
			if costIsLogged && s.LoggedCost > 0 {
				sp.LoggedCost = s.LoggedCost
				sp.Paid = s.LoggedCost
				sp.Ideal = s.LoggedCost
			}
			out[i] = sp
			continue
		}

		// IdealCC is a synthetic re-derivation with no real 5m/1h split, so
		// — same convention as compute.Summarize — it's priced entirely at
		// the 5m rate (CacheCreation1h left at 0).
		idealStep := compute.StepData{Input: row.IdealIn, CacheCreation: row.IdealCC, CacheRead: row.IdealCR, Output: row.Output}
		paid := compute.PiStepActualCost(s.Data, prices)
		if costIsLogged && s.LoggedCost > 0 {
			paid = s.LoggedCost
		}
		out[i] = stepPricing{Model: s.Model, Paid: paid, Ideal: compute.PiStepActualCost(idealStep, prices), Priced: true}
	}
	return out
}

// sumStepPricing adds up Paid and Ideal across one session's per-step
// pricing.
func sumStepPricing(steps []stepPricing) (paid, ideal float64) {
	for _, s := range steps {
		paid += s.Paid
		ideal += s.Ideal
	}
	return paid, ideal
}

// agentPricingSpec is what computeSessionPricing needs to price one agent's
// steps correctly: how to resolve a model's rate at a given time, whether
// the ideal-cache carry-forward is Claude-style, and whether this agent logs
// its own real billed cost per step (see totalsByAgent's doc comment in
// main.go). It's the single place mapping a store agent name to its pricing
// rules, so totalsByAgent and the web dashboard can never disagree about
// which table/style/logged-cost-preference applies to a given session.
type agentPricingSpec struct {
	Label        string // row label used by `total` ("OC", "PI", "CLAUDE")
	PriceFunc    func(model string, at int64) (compute.ModelPrices, bool)
	ClaudeStyle  bool
	CostIsLogged bool
}

// buildPricingSpecs returns the pricing rules for every agent, keyed by the
// store's lowercase agent name ("opencode", "pi", "claude").
//
// Every PriceFunc here resolves at the timestamp it is handed, so a session
// keeps the rate that was in effect while it ran. Anything that prices a
// stored message must pass that message's own time; passing time.Now() would
// silently reprice history the next time any dated window opens.
func buildPricingSpecs() map[string]agentPricingSpec {
	return map[string]agentPricingSpec{
		"opencode": {
			Label:        "OC",
			PriceFunc:    func(m string, at int64) (compute.ModelPrices, bool) { return resolveAgentPricesAt("opencode", m, at) },
			ClaudeStyle:  false,
			CostIsLogged: true,
		},
		"pi": {
			Label:        "PI",
			PriceFunc:    func(m string, at int64) (compute.ModelPrices, bool) { return resolveAgentPricesAt("pi", m, at) },
			ClaudeStyle:  true,
			CostIsLogged: true,
		},
		"claude": {
			Label: "CLAUDE",
			// Resolved at the message's own timestamp, like the other two
			// agents. This used to hoist claudeGlobalModelPrices() out of
			// the closure and ignore "at" entirely, which repriced every
			// historical session at today's rate — harmless while no
			// built-in model had a dated window in the past, and a 50%
			// overstatement of every pre-cutover Sonnet 5 session the
			// moment claudeSonnet5PriceChange goes by. claudeModelPricesAt
			// is a handful of lookups against memoized overlays, so what
			// the hoist was avoiding (claudeGlobalModelPrices rebuilding a
			// map of every known model) doesn't arise here anyway.
			PriceFunc: func(m string, at int64) (compute.ModelPrices, bool) {
				// at == 0 is "no particular message", used by the callers
				// that only ask whether a model is priceable at all.
				when := time.Now()
				if at > 0 {
					when = time.UnixMilli(at)
				}
				p, ok := claudeModelPricesAt(m, when)
				return p, ok && p.Input > 0
			},
			ClaudeStyle:  true,
			CostIsLogged: false,
		},
	}
}

// sessionStep is one assistant message: its token usage, the model that
// served it, and whatever cost the source logged for it.
//
// Model and LoggedCost sit here rather than in compute.StepData, which stays
// provider-agnostic — the compute package prices tokens and knows nothing
// about model naming or about agents that report their own totals.
type sessionStep struct {
	Model string
	// Provider is only populated when the caller queried for it
	// (sessionStepsForPricing does; aggregateSessionsFromStore's per-step
	// query doesn't, since none of its callers need it). It exists purely
	// so perModelUsage can carry a WebModelUsage.Provider through the same
	// repricing pass that produces Cost, instead of a second query.
	Provider string
	// LoggedCost is the cost the agent's own log reported for this message,
	// or 0 when it reported none. Only PI and OpenCode log a real
	// provider-billed figure; for Claude this is tokeneks' own ingest-time
	// recomputation, so callers must not treat it as ground truth without
	// checking which agent it came from.
	LoggedCost float64
	Data       compute.StepData
	// CreatedAt is this message's own timestamp (ms epoch), needed to
	// resolve a derived opencode/pi rate at the window that was actually in
	// effect when the message ran — a derived model can have more than one
	// effective window now that prices derive fits rate changes as
	// separate time segments (see prices_derive.go). Resolving "the
	// current rate" for every step regardless of when it ran would price a
	// step from before a rate change at the rate that came after it.
	CreatedAt int64
}

// aggregateSessionsFromStore returns CLI-ready session summaries for one agent,
// optionally filtered by date (YYYY-MM-DD) or by a days window.
// If date is non-empty it takes precedence over days.
// Sessions with no messages are excluded — the watcher keeps them in the
// store purely as a mtime-filter baseline (so it doesn't re-parse them on
// every poll), but they're never meant to surface in any list.
//
// date is a local calendar day (whatever a caller's --date flag was typed
// as), so last_activity (a UTC ms epoch) is read with SQLite's 'localtime'
// modifier before taking its date — otherwise date() defaults to UTC and
// the comparison silently shifts by the machine's UTC offset, the same bug
// fixed in claudeSessions (claude.go) and dashboardWindowMs (web.go).
func aggregateSessionsFromStore(ctx context.Context, agent string, days int, date string) ([]CLISession, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	var where string
	var args []any
	if date != "" {
		where = `WHERE agent = ? AND date(last_activity / 1000, 'unixepoch', 'localtime') = ?
		          AND EXISTS (SELECT 1 FROM message m
		                      WHERE m.agent = s.agent AND m.session_id = s.session_id)`
		args = []any{agent, date}
	} else {
		where = `WHERE agent = ? AND last_activity >= ?
		          AND EXISTS (SELECT 1 FROM message m
		                      WHERE m.agent = s.agent AND m.session_id = s.session_id)`
		args = []any{agent, cutoff}
	}

	rows, err := st.DB().QueryContext(ctx, `
		SELECT s.session_id, COALESCE(s.project, ''), COALESCE(s.parent_id, ''),
		       s.created_at, s.last_activity,
		       (SELECT m.model FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1) AS model,
		       COALESCE((SELECT m.provider FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant' ORDER BY m.msg_index ASC LIMIT 1), '') AS provider,
		       COALESCE((SELECT SUM(m.input_tokens)  FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.output_tokens) FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.cache_read)    FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.cache_write)   FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       COALESCE((SELECT SUM(m.cost)          FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant'), 0),
		       (SELECT COUNT(*) FROM message m WHERE m.agent = s.agent AND m.session_id = s.session_id AND m.role = 'assistant')
		FROM session s `+where+` ORDER BY s.created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CLISession{}
	for rows.Next() {
		var s CLISession
		var model, provider *string
		if err := rows.Scan(&s.ID, &s.Title, &s.ParentID, &s.CreatedAt, &s.LastActivity, &model, &provider, &s.TokensIn, &s.TokensOut, &s.CacheRead, &s.CacheWrite, &s.Cost, &s.StepCount); err != nil {
			return nil, err
		}
		if model != nil {
			s.Model = *model
		}
		if provider != nil {
			s.Provider = *provider
		}
		s.Agent = agentDisplayName(agent)

		// One row per assistant message, in the order they happened. The
		// model is needed because sessions routinely switch models mid-run,
		// so pricing a whole session at its first message's rate is wrong;
		// the cost is needed because PI and OpenCode log what the provider
		// actually billed, which beats any recomputation from a rate table;
		// created_at is needed to resolve a derived rate at the window that
		// was actually in effect for that message (see sessionStep.CreatedAt).
		stepRows, err := st.DB().QueryContext(ctx, `
			SELECT input_tokens, cache_read, cache_write, cache_write_1h, output_tokens,
			       COALESCE(model, ''), cost, created_at
			FROM message WHERE agent = ? AND session_id = ? AND role = 'assistant'
			ORDER BY msg_index ASC
		`, agent, s.ID)
		if err != nil {
			return nil, err
		}
		for stepRows.Next() {
			var ss sessionStep
			var in, cr, cw, cw1h, o int
			if err := stepRows.Scan(&in, &cr, &cw, &cw1h, &o, &ss.Model, &ss.LoggedCost, &ss.CreatedAt); err != nil {
				stepRows.Close()
				return nil, err
			}
			ss.Data = compute.StepData{Input: in, CacheRead: cr, CacheCreation: cw, CacheCreation1h: cw1h, Output: o}
			s.Steps = append(s.Steps, ss)
		}
		stepRows.Close()
		out = append(out, s)
	}
	return out, rows.Err()
}

// ensureStoreReady opens the store and brings it up to date before a read.
//
// The sync is incremental: only sessions whose source changed since the
// last run are re-parsed, so this costs about a second for a few thousand
// already-ingested sessions. It used to sync only when the store was
// completely empty, which meant every CLI read after the first one showed
// whatever the last `sync`/`web` run happened to leave behind.
//
// The first run on an empty store has to parse every session, so it says
// so rather than looking hung.
func ensureStoreReady() error {
	st := getTokeneksStore()
	if st == nil {
		var err error
		st, err = openTokeneksStore()
		if err != nil {
			return err
		}
		setTokeneksStore(st)
	}
	ctx := context.Background()
	if n, err := st.CountSessions(ctx, ""); err == nil && n == 0 {
		fmt.Fprintln(os.Stderr, "building the session store for the first time, this takes a while...")
	}
	sources, parsers := buildAgentIO()
	ing := &ingest.Ingestor{
		Store:     st,
		Agents:    agentRegistry.Keys(),
		SourceFor: sources,
		ParserFor: parsers,
	}
	_, err := ing.Sync(ctx)
	return err
}

// avoid "imported and not used" if the file changes
var _ = sql.ErrNoRows
