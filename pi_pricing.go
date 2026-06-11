package main

import (
	"encoding/json"
	"os"
	"sync"

	"tokeneks/compute"
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
	piModelPricesMap  map[string]compute.ModelPrices
)

func piGlobalModelPrices() map[string]compute.ModelPrices {
	piModelPricesOnce.Do(func() {
		piModelPricesMap = make(map[string]compute.ModelPrices)

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
				prices := compute.ModelPrices{
					Input:                 model.Cost.Input,
					Output:                model.Cost.Output,
					CacheRead:             model.Cost.CacheRead,
					CacheCreation:         model.Cost.CacheWrite,
					SupportsCacheCreation: model.Cost.CacheWrite > 0,
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
