// Package bidi is a WebDriver BiDi client used to establish, by experiment,
// which of brw's primitives a BiDi-only browser can reproduce. It is a
// prototype: nothing in brw's tool surface routes through it, and no capability
// is advertised on its behalf. docs/bidi-prototype.md records the measurements
// this package produced and the decision taken from them.
//
// It is deliberately not a browser.Controller. A Controller has to answer for
// settle, actionability, downloads, dialogs and artifacts at once, and the
// point of the prototype is to find out whether those are reproducible one at a
// time before any of them is promised.
package bidi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// maxMessageBytes bounds one BiDi frame. A screenshot or a printed PDF arrives
// base64 in the command result, so the limit has to clear a full-page capture;
// the library's 32 KiB default truncates one into a read error.
const maxMessageBytes = 64 << 20

// CommandError is a BiDi error response. Code is the spec's error code
// ("unknown command", "invalid argument", "no such frame"), which is the part a
// caller can branch on: an unsupported command and a malformed argument are the
// same HTTP-less failure otherwise.
type CommandError struct {
	Code    string
	Message string
}

func (e *CommandError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// IsUnknownCommand reports whether err is the browser saying it does not
// implement the command at all, as opposed to rejecting the arguments. The
// distinction is the whole answer to "does this backend have the primitive".
func IsUnknownCommand(err error) bool {
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	return cmdErr.Code == "unknown command"
}

// Event is one BiDi event, kept as raw params so a caller decodes only the
// fields it asserts on.
type Event struct {
	Method   string
	Params   json.RawMessage
	Received time.Time
}

type pending struct {
	result chan json.RawMessage
	fail   chan error
}

// maxRecordedEvents bounds the event ring. A session that subscribes to
// network and log events produces thousands a minute, and both the memory and
// Await's scan are linear in what is kept. The oldest are dropped; Await
// carries a sequence rather than an index so a drop cannot make it skip one.
const maxRecordedEvents = 4096

// Conn is a live BiDi session over a WebSocket. It multiplexes command
// responses by id and records recent events in a bounded ring, so a caller can
// subscribe first and assert afterwards without racing the browser.
type Conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	waiting map[uint64]pending
	events  []Event
	// firstSeq is the sequence number of events[0]. Sequence numbers are
	// assigned in arrival order and never reused, so they stay meaningful after
	// the ring drops from the front.
	firstSeq uint64
	// nextSeq is the sequence the next arriving event will take.
	nextSeq uint64
	// closed carries the reader goroutine's exit reason, so a command still in
	// flight when the socket drops fails with that reason instead of hanging.
	readErr  error
	closedCh chan struct{}
	// eventCh is closed and replaced on every event so waiters wake without a
	// poll loop.
	eventCh chan struct{}
}

// Dial opens a BiDi session at url (ws://host:port/session).
func Dial(ctx context.Context, url string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial bidi endpoint %s: %w", url, err)
	}
	ws.SetReadLimit(maxMessageBytes)
	c := &Conn{
		ws:       ws,
		waiting:  map[uint64]pending{},
		closedCh: make(chan struct{}),
		eventCh:  make(chan struct{}),
	}
	go c.read()
	return c, nil
}

// Close ends the session's socket. Commands blocked on a response fail with the
// close reason rather than waiting out their context.
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "bidi prototype done")
}

func (c *Conn) read() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			waiting := c.waiting
			c.waiting = map[uint64]pending{}
			c.mu.Unlock()
			for _, p := range waiting {
				p.fail <- err
			}
			close(c.closedCh)
			return
		}
		c.dispatch(data)
	}
}

type frame struct {
	Type       string          `json:"type"`
	ID         *uint64         `json:"id"`
	Result     json.RawMessage `json:"result"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params"`
	Error      string          `json:"error"`
	Message    string          `json:"message"`
	Stacktrace string          `json:"stacktrace"`
}

func (c *Conn) dispatch(data []byte) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	switch f.Type {
	case "event":
		c.mu.Lock()
		c.events = append(c.events, Event{Method: f.Method, Params: f.Params, Received: time.Now()})
		c.nextSeq++
		if len(c.events) > maxRecordedEvents {
			drop := len(c.events) - maxRecordedEvents
			c.events = append(c.events[:0], c.events[drop:]...)
			c.firstSeq += uint64(drop)
		}
		close(c.eventCh)
		c.eventCh = make(chan struct{})
		c.mu.Unlock()
	case "success", "error":
		if f.ID == nil {
			return
		}
		c.mu.Lock()
		p, ok := c.waiting[*f.ID]
		delete(c.waiting, *f.ID)
		c.mu.Unlock()
		if !ok {
			return
		}
		if f.Type == "error" {
			p.fail <- &CommandError{Code: f.Error, Message: f.Message}
			return
		}
		p.result <- f.Result
	}
}

// Command sends one BiDi command and decodes its result into out, which may be
// nil when the caller only needs the success. A BiDi error response becomes a
// *CommandError rather than a generic error, so "unknown command" stays
// distinguishable from "bad arguments".
func (c *Conn) Command(ctx context.Context, method string, params any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	p := pending{result: make(chan json.RawMessage, 1), fail: make(chan error, 1)}
	c.waiting[id] = p
	c.mu.Unlock()

	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		c.forget(id)
		return err
	}
	// One writer at a time: the WebSocket library rejects concurrent writes, and
	// a prototype that subscribes while a navigation command is in flight does
	// exactly that.
	c.writeMu.Lock()
	err = c.ws.Write(ctx, websocket.MessageText, payload)
	c.writeMu.Unlock()
	if err != nil {
		c.forget(id)
		return fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case err := <-p.fail:
		return fmt.Errorf("%s: %w", method, err)
	case result := <-p.result:
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}
}

func (c *Conn) forget(id uint64) {
	c.mu.Lock()
	delete(c.waiting, id)
	c.mu.Unlock()
}

// Subscribe asks for the named event methods. BiDi refuses an event name it
// does not implement, so a failed subscribe is itself the capability answer.
func (c *Conn) Subscribe(ctx context.Context, events ...string) error {
	return c.Command(ctx, "session.subscribe", map[string]any{"events": events}, nil)
}

// Await returns the first recorded event for method that satisfies match,
// waiting for one to arrive if none has yet. Events still in the ring when the
// call starts count: a settle machinery built on this has to be able to
// subscribe, act, and then ask, without losing an event that landed in between.
//
// The cursor is a sequence number, not a slice index, so an event dropped from
// the ring while this call waits cannot shift the position out from under it.
//
// A nil match accepts any event with that method.
func (c *Conn) Await(ctx context.Context, method string, match func(json.RawMessage) bool) (Event, error) {
	var cursor uint64
	for {
		c.mu.Lock()
		if cursor < c.firstSeq {
			cursor = c.firstSeq
		}
		for ; cursor < c.nextSeq; cursor++ {
			ev := c.events[cursor-c.firstSeq]
			if ev.Method != method {
				continue
			}
			if match == nil || match(ev.Params) {
				c.mu.Unlock()
				return ev, nil
			}
		}
		wait := c.eventCh
		readErr := c.readErr
		c.mu.Unlock()
		if readErr != nil {
			return Event{}, fmt.Errorf("await %s: %w", method, readErr)
		}
		select {
		case <-ctx.Done():
			return Event{}, fmt.Errorf("await %s: %w", method, ctx.Err())
		case <-wait:
		case <-c.closedCh:
			return Event{}, fmt.Errorf("await %s: connection closed", method)
		}
	}
}

// Events returns a copy of everything recorded so far.
func (c *Conn) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}
