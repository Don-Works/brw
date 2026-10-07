package testbed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) subscribe(runID string, cursor uint64, kind string) ([]Event, chan Event, error) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	if runID != s.m.state.RunID || cursor > s.m.state.Cursor {
		return nil, nil, errors.New("stale run or invalid cursor")
	}
	select {
	case <-s.closed:
		return nil, nil, errors.New("fixture closed")
	default:
	}
	if len(s.m.listeners) >= 64 {
		return nil, nil, errors.New("fixture connection budget exhausted")
	}
	ch := make(chan Event, 32)
	s.m.listeners[ch] = struct{}{}
	backlog := make([]Event, 0)
	for _, e := range s.m.events {
		if e.ID > cursor {
			e.Replay = true
			backlog = append(backlog, e)
		}
	}
	if kind == "sse" {
		s.m.state.SSEConnectionCount++
	} else {
		s.m.state.WSConnectionCount++
	}
	return backlog, ch, nil
}

func (s *Server) unsubscribe(ch chan Event) {
	s.m.mu.Lock()
	delete(s.m.listeners, ch)
	s.m.mu.Unlock()
}

func streamCursor(r *http.Request) (uint64, error) {
	text := r.URL.Query().Get("cursor")
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		text = last
	}
	if text == "" {
		return 0, nil
	}
	return strconv.ParseUint(text, 10, 64)
}

func (s *Server) streamMeasured(n int) {
	s.m.mu.Lock()
	s.m.state.Measurements.StreamMessages++
	s.m.state.Measurements.StreamBytes += uint64(n)
	s.m.mu.Unlock()
}

func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	_, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	cursor, err := streamCursor(r)
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	runID := r.URL.Query().Get("run_id")
	backlog, ch, err := s.subscribe(runID, cursor, "sse")
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer s.unsubscribe(ch)
	w.Header().Set("Content-Type", "text/event-stream")
	controller := http.NewResponseController(w)
	write := func(body []byte) bool {
		if controller.SetWriteDeadline(time.Now().Add(3*time.Second)) != nil {
			return false
		}
		n, err := w.Write(body)
		if err != nil || n != len(body) || controller.Flush() != nil {
			return false
		}
		s.streamMeasured(n)
		return true
	}
	if !write([]byte("retry: 150\nevent: hello\ndata: {\"type\":\"hello\"}\n\n")) {
		return
	}
	send := func(e Event) bool {
		body, _ := json.Marshal(e)
		return write([]byte(fmt.Sprintf("id: %d\ndata: %s\n\n", e.ID, body)))
	}
	for _, e := range backlog {
		if !send(e) {
			return
		}
	}
	heartbeat := time.NewTicker(500 * time.Millisecond)
	defer heartbeat.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !write([]byte("event: heartbeat\ndata: {\"type\":\"heartbeat\"}\n\n")) {
				return
			}
		case e, open := <-ch:
			if !open {
				return
			}
			if !send(e) || e.Kind == "disconnect" {
				return
			}
		}
	}
}

type wsMessage struct {
	Type string `json:"type"`
	acknowledgement
}

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	cursor, err := streamCursor(r)
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 10)
	runID := r.URL.Query().Get("run_id")
	backlog, ch, err := s.subscribe(runID, cursor, "ws")
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "stale fixture run or cursor")
		return
	}
	defer s.unsubscribe(ch)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	messages := make(chan wsMessage, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message wsMessage
			if kind != websocket.MessageText || !validJSON(data, &message) {
				return
			}
			select {
			case messages <- message:
			case <-ctx.Done():
				return
			}
		}
	}()
	send := func(value any) bool {
		data, err := json.Marshal(value)
		if err != nil {
			return false
		}
		writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		if conn.Write(writeCtx, websocket.MessageText, data) != nil {
			return false
		}
		s.streamMeasured(len(data))
		return true
	}
	if !send(map[string]any{"type": "hello", "run_id": r.URL.Query().Get("run_id"), "cursor": cursor}) {
		return
	}
	for _, e := range backlog {
		if !send(e) {
			return
		}
	}
	heartbeat := time.NewTicker(500 * time.Millisecond)
	defer heartbeat.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-heartbeat.C:
			if !send(map[string]string{"type": "heartbeat"}) {
				return
			}
		case message := <-messages:
			switch message.Type {
			case "ack":
				if message.RunID != runID || s.acknowledge(message.acknowledgement) != nil {
					_ = conn.Close(websocket.StatusPolicyViolation, "invalid fixture acknowledgement")
					return
				}
				if !send(map[string]any{"type": "ack", "cursor": message.Cursor}) {
					return
				}
			case "ping":
				if !send(map[string]string{"type": "pong"}) {
					return
				}
			default:
				_ = conn.Close(websocket.StatusPolicyViolation, "unsupported fixture message")
				return
			}
		case e, open := <-ch:
			if !open {
				return
			}
			if !send(e) {
				return
			}
			if e.Kind == "disconnect" {
				_ = conn.Close(websocket.StatusServiceRestart, "deterministic fixture disconnect")
				return
			}
		}
	}
}
