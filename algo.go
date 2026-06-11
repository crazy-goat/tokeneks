package main

import (
	"fmt"
	"strings"
)

// Prices per 1M tokens
const (
	PriceInput     = 0.95
	PriceCacheRead = 0.16
	PriceOutput    = 4.00
)

// OpenCode model prices
var ocModelPrices = map[string]ModelPrices{
	"Kimi K2.6": {
		Input:     0.95,
		CacheRead: 0.16,
		Output:    4.00,
	},
	"GPT 5.4 mini": {
		Input:     0.75,
		CacheRead: 0.075,
		Output:    4.50,
	},
}

// ModelPrices holds per-model pricing
type ModelPrices struct {
	Input         float64
	CacheCreation float64
	CacheRead     float64
	Output        float64
}

// Add CacheCreation to StepData
type StepData struct {
	Input         int
	CacheCreation int // 0 for Kimi, populated for Claude
	CacheRead     int
	Output        int
}

// IdealRow holds computed ideal values per step.
type IdealRow struct {
	Input         int
	CacheCreation int
	CacheRead     int
	Output        int
	IdealCR       int
	IdealCC       int
	IdealIn       int
	Waste         int
	IsCompact     bool
}

func (r IdealRow) Note() string {
	if r.IsCompact {
		return "COMPACT"
	}
	if r.Waste == 0 {
		return "HIT"
	}
	if r.CacheRead > 1000 {
		return "PARTIAL"
	}
	return "MISS"
}

// Backwards-compatible aliases for existing call sites.
type ClaudeIdealRow = IdealRow

type ClaudeSummary = Summary

// Summary holds aggregated totals.
type Summary struct {
	TotalCC      int
	TotalCR      int
	TotalIn      int
	TotalOut     int
	TotalIdealCR int
	TotalIdealCC int
	TotalIdealIn int
	TotalWaste   int
	Actual       float64
	Ideal        float64
	Overpay      float64
	PctIdeal     float64
}

func Summarize(rows []IdealRow, prices ModelPrices) Summary {
	var s Summary
	for _, r := range rows {
		s.TotalCC += r.CacheCreation
		s.TotalCR += r.CacheRead
		s.TotalIn += r.Input
		s.TotalOut += r.Output
		s.TotalIdealCR += r.IdealCR
		s.TotalIdealCC += r.IdealCC
		s.TotalIdealIn += r.IdealIn
		s.TotalWaste += r.Waste
	}
	step := StepData{Input: s.TotalIn, CacheCreation: s.TotalCC, CacheRead: s.TotalCR, Output: s.TotalOut}
	idealStep := StepData{Input: s.TotalIdealIn, CacheCreation: s.TotalIdealCC, CacheRead: s.TotalIdealCR, Output: s.TotalOut}
	s.Actual = piStepActualCost(step, prices)
	s.Ideal = piStepActualCost(idealStep, prices)
	s.Overpay = s.Actual - s.Ideal
	if s.Overpay < 0 {
		s.Overpay = 0
	}
	if s.Ideal > 0 {
		s.PctIdeal = s.Overpay / s.Ideal * 100
	}
	return s
}

func SummarizeClaude(rows []ClaudeIdealRow, prices ModelPrices) ClaudeSummary {
	return Summarize(rows, prices)
}

func ComputeIdealClaude(steps []StepData, prices ModelPrices) []ClaudeIdealRow {
	idealCR := 0
	rows := make([]ClaudeIdealRow, len(steps))

	for i, s := range steps {
		totalCtx := s.Input + s.CacheCreation + s.CacheRead

		isCompact := false
		if i > 0 {
			prevTotal := steps[i-1].Input + steps[i-1].CacheCreation + steps[i-1].CacheRead
			if totalCtx < prevTotal*compactThresholdPct/100 {
				isCompact = true
				idealCR = s.CacheRead
			}
		} else if idealCR > totalCtx {
			idealCR = totalCtx
		}

		// Ideal cache read = min(ideal context, total context)
		if idealCR > totalCtx {
			idealCR = totalCtx
		}

		newTokens := totalCtx - idealCR
		if newTokens < 0 {
			newTokens = 0
		}

		idealCC := 0
		idealIn := newTokens
		if s.CacheCreation > 0 {
			// In ideal scenario: new tokens are cache_creation (written for next step reuse)
			// Regular input is 0 (everything cacheable)
			idealCC = newTokens
			idealIn = 0
		}

		// Waste = what should have been cache_read but wasn't (or was input/CC)
		waste := idealCR - s.CacheRead
		if waste < 0 {
			waste = 0
		}

		rows[i] = IdealRow{
			Input:         s.Input,
			CacheCreation: s.CacheCreation,
			CacheRead:     s.CacheRead,
			Output:        s.Output,
			IdealCR:       idealCR,
			IdealCC:       idealCC,
			IdealIn:       idealIn,
			Waste:         waste,
			IsCompact:     isCompact,
		}

		idealCR = idealCR + idealCC + s.Output
	}

	return rows
}

func printDetailRows(rows []IdealRow, prices ModelPrices, showCC bool) {
	if showCC {
		fmt.Printf("%4s  %7s  %7s  %7s  %6s  │  %8s  %8s  %6s  │  %7s  %8s\n",
			"Step", "c.read", "c.write", "input", "output", "i_cr", "i_cc", "out", "waste", "note")
		fmt.Println(strings.Repeat("-", separatorWidthClaude))

		for i, r := range rows {
			fmt.Printf("%4d  %7d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d  %8s\n",
				i+1, r.CacheRead, r.CacheCreation, r.Input, r.Output,
				r.IdealCR, r.IdealCC, r.Output,
				r.Waste, r.Note())
		}

		fmt.Println(strings.Repeat("-", separatorWidthClaude))
		s := SummarizeClaude(rows, prices)
		fmt.Printf("%4s  %7d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
			"SUM", s.TotalCR, s.TotalCC, s.TotalIn, s.TotalOut,
			s.TotalIdealCR, s.TotalIdealCC, s.TotalOut,
			s.TotalWaste)
		fmt.Printf("%4s  %7.2f  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
			"$",
			float64(s.TotalCR)*prices.CacheRead/tokensPerMillion, float64(s.TotalCC)*prices.CacheCreation/tokensPerMillion,
			float64(s.TotalIn)*prices.Input/tokensPerMillion, float64(s.TotalOut)*prices.Output/tokensPerMillion,
			float64(s.TotalIdealCR)*prices.CacheRead/tokensPerMillion, float64(s.TotalIdealCC)*prices.CacheCreation/tokensPerMillion,
			float64(s.TotalOut)*prices.Output/tokensPerMillion,
			float64(s.TotalWaste)*(prices.Input-prices.CacheRead)/tokensPerMillion)
		return
	}

	fmt.Printf("%4s  %7s  %7s  %6s  │  %8s  %8s  %6s  │  %7s  %8s\n",
		"Step", "c.read", "input", "output", "i_cr", "i_in", "out", "waste", "note")
	fmt.Println(strings.Repeat("-", separatorWidthKimi))

	for i, r := range rows {
		fmt.Printf("%4d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d  %8s\n",
			i+1, r.CacheRead, r.Input, r.Output,
			r.IdealCR, r.IdealIn, r.Output,
			r.Waste, r.Note())
	}

	fmt.Println(strings.Repeat("-", separatorWidthKimi))
	s := Summarize(rows, prices)
	fmt.Printf("%4s  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
		"SUM", s.TotalCR, s.TotalIn, s.TotalOut,
		s.TotalIdealCR, s.TotalIdealIn, s.TotalOut,
		s.TotalWaste)
	fmt.Printf("%4s  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
		"$",
		float64(s.TotalCR)*prices.CacheRead/tokensPerMillion, float64(s.TotalIn)*prices.Input/tokensPerMillion, float64(s.TotalOut)*prices.Output/tokensPerMillion,
		float64(s.TotalIdealCR)*prices.CacheRead/tokensPerMillion, float64(s.TotalIdealIn)*prices.Input/tokensPerMillion, float64(s.TotalOut)*prices.Output/tokensPerMillion,
		float64(s.TotalWaste)*(prices.Input-prices.CacheRead)/tokensPerMillion)
}

func printDetailRowsClaude(rows []IdealRow, prices ModelPrices) {
	printDetailRows(rows, prices, true)
}

// ComputeIdeal calculates ideal cache_read and waste per step (original for Kimi K2.6)
func ComputeIdeal(steps []StepData) []IdealRow {
	idealCR := 0
	rows := make([]IdealRow, len(steps))

	for i, s := range steps {
		totalCtx := s.Input + s.CacheRead

		isCompact := false
		if i > 0 {
			prevTotal := steps[i-1].Input + steps[i-1].CacheRead
			if totalCtx < prevTotal*compactThresholdPct/100 {
				isCompact = true
				idealCR = s.CacheRead
			}
		} else if idealCR > totalCtx {
			idealCR = totalCtx
		}

		if idealCR > totalCtx {
			idealCR = totalCtx
		}

		newTokens := totalCtx - idealCR
		if newTokens < 0 {
			newTokens = 0
		}

		idealIn := newTokens

		waste := idealCR - s.CacheRead
		if waste < 0 {
			waste = 0
		}

		rows[i] = IdealRow{
			Input:         s.Input,
			CacheCreation: s.CacheCreation,
			CacheRead:     s.CacheRead,
			Output:        s.Output,
			IdealIn:       idealIn,
			IdealCR:       idealCR,
			IdealCC:       0,
			Waste:         waste,
			IsCompact:     isCompact,
		}

		idealCR = idealCR + s.Output
	}

	return rows
}
