package gateway

import (
	"encoding/json"
	"sort"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type completedResponseOutputItem struct {
	index int64
	id    string
	raw   json.RawMessage
}

// responsesOutputAccumulator retains complete output_item.done payloads. Codex
// can send an empty terminal output even after emitting reasoning ciphertext.
// The enclosing WS/SSE reader bounds these events with streamResponseBudget.
type responsesOutputAccumulator struct {
	items   []completedResponseOutputItem
	byID    map[string]int
	byIndex map[int64]int
}

func (a *responsesOutputAccumulator) apply(eventType string, data []byte) []byte {
	switch eventType {
	case "response.output_item.done":
		a.collect(data)
	case "response.completed", "response.done", "response.incomplete":
		return a.complete(data)
	}
	return data
}

func (a *responsesOutputAccumulator) collect(data []byte) {
	item := gjson.GetBytes(data, "item")
	if !item.IsObject() {
		return
	}
	entry := completedResponseOutputItem{index: -1, id: item.Get("id").String(), raw: json.RawMessage(item.Raw)}
	if index := gjson.GetBytes(data, "output_index"); index.Type == gjson.Number && index.Int() >= 0 {
		entry.index = index.Int()
	}
	if a.byID == nil {
		a.byID = make(map[string]int)
		a.byIndex = make(map[int64]int)
	}
	position, exists := a.byID[entry.id]
	if !exists && entry.index >= 0 {
		position, exists = a.byIndex[entry.index]
	}
	if exists {
		previous := a.items[position]
		delete(a.byID, previous.id)
		delete(a.byIndex, previous.index)
		a.items[position] = entry
	} else {
		position = len(a.items)
		a.items = append(a.items, entry)
	}
	if entry.id != "" {
		a.byID[entry.id] = position
	}
	if entry.index >= 0 {
		a.byIndex[entry.index] = position
	}
}

func (a *responsesOutputAccumulator) complete(data []byte) []byte {
	if len(a.items) == 0 || !gjson.GetBytes(data, "response").IsObject() {
		return data
	}
	snapshot := gjson.GetBytes(data, "response.output").Array()
	output := make([]json.RawMessage, len(snapshot))
	byID := make(map[string]int, len(snapshot))
	for index, item := range snapshot {
		output[index] = json.RawMessage(item.Raw)
		if id := item.Get("id").String(); id != "" {
			byID[id] = index
		}
	}
	var missing []completedResponseOutputItem
	changed := false
	for _, item := range a.items {
		position, exists := byID[item.id]
		// ID-less items can only be reconciled by index and type. Never replace
		// a different identified item when a partial snapshot has shifted slots.
		if !exists && item.index >= 0 && item.index < int64(len(snapshot)) {
			candidate := snapshot[item.index]
			if (item.id == "" || candidate.Get("id").String() == "") &&
				candidate.Get("type").String() == gjson.GetBytes(item.raw, "type").String() {
				position, exists = int(item.index), true
			}
		}
		if !exists {
			missing = append(missing, item)
			continue
		}
		if patched := mergeCompletedResponseItem(output[position], item.raw); patched != nil {
			output[position] = patched
			changed = true
		}
	}
	if !changed && len(missing) == 0 {
		return data
	}
	sort.SliceStable(missing, func(i, j int) bool {
		if missing[i].index < 0 {
			return false
		}
		return missing[j].index < 0 || missing[i].index < missing[j].index
	})
	merged := make([]json.RawMessage, 0, len(output)+len(missing))
	position := 0
	for _, item := range missing {
		for position < len(output) && (item.index < 0 || int64(len(merged)) < item.index) {
			merged = append(merged, output[position])
			position++
		}
		merged = append(merged, item.raw)
	}
	merged = append(merged, output[position:]...)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return data
	}
	patched, err := sjson.SetRawBytes(data, "response.output", encoded)
	if err != nil {
		return data
	}
	return patched
}

// The terminal snapshot wins for existing fields. Only restore missing fields
// (or empty ciphertext) from the completed item; leave encrypted bytes opaque.
func mergeCompletedResponseItem(snapshot, completed json.RawMessage) json.RawMessage {
	var fields, source map[string]json.RawMessage
	if json.Unmarshal(snapshot, &fields) != nil || fields == nil || json.Unmarshal(completed, &source) != nil {
		return nil
	}
	changed := false
	for key, value := range source {
		current, exists := fields[key]
		if exists && (key != "encrypted_content" || gjson.ParseBytes(current).String() != "" || gjson.ParseBytes(value).String() == "") {
			continue
		}
		fields[key] = value
		changed = true
	}
	if !changed {
		return nil
	}
	patched, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return patched
}
