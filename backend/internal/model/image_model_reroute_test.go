package model

import "testing"

func TestImageTwoPointFiveVariantsAreRegistered(t *testing.T) {
	want := map[string]string{
		"gpt-image-2.5-sunburst": "GPT Image 2.5 Sunburst",
		"gpt-image-2.5-flare":     "GPT Image 2.5 Flare",
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

func TestBareImageTwoPointFiveReroutesToSunburst(t *testing.T) {
	target, ok := RerouteTarget("  GPT-IMAGE-2.5  ")
	if !ok || target != "gpt-image-2.5-sunburst" {
		t.Fatalf("RerouteTarget(bare) = %q, %v", target, ok)
	}
	for _, untouched := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2", ""} {
		if got, ok := RerouteTarget(untouched); ok {
			t.Fatalf("RerouteTarget(%q) = %q, want no reroute", untouched, got)
		}
	}

	if got := CanonicalModel("gpt-image-2.5"); got != "gpt-image-2.5-sunburst" {
		t.Fatalf("CanonicalModel(bare) = %q", got)
	}
	if got := CanonicalModel(" gpt-image-2.5-flare "); got != "gpt-image-2.5-flare" {
		t.Fatalf("CanonicalModel(flare) = %q", got)
	}

	// 计价与已知性跟随重路由目标：裸名可用，且不会掉进 DefaultSpec 兜底价。
	bare := Lookup("gpt-image-2.5")
	sunburst := registry["gpt-image-2.5-sunburst"]
	if bare.Name != sunburst.Name || bare.InputPrice != sunburst.InputPrice || bare.OutputPrice != sunburst.OutputPrice || bare.ImagePrice != sunburst.ImagePrice {
		t.Fatalf("Lookup(gpt-image-2.5) = %#v, want sunburst spec", bare)
	}
	if !IsKnown("gpt-image-2.5") {
		t.Fatal("bare gpt-image-2.5 should be known through the reroute")
	}
	if !IsImageOnly("gpt-image-2.5") {
		t.Fatal("bare gpt-image-2.5 should stay image-only")
	}
}
