package metrics

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/agent"
)

const (
	requestTimeout = 30 * time.Second
	gzipThreshold  = 8 << 10
)

// Client delivers batches to the metrics gateway with the agent's own
// credential.
type Client struct {
	Credential string
	UserAgent  string
	HTTP       *http.Client
}

// Send posts a prepared batch to endpoint. A non-2xx answer is an
// *agent.ProblemError.
func (client *Client) Send(ctx context.Context, endpoint string, prepared *Prepared) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body := io.Reader(bytes.NewReader(prepared.Body))
	compressed := len(prepared.Body) > gzipThreshold
	if compressed {
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		if _, err := writer.Write(prepared.Body); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		body = &buffer
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/samples", body)
	if err != nil {
		return nil, fmt.Errorf("create samples request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.Credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", client.UserAgent)
	if compressed {
		request.Header.Set("Content-Encoding", "gzip")
	}

	response, err := client.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, agent.DecodeProblem(response)
	}

	var result Response
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode samples response: %w", err)
	}
	return &result, nil
}

// LiveRequest is the body of POST /v1/live.
type LiveRequest struct {
	ProtocolVersion int      `json:"protocol_version"`
	Samples         []Sample `json:"samples"`
}

// LiveResponse is the answer to live samples: Watchers is how many browsers
// stream the server.
type LiveResponse struct {
	ProtocolVersion int `json:"protocol_version"`
	Forwarded       int `json:"forwarded"`
	Watchers        int `json:"watchers"`
}

// SendLive posts live samples. A non-2xx answer is an *agent.ProblemError.
func (client *Client) SendLive(ctx context.Context, endpoint string, samples []Sample) (*LiveResponse, error) {
	body, err := json.Marshal(LiveRequest{ProtocolVersion: ProtocolVersion, Samples: samples})
	if err != nil {
		return nil, fmt.Errorf("encode live samples: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/live", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create live request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.Credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", client.UserAgent)

	response, err := client.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, agent.DecodeProblem(response)
	}
	var result LiveResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode live response: %w", err)
	}
	return &result, nil
}
