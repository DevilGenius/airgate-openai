package gateway

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadAssetsFromDir(t *testing.T) {
	before := (&OpenAIGateway{}).GetWebAssets()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web", "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "dist", "index.js"), []byte("wrong project"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	after := (&OpenAIGateway{}).GetWebAssets()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("working directory changed immutable plugin assets")
	}
}

func TestGetWebAssetsFallsBackToEmbedded(t *testing.T) {
	assets := (&OpenAIGateway{logger: slog.Default()}).GetWebAssets()
	if len(assets) == 0 {
		t.Fatal("expected embedded web assets")
	}
	if len(assets["index.js"]) == 0 {
		t.Fatal("expected embedded index.js")
	}
}

func TestPersistencePureHelpers(t *testing.T) {
	if got := sessionPersistKey(" key "); got != "key" {
		t.Fatalf("sessionPersistKey = %q", got)
	}
	if got := nullableUTCTime(time.Time{}); got != nil {
		t.Fatalf("zero nullable time = %#v", got)
	}
	local := time.Date(2026, 6, 20, 1, 2, 3, 0, time.FixedZone("T", 8*3600))
	if got := nullableUTCTime(local).(time.Time); got.Location() != time.UTC || got.Hour() != 17 {
		t.Fatalf("nullable time should be UTC previous day, got %v", got)
	}

	if cloneCodexUsageSnapshot(nil) != nil {
		t.Fatal("nil snapshot clone should be nil")
	}
	snapshot := &CodexUsageSnapshot{LimitName: "spark"}
	cloned := cloneCodexUsageSnapshot(snapshot)
	if cloned == snapshot {
		t.Fatal("snapshot clone should allocate")
	}
	if cloned.LimitName != "spark" || cloned.CapturedAt.IsZero() {
		t.Fatalf("snapshot clone = %#v", cloned)
	}
}
