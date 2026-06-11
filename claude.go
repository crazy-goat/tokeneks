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

const defaultClaudeSessions = "~/.claude/projects"
const defaultClaudePricing = "~/.tokeneks/claude_models.json"

var claudePrices = initClaudePrices()

type claudePricesFile map[string]struct {
	Input         float64 `json:"input"`
	CacheCreation float64 `json:"cacheCreation"`
	CacheRead     float64 `json:"cacheRead"`
	Output        float64 `json:"output"`
}

func initClaudePrices() map[string]ModelPrices {
	prices := map[string]ModelPrices{
		"claude-opus-4-7": {
			Input:         5.5,
			CacheCreation: 6.75,
			CacheRead:     0.55,
			Output:        27.5,
		},
		"claude-opus-4-8": {
			Input:         5.5,
			CacheCreation: 6.75,
			CacheRead:     0.55,
			Output:        27.5,
		},
		"claude-sonnet-4-6": {
			Input:         3.0,
			CacheCreation: 3.75,
			CacheRead:     0.3,
			Output:        15.0,
		},
	}

	b, err := os.ReadFile(expandHome(defaultClaudePricing))
	if err != nil {
		return prices
	}

	var file claudePricesFile
	if err := json.Unmarshal(b, &file); err != nil {
		return prices
	}

	for model, p := range file {
		prices[model] = ModelPrices{
			Input:         p.Input,
			CacheCreation: p.CacheCreation,
			CacheRead:     p.CacheRead,
			Output:        p.Output,
		}
	}
	return prices
}

type claudeMessage struct {
	Type    string `json:"type"`
	Message struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"message"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

type claudeSessionStep struct {
	Model string
	Step  StepData
}

type claudeMessageResult struct {
	Steps          []claudeSessionStep
	Models         []string
	ToolCalls      int
	LastUserPrompt string
	LastActivity   time.Time
}

func claudeMessages(fp string) (claudeMessageResult, error) {
	f, err := os.Open(fp)
	if err != nil {
		return claudeMessageResult{}, err
	}
	defer f.Close()

	var steps []claudeSessionStep
	var models []string
	var toolCalls int
	var lastUserPrompt string
	var lastActivity time.Time
	scanner := newJSONLScanner(f)

	for scanner.Scan() {
		var msg claudeMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if ts, err := parseTimestamp(msg.Timestamp); err == nil && ts.After(lastActivity) {
			lastActivity = ts
		}
		if msg.Type == "user" {
			continue
		}
		if msg.Type != "assistant" || msg.Message.Model == "" {
			continue
		}
		if msg.Message.Usage.InputTokens+msg.Message.Usage.CacheCreationInputTokens+
			msg.Message.Usage.CacheReadInputTokens+msg.Message.Usage.OutputTokens == 0 {
			continue
		}

		for _, c := range msg.Message.Content {
			if c.Type == "tool_use" {
				toolCalls++
			}
		}

		models = append(models, msg.Message.Model)
		steps = append(steps, claudeSessionStep{
			Model: msg.Message.Model,
			Step: StepData{
				Input:         msg.Message.Usage.InputTokens,
				CacheCreation: msg.Message.Usage.CacheCreationInputTokens,
				CacheRead:     msg.Message.Usage.CacheReadInputTokens,
				Output:        msg.Message.Usage.OutputTokens,
			},
		})
	}
	return claudeMessageResult{Steps: steps, Models: models, ToolCalls: toolCalls, LastUserPrompt: lastUserPrompt, LastActivity: lastActivity}, scanner.Err()
}

type claudeSession struct {
	ID            string
	Filepath      string
	Project       string
	Date          string
	DominantModel string
	Msgs          int
	ToolCalls     int
	Birth         time.Time
	LastActivity  time.Time
	SubagentCount int
}

func claudeSessions(days int, date, modelFilter string) ([]claudeSession, error) {
	baseDir := expandHome(defaultClaudeSessions)
	cutoff := time.Now().AddDate(0, 0, -days)

	var sessions []claudeSession

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, err
	}

	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}
		projectDir := filepath.Join(baseDir, dirEntry.Name())

		files, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}

		for _, fileEntry := range files {
			if filepath.Ext(fileEntry.Name()) != ".jsonl" {
				continue
			}
			if len(fileEntry.Name()) < 37 { // UUID is 36 chars + .jsonl
				continue
			}

			fp := filepath.Join(projectDir, fileEntry.Name())
			info, err := os.Stat(fp)
			if err != nil {
				continue
			}

			if date != "" {
				if info.ModTime().UTC().Format("2006-01-02") != date {
					continue
				}
			} else {
				if info.ModTime().Before(cutoff) {
					continue
				}
			}

			res, err := claudeMessages(fp)
			if err != nil || len(res.Models) == 0 {
				continue
			}

			modelCount := make(map[string]int)
			for _, m := range res.Models {
				modelCount[m]++
			}
			var dominantModel string
			var maxCount int
			for m, c := range modelCount {
				if c > maxCount || (c == maxCount && (dominantModel == "" || m < dominantModel)) {
					maxCount = c
					dominantModel = m
				}
			}

			sessionID := strings.TrimSuffix(fileEntry.Name(), ".jsonl")
			project := cleanClaudeProjectName(dirEntry.Name())
			fileDate := info.ModTime().UTC().Format("2006-01-02")
			subagentCount := 0
			if subEntries, err := os.ReadDir(filepath.Join(projectDir, sessionID, "subagents")); err == nil {
				for _, subEntry := range subEntries {
					if !subEntry.IsDir() && filepath.Ext(subEntry.Name()) == ".jsonl" {
						subagentCount++
					}
				}
			}

			sessions = append(sessions, claudeSession{
				ID:            sessionID,
				Filepath:      fp,
				Project:       project,
				Date:          fileDate,
				DominantModel: dominantModel,
				Msgs:          len(res.Models),
				ToolCalls:     res.ToolCalls,
				Birth:         getCreatedAtFromInfo(info),
				LastActivity:  res.LastActivity,
				SubagentCount: subagentCount,
			})
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Birth.Before(sessions[j].Birth)
	})

	return sessions, nil
}

func cleanClaudeProjectName(dirName string) string {
	// Convert -Users-username-work-project-name to work/project-name
	name := dirName
	if home, err := os.UserHomeDir(); err == nil {
		user := filepath.Base(home)
		dashedUser := strings.ReplaceAll(user, ".", "-")
		prefix1 := "-Users-" + dashedUser + "-"
		prefix2 := "-Users-" + dashedUser
		if strings.HasPrefix(name, prefix1) {
			name = name[len(prefix1):]
		} else if strings.HasPrefix(name, prefix2) {
			name = name[len(prefix2):]
		}
	}
	if name == "" {
		return "(root)"
	}
	return strings.ReplaceAll(name, "-", "/")
}

func resolveClaudeSessionPath(input string) (string, string, error) {
	if strings.HasSuffix(input, ".jsonl") || strings.Contains(input, "/") {
		return input, "", nil
	}

	baseDir := expandHome(defaultClaudeSessions)
	target := input + ".jsonl"
	var matches []string

	err := filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) == target {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("Claude session not found: %s", input)
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("multiple Claude sessions found for ID %s", input)
	}
	return matches[0], input, nil
}

func claudeDetail(input string) error {
	fp, sessionID, err := resolveClaudeSessionPath(input)
	if err != nil {
		return err
	}

	res, err := claudeMessages(fp)
	if err != nil {
		return err
	}
	if len(res.Steps) == 0 {
		return fmt.Errorf("no Claude messages in %s", fp)
	}

	dirName := filepath.Base(filepath.Dir(fp))
	project := cleanClaudeProjectName(dirName)
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(fp), ".jsonl")
	}

	modelCount := make(map[string]int)
	for _, m := range res.Models {
		modelCount[m]++
	}
	var primaryModel string
	var maxCount int
	for m, c := range modelCount {
		if c > maxCount || (c == maxCount && (primaryModel == "" || m < primaryModel)) {
			maxCount = c
			primaryModel = m
		}
	}
	prices := claudePrices[primaryModel]
	if prices.Input == 0 {
		return fmt.Errorf("no prices configured for model %s", primaryModel)
	}

	fmt.Printf("Session:  %s\n", sessionID)
	fmt.Printf("File:     %s\n", filepath.Base(fp))
	fmt.Printf("Project:  %s\n", project)
	fmt.Printf("Model:    %s\n", primaryModel)
	fmt.Printf("Messages: %d\n", len(res.Steps))
	fmt.Printf("ToolCalls: %d\n\n", res.ToolCalls)

	byModel := make(map[string][]StepData)
	for _, step := range res.Steps {
		byModel[step.Model] = append(byModel[step.Model], step.Step)
	}

	modelNames := make([]string, 0, len(byModel))
	for model := range byModel {
		modelNames = append(modelNames, model)
	}
	sort.Strings(modelNames)

	var totalActual, totalIdeal float64
	for i, model := range modelNames {
		prices := claudePrices[model]
		if prices.Input == 0 {
			return fmt.Errorf("no prices configured for model %s", model)
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("=== %s (%d messages) ===\n\n", model, len(byModel[model]))
		rows := ComputeIdealClaude(byModel[model], prices)
		printDetailRowsClaude(rows, prices)
		s := SummarizeClaude(rows, prices)
		totalActual += s.Actual
		totalIdeal += s.Ideal
		fmt.Printf("\nSubtotal actual: $%.2f\n", s.Actual)
		fmt.Printf("Subtotal ideal:  $%.2f\n", s.Ideal)
		fmt.Printf("Subtotal overpay: $%.2f (%.1f%% of ideal)\n", s.Overpay, s.PctIdeal)
	}

	totalOverpay := totalActual - totalIdeal
	if totalOverpay < 0 {
		totalOverpay = 0
	}
	pctIdeal := 0.0
	if totalIdeal > 0 {
		pctIdeal = totalOverpay / totalIdeal * 100
	}

	fmt.Printf("\nTOTAL\n")
	fmt.Printf("Actual paid:  $%.2f\n", totalActual)
	fmt.Printf("Ideal paid:   $%.2f\n", totalIdeal)
	fmt.Printf("Overpay:      $%.2f (%.1f%% of ideal)\n", totalOverpay, pctIdeal)

	return nil
}

func claudeList(days int, date string) error {
	sessions, err := claudeSessions(days, date, "")
	if err != nil {
		return err
	}

	fmt.Printf("%19s  %-36s  %-14s  %-25s  %4s  %8s  %8s  %7s  %10s  %7s  %8s  %8s\n",
		"DateTime", "SessionID", "DominantModel", "Project", "Msgs", "Tokens", "Paid", "Ideal", "Overpay", "%ideal", "$/1M", "i$/1M")
	fmt.Println(strings.Repeat("-", 179))

	var totalActual, totalIdeal float64
	var totalIn, totalCC, totalCR, totalOut int

	for _, sess := range sessions {
		res, err := claudeMessages(sess.Filepath)
		if err != nil || len(res.Steps) == 0 {
			continue
		}

		byModel := make(map[string][]StepData)
		for _, step := range res.Steps {
			prices := claudePrices[step.Model]
			if prices.Input == 0 {
				byModel = nil
				break
			}
			byModel[step.Model] = append(byModel[step.Model], step.Step)
		}
		if byModel == nil {
			continue
		}

		var s ClaudeSummary
		for model, modelSteps := range byModel {
			prices := claudePrices[model]
			rows := ComputeIdealClaude(modelSteps, prices)
			part := SummarizeClaude(rows, prices)
			s.TotalCC += part.TotalCC
			s.TotalCR += part.TotalCR
			s.TotalIn += part.TotalIn
			s.TotalOut += part.TotalOut
			s.TotalIdealCR += part.TotalIdealCR
		s.TotalIdealIn += part.TotalIdealIn
			s.TotalIdealCC += part.TotalIdealCC
						s.TotalWaste += part.TotalWaste
			s.Actual += part.Actual
			s.Ideal += part.Ideal
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
		totalCC += s.TotalCC
		totalCR += s.TotalCR
		totalOut += s.TotalOut

		timestamp := sess.Birth.UTC().Format("2006-01-02 15:04:05")
		project := sess.Project
		if len(project) > 25 {
			project = project[:23] + ".."
		}
		modelShort := sess.DominantModel
		if modelShort == "claude-opus-4-7" {
			modelShort = "opus-4.7"
		}
		if modelShort == "claude-sonnet-4-6" {
			modelShort = "sonnet-4.6"
		}

		tokens := s.TotalIn + s.TotalCC + s.TotalCR + s.TotalOut
		costPer1M := 0.0
		if tokens > 0 {
			costPer1M = s.Actual / float64(tokens) * 1e6
		}
		idealPer1M := 0.0
		if tokens > 0 {
			idealPer1M = s.Ideal / float64(tokens) * 1e6
		}

		fmt.Printf("%19s  %-36s  %-14s  %-25s  %4d  %8s  %8.2f  %7.2f  %10.2f  %6.1f%%  %8.2f  %8.2f\n",
			timestamp, sess.ID, modelShort, project, sess.Msgs, formatTokens(tokens), s.Actual, s.Ideal, s.Overpay, s.PctIdeal, costPer1M, idealPer1M)
	}

	fmt.Println(strings.Repeat("-", 179))
	totalOverpay := max(totalActual-totalIdeal, 0)
	pct := float64(0)
	if totalIdeal > 0 {
		pct = totalOverpay / totalIdeal * 100
	}

	totalTokens := totalIn + totalCC + totalCR + totalOut
	totalCostPer1M := 0.0
	if totalTokens > 0 {
		totalCostPer1M = totalActual / float64(totalTokens) * 1e6
	}
	totalIdealPer1M := 0.0
	if totalTokens > 0 {
		totalIdealPer1M = totalIdeal / float64(totalTokens) * 1e6
	}

	fmt.Printf("%19s  %-36s  %-14s  %-25s  %4s  %8s  %8.2f  %7.2f  %10.2f  %6.1f%%  %8.2f  %8.2f\n",
		"TOTAL", "", "", "", "", formatTokens(totalTokens), totalActual, totalIdeal, totalOverpay, pct, totalCostPer1M, totalIdealPer1M)
	fmt.Println()
	fmt.Printf("Opus4.7:  In=$%.2f/M  CC=$%.2f/M  CR=$%.2f/M  Out=$%.2f/M\n",
		claudePrices["claude-opus-4-7"].Input,
		claudePrices["claude-opus-4-7"].CacheCreation,
		claudePrices["claude-opus-4-7"].CacheRead,
		claudePrices["claude-opus-4-7"].Output)
	fmt.Printf("Sonnet4.6: In=$%.2f/M  CC=$%.2f/M  CR=$%.2f/M  Out=$%.2f/M\n",
		claudePrices["claude-sonnet-4-6"].Input,
		claudePrices["claude-sonnet-4-6"].CacheCreation,
		claudePrices["claude-sonnet-4-6"].CacheRead,
		claudePrices["claude-sonnet-4-6"].Output)

	return nil
}
