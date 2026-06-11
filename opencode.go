package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const defaultDB = "~/.local/share/opencode/opencode.db"

func ocSteps(sessionID string) ([]StepData, error) {
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

	var steps []StepData
	for rows.Next() {
		var s StepData
		if err := rows.Scan(&s.Input, &s.CacheRead, &s.Output); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, nil
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

type ocSession struct {
	ID               string
	Title            string
	Model            string
	Provider         string
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
				s.cost, ifnull(s.tokens_input,0), ifnull(s.tokens_output,0), ifnull(s.tokens_cache_read,0), ifnull(s.tokens_cache_write,0), ifnull(s.parent_id, '')
			FROM session s
			JOIN part p ON p.session_id = s.id
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
				s.cost, ifnull(s.tokens_input,0), ifnull(s.tokens_output,0), ifnull(s.tokens_cache_read,0), ifnull(s.tokens_cache_write,0), ifnull(s.parent_id, '')
			FROM session s
			JOIN part p ON p.session_id = s.id
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
			&s.Cost, &s.TokensInput, &s.TokensOutput, &s.TokensCacheRead, &s.TokensCacheWrite, &s.ParentID); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
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

	rows := ComputeIdeal(steps)
	printDetailRows(rows, prices)

	s := Summarize(rows, prices)
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
	for _, sess := range sessions {
		steps, err := ocSteps(sess.ID)
		if err != nil {
			continue
		}
		rows := ComputeIdeal(steps)

		prices := ocModelPrices[sess.Model]
		if prices.Input == 0 {
			prices = defaultPrices
			unpricedModels[sess.Model] = struct{}{}
		}
		modelSet[sess.Model] = struct{}{}

		s := Summarize(rows, prices)

		totalActual += s.Actual
		totalIdeal += s.Ideal
		totalIn += s.TotalIn
		totalCR += s.TotalCR
		totalOut += s.TotalOut

		timestamp := time.Unix(sess.CreatedAt/1000, 0).UTC().Format("2006-01-02 15:04:05")
		shortTitle := sess.Title
		if len(shortTitle) > 30 {
			shortTitle = shortTitle[:28] + ".."
		}

		tokens := s.TotalIn + s.TotalCR + s.TotalOut
		costPer1M := perMillion(s.Actual, tokens)
		idealPer1M := perMillion(s.Ideal, tokens)

		modelDisplay := sess.Model
		if sess.Provider != "" {
			modelDisplay = sess.Provider + "/" + sess.Model
		}
		fmt.Printf("%19s  %-18.18s  %-27s  %-30s  %5d  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
			timestamp, modelDisplay, sess.ID, shortTitle, sess.Steps, formatTokens(tokens), s.Actual, s.Ideal, s.Overpay, s.PctIdeal, costPer1M, idealPer1M)
	}

	fmt.Println(strings.Repeat("-", separatorWidthOpenCode))
	totalOverpay := max(totalActual-totalIdeal, 0)
	pct := 0.0
	if totalIdeal > 0 {
		pct = totalOverpay / totalIdeal * 100
	}

	totalTokens := totalIn + totalCR + totalOut
	totalCostPer1M := perMillion(totalActual, totalTokens)
	totalIdealPer1M := perMillion(totalIdeal, totalTokens)

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
