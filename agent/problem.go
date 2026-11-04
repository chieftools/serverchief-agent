package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Problem types from the protocol's behaviour table.
const (
	ProblemInvalidRequest      = "invalid_request"
	ProblemUnauthenticated     = "unauthenticated"
	ProblemRevoked             = "revoked"
	ProblemEnrollmentConflict  = "enrollment_conflict"
	ProblemLimitReached        = "limit_reached"
	ProblemPayloadTooLarge     = "payload_too_large"
	ProblemValidationFailed    = "validation_failed"
	ProblemUnsupportedRevision = "unsupported_revision"
)

// ErrStopped is returned by Run when the backend no longer accepts the
// agent's credential. The service must not be restarted automatically.
var ErrStopped = errors.New("agent stopped")

// ProblemError is an application/problem+json answer (fixtures/problem.json),
// or a synthesized one for a non-2xx answer without a problem body.
type ProblemError struct {
	Status int                 `json:"status"`
	Type   string              `json:"type"`
	Title  string              `json:"title"`
	Detail string              `json:"detail"`
	Errors map[string][]string `json:"errors,omitempty"`
	// RetryAfter comes from the Retry-After header or the retry_after field.
	RetryAfter time.Duration `json:"-"`
	// ExpectedOffset is the expected_offset of a 409 offset_mismatch for
	// live output, nil without one.
	ExpectedOffset *int64 `json:"-"`
}

func (problem *ProblemError) Error() string {
	message := problem.Detail
	if message == "" {
		message = problem.Title
	}
	if message == "" {
		message = http.StatusText(problem.Status)
	}
	label := strconv.Itoa(problem.Status)
	if problem.Type != "" {
		label += " " + problem.Type
	}
	if len(problem.Errors) > 0 {
		message += " (" + formatValidationErrors(problem.Errors) + ")"
	}
	return fmt.Sprintf("server answered %s: %s", label, message)
}

func formatValidationErrors(errors map[string][]string) string {
	fields := make([]string, 0, len(errors))
	for field, messages := range errors {
		fields = append(fields, field+": "+strings.Join(messages, "; "))
	}
	slices.Sort(fields)
	return strings.Join(fields, ", ")
}

// decodeProblem builds a ProblemError from a non-2xx response. A body that
// is not a problem document still yields a usable error.
func decodeProblem(response *http.Response) *ProblemError {
	problem := &ProblemError{Status: response.StatusCode}
	body, _ := readLimited(response.Body, 64<<10)
	var document struct {
		Type           string              `json:"type"`
		Title          string              `json:"title"`
		Status         int                 `json:"status"`
		Detail         string              `json:"detail"`
		RetryAfter     json.Number         `json:"retry_after"`
		Errors         map[string][]string `json:"errors"`
		ExpectedOffset *int64              `json:"expected_offset"`
	}
	if len(body) > 0 && json.Unmarshal(body, &document) == nil {
		problem.Type = document.Type
		problem.Title = document.Title
		problem.Detail = document.Detail
		problem.Errors = document.Errors
		if document.ExpectedOffset != nil && *document.ExpectedOffset >= 0 {
			problem.ExpectedOffset = document.ExpectedOffset
		}
		if seconds, err := document.RetryAfter.Float64(); err == nil && seconds > 0 {
			problem.RetryAfter = time.Duration(seconds * float64(time.Second))
		}
	} else if text := strings.TrimSpace(string(body)); text != "" && len(text) < 200 && !strings.HasPrefix(text, "<") {
		problem.Detail = text
	}
	if wait, ok := parseRetryAfter(response.Header.Get("Retry-After"), time.Now()); ok {
		problem.RetryAfter = wait
	}
	return problem
}

// DecodeProblem is decodeProblem for other services that answer with the
// same problem documents, such as the metrics gateway.
func DecodeProblem(response *http.Response) *ProblemError {
	return decodeProblem(response)
}

// parseRetryAfter accepts delay-seconds and HTTP-date forms.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// failureKind says how the daemon reacts to a failed request.
type failureKind int

const (
	// failureTransient: network errors and 5xx. Exponential backoff.
	failureTransient failureKind = iota
	// failureThrottled: 429 or 503. Honour Retry-After.
	failureThrottled
	// failureRejected: 400, 413, 422 and other 4xx. The body would fail
	// again, so it is not retried unchanged; the normal schedule resumes.
	failureRejected
	// failureUnsupported: 426. Heartbeat hourly until an update fixes it.
	failureUnsupported
	// failureFatal: 401. Stop every loop.
	failureFatal
)

func classify(err error) (failureKind, *ProblemError) {
	var problem *ProblemError
	if !errors.As(err, &problem) {
		return failureTransient, nil
	}
	switch {
	case problem.Status == http.StatusUnauthorized:
		return failureFatal, problem
	case problem.Status == http.StatusUpgradeRequired:
		return failureUnsupported, problem
	case problem.Status == http.StatusTooManyRequests || problem.Status == http.StatusServiceUnavailable:
		return failureThrottled, problem
	case problem.Status >= 500:
		return failureTransient, problem
	case problem.Status >= 400:
		return failureRejected, problem
	default:
		return failureTransient, problem
	}
}

// IsUnauthorized reports whether err is a 401 from the backend.
func IsUnauthorized(err error) bool {
	kind, _ := classify(err)
	return kind == failureFatal
}
