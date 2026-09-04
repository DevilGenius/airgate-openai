package model

import (
	"sort"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func TestModelPredicates(t *testing.T) {
	if !IsKnown(" GPT-5.4 ") {
		t.Fatal("IsKnown should trim and normalize registered IDs")
	}
	if IsKnown("gpt-5.9") {
		t.Fatal("IsKnown should only report exact registry hits")
	}
	if IsKnown(" ") {
		t.Fatal("blank model should not be known")
	}
	for _, removed := range []string{"gpt-image-1", "gpt-image-1.5"} {
		if IsKnown(removed) {
			t.Fatalf("removed model %q should not be known", removed)
		}
	}

	if !IsImageOnly("gpt-image-2") {
		t.Fatal("gpt-image-2 should be image-only")
	}
	if !IsImageOnly("future-image-model") {
		t.Fatal("image keyword fallback should be image-only")
	}
	if IsImageOnly("gpt-5.4") {
		t.Fatal("chat model should not be image-only")
	}
}

func TestAllSpecsFilteringAndSorting(t *testing.T) {
	chatOnly := AllSpecs(false)
	if len(chatOnly) == 0 {
		t.Fatal("expected chat models")
	}
	assertSortedModelInfos(t, chatOnly)
	for _, m := range chatOnly {
		if (&m).HasCapability(sdk.ModelCapImageGeneration) {
			t.Fatalf("AllSpecs(false) included image model %q", m.ID)
		}
		if !(&m).HasCapability(sdk.ModelCapChat) || !(&m).HasCapability(sdk.ModelCapReasoning) {
			t.Fatalf("chat model %q missing chat/reasoning capabilities: %#v", m.ID, m.Capabilities)
		}
	}

	withImages := AllSpecs(true)
	if len(withImages) <= len(chatOnly) {
		t.Fatalf("AllSpecs(true) len=%d should exceed chat-only len=%d", len(withImages), len(chatOnly))
	}
	assertSortedModelInfos(t, withImages)
	foundImage := false
	for _, m := range withImages {
		if m.ID == "gpt-image-2" {
			foundImage = true
			if !(&m).HasCapability(sdk.ModelCapImageGeneration) {
				t.Fatalf("image model capabilities = %#v", m.Capabilities)
			}
			if (&m).HasCapability(sdk.ModelCapChat) {
				t.Fatalf("image model should not declare chat capability: %#v", m.Capabilities)
			}
		}
	}
	if !foundImage {
		t.Fatal("AllSpecs(true) should include gpt-image-2")
	}
}

func TestAllModelsAndPricingSpecs(t *testing.T) {
	models := AllModels()
	pricing := AllPricingSpecs()
	if len(models) != len(registry) {
		t.Fatalf("AllModels len=%d, want %d", len(models), len(registry))
	}
	if len(pricing) != len(registry) {
		t.Fatalf("AllPricingSpecs len=%d, want %d", len(pricing), len(registry))
	}
	assertSortedModelInfos(t, models)
	if !sort.SliceIsSorted(pricing, func(i, j int) bool { return pricing[i].ID < pricing[j].ID }) {
		t.Fatal("AllPricingSpecs should be sorted by ID")
	}

	for _, item := range pricing {
		spec, ok := registry[item.ID]
		if !ok {
			t.Fatalf("pricing item %q missing from registry", item.ID)
		}
		if item.Spec.Name != spec.Name || item.Spec.InputPrice != spec.InputPrice {
			t.Fatalf("pricing item %q does not preserve spec", item.ID)
		}
	}
}

func TestModelInfoMapping(t *testing.T) {
	image := toModelInfo("gpt-image-2", registry["gpt-image-2"])
	if image.Name == "" || image.ContextWindow == 0 {
		t.Fatalf("image model info missing fields: %#v", image)
	}
	if got := modelCapabilities(registry["gpt-image-2"]); len(got) != 1 || got[0] != sdk.ModelCapImageGeneration {
		t.Fatalf("image capabilities = %#v", got)
	}
	if got := modelCapabilities(registry["gpt-5.4"]); len(got) != 2 || got[0] != sdk.ModelCapChat || got[1] != sdk.ModelCapReasoning {
		t.Fatalf("chat capabilities = %#v", got)
	}
}

func assertSortedModelInfos(t *testing.T, models []sdk.ModelInfo) {
	t.Helper()
	if !sort.SliceIsSorted(models, func(i, j int) bool { return models[i].ID < models[j].ID }) {
		t.Fatalf("models not sorted by ID: %#v", models)
	}
}
