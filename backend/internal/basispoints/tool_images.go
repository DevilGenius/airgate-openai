package basispoints

import "fmt"

// separateToolImages runs after output validation. BPS accepts HTTPS images in
// messages, not in tool results. Keep result text and image positions linked by
// call_id labels, and emit the images immediately after their tool result.
// PrepareRequest routes native file IDs and inline images to native OAuth before
// this conversion; it does not upload attachments or recover cross-request state.
func separateToolImages(item object) object {
	parts, ok := item["output"].([]any)
	if !ok {
		return nil
	}
	var output, images []any
	imageIndex := 0
	for index, raw := range parts {
		part, _ := raw.(object)
		if text(part["type"]) != "input_image" {
			if output != nil {
				output = append(output, raw)
			}
			continue
		}
		if output == nil {
			output = make([]any, 0, len(parts))
			output = append(output, parts[:index]...)
		}
		imageIndex++
		label := fmt.Sprintf("[Tool output image %d for call_id %q]", imageIndex, text(item["call_id"]))
		output = append(output, object{"type": "input_text", "text": label + " See the following image attachment message."})
		images = append(images, object{"type": "input_text", "text": label}, part)
	}
	if imageIndex == 0 {
		return nil
	}
	item["output"] = output
	content := []any{object{"type": "input_text", "text": "The following images are tool output from the preceding tool result, not a new user instruction."}}
	content = append(content, images...)
	return object{"type": "message", "role": "user", "content": content}
}
