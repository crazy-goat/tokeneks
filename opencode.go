package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"tokeneks/compute"
)

const defaultDB = "~/.local/share/opencode/opencode.db"

// ocStep pairs a step's token usage with the cost OpenCode itself logged for
// it (part.data.cost — the reseller-billed figure, not a rate-table guess)
// and the model that actually served it. Cost lives outside
// compute.StepData rather than as a new field on it because that type is
// shared with agents (PI's Claude-style steps) whose logged-cost plumbing
// is separate; keeping it here mirrors sessionStep in web_store.go, which
// pairs the same two things for the ingested store.
//
// Model does not live on the part row itself — OpenCode's step-finish parts
// carry only tokens/cost, never a model id. It lives on the *message* the
// part belongs to (message.data.modelID, or message.data.model.modelID on
// some rows — confirmed against the live db, 0 of 97608 step-finish rows
// have no resolvable model via that fallback chain), so ocSteps/
// ocStepsBatch join part to message on part.message_id to reach it. Without
// this, every step in a session was priced at the session-level
// s.model — sess.Model — even though OpenCode sessions routinely switch
// models mid-run (27% of ingested OC steps, per the store), which silently
// misattributed a switched-to model's tokens to whatever model happened to
// go first.
type ocStep struct {
	Data       compute.StepData
	LoggedCost float64
	Model      string
}

func ocSteps(sessionID string) ([]ocStep, error) {
	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT
			json_extract(p.data, '$.tokens.input'),
			json_extract(p.data, '$.tokens.cache.read'),
			ifnull(json_extract(p.data, '$.tokens.cache.write'), 0),
			json_extract(p.data, '$.tokens.output'),
			ifnull(json_extract(p.data, '$.cost'), 0),
			ifnull(json_extract(m.data, '$.modelID'), ifnull(json_extract(m.data, '$.model.modelID'), ''))
		FROM part p
		LEFT JOIN message m ON m.id = p.message_id
		WHERE p.session_id = ?
		AND json_extract(p.data, '$.type') = 'step-finish'
		ORDER BY p.time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []ocStep
	for rows.Next() {
		var s ocStep
		if err := rows.Scan(&s.Data.Input, &s.Data.CacheRead, &s.Data.CacheCreation, &s.Data.Output, &s.LoggedCost, &s.Model); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

func ocStepsBatch(ids []string) (map[string][]ocStep, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `
		SELECT
			p.session_id,
			json_extract(p.data, '$.tokens.input'),
			json_extract(p.data, '$.tokens.cache.read'),
			ifnull(json_extract(p.data, '$.tokens.cache.write'), 0),
			json_extract(p.data, '$.tokens.output'),
			ifnull(json_extract(p.data, '$.cost'), 0),
			ifnull(json_extract(m.data, '$.modelID'), ifnull(json_extract(m.data, '$.model.modelID'), ''))
		FROM part p
		LEFT JOIN message m ON m.id = p.message_id
		WHERE p.session_id IN (` + strings.Join(placeholders, ",") + `)
		AND json_extract(p.data, '$.type') = 'step-finish'
		ORDER BY p.session_id, p.time_created
	`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]ocStep)
	for rows.Next() {
		var sessionID string
		var step ocStep
		if err := rows.Scan(&sessionID, &step.Data.Input, &step.Data.CacheRead, &step.Data.CacheCreation, &step.Data.Output, &step.LoggedCost, &step.Model); err != nil {
			return nil, err
		}
		result[sessionID] = append(result[sessionID], step)
	}
	return result, rows.Err()
}

// ocActualCost is OpenCode's half of the same preference totalsByAgent
// applies in main.go: prefer what was actually billed over what a static
// rate table would compute for the same tokens, because the billed figure
// already accounts for the reseller's real rates, discounts and any
// mid-window price change that the table cannot express. The preference is
// applied per step, not per session, so a session where only some steps
// logged a cost (OpenCode occasionally omits it) still blends correctly
// instead of falling entirely to one source or the other.
func ocActualCost(steps []ocStep, prices compute.ModelPrices) float64 {
	var actual float64
	for _, s := range steps {
		if s.LoggedCost > 0 {
			actual += s.LoggedCost
			continue
		}
		actual += compute.PiStepActualCost(s.Data, prices)
	}
	return actual
}

// ocSessionSummary prices a session's steps at each step's own resolved
// model rate, mirroring totalsByAgent's per-row approach in main.go for the
// identical defect: OpenCode sessions routinely switch models mid-run (13 of
// 175 ingested sessions covering 27% of all OC steps), so pricing a whole
// session at one model — the old behavior, via a single model argument
// resolved once — silently misattributed every switched-to model's tokens
// to whichever model happened to serve the session's first step.
//
// The ideal-cache computation still runs exactly once, over the session's
// full, untouched step sequence, before any per-model split happens.
// ComputeIdeal models one continuous conversation: each step's ideal cache
// read is derived from how much of the *previous* step's context could
// have been reused, and that pointer carries forward step to step.
// Splitting the session by model first — pricing each model's steps as
// their own isolated ComputeIdeal call — would throw that carried-forward
// state away at every model switch, forcing a cold start right there and
// inflating Ideal past the real Actual cost (see
// TestTotalsByAgent_SessionSpanningTwoModelsPricedPerModel in
// total_test.go, which guards the identical case for PI/Claude). Only the
// per-row $ conversion below uses that row's own model rate; the
// token-level algorithm itself never sees which model served which row.
//
// Actual prefers each row's own logged cost (the same preference
// ocActualCost applies, just inlined here instead of pooled across the
// whole session so a row's fallback never borrows another row's model
// rate), falling back to that row's own model rate when nothing was
// logged. A row whose model has no price anywhere in resolveAgentPrices'
// chain is excluded from both Actual and Ideal — there is nothing to price
// it at, and guessing would look exactly as authoritative as a real
// number — and its tokens plus logged cost are returned in unpriced
// instead, keyed by the model that actually served it rather than by
// whatever model labels the session as a whole. That keying is what lets
// this report line up with totalsByAgent's OC unpriced rows, which already
// attribute per step.
//
// The bool return is false only when not one step in the session could be
// priced at all — the caller should drop the session's row entirely in
// that case, same as the old "session has no price" contract, rather than
// print an all-zero line that would look like a real free session sitting
// among priced ones.
func ocSessionSummary(steps []ocStep) (compute.Summary, []unpricedModel, bool) {
	tokenSteps := make([]compute.StepData, len(steps))
	for i, s := range steps {
		tokenSteps[i] = s.Data
	}
	idealRows := compute.ComputeIdeal(tokenSteps)

	var summary compute.Summary
	unpricedByModel := map[string]*unpricedModel{}
	anyPriced := false

	for i, row := range idealRows {
		st := steps[i]
		prices, ok := resolveAgentPrices("opencode", st.Model)
		if !ok {
			u := unpricedByModel[st.Model]
			if u == nil {
				u = &unpricedModel{Agent: "OC", Model: st.Model}
				unpricedByModel[st.Model] = u
			}
			u.Steps++
			u.Tokens += row.Input + row.CacheCreation + row.CacheRead + row.Output
			u.LoggedCost += st.LoggedCost
			continue
		}
		anyPriced = true

		summary.TotalIn += row.Input
		summary.TotalCC += row.CacheCreation
		summary.TotalCC1h += row.CacheCreation1h
		summary.TotalCR += row.CacheRead
		summary.TotalOut += row.Output
		summary.TotalIdealIn += row.IdealIn
		summary.TotalIdealCC += row.IdealCC
		summary.TotalIdealCR += row.IdealCR
		summary.TotalWaste += row.Waste

		if st.LoggedCost > 0 {
			summary.Actual += st.LoggedCost
		} else {
			summary.Actual += compute.PiStepActualCost(st.Data, prices)
		}
		// IdealCC is a synthetic re-derivation with no real 5m/1h split, so —
		// same convention as compute.Summarize — it's priced entirely at the
		// 5m rate (CacheCreation1h left at 0).
		idealStep := compute.StepData{Input: row.IdealIn, CacheCreation: row.IdealCC, CacheRead: row.IdealCR, Output: row.Output}
		summary.Ideal += compute.PiStepActualCost(idealStep, prices)
	}

	summary.Overpay = max(summary.Actual-summary.Ideal, 0)
	if summary.Ideal > 0 {
		summary.PctIdeal = summary.Overpay / summary.Ideal * 100
	}

	unpriced := make([]unpricedModel, 0, len(unpricedByModel))
	for _, u := range unpricedByModel {
		unpriced = append(unpriced, *u)
	}

	return summary, unpriced, anyPriced
}

type ocSession struct {
	ID               string
	Title            string
	Model            string
	Provider         string
	Project          string
	Steps            int
	Cost             float64
	TokensInput      int
	TokensOutput     int
	TokensCacheRead  int
	TokensCacheWrite int
	CreatedAt        int64
	LastActivity     int64
	ParentID         string
}

// ocSessions returns OpenCode session summaries, optionally filtered to one
// calendar date. date is the user-typed --date, a local calendar day, so the
// SQL below reads s.time_created (UTC ms epoch) with the 'localtime'
// modifier before taking its date — SQLite's date() defaults to UTC, which
// would otherwise shift the comparison by the machine's UTC offset and drop
// sessions from around local midnight, the same bug fixed in claudeSessions
// (claude.go) and dashboardWindowMs (web.go).
func ocSessions(days int, date string) ([]ocSession, error) {
	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	var query string
	var args []any

	if date != "" {
		query = `
			SELECT s.id, s.title, json_extract(s.model, '$.id'), ifnull(json_extract(s.model, '$.providerID'),''), s.time_created, ifnull(MAX(p.time_created), s.time_created), count(*) as steps,
				ifnull(s.tokens_input,0), ifnull(s.tokens_output,0), ifnull(s.tokens_cache_read,0), ifnull(s.tokens_cache_write,0), ifnull(s.parent_id, ''), ifnull(sum(json_extract(p.data, '$.cost')), 0),
				coalesce(pr.name, pr.worktree, '')
			FROM session s
			JOIN part p ON p.session_id = s.id
			LEFT JOIN project pr ON pr.id = s.project_id
			WHERE json_extract(p.data, '$.type') = 'step-finish'
			AND date(s.time_created / 1000, 'unixepoch', 'localtime') = ?
			GROUP BY s.id
			ORDER BY s.time_created ASC
		`
		args = append(args, date)
	} else {
		cutoff := fmt.Sprintf("-%d days", days)
		query = `
			SELECT s.id, s.title, json_extract(s.model, '$.id'), ifnull(json_extract(s.model, '$.providerID'),''), s.time_created, ifnull(MAX(p.time_created), s.time_created), count(*) as steps,
				ifnull(s.tokens_input,0), ifnull(s.tokens_output,0), ifnull(s.tokens_cache_read,0), ifnull(s.tokens_cache_write,0), ifnull(s.parent_id, ''), ifnull(sum(json_extract(p.data, '$.cost')), 0),
				coalesce(pr.name, pr.worktree, '')
			FROM session s
			JOIN part p ON p.session_id = s.id
			LEFT JOIN project pr ON pr.id = s.project_id
			WHERE json_extract(p.data, '$.type') = 'step-finish'
			AND s.time_created > (strftime('%s', 'now', ?) * 1000)
			GROUP BY s.id
			ORDER BY s.time_created ASC
		`
		args = append(args, cutoff)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []ocSession
	for rows.Next() {
		var s ocSession
		if err := rows.Scan(&s.ID, &s.Title, &s.Model, &s.Provider, &s.CreatedAt, &s.LastActivity, &s.Steps,
			&s.TokensInput, &s.TokensOutput, &s.TokensCacheRead, &s.TokensCacheWrite, &s.ParentID, &s.Cost, &s.Project); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

func ocDetail(sessionID string) error {
	steps, err := ocSteps(sessionID)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("no step-finish data for session %s", sessionID)
	}

	db, err := openOCDB()
	if err != nil {
		return err
	}
	var title, model string
	if err := db.QueryRow("SELECT title, json_extract(model, '$.id') FROM session WHERE id = ?", sessionID).Scan(&title, &model); err != nil {
		return err
	}

	fmt.Printf("Session: %s\n", sessionID)
	fmt.Printf("Title:   %s\n", title)
	fmt.Printf("Model:   %s\n\n", model)

	tokenSteps := make([]compute.StepData, len(steps))
	pricing := make([]detailRowPrice, len(steps))
	for i, st := range steps {
		tokenSteps[i] = st.Data
		p, ok := resolveAgentPrices("opencode", st.Model)
		pricing[i] = detailRowPrice{Model: st.Model, Prices: p, Priced: ok, LoggedCost: st.LoggedCost}
	}
	rows := compute.ComputeIdeal(tokenSteps)
	// The table prices each row strictly from its own resolved rate — same
	// per-step rate ocSessionSummary uses below — and never from that row's
	// logged cost (see detailRowCost's doc comment in algo.go). The headline
	// further down applies the opposite preference: it takes each row's
	// logged cost over its rate whenever OpenCode actually logged one. Those
	// are two legitimate, differently-sourced numbers for the same session —
	// what tokeneks' price table would have charged vs. what the provider
	// actually billed — not a table that's "wrong" relative to the headline.
	// printDetailCostReconciliation below names the resulting gap explicitly
	// whenever it's real and large enough to matter, instead of leaving the
	// reader to notice the table's $ sum doesn't match the headline and
	// wonder why.
	printDetailRows(rows, pricing, false)
	printDetailCostReconciliation(computeDetailCostRecon(rows, pricing))

	// The headline takes Actual/Ideal/Overpay/%ideal from ocSessionSummary's
	// per-step pass, which prefers each row's own logged cost over its rate
	// — see computeDetailCostRecon's doc comment for how that lines up with
	// (and diverges from) the rate-derived table above.
	s, _, ok := ocSessionSummary(steps)
	if !ok {
		return fmt.Errorf("no prices configured for any model served in session %s", sessionID)
	}
	fmt.Printf("\nActual paid:  $%.2f\n", s.Actual)
	fmt.Printf("Ideal paid:   $%.2f\n", s.Ideal)
	fmt.Printf("Overpay:      $%.2f (%.1f%% of ideal)\n", s.Overpay, s.PctIdeal)

	return nil
}

func ocList(days int, date string) error {
	sessions, err := ocSessions(days, date)
	if err != nil {
		return err
	}

	fmt.Printf("%19s  %-18s  %-27s  %-30s  %5s  %7s  %6s  %6s  %8s  %7s  %7s  %7s\n",
		"DateTime", "DominantModel", "Session", "Title", "Steps", "Tokens", "Paid", "Ideal", "Overpay", "%ideal", "$/1M", "i$/1M")
	fmt.Println(strings.Repeat("-", separatorWidthOpenCode))

	var totalActual, totalIdeal float64
	var totalIn, totalCR, totalOut int

	modelSet := make(map[string]struct{})
	unpricedByModel := map[string]*unpricedModel{}
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
		modelSet[sess.Model] = struct{}{}
	}

	stepsBySession, err := ocStepsBatch(ids)
	if err != nil {
		return err
	}

	for _, sess := range sessions {
		steps := stepsBySession[sess.ID]
		// modelSet feeds the per-model rate footer below. sess.Model alone
		// (added above) would miss any model that only ever served a
		// non-first step of a multi-model session — the exact case this
		// change exists to stop mispricing — so every step's own model is
		// folded in too.
		for _, st := range steps {
			modelSet[st.Model] = struct{}{}
		}

		summary, unpriced, ok := ocSessionSummary(steps)
		// Fold in this session's per-step unpriced attribution regardless of
		// ok — a session can have some priced steps and some not, and each
		// unpriced step's tokens/cost belong to the model that actually
		// served it, not to sess.Model.
		for _, u := range unpriced {
			cur := unpricedByModel[u.Model]
			if cur == nil {
				cur = &unpricedModel{Agent: "OC", Model: u.Model}
				unpricedByModel[u.Model] = cur
			}
			cur.Tokens += u.Tokens
			cur.Steps += u.Steps
			cur.LoggedCost += u.LoggedCost
		}
		if !ok {
			// Not one step in this session could be priced — exclude the
			// whole row rather than print it at a $0 cost that would look
			// as real as the priced rows around it. Its tokens/logged cost
			// already landed in unpricedByModel above. warnNoAgentPrice
			// (inside resolveAgentPrices, called by ocSessionSummary)
			// already told stderr why.
			continue
		}
		totalActual += summary.Actual
		totalIdeal += summary.Ideal
		totalIn += summary.TotalIn
		totalCR += summary.TotalCR
		totalOut += summary.TotalOut

		timestamp := time.Unix(sess.CreatedAt/1000, 0).UTC().Format("2006-01-02 15:04:05")
		shortTitle := sess.Title
		if len(shortTitle) > 30 {
			shortTitle = shortTitle[:28] + ".."
		}

		tokens := summary.TotalIn + summary.TotalCR + summary.TotalOut
		costPer1M := compute.PerMillion(summary.Actual, tokens)
		idealPer1M := compute.PerMillion(summary.Ideal, tokens)

		modelDisplay := sess.Model
		if sess.Provider != "" {
			modelDisplay = sess.Provider + "/" + sess.Model
		}
		fmt.Printf("%19s  %-18.18s  %-27s  %-30s  %5d  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
			timestamp, modelDisplay, sess.ID, shortTitle, sess.Steps, formatTokens(tokens), summary.Actual, summary.Ideal, summary.Overpay, summary.PctIdeal, costPer1M, idealPer1M)
	}

	fmt.Println(strings.Repeat("-", separatorWidthOpenCode))
	totalTokens := totalIn + totalCR + totalOut
	totalOverpay, pct, totalCostPer1M, totalIdealPer1M := footerTotals(totalActual, totalIdeal, totalTokens)

	fmt.Printf("%19s  %-18s  %-27s  %-30s  %5s  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
		"TOTAL", "", "", "", "", formatTokens(totalTokens), totalActual, totalIdeal, totalOverpay, pct, totalCostPer1M, totalIdealPer1M)
	fmt.Println()

	unpriced := make([]unpricedModel, 0, len(unpricedByModel))
	for _, u := range unpricedByModel {
		unpriced = append(unpriced, *u)
	}
	sort.Slice(unpriced, func(i, j int) bool { return unpriced[i].Model < unpriced[j].Model })
	printUnpricedModels(unpriced)

	for m := range modelSet {
		p, ok := resolveAgentPrices("opencode", m)
		if !ok {
			continue // already reported above; nothing real to print here
		}
		fmt.Printf("%s: Input=$%.2f/M  CacheRead=$%.3f/M  Output=$%.2f/M\n", m, p.Input, p.CacheRead, p.Output)
	}

	return nil
}
