package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/profiler"
	"github.com/chromedp/cdproto/tracing"
	"github.com/chromedp/chromedp"
)

// profileState is the per-tab in-flight capture bookkeeping. Zero value usable.
type profileState struct {
	mu     sync.Mutex
	traces map[string]*traceCapture
	cpus   map[string]bool
}

func (p *profileState) initLocked() {
	if p.traces == nil {
		p.traces = map[string]*traceCapture{}
	}
	if p.cpus == nil {
		p.cpus = map[string]bool{}
	}
}

// traceCapture accumulates one trace between start and stop. done closes when
// Chrome reports the trace complete, which can arrive after tracing.End returns.
type traceCapture struct {
	chunks []json.RawMessage
	done   chan struct{}
	loss   bool
	closed bool
}

// Profile starts or stops a Chrome performance trace or a V8 CPU profile. The
// returned Data is stored as an artifact by the surface that owns the store;
// Manager itself never writes to disk.
func (m *Manager) Profile(ctx context.Context, opts ProfileOptions) (ProfileResult, error) {
	req, err := NormalizeProfile(opts)
	if err != nil {
		return ProfileResult{}, err
	}
	tabID, _, cancel, err := m.contextForTab(ctx, "")
	if err != nil {
		return ProfileResult{}, err
	}
	cancel()
	liveCtx, err := m.tabContext(tabID)
	if err != nil {
		return ProfileResult{}, err
	}

	if req.Action == "start" {
		return m.startProfile(ctx, liveCtx, tabID, req)
	}
	return m.stopProfile(ctx, liveCtx, tabID, req)
}

func (m *Manager) startProfile(ctx context.Context, liveCtx context.Context, tabID string, req ProfileOptions) (ProfileResult, error) {
	m.profiles.mu.Lock()
	m.profiles.initLocked()
	switch req.Kind {
	case ProfileKindTrace:
		if _, running := m.profiles.traces[tabID]; running {
			m.profiles.mu.Unlock()
			return ProfileResult{}, fmt.Errorf("a trace is already running on this tab; stop it before starting another")
		}
		capture := &traceCapture{done: make(chan struct{})}
		m.profiles.traces[tabID] = capture
		m.profiles.mu.Unlock()

		chromedp.ListenTarget(liveCtx, func(ev any) {
			switch e := ev.(type) {
			case *tracing.EventDataCollected:
				chunk, merr := json.Marshal(e.Value)
				if merr != nil {
					return
				}
				m.profiles.mu.Lock()
				if live := m.profiles.traces[tabID]; live == capture && !live.closed {
					live.chunks = append(live.chunks, chunk)
				}
				m.profiles.mu.Unlock()
			case *tracing.EventTracingComplete:
				m.profiles.mu.Lock()
				if live := m.profiles.traces[tabID]; live == capture && !live.closed {
					live.loss = e.DataLossOccurred
					live.closed = true
					close(live.done)
				}
				m.profiles.mu.Unlock()
			}
		})

		if err := chromedp.Run(liveCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			return tracing.Start().
				WithTransferMode(tracing.TransferModeReportEvents).
				WithTraceConfig(&tracing.TraceConfig{IncludedCategories: req.Categories}).
				Do(runCtx)
		})); err != nil {
			m.profiles.mu.Lock()
			delete(m.profiles.traces, tabID)
			m.profiles.mu.Unlock()
			return ProfileResult{}, fmt.Errorf("start trace: %w", err)
		}
		return ProfileResult{OK: true, Action: "start", Kind: ProfileKindTrace, TabID: tabID, Running: true,
			Note: "tracing; call brw_profile action=stop to finish and store the trace"}, nil

	case ProfileKindCPU:
		if m.profiles.cpus[tabID] {
			m.profiles.mu.Unlock()
			return ProfileResult{}, fmt.Errorf("a CPU profile is already running on this tab; stop it before starting another")
		}
		m.profiles.cpus[tabID] = true
		m.profiles.mu.Unlock()
		if err := chromedp.Run(liveCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			if err := profiler.Enable().Do(runCtx); err != nil {
				return err
			}
			return profiler.Start().Do(runCtx)
		})); err != nil {
			m.profiles.mu.Lock()
			delete(m.profiles.cpus, tabID)
			m.profiles.mu.Unlock()
			return ProfileResult{}, fmt.Errorf("start CPU profile: %w", err)
		}
		return ProfileResult{OK: true, Action: "start", Kind: ProfileKindCPU, TabID: tabID, Running: true,
			Note: "profiling; call brw_profile action=stop to finish and store the profile"}, nil
	}
	return ProfileResult{}, fmt.Errorf("unknown profile kind %q", req.Kind)
}

func (m *Manager) stopProfile(ctx context.Context, liveCtx context.Context, tabID string, req ProfileOptions) (ProfileResult, error) {
	switch req.Kind {
	case ProfileKindTrace:
		m.profiles.mu.Lock()
		m.profiles.initLocked()
		capture := m.profiles.traces[tabID]
		m.profiles.mu.Unlock()
		if capture == nil {
			return ProfileResult{}, fmt.Errorf("no trace is running on this tab; start one with action=start")
		}
		if err := chromedp.Run(liveCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			return tracing.End().Do(runCtx)
		})); err != nil {
			return ProfileResult{}, fmt.Errorf("stop trace: %w", err)
		}
		select {
		case <-capture.done:
		case <-time.After(30 * time.Second):
			// Take whatever arrived rather than hanging on an event Chrome did
			// not send.
		}
		m.profiles.mu.Lock()
		delete(m.profiles.traces, tabID)
		m.profiles.mu.Unlock()

		data := assembleTrace(capture.chunks)
		result := ProfileResult{OK: true, Action: "stop", Kind: ProfileKindTrace, TabID: tabID, Bytes: len(data), Data: data}
		if capture.loss {
			result.Note = "Chrome reported trace data loss (the ring buffer wrapped); increase the capture cadence or shorten the window"
		}
		return result, nil

	case ProfileKindCPU:
		m.profiles.mu.Lock()
		m.profiles.initLocked()
		if !m.profiles.cpus[tabID] {
			m.profiles.mu.Unlock()
			return ProfileResult{}, fmt.Errorf("no CPU profile is running on this tab; start one with action=start")
		}
		m.profiles.mu.Unlock()
		var profile *profiler.Profile
		if err := chromedp.Run(liveCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			p, derr := profiler.Stop().Do(runCtx)
			profile = p
			return derr
		})); err != nil {
			return ProfileResult{}, fmt.Errorf("stop CPU profile: %w", err)
		}
		m.profiles.mu.Lock()
		delete(m.profiles.cpus, tabID)
		m.profiles.mu.Unlock()
		if profile == nil {
			return ProfileResult{OK: true, Action: "stop", Kind: ProfileKindCPU, TabID: tabID,
				Note: "Chrome returned no profile"}, nil
		}
		data, err := json.Marshal(profile)
		if err != nil {
			return ProfileResult{}, fmt.Errorf("encode CPU profile: %w", err)
		}
		return ProfileResult{OK: true, Action: "stop", Kind: ProfileKindCPU, TabID: tabID, Bytes: len(data), Data: data}, nil
	}
	return ProfileResult{}, fmt.Errorf("unknown profile kind %q", req.Kind)
}

// assembleTrace concatenates the per-event arrays Chrome delivered into one
// trace JSON array.
func assembleTrace(chunks []json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	for _, chunk := range chunks {
		inner := bytes.TrimSpace(chunk)
		if len(inner) < 2 || inner[0] != '[' || inner[len(inner)-1] != ']' {
			continue
		}
		inner = bytes.TrimSpace(inner[1 : len(inner)-1])
		if len(inner) == 0 {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.Write(inner)
		first = false
	}
	buf.WriteByte(']')
	return buf.Bytes()
}
