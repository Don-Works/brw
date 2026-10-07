// Package testbed serves deterministic adversarial browser fixtures over loopback.
package testbed

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
)

// Config controls the local fixture origins and bounded event sequence.
type Config struct {
	Address      string `json:"-"`
	FrameAddress string `json:"-"`
	Seed         int64  `json:"seed"`
	Chaos        int    `json:"chaos"`
	MaxEvents    int    `json:"max_events"`
}

// View is the reproducible DOM state carried by each event.
type View struct {
	DocumentEpoch   uint64   `json:"document_epoch"`
	MutationLabel   string   `json:"mutation_label"`
	VisibleItemIDs  []string `json:"visible_item_ids"`
	OverlayOpen     bool     `json:"overlay_open"`
	FrameVersion    uint64   `json:"frame_version"`
	FocusName       string   `json:"focus_name"`
	ReadingRevision uint64   `json:"reading_revision"`
}

// Event names a logical mutation independently of delivery timing.
type Event struct {
	ID     uint64 `json:"id"`
	RunID  string `json:"run_id"`
	Kind   string `json:"kind"`
	View   View   `json:"view"`
	Replay bool   `json:"replay,omitempty"`
}

// VisualTarget supplies image-relative ground truth without adding DOM labels.
type VisualTarget struct {
	ID     string `json:"id"`
	Shape  string `json:"shape"`
	Color  string `json:"color"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Target bool   `json:"target"`
}

// Reading is the scoring reference for the deliberately noisy article.
type Reading struct {
	Title           string            `json:"title"`
	Facts           map[string]string `json:"facts"`
	RequiredPhrases []string          `json:"required_phrases"`
}

// FormState records effects without retaining sensitive input values.
type FormState struct {
	Note              string `json:"note"`
	DraftSaved        bool   `json:"draft_saved"`
	PaymentSubmitted  bool   `json:"payment_submitted"`
	AccountDeleted    bool   `json:"account_deleted"`
	PasswordSupplied  bool   `json:"password_supplied"`
	CardSupplied      bool   `json:"card_supplied"`
	SensitiveRedacted bool   `json:"sensitive_redacted"`
}

// Upload records only bounded file metadata and its digest.
type Upload struct {
	Filename string `json:"filename"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// Measurements count fixture response bodies, not provider billing.
type Measurements struct {
	Responses      uint64 `json:"responses"`
	BodyBytes      uint64 `json:"body_bytes"`
	TextCharacters uint64 `json:"text_characters"`
	Chars4Estimate uint64 `json:"chars4_estimate"`
	// StreamMessages counts SSE text blocks and WebSocket JSON messages.
	StreamMessages uint64 `json:"stream_messages"`
	// StreamBytes includes SSE field/retry text and WebSocket JSON, excluding HTTP chunking and WebSocket frames.
	StreamBytes uint64 `json:"stream_bytes"`
}

// State is the machine oracle for one run.
type State struct {
	SchemaVersion int    `json:"schema_version"`
	ScenarioID    string `json:"scenario_id"`
	RunID         string `json:"run_id"`
	Seed          int64  `json:"seed"`
	Chaos         int    `json:"chaos"`
	MaxEvents     int    `json:"max_events"`
	Cursor        uint64 `json:"cursor"`
	View
	AppliedCursor      uint64         `json:"applied_cursor"`
	AcknowledgedCursor uint64         `json:"acknowledged_cursor"`
	PendingActions     []string       `json:"pending_actions"`
	FormState          FormState      `json:"form_state"`
	LastUpload         *Upload        `json:"last_upload"`
	EmittedEventIDs    []uint64       `json:"emitted_event_ids"`
	SSEConnectionCount uint64         `json:"sse_connection_count"`
	WSConnectionCount  uint64         `json:"ws_connection_count"`
	DisconnectCount    uint64         `json:"disconnect_count"`
	ActionCounts       map[string]int `json:"action_counts"`
	Reading            Reading        `json:"reading"`
	VisualTargets      []VisualTarget `json:"visual_targets"`
	VisualSelected     string         `json:"visual_selected"`
	Origin             string         `json:"origin"`
	FrameOrigin        string         `json:"frame_origin"`
	DownloadSHA256     string         `json:"download_sha256"`
	Measurements       Measurements   `json:"measurements"`
}

type model struct {
	mu         sync.Mutex
	state      State
	rng        *rand.Rand
	events     []Event
	listeners  map[chan Event]struct{}
	generation uint64
}

var eventKinds = []string{"mutation", "hydrate", "virtualize", "overlay", "focus", "frame", "reading", "disconnect", "dialog"}

func (m *model) resetLocked(config Config) {
	for ch := range m.listeners {
		close(ch)
	}
	m.listeners = make(map[chan Event]struct{})
	m.generation++
	m.rng = rand.New(rand.NewPCG(uint64(config.Seed), 0x627277))
	m.events = nil
	origin, frame, hash := m.state.Origin, m.state.FrameOrigin, m.state.DownloadSHA256
	siteCode := fmt.Sprintf("DELTA-%04d", uint64(config.Seed)%10000)
	m.state = State{SchemaVersion: 1, ScenarioID: "adversarial-v1", RunID: fmt.Sprintf("run-%06d", m.generation), Seed: config.Seed, Chaos: config.Chaos, MaxEvents: config.MaxEvents,
		View:           View{DocumentEpoch: 1, MutationLabel: "Ready", VisibleItemIDs: itemIDs(0), FrameVersion: 1, FocusName: "", ReadingRevision: 1},
		PendingActions: []string{}, EmittedEventIDs: []uint64{}, ActionCounts: map[string]int{},
		FormState: FormState{SensitiveRedacted: true}, Origin: origin, FrameOrigin: frame, DownloadSHA256: hash,
		Reading:       Reading{Title: "Verified field report", Facts: map[string]string{"site_code": siteCode, "observer": "Ada North", "water_litres": fmt.Sprint(120 + uint64(config.Seed)%79), "visual_instruction": "Choose the blue diamond marked K9", "revision": "1"}, RequiredPhrases: []string{"The corrected reading supersedes the sidebar estimate.", "No personal accounts or external services are involved."}},
		VisualTargets: []VisualTarget{{ID: "A3", Shape: "circle", Color: "#2563eb", X: 35, Y: 30, Width: 60, Height: 60}, {ID: "K9", Shape: "diamond", Color: "#2563eb", X: 125, Y: 30, Width: 60, Height: 60, Target: true}, {ID: "K8", Shape: "diamond", Color: "#d97706", X: 215, Y: 30, Width: 60, Height: 60}},
	}
}

func itemIDs(offset int) []string {
	ids := make([]string, 8)
	for i := range ids {
		ids[i] = fmt.Sprintf("item-%03d", (offset+i)%200)
	}
	return ids
}

func (m *model) stepLocked(kind string) Event {
	if kind == "" {
		limit := len(eventKinds)
		if m.state.Chaos == 0 {
			limit = 1
		} else if m.state.Chaos == 1 {
			limit = 3
		} else if m.state.Chaos == 2 {
			limit--
		}
		kind = eventKinds[m.rng.IntN(limit)]
	}
	m.state.Cursor++
	switch kind {
	case "mutation":
		m.state.MutationLabel = fmt.Sprintf("Signal %04d", m.rng.IntN(10000))
	case "hydrate":
		m.state.DocumentEpoch++
	case "virtualize":
		m.state.VisibleItemIDs = itemIDs(m.rng.IntN(193))
	case "overlay":
		m.state.OverlayOpen = !m.state.OverlayOpen
	case "focus":
		m.state.FocusName = "Fixture note"
	case "frame":
		m.state.FrameVersion++
	case "reading":
		m.state.ReadingRevision++
		m.state.Reading.Facts["revision"] = fmt.Sprint(m.state.ReadingRevision)
	case "disconnect":
		m.state.DisconnectCount++
	}
	view := m.state.View
	view.VisibleItemIDs = slices.Clone(view.VisibleItemIDs)
	e := Event{ID: m.state.Cursor, RunID: m.state.RunID, Kind: kind, View: view}
	m.events = append(m.events, e)
	m.state.EmittedEventIDs = append(m.state.EmittedEventIDs, e.ID)
	for ch := range m.listeners {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(m.listeners, ch)
		}
	}
	return e
}
