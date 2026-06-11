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
	return float64(step.Input)*prices.Input/TokensPerMillion +
		float64(step.CacheCreation)*prices.CacheCreation/TokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/TokensPerMillion +
		float64(step.Output)*prices.Output/TokensPerMillion
}
