package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"
)

//go:embed web/index.html
var webIndexHTML []byte

type WebModelUsage struct {
	Model      string  `json:"model"`
	Provider   string  `json:"provider"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Cost       float64 `json:"cost"`
	Messages   int     `json:"messages"`
}

type WebSession struct {
	Agent           string          `json:"agent"`
	ID              string          `json:"id"`
	Date            string          `json:"date"`
	Project         string          `json:"project"`
	DominantModel   string          `json:"dominantModel"`
	LastMessage     string          `json:"lastMessage"`
	Models          []WebModelUsage `json:"models"`
	TotalInput      int             `json:"totalInput"`
	TotalOutput     int             `json:"totalOutput"`
	TotalCacheRead  int             `json:"totalCacheRead"`
	TotalCacheWrite int             `json:"totalCacheWrite"`
	TotalCost       float64         `json:"totalCost"`
	Messages        int             `json:"messages"`
	ToolCalls       int             `json:"toolCalls"`
	PromptInput     int             `json:"promptInput"`
	ParentID        string          `json:"parentId,omitempty"`
	ChildCount      int             `json:"childCount,omitempty"`
	IsSubsession    bool            `json:"isSubsession,omitempty"`
}

func gatherWebSessions(days int) ([]WebSession, error) {
	var result []WebSession

	// OpenCode
	ocSess, err := ocSessions(days, "")
	if err != nil {
		log.Printf("web aggregator: OpenCode sessions load failed: %v", err)
	} else {
		for _, sess := range ocSess {
			usage := WebModelUsage{
				Model:     sess.Model,
				Provider:  sess.Provider,
				Input:     sess.TokensInput,
				CacheRead: sess.TokensCacheRead,
				Output:    sess.TokensOutput,
				Messages:  sess.Steps,
				Cost:      sess.Cost,
			}
			toolCalls, _ := ocToolCalls(sess.ID)
			lastAt := time.Unix(sess.LastActivity/1000, 0).UTC().Format("2006-01-02 15:04:05")
			if sess.LastActivity == 0 {
				lastAt = time.Unix(sess.CreatedAt/1000, 0).UTC().Format("2006-01-02 15:04:05")
			}
			result = append(result, WebSession{
				Agent:           "OpenCode",
				ID:              sess.ID,
				Date:            time.Unix(sess.CreatedAt/1000, 0).UTC().Format("2006-01-02 15:04"),
				Project:         sess.Title,
				DominantModel:   sess.Model,
				LastMessage:     lastAt,
				Models:          []WebModelUsage{usage},
				TotalInput:      sess.TokensInput,
				PromptInput:     sess.TokensInput,
				TotalOutput:     sess.TokensOutput,
				TotalCacheRead:  sess.TokensCacheRead,
				TotalCacheWrite: sess.TokensCacheWrite,
				TotalCost:       sess.Cost,
				Messages:        sess.Steps,
				ToolCalls:       toolCalls,
				ParentID:        sess.ParentID,
				IsSubsession:    sess.ParentID != "",
			})
		}
	}

	// PI
	piSess, err := piSessions(days, "")
	if err != nil {
		log.Printf("web aggregator: PI sessions load failed: %v", err)
	} else {
		for _, sess := range piSess {
			data, err := piSessionUsage(sess.Filepath)
			if err != nil || len(data.Steps) == 0 {
				continue
			}
			byModel := make(map[string]*WebModelUsage)
			for _, step := range data.Steps {
				if _, ok := byModel[step.Model]; !ok {
					provider := data.ModelProviders[step.Model]
					byModel[step.Model] = &WebModelUsage{Model: step.Model, Provider: provider}
				}
				u := byModel[step.Model]
				u.Input += step.Step.Input
				u.CacheRead += step.Step.CacheRead
				u.CacheWrite += step.Step.CacheCreation
				u.Output += step.Step.Output
				u.Cost += step.Cost
				u.Messages++
			}
			var models []WebModelUsage
			var totalInput, totalPromptInput, totalOutput, totalCR, totalCW int
			var totalCost float64
			for _, u := range byModel {
				totalCost += u.Cost
				totalInput += u.Input
				totalPromptInput += u.Input + u.CacheWrite
				totalOutput += u.Output
				totalCR += u.CacheRead
				totalCW += u.CacheWrite
				models = append(models, *u)
			}
			sort.Slice(models, func(i, j int) bool {
				return models[i].Cost > models[j].Cost
			})
			lastAt := sess.LastActivity.UTC().Format("2006-01-02 15:04:05")
			if sess.LastActivity.IsZero() {
				lastAt = sess.Birth.UTC().Format("2006-01-02 15:04:05")
			}
			result = append(result, WebSession{
				Agent:           "PI",
				ID:              sess.ID,
				Date:            sess.Birth.UTC().Format("2006-01-02 15:04"),
				Project:         sess.Title,
				DominantModel:   sess.DominantModel,
				LastMessage:     lastAt,
				Models:          models,
				TotalInput:      totalInput,
				PromptInput:     totalPromptInput,
				TotalOutput:     totalOutput,
				TotalCacheRead:  totalCR,
				TotalCacheWrite: totalCW,
				TotalCost:       totalCost,
				Messages:        len(data.Steps),
				ToolCalls:       data.ToolCalls,
				ParentID:        sess.ParentID,
				ChildCount:      sess.ChildCount,
				IsSubsession:    sess.IsSubsession,
			})
			for _, childPath := range piSubsessionPaths(sess.Filepath) {
				childData, err := piSessionUsage(childPath)
				if err != nil || len(childData.Steps) == 0 {
					continue
				}
				childByModel := make(map[string]*WebModelUsage)
				for _, step := range childData.Steps {
					if _, ok := childByModel[step.Model]; !ok {
						provider := childData.ModelProviders[step.Model]
						childByModel[step.Model] = &WebModelUsage{Model: step.Model, Provider: provider}
					}
					u := childByModel[step.Model]
					u.Input += step.Step.Input
					u.CacheRead += step.Step.CacheRead
					u.CacheWrite += step.Step.CacheCreation
					u.Output += step.Step.Output
					u.Cost += step.Cost
					u.Messages++
				}
				var childModels []WebModelUsage
				var childInput, childPromptInput, childOutput, childCR, childCW int
				var childCost float64
				for _, u := range childByModel {
					childCost += u.Cost
					childInput += u.Input
					childPromptInput += u.Input + u.CacheWrite
					childOutput += u.Output
					childCR += u.CacheRead
					childCW += u.CacheWrite
					childModels = append(childModels, *u)
				}
				sort.Slice(childModels, func(i, j int) bool {
					return childModels[i].Cost > childModels[j].Cost
				})
				childBirth := getCreatedAt(childPath)
				childLastAt := childData.LastActivity.UTC().Format("2006-01-02 15:04:05")
				if childData.LastActivity.IsZero() {
					childLastAt = childBirth.UTC().Format("2006-01-02 15:04:05")
				}
				childTitle := childData.Title
				if childTitle == "" {
					childTitle = sess.Title
				}
				result = append(result, WebSession{
					Agent:           "PI",
					ID:              piSessionIDFromPath(childPath),
					Date:            childBirth.UTC().Format("2006-01-02 15:04"),
					Project:         childTitle,
					DominantModel:   childData.DominantModel,
					LastMessage:     childLastAt,
					Models:          childModels,
					TotalInput:      childInput,
					PromptInput:     childPromptInput,
					TotalOutput:     childOutput,
					TotalCacheRead:  childCR,
					TotalCacheWrite: childCW,
					TotalCost:       childCost,
					Messages:        len(childData.Steps),
					ToolCalls:       childData.ToolCalls,
					ParentID:        sess.ID,
					IsSubsession:    true,
				})
			}
		}
	}

	// Claude
	clSess, err := claudeSessions(days, "", "")
	if err != nil {
		log.Printf("web aggregator: Claude sessions load failed: %v", err)
	} else {
		for _, sess := range clSess {
			res, err := claudeMessages(sess.Filepath)
			if err != nil || len(res.Steps) == 0 {
				continue
			}
			byModel := make(map[string]*WebModelUsage)
			for _, step := range res.Steps {
				if _, ok := byModel[step.Model]; !ok {
					byModel[step.Model] = &WebModelUsage{Model: step.Model}
				}
				u := byModel[step.Model]
				u.Input += step.Step.Input
				u.CacheRead += step.Step.CacheRead
				u.CacheWrite += step.Step.CacheCreation
				u.Output += step.Step.Output
				u.Messages++
			}
			var models []WebModelUsage
			var totalInput, totalPromptInput, totalOutput, totalCR, totalCW int
			var totalCost float64
			for _, u := range byModel {
				prices := claudeGlobalModelPrices()[u.Model]
				u.Cost = piStepActualCost(StepData{Input: u.Input, CacheCreation: u.CacheWrite, CacheRead: u.CacheRead, Output: u.Output}, prices)
				totalCost += u.Cost
				totalInput += u.Input
				totalPromptInput += u.Input + u.CacheWrite
				totalOutput += u.Output
				totalCR += u.CacheRead
				totalCW += u.CacheWrite
				models = append(models, *u)
			}
			sort.Slice(models, func(i, j int) bool {
				return models[i].Cost > models[j].Cost
			})
			lastAt := sess.LastActivity.UTC().Format("2006-01-02 15:04:05")
			if sess.LastActivity.IsZero() {
				lastAt = sess.Birth.UTC().Format("2006-01-02 15:04:05")
			}
			result = append(result, WebSession{
				Agent:           "Claude",
				ID:              sess.ID,
				Date:            sess.Birth.UTC().Format("2006-01-02 15:04"),
				Project:         sess.Project,
				DominantModel:   sess.DominantModel,
				LastMessage:     lastAt,
				Models:          models,
				TotalInput:      totalInput,
				PromptInput:     totalPromptInput,
				TotalOutput:     totalOutput,
				TotalCacheRead:  totalCR,
				TotalCacheWrite: totalCW,
				TotalCost:       totalCost,
				Messages:        sess.Msgs,
				ToolCalls:       sess.ToolCalls,
				ChildCount:      sess.SubagentCount,
			})
		}
	}

	ocChildren := make(map[string]int)
	for i := range result {
		if result[i].Agent == "OpenCode" && result[i].ParentID != "" {
			ocChildren[result[i].ParentID]++
		}
	}
	for i := range result {
		if result[i].Agent == "OpenCode" {
			result[i].ChildCount += ocChildren[result[i].ID]
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Date > result[j].Date
	})

	return result, nil
}

//go:embed web/detail.html
var webDetailHTML []byte

func filterWebSessionsByDateRange(sessions []WebSession, start, end string) []WebSession {
	if start == "" && end == "" {
		return sessions
	}
	var startTime, endTime time.Time
	var err error
	if start != "" {
		startTime, err = time.Parse("2006-01-02", start)
		if err != nil {
			return sessions
		}
	}
	if end != "" {
		endTime, err = time.Parse("2006-01-02", end)
		if err != nil {
			return sessions
		}
		endTime = endTime.Add(24*time.Hour - time.Nanosecond)
	}
	filtered := make([]WebSession, 0, len(sessions))
	for _, s := range sessions {
		ts, err := time.Parse("2006-01-02 15:04", s.Date)
		if err != nil {
			continue
		}
		if !startTime.IsZero() && ts.Before(startTime) {
			continue
		}
		if !endTime.IsZero() && ts.After(endTime) {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func runWeb(port string, days int) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webIndexHTML)
	})

	mux.HandleFunc("/detail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webDetailHTML)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		end := r.URL.Query().Get("end")
		effectiveDays := days
		if start != "" {
			if startTime, err := time.Parse("2006-01-02", start); err == nil {
				effectiveDays = int(time.Since(startTime).Hours()/24) + 2
				if effectiveDays < 1 {
					effectiveDays = 1
				}
			}
		}
		sessions, err := gatherWebSessions(effectiveDays)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sessions = filterWebSessionsByDateRange(sessions, start, end)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		json.NewEncoder(w).Encode(sessions)
	})

	mux.HandleFunc("/api/session/", handleAPISessionDetail)

	fmt.Printf("Web dashboard running on http://localhost:%s\n", port)
	return http.ListenAndServe(":"+port, mux)
}
