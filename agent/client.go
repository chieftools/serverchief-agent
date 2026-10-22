package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"go.chief.tools/serverchief-agent/inventory"
)

const (
	// GzipThreshold is the request body size above which bodies are sent
	// with Content-Encoding: gzip.
	GzipThreshold = 8 << 10
	// MaximumBodyBytes is the backend's limit after decompression.
	MaximumBodyBytes = 16 << 20

	maximumResponseBytes = 1 << 20
)

// Client talks to the backend. HTTPClient is the implementation; tests
// substitute fakes.
type Client interface {
	Heartbeat(ctx context.Context, request HeartbeatRequest) (HeartbeatResponse, error)
	SendInventory(ctx context.Context, envelope *inventory.Envelope) (InventoryResponse, error)
	// FetchConfig sends If-None-Match: etag when etag is set.
	FetchConfig(ctx context.Context, etag string) (ConfigResult, error)
	// AuthorizeChannel signs a realtime subscription.
	AuthorizeChannel(ctx context.Context, authEndpoint, socketID, channel string) (string, error)
}

// ConfigResult is the answer to GET /config.
type ConfigResult struct {
	Config      ServerConfig
	NotModified bool
}

// HTTPClient implements the /agent/v1 protocol over HTTPS.
type HTTPClient struct {
	Endpoint   string
	Credential string
	Version    string
	HTTP       *http.Client
}

// NewClient returns a protocol client. credential may be empty for
// enrollment. A nil httpClient uses http.DefaultClient.
func NewClient(endpoint, credential, version string, httpClient *http.Client) *HTTPClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &HTTPClient{
		Endpoint:   strings.TrimRight(endpoint, "/"),
		Credential: credential,
		Version:    normalizeVersion(version),
		HTTP:       httpClient,
	}
}

// UserAgent is "serverchief-agent/<version> (linux; <arch>)".
func (client *HTTPClient) UserAgent() string {
	return UserAgent(client.Version)
}

// UserAgent formats the agent's User-Agent header.
func UserAgent(version string) string {
	return fmt.Sprintf("serverchief-agent/%s (%s; %s)", normalizeVersion(version), runtime.GOOS, runtime.GOARCH)
}

func normalizeVersion(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return "dev"
	}
	return version
}

// Enroll calls POST /enroll. It is the only request without a bearer.
func (client *HTTPClient) Enroll(ctx context.Context, request EnrollmentRequest) (EnrollmentResponse, error) {
	var response EnrollmentResponse
	if _, err := client.do(ctx, http.MethodPost, "/enroll", false, request, nil, &response); err != nil {
		return EnrollmentResponse{}, err
	}
	if response.ServerID == "" || response.Generation == 0 {
		return EnrollmentResponse{}, errors.New("enrollment response lacks server_id or generation")
	}
	return response, nil
}

// Heartbeat calls POST /heartbeat.
func (client *HTTPClient) Heartbeat(ctx context.Context, request HeartbeatRequest) (HeartbeatResponse, error) {
	if request.Hashes == nil {
		request.Hashes = map[string]string{}
	}
	var response HeartbeatResponse
	_, err := client.do(ctx, http.MethodPost, "/heartbeat", true, request, nil, &response)
	return response, err
}

// SendInventory calls POST /inventory.
func (client *HTTPClient) SendInventory(ctx context.Context, envelope *inventory.Envelope) (InventoryResponse, error) {
	var response InventoryResponse
	_, err := client.do(ctx, http.MethodPost, "/inventory", true, envelope, nil, &response)
	return response, err
}

// FetchConfig calls GET /config, answering NotModified on 304.
func (client *HTTPClient) FetchConfig(ctx context.Context, etag string) (ConfigResult, error) {
	headers := http.Header{}
	if etag != "" {
		headers.Set("If-None-Match", etag)
	}
	var config ServerConfig
	response, err := client.do(ctx, http.MethodGet, "/config", true, nil, headers, &config)
	if err != nil {
		return ConfigResult{}, err
	}
	if response.StatusCode == http.StatusNotModified {
		return ConfigResult{NotModified: true}, nil
	}
	if config.ETag == "" {
		config.ETag = response.Header.Get("ETag")
	}
	return ConfigResult{Config: config}, nil
}

// AuthorizeChannel calls the broadcasting auth endpoint with the form
// fields socket_id and channel_name and returns the "auth" signature. The
// credential is only ever sent to the enrolled endpoint's origin: an auth
// endpoint elsewhere is replaced by <endpoint>/broadcasting/auth.
func (client *HTTPClient) AuthorizeChannel(ctx context.Context, authEndpoint, socketID, channel string) (string, error) {
	target := client.authEndpoint(authEndpoint)
	form := url.Values{"socket_id": {socketID}, "channel_name": {channel}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create channel auth request: %w", err)
	}
	client.setHeaders(request, true)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := client.HTTP.Do(request)
	if err != nil {
		return "", fmt.Errorf("authorize channel: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", decodeProblem(response)
	}
	var answer struct {
		Auth string `json:"auth"`
	}
	data, err := readLimited(response.Body, maximumResponseBytes)
	if err != nil {
		return "", fmt.Errorf("read channel auth: %w", err)
	}
	if err := json.Unmarshal(data, &answer); err != nil || answer.Auth == "" {
		return "", fmt.Errorf("channel auth response has no signature")
	}
	return answer.Auth, nil
}

func (client *HTTPClient) authEndpoint(candidate string) string {
	fallback := client.Endpoint + "/broadcasting/auth"
	if candidate == "" {
		return fallback
	}
	endpoint, err := url.Parse(client.Endpoint)
	if err != nil {
		return fallback
	}
	target, err := url.Parse(candidate)
	if err != nil || target.Scheme != endpoint.Scheme || target.Host != endpoint.Host {
		return fallback
	}
	return candidate
}

func (client *HTTPClient) setHeaders(request *http.Request, authenticated bool) {
	request.Header.Set(ProtocolHeader, ProtocolRevision)
	request.Header.Set("User-Agent", client.UserAgent())
	request.Header.Set("Accept", "application/json, application/problem+json")
	if authenticated && client.Credential != "" {
		request.Header.Set("Authorization", "Bearer "+client.Credential)
	}
}

// do sends a JSON request and decodes a 2xx JSON answer into target. A 304
// is returned as is; every other status becomes a *ProblemError.
func (client *HTTPClient) do(ctx context.Context, method, path string, authenticated bool, body any, headers http.Header, target any) (*http.Response, error) {
	var reader io.Reader
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode %s request: %w", path, err)
		}
		if len(encoded) > MaximumBodyBytes {
			return nil, &ProblemError{Status: http.StatusRequestEntityTooLarge, Type: ProblemPayloadTooLarge,
				Detail: fmt.Sprintf("request body is %d bytes, above the %d byte limit; not sent", len(encoded), MaximumBodyBytes)}
		}
		reader = bytes.NewReader(encoded)
	}
	compressed := false
	if len(encoded) > GzipThreshold {
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		if _, err := writer.Write(encoded); err != nil {
			return nil, fmt.Errorf("compress %s request: %w", path, err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("compress %s request: %w", path, err)
		}
		reader = &buffer
		compressed = true
	}

	request, err := http.NewRequestWithContext(ctx, method, client.Endpoint+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", path, err)
	}
	client.setHeaders(request, authenticated)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if compressed {
		request.Header.Set("Content-Encoding", "gzip")
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	response, err := client.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusNotModified:
		return response, nil
	case response.StatusCode < 200 || response.StatusCode > 299:
		return response, decodeProblem(response)
	}
	data, err := readLimited(response.Body, maximumResponseBytes)
	if err != nil {
		return response, fmt.Errorf("read %s response: %w", path, err)
	}
	if target != nil {
		if err := json.Unmarshal(data, target); err != nil {
			return response, fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return response, nil
}

func readLimited(reader io.Reader, maximum int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximum {
		return nil, errors.New("response exceeds its size limit")
	}
	return data, nil
}
