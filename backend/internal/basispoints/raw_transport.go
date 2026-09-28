package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

func supportsFunctionCodeTransport(name, kind string, parameters any) bool {
	if kind != "function" || !validTransportName(name) {
		return false
	}
	schema, _ := parameters.(object)
	properties, _ := schema["properties"].(object)
	code, _ := properties["code"].(object)
	return schema["type"] == "object" && code["type"] == "string" && requiredTransportField(schema, "code")
}

func supportsFunctionCmdTransport(name, kind string, parameters any) bool {
	if (name != "exec_command" && !strings.HasSuffix(name, ".exec_command")) || kind != "function" || !validTransportName(name) || supportsFunctionCodeTransport(name, kind, parameters) {
		return false
	}
	schema, _ := parameters.(object)
	properties, _ := schema["properties"].(object)
	cmd, _ := properties["cmd"].(object)
	return schema["type"] == "object" && cmd["type"] == "string" && requiredTransportField(schema, "cmd")
}

// Optional payload fields must use JSON envelopes, which can represent absence
// without inventing an empty command or mistaking an envelope for raw source.
func requiredTransportField(schema object, field string) bool {
	required, _ := schema["required"].([]any)
	for _, name := range required {
		if name == field {
			return true
		}
	}
	return false
}

func validTransportName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsSpace) < 0 && strings.IndexFunc(name, unicode.IsControl) < 0
}

// Routing comes only from an exact catalog reference, never from summary prose
// or executable text. A JSON envelope can carry its own target without references.
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

func (b *Bridge) rawTransportField(name string) string {
	info, ok := b.tools[name]
	if !ok {
		return ""
	}
	switch {
	case info.Kind == "custom":
		return "input"
	case supportsFunctionCodeTransport(name, info.Kind, info.Parameters):
		return "code"
	case supportsFunctionCmdTransport(name, info.Kind, info.Parameters):
		return "cmd"
	default:
		return ""
	}
}

// One catalog-derived decoder handles CUSTOM, FUNCTION_CODE and FUNCTION_CMD.
// Raw payloads are opaque, including payloads that happen to be valid JSON.
// Ordinary FUNCTION envelopes retain their explicit name/arguments representation.
func (b *Bridge) toolTransportEnvelope(arguments object) (object, string, error) {
	name, err := transportReference(arguments)
	if err != nil {
		return nil, "", err
	}
	if name != "" {
		if _, ok := b.tools[name]; !ok {
			return nil, "", unknownClientToolError{}
		}
	}
	field := b.rawTransportField(name)
	if field == "" {
		envelope, err := decodeTransportEnvelope(arguments["code"])
		if err != nil {
			if recovered, ok := recoverTransportEnvelope(arguments["code"], b.tools); ok {
				envelope = recovered
			} else {
				return nil, "", fmt.Errorf("%w; raw input requires references containing exactly one declared CUSTOM, FUNCTION_CODE or FUNCTION_CMD tool", err)
			}
		}
		target, err := envelopeName(envelope)
		if err != nil {
			return nil, "", err
		}
		if name != "" && target != name {
			return nil, "", fmt.Errorf("basispoints transport references conflict with the JSON envelope target")
		}
		return envelope, "", nil
	}
	code, ok := arguments["code"].(string)
	if !ok || len(code) > maxEnvelopeBytes {
		return nil, field, fmt.Errorf("basispoints raw transport code must be a bounded string")
	}
	if field == "input" {
		return object{"name": name, "input": code}, field, nil
	}
	metadata, ok := arguments["extended_summary"].(string)
	if !ok || len(metadata) > maxEnvelopeBytes-len(code) {
		return nil, field, fmt.Errorf("basispoints raw function transport requires bounded JSON arguments in extended_summary")
	}
	var args object
	if decode([]byte(metadata), &args) != nil || args == nil {
		return nil, field, fmt.Errorf("basispoints raw function transport extended_summary must contain one JSON object")
	}
	if duplicate, exists := args[field]; exists {
		if field != "cmd" || duplicate != code {
			return nil, field, fmt.Errorf("basispoints raw function transport has duplicate or conflicting payload in extended_summary")
		}
	}
	args[field] = code
	encoded, err := json.Marshal(args)
	if err != nil || len(encoded) > maxEnvelopeBytes {
		return nil, field, fmt.Errorf("basispoints raw function transport arguments exceed the size limit")
	}
	return object{"name": name, "arguments": args}, field, nil
}

func encodeRawFunctionTransport(name, field string, args object) (object, error) {
	metadata := make(object, len(args))
	for key, value := range args {
		if key != field {
			metadata[key] = value
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("basispoints history function metadata cannot be serialized")
	}
	return object{"summary": "Replay a previously requested client tool", "code": args[field],
		"extended_summary": string(encoded), "destructive": false, "references": []any{name}}, nil
}
