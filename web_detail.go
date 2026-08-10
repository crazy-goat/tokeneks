package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"tokeneks/compute"
)

type ToolCallInfo struct {
	Name       string          `json:"name"`
	ID         string          `json:"id,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      bool            `json:"error,omitempty"`
	Status     string          `json:"status,omitempty"`
	DurationMs int64           `json:"durationMs,omitempty"`
}

type StepInfo struct {
	Step       int    `json:"step"`
	Timestamp  string `json:"timestamp,omitempty"`
	Model      string `json:"model,omitempty"`
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	CacheRead  int    `json:"cacheRead"`
	CacheWrite int    `json:"cacheWrite"`
	// CacheWrite1h is the 1-hour-TTL slice of CacheWrite; Claude-only, needed
	// to price cache writes at the right rate instead of always the 5m one.
	CacheWrite1h int     `json:"cacheWrite1h"`
	Cost         float64 `json:"cost"`
	// Ideal is this step's own counterfactual cost, priced at the same
	// per-step rate as Cost — see computeSessionPricing in web_store.go.
	// Not omitempty: a step can legitimately have Ideal == 0 (its first
	// message in the session, before any context exists to reuse), and
	// that must render as $0.0000, not vanish from the JSON.
	Ideal      float64        `json:"ideal"`
	Thinking   string         `json:"thinking,omitempty"`
	Response   string         `json:"response,omitempty"`
	UserPrompt string         `json:"userPrompt,omitempty"`
	StopReason string         `json:"stopReason,omitempty"`
	ToolCalls  []ToolCallInfo `json:"toolCalls,omitempty"`
}

type ToolDurationStat struct {
	Name  string `json:"name"`
	AvgMs int64  `json:"avgMs"`
	MaxMs int64  `json:"maxMs"`
	Count int    `json:"count"`
}

type ModelStats struct {
	Model      string  `json:"model"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Steps      int     `json:"steps"`
	Cost       float64 `json:"cost"`
	CacheHit   float64 `json:"cacheHit"`
}

type SessionLink struct {
	Agent           string  `json:"agent"`
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Project         string  `json:"project,omitempty"`
	Model           string  `json:"model,omitempty"`
	Date            string  `json:"date,omitempty"`
	TotalInput      int     `json:"totalInput,omitempty"`
	TotalOutput     int     `json:"totalOutput,omitempty"`
	TotalCacheRead  int     `json:"totalCacheRead,omitempty"`
	TotalCacheWrite int     `json:"totalCacheWrite,omitempty"`
	CacheHitRate    float64 `json:"cacheHitRate,omitempty"`
	Steps           int     `json:"steps,omitempty"`
	TotalCost       float64 `json:"totalCost,omitempty"`
}

type SessionDetail struct {
	Agent     string     `json:"agent"`
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Project   string     `json:"project"`
	Model     string     `json:"model"`
	Date      string     `json:"date"`
	Duration  string     `json:"duration"`
	Steps     []StepInfo `json:"steps"`
	TotalCost float64    `json:"totalCost"`
	// Ideal/Overpay/OverpayPct are this session's own totals — see
	// WebSession for what they mean and computeSessionPricing (web_store.go)
	// for how they're computed. Only getSessionDetailFromStore (the path
	// the web dashboard actually uses) fills these in; the legacy
	// direct-parse *SessionDetail builders below (used only by the
	// ingester, not the web UI) leave them at zero.
	Ideal             float64       `json:"ideal"`
	Overpay           float64       `json:"overpay"`
	OverpayPct        float64       `json:"overpayPct"`
	UnpricedTokens    int           `json:"unpricedTokens,omitempty"`
	PartiallyUnpriced bool          `json:"partiallyUnpriced,omitempty"`
	Parent            *SessionLink  `json:"parent,omitempty"`
	Children          []SessionLink `json:"children,omitempty"`
	// TotalCostInclChildren is the cost of this session's own steps plus
	// the cost of all descendant subsessions (recursively). Not part of
	// TotalCost so per-session numbers stay comparable across the UI.
	TotalCostInclChildren float64 `json:"totalCostInclChildren,omitempty"`

	TotalInput      int                `json:"totalInput"`
	TotalOutput     int                `json:"totalOutput"`
	TotalCacheRead  int                `json:"totalCacheRead"`
	TotalCacheWrite int                `json:"totalCacheWrite"`
	TotalToolCalls  int                `json:"totalToolCalls"`
	ToolErrors      int                `json:"toolErrors"`
	CacheHitRate    float64            `json:"cacheHitRate"`
	StopReasons     map[string]int     `json:"stopReasons"`
	AvgThinkingLen  int                `json:"avgThinkingLen"`
	MaxThinkingLen  int                `json:"maxThinkingLen"`
	AvgResponseLen  int                `json:"avgResponseLen"`
	MaxResponseLen  int                `json:"maxResponseLen"`
	ToolDurations   []ToolDurationStat `json:"toolDurations,omitempty"`
	ModelStats      []ModelStats       `json:"modelStats,omitempty"`
}

// sortChildrenByDate orders subsessions oldest first. Children without a date
// keep their discovery order at the end of the list.
func sortChildrenByDate(d *SessionDetail) {
	sort.SliceStable(d.Children, func(i, j int) bool {
		a, b := d.Children[i].Date, d.Children[j].Date
		if a == "" || b == "" {
			return a != "" && b == ""
		}
		return a < b
	})
}

func fillSessionStats(d *SessionDetail) {
	if len(d.Steps) == 0 {
		return
	}
	var totalInput, totalOutput, totalCR, totalCW, totalTC, toolErrs int
	var thinkTotal, thinkMax, respTotal, respMax int
	var thinkCount, respCount int
	stopReasons := make(map[string]int)
	modelsSeen := make(map[string]bool)
	var modelsOrder []string
	modelStatsMap := make(map[string]*ModelStats)

	var firstTs, lastTs time.Time
	type durAccum struct {
		total int64
		max   int64
		count int
	}
	durByTool := make(map[string]*durAccum)
	for i, s := range d.Steps {
		totalInput += s.Input
		totalOutput += s.Output
		totalCR += s.CacheRead
		totalCW += s.CacheWrite
		totalTC += len(s.ToolCalls)
		if s.Model != "" {
			if !modelsSeen[s.Model] {
				modelsSeen[s.Model] = true
				modelsOrder = append(modelsOrder, s.Model)
			}
			ms, ok := modelStatsMap[s.Model]
			if !ok {
				ms = &ModelStats{Model: s.Model}
				modelStatsMap[s.Model] = ms
			}
			ms.Input += s.Input
			ms.Output += s.Output
			ms.CacheRead += s.CacheRead
			ms.CacheWrite += s.CacheWrite
			ms.Cost += s.Cost
			ms.Steps++
		}
		if s.StopReason != "" {
			stopReasons[s.StopReason]++
		}
		for _, tc := range s.ToolCalls {
			if tc.Error {
				toolErrs++
			}
			if tc.DurationMs > 0 {
				a, ok := durByTool[tc.Name]
				if !ok {
					a = &durAccum{}
					durByTool[tc.Name] = a
				}
				a.total += tc.DurationMs
				a.count++
				if tc.DurationMs > a.max {
					a.max = tc.DurationMs
				}
			}
		}
		if s.Thinking != "" {
			l := len(s.Thinking)
			thinkTotal += l
			thinkCount++
			if l > thinkMax {
				thinkMax = l
			}
		}
		if s.Response != "" {
			l := len(s.Response)
			respTotal += l
			respCount++
			if l > respMax {
				respMax = l
			}
		}
		// parse timestamp for duration
		if s.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, s.Timestamp); err == nil {
				if i == 0 {
					firstTs = t
				}
				lastTs = t
			} else if t, err := time.Parse("2006-01-02 15:04:05", s.Timestamp); err == nil {
				if i == 0 {
					firstTs = t
				}
				lastTs = t
			}
		}
	}

	d.TotalInput = totalInput
	d.TotalOutput = totalOutput
	d.TotalCacheRead = totalCR
	d.TotalCacheWrite = totalCW
	// TotalCacheWrite stays as is (not added to totalInput)
	d.TotalToolCalls = totalTC
	d.ToolErrors = toolErrs
	d.StopReasons = stopReasons
	// Set model from all unique models found in steps (overrides single session-level model)
	if len(modelsOrder) > 1 {
		d.Model = strings.Join(modelsOrder, ", ")
	} else if d.Model == "" && len(modelsOrder) > 0 {
		d.Model = strings.Join(modelsOrder, ", ")
	}
	// Build model stats array in order of appearance
	if len(modelStatsMap) > 0 {
		modelStats := make([]ModelStats, 0, len(modelStatsMap))
		for _, modelName := range modelsOrder {
			ms := modelStatsMap[modelName]
			if ms.Input+ms.CacheRead > 0 {
				ms.CacheHit = float64(ms.CacheRead) / float64(ms.Input+ms.CacheRead) * 100
			}
			modelStats = append(modelStats, *ms)
		}
		d.ModelStats = modelStats
	}
	// Cache hit rate based on token ratio (not step count)
	if totalInput+totalCR > 0 {
		d.CacheHitRate = float64(totalCR) / float64(totalInput+totalCR) * 100
	}
	if thinkCount > 0 {
		d.AvgThinkingLen = thinkTotal / thinkCount
		d.MaxThinkingLen = thinkMax
	}
	if respCount > 0 {
		d.AvgResponseLen = respTotal / respCount
		d.MaxResponseLen = respMax
	}
	// aggregate tool durations
	if len(durByTool) > 0 {
		toolDurs := make([]ToolDurationStat, 0, len(durByTool))
		for name, a := range durByTool {
			toolDurs = append(toolDurs, ToolDurationStat{
				Name:  name,
				AvgMs: a.total / int64(a.count),
				MaxMs: a.max,
				Count: a.count,
			})
		}
		d.ToolDurations = toolDurs
	}

	if !firstTs.IsZero() && !lastTs.IsZero() {
		dur := lastTs.Sub(firstTs)
		if dur < time.Minute {
			d.Duration = fmt.Sprintf("%ds", int(dur.Seconds()))
		} else if dur < time.Hour {
			d.Duration = fmt.Sprintf("%dm %ds", int(dur.Minutes()), int(dur.Seconds())%60)
		} else {
			d.Duration = fmt.Sprintf("%dh %dm", int(dur.Hours()), int(dur.Minutes())%60)
		}
	}
}

func sessionRequestParts(path, prefix string) (agent, id string, ok bool) {
	trimmed := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writeSessionRevisionPart(h hash.Hash64, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(h, "%s|%d|%d\n", path, info.ModTime().UTC().UnixNano(), info.Size())
	return nil
}

func piSessionRevision(fp string) (string, error) {
	h := fnv.New64a()
	if err := writeSessionRevisionPart(h, fp); err != nil {
		return "", err
	}
	sessionDir := strings.TrimSuffix(fp, ".jsonl")
	_ = filepath.WalkDir(sessionDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || filepath.Ext(path) != ".jsonl" || path == fp {
			return nil
		}
		_ = writeSessionRevisionPart(h, path)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum64()), nil
}

func claudeSessionRevision(fp string) (string, error) {
	h := fnv.New64a()
	if err := writeSessionRevisionPart(h, fp); err != nil {
		return "", err
	}
	sessID := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
	subDir := filepath.Join(filepath.Dir(fp), sessID, "subagents")
	_ = filepath.WalkDir(subDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		_ = writeSessionRevisionPart(h, path)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum64()), nil
}

func ocSessionRevision(sessionID string) (string, error) {
	db, err := openOCDB()
	if err != nil {
		return "", err
	}
	var sessionMax, sessionCount, partMax, partCount int64
	if err := db.QueryRow(`SELECT ifnull(MAX(time_created), 0), COUNT(*) FROM session WHERE id = ? OR parent_id = ?`, sessionID, sessionID).Scan(&sessionMax, &sessionCount); err != nil {
		return "", err
	}
	if err := db.QueryRow(`SELECT ifnull(MAX(time_created), 0), COUNT(*) FROM part WHERE session_id = ? OR session_id IN (SELECT id FROM session WHERE parent_id = ?)`, sessionID, sessionID).Scan(&partMax, &partCount); err != nil {
		return "", err
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%d|%d|%d|%d", sessionID, sessionMax, sessionCount, partMax, partCount)
	return fmt.Sprintf("%x", h.Sum64()), nil
}

func sessionRevision(agent, id string) (string, error) {
	return sessionRevisionFromStore(context.Background(), agent, id)
}

func handleAPISessionStream(w http.ResponseWriter, r *http.Request) {
	agent, id, ok := sessionRequestParts(r.URL.Path, "/api/session-stream/")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	revision, err := sessionRevision(agent, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err.Error() == "unknown agent" {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, "event: revision\ndata: %s\n\n", revision)
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
			nextRevision, err := sessionRevision(agent, id)
			if err != nil {
				log.Printf("session stream stopped for %s/%s: %v", agent, id, err)
				return
			}
			if nextRevision == revision {
				continue
			}
			revision = nextRevision
			fmt.Fprintf(w, "event: revision\ndata: %s\n\n", revision)
			flusher.Flush()
		}
	}
}

func handleAPISessionDetail(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("panic in handleAPISessionDetail: %v", rec)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()
	agent, id, ok := sessionRequestParts(r.URL.Path, "/api/session/")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	detail, err := getSessionDetailFromStore(r.Context(), agent, id)
	if err != nil {
		if err.Error() == "no messages for session "+strings.ToLower(agent)+"/"+id {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no messages") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if revision, err := sessionRevisionFromStore(r.Context(), agent, id); err == nil {
		w.Header().Set("X-Session-Revision", revision)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	json.NewEncoder(w).Encode(detail)
}

func handleAPISessionMarkdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agent, id, ok := sessionRequestParts(r.URL.Path, "/api/session-markdown/")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	detail, err := getSessionDetailFromStore(r.Context(), agent, id)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "no messages") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	includeSubagents := r.URL.Query().Get("include_subagents") == "1" || r.URL.Query().Get("include_subagents") == "true"
	var markdown strings.Builder
	appendMarkdownSession(&markdown, r.Context(), detail, 1, includeSubagents, make(map[sessKey]bool))

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+markdownDownloadFilename(agent, id)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(markdown.String()))
}

func appendMarkdownSession(b *strings.Builder, ctx context.Context, detail *SessionDetail, level int, includeSubagents bool, seen map[sessKey]bool) {
	key := sessKey{agent: strings.ToLower(detail.Agent), id: detail.ID}
	if seen[key] {
		return
	}
	seen[key] = true

	title := detail.Title
	if strings.TrimSpace(title) == "" {
		title = detail.Agent + " session " + detail.ID
	}
	fmt.Fprintf(b, "%s %s\n\n", markdownHeading(level), markdownInline(title))
	if detail.Agent != "" {
		fmt.Fprintf(b, "- Agent: `%s`\n", markdownCodeInline(detail.Agent))
	}
	if detail.ID != "" {
		fmt.Fprintf(b, "- Session ID: `%s`\n", markdownCodeInline(detail.ID))
	}
	if detail.Project != "" {
		fmt.Fprintf(b, "- Project: `%s`\n", markdownCodeInline(detail.Project))
	}
	if detail.Model != "" {
		fmt.Fprintf(b, "- Model: `%s`\n", markdownCodeInline(detail.Model))
	}
	if detail.Date != "" {
		fmt.Fprintf(b, "- Date: %s\n", markdownInline(detail.Date))
	}
	totalCost := detail.TotalCost
	if totalCost == 0 {
		for _, step := range detail.Steps {
			totalCost += step.Cost
		}
	}
	fmt.Fprintf(b, "- Steps: %d\n- Total cost: $%.4f\n\n", len(detail.Steps), totalCost)

	for _, step := range detail.Steps {
		fmt.Fprintf(b, "%s Step %d\n\n", markdownHeading(level+1), step.Step)
		if step.Timestamp != "" {
			fmt.Fprintf(b, "- Timestamp: %s\n", markdownInline(step.Timestamp))
		}
		if step.Model != "" {
			fmt.Fprintf(b, "- Model: `%s`\n", markdownCodeInline(step.Model))
		}
		fmt.Fprintf(b, "- Tokens: input %d, output %d, cache read %d, cache write %d\n- Cost: $%.4f\n\n",
			step.Input, step.Output, step.CacheRead, step.CacheWrite, step.Cost)

		appendMarkdownTextSection(b, level+2, "User prompt", step.UserPrompt)
		appendMarkdownTextSection(b, level+2, "Thinking", step.Thinking)
		appendMarkdownTextSection(b, level+2, "Response", step.Response)
		if step.StopReason != "" {
			fmt.Fprintf(b, "%s Stop reason\n\n%s\n\n", markdownHeading(level+2), markdownInline(step.StopReason))
		}

		for _, tool := range step.ToolCalls {
			name := tool.Name
			if strings.TrimSpace(name) == "" {
				name = "unnamed"
			}
			fmt.Fprintf(b, "%s Tool: %s\n\n", markdownHeading(level+2), markdownInline(name))
			if tool.ID != "" {
				fmt.Fprintf(b, "- Call ID: `%s`\n", markdownCodeInline(tool.ID))
			}
			if tool.Status != "" {
				fmt.Fprintf(b, "- Status: %s\n", markdownInline(tool.Status))
			}
			if tool.Error {
				b.WriteString("- Error: true\n")
			}
			if tool.DurationMs > 0 {
				fmt.Fprintf(b, "- Duration: %d ms\n", tool.DurationMs)
			}
			if tool.ID != "" || tool.Status != "" || tool.Error || tool.DurationMs > 0 {
				b.WriteString("\n")
			}
			appendMarkdownCodeSection(b, level+3, "Input", markdownRawValue(tool.Input))
			appendMarkdownCodeSection(b, level+3, "Output", markdownRawValue(tool.Output))
		}
	}

	if !includeSubagents || len(detail.Children) == 0 {
		return
	}

	fmt.Fprintf(b, "%s Subagents\n\n", markdownHeading(level+1))
	totalIncl := totalCost
	for _, child := range detail.Children {
		childDetail, err := getSessionDetailFromStore(ctx, child.Agent, child.ID)
		if err != nil {
			childTitle := child.Title
			if childTitle == "" {
				childTitle = child.ID
			}
			fmt.Fprintf(b, "%s %s\n\n> Unable to load subagent session: %s\n\n",
				markdownHeading(level+2), markdownInline(childTitle), markdownInline(err.Error()))
			totalIncl += child.TotalCost
			continue
		}
		totalIncl += childDetail.TotalCostInclChildren
		appendMarkdownSession(b, ctx, childDetail, level+2, true, seen)
	}
	fmt.Fprintf(b, "**Total cost including subagents: $%.4f**\n\n", totalIncl)
}

func appendMarkdownTextSection(b *strings.Builder, level int, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fmt.Fprintf(b, "%s %s\n\n%s\n\n", markdownHeading(level), label, value)
}

func appendMarkdownCodeSection(b *strings.Builder, level int, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fence := "```"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	fmt.Fprintf(b, "%s %s\n\n%s\n%s\n%s\n\n", markdownHeading(level), label, fence, value, fence)
}

func markdownRawValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if formatted, err := json.MarshalIndent(value, "", "  "); err == nil {
			return string(formatted)
		}
	}
	return string(raw)
}

func markdownHeading(level int) string {
	if level < 1 {
		level = 1
	}
	return strings.Repeat("#", level)
}

func markdownInline(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"#", "\\#",
		"*", "\\*",
		"_", "\\_",
		"[", "\\[",
		"]", "\\]",
	).Replace(value)
}

func markdownCodeInline(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}

func markdownDownloadFilename(agent, id string) string {
	var safe strings.Builder
	for _, r := range strings.ToLower(agent + "-" + id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe.WriteRune(r)
		default:
			safe.WriteRune('-')
		}
	}
	name := strings.Trim(safe.String(), "-.")
	if name == "" {
		name = "session"
	}
	return "session-" + name + ".md"
}

func ocSessionDetail(sessionID string) (*SessionDetail, error) {
	db, err := openOCDB()
	if err != nil {
		return nil, err
	}

	var title, modelRaw, parentID, projectName string
	var createdAt int64
	var modelName string
	if err := db.QueryRow(`
		SELECT s.title, s.model, s.time_created, ifnull(s.parent_id, ''), coalesce(p.name, p.worktree, '')
		FROM session s
		LEFT JOIN project p ON p.id = s.project_id
		WHERE s.id = ?
	`, sessionID).Scan(&title, &modelRaw, &createdAt, &parentID, &projectName); err != nil && err != sql.ErrNoRows {
		// non-row errors are unexpected; proceed with zero values
	}
	if modelRaw != "" {
		var m struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(modelRaw), &m); err == nil {
			modelName = m.ID
		}
	}

	// preload message roles and per-message model for this session
	msgRows, err := db.Query(`SELECT id, json_extract(data, '$.role') as role, ifnull(json_extract(data, '$.modelID'), json_extract(data, '$.model.modelID')) as model FROM message WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	msgRole := make(map[string]string)  // message_id -> role
	msgModel := make(map[string]string) // message_id -> modelID
	for msgRows.Next() {
		var mid, role, modelID string
		if err := msgRows.Scan(&mid, &role, &modelID); err == nil {
			msgRole[mid] = role
			if modelID != "" {
				msgModel[mid] = modelID
			}
		}
	}
	msgRows.Close()

	rows, err := db.Query(`SELECT message_id, json(data), time_created FROM part WHERE session_id = ? ORDER BY time_created ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []StepInfo
	curIdx := -1
	msgToStepIdx := make(map[string]int)
	var totalCost float64
	var pendingText, pendingThinking string
	var lastUserPrompt string

	type ocPart struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
		State  struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
		} `json:"state"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Cost float64 `json:"cost"`
		Time struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time"`
	}

	for rows.Next() {
		var msgID, raw string
		var ts int64
		if err := rows.Scan(&msgID, &raw, &ts); err != nil {
			continue
		}
		var part ocPart
		if err := json.Unmarshal([]byte(raw), &part); err != nil {
			continue
		}
		role := msgRole[msgID]

		switch part.Type {
		case "text":
			t := strings.TrimSpace(part.Text)
			if t == "" {
				break
			}
			if role == "user" {
				lastUserPrompt = t
				if title == "" {
					title = truncate(t, 80)
				}
			} else {
				if pendingText != "" {
					pendingText += "\n\n" + t
				} else {
					pendingText = t
				}
			}
		case "reasoning":
			t := strings.TrimSpace(part.Text)
			if t == "" {
				break
			}
			if pendingThinking != "" {
				pendingThinking += "\n\n" + t
			} else {
				pendingThinking = t
			}
		case "step-finish":
			stopReason := part.Reason
			if stopReason == "tool-calls" {
				stopReason = "toolUse"
			}
			stepModel := modelName
			if m, ok := msgModel[msgID]; ok && m != "" {
				stepModel = m
			}
			s := StepInfo{
				Step:       len(steps) + 1,
				Timestamp:  time.Unix(ts/1000, (ts%1000)*int64(time.Millisecond)).UTC().Format(time.RFC3339),
				Model:      stepModel,
				Input:      part.Tokens.Input,
				Output:     part.Tokens.Output,
				CacheRead:  part.Tokens.Cache.Read,
				CacheWrite: part.Tokens.Cache.Write,
				Cost:       part.Cost,
				Thinking:   pendingThinking,
				Response:   pendingText,
				StopReason: stopReason,
				UserPrompt: lastUserPrompt,
			}
			lastUserPrompt = ""
			pendingText = ""
			pendingThinking = ""
			steps = append(steps, s)
			curIdx = len(steps) - 1
			msgToStepIdx[msgID] = curIdx
			totalCost += part.Cost
		case "tool":
			idx, ok := msgToStepIdx[msgID]
			if !ok {
				idx = curIdx
			}
			if idx >= 0 {
				var out json.RawMessage
				if b, err := json.Marshal(part.State.Output); err == nil {
					out = json.RawMessage(b)
				}
				status := part.State.Status
				isErr := toolCallIsError(status)
				var input json.RawMessage
				if len(part.State.Input) > 0 {
					input = part.State.Input
				}
				tc := ToolCallInfo{
					Name:   part.Tool,
					ID:     part.CallID,
					Input:  input,
					Output: out,
					Error:  isErr,
					Status: status,
				}
				// tool duration from time.start/time.end if available
				if part.Time.Start > 0 && part.Time.End > 0 {
					tc.DurationMs = (part.Time.End - part.Time.Start) / int64(time.Millisecond)
				}
				steps[idx].ToolCalls = append(steps[idx].ToolCalls, tc)
			}
		}
	}

	d := &SessionDetail{
		Agent:     "OpenCode",
		ID:        sessionID,
		Title:     title,
		Project:   projectName,
		Model:     modelName,
		Date:      time.Unix(createdAt/1000, 0).UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	if parentID != "" {
		var parentTitle string
		if err := db.QueryRow("SELECT title FROM session WHERE id = ?", parentID).Scan(&parentTitle); err != nil && err != sql.ErrNoRows {
			// best-effort parent title lookup; leave empty
		}
		d.Parent = &SessionLink{Agent: "OpenCode", ID: parentID, Title: parentTitle}
	}
	childRows, err := db.Query("SELECT id, title, json_extract(model, '$.id'), ifnull(tokens_input,0), ifnull(tokens_output,0), ifnull(tokens_cache_read,0), ifnull(tokens_cache_write,0), cost, ifnull(time_created,0) FROM session WHERE parent_id = ? ORDER BY time_created ASC", sessionID)
	if err == nil {
		defer childRows.Close()
		for childRows.Next() {
			var child SessionLink
			var childCreated int64
			if err := childRows.Scan(&child.ID, &child.Title, &child.Model, &child.TotalInput, &child.TotalOutput, &child.TotalCacheRead, &child.TotalCacheWrite, &child.TotalCost, &childCreated); err == nil {
				child.Agent = "OpenCode"
				if childCreated > 0 {
					child.Date = time.Unix(childCreated/1000, 0).UTC().Format("2006-01-02 15:04")
				}
				child.Steps, _ = ocStepCount(db, child.ID)
				if child.TotalInput+child.TotalCacheRead > 0 {
					child.CacheHitRate = float64(child.TotalCacheRead) / float64(child.TotalInput+child.TotalCacheRead) * 100
				}
				d.Children = append(d.Children, child)
			}
		}
	}
	fillSessionStats(d)
	return d, rows.Err()
}

func ocStepCount(db *sql.DB, sessionID string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM part WHERE session_id = ? AND json_extract(data, '$.type') = 'step-finish'`, sessionID).Scan(&count)
	return count, err
}

func piSessionDetail(fp string) (*SessionDetail, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type piDetEntry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role     string  `json:"role"`
			Provider string  `json:"provider"`
			Model    string  `json:"model"`
			Usage    piUsage `json:"usage"`
			Content  []struct {
				Type              string          `json:"type"`
				Text              string          `json:"text"`
				Thinking          string          `json:"thinking"`
				ThinkingSignature string          `json:"thinkingSignature"`
				Name              string          `json:"name"`
				Arguments         json.RawMessage `json:"arguments"`
				ID                string          `json:"id"`
			} `json:"content"`
			StopReason string `json:"stopReason"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			IsError    bool   `json:"isError"`
		} `json:"message"`
	}

	var steps []StepInfo
	var title string
	var lastUserPrompt string
	toolCallTimes := make(map[string]time.Time) // toolCallId -> assistant msg timestamp
	parseTS := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02 15:04:05", s)
	}

	scanner := newJSONLScanner(f)

	for scanner.Scan() {
		var entry piDetEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "message" {
			continue
		}
		if entry.Message.Role == "user" {
			for _, c := range entry.Message.Content {
				if c.Type == "text" {
					t := strings.TrimSpace(c.Text)
					if t != "" {
						// store full prompt for the next assistant step
						lastUserPrompt = t
						// first user message also sets the session title
						if title == "" {
							title = truncate(t, 80)
						}
						break
					}
				}
			}
			continue
		}
		if entry.Message.Role == "toolResult" {
			var out json.RawMessage
			if len(entry.Message.Content) > 0 {
				if entry.Message.Content[0].Type == "text" {
					if b, err := json.Marshal(entry.Message.Content[0].Text); err == nil {
						out = json.RawMessage(b)
					}
				} else {
					b, _ := json.Marshal(entry.Message.Content)
					out = b
				}
			}
			tc := ToolCallInfo{
				ID:     entry.Message.ToolCallID,
				Name:   entry.Message.ToolName,
				Output: out,
				Error:  entry.Message.IsError,
			}
			// compute tool duration from recorded timestamps
			if reqTS, ok := toolCallTimes[entry.Message.ToolCallID]; ok {
				if respTS, err := parseTS(entry.Timestamp); err == nil {
					tc.DurationMs = respTS.Sub(reqTS).Milliseconds()
				}
			}
			delete(toolCallTimes, entry.Message.ToolCallID)
			for i := len(steps) - 1; i >= 0; i-- {
				matched := false
				for j := range steps[i].ToolCalls {
					if steps[i].ToolCalls[j].ID != tc.ID {
						continue
					}
					steps[i].ToolCalls[j].Output = tc.Output
					steps[i].ToolCalls[j].Error = tc.Error
					steps[i].ToolCalls[j].DurationMs = tc.DurationMs
					matched = true
					break
				}
				if matched {
					break
				}
			}
			continue
		}
		if entry.Message.Model == "" {
			continue
		}
		if entry.Message.Usage.Total == 0 {
			continue
		}

		assTS, _ := parseTS(entry.Timestamp)

		s := StepInfo{
			Step:       len(steps) + 1,
			Timestamp:  entry.Timestamp,
			Model:      entry.Message.Model,
			Input:      entry.Message.Usage.Input,
			Output:     entry.Message.Usage.Output,
			CacheRead:  entry.Message.Usage.CacheRead,
			CacheWrite: entry.Message.Usage.CacheWrite,
			Cost:       entry.Message.Usage.Cost.Total,
			StopReason: entry.Message.StopReason,
			UserPrompt: lastUserPrompt,
		}
		lastUserPrompt = "" // consumed
		// extract thinking, text and tool calls from assistant message content
		for _, c := range entry.Message.Content {
			switch c.Type {
			case "thinking":
				s.Thinking = c.Thinking
			case "text":
				s.Response = c.Text
			case "toolCall":
				s.ToolCalls = append(s.ToolCalls, ToolCallInfo{
					ID:    c.ID,
					Name:  c.Name,
					Input: c.Arguments,
				})
				// record tool call timestamp for duration calculation
				if c.ID != "" && !assTS.IsZero() {
					toolCallTimes[c.ID] = assTS
				}
			}
		}
		steps = append(steps, s)
	}

	birth := getCreatedAt(fp)
	sessID := piSessionIDFromPath(fp)
	projectDir := filepath.Base(filepath.Dir(fp))
	project := cleanProjectName(projectDir)
	// Subagent sessions inherit the parent's project — for the nested layout
	// the session file itself lives below the project directory, so the name
	// has to be taken from the parent's path.
	var parent *SessionLink
	if parentPath, parentID, ok := piResolveParent(fp); ok {
		projectDir = filepath.Base(filepath.Dir(parentPath))
		project = cleanProjectName(projectDir)
		parentTitle := project
		if parentData, err := piSessionUsage(parentPath); err == nil && parentData.Title != "" {
			parentTitle = parentData.Title
		}
		parent = &SessionLink{Agent: "PI", ID: parentID, Title: parentTitle, Project: project}
	}

	var totalCost float64
	for _, s := range steps {
		totalCost += s.Cost
	}

	d := &SessionDetail{
		Agent:     "PI",
		ID:        sessID,
		Title:     title,
		Project:   project,
		Date:      birth.UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	d.Parent = parent
	for _, childPath := range piSubsessionPaths(fp) {
		childDetail, err := piSessionDetail(childPath)
		if err != nil {
			continue
		}
		childTitle := childDetail.Title
		if childTitle == "" {
			childTitle = childDetail.Project
		}
		d.Children = append(d.Children, SessionLink{
			Agent:           "PI",
			ID:              childDetail.ID,
			Title:           childTitle,
			Project:         childDetail.Project,
			Model:           childDetail.Model,
			Date:            childDetail.Date,
			TotalInput:      childDetail.TotalInput,
			TotalOutput:     childDetail.TotalOutput,
			TotalCacheRead:  childDetail.TotalCacheRead,
			TotalCacheWrite: childDetail.TotalCacheWrite,
			CacheHitRate:    childDetail.CacheHitRate,
			Steps:           len(childDetail.Steps),
			TotalCost:       childDetail.TotalCost,
		})
	}
	sortChildrenByDate(d)
	fillSessionStats(d)
	return d, scanner.Err()
}

func claudeSessionDetail(fp string) (*SessionDetail, error) {
	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type claudeContentItem struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		ID    string          `json:"id"`
		Input json.RawMessage `json:"input"`
	}

	type claudeDetMsg struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
			Usage      struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				// CacheCreation splits the total above by TTL; only the 1h
				// slice is needed since 5m is whatever remains of the total.
				CacheCreation struct {
					Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		} `json:"message,omitempty"`
		Cwd string `json:"cwd"`
	}

	var project string
	var steps []StepInfo
	var lastUserPrompt string
	toolCallStart := make(map[string]time.Time) // tool_use_id -> assistant timestamp
	msgIndexByID := make(map[string]int)
	parseTS := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02 15:04:05", s)
	}

	scanner := newJSONLScanner(f)

	for scanner.Scan() {
		var msg claudeDetMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if project == "" && msg.Cwd != "" {
			project = filepath.Base(msg.Cwd)
		}

		// user messages: prompt text or tool results
		if msg.Type == "user" && msg.Message.Model == "" {
			if len(msg.Message.Content) > 0 && msg.Message.Content[0] == '[' {
				var items []struct {
					Type      string          `json:"type"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
					IsError   bool            `json:"is_error"`
				}
				if err := json.Unmarshal(msg.Message.Content, &items); err == nil {
					for _, item := range items {
						if item.Type != "tool_result" || item.ToolUseID == "" {
							continue
						}
						var startTS time.Time
						if ts, ok := toolCallStart[item.ToolUseID]; ok {
							startTS = ts
						}
						for i := len(steps) - 1; i >= 0; i-- {
							matched := false
							for j := range steps[i].ToolCalls {
								if steps[i].ToolCalls[j].ID != item.ToolUseID {
									continue
								}
								if len(item.Content) > 0 {
									steps[i].ToolCalls[j].Output = item.Content
								}
								steps[i].ToolCalls[j].Error = item.IsError
								if respTS, err := parseTS(msg.Timestamp); err == nil && !startTS.IsZero() {
									steps[i].ToolCalls[j].DurationMs = respTS.Sub(startTS).Milliseconds()
								}
								matched = true
								break
							}
							if matched {
								break
							}
						}
						delete(toolCallStart, item.ToolUseID)
					}
				}
			} else {
				var contentStr string
				if err := json.Unmarshal(msg.Message.Content, &contentStr); err == nil {
					t := strings.TrimSpace(contentStr)
					if t != "" {
						lastUserPrompt = t
					}
				}
			}
			continue
		}

		if msg.Type != "assistant" || msg.Message.Model == "" || msg.Message.ID == "" {
			continue
		}
		u := msg.Message.Usage
		if u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens+u.OutputTokens == 0 {
			continue
		}

		assTS, _ := parseTS(msg.Timestamp)
		var contentItems []claudeContentItem
		if len(msg.Message.Content) > 0 && msg.Message.Content[0] == '[' {
			json.Unmarshal(msg.Message.Content, &contentItems) // best-effort; nil on failure
		}

		idx, exists := msgIndexByID[msg.Message.ID]
		if !exists {
			prices := claudeGlobalModelPrices()[msg.Message.Model]
			cost := compute.PiStepActualCost(compute.StepData{
				Input:           u.InputTokens,
				CacheCreation:   u.CacheCreationInputTokens,
				CacheCreation1h: u.CacheCreation.Ephemeral1hInputTokens,
				CacheRead:       u.CacheReadInputTokens,
				Output:          u.OutputTokens,
			}, prices)
			steps = append(steps, StepInfo{
				Step:         len(steps) + 1,
				Timestamp:    msg.Timestamp,
				Model:        msg.Message.Model,
				Input:        u.InputTokens,
				Output:       u.OutputTokens,
				CacheRead:    u.CacheReadInputTokens,
				CacheWrite:   u.CacheCreationInputTokens,
				CacheWrite1h: u.CacheCreation.Ephemeral1hInputTokens,
				Cost:         cost,
				StopReason:   msg.Message.StopReason,
				UserPrompt:   lastUserPrompt,
			})
			lastUserPrompt = ""
			idx = len(steps) - 1
			msgIndexByID[msg.Message.ID] = idx
		}

		s := &steps[idx]
		if s.Timestamp == "" {
			s.Timestamp = msg.Timestamp
		}
		if s.Model == "" {
			s.Model = msg.Message.Model
		}
		if s.StopReason == "" {
			s.StopReason = msg.Message.StopReason
		}

		for _, c := range contentItems {
			switch c.Type {
			case "thinking":
				if s.Thinking == "" {
					s.Thinking = strings.TrimSpace(c.Text)
				}
			case "text":
				text := strings.TrimSpace(c.Text)
				if text != "" {
					if s.Response == "" {
						s.Response = text
					} else if !strings.Contains(s.Response, text) {
						s.Response += "\n\n" + text
					}
				}
			case "tool_use":
				tc := ToolCallInfo{Name: c.Name, ID: c.ID, Input: c.Input}
				s.ToolCalls = append(s.ToolCalls, tc)
				if c.ID != "" && !assTS.IsZero() {
					toolCallStart[c.ID] = assTS
				}
			}
		}
	}

	birth := getCreatedAt(fp)
	sessID := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
	var totalCost float64
	for _, s := range steps {
		totalCost += s.Cost
	}

	d := &SessionDetail{
		Agent:     "Claude",
		ID:        sessID,
		Project:   project,
		Date:      birth.UTC().Format("2006-01-02 15:04"),
		Steps:     steps,
		TotalCost: totalCost,
	}
	if strings.Contains(fp, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
		parentID := filepath.Base(filepath.Dir(filepath.Dir(fp)))
		if parentID != "" {
			d.Parent = &SessionLink{Agent: "Claude", ID: parentID, Project: project}
		}
	}
	baseDir := filepath.Dir(fp)
	subDir := filepath.Join(baseDir, sessID, "subagents")
	if subEntries, err := os.ReadDir(subDir); err == nil {
		for _, subEntry := range subEntries {
			if subEntry.IsDir() || filepath.Ext(subEntry.Name()) != ".jsonl" {
				continue
			}
			childID := strings.TrimSuffix(subEntry.Name(), ".jsonl")
			childTitle := childID
			if sf, err := os.Open(filepath.Join(subDir, subEntry.Name())); err == nil {
				scanner := newJSONLScanner(sf)
				if scanner.Scan() {
					var first struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &first); err == nil {
						if first.Message.Content != "" {
							childTitle = truncate(first.Message.Content, 90)
						}
					}
				}
				sf.Close()
			}
			child := SessionLink{Agent: "Claude", ID: childID, Title: childTitle, Project: project}
			childFP := filepath.Join(subDir, subEntry.Name())
			if childDetail, err := claudeSessionDetail(childFP); err == nil {
				child.Model = childDetail.Model
				child.TotalInput = childDetail.TotalInput
				child.TotalOutput = childDetail.TotalOutput
				child.TotalCacheRead = childDetail.TotalCacheRead
				child.TotalCacheWrite = childDetail.TotalCacheWrite
				child.CacheHitRate = childDetail.CacheHitRate
				child.Steps = len(childDetail.Steps)
				child.TotalCost = childDetail.TotalCost
				child.Date = childDetail.Date
			}
			d.Children = append(d.Children, child)
		}
	}
	sortChildrenByDate(d)
	fillSessionStats(d)
	return d, scanner.Err()
}
