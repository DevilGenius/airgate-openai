package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/DevilGenius/airgate-openai/backend/internal/model"
)

func TestGeneratedManifestInSync(t *testing.T) {
	generated, err := renderManifest()
	if err != nil {
		t.Fatalf("生成 plugin.yaml 失败: %v", err)
	}

	manifestPath, err := manifestFilePath()
	if err != nil {
		t.Fatalf("定位 plugin.yaml 失败: %v", err)
	}

	current, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读取 plugin.yaml 失败: %v", err)
	}

	if !bytes.Equal(generated, current) {
		t.Fatalf("plugin.yaml 与运行时元信息不同步，请执行: go run ./cmd/genmanifest")
	}
}

func TestConvertModelsIncludesGPT6FullPricing(t *testing.T) {
	models := convertModels(model.AllPricingSpecs())
	byID := make(map[string]modelInfo, len(models))
	for _, item := range models {
		byID[item.ID] = item
	}

	cases := []struct {
		model         string
		contextWindow int
		input         float64
		cached        float64
		cacheCreation float64
		output        float64
	}{
		{"gpt-6-astra", 1050000, 10, 1, 12.5, 50},
		{"gpt-6-sol", 1050000, 2, 0.2, 2.5, 10},
		{"gpt-6-luna", 372000, 0.1, 0.01, 0.125, 0.5},
	}
	for _, tc := range cases {
		item, ok := byID[tc.model]
		if !ok {
			t.Fatalf("generated models missing %s", tc.model)
		}
		if item.ContextWindow != tc.contextWindow || item.MaxOutputTokens != 128000 {
			t.Errorf("%s token limits = (%d, %d), want (%d, 128000)", tc.model, item.ContextWindow, item.MaxOutputTokens, tc.contextWindow)
		}
		if item.InputPrice != tc.input || item.CachedInputPrice != tc.cached || item.CacheCreationPrice != tc.cacheCreation || item.OutputPrice != tc.output {
			t.Errorf("%s standard pricing = (%v, %v, %v, %v), want (%v, %v, %v, %v)",
				tc.model,
				item.InputPrice, item.CachedInputPrice, item.CacheCreationPrice, item.OutputPrice,
				tc.input, tc.cached, tc.cacheCreation, tc.output,
			)
		}
		if item.CacheCreationPricePriority != tc.cacheCreation*2 || item.CacheCreationPriceFlex != tc.cacheCreation*0.5 {
			t.Errorf("%s cache creation pricing = (%v, %v), want (%v, %v)",
				tc.model,
				item.CacheCreationPricePriority, item.CacheCreationPriceFlex,
				tc.cacheCreation*2, tc.cacheCreation*0.5,
			)
		}
		if item.LongContextThreshold != 272000 || item.LongContextInputMultiplier != 2 || item.LongContextCachedMultiplier != 2 || item.LongContextCacheCreationMultiplier != 2 || item.LongContextOutputMultiplier != 1.5 {
			t.Errorf("%s long context pricing incomplete: %+v", tc.model, item)
		}
	}
}

func TestConvertModelsIncludesGPT56FullPricing(t *testing.T) {
	models := convertModels(model.AllPricingSpecs())
	byID := make(map[string]modelInfo, len(models))
	for _, item := range models {
		byID[item.ID] = item
	}

	cases := []struct {
		model         string
		cacheCreation float64
	}{
		{"gpt-5.6-sol", 6.25},
		{"gpt-5.6-terra", 2.5},
		{"gpt-5.6-luna", 1.25},
	}
	for _, tc := range cases {
		item, ok := byID[tc.model]
		if !ok {
			t.Fatalf("generated models missing %s", tc.model)
		}
		if item.CacheCreationPrice != tc.cacheCreation || item.CacheCreationPricePriority != tc.cacheCreation*2 || item.CacheCreationPriceFlex != tc.cacheCreation*0.5 {
			t.Errorf("%s cache creation pricing = (%v, %v, %v), want (%v, %v, %v)",
				tc.model,
				item.CacheCreationPrice, item.CacheCreationPricePriority, item.CacheCreationPriceFlex,
				tc.cacheCreation, tc.cacheCreation*2, tc.cacheCreation*0.5,
			)
		}
		if item.LongContextThreshold != 272000 || item.LongContextInputMultiplier != 2 || item.LongContextCachedMultiplier != 2 || item.LongContextCacheCreationMultiplier != 2 || item.LongContextOutputMultiplier != 1.5 {
			t.Errorf("%s long context pricing incomplete: %+v", tc.model, item)
		}
	}
}
