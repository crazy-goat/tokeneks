package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"tokeneks/compute"
)

// defaultPISessions is a var, not a const, so tests can point it at a
// temporary directory instead of the real ~/.pi/agent/sessions.
var defaultPISessions = "~/.pi/agent/sessions"

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
	// CreatedAt is this step's own timestamp (ms epoch), needed to resolve a
	// derived PI rate at the window that was actually in effect when the
	// message ran — see sessionStep.CreatedAt (web_store.go) for the full
	// rationale. Also what lets piDetail/piList run the ideal-cache pass
	// once over the session's untouched step order and price each row at its
	// own model's rate afterward, instead of splitting by model first (see
	// computeSessionPricing's doc comment for why that inflates Ideal).
	CreatedAt int64
}

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
		ts, tsErr := parseTimestamp(entry.Timestamp)
		if tsErr == nil && ts.After(data.LastActivity) {
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
		createdAt := ts.UnixMilli()
		if tsErr != nil {
			// A message with no parseable timestamp still needs some "at" to
			// resolve a rate with; falling back to now is safer than the zero
			// Time's epoch-1 UnixMilli(), which would look like a message from
			// 1970 and could dodge every dated rate window.
			createdAt = time.Now().UnixMilli()
		}
		data.Steps = append(data.Steps, piSessionStep{Model: entry.Message.Model, Step: step, Cost: entry.Message.Usage.Cost.Total, CreatedAt: createdAt})
		if entry.Message.Provider != "" {
			data.ModelProviders[entry.Message.Model] = entry.Message.Provider
		}
		modelCounts[entry.Message.Model]++
	}

	data.DominantModel = dominantModel(modelCounts)

	return data, scanner.Err()
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

// piFilenameCreatedAt parses the UTC creation instant PI encodes at the
// start of every session filename — a JS Date.toISOString() with ':' and
// '.' swapped for '-' to stay filesystem-safe, e.g.
// "2026-05-11T21-07-38-405Z_<uuid>.jsonl" (confirmed against every
// file under a real ~/.pi/agent/sessions tree while fixing this). Returns
// false when name isn't in that exact 24-character shape — e.g. the bare
// "<date>_<id>" form fileDateFromFilename also accepts — since there is no
// time-of-day to recover from a date-only prefix.
func piFilenameCreatedAt(name string) (time.Time, bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	if strings.IndexByte(base, '_') != 24 || len(base) < 24 {
		return time.Time{}, false
	}
	ts := base[:24]
	if ts[10] != 'T' || ts[13] != '-' || ts[16] != '-' || ts[19] != '-' || ts[23] != 'Z' {
		return time.Time{}, false
	}
	rfc := ts[:13] + ":" + ts[14:16] + ":" + ts[17:19] + "." + ts[20:23] + "Z"
	t, err := time.Parse(time.RFC3339Nano, rfc)
	return t, err == nil
}

// piSessionLocalDate returns the local calendar date a PI session filename
// represents, for comparing against --date (a local calendar day the user
// typed, not a UTC one). PI's filename embeds the session's creation instant
// in UTC (see piFilenameCreatedAt); reading its date prefix as-is — what
// fileDateFromFilename does — is the UTC calendar day, and comparing that
// straight against a local --date is the same bug fixed in claudeSessions
// (claude.go) and piSubsessionCount below: it misfiles sessions created
// near local midnight into the wrong day. Falls back to the bare prefix
// when the filename isn't in the full-timestamp shape, since a date-only
// name carries no time-of-day to convert.
func piSessionLocalDate(name string) (string, bool) {
	if t, ok := piFilenameCreatedAt(name); ok {
		return t.Local().Format("2006-01-02"), true
	}
	return fileDateFromFilename(name)
}

func piSessions(days int, date string) ([]piSession, error) {
	baseDir := expandHome(defaultPISessions)
	cutoff := time.Now().AddDate(0, 0, -days)

	var sessions []piSession

	if err := walkSessionFiles(baseDir, func(fp string, info os.FileInfo) error {
		if date != "" {
			fdate, ok := piSessionLocalDate(filepath.Base(fp))
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
			Date:          func() string { d, _ := piSessionLocalDate(sessionName); return d }(),
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
			// date is the user-typed --date, a local calendar day — see
			// claudeSessions (claude.go) for the same fix and the reasoning:
			// formatting in UTC shifts the comparison by the machine's UTC
			// offset and misfiles sessions from around local midnight.
			return info.ModTime().Local().Format("2006-01-02") == date
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
		name = strings.TrimPrefix(name, prefix)
	}
	if name == "" {
		return "(root)"
	}
	return strings.ReplaceAll(name, "-", "/")
}

// piSessionNotFoundError marks a clean "no file matched" result from
// resolvePISessionPath's walk (neither a root session nor a subsession id
// matched), as opposed to the walk itself failing (a permissions problem, a
// corrupt directory, ...) — that case is returned as-is and must never be
// mistaken for this one. piDetail uses errors.As to route only this case to
// the store fallback (see piDetailFromStore) and lets every other error
// propagate untouched.
type piSessionNotFoundError struct {
	id string
}

func (e *piSessionNotFoundError) Error() string {
	return fmt.Sprintf("PI session not found: %s", e.id)
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
	return "", "", &piSessionNotFoundError{id: input}
}

func piDetail(input string, days int) error {
	fp, sessionID, err := resolvePISessionPath(input, days)
	if err != nil {
		var notFound *piSessionNotFoundError
		if errors.As(err, &notFound) {
			// The walk completed cleanly and matched neither a root session
			// nor a subsession id — the source JSONL may have been rotated
			// or deleted while the session stayed in the store (a real
			// cache since commit 2486511). Fall back to it rather than
			// failing outright. Any other error from resolvePISessionPath
			// (a walk failure, a permissions problem) skips this branch
			// entirely and is returned untouched below.
			return piDetailFromStore(notFound.id, err)
		}
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

	// The ideal-cache algorithm carries a model of the prompt cache forward
	// step to step; it is only correct run once over the session's full,
	// untouched step order. This used to split data.Steps by model first
	// (groupStepsByModel) and run the ideal pass per group, which resets that
	// carry-forward state at every model switch — inflating Ideal and, on a
	// real mixed-model session, producing the impossible Paid < Ideal. See
	// computeSessionPricing (web_store.go), the reference implementation this
	// now reuses so the CLI and the web dashboard can never disagree about a
	// session's numbers.
	spec := buildPricingSpecs()["pi"]

	steps := make([]sessionStep, len(data.Steps))
	for i, st := range data.Steps {
		steps[i] = sessionStep{Model: st.Model, LoggedCost: st.Cost, Data: st.Step, CreatedAt: st.CreatedAt}
	}

	printPIDetailTableAndTotal(steps, spec)

	return nil
}

// printPIDetailTableAndTotal renders the per-step table, TOTAL section and
// unpriced-model report shared by piDetail's file-backed path and its store
// fallback (piDetailFromStore) — by the time either calls this, they already
// have an identically-shaped []sessionStep in hand, and everything past that
// point is identical regardless of whether the steps came from the source
// JSONL or from the store.
func printPIDetailTableAndTotal(steps []sessionStep, spec agentPricingSpec) {
	tokens := make([]compute.StepData, len(steps))
	for i, s := range steps {
		tokens[i] = s.Data
	}

	var rows []compute.IdealRow
	if spec.ClaudeStyle {
		rows = compute.ComputeIdealClaude(tokens, compute.ModelPrices{})
	} else {
		rows = compute.ComputeIdeal(tokens)
	}

	// pricing[i] carries rows[i]'s own model's price, resolved at that
	// step's own timestamp, so a row is never priced at whichever model
	// happens to be dominant — see detailRowPrice's doc comment (algo.go).
	pricing := make([]detailRowPrice, len(steps))
	for i, s := range steps {
		prices, ok := spec.PriceFunc(s.Model, s.CreatedAt)
		pricing[i] = detailRowPrice{Model: s.Model, Prices: prices, Priced: ok, LoggedCost: s.LoggedCost}
	}

	if spec.ClaudeStyle {
		printDetailRowsClaude(rows, pricing)
	} else {
		printDetailRows(rows, pricing, false)
	}

	// Paid/Ideal for the TOTAL line, and the unpriced-model report, both come
	// from computeSessionPricing rather than from the printed rows above:
	// unlike the per-row display (which only falls back to a model's logged
	// cost when it has no resolvable rate at all), computeSessionPricing
	// always prefers PI's own logged cost as Paid when there is one — the
	// same preference totalsByAgent (main.go) uses for the `total` command,
	// so the TOTAL line here agrees with it.
	stepPrices := computeSessionPricing(steps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc)
	totalActual, totalIdeal := sumStepPricing(stepPrices)

	unpricedByModel := map[string]*unpricedModel{}
	for _, sp := range stepPrices {
		if sp.Priced {
			continue
		}
		u := unpricedByModel[sp.Model]
		if u == nil {
			u = &unpricedModel{Agent: "PI", Model: sp.Model}
			unpricedByModel[sp.Model] = u
		}
		u.Steps++
		u.Tokens += sp.Tokens
		u.LoggedCost += sp.LoggedCost
	}
	unpriced := make([]unpricedModel, 0, len(unpricedByModel))
	for _, u := range unpricedByModel {
		unpriced = append(unpriced, *u)
	}
	sort.Slice(unpriced, func(i, j int) bool { return unpriced[i].Model < unpriced[j].Model })

	totalOverpay, pctIdeal, _, _ := footerTotals(totalActual, totalIdeal, 0)

	fmt.Printf("\nTOTAL\n")
	fmt.Printf("Actual paid:  $%.2f\n", totalActual)
	fmt.Printf("Ideal paid:   $%.2f\n", totalIdeal)
	fmt.Printf("Overpay:      $%.2f (%.1f%% of ideal)\n", totalOverpay, pctIdeal)

	printUnpricedModels(unpriced)
}

// piDetailFromStore renders `pi detail` for a session whose source JSONL is
// no longer on disk but is still cached in the local store — every
// previously-ingested session is a real cache since 2486511 ("perf(ingest):
// use the store as a cache instead of re-parsing every session"), so a
// rotated or deleted source file no longer has to mean the session is gone.
//
// The per-step token/cost/model data the table below needs lives in the
// message table exactly as it would have come from the file (see
// store/messages.go's GetMessages and ingest_main.go's detailToStore, which
// is what put it there at ingest time), so this reproduces byte-for-byte
// what the file-backed path would have printed for the same session, with
// one visible difference: the "File:" line, since the store never recorded
// the file's own path, and an explicit note that the source is gone.
//
// notFound is resolvePISessionPath's original "session not found" error. It
// is returned unchanged when the store doesn't know the session either
// (sql.ErrNoRows) — never silently swallowed — and a genuine store error
// (corrupt db, open failure) is wrapped around it instead of being
// misreported as "not found".
func piDetailFromStore(sessionID string, notFound error) error {
	st, err := ensureDetailStore()
	if err != nil {
		return fmt.Errorf("%w (also failed to open the store to check for a cached copy: %v)", notFound, err)
	}

	ctx := context.Background()
	sess, err := st.GetSession(ctx, "pi", sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return notFound
		}
		return fmt.Errorf("checking store for PI session %s: %w", sessionID, err)
	}

	msgs, err := st.GetMessages(ctx, "pi", sessionID)
	if err != nil {
		return fmt.Errorf("reading store for PI session %s: %w", sessionID, err)
	}
	steps := stepsFromAssistantMessages(msgs)
	if len(steps) == 0 {
		return fmt.Errorf("PI session %s is in the store but has no assistant messages recorded", sessionID)
	}

	modelCounts := make(map[string]int, len(steps))
	for _, s := range steps {
		modelCounts[s.Model]++
	}
	dominant := dominantModel(modelCounts)

	fmt.Printf("Session:  %s\n", sessionID)
	fmt.Println("File:     (not found on disk)")
	fmt.Printf("Project:  %s\n", sess.Project)
	fmt.Printf("Messages: %d\n", len(steps))
	fmt.Printf("Model:    %s\n\n", dominant)
	fmt.Println("note: the source session file is missing from disk; the figures above and below are read from tokeneks' local store cache (as of this session's last ingest) instead of the raw JSONL.")
	fmt.Println()

	spec := buildPricingSpecs()["pi"]
	printPIDetailTableAndTotal(steps, spec)

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

	spec := buildPricingSpecs()["pi"]

	unpricedByModel := map[string]*unpricedModel{}
	for _, sess := range sessions {
		data := sess.Data
		if data == nil || len(data.Steps) == 0 {
			continue
		}

		// One continuous pass over this session's untouched step order, not
		// split by model first — see piDetail and computeSessionPricing
		// (web_store.go) for why splitting throws away the ideal-cache
		// carry-forward state at every model switch.
		steps := make([]sessionStep, len(data.Steps))
		var tokIn, tokCR, tokOut int
		for i, st := range data.Steps {
			steps[i] = sessionStep{Model: st.Model, LoggedCost: st.Cost, Data: st.Step, CreatedAt: st.CreatedAt}
			tokIn += st.Step.Input
			tokCR += st.Step.CacheRead
			tokOut += st.Step.Output
		}

		stepPrices := computeSessionPricing(steps, spec.ClaudeStyle, spec.CostIsLogged, spec.PriceFunc)
		actual, ideal := sumStepPricing(stepPrices)
		for _, sp := range stepPrices {
			if sp.Priced {
				continue
			}
			u := unpricedByModel[sp.Model]
			if u == nil {
				u = &unpricedModel{Agent: "PI", Model: sp.Model}
				unpricedByModel[sp.Model] = u
			}
			u.Steps++
			u.Tokens += sp.Tokens
			u.LoggedCost += sp.LoggedCost
		}

		overpay, pctIdeal, costPer1M, idealPer1M := footerTotals(actual, ideal, tokIn+tokCR+tokOut)

		totalActual += actual
		totalIdeal += ideal
		totalIn += tokIn
		totalCR += tokCR
		totalOut += tokOut

		timestamp := sess.Birth.UTC().Format("2006-01-02 15:04:05")
		tokens := tokIn + tokCR + tokOut

		fmt.Printf("%19s  %-36s  %-18.18s  %4d  %7s  %6.2f  %6.2f  %8.2f  %6.1f%%  %7.2f  %7.2f\n",
			timestamp, sess.ID, sess.DominantModel, sess.Msgs, formatTokens(tokens), actual, ideal, overpay, pctIdeal, costPer1M, idealPer1M)
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
