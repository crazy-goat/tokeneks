package compute

const TokensPerMillion = 1_000_000

// CompactThresholdPct is the compact-display threshold in percent.
// It is a var (not const) so tests can verify the threshold is used.
var CompactThresholdPct = 80

func PerMillion(cost float64, tokens int) float64 {
	if tokens == 0 {
		return 0
	}
	return cost / float64(tokens) * TokensPerMillion
}

func PiStepActualCost(step StepData, prices ModelPrices) float64 {
	// Anthropic bills 1h-TTL cache writes at 2x base input, not the 1.25x 5m
	// rate; CacheCreation1h==0 (every non-Claude step) collapses this back to
	// the original single-rate formula.
	cc5m := step.CacheCreation - step.CacheCreation1h
	if cc5m < 0 {
		// A handful of logged records report a zero cache_creation total
		// alongside a non-zero 1h slice; without this the 5m term goes
		// negative and quietly discounts the step.
		cc5m = 0
	}
	return float64(step.Input)*prices.Input/TokensPerMillion +
		float64(cc5m)*prices.CacheCreation/TokensPerMillion +
		float64(step.CacheCreation1h)*prices.CacheCreation1h/TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/TokensPerMillion +
		float64(step.Output)*prices.Output/TokensPerMillion
}
