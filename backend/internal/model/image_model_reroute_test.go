package model

import "testing"

func TestImageTwoPointFiveVariantsAreRegistered(t *testing.T) {
	want := map[string]string{
		"gpt-image-2.5-sunburst": "GPT Image 2.5 Sunburst",
		"gpt-image-2.5-flare":    "GPT Image 2.5 Flare",
	}
	for id, name := range want {
		spec, ok := registry[id]
		if !ok {
			t.Fatalf("registry missing %q", id)
		}
		if spec.Name != name {
			t.Fatalf("%s name = %q, want %q", id, spec.Name, name)
		}
		// 与 gpt-image-2 同价：input 5 / cached 0.5 / output 30（$/1M tokens），每张 $0.20。
		if spec.InputPrice != 5.0 || spec.CachedPrice != 0.5 || spec.OutputPrice != 30.0 || spec.ImagePrice != 0.20 {
			t.Fatalf("%s pricing = %#v, want gpt-image-2 pricing", id, spec)
		}
		if !IsKnown(id) {
			t.Fatalf("%s should be known", id)
		}
		if !IsImageOnly(id) {
			t.Fatalf("%s should be image-only", id)
		}
	}
}

func TestPricingLookupDoesNotRegisterOrRouteUnknownModels(t *testing.T) {
	if IsKnown("gpt-image-2.5") {
		t.Fatal("pricing registry must not resolve routing aliases")
	}
	if !IsImageOnly("gpt-image-2.5") {
		t.Fatal("image family pricing must remain available")
	}
	if Lookup("codex-auto-review") != Lookup(DefaultModelID) {
		t.Fatal("unknown model pricing fallback changed")
	}
}
