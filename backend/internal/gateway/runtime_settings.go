package gateway

import (
	"context"
	"strconv"
)

// Runtime settings arrive through the SDK lifecycle contract. The gateway owns
// their meaning and implementation; Core does not call a provider-specific API.
func (g *OpenAIGateway) ApplyRuntimeSettings(_ context.Context, settings map[string]string) error {
	state := g.runtimeHash.State()
	for key, value := range settings {
		switch key {
		case "text_hash_enabled":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return err
			}
			state.TextEnabled = enabled
		case "image_hash_enabled":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return err
			}
			state.ImageEnabled = enabled
		}
	}
	g.runtimeHash.SetState(state)
	return nil
}
