package gateway

import (
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

// Count wire bytes only. Buffer owners enforce their own retained-size limits;
// this layer must not parse deltas or keep a second copy of output-item state.
type streamResponseBudget struct {
	total int
	limit *responseLimit
}

func (b *streamResponseBudget) take(eventBytes, wireBytes int) error {
	if eventBytes > maxResponseEventBytes || wireBytes > b.limit.streamLimitBytes()-b.total {
		return b.exceeded()
	}
	b.total += wireBytes
	return nil
}

func (b *streamResponseBudget) exceeded() error {
	b.limit.trip()
	return errResponseTooLarge
}

// The common path needs only len: a smaller event cannot contain an oversized
// response. Inspect the JSON envelope only between the body and event limits.
func checkResponseEvent(data []byte) error {
	if len(data) <= sdk.MaxBufferedResponseBytes {
		return nil
	}
	if len(data) > maxResponseEventBytes || len(gjson.GetBytes(data, "response").Raw) > sdk.MaxBufferedResponseBytes {
		return errResponseTooLarge
	}
	return nil
}
