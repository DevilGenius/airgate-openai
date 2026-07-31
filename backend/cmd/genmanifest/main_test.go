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
		{"gpt-5.6-luna", 0.25},
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
