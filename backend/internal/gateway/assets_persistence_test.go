package gateway

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAssetsFromDir(t *testing.T) {
	if got := loadAssetsFromDir(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing dir assets = %#v", got)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "style.css"), []byte("body{}"), 0o600); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	assets := loadAssetsFromDir(root)
	if string(assets["index.js"]) != "console.log(1)" {
		t.Fatalf("index asset = %q", assets["index.js"])
	}
	if string(assets["nested/style.css"]) != "body{}" {
		t.Fatalf("nested asset = %q", assets["nested/style.css"])
	}

	empty := t.TempDir()
	if got := loadAssetsFromDir(empty); got != nil {
		t.Fatalf("empty dir assets = %#v", got)
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
