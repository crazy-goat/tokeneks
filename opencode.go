package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"tokeneks/compute"
)

const defaultDB = "~/.local/share/opencode/opencode.db"

func ocSteps(sessionID string) ([]compute.StepData, error) {
	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT 
			json_extract(data, '$.tokens.input'),
			json_extract(data, '$.tokens.cache.read'),
			json_extract(data, '$.tokens.output')
		FROM part 
		WHERE session_id = ?
		AND json_extract(data, '$.type') = 'step-finish'
		ORDER BY time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []compute.StepData
	for rows.Next() {
		var s compute.StepData
		if err := rows.Scan(&s.Input, &s.CacheRead, &s.Output); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

func ocStepsBatch(ids []string) (map[string][]compute.StepData, error) {
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
			session_id,
			json_extract(data, '$.tokens.input'),
			json_extract(data, '$.tokens.cache.read'),
			json_extract(data, '$.tokens.output')
		FROM part
		WHERE session_id IN (` + strings.Join(placeholders, ",") + `)
		AND json_extract(data, '$.type') = 'step-finish'
		ORDER BY session_id, time_created
	`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]compute.StepData)
	for rows.Next() {
		var sessionID string
		var step compute.StepData
		if err := rows.Scan(&sessionID, &step.Input, &step.CacheRead, &step.Output); err != nil {
			return nil, err
		}
		result[sessionID] = append(result[sessionID], step)
	}
	return result, rows.Err()
}

func ocSessionSummary(steps []compute.StepData, model string) compute.Summary {
	prices := ocModelPrices[model]
	if prices.Input == 0 {
		prices = ocModelPrices["Kimi K2.6"]
	}
	return compute.Summarize(compute.ComputeIdeal(steps), prices)
}

func ocToolCalls(sessionID string) (int, error) {
	db, err := openOCDB()
	if err != nil {
		return 0, err
	}

	var count int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM part 
		WHERE session_id = ?
		AND json_extract(data, '$.type') = 'tool'
	`, sessionID).Scan(&count)
	return count, err
}

func ocSessionCost(sessionID, model string) (float64, error) {
	steps, err := ocSteps(sessionID)
	if err != nil {
		return 0, err
	}
	prices := ocModelPrices[model]
	if prices.Input == 0 {
		prices = ocModelPrices["Kimi K2.6"]
	}
	return compute.Summarize(compute.ComputeIdeal(steps), prices).Actual, nil
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
			AND date(s.time_created / 1000, 'unixepoch') = ?
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

	prices := ocModelPrices[model]
	if prices.Input == 0 {
		return fmt.Errorf("no prices configured for model %s", model)
	}

	fmt.Printf("Session: %s\n", sessionID)
	fmt.Printf("Title:   %s\n", title)
	fmt.Printf("Model:   %s\n\n", model)

	rows := compute.ComputeIdeal(steps)
	printDetailRows(rows, prices, false)

	s := compute.Summarize(rows, prices)
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

	defaultPrices := ocModelPrices["Kimi K2.6"]

	var totalActual, totalIdeal float64
	var totalIn, totalCR, totalOut int

	modelSet := make(map[string]struct{})
	unpricedModels := make(map[string]struct{})
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
		modelSet[sess.Model] = struct{}{}
		if ocModelPrices[sess.Model].Input == 0 {
			unpricedModels[sess.Model] = struct{}{}
		}
	}

	stepsBySession, err := ocStepsBatch(ids)
	if err != nil {
		return err
	}

	for _, sess := range sessions {
		steps := stepsBySession[sess.ID]
		summary := ocSessionSummary(steps, sess.Model)
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

	if len(unpricedModels) > 0 {
		models := make([]string, 0, len(unpricedModels))
		for m := range unpricedModels {
			models = append(models, m)
		}
		sort.Strings(models)
		fmt.Printf("WARNING: using Kimi K2.6 default pricing for unpriced model(s): %s\n", strings.Join(models, ", "))
	}

	for m := range modelSet {
		p := ocModelPrices[m]
		if p.Input == 0 {
			p = defaultPrices
		}
		fmt.Printf("%s: Input=$%.2f/M  CacheRead=$%.3f/M  Output=$%.2f/M\n", m, p.Input, p.CacheRead, p.Output)
	}

	return nil
}
