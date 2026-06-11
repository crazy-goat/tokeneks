package main

import (
	"fmt"
	"strings"

	"tokeneks/compute"
)

// Prices per 1M tokens
const (
	PriceInput     = 0.95
	PriceCacheRead = 0.16
	PriceOutput    = 4.00
)

// OpenCode model prices
var ocModelPrices = map[string]compute.ModelPrices{
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

func printDetailRows(rows []compute.IdealRow, prices compute.ModelPrices, showCC bool) {
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
		s := compute.SummarizeClaude(rows, prices)
		fmt.Printf("%4s  %7d  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
			"SUM", s.TotalCR, s.TotalCC, s.TotalIn, s.TotalOut,
			s.TotalIdealCR, s.TotalIdealCC, s.TotalOut,
			s.TotalWaste)
		fmt.Printf("%4s  %7.2f  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
			"$",
			float64(s.TotalCR)*prices.CacheRead/compute.TokensPerMillion, float64(s.TotalCC)*prices.CacheCreation/compute.TokensPerMillion,
			float64(s.TotalIn)*prices.Input/compute.TokensPerMillion, float64(s.TotalOut)*prices.Output/compute.TokensPerMillion,
			float64(s.TotalIdealCR)*prices.CacheRead/compute.TokensPerMillion, float64(s.TotalIdealCC)*prices.CacheCreation/compute.TokensPerMillion,
			float64(s.TotalOut)*prices.Output/compute.TokensPerMillion,
			float64(s.TotalWaste)*(prices.Input-prices.CacheRead)/compute.TokensPerMillion)
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
	s := compute.Summarize(rows, prices)
	fmt.Printf("%4s  %7d  %7d  %6d  │  %8d  %8d  %6d  │  %7d\n",
		"SUM", s.TotalCR, s.TotalIn, s.TotalOut,
		s.TotalIdealCR, s.TotalIdealIn, s.TotalOut,
		s.TotalWaste)
	fmt.Printf("%4s  %7.2f  %7.2f  %6.2f  │  %8.2f  %8.2f  %6.2f  │  %7.2f\n",
		"$",
		float64(s.TotalCR)*prices.CacheRead/compute.TokensPerMillion, float64(s.TotalIn)*prices.Input/compute.TokensPerMillion, float64(s.TotalOut)*prices.Output/compute.TokensPerMillion,
		float64(s.TotalIdealCR)*prices.CacheRead/compute.TokensPerMillion, float64(s.TotalIdealIn)*prices.Input/compute.TokensPerMillion, float64(s.TotalOut)*prices.Output/compute.TokensPerMillion,
		float64(s.TotalWaste)*(prices.Input-prices.CacheRead)/compute.TokensPerMillion)
}

func printDetailRowsClaude(rows []compute.IdealRow, prices compute.ModelPrices) {
	printDetailRows(rows, prices, true)
}
