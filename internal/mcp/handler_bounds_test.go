package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
)

// callToolJSON runs one tool call and decodes the JSON payload the agent would
// actually receive, so these tests cover the handler wiring rather than the
// helpers underneath it.
func callToolJSON(t *testing.T, srv *Server, name, args string) map[string]any {
	t.Helper()
	result, rpcErr := srv.callTool(context.Background(), name, json.RawMessage(args))
	if rpcErr != nil {
		t.Fatalf("%s: rpc error %v", name, rpcErr)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T, want a result map", name, result)
	}
	content, _ := payload["content"].([]toolContent)
	if len(content) == 0 {
		t.Fatalf("%s returned no content: %#v", name, payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(content[0].Text), &decoded); err != nil {
		t.Fatalf("%s payload is not a JSON object: %v (%s)", name, err, content[0].Text)
	}
	return decoded
}

type longPageController struct {
	fakeController
}

func (longPageController) Read(context.Context) (readability.PageRead, error) {
	return readability.PageRead{
		URL:      "https://example.com/long",
		Title:    "Long",
		Main:     strings.Repeat("z", 60000),
		Headings: []readability.Heading{{Level: 1, Text: "Chapter"}},
		Links:    []readability.Link{{Text: "next", Href: "/2"}},
	}, nil
}

type upstreamWindowController struct {
	longPageController
	fullCalls   int
	windowCalls int
	options     readability.ReadOptions
}

func (c *upstreamWindowController) Read(context.Context) (readability.PageRead, error) {
	c.fullCalls++
	return c.longPageController.Read(context.Background())
}

func (c *upstreamWindowController) ReadWindow(_ context.Context, options readability.ReadOptions) (readability.PageRead, error) {
	c.windowCalls++
	c.options = options
	full, _ := c.longPageController.Read(context.Background())
	return readability.Window(full, options), nil
}

func TestReadToolUsesBrowserHostWindowCapability(t *testing.T) {
	controller := &upstreamWindowController{}
	srv := &Server{manager: controller, toolProfile: "all"}
	got := callToolJSON(t, srv, "brw_read", `{"max_chars":321,"offset":99,"include":["main"]}`)
	if controller.fullCalls != 0 || controller.windowCalls != 1 {
		t.Fatalf("full calls=%d window calls=%d", controller.fullCalls, controller.windowCalls)
	}
	if controller.options.MaxChars != 321 || controller.options.Offset != 99 || len(controller.options.Include) != 1 {
		t.Fatalf("forwarded options=%+v", controller.options)
	}
	if main, _ := got["main"].(string); len(main) != 321 {
		t.Fatalf("windowed main len=%d", len(main))
	}
}

func TestReadToolBoundsProseByDefault(t *testing.T) {
	srv := &Server{manager: longPageController{}, toolProfile: "all"}
	got := callToolJSON(t, srv, "brw_read", `{}`)

	main, _ := got["main"].(string)
	if len(main) != readability.DefaultReadMaxChars {
		t.Fatalf("default read returned %d chars, want the %d-char bound", len(main), readability.DefaultReadMaxChars)
	}
	if truncated, _ := got["main_truncated"].(bool); !truncated {
		t.Fatal("bounded read did not set main_truncated")
	}
	if next, _ := got["next_offset"].(float64); int(next) != readability.DefaultReadMaxChars {
		t.Fatalf("next_offset = %v, want %d", got["next_offset"], readability.DefaultReadMaxChars)
	}
	// The removed duplicate must not come back through the handler either.
	if _, ok := got["text"]; ok {
		t.Fatal("brw_read still returns a duplicate top-level text field")
	}
}

func TestReadToolHonoursExplicitBounds(t *testing.T) {
	srv := &Server{manager: longPageController{}, toolProfile: "all"}

	got := callToolJSON(t, srv, "brw_read", `{"max_chars":100,"offset":50}`)
	if main, _ := got["main"].(string); len(main) != 100 {
		t.Fatalf("max_chars=100 returned %d chars", len(main))
	}

	unbounded := callToolJSON(t, srv, "brw_read", `{"max_chars":-1}`)
	if main, _ := unbounded["main"].(string); len(main) != 60000 {
		t.Fatalf("max_chars=-1 returned %d chars, want the whole 60000", len(main))
	}
}

func TestReadToolIncludeSkipsProse(t *testing.T) {
	srv := &Server{manager: longPageController{}, toolProfile: "all"}
	got := callToolJSON(t, srv, "brw_read", `{"include":["headings","links"]}`)

	if main, _ := got["main"].(string); main != "" {
		t.Fatalf("include without main still returned %d chars of prose", len(main))
	}
	if headings, _ := got["headings"].([]any); len(headings) != 1 {
		t.Fatalf("headings = %v, want the one heading", got["headings"])
	}
}

func TestReadToolRejectsUnknownSection(t *testing.T) {
	srv := &Server{manager: longPageController{}, toolProfile: "all"}
	_, rpcErr := srv.callTool(context.Background(), "brw_read", json.RawMessage(`{"include":["heaadings"]}`))
	if rpcErr == nil {
		t.Fatal("unknown include section accepted; the read would come back silently empty")
	}
}

type noisyConsoleController struct {
	fakeController
	calls int
}

func (c *noisyConsoleController) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	c.calls++
	if c.calls > 1 {
		// The browser buffer drains on read, exactly as the real backends do.
		return nil, nil
	}
	return []browser.ConsoleMessage{
		{Level: "log", Text: "mounted"},
		{Level: "error", Text: "TypeError: undefined is not a function"},
		{Level: "log", Text: "route change"},
	}, nil
}

func TestConsoleToolFilterKeepsUnmatchedMessagesReadable(t *testing.T) {
	srv := &Server{manager: &noisyConsoleController{}, toolProfile: "all"}

	errorsOnly := callToolJSON(t, srv, "brw_console", `{"only_errors":true}`)
	if returned, _ := errorsOnly["returned"].(float64); returned != 1 {
		t.Fatalf("only_errors returned %v messages, want 1", errorsOnly["returned"])
	}
	if retained, _ := errorsOnly["retained"].(float64); retained != 2 {
		t.Fatalf("retained = %v, want the 2 filtered-out log lines still buffered", errorsOnly["retained"])
	}

	// The browser buffer is empty by now, so anything the first call returns
	// must come from brw's own retention.
	rest := callToolJSON(t, srv, "brw_console", `{}`)
	if returned, _ := rest["returned"].(float64); returned != 2 {
		t.Fatalf("second read returned %v, want the 2 messages the filter skipped", rest["returned"])
	}
}

func TestConsoleToolRejectsInvalidPattern(t *testing.T) {
	srv := &Server{manager: &noisyConsoleController{}, toolProfile: "all"}
	_, rpcErr := srv.callTool(context.Background(), "brw_console", json.RawMessage(`{"pattern":"[unclosed"}`))
	if rpcErr == nil {
		t.Fatal("invalid regex accepted; it would silently match nothing")
	}
}

type countingPressController struct {
	fakeController
	presses int
	scrolls int
}

func (c *countingPressController) Press(context.Context, string) (browser.ActionResult, error) {
	c.presses++
	return browser.ActionResult{OK: true}, nil
}

func (c *countingPressController) Scroll(context.Context, string) (browser.ActionResult, error) {
	c.scrolls++
	return browser.ActionResult{OK: true}, nil
}

func TestPressAndScrollRepeatCollapseRoundTrips(t *testing.T) {
	ctrl := &countingPressController{}
	srv := &Server{manager: ctrl, toolProfile: "all"}

	callToolJSON(t, srv, "brw_press", `{"key":"ArrowDown","repeat":20}`)
	if ctrl.presses != 20 {
		t.Fatalf("repeat=20 pressed %d times, want 20 in one call", ctrl.presses)
	}

	callToolJSON(t, srv, "brw_scroll", `{"direction":"down","repeat":3}`)
	if ctrl.scrolls != 3 {
		t.Fatalf("repeat=3 scrolled %d times, want 3", ctrl.scrolls)
	}

	// An omitted repeat must behave exactly as it did before the option existed.
	callToolJSON(t, srv, "brw_press", `{"key":"Enter"}`)
	if ctrl.presses != 21 {
		t.Fatalf("omitted repeat pressed %d extra times, want exactly 1", ctrl.presses-20)
	}
}

func TestPressRepeatRejectsOutOfRange(t *testing.T) {
	srv := &Server{manager: &countingPressController{}, toolProfile: "all"}
	_, rpcErr := srv.callTool(context.Background(), "brw_press", json.RawMessage(`{"key":"a","repeat":5000}`))
	if rpcErr == nil {
		t.Fatal("out-of-range repeat accepted; it would hold the tab for 5000 presses")
	}
}
