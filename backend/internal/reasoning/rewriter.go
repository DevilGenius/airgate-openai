// Package reasoning separates client effort normalization from upstream limits.
package reasoning

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Rewriter maps a requested effort to one transport's wire value.
// Its output must not replace the original request recorded by Core.
type Rewriter interface {
	Rewrite(requested string) (string, error)
}

// Normalize resolves spelling aliases without applying a model or transport cap.
// Unknown values remain available for the upstream-specific validation policy.
func Normalize(requested string) string {
	trimmed := strings.TrimSpace(requested)
	key := strings.ToLower(trimmed)
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, " ", "")
	switch key {
	case "none", "off", "disabled":
		return "none"
	case "minimal", "min":
		return "minimal"
	case "low", "medium", "high", "max", "ultra":
		return key
	case "mid", "normal", "default":
		return "medium"
	case "xhigh", "extrahigh", "veryhigh":
		return "xhigh"
	case "maximum":
		return "max"
	default:
		return trimmed
	}
}

// Requested selects the first nonblank string without changing its spelling.
// Responses effort takes precedence over Chat and Anthropic compatibility fields.
func Requested(body []byte) string {
	for _, path := range []string{"reasoning.effort", "reasoning_effort", "output_config.effort"} {
		node := gjson.GetBytes(body, path)
		if node.Type == gjson.String && strings.TrimSpace(node.String()) != "" {
			return node.String()
		}
	}
	return ""
}

// OAuth retains the model capabilities selected by the gateway. Unknown values
// continue to reach native upstream validation, matching the existing policy.
type OAuth struct {
	Max   bool
	Ultra bool
}

func (r OAuth) Rewrite(requested string) (string, error) {
	switch effort := Normalize(requested); effort {
	case "minimal":
		return "none", nil
	case "max":
		if r.Max {
			return effort, nil
		}
		return "xhigh", nil
	case "ultra":
		if r.Ultra {
			return effort, nil
		}
		if r.Max {
			return "max", nil
		}
		return "xhigh", nil
	default:
		return effort, nil
	}
}

// Basispoints accepts only the BAS wire levels verified for the Excel profile.
type Basispoints struct{}

func (Basispoints) Rewrite(requested string) (string, error) {
	switch effort := Normalize(requested); effort {
	case "", "medium":
		return "medium", nil
	case "none", "minimal":
		return "low", nil
	case "low", "high":
		return effort, nil
	case "xhigh", "max", "ultra":
		return "xhigh", nil
	default:
		return "", fmt.Errorf("basispoints reasoning effort %q is unsupported", requested)
	}
}

var (
	_ Rewriter = OAuth{}
	_ Rewriter = Basispoints{}
)

// RewriteFields changes explicit string fields without injecting absent values
// or mutating the caller's body. BAS selects its one wire field during Prepare.
func RewriteFields(body []byte, rewriter Rewriter, paths ...string) ([]byte, error) {
	result := body
	for _, path := range paths {
		node := gjson.GetBytes(result, path)
		if !node.Exists() || node.Type != gjson.String {
			continue
		}
		effort, err := rewriter.Rewrite(node.String())
		if err != nil {
			return nil, err
		}
		if effort == "" || effort == node.String() {
			continue
		}
		result, err = sjson.SetBytes(result, path, effort)
		if err != nil {
			return nil, fmt.Errorf("rewrite reasoning effort at %s: %w", path, err)
		}
	}
	return result, nil
}
