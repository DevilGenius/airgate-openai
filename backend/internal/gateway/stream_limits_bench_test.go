package gateway

import (
	"strings"
	"testing"
)

func BenchmarkParseSSEStreamLimits(b *testing.B) {
	delta := `data: {"type":"response.output_text.delta","item_id":"text","output_index":0,"content_index":0,"delta":"` + strings.Repeat("x", 64) + "\"}\n\n"
	image := `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"img","type":"image_generation_call","result":"` + strings.Repeat("A", 4<<20) + "\"}}\n\n"
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
	for _, test := range []struct{ name, stream string }{
		{"Text128Deltas", strings.Repeat(delta, 128) + terminal},
		{"Image4MiB", image + terminal},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.SetBytes(int64(len(test.stream)))
			b.ReportAllocs()
			for b.Loop() {
				if result := ParseSSEStream(strings.NewReader(test.stream), nil); result.Err != nil {
					b.Fatal(result.Err)
				}
			}
		})
	}
}
