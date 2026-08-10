package compute

// ModelPrices holds per-model pricing.
//
// CacheCreation is the 5-minute-TTL cache write rate. CacheCreation1h is the
// 1-hour-TTL rate (Anthropic bills it at 2x base input, vs 1.25x for 5m).
// Only Claude publishes the split; other agents leave it at zero, which
// makes the actual-cost formula degenerate to the old single-rate one.
type ModelPrices struct {
	Input                 float64
	Output                float64
	CacheRead             float64
	CacheCreation         float64
	CacheCreation1h       float64
	SupportsCacheCreation bool
}

// StepData is the provider-agnostic token usage for a single step.
//
// CacheCreation is the total cache-write tokens (used as-is by the ideal-cost
// algorithm below). CacheCreation1h carries the 1-hour-TTL slice of that
// total, needed only for actual-cost pricing — see PiStepActualCost.
type StepData struct {
	Input           int
	CacheCreation   int
	CacheCreation1h int
	CacheRead       int
	Output          int
}

// IdealRow holds computed ideal values per step.
//
// CacheCreation1h is carried through from StepData purely so Summarize can
// price the actual side correctly; it plays no part in the ideal-cost math.
type IdealRow struct {
	Input           int
	CacheCreation   int
	CacheCreation1h int
	CacheRead       int
	Output          int
	IdealCR         int
	IdealCC         int
	IdealIn         int
	Waste           int
	IsCompact       bool
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
	TotalCC1h    int
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
		s.TotalCC1h += r.CacheCreation1h
		s.TotalCR += r.CacheRead
		s.TotalIn += r.Input
		s.TotalOut += r.Output
		s.TotalIdealCR += r.IdealCR
		s.TotalIdealCC += r.IdealCC
		s.TotalIdealIn += r.IdealIn
		s.TotalWaste += r.Waste
	}
	step := StepData{Input: s.TotalIn, CacheCreation: s.TotalCC, CacheCreation1h: s.TotalCC1h, CacheRead: s.TotalCR, Output: s.TotalOut}
	// The ideal cache-write total (IdealCC) is a synthetic re-derivation, not
	// real usage, so it has no real 5m/1h split — price it entirely at the 5m
	// rate, same as before this fix.
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
			Input:           s.Input,
			CacheCreation:   s.CacheCreation,
			CacheCreation1h: s.CacheCreation1h,
			CacheRead:       s.CacheRead,
			Output:          s.Output,
			IdealCR:         idealCR,
			IdealCC:         idealCC,
			IdealIn:         idealIn,
			Waste:           waste,
			IsCompact:       isCompact,
		}

		// Carry into next step's idealCR everything that is now part of the
		// conversation's context: idealIn and idealCC together reconstruct
		// totalCtx (newTokens split one way or the other, whichever branch
		// ran above), plus this step's output, which gets appended to the
		// transcript and becomes context for the next turn too.
		//
		// Dropping idealIn here (the bug this fixes) throws freshly-read
		// tokens out of the running estimate the moment they're read, so
		// they get charged as brand-new IdealIn again at every subsequent
		// step, forever, at the full input rate rather than the ~6x cheaper
		// cache-read rate. It was invisible whenever CacheCreation > 0
		// because that branch already forces idealIn to 0 and carries
		// idealCC — this line only bites on cache-write-free steps, which is
		// the overwhelming majority of OpenCode's and a small slice of PI's.
		idealCR = idealCR + idealIn + idealCC + s.Output
	}

	_ = prices
	return rows
}

// ComputeIdeal calculates ideal cache_read and waste per step. It is used by
// OpenCode, and by PI for models whose price table has no cache-creation
// rate (prices.SupportsCacheCreation == false) — the print format differs
// (no cache-write column) but the underlying arithmetic no longer needs to.
//
// This used to be a separate implementation that ignored CacheCreation
// entirely — a real bug, since 15%+ of OpenCode's steps do report cache
// writes, and those tokens are real context that would otherwise vanish
// from the ideal-cache estimate. ComputeIdealClaude already handles
// CacheCreation correctly (and degrades to the exact same arithmetic when
// CacheCreation is 0 throughout), so this is now a thin wrapper rather than
// a parallel algorithm that can drift out of sync with it again.
func ComputeIdeal(steps []StepData) []IdealRow {
	return ComputeIdealClaude(steps, ModelPrices{})
}
