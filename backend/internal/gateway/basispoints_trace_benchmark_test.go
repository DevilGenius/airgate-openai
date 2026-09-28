package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

// Exercises the real plugin Forward path against the local TLS BPS fixture.
// It never uses real upstream credentials, the running dev server, or billing.
func BenchmarkBasispointsTraceForward(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		for _, failure := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				b.Run(fmt.Sprintf("%dKiB/failure=%t/trace=%t", size>>10, failure, enabled), func(b *testing.B) {
					req := bpsRequest()
					req.TraceFinalError = enabled
					req.Body, _ = json.Marshal(map[string]any{"model": req.Model, "input": []any{map[string]any{"role": "user", "content": strings.Repeat("history context ", size/16)}}})
					req.Headers.Set("Content-Type", "application/json")
					g := bpsGateway(b, req, func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if failure {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(422)
							_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"fixture parameter error"}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, bpsCompleted)
					})
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					g.logger = logger
					ctx := sdk.WithLogger(context.Background(), logger)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						copy := *req
						copy.Headers = req.Headers.Clone()
						outcome, err := g.Forward(ctx, &copy)
						if err != nil {
							b.Fatal(err)
						}
						if failure && outcome.Kind != sdk.OutcomeClientError || !failure && outcome.Kind != sdk.OutcomeSuccess {
							b.Fatalf("unexpected outcome %s", outcome.Kind)
						}
						if failure && enabled && outcome.FinalErrorDiagnostic == nil {
							b.Fatal("missing trace")
						}
					}
				})
			}
		}
	}
}
