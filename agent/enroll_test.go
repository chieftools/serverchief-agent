package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/inventory"
)

const testToken = "scenr_Q2xhdWRlQ29kZVNlcnZlckNoaWVmRml4dHVyZTA1"

type recordingRunner struct {
	mu       sync.Mutex
	commands []string
}

func (runner *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.commands = append(runner.commands, strings.Join(append([]string{name}, args...), " "))
	return nil
}

func dirEntry() *fstest.MapFile { return &fstest.MapFile{Mode: fs.ModeDir | 0o755} }

var systemdHost = fstest.MapFS{
	"run/systemd/system":                     dirEntry(),
	"lib/systemd/system/serverchief.service": &fstest.MapFile{Data: []byte("[Unit]\n")},
}

// enrollServer answers POST /enroll with the fixture, bumping the
// generation per server_id, and records every request body.
type enrollServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []EnrollmentRequest
	status   []int // one-shot statuses, consumed in order
	problem  string
	gen      uint64
}

func newEnrollServer(t *testing.T, tlsServer bool) *enrollServer {
	t.Helper()
	server := &enrollServer{}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/agent/v1/enroll" || request.Header.Get("Authorization") != "" {
			writeJSONResponse(writer, 400, `{"type":"invalid_request","status":400,"title":"bad"}`)
			return
		}
		var body EnrollmentRequest
		_ = json.NewDecoder(request.Body).Decode(&body)
		server.mu.Lock()
		defer server.mu.Unlock()
		server.requests = append(server.requests, body)
		if len(server.status) > 0 {
			status := server.status[0]
			server.status = server.status[1:]
			writeJSONResponse(writer, status, server.problem)
			return
		}
		server.gen++
		var response map[string]any
		_ = json.Unmarshal(readFixtureNoT("enroll-response.json"), &response)
		response["generation"] = server.gen
		encoded, _ := json.Marshal(response)
		writeJSONResponse(writer, 200, string(encoded))
	})
	if tlsServer {
		server.Server = httptest.NewTLSServer(handler)
	} else {
		server.Server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return server
}

func (server *enrollServer) recorded() []EnrollmentRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]EnrollmentRequest(nil), server.requests...)
}

func testEnrollOptions(t *testing.T, server *enrollServer, runner *recordingRunner, host fs.FS) EnrollOptions {
	t.Helper()
	directory := t.TempDir()
	paths := Paths{
		Identity: filepath.Join(directory, "etc", "identity.json"),
		Pending:  filepath.Join(directory, "etc", "setup.json"),
	}.WithStateDirectory(filepath.Join(directory, "var"))
	return EnrollOptions{
		Paths:        paths,
		Endpoint:     server.URL + "/agent/v1",
		Version:      "0.1.0",
		Hostname:     "web-01",
		Platform:     "ubuntu 16.04.7",
		InitSystem:   "systemd",
		GOOS:         "linux",
		Architecture: "arm64",
		RootCheck:    func() bool { return true },
		HTTPClient:   server.Client(),
		Service:      DetectService(host, runner),
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestEnrollWritesIdentityAndStartsService(t *testing.T) {
	server := newEnrollServer(t, false)
	runner := &recordingRunner{}
	options := testEnrollOptions(t, server, runner, systemdHost)

	result, err := Enroll(context.Background(), testToken, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity.ServerID != "01jb8z5k2m3n4p5q6r7s8t9v0w" || result.Identity.Generation != 1 || result.Reenrolled || result.Service != initsys.Systemd {
		t.Fatalf("result = %+v", result)
	}

	requests := server.recorded()
	if len(requests) != 1 {
		t.Fatalf("%d enroll requests", len(requests))
	}
	request := requests[0]
	if request.Token != testToken || request.ProtocolRevision != "1.2.0" || request.Hostname != "web-01" || request.Platform != "ubuntu 16.04.7" ||
		request.Architecture != "arm64" || request.InitSystem != "systemd" || request.AgentVersion != "0.1.0" ||
		request.PreviousServerID != nil || request.PreviousGeneration != nil {
		t.Fatalf("request = %+v", request)
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(request.AttemptID) {
		t.Fatalf("attempt_id %q is not a ULID", request.AttemptID)
	}
	if !regexp.MustCompile(`^scagt_[A-Za-z0-9_-]{43}$`).MatchString(request.Credential) {
		t.Fatalf("credential %q has the wrong shape", request.Credential)
	}

	identity, err := LoadIdentity(options.Paths.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Credential != request.Credential || identity.Endpoint != options.Endpoint || identity.SetupTokenHash != digest(testToken) {
		t.Fatalf("identity = %+v", identity)
	}
	if mode := fileMode(t, options.Paths.Identity); mode != 0o600 {
		t.Fatalf("identity.json mode = %o", mode)
	}
	if _, err := os.Stat(options.Paths.Pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup.json was not removed")
	}
	config, found, err := LoadServerConfig(options.Paths.ServerConfig)
	if err != nil || !found || config.ETag != `"c1-5f3a9e"` || config.Realtime == nil {
		t.Fatalf("server config cache = %+v, %v, %v", config, found, err)
	}
	if want := []string{"systemctl daemon-reload", "systemctl enable serverchief.service", "systemctl start serverchief.service"}; !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("service commands = %v", runner.commands)
	}
}

func TestEnrollRetryReusesJournal(t *testing.T) {
	server := newEnrollServer(t, false)
	server.status = []int{http.StatusServiceUnavailable}
	server.problem = `{"type":"maintenance","title":"Down","status":503}`
	runner := &recordingRunner{}
	options := testEnrollOptions(t, server, runner, systemdHost)

	if _, err := Enroll(context.Background(), testToken, options); err == nil {
		t.Fatal("Enroll() succeeded against a 503")
	}
	if mode := fileMode(t, options.Paths.Pending); mode != 0o600 {
		t.Fatalf("setup.json mode = %o", mode)
	}
	var journal pendingEnrollment
	data, _ := os.ReadFile(options.Paths.Pending)
	if err := json.Unmarshal(data, &journal); err != nil || journal.TokenHash != digest(testToken) || strings.Contains(string(data), testToken) {
		t.Fatalf("journal = %s (%v)", data, err)
	}
	if _, err := os.Stat(options.Paths.Identity); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("identity written after a failed enrollment")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("service touched after a failed enrollment: %v", runner.commands)
	}

	if _, err := Enroll(context.Background(), testToken, options); err != nil {
		t.Fatal(err)
	}
	requests := server.recorded()
	if len(requests) != 2 || requests[0].AttemptID != requests[1].AttemptID || requests[0].Credential != requests[1].Credential {
		t.Fatalf("retry did not reuse the journal: %+v", requests)
	}
	if requests[0].AttemptID != journal.AttemptID || requests[0].Credential != journal.Credential {
		t.Fatal("the journal was not written before the first request")
	}

	// A different token starts a new attempt.
	other := strings.Replace(testToken, "Q2", "Z9", 1)
	server.status = []int{http.StatusUnauthorized}
	server.problem = `{"type":"unauthenticated","title":"Unauthenticated","status":401,"detail":"Enrollment token is invalid or expired"}`
	_, err := Enroll(context.Background(), other, options)
	if err == nil || !strings.Contains(err.Error(), "Enrollment token is invalid or expired") || !strings.Contains(err.Error(), "create a new token") {
		t.Fatalf("401 error = %v", err)
	}
	if requests := server.recorded(); requests[2].AttemptID == requests[1].AttemptID || requests[2].Credential == requests[1].Credential {
		t.Fatal("a new token reused the previous attempt")
	}
}

func TestEnrollConflictExplainsItself(t *testing.T) {
	server := newEnrollServer(t, false)
	server.status = []int{http.StatusConflict}
	server.problem = `{"type":"enrollment_conflict","title":"Conflict","status":409}`
	options := testEnrollOptions(t, server, &recordingRunner{}, systemdHost)
	_, err := Enroll(context.Background(), testToken, options)
	if err == nil || !strings.Contains(err.Error(), "different setup attempt") {
		t.Fatalf("409 error = %v", err)
	}
}

func TestReenrollSendsPreviousIdentityAndRestarts(t *testing.T) {
	server := newEnrollServer(t, false)
	runner := &recordingRunner{}
	options := testEnrollOptions(t, server, runner, systemdHost)
	first, err := Enroll(context.Background(), testToken, options)
	if err != nil {
		t.Fatal(err)
	}

	store, _ := inventory.OpenStateStore(options.Paths.State)
	if _, err := store.NextSeq(); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSent(map[string]string{"system": "abc"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	runner.commands = nil
	second, err := Enroll(context.Background(), strings.Replace(testToken, "Q2", "R2", 1), options)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reenrolled || second.Identity.Generation != 2 || second.Identity.Credential == first.Identity.Credential {
		t.Fatalf("re-enrollment = %+v", second)
	}
	request := server.recorded()[1]
	if request.PreviousServerID == nil || *request.PreviousServerID != first.Identity.ServerID || request.PreviousGeneration == nil || *request.PreviousGeneration != 1 {
		t.Fatalf("previous identity not sent: %+v", request)
	}
	want := []string{"systemctl stop serverchief.service", "systemctl daemon-reload", "systemctl enable serverchief.service", "systemctl restart serverchief.service"}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("service commands = %v", runner.commands)
	}
	store, _ = inventory.OpenStateStore(options.Paths.State)
	if store.Hash("system") != "" || store.Seq() != 0 {
		t.Fatalf("state after re-enrollment: hash %q, seq %d", store.Hash("system"), store.Seq())
	}

	// Running setup again with the token that already completed only
	// restarts the service.
	runner.commands = nil
	again, err := Enroll(context.Background(), strings.Replace(testToken, "Q2", "R2", 1), options)
	if err != nil || again.Identity.Generation != 2 || len(server.recorded()) != 2 {
		t.Fatalf("repeat setup = %+v, %v (%d requests)", again, err, len(server.recorded()))
	}
	if runner.commands[len(runner.commands)-1] != "systemctl restart serverchief.service" {
		t.Fatalf("repeat setup commands = %v", runner.commands)
	}
}

func TestEnrollWithoutInitSystemPrintsManualStart(t *testing.T) {
	server := newEnrollServer(t, false)
	runner := &recordingRunner{}
	for name, host := range map[string]fstest.MapFS{
		"no init":            {},
		"sysv, not packaged": {"etc/init.d": dirEntry()},
		"systemd, no unit":   {"run/systemd/system": dirEntry()},
	} {
		options := testEnrollOptions(t, server, runner, host)
		result, err := Enroll(context.Background(), testToken, options)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Service != "" || result.ManualStart == "" {
			t.Fatalf("%s: result = %+v", name, result)
		}
		var output strings.Builder
		PrintEnrollResult(&output, result)
		if !strings.Contains(output.String(), "serverchief run") {
			t.Fatalf("%s: output = %q", name, output.String())
		}
		server.mu.Lock()
		server.gen = 0
		server.mu.Unlock()
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands ran without a service: %v", runner.commands)
	}
}

func TestEnrollSysVService(t *testing.T) {
	server := newEnrollServer(t, false)
	runner := &recordingRunner{}
	host := fstest.MapFS{"etc/init.d/serverchief": &fstest.MapFile{Data: []byte("#!/bin/sh\n")}, "usr/sbin/update-rc.d": &fstest.MapFile{}}
	result, err := Enroll(context.Background(), testToken, testEnrollOptions(t, server, runner, host))
	if err != nil || result.Service != initsys.SysV {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if want := []string{"update-rc.d serverchief defaults", "/etc/init.d/serverchief start"}; !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %v", runner.commands)
	}
}

func TestEnrollTrustsCAFile(t *testing.T) {
	server := newEnrollServer(t, true)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	certificate := server.Certificate()
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	options := testEnrollOptions(t, server, &recordingRunner{}, systemdHost)
	options.HTTPClient = nil

	if _, err := Enroll(context.Background(), testToken, options); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("Enroll() without the CA = %v, want a certificate error", err)
	}
	options.CAFile = caFile
	result, err := Enroll(context.Background(), testToken, options)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := LoadIdentity(options.Paths.Identity)
	if identity.CAFile != caFile || result.Identity.CAFile != caFile {
		t.Fatalf("CA file not persisted: %q", identity.CAFile)
	}
	if _, err := NewHTTPClient(identity.CAFile); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a certificate"), 0o600)
	options.CAFile = bad
	if _, err := Enroll(context.Background(), testToken, options); err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("bad CA file = %v", err)
	}
}

func TestEnrollValidatesPreconditions(t *testing.T) {
	root := func() bool { return true }
	cases := []struct {
		name    string
		token   string
		options EnrollOptions
		want    string
	}{
		{"not root", testToken, EnrollOptions{RootCheck: func() bool { return false }}, "must run as root"},
		{"darwin", testToken, EnrollOptions{RootCheck: root, GOOS: "darwin", Architecture: "arm64"}, "requires Linux"},
		{"386", testToken, EnrollOptions{RootCheck: root, GOOS: "linux", Architecture: "386"}, "requires Linux"},
		{"bad prefix", "abc", EnrollOptions{RootCheck: root, GOOS: "linux", Architecture: "amd64"}, "must start with scenr_"},
		{"empty token", "scenr_", EnrollOptions{RootCheck: root, GOOS: "linux", Architecture: "amd64"}, "must start with scenr_"},
		{"not base62", "scenr_Q2xhdWRlQ29kZVNlcnZlckNoaWVmRml4dHVy-ZTA", EnrollOptions{RootCheck: root, GOOS: "linux", Architecture: "amd64"}, "contains"},
		{"http endpoint", testToken, EnrollOptions{RootCheck: root, GOOS: "linux", Architecture: "amd64", Endpoint: "http://example.com/agent/v1"}, "https://"},
	}
	for _, test := range cases {
		_, err := Enroll(context.Background(), test.token, test.options)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Enroll() = %v, want %q", test.name, err, test.want)
		}
	}
	for _, valid := range []string{"scenr_" + strings.Repeat("aZ9", 13) + "x", "scenr_Q2xhdWRlQ29kZVNlcnZlckNoaWVmRml4dHVyZTA", "scenr_abc"} {
		if err := ValidateEnrollmentToken(valid); err != nil {
			t.Fatalf("%s was rejected: %v", valid, err)
		}
	}
}

func TestLoadIdentity(t *testing.T) {
	directory := t.TempDir()
	if _, err := LoadIdentity(filepath.Join(directory, "missing.json")); !errors.Is(err, ErrNotEnrolled) || !strings.Contains(err.Error(), "serverchief setup <token>") {
		t.Fatalf("missing identity = %v", err)
	}
	incomplete := filepath.Join(directory, "identity.json")
	_ = os.WriteFile(incomplete, []byte(`{"endpoint":"https://server.chief.app/agent/v1"}`), 0o600)
	if _, err := LoadIdentity(incomplete); err == nil {
		t.Fatal("incomplete identity accepted")
	}
}

func TestNewULIDAndCredential(t *testing.T) {
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	first, _ := newULID(now)
	second, _ := newULID(now.Add(time.Millisecond))
	if len(first) != 26 || first >= second {
		t.Fatalf("ULIDs %s, %s", first, second)
	}
	credential, _ := newCredential()
	if len(credential) != len(CredentialPrefix)+43 {
		t.Fatalf("credential length %d", len(credential))
	}
}
