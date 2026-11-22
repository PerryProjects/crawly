package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is the envelope layout version written to Redis.
const SchemaVersion = 1

// Status is the outcome stored in an envelope.
type Status string

// Envelope statuses.
const (
	StatusOK       Status = "ok"
	StatusNotFound Status = "not_found"
	StatusError    Status = "error"
)

// Envelope is the JSON document stored under a cache key.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Status        Status          `json:"status"`
	FetchedAt     time.Time       `json:"fetched_at"`
	Data          json.RawMessage `json:"data,omitempty"`
	ErrorCode     string          `json:"error_code,omitempty"`
}

// Encode serializes the envelope.
func (e Envelope) Encode() ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("cache: encode envelope: %w", err)
	}
	return b, nil
}

// ErrBadEnvelope marks a stored value that cannot be used.
var ErrBadEnvelope = errors.New("cache: bad envelope")

// DecodeEnvelope parses and validates a stored envelope.
func DecodeEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadEnvelope, err)
	}
	if e.SchemaVersion != SchemaVersion {
		return Envelope{}, fmt.Errorf("%w: schema_version %d", ErrBadEnvelope, e.SchemaVersion)
	}
	switch e.Status {
	case StatusOK:
		if len(e.Data) == 0 {
			return Envelope{}, fmt.Errorf("%w: status ok without data", ErrBadEnvelope)
		}
	case StatusNotFound, StatusError:
	default:
		return Envelope{}, fmt.Errorf("%w: status %q", ErrBadEnvelope, e.Status)
	}
	return e, nil
}
