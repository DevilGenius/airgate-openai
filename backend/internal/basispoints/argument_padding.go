package basispoints

import "fmt"

// The native Office envelope has shallow JSON fields. Long whitespace outside
// its strings cannot add code or a reference. Bound that padding, not generation
// time or whitespace in the client's code. State is incremental and per item.
const maxNativeArgumentPadding = 4096
const maxPendingNativeArguments = 1024

type nativeArgumentPadding struct {
	inString bool
	escaped  bool
	padding  int
}

func (s *nativeArgumentPadding) consume(delta string) error {
	for i := 0; i < len(delta); i++ {
		c := delta[i]
		if s.inString {
			if s.escaped {
				s.escaped = false
			} else if c == '\\' {
				s.escaped = true
			} else if c == '"' {
				s.inString = false
			}
			continue
		}
		switch c {
		case ' ', '\t', '\r', '\n':
			s.padding++
			if s.padding > maxNativeArgumentPadding {
				return fmt.Errorf("basispoints native tool arguments exceeded the non-content JSON whitespace limit")
			}
		default:
			s.padding = 0
			s.inString = c == '"'
		}
	}
	return nil
}

type nativeArgumentPaddingGuard struct {
	items map[string]*nativeArgumentPadding
}

func (g *nativeArgumentPaddingGuard) observe(kind string, payload object) error {
	switch kind {
	case "response.output_item.added":
		item, _ := payload["item"].(object)
		if text(item["type"]) != "function_call" {
			return nil
		}
		name := text(item["name"])
		if name != "run_officejs" && name != "functions.run_officejs" {
			return nil
		}
		id := text(item["id"])
		if id == "" || g.items[id] != nil {
			return nil
		}
		if len(g.items) >= maxPendingNativeArguments {
			return fmt.Errorf("basispoints response contains too many pending native tool items")
		}
		if g.items == nil {
			g.items = make(map[string]*nativeArgumentPadding)
		}
		state := new(nativeArgumentPadding)
		if err := state.consume(text(item["arguments"])); err != nil {
			return err
		}
		g.items[id] = state
	case "response.function_call_arguments.delta":
		if state := g.items[text(payload["item_id"])]; state != nil {
			return state.consume(text(payload["delta"]))
		}
	case "response.output_item.done":
		item, _ := payload["item"].(object)
		delete(g.items, text(item["id"]))
	}
	return nil
}
