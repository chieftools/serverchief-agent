package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/inventory"
)

// packagesFixture returns the packages and updates datasets of
// fixtures/inventory-packages-request.json.
func packagesFixture(t *testing.T) map[string]inventory.DatasetEnvelope {
	t.Helper()
	var envelope inventory.Envelope
	if err := json.Unmarshal(readFixture(t, "inventory-packages-request.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Datasets
}

// TestPackagesFixtureMatchesCollectorTypes decodes the spec's packages and
// updates payloads into the collector types: re-encoding them must
// reproduce the fixture hashes, so the agent's field names and ordering are
// the ones the backend expects.
func TestPackagesFixtureMatchesCollectorTypes(t *testing.T) {
	datasets := packagesFixture(t)
	targets := map[string]any{"packages": &collector.PackagesInfo{}, "updates": &collector.UpdatesInfo{}}
	for name, target := range targets {
		dataset, ok := datasets[name]
		if !ok || dataset.Mode != inventory.ModeFull {
			t.Fatalf("fixture lacks a full %s dataset", name)
		}
		if hash, err := inventory.HashValue(json.RawMessage(dataset.Payload)); err != nil || hash != dataset.Hash {
			t.Fatalf("%s: canonical hash %s (%v), fixture says %s", name, hash, err, dataset.Hash)
		}
		if err := json.Unmarshal(dataset.Payload, target); err != nil {
			t.Fatal(err)
		}
		if hash, err := inventory.HashValue(target); err != nil || hash != dataset.Hash {
			t.Errorf("%s: collector type hashes to %s (%v), fixture says %s", name, hash, err, dataset.Hash)
		}
	}
	updates := targets["updates"].(*collector.UpdatesInfo)
	if updates.CacheUpdatedAt == nil || updates.CacheUpdatedAt.Format("2006-01-02T15:04:05Z07:00") != "2026-10-22T14:55:00Z" {
		t.Fatalf("cache_updated_at = %v", updates.CacheUpdatedAt)
	}
}

// backendApply applies a delta the way the spec tells the backend to: to
// the stored full payload's items only, keyed by manager (from the top
// level)/name/arch, keeping every other top-level key from the base, and
// sorting the result by key in byte order. It deliberately shares no code
// with the agent's delta builder.
func backendApply(base map[string]any, delta map[string]any) (map[string]any, error) {
	manager, _ := base["manager"].(string)
	key := func(item map[string]any) string {
		return fmt.Sprintf("%s/%s/%s", manager, item["name"], item["arch"])
	}
	items := make(map[string]map[string]any)
	for _, element := range base["items"].([]any) {
		item := element.(map[string]any)
		items[key(item)] = item
	}
	list := func(field string) []any {
		values, _ := delta[field].([]any)
		return values
	}
	for _, removed := range list("removed") {
		if _, ok := items[removed.(string)]; !ok {
			return nil, fmt.Errorf("removed %v is missing", removed)
		}
		delete(items, removed.(string))
	}
	for _, element := range list("changed") {
		item := element.(map[string]any)
		if _, ok := items[key(item)]; !ok {
			return nil, fmt.Errorf("changed %s is missing", key(item))
		}
		items[key(item)] = item
	}
	for _, element := range list("added") {
		item := element.(map[string]any)
		if _, ok := items[key(item)]; ok {
			return nil, fmt.Errorf("added %s exists", key(item))
		}
		items[key(item)] = item
	}
	keys := make([]string, 0, len(items))
	for name := range items {
		keys = append(keys, name)
	}
	slices.Sort(keys)
	applied := make([]any, len(keys))
	for index, name := range keys {
		applied[index] = items[name]
	}

	result := make(map[string]any, len(base))
	for field, value := range base {
		result[field] = value
	}
	result["items"] = applied
	return result, nil
}

// TestPackagesDeltaReproducesHashOnTheBackend builds a delta against the
// spec fixture, sends it through the envelope's wire form and applies it as
// the backend does: the result must hash to the dataset's hash.
func TestPackagesDeltaReproducesHashOnTheBackend(t *testing.T) {
	fixture := packagesFixture(t)["packages"]
	baseCanonical, err := inventory.Canonical(json.RawMessage(fixture.Payload))
	if err != nil {
		t.Fatal(err)
	}

	var current collector.PackagesInfo
	if err := json.Unmarshal(fixture.Payload, &current); err != nil {
		t.Fatal(err)
	}
	removed := collector.PackageKey(current.Manager, current.Items[3])
	current.Items = slices.Delete(current.Items, 3, 4)
	current.Items[10].Version += "+security1"
	changed := collector.PackageKey(current.Manager, current.Items[10])
	current.Items = append(current.Items,
		collector.Package{Name: "nginx", Version: "1.10.3-0ubuntu0.16.04.5", Arch: "arm64"},
		collector.Package{Name: "a-first", Version: "1.0", Arch: "all", Source: "first"},
		// Same name, second architecture: a distinct key.
		collector.Package{Name: "zlib1g", Version: "1:1.2.8.dfsg-2ubuntu4.3", Arch: "armhf", Source: "zlib"},
	)
	dataset, err := inventory.NewDataset("packages", &current)
	if err != nil {
		t.Fatal(err)
	}

	delta, ok, err := buildDelta(baseCanonical, dataset.Canonical, (&collector.Packages{}).DeltaKeyFields())
	if err != nil || !ok {
		t.Fatalf("no delta: %v %v", ok, err)
	}
	envelope := inventory.NewEnvelope(5)
	if err := envelope.AddDelta(dataset, fixture.Hash, delta); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	// The backend's side, from the wire form only.
	var received struct {
		Datasets map[string]struct {
			Hash     string         `json:"hash"`
			Mode     string         `json:"mode"`
			BaseHash string         `json:"base_hash"`
			Payload  map[string]any `json:"payload"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	sent := received.Datasets["packages"]
	if sent.Mode != "delta" || sent.BaseHash != fixture.Hash || sent.Hash != dataset.Hash {
		t.Fatalf("sent %s base %s hash %s", sent.Mode, sent.BaseHash, sent.Hash)
	}
	if got := fmt.Sprint(sent.Payload["removed"]); got != "["+removed+"]" || !strings.HasPrefix(removed, "dpkg/") {
		t.Fatalf("removed = %s, want [%s]", got, removed)
	}
	if changedItems := sent.Payload["changed"].([]any); len(changedItems) != 1 || len(sent.Payload["added"].([]any)) != 3 {
		t.Fatalf("delta = %v", sent.Payload)
	} else if item := changedItems[0].(map[string]any); "dpkg/"+item["name"].(string)+"/"+item["arch"].(string) != changed {
		t.Fatalf("changed %v, want %s", item, changed)
	}
	for _, field := range []string{"manager", "items"} {
		if _, ok := sent.Payload[field]; ok {
			t.Fatalf("delta carries top-level %q", field)
		}
	}

	var stored map[string]any
	if err := json.Unmarshal(fixture.Payload, &stored); err != nil {
		t.Fatal(err)
	}
	applied, err := backendApply(stored, sent.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if hash, err := inventory.HashValue(applied); err != nil || hash != sent.Hash {
		t.Fatalf("backend hash %s (%v), agent sent %s", hash, err, sent.Hash)
	}
	if applied["manager"] != "dpkg" {
		t.Fatalf("manager = %v", applied["manager"])
	}
}

// TestPackagesManagerChangeIsSentInFull covers the rule that a change of a
// top-level key other than items (manager) forces a full send.
func TestPackagesManagerChangeIsSentInFull(t *testing.T) {
	fixture := packagesFixture(t)["packages"]
	var current collector.PackagesInfo
	if err := json.Unmarshal(fixture.Payload, &current); err != nil {
		t.Fatal(err)
	}
	baseCanonical, _ := inventory.Canonical(json.RawMessage(fixture.Payload))
	current.Manager = collector.ManagerRPM
	dataset, _ := inventory.NewDataset("packages", &current)
	if _, ok, err := buildDelta(baseCanonical, dataset.Canonical, (&collector.Packages{}).DeltaKeyFields()); err != nil || ok {
		t.Fatalf("manager change produced a delta: %v %v", ok, err)
	}
}

// TestCertificatesFixtureMatchesCollectorTypes decodes the spec's
// certificates payload, and the vhosts payload that links to it by
// fingerprint, into the collector types: re-encoding must reproduce the
// fixture hashes.
func TestCertificatesFixtureMatchesCollectorTypes(t *testing.T) {
	var envelope inventory.Envelope
	if err := json.Unmarshal(readFixture(t, "inventory-certificates-request.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	targets := map[string]any{"certificates": &collector.CertificatesInfo{}, "vhosts": &collector.VHostsInfo{}}
	for name, target := range targets {
		dataset, ok := envelope.Datasets[name]
		if !ok || dataset.Mode != inventory.ModeFull {
			t.Fatalf("fixture lacks a full %s dataset", name)
		}
		if hash, err := inventory.HashValue(json.RawMessage(dataset.Payload)); err != nil || hash != dataset.Hash {
			t.Fatalf("%s: canonical hash %s (%v), fixture says %s", name, hash, err, dataset.Hash)
		}
		decoder := json.NewDecoder(strings.NewReader(string(dataset.Payload)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hash, err := inventory.HashValue(target); err != nil || hash != dataset.Hash {
			t.Errorf("%s: collector type hashes to %s (%v), fixture says %s", name, hash, err, dataset.Hash)
		}
	}
	certificates := targets["certificates"].(*collector.CertificatesInfo)
	byFingerprint := map[string]collector.Certificate{}
	for _, item := range certificates.Items {
		byFingerprint[item.Fingerprint] = item
	}
	for _, site := range targets["vhosts"].(*collector.VHostsInfo).Items {
		if site.Certificate == nil {
			continue
		}
		item, ok := byFingerprint[site.Certificate.Fingerprint]
		if !ok || !slices.ContainsFunc(item.UsedBy, func(use collector.CertificateUse) bool {
			return use.Kind == collector.CertificateUseSite && use.Ref == collector.VHostKey(site)
		}) {
			t.Errorf("site %s does not link to its certificate", collector.VHostKey(site))
		}
	}
	// The agent's own normalisation keeps the fixture as it is.
	normalized := *certificates
	normalized.Items = slices.Clone(certificates.Items)
	normalized.Normalize()
	if hash, _ := inventory.HashValue(&normalized); hash != envelope.Datasets["certificates"].Hash {
		t.Error("Normalize reorders the fixture")
	}
}

// TestApplicationsFixtureMatchesCollectorTypes decodes the spec's
// applications payload into the collector types: re-encoding and the
// agent's normalisation must reproduce the fixture hash.
func TestApplicationsFixtureMatchesCollectorTypes(t *testing.T) {
	var envelope inventory.Envelope
	if err := json.Unmarshal(readFixture(t, "inventory-applications-request.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	dataset, ok := envelope.Datasets["applications"]
	if !ok || dataset.Mode != inventory.ModeFull {
		t.Fatal("fixture lacks a full applications dataset")
	}
	var applications collector.ApplicationsInfo
	decoder := json.NewDecoder(strings.NewReader(string(dataset.Payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&applications); err != nil {
		t.Fatal(err)
	}
	applications.Normalize()
	if hash, err := inventory.HashValue(&applications); err != nil || hash != dataset.Hash {
		t.Errorf("collector type hashes to %s (%v), fixture says %s", hash, err, dataset.Hash)
	}
}

// TestCertificatesDeltaReproducesHashOnTheBackend builds a fingerprint-keyed
// delta against the spec fixture and applies it the way the backend does.
func TestCertificatesDeltaReproducesHashOnTheBackend(t *testing.T) {
	var envelope inventory.Envelope
	if err := json.Unmarshal(readFixture(t, "inventory-certificates-request.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	fixture := envelope.Datasets["certificates"]
	baseCanonical, err := inventory.Canonical(json.RawMessage(fixture.Payload))
	if err != nil {
		t.Fatal(err)
	}
	var current collector.CertificatesInfo
	if err := json.Unmarshal(fixture.Payload, &current); err != nil {
		t.Fatal(err)
	}
	removed := current.Items[2].Fingerprint
	current.Items = current.Items[:2]
	current.Items[0].UsedBy = append(current.Items[0].UsedBy, collector.CertificateUse{Kind: collector.CertificateUsePostfix, Ref: "postfix"})
	added := current.Items[1]
	added.Fingerprint = strings.Repeat("0", 64)
	current.Items = append(current.Items, added)
	current.Normalize()
	dataset, err := inventory.NewDataset("certificates", &current)
	if err != nil {
		t.Fatal(err)
	}
	delta, ok, err := buildDelta(baseCanonical, dataset.Canonical, (&collector.Certificates{}).DeltaKeyFields())
	if err != nil || !ok {
		t.Fatalf("no delta: %v %v", ok, err)
	}
	if !slices.Equal(delta.Removed, []string{removed}) || len(delta.Added) != 1 || len(delta.Changed) != 1 {
		t.Fatalf("delta = %+v", delta)
	}

	// The backend's side: key by fingerprint, apply, sort by key.
	var stored map[string]any
	if err := json.Unmarshal(fixture.Payload, &stored); err != nil {
		t.Fatal(err)
	}
	items := map[string]any{}
	for _, element := range stored["items"].([]any) {
		items[element.(map[string]any)["fingerprint"].(string)] = element
	}
	wire, _ := json.Marshal(delta)
	var received struct {
		Added   []map[string]any `json:"added"`
		Removed []string         `json:"removed"`
		Changed []map[string]any `json:"changed"`
	}
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	for _, key := range received.Removed {
		delete(items, key)
	}
	for _, item := range append(received.Changed, received.Added...) {
		items[item["fingerprint"].(string)] = item
	}
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	applied := make([]any, len(keys))
	for index, key := range keys {
		applied[index] = items[key]
	}
	if hash, err := inventory.HashValue(map[string]any{"items": applied}); err != nil || hash != dataset.Hash {
		t.Fatalf("backend hash %s (%v), agent sent %s", hash, err, dataset.Hash)
	}
}

// TestRepositoriesFixtureMatchesCollectorTypes decodes the spec's
// repositories payload, and the packages payload whose origins refer to
// it, into the collector types: re-encoding must reproduce the fixture
// hashes, every origin must name a repository and each repository's
// packages count must match the origins.
func TestRepositoriesFixtureMatchesCollectorTypes(t *testing.T) {
	var envelope inventory.Envelope
	if err := json.Unmarshal(readFixture(t, "inventory-repositories-request.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	targets := map[string]any{"repositories": &collector.RepositoriesInfo{}, "packages": &collector.PackagesInfo{}}
	for name, target := range targets {
		dataset, ok := envelope.Datasets[name]
		if !ok || dataset.Mode != inventory.ModeFull {
			t.Fatalf("fixture lacks a full %s dataset", name)
		}
		if hash, err := inventory.HashValue(json.RawMessage(dataset.Payload)); err != nil || hash != dataset.Hash {
			t.Fatalf("%s: canonical hash %s (%v), fixture says %s", name, hash, err, dataset.Hash)
		}
		decoder := json.NewDecoder(strings.NewReader(string(dataset.Payload)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hash, err := inventory.HashValue(target); err != nil || hash != dataset.Hash {
			t.Errorf("%s: collector type hashes to %s (%v), fixture says %s", name, hash, err, dataset.Hash)
		}
	}
	repositories := targets["repositories"].(*collector.RepositoriesInfo)
	packages := targets["packages"].(*collector.PackagesInfo)
	counts := map[string]int{}
	empty := 0
	for _, item := range packages.Items {
		if item.Origins == nil {
			t.Errorf("%s: origins left out", item.Name)
		}
		if len(item.Origins) == 0 {
			empty++
		}
		if !slices.IsSorted(item.Origins) {
			t.Errorf("%s: origins %v are not sorted", item.Name, item.Origins)
		}
		for _, id := range item.Origins {
			counts[id]++
		}
	}
	if empty == 0 {
		t.Error("the fixture has no package without origins")
	}
	ids := map[string]bool{}
	for _, item := range repositories.Items {
		ids[item.ID] = true
		if suite, _, found := strings.Cut(item.ID, ":"); !found || suite != item.Suite {
			t.Errorf("apt id %q is not prefixed with its suite %q", item.ID, item.Suite)
		}
		if item.Packages != counts[item.ID] {
			t.Errorf("%s counts %d packages, the origins %d", item.ID, item.Packages, counts[item.ID])
		}
	}
	for id := range counts {
		if !ids[id] {
			t.Errorf("origin %s names no repository", id)
		}
	}
}
