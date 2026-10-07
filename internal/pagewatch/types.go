package pagewatch

import (
	"context"
	"time"
)

// RegisterOptions defines a fixed, read-only page observation.
type RegisterOptions struct {
	ID                string `json:"id,omitempty"`
	URL               string `json:"url"`
	Selector          string `json:"selector,omitempty"`
	Mode              string `json:"mode,omitempty"`
	IntervalMS        int    `json:"interval_ms,omitempty"`
	RefreshIntervalMS int    `json:"refresh_interval_ms,omitempty"`
}

// Watcher reports one durable page watcher without its sampled content.
type Watcher struct {
	RegisterOptions
	Enabled      bool       `json:"enabled"`
	Status       string     `json:"status"`
	TabID        string     `json:"tab_id,omitempty"`
	Seq          uint64     `json:"seq"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSampleAt *time.Time `json:"last_sample_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

// ManageOptions lists, removes, pauses, or resumes a watcher.
type ManageOptions struct {
	Action string `json:"action,omitempty"`
	ID     string `json:"id,omitempty"`
}

// ManageResult reports the remaining watchers after a management operation.
type ManageResult struct {
	Watchers []Watcher `json:"watchers"`
	Removed  bool      `json:"removed,omitempty"`
}

// Event records a change signal; page text and titles are never included.
type Event struct {
	WatcherID string    `json:"watcher_id"`
	Seq       uint64    `json:"seq"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason,omitempty"`
	URL       string    `json:"url"`
	Mode      string    `json:"mode"`
	Digest    string    `json:"digest"`
	Count     int       `json:"count"`
}

// EventsOptions selects a bounded window of durable events.
type EventsOptions struct {
	WatcherID string `json:"watcher_id"`
	SinceSeq  uint64 `json:"since_seq,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// EventsResult includes a gap when the requested cursor predates retention.
type EventsResult struct {
	WatcherID string  `json:"watcher_id"`
	Events    []Event `json:"events"`
	LatestSeq uint64  `json:"latest_seq"`
	OldestSeq uint64  `json:"oldest_seq"`
	Gap       bool    `json:"gap"`
	HasMore   bool    `json:"has_more"`
}

// API is implemented by the browser-host service and its HTTP proxy.
type API interface {
	WatchPage(context.Context, RegisterOptions) (Watcher, error)
	PageWatchers(context.Context, ManageOptions) (ManageResult, error)
	PageEvents(context.Context, EventsOptions) (EventsResult, error)
}
