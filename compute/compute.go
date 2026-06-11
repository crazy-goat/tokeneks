package compute

// ModelPrices holds per-model pricing.
type ModelPrices struct {
	Input                 float64
	Output                float64
	CacheRead             float64
	CacheCreation         float64
	SupportsCacheCreation bool
}

// StepData is the provider-agnostic token usage for a single step.
type StepData struct {
	Input         int
	CacheCreation int
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
	s.Actual = PiStepActualCost(step, prices)
	s.Ideal = PiStepActualCost(idealStep, prices)
	s.Overpay = s.Actual - s.Ideal
	if s.Overpay < 0 {
		s.Overpay = 0
	}
	if s.Ideal > 0 {
		s.PctIdeal = s.Overpay / s.Ideal * 100
	}
	return s
}

func SummarizeClaude(rows []IdealRow, prices ModelPrices) Summary {
	return Summarize(rows, prices)
}

func ComputeIdealClaude(steps []StepData, prices ModelPrices) []IdealRow {
	idealCR := 0
	rows := make([]IdealRow, len(steps))

	for i, s := range steps {
		totalCtx := s.Input + s.CacheCreation + s.CacheRead

		isCompact := false
		if i > 0 {
			prevTotal := steps[i-1].Input + steps[i-1].CacheCreation + steps[i-1].CacheRead
			if totalCtx < prevTotal*CompactThresholdPct/100 {
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

		idealCC := 0
		idealIn := newTokens
		if s.CacheCreation > 0 {
			idealCC = newTokens
			idealIn = 0
		}

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

	_ = prices
	return rows
}

// ComputeIdeal calculates ideal cache_read and waste per step.
func ComputeIdeal(steps []StepData) []IdealRow {
	idealCR := 0
	rows := make([]IdealRow, len(steps))

	for i, s := range steps {
		totalCtx := s.Input + s.CacheRead

		isCompact := false
		if i > 0 {
			prevTotal := steps[i-1].Input + steps[i-1].CacheRead
			if totalCtx < prevTotal*CompactThresholdPct/100 {
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
