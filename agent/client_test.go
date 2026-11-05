package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "protocol", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sameJSON compares two documents structurally.
func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(got, &left); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal(want, &right); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	return reflect.DeepEqual(left, right)
}

func TestEnrollmentRequestMatchesFixture(t *testing.T) {
	fixture := readFixture(t, "enroll-request.json")
	var request EnrollmentRequest
	if err := json.Unmarshal(fixture, &request); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, encoded, fixture) {
		t.Fatalf("EnrollmentRequest does not round-trip the fixture:\n%s", encoded)
	}
	if request.PreviousServerID != nil || request.PreviousGeneration != nil {
		t.Fatal("fixture previous_* should decode as null")
	}

	generation := uint64(3)
	serverID := "01jb8z5k2m3n4p5q6r7s8t9v0w"
	request.PreviousServerID, request.PreviousGeneration = &serverID, &generation
	encoded, _ = json.Marshal(request)
	if !strings.Contains(string(encoded), `"previous_generation":3`) || !strings.Contains(string(encoded), `"previous_server_id":"01jb8z5k2m3n4p5q6r7s8t9v0w"`) {
		t.Fatalf("re-enrollment request = %s", encoded)
	}
}

func TestHeartbeatRequestMatchesFixture(t *testing.T) {
	fixture := readFixture(t, "heartbeat-request.json")
	var request HeartbeatRequest
	if err := json.Unmarshal(fixture, &request); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(request)
	if !sameJSON(t, encoded, fixture) {
		t.Fatalf("HeartbeatRequest does not round-trip the fixture:\n%s", encoded)
	}
}

func TestResponseFixturesDecode(t *testing.T) {
	var enrollment EnrollmentResponse
	if err := json.Unmarshal(readFixture(t, "enroll-response.json"), &enrollment); err != nil {
		t.Fatal(err)
	}
	config := enrollment.ServerConfig()
	if enrollment.ServerID != "01jb8z5k2m3n4p5q6r7s8t9v0w" || enrollment.Generation != 1 || config.ETag != `"c1-5f3a9e"` ||
		config.Intervals.Datasets["hardware"] != 3600 || config.Realtime == nil || config.Realtime.Port != 443 {
		t.Fatalf("enroll response = %+v", enrollment)
	}
	if config.Metrics == nil || *config.Metrics != (MetricsConfig{Enabled: true, Endpoint: "https://metrics.server.chief.test", Interval: 60}) {
		t.Fatalf("enroll response metrics = %+v", config.Metrics)
	}

	var server ServerConfig
	if err := json.Unmarshal(readFixture(t, "config-response.json"), &server); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(server, config) {
		t.Fatalf("config fixture %+v differs from the enroll response's %+v", server, config)
	}

	var heartbeat HeartbeatResponse
	if err := json.Unmarshal(readFixture(t, "heartbeat-response.json"), &heartbeat); err != nil {
		t.Fatal(err)
	}
	if heartbeat.ConfigETag != config.ETag || heartbeat.Resync == nil || heartbeat.CommandsAvailable {
		t.Fatalf("heartbeat response = %+v", heartbeat)
	}

	var inventoryResponse InventoryResponse
	if err := json.Unmarshal(readFixture(t, "inventory-response.json"), &inventoryResponse); err != nil {
		t.Fatal(err)
	}
	if inventoryResponse.Seq != 1 || len(inventoryResponse.Accepted) != 4 {
		t.Fatalf("inventory response = %+v", inventoryResponse)
	}
}

func TestInventoryFixtureHashesReproduce(t *testing.T) {
	var envelope inventory.Envelope
	if err := json.Unmarshal(readFixture(t, "inventory-request.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Datasets) != 4 {
		t.Fatalf("fixture has %d datasets", len(envelope.Datasets))
	}
	for name, dataset := range envelope.Datasets {
		hash, err := inventory.HashValue(dataset.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if hash != dataset.Hash {
			t.Errorf("%s: canonical hash %s, fixture says %s", name, hash, dataset.Hash)
		}
	}
}

func TestProblemFixtureDecodes(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": {"application/problem+json"}},
		Body:       io.NopCloser(bytes.NewReader(readFixture(t, "problem.json"))),
	}
	problem := decodeProblem(response)
	if problem.Type != ProblemRevoked || problem.Title != "Credential revoked" || !strings.Contains(problem.Detail, "serverchief setup") {
		t.Fatalf("problem = %+v", problem)
	}
	if kind, _ := classify(problem); kind != failureFatal || !IsUnauthorized(problem) {
		t.Fatalf("revoked classified as %v", kind)
	}
}

// recordedRequest is what the test server saw.
type recordedRequest struct {
	method, path string
	header       http.Header
	body         []byte
	gzipped      bool
}

func newRecordingServer(t *testing.T, handler func(http.ResponseWriter, *recordedRequest)) (*httptest.Server, *[]*recordedRequest) {
	t.Helper()
	var requests []*recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		recorded := &recordedRequest{method: request.Method, path: request.URL.Path, header: request.Header.Clone()}
		if request.Header.Get("Content-Encoding") == "gzip" {
			recorded.gzipped = true
			reader, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Errorf("gzip: %v", err)
			} else if body, err = io.ReadAll(reader); err != nil {
				t.Errorf("gunzip: %v", err)
			}
		}
		recorded.body = body
		requests = append(requests, recorded)
		handler(writer, recorded)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func writeJSONResponse(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

func TestHTTPClientEndpointsAndHeaders(t *testing.T) {
	enrollResponse := string(readFixtureNoT("enroll-response.json"))
	server, requests := newRecordingServer(t, func(writer http.ResponseWriter, request *recordedRequest) {
		switch request.path {
		case "/agent/v1/enroll":
			writeJSONResponse(writer, 200, enrollResponse)
		case "/agent/v1/heartbeat":
			writeJSONResponse(writer, 200, `{"resync":["network"],"config_etag":"\"c2\"","commands_available":false}`)
		case "/agent/v1/inventory":
			writeJSONResponse(writer, 200, `{"accepted":{"system":"abc"},"resync":[],"ignored":["bogus"],"seq":7}`)
		case "/agent/v1/config":
			if request.header.Get("If-None-Match") == `"c2"` {
				writer.WriteHeader(http.StatusNotModified)
				return
			}
			writer.Header().Set("ETag", `"c2"`)
			writeJSONResponse(writer, 200, `{"intervals":{"heartbeat":120,"datasets":{"system":60}},"datasets":["system"],"realtime":null}`)
		case "/agent/v1/broadcasting/auth":
			writeJSONResponse(writer, 200, `{"auth":"serverchief-local:signature"}`)
		default:
			writeJSONResponse(writer, 404, `{}`)
		}
	})
	ctx := context.Background()
	client := NewClient(server.URL+"/agent/v1/", "scagt_secret", "v1.2.3", server.Client())

	enrollment, err := client.Enroll(ctx, EnrollmentRequest{Token: "scenr_x", AttemptID: "A", Credential: "scagt_secret"})
	if err != nil || enrollment.ServerID != "01jb8z5k2m3n4p5q6r7s8t9v0w" {
		t.Fatalf("Enroll() = %+v, %v", enrollment, err)
	}
	heartbeat, err := client.Heartbeat(ctx, HeartbeatRequest{AgentVersion: "1.2.3"})
	if err != nil || !reflect.DeepEqual(heartbeat.Resync, []string{"network"}) || heartbeat.ConfigETag != `"c2"` {
		t.Fatalf("Heartbeat() = %+v, %v", heartbeat, err)
	}
	envelope := inventory.NewEnvelope(7)
	envelope.Datasets["system"] = inventory.DatasetEnvelope{Hash: "abc", Mode: inventory.ModeFull, Payload: json.RawMessage(`{"a":1}`)}
	accepted, err := client.SendInventory(ctx, envelope)
	if err != nil || accepted.Seq != 7 || accepted.Accepted["system"] != "abc" || !reflect.DeepEqual(accepted.Ignored, []string{"bogus"}) {
		t.Fatalf("SendInventory() = %+v, %v", accepted, err)
	}
	config, err := client.FetchConfig(ctx, "")
	if err != nil || config.NotModified || config.Config.ETag != `"c2"` || config.Config.Intervals.Datasets["system"] != 60 {
		t.Fatalf("FetchConfig() = %+v, %v", config, err)
	}
	config, err = client.FetchConfig(ctx, `"c2"`)
	if err != nil || !config.NotModified {
		t.Fatalf("FetchConfig(etag) = %+v, %v", config, err)
	}
	auth, err := client.AuthorizeChannel(ctx, server.URL+"/agent/v1/broadcasting/auth", "123.456", "private-agent.server_x.generation_1")
	if err != nil || auth != "serverchief-local:signature" {
		t.Fatalf("AuthorizeChannel() = %q, %v", auth, err)
	}

	if len(*requests) != 6 {
		t.Fatalf("%d requests", len(*requests))
	}
	for _, request := range *requests {
		if request.header.Get(ProtocolHeader) != "1.3.0" {
			t.Errorf("%s: protocol header %q", request.path, request.header.Get(ProtocolHeader))
		}
		if userAgent := request.header.Get("User-Agent"); !strings.HasPrefix(userAgent, "serverchief-agent/1.2.3 (") || !strings.HasSuffix(userAgent, ")") {
			t.Errorf("%s: User-Agent %q", request.path, userAgent)
		}
		authorization := request.header.Get("Authorization")
		if request.path == "/agent/v1/enroll" {
			if authorization != "" {
				t.Errorf("enroll sent Authorization %q", authorization)
			}
		} else if authorization != "Bearer scagt_secret" {
			t.Errorf("%s: Authorization %q", request.path, authorization)
		}
		if request.method == http.MethodPost && request.path != "/agent/v1/broadcasting/auth" && request.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type %q", request.path, request.header.Get("Content-Type"))
		}
		if request.gzipped {
			t.Errorf("%s: small body was gzipped", request.path)
		}
	}
	enroll := (*requests)[0]
	if !strings.Contains(string(enroll.body), `"token":"scenr_x"`) {
		t.Fatalf("enroll body = %s", enroll.body)
	}
	if got := (*requests)[4].header.Get("If-None-Match"); got != `"c2"` {
		t.Fatalf("If-None-Match = %q", got)
	}
	authRequest := (*requests)[5]
	if authRequest.header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
		string(authRequest.body) != "channel_name=private-agent.server_x.generation_1&socket_id=123.456" {
		t.Fatalf("auth request = %q %s", authRequest.header.Get("Content-Type"), authRequest.body)
	}
}

func readFixtureNoT(name string) []byte {
	data, err := os.ReadFile(filepath.Join("testdata", "protocol", name))
	if err != nil {
		panic(err)
	}
	return data
}

func TestHTTPClientGzipsLargeBodies(t *testing.T) {
	server, requests := newRecordingServer(t, func(writer http.ResponseWriter, _ *recordedRequest) {
		writeJSONResponse(writer, 200, `{"accepted":{},"resync":[],"ignored":[],"seq":1}`)
	})
	client := NewClient(server.URL, "scagt_secret", "1.0.0", server.Client())
	envelope := inventory.NewEnvelope(1)
	payload, _ := json.Marshal(map[string]string{"blob": strings.Repeat("x", 20<<10)})
	envelope.Datasets["system"] = inventory.DatasetEnvelope{Hash: "h", Mode: inventory.ModeFull, Payload: payload}
	if _, err := client.SendInventory(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	request := (*requests)[0]
	if !request.gzipped {
		t.Fatal("large body was not gzipped")
	}
	var decoded inventory.Envelope
	if err := json.Unmarshal(request.body, &decoded); err != nil || decoded.Seq != 1 || len(decoded.Datasets["system"].Payload) != len(payload) {
		t.Fatalf("decompressed body = %v, %v", decoded.Seq, err)
	}

	huge := inventory.NewEnvelope(2)
	huge.Datasets["system"] = inventory.DatasetEnvelope{Hash: "h", Mode: inventory.ModeFull, Payload: json.RawMessage(`"` + strings.Repeat("y", MaximumBodyBytes) + `"`)}
	_, err := client.SendInventory(context.Background(), huge)
	if kind, problem := classify(err); kind != failureRejected || problem.Type != ProblemPayloadTooLarge || len(*requests) != 1 {
		t.Fatalf("oversized envelope: %v (sent %d)", err, len(*requests))
	}
}

func TestHTTPClientProblemMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		kind       failureKind
		problem    string
		retryAfter time.Duration
	}{
		{"revoked", 401, nil, string(readFixtureNoT("problem.json")), failureFatal, ProblemRevoked, 0},
		{"unauthenticated", 401, nil, `{"type":"unauthenticated","title":"Unauthenticated","status":401}`, failureFatal, ProblemUnauthenticated, 0},
		{"unsupported revision", 426, nil, `{"type":"unsupported_revision","title":"Upgrade","status":426}`, failureUnsupported, ProblemUnsupportedRevision, 0},
		{"rate limited header", 429, map[string]string{"Retry-After": "42"}, `{"type":"rate_limited","title":"Slow down","status":429}`, failureThrottled, "rate_limited", 42 * time.Second},
		{"unavailable body", 503, nil, `{"type":"maintenance","title":"Down","status":503,"retry_after":17}`, failureThrottled, "maintenance", 17 * time.Second},
		{"header wins", 503, map[string]string{"Retry-After": "5"}, `{"status":503,"retry_after":17}`, failureThrottled, "", 5 * time.Second},
		{"validation", 422, nil, `{"type":"validation_failed","title":"Invalid","status":422,"errors":{"seq":["must be an integer"]}}`, failureRejected, ProblemValidationFailed, 0},
		{"bad request", 400, nil, `{"type":"invalid_request","title":"Bad","status":400}`, failureRejected, ProblemInvalidRequest, 0},
		{"too large", 413, nil, `{"type":"payload_too_large","title":"Too large","status":413}`, failureRejected, ProblemPayloadTooLarge, 0},
		{"server error", 500, nil, `{"type":"server_error","title":"Oops","status":500}`, failureTransient, "server_error", 0},
		{"html bad gateway", 502, nil, `<html>bad gateway</html>`, failureTransient, "", 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				for name, value := range test.header {
					writer.Header().Set(name, value)
				}
				writer.Header().Set("Content-Type", "application/problem+json")
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := NewClient(server.URL, "scagt_secret", "1.0.0", server.Client())
			_, err := client.Heartbeat(context.Background(), HeartbeatRequest{})
			var problem *ProblemError
			if !errors.As(err, &problem) {
				t.Fatalf("err = %v, want *ProblemError", err)
			}
			kind, _ := classify(err)
			if kind != test.kind || problem.Status != test.status || problem.Type != test.problem || problem.RetryAfter != test.retryAfter {
				t.Fatalf("got kind %v, %+v", kind, problem)
			}
			if test.name == "validation" && !strings.Contains(err.Error(), "seq: must be an integer") {
				t.Fatalf("validation error text = %q", err)
			}
		})
	}
}

func TestHTTPClientNetworkErrorIsTransient(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	client := NewClient(server.URL, "scagt_secret", "1.0.0", nil)
	_, err := client.Heartbeat(context.Background(), HeartbeatRequest{})
	if kind, problem := classify(err); err == nil || kind != failureTransient || problem != nil {
		t.Fatalf("network error = %v classified %v", err, kind)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	if wait, ok := parseRetryAfter("120", now); !ok || wait != 2*time.Minute {
		t.Fatalf("seconds = %s %v", wait, ok)
	}
	if wait, ok := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); !ok || wait != 90*time.Second {
		t.Fatalf("date = %s %v", wait, ok)
	}
	for _, invalid := range []string{"", "-1", "soon"} {
		if _, ok := parseRetryAfter(invalid, now); ok {
			t.Fatalf("%q accepted", invalid)
		}
	}
}

func TestAuthorizeChannelKeepsCredentialOnEndpointOrigin(t *testing.T) {
	var hits []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits = append(hits, request.URL.Path)
		writeJSONResponse(writer, 200, `{"auth":"k:s"}`)
	}))
	defer server.Close()
	client := NewClient(server.URL+"/agent/v1", "scagt_secret", "1.0.0", server.Client())
	for _, foreign := range []string{"https://evil.example/agent/v1/broadcasting/auth", ""} {
		if _, err := client.AuthorizeChannel(context.Background(), foreign, "1.2", "private-x"); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(hits, []string{"/agent/v1/broadcasting/auth", "/agent/v1/broadcasting/auth"}) {
		t.Fatalf("auth requests went to %v", hits)
	}
}

func TestUserAgentIncludesTheEnrolledServerID(t *testing.T) {
	platform := " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
	for _, test := range []struct {
		name, serverID, want string
	}{
		{"before enrollment", "", "serverchief-agent/1.2.3" + platform},
		{"enrolled", "01jb8z5k2m3n4p5q6r7s8t9v0w", "serverchief-agent/1.2.3" + platform + " server/01jb8z5k2m3n4p5q6r7s8t9v0w"},
		{"unsafe characters", "01jb8z5k/../x y", "serverchief-agent/1.2.3" + platform},
		{"too long", strings.Repeat("a", 65), "serverchief-agent/1.2.3" + platform},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := UserAgent("v1.2.3", test.serverID); got != test.want {
				t.Fatalf("UserAgent() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClientSendsTheServerIDInItsUserAgent(t *testing.T) {
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		userAgent = request.Header.Get("User-Agent")
		writeJSONResponse(writer, 200, `{"resync":[],"config_etag":"\"c1\"","commands_available":false}`)
	}))
	defer server.Close()

	client := NewClient(server.URL+"/agent/v1", "scagt_secret", "1.2.3", server.Client()).WithServerID("01jb8z5k2m3n4p5q6r7s8t9v0w")
	if _, err := client.Heartbeat(context.Background(), HeartbeatRequest{AgentVersion: "1.2.3"}); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if want := UserAgent("1.2.3", "01jb8z5k2m3n4p5q6r7s8t9v0w"); userAgent != want || !strings.HasSuffix(userAgent, " server/01jb8z5k2m3n4p5q6r7s8t9v0w") {
		t.Fatalf("User-Agent = %q, want %q", userAgent, want)
	}
}
