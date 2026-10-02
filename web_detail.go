package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
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

// sessionRevision returns an opaque value that changes whenever the session's
// data does, so the SSE stream below can poll for "did this change?" without
// re-rendering the whole detail.
//
// It reads the store, not the source files. Each agent used to have its own
// file-based variant here (hashing the session's mtime/size plus every
// subagent file, or aggregating MAX(time_created)/COUNT(*) out of OpenCode's
// DB), but the store became the single read path for the web layer and
// nothing called them any more: the store's own ingest already advances a
// revision whenever it re-parses a changed source. Keeping three per-agent
// hashers alive next to the one function that actually runs only invited
// fixing a bug in the copy nobody executes.
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
	_, _ = fmt.Fprintf(w, "event: revision\ndata: %s\n\n", revision)
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
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
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
			_, _ = fmt.Fprintf(w, "event: revision\ndata: %s\n\n", revision)
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
	body, err := json.Marshal(detail)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(append(body, '\n'))
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
