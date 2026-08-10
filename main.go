package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"tokeneks/compute"

	"github.com/spf13/cobra"
)

var days int
var dateFilter string

func registerAgentCommands(root *cobra.Command, agent Agent, listShort, detailUse, detailShort string) {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: listShort,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.List(days, dateFilter)
		},
	}
	listCmd.Flags().StringVarP(&dateFilter, "date", "D", "", "filter by specific date (YYYY-MM-DD)")

	detailCmd := &cobra.Command{
		Use:   detailUse,
		Short: detailShort,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.Detail(args[0], days)
		},
	}

	root.AddCommand(listCmd, detailCmd)
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "tokeneks",
		Short: "TokenEKS - Token Efficiency Kontrol Suite",
	}

	rootCmd.PersistentFlags().IntVarP(&days, "days", "d", 7, "number of days to analyze")

	ocCmd := &cobra.Command{
		Use:   "oc",
		Short: "OpenCode sessions",
	}
	registerAgentCommands(ocCmd, agents["oc"], "List all Kimi K2.6 sessions with summary", "detail <session-id>", "Per-step analysis for a specific session")

	piCmd := &cobra.Command{
		Use:   "pi",
		Short: "PI Agent sessions",
	}
	registerAgentCommands(piCmd, agents["pi"], "List all Kimi K2.6 sessions with summary", "detail <session-id|filepath>", "Per-message analysis for a specific PI session")

	claudeCmd := &cobra.Command{
		Use:   "claude",
		Short: "Claude Code sessions (Opus 5, Fable 5, Sonnet 5)",
	}
	registerAgentCommands(claudeCmd, agents["claude"], "List Claude Code sessions with cache analysis", "detail <session-id|filepath>", "Per-message analysis for a Claude Code session")

	// total
	totalCmd := &cobra.Command{
		Use:   "total",
		Short: "Combined summary (OC + PI + Claude)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return printTotal(days)
		},
	}

	// web
	var webPort string
	webCmd := &cobra.Command{
		Use:   "web",
		Short: "Start web dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeb(webPort, days)
		},
	}
	webCmd.Flags().StringVarP(&webPort, "port", "p", "8080", "HTTP port")

	// sync — read all agent sources and write to the local store.
	// Only sessions whose source changed since the last run are re-parsed;
	// --force re-parses everything.
	var syncForce bool
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Ingest new/changed sessions from all agents into the local store",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(false, syncForce)
		},
	}
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "re-parse every session, even unchanged ones")

	var syncWatchForce bool
	syncWatchCmd := &cobra.Command{
		Use:   "watch",
		Short: "Ingest once, then watch agent sources for changes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(true, syncWatchForce)
		},
	}
	syncWatchCmd.Flags().BoolVar(&syncWatchForce, "force", false, "re-parse every session on the initial pass")

	// prices — sync the model price catalog from models.dev into the store.
	// Model ids there match the ones the agents log, so the rows join to
	// message.model directly.
	pricesCmd := &cobra.Command{
		Use:   "prices",
		Short: "Model pricing stored in the local database",
	}

	var pricesUpdateProvider string
	pricesUpdateCmd := &cobra.Command{
		Use:   "update",
		Short: "Fetch current model prices from models.dev into the store",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPricesUpdate(pricesUpdateProvider)
		},
	}
	pricesUpdateCmd.Flags().StringVar(&pricesUpdateProvider, "provider", "",
		"only this models.dev provider id (e.g. anthropic); default: all providers")

	var pricesListProvider string
	pricesListCmd := &cobra.Command{
		Use:   "list",
		Short: "Show prices currently stored",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPricesList(pricesListProvider)
		},
	}
	pricesListCmd.Flags().StringVar(&pricesListProvider, "provider", "",
		"only this provider id (e.g. anthropic); default: all providers")

	// prices check — the measuring stick: compare what each agent logged
	// against what tokeneks' own rate table would compute for the same
	// tokens, per (agent, model). Reports coverage; never fails.
	pricesCheckCmd := &cobra.Command{
		Use:   "check",
		Short: "Compare logged costs against tokeneks' rate table, per model",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPricesCheck(days)
		},
	}

	// prices derive — recover real rates from pi's and opencode's own logged
	// costs by least-squares, for models `prices check` reports as MISSING
	// (retired from the live catalogs those commands read from).
	var deriveMinMessages int
	var deriveMaxError float64
	var deriveMinColumnShare float64
	var deriveMinColumnCV float64
	var deriveSegmentTolerance float64
	var deriveMaxPlausibleRate float64
	var deriveDryRun bool
	pricesDeriveCmd := &cobra.Command{
		Use:   "derive",
		Short: "Fit per-model rates from pi/opencode logged costs and store them",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPricesDerive(days, deriveMinMessages, deriveMaxError, deriveMinColumnShare, deriveMinColumnCV, deriveSegmentTolerance, deriveMaxPlausibleRate, deriveDryRun)
		},
	}
	pricesDeriveCmd.Flags().IntVar(&deriveMinMessages, "min-messages", 20,
		"skip models with fewer than this many logged-cost messages; also the floor on how small a detected segment may be")
	// 35%, not the 1% a noise-free synthetic fit (or PI's real, clean-billed
	// data) reaches: real opencode/reseller billing carries genuine
	// per-message jitter — verified directly against this project's own
	// store, cache-heavy messages bill anywhere from ~13% under to ~30% over
	// what a linear fit predicts, on messages from the same model, same
	// day, no rate change in sight — that no 4-column linear model removes
	// even at a several-hundred-message window. At 1%, every noisy
	// opencode model (this task's whole motivation) stays MISSING or
	// catalog-priced forever, which is strictly worse: a segment fitting to
	// 10-30% residual is still far closer to the real bill than a
	// models.dev catalog rate this project measured wrong by up to 295%,
	// and unlike that catalog rate, the residual is printed by `prices
	// check` on every row, so a bad fit stays visible instead of silently
	// passing as "priced". 35% is chosen, not just "loose enough": it's
	// comfortably above every segment judged legitimate against the real
	// store while implementing this flag (worst case ~31%), while still
	// rejecting the two clearly pathological segments found there (58% and
	// 135% — a coarse block that happened to straddle an exact
	// change-point, correctly caught rather than trusted) that a much
	// looser number would have let through.
	pricesDeriveCmd.Flags().Float64Var(&deriveMaxError, "max-error", 35.0,
		"reject fits whose relative residual exceeds this percent")
	pricesDeriveCmd.Flags().Float64Var(&deriveMinColumnShare, "min-column-share", 1.0,
		"reject a column whose share of the total fitted cost is below this percent — a low share means the fitted rate is numerically arbitrary, not just imprecise; also reused as the token-share boundary between pinning a near-constant column to 0 and rejecting the segment over it (see --min-column-cv)")
	// 0.1: real opencode token columns that carry genuine signal measure a
	// coefficient of variation (stddev/mean) of 0.6 or higher in every
	// 100+-message window checked while implementing this flag (MiniMax
	// M3's lowest was 0.68); a column real usage never touches, or that a
	// provider logs as a fixed placeholder (input_tokens==3 for the
	// overwhelming majority of some opencode models' messages), measures
	// exactly 0 or a hair above it. 0.1 sits with wide margin below every
	// real column and wide margin above the placeholder case, so it
	// separates the two without being anywhere near either edge.
	pricesDeriveCmd.Flags().Float64Var(&deriveMinColumnCV, "min-column-cv", 0.1,
		"treat a column as unmeasurable rather than fit it freely when its token values vary less than this coefficient of variation (stddev/mean) despite being nonzero — see fitModelRates")
	pricesDeriveCmd.Flags().Float64Var(&deriveSegmentTolerance, "segment-tolerance", 15.0,
		"treat two adjacent time-segments as the same rate (and merge them) when every identified column's fitted value is within this percent of the other's — see detectSegments")
	pricesDeriveCmd.Flags().Float64Var(&deriveMaxPlausibleRate, "max-plausible-rate", 0,
		"ceiling in $/M a fitted column may not exceed; 0 = auto, computed as 2x the highest rate in the store's synced models.dev catalog (falls back to $300/M if that catalog has never been synced) — see defaultPlausibilityGate")
	pricesDeriveCmd.Flags().BoolVar(&deriveDryRun, "dry-run", false,
		"fit and report but don't write to the store")

	pricesCmd.AddCommand(pricesUpdateCmd, pricesListCmd, pricesCheckCmd, pricesDeriveCmd)

	rootCmd.AddCommand(ocCmd, piCmd, claudeCmd, totalCmd, webCmd, syncCmd, syncWatchCmd, pricesCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// agentTotal is one row of the `total` table: one agent's paid vs ideal
// spend, summed across every model it used.
type agentTotal struct {
	Label  string
	Actual float64
	Ideal  float64
}

// unpricedModel is a model that had no usable price in its agent's table.
// Steps priced this way would just be wrong, so totalsByAgent pulls them out
// of Actual/Ideal entirely and reports them here instead.
// The agent is part of the identity: the same model name can be priced
// under one agent's table and missing from another's (PI logs "Kimi K2.6",
// which only ocModelPrices knows about), so reporting the name alone reads
// as a contradiction.
type unpricedModel struct {
	Agent  string
	Model  string
	Tokens int
	Steps  int
	// LoggedCost is what the agent's own log says these steps cost. It is
	// only known for agents that log a cost (PI, OpenCode) and is reported
	// so the warning can say how much money is missing, not just how many
	// tokens — but it is not folded into the totals, because without rates
	// there is no matching Ideal to compare it against.
	LoggedCost float64
}

// piPricesFunc resolves the PI price table. It's a package-level var, not a
// direct call to piGlobalModelPrices, so tests can swap in a table that
// doesn't depend on the machine-local ~/.pi/agent/models.json (which
// piGlobalModelPrices reads once via sync.Once and can't be pointed
// elsewhere per-test).
var piPricesFunc = piGlobalModelPrices

// totalsByAgent computes the OC/PI/Claude rows for the `total` command. It
// is the whole computation, kept separate from printTotal's formatting so
// it can be tested directly.
//
// The ideal-cache algorithm (ComputeIdeal/ComputeIdealClaude) models one
// continuous conversation: each step's ideal cache read is derived from how
// much of the *previous step's* context could have been reused, and that
// running pointer carries forward step to step. So it always runs once per
// session over the session's full, untouched step order — splitting the
// session by model first (which an earlier version of this function did)
// throws that carried-forward state away at every model switch, which
// makes "ideal" assume a cold start right there and can inflate it past the
// real Actual cost. That produces exactly the impossible Paid<Ideal output
// this function exists to fix, just relocated rather than fixed.
//
// What model pricing actually needs — sessions routinely switch models
// mid-run, so pricing a whole session at its first message's rate (the old
// behavior) silently mispriced any session that switched — is applied
// afterward, per row: each IdealRow already lines up 1:1 with the step that
// produced it, so its $ cost uses that step's own model rate instead of
// pooling raw token counts across models into one Summarize call (which
// only accepts a single price for the whole set).
//
// Paid prefers what the agent logged over what tokeneks can recompute. PI
// and OpenCode record the provider's own billed figure per message, which
// is authoritative: it already accounts for rate changes, discounts and
// tiers that a static rate table cannot know about, and it stays correct
// for models the table has since lost. Rates are the fallback for steps
// with no logged cost. Ideal is always computed from rates — it is a
// counterfactual, so there is nothing logged to read it from.
//
// Neither OC nor PI nor Claude falls back to some other model's rate for a
// model missing from its table — OC used to (Kimi K2.6, unconditionally),
// which is what let eleven different unrelated models get priced at a
// twelfth model's rate and made the Ideal column fiction. Inventing a price
// for an unknown model would be indistinguishable from a real number in the
// output; unpriced steps are excluded from Actual/Ideal for every agent and
// reported back via the unpriced return value instead.
func totalsByAgent(ctx context.Context, days int) ([]agentTotal, []unpricedModel, error) {
	// claudePrices is computed once (not inside priceFunc) because
	// claudeGlobalModelPrices rebuilds a map from the memoized overlays on
	// every call — fine done once per report, wasteful done once per step.
	claudePrices := claudeGlobalModelPrices()

	type spec struct {
		store string // agent column value in the store
		label string // row label in the printed table
		// priceFunc resolves model's price as of the step's own timestamp
		// (ms epoch) — a derived opencode/pi rate can now have more than
		// one effective window (see prices_derive.go's segmentation), so
		// resolving "the current rate" for every step regardless of when
		// it ran would price a step from before a rate change at the rate
		// that came after it. Claude ignores at for now (see prices_check.go
		// and claude.go's own dated-window mechanism for the parallel path
		// that already exists there); it's part of the signature only so
		// all three specs share one shape.
		priceFunc   func(model string, at int64) (compute.ModelPrices, bool)
		claudeStyle bool // true => ComputeIdealClaude (PI, Claude); false => ComputeIdeal (OC)
		// costIsLogged marks agents whose message.cost came from their own
		// log rather than from tokeneks. Claude Code stopped writing costUSD
		// (1 of 141 recent session files still has it), so its stored cost is
		// this program's own ingest-time recomputation — preferring it would
		// freeze whatever rates were in effect at ingest and make
		// `prices update` silently a no-op until the next `sync --force`.
		costIsLogged bool
	}
	specs := []spec{
		{store: "opencode", label: "OC", priceFunc: func(m string, at int64) (compute.ModelPrices, bool) { return resolveAgentPricesAt("opencode", m, at) }, claudeStyle: false, costIsLogged: true},
		{store: "pi", label: "PI", priceFunc: func(m string, at int64) (compute.ModelPrices, bool) { return resolveAgentPricesAt("pi", m, at) }, claudeStyle: true, costIsLogged: true},
		{store: "claude", label: "CLAUDE", priceFunc: func(m string, _ int64) (compute.ModelPrices, bool) {
			p, ok := claudePrices[m]
			return p, ok && p.Input > 0
		}, claudeStyle: true, costIsLogged: false},
	}

	var rows []agentTotal
	unpricedByModel := map[string]*unpricedModel{}

	for _, sp := range specs {
		sessions, err := aggregateSessionsFromStore(ctx, sp.store, days, "")
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", sp.label, err)
		}

		var actual, ideal float64
		for _, sess := range sessions {
			if len(sess.Steps) == 0 {
				continue
			}

			tokens := make([]compute.StepData, len(sess.Steps))
			for i, s := range sess.Steps {
				tokens[i] = s.Data
			}

			// ComputeIdealClaude's prices argument is unused by the
			// algorithm itself (see compute.go) — it only shapes the
			// IdealCC/IdealIn split, which depends on CacheCreation, not on
			// any rate — so a zero-value table here is fine; real pricing
			// happens per row below.
			var idealRows []compute.IdealRow
			if sp.claudeStyle {
				idealRows = compute.ComputeIdealClaude(tokens, compute.ModelPrices{})
			} else {
				idealRows = compute.ComputeIdeal(tokens)
			}

			for i, row := range idealRows {
				step := sess.Steps[i]

				prices, priced := sp.priceFunc(step.Model, step.CreatedAt)
				if !priced {
					key := sp.label + "\x00" + step.Model
					u := unpricedByModel[key]
					if u == nil {
						u = &unpricedModel{Agent: sp.label, Model: step.Model}
						unpricedByModel[key] = u
					}
					u.Steps++
					u.Tokens += row.Input + row.CacheCreation + row.CacheRead + row.Output
					if sp.costIsLogged {
						u.LoggedCost += step.LoggedCost
					}
					continue
				}

				// IdealCC is a synthetic re-derivation with no real 5m/1h
				// split, so — same convention as compute.Summarize — it's
				// priced entirely at the 5m rate (CacheCreation1h left at 0).
				idealStep := compute.StepData{Input: row.IdealIn, CacheCreation: row.IdealCC, CacheRead: row.IdealCR, Output: row.Output}
				if sp.costIsLogged && step.LoggedCost > 0 {
					actual += step.LoggedCost
				} else {
					actual += compute.PiStepActualCost(step.Data, prices)
				}
				ideal += compute.PiStepActualCost(idealStep, prices)
			}
		}

		rows = append(rows, agentTotal{Label: sp.label, Actual: actual, Ideal: ideal})
	}

	unpriced := make([]unpricedModel, 0, len(unpricedByModel))
	for _, u := range unpricedByModel {
		unpriced = append(unpriced, *u)
	}
	sort.Slice(unpriced, func(i, j int) bool {
		if unpriced[i].Agent != unpriced[j].Agent {
			return unpriced[i].Agent < unpriced[j].Agent
		}
		return unpriced[i].Model < unpriced[j].Model
	})

	return rows, unpriced, nil
}

func printTotal(days int) error {
	if err := ensureStoreReady(); err != nil {
		return err
	}

	rows, unpriced, err := totalsByAgent(context.Background(), days)
	if err != nil {
		return err
	}

	var totalActual, totalIdeal float64
	for _, r := range rows {
		totalActual += r.Actual
		totalIdeal += r.Ideal
	}
	totalOverpay := max(totalActual-totalIdeal, 0)
	totalPct := 0.0
	if totalIdeal > 0 {
		totalPct = totalOverpay / totalIdeal * 100
	}

	fmt.Println("          Paid      Ideal    Overpay   %ideal")
	fmt.Println("───────────────────────────────────────────────")
	for _, r := range rows {
		overpay := max(r.Actual-r.Ideal, 0)
		pct := 0.0
		if r.Ideal > 0 {
			pct = overpay / r.Ideal * 100
		}
		fmt.Printf("%-6s  %8.2f  %8.2f  %8.2f  %5.1f%%\n", r.Label, r.Actual, r.Ideal, overpay, pct)
	}
	fmt.Println("───────────────────────────────────────────────")
	fmt.Printf("%-6s  %8.2f  %8.2f  %8.2f  %5.1f%%\n", "TOTAL", totalActual, totalIdeal, totalOverpay, totalPct)

	printUnpricedModels(unpriced)

	return nil
}

// printUnpricedModels prints the standard "excluded from totals" warning
// block for a list of unpriced models, or nothing when there are none.
// Shared by `total`, `oc list`, and `pi list` so a genuine pricing gap
// reads identically everywhere it surfaces, rather than each command
// inventing its own wording for the same situation.
func printUnpricedModels(unpriced []unpricedModel) {
	if len(unpriced) == 0 {
		return
	}
	fmt.Println()
	fmt.Printf("warning: %d model(s) have no price and are excluded from the totals above:\n", len(unpriced))
	for _, u := range unpriced {
		// The logged cost is only shown when the agent reported one — a
		// blank column means "unknown", not "free".
		logged := "        "
		if u.LoggedCost > 0 {
			logged = fmt.Sprintf("$%7.2f", u.LoggedCost)
		}
		fmt.Printf("  %-6s  %-24s  %8s tok  %4d steps  %s logged\n",
			u.Agent, u.Model, formatTokens(u.Tokens), u.Steps, logged)
	}
}
