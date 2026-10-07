// Package bidi is a WebDriver BiDi client used to establish, by experiment, which of brw's primitives a BiDi-only browser can reproduce.
package bidi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const maxMessageBytes = 64 << 20

// CommandError is a BiDi error response.
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

// IsUnknownCommand reports whether err is the browser saying it does not implement the command at all, as opposed to rejecting the arguments.
func IsUnknownCommand(err error) bool {
	var cmdErr *CommandError
	return errors.As(err, &cmdErr) && cmdErr.Code == "unknown command"
}

// Event is one BiDi event, kept as raw params so a caller decodes only the fields it asserts on.
type Event struct {
	Method   string
	Params   json.RawMessage
	Received time.Time
}

type pending struct {
	result chan json.RawMessage
	fail   chan error
}

const maxRecordedEvents = 4096

// Conn is a live BiDi session over a WebSocket.
type Conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	waiting map[uint64]pending
	events  []Event

	firstSeq uint64

	nextSeq uint64

	readErr  error
	closedCh chan struct{}

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

// Close ends the session's socket.
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
	Type    string          `json:"type"`
	ID      *uint64         `json:"id"`
	Result  json.RawMessage `json:"result"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
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
			c.events = slices.Delete(c.events, 0, drop)
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

// Command sends one BiDi command and decodes its result into out, which may be nil when the caller only needs the success.
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

// Subscribe asks for the named event methods.
func (c *Conn) Subscribe(ctx context.Context, events ...string) error {
	return c.Command(ctx, "session.subscribe", map[string]any{"events": events}, nil)
}

// Await returns the first recorded event for method that satisfies match, waiting for one to arrive if none has yet.
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
