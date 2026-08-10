package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"tokeneks/compute"
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
	Step  compute.StepData
	Cost  float64 // actual cost from session data
}

func (s piSessionStep) modelKey() string           { return s.Model }
func (s piSessionStep) stepData() compute.StepData { return s.Step }

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

		step := compute.StepData{
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

func piMessages(fp string) ([]compute.StepData, error) {
	data, err := piSessionUsage(fp)
	if err != nil {
		return nil, err
	}
	steps := make([]compute.StepData, 0, len(data.Steps))
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
	Data          *piSessionData
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
		sessionID, _ := piSessionIDFromFilename(sessionName)
		childCount := piSubsessionCount(fp, cutoff, date)

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
			Data:          &data,
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

// piSubsessionPaths returns the session files of every subagent spawned by
// sessionFilepath. PI writes subagent sessions in two layouts and both are
// covered here:
//
//   - nested:  <parentBase>/<shortID>/run-<N>/session.jsonl
//   - sibling: a top-level <date>_<id>.jsonl in the same project directory
//     whose session header carries parentSession: <parentPath>
//
// The sibling layout is used for subagents that run in the parent's own
// project directory (e.g. worker/coder runs), so the nested scan alone misses
// them — their <shortID> directory is created but left empty.
func piSubsessionPaths(sessionFilepath string) []string {
	paths := piSiblingSubsessionPaths(sessionFilepath)

	sessionDir := strings.TrimSuffix(sessionFilepath, ".jsonl")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		sort.Strings(paths)
		return paths
	}
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

// piSubsessionCount counts the subagent sessions of sessionFilepath that fall
// inside the requested window. Both storage layouts are counted — see
// piSubsessionPaths.
func piSubsessionCount(sessionFilepath string, cutoff time.Time, date string) int {
	inWindow := func(fp string) bool {
		info, err := os.Stat(fp)
		if err != nil || info.IsDir() {
			return false
		}
		if date != "" {
			return info.ModTime().UTC().Format("2006-01-02") == date
		}
		return !info.ModTime().Before(cutoff)
	}

	count := 0
	for _, fp := range piSiblingSubsessionPaths(sessionFilepath) {
		if inWindow(fp) {
			count++
		}
	}

	sessionDir := strings.TrimSuffix(sessionFilepath, ".jsonl")
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return count
	}
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
			if inWindow(filepath.Join(sessionDir, entry.Name(), runDir.Name(), "session.jsonl")) {
				count++
			}
		}
	}
	return count
}

// piSiblingSubsessionPaths returns the top-level session files in the same
// project directory as sessionFilepath whose session header links back to it.
// These are subagent sessions PI stored alongside the parent instead of
// nesting them under <parentBase>/<shortID>/run-<N>/.
func piSiblingSubsessionPaths(sessionFilepath string) []string {
	dir := filepath.Dir(sessionFilepath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	target := absPath(sessionFilepath)
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		fp := filepath.Join(dir, entry.Name())
		if absPath(fp) == target {
			continue
		}
		parentPath, _, ok := piSessionHeaderParent(fp)
		if ok && absPath(parentPath) == target {
			paths = append(paths, fp)
		}
	}
	return paths
}

// piSessionHeaderParent reads the `type: "session"` header line of a PI
// session file and returns the parent it links to via `parentSession`, plus
// that parent's session ID. Not every session has one — only subagent
// sessions stored in the sibling layout (see piSubsessionPaths).
func piSessionHeaderParent(fp string) (parentPath, parentID string, ok bool) {
	f, err := os.Open(fp)
	if err != nil {
		return "", "", false
	}
	defer f.Close()

	scanner := newJSONLScanner(f)
	if !scanner.Scan() {
		return "", "", false
	}
	var hdr struct {
		Type          string `json:"type"`
		ParentSession string `json:"parentSession"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &hdr); err != nil {
		return "", "", false
	}
	if hdr.Type != "session" || hdr.ParentSession == "" {
		return "", "", false
	}
	if info, err := os.Stat(hdr.ParentSession); err != nil || info.IsDir() {
		return "", "", false
	}
	sessionID := piSessionIDFromPath(hdr.ParentSession)
	if sessionID == "" {
		return "", "", false
	}
	return hdr.ParentSession, sessionID, true
}

// piResolveParent returns the parent session of fp, checking the nested
// run-directory layout first and the session-header link second.
func piResolveParent(fp string) (parentPath, parentID string, ok bool) {
	if parentPath, parentID, ok := piParentSessionInfo(fp); ok {
		return parentPath, parentID, true
	}
	return piSessionHeaderParent(fp)
}

// absPath returns the absolute form of p, falling back to p when the working
// directory cannot be resolved. Used to compare session paths for identity.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
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

	baseDir := expandHome(defaultPISessions)
	cutoff := time.Now().AddDate(0, 0, -days)

	var rootMatch string
	var childMatch string

	if err := walkSessionFiles(baseDir, func(fp string, info os.FileInfo) error {
		if days >= 0 && getCreatedAtFromInfo(info).Before(cutoff) {
			return nil
		}

		sessionName := filepath.Base(fp)
		sessionID, err := piSessionIDFromFilename(sessionName)
		if err != nil {
			return nil
		}
		if rootMatch == "" && sessionID == input {
			rootMatch = fp
			return nil
		}
		if childMatch == "" {
			for _, childPath := range piSubsessionPaths(fp) {
				if piSessionIDFromPath(childPath) == input {
					childMatch = childPath
					return nil
				}
			}
		}
		return nil
	}); err != nil {
		return "", "", err
	}

	if rootMatch != "" {
		return rootMatch, input, nil
	}
	if childMatch != "" {
		return childMatch, input, nil
	}
	return "", "", fmt.Errorf("PI session not found: %s", input)
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

	byModel := groupStepsByModel(data.Steps)

	// groupStepsByModel keeps only tokens, not the per-message logged cost
	// (compute.StepData has no Cost field), so the unpriced report below
	// needs its own pass over the untouched piSessionStep list to know how
	// much money a model with no resolvable price was actually billed.
	loggedByModel := make(map[string]float64, len(byModel))
	for _, st := range data.Steps {
		loggedByModel[st.Model] += st.Cost
	}

	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)

	var totalActual, totalIdeal float64
	var unpriced []unpricedModel
	printedAny := false
	for _, model := range models {
		steps := byModel[model]

		prices, ok := resolveAgentPrices("pi", model)
		if !ok {
			var tokens int
			for _, s := range steps {
				tokens += s.Input + s.CacheCreation + s.CacheRead + s.Output
			}
			unpriced = append(unpriced, unpricedModel{Agent: "PI", Model: model, Tokens: tokens, Steps: len(steps), LoggedCost: loggedByModel[model]})
			continue
		}

		if printedAny {
			fmt.Println()
		}
		printedAny = true
		fmt.Printf("=== %s (%d messages) ===\n\n", model, len(steps))

		if !prices.SupportsCacheCreation {
			rows := compute.ComputeIdeal(steps)
			printDetailRows(rows, uniformDetailPricing(len(rows), model, prices), false)
			s := compute.Summarize(rows, prices)
			totalActual += s.Actual
			totalIdeal += s.Ideal
			fmt.Printf("\nSubtotal actual: $%.2f\n", s.Actual)
			fmt.Printf("Subtotal ideal:  $%.2f\n", s.Ideal)
			fmt.Printf("Subtotal overpay: $%.2f (%.1f%% of ideal)\n", s.Overpay, s.PctIdeal)
		} else {
			rows := compute.ComputeIdealClaude(steps, prices)
			printDetailRowsClaude(rows, uniformDetailPricing(len(rows), model, prices))
			s := compute.SummarizeClaude(rows, prices)
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

	printUnpricedModels(unpriced)

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

	unpricedByModel := map[string]*unpricedModel{}
	for _, sess := range sessions {
		data := sess.Data
		if data == nil || len(data.Steps) == 0 {
			continue
		}

		byModel := groupStepsByModel(data.Steps)

		// See piDetail for why this needs its own pass: groupStepsByModel
		// only keeps compute.StepData, which has no Cost field.
		loggedByModel := make(map[string]float64, len(byModel))
		for _, st := range data.Steps {
			loggedByModel[st.Model] += st.Cost
		}

		var s compute.Summary
		for model, steps := range byModel {
			prices, ok := resolveAgentPrices("pi", model)
			if !ok {
				u := unpricedByModel[model]
				if u == nil {
					u = &unpricedModel{Agent: "PI", Model: model}
					unpricedByModel[model] = u
				}
				for _, st := range steps {
					u.Tokens += st.Input + st.CacheCreation + st.CacheRead + st.Output
				}
				u.Steps += len(steps)
				u.LoggedCost += loggedByModel[model]
				continue
			}
			if !prices.SupportsCacheCreation {
				rows := compute.ComputeIdeal(steps)
				part := compute.Summarize(rows, prices)
				s.TotalCR += part.TotalCR
				s.TotalIn += part.TotalIn
				s.TotalOut += part.TotalOut
				s.TotalIdealCR += part.TotalIdealCR
				s.TotalIdealIn += part.TotalIdealIn
				s.TotalWaste += part.TotalWaste
				s.Actual += part.Actual
				s.Ideal += part.Ideal
			} else {
				rows := compute.ComputeIdealClaude(steps, prices)
				part := compute.SummarizeClaude(rows, prices)
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
		costPer1M := compute.PerMillion(s.Actual, tokens)
		idealPer1M := compute.PerMillion(s.Ideal, tokens)

		fmt.Printf("%19s  %-36s  %-18.18s  %4d  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
			timestamp, sess.ID, sess.DominantModel, sess.Msgs, formatTokens(tokens), s.Actual, s.Ideal, s.Overpay, s.PctIdeal, costPer1M, idealPer1M)
	}

	fmt.Println(strings.Repeat("-", separatorWidthPi))
	totalTokens := totalIn + totalCR + totalOut
	totalOverpay, pct, totalCostPer1M, totalIdealPer1M := footerTotals(totalActual, totalIdeal, totalTokens)

	fmt.Printf("%19s  %-36s  %-18s  %4s  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
		"TOTAL", "", "", "", formatTokens(totalTokens), totalActual, totalIdeal, totalOverpay, pct, totalCostPer1M, totalIdealPer1M)

	unpriced := make([]unpricedModel, 0, len(unpricedByModel))
	for _, u := range unpricedByModel {
		unpriced = append(unpriced, *u)
	}
	sort.Slice(unpriced, func(i, j int) bool { return unpriced[i].Model < unpriced[j].Model })
	printUnpricedModels(unpriced)

	return nil
}
