// Package metrics samples host usage on a fixed interval, keeps the samples
// in an on-disk spool and delivers them to the metrics gateway (metrics
// protocol v1, serverchief/protocol/metrics/v1).
package metrics

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the metrics protocol the agent speaks.
const ProtocolVersion = 1

// Point is one value of a metric; Instance names the mount point, device or
// interface, or is empty.
type Point struct {
	Metric   string
	Instance string
	Value    float64
}

// MarshalJSON encodes a point as the protocol's [metric, instance, value].
func (point Point) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{point.Metric, point.Instance, point.Value})
}

// UnmarshalJSON decodes [metric, instance, value].
func (point *Point) UnmarshalJSON(data []byte) error {
	var triple []json.RawMessage
	if err := json.Unmarshal(data, &triple); err != nil || len(triple) != 3 {
		return fmt.Errorf("a point is a [metric, instance, value] triple")
	}
	if err := json.Unmarshal(triple[0], &point.Metric); err != nil {
		return err
	}
	if err := json.Unmarshal(triple[1], &point.Instance); err != nil {
		return err
	}
	return json.Unmarshal(triple[2], &point.Value)
}

// Sample is every point taken at one moment, At in Unix seconds.
type Sample struct {
	At     int64   `json:"at"`
	Points []Point `json:"points"`
}

// Batch is the body of POST /v1/samples.
type Batch struct {
	ProtocolVersion int      `json:"protocol_version"`
	BatchID         string   `json:"batch_id"`
	Samples         []Sample `json:"samples"`
}

// Response is the answer to a stored batch.
type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	BatchID         string `json:"batch_id"`
	Accepted        int    `json:"accepted"`
	Dropped         int    `json:"dropped"`
	Points          int    `json:"points"`
}

// MaximumBatchSamples is the most samples one batch carries.
const MaximumBatchSamples = 60
