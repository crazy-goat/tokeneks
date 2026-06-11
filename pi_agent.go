package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultPISessions = "~/.pi/agent/sessions"

type piUsage struct {
	Input      int    `json:"input"`
	CacheRead  int    `json:"cacheRead"`
	CacheWrite int    `json:"cacheWrite"`
	Output     int    `json:"output"`
	Total      int    `json:"totalTokens"`
	Cost       piCost `json:"cost"`
}

type piCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

type piMessageEntry struct {
	Type      string `json:"type"`
	ModelID   string `json:"modelId"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role     string  `json:"role"`
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		Usage    piUsage `json:"usage"`
		Content  []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type piSessionStep struct {
	Model string
	Step  StepData
	Cost  float64 // actual cost from session data
}

func (s piSessionStep) modelKey() string   { return s.Model }
func (s piSessionStep) stepData() StepData { return s.Step }

type piSessionData struct {
	DominantModel  string
	Title          string
	LastUserPrompt string
	LastActivity   time.Time
	Steps          []piSessionStep
	ModelProviders map[string]string
	ToolCalls      int
}

func countPIToolCalls(entry piMessageEntry) int {
	if entry.Message.Role != "assistant" {
		return 0
	}
	var n int
	for _, c := range entry.Message.Content {
		if c.Type == "toolCall" {
			n++
		}
	}
	return n
}

func piSessionUsage(fp string) (piSessionData, error) {
	f, err := os.Open(fp)
	if err != nil {
		return piSessionData{}, err
	}
	defer f.Close()

	var data piSessionData
	data.ModelProviders = make(map[string]string)
	modelCounts := make(map[string]int)
	scanner := newJSONLScanner(f)

	for scanner.Scan() {
		var entry piMessageEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if ts, err := parseTimestamp(entry.Timestamp); err == nil && ts.After(data.LastActivity) {
			data.LastActivity = ts
		}

		if entry.Type != "message" {
			continue
		}

		if entry.Message.Role == "user" {
			for _, c := range entry.Message.Content {
				if c.Type == "text" {
					text := strings.TrimSpace(c.Text)
					if text != "" {
						data.LastUserPrompt = text
						if data.Title == "" {
							data.Title = truncate(text, 80)
						}
						break
					}
				}
			}
			continue
		}

		data.ToolCalls += countPIToolCalls(entry)

		if entry.Message.Model == "" {
			continue
		}
		if entry.Message.Usage.Total == 0 {
			continue
		}

		step := StepData{
			Input:         entry.Message.Usage.Input,
			CacheCreation: entry.Message.Usage.CacheWrite,
			CacheRead:     entry.Message.Usage.CacheRead,
			Output:        entry.Message.Usage.Output,
		}
		data.Steps = append(data.Steps, piSessionStep{Model: entry.Message.Model, Step: step, Cost: entry.Message.Usage.Cost.Total})
		if entry.Message.Provider != "" {
			data.ModelProviders[entry.Message.Model] = entry.Message.Provider
		}
		modelCounts[entry.Message.Model]++
	}

	data.DominantModel = dominantModel(modelCounts)

	return data, scanner.Err()
}

func piMessages(fp string) ([]StepData, error) {
	data, err := piSessionUsage(fp)
	if err != nil {
		return nil, err
	}
	steps := make([]StepData, 0, len(data.Steps))
	for _, step := range data.Steps {
		steps = append(steps, step.Step)
	}
	return steps, nil
}

type piSession struct {
	ID            string
	Filepath      string
	Project       string
	Title         string
	Date          string
	Msgs          int
	ToolCalls     int
	Birth         time.Time
	LastActivity  time.Time
	DominantModel string
	ParentID      string
	ChildCount    int
	IsSubsession  bool
}

func piSessions(days int, date string) ([]piSession, error) {
	baseDir := expandHome(defaultPISessions)
	cutoff := time.Now().AddDate(0, 0, -days)

	var sessions []piSession

	if err := walkSessionFiles(baseDir, func(fp string, info os.FileInfo) error {
		if date != "" {
			fdate, ok := fileDateFromFilename(filepath.Base(fp))
			if !ok || fdate != date {
				return nil
			}
		} else if info.ModTime().Before(cutoff) {
			return nil
		}

		data, err := piSessionUsage(fp)
		if err != nil || len(data.Steps) == 0 {
			return nil
		}

		dirEntry := filepath.Base(filepath.Dir(fp))
		project := cleanProjectName(dirEntry)
		title := data.Title
		if title == "" {
			title = project
		}
		sessionName := filepath.Base(fp)
		sessionBase := strings.TrimSuffix(sessionName, ".jsonl")
		sessionID, _ := piSessionIDFromFilename(sessionName)
		childCount := piSubsessionCount(filepath.Join(filepath.Dir(fp), sessionBase), cutoff, date)

		sessions = append(sessions, piSession{
			ID:            sessionID,
			Filepath:      fp,
			Project:       project,
			Title:         title,
			Date:          func() string { d, _ := fileDateFromFilename(sessionName); return d }(),
			Msgs:          len(data.Steps),
			ToolCalls:     data.ToolCalls,
			Birth:         getCreatedAtFromInfo(info),
			LastActivity:  data.LastActivity,
			DominantModel: data.DominantModel,
			ChildCount:    childCount,
		})
		return nil
	}); err != nil {
		return nil, err
	}

	// Sort by file birth time ascending — oldest first, newest last
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Birth.Before(sessions[j].Birth)
	})

	return sessions, nil
}

func piSubsessionPaths(sessionFilepath string) []string {
	sessionDir := strings.TrimSuffix(sessionFilepath, ".jsonl")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runDirs, err := os.ReadDir(filepath.Join(sessionDir, entry.Name()))
		if err != nil {
			continue
		}
		for _, runDir := range runDirs {
			if !runDir.IsDir() || !strings.HasPrefix(runDir.Name(), "run-") {
				continue
			}
			fp := filepath.Join(sessionDir, entry.Name(), runDir.Name(), "session.jsonl")
			if info, err := os.Stat(fp); err == nil && !info.IsDir() {
				paths = append(paths, fp)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func piSubsessionCount(sessionDir string, cutoff time.Time, date string) int {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runDirs, err := os.ReadDir(filepath.Join(sessionDir, entry.Name()))
		if err != nil {
			continue
		}
		for _, runDir := range runDirs {
			if !runDir.IsDir() || !strings.HasPrefix(runDir.Name(), "run-") {
				continue
			}
			fp := filepath.Join(sessionDir, entry.Name(), runDir.Name(), "session.jsonl")
			info, err := os.Stat(fp)
			if err != nil || info.IsDir() {
				continue
			}
			if date != "" {
				if info.ModTime().UTC().Format("2006-01-02") != date {
					continue
				}
			} else if info.ModTime().Before(cutoff) {
				continue
			}
			count++
		}
	}
	return count
}

func piParentSessionInfo(fp string) (parentPath, parentID string, ok bool) {
	runDir := filepath.Dir(fp)
	if !strings.HasPrefix(filepath.Base(runDir), "run-") {
		return "", "", false
	}
	runIDDir := filepath.Dir(runDir)
	parentDir := filepath.Dir(runIDDir)
	parentBase := filepath.Base(parentDir)
	parentPath = parentDir + ".jsonl"
	if _, err := os.Stat(parentPath); err != nil {
		return "", "", false
	}
	sessionID, ok := sessionIDFromBase(parentBase)
	if !ok {
		return "", "", false
	}
	return parentPath, sessionID, true
}

func piSessionIDFromPath(fp string) string {
	base := filepath.Base(fp)
	if base == "session.jsonl" {
		runDir := filepath.Dir(fp)
		runIDDir := filepath.Dir(runDir)
		return filepath.Base(runIDDir)
	}
	if strings.HasSuffix(base, ".jsonl") {
		if sessionID, err := piSessionIDFromFilename(base); err == nil {
			return sessionID
		}
	}
	return strings.TrimSuffix(base, ".jsonl")
}

func cleanProjectName(dirName string) string {
	name := strings.Trim(dirName, "-")
	// Derive the home-prefix dynamically instead of hardcoding a username
	if home, err := os.UserHomeDir(); err == nil {
		user := filepath.Base(home)
		prefix := "Users-" + user + "-"
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
		}
	}
	if name == "" {
		return "(root)"
	}
	return strings.ReplaceAll(name, "-", "/")
}

func resolvePISessionPath(input string, days int) (string, string, error) {
	if strings.HasSuffix(input, ".jsonl") || strings.Contains(input, "/") {
		return input, "", nil
	}

	sessions, err := piSessions(days, "")
	if err != nil {
		return "", "", err
	}

	var matches []piSession
	for _, sess := range sessions {
		if sess.ID == input {
			matches = append(matches, sess)
		}
	}
	if len(matches) == 1 {
		return matches[0].Filepath, matches[0].ID, nil
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("multiple PI sessions found for ID %s", input)
	}

	var childMatches []string
	for _, sess := range sessions {
		for _, childPath := range piSubsessionPaths(sess.Filepath) {
			if piSessionIDFromPath(childPath) == input {
				childMatches = append(childMatches, childPath)
			}
		}
	}
	if len(childMatches) == 0 {
		return "", "", fmt.Errorf("PI session not found: %s", input)
	}
	if len(childMatches) > 1 {
		return "", "", fmt.Errorf("multiple PI sessions found for ID %s", input)
	}
	return childMatches[0], input, nil
}

func piDetail(input string, days int) error {
	fp, sessionID, err := resolvePISessionPath(input, days)
	if err != nil {
		return err
	}

	data, err := piSessionUsage(fp)
	if err != nil {
		return err
	}
	if len(data.Steps) == 0 {
		return fmt.Errorf("no billable messages in %s", fp)
	}

	dirName := filepath.Base(filepath.Dir(fp))
	project := cleanProjectName(dirName)
	if sessionID == "" {
		sessionID, _ = piSessionIDFromFilename(filepath.Base(fp))
		if sessionID == "" {
			sessionID = strings.TrimSuffix(filepath.Base(fp), ".jsonl")
		}
	}

	fmt.Printf("Session:  %s\n", sessionID)
	fmt.Printf("File:     %s\n", filepath.Base(fp))
	fmt.Printf("Project:  %s\n", project)
	fmt.Printf("Messages: %d\n", len(data.Steps))
	fmt.Printf("Model:    %s\n\n", data.DominantModel)

	globalPrices := piGlobalModelPrices()
	byModel := groupStepsByModel(data.Steps)

	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)

	var totalActual, totalIdeal float64
	for i, model := range models {
		prices := globalPrices[model]
		steps := byModel[model]

		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("=== %s (%d messages) ===\n\n", model, len(steps))

		if prices.CacheCreation == 0 {
			rows := ComputeIdeal(steps)
			printDetailRows(rows, prices)
			s := Summarize(rows, prices)
			totalActual += s.Actual
			totalIdeal += s.Ideal
			fmt.Printf("\nSubtotal actual: $%.2f\n", s.Actual)
			fmt.Printf("Subtotal ideal:  $%.2f\n", s.Ideal)
			fmt.Printf("Subtotal overpay: $%.2f (%.1f%% of ideal)\n", s.Overpay, s.PctIdeal)
		} else {
			rows := ComputeIdealClaude(steps, prices)
			printDetailRowsClaude(rows, prices)
			s := SummarizeClaude(rows, prices)
			totalActual += s.Actual
			totalIdeal += s.Ideal
			fmt.Printf("\nSubtotal actual: $%.2f\n", s.Actual)
			fmt.Printf("Subtotal ideal:  $%.2f\n", s.Ideal)
			fmt.Printf("Subtotal overpay: $%.2f (%.1f%% of ideal)\n", s.Overpay, s.PctIdeal)
		}
	}

	totalOverpay, pctIdeal, _, _ := footerTotals(totalActual, totalIdeal, 0)

	fmt.Printf("\nTOTAL\n")
	fmt.Printf("Actual paid:  $%.2f\n", totalActual)
	fmt.Printf("Ideal paid:   $%.2f\n", totalIdeal)
	fmt.Printf("Overpay:      $%.2f (%.1f%% of ideal)\n", totalOverpay, pctIdeal)

	return nil
}

func piList(days int, date string) error {
	sessions, err := piSessions(days, date)
	if err != nil {
		return err
	}

	fmt.Printf("%19s  %-36s  %-18s  %4s  %7s  %6s  %6s  %8s  %7s  %7s  %7s\n",
		"DateTime", "SessionID", "DominantModel", "Msgs", "Tokens", "Paid", "Ideal", "Overpay", "%ideal", "$/1M", "i$/1M")
	fmt.Println(strings.Repeat("-", separatorWidthPi))

	var totalActual, totalIdeal float64
	var totalIn, totalCR, totalOut int

	globalPrices := piGlobalModelPrices()
	for _, sess := range sessions {
		data, err := piSessionUsage(sess.Filepath)
		if err != nil || len(data.Steps) == 0 {
			continue
		}

		byModel := groupStepsByModel(data.Steps)

		var s Summary
		for model, steps := range byModel {
			prices := globalPrices[model]
			if prices.CacheCreation == 0 {
				rows := ComputeIdeal(steps)
				part := Summarize(rows, prices)
				s.TotalCR += part.TotalCR
				s.TotalIn += part.TotalIn
				s.TotalOut += part.TotalOut
				s.TotalIdealCR += part.TotalIdealCR
				s.TotalIdealIn += part.TotalIdealIn
				s.TotalWaste += part.TotalWaste
				s.Actual += part.Actual
				s.Ideal += part.Ideal
			} else {
				rows := ComputeIdealClaude(steps, prices)
				part := SummarizeClaude(rows, prices)
				s.TotalCR += part.TotalCR
				s.TotalIn += part.TotalIn
				s.TotalOut += part.TotalOut
				s.TotalIdealCR += part.TotalIdealCR
				s.TotalIdealIn += part.TotalIdealIn
				s.TotalWaste += part.TotalWaste
				s.Actual += part.Actual
				s.Ideal += part.Ideal
			}
		}
		s.Overpay = s.Actual - s.Ideal
		if s.Overpay < 0 {
			s.Overpay = 0
		}
		if s.Ideal > 0 {
			s.PctIdeal = s.Overpay / s.Ideal * 100
		}

		totalActual += s.Actual
		totalIdeal += s.Ideal
		totalIn += s.TotalIn
		totalCR += s.TotalCR
		totalOut += s.TotalOut

		timestamp := sess.Birth.UTC().Format("2006-01-02 15:04:05")

		tokens := s.TotalIn + s.TotalCR + s.TotalOut
		costPer1M := perMillion(s.Actual, tokens)
		idealPer1M := perMillion(s.Ideal, tokens)

		fmt.Printf("%19s  %-36s  %-18.18s  %4d  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
			timestamp, sess.ID, sess.DominantModel, sess.Msgs, formatTokens(tokens), s.Actual, s.Ideal, s.Overpay, s.PctIdeal, costPer1M, idealPer1M)
	}

	fmt.Println(strings.Repeat("-", separatorWidthPi))
	totalTokens := totalIn + totalCR + totalOut
	totalOverpay, pct, totalCostPer1M, totalIdealPer1M := footerTotals(totalActual, totalIdeal, totalTokens)

	fmt.Printf("%19s  %-36s  %-18s  %4s  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
		"TOTAL", "", "", "", formatTokens(totalTokens), totalActual, totalIdeal, totalOverpay, pct, totalCostPer1M, totalIdealPer1M)

	return nil
}
