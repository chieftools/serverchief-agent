package inventory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefreshStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "metadata-refresh.json")
	stamp, err := OpenRefreshStamp(path)
	if err != nil || !stamp.At().IsZero() {
		t.Fatalf("missing file: %v %v", stamp.At(), err)
	}
	at := time.Date(2026, 10, 24, 14, 0, 0, 999, time.FixedZone("CEST", 2*3600))
	if err := stamp.Record(at); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if !stamp.At().Equal(want) {
		t.Fatalf("At() = %s", stamp.At())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{\"refreshed_at\":\"2026-10-24T12:00:00Z\"}\n" {
		t.Fatalf("file = %q, %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	reopened, err := OpenRefreshStamp(path)
	if err != nil || !reopened.At().Equal(want) {
		t.Fatalf("reopened: %s %v", reopened.At(), err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if broken, err := OpenRefreshStamp(path); err == nil || !broken.At().IsZero() {
		t.Fatalf("corrupt file: %s %v", broken.At(), err)
	}
}
