package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// rebootArgs are the arguments of reboot. Unknown arguments are rejected,
// so one a later revision adds is never ignored silently.
type rebootArgs struct {
	NotAfter json.RawMessage `json:"not_after"`
}

// parseRebootArgs validates reboot's arguments and returns its not_after
// deadline, zero for none.
func parseRebootArgs(raw json.RawMessage) (time.Time, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}, nil
	}
	var args rebootArgs
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return time.Time{}, err
	}
	return parseDeadline(args.NotAfter)
}

// parseDeadline reads a command's not_after: an RFC 3339 time, or null or
// absent for none (the zero time).
func parseDeadline(raw json.RawMessage) (time.Time, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}, nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err != nil {
		return time.Time{}, errors.New("not_after must be null or an RFC 3339 time")
	}
	deadline, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, errors.New("not_after must be null or an RFC 3339 time")
	}
	return deadline.UTC(), nil
}

// passed reports whether now reached a deadline; the zero deadline never
// passes.
func passed(deadline, now time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}

// waitUntil is the end of a wait for the package manager: its patience,
// or the deadline when that comes first.
func waitUntil(patience, deadline time.Time) time.Time {
	if !deadline.IsZero() && deadline.Before(patience) {
		return deadline
	}
	return patience
}

func tooLateDetail(deadline time.Time) string {
	return fmt.Sprintf("the command was not started before its deadline (not_after %s)", deadline.UTC().Format(time.RFC3339))
}
