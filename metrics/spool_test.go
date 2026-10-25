package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSpool(t *testing.T, now time.Time) *Spool {
	t.Helper()
	return &Spool{Directory: filepath.Join(t.TempDir(), "metrics"), Now: func() time.Time { return now }}
}

func sampleAt(at int64) Sample {
	return Sample{At: at, Points: []Point{{Metric: "cpu.user", Value: 12.5}, {Metric: "fs.used_pct", Instance: "/", Value: 40}}}
}

func TestSpoolBatchesOldestSamplesAndResendsTheSameBatch(t *testing.T) {
	now := time.Unix(1793001600, 0)
	spool := newTestSpool(t, now)
	for minute := range 70 {
		if err := spool.Add(sampleAt(now.Unix() - int64(70-minute)*60)); err != nil {
			t.Fatal(err)
		}
	}

	first, err := spool.Next()
	if err != nil || first == nil {
		t.Fatalf("next = %v, %v", first, err)
	}
	var batch Batch
	if err := json.Unmarshal(first.Body, &batch); err != nil {
		t.Fatal(err)
	}
	if first.Samples != MaximumBatchSamples || batch.ProtocolVersion != 1 || batch.BatchID != first.ID || !validBatchID(first.ID) {
		t.Fatalf("batch %s with %d samples, protocol %d", first.ID, first.Samples, batch.ProtocolVersion)
	}
	if batch.Samples[0].At != now.Unix()-70*60 || batch.Samples[59].At != now.Unix()-11*60 {
		t.Fatalf("batch covers %d..%d, want the oldest 60", batch.Samples[0].At, batch.Samples[59].At)
	}

	// Until Done, the same bytes go out again, even after a restart.
	reopened := &Spool{Directory: spool.Directory, Now: spool.Now}
	again, err := reopened.Next()
	if err != nil || again.ID != first.ID || string(again.Body) != string(first.Body) {
		t.Fatalf("retry = %v, %v; want the same batch", again, err)
	}

	if err := spool.Done(); err != nil {
		t.Fatal(err)
	}
	rest, err := spool.Next()
	if err != nil || rest == nil || rest.Samples != 10 || rest.ID == first.ID {
		t.Fatalf("rest = %+v, %v", rest, err)
	}
	if err := spool.Done(); err != nil {
		t.Fatal(err)
	}
	if empty, err := spool.Next(); err != nil || empty != nil || spool.Bytes() != 0 {
		t.Fatalf("empty spool = %v, %v, %d bytes", empty, err, spool.Bytes())
	}
}

func TestSpoolDropsSamplesOlderThanADay(t *testing.T) {
	now := time.Unix(1793001600, 0)
	spool := newTestSpool(t, now)
	for _, at := range []int64{now.Unix() - 25*3600, now.Unix() - 23*3600, now.Unix() - 60} {
		if err := spool.Add(sampleAt(at)); err != nil {
			t.Fatal(err)
		}
	}

	prepared, _ := spool.Next()
	var batch Batch
	_ = json.Unmarshal(prepared.Body, &batch)
	if len(batch.Samples) != 2 || batch.Samples[0].At != now.Unix()-23*3600 {
		t.Fatalf("kept %d samples starting at %d", len(batch.Samples), batch.Samples[0].At)
	}
}

func TestSpoolStaysWithinItsSizeCap(t *testing.T) {
	now := time.Unix(1793001600, 0)
	spool := newTestSpool(t, now)
	large := Sample{Points: make([]Point, 2000)}
	for index := range large.Points {
		large.Points[index] = Point{Metric: "net.rx_bps", Instance: strings.Repeat("i", 20), Value: 123456.789}
	}
	for minute := range 120 {
		large.At = now.Unix() - int64(120-minute)*60
		if err := spool.Add(large); err != nil {
			t.Fatal(err)
		}
	}

	if size := spool.Bytes(); size > MaximumSpoolBytes {
		t.Fatalf("spool holds %d bytes, above %d", size, MaximumSpoolBytes)
	}
	names, _ := spool.sampleFiles()
	if len(names) == 0 || names[len(names)-1] != "00000000001793001540.json" {
		t.Fatalf("the newest sample must survive; kept %d, newest %v", len(names), names[len(names)-1:])
	}
}

func TestSpoolReplacesADamagedBatchAndClears(t *testing.T) {
	now := time.Unix(1793001600, 0)
	spool := newTestSpool(t, now)
	if err := spool.Add(sampleAt(now.Unix())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spool.batchPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	prepared, err := spool.Next()
	if err != nil || prepared == nil || prepared.Samples != 1 {
		t.Fatalf("next after a damaged batch = %+v, %v", prepared, err)
	}

	if err := spool.Add(sampleAt(now.Unix() + 60)); err != nil {
		t.Fatal(err)
	}
	if err := spool.Clear(); err != nil {
		t.Fatal(err)
	}
	if spool.Bytes() != 0 {
		t.Fatal("clear left samples behind")
	}
}
