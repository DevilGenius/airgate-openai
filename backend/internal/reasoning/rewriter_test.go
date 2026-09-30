package reasoning

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRewriterContract(t *testing.T) {
	policies := []struct {
		name     string
		rewriter Rewriter
	}{
		{"oauth", OAuth{}},
		{"oauth max", OAuth{Max: true}},
		{"oauth ultra", OAuth{Max: true, Ultra: true}},
		{"basispoints", Basispoints{}},
	}
	for _, tc := range []struct {
		input string
		want  [4]string
	}{
		{"", [4]string{"", "", "", "medium"}},
		{"low", [4]string{"low", "low", "low", "low"}},
		{" HIGH ", [4]string{"high", "high", "high", "high"}},
		{" normal ", [4]string{"medium", "medium", "medium", "medium"}},
		{"mid", [4]string{"medium", "medium", "medium", "medium"}},
		{"default", [4]string{"medium", "medium", "medium", "medium"}},
		{"x-high", [4]string{"xhigh", "xhigh", "xhigh", "xhigh"}},
		{"maximum", [4]string{"xhigh", "max", "max", "xhigh"}},
		{" ULTRA ", [4]string{"xhigh", "max", "ultra", "xhigh"}},
		{"none", [4]string{"none", "none", "none", "low"}},
		{"minimal", [4]string{"none", "none", "none", "low"}},
		{"off", [4]string{"none", "none", "none", "low"}},
	} {
		for i, policy := range policies {
			t.Run(policy.name+"/"+tc.input, func(t *testing.T) {
				got, err := policy.rewriter.Rewrite(tc.input)
				if err != nil || got != tc.want[i] {
					t.Fatalf("Rewrite(%q) = %q, %v; want %q", tc.input, got, err, tc.want[i])
				}
			})
		}
	}
	if got, err := (OAuth{}).Rewrite("future-effort"); err != nil || got != "future-effort" {
		t.Fatalf("native validation policy changed: %q / %v", got, err)
	}
	if _, err := (Basispoints{}).Rewrite("future-effort"); err == nil {
		t.Fatal("unsupported BAS effort accepted")
	}
}

func TestRewriteFieldsPreservesOriginalBodyAndNonEffortData(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rewriter Rewriter
		want     string
	}{
		{"oauth", OAuth{Max: true}, "max"},
		{"basispoints", Basispoints{}, "xhigh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"reasoning":{"effort":" ULTRA "},"reasoning_effort":"maximum","output_config":{"effort":"max"},"input":"keep exact text","large":9007199254740993,"enabled":false,"nullable":null}`)
			before := bytes.Clone(body)
			result, err := RewriteFields(body, tc.rewriter, "reasoning.effort", "reasoning_effort", "output_config.effort")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, before) {
				t.Fatal("caller request was mutated")
			}
			for _, path := range []string{"reasoning.effort", "reasoning_effort", "output_config.effort"} {
				if got := gjson.GetBytes(result, path).String(); got != tc.want {
					t.Fatalf("%s = %q, want %q", path, got, tc.want)
				}
			}
			for _, path := range []string{"input", "large", "enabled", "nullable"} {
				if gjson.GetBytes(result, path).Raw != gjson.GetBytes(before, path).Raw {
					t.Fatalf("non-effort field changed: %s", path)
				}
			}
		})
	}
}

func TestRewriteFieldsMissingAndRejectedEffort(t *testing.T) {
	body := []byte(`{"input":"hello"}`)
	result, err := RewriteFields(body, Basispoints{}, "reasoning.effort")
	if err != nil || !bytes.Equal(result, body) {
		t.Fatalf("missing field was injected: %s / %v", result, err)
	}
	body = []byte(`{"reasoning":{"effort":"ultra"},"reasoning_effort":"invalid"}`)
	before := bytes.Clone(body)
	result, err = RewriteFields(body, Basispoints{}, "reasoning.effort", "reasoning_effort")
	if err == nil || result != nil || !bytes.Equal(body, before) {
		t.Fatalf("failed rewrite exposed a partial result: %s / %v", result, err)
	}
}
