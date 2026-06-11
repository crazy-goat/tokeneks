package main

import (
	"encoding/json"
	"os"
	"sync"
)

const defaultPIModels = "~/.pi/agent/models.json"

type piModelsFile struct {
	Providers map[string]struct {
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Cost struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cacheRead"`
				CacheWrite float64 `json:"cacheWrite"`
			} `json:"cost"`
		} `json:"models"`
	} `json:"providers"`
}

var (
	piModelPricesOnce sync.Once
	piModelPricesMap  map[string]ModelPrices
)

func piGlobalModelPrices() map[string]ModelPrices {
	piModelPricesOnce.Do(func() {
		piModelPricesMap = make(map[string]ModelPrices)

		modelsBytes, err := os.ReadFile(expandHome(defaultPIModels))
		if err != nil {
			return
		}

		var modelsFile piModelsFile
		if err := json.Unmarshal(modelsBytes, &modelsFile); err != nil {
			return
		}

		for _, provider := range modelsFile.Providers {
			for _, model := range provider.Models {
				prices := ModelPrices{
					Input:         model.Cost.Input,
					CacheCreation: model.Cost.CacheWrite,
					CacheRead:     model.Cost.CacheRead,
					Output:        model.Cost.Output,
				}
				if model.ID != "" {
					piModelPricesMap[model.ID] = prices
				}
				if model.Name != "" {
					piModelPricesMap[model.Name] = prices
				}
			}
		}
	})

	return piModelPricesMap
}

func piStepActualCost(step StepData, prices ModelPrices) float64 {
	return float64(step.Input)*prices.Input/tokensPerMillion +
		float64(step.CacheCreation)*prices.CacheCreation/tokensPerMillion +
		float64(step.CacheRead)*prices.CacheRead/tokensPerMillion +
		float64(step.Output)*prices.Output/tokensPerMillion
}
