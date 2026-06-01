package gateway

import "testing"

func TestImageFallbackSizeForCommon4KResolutions(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"2880x2880", "2048x2048"},
		{"3456x2304", "2496x1664"},
		{"2304x3456", "1664x2496"},
		{"3840x2160", "2560x1440"},
		{"2160x3840", "1440x2560"},
		{"3200x2400", "2352x1760"},
		{"2400x3200", "1760x2352"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := imageFallbackSizeFor4K(tc.input)
			if !ok {
				t.Fatalf("imageFallbackSizeFor4K(%q) ok=false, want true", tc.input)
			}
			if got != tc.want {
				t.Fatalf("imageFallbackSizeFor4K(%q) = %q, want %q", tc.input, got, tc.want)
			}
			tier, _, ok := imageTierPriceForSize(got)
			if !ok || tier != "2k" {
				t.Fatalf("fallback size %q tier = (%q, %v), want 2k", got, tier, ok)
			}
			width, height, ok := parseImageSize(got)
			if !ok {
				t.Fatalf("fallback size %q should parse", got)
			}
			if width%imageDimensionMultiple != 0 || height%imageDimensionMultiple != 0 {
				t.Fatalf("fallback size %q dimensions must be multiples of %d", got, imageDimensionMultiple)
			}
			if err := validateImageSize(got, "gpt-image-2"); err != nil {
				t.Fatalf("fallback size %q should pass gpt-image-2 validation: %v", got, err)
			}
		})
	}
}
