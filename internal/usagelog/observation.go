package usagelog

import (
	"context"
	"encoding/json"
	"strings"
)

type Observation struct {
	SnapshotMode     string
	OutputFormat     string
	DeltaRequested   *bool
	DeltaReturned    *bool
	ResultTruncated  *bool
	ReadSettleMS     *int64
	ElementLimit     *int64
	ReturnedElements *int64
}

type observationContextKey struct{}

func Flag(value bool) *bool { return &value }

func WithObservation(ctx context.Context, value *Observation) context.Context {
	return context.WithValue(ctx, observationContextKey{}, value)
}

func ObservationFromContext(ctx context.Context) *Observation {
	value, _ := ctx.Value(observationContextKey{}).(*Observation)
	return value
}

func (o *Observation) Apply(event *Event) {
	event.SnapshotMode = o.SnapshotMode
	event.OutputFormat = o.OutputFormat
	event.DeltaRequested = o.DeltaRequested
	event.DeltaReturned = o.DeltaReturned
	event.ResultTruncated = o.ResultTruncated
	event.ReadSettleMS = o.ReadSettleMS
	event.ElementLimit = o.ElementLimit
	event.ReturnedElements = o.ReturnedElements
	SanitizeObservation(event)
}

func ObservationOptions(operation string, input []byte) *Observation {
	if operation != "brw_snapshot" && operation != "brw_read" && operation != "brw_list_tabs" {
		return nil
	}
	var options struct {
		Mode     string `json:"mode"`
		Format   string `json:"format"`
		Since    int64  `json:"since"`
		Limit    int64  `json:"limit"`
		SettleMS *int64 `json:"settle_ms"`
	}
	if len(input) > 0 && json.Unmarshal(input, &options) != nil {
		return nil
	}
	result := &Observation{}
	if operation == "brw_list_tabs" {
		return result
	}
	if operation == "brw_read" {
		if options.SettleMS != nil && *options.SettleMS >= 0 && *options.SettleMS <= 5000 {
			result.ReadSettleMS = options.SettleMS
		}
		return result
	}
	mode := strings.ToLower(strings.TrimSpace(options.Mode))
	if mode == "" {
		mode = "frontier"
	}
	switch mode {
	case "frontier", "all", "form_lens":
		result.SnapshotMode = mode
	}
	format := strings.ToLower(options.Format)
	if format == "" {
		format = "json"
	}
	switch format {
	case "json", "compact":
		result.OutputFormat = format
	}
	result.DeltaRequested = Flag(options.Since > 0)
	limit := options.Limit
	if mode == "frontier" && limit <= 0 {
		limit = 40
	} else if limit < 0 {
		limit = 0
	}
	if limit <= 1<<20 {
		result.ElementLimit = Count(limit)
	}
	return result
}

func ValidObservation(event Event) bool {
	switch event.SnapshotMode {
	case "", "frontier", "all", "form_lens":
	default:
		return false
	}
	switch event.OutputFormat {
	case "", "json", "compact", "human":
	default:
		return false
	}
	if event.ReadSettleMS != nil && (*event.ReadSettleMS < 0 || *event.ReadSettleMS > 5000) {
		return false
	}
	for _, count := range []*int64{event.ElementLimit, event.ReturnedElements} {
		if count != nil && (*count < 0 || *count > 1<<20) {
			return false
		}
	}
	return true
}

func SanitizeObservation(event *Event) {
	switch event.SnapshotMode {
	case "frontier", "all", "form_lens":
	default:
		event.SnapshotMode = ""
	}
	switch event.OutputFormat {
	case "json", "compact", "human":
	default:
		event.OutputFormat = ""
	}
	if event.ReadSettleMS != nil && (*event.ReadSettleMS < 0 || *event.ReadSettleMS > 5000) {
		event.ReadSettleMS = nil
	}
	for _, count := range []**int64{&event.ElementLimit, &event.ReturnedElements} {
		if *count != nil && (**count < 0 || **count > 1<<20) {
			*count = nil
		}
	}
}
