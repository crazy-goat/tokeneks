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

// ClaudeIdealRow extends IdealRow with CacheCreation fields
type ClaudeIdealRow struct {
	Input          int
	CacheCreation  int
	CacheRead      int
	Output         int
	IdealCR        int
	IdealCC        int
	IdealIn        int
	Waste          int
	IsCompact      bool
}

func (r ClaudeIdealRow) Note() string {
	if r.IsCompact {
		return "COMPACT"
	}
	if r.Waste == 0 && r.IdealCC == 0 {
		return "HIT"
	}
	if r.CacheRead > 1000 {
		return "PARTIAL"
	}
	return "MISS"
}

// ComputeIdealClaude calculates ideal cache_read for Claude with cache_creation support
func ComputeIdealClaude(steps []StepData, prices ModelPrices) []ClaudeIdealRow {
	idealCR := 0
	rows := make([]ClaudeIdealRow, len(steps))

	for i, s := range steps {
		totalCtx := s.Input + s.CacheCreation + s.CacheRead

		isCompact := false
		if i > 0 {
			prevTotal := steps[i-1].Input + steps[i-1].CacheCreation + steps[i-1].CacheRead
			if totalCtx < prevTotal*80/100 {
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

		rows[i] = ClaudeIdealRow{
			Input:          s.Input,
			CacheCreation:  s.CacheCreation,
			CacheRead:      s.CacheRead,
			Output:         s.Output,
			IdealCR:        idealCR,
			IdealCC:        idealCC,
			IdealIn:        idealIn,
			Waste:          waste,
			IsCompact:      isCompact,
		}

		// Update idealCR for next step: current cache + new CC + output
		idealCR = idealCR + idealCC + s.Output
	}

	return rows
}

// ClaudeSummary extends Summary with CacheCreation
type ClaudeSummary struct {
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

// IdealRow holds computed ideal values per step (original for Kimi)
type IdealRow struct {
	Input     int
	CacheRead int
	Output    int
	IdealIn   int
	IdealCR   int
	Waste     int
	IsCompact bool
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

// Summary holds aggregated totals (original for Kimi)
type Summary struct {
	TotalCR      int
	TotalIn      int
	TotalOut     int
	TotalIdealCR int
	TotalIdealIn int
	TotalWaste   int
	Actual       float64
	Ideal        float64
	Overpay      float64
	PctIdeal     float64
}

func SummarizeClaude(rows []ClaudeIdealRow, prices ModelPrices) ClaudeSummary {
	var s ClaudeSummary
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
	s.Actual = float64(s.TotalIn)*prices.Input/1e6 +
		float64(s.TotalCC)*prices.CacheCreation/1e6 +
		float64(s.TotalCR)*prices.CacheRead/1e6 +
		float64(s.TotalOut)*prices.Output/1e6
	s.Ideal = float64(s.TotalIdealIn)*prices.Input/1e6 +
		float64(s.TotalIdealCC)*prices.CacheCreation/1e6 +
		float64(s.TotalIdealCR)*prices.CacheRead/1e6 +
		float64(s.TotalOut)*prices.Output/1e6
	s.Overpay = s.Actual - s.Ideal
	if s.Overpay < 0 {
		s.Overpay = 0
	}
	if s.Ideal > 0 {
		s.PctIdeal = s.Overpay / s.Ideal * 100
	}
	return s
}

func Summarize(rows []IdealRow, prices ModelPrices) Summary {
	var s Summary
	for _, r := range rows {
		s.TotalCR += r.CacheRead
		s.TotalIn += r.Input
		s.TotalOut += r.Output
		s.TotalIdealCR += r.IdealCR
		s.TotalIdealIn += r.IdealIn
		s.TotalWaste += r.Waste
	}
	s.Actual = float64(s.TotalIn)*prices.Input/1e6 + float64(s.TotalCR)*prices.CacheRead/1e6 + float64(s.TotalOut)*prices.Output/1e6
	s.Ideal = float64(s.TotalIdealIn)*prices.Input/1e6 + float64(s.TotalIdealCR)*prices.CacheRead/1e6 + float64(s.TotalOut)*prices.Output/1e6
	s.Overpay = s.Actual - s.Ideal
	if s.Overpay < 0 {
		s.Overpay = 0
	}
	if s.Ideal > 0 {
		s.PctIdeal = s.Overpay / s.Ideal * 100
	}
	return s
}

func printDetailRowsClaude(rows []ClaudeIdealRow, prices ModelPrices) {
	fmt.Printf("%4s  %7s  %7s  %7s  %6s  │  %8s  %8s  %8s  %6s  │  %7s  %8s\n",
		"Step", "c.read", "c.write", "input", "output", "i_cr", "i_cc", "i_in", "out", "waste", "note")
	fmt.Println(string(strings.Repeat("-", 108)))

	for i, r := range rows {
		fmt.Printf("%4d  %7d  %7d  %7d  %6d  │  %8d  %8d  %8d  %6d  │  %7d  %8s\n",
			i+1, r.CacheRead, r.CacheCreation, r.Input, r.Output,
			r.IdealCR, r.IdealCC, r.IdealIn, r.Output,
			r.Waste, r.Note())
	}

	fmt.Println(string(strings.Repeat("-", 108)))
	s := SummarizeClaude(rows, prices)
	fmt.Printf("%4s  %7d  %7d  %7d  %6d  │  %8d  %8d  %8d  %6d  │  %7d\n",
		"SUM", s.TotalCR, s.TotalCC, s.TotalIn, s.TotalOut,
		s.TotalIdealCR, s.TotalIdealCC, s.TotalIdealIn, s.TotalOut,
		s.TotalWaste)
	fmt.Printf("%4s  %7.2f  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
		"$",
		float64(s.TotalCR)*prices.CacheRead/1e6, float64(s.TotalCC)*prices.CacheCreation/1e6,
		float64(s.TotalIn)*prices.Input/1e6, float64(s.TotalOut)*prices.Output/1e6,
		float64(s.TotalIdealCR)*prices.CacheRead/1e6, float64(s.TotalIdealCC)*prices.CacheCreation/1e6,
		float64(s.TotalIdealIn)*prices.Input/1e6, float64(s.TotalOut)*prices.Output/1e6,
		float64(s.TotalWaste)*(prices.Input-prices.CacheRead)/1e6)
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
			if totalCtx < prevTotal*80/100 {
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
			Input:     s.Input,
			CacheRead: s.CacheRead,
			Output:    s.Output,
			IdealIn:   idealIn,
			IdealCR:   idealCR,
			Waste:     waste,
			IsCompact: isCompact,
		}

		idealCR = idealCR + s.Output
	}

	return rows
}

func printDetailRows(rows []IdealRow, prices ModelPrices) {
	fmt.Printf("%4s  %7s  %7s  %6s  │  %8s  %8s  %6s  │  %7s  %8s\n",
		"Step", "c.read", "input", "output", "i_cr", "i_in", "out", "waste", "note")
	fmt.Println(string(strings.Repeat("-", 88)))

	for i, r := range rows {
		fmt.Printf("%4d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d  %8s\n",
			i+1, r.CacheRead, r.Input, r.Output,
			r.IdealCR, r.IdealIn, r.Output,
			r.Waste, r.Note())
	}

	fmt.Println(string(strings.Repeat("-", 88)))
	s := Summarize(rows, prices)
	fmt.Printf("%4s  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
		"SUM", s.TotalCR, s.TotalIn, s.TotalOut,
		s.TotalIdealCR, s.TotalIdealIn, s.TotalOut,
		s.TotalWaste)
	fmt.Printf("%4s  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
		"$",
		float64(s.TotalCR)*prices.CacheRead/1e6, float64(s.TotalIn)*prices.Input/1e6, float64(s.TotalOut)*prices.Output/1e6,
		float64(s.TotalIdealCR)*prices.CacheRead/1e6, float64(s.TotalIdealIn)*prices.Input/1e6, float64(s.TotalOut)*prices.Output/1e6,
		float64(s.TotalWaste)*(prices.Input-prices.CacheRead)/1e6)
}
