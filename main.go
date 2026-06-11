package main

import (
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

	rootCmd.AddCommand(ocCmd, piCmd, claudeCmd, totalCmd, webCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func printTotal(days int) error {
	kimi := ocModelPrices["Kimi K2.6"]

	// OC total
	ocSessions, err := ocSessions(days, "")
	if err != nil {
		return fmt.Errorf("OC: %w", err)
	}
	var ocActual, ocIdeal float64
	ocIDs := make([]string, 0, len(ocSessions))
	for _, sess := range ocSessions {
		ocIDs = append(ocIDs, sess.ID)
	}
	ocStepsBySession, err := ocStepsBatch(ocIDs)
	if err != nil {
		return fmt.Errorf("OC: %w", err)
	}
	for _, sess := range ocSessions {
		s := ocSessionSummary(ocStepsBySession[sess.ID], sess.Model)
		ocActual += s.Actual
		ocIdeal += s.Ideal
	}

	// PI total
	piSess, err := piSessions(days, "")
	if err != nil {
		return fmt.Errorf("PI: %w", err)
	}
	var piActual, piIdeal float64
	for _, sess := range piSess {
		steps, err := piMessages(sess.Filepath)
		if err != nil || len(steps) == 0 {
			continue
		}
		rows := compute.ComputeIdeal(steps)
		s := compute.Summarize(rows, kimi)
		piActual += s.Actual
		piIdeal += s.Ideal
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
