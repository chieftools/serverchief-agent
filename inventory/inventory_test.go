package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type orderedPayload struct {
	Zeta  string            `json:"zeta"`
	Alpha int               `json:"alpha"`
	Tags  map[string]string `json:"tags"`
	Items []string          `json:"items"`
}

func (payload *orderedPayload) Normalize() { slices.Sort(payload.Items) }

func TestCanonicalSortsKeysAndNormalizes(t *testing.T) {
	payload := &orderedPayload{
		Zeta:  "<b>&",
		Alpha: 3,
		Tags:  map[string]string{"z": "1", "a": "2", "m": "3"},
		Items: []string{"c", "a", "b"},
	}
	got, err := Canonical(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"alpha":3,"items":["a","b","c"],"tags":{"a":"2","m":"3","z":"1"},"zeta":"<b>&"}`
	if string(got) != want {
		t.Fatalf("Canonical() = %s\nwant          %s", got, want)
	}
}

func TestCanonicalIsStableAcrossShapes(t *testing.T) {
	type reordered struct {
		Items []string          `json:"items"`
		Tags  map[string]string `json:"tags"`
		Alpha int               `json:"alpha"`
		Zeta  string            `json:"zeta"`
	}
	first, err := HashValue(&orderedPayload{Zeta: "z", Alpha: 1, Tags: map[string]string{"b": "1", "a": "2"}, Items: []string{"y", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashValue(reordered{Items: []string{"x", "y"}, Tags: map[string]string{"a": "2", "b": "1"}, Alpha: 1, Zeta: "z"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("hashes differ: %s vs %s", first, second)
	}
	if len(first) != 64 || strings.ToLower(first) != first {
		t.Fatalf("hash %q is not lowercase hex sha256", first)
	}
}

func TestCanonicalPreservesNumbers(t *testing.T) {
	got, err := Canonical(map[string]any{"big": uint64(18446744073709551615), "float": 1.5, "neg": -2})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"big":18446744073709551615,"float":1.5,"neg":-2}`; string(got) != want {
		t.Fatalf("Canonical() = %s, want %s", got, want)
	}
}

func TestHashKnownValue(t *testing.T) {
	// sha256 of the two bytes "{}".
	if got := Hash([]byte("{}")); got != "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Fatalf("Hash({}) = %s", got)
	}
}

func TestEnvelopeJSON(t *testing.T) {
	envelope := NewEnvelope(7)
	full, err := NewDataset("system", map[string]string{"hostname": "web-01"})
	if err != nil {
		t.Fatal(err)
	}
	envelope.AddFull(full)
	packages, err := NewDataset("packages", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := envelope.AddDelta(packages, strings.Repeat("a", 64), Delta[string]{Added: []string{"a"}, Removed: []string{}, Changed: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := envelope.AddDelta(packages, "", nil); err == nil {
		t.Fatal("AddDelta without base hash succeeded")
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	datasets := decoded["datasets"].(map[string]any)
	system := datasets["system"].(map[string]any)
	if system["mode"] != "full" || system["hash"] != full.Hash || system["base_hash"] != nil {
		t.Fatalf("system envelope = %#v", system)
	}
	if payload := system["payload"].(map[string]any); payload["hostname"] != "web-01" {
		t.Fatalf("system payload = %#v", payload)
	}
	delta := datasets["packages"].(map[string]any)
	if delta["mode"] != "delta" || delta["base_hash"] != strings.Repeat("a", 64) {
		t.Fatalf("packages envelope = %#v", delta)
	}
	if decoded["seq"].(float64) != 7 {
		t.Fatalf("seq = %v", decoded["seq"])
	}
	if _, ok := decoded["errors"]; ok {
		t.Fatal("errors present without failures")
	}
}

type pkg struct {
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

func pkgKey(p pkg) string { return p.Name + ":" + p.Arch }

func TestDiffAndApply(t *testing.T) {
	previous := []pkg{
		{"openssl", "amd64", "3.0.13"},
		{"php8.2-fpm", "amd64", "8.2.34"},
		{"nginx", "amd64", "1.24.0"},
	}
	current := []pkg{
		{"php8.2-fpm", "amd64", "8.2.35"},
		{"curl", "amd64", "8.5.0"},
		{"nginx", "amd64", "1.24.0"},
	}
	delta, err := Diff(previous, current, pkgKey)
	if err != nil {
		t.Fatal(err)
	}
	want := Delta[pkg]{
		Added:   []pkg{{"curl", "amd64", "8.5.0"}},
		Removed: []string{"openssl:amd64"},
		Changed: []pkg{{"php8.2-fpm", "amd64", "8.2.35"}},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Fatalf("Diff() = %#v\nwant %#v", delta, want)
	}

	applied, err := Apply(previous, delta, pkgKey)
	if err != nil {
		t.Fatal(err)
	}
	sorted := slices.Clone(current)
	slices.SortFunc(sorted, func(a, b pkg) int { return strings.Compare(pkgKey(a), pkgKey(b)) })
	if !reflect.DeepEqual(applied, sorted) {
		t.Fatalf("Apply() = %#v, want %#v", applied, sorted)
	}
}

func TestDiffUnchangedIsEmpty(t *testing.T) {
	items := []pkg{{"a", "amd64", "1"}, {"b", "arm64", "2"}}
	delta, err := Diff(items, slices.Clone(items), pkgKey)
	if err != nil {
		t.Fatal(err)
	}
	if !delta.Empty() {
		t.Fatalf("delta = %#v, want empty", delta)
	}
	data, _ := Canonical(delta)
	if string(data) != `{"added":[],"changed":[],"removed":[]}` {
		t.Fatalf("empty delta encodes as %s", data)
	}
}

func TestDiffRejectsDuplicateKeys(t *testing.T) {
	if _, err := Diff(nil, []pkg{{"a", "x", "1"}, {"a", "x", "2"}}, pkgKey); err == nil {
		t.Fatal("Diff() accepted duplicate keys")
	}
}

func TestApplyRejectsInconsistentDelta(t *testing.T) {
	base := []pkg{{"a", "x", "1"}}
	cases := []Delta[pkg]{
		{Removed: []string{"missing:x"}},
		{Changed: []pkg{{"missing", "x", "1"}}},
		{Added: []pkg{{"a", "x", "2"}}},
	}
	for _, delta := range cases {
		if _, err := Apply(base, delta, pkgKey); err == nil {
			t.Fatalf("Apply(%#v) succeeded", delta)
		}
	}
}

func TestStateStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	store, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Hash("system") != "" {
		t.Fatal("fresh store has a hash")
	}
	seq, err := store.NextSeq()
	if err != nil || seq != 1 {
		t.Fatalf("NextSeq() = %d, %v", seq, err)
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	if err := store.MarkSent(map[string]string{"system": "aaa", "network": "bbb"}, now); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Hashes(); !reflect.DeepEqual(got, map[string]string{"system": "aaa", "network": "bbb"}) {
		t.Fatalf("Hashes() = %#v", got)
	}
	if seq, _ := reopened.NextSeq(); seq != 2 {
		t.Fatalf("NextSeq() after reopen = %d, want 2", seq)
	}
	if err := reopened.Forget("network"); err != nil {
		t.Fatal(err)
	}
	if reopened.Hash("network") != "" || reopened.Hash("system") != "aaa" {
		t.Fatalf("Forget() left %#v", reopened.Hashes())
	}
	if err := reopened.Reset(); err != nil {
		t.Fatal(err)
	}
	if snapshot := reopened.Snapshot(); len(snapshot.Datasets) != 0 || snapshot.Seq != 2 {
		t.Fatalf("Reset() left %#v", snapshot)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestStateStoreRecoversFromCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Hashes()) != 0 {
		t.Fatal("corrupt state produced hashes")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file not moved aside: %v", err)
	}
}
