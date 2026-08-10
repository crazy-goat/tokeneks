package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"tokeneks/compute"
	"tokeneks/store"
)

// deriveMsg is one assistant message's tokens and logged cost, the raw input
// to the least-squares fit below.
type deriveMsg struct {
	Input      int
	CacheRead  int
	CacheWrite int
	Output     int
	Cost       float64
	CreatedAt  int64 // ms epoch; only used for change-point detection (see detectSegments)
}

// nnlsSolve fits y ≈ X·coef with every coefficient constrained to be >= 0.
//
// The approach: solve the unconstrained normal equations; if any fitted
// coefficient comes back negative, pin it to zero, drop that column, and
// re-solve on what's left; repeat until nothing left is negative. This is
// the standard active-set idea for non-negative least squares, and it's
// enough for the small (<=4 variable) systems here — a naive unconstrained
// fit on this data produces nonsense like a negative input rate, and this
// loop is what stops that from ever being reported as a "rate".
//
// A column that is exactly zero across every row is unidentifiable before
// the solver even runs — no amount of data can tell you the rate for a
// token type nobody used — so it's pinned to zero up front rather than fed
// in, where it would otherwise be free to take on an arbitrary value.
//
// Non-negativity does NOT imply identifiability, though: a column that is
// nonzero but whose dollar contribution is tiny next to the others can
// still land on an arbitrary non-negative value with ~0% residual, because
// almost any value for it is equally consistent with the data. That's a
// separate problem from what this function solves — see fitModelRates'
// per-column cost-share, which is what actually catches it.
func nnlsSolve(X [][]float64, y []float64) []float64 {
	var n int
	if len(X) > 0 {
		n = len(X[0])
	}
	coef := make([]float64, n)

	active := make([]int, 0, n)
	for j := 0; j < n; j++ {
		zero := true
		for _, row := range X {
			if row[j] != 0 {
				zero = false
				break
			}
		}
		if !zero {
			active = append(active, j)
		}
	}

	for len(active) > 0 {
		sub := solveOLS(X, y, active)

		worst := -1
		worstVal := 0.0
		for k, j := range active {
			if sub[k] < -1e-9 && (worst == -1 || sub[k] < worstVal) {
				worst = j
				worstVal = sub[k]
			}
		}
		if worst == -1 {
			for k, j := range active {
				coef[j] = math.Max(sub[k], 0) // clamp floating-point noise at exactly 0
			}
			break
		}

		next := active[:0:0]
		for _, j := range active {
			if j != worst {
				next = append(next, j)
			}
		}
		active = next
	}

	return coef
}

// solveOLS solves the normal equations (X^T X) coef = X^T y restricted to
// the given columns, returning one coefficient per entry of cols (not per
// column of X).
func solveOLS(X [][]float64, y []float64, cols []int) []float64 {
	k := len(cols)
	ata := make([][]float64, k)
	for i := range ata {
		ata[i] = make([]float64, k)
	}
	atb := make([]float64, k)

	for r := range X {
		for i, ci := range cols {
			atb[i] += X[r][ci] * y[r]
			for j, cj := range cols {
				ata[i][j] += X[r][ci] * X[r][cj]
			}
		}
	}
	return gaussSolve(ata, atb)
}

// gaussSolve solves a·x = b by Gauss-Jordan elimination with partial
// pivoting. A column whose pivot is negligible even after pivoting is
// numerically unidentifiable given the rest of the system — its contribution
// is zeroed out of every row so it can't leak into other variables, and its
// solution entry is forced to exactly 0, rather than dividing by
// (near-)zero and returning NaN/Inf.
//
// The singularity threshold is relative to a's own scale, not a fixed
// absolute number: fitModelRates feeds this token counts pre-divided by 1e6,
// so a well-conditioned matrix here can legitimately have entries in the
// 1e-6 range, and a fixed absolute threshold anywhere near that would flag
// perfectly good systems as singular.
func gaussSolve(a [][]float64, b []float64) []float64 {
	n := len(b)
	m := make([][]float64, n)
	scale := 0.0
	for i := range a {
		row := make([]float64, n+1)
		copy(row, a[i])
		row[n] = b[i]
		m[i] = row
		for _, v := range a[i] {
			if av := math.Abs(v); av > scale {
				scale = av
			}
		}
	}
	threshold := scale * 1e-9
	if threshold == 0 {
		threshold = 1e-9 // a is entirely zero; nothing to scale against
	}

	singular := make([]bool, n)
	for col := 0; col < n; col++ {
		piv := col
		maxAbs := math.Abs(m[col][col])
		for r := col + 1; r < n; r++ {
			if v := math.Abs(m[r][col]); v > maxAbs {
				maxAbs = v
				piv = r
			}
		}
		if maxAbs < threshold {
			singular[col] = true
			for r := 0; r < n; r++ {
				m[r][col] = 0
			}
			continue
		}
		m[col], m[piv] = m[piv], m[col]
		pv := m[col][col]
		for c := col; c <= n; c++ {
			m[col][c] /= pv
		}
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			factor := m[r][col]
			if factor == 0 {
				continue
			}
			for c := col; c <= n; c++ {
				m[r][c] -= factor * m[col][c]
			}
		}
	}

	x := make([]float64, n)
	for i := 0; i < n; i++ {
		if !singular[i] {
			x[i] = m[i][n]
		}
	}
	return x
}

// fitColumnNames labels the four token types fitModelRates solves for, in
// the fixed order used throughout this file (fitResult.Columns, deriveMsg's
// fields, the printed table).
var fitColumnNames = [4]string{"input", "cache_read", "cache_write", "output"}

// fitColumn is one token type's outcome from fitModelRates.
type fitColumn struct {
	Coef float64 // fitted $/M rate; 0 for a column that doesn't Participate, that NNLS pinned to 0, or that was Pinned before the solve even ran
	// Participates is false when this token type never appears in the
	// model's messages at all (e.g. cache_write for a model that never
	// writes to cache). Its Coef/SharePct are then not a claim about
	// anything — there is simply no data to claim it from.
	Participates bool
	// Pinned is true when this column's near-zero-variance token values
	// were excluded from the free solve entirely — before nnlsSolve ever
	// ran, not because it returned 0 — because the data cannot determine
	// this column's rate at all (see fitModelRates' variance pre-filter)
	// and its token share is small enough that assuming 0 is safe. Distinct
	// from "doesn't Participate": this token type DID appear in the
	// messages, just without enough variation to fit a rate from. A pinned
	// column must always be reported as pinned, never as a fitted 0 — the
	// two look identical in dollar terms but mean very different things
	// ("measured as zero" vs "not measurable, assumed zero because it can't
	// matter much either way").
	Pinned bool
	// SharePct is this column's percentage of the total fitted cost —
	// coef*sum(tokens)/1e6, divided by the sum of that across all four
	// columns. Only meaningful when Participates is true.
	//
	// This is the identifiability signal, and it is a different question
	// from the residual: a column can carry a fitted, non-negative,
	// exactly-0%-residual coefficient that is still numerically arbitrary,
	// because it barely moves the total the fit is being scored against.
	// Anthropic's real $5/M input rate and an unrelated $0.69/M both fit
	// opencode's "Claude Opus 4.8" messages to 0.00% residual in practice —
	// the input column there explains ~0.01% of the money, so essentially
	// any value for it is equally consistent with the data. SharePct is
	// what catches that; ResidualPct cannot.
	SharePct float64
}

// fitResult is the complete outcome of fitting one (agent, model)'s rates.
type fitResult struct {
	Columns     [4]fitColumn
	ResidualPct float64
	// ConstantUnidentified names columns whose token values had near-zero
	// variance (see fitModelRates' pre-filter) but too much token share to
	// safely assume 0 for — pinning would absorb a non-negligible amount
	// of real cost into the surviving columns and distort them. These
	// force the whole segment to be rejected as unidentified regardless of
	// how the resulting fit's residual or dollar shares look, because the
	// column's rate is fundamentally undeterminable from this data, not
	// merely imprecise — see deriveModelRates.
	ConstantUnidentified []string
}

// Prices converts the fitted columns into compute.ModelPrices.
func (f fitResult) Prices() compute.ModelPrices {
	return compute.ModelPrices{
		Input:                 f.Columns[0].Coef,
		CacheRead:             f.Columns[1].Coef,
		CacheCreation:         f.Columns[2].Coef,
		Output:                f.Columns[3].Coef,
		SupportsCacheCreation: f.Columns[2].Coef > 0,
	}
}

// unidentifiedColumns returns the names of columns that carry a positive,
// fitted coefficient but whose share of the total fitted cost is below
// minColumnSharePct. A column pinned to 0 (whether because it never
// Participates, or because NNLS dropped it) makes no claim and is never
// "unidentified" — there's nothing uncertain about a rate the data forced
// to zero or never touched.
func (f fitResult) unidentifiedColumns(minColumnSharePct float64) []string {
	var names []string
	for j, c := range f.Columns {
		if c.Participates && c.Coef > 0 && c.SharePct < minColumnSharePct {
			names = append(names, fitColumnNames[j])
		}
	}
	return names
}

// plausibilityGate bounds what a fitted coefficient is allowed to look
// like as a real per-million-token price, independent of residual or cost
// share — the third gate this project has needed for the same underlying
// reason: a coefficient on a near-constant column (real opencode logs
// report input_tokens==3 for the overwhelming majority of some models'
// messages — a placeholder, not a measurement) can be non-negative
// (nnlsSolve), low-residual and comfortably above minColumnSharePct
// (fitColumn.SharePct), and still be a number no price table has ever
// published, because the column's *dollar* contribution stays tiny
// regardless of what arbitrary value the fit assigns its rate. A $600/M
// input rate times 3 tokens is 18/10,000ths of a cent either way — the
// residual and share gates literally cannot see the difference between
// that and the true rate, because in dollar terms there isn't one. Only a
// gate that looks at the coefficient's own value, not what it costs, can
// catch this.
type plausibilityGate struct {
	// MaxRatePerM is an absolute ceiling in $/M: no fitted, participating
	// column may exceed it.
	MaxRatePerM float64
	// MinCacheReadShareOfInput is a floor on cache_read/input when both are
	// fitted and nonzero. Real catalogs that price cache reads at all
	// charge at least ~1/154th of the input rate for it (the lowest ratio
	// anywhere in this store's synced models.dev catalog at the time this
	// was written); a fitted ratio far below even a generously loosened
	// version of that floor means the input side of the ratio is the
	// arbitrary one, not that this model has an unusually deep cache
	// discount.
	MinCacheReadShareOfInput float64
	// MaxInputToOutputRatio is a ceiling on input/output. Real catalogs
	// price input above output for well under 1% of every model in the
	// synced models.dev catalog, and even those rare exceptions never
	// exceed roughly 1.5x — output is priced higher than input for
	// essentially every real model, often by 3-10x. A fitted input several
	// times output has the two swapped, or one of them is arbitrary.
	MaxInputToOutputRatio float64
}

// defaultPlausibilityGate builds the gate deriveModelRates checks segments
// against. ceilingOverride, if positive, wins outright — see
// runPricesDerive's --max-plausible-rate flag. Otherwise the ceiling is
// computed from the real models.dev catalog already synced into the store
// (maxObservedCatalogRate), with headroom for prices moving up before the
// next `prices update`, rather than a guessed number that goes stale the
// moment a new, genuinely expensive model ships. Only when the store has
// no catalog to compute from at all (never synced) does this fall back to
// a hardcoded floor.
//
// The internal-consistency bounds (cache_read share, input/output ratio)
// aren't independently configurable — they're derived, in the doc comments
// above, from the shape of real pricing data itself, not from a judgment
// call the way the ceiling multiplier is, so there's no "correct value"
// for a flag to let someone pick instead.
func defaultPlausibilityGate(ctx context.Context, st *store.Store, ceilingOverride float64) plausibilityGate {
	ceiling := ceilingOverride
	if ceiling <= 0 {
		if observed, ok := maxObservedCatalogRate(ctx, st); ok {
			ceiling = observed * plausibilityCeilingSafetyFactor
		} else {
			ceiling = plausibilityCeilingFallback
		}
	}
	return plausibilityGate{
		MaxRatePerM:              ceiling,
		MinCacheReadShareOfInput: minCacheReadShareOfInput,
		MaxInputToOutputRatio:    maxInputToOutputRatio,
	}
}

// plausibilityCeilingSafetyFactor is how far above the highest rate
// actually observed in the synced models.dev catalog the computed ceiling
// sits — headroom for real prices moving up before the next `prices
// update`, without the ceiling being so loose it stops catching anything.
const plausibilityCeilingSafetyFactor = 2.0

// plausibilityCeilingFallback is the ceiling used only when the store has
// never synced a models.dev catalog to compute a real one from (see
// defaultPlausibilityGate). Twice the highest per-column rate models.dev
// listed anywhere in its catalog as of this writing ($150/M, OpenAI's
// o1-pro input) — a fallback, not the intended path: any store that has
// ever run `prices update` computes its own ceiling from real data instead,
// so this number's own staleness only matters for a store that has not.
const plausibilityCeilingFallback = 300.0

// minCacheReadShareOfInput and maxInputToOutputRatio back
// plausibilityGate's two internal-consistency checks. Both are set with
// deliberate headroom below/above the most extreme ratio actually observed
// across every provider and model in this store's synced models.dev
// catalog (checked directly against it while implementing this gate:
// cache_read/input ranges 1/154th to 3x when both are priced; input
// exceeds output in under 0.2% of models, and even then by at most ~1.5x)
// — loose enough that no real catalog entry trips them, tight enough that
// the four-figure ratios a degenerate placeholder column produces still do.
const (
	minCacheReadShareOfInput = 0.001 // 1/1000 — real catalogs' floor is ~1/154
	maxInputToOutputRatio    = 5.0   // real catalogs' ceiling is ~1.5x, and only in <0.2% of models
)

// maxObservedCatalogRate returns the highest per-column rate anywhere in
// the models.dev catalog synced into the store — across every provider,
// every model, every column — so defaultPlausibilityGate's ceiling is
// computed from real published prices rather than a guessed constant.
// Returns 0, false when the store has no models.dev rows at all (a fresh,
// never-synced store), so the caller knows to fall back instead of gating
// on a ceiling of 0.
func maxObservedCatalogRate(ctx context.Context, st *store.Store) (float64, bool) {
	var maxIn, maxOut, maxCR, maxCW sql.NullFloat64
	err := st.DB().QueryRowContext(ctx, `
		SELECT MAX(input), MAX(output), MAX(cache_read), MAX(cache_write)
		FROM model_price WHERE source = ?
	`, priceSourceModelsDev).Scan(&maxIn, &maxOut, &maxCR, &maxCW)
	if err != nil || !maxIn.Valid {
		return 0, false
	}
	max := maxIn.Float64
	for _, v := range []sql.NullFloat64{maxOut, maxCR, maxCW} {
		if v.Valid && v.Float64 > max {
			max = v.Float64
		}
	}
	return max, max > 0
}

// implausibleColumns returns a human-readable reason for every way f's
// fitted rates fail gate — an absolute ceiling per column, plus the two
// cross-column consistency checks described on plausibilityGate. Only a
// column that Participates with a positive Coef is checked against the
// ceiling (same convention as unidentifiedColumns: a column pinned to 0
// makes no claim, so there's nothing to judge implausible), and a
// cross-column check only fires when both sides of it are themselves
// positive and participating — there's nothing inconsistent about a model
// that simply doesn't price cache reads at all.
func (f fitResult) implausibleColumns(gate plausibilityGate) []string {
	var bad []string
	positive := func(c fitColumn) bool { return c.Participates && c.Coef > 0 }

	for j, c := range f.Columns {
		if positive(c) && c.Coef > gate.MaxRatePerM {
			bad = append(bad, fmt.Sprintf("%s=%.2f>%.2f/M", fitColumnNames[j], c.Coef, gate.MaxRatePerM))
		}
	}

	in, cr, out := f.Columns[0], f.Columns[1], f.Columns[3]
	if positive(in) && positive(cr) {
		if ratio := cr.Coef / in.Coef; ratio < gate.MinCacheReadShareOfInput {
			bad = append(bad, fmt.Sprintf("cache_read/input=%.6f<%.6f", ratio, gate.MinCacheReadShareOfInput))
		}
	}
	if positive(in) && positive(out) {
		if ratio := in.Coef / out.Coef; ratio > gate.MaxInputToOutputRatio {
			bad = append(bad, fmt.Sprintf("input/output=%.2f>%.2f", ratio, gate.MaxInputToOutputRatio))
		}
	}
	return bad
}

// columnCV returns the coefficient of variation (population stddev / mean)
// of one token column across msgs, and whether the column has any nonzero
// values at all. An all-zero column has no defined CV (returned as 0) —
// that case is a different, already-handled situation ("this token type
// never appears at all", nnlsSolve's own pre-filter and Participates
// below), not "appears, but never varies", which is what CV measures.
func columnCV(msgs []deriveMsg, get func(deriveMsg) int) (cv float64, nonzero bool) {
	if len(msgs) == 0 {
		return 0, false
	}
	var sum float64
	for _, m := range msgs {
		sum += float64(get(m))
	}
	mean := sum / float64(len(msgs))
	if mean == 0 {
		return 0, false
	}
	var variance float64
	for _, m := range msgs {
		d := float64(get(m)) - mean
		variance += d * d
	}
	variance /= float64(len(msgs))
	return math.Sqrt(variance) / mean, true
}

// fitColumnGetters extracts each of the four token columns from a
// deriveMsg, in fitColumnNames' order — shared by fitModelRates' variance
// pre-filter, which needs to read raw token counts before any $/M fitting
// happens.
var fitColumnGetters = [4]func(deriveMsg) int{
	func(m deriveMsg) int { return m.Input },
	func(m deriveMsg) int { return m.CacheRead },
	func(m deriveMsg) int { return m.CacheWrite },
	func(m deriveMsg) int { return m.Output },
}

// fitModelRates fits (input, cache_read, cache_write, output) $/M rates from
// a model's logged costs, non-negative, and reports both quality signals
// deriveModelRates gates on: the relative residual
// sum|predicted-actual|/sum(actual), and each column's share of the total
// fitted cost (see fitColumn.SharePct).
//
// Before any of that, a variance pre-filter looks for a column whose token
// values barely vary (coefficient of variation below minColumnCV) despite
// being nonzero — real opencode logs report input_tokens==3 for the
// overwhelming majority of some models' messages, a placeholder rather
// than a measurement. Such a column's rate is mathematically arbitrary:
// nothing in a near-constant column can pin down what multiplies it,
// so the free solve is free to assign it any non-negative value that
// trades off against the columns that actually vary — which is exactly how
// $602/M "input" rates got into this store despite passing both the
// residual and cost-share gates (see plausibilityGate's doc comment).
// What to do about it depends on how much token volume the column
// represents:
//
//   - Negligible token share (below minColumnSharePct, the same bar
//     unidentifiedColumns gates storage on): the column is excluded from
//     the free solve entirely and its coefficient is pinned to exactly 0
//     (fitColumn.Pinned), the same way an all-zero column already is. The
//     real cost those tokens carried doesn't vanish — it gets absorbed
//     into whichever surviving columns' coefficients the solve adjusts to
//     compensate — and the error that absorption can possibly introduce
//     is bounded by the pinned column's own tiny token share: a few tokens
//     out of a hundred thousand cannot move the total by much no matter
//     what their true rate was.
//   - Material token share: pinning would absorb a non-negligible amount
//     of real cost into the surviving columns and distort them, so this
//     column is recorded in ConstantUnidentified instead — deriveModelRates
//     rejects the whole segment over it, the same way it already rejects
//     over unidentifiedColumns, because a constant-but-large column is
//     still fundamentally unidentifiable, just not one that can be papered
//     over.
//
// A column with real variance (CV at or above minColumnCV) is untouched by
// any of this and fits exactly as it always has — this is deliberately the
// large majority of columns in practice (see this project's own real
// per-segment measurements: every column with genuine usage has CV well
// above 0.6, while a placeholder column measures exactly 0).
//
// cache_write_1h is deliberately not a fifth term: only Claude populates it,
// and Claude is excluded from derivation entirely (see runPricesDerive), so
// every message here has cache_write_1h == 0 and a 4-term fit is complete.
func fitModelRates(msgs []deriveMsg, minColumnCV, minColumnSharePct float64) fitResult {
	X := make([][]float64, len(msgs))
	y := make([]float64, len(msgs))
	for i, m := range msgs {
		X[i] = []float64{
			float64(m.Input) / compute.TokensPerMillion,
			float64(m.CacheRead) / compute.TokensPerMillion,
			float64(m.CacheWrite) / compute.TokensPerMillion,
			float64(m.Output) / compute.TokensPerMillion,
		}
		y[i] = m.Cost
	}

	// origParticipates reflects the raw data ("did this token type ever
	// appear at all"), computed before the pre-filter below may zero out a
	// pinned column's X entries — Participates must still report the truth
	// about the data, not about what the solver was allowed to see.
	var origParticipates [4]bool
	for j := 0; j < 4; j++ {
		for _, row := range X {
			if row[j] != 0 {
				origParticipates[j] = true
				break
			}
		}
	}

	var tokenTotals [4]float64
	for _, m := range msgs {
		for j, get := range fitColumnGetters {
			tokenTotals[j] += float64(get(m))
		}
	}
	totalTokens := tokenTotals[0] + tokenTotals[1] + tokenTotals[2] + tokenTotals[3]

	var pinned [4]bool
	var constantUnidentified []string
	for j, get := range fitColumnGetters {
		if !origParticipates[j] {
			continue // all-zero; nnlsSolve's own pre-filter already handles this
		}
		cv, _ := columnCV(msgs, get)
		if cv >= minColumnCV {
			continue // real variance; nothing to pre-filter
		}
		tokenSharePct := 0.0
		if totalTokens > 0 {
			tokenSharePct = tokenTotals[j] / totalTokens * 100
		}
		if tokenSharePct < minColumnSharePct {
			pinned[j] = true
			for i := range X {
				X[i][j] = 0
			}
		} else {
			constantUnidentified = append(constantUnidentified, fitColumnNames[j])
		}
	}

	coef := nnlsSolve(X, y)

	var res fitResult
	res.ConstantUnidentified = constantUnidentified
	for j := 0; j < 4; j++ {
		res.Columns[j].Coef = coef[j]
		res.Columns[j].Participates = origParticipates[j]
		res.Columns[j].Pinned = pinned[j]
	}

	var errSum, costSum float64
	var contribution [4]float64
	for i, row := range X {
		var predicted float64
		for j := 0; j < 4; j++ {
			c := row[j] * coef[j]
			predicted += c
			contribution[j] += c
		}
		errSum += math.Abs(predicted - y[i])
		costSum += y[i]
	}
	if costSum > 0 {
		res.ResidualPct = errSum / costSum * 100
	}

	totalFitted := contribution[0] + contribution[1] + contribution[2] + contribution[3]
	if totalFitted > 0 {
		for j := 0; j < 4; j++ {
			res.Columns[j].SharePct = contribution[j] / totalFitted * 100
		}
	}

	return res
}

// deriveSegment is one contiguous, gated time slice of a (agent, model)'s
// messages — the span detectSegments judged to share one rate, already run
// through fitModelRates.
//
// From/To follow claude.go's claudePriceWindow convention: From is
// inclusive, To is exclusive, and a zero value means unbounded on that
// side. detectSegments' first segment always has From==0 (open at the
// start — there is no earlier segment to abut) and its last segment always
// has To==0 (open at the end — the rate is still in effect as far as this
// derive run knows).
type deriveSegment struct {
	From, To int64
	Fit      fitResult
	Msgs     int
}

// segmentInitialChunkFactor sets detectSegments' coarse starting block size
// as a multiple of minMessages. It can't be 1 (i.e. one block per
// minMessages messages): a single opencode message's own blended $/Mtok
// swings by an order of magnitude purely from its input/cache/output mix
// (a cache-read-heavy message is cheap per token, an input-heavy one is
// expensive, even under one unchanged rate table), so comparing
// message-sized or even minMessages-sized windows compares mix noise to
// mix noise and finds "changes" that were never there. A block several
// times larger lets that mix noise average out before two blocks are ever
// compared — see detectSegments.
const segmentInitialChunkFactor = 5

// detectSegments splits msgs — already sorted by CreatedAt ascending, see
// gatherDeriveMessages — into contiguous segments that each admit one
// stable rate, gating nothing itself (deriveModelRates applies the
// residual/identifiability gates to what this returns, exactly as it did
// for the single whole-window fit before segmentation existed).
//
// Method: cut msgs into coarse fixed-size blocks (segmentInitialChunkFactor
// * minMessages messages each, the trailing remainder folded into the last
// block so every block starts out at least minMessages long), fit every
// block independently, then repeatedly merge each adjacent pair whose fits
// agree (see fitsAgree) — refitting the merged span — until no two
// neighbors merge. A model whose rate never changed converges to exactly
// one segment, because every block's fit agrees with its neighbor's; a
// real rate change survives as a boundary because no amount of merging
// makes the two sides of it agree. A single odd message can't create a
// segment on its own because it never gets its own block — it's outvoted
// by the dozens of ordinary messages sharing its block, the same reason
// minMessages-sized blocks would be too noisy on their own.
//
// minColumnCV and minColumnSharePct are threaded straight into every
// fitModelRates call (see its doc comment for what they gate there) —
// segmentation and the final per-segment fit share the exact same
// pre-filter, so a block's fitted rates already reflect any pinned or
// forced-unidentified columns before fitsAgree ever compares two blocks.
func detectSegments(msgs []deriveMsg, minMessages int, tolerancePct, minColumnCV, minColumnSharePct float64) []deriveSegment {
	n := len(msgs)
	if n == 0 {
		return nil
	}
	chunkSize := minMessages * segmentInitialChunkFactor
	if chunkSize < minMessages {
		chunkSize = minMessages // guard against a pathological/overflowed minMessages
	}
	fit := func(lo, hi int) fitResult { return fitModelRates(msgs[lo:hi], minColumnCV, minColumnSharePct) }

	type block struct {
		lo, hi int
		fit    fitResult
	}
	var blocks []block
	for lo := 0; lo < n; lo += chunkSize {
		hi := lo + chunkSize
		if hi > n {
			hi = n
		}
		if len(blocks) > 0 && n-lo < chunkSize {
			// A short trailing remainder folds into the previous block
			// rather than ever standing alone under minMessages long.
			last := &blocks[len(blocks)-1]
			last.hi = n
			last.fit = fit(last.lo, last.hi)
			break
		}
		blocks = append(blocks, block{lo: lo, hi: hi, fit: fit(lo, hi)})
	}

	for merged := true; merged && len(blocks) > 1; {
		merged = false
		for i := 0; i < len(blocks)-1; i++ {
			if fitsAgree(blocks[i].fit, blocks[i+1].fit, tolerancePct, minColumnSharePct) {
				blocks[i].hi = blocks[i+1].hi
				blocks[i].fit = fit(blocks[i].lo, blocks[i].hi)
				blocks = append(blocks[:i+1], blocks[i+2:]...)
				merged = true
				break
			}
		}
	}

	// Defensive floor: every block above already starts out at least
	// minMessages long (chunkSize is a multiple of it, and folding the
	// remainder never leaves a short one), and merging only grows a block,
	// so this should never fire — it's here so a future change to the
	// chunking above can't silently ship a segment too small to trust
	// without also breaking a test for it (see prices_derive_test.go).
	for i := 0; i < len(blocks); {
		if blocks[i].hi-blocks[i].lo >= minMessages || len(blocks) == 1 {
			i++
			continue
		}
		if i > 0 {
			blocks[i-1].hi = blocks[i].hi
			blocks[i-1].fit = fit(blocks[i-1].lo, blocks[i-1].hi)
		} else {
			blocks[1].lo = blocks[0].lo
			blocks[1].fit = fit(blocks[1].lo, blocks[1].hi)
			i = 1
		}
		blocks = append(blocks[:i], blocks[i+1:]...)
	}

	segs := make([]deriveSegment, len(blocks))
	for i, b := range blocks {
		segs[i] = deriveSegment{Fit: b.fit, Msgs: b.hi - b.lo}
		if i > 0 {
			segs[i].From = msgs[b.lo].CreatedAt
		}
		if i < len(blocks)-1 {
			segs[i].To = msgs[blocks[i+1].lo].CreatedAt
		}
	}
	return segs
}

// fitsAgree reports whether two segments' fitted rates are the same price
// table within tolerancePct — the test detectSegments' merge loop uses to
// decide two adjacent blocks share one real rate rather than straddling a
// genuine change.
//
// A column is only compared when BOTH sides consider it identified:
// Participates, a positive coefficient, and a cost share at or above
// minColumnSharePct — the same bar fitResult.unidentifiedColumns gates
// storage on. Requiring only one side to identify it (or comparing an
// unidentified column's raw coefficient at all) would compare arbitrary
// noise to arbitrary noise: a column that barely moves the money — real
// opencode data has this for a near-constant "input" placeholder some
// providers log, and it happens to sit right at minColumnSharePct's edge —
// can flip between just-above and just-below the identifiability line from
// one window to the next with no underlying rate change at all, and with
// the stricter either-side rule that flip alone would permanently wall two
// windows apart. Skipping the column whenever either side is unsure about
// it means a segment boundary only survives merging because a column both
// sides DO trust disagrees — which is the only kind of disagreement that
// should ever count as a real rate change.
func fitsAgree(a, b fitResult, tolerancePct, minColumnSharePct float64) bool {
	identified := func(c fitColumn) bool {
		return c.Participates && c.Coef > 0 && c.SharePct >= minColumnSharePct
	}
	for j := 0; j < 4; j++ {
		if !identified(a.Columns[j]) || !identified(b.Columns[j]) {
			continue
		}
		ac, bc := a.Columns[j].Coef, b.Columns[j].Coef
		if denom := math.Max(ac, bc); math.Abs(ac-bc)/denom*100 > tolerancePct {
			return false
		}
	}
	return true
}

// gatherDeriveMessages returns every assistant message with a logged cost
// for the pi and opencode agents (never claude — its message.cost is
// tokeneks' own recomputation, so fitting against it would just be fitting
// tokeneks against itself), grouped by (agent, model).
func gatherDeriveMessages(ctx context.Context, days int) (map[string][]deriveMsg, error) {
	st := getTokeneksStore()
	if st == nil {
		return nil, fmt.Errorf("store not open")
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	// Ordered by created_at (id as a stable tiebreak for same-millisecond
	// rows) because detectSegments walks each group in time order to find
	// where a model's rate changed — an unordered result would make "the
	// series" meaningless.
	rows, err := st.DB().QueryContext(ctx, `
		SELECT agent, model, input_tokens, cache_read, cache_write, output_tokens, cost, created_at
		FROM message
		WHERE role = 'assistant' AND agent IN ('pi', 'opencode')
		  AND model IS NOT NULL AND model != '' AND cost > 0 AND created_at >= ?
		ORDER BY agent, model, created_at ASC, id ASC
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]deriveMsg)
	for rows.Next() {
		var agent, model string
		var m deriveMsg
		if err := rows.Scan(&agent, &model, &m.Input, &m.CacheRead, &m.CacheWrite, &m.Output, &m.Cost, &m.CreatedAt); err != nil {
			return nil, err
		}
		key := agent + "\x00" + model
		out[key] = append(out[key], m)
	}
	return out, rows.Err()
}

// derivedProvider is the store provider id `prices derive` writes rows
// under. It is prefixed rather than the bare agent name ("pi", "opencode")
// because models.dev itself ships a provider literally named "opencode"
// (its own hosted-model router) with 60+ rows already in model_price —
// sharing that exact string would put a derived row one model-name
// normalization away from silently overwriting, or being overwritten by,
// models.dev's unrelated opencode-provider catalog in the (provider, model)
// primary key. The "derived:" prefix can't collide with a models.dev
// provider id by construction, since those are bare slugs with no colon.
func derivedProvider(agent string) string {
	return "derived:" + agent
}

// oldSchemeDerivedProviders are the provider values `prices derive` used to
// write before derivedProvider added the "derived:" prefix. Any row still
// there is stale under the old scheme and shares a PK namespace with
// models.dev's real "opencode" provider, so it's purged unconditionally —
// see deriveModelRates.
var oldSchemeDerivedProviders = []string{"pi", "opencode"}

// deriveResult is one (agent, model, segment)'s outcome, kept for the
// summary print regardless of whether the fit was accepted. A model whose
// rate never changed produces exactly one deriveResult, same as before
// segmentation existed; a model detectSegments split produces one per
// segment, each gated independently.
type deriveResult struct {
	Agent, Model string
	// From/To is this result's segment window (0/0 for a single, fully
	// open segment — see deriveSegment). Meaningless for a "rejected:
	// sample size" result, which never reached segmentation.
	From, To int64
	Msgs     int
	Fit      fitResult
	Status   string // "stored", "dry-run", "rejected: error", "rejected: sample size", "rejected: unidentified(...)", "rejected: implausible(...)"
}

// deriveModelRates is the whole computation behind `prices derive`: group
// pi/opencode's logged-cost messages by model, split each group into
// time-segments that each admit one stable rate (detectSegments), fit and
// gate every segment independently on sample size, fit quality, and
// per-column identifiability, and persist accepted fits — kept separate
// from runPricesDerive's store-sync-then-print wrapper so it can be tested
// directly against a store that hasn't gone through ensureStoreReady's
// real filesystem sync (same split as totalsByAgent/printTotal in main.go).
//
// Every run (other than --dry-run) fully reconciles the store for every
// (agent, model) considered here: its entire existing window set is
// deleted and replaced with whatever segments qualify on this run, rather
// than patching individual windows in place. A re-run's segment boundaries
// rarely land on the exact same timestamps as the previous run's (more
// messages, a different --days, a different --segment-tolerance), so
// leaving old windows in place and only overwriting the ones that happen
// to match again would accumulate stale, non-contiguous windows instead of
// reflecting this run's view of the data. The old bare-agent-name provider
// scheme is purged unconditionally on every non-dry-run call for the same
// "don't leave stale rows looking current" reason — see
// oldSchemeDerivedProviders.
//
// maxPlausibleRate is the override for defaultPlausibilityGate's ceiling
// (<=0 means "compute it from the store's own models.dev catalog" — see
// that function); the gate itself is built once per call, not per segment,
// since it depends on a store query whose answer can't change mid-run.
//
// minColumnCV is fitModelRates' variance pre-filter threshold — see its doc
// comment for what it does and why minColumnSharePct is reused as the
// pin-vs-reject boundary there too.
func deriveModelRates(ctx context.Context, st *store.Store, days, minMessages int, maxErrorPct, minColumnSharePct, minColumnCV, segmentTolerancePct, maxPlausibleRate float64, dryRun bool) ([]deriveResult, error) {
	grouped, err := gatherDeriveMessages(ctx, days)
	if err != nil {
		return nil, err
	}

	gate := defaultPlausibilityGate(ctx, st, maxPlausibleRate)

	keys := make([]string, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var results []deriveResult
	var toStore []store.ModelPrice
	var toDelete [][2]string
	now := time.Now().UnixMilli()

	for _, key := range keys {
		var agent, model string
		for i := 0; i < len(key); i++ {
			if key[i] == 0 {
				agent, model = key[:i], key[i+1:]
				break
			}
		}
		msgs := grouped[key]

		if len(msgs) < minMessages {
			results = append(results, deriveResult{Agent: agent, Model: model, Msgs: len(msgs), Status: "rejected: sample size"})
			continue
		}

		// Every (agent, model) that reaches segmentation gets its whole
		// window set deleted and replaced below, regardless of how its
		// segments individually gate — see the doc comment above.
		toDelete = append(toDelete, [2]string{derivedProvider(agent), model})

		for _, seg := range detectSegments(msgs, minMessages, segmentTolerancePct, minColumnCV, minColumnSharePct) {
			res := deriveResult{Agent: agent, Model: model, From: seg.From, To: seg.To, Msgs: seg.Msgs, Fit: seg.Fit}

			switch {
			case seg.Fit.ResidualPct > maxErrorPct:
				res.Status = "rejected: error"
			case len(seg.Fit.ConstantUnidentified) > 0:
				// A constant-but-material-share column (see fitModelRates):
				// pinning it would have absorbed a real chunk of the bill
				// into the surviving columns, so it was never pinned, and
				// this segment is rejected the same way a low-dollar-share
				// column already is — both are "the data cannot determine
				// this rate", just detected a different way.
				res.Status = "rejected: unidentified(" + strings.Join(seg.Fit.ConstantUnidentified, ",") + ")"
			default:
				if bad := seg.Fit.unidentifiedColumns(minColumnSharePct); len(bad) > 0 {
					res.Status = "rejected: unidentified(" + strings.Join(bad, ",") + ")"
				} else if bad := seg.Fit.implausibleColumns(gate); len(bad) > 0 {
					res.Status = "rejected: implausible(" + strings.Join(bad, ",") + ")"
				}
			}

			if res.Status == "" {
				if dryRun {
					res.Status = "dry-run"
				} else {
					res.Status = "stored"
					prices := seg.Fit.Prices()
					toStore = append(toStore, store.ModelPrice{
						Provider: derivedProvider(agent), Model: model, Name: model,
						Input: prices.Input, Output: prices.Output,
						CacheRead: prices.CacheRead, CacheWrite: prices.CacheCreation, CacheWrite1h: 0,
						Source: priceSourceDerived, UpdatedAt: now,
						EffectiveFrom: seg.From, EffectiveTo: seg.To,
					})
				}
			}
			results = append(results, res)
		}
	}

	if !dryRun {
		if _, err := st.DeleteModelPricesByProviders(ctx, priceSourceDerived, oldSchemeDerivedProviders); err != nil {
			return nil, fmt.Errorf("purge old-scheme derived prices: %w", err)
		}

		if err := st.DeleteModelPrices(ctx, priceSourceDerived, toDelete); err != nil {
			return nil, fmt.Errorf("delete stale derived prices: %w", err)
		}

		if len(toStore) > 0 {
			if err := st.UpsertModelPrices(ctx, toStore); err != nil {
				return nil, fmt.Errorf("store derived prices: %w", err)
			}
		}

		// resolveAgentPrices' derived:pi/derived:opencode layer is cached
		// per process (see prices_resolve.go); drop the memo so a later
		// lookup in this same process — from `prices check`, `total`, ...,
		// or the next test in this file — sees what was just written
		// instead of whatever was memoized before this call.
		resetResolvedPrices()
	}

	// Stable, not just Slice: two results for the same (agent, model) must
	// keep the chronological order detectSegments produced them in (From
	// ascending) rather than being shuffled by an unstable sort landing on
	// equal Agent/Model keys.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Agent != results[j].Agent {
			return results[i].Agent < results[j].Agent
		}
		return results[i].Model < results[j].Model
	})

	return results, nil
}

// fmtSegBound formats one end of a deriveResult's segment window: "open" for
// the sentinel 0 (unbounded — see deriveSegment), otherwise the date the
// window starts/ends on. Dates, not full timestamps: this is a report for a
// human comparing segments against the day-level shifts they already know
// about (see prices_derive.go's package doc / the task that motivated this),
// not a debugger.
func fmtSegBound(ms int64) string {
	if ms == 0 {
		return "open"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// runPricesDerive is the `prices derive` CLI entry point: bring the store up
// to date, run deriveModelRates, and print the report.
func runPricesDerive(days, minMessages int, maxErrorPct, minColumnSharePct, minColumnCV, segmentTolerancePct, maxPlausibleRate float64, dryRun bool) error {
	if err := ensureStoreReady(); err != nil {
		return err
	}
	st := getTokeneksStore()

	results, err := deriveModelRates(context.Background(), st, days, minMessages, maxErrorPct, minColumnSharePct, minColumnCV, segmentTolerancePct, maxPlausibleRate, dryRun)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Println("no pi/opencode messages with a logged cost in this window")
		return nil
	}

	fmt.Printf("%-9s %-24s %10s %10s %5s  %7s %7s %7s %7s  %7s  %6s %6s %6s %6s  %s\n",
		"agent", "model", "from", "to", "msgs", "in$", "cr$", "cw$", "out$", "err%", "in%", "cr%", "cw%", "out%", "status")

	// A pinned column must never look like a fitted 0 — "pinned" reads
	// completely differently from "0.000" (measured as zero vs not
	// measurable, assumed zero because it can't matter much either way),
	// and only one of those is something the data actually showed.
	fmtRate := func(c fitColumn) string {
		if c.Pinned {
			return "pinned"
		}
		if !c.Participates {
			return "—"
		}
		return fmt.Sprintf("%.3f", c.Coef)
	}
	fmtShare := func(c fitColumn) string {
		if c.Pinned {
			return "pinned"
		}
		if !c.Participates {
			return "—"
		}
		return fmt.Sprintf("%.1f%%", c.SharePct)
	}

	var stored, rejectedError, rejectedUnidentified, rejectedImplausible, rejectedSize int
	for _, r := range results {
		if r.Status == "rejected: sample size" {
			rejectedSize++
			fmt.Printf("%-9s %-24s %10s %10s %5d  %7s %7s %7s %7s  %7s  %6s %6s %6s %6s  %s\n",
				r.Agent, truncate(r.Model, 24), "—", "—", r.Msgs, "—", "—", "—", "—", "—", "—", "—", "—", "—", r.Status)
			continue
		}

		switch {
		case r.Status == "stored" || r.Status == "dry-run":
			stored++
		case r.Status == "rejected: error":
			rejectedError++
		case strings.HasPrefix(r.Status, "rejected: unidentified"):
			rejectedUnidentified++
		case strings.HasPrefix(r.Status, "rejected: implausible"):
			rejectedImplausible++
		}

		cols := r.Fit.Columns
		fmt.Printf("%-9s %-24s %10s %10s %5d  %7s %7s %7s %7s  %6.2f%%  %6s %6s %6s %6s  %s\n",
			r.Agent, truncate(r.Model, 24), fmtSegBound(r.From), fmtSegBound(r.To), r.Msgs,
			fmtRate(cols[0]), fmtRate(cols[1]), fmtRate(cols[2]), fmtRate(cols[3]),
			r.Fit.ResidualPct,
			fmtShare(cols[0]), fmtShare(cols[1]), fmtShare(cols[2]), fmtShare(cols[3]),
			r.Status)
	}

	fmt.Println()
	verb := "stored"
	if dryRun {
		verb = "would store (dry run)"
	}
	fmt.Printf("%d segment(s) fitted and %s, %d rejected for error > %.1f%%, %d rejected as unidentified (a column's cost share < %.1f%%), %d rejected as implausible (see plausibilityGate), %d rejected for fewer than %d messages\n",
		stored, verb, rejectedError, maxErrorPct, rejectedUnidentified, minColumnSharePct, rejectedImplausible, rejectedSize, minMessages)

	return nil
}
