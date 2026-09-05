package model

import "testing"

// TestLookup_UnknownModelFallsBackToBillablePrice 回归测试：未注册模型必须走兜底定价，
// 绝不能返回 0 价（那会导致免费流量 / 计费全 0）。
func TestLookup_UnknownModelFallsBackToBillablePrice(t *testing.T) {
	spec := Lookup("some-brand-new-unknown-model-xyz")
	if spec.InputPrice <= 0 || spec.OutputPrice <= 0 {
		t.Fatalf("未知模型必须有兜底价格；InputPrice=%v OutputPrice=%v", spec.InputPrice, spec.OutputPrice)
	}
	if spec != Lookup("gpt-5.6-luna") {
		t.Fatalf("unknown model pricing = %+v, want GPT-5.6-Luna pricing", spec)
	}
}

// TestLookup_ByKeyword 按关键词推断到对应系列。
func TestLookup_ByKeyword(t *testing.T) {
	cases := []struct {
		name      string
		modelID   string
		wantMatch string // 期望命中的注册键（按 InputPrice 识别）
	}{
		{"未知 codex 系列 → gpt-5.3-codex-spark", "gpt-5.9-codex-preview", "gpt-5.3-codex-spark"},
		{"未知 image 系列 → gpt-image-2", "gpt-image-3", "gpt-image-2"},
		{"未知 mini 系列 → gpt-5.4-mini", "gpt-5.9-mini", "gpt-5.4-mini"},
		{"未知 nano 系列 → gpt-5.4-mini", "gpt-5.9-nano", "gpt-5.4-mini"},
		{"未知 gpt-5 系列 → gpt-5.6-luna", "gpt-5.9", "gpt-5.6-luna"},
		{"移除的 gpt-5.4 → gpt-5.6-luna", "gpt-5.4", "gpt-5.6-luna"},
		{"o1 推理模型 → gpt-5.6-luna", "o1-preview", "gpt-5.6-luna"},
		{"o3 mini 推理模型 → gpt-5.4-mini", "o3-mini", "gpt-5.4-mini"}, // "mini" 优先
		{"gpt-4 系列 → gpt-5.6-luna", "gpt-4o", "gpt-5.6-luna"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Lookup(tc.modelID)
			want := registry[tc.wantMatch]
			if got.InputPrice != want.InputPrice || got.OutputPrice != want.OutputPrice {
				t.Errorf("Lookup(%q): got InputPrice=%v OutputPrice=%v, want match %q (In=%v Out=%v)",
					tc.modelID, got.InputPrice, got.OutputPrice,
					tc.wantMatch, want.InputPrice, want.OutputPrice)
			}
		})
	}
}

// TestLookup_KnownModelUnchanged 已注册模型不受关键词兜底影响。
func TestLookup_KnownModelUnchanged(t *testing.T) {
	t.Run("gpt-6-astra", func(t *testing.T) {
		spec := Lookup("gpt-6-astra")
		if spec.InputPrice != 10.0 || spec.CachedPrice != 1.0 || spec.CacheCreationPrice != 12.5 || spec.OutputPrice != 50.0 {
			t.Errorf("gpt-6-astra 定价变化: In=%v Cached=%v CacheCreation=%v Out=%v", spec.InputPrice, spec.CachedPrice, spec.CacheCreationPrice, spec.OutputPrice)
		}
		if spec.ContextWindow != 1050000 {
			t.Errorf("gpt-6-astra ContextWindow = %v, want 1050000", spec.ContextWindow)
		}
		if spec.MaxOutputTokens != 128000 {
			t.Errorf("gpt-6-astra MaxOutputTokens = %v, want 128000", spec.MaxOutputTokens)
		}
		if spec.LongContextThreshold != 272000 {
			t.Errorf("gpt-6-astra LongContextThreshold = %v, want 272000", spec.LongContextThreshold)
		}
		if spec.LongContextInputMultiplier != 2 || spec.LongContextCachedMultiplier != 2 || spec.LongContextCacheCreationMultiplier != 2 || spec.LongContextOutputMultiplier != 1.5 {
			t.Errorf("gpt-6-astra 长上下文倍率变化: In=%v Cached=%v CacheCreation=%v Out=%v", spec.LongContextInputMultiplier, spec.LongContextCachedMultiplier, spec.LongContextCacheCreationMultiplier, spec.LongContextOutputMultiplier)
		}
	})

	t.Run("gpt-5.5", func(t *testing.T) {
		spec := Lookup("gpt-5.5")
		if spec.InputPrice != 5.0 || spec.OutputPrice != 30.0 || spec.CachedPrice != 0.5 {
			t.Errorf("gpt-5.5 定价变化: In=%v Out=%v Cached=%v", spec.InputPrice, spec.OutputPrice, spec.CachedPrice)
		}
		if spec.ContextWindow != 272000 {
			t.Errorf("gpt-5.5 ContextWindow = %v, want 272000", spec.ContextWindow)
		}
		if spec.InputPricePriority != 12.5 || spec.OutputPricePriority != 75.0 || spec.CachedPricePriority != 1.25 {
			t.Errorf("gpt-5.5 priority 定价变化: In=%v Out=%v Cached=%v", spec.InputPricePriority, spec.OutputPricePriority, spec.CachedPricePriority)
		}
		if spec.InputPriceFast != 0 || spec.OutputPriceFast != 0 || spec.CachedPriceFast != 0 {
			t.Errorf("gpt-5.5 不应配置 fast 定价: In=%v Out=%v Cached=%v", spec.InputPriceFast, spec.OutputPriceFast, spec.CachedPriceFast)
		}
	})

	t.Run("gpt-5.6", func(t *testing.T) {
		cases := []struct {
			model         string
			input         float64
			cached        float64
			cacheCreation float64
			output        float64
			contextWindow int
		}{
			{model: "gpt-5.6-sol", input: 5.0, cached: 0.5, cacheCreation: 6.25, output: 30.0, contextWindow: 1050000},
			{model: "gpt-5.6-terra", input: 2.0, cached: 0.2, cacheCreation: 2.5, output: 12.0, contextWindow: 372000},
			{model: "gpt-5.6-luna", input: 1.0, cached: 0.1, cacheCreation: 1.25, output: 6.0, contextWindow: 372000},
		}
		for _, tc := range cases {
			spec := Lookup(tc.model)
			if spec.InputPrice != tc.input || spec.CachedPrice != tc.cached || spec.CacheCreationPrice != tc.cacheCreation || spec.OutputPrice != tc.output {
				t.Errorf("%s 定价变化: In=%v Cached=%v CacheCreation=%v Out=%v", tc.model, spec.InputPrice, spec.CachedPrice, spec.CacheCreationPrice, spec.OutputPrice)
			}
			if spec.ContextWindow != tc.contextWindow {
				t.Errorf("%s ContextWindow = %v, want %v", tc.model, spec.ContextWindow, tc.contextWindow)
			}
			if spec.MaxOutputTokens != 128000 {
				t.Errorf("%s MaxOutputTokens = %v, want 128000", tc.model, spec.MaxOutputTokens)
			}
			if spec.LongContextThreshold != 272000 {
				t.Errorf("%s LongContextThreshold = %v, want 272000", tc.model, spec.LongContextThreshold)
			}
			if spec.LongContextInputMultiplier != 2 || spec.LongContextCachedMultiplier != 2 || spec.LongContextCacheCreationMultiplier != 2 || spec.LongContextOutputMultiplier != 1.5 {
				t.Errorf("%s 长上下文倍率变化: In=%v Cached=%v CacheCreation=%v Out=%v", tc.model, spec.LongContextInputMultiplier, spec.LongContextCachedMultiplier, spec.LongContextCacheCreationMultiplier, spec.LongContextOutputMultiplier)
			}
			if spec.CacheCreationPricePriority != tc.cacheCreation*2 || spec.CacheCreationPriceFlex != tc.cacheCreation*0.5 {
				t.Errorf("%s 缓存写入档位价格变化: Priority=%v Flex=%v", tc.model, spec.CacheCreationPricePriority, spec.CacheCreationPriceFlex)
			}
		}
	})

	t.Run("gpt-image-2", func(t *testing.T) {
		spec := Lookup("gpt-image-2")
		if spec.InputPrice != 5.0 || spec.OutputPrice != 30.0 || spec.CachedPrice != 0.5 {
			t.Errorf("gpt-image-2 token 定价变化: In=%v Out=%v Cached=%v", spec.InputPrice, spec.OutputPrice, spec.CachedPrice)
		}
		if spec.ImagePrice <= 0 {
			t.Errorf("gpt-image-2 ImagePrice = %v, want image capability marker", spec.ImagePrice)
		}
	})
}
