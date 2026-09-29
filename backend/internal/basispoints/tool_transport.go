package basispoints

import (
	"fmt"
	"strings"
	"unicode"
)

func validTransportName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsSpace) < 0 && strings.IndexFunc(name, unicode.IsControl) < 0
}

// Raw CUSTOM inputs require an exact catalog reference. FUNCTION envelopes
// carry their own target; a supplied reference must agree with that target.
func transportReference(arguments object) (string, error) {
	value, exists := arguments["references"]
	if !exists {
		return "", nil
	}
	refs, ok := value.([]any)
	if !ok || len(refs) > 1 {
		return "", fmt.Errorf("basispoints transport references must contain exactly one catalog tool name")
	}
	if len(refs) == 0 {
		return "", nil
	}
	name, ok := refs[0].(string)
	if !ok || !validTransportName(name) {
		return "", fmt.Errorf("basispoints transport references must contain an exact nonempty catalog tool name")
	}
	return name, nil
}

// FUNCTION parameters live together in code's complete JSON envelope; CUSTOM
// input is opaque code. Neither summary field carries executable parameters.
func (b *Bridge) toolTransportEnvelope(arguments object) (object, error) {
	name, err := transportReference(arguments)
	if err != nil {
		return nil, err
	}
	if name != "" {
		info, ok := b.tools[name]
		if !ok {
			return nil, unknownClientToolError{}
		}
		if info.Kind == "custom" {
			input, ok := arguments["code"].(string)
			if !ok || len(input) > maxEnvelopeBytes {
				return nil, fmt.Errorf("basispoints custom transport code must be a bounded string")
			}
			return object{"name": name, "input": input}, nil
		}
	}
	envelope, err := decodeTransportEnvelope(arguments["code"])
	if err != nil {
		if recovered, ok := recoverTransportEnvelope(arguments["code"], b.tools); ok {
			envelope = recovered
		} else {
			return nil, fmt.Errorf("%w; FUNCTION requires all arguments in the code envelope; raw CUSTOM input requires references containing exactly one declared custom tool", err)
		}
	}
	target, err := envelopeName(envelope)
	if err != nil {
		return nil, err
	}
	if name != "" && target != name {
		return nil, fmt.Errorf("basispoints transport references conflict with the JSON envelope target")
	}
	return envelope, nil
}
