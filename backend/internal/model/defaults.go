package model

// Canonical IDs identify model specifications independently of routing roles.
const (
	GPT6Astra  = "gpt-6-astra"
	GPT56Sol   = "gpt-5.6-sol"
	GPT56Terra = "gpt-5.6-terra"
	GPT56Luna  = "gpt-5.6-luna"
)

// Built-in routing defaults live here. Changing a role does not change the
// corresponding model's registered identity or pricing.
const (
	DefaultModelID       = GPT56Sol
	DefaultFableModelID  = GPT6Astra
	DefaultOpusModelID   = GPT56Sol
	DefaultSonnetModelID = GPT56Terra
	DefaultHaikuModelID  = GPT56Luna
)
