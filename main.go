package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var days int
var dateFilter string

func main() {
	rootCmd := &cobra.Command{
		Use:   "tokeneks",
		Short: "TokenEKS - Token Efficiency Kontrol Suite",
	}

	rootCmd.PersistentFlags().IntVarP(&days, "days", "d", 7, "number of days to analyze")

	// oc list
	ocCmd := &cobra.Command{
		Use:   "oc",
		Short: "OpenCode sessions",
	}

	ocListCmd := &cobra.Command{
		Use:   "list",
		Short: "List all Kimi K2.6 sessions with summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			return ocList(days, dateFilter)
		},
	}
	ocListCmd.Flags().StringVarP(&dateFilter, "date", "D", "", "filter by specific date (YYYY-MM-DD)")

	// oc detail <session_id>
	ocDetailCmd := &cobra.Command{
		Use:   "detail <session-id>",
		Short: "Per-step analysis for a specific session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ocDetail(args[0])
		},
	}

	ocCmd.AddCommand(ocListCmd, ocDetailCmd)

	// pi list
	piCmd := &cobra.Command{
		Use:   "pi",
		Short: "PI Agent sessions",
	}

	piListCmd := &cobra.Command{
		Use:   "list",
		Short: "List all Kimi K2.6 sessions with summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			return piList(days, dateFilter)
		},
	}
	piListCmd.Flags().StringVarP(&dateFilter, "date", "D", "", "filter by specific date (YYYY-MM-DD)")

	// pi detail <session-id|filepath>
	piDetailCmd := &cobra.Command{
		Use:   "detail <session-id|filepath>",
		Short: "Per-message analysis for a specific PI session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return piDetail(args[0])
		},
	}

	piCmd.AddCommand(piListCmd, piDetailCmd)

	// claude list
	claudeCmd := &cobra.Command{
		Use:   "claude",
		Short: "Claude Code sessions (Opus 4.7, Sonnet 4.6)",
	}
	claudeListCmd := &cobra.Command{
		Use:   "list",
		Short: "List Claude Code sessions with cache analysis",
		RunE: func(cmd *cobra.Command, args []string) error {
			return claudeList(days, dateFilter)
		},
	}
	claudeListCmd.Flags().StringVarP(&dateFilter, "date", "D", "", "filter by specific date (YYYY-MM-DD)")

	// claude detail <session-id|filepath>
	claudeDetailCmd := &cobra.Command{
		Use:   "detail <session-id|filepath>",
		Short: "Per-message analysis for a Claude Code session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return claudeDetail(args[0])
		},
	}

	claudeCmd.AddCommand(claudeListCmd, claudeDetailCmd)

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
	for _, sess := range ocSessions {
		steps, err := ocSteps(sess.ID)
		if err != nil {
			continue
		}
		rows := ComputeIdeal(steps)
		prices := ocModelPrices[sess.Model]
		if prices.Input == 0 {
			prices = kimi
		}
		s := Summarize(rows, prices)
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
		rows := ComputeIdeal(steps)
		s := Summarize(rows, kimi)
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
