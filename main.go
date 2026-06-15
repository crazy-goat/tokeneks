package main

import (
	"context"
	"fmt"
	"os"
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
		Short: "Claude Code sessions (Opus 4.7, Sonnet 4.6)",
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

	// sync — read all agent sources and write to the local store
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Ingest sessions from all agents into the local store",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(false)
		},
	}
	syncWatchCmd := &cobra.Command{
		Use:   "watch",
		Short: "Ingest once, then watch agent sources for changes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(true)
		},
	}

	rootCmd.AddCommand(ocCmd, piCmd, claudeCmd, totalCmd, webCmd, syncCmd, syncWatchCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func printTotal(days int) error {
	if err := ensureStoreReady(""); err != nil {
		return err
	}
	kimi := ocModelPrices["Kimi K2.6"]

	summarize := func(agent string) (actual, ideal float64, err error) {
		sessions, err := aggregateSessionsFromStore(context.Background(), agent, days, "")
		if err != nil {
			return 0, 0, err
		}
		for _, sess := range sessions {
			prices := kimi
			if p, ok := ocModelPrices[sess.Model]; ok && p.Input > 0 {
				prices = p
			}
			rows := compute.ComputeIdeal(sess.Steps)
			s := compute.Summarize(rows, prices)
			actual += s.Actual
			ideal += s.Ideal
		}
		return actual, ideal, nil
	}

	ocActual, ocIdeal, err := summarize("opencode")
	if err != nil {
		return fmt.Errorf("OC: %w", err)
	}
	piActual, piIdeal, err := summarize("pi")
	if err != nil {
		return fmt.Errorf("PI: %w", err)
	}

	totalActual := ocActual + piActual
	totalIdeal := ocIdeal + piIdeal
	totalOverpay := totalActual - totalIdeal
	if totalOverpay < 0 {
		totalOverpay = 0
	}

	ocOverpay := max(ocActual-ocIdeal, 0)
	ocPct := 0.0
	if ocIdeal > 0 {
		ocPct = ocOverpay / ocIdeal * 100
	}
	piOverpay := max(piActual-piIdeal, 0)
	piPct := 0.0
	if piIdeal > 0 {
		piPct = piOverpay / piIdeal * 100
	}
	totalPct := 0.0
	if totalIdeal > 0 {
		totalPct = totalOverpay / totalIdeal * 100
	}

	fmt.Println("         Paid     Ideal    Overpay   %ideal")
	fmt.Println("─────────────────────────────────────────────")
	fmt.Printf("OC     %7.2f  %7.2f  %7.2f  %5.1f%%\n", ocActual, ocIdeal, ocOverpay, ocPct)
	fmt.Printf("PI     %7.2f  %7.2f  %7.2f  %5.1f%%\n", piActual, piIdeal, piOverpay, piPct)
	fmt.Println("─────────────────────────────────────────────")
	fmt.Printf("TOTAL  %7.2f  %7.2f  %7.2f  %5.1f%%\n", totalActual, totalIdeal, totalOverpay, totalPct)
	fmt.Println()
	fmt.Printf("Input=$%.2f/M  CacheRead=$%.2f/M  Output=$%.2f/M\n", PriceInput, PriceCacheRead, PriceOutput)

	return nil
}
