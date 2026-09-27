package basispoints

// Discovery annotations may change between the current catalog and historical
// additional_tools. Compare the execution contract and retain the first catalog
// entry. Reuse collectTools' resolved parameters to keep schema interpretation
// and duplicate detection identical; unknown execution constraints still count.
func toolDefinitionFingerprint(item object, parameters any) string {
	definition := make(object, len(item))
	function := text(item["type"]) == "function"
	for field, value := range item {
		switch field {
		case "description", "defer_loading":
			continue
		case "parameters", "inputSchema", "input_schema":
			if function {
				continue
			}
		}
		definition[field] = value
	}
	if function && parameters != nil {
		definition["parameters"] = parameters
	}
	return fingerprint(definition)
}
